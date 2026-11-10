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

package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerv-core/internal/config"
	"nerv-core/internal/gpu"
	"nerv-core/internal/scheduler"
	"nerv-core/internal/store"
)

// fixture builds a whole daemon over temp dirs and an httptest server.
func fixture(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	ts, art, _ := fixtureTweak(t, nil, nil)
	return ts, art
}

// fixtureTweak is fixture with a config hook and a GPU pool override.
func fixtureTweak(t *testing.T, tweak func(*config.Config), pool *gpu.Pool) (*httptest.Server, string, *Server) {
	t.Helper()
	root := t.TempDir()
	dirs := map[string]string{}
	for _, d := range []string{"tools", "artifactory", "kb", "loras", "logs"} {
		dirs[d] = filepath.Join(root, d)
		if err := os.MkdirAll(dirs[d], 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeTool(t, dirs["tools"], "echo.sh",
		`{"summary":"writes text to output","fields":[
		   {"name":"text","required":true},{"name":"output","io":"out"},
		   {"name":"seed","default":-1,"hidden":true}]}`,
		`printf '%s' "$NERV_TEXT" > "$NERV_OUTPUT"`)
	if err := os.WriteFile(filepath.Join(dirs["kb"], "README.md"),
		[]byte("door document\nline two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ProjectRoot: root,
		Domains: map[string]config.Domain{
			"tools":       {Path: dirs["tools"], Commands: []string{"probe", "read", "execute"}},
			"artifactory": {Path: dirs["artifactory"], Commands: []string{"browse", "probe", "read", "fetch", "write", "mount"}},
			"kb":          {Path: dirs["kb"], Commands: []string{"read", "probe"}, Hint: "custom kb hint"},
			"loras":       {Path: dirs["loras"], Commands: []string{"browse", "probe", "fetch"}},
		},
		MaxOutputChars: 32768,
	}
	if tweak != nil {
		tweak(cfg)
	}
	srv := New(cfg, pool, map[string]*scheduler.Scheduler{
		"tools": scheduler.New(scheduler.Deps{
			ToolsDir:    dirs["tools"],
			Artifactory: store.Store{Root: dirs["artifactory"]},
			Pool:        pool,
		}),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		srv.Shutdown(3 * time.Second)
	})
	return ts, dirs["artifactory"], srv
}

// cudaPool builds a frozen test pool over a fake cuda enumerator.
func cudaPool(n int) *gpu.Pool {
	enums := map[string]gpu.Enumerator{}
	if n > 0 {
		enums["cuda"] = func() ([]gpu.Slot, error) {
			var out []gpu.Slot
			for i := 0; i < n; i++ {
				out = append(out, gpu.Slot{Backend: "cuda", Index: i})
			}
			return out, nil
		}
	}
	p, _, err := gpu.Build("", "", enums)
	if err != nil {
		panic(err)
	}
	return p
}

func writeTool(t *testing.T, toolsDir, name, info, body string) {
	t.Helper()
	script := "#!/usr/bin/env sh\ncase \"${1:-}\" in\n  --tool-info) cat <<'INFOEOF'\n" +
		info + "\nINFOEOF\n    ;;\n  *) " + body + " ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// rpc POSTs one request and decodes the JSON response.
func rpc(t *testing.T, ts *httptest.Server, id string, method string, params map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != "" {
		body["id"] = json.RawMessage(id)
	}
	if params != nil {
		body["params"] = params
	}
	raw, _ := json.Marshal(body)
	resp, err := http.Post(ts.URL+"/mcp", "application/json", bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	all, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if err := json.Unmarshal(all, &out); err != nil {
		t.Fatalf("response not JSON (%s): %q", resp.Header.Get("Content-Type"), string(all)[:min(200, len(all))])
	}
	return out
}

func call(t *testing.T, ts *httptest.Server, tool string, args map[string]any) (ToolResult, map[string]any) {
	t.Helper()
	out := rpc(t, ts, `1`, "tools/call", map[string]any{"name": tool, "arguments": args})
	if e, ok := out["error"]; ok {
		t.Fatalf("rpc error: %v", e)
	}
	b, _ := json.Marshal(out["result"])
	var res ToolResult
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("bad tool result: %v (%s)", err, b)
	}
	return res, out
}

func firstText(res ToolResult) string {
	for _, c := range res.Content {
		if c.Type == "text" {
			return c.Text
		}
	}
	return ""
}

func TestInitializeAndList(t *testing.T) {
	ts, _ := fixture(t)
	init := rpc(t, ts, `1`, "initialize", map[string]any{"protocolVersion": ProtocolVersion})
	resMap, _ := init["result"].(map[string]any)
	if resMap == nil || resMap["protocolVersion"] == nil {
		t.Fatalf("initialize: %+v", init)
	}
	out := rpc(t, ts, `2`, "tools/list", nil)
	resMap, _ = out["result"].(map[string]any)
	b, _ := json.Marshal(resMap["tools"])
	var tools []toolDef
	if err := json.Unmarshal(b, &tools); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, td := range tools {
		names = append(names, td.Name)
	}
	want := "artifactory kb loras tools dice.roll calc.resolution" // domains sorted, then utilities
	if strings.Join(names, " ") != want {
		t.Errorf("order: %v want %s", names, want)
	}
	if strings.Contains(string(b), "gpu.list") {
		t.Error("gpu.list must stay hidden from tools/list")
	}
	for _, td := range tools {
		if td.Name == "kb" && td.Description != "custom kb hint" {
			t.Errorf("hint override lost: %q", td.Description)
		}
	}
}

// TestMCPNegotiateVersion exercises MCP protocol-version negotiation. A client
// may convey its supported version in the OPTIONAL MCP-Protocol-Version header
// (the Cline path: header set, body empty), in initialize.params.protocolVersion,
// or both. The server must echo whichever the client uses when it is at or below
// ProtocolVersion, and fall back to its own when the client sends nothing or a
// version newer than the server's.
func TestMCPNegotiateVersion(t *testing.T) {
	ts, _ := fixture(t)
	client := &http.Client{}

	post := func(t *testing.T, header string, body map[string]any) any {
		t.Helper()
		b, _ := json.Marshal(body)
		var req *http.Request
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/mcp", bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/json")
		if header != "" {
			req.Header.Set("MCP-Protocol-Version", header)
		}
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		res, _ := got["result"].(map[string]any)
		return res["protocolVersion"]
	}

	// (a) version conveyed ONLY in the header, as Cline does.
	if got := post(t, "2025-06-18", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{"capabilities": map[string]any{}},
	}); got != "2025-06-18" {
		t.Errorf("header-only version not echoed, got %v", got)
	}

	// (b) version conveyed ONLY in params.
	if got := post(t, "", map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "initialize",
		"params": map[string]any{"protocolVersion": "2024-11-05", "capabilities": map[string]any{}},
	}); got != "2024-11-05" {
		t.Errorf("params-only version not echoed, got %v", got)
	}

	// (c) nothing conveyed -> server's own ProtocolVersion.
	if got := post(t, "", map[string]any{"jsonrpc": "2.0", "id": 3, "method": "initialize"}); got != ProtocolVersion {
		t.Errorf("expected server protocol version %q, got %v", ProtocolVersion, got)
	}

	// (d) client newer than the server -> server falls back to its own.
	if got := post(t, "2099-01-01", map[string]any{"jsonrpc": "2.0", "id": 4, "method": "initialize"}); got != ProtocolVersion {
		t.Errorf("expected server protocol version %q for a newer client, got %v", ProtocolVersion, got)
	}
}

func TestKBReadAndTraversal(t *testing.T) {
	ts, _ := fixture(t)
	res, _ := call(t, ts, "kb", map[string]any{"command": "read", "path": "README.md"})
	if res.IsError || !strings.Contains(firstText(res), "door document") {
		t.Fatalf("kb read: %+v", res)
	}
	res, _ = call(t, ts, "kb", map[string]any{"command": "read", "path": "../x"})
	if !res.IsError {
		t.Error("traversal accepted")
	}
}

func TestAllowListAndRawAccess(t *testing.T) {
	ts, _ := fixture(t)
	res, _ := call(t, ts, "kb", map[string]any{"command": "write", "path": "x.md", "content": "y"})
	if !res.IsError || !strings.Contains(firstText(res), "available") {
		t.Fatalf("kb write must be denied with list: %+v", res)
	}
	res, _ = call(t, ts, "kb", map[string]any{"command": "browse", "fullRawAccess": true})
	if res.IsError {
		t.Errorf("fullRawAccess browse failed: %s", firstText(res))
	}
}
