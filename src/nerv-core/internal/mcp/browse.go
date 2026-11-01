// Package mcp — see jsonrpc.go. This file implements the single directory
// tool `browse` (PLAN-m7-surface D3): four commands (ls|probe|get|write) over
// four roots (artifactory|tools|kb|loras). The per-root scope matrix is
// enforced here, at one choke point: the tools root exposes metadata only
// (script sources never leave the toolbox), write exists on artifactory only,
// and artifactURL is attached on artifactory only.
package mcp

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"nerv-core/internal/browse"
)

// browseHint is the manual injected into the `browse` tool's description (the
// MCP "hint"): the whole directory surface in one page. volume_ack is
// deliberately NOT mentioned here — it is revealed only by the volume warning
// (PLAN-m7-surface D4), so a full dump is always a deliberate two-step act.
const browseHint = `Browse one studio root directory. Required: root, command.
Roots: artifactory (generated files; read + write), tools (sh tool tree; metadata only, script sources never leave the toolbox), kb (knowledge documents), loras (models and samples).
- ls [path] [recursive]  no path: navigation tree of subdirectories with file counters (no file names). path=<dir>: entry names, dirs first; recursive=true descends (both bounded). On the tools root each .sh entry carries its one-line summary.
- probe path             metadata only, by file type: .sh tool summary+fields, .safetensors header, image dims, video/audio duration+codec, text size+lines. Artifactory items also carry artifactURL (raw bytes via that link; render images in chat as ![alt](artifactURL)).
- get path               content into context, bounded: image -> downscaled JPEG, video -> single thumbnail, audio -> short preview, text -> capped. Text paging: lines="A-B" or bytes="A-B" (1-based, inclusive; A or B may be empty). Overrides: max_dim, quality, format(jpeg|png), max_bytes (text cap), max_block_bytes (media budget, default 512 KiB), seek, rate, channels, bitrate, max_duration. Not available on the tools root.
- write path content [append=true]  create/overwrite/append a TEXT file (artifactory root only).
Commands not listed for a root are refused. Output over the volume limit is refused with a warning — narrow the request or page with ranges.`

// kbRouterHint accompanies every ls result on the kb root (PLAN-kb K8): the
// KB is useless read out of order, and the README is the only router.
const kbRouterHint = "read kb/README.md first — the routing rules."

// browseRootCommands is the scope matrix (D3): the commands each root serves.
var browseRootCommands = map[string][]string{
	"artifactory": {"ls", "probe", "get", "write"},
	"tools":       {"ls", "probe"},
	"kb":          {"ls", "probe", "get"},
	"loras":       {"ls", "probe", "get"},
}

// browseRootDir maps a root name to its configured directory.
func (s *Server) browseRootDir(root string) (string, bool) {
	switch root {
	case "artifactory":
		return s.config.ArtifactoryDir, true
	case "tools":
		return s.config.ToolsDir, true
	case "kb":
		return s.config.KBDir, true
	case "loras":
		return s.config.LoraModelDir, true
	}
	return "", false
}

// registerBrowseTool wires the single unified directory tool (SPECS.md §4.1).
// volume_ack is honored by the volume guard but intentionally absent from the
// inputSchema: the consumer-AI learns it from the volume warning, not upfront.
func registerBrowseTool(s *Server) {
	props := map[string]any{
		"root": map[string]any{
			"type":        "string",
			"enum":        []string{"artifactory", "tools", "kb", "loras"},
			"description": "which studio directory to browse",
		},
		"command": map[string]any{
			"type":        "string",
			"enum":        []string{"ls", "probe", "get", "write"},
			"description": "action; per-root availability is in the tool description (tools: ls|probe only, kb/loras: no write, artifactory: all)",
		},
		"path":      stringSchema(),
		"recursive": boolSchema(),
		"content":   stringSchema(),
		"append":    boolSchema(),
	}
	for k, v := range artifactGetProps() {
		props[k] = v
	}
	s.tools["browse"] = &Tool{
		Name:        "browse",
		Description: browseHint,
		InputSchema: map[string]any{"type": "object", "properties": props, "required": []string{"root", "command"}},
		Call:        s.handleBrowse,
	}
}

