package minimax

// Private uploads.
//
// The message endpoint no longer accepts an image by external URL. An
// attachment must carry an object_key the account owns, or an allow-listed
// legacy URL, and anything else is answered
//
//	[1406010011] attachment requires one owned object_key or allowed legacy URL
//
// The web bundle's flow is a private upload in two calls: mint a policy, then
// POST the bytes to the policy's OSS URL, and reference the key it returns. This
// file reproduces that flow. cmd/uploadprobe confirmed every step against the
// live upstream — prepare answers an object_key and a policy, the OSS POST
// answers 204, and a message carrying the key is accepted — which is why the
// shapes here are pinned rather than guessed.
//
// The bytes come from the caller. A remote URL is deliberately *not* fetched
// here: the upstream's own client uploads bytes it already has, and downloading
// a caller-supplied address is a capability the upstream never had. Adding it
// would be the gateway inventing a feature rather than exposing one, and it
// would put the gateway on the far side of an SSRF for no gain.
//
// An object key belongs to the account that minted it, so resolution happens
// per turn attempt, with the account that will send the message: a retry on a
// different account has to upload again, because the first account's key is not
// one the second account owns.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"

	"minimax2api/internal/config"
)

const (
	// DefaultUploadsPath mints a private upload policy.
	DefaultUploadsPath = "/minimax-cloud/api/v1/uploads/prepare"
	// uploadPurposeMessageAttachment is the `purpose` value a chat attachment
	// uses. The bundle's enum is MessageAttachment:1.
	uploadPurposeMessageAttachment = 1
)

// uploadsPath is where a private upload policy is minted.
func uploadsPath(settings config.Settings) string {
	if path := strings.TrimSpace(settings.Upstream.UploadsPath); path != "" {
		return path
	}
	return DefaultUploadsPath
}

// uploadPolicy is the response to a prepare call: the key to reference and the
// OSS POST that will store the bytes.
type uploadPolicy struct {
	ObjectKey  string
	URL        string
	FormFields map[string]string
}

// prepareUpload mints a private upload policy for one file.
func (c *Client) prepareUpload(ctx context.Context, cred Credential, name, mime string, size int) (uploadPolicy, error) {
	settings := c.settings()
	payload, err := json.Marshal(map[string]any{
		"purpose":    uploadPurposeMessageAttachment,
		"file_name":  name,
		"mime_type":  mime,
		"size_bytes": size,
	})
	if err != nil {
		return uploadPolicy{}, err
	}
	req, err := c.newRequest(ctx, settings, cred, http.MethodPost, requestTarget{Path: uploadsPath(settings)}, payload)
	if err != nil {
		return uploadPolicy{}, err
	}
	resp, err := c.do(req, settings)
	if err != nil {
		return uploadPolicy{}, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return uploadPolicy{}, ErrInvalidCredential
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return uploadPolicy{}, fmt.Errorf("minimax upload prepare HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return parseUploadPolicy(body)
}

// parseUploadPolicy reads the prepare response.
//
// The object may arrive directly or wrapped in `data`, and both shapes have
// been observed in the bundle's own unwrapping, so both are accepted here.
func parseUploadPolicy(body []byte) (uploadPolicy, error) {
	raw := body
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err == nil {
		if _, ok := top["object_key"]; !ok {
			if data, ok := top["data"]; ok {
				raw = data
			}
		}
	}
	var parsed struct {
		ObjectKey string `json:"object_key"`
		Upload    struct {
			URL        string            `json:"url"`
			Method     string            `json:"method"`
			FormFields map[string]string `json:"form_fields"`
		} `json:"upload_instruction"`
		BaseResp struct {
			StatusCode int    `json:"status_code"`
			StatusMsg  string `json:"status_msg"`
		} `json:"base_resp"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return uploadPolicy{}, fmt.Errorf("minimax upload prepare: %w", err)
	}
	if parsed.BaseResp.StatusCode != 0 {
		return uploadPolicy{}, fmt.Errorf("minimax upload prepare: %d %s",
			parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	if parsed.ObjectKey == "" || parsed.Upload.URL == "" {
		return uploadPolicy{}, fmt.Errorf("minimax upload prepare: incomplete policy: %s", snippet(body))
	}
	if !strings.EqualFold(parsed.Upload.Method, http.MethodPost) {
		return uploadPolicy{}, fmt.Errorf("minimax upload prepare: unexpected method %q", parsed.Upload.Method)
	}
	// The policy has to describe the object we are about to store. The host is
	// deliberately not pinned to an OSS domain: a self-hosted relay and a
	// regional deployment are supported configurations, and the policy comes
	// from the configured upstream over TLS either way.
	for _, field := range []string{"key", "OSSAccessKeyId", "policy", "Signature"} {
		if strings.TrimSpace(parsed.Upload.FormFields[field]) == "" {
			return uploadPolicy{}, fmt.Errorf("minimax upload prepare: policy is missing %s", field)
		}
	}
	if parsed.Upload.FormFields["key"] != parsed.ObjectKey {
		return uploadPolicy{}, errors.New("minimax upload prepare: policy key does not match object_key")
	}
	return uploadPolicy{
		ObjectKey:  parsed.ObjectKey,
		URL:        parsed.Upload.URL,
		FormFields: parsed.Upload.FormFields,
	}, nil
}

// uploadObject POSTs the bytes to the policy's OSS URL.
//
// The file part is appended after every policy field, which is what the bundle
// does and what the OSS POST policy expects.
func (c *Client) uploadObject(ctx context.Context, settings config.Settings, policy uploadPolicy, name string, data []byte) error {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	for field, value := range policy.FormFields {
		if err := writer.WriteField(field, value); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("file", name)
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, policy.URL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("content-type", writer.FormDataContentType())

	resp, err := c.do(req, settings)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("minimax upload HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

// ResolveImages gives every image an object_key the account owns.
//
// Images that already carry a key are passed through; the rest are uploaded
// under the given credential from the bytes they already hold. An error from any
// step is returned as-is: the caller retries on another account, and the account
// that failed is the one the message must not be sent under.
func (c *Client) ResolveImages(ctx context.Context, cred Credential, images []UploadedImage) ([]UploadedImage, error) {
	settings := c.settings()
	out := make([]UploadedImage, 0, len(images))
	for _, image := range images {
		if image.ObjectKey != "" {
			out = append(out, image)
			continue
		}
		if len(image.Data) == 0 {
			return nil, errors.New("image has no bytes to upload")
		}
		name := image.FileName
		if name == "" {
			name = "image.png"
		}
		mime := image.MimeType
		if mime == "" {
			mime = "image/png"
		}
		policy, err := c.prepareUpload(ctx, cred, name, mime, len(image.Data))
		if err != nil {
			return nil, err
		}
		if err := c.uploadObject(ctx, settings, policy, name, image.Data); err != nil {
			return nil, err
		}
		image.ObjectKey = policy.ObjectKey
		image.FileName = name
		image.MimeType = mime
		image.SizeBytes = len(image.Data)
		out = append(out, image)
	}
	return out, nil
}
