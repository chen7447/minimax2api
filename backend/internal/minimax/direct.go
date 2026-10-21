package minimax

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"minimax2api/internal/config"
)

// The direct model API.
//
// The agent conversation endpoint this package was originally built around is
// not a model surface. It answers as MiniMax's own cloud agent: its own persona
// ("I am Mavis"), its own Linux sandbox, its own long-term memory — and its
// request body is a fixed field list with nothing in it that could carry a tool
// declaration (see the gateway's tools.go). Asking it to act as somebody else's
// coding agent does not work, and no amount of prompt shaping changes what it
// is: a stronger "you are not in a sandbox" preamble raises the tool-call hit
// rate, but the tool arguments still come back pointing at /workspace.
//
// The official client does not send model turns there. It posts to
//
//	POST <host>/mavis/api/v1/llm/v1/messages
//
// which is an Anthropic Messages endpoint: structured messages, native tool
// use, plain SSE, no session, no echo, no restatement. Authentication is a
// bearer token and nothing else — no `x-signature`, no `yy`, and no metadata
// query string, so none of the fingerprint plumbing this package needs for the
// agent transport applies here.
//
// This file is that second transport. It is deliberately additive rather than a
// replacement: the agent transport still carries the video and image routes,
// which live in the agent's plugin layer and have no equivalent on the model
// endpoint.

// Default paths for the direct transport. Settings override both, for the same
// reason every other path is a setting: a rename upstream should be a console
// edit rather than a rebuild.
const (
	// DefaultModelPath is where one turn is posted.
	DefaultModelPath = "/mavis/api/v1/llm/v1/messages"
	// DefaultModelsPath lists the model ids the endpoint accepts. It is also the
	// catalogue the console shows, so a new upstream model is visible without
	// guessing at its spelling.
	DefaultModelsPath = "/mavis/api/v1/models"
)

// DefaultDirectMaxTokens is used when the caller did not ask for a limit.
//
// `max_tokens` is required by the protocol and has no server-side default, so
// something has to be sent. 8192 is far above a normal turn's output and far
// below every catalogue model's 128000 ceiling — the point is to never be the
// reason an answer was cut short.
const DefaultDirectMaxTokens = 8192

// DefaultDirectProbeModel is the model a credential probe asks for.
//
// It is the one id present for every account: the model catalogue is a property
// of the product, not of the account, so this does not need to be discovered.
const DefaultDirectProbeModel = "MiniMax-M3"

// DirectMessage is one turn of a conversation, already in Anthropic shape.
//
// Content is left as raw JSON because the protocol allows either a bare string
// or an array of blocks, and the caller is the one that knows which it built.
// Re-encoding it here would be a second place for the shape to drift.
type DirectMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// DirectTool is one function declaration, in Anthropic shape.
//
// The schema lives in `input_schema` here, not nested under `function` as
// OpenAI spells it; the gateway converts once, at the edge.
type DirectTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// DirectCall is one tool call the model asked for.
//
// Arguments is a JSON *object encoded as a string*, which is the shape OpenAI
// reports and therefore the shape every caller downstream already parses. The
// endpoint returns it as an object; the conversion happens where the response
// is read so it cannot drift from the OpenAI surface.
type DirectCall struct {
	ID        string
	Name      string
	Arguments string
}

// DirectResult is the aggregated outcome of one direct call.
type DirectResult struct {
	Text       string
	Thinking   string
	Calls      []DirectCall
	StopReason string
	// Model is the id the upstream echoed back. It is worth keeping: the
	// endpoint answers with the model it actually ran, which is the only way to
	// notice that a requested variant was silently substituted.
	Model        string
	InputTokens  int
	OutputTokens int
}

// DirectOptions describes one call to the model endpoint.
type DirectOptions struct {
	Credential Credential
	// Model is the upstream id (`MiniMax-M3`), not the catalogue id
	// (`minimax-m3`). The two are different namespaces and only this one is
	// understood upstream.
	Model  string
	System string
	// Messages is the whole conversation. The endpoint is stateless, so nothing
	// is remembered between calls and the caller resends history every turn.
	Messages    []DirectMessage
	Tools       []DirectTool
	MaxTokens   int
	Timeout     time.Duration
	IdleTimeout time.Duration
	// DisableThinking explicitly turns off the reasoning channel for this turn.
	// The public pool uses it so translation requests never pay for thinking.
	DisableThinking bool
	OnDelta         func(string)
	OnThinking      func(string)
	// OnFrame sees every decoded SSE payload, before anything interprets it.
	OnFrame func(map[string]any)
	// DisableStream asks for a single JSON response instead of SSE.
	DisableStream bool
}

