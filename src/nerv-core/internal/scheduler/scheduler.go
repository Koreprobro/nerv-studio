// Package scheduler runs a submitted job queue across the GPU pool with
// file-existence dataflow (SPECS.md §2, §3).
//
// There is no dependency graph: a job names the output file it writes through
// the `output` param and declares its input files through fields its --tool-info
// marks "file": true. The scheduler repeatedly scans the queue: pending jobs
// whose input files all exist are packed into free GPU slots and run as a wave;
// after each wave the scan repeats. When nothing is running and jobs still wait
// on files that never appeared, those jobs fail — a submit-side cycle lands in
// exactly that trap, so no cycle detection is needed.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"nerv-core/internal/artifactory"
	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
)

// Progress is called with a job and its new status whenever the status changes.
// The status is passed by value so the callback never races on the job's
// mutable Status field (which the scheduler may rewrite for the next event).
type Progress func(*job.Job, job.Status)

// toolInfo is the scheduler's slice of a tool's --tool-info: the GPU slots the
// tool needs and the names of fields declared with "file": true (input files).
type toolInfo struct {
	GPUCount   int
	FileFields []string
}

// Scheduler runs a submitted queue across the GPU pool.
type Scheduler struct {
	ToolsDir     string
	loraModelDir string // LoRA models dir, passed to sh as NERV_LORA_MODEL_DIR
	kbDir        string // knowledge base dir, passed to sh as NERV_KB_DIR
	Artifactory  *artifactory.Artifactory
	GPUs         []gpu.GPU // visible pool, index 0..N-1

	mu    sync.RWMutex // guards job status/Slots during dispatch
	jobs  []*job.Job
	infos map[string]toolInfo   // --tool-info cache, keyed by tool id
	files map[*job.Job][]string // job input files, artifactory-relative
}

// Jobs returns a snapshot of the queue's jobs (safe to read during dispatch).
func (s *Scheduler) Jobs() []*job.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*job.Job, len(s.jobs))
	copy(out, s.jobs)
	return out
}

// JobSnapshot is an immutable copy of a job's mutable runtime fields.
type JobSnapshot struct {
	Tool   string
	Output string
	Status job.Status
	Error  string
	Slots  []int
}

// JobsSnapshot returns an immutable copy of every job's runtime state. It is
// safe to call during dispatch: the returned values are copied under the lock,
// so callers never race with the scheduler's mutable job fields.
func (s *Scheduler) JobsSnapshot() []JobSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]JobSnapshot, len(s.jobs))
	for i, j := range s.jobs {
		slots := append([]int(nil), j.Slots...)
		out[i] = JobSnapshot{
			Tool:   j.Tool,
			Output: j.Output(),
			Status: j.Status,
			Error:  j.Error,
			Slots:  slots,
		}
	}
	return out
}

// New returns a Scheduler over the given GPU pool.
func New(toolsDir, loraModelDir, kbDir string, af *artifactory.Artifactory, gpus []gpu.GPU) *Scheduler {
	return &Scheduler{
		ToolsDir:     toolsDir,
		loraModelDir: loraModelDir,
		kbDir:        kbDir,
		Artifactory:  af,
		GPUs:         gpus,
		infos:        map[string]toolInfo{},
		files:        map[*job.Job][]string{},
	}
}

