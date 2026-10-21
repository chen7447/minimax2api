package minimax

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"minimax2api/internal/config"
)

// directTestClient points a client at a stub upstream.
func directTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	settings := config.DefaultSettings(t.TempDir())
	settings.Upstream.BaseURL = server.URL
	return New(func() config.Settings { return settings }), server
}

// --- request shape ---------------------------------------------------------

// The two transports are different protocols, and the difference is the point:
// the agent transport signs every call, while the model endpoint authenticates
// with a bearer token and nothing else. Sending the signature headers here
// would work, but it would also mean the gateway had stopped distinguishing
// them.
func TestDirectRequestCarriesBearerAndNoSignature(t *testing.T) {
	var (
		gotAuth      string
		gotQuery     string
		gotSignature string
		gotYY        string
		gotPath      string
		gotBody      map[string]any
	)
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotSignature = r.Header.Get("x-signature")
		gotYY = r.Header.Get("yy")
		gotQuery = r.URL.RawQuery
		gotPath = r.URL.Path
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	})

	_, err := client.Messages(context.Background(), DirectOptions{
		Credential:    Credential{Token: "tok-123"},
		Model:         "MiniMax-M3",
		Messages:      []DirectMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		DisableStream: true,
	})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	if gotAuth != "Bearer tok-123" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if gotSignature != "" || gotYY != "" {
		t.Fatalf("signature headers must not be sent: x-signature=%q yy=%q", gotSignature, gotYY)
	}
	if gotQuery != "" {
		t.Fatalf("the model endpoint takes no metadata query, got %q", gotQuery)
	}
	if gotPath != DefaultModelPath {
		t.Fatalf("path = %q, want %q", gotPath, DefaultModelPath)
	}
	if gotBody["model"] != "MiniMax-M3" {
		t.Fatalf("model = %v", gotBody["model"])
	}
	// max_tokens is required by the protocol and has no server-side default, so
	// an omitted limit has to become a real number rather than being dropped.
	if gotBody["max_tokens"] != float64(DefaultDirectMaxTokens) {
		t.Fatalf("max_tokens = %v", gotBody["max_tokens"])
	}
}

// A tool declaration has to travel as a field, not inside the prompt. This is
// the whole reason the direct transport exists.
func TestDirectRequestCarriesToolsAsAField(t *testing.T) {
	var gotBody map[string]any
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &gotBody)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn"}`)
	})

	_, err := client.Messages(context.Background(), DirectOptions{
		Credential: Credential{Token: "tok"},
		Model:      "MiniMax-M3",
		Messages:   []DirectMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []DirectTool{{
			Name:        "Write",
			Description: "Write a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"}}}`),
		}},
		DisableStream: true,
	})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", gotBody["tools"])
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "Write" {
		t.Fatalf("tool name = %v", first["name"])
	}
	// Anthropic spells the schema `input_schema`; OpenAI nests it under
	// `function.parameters`. Only one of those is understood upstream.
	if _, ok := first["input_schema"]; !ok {
		t.Fatalf("tool has no input_schema: %#v", first)
	}
}

// --- streaming -------------------------------------------------------------

// directStreamBody is the event sequence a real turn produces: a text block,
// then a tool_use block whose arguments arrive as fragments.
func directStreamBody() string {
	frames := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"model":"MiniMax-M3","usage":{"input_tokens":12,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"I'll create "}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"the file."}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_1","name":"Write"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"C:\\\\tmp"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\\\\a.txt\",\"content\":\"hi\"}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":21}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	return strings.Join(frames, "\n\n") + "\n\n"
}

func TestDirectStreamAssemblesTextAndToolCall(t *testing.T) {
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, directStreamBody())
	})

	var streamed string
	result, err := client.Messages(context.Background(), DirectOptions{
		Credential: Credential{Token: "tok"},
		Model:      "MiniMax-M3",
		Messages:   []DirectMessage{{Role: "user", Content: json.RawMessage(`"make a file"`)}},
		OnDelta:    func(text string) { streamed += text },
	})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}

	if result.Text != "I'll create the file." {
		t.Fatalf("text = %q", result.Text)
	}
	if streamed != result.Text {
		t.Fatalf("streamed %q, aggregated %q", streamed, result.Text)
	}
	if result.StopReason != "tool_use" {
		t.Fatalf("stop reason = %q", result.StopReason)
	}
	if result.Model != "MiniMax-M3" {
		t.Fatalf("model = %q", result.Model)
	}
	if result.InputTokens != 12 || result.OutputTokens != 21 {
		t.Fatalf("usage = %d/%d", result.InputTokens, result.OutputTokens)
	}
	if len(result.Calls) != 1 {
		t.Fatalf("calls = %#v", result.Calls)
	}
	call := result.Calls[0]
	if call.ID != "call_1" || call.Name != "Write" {
		t.Fatalf("call = %#v", call)
	}
	// The fragments have to be joined before the arguments mean anything.
	if call.Arguments != `{"file_path":"C:\\tmp\\a.txt","content":"hi"}` {
		t.Fatalf("arguments = %q", call.Arguments)
	}
}

func TestDirectStreamSplitsReasoningFromAnswer(t *testing.T) {
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, strings.Join([]string{
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"让我想"}}`,
			`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
			`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答案"}}`,
		}, "\n\n")+"\n\n")
	})

	var thinking, text string
	result, err := client.Messages(context.Background(), DirectOptions{
		Credential: Credential{Token: "tok"},
		Model:      "MiniMax-M3",
		Messages:   []DirectMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		OnDelta:    func(s string) { text += s },
		OnThinking: func(s string) { thinking += s },
	})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if result.Thinking != "让我想" || thinking != "让我想" {
		t.Fatalf("thinking = %q / %q", result.Thinking, thinking)
	}
	if result.Text != "答案" || text != "答案" {
		t.Fatalf("text = %q / %q", result.Text, text)
	}
}

