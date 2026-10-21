package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// These tests cover the model transport: the conversation has to arrive at the
// upstream as structured messages with real tool declarations, and the answer
// has to come back as OpenAI tool_calls. The point of the transport is that
// none of it goes through a prompt convention, so several of these assert on
// what is *absent* from the upstream request.

// directHarness is a harness switched onto the model transport.
func directHarness(t *testing.T, upstream http.HandlerFunc) *harness {
	t.Helper()
	h := newHarness(t, upstream)
	h.settings.Upstream.Mode = "direct"
	return h
}

// anthropicReplyJSON is a non-streaming answer in the protocol's shape.
func anthropicReplyJSON(content string, stopReason string) string {
	return `{"id":"m1","type":"message","role":"assistant","model":"MiniMax-M3",` +
		`"content":` + content + `,"stop_reason":"` + stopReason + `",` +
		`"usage":{"input_tokens":10,"output_tokens":4}}`
}

// anthropicStreamWithToolCall is the event sequence a tool-calling turn emits.
func anthropicStreamWithToolCall() string {
	frames := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"model":"MiniMax-M3","usage":{"input_tokens":10,"output_tokens":0}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"writing"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call_77","name":"Write"}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_path\":\"C:\\\\tmp\\\\a.txt\""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":",\"content\":\"hi\"}"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":1}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":9}}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
	return strings.Join(frames, "\n\n") + "\n\n"
}

// --- non-streaming ---------------------------------------------------------

func TestChatDirectReturnsNativeToolCalls(t *testing.T) {
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicReplyJSON(
			`[{"type":"text","text":"creating"},{"type":"tool_use","id":"call_9","name":"Write","input":{"file_path":"C:\\tmp\\a.txt","content":"hi"}}]`,
			"tool_use"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model": "minimax-m3",
		"messages": []map[string]any{
			{"role": "user", "content": "make a file"},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":       "Write",
				"parameters": map[string]any{"type": "object"},
			},
		}},
	}, h.key)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	body := decodeJSON(t, rec)
	choice := body["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	if message["content"] != "creating" {
		t.Fatalf("content = %v", message["content"])
	}
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls = %#v", calls)
	}
	call := calls[0].(map[string]any)
	if call["id"] != "call_9" || call["type"] != "function" {
		t.Fatalf("call = %#v", call)
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "Write" {
		t.Fatalf("name = %v", fn["name"])
	}
	// `arguments` is a JSON *string* on the OpenAI surface, and the object the
	// endpoint returned has to have been encoded rather than nested.
	args, ok := fn["arguments"].(string)
	if !ok {
		t.Fatalf("arguments is %T, want string", fn["arguments"])
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		t.Fatalf("arguments is not JSON: %v (%q)", err, args)
	}
	if parsed["file_path"] != `C:\tmp\a.txt` {
		t.Fatalf("arguments = %v", parsed)
	}
}

// The upstream must receive messages, tools and tool results as fields. This is
// the difference the whole transport exists for, so it is asserted against the
// request rather than against the response.
func TestChatDirectSendsStructuredMessagesAndTools(t *testing.T) {
	var received map[string]any
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicReplyJSON(`[{"type":"text","text":"ok"}]`, "end_turn"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model": "minimax-m3",
		"messages": []map[string]any{
			{"role": "system", "content": "You are ZCode."},
			{"role": "user", "content": "read the file"},
			{"role": "assistant", "content": "on it", "tool_calls": []map[string]any{{
				"id": "call_1", "type": "function",
				"function": map[string]any{"name": "Read", "arguments": `{"file_path":"/a"}`},
			}}},
			{"role": "tool", "tool_call_id": "call_1", "content": "file body"},
		},
		"tools": []map[string]any{{
			"type": "function",
			"function": map[string]any{
				"name":        "Read",
				"description": "Read a file",
				"parameters":  map[string]any{"type": "object", "properties": map[string]any{"file_path": map[string]any{"type": "string"}}},
			},
		}},
	}, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	if received["system"] != "You are ZCode." {
		t.Fatalf("system = %v", received["system"])
	}
	if received["model"] != "MiniMax-M3" {
		t.Fatalf("model = %v (the catalogue id must be translated to the upstream id)", received["model"])
	}

	messages := received["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3 (system is its own field): %#v", len(messages), messages)
	}

	user := messages[0].(map[string]any)
	if user["role"] != "user" {
		t.Fatalf("first message role = %v", user["role"])
	}

	assistant := messages[1].(map[string]any)
	blocks := assistant["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("assistant blocks = %#v", blocks)
	}
	call := blocks[1].(map[string]any)
	if call["type"] != "tool_use" || call["id"] != "call_1" || call["name"] != "Read" {
		t.Fatalf("tool_use block = %#v", call)
	}
	// `input` is an object in this protocol, not a JSON string.
	if _, ok := call["input"].(map[string]any); !ok {
		t.Fatalf("input = %#v, want an object", call["input"])
	}

	// A tool result belongs to a *user* turn here, carrying the id it answers.
	result := messages[2].(map[string]any)
	if result["role"] != "user" {
		t.Fatalf("tool result role = %v", result["role"])
	}
	resultBlock := result["content"].([]any)[0].(map[string]any)
	if resultBlock["type"] != "tool_result" || resultBlock["tool_use_id"] != "call_1" {
		t.Fatalf("tool_result block = %#v", resultBlock)
	}

	tools := received["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools = %#v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "Read" {
		t.Fatalf("tool = %#v", tool)
	}
	if _, ok := tool["input_schema"]; !ok {
		t.Fatalf("tool has no input_schema: %#v", tool)
	}

	// Nothing may be emulated: no `<tool_calls>` block, and no declaration
	// pasted into the prompt.
	raw, _ := json.Marshal(received)
	if strings.Contains(string(raw), toolOpenTag) {
		t.Fatalf("the model transport must not inject the tool prompt: %s", raw)
	}
}