// Build validates the queue and resolves each job's GPU placement and file
// inputs from --tool-info. It must be called before Dispatch.
func (s *Scheduler) Build(submit job.Submit) error {
	for i := range submit.Queue {
		j := &submit.Queue[i]
		if j.Tool == "" {
			return fmt.Errorf("job #%d: 'tool' is required", i+1)
		}
		if err := withinRel(j.Tool); err != nil {
			return fmt.Errorf("job #%d: tool %q: %w", i+1, j.Tool, err)
		}
		// Tool-id leniency: browse lists scripts as "<id>.sh"; a consumer that
		// drops the suffix gets the script anyway (runJob joins ToolsDir+Tool).
		if !strings.HasSuffix(j.Tool, ".sh") {
			if _, err := os.Stat(filepath.Join(s.ToolsDir, j.Tool+".sh")); err == nil {
				j.Tool += ".sh"
			}
		}
		out := j.Output()
		if out == "" {
			return fmt.Errorf("job #%d (tool=%s): params.output is required", i+1, j.Tool)
		}
		if err := withinRel(out); err != nil {
			return fmt.Errorf("job #%d (tool=%s): output %q: %w", i+1, j.Tool, out, err)
		}
		j.Params["output"] = out // store the normalized form: env must match paths
		info := s.info(j.Tool)
		if j.GPUCount < 0 {
			j.GPUCount = 0
		}
		// GPU placement is the scheduler's decision, derived from the tool's
		// self-declared --tool-info (SPECS.md §2). The consumer does not set
		// it: when gpu-count is left unset (0), resolve it from the tool. An
		// explicit positive value overrides.
		if j.GPUCount == 0 {
			j.GPUCount = info.GPUCount
		}
		// Collect declared file inputs (fields marked "file": true). Values
		// stay artifactory-relative: the sh side composes them with
		// NERV_ARTIFACTORY_DIR, the scheduler stats them under the root.
		var files []string
		for _, name := range info.FileFields {
			switch v := j.Params[name].(type) {
			case string:
				if v == "" {
					continue
				}
				v = job.NormalizePath(v)
				if err := withinRel(v); err != nil {
					return fmt.Errorf("job #%d (tool=%s): field %q: %w", i+1, j.Tool, name, err)
				}
				j.Params[name] = v // normalized form flows to env
				files = append(files, v)
			case []any:
				for i, e := range v {
					sv, ok := e.(string)
					if !ok || sv == "" {
						continue
					}
					sv = job.NormalizePath(sv)
					if err := withinRel(sv); err != nil {
						return fmt.Errorf("job #%d (tool=%s): field %q: %w", i+1, j.Tool, name, err)
					}
					v[i] = sv
					files = append(files, sv)
				}
				j.Params[name] = v // normalized elements flow to env
			}
		}
		j.Status = job.Pending
		s.files[j] = files
		s.jobs = append(s.jobs, j)
	}
	return nil
}

// info returns the cached --tool-info view of a tool (a queue reusing a tool
// probes it once).
func (s *Scheduler) info(tool string) toolInfo {
	if v, ok := s.infos[tool]; ok {
		return v
	}
	v := toolInfoRaw(s.ToolsDir, tool)
	s.infos[tool] = v
	return v
}