// A stream error has to surface as an error rather than as an empty answer.
func TestDirectStreamSurfacesErrorEvent(t *testing.T) {
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, `event: error`+"\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"50110:insufficient balance (1008)"}}`+"\n\n")
	})

	_, err := client.Messages(context.Background(), DirectOptions{
		Credential: Credential{Token: "tok"},
		Model:      "MiniMax-M3",
		Messages:   []DirectMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
	})
	if err == nil {
		t.Fatal("an error event must not be swallowed")
	}
	if !strings.Contains(err.Error(), "50110") {
		t.Fatalf("error = %v", err)
	}
}

// --- non-streaming ---------------------------------------------------------

func TestMessagesParsesNonStreamingResponse(t *testing.T) {
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"MiniMax-M3",`+
			`"content":[{"type":"text","text":"done"},{"type":"tool_use","id":"call_9","name":"Read","input":{"file_path":"/a"}}],`+
			`"stop_reason":"tool_use","usage":{"input_tokens":5,"output_tokens":7}}`)
	})

	var streamed string
	result, err := client.Messages(context.Background(), DirectOptions{
		Credential:    Credential{Token: "tok"},
		Model:         "MiniMax-M3",
		Messages:      []DirectMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		OnDelta:       func(s string) { streamed += s },
		DisableStream: true,
	})
	if err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if result.Text != "done" || streamed != "done" {
		t.Fatalf("text = %q / streamed %q", result.Text, streamed)
	}
	if len(result.Calls) != 1 || result.Calls[0].Name != "Read" {
		t.Fatalf("calls = %#v", result.Calls)
	}
	if result.Calls[0].Arguments != `{"file_path":"/a"}` {
		t.Fatalf("arguments = %q", result.Calls[0].Arguments)
	}
	if result.OutputTokens != 7 {
		t.Fatalf("output tokens = %d", result.OutputTokens)
	}
}

// A 401 is a credential verdict, not a generic failure: the pool retires an
// account that reports one.
func TestMessagesReportsRejectedCredential(t *testing.T) {
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"message":"invalid access token"}}`)
	})
	_, err := client.Messages(context.Background(), DirectOptions{
		Credential:    Credential{Token: "bad"},
		Model:         "MiniMax-M3",
		Messages:      []DirectMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		DisableStream: true,
	})
	if err != ErrInvalidCredential {
		t.Fatalf("err = %v, want ErrInvalidCredential", err)
	}
}

// --- helpers ---------------------------------------------------------------

