// Package browse — tool-introspection helpers for the `browse` tool (SPECS.md
// §4.2). Only the self-declared `--tool-info` metadata command runs; no
// generation logic, and never the script source.
package browse

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Summarize runs <root>/<path> --tool-info and returns the parsed
// self-declared interface (summary + fields + gpu-count). Only the metadata
// command runs — no generation logic, never the script source.
func Summarize(root, path string) (map[string]any, error) {
	abs, err := resolve(root, path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return nil, fmt.Errorf("not a file: %q", path)
	}
	cmd := exec.Command("sh", abs, "--tool-info")
	// Isolate the probe: an unguarded script body must not see the daemon's
	// CWD/env (it would otherwise write garbage next to the daemon).
	cmd.Dir = os.TempDir()
	cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("run --tool-info: %v: %s", err, strings.TrimSpace(string(out)))
	}
	var iface map[string]any
	if err := json.Unmarshal(out, &iface); err != nil {
		return nil, fmt.Errorf("parse --tool-info: %w", err)
	}
	return iface, nil
}

// ScriptSummary returns the one-line `summary` a .sh tool self-declares, or ""
// when the script is absent/unreadable/fails --tool-info (a broken tool must
// never abort an ls listing).
func ScriptSummary(root, path string) string {
	iface, err := Summarize(root, path)
	if err != nil {
		return ""
	}
	if s, ok := iface["summary"].(string); ok {
		return s
	}
	return ""
}
