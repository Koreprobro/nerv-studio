package mcp

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nerv-core/internal/config"
)

// callToolErr calls a tool expecting an MCP tool error (isError content —
// M7.2) and returns the error text. It fails if the server answered with a
// JSON-RPC protocol error instead.
func callToolErr(t *testing.T, client *http.Client, url, name string, args map[string]any) string {
	t.Helper()
	resp := post(t, client, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("parse result for %s: %v (%s)", name, err, body)
	}
	if r.Error != nil {
		t.Fatalf("domain failure must be isError content, not a JSON-RPC error: %s", body)
	}
	if !r.Result.IsError || len(r.Result.Content) == 0 {
		t.Fatalf("expected isError result for %s, got %s", name, body)
	}
	return r.Result.Content[0].Text
}

// callToolText calls a tool and returns the raw first text block without
// JSON-decoding (for tools that answer plain text, e.g. browse get).
func callToolText(t *testing.T, client *http.Client, url, name string, args map[string]any) string {
	t.Helper()
	resp := post(t, client, url, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var r struct {
		Result struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatalf("parse result for %s: %v (%s)", name, err, body)
	}
	if r.Error != nil {
		t.Fatalf("unexpected JSON-RPC error for %s: %s", name, body)
	}
	if r.Result.IsError || len(r.Result.Content) == 0 {
		t.Fatalf("expected ok text result for %s, got %s", name, body)
	}
	return r.Result.Content[0].Text
}

// decodeBase64 decodes a content block's data field.
func decodeBase64(s string) ([]byte, error) { return base64.StdEncoding.DecodeString(s) }

// writeBusyPNG writes a checkerboard+gradient image whose JPEG encoding is
// expensive (high-frequency content), so a default-quality render of a big
// canvas blows past a small byte budget.
func writeBusyPNG(t *testing.T, root, rel string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := uint8((x*7 + y*13) % 256)
			if (x/4+y/4)%2 == 0 {
				img.Set(x, y, color.RGBA{R: c, G: uint8(255 - int(c)/2), B: c / 3, A: 255})
			} else {
				img.Set(x, y, color.RGBA{R: uint8(255 - int(c)), G: c, B: uint8(200 - int(c)/4), A: 255})
			}
		}
	}
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}

