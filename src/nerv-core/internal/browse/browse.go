// Package browse implements lazy, bounded filesystem browsing over a root
// directory (SPECS.md §4.1): an overview tree with file counters, a one-level
// list, and per-namespace detail. It also reads safetensors headers (metadata
// only, no tensor load) and writes text artifacts with executable-format
// protection (the `write` tool, SPECS.md §4.3). All traversal is anchored at
// the given root; a path that escapes the root via ".." is rejected.
package browse

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Entry is one child of a directory.
type Entry struct {
	Name string `json:"name"`
	Type string `json:"type"` // "dir" | "file"
	Size int64  `json:"size"`
}

// Node is a directory-tree node for overview (bounded).
type Node struct {
	Name     string  `json:"name"`
	Type     string  `json:"type"` // "dir" | "file"
	Size     int64   `json:"size,omitempty"`
	Files    int     `json:"files,omitempty"`    // files in this directory
	Dirs     int     `json:"dirs,omitempty"`     // subdirectories
	Children []*Node `json:"children,omitempty"` // bounded, recursive
}

// Bounds keep overview/list output concise (SPECS.md §4.1: no unbounded dumps).
const (
	overviewMaxDepth = 3   // recursion levels below the requested path
	overviewMaxEntry = 128 // children reported per directory
	listMaxEntry     = 512
)

// textExts is the allowlist of file types the `write` tool may create. It is
// deliberately conservative: executable / binary formats (.sh, .py, .js, .bin,
// .exe, .so, …) are NOT in it and are therefore rejected.
var textExts = map[string]struct{}{
	".md": {}, ".markdown": {}, ".txt": {}, ".text": {}, ".textproto": {},
	".json": {}, ".jsonl": {}, ".ndjson": {},
	".yaml": {}, ".yml": {}, ".toml": {}, ".ini": {}, ".cfg": {}, ".conf": {},
	".csv": {}, ".tsv": {}, ".tab": {},
	".xml": {}, ".html": {}, ".htm": {}, ".css": {}, ".scss": {}, ".less": {},
	".log": {}, ".env": {}, ".gitattributes": {}, ".gitignore": {},
	".rst": {}, ".adoc": {}, ".org": {}, ".nerv": {},
}

// allowedTextExt reports whether an extension (with leading dot, lower-cased) is
// a safe text type. An empty extension is rejected (must name the type).
func allowedTextExt(ext string) bool {
	_, ok := textExts[strings.ToLower(ext)]
	return ok
}

// resolve joins path to root and rejects traversal outside root. The returned
// path is the absolute, cleaned on-disk location.
func resolve(root, path string) (string, error) {
	if path == "" {
		path = "."
	}
	rel := filepath.ToSlash(filepath.Clean(path))
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if abs != root && !strings.HasPrefix(abs, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root: %q", path)
	}
	return abs, nil
}

// Resolve is the exported form: join path under root, rejecting traversal.
func Resolve(root, path string) (string, error) { return resolve(root, path) }

