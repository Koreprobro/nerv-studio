package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
)

// FlushWriter forwards writes to w and immediately flushes the underlying HTTP
// response when it supports it (http.Flusher). SSE REQUIRES this: a bufio flush
// alone leaves events in the server's response buffer, so the client sees
// nothing until the handler returns — mid-stream progress and client-disconnect
// detection both silently die (found by the M2 test).
type FlushWriter struct{ W io.Writer }

func (f FlushWriter) Write(p []byte) (int, error) {
	n, err := f.W.Write(p)
	if fl, ok := f.W.(http.Flusher); ok {
		fl.Flush()
	}
	return n, err
}

// SSEWriter emits JSON-RPC messages as SSE events on the 2026-07-28 Streamable
// HTTP transport: each message is "data: <json>\n\n", periodic keep-alive
// comments (":\r\n") hold the connection through idle proxies, and the server
// sets X-Accel-Buffering: no so reverse proxies do not buffer the events.
type SSEWriter struct {
	w  io.Writer
	bw *bufio.Writer
}

// NewSSEWriter wraps w as an SSE writer.
func NewSSEWriter(w io.Writer) *SSEWriter {
	return &SSEWriter{w: w, bw: bufio.NewWriter(w)}
}

// Event writes v as an SSE data event and flushes.
func (s *SSEWriter) Event(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if _, err := s.bw.WriteString("data: "); err != nil {
		return err
	}
	if _, err := s.bw.Write(b); err != nil {
		return err
	}
	if _, err := s.bw.WriteString("\n\n"); err != nil {
		return err
	}
	return s.bw.Flush()
}

// Keepalive writes an SSE comment to keep the connection alive.
func (s *SSEWriter) Keepalive() error {
	if _, err := s.bw.WriteString(":\r\n"); err != nil {
		return err
	}
	return s.bw.Flush()
}

// Flush flushes any buffered data.
func (s *SSEWriter) Flush() error { return s.bw.Flush() }
