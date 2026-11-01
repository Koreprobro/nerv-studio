package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"nerv-core/internal/artifactory"
	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
)

// writeTool creates a fixture tool: `info` is the --tool-info JSON, `body` the
// generation logic.
func writeTool(t *testing.T, toolsDir, name, info, body string) {
	t.Helper()
	script := "#!/usr/bin/env sh\ncase \"${1:-}\" in\n  --tool-info) cat <<'INFOEOF'\n" +
		info + "\nINFOEOF\n    exit 0;;\nesac\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(toolsDir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

const genInfo = `{"summary":"gen","fields":[{"name":"output","required":true},{"name":"order"},{"name":"log"}],"gpu-count":1}`

// genBody writes NERV_OUTPUT under the artifactory root and logs
// "<order>|<cuda-slots>" when NERV_LOG is set.
const genBody = `out="$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"
mkdir -p "$(dirname "$out")"
[ -n "${NERV_LOG:-}" ] && echo "${NERV_ORDER:-}|${CUDA_VISIBLE_DEVICES:-}" >> "$NERV_LOG"
echo content > "$out"`

const catInfo = `{"summary":"cat","fields":[{"name":"output","required":true},{"name":"input","file":true}],"gpu-count":0}`

const catBody = `out="$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"
cat "$NERV_ARTIFACTORY_DIR/$NERV_INPUT" > "$out"`

// TestBuildValidation pins the submit-side rejections: missing tool, missing
// output, absolute/traversal paths (output and file fields).
func TestBuildValidation(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "cat.sh", catInfo, catBody)
	s := New(toolsDir, "", "", artifactory.New(t.TempDir()), nil)

	cases := []struct {
		name string
		j    job.Job
		want string
	}{
		{"no tool", job.Job{Params: map[string]any{"output": "a.txt"}}, "'tool' is required"},
		{"no output", job.Job{Tool: "cat.sh"}, "output"},
		{"abs output", job.Job{Tool: "cat.sh", Params: map[string]any{"output": "/etc/x"}}, "absolute"},
		{"esc output", job.Job{Tool: "cat.sh", Params: map[string]any{"output": "../x"}}, "escapes"},
		{"esc input", job.Job{Tool: "cat.sh", Params: map[string]any{"output": "a.txt", "input": "../secret"}}, "field"},
	}
	for _, tc := range cases {
		err := s.Build(job.Submit{Queue: []job.Job{tc.j}})
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want substring %q", tc.name, err, tc.want)
		}
	}
}

// TestFileDataflow is the core M10 scenario: a producer's output file releases
// the consumer waiting on it inside the same queue; an existing file releases
// its reader on the first pass.
func TestFileDataflow(t *testing.T) {
	toolsDir := t.TempDir()
	af := artifactory.New(t.TempDir())
	writeTool(t, toolsDir, "gen.sh", genInfo, genBody)
	writeTool(t, toolsDir, "cat.sh", catInfo, catBody)
	if err := os.WriteFile(filepath.Join(af.Root, "seed.txt"), []byte("seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := New(toolsDir, "", "", af, []gpu.GPU{{Index: 0}})
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "cat.sh", Params: map[string]any{"output": "c.txt", "input": "seed.txt"}},
		{Tool: "cat.sh", Params: map[string]any{"output": "b.txt", "input": "a.txt"}}, // waits for gen
		{Tool: "gen.sh", Params: map[string]any{"output": "a.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	for _, j := range s.jobs {
		if j.Status != job.Done {
			t.Errorf("job %v not done: %s", j.Params, j.Error)
		}
	}
	got, err := os.ReadFile(filepath.Join(af.Root, "b.txt"))
	if err != nil || string(got) != "content\n" {
		t.Fatalf("consumer saw %q (%v)", got, err)
	}
}

// TestStuckFails: a job waiting on a file nothing will ever produce fails with
// the missing paths named (and the wave returns an error).
func TestStuckFails(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "cat.sh", catInfo, catBody)
	s := New(toolsDir, "", "", artifactory.New(t.TempDir()), nil)
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "cat.sh", Params: map[string]any{"output": "b.txt", "input": "never.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	err := s.Dispatch(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "never appeared") ||
		!strings.Contains(err.Error(), "never.txt") {
		t.Fatalf("dispatch: got %v, want never-appeared failure", err)
	}
	if s.jobs[0].Status != job.Failed {
		t.Fatalf("status = %s", s.jobs[0].Status)
	}
}

// TestCycleStuck: a circular queue needs no special detection — nobody is
// runnable, so the stuck rule fails everyone.
func TestCycleStuck(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "cat.sh", catInfo, catBody)
	s := New(toolsDir, "", "", artifactory.New(t.TempDir()), nil)
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "cat.sh", Params: map[string]any{"output": "a.txt", "input": "b.txt"}},
		{Tool: "cat.sh", Params: map[string]any{"output": "b.txt", "input": "a.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(context.Background(), nil); err == nil {
		t.Fatal("expected cycle to fail via the stuck rule")
	}
	for i, j := range s.jobs {
		if j.Status != job.Failed || !strings.Contains(j.Error, "never appeared") {
			t.Errorf("job %d: status=%s err=%s", i, j.Status, j.Error)
		}
	}
}

// TestNoWriteFails: exit 0 without producing the declared output file is a
// failed job (the exit code alone is not trusted, M10).
func TestNoWriteFails(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "lazy.sh",
		`{"summary":"lazy","fields":[{"name":"output","required":true}],"gpu-count":0}`,
		"exit 0")
	s := New(toolsDir, "", "", artifactory.New(t.TempDir()), nil)
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "lazy.sh", Params: map[string]any{"output": "x.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	err := s.Dispatch(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "wrote no output") {
		t.Fatalf("dispatch: got %v, want wrote-no-output failure", err)
	}
}

