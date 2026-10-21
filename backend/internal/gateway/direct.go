package gateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"minimax2api/internal/minimax"
	"minimax2api/internal/pool"
	"minimax2api/internal/store"
)

// The direct model transport.
//
// Everything in this file exists because the agent conversation endpoint is not
// a model surface. It answers as MiniMax's own cloud agent — its own persona,
// its own sandbox, its own memory — and its request body has no field that can
// carry a tool declaration, which is why tools.go has to emulate function
// calling with a prompt convention and a parser.
//
// The model endpoint the official client uses (`/mavis/api/v1/llm/v1/messages`)
// does not have any of those problems: it speaks Anthropic Messages, so a tool
// declaration is a field, a tool call is a `tool_use` block, and a tool result
// is a `tool_result` block. There is no session, no echo and no restatement, so
// the whole "flatten the conversation into one string" step disappears.
//
// What this file therefore is, is a translation between two protocols that are
// genuinely different — OpenAI Chat Completions on the outside, Anthropic
// Messages on the inside — rather than a shim over a black box. The mapping is
// deliberately lossless in both directions: a `tool_calls` array in, a
// `tool_use` block out, and back again.
//
// See internal/minimax/direct.go for the transport itself and
// config.UpstreamSettings.Mode for the switch.

// directModelFallback maps catalogue ids onto upstream model ids for entries
// whose own UpstreamModel is empty.
//
// An install that predates the model selector has these entries stored with an
// empty UpstreamModel, and the stored catalogue is not rewritten by an upgrade
// (MergeBuiltinModels only adds ids that are missing). Reading the fallback
// here rather than migrating the catalogue keeps one table to reason about
// instead of a table plus a migration.
var directModelFallback = map[string]string{
	"minimax-agent":              "MiniMax-M3",
	"minimax-m3-thinking":        "MiniMax-M3",
	"minimax-m3.1-flash":         "MiniMax-M3.1-Flash",
	"minimax-m3.1-flash-preview": "MiniMax-M3.1-Flash-Preview",
	"minimax-m3":                 "MiniMax-M3",
	"minimax-m2.7":               "MiniMax-M2.7",
	"minimax-m2.7-highspeed":     "MiniMax-M2.7-highspeed",
}

// defaultDirectModel is the model a turn falls back to when nothing names one.
// It is the entry every account is guaranteed to have.
const defaultDirectModel = "MiniMax-M3"

// directMode reports whether chat turns should use the model endpoint.
func (g *Gateway) directMode() bool {
	return g.settings().UsesDirectModelAPI()
}

// directModelID is the upstream id a catalogue entry maps to.
func directModelID(model *store.ModelConfig) string {
	if model == nil {
		return defaultDirectModel
	}
	if id := strings.TrimSpace(model.UpstreamModel); id != "" {
		return id
	}
	if id, ok := directModelFallback[strings.ToLower(strings.TrimSpace(model.ID))]; ok {
		return id
	}
	return defaultDirectModel
}

// ------------------------------------------------------------- openai → direct

// openAIToolCall is an assistant-side call as OpenAI reports it.
type openAIToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// directFromOpenAI renders an OpenAI conversation as Anthropic messages.
//
// The two protocols disagree about where a system prompt lives (its own field
// rather than a message), about who owns a tool result (a `tool` message rather
// than a `user` turn), and about whether content is a string or a list of
// blocks. All three are handled here, once, rather than at each call site.
func directFromOpenAI(messages []chatMessage) (string, []minimax.DirectMessage, error) {
	var system strings.Builder
	out := make([]minimax.DirectMessage, 0, len(messages))

	for _, message := range messages {
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "system", "developer":
			text, _ := parseContent(message.Content)
			if text != "" {
				if system.Len() > 0 {
					system.WriteString("\n\n")
				}
				system.WriteString(text)
			}

		case "tool", "function":
			// A tool result belongs to a *user* turn in Anthropic, and every
			// result answering one assistant turn has to travel in the same
			// message — the protocol rejects a split. OpenAI clients send one
			// message per result, so consecutive results are folded together.
			text, _ := parseContent(message.Content)
			out = appendToolResult(out, map[string]any{
				"type":        "tool_result",
				"tool_use_id": message.ToolCallID,
				"content":     text,
			})

		case "assistant":
			blocks := make([]map[string]any, 0, 1+len(message.ToolCalls))
			if text, _ := parseContent(message.Content); text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": text})
			}
			for _, call := range message.ToolCalls {
				name := strings.TrimSpace(call.Function.Name)
				if name == "" {
					continue
				}
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    callID(call),
					"name":  name,
					"input": rawJSONObject(call.Function.Arguments),
				})
			}
			// An assistant turn with neither text nor calls is not a turn. The
			// protocol rejects an empty content list, and sending one would fail
			// the whole request over a message that says nothing.
			if len(blocks) > 0 {
				out = append(out, directMessage("assistant", blocks))
			}

		default:
			blocks, err := directUserBlocks(message.Content)
			if err != nil {
				return "", nil, err
			}
			if len(blocks) > 0 {
				out = append(out, directMessage("user", blocks))
			}
		}
	}
	return strings.TrimSpace(system.String()), out, nil
}

