package mcp

import (
	"math/rand"
	"sort"
	"testing"
)

func TestParseDiceValid(t *testing.T) {
	cases := []struct {
		notation string
		terms    int
	}{
		{"2d6", 1}, {"d100", 1}, {"4d6kh3+2", 2}, {"2d8+1d4-1", 3},
		{" 3D6 ", 1}, {"1d20kl1", 1}, {"7", 1}, {"2d6-3", 2},
		{"1d4+1d4+1d4+3", 4},
	}
	for _, c := range cases {
		expr, err := parseDice(c.notation)
		if err != nil {
			t.Fatalf("parseDice(%q): %v", c.notation, err)
		}
		if len(expr) != c.terms {
			t.Fatalf("parseDice(%q): got %d terms, want %d", c.notation, len(expr), c.terms)
		}
	}
	e, err := parseDice("4d6kh3+2")
	if err != nil {
		t.Fatal(err)
	}
	if e[0].Kind != "dice" || e[0].Count != 4 || e[0].Sides != 6 || e[0].Keep != "high" || e[0].KeepCount != 3 {
		t.Fatalf("4d6kh3 parsed as %+v", e[0])
	}
	if e[1].Kind != "constant" || e[1].Value != 2 || e[1].Sign != 1 {
		t.Fatalf("modifier parsed as %+v", e[1])
	}
	if e, err := parseDice("2d8-1d4"); err != nil || e[1].Sign != -1 {
		t.Fatalf("negative term: %+v %v", e, err)
	}
}

func TestParseDiceInvalid(t *testing.T) {
	bad := []string{"", "d", "2d", "2d6k", "2d6k0", "2d6k5", "1d1", "0d6", "2d6+", "+", "1d2d3", "k2", "2d6k2k2", "1d9999999999"}
	for _, s := range bad {
		if _, err := parseDice(s); err == nil {
			t.Fatalf("parseDice(%q): expected error, got nil", s)
		}
	}
}

func TestDiceRollSeededReproducible(t *testing.T) {
	roll := func(seed float64) map[string]any {
		raw := "2d8+1d4-1"
		expr, err := parseDice(raw)
		if err != nil {
			t.Fatal(err)
		}
		rl, sd := diceSource(map[string]any{"seed": seed})
		if sd == nil || *sd != int64(seed) {
			t.Fatalf("seed mode not selected: %v", sd)
		}
		out, err := expr.roll(raw, rl, sd)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	a, b := roll(42), roll(42)
	if _, ok := a["source"]; ok {
		t.Fatal("result must not report the entropy source")
	}
	if a["total"] != b["total"] {
		t.Fatalf("same seed differs: %v vs %v", a["total"], b["total"])
	}
	ja, jb := anyToString(a), anyToString(b)
	if ja != jb {
		t.Fatalf("serialized seeded rolls differ:\n%s\n%s", ja, jb)
	}
	c := roll(43)
	if ja == anyToString(c) {
		t.Fatal("different seed produced an identical roll")
	}
}

func TestDiceRollRanges(t *testing.T) {
	raw := "2d6+1d4-1"
	expr, err := parseDice(raw)
	if err != nil {
		t.Fatal(err)
	}
	rl, _ := diceSource(map[string]any{"seed": float64(7)})
	for i := 0; i < 500; i++ {
		out, err := expr.roll(raw, rl, nil)
		if err != nil {
			t.Fatal(err)
		}
		total := out["total"].(int)
		if total < 2 || total > 15 { // 2*1+1-1 .. 2*6+4-1
			t.Fatalf("roll %d out of range: %d", i, total)
		}
	}
	// keep semantics: kept must be the extremes and sum consistent.
	expr, err = parseDice("4d6kh3")
	if err != nil {
		t.Fatal(err)
	}
	out, err := expr.roll("4d6kh3", func(n uint64) (uint64, error) { return uint64(rand.Int63n(int64(n))), nil }, nil)
	if err != nil {
		t.Fatal(err)
	}
	term := out["terms"].([]map[string]any)[0]
	rolls, kept := term["rolls"].([]int), term["kept"].([]int)
	if len(rolls) != 4 || len(kept) != 3 {
		t.Fatalf("keep shapes wrong: %v / %v", rolls, kept)
	}
	// kept must be exactly the top 3 of the sorted rolls (ties included).
	sortedRolls := append([]int(nil), rolls...)
	sort.Ints(sortedRolls)
	want := sortedRolls[len(sortedRolls)-3:]
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("kept %v is not the highest 3 of %v", kept, rolls)
		}
	}
	if term["sum"].(int) != kept[0]+kept[1]+kept[2] {
		t.Fatal("sum inconsistent with kept")
	}
}

func TestHWRNGSource(t *testing.T) {
	f, err := openHWRNG()
	if err != nil {
		t.Skipf("no hardware rng on this host: %v", err)
	}
	seen := map[uint64]bool{}
	for i := 0; i < 200; i++ {
		v, err := readerUint64n(f, 6)
		if err != nil {
			t.Fatalf("hwrng read: %v", err)
		}
		if v > 5 {
			t.Fatalf("hwrng value out of range: %d", v)
		}
		seen[v] = true
	}
	if len(seen) < 4 { // 200 draws over 6 faces missing >2 faces is ~impossible
		t.Fatalf("hwrng output looks degenerate: %v", seen)
	}
}

func TestDiceSourceFallback(t *testing.T) {
	rl, seed := diceSource(map[string]any{})
	if seed != nil {
		t.Fatal("unexpected seed")
	}
	for i := 0; i < 200; i++ {
		v, err := rl(20)
		if err != nil || v > 19 {
			t.Fatalf("roller: %d %v", v, err)
		}
	}
}
