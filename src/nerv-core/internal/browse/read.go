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

package browse

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Text is read output: a byte-capped window with a paging note.
type Text struct {
	Content   string `json:"content"`
	Note      string `json:"note,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Read returns a bounded text window (command `read`): lines="A-B" or
// bytes="A-B" (1-based, inclusive; A or B may be empty), mutually
// exclusive. Ranges apply before the max_bytes cap — the AI pages through
// a document in slices instead of dumping it. Range reads are window
// capped (rangeReadMax): an unbounded ReadFile of a huge log would be a
// memory bomb before truncation applies.
func (d *Domain) Read(path, linesSpec, bytesSpec string, maxBytes int) (*Text, error) {
	abs, err := d.Resolve(path)
	if err != nil {
		return nil, err
	}
	b := d.effective()
	if maxBytes <= 0 {
		maxBytes = b.MaxBytes
	}
	if linesSpec != "" && bytesSpec != "" {
		return nil, fmt.Errorf("lines and bytes ranges are mutually exclusive (pick one)")
	}
	limit := int64(maxBytes) + 1
	if linesSpec != "" || bytesSpec != "" {
		limit = rangeReadMax
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return nil, err
	}
	total := int64(len(data))
	if st, err := f.Stat(); err == nil {
		total = st.Size()
	}
	out := &Text{}
	switch {
	case linesSpec != "":
		lo, hi, perr := parseRangeSpec(linesSpec)
		if perr != nil {
			return nil, fmt.Errorf(`bad lines range %q (want "A-B", 1-based inclusive, A or B optional)`, linesSpec)
		}
		lines := strings.Split(string(data), "\n")
		if lo > len(lines) {
			return nil, fmt.Errorf("lines range starts past end (%d lines in the read window)", len(lines))
		}
		end := len(lines)
		if hi > 0 && hi < end {
			end = hi
		}
		out.Content = strings.Join(lines[lo-1:end], "\n")
		out.Note = fmt.Sprintf("lines %d-%d", lo, end)
	case bytesSpec != "":
		lo, hi, perr := parseRangeSpec(bytesSpec)
		if perr != nil {
			return nil, fmt.Errorf(`bad bytes range %q (want "A-B", 1-based inclusive, A or B optional)`, bytesSpec)
		}
		if lo > len(data) {
			return nil, fmt.Errorf("bytes range starts past end (file is %d bytes)", total)
		}
		end := len(data)
		if hi > 0 && hi < end {
			end = hi
		}
		// ToValidUTF8 replaces sequences cut at the window edges.
		out.Content = strings.ToValidUTF8(string(data[lo-1:end]), "\uFFFD")
		out.Note = fmt.Sprintf("bytes %d-%d of %d", lo, end, total)
	default:
		text := string(data)
		if len(text) > maxBytes {
			text = text[:maxBytes]
			out.Truncated = true
		}
		out.Content = text
		if total > int64(len(text)) {
			out.Note = fmt.Sprintf("preview of %d bytes (max_bytes %d; pass lines/bytes to page)", total, maxBytes)
		}
	}
	if len(out.Content) > maxBytes { // range wider than the cap
		out.Content = out.Content[:maxBytes]
		out.Truncated = true
	}
	return out, nil
}

// parseRangeSpec parses "A-B" with optional sides ("5-", "-10", "3-7", "").
func parseRangeSpec(s string) (lo, hi int, err error) {
	s = strings.TrimSpace(s)
	lo, hi = 1, 0
	if s == "" {
		return 1, 0, nil
	}
	a, b, hasDash := strings.Cut(s, "-")
	if a != "" {
		lo, err = strconv.Atoi(a)
		if err != nil || lo < 1 {
			return 0, 0, fmt.Errorf("bad range bound %q", s)
		}
	}
	if hasDash && b != "" {
		hi, err = strconv.Atoi(b)
		if err != nil || hi < 1 {
			return 0, 0, fmt.Errorf("bad range bound %q", s)
		}
	}
	if hasDash && hi > 0 && hi < lo {
		return 0, 0, fmt.Errorf("range end before start: %q", s)
	}
	return lo, hi, nil
}