// A truncated argument payload must not be handed downstream as invalid JSON:
// a caller that cannot parse arguments fails the whole turn, while an empty
// object at least preserves which tool was requested.
func TestNormaliseArgumentsRepairsUnusablePayloads(t *testing.T) {
	cases := map[string]string{
		"":                "{}",
		"   ":             "{}",
		`{"a":1}`:         `{"a":1}`,
		`{"a":`:           "{}",
		`not json at all`: "{}",
		// Valid JSON that is not an object is kept rather than discarded: it is
		// what the model produced, and re-encoding it as a JSON string still
		// gives the caller something parseable.
		`"hi"`: `"\"hi\""`,
	}
	for input, want := range cases {
		if got := normaliseArguments(input); got != want {
			t.Errorf("normaliseArguments(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestParseModelIDsReadsTheProviderRegistry(t *testing.T) {
	raw := []byte(`{"version":"1.0","providers":[{"providerId":"minimax","config":{"models":{
		"MiniMax-M3":{"tool_call":true},"MiniMax-M2.7":{"tool_call":true}}}}]}`)
	ids := parseModelIDs(raw)
	if len(ids) != 2 {
		t.Fatalf("ids = %#v", ids)
	}
	seen := map[string]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	for _, want := range []string{"MiniMax-M3", "MiniMax-M2.7"} {
		if !seen[want] {
			t.Errorf("missing %q in %#v", want, ids)
		}
	}
}

func TestModelsListsIDsOverBearerAuth(t *testing.T) {
	var gotAuth string
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		if r.URL.Path != DefaultModelsPath {
			t.Errorf("path = %q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"providers":[{"config":{"models":{"MiniMax-M3":{}}}}]}`)
	})
	ids, err := client.Models(context.Background(), Credential{Token: "tok"})
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("authorization = %q", gotAuth)
	}
	if len(ids) != 1 || ids[0] != "MiniMax-M3" {
		t.Fatalf("ids = %#v", ids)
	}
}

// The registry endpoint is public, which is what lets the console refresh the
// catalogue with an empty pool. Sending no Authorization header is part of that
// contract: a credential is not merely unnecessary, it is absent.
func TestPublicModelsSendsNoAuthorization(t *testing.T) {
	var (
		gotAuth string
		gotPath string
	)
	client, _ := directTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("authorization")
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"providers":[{"config":{"models":{"MiniMax-M3.1-Flash":{},"MiniMax-M3":{}},"whitelist":["MiniMax-M2.7","MiniMax-M3"],"model_order":["MiniMax-M3.1-Flash","MiniMax-M3"]}}]}`)
	})
	ids, err := client.PublicModels(context.Background())
	if err != nil {
		t.Fatalf("PublicModels: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("authorization = %q, want no header", gotAuth)
	}
	if gotPath != DefaultModelsPath {
		t.Errorf("path = %q, want %q", gotPath, DefaultModelsPath)
	}
	// The upstream's own order leads, the whitelist-only id (M2.7) is still
	// accepted and must not look unlisted, and the tail is deterministic.
	want := []string{"MiniMax-M3.1-Flash", "MiniMax-M3", "MiniMax-M2.7"}
	if len(ids) != len(want) {
		t.Fatalf("ids = %#v, want %#v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %#v, want %#v", ids, want)
		}
	}
}

// The SSE reader is a real parser, not a split on blank lines: a comment line
// is a keep-alive and carries no payload.
func TestSSEReaderSkipsCommentsAndJoinsDataLines(t *testing.T) {
	body := io.NopCloser(strings.NewReader(
		": keep-alive\n\n" +
			"event: ping\ndata: {\"a\":1}\n\n" +
			"data: one\ndata: two\n\n"))
	reader := newSSEReader(body, 0)
	defer reader.stop()

	// The keep-alive block carries nothing and is skipped outright, so the first
	// event returned is the one that follows it.
	event, data, err := reader.next()
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if event != "ping" || data != `{"a":1}` {
		t.Fatalf("first event = %q/%q", event, data)
	}

	event, data, err = reader.next()
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if data != "onetwo" {
		t.Fatalf("repeated data lines must be joined, got %q", data)
	}

	if _, _, err := reader.next(); err != io.EOF {
		t.Fatalf("err = %v, want io.EOF", err)
	}
}

// A stream cut off mid-turn still has to yield what arrived.
func TestSSEReaderReturnsTrailingFrameWithoutBlankLine(t *testing.T) {
	body := io.NopCloser(strings.NewReader(`data: {"a":1}`))
	reader := newSSEReader(body, 0)
	defer reader.stop()
	_, data, err := reader.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if data != `{"a":1}` {
		t.Fatalf("data = %q", data)
	}
}

func TestParseDirectMessageReportsUpstreamError(t *testing.T) {
	_, err := parseDirectMessage([]byte(`{"error":{"type":"invalid_request_error","message":"bad model"}}`))
	if err == nil || !strings.Contains(err.Error(), "bad model") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseDirectMessageRejectsGarbage(t *testing.T) {
	if _, err := parseDirectMessage([]byte("<html>")); err == nil {
		t.Fatal("a non-JSON body must not be read as a message")
	}
}