// callID keeps a call addressable. A client that omitted the id (which the
// protocol requires, so this is defensive) still gets one, because a
// tool_result without a matching tool_use_id is rejected outright.
func callID(call openAIToolCall) string {
	if id := strings.TrimSpace(call.ID); id != "" {
		return id
	}
	return "call_" + randomID(24)
}

// directUserBlocks renders one user turn, images included.
//
// Images travel inline as base64 blocks. The agent transport has to upload them
// first and attach an account-owned object key; this endpoint takes the bytes
// directly, which is one fewer moving part and one fewer account-bound step.
func directUserBlocks(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var plain string
	if err := json.Unmarshal(raw, &plain); err == nil {
		if plain == "" {
			return nil, nil
		}
		return []map[string]any{{"type": "text", "text": plain}}, nil
	}

	var parts []contentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, nil
	}
	blocks := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch part.Type {
		case "text", "input_text":
			if part.Text != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			}
		case "image_url", "input_image":
			block, err := directImageBlock(part.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, block)
		}
	}
	return blocks, nil
}

// directImageBlock turns an OpenAI image reference into an Anthropic one.
//
// A data URI is decoded and sent as base64. A remote URL is passed through as a
// url source, which the protocol allows — but note that the *agent* transport
// refuses remote URLs because the upstream stopped fetching them, and whether
// this endpoint does the same has not been measured. If it does, the failure is
// a clear upstream error naming the image rather than a silent drop.
func directImageBlock(raw string) (map[string]any, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, fmt.Errorf("empty image reference")
	}
	if strings.HasPrefix(raw, "data:") {
		index := strings.Index(raw, ",")
		if index < 0 {
			return nil, fmt.Errorf("invalid data URI")
		}
		header := raw[:index]
		mime := strings.TrimSpace(strings.TrimPrefix(strings.SplitN(header, ";", 2)[0], "data:"))
		if mime == "" {
			mime = "image/png"
		}
		data := strings.TrimSpace(raw[index+1:])
		if _, err := base64.StdEncoding.DecodeString(data); err != nil {
			return nil, fmt.Errorf("invalid base64 image payload: %w", err)
		}
		return map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "base64", "media_type": mime, "data": data},
		}, nil
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		return map[string]any{
			"type":   "image",
			"source": map[string]any{"type": "url", "url": raw},
		}, nil
	}
	return nil, fmt.Errorf("unsupported image reference: expected a data: URI or an http(s) URL")
}

// appendToolResult folds a tool result into the trailing user turn when that
// turn is nothing but tool results, and starts a new turn otherwise.
func appendToolResult(out []minimax.DirectMessage, block map[string]any) []minimax.DirectMessage {
	if len(out) > 0 {
		last := &out[len(out)-1]
		if last.Role == "user" {
			var blocks []map[string]any
			if err := json.Unmarshal(last.Content, &blocks); err == nil && allToolResults(blocks) {
				last.Content = mustJSON(append(blocks, block))
				return out
			}
		}
	}
	return append(out, directMessage("user", []map[string]any{block}))
}

func allToolResults(blocks []map[string]any) bool {
	if len(blocks) == 0 {
		return false
	}
	for _, block := range blocks {
		if kind, _ := block["type"].(string); kind != "tool_result" {
			return false
		}
	}
	return true
}