func modelPath(settings config.Settings) string {
	if path := strings.TrimSpace(settings.Upstream.ModelPath); path != "" {
		return path
	}
	return DefaultModelPath
}

func modelsPath(settings config.Settings) string {
	if path := strings.TrimSpace(settings.Upstream.ModelsPath); path != "" {
		return path
	}
	return DefaultModelsPath
}

// newDirectRequest builds a request for the model endpoint.
//
// It shares nothing with newRequest on purpose. The agent transport signs every
// call — `x-signature` over the body, `yy` over the full URL including the
// 22-parameter fingerprint query — while this endpoint authenticates with a
// bearer token alone. Sending the signature headers here would not break
// anything, but it would suggest the two transports are the same protocol when
// the whole point is that they are not.
func (c *Client) newDirectRequest(ctx context.Context, settings config.Settings, cred Credential, method, path string, body []byte, stream bool) (*http.Request, error) {
	rawURL := strings.TrimRight(c.baseURL(settings, cred), "/") + path

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	if token := strings.TrimSpace(cred.Token); token != "" {
		req.Header.Set("authorization", "Bearer "+token)
	}
	req.Header.Set("accept", "text/event-stream, application/json, */*")
	req.Header.Set("content-type", "application/json")
	if language := strings.TrimSpace(settings.Upstream.Language); language != "" {
		req.Header.Set("accept-language", language)
	}
	req.Header.Set("user-agent", settings.Upstream.UserAgent)
	// A streaming turn must not be compressed: this upstream answers the
	// advertisement by buffering the whole stream before sending it. See New.
	if stream {
		req.Header.Set("accept-encoding", "identity")
	}
	return req, nil
}

