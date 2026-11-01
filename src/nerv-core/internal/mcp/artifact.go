// Package mcp — see jsonrpc.go. This file implements how generated artifacts
// return to the client: a read-only GET /artifacts/<rel> byte surface for GUI
// display, plus the artifactory probe (metadata only) and get (bounded,
// type-aware content block) commands. The design keeps bytes out of the
// wave/job responses (references only) so a batch of N images never becomes a
// token bomb in the consumer-AI context: the AI pulls an artifact in on demand
// via the get command, paying only for the one(s) it actually reasons about.
package mcp

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	_ "image/gif"
	"image/jpeg"
	png "image/png"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	xdraw "golang.org/x/image/draw"
)

// Default compression bounds per media type. Each yields a bounded preview so
// the get command cannot blow up model context; callers override via params.
const (
	imgDefaultMaxDim = 1600      // longest side, px
	imgDefaultQual   = 82        // JPEG quality
	mediaBlockBudget = 512 << 10 // max encoded bytes per media block (D4)
	txtDefaultMax    = 16 << 10  // 16 KiB text preview
	txtRangeReadMax  = 4 << 20   // max file bytes read to serve a text range
	vidDefaultMaxDim = 800       // thumbnail longest side, px
	vidDefaultSeek   = "00:00:01"
	audioDefaultRate = 16000
	audioDefaultChan = 1
	audioDefaultBit  = "64k"
	audioDefaultMaxD = 30
)

// resolveArtifactPath maps a declared artifact path (relative to the
// artifactory root, e.g. "scene/cyberpunk_city.png") to its on-disk
// location. It also accepts an absolute on-disk path. The returned
// rel is the clean, slash-joined path usable as an /artifacts/<rel> URL. A
// traversal outside the artifactory root is rejected.
func (s *Server) resolveArtifactPath(declared string) (abs, rel string, err error) {
	declared = strings.TrimSpace(declared)
	if declared == "" {
		return "", "", fmt.Errorf("empty artifact path")
	}
	if filepath.IsAbs(declared) {
		rel, err = filepath.Rel(s.artifact.Root, declared)
		rel = filepath.ToSlash(rel)
		if err != nil {
			return "", "", fmt.Errorf("cannot resolve %q: %w", declared, err)
		}
	} else {
		rel = filepath.ToSlash(filepath.Clean(declared))
	}
	abs = filepath.Join(s.artifact.Root, filepath.FromSlash(rel))
	if abs != s.artifact.Root && !strings.HasPrefix(abs, s.artifact.Root+string(os.PathSeparator)) {
		return "", "", fmt.Errorf("artifact path escapes artifactory root: %q", declared)
	}
	return abs, rel, nil
}

// artifactURLPath converts an absolute resolved artifact path (an on-disk
// location under the artifactory root) to the relative URL path the GUI reads
// it over (GET /artifacts/<rel>). It returns the path unchanged when it cannot
// be mapped under the artifactory root. Used by the job/wave responses so the
// client gets a fetchable /artifacts/<rel> link, not a server-absolute path.
func (s *Server) artifactURLPath(resolved string) string {
	if resolved == "" {
		return ""
	}
	_, rel, err := s.resolveArtifactPath(resolved)
	if err != nil {
		return resolved
	}
	return "/artifacts/" + rel
}