func directMessage(role string, blocks []map[string]any) minimax.DirectMessage {
	return minimax.DirectMessage{Role: role, Content: mustJSON(blocks)}
}

func mustJSON(value any) json.RawMessage {
	raw, err := json.Marshal(value)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

// directTools converts OpenAI declarations to Anthropic ones.
//
// A declaration with no usable schema still travels: the protocol requires
// `input_schema`, and a tool with no arguments is better expressed as an empty
// object schema than dropped from the list.
func directTools(tools []toolSpec) []minimax.DirectTool {
	out := make([]minimax.DirectTool, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Function.Name)
		if name == "" {
			continue
		}
		schema := tool.Function.Parameters
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, minimax.DirectTool{
			Name:        name,
			Description: tool.Function.Description,
			InputSchema: schema,
		})
	}
	return out
}

// ----------------------------------------------------------------- direct → openai

// openAIToolCalls renders upstream calls in the OpenAI wire shape.
func openAIToolCalls(calls []minimax.DirectCall) []toolCall {
	out := make([]toolCall, 0, len(calls))
	for _, call := range calls {
		name := strings.TrimSpace(call.Name)
		if name == "" {
			continue
		}
		id := strings.TrimSpace(call.ID)
		if id == "" {
			id = "call_" + randomID(24)
		}
		out = append(out, toolCall{ID: id, Name: name, Arguments: call.Arguments})
	}
	return out
}

// --------------------------------------------------------------------- the turn

// directTurnRequest is one conversation handed to the model transport.
type directTurnRequest struct {
	System     string
	Messages   []minimax.DirectMessage
	Tools      []minimax.DirectTool
	Model      *store.ModelConfig
	MaxTokens  int
	SessionKey string
	// Group is the client key's pool ("" = personal, "gongyi" = public).
	Group           string
	DisableThinking bool
	Started         time.Time
	// Stream mirrors what the caller asked for. Unlike the agent transport —
	// which only ever has a stream to read — this endpoint offers both, so a
	// client that did not ask for streaming gets a single JSON response instead
	// of an SSE body that has to be reassembled.
	Stream     bool
	OnDelta    func(string)
	OnThinking func(string)
	Committed  func() bool
}

type directTurnResult struct {
	Result      *minimax.DirectResult
	AccountName string
	FirstToken  int64
	Retries     int
	Streamed    bool
}

// runDirectTurn leases an account, calls the model endpoint, and retries across
// accounts while nothing has been committed to the client.
//
// It mirrors runTurn deliberately rather than sharing code with it: the two
// transports have different request types and different failure signals, and
// the parts worth sharing — failover, cooldown accounting, the committed
// boundary — are a dozen lines that would need generics to share honestly. What
// must not drift is the *behaviour*, and that is what the parallel structure
// makes visible.
func (g *Gateway) runDirectTurn(ctx context.Context, req directTurnRequest) (*directTurnResult, error) {
	settings := g.settings()
	out := &directTurnResult{}

	var (
		mu         sync.Mutex
		firstToken int64
		streamed   bool
	)
	markFirst := func() {
		mu.Lock()
		defer mu.Unlock()
		if firstToken == 0 {
			firstToken = time.Since(req.Started).Milliseconds()
		}
		streamed = true
	}

	var lastErr error
	for attempt := 0; attempt < settings.Routing.MaxAttempts; attempt++ {
		lease, err := g.pool.Acquire(ctx, req.SessionKey, req.Group)
		if err != nil {
			return out, err
		}
		out.AccountName = displayName(lease.Account)
		if attempt > 0 {
			out.Retries = attempt
		}

		opts := minimax.DirectOptions{
			Credential:      CredentialOf(lease.Account),
			Model:           directModelID(req.Model),
			System:          req.System,
			Messages:        req.Messages,
			Tools:           req.Tools,
			MaxTokens:       req.MaxTokens,
			Timeout:         timeoutFor(settings, req.Model),
			IdleTimeout:     settings.StreamIdleTimeout(),
			DisableThinking: req.DisableThinking,
			DisableStream:   !req.Stream,
		}
		if req.OnDelta != nil {
			opts.OnDelta = func(text string) {
				markFirst()
				req.OnDelta(text)
			}
		}
		if req.OnThinking != nil {
			opts.OnThinking = func(text string) {
				markFirst()
				req.OnThinking(text)
			}
		}

		result, err := g.client.Messages(ctx, opts)
		// A drained balance arrives as a 200 whose body is the refusal, exactly
		// as it does on the agent transport — so it has to be read out of the
		// content here too, or the pool keeps routing to an empty account.
		if err == nil && result != nil && quotaDrainedAnswer(result.Text) {
			err = pool.ErrQuotaDrained
		}
		// The model endpoint can also answer with a real HTTP 402; both forms
		// are the same pool verdict — the account's balance is spent.
		if errors.Is(err, minimax.ErrInsufficientBalance) {
			err = pool.ErrQuotaDrained
		}
		lease.Release(err)

		lastErr = err
		if err == nil {
			mu.Lock()
			out.FirstToken, out.Streamed = firstToken, streamed
			mu.Unlock()
			if out.FirstToken == 0 {
				out.FirstToken = time.Since(req.Started).Milliseconds()
			}
			out.Result = result
			return out, nil
		}
		if ctx.Err() != nil || isPoolExhausted(err) {
			break
		}
		if req.Committed != nil && req.Committed() {
			break
		}
	}

	mu.Lock()
	out.FirstToken, out.Streamed = firstToken, streamed
	mu.Unlock()
	return out, lastErr
}

