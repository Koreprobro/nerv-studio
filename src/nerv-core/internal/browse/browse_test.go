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
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func newDomain(t *testing.T) (*Domain, string) {
	t.Helper()
	root := t.TempDir()
	return &Domain{Name: "test", Root: root}, root
}

func TestResolveGuards(t *testing.T) {
	d, _ := newDomain(t)
	if got, err := d.Resolve(""); err != nil || got != d.Root {
		t.Errorf("empty path: %q %v", got, err)
	}
	for _, bad := range []string{"..", "../up", "/abs/path", "a/../../b"} {
		if _, err := d.Resolve(bad); err == nil {
			t.Errorf("escape %q accepted", bad)
		}
	}
	if got, err := d.Resolve("a/../b.txt"); err != nil || !strings.HasSuffix(got, filepath.Join("b.txt")) {
		t.Errorf("internal ..: %q %v", got, err)
	}
}

func TestLsSortingBounds(t *testing.T) {
	d, root := newDomain(t)
	mustWrite(t, filepath.Join(root, "b.txt"), "one")
	mustWrite(t, filepath.Join(root, "adir", "keep.txt"), "k")
	mustMkdir(t, filepath.Join(root, "zdir", "inner"))
	node, err := d.Ls("", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(node.Children) != 3 {
		t.Fatalf("children: %d", len(node.Children))
	}
	if node.Children[0].Name != "adir" || node.Children[1].Name != "zdir" {
		t.Errorf("dirs must sort first: %+v", node.Children)
	}
	if node.Children[0].Children != nil {
		t.Error("non-recursive ls must not descend")
	}
	rec, err := d.Ls("", true)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Children[1].Children[0].Name != "inner" {
		t.Error("recursive ls must descend")
	}
	// Bound: 600 files → 512 listed + More.
	mustMkdir(t, filepath.Join(root, "more"))
	for i := 0; i < 600; i++ {
		mustWrite(t, filepath.Join(root, "more", fmt.Sprintf("f%03d.f", i)), "y")
	}
	more, err := d.Ls("more", false)
	if err != nil {
		t.Fatal(err)
	}
	if len(more.Children) != listMaxEntry || more.More != 600-listMaxEntry {
		t.Errorf("bound: %d children, more %d", len(more.Children), more.More)
	}
}

func TestProbeMeta(t *testing.T) {
	d, root := newDomain(t)
	mustWrite(t, filepath.Join(root, "doc.md"), "l1\nl2\nl3\n")
	m, err := d.Probe("doc.md")
	if err != nil {
		t.Fatal(err)
	}
	if m.Type != "file" || m.Lines != 3 || m.Mime != "text/plain; charset=utf-8" {
		t.Errorf("meta: %+v", m)
	}
	img := image.NewRGBA(image.Rect(0, 0, 40, 25))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	mustWriteBytes(t, filepath.Join(root, "p.png"), buf.Bytes())
	m2, err := d.Probe("p.png")
	if err != nil {
		t.Fatal(err)
	}
	if m2.Width != 40 || m2.Height != 25 {
		t.Errorf("image dims: %+v", m2)
	}
	mustMkdir(t, filepath.Join(root, "sub", "inner"))
	m3, err := d.Probe("sub")
	if err != nil {
		t.Fatal(err)
	}
	if m3.Type != "dir" || m3.Dirs != 1 {
		t.Errorf("dir meta: %+v", m3)
	}
	if _, err := d.Probe("../outside"); err == nil {
		t.Error("probe escape accepted")
	}
}

func TestReadRanges(t *testing.T) {
	d, root := newDomain(t)
	mustWrite(t, filepath.Join(root, "big.txt"), strings.Repeat("line\n", 100))
	tx, err := d.Read("big.txt", "10-12", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(tx.Content, "line") != 3 {
		t.Errorf("lines range: %q", tx.Content)
	}
	tx, err = d.Read("big.txt", "", "1-5", 0)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Content != "line\n" {
		t.Errorf("bytes range: %q", tx.Content)
	}
	if _, err := d.Read("big.txt", "1-2", "1-2", 0); err == nil {
		t.Error("mutually exclusive specs accepted")
	}
	tx, err = d.Read("big.txt", "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if !tx.Truncated || len(tx.Content) != 10 || tx.Note == "" {
		t.Errorf("preview cap: %+v", tx)
	}
	if _, err := d.Read("big.txt", "200-300", "", 0); err == nil {
		t.Error("range past end accepted")
	}
}

func TestFetchImageBudget(t *testing.T) {
	d, root := newDomain(t)
	img := image.NewRGBA(image.Rect(0, 0, 900, 700))
	for y := 0; y < 700; y += 7 {
		for x := 0; x < 900; x += 7 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 9, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	mustWriteBytes(t, filepath.Join(root, "big.png"), buf.Bytes())

	d.Budget.MaxBlockBytes = 64 << 10
	med, err := d.Fetch("big.png", FetchOpts{MaxDim: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(med.Data) == 0 || med.Mime != "image/jpeg" {
		t.Fatalf("fetch image: %+v", med)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(med.Data))
	if err != nil {
		t.Fatalf("preview must decode: %v", err)
	}
	if cfg.Width > 100 || cfg.Height > 100 {
		t.Errorf("max_dim ignored: %dx%d", cfg.Width, cfg.Height)
	}
	if len(med.Data) > 64<<10 && !med.Degraded {
		t.Error("over-budget block must be flagged degraded")
	}
}

func TestFetchAudioVideo(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not available")
	}
	d, root := newDomain(t)
	wav := filepath.Join(root, "tone.wav")
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error", "-f", "lavfi",
		"-i", "sine=frequency=440:duration=2", wav)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg generate: %v %s", err, out)
	}
	med, err := d.Fetch("tone.wav", FetchOpts{MaxDuration: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(med.Data) == 0 || med.Mime != "audio/mpeg" {
		t.Fatalf("audio fetch: %+v (%s)", med, med.Note)
	}
}

func TestWriteAndMkdir(t *testing.T) {
	d, root := newDomain(t)
	rel, err := d.WriteText("proj/notes.md", "hello", false)
	if err != nil || rel != "proj/notes.md" {
		t.Fatalf("write: %q %v", rel, err)
	}
	if _, err := d.WriteText("proj/notes.md", " world", true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root, "proj", "notes.md"))
	if string(b) != "hello world" {
		t.Errorf("append: %q", b)
	}
	if _, err := d.WriteText("run.sh", "x", false); err == nil {
		t.Error("executable extension must be rejected")
	}
	if _, err := d.WriteText("noext", "x", false); err == nil {
		t.Error("empty extension must be rejected")
	}
	if _, err := d.WriteText("../evil.md", "x", false); err == nil {
		t.Error("write escape accepted")
	}
	dir, err := d.Mkdir("proj/sub/")
	if err != nil || dir != "proj/sub/" {
		t.Fatalf("mkdir: %q %v", dir, err)
	}
	if _, err := d.Mkdir("proj/notes.md"); err == nil {
		t.Error("mkdir onto file accepted")
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustWriteBytes(t *testing.T, path string, data []byte) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