// handleBrowse is the single choke point: root → dir lookup, command → matrix
// check, then dispatch. A command outside the root's column is refused with
// that root's valid commands listed.
func (s *Server) handleBrowse(ctx context.Context, args map[string]any) (any, error) {
	root := asString(args, "root")
	dir, ok := s.browseRootDir(root)
	if !ok {
		return nil, fmt.Errorf("unknown root %q (expected artifactory|tools|kb|loras)", root)
	}
	cmd := asString(args, "command")
	allowed := false
	for _, c := range browseRootCommands[root] {
		if c == cmd {
			allowed = true
			break
		}
	}
	if !allowed {
		return nil, fmt.Errorf("root %q does not support command %q; valid commands for this root: %s", root, cmd, strings.Join(browseRootCommands[root], "|"))
	}
	switch cmd {
	case "ls":
		out, err := s.browseLs(root, dir, args)
		if err != nil {
			return nil, err
		}
		if root == "kb" {
			// Router hint (PLAN-kb K8): every kb listing points at the routing
			// rules, so the consumer-AI reads kb/README.md before layer docs.
			return map[string]any{"listing": out, "hint": kbRouterHint}, nil
		}
		return out, nil
	case "probe":
		return s.browseProbe(ctx, root, dir, args)
	case "get":
		return s.browseGet(root, dir, args)
	case "write":
		doAppend, _ := args["append"].(bool)
		rel, err := browse.Write(dir, asString(args, "path"), asString(args, "content"), doAppend)
		if err != nil {
			return nil, err
		}
		return map[string]any{"path": rel, "written": true}, nil
	}
	return nil, fmt.Errorf("unhandled command: %q", cmd)
}

// browsePath resolves declared under the given root. The artifactory root
// keeps artifact semantics (an absolute on-disk path is accepted); the
// other roots are plain root-relative joins. Traversal outside the root is
// rejected either way.
func (s *Server) browsePath(root, dir, declared string) (abs, rel string, err error) {
	if root == "artifactory" {
		return s.resolveArtifactPath(declared)
	}
	abs, err = browse.Resolve(dir, declared)
	if err != nil {
		return "", "", err
	}
	r, err := filepath.Rel(dir, abs)
	if err != nil {
		return "", "", err
	}
	return abs, filepath.ToSlash(r), nil
}

// browseLs: without path a fool-proof navigation view (subdirectories + file
// counters, no file names — a zero-argument call cannot dump a thousand
// names); with path the directory's entry names (dirs first); recursive=true
// a flat bounded name list. On the tools root, .sh entries carry their
// one-line self-declared summary.
func (s *Server) browseLs(root, dir string, args map[string]any) (any, error) {
	path := asString(args, "path")
	if path == "" {
		return browse.Tree(dir, ".")
	}
	if rec, _ := args["recursive"].(bool); rec {
		return browse.List(dir, path)
	}
	entries, err := browse.Children(dir, path)
	if err != nil {
		return nil, err
	}
	if root != "tools" {
		return entries, nil
	}
	out := make([]map[string]any, 0, len(entries))
	for _, e := range entries {
		m := map[string]any{"name": e.Name, "type": e.Type}
		if e.Type == "file" && strings.EqualFold(filepath.Ext(e.Name), ".sh") {
			if sum := browse.ScriptSummary(dir, filepath.ToSlash(filepath.Join(path, e.Name))); sum != "" {
				m["summary"] = sum
			}
		}
		out = append(out, m)
	}
	return out, nil
}