// ------------------------------------------------------------- chat completion

// chatDirect implements POST /v1/chat/completions on the model transport.
func (g *Gateway) chatDirect(
	out *respWriter,
	r *http.Request,
	key *store.ClientKey,
	model *store.ModelConfig,
	request chatRequest,
	rawBody []byte,
	started time.Time,
) {
	system, messages, err := directFromOpenAI(request.Messages)
	if err != nil {
		writeError(out, http.StatusBadRequest, err.Error())
		return
	}
	if len(messages) == 0 {
		writeError(out, http.StatusBadRequest, "messages must contain text or image content")
		return
	}
	tools := directTools(request.Tools)

	var chunkMu sync.Mutex
	writeChunk := func(delta map[string]any, finish any) {
		chunkMu.Lock()
		defer chunkMu.Unlock()
		ensureStreamHeaders(out)
		writeChunkRaw(out, model, delta, finish)
		out.Flush()
	}

	var committed bool
	var onDelta, onThinking func(string)
	if request.Stream {
		onDelta = func(text string) {
			committed = true
			writeChunk(map[string]any{"content": text}, nil)
		}
		onThinking = func(text string) {
			committed = true
			writeChunk(map[string]any{"reasoning_content": text}, nil)
		}
		writeChunk(map[string]any{"role": "assistant", "content": ""}, nil)
	}

	turn, lastErr := g.runDirectTurn(r.Context(), directTurnRequest{
		System:          system,
		Messages:        messages,
		Tools:           tools,
		Model:           model,
		MaxTokens:       request.maxTokens(),
		SessionKey:      request.User,
		Group:           key.Pool,
		DisableThinking: key.Pool == "gongyi",
		Started:         started,
		Stream:          request.Stream,
		OnDelta:         onDelta,
		OnThinking:      onThinking,
		Committed:       func() bool { return committed },
	})

	promptTokens := estimateTokens(system + conversationText(messages))
	if lastErr != nil {
		g.recordAudit(r, key, model, turn.AccountName, started, http.StatusBadGateway, 0, promptTokens, 0, request.Stream, turn.Retries, string(rawBody), "", lastErr.Error())
		if out.wroteHeader {
			writeSSEError(out, lastErr.Error())
			return
		}
		writeError(out, http.StatusBadGateway, lastErr.Error())
		return
	}
	result := turn.Result
	if result == nil {
		writeError(out, http.StatusBadGateway, "upstream returned no result")
		return
	}

	calls := openAIToolCalls(result.Calls)
	completionTokens := result.OutputTokens
	if completionTokens <= 0 {
		completionTokens = estimateTokens(result.Text + result.Thinking)
	}
	g.store.RecordModelUsage(model.ID, 1, int64(promptTokens+completionTokens))
	g.store.BumpClientKeyUsage(key.ID)

	if request.Stream {
		if !turn.Streamed {
			if result.Thinking != "" {
				writeChunk(map[string]any{"reasoning_content": result.Thinking}, nil)
			}
			if result.Text != "" {
				writeChunk(map[string]any{"content": result.Text}, nil)
			}
		}
		if len(calls) > 0 {
			writeChunk(map[string]any{"tool_calls": streamedToolCallDeltas(calls)}, "tool_calls")
		} else {
			writeChunk(map[string]any{}, "stop")
		}
		_, _ = io.WriteString(out, "data: [DONE]\n\n")
		out.Flush()
		g.recordAudit(r, key, model, turn.AccountName, started, http.StatusOK, turn.FirstToken, promptTokens, completionTokens, true, turn.Retries, string(rawBody), result.Text, "")
		return
	}

	message := map[string]any{
		"role":              "assistant",
		"content":           result.Text,
		"reasoning_content": result.Thinking,
	}
	finishReason := "stop"
	if len(calls) > 0 {
		message["tool_calls"] = toolCallsPayload(calls)
		finishReason = "tool_calls"
	}

	response := map[string]any{
		"id":      "chatcmpl-" + randomID(12),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model.ID,
		"choices": []any{
			map[string]any{"index": 0, "message": message, "finish_reason": finishReason},
		},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	}
	raw, _ := json.Marshal(response)
	g.recordAudit(r, key, model, turn.AccountName, started, http.StatusOK, turn.FirstToken, promptTokens, completionTokens, false, turn.Retries, string(rawBody), string(raw), "")
	out.Header().Set("content-type", "application/json")
	out.WriteHeader(http.StatusOK)
	_, _ = out.Write(raw)
}

