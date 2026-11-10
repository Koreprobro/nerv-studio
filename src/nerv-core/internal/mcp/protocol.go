// SPDX-License-Identifier: AGPL-3.0-or-later
//
// nerv-core — omni-channel AI studio core: a thin MCP orchestrator of
// system-installed generation tools, a GPU scheduler and an artifact server.
// Copyright (C) 2026 Koreprobro
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package mcp is the only network surface of the daemon: streamable HTTP
// JSON-RPC (MCP) at /mcp plus the /artifacts/ file mount. The tool list
// is fixed by REBUILD §2.2 — kb, artifactory, tools, loras, dice.roll,
// calc.resolution, execute — four domain tools over one browse mechanism
// and three utilities; gpu.list stays hidden (operator diagnostics).
package mcp

import (
	"encoding/json"
	"net/http"
)

// JSON-RPC protocol error codes.
const (
	codeParse    = -32700
	codeInvalid  = -32600
	codeNoMethod = -32601
	codeNoParams = -32602
	codeInternal = -32603
	codeNotInit  = -32002
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"` // absent for notifications
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
	Meta    map[string]any  `json:"_meta,omitempty"` // Streamable HTTP request metadata
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// ToolContent is one MCP content block.
type ToolContent struct {
	Type     string `json:"type"` // "text" | "image" | "audio"
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"` // base64 for media blocks
	MimeType string `json:"mimeType,omitempty"`
}

// ToolResult is the tools/call payload; protocol errors are separate,
// tool-level failures ride isError (the wave answer is always deliverable).
type ToolResult struct {
	Content []ToolContent `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

func textBlock(s string) ToolContent { return ToolContent{Type: "text", Text: s} }
func imageBlock(b64, mime string) ToolContent {
	return ToolContent{Type: "image", Data: b64, MimeType: mime}
}
func audioBlock(b64, mime string) ToolContent {
	return ToolContent{Type: "audio", Data: b64, MimeType: mime}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Result: result})
}

func writeError(w http.ResponseWriter, id json.RawMessage, code int, msg string) {
	writeJSON(w, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

// isNotification: JSON-RPC messages without an id (initialized, cancelled).
func (r *rpcRequest) isNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// argument helpers over the decoded tools/call arguments map.
func asString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func optString(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}

func optInt(m map[string]any, k string) (int, bool) {
	if v, ok := m[k].(float64); ok {
		return int(v), true
	}
	return 0, false
}

func optBool(m map[string]any, k string) bool {
	v, _ := m[k].(bool)
	return v
}