// browseProbe returns metadata WITHOUT bytes, dispatched by file type: .sh
// (tools root only — never run scripts from other roots) → self-declared
// summary+fields via --tool-info; .safetensors → header metadata; image →
// dims; video/audio → duration/codec; text → size+lines. artifactURL is
// attached on the artifactory root only. No command ever returns file bytes
// or script source from here.
func (s *Server) browseProbe(ctx context.Context, root, dir string, args map[string]any) (any, error) {
	declared := asString(args, "path")
	if declared == "" {
		declared = asString(args, "artifact")
	}
	abs, rel, err := s.browsePath(root, dir, declared)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not found: %q", declared)
		}
		return nil, err
	}
	meta := map[string]any{"path": rel, "sizeBytes": info.Size()}
	if root == "artifactory" {
		meta["artifactURL"] = s.artifactURL(abs)
	}
	if info.IsDir() {
		meta["type"] = "dir"
		if root == "tools" {
			children, err := browse.Children(dir, declared)
			if err != nil {
				return nil, err
			}
			tools := make([]map[string]any, 0, len(children))
			for _, c := range children {
				if c.Type != "file" || !strings.EqualFold(filepath.Ext(c.Name), ".sh") {
					continue
				}
				iface, err := browse.Summarize(dir, filepath.ToSlash(filepath.Join(declared, c.Name)))
				if err != nil {
					continue // a broken tool never aborts the listing
				}
				view := publicToolView(iface)
				if HumanFrom(ctx) {
					view = iface // human-mode: the full interface, private fields included
				}
				tools = append(tools, map[string]any{"name": c.Name, "summary": view["summary"], "fields": view["fields"]})
			}
			meta["tools"] = tools
			meta["note"] = "probe one .sh path directly for its full self-declared interface"
		}
		return ToolResult{Content: []ToolContent{textContent(anyToString(meta))}}, nil
	}
	// Script contract introspection — tools root only, and only the declared
	// metadata command runs; the source is never returned.
	if root == "tools" && strings.EqualFold(filepath.Ext(abs), ".sh") {
		iface, err := browse.Summarize(dir, declared)
		if err != nil {
			return nil, fmt.Errorf("probe tool %q: %w", rel, err)
		}
		if !HumanFrom(ctx) {
			iface = publicToolView(iface) // consumer-AI never sees private fields (§3.1)
		}
		return ToolResult{Content: []ToolContent{textContent(anyToString(iface))}}, nil
	}
	if strings.EqualFold(filepath.Ext(abs), ".safetensors") {
		sm, err := browse.SafetensorsMeta(abs)
		if err != nil {
			return nil, err
		}
		meta["safetensorsMeta"] = sm
		return ToolResult{Content: []ToolContent{textContent(anyToString(meta))}}, nil
	}
	mime := sniffFile(abs)
	meta["mimeType"] = mime
	switch classify(mime, abs) {
	case "image":
		if cfg, format, ok := stdlibImageConfig(abs); ok {
			meta["width"], meta["height"], meta["format"] = cfg.Width, cfg.Height, format
		}
	case "video", "audio":
		for k, v := range ffprobeMedia(abs) {
			meta[k] = v
		}
	case "text":
		meta["lines"], _ = countLines(abs)
	}
	return ToolResult{Content: []ToolContent{textContent(anyToString(meta))}}, nil
}

// browseGet pulls bounded content into the model context. Reached only on the
// roots whose matrix column includes get (artifactory|kb|loras); the tools
// root is refused by the matrix, so script sources never leave the toolbox.
func (s *Server) browseGet(root, dir string, args map[string]any) (any, error) {
	declared := asString(args, "path")
	if declared == "" {
		declared = asString(args, "artifact")
	}
	abs, rel, err := s.browsePath(root, dir, declared)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("not found: %q", declared)
		}
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("get needs a file, %q is a directory", declared)
	}
	mime := sniffFile(abs)
	switch classify(mime, abs) {
	case "image":
		return s.getImage(abs, mime, args)
	case "video":
		return s.getVideoThumb(abs, args)
	case "audio":
		return s.getAudio(abs, args)
	case "text":
		return s.getText(abs, args)
	default:
		note := "unknown binary type; bytes not inlined"
		if root == "artifactory" {
			note = "unknown binary type; fetch raw bytes via the artifactURL"
		}
		return ToolResult{Content: []ToolContent{textContent(anyToString(map[string]any{
			"path": rel, "sizeBytes": info.Size(), "mimeType": mime, "note": note,
		}))}}, nil
	}
}