// TestProducerFailCascade: a failing producer releases nothing, so its waiting
// consumer fails via the stuck rule.
func TestProducerFailCascade(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "boom.sh",
		`{"summary":"boom","fields":[{"name":"output","required":true}],"gpu-count":0}`,
		"exit 3")
	writeTool(t, toolsDir, "cat.sh", catInfo, catBody)
	s := New(toolsDir, "", "", artifactory.New(t.TempDir()), nil)
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "cat.sh", Params: map[string]any{"output": "b.txt", "input": "a.txt"}},
		{Tool: "boom.sh", Params: map[string]any{"output": "a.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(context.Background(), nil); err == nil {
		t.Fatal("expected wave failure")
	}
	if s.jobs[0].Status != job.Failed || !strings.Contains(s.jobs[0].Error, "never appeared") {
		t.Errorf("consumer: %s / %s", s.jobs[0].Status, s.jobs[0].Error)
	}
	if s.jobs[1].Status != job.Failed || strings.Contains(s.jobs[1].Error, "never appeared") {
		t.Errorf("producer: %s / %s", s.jobs[1].Status, s.jobs[1].Error)
	}
}

// TestGPUPlacementFromToolInfo: gpu-count arrives from --tool-info and the job
// captures that many exclusive slots.
func TestGPUPlacementFromToolInfo(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "gen.sh", genInfo, genBody)
	writeTool(t, toolsDir, "big.sh",
		strings.Replace(genInfo, `"gpu-count":1`, `"gpu-count":2`, 1), genBody)
	afDir := t.TempDir()
	logPath := filepath.Join(afDir, "run.log")
	s := New(toolsDir, "", "", artifactory.New(afDir),
		[]gpu.GPU{{Index: 0}, {Index: 1}, {Index: 2}, {Index: 3}})
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "gen.sh", Params: map[string]any{"output": "a.txt", "log": logPath}},
		{Tool: "big.sh", Params: map[string]any{"output": "b.txt", "log": logPath}},
	}}); err != nil {
		t.Fatal(err)
	}
	if s.jobs[0].GPUCount != 1 || s.jobs[1].GPUCount != 2 {
		t.Fatalf("gpu counts: %d %d", s.jobs[0].GPUCount, s.jobs[1].GPUCount)
	}
	if err := s.Dispatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(s.jobs[0].Slots) != 1 || len(s.jobs[1].Slots) != 2 {
		t.Fatalf("slots: %v / %v", s.jobs[0].Slots, s.jobs[1].Slots)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(log)), "\n") {
		if line == "" || strings.Count(line, "|") != 1 || strings.HasSuffix(line, "|") {
			t.Errorf("log line without CUDA_VISIBLE_DEVICES: %q", line)
		}
	}
}

