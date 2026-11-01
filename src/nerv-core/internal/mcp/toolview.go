// toolview.go — the consumer-AI view of a tool's self-declared --tool-info
// interface (SPECS.md §3.1). Private fields are human-side (GUI workbench)
// and are DROPPED SILENTLY at the MCP boundary: no marker, no count, no hint —
// an inferable hidden class would drive the consumer-AI to hallucinate
// parameters. The raw interface (browse.Summarize) stays raw for the scheduler
// and the future user-scope raw endpoint; input is never filtered.
package mcp

// publicToolView returns the consumer-AI view of a tool interface map: the
// fields list with private entries removed, and the legacy top-level
// "private" map (pre-2026-10 contract) dropped defensively.
func publicToolView(iface map[string]any) map[string]any {
	out := make(map[string]any, len(iface))
	for k, v := range iface {
		if k == "private" {
			continue
		}
		if k == "fields" {
			out[k] = publicFields(v)
			continue
		}
		out[k] = v
	}
	return out
}

// publicFields filters a decoded "fields" JSON array, removing every field
// object marked "private": true. Anything that is not a field object passes
// through (a malformed interface must not panic the surface).
func publicFields(v any) any {
	fields, ok := v.([]any)
	if !ok {
		return v
	}
	kept := make([]any, 0, len(fields))
	for _, f := range fields {
		if obj, ok := f.(map[string]any); ok {
			if priv, ok := obj["private"].(bool); ok && priv {
				continue
			}
		}
		kept = append(kept, f)
	}
	return kept
}
