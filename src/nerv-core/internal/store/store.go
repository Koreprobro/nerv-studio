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

// Package store is the artifactory root helper (the old 30-line artifactory
// package folded in, REBUILD §4.4): paths inside the artifactory are
// canonical artifactory-relative; absolute composition happens only here.
package store

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Store is the artifactory root on disk.
type Store struct {
	Root string
}

// Abs composes an artifactory-relative path with the root.
func (s Store) Abs(rel string) string {
	return filepath.Join(s.Root, filepath.FromSlash(rel))
}

// Exists reports a regular file present at rel.
func (s Store) Exists(rel string) bool {
	st, err := os.Stat(s.Abs(rel))
	return err == nil && !st.IsDir()
}

// EnsureOutputDir creates the parent directory of an output path.
func (s Store) EnsureOutputDir(rel string) error {
	return os.MkdirAll(filepath.Dir(s.Abs(rel)), 0o755)
}

// ArchiveIndex moves an already-existing file at rel aside under an indexed
// name — index BEFORE the extension ("name-1.png", "name-2.png"; "name-1"
// when there is none), the first free index wins. It returns the archived
// rel path ("" when nothing existed to archive). This is the core's
// same-name overwrite protection (SPECS §6): the consumer-AI gets exactly
// the name it declared, conflicts resolve in favor of the new file.
func (s Store) ArchiveIndex(rel string) (string, error) {
	abs := s.Abs(rel)
	st, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if st.IsDir() {
		return "", fmt.Errorf("output path is a directory: %s", rel)
	}
	dir, base := path.Split(filepath.ToSlash(rel))
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for i := 1; i <= 10000; i++ {
		cand := dir + stem + "-" + strconv.Itoa(i) + ext
		_, err := os.Stat(s.Abs(cand))
		if err == nil {
			continue // index taken; try the next one
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		if err := os.Rename(abs, s.Abs(cand)); err != nil {
			return "", err
		}
		return cand, nil
	}
	return "", fmt.Errorf("no free archive index for %s", rel)
}