// browseFixtureServer builds a server over four populated roots.
func browseFixtureServer(t *testing.T, maxOutputChars int) (*httptest.Server, string) {
	t.Helper()
	afDir := t.TempDir()
	kbDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(kbDir, "note.md"), []byte("kb note"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(kbDir, "guides"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "guides", "roadmap.md"), []byte("kb guide"), 0o644); err != nil {
		t.Fatal(err)
	}
	toolsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(toolsDir, "smoke.sh"), []byte(smokeTool), 0o755); err != nil {
		t.Fatal(err)
	}
	loraDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(loraDir, "checkpoints"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeSafetensors(t, filepath.Join(loraDir, "checkpoints", "model.safetensors"), map[string]string{"format": "safetensors"})
	if err := os.WriteFile(filepath.Join(loraDir, "README.md"), []byte("lora readme"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ToolsDir:       toolsDir,
		KBDir:          kbDir,
		ArtifactoryDir: afDir,
		LoraModelDir:   loraDir,
		Port:           9696,
		MaxOutputChars: maxOutputChars,
	}
	srv := New(cfg, nil, nil, log.New(io.Discard, "", 0))
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, afDir
}

// TestBrowseSurface exercises the single directory tool: the root/command
// matrix, ls views, probe dispatch, get bounds, write guards and the traversal
// guard per root (M7.1).
func TestBrowseSurface(t *testing.T) {
	ts, afDir := browseFixtureServer(t, 0) // guard off for the surface tests
	client := &http.Client{}
	call := func(args map[string]any) any { return callTool(t, client, ts.URL, "browse", args) }

	// write on artifactory creates the file.
	m := call(map[string]any{"root": "artifactory", "command": "write", "path": "projects/plan.md", "content": "# Plan\nnotes"}).(map[string]any)
	if m["written"] != true || m["path"] != "projects/plan.md" {
		t.Fatalf("write result = %v", m)
	}
	if _, err := os.Stat(filepath.Join(afDir, "projects/plan.md")); err != nil {
		t.Fatalf("written file missing: %v", err)
	}

	// ls WITHOUT path: navigation tree with counters and NO file names.
	tree := callToolText(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "ls"})
	if strings.Contains(tree, "plan.md") {
		t.Fatalf("ls without path leaked file names: %s", tree)
	}
	var node map[string]any
	if err := json.Unmarshal([]byte(tree), &node); err != nil {
		t.Fatal(err)
	}
	if node["type"] != "dir" {
		t.Fatalf("ls root node type = %v", node["type"])
	}
	kids, _ := node["children"].([]any)
	foundProj := false
	for _, k := range kids {
		c := k.(map[string]any)
		if c["name"] == "projects" {
			foundProj = true
			if c["files"].(float64) != 1 {
				t.Fatalf("projects file counter = %v, want 1", c["files"])
			}
		}
	}
	if !foundProj {
		t.Fatalf("ls root tree missing projects/: %s", tree)
	}

	// ls WITH path: entry names.
	entries := call(map[string]any{"root": "artifactory", "command": "ls", "path": "."}).([]any)
	found := false
	for _, e := range entries {
		if e.(map[string]any)["name"] == "projects" {
			found = true
		}
	}
	if !found {
		t.Fatalf("artifactory ls (path=.) missing projects/: %v", entries)
	}

	// probe on artifactory: metadata + artifactURL, no content.
	p := call(map[string]any{"root": "artifactory", "command": "probe", "path": "projects/plan.md"}).(map[string]any)
	if p["sizeBytes"].(float64) != float64(len("# Plan\nnotes")) {
		t.Fatalf("probe sizeBytes = %v", p["sizeBytes"])
	}
	if url, _ := p["artifactURL"].(string); !strings.Contains(url, "/artifacts/projects/plan.md") {
		t.Fatalf("probe artifactURL = %v", p["artifactURL"])

		// kb: get returns document text; ls path returns names; recursive flat.
		if got := callToolText(t, client, ts.URL, "browse", map[string]any{"root": "kb", "command": "get", "path": "note.md"}); got != "kb note" {
			t.Fatalf("kb get = %q", got)
		}
		kbList := call(map[string]any{"root": "kb", "command": "ls", "path": "."}).([]any)
		foundNote := false
		for _, e := range kbList {
			if e.(map[string]any)["name"] == "note.md" {
				foundNote = true
			}
		}
		if !foundNote {
			t.Fatalf("kb ls missing note.md: %v", kbList)
		}
		rec := call(map[string]any{"root": "kb", "command": "ls", "path": ".", "recursive": true}).([]any)
		foundGuide := false
		for _, e := range rec {
			if e.(string) == "guides/roadmap.md" {
				foundGuide = true
			}
		}
		if !foundGuide {
			t.Fatalf("kb ls recursive missing guides/roadmap.md: %v", rec)
		}

		// tools root: ls lists scripts WITH one-line summaries.
		tls := call(map[string]any{"root": "tools", "command": "ls", "path": "."}).([]any)
		foundSmoke := false
		for _, e := range tls {
			c := e.(map[string]any)
			if c["name"] == "smoke.sh" {
				foundSmoke = true
				if c["summary"] != "smoke gen a thing" {
					t.Fatalf("tools ls summary = %v", c["summary"])
				}
			}
		}
		if !foundSmoke {
			t.Fatalf("tools ls missing smoke.sh: %v", tls)
		}

		// probe on tools: summary + fields, never the script source.
		d := call(map[string]any{"root": "tools", "command": "probe", "path": "smoke.sh"}).(map[string]any)
		if d["summary"] != "smoke gen a thing" {
			t.Fatalf("tools probe summary = %v", d["summary"])
		}
		if _, ok := d["fields"].([]any); !ok {
			t.Fatalf("tools probe fields = %v", d["fields"])
		}
		if strings.Contains(anyToString(d), "#!/usr/bin/env") {
			t.Fatal("tools probe leaked script source")
		}

		// probe on a tools DIR lists contained tools with summaries.
		tdir := call(map[string]any{"root": "tools", "command": "probe", "path": "."}).(map[string]any)
		toolsMeta, _ := tdir["tools"].([]any)
		if len(toolsMeta) != 1 || toolsMeta[0].(map[string]any)["name"] != "smoke.sh" {
			t.Fatalf("tools probe dir = %v", tdir)
		}

		// loras: probe dispatches on safetensors; get reads a text file.
		lp := call(map[string]any{"root": "loras", "command": "probe", "path": "checkpoints/model.safetensors"}).(map[string]any)
		sm, _ := lp["safetensorsMeta"].(map[string]any)
		if sm == nil || sm["format"] != "safetensors" {
			t.Fatalf("loras probe safetensorsMeta = %v", lp["safetensorsMeta"])
		}
		if _, ok := lp["artifactURL"]; ok {
			t.Fatal("artifactURL must only appear on the artifactory root")
		}
		if got := callToolText(t, client, ts.URL, "browse", map[string]any{"root": "loras", "command": "get", "path": "README.md"}); got != "lora readme" {
			t.Fatalf("loras get = %q", got)
		}

		// --- matrix refusals (isError content, not protocol errors) ---
		for _, bad := range []struct{ root, command string }{
			{"tools", "get"},   // script sources never leave the toolbox
			{"tools", "write"}, //
			{"kb", "write"},
			{"loras", "write"},
		} {
			msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": bad.root, "command": bad.command, "path": "."})
			if !strings.Contains(msg, "does not support command") {
				t.Fatalf("refusal %s/%s = %q", bad.root, bad.command, msg)
			}
			if !strings.Contains(msg, "ls") {
				t.Fatalf("refusal must list valid commands: %q", msg)
			}
		}
		// unknown root and unknown command.
		if msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": "bogus", "command": "ls"}); !strings.Contains(msg, "unknown root") {
			t.Fatalf("unknown root = %q", msg)
		}
		if msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "frobnicate"}); !strings.Contains(msg, "does not support command") {
			t.Fatalf("unknown command = %q", msg)
		}
		// write keeps the executable-format guard.
		if msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "write", "path": "x.sh", "content": "echo hi"}); !strings.Contains(msg, "disallowed file type") {
			t.Fatalf(".sh write = %q", msg)
		}
		// traversal guard on every root.
		for _, root := range []string{"artifactory", "tools", "kb", "loras"} {
			msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": root, "command": "ls", "path": "../../etc"})
			if !strings.Contains(msg, "escape") {
				t.Fatalf("traversal on %s not refused: %q", root, msg)
			}
		}
		// get on a directory is refused with a helpful message.
		if msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": "kb", "command": "get", "path": "guides"}); !strings.Contains(msg, "directory") {
			t.Fatalf("get on dir = %q", msg)
		}
	}

}

