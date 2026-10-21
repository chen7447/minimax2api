package minimax

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"time"
)

// sseReader yields one `event:` / `data:` pair at a time from an SSE body.
//
// The agent transport reads its stream by splitting a growing buffer on blank
// lines, which works because its frames are small and never carry multi-line
// data. The model endpoint speaks the SSE spec, where `data:` may repeat and a
// `:` line is a comment — so this is a real parser rather than a split.
//
// The idle timer is what keeps a stalled upstream from hanging a request
// forever: it closes the body, which unblocks the pending read. Every read
// resets it, so it bounds the gap between bytes rather than the whole stream.
type sseReader struct {
	reader *bufio.Reader
	body   io.ReadCloser
	timer  *time.Timer
	idle   time.Duration
}

func newSSEReader(body io.ReadCloser, idle time.Duration) *sseReader {
	reader := &sseReader{
		reader: bufio.NewReaderSize(body, 64*1024),
		body:   body,
		idle:   idle,
	}
	if idle > 0 {
		reader.timer = time.AfterFunc(idle, func() { _ = body.Close() })
	}
	return reader
}

func (r *sseReader) stop() {
	if r.timer != nil {
		r.timer.Stop()
	}
}

// next returns the next event. io.EOF means the stream ended cleanly, which is
// not an error for a caller that already has the frames it needs.
func (r *sseReader) next() (event, data string, err error) {
	var payload strings.Builder
	for {
		line, readErr := r.reader.ReadString('\n')
		if r.timer != nil {
			r.timer.Reset(r.idle)
		}

		trimmed := strings.TrimRight(line, "\r\n")
		switch {
		case strings.HasPrefix(trimmed, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
		case strings.HasPrefix(trimmed, "data:"):
			// A repeated data line is a continuation, joined without a
			// separator — that is what the spec says and what the endpoint does
			// not currently exercise, so getting it right costs nothing.
			payload.WriteString(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
		case strings.HasPrefix(trimmed, ":"):
			// A comment. The endpoint sends these as keep-alives.
		case trimmed == "":
			if payload.Len() > 0 || event != "" {
				return event, payload.String(), nil
			}
		}

		if readErr != nil {
			if payload.Len() > 0 || event != "" {
				return event, payload.String(), nil
			}
			if errors.Is(readErr, io.EOF) {
				return "", "", io.EOF
			}
			return "", "", readErr
		}
	}
}
