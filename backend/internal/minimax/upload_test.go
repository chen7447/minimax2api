package minimax

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"minimax2api/internal/config"
)

// --- prepare / policy ------------------------------------------------------

// The default path is a literal in the config package, which cannot import this
// one. This pins the two together, the same way the session path is pinned.
func TestUploadsPathMatchesThePinnedDefault(t *testing.T) {
	settings := config.DefaultSettings(t.TempDir())
	if got := uploadsPath(settings); got != DefaultUploadsPath {
		t.Fatalf("uploadsPath = %q, want %q", got, DefaultUploadsPath)
	}
	settings.Upstream.UploadsPath = "/custom/upload"
	if got := uploadsPath(settings); got != "/custom/upload" {
		t.Fatalf("an explicit path must win, got %q", got)
	}
}

// The response arrives directly or wrapped in `data`; both are accepted.
func TestParseUploadPolicyAcceptsBothEnvelopes(t *testing.T) {
	inner := `{"object_key":"11111111-1111-4111-8111-111111111111",` +
		`"upload_instruction":{"url":"https://oss.example/","method":"POST",` +
		`"form_fields":{"key":"11111111-1111-4111-8111-111111111111",` +
		`"OSSAccessKeyId":"ak","policy":"p","Signature":"s"}}}`
	for _, tc := range []struct {
		name string
		body string
	}{
		{"direct", inner},
		{"wrapped", `{"data":` + inner + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy, err := parseUploadPolicy([]byte(tc.body))
			if err != nil {
				t.Fatalf("parseUploadPolicy: %v", err)
			}
			if policy.ObjectKey != "11111111-1111-4111-8111-111111111111" {
				t.Fatalf("object key = %q", policy.ObjectKey)
			}
			if policy.URL != "https://oss.example/" {
				t.Fatalf("url = %q", policy.URL)
			}
		})
	}
}

// A policy that does not describe the object we are about to store is refused,
// rather than used to POST the bytes somewhere unspecified.
func TestParseUploadPolicyRejectsInconsistentPolicies(t *testing.T) {
	valid := map[string]string{
		"key":            "11111111-1111-4111-8111-111111111111",
		"OSSAccessKeyId": "ak",
		"policy":         "p",
		"Signature":      "s",
	}
	cases := []struct {
		name   string
		key    string
		method string
		fields map[string]string
	}{
		{"missing signature", "11111111-1111-4111-8111-111111111111", "POST", map[string]string{
			"key": "11111111-1111-4111-8111-111111111111", "OSSAccessKeyId": "ak", "policy": "p",
		}},
		{"key does not match", "11111111-1111-4111-8111-111111111111", "POST", map[string]string{
			"key": "22222222-2222-4222-8222-222222222222", "OSSAccessKeyId": "ak", "policy": "p", "Signature": "s",
		}},
		{"wrong method", "11111111-1111-4111-8111-111111111111", "PUT", valid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"object_key":%q,"upload_instruction":{"url":"https://oss.example/","method":%q,"form_fields":%s}}`,
				tc.key, tc.method, jsonFields(tc.fields))
			if _, err := parseUploadPolicy([]byte(body)); err == nil {
				t.Fatal("a policy that does not match the upload must be refused")
			}
		})
	}
}

func jsonFields(fields map[string]string) string {
	parts := make([]string, 0, len(fields))
	for key, value := range fields {
		parts = append(parts, fmt.Sprintf("%q:%q", key, value))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// --- resolution ------------------------------------------------------------

// uploadStub serves the two calls a private upload makes and records what was
// stored.
func uploadStub(t *testing.T, received *[]byte) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/uploads/prepare"):
			w.Header().Set("content-type", "application/json")
			_, _ = fmt.Fprintf(w, `{"object_key":"11111111-1111-4111-8111-111111111111",`+
				`"upload_instruction":{"url":%q,"method":"POST","form_fields":`+
				`{"key":"11111111-1111-4111-8111-111111111111","OSSAccessKeyId":"ak","policy":"p","Signature":"s"}}}`,
				server.URL+"/oss")
		case r.URL.Path == "/oss":
			if err := r.ParseMultipartForm(1 << 20); err == nil {
				if file, _, err := r.FormFile("file"); err == nil {
					defer file.Close()
					*received, _ = io.ReadAll(file)
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func uploadTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	settings := config.DefaultSettings(t.TempDir())
	settings.Upstream.BaseURL = server.URL
	return New(func() config.Settings { return settings })
}

// Inline bytes are uploaded as-is, and the image comes back with the key, name
// and type the attachment needs.
func TestResolveImagesUploadsInlineData(t *testing.T) {
	var received []byte
	server := uploadStub(t, &received)
	client := uploadTestClient(t, server)

	images, err := client.ResolveImages(context.Background(), Credential{Token: "t"}, []UploadedImage{{
		Data:     []byte("png-bytes"),
		FileName: "inline.png",
		MimeType: "image/png",
	}})
	if err != nil {
		t.Fatalf("ResolveImages: %v", err)
	}
	if len(images) != 1 || images[0].ObjectKey == "" {
		t.Fatalf("images = %+v, want one with an object key", images)
	}
	if images[0].SizeBytes != len("png-bytes") {
		t.Fatalf("size = %d", images[0].SizeBytes)
	}
	if string(received) != "png-bytes" {
		t.Fatalf("stored bytes = %q, want the inline payload", received)
	}
}

// An image with no bytes is a caller error, not something to upload empty.
func TestResolveImagesRejectsEmptyData(t *testing.T) {
	var received []byte
	server := uploadStub(t, &received)
	client := uploadTestClient(t, server)

	if _, err := client.ResolveImages(context.Background(), Credential{Token: "t"}, []UploadedImage{{}}); err == nil {
		t.Fatal("an image with no bytes must be refused")
	}
	if len(received) != 0 {
		t.Fatalf("nothing should have been uploaded, got %q", received)
	}
}
