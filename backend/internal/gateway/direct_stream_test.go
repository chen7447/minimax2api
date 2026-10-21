package gateway

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestChatDirectForwardsDeltasAsTheyArrive pins the one property a streaming
// client actually pays for: a delta reaches the caller before the next one is
// produced upstream.
//
// httptest.ResponseRecorder cannot show this — it is a buffer that is only read
// after the handler returns, so a gateway that coalesces the whole answer looks
// identical to one that streams. This test therefore puts the gateway behind a
// real listener and gates the upstream frame by frame.
func TestChatDirectForwardsDeltasAsTheyArrive(t *testing.T) {
	gate := make(chan struct{})
	upstreamDone := make(chan struct{})

	h := directHarness(t, func(w http.ResponseWriter, r *http.Request) {
		defer close(upstreamDone)
		w.Header().Set("content-type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(s string) {
			_, _ = io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		write(`event: message_start` + "\n" + `data: {"type":"message_start","message":{"model":"MiniMax-M3"}}` + "\n\n")
		write(`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n")
		for _, text := range []string{"one", "two", "three"} {
			<-gate
			write(`event: content_block_delta` + "\n" +
				`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"` + text + `"}}` + "\n\n")
		}
		write(`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"}}` + "\n\n")
		write(`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n")
	})
	h.addAccount(t, "acct", "tok", 50)

	// The gateway behind a real listener, so the response is a real chunked body.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.gateway.ChatCompletions(w, r)
	}))
	defer server.Close()

	body := strings.NewReader(`{"model":"minimax-m3","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	req, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("authorization", "Bearer "+h.key)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	// Arrival times are recorded as the reader sees them, not as the handler
	// claims to have written them.
	deltas := make(chan string, 8)
	go func() {
		reader := bufio.NewReader(resp.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(deltas)
				return
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						Content string `json:"content"`
					} `json:"delta"`
				} `json:"choices"`
			}
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &chunk) != nil {
				continue
			}
			if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
				deltas <- chunk.Choices[0].Delta.Content
			}
		}
	}()

	for _, want := range []string{"one", "two", "three"} {
		gate <- struct{}{}
		select {
		case got, ok := <-deltas:
			if !ok {
				t.Fatalf("stream ended before %q arrived", want)
			}
			if got != want {
				t.Fatalf("delta = %q, want %q", got, want)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("%q never reached the caller while the upstream was still open: "+
				"the gateway is buffering the stream", want)
		}
	}
}
