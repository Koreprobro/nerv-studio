package mcp

import (
	"context"
	crand "crypto/rand"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// diceHint is the manual injected into the `dice.roll` tool's description:
// the roller is universal (full RPG notation) and fully serializable — the
// request and the result are plain JSON, so any roll, including a seeded
// one, can be stored and replayed verbatim.
const diceHint = `Roll dice — universal and fully serializable: request and result are plain JSON, so any roll can be stored and replayed verbatim.
notation: [count]d<sides> terms combined with + / -, optional keep on a dice term (4d6kh3 = keep the highest 3, 4d6kl1 = the lowest 1); bare integers are constant terms (2d8+1d4-1, d100).
seed (optional integer): makes the roll deterministic — resending the same {notation, seed} reproduces the exact same roll.`

// Roller bounds: a roll is a tiny call, not a compute job — anything beyond
// these caps is a mistake in the notation, not an intent.
const (
	diceHWDevice  = "/dev/hwrng"
	diceMaxNotLen = 96
	diceMaxTerms  = 16
	diceMaxCount  = 1000
	diceMaxSides  = 1_000_000
	diceMaxConst  = 1_000_000_000
)

// registerDiceTool wires the universal serializable dice roller.
func registerDiceTool(s *Server) {
	s.tools["dice.roll"] = &Tool{
		Name:        "dice.roll",
		Description: diceHint,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"notation": map[string]any{"type": "string", "description": "dice expression: 2d6, d100, 4d6kh3+2, 2d8+1d4-1"},
				"seed":     map[string]any{"type": "integer", "description": "optional; makes the roll deterministic and replayable"},
			},
			"required": []string{"notation"},
		},
		Call: func(ctx context.Context, args map[string]any) (any, error) {
			raw := asString(args, "notation")
			expr, err := parseDice(raw)
			if err != nil {
				return nil, err
			}
			rl, seed := diceSource(args)
			return expr.roll(raw, rl, seed)
		},
	}
}

// roller draws an unbiased uint64 in [0,n).
type roller func(n uint64) (uint64, error)

// diceSource picks the entropy backend: an explicit seed wins (deterministic
// replay); otherwise a physical draw — the hardware noise source when the
// node is accessible, else the kernel entropy pool. The choice is an
// implementation detail and is never reported to the client.
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

// jsonInt64 coerces a JSON-decoded number (float64 in practice) to int64.
func jsonInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case int64:
		return n, true
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

func openHWRNG() (*os.File, error) {
	hwOnce.Do(func() { hwFile, hwErr = os.Open(diceHWDevice) })
	return hwFile, hwErr
}

// entropyReader prefers the raw hardware noise source (/dev/hwrng — the
// on-die RNG on modern CPUs); when the node is absent or inaccessible it
// falls back to the kernel entropy pool, never to a plain PRNG.
func entropyReader() io.Reader {
	if f, err := openHWRNG(); err == nil {
		return f
	}
	return crand.Reader
}

// readerUint64n draws an unbiased value in [0,n) by rejection sampling over
// 8-byte reads (modulo bias is never introduced).
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

// diceTerm is one parsed term of the expression: dice or a signed constant.
type diceTerm struct {
	Kind      string // "dice" | "constant"
	Sign      int    // +1 | -1
	Count     int
	Sides     int
	Keep      string // "" | "high" | "low"
	KeepCount int
	Value     int // constant term
}

type diceExpr []diceTerm

// parseDice parses the notation grammar:
//
//	expr := ( ('+'|'-')? term )+      term := [count]'d'sides[keep] | integer
//	keep := ('k'|'kh'|'kl') count
//
// Case- and space-insensitive. All bounds are checked here, before any roll.
func parseDice(raw string) (diceExpr, error) {
	s := strings.ToLower(strings.ReplaceAll(raw, " ", ""))
	if s == "" || len(s) > diceMaxNotLen {
		return nil, fmt.Errorf("dice: notation must be 1..%d characters", diceMaxNotLen)
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

// roll executes the expression and returns the serializable result: the full
// per-term breakdown and the total.
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

// keepExtremes returns the keepCount highest (keep=="high") or lowest
// ("low") values of rolls, in ascending order.
func keepExtremes(rolls []int, keep string, n int) []int {
	sorted := make([]int, len(rolls))
	copy(sorted, rolls)
	sort.Ints(sorted)
	if keep == "low" {
		return sorted[:n]
	}
	return sorted[len(sorted)-n:]
}