// Consecutive tool results answering one assistant turn have to arrive in a
// single user message: the protocol rejects a split.
func TestChatDirectMergesConsecutiveToolResults(t *testing.T) {
	var received map[string]any
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicReplyJSON(`[{"type":"text","text":"ok"}]`, "end_turn"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model": "minimax-m3",
		"messages": []map[string]any{
			{"role": "user", "content": "do both"},
			{"role": "assistant", "content": "", "tool_calls": []map[string]any{
				{"id": "call_a", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{}`}},
				{"id": "call_b", "type": "function", "function": map[string]any{"name": "Read", "arguments": `{}`}},
			}},
			{"role": "tool", "tool_call_id": "call_a", "content": "A"},
			{"role": "tool", "tool_call_id": "call_b", "content": "B"},
		},
	}, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	messages := received["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3: %#v", len(messages), messages)
	}
	blocks := messages[2].(map[string]any)["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("the two results must share one turn, got %#v", blocks)
	}
	if blocks[0].(map[string]any)["tool_use_id"] != "call_a" || blocks[1].(map[string]any)["tool_use_id"] != "call_b" {
		t.Fatalf("blocks = %#v", blocks)
	}
}

// --- streaming -------------------------------------------------------------

func TestChatDirectStreamsToolCalls(t *testing.T) {
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, anthropicStreamWithToolCall())
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model":    "minimax-m3",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "make a file"}},
	}, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	var text strings.Builder
	var calls []map[string]any
	var finish string
	for _, payload := range sseDataLines(t, rec) {
		if payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("chunk is not JSON: %v (%q)", err, payload)
		}
		choice := chunk["choices"].([]any)[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			text.WriteString(content)
		}
		if raw, ok := delta["tool_calls"].([]any); ok {
			for _, item := range raw {
				calls = append(calls, item.(map[string]any))
			}
		}
		if choice["finish_reason"] != nil {
			finish, _ = choice["finish_reason"].(string)
		}
	}

	if text.String() != "writing" {
		t.Fatalf("streamed text = %q", text.String())
	}
	if finish != "tool_calls" {
		t.Fatalf("finish_reason = %q", finish)
	}
	if len(calls) != 1 {
		t.Fatalf("streamed calls = %#v", calls)
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "Write" {
		t.Fatalf("name = %v", fn["name"])
	}
	// The fragments have to have been joined before the delta was emitted.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(fn["arguments"].(string)), &parsed); err != nil {
		t.Fatalf("arguments is not JSON: %v (%q)", err, fn["arguments"])
	}
	if parsed["content"] != "hi" {
		t.Fatalf("arguments = %v", parsed)
	}
}

// --- anthropic front end ---------------------------------------------------

func anthropicPost(t *testing.T, h *harness, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+h.key)
	rec := httptest.NewRecorder()
	h.gateway.AnthropicMessages(rec, req)
	return rec
}

// The Anthropic front end is where the two protocols coincide, so a tool_use
// block has to survive the round trip unchanged.
func TestAnthropicDirectKeepsToolUseBlocks(t *testing.T) {
	var received map[string]any
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicReplyJSON(
			`[{"type":"text","text":"ok"},{"type":"tool_use","id":"call_3","name":"Read","input":{"file_path":"/b"}}]`,
			"tool_use"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := anthropicPost(t, h, map[string]any{
		"model":      "minimax-m3",
		"max_tokens": 1024,
		"system":     "You are ZCode.",
		"messages": []map[string]any{
			{"role": "user", "content": "read the file"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "thinking", "thinking": "prior reasoning"},
				{"type": "tool_use", "id": "call_2", "name": "Read", "input": map[string]any{"file_path": "/a"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "call_2", "content": "file body"},
			}},
		},
		"tools": []map[string]any{{
			"name": "Read", "description": "Read a file",
			"input_schema": map[string]any{"type": "object"},
		}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}

	// Upstream: the thinking block is dropped, everything else survives.
	messages := received["messages"].([]any)
	assistantBlocks := messages[1].(map[string]any)["content"].([]any)
	if len(assistantBlocks) != 1 {
		t.Fatalf("prior reasoning must not be replayed: %#v", assistantBlocks)
	}
	if assistantBlocks[0].(map[string]any)["type"] != "tool_use" {
		t.Fatalf("blocks = %#v", assistantBlocks)
	}
	resultBlock := messages[2].(map[string]any)["content"].([]any)[0].(map[string]any)
	if resultBlock["type"] != "tool_result" || resultBlock["tool_use_id"] != "call_2" {
		t.Fatalf("tool_result = %#v", resultBlock)
	}

	// Back to the client: an Anthropic response with an Anthropic tool_use.
	var response map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if response["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", response["stop_reason"])
	}
	blocks := response["content"].([]any)
	var toolUse map[string]any
	for _, block := range blocks {
		if block.(map[string]any)["type"] == "tool_use" {
			toolUse = block.(map[string]any)
		}
	}
	if toolUse == nil {
		t.Fatalf("no tool_use in %#v", blocks)
	}
	if toolUse["id"] != "call_3" || toolUse["name"] != "Read" {
		t.Fatalf("tool_use = %#v", toolUse)
	}
	if _, ok := toolUse["input"].(map[string]any); !ok {
		t.Fatalf("input = %#v, want an object", toolUse["input"])
	}
}

// --- the agent transport is untouched --------------------------------------

// The two transports are selected, not merged: switching to direct must not
// change what the agent path sends, and staying on agent must not acquire any
// of the direct behaviour.
func TestAgentModeStillEmulatesTools(t *testing.T) {
	var received map[string]any
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		// The agent transport runs a session handshake before the turn, so the
		// stub has to answer both paths — only the message call carries the
		// prompt under test.
		if !strings.HasSuffix(r.URL.Path, "/message") {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"session_id":"sess_test"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &received)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, upstreamText("ok"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model":    "minimax-m3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "Read", "parameters": map[string]any{"type": "object"}},
		}},
	}, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	// The agent transport has no tools field, so the declaration travels in the
	// turn text. That is the behaviour the direct transport exists to avoid, and
	// it has to still be there for anyone who has not switched.
	content, _ := received["content"].(string)
	if !strings.Contains(content, toolOpenTag) {
		t.Fatalf("the agent transport should still inject the tool prompt: %q", content)
	}
	if _, ok := received["tools"]; ok {
		t.Fatalf("the agent transport has no tools field: %#v", received)
	}
}

// A client that never asked for tools still gets plain chat on both transports.
func TestDirectModePlainChat(t *testing.T) {
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		_, _ = io.WriteString(w, anthropicReplyJSON(`[{"type":"text","text":"hello"}]`, "end_turn"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model":    "minimax-m3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	choice := decodeJSON(t, rec)["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	if choice["message"].(map[string]any)["content"] != "hello" {
		t.Fatalf("message = %#v", choice["message"])
	}
}

// Switching chat traffic to the model endpoint must not strand the plugin
// routes: image and video turns live in the agent's plugin layer, which the
// model endpoint does not have. A video request therefore keeps reaching the
// agent even on an install whose chat traffic has moved.
func TestDirectModeKeepsVideoOnTheAgentTransport(t *testing.T) {
	var messageBody map[string]any
	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/message") {
			w.Header().Set("content-type", "application/json")
			_, _ = io.WriteString(w, `{"session_id":"sess_test"}`)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &messageBody)
		w.Header().Set("content-type", "text/event-stream")
		_, _ = io.WriteString(w, upstreamText("queued"))
	})
	h.addAccount(t, "acct", "tok", 50)

	rec := h.chat(t, map[string]any{
		"model":    "minimax-h3",
		"messages": []map[string]any{{"role": "user", "content": "做一只猫在草地上跑"}},
	}, h.key)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if messageBody == nil {
		t.Fatal("a video turn must reach the agent conversation endpoint, not the model endpoint")
	}
	content, _ := messageBody["content"].(string)
	if !strings.Contains(content, "@"+h.settings.Video.PluginName) {
		t.Fatalf("the video turn must still be rewritten into a plugin reference: %q", content)
	}
}