// TestVolumeGuard guards D4: oversized TEXT is refused with an actionable
// warning that reveals volume_ack; ack gets the full dump; text ranges and
// media blocks behave (image get never trips the char guard).
func TestVolumeGuard(t *testing.T) {
	ts, afDir := browseFixtureServer(t, 1500) // tiny limit to trip on demand
	client := &http.Client{}

	// fat directory: 300 files.
	fat := filepath.Join(afDir, "fat")
	if err := os.MkdirAll(fat, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 300; i++ {
		name := fmt.Sprintf("artifact_number_%03d.txt", i)
		if err := os.WriteFile(filepath.Join(fat, name), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// a numbered text doc for range paging.
	var doc strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&doc, "LINE %03d\n", i)
	}
	if err := os.MkdirAll(filepath.Join(afDir, "artifacts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(afDir, "artifacts", "doc.txt"), []byte(doc.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	// over-threshold ls: volume warning, no payload leak, reveals volume_ack.
	warn := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "ls", "path": "fat"})
	if !strings.Contains(warn, "volume guard") || !strings.Contains(warn, "volume_ack") {
		t.Fatalf("volume warning = %q", warn)
	}
	if strings.Contains(warn, "artifact_number_099") {
		t.Fatal("volume warning leaked the payload")
	}
	// ack → full dump.
	full := callTool(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "ls", "path": "fat", "volume_ack": true}).([]any)
	if len(full) != 300 {
		t.Fatalf("acked ls returned %d entries, want 300", len(full))
	}
	// volume_ack is NOT advertised in tools/list — revealed by the warning only.
	listResp := post(t, client, ts.URL, map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/list"})
	defer listResp.Body.Close()
	var lt map[string]any
	json.NewDecoder(listResp.Body).Decode(&lt)
	if strings.Contains(anyToString(lt), "volume_ack") {
		t.Fatal("volume_ack must not be advertised in tools/list")
	}

	// text ranges page inside the limit.
	seg := callToolText(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "get", "path": "artifacts/doc.txt", "lines": "10-12"})
	if !strings.Contains(seg, "LINE 010") || !strings.Contains(seg, "LINE 012") || strings.Contains(seg, "LINE 013") {
		t.Fatalf("lines range = %q", seg)
	}
	open := callToolText(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "get", "path": "artifacts/doc.txt", "lines": "99-"})
	if !strings.Contains(open, "LINE 099") || !strings.Contains(open, "LINE 100") || strings.Contains(open, "LINE 098") {
		t.Fatalf("open range = %q", open)
	}
	bseg := callToolText(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "get", "path": "artifacts/doc.txt", "bytes": "1-4"})
	if !strings.HasPrefix(bseg, "LINE") {
		t.Fatalf("bytes range = %q", bseg)
	}
	// lines + bytes together is a caller mistake.
	if msg := callToolErr(t, client, ts.URL, "browse", map[string]any{"root": "artifactory", "command": "get", "path": "artifacts/doc.txt", "lines": "1-2", "bytes": "1-2"}); !strings.Contains(msg, "mutually exclusive") {
		t.Fatalf("mixed ranges = %q", msg)
	}

	// media get never trips the TEXT guard even under a tiny limit: a PNG
	// returns an image block, not the warning.
	writeBigPNG(t, afDir, "artifacts/pic.png", 64, 64)
	out := callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "path": "artifacts/pic.png"})
	blocks := resultBlocks(t, out)
	if len(blocks) != 1 || blocks[0].Type != "image" {
		t.Fatalf("image get under tiny guard = %+v", blocks)
	}
}

