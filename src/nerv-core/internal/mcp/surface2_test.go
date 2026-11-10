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
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nerv-core/internal/config"
)

// SSE rides the blocking mode only (§2.3): offload on + a GPU wave. The
// final response and the progress notifications share one stream.
func TestExecuteSSE(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) { cfg.LLMOffload = true }, cudaPool(1))
	writeTool(t, filepath.Join(filepath.Dir(art), "tools"), "gpuecho.sh",
		`{"summary":"one cuda gpu","fields":[{"name":"text","required":true},{"name":"output","io":"out"},
		   {"name":"gpu-require","default":1},{"name":"gpu-backend","default":"cuda"}]}`,
		`printf '%s' "$NERV_TEXT" > "$NERV_OUTPUT"`)
	rel := writeQueue(t, art, "sse-q.json", `[{"tool":"gpuecho","params":{"text":"sse","output":"sse.txt"}}]`)
	params := map[string]any{
		"name":      "tools",
		"arguments": map[string]any{"command": "execute", "queue": rel},
		"_meta":     map[string]any{"progressToken": "tok-1"},
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": `7`, "method": "tools/call", "params": params})
	resp, err := http.Post(ts.URL+"/mcp", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("SSE content-type: %q", ct)
	}
	all, _ := io.ReadAll(resp.Body)
	txt := string(all)
	if !strings.Contains(txt, "notifications/progress") || !strings.Contains(txt, "tok-1") {
		t.Errorf("progress notification missing: %.400s", txt)
	}
	if !strings.Contains(txt, "sse.txt") {
		t.Error("final response event missing")
	}
}

func TestDiceRoll(t *testing.T) {
	ts, _ := fixture(t)
	res, _ := call(t, ts, "dice.roll", map[string]any{"notation": "4d6kh3+2", "seed": 42})
	if res.IsError {
		t.Fatalf("dice: %s", firstText(res))
	}
	first := firstText(res)
	res2, _ := call(t, ts, "dice.roll", map[string]any{"notation": "4d6kh3+2", "seed": 42})
	if firstText(res2) != first {
		t.Error("same seed must replay identically")
	}
	var out struct {
		Total float64 `json:"total"`
	}
	if err := json.Unmarshal([]byte(first), &out); err != nil {
		t.Fatalf("dice result JSON: %v", err)
	}
	if out.Total < 5 || out.Total > 20 {
		t.Errorf("4d6kh3+2 total out of range: %v", out.Total)
	}
	res, _ = call(t, ts, "dice.roll", map[string]any{"notation": "2d6+xyz"})
	if !res.IsError {
		t.Error("bad notation accepted")
	}
}

func TestCalcResolution(t *testing.T) {
	ts, _ := fixture(t)
	res, _ := call(t, ts, "calc.resolution", map[string]any{"aspect": "16:9"})
	if res.IsError {
		t.Fatalf("calc.resolution: %s", firstText(res))
	}
	txt := firstText(res)
	// golden pair of the sh reference calculator (studio defaults)
	if !strings.Contains(txt, `"width": 1888`) || !strings.Contains(txt, `"height": 1056`) {
		t.Errorf("studio-spec 16:9 must be 1888x1056: %s", txt)
	}
	res, _ = call(t, ts, "calc.resolution", map[string]any{
		"aspect": "16:9", "multiple": 16, "targetMP": 1, "minSide": 256, "maxSide": 1024,
	})
	txt = firstText(res)
	if !strings.Contains(txt, `"width": 1344`) || !strings.Contains(txt, `"height": 752`) {
		t.Errorf("custom spec 16:9 must be 1344x752: %s", txt)
	}
	res, _ = call(t, ts, "calc.resolution", map[string]any{"aspect": "30:1"})
	if !res.IsError || !strings.Contains(firstText(res), "7680x256") {
		t.Errorf("side below the floor must error with the pair: %s", firstText(res))
	}
	res, _ = call(t, ts, "calc.resolution", map[string]any{})
	if !res.IsError {
		t.Error("missing aspect must error")
	}
}

func TestVolumeGuard(t *testing.T) {
	ts, art := fixture(t)
	if err := os.WriteFile(filepath.Join(art, "huge.txt"),
		[]byte(strings.Repeat("0123456789 ", 4200)), 0o644); err != nil { // ~42 KB
		t.Fatal(err)
	}
	res, _ := call(t, ts, "artifactory", map[string]any{
		"command": "read", "path": "huge.txt", "max_bytes": 64000})
	if !res.IsError || !strings.Contains(firstText(res), "volume_ack") {
		t.Fatalf("guard must refuse with the ack hint: %.120s", firstText(res))
	}
	res, _ = call(t, ts, "artifactory", map[string]any{
		"command": "read", "path": "huge.txt", "max_bytes": 64000, "volume_ack": true})
	if res.IsError {
		t.Errorf("volume_ack must lift the guard: %.120s", firstText(res))
	}
}

func TestGPUListHiddenButCallable(t *testing.T) {
	ts, _ := fixture(t)
	res, _ := call(t, ts, "gpu.list", nil)
	if res.IsError {
		t.Errorf("gpu.list call: %s", firstText(res))
	}
	out := rpc(t, ts, `9`, "tools/list", nil)
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "gpu.list") {
		t.Error("gpu.list leaked into tools/list")
	}
}

func TestUnknownToolAndMethod(t *testing.T) {
	ts, _ := fixture(t)
	out := rpc(t, ts, `3`, "tools/call", map[string]any{"name": "nope", "arguments": map[string]any{}})
	if out["error"] == nil {
		t.Error("unknown tool must be a protocol error")
	}
	out = rpc(t, ts, `4`, "bogus/method", nil)
	if out["error"] == nil {
		t.Error("unknown method must error")
	}
}
