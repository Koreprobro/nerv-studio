package browse

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// miniSafetensors writes a valid safetensors file (8-byte LE header length +
// JSON header with __metadata__ + dummy tensor bytes) at path and returns it.
func miniSafetensors(t *testing.T, path string, meta map[string]string) string {
	t.Helper()
	header := map[string]any{
		"__metadata__": meta,
		"w":            map[string]any{"dtype": "F32", "shape": []int{2, 2}, "data_offsets": []int{0, 16}},
	}
	hb, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	var lenBuf [8]byte
	binary.LittleEndian.PutUint64(lenBuf[:], uint64(len(hb)))
	var data [16]byte
	if err := os.WriteFile(path, append(lenBuf[:], append(append([]byte{}, hb...), data[:]...)...), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestChildrenListsDirsThenFiles(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"b.txt", "a.txt"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := Children(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("want 3 entries, got %d: %v", len(entries), entries)
	}
	if entries[0].Type != "dir" || entries[0].Name != "sub" {
		t.Fatalf("first entry must be the dir 'sub', got %+v", entries[0])
	}
	if entries[1].Name != "a.txt" || entries[2].Name != "b.txt" {
		t.Fatalf("files not sorted by name: %v", entries)
	}
}

func TestTreeCountsAndBounds(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "deep/deeper"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"x.md", "y.md"} {
		if err := os.WriteFile(filepath.Join(root, "deep", n), []byte("z"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	node, err := Tree(root, "")
	if err != nil {
		t.Fatal(err)
	}
	if node.Type != "dir" || node.Files != 0 {
		t.Fatalf("root must be a dir with 0 top-level files, got type=%s files=%d", node.Type, node.Files)
	}
	if node.Dirs != 1 || len(node.Children) != 1 {
		t.Fatalf("root must have 1 subdir, got dirs=%d children=%d", node.Dirs, len(node.Children))
	}
	deep := node.Children[0]
	if deep.Files != 2 {
		t.Fatalf("deep dir must count 2 files, got %d", deep.Files)
	}
	if deep.Dirs != 1 || len(deep.Children) != 1 {
		t.Fatalf("deep dir must recurse into deeper, got dirs=%d children=%d", deep.Dirs, len(deep.Children))
	}
}

func TestWriteAllowAndReject(t *testing.T) {
	root := t.TempDir()

	rel, err := Write(root, "projects/plan.md", "# Hello\nnotes", false)
	if err != nil {
		t.Fatalf("write .md: %v", err)
	}
	if rel != "projects/plan.md" {
		t.Fatalf("rel = %q, want projects/plan.md", rel)
	}
	got, err := os.ReadFile(filepath.Join(root, "projects/plan.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "# Hello\nnotes" {
		t.Fatalf("content = %q", got)
	}

	if _, err := Write(root, "projects/plan.md", " more", true); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "projects/plan.md"))
	if string(got) != "# Hello\nnotes more" {
		t.Fatalf("append content = %q", got)
	}

	if _, err := Write(root, "projects/plan.md", "fresh", false); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(filepath.Join(root, "projects/plan.md"))
	if string(got) != "fresh" {
		t.Fatalf("overwrite content = %q", got)
	}

	if _, err := Write(root, "run.sh", "echo hi", false); err == nil {
		t.Fatal("expected rejection for .sh, got nil")
	}
	if _, err := Write(root, "bin", "#!", false); err == nil {
		t.Fatal("expected rejection for no extension, got nil")
	}
	if _, err := Write(root, "../escape.md", "x", false); err == nil {
		t.Fatal("expected rejection for traversal, got nil")
	}
}

func TestSafetensorsMeta(t *testing.T) {
	p := miniSafetensors(t, filepath.Join(t.TempDir(), "model.safetensors"), map[string]string{"format": "pt", "diffusers_config": `{"alpha":4.0}`})
	meta, err := SafetensorsMeta(p)
	if err != nil {
		t.Fatal(err)
	}
	if meta["format"] != "pt" {
		t.Fatalf("format = %q, want pt", meta["format"])
	}
	if meta["diffusers_config"] == "" {
		t.Fatalf("diffusers_config metadata missing: %v", meta)
	}
}

func TestSummarizeToolContract(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "gen.sh")
	const info = `{"summary":"gen a thing","fields":[{"name":"prompt","required":true}],"gpu-count":1}`
	if err := os.WriteFile(script, []byte("#!/usr/bin/env sh\ncase ${1:-} in --tool-info)\necho '"+info+"'\n;;\nesac\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	iface, err := Summarize(root, "gen.sh")
	if err != nil {
		t.Fatal(err)
	}
	if iface["summary"] != "gen a thing" {
		t.Fatalf("summary = %v", iface["summary"])
	}
	if got := ScriptSummary(root, "gen.sh"); got != "gen a thing" {
		t.Fatalf("ScriptSummary = %q", got)
	}
	// A broken/missing script yields "" and never aborts a listing.
	if got := ScriptSummary(root, "missing.sh"); got != "" {
		t.Fatalf("ScriptSummary(missing) = %q, want empty", got)
	}
}

func TestListFlatRecursive(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"a.sh", "sub/b.sh"} {
		if err := os.WriteFile(filepath.Join(root, n), []byte("#!/bin/sh\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := List(root, "")
	if err != nil {
		t.Fatal(err)
	}
	// names-only, root first, then every descendant path (dirs + files).
	if len(got) != 4 {
		t.Fatalf("want 4 paths (root + a.sh + sub + sub/b.sh), got %v", got)
	}
	if got[0] != "." {
		t.Fatalf("root path must be first, got %q", got[0])
	}
	contains := func(p string) bool {
		for _, g := range got {
			if g == p {
				return true
			}
		}
		return false
	}
	if !contains("a.sh") || !contains("sub") || !contains("sub/b.sh") {
		t.Fatalf("list missing expected paths: %v", got)
	}
	// Subpath lists under that directory only.
	sub, err := List(root, "sub")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 2 || sub[0] != "sub" || sub[1] != "sub/b.sh" {
		t.Fatalf("subpath list = %v", sub)
	}
	// Traversal out of root is rejected.
	if _, err := List(root, "../outside"); err == nil {
		t.Fatal("expected error for traversal outside root")
	}
}
