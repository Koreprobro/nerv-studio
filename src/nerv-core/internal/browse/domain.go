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

// Package browse is the single mechanism behind the domain tools
// kb/artifactory/tools/loras (REBUILD §2.2, §2.6): one jail by
// construction (a root + this code), commands ls/probe/read/fetch/write,
// bounded output everywhere. Subdirectories are supported always; the
// flat layout of kb/tools is authoring style, not a contract.
package browse

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Budget carries the media/preview defaults (the config "media" section
// maps 1:1 here; zero fields fall back to package defaults).
type Budget struct {
	MaxBytes      int    // read preview cap
	MaxBlockBytes int    // per media block budget
	MaxDim        int    // image longest side, px
	VideoMaxDim   int    // video thumbnail longest side, px
	Quality       int    // JPEG quality, 1-100
	Format        string // "jpeg" | "png"
	Rate          int    // audio sample rate, Hz
	Channels      int    // audio channels
	Bitrate       string // audio bitrate, e.g. "64k"
	MaxDuration   int    // audio/video preview length, s
}

// DefaultBudget is the built-in fallback when the config leaves a field 0.
func DefaultBudget() Budget {
	return Budget{
		MaxBytes: 16 << 10, MaxBlockBytes: 512 << 10,
		MaxDim: 1600, VideoMaxDim: 800, Quality: 82, Format: "jpeg",
		Rate: 16000, Channels: 1, Bitrate: "64k", MaxDuration: 30,
	}
}

// Domain binds the mechanism to one storage root.
type Domain struct {
	Name   string // domain name in messages ("artifactory", "kb", …)
	Root   string // absolute jail root
	Budget Budget
}

// effective fills zero Budget fields from the built-in defaults.
func (d *Domain) effective() Budget {
	b, def := d.Budget, DefaultBudget()
	if b.MaxBytes <= 0 {
		b.MaxBytes = def.MaxBytes
	}
	if b.MaxBlockBytes <= 0 {
		b.MaxBlockBytes = def.MaxBlockBytes
	}
	if b.MaxDim <= 0 {
		b.MaxDim = def.MaxDim
	}
	if b.VideoMaxDim <= 0 {
		b.VideoMaxDim = def.VideoMaxDim
	}
	if b.Quality <= 0 {
		b.Quality = def.Quality
	}
	if b.Format == "" {
		b.Format = def.Format
	}
	if b.Rate <= 0 {
		b.Rate = def.Rate
	}
	if b.Channels <= 0 {
		b.Channels = def.Channels
	}
	if b.Bitrate == "" {
		b.Bitrate = def.Bitrate
	}
	if b.MaxDuration <= 0 {
		b.MaxDuration = def.MaxDuration
	}
	return b
}

// Resolve joins path under the root and rejects escapes; "" is the root.
func (d *Domain) Resolve(path string) (string, error) {
	if path == "" {
		return d.Root, nil
	}
	if filepath.IsAbs(path) {
		return "", fmt.Errorf("%s: absolute path not allowed: %q", d.Name, path)
	}
	rel := filepath.ToSlash(filepath.Clean(path))
	if rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("%s: path escapes the root: %q", d.Name, path)
	}
	return filepath.Join(d.Root, filepath.FromSlash(rel)), nil
}

// Bounding limits (no unbounded dumps).
const (
	treeMaxDepth  = 3
	treeMaxPerDir = 128
	listMaxEntry  = 512
	rangeReadMax  = 4 << 20 // text range reads are window-capped (memory-bomb guard)
)

// Node is one tree/list entry (bounded).
type Node struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"` // "dir" | "file"
	Size     int64   `json:"size,omitempty"`
	Files    int     `json:"files,omitempty"`
	Dirs     int     `json:"dirs,omitempty"`
	Children []*Node `json:"children,omitempty"`
	More     int     `json:"more,omitempty"` // children beyond the bound
}

// Ls lists a directory: one level by default, a bounded tree when recursive.
// Sort is dirs-first, then files, each by name.
func (d *Domain) Ls(path string, recursive bool) (*Node, error) {
	abs, err := d.Resolve(path)
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(filepath.Clean(path))
	if path == "" {
		name = d.Name
	}
	node := &Node{Name: name, Type: "dir"}
	if !st.IsDir() {
		node.Type, node.Size = "file", st.Size()
		return node, nil
	}
	depth := 1
	if recursive {
		depth = treeMaxDepth
	}
	if err := d.expand(abs, node, depth, listMaxEntry); err != nil {
		return nil, err
	}
	return node, nil
}

func (d *Domain) expand(abs string, node *Node, depth, max int) error {
	dirEntries, err := os.ReadDir(abs)
	if err != nil {
		return err
	}
	var dirs, files []os.DirEntry
	for _, de := range dirEntries {
		if de.IsDir() {
			dirs = append(dirs, de)
		} else {
			files = append(files, de)
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name() < dirs[j].Name() })
	sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
	node.Dirs, node.Files = len(dirs), len(files)
	all := append(dirs, files...)
	for i, de := range all {
		if len(node.Children) >= max {
			node.More = len(all) - i
			break
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		child := &Node{Name: de.Name()}
		if de.IsDir() {
			child.Type = "dir"
			if depth > 1 {
				if err := d.expand(filepath.Join(abs, de.Name()), child, depth-1, max); err != nil {
					return err
				}
			}
		} else {
			child.Type = "file"
			child.Size = info.Size()
		}
		node.Children = append(node.Children, child)
	}
	return nil
}
