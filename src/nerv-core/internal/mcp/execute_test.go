// SPDX-License-Identifier: AGPL-3.0-or-later
//
// nerv-core — omni-channel AI studio core: a thin MCP orchestrator of
// system-installed generation tools, a GPU scheduler and an artifact server.
// Copyright (C) 2026 Koreprobro
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful, but
// WITHOUT ANY WARRANTY; without even the implied warranty of
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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerv-core/internal/config"
)

// writeQueue puts a queue file into the artifactory and returns its path.
func writeQueue(t *testing.T, art, rel, content string) string {
	t.Helper()
	p := filepath.Join(art, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return rel
}

func viewJSON(t *testing.T, res ToolResult) map[string]any {
	t.Helper()
	var v map[string]any
	if err := json.Unmarshal([]byte(firstText(res)), &v); err != nil {
		t.Fatalf("bad wave view: %v (%s)", err, firstText(res))
	}
	return v
}

func waveIDOf(t *testing.T, res ToolResult) string {
	t.Helper()
	id, _ := viewJSON(t, res)["wave"].(string)
	if id == "" {
		t.Fatalf("no wave id in: %s", firstText(res))
	}
	return id
}

// awaitWave polls {wave: id} until the ledger reports the wave finished.
func awaitWave(t *testing.T, ts *httptest.Server, id string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "wave": id})
		if res.IsError {
			t.Fatalf("wave lookup: %s", firstText(res))
		}
		v := viewJSON(t, res)
		if v["status"] == "finished" {
			return v
		}
		if time.Now().After(deadline) {
			t.Fatalf("wave %s never finished: %v", id, v)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// The queue-file contract (§2.3, §4.9): a path is the only launch input,
// a bare JSON array the only file shape.
func TestExecuteQueueFileContract(t *testing.T) {
	ts, art := fixture(t)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": []any{map[string]any{"tool": "echo"}}})
	if !res.IsError || !strings.Contains(firstText(res), "FILE path") {
		t.Errorf("inline array must be retired: %s", firstText(res))
	}
	writeQueue(t, art, "envelope.json", `{"queue": [{"tool": "echo"}]}`)
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": "envelope.json"})
	if !res.IsError || !strings.Contains(firstText(res), "bare JSON array") {
		t.Errorf("envelope file must be rejected: %s", firstText(res))
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": "ghost.json"})
	if !res.IsError || !strings.Contains(firstText(res), "queue file") {
		t.Errorf("missing queue file: %s", firstText(res))
	}
	writeQueue(t, art, "empty.json", `[]`)
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": "empty.json"})
	if !res.IsError {
		t.Error("empty queue must error")
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": "../escape.json"})
	if !res.IsError {
		t.Error("escaping queue path must be rejected")
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute"})
	if !res.IsError || !strings.Contains(firstText(res), "execute needs") {
		t.Errorf("no-argument usage error: %s", firstText(res))
	}
}

func TestExecuteWaveArtifacts(t *testing.T) {
	ts, art := fixture(t)
	q := `[{"tool":"echo","params":{"text":"one","output":"a.txt"}},
	       {"tool":"echo","params":{"text":"two","output":"artifacts/b.txt"}},
	       {"tool":"ghost","params":{"output":"x.txt"}}]`
	rel := writeQueue(t, art, "q.json", q)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	if res.IsError {
		t.Fatalf("execute: %s", firstText(res))
	}
	v := viewJSON(t, res)
	if v["mode"] != "async" || !strings.Contains(firstText(res), "ghost") {
		t.Fatalf("async wave answer: %s", firstText(res))
	}
	fin := awaitWave(t, ts, waveIDOf(t, res))
	txt, _ := json.Marshal(fin)
	if fin["ok"] != true {
		t.Errorf("wave must be ok: %s", txt)
	}
	if !strings.Contains(string(txt), `"/artifacts/a.txt"`) {
		t.Errorf("artifactURL missing: %s", txt)
	}
	if !strings.Contains(string(txt), `"dur"`) {
		t.Errorf("dur missing: %s", txt)
	}
	if b, err := os.ReadFile(filepath.Join(art, "b.txt")); err != nil || string(b) != "two" {
		t.Errorf("normalized output not on disk: %v %q", err, b)
	}
	resp, err := http.Get(ts.URL + "/artifacts/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(body) != "one" {
		t.Errorf("artifactURL fetch: %d %q", resp.StatusCode, body)
	}
}

func TestExecuteArchivesOldOutput(t *testing.T) {
	ts, art := fixture(t)
	if err := os.WriteFile(filepath.Join(art, "a.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel := writeQueue(t, art, "q.json", `[{"tool":"echo","params":{"text":"fresh","output":"a.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	fin := awaitWave(t, ts, waveIDOf(t, res))
	txt, _ := json.Marshal(fin)
	if !strings.Contains(string(txt), "a-1.txt") {
		t.Errorf("wave must report the archived old file: %s", txt)
	}
	if b, _ := os.ReadFile(filepath.Join(art, "a-1.txt")); string(b) != "stale" {
		t.Errorf("old file must live at a-1.txt: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(art, "a.txt")); string(b) != "fresh" {
		t.Errorf("declared name must hold the new file: %q", b)
	}
}

func TestToolsProbeHiddenAndCapacity(t *testing.T) {
	ts, _ := fixture(t)
	res, _ := call(t, ts, "tools", map[string]any{"command": "probe", "path": "echo"})
	if res.IsError {
		t.Fatalf("probe: %s", firstText(res))
	}
	txt := firstText(res)
	if strings.Contains(txt, `"seed"`) {
		t.Errorf("hidden field leaked: %s", txt)
	}
	if !strings.Contains(txt, "unlimited") {
		t.Errorf("CPU tool capacity must be unlimited: %s", txt)
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "probe", "path": "echo", "fullRawAccess": true})
	if !strings.Contains(firstText(res), `"seed"`) {
		t.Errorf("fullRawAccess must reveal hidden fields: %s", firstText(res))
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "probe", "path": "ghost"})
	if !res.IsError {
		t.Error("unknown tool probe must error")
	}
}

func TestExecuteFailedWave(t *testing.T) {
	ts, art := fixture(t)
	writeTool(t, filepath.Join(filepath.Dir(art), "tools"), "ghostout.sh",
		`{"summary":"claims an output it never writes","fields":[{"name":"output","io":"out"}]}`,
		`true`)
	rel := writeQueue(t, art, "q.json", `[{"tool":"ghostout","params":{"output":"never.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	fin := awaitWave(t, ts, waveIDOf(t, res))
	if fin["ok"] == true {
		t.Errorf("failed wave must be not-ok: %v", fin)
	}
	if !strings.Contains(firstText(mustLookup(t, ts, fin)), "failed") {
		t.Errorf("status failed missing: %v", fin)
	}
}

func mustLookup(t *testing.T, ts *httptest.Server, view map[string]any) ToolResult {
	t.Helper()
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "wave": view["wave"]})
	return res
}

// Blocking mode without a progressToken answers plain JSON when the wave
// ends (§2.3); with a token the same wave streams SSE (TestExecuteSSE).
func TestExecuteBlockingPlainJSON(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) { cfg.LLMOffload = true }, cudaPool(1))
	writeTool(t, filepath.Join(filepath.Dir(art), "tools"), "gpuecho.sh",
		`{"summary":"one cuda gpu","fields":[{"name":"text","required":true},{"name":"output","io":"out"},
		   {"name":"gpu-require","default":1},{"name":"gpu-backend","default":"cuda"}]}`,
		`printf '%s' "$NERV_TEXT" > "$NERV_OUTPUT"`)
	rel := writeQueue(t, art, "q.json", `[{"tool":"gpuecho","params":{"text":"gpu","output":"g.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	if res.IsError {
		t.Fatalf("blocking execute: %s", firstText(res))
	}
	v := viewJSON(t, res)
	if v["mode"] != "blocking" || v["status"] != "finished" || v["ok"] != true {
		t.Errorf("plain-JSON blocking answer: %s", firstText(res))
	}
	if b, err := os.ReadFile(filepath.Join(art, "g.txt")); err != nil || string(b) != "gpu" {
		t.Errorf("blocking wave output: %v %q", err, b)
	}
}

// Async ownership (§2.3): the launch answer returns before the wave ends
// and the wave keeps running after the launching request is gone.
func TestExecuteAsyncSurvivesDisconnect(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) { cfg.LLMOffload = true }, nil)
	writeTool(t, filepath.Join(filepath.Dir(art), "tools"), "slow.sh",
		`{"summary":"sleeps then writes","fields":[{"name":"output","io":"out"}]}`,
		`sleep 0.4; printf slow > "$NERV_OUTPUT"`)
	rel := writeQueue(t, art, "q.json", `[{"tool":"slow","params":{"output":"s.txt"}}]`)
	t0 := time.Now()
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	if time.Since(t0) > 300*time.Millisecond {
		t.Errorf("async launch blocked the caller (%s)", time.Since(t0))
	}
	if res.IsError || viewJSON(t, res)["mode"] != "async" {
		t.Fatalf("CPU queue under offload must ride async: %s", firstText(res))
	}
	awaitWave(t, ts, waveIDOf(t, res))
	if b, err := os.ReadFile(filepath.Join(art, "s.txt")); err != nil || string(b) != "slow" {
		t.Errorf("async output after the request died: %v %q", err, b)
	}
}

// Ledger (§2.2): exact ids resolve, "latest" is the consumer view —
// raw-tagged waves hide from it and reappear under fullRawAccess.
func TestWaveLedgerLatestAndTag(t *testing.T) {
	ts, art := fixture(t)
	rel := writeQueue(t, art, "q.json", `[{"tool":"echo","params":{"text":"a","output":"l1.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	id1 := waveIDOf(t, res)
	awaitWave(t, ts, id1)
	rel2 := writeQueue(t, art, "q2.json", `[{"tool":"echo","params":{"text":"b","output":"l2.txt"}}]`)
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel2, "fullRawAccess": true})
	id2 := waveIDOf(t, res)
	if id2 == id1 {
		t.Fatal("wave ids must differ")
	}
	awaitWave(t, ts, id2)
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "latest"})
	if viewJSON(t, res)["wave"] != id1 {
		t.Errorf("consumer latest must skip the raw-tagged wave: %s", firstText(res))
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "latest", "fullRawAccess": true})
	if viewJSON(t, res)["wave"] != id2 {
		t.Errorf("raw latest must see the raw-tagged wave: %s", firstText(res))
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "nope-1"})
	if !res.IsError {
		t.Error("unknown wave id must error")
	}
}

// Debug wave logs through the surface (§2.7): --debug adds the log pointer,
// the file lands beside the queue file, and a second wave of the same queue
// rotates the old log out through the archive mechanism.
func TestDebugWaveLogs(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) { cfg.Debug = true }, nil)
	rel := writeQueue(t, art, "q.json", `[{"tool":"echo","params":{"text":"d","output":"dbg.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	v := viewJSON(t, res)
	if v["log"] != "q.log" {
		t.Fatalf("log pointer wrong: %s", firstText(res))
	}
	awaitWave(t, ts, waveIDOf(t, res))
	first, err := os.ReadFile(filepath.Join(art, "q.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), `"event":"end"`) {
		t.Errorf("wave log incomplete: %s", first)
	}
	// Second wave on the same queue: the old log must archive to q-1.log.
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	awaitWave(t, ts, waveIDOf(t, res))
	if _, err := os.Stat(filepath.Join(art, "q-1.log")); err != nil {
		t.Errorf("old wave log not rotated: %v", err)
	}
}