// artifactURL returns the full HTTP URL (scheme://host:port + the relative
// /artifacts/<rel> path) to fetch an artifact's bytes over GET
// /artifacts/<rel>. It wraps artifactURLPath so the client gets a directly
// clickable link, not just a relative path. The host is the MCP server's bind
// address (127.0.0.1, local-only deployment) and the port is cfg.Port.
func (s *Server) artifactURL(resolved string) string {
	rel := s.artifactURLPath(resolved)
	if rel == "" {
		return ""
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", s.config.Port, rel)
}

// artifactRel maps an artifact path (absolute on-disk or artifactory-relative)
// to the artifactory-relative path (slash form) — the value the consumer feeds
// back to browse as root=artifactory path=..., and the tail of artifactURL
// (artifactURL = http://host:port + "/artifacts/" + artifactRel).
func (s *Server) artifactRel(resolved string) string {
	if resolved == "" {
		return ""
	}
	_, rel, err := s.resolveArtifactPath(resolved)
	if err != nil {
		return resolved
	}
	return rel
}

// serveArtifact serves raw artifact bytes for the GUI. It is read-only, works
// on immutable artifacts (aggressive immutable cache header), and supports
// HTTP Range. Content-Type is sniffed by http.ServeContent.
func (s *Server) serveArtifact(w http.ResponseWriter, r *http.Request) {
	rel := filepath.ToSlash(filepath.Clean(strings.TrimPrefix(r.URL.Path, "/artifacts/")))
	abs, _, err := s.resolveArtifactPath(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		if os.IsNotExist(err) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	// Artifacts are immutable and append-only indexed; cache forever.
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, filepath.Base(abs), time.Time{}, f)
}

// The probe/get command surface lives in browse.go (the single `browse` tool);
// this file provides the URL helpers, the raw byte surface, and the bounded
// per-type renderers below.

// blockBudget resolves the per-media-block byte budget (D4): encoded blocks
// over the budget are auto-degraded by the renderers, never dumped whole.
func blockBudget(args map[string]any) int {
	b := optInt(args, "max_block_bytes", mediaBlockBudget)
	if b < 1 {
		b = mediaBlockBudget
	}
	return b
}

// getImage returns a bounded image block. png/jpeg/gif are decoded, downscaled
// past max_dim and re-encoded (JPEG by default, PNG on request). The encoded
// block must fit max_block_bytes: over budget the renderer steps quality down
// first, then shrinks dimensions, until it fits (D4 media budget).
// Unsupported image formats (webp/heic/avif) pass through as raw bytes, capped
// to the same budget — an oversized binary inlined into context is a token
// bomb (audit M6).
func (s *Server) getImage(abs, mime string, args map[string]any) (any, error) {
	maxDim := optInt(args, "max_dim", imgDefaultMaxDim)
	qual := optInt(args, "quality", imgDefaultQual)
	format := optString(args, "format", "jpeg")
	budget := blockBudget(args)
	if maxDim < 1 {
		maxDim = imgDefaultMaxDim
	}

	if img, err := decodeImage(abs); err == nil {
		b := img.Bounds()
		w, h := b.Dx(), b.Dy()
		scale := 1.0
		if float64(w) > float64(maxDim) || float64(h) > float64(maxDim) {
			scale = float64(maxDim) / float64(max(w, h))
		}
		if out, outMime, ok := encodeFitting(img, w, h, scale, qual, format, budget); ok {
			return ToolResult{Content: []ToolContent{imageContent(out, outMime)}}, nil
		}
	}
	// Undecodable format: pass raw bytes through, but capped to the budget.
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if len(data) > budget {
		return ToolResult{Content: []ToolContent{textContent(fmt.Sprintf(
			"raw image too large to inline: %d bytes (budget %d), format %s — fetch via GET /artifacts/<rel> or request a smaller max_dim", len(data), budget, mime))}}, nil
	}
	return ToolResult{Content: []ToolContent{imageContent(data, mime)}}, nil
}

// encodeFitting encodes img (scaled) and degrades it until it fits budget:
// quality steps down first, then the scale shrinks. Returns the last encoded
// block even if still over budget (best effort — a degraded preview beats an
// error), with ok=false so callers can note it.
func encodeFitting(img image.Image, w, h int, scale float64, qual int, format string, budget int) ([]byte, string, bool) {
	q := qual
	sc := scale
	var lastOut []byte
	var lastMime string
	for attempt := 0; attempt < 8; attempt++ {
		nw := int(float64(w) * sc)
		nh := int(float64(h) * sc)
		if nw < 1 {
			nw = 1
		}
		if nh < 1 {
			nh = 1
		}
		out, outMime, err := encodeImage(img, nw, nh, q, format)
		if err != nil {
			return lastOut, lastMime, false
		}
		lastOut, lastMime = out, outMime
		if len(out) <= budget {
			return out, outMime, true
		}
		if q > 40 {
			q -= 15
			continue
		}
		sc *= 0.7
		if sc < 0.1 {
			break
		}
	}
	return lastOut, lastMime, lastOut != nil && len(lastOut) <= budget
}

// getVideoThumb collapses a video to a single thumbnail frame (mpeg-2jpeg at
// max_dim) so it lands in context as a small image rather than a big clip.
func (s *Server) getVideoThumb(abs string, args map[string]any) (any, error) {
	maxDim := optInt(args, "max_dim", vidDefaultMaxDim)
	seek := optString(args, "seek", vidDefaultSeek)
	if maxDim < 1 {
		maxDim = vidDefaultMaxDim
	}
	out, err := os.CreateTemp("", "nerv-vid-*.jpg")
	if err != nil {
		return videoFallback(err), nil
	}
	defer os.Remove(out.Name())
	defer out.Close()

	// Compute an explicit W:H so the vf expression stays a plain integer
	// (no quoting/expr ambiguity). Fall back to a square cap if dims are unknown.
	vf := fmt.Sprintf("scale=%d:%d", maxDim, maxDim)
	if w, h, ok := probeVideoDims(abs); ok {
		sw, sh := fit(w, h, maxDim)
		vf = fmt.Sprintf("scale=%d:%d", sw, sh)
	}
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-ss", seek, "-i", abs, "-frames:v", "1",
		"-vf", vf,
		"-codec:v", "mjpeg", "-pix_fmt", "yuv420p", "-q:v", "8",
		"-strict", "unofficial", out.Name())
	if err := run(cmd); err != nil {
		return videoFallback(err), nil
	}
	data, err := os.ReadFile(out.Name())
	if err != nil {
		return videoFallback(err), nil
	}
	if len(data) == 0 {
		return videoFallback(fmt.Errorf("empty thumbnail (seek past end?)")), nil
	}
	// Media budget (D4): a thumbnail over max_block_bytes is re-encoded from
	// its own bytes, degrading quality then dimensions until it fits.
	if budget := blockBudget(args); len(data) > budget {
		if img, _, derr := image.Decode(bytes.NewReader(data)); derr == nil {
			b := img.Bounds()
			if fit, fitMime, _ := encodeFitting(img, b.Dx(), b.Dy(), 1.0, imgDefaultQual, "jpeg", budget); fit != nil {
				return ToolResult{Content: []ToolContent{imageContent(fit, fitMime)}}, nil
			}
		}
	}
	return ToolResult{Content: []ToolContent{imageContent(data, "image/jpeg")}}, nil
}

