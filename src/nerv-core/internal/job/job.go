// Package job models a queue job and its lifecycle status (SPECS.md §2).
package job

import "strings"

// Status is a job's lifecycle state.
type Status string

const (
	Pending Status = "pending"
	Running Status = "running"
	Done    Status = "done"
	Failed  Status = "failed"
)

// Job is one node of a submitted queue. There is no dependency envelope: data
// flows through files. Params carry everything — `output` (the declared
// artifactory-relative file the tool writes, extension included) and any field
// the tool's --tool-info marks `"file": true` (inputs the scheduler waits for).
type Job struct {
	// Tool (required) is the script path relative to tools/ (e.g.
	// "image/anima/anima-base.sh").
	Tool string `json:"tool"`
	// Params are the tool's parameters (passed through untouched as
	// NERV_<FIELD> env vars; private fields are the GUI's to set — the core
	// never validates them against the tool's declared field list).
	Params map[string]any `json:"params,omitempty"`
	// GPUCount is the number of GPU slots this job captures exclusively.
	GPUCount int `json:"gpu-count"`

	// --- scheduler runtime state ---
	Status Status `json:"status"`
	Error  string `json:"error,omitempty"`
	// Slots are the visible GPU indices (0..N-1) this job captures exclusively
	// for its run (empty for CPU jobs, gpu-count == 0).
	Slots []int `json:"slots,omitempty"`
}

// Submit is the queue.submit input: a queue of jobs.
type Submit struct {
	Queue []Job `json:"queue"`
}

// NormalizePath drops a leading "artifacts/" element: that is the URL-mount
// name (GET /artifacts/<rel>), not a directory of the artifactory root. Legacy
// doc examples led consumers to submit "artifacts/scene/..." (M7.8); the
// leniency is kept for output and file-field values (M10).
func NormalizePath(p string) string {
	p = strings.TrimSpace(p)
	if t := strings.TrimPrefix(p, "artifacts/"); t != p && t != "" {
		return t
	}
	return p
}

// Output returns the declared output file path (params["output"], normalized).
// Empty means the job declared no output — Build rejects that.
func (j *Job) Output() string {
	if v, ok := j.Params["output"].(string); ok {
		return NormalizePath(v)
	}
	return ""
}
