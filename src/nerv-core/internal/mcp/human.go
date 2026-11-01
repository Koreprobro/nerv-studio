// human.go — the human-mode request marker (GUI-PLAN.md §6, SPECS.md §4.4).
//
// The GUI is an MCP client like any other, but it speaks for the HUMAN: it
// sees the full tool interfaces (private fields included) and the full wave
// history, including human-launched waves that are invisible to the
// consumer-AI. The mode is activated per request by an HTTP header carrying a
// secret token set by the operator (--human-token); the consumer-AI is never
// told the token exists. The token lives in the transport layer only — never
// in tool params, schemas, or logs, so it cannot leak into the consumer's
// world model (which is built from the MCP surface alone).
package mcp

import (
	"context"
	"crypto/subtle"
	"net/http"
)

// humanHeader is the transport-level marker; the MCP message body is untouched.
const humanHeader = "X-Nerv-Human"

type humanCtxKey struct{}

// withHuman tags a request context with the human-mode verdict.
func withHuman(ctx context.Context, human bool) context.Context {
	return context.WithValue(ctx, humanCtxKey{}, human)
}

// HumanFrom reports whether a request context is human-mode.
func HumanFrom(ctx context.Context) bool {
	h, _ := ctx.Value(humanCtxKey{}).(bool)
	return h
}

// isHumanRequest compares the request's human header against the operator
// token (constant-time; an unset token disables human mode entirely).
func (s *Server) isHumanRequest(r *http.Request) bool {
	tok := s.config.HumanToken
	if tok == "" {
		return false
	}
	got := r.Header.Get(humanHeader)
	if got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(tok)) == 1
}