// conversationText renders the conversation for token estimation only.
//
// The real usage numbers come from the upstream when it reports them; this is
// the fallback for when it does not, and for the audit row's prompt count.
func conversationText(messages []minimax.DirectMessage) string {
	var builder strings.Builder
	for _, message := range messages {
		builder.WriteString(string(message.Content))
	}
	return builder.String()
}

// --------------------------------------------------------------- /v1/messages

// anthropicDirect implements POST /v1/messages on the model transport.
//
// This is the front end where the two protocols nearly coincide: the request is
// already Anthropic Messages and so is the upstream, so this is a filter rather
// than a translation. It still runs the same normalisation the OpenAI path
// does, because a client may send a bare string where the protocol allows
// blocks, and prior `thinking` blocks have to be dropped either way.
func (g *Gateway) anthropicDirect(
	out *respWriter,
	r *http.Request,
	key *store.ClientKey,
	model *store.ModelConfig,
	request anthropicRequest,
	rawBody []byte,
	started time.Time,
) {
	system := anthropicBlocksText(request.System)
	messages := directFromAnthropic(request.Messages)
	if len(messages) == 0 {
		writeAnthropicError(out, http.StatusBadRequest, "invalid_request_error", "messages must contain text or image content")
		return
	}
	tools := directAnthropicTools(request.Tools)

	maxTokens := request.MaxTokens
	if maxTokens <= 0 {
		maxTokens = minimax.DefaultDirectMaxTokens
	}

	stream := newAnthropicStream(out, model.ID, estimateTokens(system+conversationText(messages)))

	var committed bool
	var onDelta, onThinking func(string)
	if request.Stream {
		onDelta = func(text string) {
			committed = true
			stream.text(text)
		}
		onThinking = func(text string) {
			committed = true
			stream.thinking(text)
		}
		stream.start()
	}

	turn, lastErr := g.runDirectTurn(r.Context(), directTurnRequest{
		System:          system,
		Messages:        messages,
		Tools:           tools,
		Model:           model,
		MaxTokens:       maxTokens,
		Group:           key.Pool,
		DisableThinking: key.Pool == "gongyi",
		Started:         started,
		Stream:          request.Stream,
		OnDelta:         onDelta,
		OnThinking:      onThinking,
		Committed:       func() bool { return committed },
	})

	promptTokens := estimateTokens(system + conversationText(messages))
	if lastErr != nil {
		g.recordAudit(r, key, model, turn.AccountName, started, http.StatusBadGateway, 0, promptTokens, 0, request.Stream, turn.Retries, string(rawBody), "", lastErr.Error())
		if out.wroteHeader {
			stream.fail(lastErr.Error())
			return
		}
		writeAnthropicError(out, http.StatusBadGateway, "api_error", lastErr.Error())
		return
	}
	result := turn.Result
	if result == nil {
		writeAnthropicError(out, http.StatusBadGateway, "api_error", "upstream returned no result")
		return
	}

	calls := openAIToolCalls(result.Calls)
	completionTokens := result.OutputTokens
	if completionTokens <= 0 {
		completionTokens = estimateTokens(result.Text + result.Thinking)
	}
	g.store.RecordModelUsage(model.ID, 1, int64(promptTokens+completionTokens))
	g.store.BumpClientKeyUsage(key.ID)

	if request.Stream {
		if !turn.Streamed {
			if result.Thinking != "" {
				stream.thinking(result.Thinking)
			}
			if result.Text != "" {
				stream.text(result.Text)
			}
		}
		stream.finish(calls, completionTokens)
		g.recordAudit(r, key, model, turn.AccountName, started, http.StatusOK, turn.FirstToken, promptTokens, completionTokens, true, turn.Retries, string(rawBody), result.Text, "")
		return
	}

	content := make([]map[string]any, 0, 3)
	if result.Thinking != "" {
		content = append(content, map[string]any{"type": "thinking", "thinking": result.Thinking})
	}
	if result.Text != "" || len(calls) == 0 {
		content = append(content, map[string]any{"type": "text", "text": result.Text})
	}
	for _, call := range calls {
		content = append(content, map[string]any{
			"type":  "tool_use",
			"id":    call.ID,
			"name":  call.Name,
			"input": rawJSONObject(call.Arguments),
		})
	}

	stopReason := "end_turn"
	if len(calls) > 0 {
		stopReason = "tool_use"
	}

	response := map[string]any{
		"id":            "msg_" + randomID(24),
		"type":          "message",
		"role":          "assistant",
		"model":         model.ID,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":  promptTokens,
			"output_tokens": completionTokens,
		},
	}
	raw, _ := json.Marshal(response)
	g.recordAudit(r, key, model, turn.AccountName, started, http.StatusOK, turn.FirstToken, promptTokens, completionTokens, false, turn.Retries, string(rawBody), string(raw), "")
	out.Header().Set("content-type", "application/json")
	out.WriteHeader(http.StatusOK)
	_, _ = out.Write(raw)
}

