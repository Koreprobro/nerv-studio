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

package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/store"
)

// writeTool creates a fixture tool: `info` is its --tool-info JSON, `body`
// the run branch.
func writeTool(t *testing.T, toolsDir, name, info, body string) {
	t.Helper()
	path := filepath.Join(toolsDir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	script := "#!/usr/bin/env sh\ncase \"${1:-}\" in\n  --tool-info) cat <<'INFOEOF'\n" +
		info + "\nINFOEOF\n    ;;\n  *) " + body + " ;;\nesac\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

const outInfo = `{"summary":"writes text to output","fields":[
	{"name":"text"},{"name":"output","io":"out"}]}`

const gpuInfo = `{"summary":"needs one cuda gpu","fields":[
	{"name":"output","io":"out"},
	{"name":"gpu-require","default":1},
	{"name":"gpu-backend","default":"cuda"}]}`

const pipeInfo = `{"summary":"joins two inputs","fields":[
	{"name":"a","io":"in"},{"name":"b","io":"in"},{"name":"output","io":"out"}]}`

const envInfo = `{"summary":"dumps env","fields":[
	{"name":"output","io":"out"},{"name":"tags"},{"name":"steps"},
	{"name":"gpu-require","default":1},{"name":"gpu-backend","default":"cuda"}]}`

type fix struct {
	s   *Scheduler
	art store.Store
}

func newFix(t *testing.T, pool *gpu.Pool) *fix {
	t.Helper()
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	art := filepath.Join(root, "art")
	for _, d := range []string{tools, art} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	s := New(Deps{
		ToolsDir:    tools,
		Artifactory: store.Store{Root: art},
		Pool:        pool,
		UserEnv:     map[string]string{"OPVAR": "op-value"},
	})
	return &fix{s: s, art: s.Deps.Artifactory}
}

func (f *fix) tool(t *testing.T, name, info, body string) {
	writeTool(t, f.s.Deps.ToolsDir, name, info, body)
}

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

func run(t *testing.T, f *fix, queue []job.Job) Result {
	t.Helper()
	return f.s.Run(context.Background(), queue, nil)
}

func resultAt(r Result, idx int) JobResult {
	for _, j := range r.Jobs {
		if j.Index == idx {
			return j
		}
	}
	return JobResult{Status: "missing"}
}

func TestFileExistenceDataflow(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "echo.sh", outInfo, `printf 'hi' > "$NERV_OUTPUT"`)
	f.tool(t, "join.sh", pipeInfo, `cat "$NERV_A" "$NERV_B" > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{
		{Tool: "echo", Params: map[string]any{"text": "1", "output": "artifacts/a.txt"}},
		{Tool: "echo.sh", Params: map[string]any{"text": "2", "output": "b.txt"}},
		{Tool: "join.sh", Params: map[string]any{"a": "a.txt", "b": "b.txt", "output": "c.txt"}},
	})
	if !r.OK() || len(r.Skipped) != 0 {
		t.Fatalf("want all done: %+v skipped %+v", r.Jobs, r.Skipped)
	}
	if got := resultAt(r, 2).Dur; got == "" {
		t.Error("per-job dur must be reported")
	}
	if data, err := os.ReadFile(f.art.Abs("c.txt")); err != nil || string(data) != "hihi" {
		t.Errorf("c.txt = %q, %v", data, err)
	}
}

func TestExclusiveSlots(t *testing.T) {
	f := newFix(t, cudaPool(2))
	f.tool(t, "spin.sh", gpuInfo, `sleep 0.3; printf x > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{
		{Tool: "spin.sh", Params: map[string]any{"output": "1.txt"}},
		{Tool: "spin.sh", Params: map[string]any{"output": "2.txt"}},
	})
	a, b := resultAt(r, 0), resultAt(r, 1)
	if a.Status != "done" || b.Status != "done" {
		t.Fatalf("both must finish: %+v", r)
	}
	if len(a.Slots) != 1 || len(b.Slots) != 1 || a.Slots[0] == b.Slots[0] {
		t.Errorf("two gpu-require-1 jobs must capture distinct slots: %v vs %v", a.Slots, b.Slots)
	}
}

func TestAdmissionFailFast(t *testing.T) {
	f := newFix(t, cudaPool(2))
	f.tool(t, "spin.sh", gpuInfo, `printf x > "$NERV_OUTPUT"`)
	f.tool(t, "rocmish.sh", strings.Replace(gpuInfo, `"cuda"`, `"rocm"`, 1),
		`printf x > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{
		{Tool: "rocmish.sh", Params: map[string]any{"output": "r.txt"}}, // no rocm in pool
		{Tool: "spin.sh", Params: map[string]any{"output": "p.txt", "gpu-pin": "cuda:7"}},
		{Tool: "spin.sh", Params: map[string]any{"output": "m.txt", "gpu-require": float64(3)}},
		{Tool: "spin.sh", Params: map[string]any{"output": "ok.txt"}},
	})
	if len(r.Skipped) != 3 {
		t.Fatalf("backend miss, pin miss and over-capacity must skip: %+v", r.Skipped)
	}
	if !strings.Contains(r.Skipped[0].Reason, "rocm") ||
		!strings.Contains(r.Skipped[1].Reason, "gpu-pin") ||
		!strings.Contains(r.Skipped[2].Reason, "exceeds") {
		t.Errorf("skip reasons: %+v", r.Skipped)
	}
	if resultAt(r, 3).Status != "done" {
		t.Error("the healthy line must still run: skipped lines do not poison the wave")
	}
}

func TestExternalSlotsAnonymous(t *testing.T) {
	p, _, err := gpu.Build("", "rocm@farm:2", map[string]gpu.Enumerator{})
	if err != nil {
		t.Fatal(err)
	}
	f := newFix(t, p)
	f.tool(t, "spin.sh", gpuInfo, `printf x > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{
		{Tool: "spin.sh", Params: map[string]any{"output": "1.txt", "gpu-backend": "rocm"}},
		{Tool: "spin.sh", Params: map[string]any{"output": "2.txt", "gpu-backend": "rocm"}},
	})
	if !r.OK() || len(r.Jobs) != 2 {
		t.Fatalf("external slots must schedule without any detection: %+v", r)
	}
	if resultAt(r, 0).Slots[0] != "rocm@farm:0" {
		t.Errorf("external slot identity: %v", resultAt(r, 0).Slots)
	}
}

func TestStuckAndGhost(t *testing.T) {
	f := newFix(t, cudaPool(1))
	f.tool(t, "join.sh", pipeInfo, `cat "$NERV_A" > "$NERV_OUTPUT"`)
	f.tool(t, "ghost.sh", outInfo, `true`) // exit 0, writes nothing
	r := run(t, f, []job.Job{
		{Tool: "join.sh", Params: map[string]any{"a": "gone.png", "b": "gone2.png", "output": "x.txt"}},
		{Tool: "ghost.sh", Params: map[string]any{"text": "t", "output": "never-written.txt"}},
	})
	if s := resultAt(r, 0); s.Status != string(job.Stuck) || !strings.Contains(s.Error, "gone.png") {
		t.Errorf("missing inputs must land as stuck: %+v", s)
	}
	if s := resultAt(r, 1); s.Status != string(job.Failed) || !strings.Contains(s.Error, "wrote no output") {
		t.Errorf("exit 0 without the declared output must fail: %+v", s)
	}
	if r.OK() {
		t.Error("stuck/failed jobs must poison OK")
	}
}

func TestSkippedLines(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "echo.sh", outInfo, `printf x > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{
		{Tool: "echo.sh", Params: map[string]any{"text": "1", "output": "a.txt", "tetx": "typo"}},
		{Tool: "no-such.sh", Params: map[string]any{"output": "b.txt"}},
		{Tool: "echo.sh", Params: map[string]any{"text": "3"}}, // no output
	})
	if len(r.Skipped) != 3 || len(r.Jobs) != 0 {
		t.Fatalf("all three lines must skip: %+v / %+v", r.Skipped, r.Jobs)
	}
	if !strings.Contains(r.Skipped[0].Reason, "tetx") ||
		!strings.Contains(r.Skipped[1].Reason, "not found") ||
		!strings.Contains(r.Skipped[2].Reason, "output") {
		t.Errorf("skip reasons: %+v", r.Skipped)
	}
}

// TestEnvContract pins the slim scheduler↔sh contract (SPECS §5): both io
// marks arrive absolute, the directory roots and the scheduler-consumed
// gpu-* params are absent, physical slot ids flow unpinned into the backend
// env (no re-indexing), user env passes through, arrays comma-join.
func TestEnvContract(t *testing.T) {
	enums := map[string]gpu.Enumerator{"cuda": func() ([]gpu.Slot, error) {
		return []gpu.Slot{{Backend: "cuda", Index: 1}, {Backend: "cuda", Index: 3}}, nil
	}}
	p, _, err := gpu.Build("cuda:1", "", enums) // pool keeps physical index 1
	if err != nil {
		t.Fatal(err)
	}
	f := newFix(t, p)
	f.tool(t, "envdump.sh", envInfo, `env | sort > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{{Tool: "envdump.sh", Params: map[string]any{
		"output": "env.txt", "tags": []any{"a", "b"}, "steps": float64(20),
	}}})
	if !r.OK() || len(r.Jobs) != 1 {
		t.Fatalf("envdump must run (skipped? %+v): %+v", r.Skipped, r)
	}
	data, err := os.ReadFile(f.art.Abs("env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	for _, want := range []string{
		"NERV_OUTPUT=" + f.art.Root + "/env.txt", // io:"out" arrives absolute
		"PATH=/",
		"OPVAR=op-value",
		"CUDA_VISIBLE_DEVICES=1", // physical index, not a re-indexed 0
		"NERV_TAGS=a,b",
		"NERV_STEPS=20",
	} {
		if !strings.Contains(env, want) {
			t.Errorf("env must contain %q; got:\n%s", want, env)
		}
	}
	for _, ban := range []string{
		"NERV_ARTIFACTORY_DIR", "NERV_TOOLS_DIR", "NERV_KB_DIR", "NERV_LORA_MODEL_DIR",
		"NERV_GPU_REQUIRE", "NERV_GPU_BACKEND", // scheduler fields stay in the core
	} {
		if strings.Contains(env, ban) {
			t.Errorf("env must not carry %s (slim contract); got:\n%s", ban, env)
		}
	}
}

// TestCalculatorEnv pins §7.3: a tool declaring the aspect-ratio + target-mpx
// pair gets the core-computed NERV_CALCULATED_WIDTH/HEIGHT; a tool declaring
// only one of the pair gets neither; a broken aspect fails the job.
func TestCalculatorEnv(t *testing.T) {
	f := newFix(t, cudaPool(0))
	const calcInfo = `{"summary":"draws at a computed size","fields":[
	{"name":"aspect-ratio","default":"16:9"},{"name":"target-mpx","default":2},
	{"name":"output","io":"out"}]}`
	f.tool(t, "calc.sh", calcInfo, `env | sort > "$NERV_OUTPUT"`)
	f.tool(t, "half.sh", `{"summary":"half a pair","fields":[
	{"name":"aspect-ratio","default":"16:9"},{"name":"output","io":"out"}]}`,
		`env | sort > "$NERV_OUTPUT"`)
	f.tool(t, "under.sh", `{"summary":"underscore spelling counts too","fields":[
	{"name":"aspect_ratio","default":"16:9"},{"name":"target_mpx","default":2},
	{"name":"output","io":"out"}]}`,
		`env | sort > "$NERV_OUTPUT"`)
	r := run(t, f, []job.Job{
		{Tool: "calc.sh", Params: map[string]any{"output": "c.txt"}},
		{Tool: "half.sh", Params: map[string]any{"output": "h.txt"}},
		{Tool: "calc.sh", Params: map[string]any{"output": "b.txt", "aspect-ratio": "16x9-broken"}},
		{Tool: "under.sh", Params: map[string]any{"output": "u.txt"}},
	})
	if len(r.Jobs) != 4 {
		t.Fatalf("four jobs expected: %+v skipped %+v", r.Jobs, r.Skipped)
	}
	data, err := os.ReadFile(f.art.Abs("c.txt"))
	if err != nil {
		t.Fatal(err)
	}
	// Studio spec (multiple 32, target 2 MP) over 16:9 → 1888x1056.
	for _, want := range []string{"NERV_CALCULATED_WIDTH=1888", "NERV_CALCULATED_HEIGHT=1056"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("calc env must contain %q; got:\n%s", want, data)
		}
	}
	half, err := os.ReadFile(f.art.Abs("h.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(half), "NERV_CALCULATED_") {
		t.Errorf("a half-declared pair must not trigger the calculator:\n%s", half)
	}
	under, err := os.ReadFile(f.art.Abs("u.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(under), "NERV_CALCULATED_WIDTH=1888") {
		t.Errorf("underscore spelling must trigger the calculator too:\n%s", under)
	}
	if resultAt(r, 2).Status != string(job.Failed) ||
		!strings.Contains(resultAt(r, 2).Error, "resolution calculator") {
		t.Errorf("broken aspect must fail the job: %+v", resultAt(r, 2))
	}
}

func TestCancelAbortsPending(t *testing.T) {
	f := newFix(t, cudaPool(1))
	f.tool(t, "spin.sh", gpuInfo, `sleep 0.4; printf x > "$NERV_OUTPUT"`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// cancel mid-first-wave: the second job must never start
		<-time.After(80 * time.Millisecond)
		cancel()
	}()
	res := f.s.Run(ctx, []job.Job{
		{Tool: "spin.sh", Params: map[string]any{"output": "1.txt"}},
		{Tool: "spin.sh", Params: map[string]any{"output": "2.txt"}},
	}, nil)
	if res.OK() {
		t.Error("canceled wave must not report OK")
	}
	anyCanceled := false
	for _, j := range res.Jobs {
		if strings.Contains(j.Error, "canceled") {
			anyCanceled = true
		}
	}
	if !anyCanceled {
		t.Errorf("some line must carry the cancel reason: %+v", res.Jobs)
	}
}

// The core archives an existing same-name output to name-<i>.ext before the
// script starts (SPECS §6): the consumer gets exactly the declared name.
func TestWaveArchivesOldOutput(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "echo.sh", outInfo, `printf '%s' "$NERV_TEXT" > "$NERV_OUTPUT"`)
	if err := os.WriteFile(f.art.Abs("out.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	q := []job.Job{{Tool: "echo", Params: map[string]any{"text": "new", "output": "out.txt"}}}
	res := f.s.Run(context.Background(), q, nil)
	if !res.OK() {
		t.Fatalf("wave: %+v", res.Jobs)
	}
	if got := res.Jobs[0].Archived; len(got) != 1 || got[0] != "out-1.txt" {
		t.Errorf("archived: got %v, want [out-1.txt]", got)
	}
	if b, _ := os.ReadFile(f.art.Abs("out.txt")); string(b) != "new" {
		t.Errorf("declared name must hold the new file: %q", b)
	}
	if b, _ := os.ReadFile(f.art.Abs("out-1.txt")); string(b) != "old" {
		t.Errorf("archived content: %q", b)
	}
	// Second roll indexes on.
	q2 := []job.Job{{Tool: "echo", Params: map[string]any{"text": "newer", "output": "out.txt"}}}
	res = f.s.Run(context.Background(), q2, nil)
	if got := res.Jobs[0].Archived; len(got) != 1 || got[0] != "out-2.txt" {
		t.Errorf("second archive: got %v, want [out-2.txt]", got)
	}
}

// An output that is also an input of the same job is the tool's own
// overwrite territory: no archive (the script must still find its input).
func TestInPlaceOutputSkipsArchive(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "inplace.sh",
		`{"summary":"rewrites its input in place","fields":[{"name":"src","io":"in"},{"name":"output","io":"out"}]}`,
		`v=$(cat "$NERV_SRC"); printf '%s+' "$v" > "$NERV_OUTPUT"`)
	if err := os.WriteFile(f.art.Abs("p.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.s.Run(context.Background(), []job.Job{
		{Tool: "inplace", Params: map[string]any{"src": "p.txt", "output": "p.txt"}},
	}, nil)
	if !res.OK() {
		t.Fatalf("wave: %+v", res.Jobs)
	}
	if len(res.Jobs[0].Archived) != 0 {
		t.Errorf("in-place job must not archive: %v", res.Jobs[0].Archived)
	}
	if b, _ := os.ReadFile(f.art.Abs("p.txt")); string(b) != "old+" {
		t.Errorf("in-place content: %q", b)
	}
	if f.art.Exists("p-1.txt") {
		t.Error("p-1.txt must not exist")
	}
}

// io:"out" is the contract, not the field name: "output" holds no privilege.
// Any declared name works (hyphenated ones surface as NERV_FILE_RESULT), and
// a tool producing several artifacts declares several io:"out" fields — one
// per file, single-valued. Every io value (in and out alike) arrives in the
// script env as a FULL absolute path; outputs are additionally archived and
// mkdir'd by the core beforehand.
func TestOutFieldsEnvFullPathMultiOutput(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "multi.sh",
		`{"summary":"hyphenated out field + second out field","fields":[
	{"name":"src","io":"in"},{"name":"file-result","io":"out"},{"name":"thumb","io":"out"}]}`,
		`printf '%s|%s' "$NERV_FILE_RESULT" "$NERV_SRC" > "$NERV_FILE_RESULT"; printf x > "$NERV_THUMB"`)
	if err := os.WriteFile(f.art.Abs("in.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(f.art.Abs("sub/r.txt")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.art.Abs("sub/r.txt"), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := f.s.Run(context.Background(), []job.Job{{Tool: "multi", Params: map[string]any{
		"src": "in.txt", "file-result": "sub/r.txt", "thumb": "t.txt",
	}}}, nil)
	if !res.OK() {
		t.Fatalf("wave: %+v skipped %+v", res.Jobs, res.Skipped)
	}
	jr := resultAt(res, 0)
	if got := strings.Join(jr.Outputs, ","); got != "sub/r.txt,t.txt" {
		t.Errorf("outputs (relative, field order): got %q", got)
	}
	if got := strings.Join(jr.Archived, ","); got != "sub/r-1.txt" {
		t.Errorf("archived: got %q", got)
	}
	b, _ := os.ReadFile(f.art.Abs("sub/r.txt"))
	if want := f.art.Root + "/sub/r.txt|" + f.art.Root + "/in.txt"; string(b) != want {
		t.Errorf("env must carry both io fields absolute: got %q want %q", b, want)
	}
	if !f.art.Exists("t.txt") {
		t.Error("second output t.txt missing")
	}
	if b, _ := os.ReadFile(f.art.Abs("sub/r-1.txt")); string(b) != "stale" {
		t.Errorf("archived content: %q", b)
	}
}