// TestMediaByteBudget guards D4 max_block_bytes: a busy image over the budget
// at default quality/dims is auto-degraded until the encoded block fits.
func TestMediaByteBudget(t *testing.T) {
	ts, afDir := browseFixtureServer(t, 0)
	const rel = "artifacts/busy.png"
	writeBusyPNG(t, afDir, rel, 1200, 1200)

	// tiny budget: the result must still be an image block that fits it.
	const budget = 64 << 10
	out := callMCP(t, ts, "browse", map[string]any{"root": "artifactory", "command": "get", "path": rel, "max_block_bytes": budget})
	blocks := resultBlocks(t, out)
	if len(blocks) != 1 || blocks[0].Type != "image" {
		t.Fatalf("budgeted get = %+v", blocks)
	}
	raw, err := decodeBase64(blocks[0].Data)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > budget {
		t.Fatalf("encoded block %d bytes exceeds budget %d", len(raw), budget)
	}
}

// TestStrictWaveId guards M7.4: an unknown waveId is an error, not a silent
// fallback to the latest wave.
func TestStrictWaveId(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	// explicit unknown ids are refused even before any wave exists.
	if msg := callToolErr(t, client, ts.URL, "job.list", map[string]any{"waveId": 999}); !strings.Contains(msg, "unknown waveId") {
		t.Fatalf("job.list waveId=999 = %q", msg)
	}
	if msg := callToolErr(t, client, ts.URL, "job.status", map[string]any{"waveId": 42, "output": "whatever.png"}); !strings.Contains(msg, "unknown waveId") {
		t.Fatalf("job.status waveId=42 = %q", msg)
	}
	// no waveId with no waves: benign empty answer (not an error).
	if res := callTool(t, client, ts.URL, "job.list", map[string]any{}); len(res.(map[string]any)["jobs"].([]any)) != 0 {
		t.Fatalf("expected empty job list, got %v", res)
	}

	// submit a wave (blocking SSE stream here: gen declares a GPU), then query
	// it explicitly by id.
	sresp := post(t, client, ts.URL, map[string]any{
		"jsonrpc": "2.0", "id": 5, "method": "tools/call",
		"params": map[string]any{"name": "queue.submit", "arguments": map[string]any{
			"queue": []any{map[string]any{"tool": "gen.sh", "params": map[string]any{"output": "w1.png"}}},
		}},
	})
	readSSE(t, sresp)
	sresp.Body.Close()
	list := callTool(t, client, ts.URL, "job.list", map[string]any{"waveId": 1}).(map[string]any)
	if jobs, _ := list["jobs"].([]any); len(jobs) != 1 {
		t.Fatalf("wave 1 jobs = %v", list)
	}
	// an unknown id stays an error once waves exist.
	if msg := callToolErr(t, client, ts.URL, "job.list", map[string]any{"waveId": 77}); !strings.Contains(msg, "unknown waveId") {
		t.Fatalf("job.list waveId=77 = %q", msg)
	}
}

