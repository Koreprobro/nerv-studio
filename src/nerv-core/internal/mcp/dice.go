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

package mcp

import (
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sync"
)

// Roller bounds: a roll is a tiny call, not a compute job.
const (
	diceHWDevice = "/dev/hwrng"
	diceMaxNot   = 96
	diceMaxTerms = 16
	diceMaxCount = 1000
	diceMaxSides = 1_000_000
	diceMaxConst = 1_000_000_000
)

func (s *Server) callDice(args map[string]any) ToolResult {
	raw := asString(args, "notation")
	expr, err := parseDice(raw)
	if err != nil {
		return toolError("%v", err)
	}
	rl, seed := diceSource(args)
	out, err := expr.roll(raw, rl, seed)
	if err != nil {
		return toolError("%v", err)
	}
	return jsonResult(out)
}

// roller draws an unbiased uint64 in [0,n).
type roller func(n uint64) (uint64, error)

// diceSource picks the entropy backend: an explicit seed wins (deterministic
// replay); otherwise a physical draw — the hardware noise source when the
// node is accessible, else the kernel entropy pool. The choice is never
// reported to the client.
func diceSource(args map[string]any) (roller, *int64) {
	if raw, ok := args["seed"]; ok && raw != nil {
		if sv, ok := jsonInt64(raw); ok {
			r := rand.New(rand.NewSource(sv))
			return func(n uint64) (uint64, error) { return uint64(r.Int63n(int64(n))), nil }, &sv
		}
	}
	src := entropyReader()
	return func(n uint64) (uint64, error) { return readerUint64n(src, n) }, nil
}

func jsonInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return int64(n), true
	case int:
		return int64(n), true
	}
	return 0, false
}

// openHWRNG opens the hardware RNG node once per process; the open error is
// remembered so a missing node costs one syscall, not one per die.
var (
	hwOnce sync.Once
	hwFile *os.File
	hwErr  error
)

func entropyReader() io.Reader {
	hwOnce.Do(func() { hwFile, hwErr = os.Open(diceHWDevice) })
	if hwErr == nil {
		return hwFile
	}
	return crand.Reader
}

// readerUint64n draws an unbiased value in [0,n) by rejection sampling
// over 8-byte reads (modulo bias is never introduced).
func readerUint64n(r io.Reader, n uint64) (uint64, error) {
	if n == 0 {
		return 0, nil
	}
	var b [8]byte
	limit := ^uint64(0) - (^uint64(0) % n)
	for {
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return 0, fmt.Errorf("dice: entropy read: %w", err)
		}
		if v := binary.LittleEndian.Uint64(b[:]); v < limit {
			return v % n, nil
		}
	}
}
