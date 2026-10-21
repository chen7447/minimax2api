package main

// uploadprobe answers one question against the live upstream: *can this
// account upload a file and reference it in a message by object_key?*
//
// The gateway used to hand the agent a URL and let it fetch the image itself.
// That stopped working: the message endpoint now answers
//
//	[1406010011] attachment requires one owned object_key or allowed legacy URL
//
// The web bundle shows the flow it expects instead — a private upload that
// mints an object_key, then an attachment carrying that key:
//
//	POST /minimax-cloud/api/v1/uploads/prepare   → object_key + OSS POST policy
//	POST <policy url>  (multipart)               → 204
//	POST /…/session/{id}/message  attachments:[{type,file_path,file_name,mime_type,object_key}]
//
// This tool reproduces exactly that, end to end, with a real account, so the
// protocol is confirmed before any of it is wired into the gateway.
//
// It prints structure, not secrets: credentials never appear, the object_key is
// masked, and the message body is truncated. Diagnostics only — not on any
// request path.
//
//	MM_TOKEN=… MM_USER_ID=… MM_DEVICE_ID=… MM_UUID=… MM_AGENT_ID=… \
//	  go run ./cmd/uploadprobe
//
// Optional: MM_REGION (global|cn), MM_BASE_URL, MM_PROXY.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"minimax2api/internal/config"
	"minimax2api/internal/minimax"
)

// onePixelPNG is a valid 1×1 PNG, so the upstream accepts it as an image
// without the probe shipping a fixture file.
const onePixelPNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// preparePath is where the web bundle mints a private upload.
const preparePath = "/minimax-cloud/api/v1/uploads/prepare"

// messagePurpose is the `purpose` enum value for a chat attachment.
// (MessageAttachment=1 in the bundle's enum.)
const messagePurpose = 1

func main() {
	proxy := os.Getenv("MM_PROXY")
	region := minimax.RegionGlobal
	if strings.EqualFold(os.Getenv("MM_REGION"), "cn") {
		region = minimax.RegionCN
	}
	cred := minimax.Credential{
		Region:       region,
		Token:        os.Getenv("MM_TOKEN"),
		UserID:       os.Getenv("MM_USER_ID"),
		AgentID:      os.Getenv("MM_AGENT_ID"),
		DeviceID:     os.Getenv("MM_DEVICE_ID"),
		UUID:         os.Getenv("MM_UUID"),
		BaseURL:      os.Getenv("MM_BASE_URL"),
		ScreenWidth:  1536,
		ScreenHeight: 864,
	}
	if cred.Token == "" || cred.UserID == "" || cred.DeviceID == "" || cred.UUID == "" || cred.AgentID == "" {
		fmt.Println("missing credentials: need MM_TOKEN MM_USER_ID MM_DEVICE_ID MM_UUID MM_AGENT_ID")
		os.Exit(2)
	}

	settings := config.DefaultSettings(os.TempDir())
	settings.Upstream.Proxy = proxy
	settings.Upstream.ScreenWidth = 1536
	settings.Upstream.ScreenHeight = 864
	client := minimax.New(func() config.Settings { return settings })

	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// MM_PROMPT sends a plain text turn straight to the upstream and prints the
	// raw SSE verbatim. It exists to answer one question the gateway cannot
	// answer about itself: when a reply comes back garbled or off-topic, did the
	// upstream send it that way, or did something in our path corrupt it? A
	// direct call with no gateway in between settles it.
	if prompt := os.Getenv("MM_PROMPT"); prompt != "" {
		rawTurn(ctx, client, cred, prompt)
		return
	}

	raw, err := base64.StdEncoding.DecodeString(onePixelPNG)
	if err != nil {
		fmt.Println("bad fixture:", err)
		os.Exit(1)
	}

	fmt.Println("== 1. create session")
	sessionID, err := client.CreateSession(ctx, cred)
	if err != nil {
		fmt.Println("   session failed:", err)
		os.Exit(1)
	}
	fmt.Printf("   session ok: %s…\n", mask(sessionID))

	fmt.Println("== 2. prepare private upload")
	objectKey, uploadURL, formFields, err := prepare(ctx, client, cred, len(raw))
	if err != nil {
		fmt.Println("   prepare failed:", err)
		os.Exit(1)
	}
	fmt.Printf("   object_key: %s\n", mask(objectKey))
	fmt.Printf("   upload url host: %s\n", hostOf(uploadURL))
	fmt.Printf("   form fields: %s\n", strings.Join(sortedKeys(formFields), ", "))

	fmt.Println("== 3. upload bytes to OSS")
	if err := upload(uploadURL, formFields, raw); err != nil {
		fmt.Println("   upload failed:", err)
		os.Exit(1)
	}
	fmt.Println("   upload ok (204)")

	fmt.Println("== 4. send message with object_key attachment")
	// Two shapes are possible. The bundle picks between them on remote-control
	// mode: the flat `{type,file_path,…,object_key}` form only in that mode,
	// the nested `{meta, cloud}` form otherwise — and the nested form is also
	// what the thrift contract declares for AttachmentInput. Send both so the
	// live upstream, not a reading of the bundle, decides which is accepted.
	shapes := []struct {
		name string
		body map[string]any
	}{
		{"flat", map[string]any{
			"type":       "image",
			"file_path":  "",
			"file_name":  "probe.png",
			"mime_type":  "image/png",
			"object_key": objectKey,
		}},
		{"nested", map[string]any{
			"meta": map[string]any{
				"attachment_type": "image",
				"file_name":       "probe.png",
				"mime_type":       "image/png",
				"size_bytes":      len(raw),
			},
			"cloud": map[string]any{"object_key": objectKey},
		}},
	}
	path := "/minimax-cloud/api/v1/session/" + sessionID + "/message"
	for _, shape := range shapes {
		body, _ := json.Marshal(map[string]any{
			"content":      "这张图是什么颜色？一句话",
			"turn_id":      fmt.Sprintf("%d", time.Now().UnixMilli()),
			"worktreeMode": false,
			"attachments":  []any{shape.body},
		})
		probe, err := client.FetchRaw(ctx, cred, http.MethodPost, path, body, true, 64<<10)
		if err != nil {
			fmt.Printf("   [%s] transport error: %v\n", shape.name, err)
			continue
		}
		fmt.Printf("   [%s] status: %d\n", shape.name, probe.Status)
		fmt.Printf("   [%s] body: %s\n", shape.name, dump(probe.Body))
	}
}

