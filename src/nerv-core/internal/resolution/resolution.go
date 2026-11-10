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

// Package resolution is the studio's resolution calculator: an aspect string
// in, generator-friendly WxH out. The MCP tool calc.resolution exposes it to
// the consumer-AI, who plans the canvas and hands the numbers to generation
// tools as width/height params — the algorithm lives here and nowhere else.
package resolution

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Spec bounds the search. Multiple is the side granularity (model/VAE
// divisor); Target is the preferred pixel count; the area is bounded
// MinSide^2..MaxSide^2, and a final side below MinSide is an error.
type Spec struct {
	Multiple int
	Target   int
	MinSide  int
	MaxSide  int
}

// Studio is the default spec: sides multiple of 32, ~2 MP target, area
// bounded 512^2..1536^2 (the sd-family envelope).
func Studio() Spec { return Spec{Multiple: 32, Target: 2_000_000, MinSide: 512, MaxSide: 1536} }

func (s Spec) normalized() Spec {
	d := Studio()
	if s.Multiple <= 0 {
		s.Multiple = d.Multiple
	}
	if s.Target <= 0 {
		s.Target = d.Target
	}
	if s.MinSide <= 0 {
		s.MinSide = d.MinSide
	}
	if s.MaxSide < s.MinSide {
		s.MaxSide = d.MaxSide
	}
	return s
}

// Ratio parses an aspect string: "16:9", "16x9", "16×9" or a plain decimal
// factor ("1.5"). A missing or non-positive side falls back to 1 (a zero
// aspect "0:0" is therefore square, matching the sh reference).
func Ratio(aspect string) (float64, error) {
	s := strings.NewReplacer("×", ":", "x", ":", "X", ":").Replace(strings.TrimSpace(aspect))
	if s == "" {
		return 0, fmt.Errorf("empty aspect")
	}
	num, den := s, ""
	if i := strings.Index(s, ":"); i >= 0 {
		num, den = s[:i], s[i+1:]
	}
	w, err := strconv.ParseFloat(strings.TrimSpace(num), 64)
	if err != nil {
		return 0, fmt.Errorf("bad aspect %q", aspect)
	}
	h := 1.0
	if strings.TrimSpace(den) != "" {
		if h, err = strconv.ParseFloat(strings.TrimSpace(den), 64); err != nil {
			return 0, fmt.Errorf("bad aspect %q", aspect)
		}
	}
	if !(w > 0) {
		w = 1
	}
	if !(h > 0) {
		h = 1
	}
	return w / h, nil
}

// Compute returns generator-friendly WxH for the aspect under spec: the ratio
// scaled to the pixel target with both sides rounded to the nearest multiple,
// the area pulled back inside MinSide^2..MaxSide^2 when it overshoots. A
// result with a side below MinSide is reported as an error but the pair is
// still returned (the caller may know better than the floor).
func Compute(aspect string, spec Spec) (int, int, error) {
	r, err := Ratio(aspect)
	if err != nil {
		return 0, 0, err
	}
	spec = spec.normalized()
	m := float64(spec.Multiple)
	round := func(x float64) int {
		q := int(math.Floor(x/m+0.5)) * spec.Multiple
		if q < spec.Multiple {
			q = spec.Multiple
		}
		return q
	}
	side := func(area float64) (int, int) {
		h := round(math.Sqrt(area / r))
		return round(float64(h) * r), h
	}
	minA := float64(spec.MinSide) * float64(spec.MinSide)
	maxA := float64(spec.MaxSide) * float64(spec.MaxSide)
	w, h := side(float64(spec.Target))
	if float64(w)*float64(h) > maxA {
		w, h = side(maxA)
	}
	if float64(w)*float64(h) < minA {
		w, h = side(minA)
	}
	if w < spec.MinSide || h < spec.MinSide {
		return w, h, fmt.Errorf("aspect %q needs side %d px minimum, got %dx%d", aspect, spec.MinSide, w, h)
	}
	return w, h, nil
}