// The debug wave log (§2.7): with RunOpts.LogPath the wave writes its
// event lines plus prefixed job output into that one file; without it the
// daemon writes no files at all (M12 default).
func TestDebugWaveDump(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "chatty.sh",
		`{"summary":"talks then writes","fields":[{"name":"output","io":"out"}]}`,
		`echo hello-from-job; printf done > "$NERV_OUTPUT"`)
	adm, skipped := f.s.Admit([]job.Job{{Tool: "chatty", Params: map[string]any{"output": "d.txt"}}})
	if len(skipped) != 0 {
		t.Fatalf("skipped: %+v", skipped)
	}
	logPath := filepath.Join(t.TempDir(), "q.log")
	res := f.s.RunAdmitted(context.Background(), adm, RunOpts{WaveID: "wave-1", LogPath: logPath}, nil)
	if !res.OK() {
		t.Fatalf("wave: %+v", res.Jobs)
	}
	logs, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"event":"begin"`, `"event":"start"`, `"event":"done"`, `"event":"end"`,
		"0 chatty | hello-from-job"} {
		if !strings.Contains(string(logs), want) {
			t.Errorf("wave log missing %s: %s", want, logs)
		}
	}
	// Quiet wave (no LogPath): nothing appears anywhere.
	adm2, _ := f.s.Admit([]job.Job{{Tool: "chatty", Params: map[string]any{"output": "d2.txt"}}})
	if res := f.s.RunAdmitted(context.Background(), adm2, RunOpts{}, nil); !res.OK() {
		t.Fatalf("quiet wave: %+v", res.Jobs)
	}
}

// A canceled wave SIGKILLs the script's whole process group and fails the
// started line (gate 9 core): the sleep child must die with the wrapper.
func TestCancelWaveKillsGroup(t *testing.T) {
	f := newFix(t, cudaPool(0))
	f.tool(t, "sleeper.sh", `{"summary":"sleeps","fields":[{"name":"output","io":"out"}]}`,
		`sleep 30 & echo $! > "$NERV_OUTPUT"; wait`)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	t0 := time.Now()
	res := f.s.Run(ctx, []job.Job{{Tool: "sleeper", Params: map[string]any{"output": "s.txt"}}}, nil)
	if time.Since(t0) > 5*time.Second {
		t.Errorf("cancel did not reach the wave (%s)", time.Since(t0))
	}
	if res.Jobs[0].Status != "failed" {
		t.Errorf("status after cancel: %+v", res.Jobs[0])
	}
}