// rawTurn sends text-only turns straight to the upstream and prints what came
// back, with no gateway in between.
//
// MM_TURN overrides turn_id. MM_REPEAT runs several turns through ONE process,
// which is the only way to reuse a keep-alive connection the way the gateway
// does: a fresh process always dials a fresh connection, and a stream that is
// not cleanly terminated only misbehaves on a connection that is reused.
func rawTurn(ctx context.Context, client *minimax.Client, cred minimax.Credential, prompt string) {
	turnID := os.Getenv("MM_TURN")
	repeat := 1
	if raw := strings.TrimSpace(os.Getenv("MM_REPEAT")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			repeat = n
		}
	}
	for i := 1; i <= repeat; i++ {
		answer, ok, err := oneTurn(ctx, client, cred, prompt, turnID)
		if err != nil {
			fmt.Printf("[%d] error: %v\n", i, err)
			continue
		}
		fmt.Printf("[%d] utf8=%s text=%s\n", i, ok, jsonString(answer))
	}
}

func oneTurn(ctx context.Context, client *minimax.Client, cred minimax.Credential, prompt, turnID string) (string, string, error) {
	sessionID, err := client.CreateSession(ctx, cred)
	if err != nil {
		return "", "", err
	}
	if turnID == "" {
		turnID = fmt.Sprintf("%d", time.Now().UnixMilli())
	}
	body, _ := json.Marshal(map[string]any{
		"content":      prompt,
		"turn_id":      turnID,
		"worktreeMode": false,
	})
	path := "/minimax-cloud/api/v1/session/" + sessionID + "/message"
	probe, err := client.FetchRaw(ctx, cred, http.MethodPost, path, body, true, 1<<20)
	if err != nil {
		return "", "", err
	}
	valid := "valid"
	if !utf8.ValidString(probe.Body) {
		valid = "INVALID"
	}
	var answer strings.Builder
	for _, line := range strings.Split(probe.Body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var obj map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data:")), &obj) != nil {
			continue
		}
		if chunk, ok := obj["agent_message_chunk"].(map[string]any); ok {
			if text, ok := chunk["msg_content"].(string); ok {
				answer.WriteString(text)
			}
		}
		if msg, ok := obj["agent_message"].(map[string]any); ok && msg["role"] == "assistant" {
			if text, ok := msg["msg_content"].(string); ok && answer.Len() == 0 {
				answer.WriteString(text)
			}
		}
	}
	return answer.String(), valid, nil
}