// getAudio returns a short, low-bitrate mono preview so audio stays small in
// context.
func (s *Server) getAudio(abs string, args map[string]any) (any, error) {
	rate := optInt(args, "rate", audioDefaultRate)
	chanN := optInt(args, "channels", audioDefaultChan)
	bit := optString(args, "bitrate", audioDefaultBit)
	maxD := optInt(args, "max_duration", audioDefaultMaxD)
	if maxD < 1 {
		maxD = audioDefaultMaxD
	}
	out, err := os.CreateTemp("", "nerv-aud-*.mp3")
	if err != nil {
		return audioFallback(err), nil
	}
	defer os.Remove(out.Name())
	defer out.Close()
	cmd := exec.Command("ffmpeg", "-y", "-loglevel", "error",
		"-i", abs, "-t", fmt.Sprintf("%d", maxD),
		"-ar", fmt.Sprintf("%d", rate), "-ac", fmt.Sprintf("%d", chanN),
		"-b:a", bit, "-f", "mp3", out.Name())
	if err := run(cmd); err != nil {
		return audioFallback(err), nil
	}
	data, err := os.ReadFile(out.Name())
	if err != nil {
		return audioFallback(err), nil
	}
	// Media budget (D4): mp3 size is ~linear in duration, so an over-budget
	// preview is re-cut proportionally shorter (one retry, floor 1s).
	if budget := blockBudget(args); len(data) > budget {
		shorter := int(int64(maxD) * int64(budget) / int64(len(data)))
		if shorter < 1 {
			shorter = 1
		}
		if shorter < maxD {
			if err := run(exec.Command("ffmpeg", "-y", "-loglevel", "error",
				"-i", abs, "-t", fmt.Sprintf("%d", shorter),
				"-ar", fmt.Sprintf("%d", rate), "-ac", fmt.Sprintf("%d", chanN),
				"-b:a", bit, "-f", "mp3", out.Name())); err == nil {
				if d2, err2 := os.ReadFile(out.Name()); err2 == nil && len(d2) > 0 {
					data = d2
				}
			}
		}
	}
	return ToolResult{Content: []ToolContent{audioContent(data, "audio/mpeg")}}, nil
}