// Children lists the immediate children of a directory (bounded, sorted: dirs
// first, then files, each by name).
func Children(root, path string) ([]Entry, error) {
	abs, err := resolve(root, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("not a directory: %q", path)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	var dirs, files []Entry
	for _, e := range entries {
		ei, err := e.Info()
		if err != nil {
			continue
		}
		name := e.Name()
		if ei.IsDir() {
			dirs = append(dirs, Entry{Name: name, Type: "dir", Size: ei.Size()})
		} else {
			files = append(files, Entry{Name: name, Type: "file", Size: ei.Size()})
		}
	}
	out := make([]Entry, 0, len(dirs)+len(files))
	out = append(out, dirs...)
	out = append(out, files...)
	if len(out) > listMaxEntry {
		out = out[:listMaxEntry]
	}
	return out, nil
}

// Tree returns a bounded overview tree rooted at path (dirs first, then files,
// by name). Files at each level are counted; recursion stops at overviewMaxDepth
// and each directory reports at most overviewMaxEntry children.
func Tree(root, path string) (*Node, error) {
	abs, err := resolve(root, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	return buildNode(abs, info, 0), nil
}

func buildNode(abs string, info os.FileInfo, depth int) *Node {
	node := &Node{Name: info.Name(), Size: info.Size()}
	if !info.IsDir() {
		node.Type = "file"
		return node
	}
	node.Type = "dir"
	entries, err := os.ReadDir(abs)
	if err == nil {
		var dirs, files []os.DirEntry
		for _, e := range entries {
			if e.IsDir() {
				dirs = append(dirs, e)
			} else {
				files = append(files, e)
			}
		}
		node.Files = len(files)
		node.Dirs = len(dirs)
		// Only subdirectories are children (the navigation tree); files are
		// reachable via list/detail. Recursion stops at overviewMaxDepth.
		if depth < overviewMaxDepth {
			for _, e := range dirs {
				ei, err := e.Info()
				if err != nil {
					continue
				}
				node.Children = append(node.Children, buildNode(filepath.Join(abs, e.Name()), ei, depth+1))
			}
		}
	}
	if len(node.Children) > overviewMaxEntry {
		node.Children = node.Children[:overviewMaxEntry]
	}
	return node
}

// List returns a bounded, recursive, flat list of every directory and file
// path (slash-separated, relative to root) under path, starting with path
// itself. It reports names only — no size/type/summary metadata — and is
// bounded by overviewMaxDepth recursion and listMaxEntry total entries
// (SPECS.md §4.1c: no unbounded dumps). All traversal is anchored at root.
func List(root, path string) ([]string, error) {
	abs, err := resolve(root, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	rel := func(p string) string {
		r, _ := filepath.Rel(root, p)
		return filepath.ToSlash(r)
	}
	out := []string{rel(abs)}
	var walk func(dirAbs string, depth int) error
	walk = func(dirAbs string, depth int) error {
		if depth >= overviewMaxDepth {
			return nil
		}
		entries, err := os.ReadDir(dirAbs)
		if err != nil {
			return err
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			child := filepath.Join(dirAbs, name)
			ei, err := os.Stat(child)
			if err != nil {
				continue
			}
			out = append(out, rel(child))
			if ei.IsDir() && depth+1 < overviewMaxDepth {
				if err := walk(child, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if info.IsDir() {
		if err := walk(abs, 0); err != nil {
			return nil, err
		}
	}
	if len(out) > listMaxEntry {
		out = out[:listMaxEntry]
	}
	return out, nil
}

// SafetensorsMeta reads only the JSON header of a safetensors file and returns
// its __metadata__ map (string->string). It reads the 8-byte little-endian
// header-length prefix plus the header bytes and never loads tensor data.
func SafetensorsMeta(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var hdrLen uint64
	if err := binary.Read(f, binary.LittleEndian, &hdrLen); err != nil {
		return nil, fmt.Errorf("read safetensors header length: %w", err)
	}
	if hdrLen == 0 || hdrLen > 64<<20 {
		return nil, fmt.Errorf("safetensors header length out of range: %d", hdrLen)
	}
	hdr := make([]byte, hdrLen)
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

// Write creates, overwrites, or appends a text file under root (the artifactory
// root). Only allowed text extensions are accepted; path traversal is rejected.
// When append is false the file is truncated (created or overwritten). It
// returns the path relative to root.
func Write(root, path, content string, append bool) (string, error) {
	if path == "" {
		return "", fmt.Errorf("path is required")
	}
	rel := filepath.ToSlash(filepath.Clean(path))
	if rel == "." || rel == "/" {
		return "", fmt.Errorf("path must be a file: %q", path)
	}
	if !allowedTextExt(filepath.Ext(path)) {
		return "", fmt.Errorf("disallowed file type (write text files only): %q", filepath.Ext(path))
	}
	abs, err := resolve(root, path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return "", err
	}
	flag := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if append {
		flag = os.O_CREATE | os.O_APPEND | os.O_WRONLY
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return rel, nil
}

// sniffFile returns the MIME type of a file's first 512 bytes.
func sniffFile(abs string) string {
	f, err := os.Open(abs)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	b := make([]byte, 512)
	n, _ := f.Read(b)
	return http.DetectContentType(b[:n])
}

// imageConfig returns the pixel dimensions of a png/jpeg/gif image, if the
// stdlib can decode its config.
func imageConfig(abs string) (int, int, bool) {
	f, err := os.Open(abs)
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	cfg, _, err := image.DecodeConfig(f)
	if err != nil {
		return 0, 0, false
	}
	return cfg.Width, cfg.Height, true
}

// sortedExtList returns the allowed text extensions for error messages.
func sortedExtList() []string {
	out := make([]string, 0, len(textExts))
	for e := range textExts {
		out = append(out, e)
	}
	sort.Strings(out)
	return out
}