// TestEnvContract pins the M10 scheduler<->sh env: NERV_ARTIFACTORY_DIR is the
// root, NERV_OUTPUT/NERV_<FIELD> carry the declared relative paths (arrays
// comma-joined), and NERV_ARTIFACT is gone.
func TestEnvContract(t *testing.T) {
	toolsDir := t.TempDir()
	af := artifactory.New(t.TempDir())
	info := `{"summary":"env","fields":[{"name":"output","required":true},{"name":"refs","file":true}],"gpu-count":0}`
	body := `out="$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"
printf '%s|%s|%s|%s' "$NERV_ARTIFACTORY_DIR" "$NERV_OUTPUT" "$NERV_REFS" "${NERV_ARTIFACT-none}" > "$out"`
	writeTool(t, toolsDir, "env.sh", info, body)
	for _, seed := range []string{"p.txt", "q.txt"} {
		if err := os.WriteFile(filepath.Join(af.Root, seed), []byte(seed), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := New(toolsDir, "", "", af, nil)
	if err := s.Build(job.Submit{Queue: []job.Job{{
		Tool: "env.sh",
		Params: map[string]any{
			"output": "artifacts/out/report.txt", // legacy prefix normalized away
			"refs":   []any{"p.txt", "q.txt"},
		},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(af.Root, "out", "report.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := af.Root + "|out/report.txt|p.txt,q.txt|none"
	if string(got) != want {
		t.Fatalf("env: got %q want %q", got, want)
	}
}

// TestPrivateParamsPassThrough (C2): params are never validated against the
// tool's declared field list — an undeclared/private param (cfg) reaches the
// script as NERV_CFG untouched (SPECS.md §3.1).
func TestPrivateParamsPassThrough(t *testing.T) {
	toolsDir := t.TempDir()
	af := artifactory.New(t.TempDir())
	info := `{"summary":"t","fields":[{"name":"output","required":true},{"name":"prompt","required":true}],"gpu-count":0}`
	body := `printf '%s/%s' "$NERV_PROMPT" "$NERV_CFG" > "$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT"`
	writeTool(t, toolsDir, "pubpriv.sh", info, body)
	s := New(toolsDir, "", "", af, nil)
	if err := s.Build(job.Submit{Queue: []job.Job{{
		Tool:   "pubpriv",
		Params: map[string]any{"output": "x.txt", "prompt": "hello", "cfg": "9.5"},
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Dispatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(af.Root, "x.txt"))
	if err != nil || string(got) != "hello/9.5" {
		t.Fatalf("NERV_CFG pass-through: got %q (%v)", got, err)
	}
}

// TestDispatchCancel: canceling mid-wave kills the running tool (failed) and
// aborts the pending jobs with "wave canceled" (audit M2 / D1).
func TestDispatchCancel(t *testing.T) {
	toolsDir := t.TempDir()
	writeTool(t, toolsDir, "sleep.sh",
		`{"summary":"sleep","fields":[{"name":"output","required":true}],"gpu-count":0}`,
		"sleep 30")
	writeTool(t, toolsDir, "cat.sh", catInfo, catBody)
	s := New(toolsDir, "", "", artifactory.New(t.TempDir()), nil)
	if err := s.Build(job.Submit{Queue: []job.Job{
		{Tool: "sleep.sh", Params: map[string]any{"output": "a.txt"}},
		{Tool: "cat.sh", Params: map[string]any{"output": "b.txt", "input": "a.txt"}},
	}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Dispatch(ctx, nil) }()
	time.Sleep(300 * time.Millisecond)
	cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("dispatch after cancel: %v", err)
	}
	if s.jobs[0].Status != job.Failed || s.jobs[1].Status != job.Failed {
		t.Fatalf("statuses: %s / %s", s.jobs[0].Status, s.jobs[1].Status)
	}
	if !strings.Contains(s.jobs[1].Error, "wave canceled") {
		t.Errorf("aborted job error: %s", s.jobs[1].Error)
	}
}
