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
			MountName:   "artifactory",
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

// rpc POSTs one request and decodes the JSON response. A blocking execute
// answers over SSE (SPECS §6.6): the JSON-RPC response is then the payload
// of the stream's last data: event — comments and notifications fall away.
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
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		all = sseLastData(string(all))
	}
	var out map[string]any
	if err := json.Unmarshal(all, &out); err != nil {
		t.Fatalf("response not JSON (%s): %q", resp.Header.Get("Content-Type"), string(all)[:min(200, len(all))])
	}
	return out
}

// sseLastData returns the payload of the last "data:" line of an SSE body
// (the JSON-RPC response; keepalive comments and progress notifications of
// a request without a token fall away).
func sseLastData(txt string) []byte {
	var last []byte
	for _, line := range strings.Split(txt, "\n") {
		if s, ok := strings.CutPrefix(line, "data: "); ok {
			last = []byte(s)
		}
	}
	return last
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

// TestRedactionBoundary guards SPECS §3.6 at the response boundary:
// absolute roots must never reach the client — neither inside error
// texts (the OWU-chat evidence: "open /home/.../kb/protocol-nsfw") nor
// inside file content read back verbatim.
func TestRedactionBoundary(t *testing.T) {
	ts, art := fixture(t)

	// Error text: a missing kb document names its root by tag.
	res, _ := call(t, ts, "kb", map[string]any{"command": "read", "path": "protocol-nsfw"})
	if !res.IsError {
		t.Fatalf("kb read of a missing path must error: %+v", res)
	}
	if txt := firstText(res); !strings.Contains(txt, "<kb>/protocol-nsfw") || strings.Contains(txt, "/kb/") {
		t.Fatalf("kb error must redact the root to its tag: %q", txt)
	}

	// Error text on the playground root: tag, not the absolute dir.
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "browse", "path": "no-such-dir"})
	if txt := firstText(res); !strings.Contains(txt, "<artifactory>/") || strings.Contains(txt, art) {
		t.Fatalf("artifactory browse error must redact the root: %q", txt)
	}

	// File content: a document quoting an absolute path is mapped on read.
	if err := os.WriteFile(filepath.Join(art, "quote.txt"),
		[]byte("seen at "+art+"/x.png\\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "read", "path": "quote.txt"})
	if res.IsError {
		t.Fatalf("read failed: %s", firstText(res))
	}
	if txt := firstText(res); strings.Contains(txt, art) || !strings.Contains(txt, "<artifactory>/x.png") {
		t.Fatalf("read content must be redacted at the boundary: %q", txt)
	}

	// Jail rule (SPECS §3.6): paths outside every mounted root vanish too
	// — full server paths survive only in debug artifacts. Tagged tails,
	// URLs and /nest/ links must come back untouched.
	body := "see /var/log/server.log and " + os.TempDir() + "/w.log and " +
		"http://127.0.0.1:9696/nest/artifactory/img.png plus /nest/artifactory/img.png plus /deep/dir/"
	if err := os.WriteFile(filepath.Join(art, "jail.txt"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "read", "path": "jail.txt"})
	txt := firstText(res)
	for _, want := range []string{"<path>/server.log", "<tmp>/w.log",
		"http://127.0.0.1:9696/nest/artifactory/img.png", "/nest/artifactory/img.png", "<path>/dir/"} {
		if !strings.Contains(txt, want) {
			t.Fatalf("jail scrub must keep %q, got %q", want, txt)
		}
	}
	for _, leak := range []string{"/var/log", "/deep", os.TempDir() + "/w.log"} {
		if strings.Contains(txt, leak) {
			t.Fatalf("jail scrub leaked %q in %q", leak, txt)
		}
	}
}

// TestReadFetchGates guards the two doors (SPECS §3.3): read is the text
// door and fetch the media door — each refuses the other's content with a
// pointer instead of ingesting mojibake or paying block format for prose.
func TestReadFetchGates(t *testing.T) {
	ts, art := fixture(t)
	png := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x0d}
	if err := os.WriteFile(filepath.Join(art, "shot.png"), png, 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ := call(t, ts, "artifactory", map[string]any{"command": "read", "path": "shot.png"})
	if txt := firstText(res); !res.IsError || !strings.Contains(txt, "use fetch") {
		t.Fatalf("read of a binary must point at fetch: %+v %q", res, txt)
	}
	if err := os.WriteFile(filepath.Join(art, "note.md"), []byte("# note\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "fetch", "path": "note.md"})
	if txt := firstText(res); !res.IsError || !strings.Contains(txt, "use read") {
		t.Fatalf("fetch of text must point at read: %+v %q", res, txt)
	}
	// The right-hand side of each door still works.
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "read", "path": "note.md"})
	if res.IsError || !strings.Contains(firstText(res), "# note") {
		t.Fatalf("read of text must pass: %+v %q", res, firstText(res))
	}
}

func TestDomainHelp(t *testing.T) {
	ts, _ := fixture(t)
	// help is always available, even ungranted, and lists only what works.
	res, _ := call(t, ts, "kb", map[string]any{"command": "help"})
	txt := firstText(res)
	if res.IsError || !strings.Contains(txt, "  read —") || !strings.Contains(txt, "  probe —") {
		t.Fatalf("kb help: %+v %q", res, txt)
	}
	if strings.Contains(txt, "write") || strings.Contains(txt, "execute") {
		t.Errorf("kb help lists ungranted commands: %q", txt)
	}
	// The toolbox listing carries the wave pair and no plain-directory noise.
	res, _ = call(t, ts, "tools", map[string]any{"command": "help"})
	txt = firstText(res)
	if res.IsError || !strings.Contains(txt, "  execute —") || !strings.Contains(txt, "  check —") {
		t.Fatalf("toolbox help must list the wave pair: %+v %q", res, txt)
	}
	// On a toolbox, probe help speaks the tool-info dialect.
	res, _ = call(t, ts, "tools", map[string]any{"command": "help", "path": "probe"})
	if res.IsError || !strings.Contains(firstText(res), "tool's card") {
		t.Errorf("toolbox probe help: %+v %q", res, firstText(res))
	}
	// Full method info names the parameters the daemon actually reads.
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "help", "path": "fetch"})
	txt = firstText(res)
	if res.IsError || !strings.Contains(txt, "max_block_bytes") || !strings.Contains(txt, "seek") {
		t.Errorf("fetch help: %+v %q", res, txt)
	}
	// Unknown method: an error that lists what is available.
	res, _ = call(t, ts, "artifactory", map[string]any{"command": "help", "path": "nope"})
	txt = firstText(res)
	if !res.IsError || !strings.Contains(txt, "available:") || !strings.Contains(txt, "fetch") {
		t.Errorf("unknown method: %+v %q", res, txt)
	}
	// tools/list advertises help in the command enum.
	out := rpc(t, ts, `2`, "tools/list", nil)
	resMap, _ := out["result"].(map[string]any)
	b, _ := json.Marshal(resMap["tools"])
	var tools []toolDef
	if err := json.Unmarshal(b, &tools); err != nil {
		t.Fatal(err)
	}
	for _, td := range tools {
		if td.Name != "artifactory" {
			continue
		}
		props, _ := td.InputSchema["properties"].(map[string]any)
		cmdProp, _ := props["command"].(map[string]any)
		arr, _ := cmdProp["enum"].([]any)
		for _, v := range arr {
			if v == "help" {
				return
			}
		}
		t.Errorf("artifactory command enum lacks help: %v", arr)
	}
}