// toolInfoRaw runs <toolsDir>/<tool> --tool-info and reads the declared
// gpu-count plus the field names marked "file": true. Any failure yields a
// zero info: a tool that cannot describe itself runs as a CPU job with no file
// inputs (the runJob output post-check still holds it accountable).
func toolInfoRaw(toolsDir, tool string) toolInfo {
	var ti toolInfo
	cmd := exec.Command("sh", filepath.Join(toolsDir, tool), "--tool-info")
	// A probe must not leak the daemon's working directory or env into an
	// unguarded script (a fall-through body would otherwise write garbage into
	// the daemon CWD).
	cmd.Dir = os.TempDir()
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return ti
	}
	var iface struct {
		GPUCount int `json:"gpu-count"`
		Fields   []struct {
			Name string `json:"name"`
			File bool   `json:"file"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(out, &iface); err != nil {
		return ti
	}
	if iface.GPUCount > 0 {
		ti.GPUCount = iface.GPUCount
	}
	for _, f := range iface.Fields {
		if f.File && f.Name != "" {
			ti.FileFields = append(ti.FileFields, f.Name)
		}
	}
	return ti
}

// withinRel rejects declared paths that escape their base root via ".." or are
// absolute (audit M4: Build-side traversal guard).
func withinRel(rel string) error {
	if filepath.IsAbs(rel) {
		return fmt.Errorf("absolute path not allowed")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes the root")
	}
	return nil
}

// Dispatch runs the queue to completion, invoking onProgress on every status
// change. It returns an error if a job fails. When ctx is canceled mid-wave,
// running tool processes are killed (runJob's process-group cancel), unstarted
// pending jobs are marked failed "wave canceled", and no new wave is launched
// (audit M2 / D1).
func (s *Scheduler) Dispatch(ctx context.Context, onProgress Progress) error {
	for {
		if err := ctx.Err(); err != nil {
			s.abortPending(err, onProgress)
			return fmt.Errorf("wave canceled: %w", err)
		}
		ready := s.readyJobs()
		if len(ready) == 0 {
			break
		}
		// Biggest-first packing reduces slot fragmentation.
		sort.SliceStable(ready, func(i, k int) bool { return ready[i].GPUCount > ready[k].GPUCount })

		allocated := make(map[int]*job.Job)
		var wave []*job.Job
		for _, j := range ready {
			slots, ok := s.allocateSlots(j.GPUCount, allocated)
			if !ok {
				continue // not enough free slots right now; try next wave
			}
			s.mu.Lock() // readers (job.status snapshots) observe Slots under mu
			j.Slots = slots
			s.mu.Unlock()
			wave = append(wave, j)
			for _, sl := range slots {
				allocated[sl] = j
			}
		}
		if len(wave) == 0 {
			return fmt.Errorf("no free GPU slots for any ready job (gpu-count may exceed pool size %d)", len(s.GPUs))
		}
		s.runWave(ctx, wave, onProgress)
	}

	// Nothing is running and no remaining pending job became ready: they wait
	// on input files that will never appear (their producer failed or was
	// never in the queue). Stuck jobs count as failed (M10).
	s.failStuck(onProgress)

	for _, j := range s.jobs {
		if j.Status == job.Failed {
			return fmt.Errorf("job %q failed: %s", j.Tool, j.Error)
		}
	}
	return nil
}

// failStuck marks every still-Pending job failed, naming the inputs that never
// appeared.
func (s *Scheduler) failStuck(onProgress Progress) {
	s.mu.Lock()
	var stuck []*job.Job
	msgs := map[*job.Job]string{}
	for _, j := range s.jobs {
		if j.Status != job.Pending {
			continue
		}
		msgs[j] = "input file(s) never appeared: " + strings.Join(s.missingFilesLocked(j), ", ")
		j.Status = job.Failed
		j.Error = msgs[j]
		stuck = append(stuck, j)
	}
	s.mu.Unlock()
	if onProgress != nil {
		for _, j := range stuck {
			onProgress(j, job.Failed)
		}
	}
}

// readyJobs returns pending jobs whose every declared input file exists.
func (s *Scheduler) readyJobs() []*job.Job {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []*job.Job
	for _, j := range s.jobs {
		if j.Status != job.Pending {
			continue
		}
		if len(s.missingFilesLocked(j)) == 0 {
			out = append(out, j)
		}
	}
	return out
}

// missingFilesLocked returns the declared input paths of j that do not exist
// under the artifactory root. The caller must hold s.mu (read or write).
func (s *Scheduler) missingFilesLocked(j *job.Job) []string {
	var missing []string
	for _, rel := range s.files[j] {
		if st, err := os.Stat(s.Artifactory.Abs(rel)); err != nil || st.IsDir() {
			missing = append(missing, rel)
		}
	}
	return missing
}

// abortPending marks every still-Pending job failed ("wave canceled") and
// reports the transition through onProgress. Called when the wave's context is
// canceled so no job silently stays pending after cancellation (audit M2).
func (s *Scheduler) abortPending(ctxErr error, onProgress Progress) {
	s.mu.Lock()
	var aborted []*job.Job
	for _, j := range s.jobs {
		if j.Status == job.Pending {
			j.Status = job.Failed
			j.Error = "wave canceled: " + ctxErr.Error()
			aborted = append(aborted, j)
		}
	}
	s.mu.Unlock()
	if onProgress != nil {
		for _, j := range aborted {
			onProgress(j, job.Failed)
		}
	}
}

// allocateSlots assigns gpuCount disjoint free slots from the pool.
func (s *Scheduler) allocateSlots(gpuCount int, allocated map[int]*job.Job) ([]int, bool) {
	if gpuCount == 0 {
		return nil, true // CPU job: no slots
	}
	var free []int
	for i := 0; i < len(s.GPUs); i++ {
		if _, inUse := allocated[i]; !inUse {
			free = append(free, i)
		}
	}
	if len(free) < gpuCount {
		return nil, false
	}
	return free[:gpuCount], true
}

// runWave runs the jobs in the wave concurrently, each with exclusive slots.
// A job that exits 0 without having written its declared output file fails:
// exit code alone is not trusted to mean a file landed (M10).
func (s *Scheduler) runWave(ctx context.Context, wave []*job.Job, onProgress Progress) {
	var wg sync.WaitGroup
	for _, j := range wave {
		wg.Add(1)
		go func(j *job.Job) {
			defer wg.Done()
			s.mu.Lock()
			j.Status = job.Running
			status := j.Status
			s.mu.Unlock()
			if onProgress != nil {
				onProgress(j, status)
			}
			err := s.runJob(ctx, j)
			if err == nil {
				outAbs := s.Artifactory.Abs(j.Output())
				if st, serr := os.Stat(outAbs); serr != nil || st.IsDir() {
					err = fmt.Errorf("tool wrote no output at %s", j.Output())
				}
			}
			s.mu.Lock()
			if err != nil {
				j.Status = job.Failed
				j.Error = err.Error()
			} else {
				j.Status = job.Done
			}
			status = j.Status
			s.mu.Unlock()
			if onProgress != nil {
				onProgress(j, status)
			}
		}(j)
	}
	wg.Wait()
}

// runJob executes a single tool script with the resolved environment.
// Exit code 0 == ok, non-zero == failed (SPECS.md §3.1). The script runs in its
// own process group and ctx cancellation SIGKILLs the whole group: the wrapper
// sh spawns the generator (sd-cli / audiocpp_cli) as a child, so killing only
// the direct child would orphan the GPU process (audit M2 / D1).
func (s *Scheduler) runJob(ctx context.Context, j *job.Job) error {
	script := filepath.Join(s.ToolsDir, j.Tool)
	if err := s.Artifactory.EnsureOutputDir(j.Output()); err != nil {
		return fmt.Errorf("create output dir: %w", err)
	}
	cmd := exec.CommandContext(ctx, "sh", script)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	cmd.Env = s.buildEnv(j)
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("%w: %s", err, msg)
		}
		return err
	}
	return nil
}

// buildEnv assembles the environment for a tool run (SPECS.md §3.1). Every
// path the script sees is artifactory-relative; the script composes it with
// NERV_ARTIFACTORY_DIR (M10): out="$NERV_ARTIFACTORY_DIR/$NERV_OUTPUT".
func (s *Scheduler) buildEnv(j *job.Job) []string {
	var env []string
	// An explicit PATH: the daemon's own environment may be sparse (systemd /
	// kiosk launches) and without PATH the tool's generator binaries would not
	// resolve at all (audit M6).
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env = append(env, "PATH="+path)
	if len(j.Slots) > 0 {
		// The tool's CUDA processes enumerate devices by PHYSICAL id (what
		// nvidia-smi reports), not by the scheduler's visible-slot index —
		// passing slot numbers would pin jobs to the wrong GPU whenever
		// --gpu-visible-devices re-indexes the pool (audit M5).
		idx := make([]string, 0, len(j.Slots))
		for _, sl := range j.Slots {
			if sl >= 0 && sl < len(s.GPUs) {
				idx = append(idx, strconv.Itoa(s.GPUs[sl].PhysicalIndex))
			}
		}
		if len(idx) > 0 {
			env = append(env, "CUDA_VISIBLE_DEVICES="+strings.Join(idx, ","))
		}
	}
	env = append(env, "NERV_ARTIFACTORY_DIR="+s.Artifactory.Root)
	env = append(env, "NERV_TOOLS_DIR="+s.ToolsDir)
	env = append(env, "NERV_KB_DIR="+s.kbDir)
	env = append(env, "NERV_LORA_MODEL_DIR="+s.loraModelDir)
	for k, v := range j.Params {
		env = append(env, "NERV_"+strings.ToUpper(k)+"="+stringify(v))
	}
	return env
}

// stringify converts a JSON-decoded param value to its env-string form. Arrays
// are comma-joined (the sh-side convention for multi-valued fields).
func stringify(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case []any:
		parts := make([]string, 0, len(t))
		for _, e := range t {
			parts = append(parts, stringify(e))
		}
		return strings.Join(parts, ",")
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