// TestTransportGuards guards M7.6: JSON-RPC batches are rejected explicitly
// and the RPC dispatch is served on /mcp only.
func TestTransportGuards(t *testing.T) {
	ts := newTestServer(t)
	client := &http.Client{}

	// batch body → Invalid Request (-32600), not a parse error.
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var br struct {
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &br); err != nil {
		t.Fatalf("batch response not JSON: %s", body)
	}
	if br.Error == nil || br.Error.Code != InvalidRequest || !strings.Contains(br.Error.Message, "batch") {
		t.Fatalf("batch response = %s", body)
	}

	// RPC on any other path is refused.
	req2, _ := http.NewRequest(http.MethodPost, ts.URL+"/", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req2.Header.Set("Content-Type", "application/json")
	resp2, err := client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	body2, _ := io.ReadAll(resp2.Body)
	var br2 struct {
		Error *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body2, &br2)
	if br2.Error == nil || br2.Error.Code != InvalidRequest {
		t.Fatalf("RPC on / accepted: %s", body2)
	}
}

// TestProgressTokenEcho guards M7.6: a tools/call carrying _meta.progressToken
// gets that token echoed in the wave's progress notifications.
func TestProgressTokenEcho(t *testing.T) {
	ts := newTestServer(t) // gen declares gpu-count 1 → blocking wave streams progress
	client := &http.Client{}
	body := map[string]any{
		"jsonrpc": "2.0", "id": 7, "method": "tools/call",
		"params": map[string]any{
			"name":      "queue.submit",
			"arguments": map[string]any{"queue": []any{map[string]any{"tool": "gen.sh", "params": map[string]any{"output": "pt.png"}}}},
			"_meta":     map[string]any{"progressToken": "tok-42"},
		},
	}
	resp := post(t, client, ts.URL, body)
	defer resp.Body.Close()
	events := readSSE(t, resp)
	var sawProgress, sawResult, sawToken bool
	for _, ev := range events {
		if m, _ := ev["method"].(string); m == "notifications/progress" {
			sawProgress = true
			params, _ := ev["params"].(map[string]any)
			if tok, _ := params["progressToken"].(string); tok == "tok-42" {
				sawToken = true
			}
		}
		if _, ok := ev["result"]; ok {
			sawResult = true
		}
	}
	if !sawResult {
		t.Fatalf("no result event in %v", events)
	}
	if !sawProgress {
		t.Fatalf("no progress event in %v", events)
	}
	if !sawToken {
		t.Fatalf("progress notifications must echo _meta.progressToken, events=%v", events)
	}
}
