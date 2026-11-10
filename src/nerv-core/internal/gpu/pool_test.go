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
	"strings"
	"testing"
)

func TestPinSet(t *testing.T) {
	pool, _, err := Build("", "cuda@host1:1", fakeEnums())
	if err != nil {
		t.Fatal(err)
	}
	pins, err := pool.PinSet("cuda:1,cuda@host1:0")
	if err != nil {
		t.Fatal(err)
	}
	if len(pins) != 2 {
		t.Errorf("pin set = %v, want cuda:1 and cuda@host1:0", pins)
	}
	bare, err := pool.PinSet("rocm")
	if err != nil || len(bare) != 1 {
		t.Errorf("bare backend pin: %v err=%v", bare, err)
	}
	if p, err := pool.PinSet(""); p != nil || err != nil {
		t.Error("empty pin must mean no pin")
	}
	if _, err := pool.PinSet("cuda:9"); err == nil {
		t.Error("a pin missing the wall must fail fast")
	}
}

func TestParseNVIDIAPure(t *testing.T) {
	slots, err := parseNVIDIA("0, 24564, NVIDIA GeForce RTX 4090\n1, 8192, NVIDIA GeForce RTX 3050\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 2 || slots[1].MemMB != 8192 || !strings.Contains(slots[0].Name, "4090") {
		t.Errorf("parse wrong: %+v", slots)
	}
	if _, err := parseNVIDIA(""); err == nil {
		t.Error("empty report must be an error (backend absent)")
	}
}

func TestPinEnvTable(t *testing.T) {
	for b, want := range map[string]string{
		"cuda": "CUDA_VISIBLE_DEVICES", "rocm": "HIP_VISIBLE_DEVICES", "intel": "ONEAPI_DEVICE_SELECTOR",
	} {
		if got, ok := PinEnv(b); !ok || got != want {
			t.Errorf("PinEnv(%q) = %q,%v want %q", b, got, ok, want)
		}
	}
	if _, ok := PinEnv("vulkan"); ok {
		t.Error("unknown backend must have no pin env (opaque names, script authority)")
	}
}

func TestFindAndBackends(t *testing.T) {
	pool, _, _ := Build("", "rocm@srv:1", fakeEnums())
	if s, ok := pool.Find("rocm", "srv", 0); !ok || s.String() != "rocm@srv:0" {
		t.Errorf("Find external: %v %v", s, ok)
	}
	if got := pool.Backends(); len(got) != 2 || got[0] != "cuda" || got[1] != "rocm" {
		t.Errorf("Backends = %v", got)
	}
}
