// Package mcp implements the streamable-HTTP MCP (2026-07-28) surface: a single
// POST endpoint that serves JSON-RPC requests and SSE-scoped responses. The
// scheduler / LLM tools live here (SPECS.md §4).
package mcp

import "encoding/json"

// JSON-RPC 2.0 error codes (RFC + Streamable HTTP custom codes).
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
	HeaderMismatch = -32020 // Streamable HTTP: header/body mismatch
)

// Request is a JSON-RPC request or notification from the client.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *jsonRPCID      `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Meta    map[string]any  `json:"_meta,omitempty"`
}

// Notification is a Request without an ID (server MUST NOT respond).
func (r Request) IsNotification() bool { return r.ID == nil }

// jsonRPCID is a JSON-RPC identifier (string or number) that also records
// whether it was present.
type jsonRPCID struct {
	Value   any
	Present bool
}

// UnmarshalJSON accepts null/absent (absent => notification) and string/number ids.
func (id *jsonRPCID) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		id.Value = nil
		id.Present = false
		return nil
	}
	id.Present = true
	return json.Unmarshal(b, &id.Value)
}

// MarshalJSON emits the id or omits it when absent.
func (id jsonRPCID) MarshalJSON() ([]byte, error) {
	if !id.Present {
		return []byte("null"), nil
	}
	return json.Marshal(id.Value)
}

// Result is a JSON-RPC response object. The jsonrpc member is REQUIRED by
// JSON-RPC 2.0 / MCP (2026-07-28) in every response and is set by the encode
// helpers (writeResult / writeError). The id member is REQUIRED too: on errors
// whose request id is unknown it is emitted as null, not omitted (audit M6).
type Result struct {
	JSONRPC string     `json:"jsonrpc,omitempty"`
	ID      *jsonRPCID `json:"id"`
	Result  any        `json:"result,omitempty"`
	Error   *RPCError  `json:"error,omitempty"`
}

// jsonRPCVersion is the JSON-RPC protocol version. It is always "2.0" — distinct
// from ProtocolVersion (the MCP transport version, "2026-07-28"), which is emitted
// in result.protocolVersion and the MCP-Protocol-Version header, never here.
const jsonRPCVersion = "2.0"

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// ToolContent is a content part of a tool-call result. It supports text blocks
// and binary media blocks (image/audio/video) per the MCP (2026-07-28) content
// model. Binary blocks carry base64 data (encoding/json encodes []byte as a
// base64 string) together with its MIME type; a client renders <img
// src="data:image/png;base64,..."> or attaches the block to the model context.
type ToolContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     []byte `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// ToolResult is the result of a tools/call.
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}
