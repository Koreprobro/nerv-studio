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

package scheduler

import (
	crand "crypto/rand"
	"encoding/binary"
)

// resolveSeed replaces the -1 seed sentinel with a hardware-random value
// drawn from crypto/rand (getrandom(2)/urandom). The result is masked to
// 63 bits so sh $(( )) arithmetic stays well-defined (int64 range). The
// launched script further masks to its engine's ceiling (sd-cpp: 63,
// ComfyUI JSON: 53, audio-cpp: 32).
//
// When the seed param is absent (the tool declares no seed field) or already
// carries a concrete value (the consumer-AI locked a seed for reproduction),
// resolveSeed is a no-op.
func resolveSeed(params map[string]any) {
	v, ok := params["seed"]
	if !ok {
		return
	}
	if n, isFloat := v.(float64); isFloat && n == -1 {
		var b [8]byte
		_, _ = crand.Read(b[:])
		params["seed"] = int64(binary.LittleEndian.Uint64(b[:]) & 0x7FFF_FFFF_FFFF_FFFF)
	}
}