// Messages performs one turn against the model endpoint.
//
// Streaming is the default because every client that wants this transport is a
// streaming client, and the endpoint's SSE is the protocol's own — no bridging,
// no pseudo-streaming queue.
func (c *Client) Messages(ctx context.Context, opts DirectOptions) (*DirectResult, error) {
	settings := c.settings()
	if strings.TrimSpace(opts.Credential.Token) == "" {
		return nil, ErrInvalidCredential
	}
	model := strings.TrimSpace(opts.Model)
	if model == "" {
		return nil, errors.New("minimax direct: no upstream model id")
	}

	maxTokens := opts.MaxTokens
	if maxTokens <= 0 {
		maxTokens = DefaultDirectMaxTokens
	}

	stream := !opts.DisableStream
	body := map[string]any{
		"model":      model,
		"max_tokens": maxTokens,
		"messages":   opts.Messages,
		"stream":     stream,
	}
	if system := strings.TrimSpace(opts.System); system != "" {
		body["system"] = system
	}
	if len(opts.Tools) > 0 {
		body["tools"] = opts.Tools
	}
	if opts.DisableThinking {
		body["thinking"] = map[string]any{"type": "disabled"}
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = settings.RequestTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := c.newDirectRequest(ctx, settings, opts.Credential, http.MethodPost, modelPath(settings), payload, stream)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, settings)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidCredential
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		if resp.StatusCode == http.StatusPaymentRequired {
			return nil, fmt.Errorf("%w: minimax model HTTP 402: %s", ErrInsufficientBalance, snippet(raw))
		}
		return nil, fmt.Errorf("minimax model HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	if stream {
		return c.consumeDirectStream(ctx, resp.Body, opts)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	result, err := parseDirectMessage(raw)
	if err != nil {
		return nil, err
	}
	if result.Text != "" && opts.OnDelta != nil {
		opts.OnDelta(result.Text)
	}
	if result.Thinking != "" && opts.OnThinking != nil {
		opts.OnThinking(result.Thinking)
	}
	return result, nil
}

// Models lists the model ids the endpoint accepts.
//
// It is the only way to learn the upstream's own spelling of a model, which is
// what the catalogue's upstream_model column has to match. The response is a
// provider registry rather than a flat list, so the ids are pulled out of
// whichever shape arrives rather than out of one fixed path.
func (c *Client) Models(ctx context.Context, cred Credential) ([]string, error) {
	if strings.TrimSpace(cred.Token) == "" {
		return nil, ErrInvalidCredential
	}
	return c.fetchModelIDs(ctx, cred)
}

// PublicModels reads the same registry without a credential.
//
// The registry endpoint is public: on 2026-10-20 it answered 200 with a full
// provider registry even for a garbage bearer token. That is what lets the
// console refresh the catalogue while the pool is empty, which is exactly the
// situation where a stale catalogue is most likely to be noticed. The turn
// endpoint still requires a token — only this read does not.
func (c *Client) PublicModels(ctx context.Context) ([]string, error) {
	return c.fetchModelIDs(ctx, Credential{})
}

func (c *Client) fetchModelIDs(ctx context.Context, cred Credential) ([]string, error) {
	settings := c.settings()
	timeout := 30 * time.Second
	if settings.Upstream.RequestTimeoutSec > 0 && settings.Upstream.RequestTimeoutSec < 60 {
		timeout = settings.RequestTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := c.newDirectRequest(ctx, settings, cred, http.MethodGet, modelsPath(settings), nil, false)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req, settings)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, ErrInvalidCredential
	}
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("minimax models HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	return parseModelIDs(raw), nil
}

// ModelProbe is what one probe of an upstream model id concluded.
type ModelProbe struct {
	// Served is true when the endpoint answered under the requested id.
	Served bool
	// Echoed is the model id the endpoint said it ran. When it differs from the
	// requested one, the upstream substituted its default model.
	Echoed string
	// Status is the HTTP status that produced the verdict.
	Status int
}

// ProbeModel asks the model endpoint to run one token under upstreamID.
//
// The endpoint does not reject an unknown id: it silently runs the account's
// default model and echoes *that* model back (verified 2026-10-20 —
// `MiniMax-No-Such-Model-9` answered 200 with `model: MiniMax-M3.1-Flash`). So
// "still served" has to be read from the echo, not from the status code. A 400
// that names the model selection is also treated as not served; every other
// failure (auth, 402, 5xx, network) is returned as an error so the caller can
// leave the catalogue untouched rather than mistake an account problem for a
// dead model.
func (c *Client) ProbeModel(ctx context.Context, cred Credential, upstreamID string) (ModelProbe, error) {
	if strings.TrimSpace(cred.Token) == "" {
		return ModelProbe{}, ErrInvalidCredential
	}
	upstreamID = strings.TrimSpace(upstreamID)
	if upstreamID == "" {
		return ModelProbe{}, errors.New("minimax probe: no upstream model id")
	}

	settings := c.settings()
	timeout := 30 * time.Second
	if settings.Upstream.RequestTimeoutSec > 0 && settings.Upstream.RequestTimeoutSec < 60 {
		timeout = settings.RequestTimeout()
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model":      upstreamID,
		"max_tokens": 1,
		"stream":     false,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	if err != nil {
		return ModelProbe{}, err
	}
	req, err := c.newDirectRequest(ctx, settings, cred, http.MethodPost, modelPath(settings), body, false)
	if err != nil {
		return ModelProbe{}, err
	}
	resp, err := c.do(req, settings)
	if err != nil {
		return ModelProbe{}, err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return ModelProbe{}, ErrInvalidCredential
	case resp.StatusCode == http.StatusBadRequest:
		text := strings.ToLower(string(raw))
		for _, marker := range []string{"invalid model", "model selection", "1406010011"} {
			if strings.Contains(text, marker) {
				return ModelProbe{Served: false, Status: resp.StatusCode}, nil
			}
		}
		return ModelProbe{}, fmt.Errorf("minimax probe HTTP %d: %s", resp.StatusCode, snippet(raw))
	case resp.StatusCode >= 400:
		return ModelProbe{}, fmt.Errorf("minimax probe HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	var parsed struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ModelProbe{}, fmt.Errorf("minimax probe: decode: %w", err)
	}
	echoed := strings.TrimSpace(parsed.Model)
	if echoed == "" {
		return ModelProbe{}, errors.New("minimax probe: response named no model")
	}
	return ModelProbe{
		Served: strings.EqualFold(echoed, upstreamID),
		Echoed: echoed,
		Status: resp.StatusCode,
	}, nil
}

// parseModelIDs collects model ids out of the catalogue response.
//
// The shape is a provider registry — `providers[].config.models` is a map keyed
// by id — but only the ids matter here, and keying off the map rather than a
// fixed path keeps this working if a provider entry is added or reordered.
//
// The order follows the upstream's own `model_order` first, then any remaining
// picker entries, then the whitelist. That order is what the console uses to
// put the model the upstream currently leads with at the top; sorting the whole
// list alphabetically would throw it away.
//
// `whitelist` is included on purpose. It is the endpoint's own list of ids it
// still accepts, and it is a superset of the picker: on 2026-10-20 the picker
// held M3.1-Flash and M3 while the whitelist also held M2.7 and M2.7-highspeed.
// Reading only the picker would leave those two looking unlisted on every
// refresh.
func parseModelIDs(raw []byte) []string {
	var parsed struct {
		Providers []struct {
			Config struct {
				Models     map[string]json.RawMessage `json:"models"`
				Whitelist  []string                   `json:"whitelist"`
				ModelOrder []string                   `json:"model_order"`
			} `json:"config"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	seen := map[string]bool{}
	var ids []string
	add := func(id string) {
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, provider := range parsed.Providers {
		for _, id := range provider.Config.ModelOrder {
			add(id)
		}
		rest := make([]string, 0, len(provider.Config.Models))
		for id := range provider.Config.Models {
			rest = append(rest, id)
		}
		sort.Strings(rest)
		for _, id := range rest {
			add(id)
		}
		whitelist := append([]string(nil), provider.Config.Whitelist...)
		sort.Strings(whitelist)
		for _, id := range whitelist {
			add(id)
		}
	}
	return ids
}

// consumeDirectStream reads the endpoint's SSE and assembles one message.
//
// The event sequence is the protocol's own: message_start, then a
// content_block_start / delta* / content_block_stop group per block, then
// message_delta and message_stop. Tool arguments arrive as `input_json_delta`
// fragments and are only whole once their block stops, which is why calls are
// emitted at the end rather than as they stream.
func (c *Client) consumeDirectStream(ctx context.Context, body io.ReadCloser, opts DirectOptions) (*DirectResult, error) {
	settings := c.settings()
	idle := opts.IdleTimeout
	if idle <= 0 {
		idle = settings.StreamIdleTimeout()
	}

	result := &DirectResult{}
	builder := newDirectStreamBuilder(result, opts)

	reader := newSSEReader(body, idle)
	defer reader.stop()

	for {
		event, raw, err := reader.next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return result, err
		}
		if raw == "" || raw == "[DONE]" {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(raw), &payload); err != nil {
			continue
		}
		if opts.OnFrame != nil {
			opts.OnFrame(payload)
		}
		if err := builder.handle(event, payload); err != nil {
			return result, err
		}
		if ctx.Err() != nil {
			return result, ctx.Err()
		}
	}
	result.Calls = builder.calls()
	return result, nil
}

// directStreamBuilder folds one SSE event into the result.
type directStreamBuilder struct {
	result *DirectResult
	opts   DirectOptions
	// blocks holds the open tool_use blocks by content index. Text and thinking
	// are appended straight to the result; a tool call is not knowable until its
	// fragments stop arriving.
	blocks map[int]*directToolBlock
	// order preserves the order blocks were opened, so calls come out in the
	// order the model produced them rather than in map order.
	order []int
}

type directToolBlock struct {
	id      string
	name    string
	partial strings.Builder
}

func newDirectStreamBuilder(result *DirectResult, opts DirectOptions) *directStreamBuilder {
	return &directStreamBuilder{result: result, opts: opts, blocks: map[int]*directToolBlock{}}
}

func (b *directStreamBuilder) handle(event string, payload map[string]any) error {
	kind, _ := payload["type"].(string)
	if kind == "" {
		// A missing type is not worth guessing at: every documented event has
		// one, and the SSE `event:` name is the same value.
		kind = event
	}

	switch kind {
	case "error":
		return errors.New(directErrorMessage(payload))
	case "message_start":
		if message, ok := payload["message"].(map[string]any); ok {
			if model, ok := message["model"].(string); ok {
				b.result.Model = model
			}
			b.readUsage(message["usage"])
		}
	case "content_block_start":
		index := intField(payload, "index")
		block, _ := payload["content_block"].(map[string]any)
		if block == nil {
			return nil
		}
		if kind, _ := block["type"].(string); kind == "tool_use" {
			entry := &directToolBlock{}
			entry.id, _ = block["id"].(string)
			entry.name, _ = block["name"].(string)
			b.blocks[index] = entry
			b.order = append(b.order, index)
		}
	case "content_block_delta":
		index := intField(payload, "index")
		delta, _ := payload["delta"].(map[string]any)
		if delta == nil {
			return nil
		}
		switch kind, _ := delta["type"].(string); kind {
		case "text_delta":
			if text, _ := delta["text"].(string); text != "" {
				b.result.Text += text
				if b.opts.OnDelta != nil {
					b.opts.OnDelta(text)
				}
			}
		case "thinking_delta":
			if text, _ := delta["thinking"].(string); text != "" {
				b.result.Thinking += text
				if b.opts.OnThinking != nil {
					b.opts.OnThinking(text)
				}
			}
		case "input_json_delta":
			if entry := b.blocks[index]; entry != nil {
				if fragment, _ := delta["partial_json"].(string); fragment != "" {
					entry.partial.WriteString(fragment)
				}
			}
		}
	case "message_delta":
		if delta, ok := payload["delta"].(map[string]any); ok {
			if reason, _ := delta["stop_reason"].(string); reason != "" {
				b.result.StopReason = reason
			}
		}
		b.readUsage(payload["usage"])
	}
	return nil
}

// calls returns the tool calls seen so far, in the order the blocks opened.
func (b *directStreamBuilder) calls() []DirectCall {
	calls := make([]DirectCall, 0, len(b.order))
	for _, index := range b.order {
		entry := b.blocks[index]
		if entry == nil || strings.TrimSpace(entry.name) == "" {
			continue
		}
		calls = append(calls, DirectCall{
			ID:        entry.id,
			Name:      entry.name,
			Arguments: normaliseArguments(entry.partial.String()),
		})
	}
	return calls
}

func (b *directStreamBuilder) readUsage(raw any) {
	usage, ok := raw.(map[string]any)
	if !ok {
		return
	}
	if value := intField(usage, "input_tokens"); value > 0 {
		b.result.InputTokens = value
	}
	if value := intField(usage, "output_tokens"); value > 0 {
		b.result.OutputTokens = value
	}
}

// parseDirectMessage reads a non-streaming response.
func parseDirectMessage(raw []byte) (*DirectResult, error) {
	var parsed struct {
		Model   string `json:"model"`
		Content []struct {
			Type     string          `json:"type"`
			Text     string          `json:"text"`
			Thinking string          `json:"thinking"`
			ID       string          `json:"id"`
			Name     string          `json:"name"`
			Input    json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
		Usage      struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("minimax model: unreadable response: %w", err)
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return nil, errors.New(parsed.Error.Message)
	}

	result := &DirectResult{
		Model:        parsed.Model,
		StopReason:   parsed.StopReason,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}
	for _, block := range parsed.Content {
		switch block.Type {
		case "text":
			result.Text += block.Text
		case "thinking":
			result.Thinking += block.Thinking
		case "tool_use":
			result.Calls = append(result.Calls, DirectCall{
				ID:        block.ID,
				Name:      block.Name,
				Arguments: normaliseArguments(string(block.Input)),
			})
		}
	}
	return result, nil
}

// directErrorMessage digs the message out of an SSE error event.
func directErrorMessage(payload map[string]any) string {
	if raw, ok := payload["error"].(map[string]any); ok {
		if message, _ := raw["message"].(string); message != "" {
			return message
		}
	}
	if message, _ := payload["error"].(string); message != "" {
		return message
	}
	return "upstream stream error"
}

// normaliseArguments turns whatever the model produced into a JSON object
// string, which is what OpenAI reports and what every caller parses.
//
// Three shapes are seen in practice: a complete object, an empty string (a call
// with no arguments), and a truncated one from a stream that was cut off. Only
// the first is passed through; the others become "{}" rather than invalid JSON,
// because a caller that cannot parse arguments fails the whole turn while a
// caller with an empty object at least sees which tool was requested.
func normaliseArguments(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	if !json.Valid([]byte(trimmed)) {
		return "{}"
	}
	if trimmed[0] != '{' {
		// A bare string or array. Wrap it so the caller still gets JSON.
		encoded, err := json.Marshal(trimmed)
		if err != nil {
			return "{}"
		}
		return string(encoded)
	}
	return trimmed
}

// intField reads an integer that may have arrived as a JSON number.
func intField(payload map[string]any, key string) int {
	switch value := payload[key].(type) {
	case float64:
		return int(value)
	case int:
		return value
	}
	return 0
}
