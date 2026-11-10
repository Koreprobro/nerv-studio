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

package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeAt(t *testing.T, s Store, rel, content string) {
	t.Helper()
	abs := s.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveIndexAbsent(t *testing.T) {
	s := Store{Root: t.TempDir()}
	got, err := s.ArchiveIndex("nope.png")
	if err != nil || got != "" {
		t.Fatalf("absent file: got %q err %v", got, err)
	}
}

func TestArchiveIndexBeforeExtension(t *testing.T) {
	s := Store{Root: t.TempDir()}
	writeAt(t, s, "scene/a.png", "old")
	got, err := s.ArchiveIndex("scene/a.png")
	if err != nil || got != "scene/a-1.png" {
		t.Fatalf("first archive: got %q err %v", got, err)
	}
	if s.Exists("scene/a.png") {
		t.Error("original must move aside")
	}
	if b, _ := os.ReadFile(s.Abs("scene/a-1.png")); string(b) != "old" {
		t.Errorf("archived content: %q", b)
	}
	// Second conflict picks the next free index.
	writeAt(t, s, "scene/a.png", "old2")
	got, _ = s.ArchiveIndex("scene/a.png")
	if got != "scene/a-2.png" {
		t.Errorf("second archive: got %q", got)
	}
	// A pre-taken index is skipped.
	writeAt(t, s, "b.txt", "x")
	writeAt(t, s, "b-1.txt", "taken")
	got, _ = s.ArchiveIndex("b.txt")
	if got != "b-2.txt" {
		t.Errorf("index collision: got %q", got)
	}
	// No extension: plain -<i> suffix.
	writeAt(t, s, "LICENSE", "x")
	got, _ = s.ArchiveIndex("LICENSE")
	if got != "LICENSE-1" {
		t.Errorf("no extension: got %q", got)
	}
}

func TestArchiveIndexDirectory(t *testing.T) {
	s := Store{Root: t.TempDir()}
	if err := os.MkdirAll(s.Abs("d.png"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ArchiveIndex("d.png"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("directory at output path must error, got %v", err)
	}
}