// getText returns a byte-capped text preview with optional range selection
// (D4): lines="A-B" or bytes="A-B" (1-based, inclusive; empty A means "from
// the start", empty B means "to the end", mutually exclusive). Ranges are
// applied before the max_bytes cap, so the AI pages through a document in
// slices instead of dumping it. Only the read window is capped — an unbounded
// ReadFile of a huge log would be a memory bomb before truncation even
// applies (audit M6).
func (s *Server) getText(abs string, args map[string]any) (any, error) {
	maxBytes := optInt(args, "max_bytes", txtDefaultMax)
	if maxBytes < 1 {
		maxBytes = txtDefaultMax
	}
	linesSpec := asString(args, "lines")
	bytesSpec := asString(args, "bytes")
	if linesSpec != "" && bytesSpec != "" {
		return nil, fmt.Errorf(`lines and bytes ranges are mutually exclusive (pick one)`)
	}

	// Read the window: a plain preview needs only maxBytes; a range may point
	// anywhere in the file, so it reads up to txtRangeReadMax.
	readLimit := int64(maxBytes) + 1
	needRange := linesSpec != "" || bytesSpec != ""
	if needRange {
		readLimit = txtRangeReadMax
	}
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, readLimit))
	if err != nil {
		return nil, err
	}
	total := "?"
	if st, err := f.Stat(); err == nil {
		total = strconv.FormatInt(st.Size(), 10)
	}

	text := string(data)
	note := ""
	switch {
	case linesSpec != "":
		lo, hi, perr := parseRangeSpec(linesSpec)
		if perr != nil {
			return nil, fmt.Errorf(`bad lines range %q (expected "A-B", 1-based inclusive, A or B optional)`, linesSpec)
		}
		lines := strings.Split(text, "\n")
		if lo > len(lines) {
			return nil, fmt.Errorf("lines range starts past end (file has %d lines in the read window)", len(lines))
		}
		end := len(lines)
		if hi > 0 && hi < end {
			end = hi
		}
		text = strings.Join(lines[lo-1:end], "\n")
		note = fmt.Sprintf("\n[lines %d-%d%s]", lo, end, rangeTotalSuffix(hi == 0 || hi > len(lines), len(lines)))
	case bytesSpec != "":
		lo, hi, perr := parseRangeSpec(bytesSpec)
		if perr != nil {
			return nil, fmt.Errorf(`bad bytes range %q (expected "A-B", 1-based inclusive, A or B optional)`, bytesSpec)
		}
		if lo > len(data) {
			return nil, fmt.Errorf("bytes range starts past end (file is %s bytes)", total)
		}
		end := len(data)
		if hi > 0 && hi < end {
			end = hi
		}
		// ToValidUTF8 replaces a sequence truncated at the window edges.
		seg := data[lo-1 : end]
		text = strings.ToValidUTF8(string(seg), "\uFFFD")
		note = fmt.Sprintf("\n[bytes %d-%d of %s]", lo, end, total)
	case len(data) > maxBytes:
		text = string(data[:maxBytes]) + fmt.Sprintf("\n... [truncated, %d of %s bytes shown]", maxBytes, total)
	}

	// The max_bytes cap applies to the selected window too.
	if len(text) > maxBytes && note == "" {
		text = text[:maxBytes] + fmt.Sprintf("\n... [truncated, %d of %s bytes shown]", maxBytes, total)
	}
	if note != "" {
		budget := maxBytes - len(note)
		if budget < 0 {
			budget = 0
		}
		if len(text) > budget {
			text = text[:budget] + "\n... [range window truncated by max_bytes]" + note
		} else {
			text += note
		}
	}
	return ToolResult{Content: []ToolContent{textContent(text)}}, nil
}

// parseRangeSpec parses "A-B", "A-", "-B" into 1-based inclusive bounds
// (hi == 0 means "to the end").
func parseRangeSpec(v string) (lo, hi int, err error) {
	i := strings.IndexByte(v, '-')
	if i < 0 {
		return 0, 0, fmt.Errorf("missing '-'")
	}
	loS, hiS := strings.TrimSpace(v[:i]), strings.TrimSpace(v[i+1:])
	if loS != "" {
		lo, err = strconv.Atoi(loS)
		if err != nil || lo < 1 {
			return 0, 0, fmt.Errorf("bad start")
		}
	} else {
		lo = 1
	}
	if hiS != "" {
		hi, err = strconv.Atoi(hiS)
		if err != nil || hi < 1 {
			return 0, 0, fmt.Errorf("bad end")
		}
		if hi < lo {
			return 0, 0, fmt.Errorf("end before start")
		}
	}
	return lo, hi, nil
}

func rangeTotalSuffix(open bool, total int) string {
	if open {
		return fmt.Sprintf(" of %d", total)
	}
	return ""
}

// --- helpers ---------------------------------------------------------------

func imageContent(data []byte, mime string) ToolContent {
	return ToolContent{Type: "image", Data: data, MimeType: mime}
}
func audioContent(data []byte, mime string) ToolContent {
	return ToolContent{Type: "audio", Data: data, MimeType: mime}
}
func textContent(s string) ToolContent {
	return ToolContent{Type: "text", Text: s}
}

// classify buckets a sniffed MIME type (+ extension fallback) for media
// handling.
func classify(mime, path string) string {
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image"
	case strings.HasPrefix(mime, "video/"):
		return "video"
	case strings.HasPrefix(mime, "audio/"):
		return "audio"
	case strings.HasPrefix(mime, "text/"), mime == "application/json", mime == "application/xml", mime == "application/csv", isTextExt(path):
		return "text"
	default:
		return "binary"
	}
}

