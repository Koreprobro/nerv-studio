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

package gpu

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// PinEnv maps a backend to the env var its tools honor for device pinning
// (§2.5). An unknown backend has no pin env: the scheduler leaves the env
// untouched and the script stays the authority (opaque names, zero
// hardcoding).
func PinEnv(backend string) (string, bool) {
	switch backend {
	case "cuda":
		return "CUDA_VISIBLE_DEVICES", true
	case "rocm":
		return "HIP_VISIBLE_DEVICES", true
	case "intel":
		return "ONEAPI_DEVICE_SELECTOR", true
	}
	return "", false
}

// DefaultEnumerators is the greedy-scoop backend table (§2.5). An
// enumerator returning an error means "backend absent on this host" — the
// pool simply proceeds without it (a warning is surfaced at startup).
func DefaultEnumerators() map[string]Enumerator {
	return map[string]Enumerator{
		"cuda":  ListNVIDIA,
		"rocm":  ListROCm,
		"intel": ListIntel,
	}
}

// ListNVIDIA enumerates local NVIDIA cards via nvidia-smi. The detected
// device index is kept as-is (no re-indexing; SPECS §3.2 visible/physical
// dance is retired, REBUILD §4.19).
func ListNVIDIA() ([]Slot, error) {
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=index,memory.total,name",
		"--format=csv,noheader,nounits").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("nvidia-smi: %w", err)
	}
	return parseNVIDIA(string(out))
}

// parseNVIDIA reads "index, memMB, name" CSV rows (pure, test seam).
func parseNVIDIA(out string) ([]Slot, error) {
	var res []Slot
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) != 3 {
			continue
		}
		id, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil {
			continue
		}
		mem, _ := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
		res = append(res, Slot{Backend: "cuda", Index: id, MemMB: mem, Name: strings.TrimSpace(parts[2])})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, errors.New("nvidia-smi: no GPUs reported")
	}
	return res, nil
}

// ListROCm enumerates local AMD cards from sysfs (vendor 0x1002 under
// /sys/class/drm/card*/device), no rocminfo dependency. VRAM total is read
// when available — informational only.
func ListROCm() ([]Slot, error) {
	matches, err := filepath.Glob("/sys/class/drm/card[0-9]*/device/vendor")
	if err != nil {
		return nil, err
	}
	var cards []string
	for _, m := range matches {
		v, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		if strings.TrimSpace(string(v)) == "0x1002" {
			cards = append(cards, filepath.Dir(m))
		}
	}
	sort.Strings(cards)
	var res []Slot
	for i, dir := range cards {
		s := Slot{Backend: "rocm", Index: i, Name: "amdgpu"}
		if b, err := os.ReadFile(filepath.Join(dir, "mem_info_vram_total")); err == nil {
			if bytes, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64); err == nil {
				s.MemMB = bytes / (1 << 20)
			}
		}
		res = append(res, s)
	}
	if len(res) == 0 {
		return nil, errors.New("sysfs: no AMD (vendor 0x1002) card found")
	}
	return res, nil
}

// ListIntel enumerates local Intel GPUs via sycl-ls (level_zero devices).
func ListIntel() ([]Slot, error) {
	out, err := exec.Command("sycl-ls").CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("sycl-ls: %w", err)
	}
	var res []Slot
	sc := bufio.NewScanner(strings.NewReader(string(out)))
	for sc.Scan() {
		if strings.Contains(sc.Text(), "level_zero:gpu") {
			res = append(res, Slot{Backend: "intel", Index: len(res), Name: "intel-gpu"})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(res) == 0 {
		return nil, errors.New("sycl-ls: no level_zero GPU reported")
	}
	return res, nil
}

// QueryLoad reports used memory (MB) of a local NVIDIA slot — a courtesy
// for the hidden gpu.list diagnostic only, not scheduler accounting.
// Other backends and remote slots report unsupported.
func QueryLoad(s Slot) (uint64, error) {
	if s.Backend != "cuda" || !s.Local() {
		return 0, fmt.Errorf("load query not supported for slot %s", s)
	}
	out, err := exec.Command("nvidia-smi",
		"--query-gpu=memory.used",
		"--id", strconv.Itoa(s.Index),
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