func jsonString(value string) string {
	raw, _ := json.Marshal(value)
	if len(raw) > 160 {
		raw = append(raw[:157], '.', '.', '.', '"')
	}
	return string(raw)
}

// prepare mints a private upload and returns the object_key and the OSS POST
// policy. The response is either the object itself or wrapped in `data`, which
// is why both shapes are unwrapped here.
func prepare(ctx context.Context, client *minimax.Client, cred minimax.Credential, size int) (string, string, map[string]string, error) {
	payload, _ := json.Marshal(map[string]any{
		"purpose":    messagePurpose,
		"file_name":  "probe.png",
		"mime_type":  "image/png",
		"size_bytes": size,
	})
	probe, err := client.FetchRaw(ctx, cred, http.MethodPost, preparePath, payload, false, 1<<20)
	if err != nil {
		return "", "", nil, err
	}
	if probe.Status != http.StatusOK {
		return "", "", nil, fmt.Errorf("HTTP %d: %s", probe.Status, firstLine(probe.Body, 400))
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(probe.Body), &top); err != nil {
		return "", "", nil, fmt.Errorf("decode: %w", err)
	}
	inner := probe.Body
	if _, ok := top["object_key"]; !ok {
		if data, ok := top["data"]; ok {
			inner = string(data)
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
	if err := json.Unmarshal([]byte(inner), &parsed); err != nil {
		return "", "", nil, fmt.Errorf("decode instruction: %w", err)
	}
	if parsed.BaseResp.StatusCode != 0 {
		return "", "", nil, fmt.Errorf("base_resp %d %s", parsed.BaseResp.StatusCode, parsed.BaseResp.StatusMsg)
	}
	if parsed.ObjectKey == "" || parsed.Upload.URL == "" || len(parsed.Upload.FormFields) == 0 {
		return "", "", nil, fmt.Errorf("incomplete instruction: %s", firstLine(probe.Body, 400))
	}
	if !strings.EqualFold(parsed.Upload.Method, "POST") {
		return "", "", nil, fmt.Errorf("unexpected upload method %q", parsed.Upload.Method)
	}
	return parsed.ObjectKey, parsed.Upload.URL, parsed.Upload.FormFields, nil
}

// upload performs the multipart POST the policy describes. The file part is
// appended last, after every policy field, which is what the bundle does.
func upload(url string, fields map[string]string, data []byte) error {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	keys := sortedKeys(fields)
	for _, key := range keys {
		if err := writer.WriteField(key, fields[key]); err != nil {
			return err
		}
	}
	part, err := writer.CreateFormFile("file", "probe.png")
	if err != nil {
		return err
	}
	if _, err := part.Write(data); err != nil {
		return err
	}
	if err := writer.Close(); err != nil {
		return err
	}

	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	client := &http.Client{
		Timeout:       120 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 400))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(body))
	}
	return nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func hostOf(raw string) string {
	if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if j := strings.IndexAny(rest, "/?#"); j >= 0 {
			return rest[:j]
		}
		return rest
	}
	return raw
}

func mask(value string) string {
	if len(value) <= 12 {
		return value
	}
	return value[:8] + "…" + value[len(value)-4:]
}

func firstLine(value string, limit int) string {
	if i := strings.IndexByte(value, '\n'); i >= 0 {
		value = value[:i]
	}
	if len(value) > limit {
		return value[:limit] + "…"
	}
	return value
}

func snippet(raw []byte) string {
	return firstLine(string(raw), 400)
}

// dump keeps the newlines so a streamed turn reads as a turn rather than as one
// truncated line.
func dump(value string) string {
	if len(value) > 4000 {
		return value[:4000] + "…"
	}
	return value
}