func isTextExt(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".txt", ".md", ".markdown", ".csv", ".log", ".json", ".xml", ".yaml", ".yml", ".toml", ".sh", ".py", ".go":
		return true
	}
	return false
}

// stdlibImageConfig decodes only the image config (no pixel decode) to get
// dimensions and format for png/jpeg/gif.
func stdlibImageConfig(abs string) (image.Config, string, bool) {
	f, err := os.Open(abs)
	if err != nil {
		return image.Config{}, "", false
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return image.Config{}, "", false
	}
	return cfg, format, true
}

func decodeImage(abs string) (image.Image, error) {
	f, err := os.Open(abs)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	return img, nil
}

// encodeImage resizes src to nw x nh and encodes it as JPEG (quality) or PNG
// (lossless). Scaling uses golang.org/x/image/draw.ApproxBiLinear: stdlib
// image/draw.Draw never resamples (it blits 1:1), which silently cropped the
// source instead of downscaling it (audit M1).
func encodeImage(src image.Image, nw, nh, qual int, format string) ([]byte, string, error) {
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	xdraw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	var (
		buf bytes.Buffer
		err error
	)
	if format == "png" {
		err = png.Encode(&buf, dst)
		return buf.Bytes(), "image/png", err
	}
	err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: qual})
	return buf.Bytes(), "image/jpeg", err
}

// ffprobeMedia runs ffprobe and returns a flat map of the relevant stream/format
// fields. It degrades gracefully: on missing tool or parse failure it returns
// nil so probe still reports size + mime.
func ffprobeMedia(abs string) map[string]any {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_format", "-show_streams",
		"-of", "default=noprint_wrappers=1", abs).CombinedOutput()
	if err != nil {
		return nil
	}
	m := map[string]any{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "duration", "codec_name", "width", "height", "frame_rate", "sample_rate", "channels":
			m[k] = v
		}
	}
	return m
}

func countLines(abs string) (int, error) {
	f, err := os.Open(abs)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	buf := make([]byte, 64<<10)
	for {
		r, err := f.Read(buf)
		n += bytes.Count(buf[:r], []byte("\n"))
		if err == io.EOF || err != nil {
			return n, nil
		}
	}
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

func videoFallback(err error) ToolResult {
	return ToolResult{Content: []ToolContent{textContent(fmt.Sprintf("video preview unavailable: %v; run artifactory probe or fetch raw via GET /artifacts/<rel>", err))}}
}
func audioFallback(err error) ToolResult {
	return ToolResult{Content: []ToolContent{textContent(fmt.Sprintf("audio preview unavailable: %v; run artifactory probe or fetch raw via GET /artifacts/<rel>", err))}}
}

// run executes cmd and returns its error (stderr folded in) when no stderr was
// pre-attached.
func run(cmd *exec.Cmd) error {
	if cmd.Stderr == nil {
		var b bytes.Buffer
		cmd.Stderr = &b
		err := cmd.Run()
		if err != nil {
			return fmt.Errorf("%w: %s", err, b.String())
		}
		return nil
	}
	return cmd.Run()
}

// probeVideoDims returns the video width/height from ffprobe, or ok=false.
// ffprobeMedia may return nil or lack width/height entirely (e.g. an audio
// file) — type-asserting a missing map value to string panics, so the lookups
// go through metaString (audit M6).
func probeVideoDims(abs string) (int, int, bool) {
	m := ffprobeMedia(abs)
	w, err := strconv.Atoi(metaString(m["width"]))
	if err != nil {
		return 0, 0, false
	}
	h, err := strconv.Atoi(metaString(m["height"]))
	if err != nil {
		return 0, 0, false
	}
	if w > 0 && h > 0 {
		return w, h, true
	}
	return 0, 0, false
}

// metaString safely reads a string out of a ffprobe metadata map.
func metaString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// fit scales (w,h) down so its longest side equals maxDim, preserving aspect.
func fit(w, h, maxDim int) (int, int) {
	if w <= maxDim && h <= maxDim {
		return w, h
	}
	scale := float64(maxDim) / float64(max(w, h))
	return int(0.5 + float64(w)*scale), int(0.5 + float64(h)*scale)
}

func asString(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}
func optString(m map[string]any, k, def string) string {
	if v, ok := m[k].(string); ok && v != "" {
		return v
	}
	return def
}
func optInt(m map[string]any, k string, def int) int {
	if v, ok := m[k].(float64); ok {
		return int(v)
	}
	return def
}
