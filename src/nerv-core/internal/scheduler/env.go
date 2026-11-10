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
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	"nerv-core/internal/gpu"
	"nerv-core/internal/job"
	"nerv-core/internal/resolution"
)

// buildEnv assembles the slim scheduler↔sh contract (SPECS §5): PATH, pin
// env for the captured local slots, the operator env, then one NERV_<FIELD>
// per effective param. Both io marks arrive ABSOLUTE (the core resolves
// every file field against the artifactory — outputs are additionally
// archived and their parent dir created before the script starts); plain
// fields flow verbatim, arrays comma-join. The directory roots left the
// contract: dirname "$0" is the tools dir, the kb is the chat's domain, the
// lora dir is operator env. Scheduler-consumed fields (gpu-require /
// gpu-backend / gpu-pin) never reach the script env. A tool that declared
// the calculator pair gets NERV_CALCULATED_WIDTH/HEIGHT (SPECS §7.3).
func (s *Scheduler) buildEnv(e *Admitted) ([]string, error) {
	// An explicit PATH: the daemon's own environment may be sparse (systemd
	// / kiosk launches) — without PATH the tool's generator binaries would
	// not resolve at all.
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
	}
	env := []string{"PATH=" + path}
	// Pin env for the captured LOCAL slots only: a remote host is reached by
	// the script's own transport, a device selector of this machine would
	// only lie to it. Typed slots need no re-indexing: slot.Index is the
	// physical id already (§2.5 — the legacy visible/physical hangover dies).
	var order []string
	byBackend := map[string][]string{}
	for _, sl := range e.slots {
		if _, ok := gpu.PinEnv(sl.Backend); !ok || !sl.Local() {
			continue // unknown backend: no pin env, the script is the authority
		}
		if _, seen := byBackend[sl.Backend]; !seen {
			order = append(order, sl.Backend)
		}
		byBackend[sl.Backend] = append(byBackend[sl.Backend], strconv.Itoa(sl.Index))
	}
	for _, b := range order {
		name, _ := gpu.PinEnv(b)
		env = append(env, name+"="+strings.Join(byBackend[b], ","))
	}
	for k, v := range s.Deps.UserEnv {
		if strings.HasPrefix(k, "NERV_") {
			continue
		}
		env = append(env, k+"="+v)
	}
	keys := make([]string, 0, len(e.j.Params))
	for k := range e.j.Params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if job.SchedulerFields[k] {
			continue // the scheduler consumed it at admission (§5.1)
		}
		v := e.j.Params[k]
		if e.outFields[k] || e.inFields[k] {
			v = absolutize(v, s.Deps.Artifactory.Root)
		}
		env = append(env, job.EnvName(k)+"="+stringify(v))
	}
	if e.calc {
		w, h, err := computedSides(e.j.Params)
		if err != nil {
			return nil, err
		}
		env = append(env,
			"NERV_CALCULATED_WIDTH="+strconv.Itoa(w),
			"NERV_CALCULATED_HEIGHT="+strconv.Itoa(h))
	}
	return env, nil
}

// declaresCalcPair is the §7.3 trigger: the tool declares BOTH calculator
// fields. Comparison is normalized — aspect-ratio and aspect_ratio spellings
// are the same env name, so both are triggers.
func declaresCalcPair(ti *job.ToolInfo) bool {
	var aspect, target bool
	for _, f := range ti.Fields {
		switch job.NormEnvField(f.Name) {
		case "ASPECT_RATIO":
			aspect = true
		case "TARGET_MPX":
			target = true
		}
	}
	return aspect && target
}

// computedSides runs the studio resolution calculator for a job whose tool
// declared the aspect-ratio + target-mpx pair: one implementation in the
// core (internal/resolution), the script reads the result through the
// NERV_CALCULATED_* pair. A broken aspect is the job's error, not a guess.
func computedSides(params map[string]any) (int, int, error) {
	aspect, _ := paramNorm(params, "ASPECT_RATIO").(string)
	spec := resolution.Studio()
	if mpx, ok := asFloat(paramNorm(params, "TARGET_MPX")); ok && mpx > 0 {
		spec.Target = int(mpx * 1_000_000)
	}
	w, h, err := resolution.Compute(aspect, spec)
	if err != nil {
		return 0, 0, fmt.Errorf("resolution calculator: %w", err)
	}
	return w, h, nil
}

// paramNorm fetches a param by normalized field name (the calculator reads
// its inputs spelling-tolerantly, matching declaresCalcPair).
func paramNorm(params map[string]any, want string) any {
	for k, v := range params {
		if job.NormEnvField(k) == want {
			return v
		}
	}
	return nil
}

func asFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	}
	return 0, false
}

// absolutize turns a declared io path into an absolute target (the queue
// and the wave answer keep the relative form — lineage lives in relative
// paths; only the script env is absolute). io:"out" fields are single-valued
// by contract; io:"in" arrays keep their relative form inside the queue and
// comma-join absolute paths into the env string.
func absolutize(v any, root string) any {
	switch t := v.(type) {
	case string:
		if t != "" {
			return root + "/" + t
		}
		return t
	case []any:
		out := make([]any, 0, len(t))
		for _, el := range t {
			out = append(out, absolutize(el, root))
		}
		return out
	}
	return v
}

// stringify converts a JSON-decoded param value to its env-string form.
// Arrays are comma-joined (the sh-side convention for multi-valued fields).
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
		for _, el := range t {
			parts = append(parts, stringify(el))
		}
		return strings.Join(parts, ",")
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