// directFromAnthropic normalises an Anthropic conversation for the model
// endpoint.
func directFromAnthropic(messages []anthropicMessage) []minimax.DirectMessage {
	out := make([]minimax.DirectMessage, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		if role != "assistant" {
			role = "user"
		}
		blocks := normaliseAnthropicBlocks(message.Content)
		if len(blocks) == 0 {
			continue
		}
		out = append(out, directMessage(role, blocks))
	}
	return out
}

// normaliseAnthropicBlocks keeps the blocks the model endpoint understands and
// drops the rest.
//
// Thinking is dropped rather than forwarded: it is context the model produced
// on an earlier turn, replaying it adds nothing and costs tokens on every
// subsequent turn. Anything unrecognised falls back to its text, which is the
// same tolerance the agent transport's flattener applies — a block this build
// has not heard of should not silently disappear from the conversation.
func normaliseAnthropicBlocks(raw json.RawMessage) []map[string]any {
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '"' {
		var text string
		if json.Unmarshal(raw, &text) == nil && text != "" {
			return []map[string]any{{"type": "text", "text": text}}
		}
		return nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	out := make([]map[string]any, 0, len(blocks))
	for _, block := range blocks {
		switch kind, _ := block["type"].(string); kind {
		case "text", "image", "tool_use", "tool_result", "document":
			out = append(out, block)
		case "thinking", "redacted_thinking":
			// See the doc comment.
		default:
			if text, ok := block["text"].(string); ok && text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		}
	}
	return out
}

// directAnthropicTools converts Anthropic declarations for the endpoint.
func directAnthropicTools(tools []anthropicTool) []minimax.DirectTool {
	out := make([]minimax.DirectTool, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(tool.Name)
		if name == "" {
			continue
		}
		schema := tool.InputSchema
		if len(schema) == 0 || !json.Valid(schema) {
			schema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		out = append(out, minimax.DirectTool{
			Name:        name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return out
}
