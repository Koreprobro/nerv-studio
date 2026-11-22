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

// The queue-file contract (SPECS §6.1): a path is the only launch input;
// file shapes are a JSON array of lines or the {"defaults"?, "jobs", "wave"?}
// envelope — anything else is rejected before the wave starts.
func TestExecuteQueueFileContract(t *testing.T) {
	ts, art := fixture(t)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": []any{map[string]any{"tool": "echo"}}})
	if !res.IsError || !strings.Contains(firstText(res), "FILE path") {
		t.Errorf("inline array must be retired: %s", firstText(res))
	}
	writeQueue(t, art, "envelope.json", `{"queue": [{"tool": "echo"}]}`)
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": "envelope.json"})
	if !res.IsError || !strings.Contains(firstText(res), `envelope needs`) {
		t.Errorf("envelope without jobs must be rejected: %s", firstText(res))
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

// A queue SET (SPECS §6.1): a comma-separated list of queue files merges
// in list order into one wave — the elephant is eaten in parts.
func TestExecuteQueueSet(t *testing.T) {
	ts, art := fixture(t)
	rel1 := writeQueue(t, art, "q1.json", `[{"tool":"echo","params":{"text":"one","output":"p1.txt"}}]`)
	rel2 := writeQueue(t, art, "q2.json", `[{"tool":"echo","params":{"text":"two","output":"p2.txt"}},
	                                       {"tool":"echo","params":{"text":"three","output":"p3.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel1 + ", " + rel2})
	if res.IsError {
		t.Fatalf("set launch: %s", firstText(res))
	}
	fin := awaitWave(t, ts, waveIDOf(t, res))
	if fin["ok"] != true {
		t.Errorf("set wave must be ok: %v", fin)
	}
	if n := len(fin["jobs"].([]any)); n != 3 {
		t.Errorf("merged wave must carry all 3 jobs: %v", fin["jobs"])
	}
	for _, f := range []struct{ name, want string }{{"p1.txt", "one"}, {"p2.txt", "two"}, {"p3.txt", "three"}} {
		if b, err := os.ReadFile(filepath.Join(art, f.name)); err != nil || string(b) != f.want {
			t.Errorf("%s: %v %q", f.name, err, b)
		}
	}
	// a broken member rejects the whole set by name, before any wave starts
	writeQueue(t, art, "q3.json", `[{"tool":"echo"}`)
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel1 + ",q3.json"})
	if !res.IsError || !strings.Contains(firstText(res), "q3.json") {
		t.Errorf("broken member must name the file: %s", firstText(res))
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "latest"})
	if res.IsError {
		t.Errorf("failed set launch must not open a wave (latest must stay the merged one): %s", firstText(res))
	}
}

// check dry-runs a queue set (SPECS §6.1): the same reader and admission
// as execute, but nothing launches — counts per file, would-be-skipped
// lines named, no wave in the ledger.
func TestCheckQueue(t *testing.T) {
	ts, art := fixture(t)
	rel1 := writeQueue(t, art, "c1.json", `[{"tool":"echo","params":{"text":"one","output":"c1.txt"}}]`)
	rel2 := writeQueue(t, art, "c2.json", `[{"tool":"ghost","params":{"output":"c2.txt"}},
	                                       {"tool":"echo","params":{"output":"c2b.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "check", "queue": rel1 + ", " + rel2})
	if res.IsError {
		t.Fatalf("check of a valid set: %s", firstText(res))
	}
	var rep struct {
		Files []struct {
			File string
			Jobs int
		} `json:"files"`
		Jobs     int `json:"jobs"`
		Admitted int `json:"admitted"`
		Skipped  []struct {
			Tool   string `json:"tool"`
			Reason string `json:"reason"`
		} `json:"skipped"`
	}
	if err := json.Unmarshal([]byte(firstText(res)), &rep); err != nil {
		t.Fatalf("check answer is not JSON: %v — %s", err, firstText(res))
	}
	if rep.Jobs != 3 || rep.Admitted != 2 || len(rep.Skipped) != 1 || rep.Skipped[0].Tool != "ghost" {
		t.Errorf("check report wrong: %s", firstText(res))
	}
	if len(rep.Files) != 2 || rep.Files[0].Jobs != 1 || rep.Files[1].Jobs != 2 {
		t.Errorf("per-file counts wrong: %s", firstText(res))
	}
	// nothing ran and no wave opened
	if _, err := os.Stat(filepath.Join(art, "c1.txt")); !os.IsNotExist(err) {
		t.Errorf("check must not run jobs: c1.txt exists")
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "latest"})
	if !res.IsError {
		t.Errorf("check must not open a wave: %s", firstText(res))
	}
	// a broken member answers isError and names the file
	writeQueue(t, art, "c3.json", `{"tool":"echo"}`) // object envelope, not a bare array
	res, _ = call(t, ts, "tools", map[string]any{"command": "check", "queue": rel1 + ",c3.json"})
	if !res.IsError || !strings.Contains(firstText(res), "c3.json") {
		t.Errorf("broken member must be named: %s", firstText(res))
	}
	// all lines unsuitable: execute would refuse, so check answers isError with the list
	writeQueue(t, art, "c4.json", `[{"tool":"ghost","params":{"output":"c4.txt"}}]`)
	res, _ = call(t, ts, "tools", map[string]any{"command": "check", "queue": "c4.json"})
	if !res.IsError || !strings.Contains(firstText(res), "ghost") {
		t.Errorf("zero-admission set must be isError with skipped: %s", firstText(res))
	}
}

func TestExecuteWaveArtifacts(t *testing.T) {
	ts, art := fixture(t)
	q := `[{"tool":"echo","params":{"text":"one","output":"a.txt"}},
	       {"tool":"echo","params":{"text":"two","output":"nest/artifactory/b.txt"}},
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
	if !strings.Contains(string(txt), `"/nest/artifactory/a.txt"`) {
		t.Errorf("artifactURL missing: %s", txt)
	}
	if !strings.Contains(string(txt), `"dur"`) {
		t.Errorf("dur missing: %s", txt)
	}
	if b, err := os.ReadFile(filepath.Join(art, "b.txt")); err != nil || string(b) != "two" {
		t.Errorf("normalized output not on disk: %v %q", err, b)
	}
	resp, err := http.Get(ts.URL + "/nest/artifactory/a.txt")
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

// Blocking mode without a progressToken still answers over one SSE stream
// (SPECS §6.6): headers flush before the wave, the final response is the
// stream's last event; with a token the same stream also carries progress
// (TestExecuteSSE).
func TestExecuteBlockingNoToken(t *testing.T) {
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

// F2: the volume guard caps both transports (SPECS §3.5) — a blocking wave
// answering over the SSE stream hits the cap on the final event too, and the
// full dump stays one reshoot-free lookup away.
func TestVolumeGuardSSE(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) {
		cfg.LLMOffload = true
		cfg.MaxOutputChars = 64
	}, cudaPool(1))
	writeTool(t, filepath.Join(filepath.Dir(art), "tools"), "gpuecho.sh",
		`{"summary":"one cuda gpu","fields":[{"name":"text","required":true},{"name":"output","io":"out"},
		   {"name":"gpu-require","default":1},{"name":"gpu-backend","default":"cuda"}]}`,
		`printf '%s' "$NERV_TEXT" > "$NERV_OUTPUT"`)
	rel := writeQueue(t, art, "q.json", `[{"tool":"gpuecho","params":{"text":"gpu","output":"g.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	txt := firstText(res)
	if !strings.Contains(txt, "[volume guard") {
		t.Fatalf("streamed wave result must hit the volume guard: %q", txt)
	}
	if !strings.Contains(txt, `execute wave="`) {
		t.Errorf("guard must point at the reshoot-free wave lookup: %q", txt)
	}
	// The lookup is guarded as well; volume_ack lifts the cap.
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "latest"})
	if !strings.Contains(firstText(res), "[volume guard") {
		t.Errorf("wave lookup must be guarded too: %q", firstText(res))
	}
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "wave": "latest", "volume_ack": true})
	if res.IsError || !strings.Contains(firstText(res), "finished") {
		t.Errorf("volume_ack must lift the cap: %q", firstText(res))
	}
}

// Async ownership (SPECS §6.6): the launch answer returns before the wave ends
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

// Ledger (SPECS §6.1): exact ids resolve, "latest" is the consumer view —
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

// Debug folds into the queue file (SPECS §6.6): --debug runs the wave log
// in the system temp dir and folds its tail into the queue file's wave
// block; the playground gets no stray .log files, and a reshoot simply
// restamps the block.
func TestDebugWaveLogs(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) { cfg.Debug = true }, nil)
	rel := writeQueue(t, art, "q.json", `[{"tool":"echo","params":{"text":"d","output":"dbg.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	id := waveIDOf(t, res)
	awaitWave(t, ts, id)
	w := readWaveBlock(t, art, "q.json")
	if dbg, _ := w["debug"].(string); !strings.Contains(dbg, `"event":"end"`) {
		t.Errorf("debug tail not folded into the queue file: %.200v", w["debug"])
	}
	if _, err := os.Stat(filepath.Join(art, "q.log")); !os.IsNotExist(err) {
		t.Errorf("playground wave log must be gone: %v", err)
	}
	// Reshoot: the block is replaced in place, nothing rotates.
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	id2 := waveIDOf(t, res)
	awaitWave(t, ts, id2)
	if w2 := readWaveBlock(t, art, "q.json"); w2["wave"] != id2 {
		t.Errorf("reshoot must restamp the block: %v", w2["wave"])
	}
	if _, err := os.Stat(filepath.Join(art, "q-1.log")); !os.IsNotExist(err) {
		t.Errorf("no rotation files beside the queue: %v", err)
	}
}

// readWaveBlock parses the wave block the daemon stamped into a queue file.
func readWaveBlock(t *testing.T, art, rel string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(art, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var obj struct {
		Wave map[string]any `json:"wave"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("queue file after wave: %v (%s)", err, raw)
	}
	if obj.Wave == nil {
		t.Fatalf("no wave block in queue file: %s", raw)
	}
	return obj.Wave
}

// The envelope form of a queue file (SPECS §6.1): defaults merge into every
// line — the tool fills silences, params merge key-wise with the line's
// values winning whole.
func TestQueueEnvelopeDefaults(t *testing.T) {
	ts, art := fixture(t)
	writeQueue(t, art, "env.json", `{
	  "defaults": {"tool": "echo", "params": {"text": "shared"}},
	  "jobs": [{"params": {"output": "e1.txt"}},
	           {"params": {"text": "own", "output": "e2.txt"}}]}`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": "env.json"})
	if res.IsError {
		t.Fatalf("envelope execute: %s", firstText(res))
	}
	awaitWave(t, ts, waveIDOf(t, res))
	for name, want := range map[string]string{"e1.txt": "shared", "e2.txt": "own"} {
		b, err := os.ReadFile(filepath.Join(art, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want %q", name, b, err, want)
		}
	}
}

// Scoped defaults (SPECS §6.1): a {"defaults": ...} line governs the lines
// that follow within its file — later declarations layer over earlier ones
// (and over the envelope base), the job line wins; the scope never crosses
// into the next file of the set, and directive lines are not jobs (the
// sub-ledger counts the jobs, not the array entries).
func TestQueueScopedDefaults(t *testing.T) {
	ts, art := fixture(t)
	q1 := writeQueue(t, art, "sd.json", `[
	  {"_comment": "batch header", "defaults": {"tool": "echo", "params": {"text": "h1"}}},
	  {"params": {"output": "sd1.txt"}},
	  {"defaults": {"params": {"text": "h2"}}},
	  {"params": {"output": "sd2.txt"}},
	  {"params": {"text": "own", "output": "sd3.txt"}}]`)
	q2 := writeQueue(t, art, "sd-b.json", `{"defaults": {"tool": "echo", "params": {"text": "per-file"}},
	  "jobs": [{"params": {"output": "sd4.txt"}},
	           {"defaults": {"params": {"text": "scoped"}}},
	           {"params": {"output": "sd8.txt"}}]}`)
	q3 := writeQueue(t, art, "sd-c.json", `[{"params": {"output": "sd6.txt"}},
	  {"tool": "echo", "params": {"output": "sd7.txt", "text": "plain"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": q1 + "," + q2 + "," + q3})
	if res.IsError {
		t.Fatalf("scoped defaults execute: %s", firstText(res))
	}
	awaitWave(t, ts, waveIDOf(t, res))
	for name, want := range map[string]string{
		"sd1.txt": "h1", "sd2.txt": "h2", "sd3.txt": "own",
		"sd4.txt": "per-file", "sd8.txt": "scoped", "sd7.txt": "plain",
	} {
		b, err := os.ReadFile(filepath.Join(art, name))
		if err != nil || string(b) != want {
			t.Errorf("%s = %q (%v), want %q", name, b, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(art, "sd6.txt")); !os.IsNotExist(err) {
		t.Errorf("defaults must not leak between files; the tool-less sd6 job should have skipped: %v", err)
	}
	w := readWaveBlock(t, art, "sd.json")
	if sub, _ := w["jobs"].([]any); len(sub) != 3 {
		t.Errorf("directive lines are not jobs: %v", w["jobs"])
	}
}

// The wave block lands back in the queue file (SPECS §6.1): the bare array
// is promoted to the envelope, verdict and sub-ledger ride in "wave", and
// resubmitting the stamped file reshoots the same recipe (the block is
// ignored on read).
func TestWaveBlockWriteBack(t *testing.T) {
	ts, art := fixture(t)
	rel := writeQueue(t, art, "q.json", `[{"tool":"echo","params":{"text":"a","output":"wb.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	id := waveIDOf(t, res)
	awaitWave(t, ts, id)
	w := readWaveBlock(t, art, "q.json")
	if w["wave"] != id || w["ok"] != true {
		t.Errorf("wave block verdict: %v", w)
	}
	jobs, _ := w["jobs"].([]any)
	if len(jobs) != 1 {
		t.Fatalf("sub-ledger size: %v", w["jobs"])
	}
	j0, _ := jobs[0].(map[string]any)
	if j0["status"] != "done" || j0["index"].(float64) != 0 {
		t.Errorf("sub-ledger line: %v", j0)
	}
	// The recipe survives the stamp: the stamped file reshoots as a fresh wave.
	res, _ = call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	id2 := waveIDOf(t, res)
	if id2 == id {
		t.Fatal("reshoot must start a fresh wave")
	}
	awaitWave(t, ts, id2)
	if w2 := readWaveBlock(t, art, "q.json"); w2["wave"] != id2 {
		t.Errorf("reshoot must restamp the queue file: %v", w2)
	}
}

// A "{{a|b}}" variant list rolls at launch: the backend receives the resolved
// string only, the stamped line keeps the template, and the stamp carries
// what was picked as "resolved" (SPECS §6.1).
func TestQueueVariants(t *testing.T) {
	ts, art := fixture(t)
	rel := writeQueue(t, art, "dyn.json", `[{"tool":"echo","params":{"text":"mood {{calm|storm}}","output":"d1.txt"}},{"tool":"echo","params":{"text":"{{plain}}","output":"d2.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	if res.IsError {
		t.Fatalf("execute with variants: %s", firstText(res))
	}
	awaitWave(t, ts, waveIDOf(t, res))
	b1, err := os.ReadFile(filepath.Join(art, "d1.txt"))
	if err != nil {
		t.Fatal(err)
	}
	s1 := strings.TrimSpace(string(b1))
	if s1 != "mood calm" && s1 != "mood storm" {
		t.Errorf("the fired string must be one option, got %q", s1)
	}
	b2, err := os.ReadFile(filepath.Join(art, "d2.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(b2)); got != "{{plain}}" {
		t.Errorf("the pipe-guard must leave {{plain}} literal, got %q", got)
	}
	// Lineage: the line keeps the recipe, the sub-ledger keeps the fire.
	raw, err := os.ReadFile(filepath.Join(art, "dyn.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs []struct {
			Params map[string]any `json:"params"`
		} `json:"jobs"`
		Wave struct {
			Jobs []map[string]any `json:"jobs"`
		} `json:"wave"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("stamped file: %v", err)
	}
	if len(doc.Jobs) != 2 || doc.Jobs[0].Params["text"] != "mood {{calm|storm}}" {
		t.Fatalf("the stamped line must keep the template: %s", raw)
	}
	if len(doc.Wave.Jobs) != 2 {
		t.Fatalf("sub-ledger: %v", doc.Wave.Jobs)
	}
	rv, _ := doc.Wave.Jobs[0]["resolved"].(map[string]any)
	if rv["text"] != s1 {
		t.Errorf("sub-ledger resolved = %v, fired was %q", rv["text"], s1)
	}
	if _, leaked := doc.Wave.Jobs[1]["resolved"]; leaked {
		t.Errorf("the guarded line must have no resolved: %v", doc.Wave.Jobs[1])
	}
}

// A "_comment" key is free annotation (SPECS §6.1): the scheduler ignores it
// on read (Check guards params keys only) and the wave stamp preserves it at
// both the envelope and the job level — the shoot-log annotation survives
// the fire, in the envelope form and in the promoted bare array alike.
func TestQueueCommentKeys(t *testing.T) {
	ts, art := fixture(t)
	q1 := writeQueue(t, art, "cmt.json", `{
	  "_comment": "wave: cover pass 1",
	  "jobs": [{"_comment": "board row S1", "tool": "echo", "params": {"text": "a", "output": "cmt1.txt"}}]}`)
	q2 := writeQueue(t, art, "cmt2.json", `[{"_comment": "cite S2", "tool": "echo", "params": {"text": "b", "output": "cmt2.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": q1 + "," + q2})
	if res.IsError {
		t.Fatalf("execute with _comment: %s", firstText(res))
	}
	awaitWave(t, ts, waveIDOf(t, res))
	for _, tt := range []struct{ file, envelope, line string }{
		{"cmt.json", "wave: cover pass 1", "board row S1"},
		{"cmt2.json", "", "cite S2"},
	} {
		raw, err := os.ReadFile(filepath.Join(art, tt.file))
		if err != nil {
			t.Fatal(err)
		}
		var obj struct {
			Comment string `json:"_comment"`
			Jobs    []struct {
				Comment string `json:"_comment"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(raw, &obj); err != nil {
			t.Fatalf("stamped %s: %v (%s)", tt.file, err, raw)
		}
		if obj.Comment != tt.envelope {
			t.Errorf("%s: envelope _comment = %q, want %q (%s)", tt.file, obj.Comment, tt.envelope, raw)
		}
		if len(obj.Jobs) != 1 || obj.Jobs[0].Comment != tt.line {
			t.Errorf("%s: job _comment must survive the stamp (%s)", tt.file, raw)
		}
	}
}

// A multi-file wave stamps every file with the shared wave id and its own
// sub-ledger on local indexes (SPECS §6.1).
func TestWaveBlockSubLedgers(t *testing.T) {
	ts, art := fixture(t)
	q1 := writeQueue(t, art, "s1.json", `[{"tool":"echo","params":{"text":"a","output":"s1a.txt"}},{"tool":"echo","params":{"text":"b","output":"s1b.txt"}}]`)
	q2 := writeQueue(t, art, "s2.json", `[{"tool":"echo","params":{"text":"c","output":"s2a.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": q1 + "," + q2})
	id := waveIDOf(t, res)
	awaitWave(t, ts, id)
	w1 := readWaveBlock(t, art, "s1.json")
	w2 := readWaveBlock(t, art, "s2.json")
	if w1["wave"] != id || w2["wave"] != id {
		t.Fatalf("both files must carry the shared wave id: %v / %v", w1["wave"], w2["wave"])
	}
	l1, _ := w1["jobs"].([]any)
	l2, _ := w2["jobs"].([]any)
	if len(l1) != 2 || len(l2) != 1 {
		t.Fatalf("sub-ledgers split by file: %d / %d", len(l1), len(l2))
	}
	if line, _ := l2[0].(map[string]any); line["index"].(float64) != 0 {
		t.Errorf("sub-ledger indexes are file-local: %v", l2[0])
	}
}

// POST /wave/stop — the red button (SPECS §6.6): it cancels the live wave,
// the queue file still receives its wave block, and pressing again answers
// idempotently.
func TestWaveStopEndpoint(t *testing.T) {
	ts, art, _ := fixtureTweak(t, func(cfg *config.Config) { cfg.LLMOffload = true }, nil)
	writeTool(t, filepath.Join(filepath.Dir(art), "tools"), "slow2.sh",
		`{"summary":"sleeps long","fields":[{"name":"output","io":"out"}]}`,
		`sleep 30; printf late > "$NERV_OUTPUT"`)
	rel := writeQueue(t, art, "q.json", `[{"tool":"slow2","params":{"output":"never.txt"}}]`)
	res, _ := call(t, ts, "tools", map[string]any{"command": "execute", "queue": rel})
	if res.IsError {
		t.Fatalf("async launch: %s", firstText(res))
	}
	id := waveIDOf(t, res)
	time.Sleep(150 * time.Millisecond) // let the runner pick the wave up
	resp, err := http.Post(ts.URL+"/wave/stop", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if body["stopped"] != true || body["wave"] != id {
		t.Fatalf("stop answer: %v", body)
	}
	fin := awaitWave(t, ts, id)
	if fin["ok"] == true {
		t.Errorf("stopped wave must be not-ok: %v", fin)
	}
	// Idempotence: the wave is over, the button answers without harm.
	resp, err = http.Post(ts.URL+"/wave/stop", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	body = nil
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if body["stopped"] != false {
		t.Errorf("stop after finish must be idempotent: %v", body)
	}
	// The queue file carries the cancelled wave block.
	if w := readWaveBlock(t, art, "q.json"); w["wave"] != id || w["ok"] == true {
		t.Errorf("cancelled wave block: %v", w)
	}
}
