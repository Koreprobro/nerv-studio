// Package gpu enumerates the GPUs available to the scheduler pool.
//
// The pool is frozen at startup (SPECS.md §3.2). It can be limited via
// CUDA_VISIBLE_DEVICES, set at startup through --gpu-visible-devices: the
// adapter re-indexes the visible GPUs 0..N-1 (visible index) while preserving
// the physical device id for backend calls (QueryLoad and the CUDA_VISIBLE_
// _DEVICES value set per dispatched job). CUDA (nvidia-smi) is implemented;
// ROCm uses the same public interface with a rocminfo-backed enumerator.
package gpu

import (
	"bufio"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// GPU describes a single visible GPU in the scheduler pool.
type GPU struct {
	Index         int // visible index, 0..N-1 — the scheduler allocation key
	PhysicalIndex int // physical device id for backend queries
	MemTotalMB    uint64
	Name          string
}

// phy is a raw physical-GPU row from nvidia-smi.
type phy struct {
	id   int
	mem  uint64
	name string
}

// List enumerates the GPUs visible to the scheduler pool, honoring
// CUDA_VISIBLE_DEVICES. GPUs are re-indexed 0..N-1 by visibility; the physical
// id is preserved for backend calls.
func List() ([]GPU, error) {
	all, err := nvidiaSMI()
	if err != nil {
		return nil, err
	}
	if ids := parseVisibleIDs(); ids != nil {
		byID := make(map[int]phy, len(all))
		for _, p := range all {
			byID[p.id] = p
		}
		out := make([]GPU, 0, len(ids))
		for i, pid := range ids {
			if p, ok := byID[pid]; ok {
				out = append(out, GPU{Index: i, PhysicalIndex: pid, MemTotalMB: p.mem, Name: p.name})
			}
		}
		return out, nil
	}
	out := make([]GPU, 0, len(all))
	for i, p := range all {
		out = append(out, GPU{Index: i, PhysicalIndex: p.id, MemTotalMB: p.mem, Name: p.name})
	}
	return out, nil
}

// QueryLoad returns the used memory (MB) of the visible GPU at index.
func QueryLoad(index int) (uint64, error) {
	gpus, err := List()
	if err != nil {
		return 0, err
	}
	if index < 0 || index >= len(gpus) {
		return 0, nil
	}
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=memory.used",
		"--id", strconv.Itoa(gpus[index].PhysicalIndex),
		"--format=csv,noheader,nounits").CombinedOutput()
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		return 0, err
	}
	return v, nil
}

// nvidiaSMI queries nvidia-smi for all physical GPUs. It is a var so tests can
// inject a fake enumerator (e.g. to emulate a machine without nvidia-smi).
var nvidiaSMI = func() ([]phy, error) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,memory.total,name",
		"--format=csv,noheader,nounits").CombinedOutput()
	if err != nil {
		return nil, err
	}
	var res []phy
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		// format: <index>, <mem>, <name>
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		id, _ := strconv.Atoi(strings.TrimSpace(parts[0]))
		mem, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		res = append(res, phy{id: id, mem: mem, name: strings.TrimSpace(parts[2])})
	}
	return res, sc.Err()
}

// Discover tries the CUDA enumerator (nvidia-smi) and then the ROCm one
// (currently a documented stub, see rocm.go). It returns the union error when
// neither backend can enumerate — the daemon then starts with an empty GPU
// pool and warns (audit M5 / D4: a CPU-only host must still serve the studio).
func Discover() ([]GPU, error) {
	gpus, nErr := List()
	if nErr == nil {
		return gpus, nil
	}
	rgpus, rErr := listROCm()
	if rErr == nil {
		return rgpus, nil
	}
	return nil, errors.Join(nErr, rErr)
}

// parseVisibleIDs returns the physical GPU ids selected by CUDA_VISIBLE_DEVICES,
// or nil when the variable is unset/empty (all physical GPUs are visible).
func parseVisibleIDs() []int {
	raw := os.Getenv("CUDA_VISIBLE_DEVICES")
	if raw == "" || raw == "-1" {
		return nil
	}
	var ids []int
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if i, err := strconv.Atoi(p); err == nil {
			ids = append(ids, i)
		}
	}
	return ids
}
