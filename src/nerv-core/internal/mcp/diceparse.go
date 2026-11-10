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
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// diceTerm is one parsed term: dice or a signed constant.
type diceTerm struct {
	Kind      string // "dice" | "constant"
	Sign      int
	Count     int
	Sides     int
	Keep      string // "" | "high" | "low"
	KeepCount int
	Value     int
}

type diceExpr []diceTerm

// parseDice: expr := ( ('+'|'-')? term )+; term := [count]'d'sides[keep] |
// integer; keep := ('k'|'kh'|'kl') count. Case- and space-insensitive;
// bounds are checked before any roll.
func parseDice(raw string) (diceExpr, error) {
	s := strings.ToLower(strings.ReplaceAll(raw, " ", ""))
	if s == "" || len(s) > diceMaxNot {
		return nil, fmt.Errorf("dice: notation must be 1..%d characters", diceMaxNot)
	}
	var expr diceExpr
	for len(s) > 0 {
		sign := 1
		switch s[0] {
		case '+':
			s = s[1:]
		case '-':
			sign, s = -1, s[1:]
		}
		tok := s
		if end := strings.IndexAny(s, "+-"); end >= 0 {
			tok, s = s[:end], s[end:]
		} else {
			s = ""
		}
		t, err := parseDiceTerm(tok)
		if err != nil {
			return nil, fmt.Errorf("dice: bad notation %q (term %q): expected [count]d<sides>[kh|kl<n>] or an integer", raw, tok)
		}
		t.Sign = sign
		expr = append(expr, t)
		if len(expr) > diceMaxTerms {
			return nil, fmt.Errorf("dice: at most %d terms", diceMaxTerms)
		}
	}
	return expr, nil
}

func parseDiceTerm(tok string) (diceTerm, error) {
	if tok == "" {
		return diceTerm{}, fmt.Errorf("empty term")
	}
	if d := strings.IndexByte(tok, 'd'); d >= 0 {
		count, err := atoiDefault(tok[:d], 1)
		if err != nil {
			return diceTerm{}, err
		}
		rest := tok[d+1:]
		keep, keepCount := "", 0
		if k := strings.IndexByte(rest, 'k'); k >= 0 {
			keep = "high"
			ks := rest[k+1:]
			if len(ks) > 0 && (ks[0] == 'h' || ks[0] == 'l') {
				if ks[0] == 'l' {
					keep = "low"
				}
				ks = ks[1:]
			}
			keepCount, err = strconv.Atoi(ks)
			if err != nil || keepCount < 1 {
				return diceTerm{}, fmt.Errorf("keep count must be a positive integer")
			}
			rest = rest[:k]
		}
		sides, err := strconv.Atoi(rest)
		if err != nil {
			return diceTerm{}, fmt.Errorf("sides must be an integer")
		}
		if count < 1 || count > diceMaxCount {
			return diceTerm{}, fmt.Errorf("die count must be 1..%d", diceMaxCount)
		}
		if sides < 2 || sides > diceMaxSides {
			return diceTerm{}, fmt.Errorf("sides must be 2..%d", diceMaxSides)
		}
		if keep != "" && keepCount > count {
			return diceTerm{}, fmt.Errorf("keep count exceeds the number of dice")
		}
		return diceTerm{Kind: "dice", Count: count, Sides: sides, Keep: keep, KeepCount: keepCount}, nil
	}
	v, err := strconv.Atoi(tok)
	if err != nil || v < 0 || v > diceMaxConst {
		return diceTerm{}, fmt.Errorf("constant must be an integer 0..%d", diceMaxConst)
	}
	return diceTerm{Kind: "constant", Value: v}, nil
}

func atoiDefault(s string, def int) (int, error) {
	if s == "" {
		return def, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("die count must be an integer")
	}
	return v, nil
}

// roll executes the expression into the serializable per-term breakdown.
func (e diceExpr) roll(raw string, rl roller, seed *int64) (map[string]any, error) {
	total, modifier := 0, 0
	terms := make([]map[string]any, 0, len(e))
	for _, t := range e {
		if t.Kind == "constant" {
			modifier += t.Sign * t.Value
			terms = append(terms, map[string]any{"kind": "constant", "sign": t.Sign, "value": t.Value})
			continue
		}
		rolls := make([]int, 0, t.Count)
		for i := 0; i < t.Count; i++ {
			v, err := rl(uint64(t.Sides))
			if err != nil {
				return nil, err
			}
			rolls = append(rolls, int(v)+1)
		}
		kept := rolls
		if t.Keep != "" {
			kept = keepExtremes(rolls, t.Keep, t.KeepCount)
		}
		sum := 0
		for _, v := range kept {
			sum += v
		}
		total += t.Sign * sum
		m := map[string]any{"kind": "dice", "sign": t.Sign, "count": t.Count, "sides": t.Sides, "rolls": rolls, "sum": sum}
		if t.Keep != "" {
			m["keep"], m["keepCount"], m["kept"] = t.Keep, t.KeepCount, kept
		}
		terms = append(terms, m)
	}
	total += modifier
	out := map[string]any{"notation": raw, "terms": terms, "modifier": modifier, "total": total}
	if seed != nil {
		out["seed"] = *seed
	}
	return out, nil
}

// keepExtremes returns the keepCount highest or lowest values, ascending.
func keepExtremes(rolls []int, keep string, n int) []int {
	sorted := make([]int, len(rolls))
	copy(sorted, rolls)
	sort.Ints(sorted)
	if keep == "low" {
		return sorted[:n]
	}
	return sorted[len(sorted)-n:]
}
