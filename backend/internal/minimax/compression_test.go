package minimax

import (
	"context"
	"net/http"
	"testing"

	"minimax2api/internal/config"
)

// A streaming request must not advertise gzip.
//
// This upstream answers `Accept-Encoding: gzip` by buffering the entire stream
// so it can compress it, which turns a streaming endpoint into a
// request/response one and makes a fast model look slow. Go's transport adds
// that header on its own, so the only way to keep it off is to say so.
//
// Measured 2026-10-06 against agent.minimax.io: with the advertisement, a
// 400-word answer arrived as two reads at 8.9s; without it, 277 reads spread
// over 9.5s.
func TestStreamingRequestsRefuseCompression(t *testing.T) {
	settings := config.DefaultSettings(t.TempDir())
	cred := Credential{Token: "tok", Region: RegionGlobal}
	client := New(func() config.Settings { return settings })

	t.Run("agent transport", func(t *testing.T) {
		req, err := client.newRequest(context.Background(), settings, cred, http.MethodPost,
			requestTarget{Path: "/minimax-cloud/api/v1/session/s/message", Stream: true}, []byte("{}"))
		if err != nil {
			t.Fatalf("newRequest: %v", err)
		}
		if got := req.Header.Get("accept-encoding"); got != "identity" {
			t.Fatalf("streaming accept-encoding = %q, want identity", got)
		}
	})

	t.Run("direct transport", func(t *testing.T) {
		req, err := client.newDirectRequest(context.Background(), settings, cred, http.MethodPost,
			DefaultModelPath, []byte("{}"), true)
		if err != nil {
			t.Fatalf("newDirectRequest: %v", err)
		}
		if got := req.Header.Get("accept-encoding"); got != "identity" {
			t.Fatalf("streaming accept-encoding = %q, want identity", got)
		}
	})

	// A non-streaming call may keep whatever the transport would send: there is
	// nothing to buffer, and compression is a win.
	t.Run("non-streaming is left alone", func(t *testing.T) {
		req, err := client.newDirectRequest(context.Background(), settings, cred, http.MethodGet,
			DefaultModelsPath, nil, false)
		if err != nil {
			t.Fatalf("newDirectRequest: %v", err)
		}
		if got := req.Header.Get("accept-encoding"); got != "" {
			t.Fatalf("non-streaming accept-encoding = %q, want it unset", got)
		}
	})
}

// The transport must not add the header either, or the explicit one above would
// be redundant rather than load-bearing.
func TestTransportDoesNotAdvertiseCompression(t *testing.T) {
	client := New(func() config.Settings { return config.DefaultSettings(t.TempDir()) })
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", client.http.Transport)
	}
	if !transport.DisableCompression {
		t.Fatal("the transport would add Accept-Encoding: gzip to every request")
	}
}
