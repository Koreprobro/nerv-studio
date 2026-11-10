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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Meta is probe output: metadata only, content is not read beyond sniffing.
type Meta struct {
	Path        string            `json:"path"`
	Type        string            `json:"type"`
	Size        int64             `json:"size,omitempty"`
	ModTime     string            `json:"mtime,omitempty"`
	Mime        string            `json:"mime,omitempty"`
	Lines       int               `json:"lines,omitempty"`
	Width       int               `json:"width,omitempty"`
	Height      int               `json:"height,omitempty"`
	Media       map[string]string `json:"media,omitempty"` // ffprobe fields
	Safetensors map[string]string `json:"safetensors,omitempty"`
	Files       int               `json:"files,omitempty"`
	Dirs        int               `json:"dirs,omitempty"`
}

// Probe reports metadata of one path without reading its content
// (a sniff of the first 512 bytes is all the content it touches).
func (d *Domain) Probe(path string) (*Meta, error) {
	abs, err := d.Resolve(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	m := &Meta{Path: filepath.ToSlash(filepath.Clean(path)), Size: st.Size(),
		ModTime: st.ModTime().UTC().Format(time.RFC3339)}
	if path == "" {
		m.Path = "."
	}
	if st.IsDir() {
		m.Type = "dir"
		ents, err := os.ReadDir(abs)
		if err != nil {
			return nil, err
		}
		for _, de := range ents {
			if de.IsDir() {
				m.Dirs++
			} else {
				m.Files++
			}
		}
		return m, nil
	}
	m.Type = "file"
	m.Mime = sniffMime(abs)
	switch {
	case strings.EqualFold(filepath.Ext(abs), ".safetensors"):
		if md, err := SafetensorsMeta(abs); err == nil {
			m.Safetensors = md
		}
	case strings.HasPrefix(m.Mime, "image/"):
		if w, h, ok := imageDims(abs); ok {
			m.Width, m.Height = w, h
		}
	case isAudioExt(abs) || isVideoExt(abs):
		m.Media = ffprobeMedia(abs)
	default:
		if strings.HasPrefix(m.Mime, "text/") {
			if n, err := countLines(abs); err == nil {
				m.Lines = n
			}
		}
	}
	return m, nil
}

// SafetensorsMeta reads the JSON header of a safetensors file
// (metadata only — no tensors loaded).
func SafetensorsMeta(abs string) (map[string]string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var n uint64
	if err := binary.Read(f, binary.LittleEndian, &n); err != nil {
		return nil, fmt.Errorf("read safetensors header length: %w", err)
	}
	if n == 0 || n > 100<<20 {
		return nil, fmt.Errorf("safetensors header length out of range: %d", n)
	}
	hdr := make([]byte, n)
	if _, err := io.ReadFull(f, hdr); err != nil {
		return nil, fmt.Errorf("read safetensors header: %w", err)
	}
	var doc struct {
		Metadata map[string]string `json:"__metadata__"`
	}
	if err := json.Unmarshal(hdr, &doc); err != nil {
		return nil, fmt.Errorf("parse safetensors header: %w", err)
	}
	return doc.Metadata, nil
}

func sniffMime(abs string) string {
	f, err := os.Open(abs)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	b := make([]byte, 512)
	n, _ := f.Read(b)
	return http.DetectContentType(b[:n])
}

func countLines(abs string) (int, error) {
	f, err := os.Open(abs)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	buf := make([]byte, 64<<10)
	n := 0
	for {
		r, err := f.Read(buf)
		for _, c := range buf[:r] {
			if c == '\n' {
				n++
			}
		}
		if err != nil {
			return n, nil
		}
	}
}
