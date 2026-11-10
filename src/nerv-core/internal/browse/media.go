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
	_ "image/gif"
	"image/jpeg"
	_ "image/jpeg"
	"image/png"
	_ "image/png"
	"os"
	"strings"
)

// Media is fetch output: rendered bytes in budget + mime, or a Note
// instead of data (fallback text: preview unavailable / raw too large).
type Media struct {
	Data     []byte `json:"-"`
	Mime     string `json:"mime,omitempty"`
	Note     string `json:"note,omitempty"`
	Degraded bool   `json:"degraded,omitempty"` // budget forced quality/size loss
}

// FetchOpts override the domain budget per call (zero = budget default).
type FetchOpts struct {
	Format        string
	MaxDim        int
	Quality       int
	MaxBlockBytes int
	MaxDuration   int
	Seek          string // "12" or "00:00:12"
	Rate          int
	Channels      int
	Bitrate       string
}

// Fetch renders media into the block budget (command `fetch`): images via
// the stdlib decoder + degrading re-encode, audio/video via ffmpeg
// (thumbnail/clip), anything else raw passthrough when it fits.
func (d *Domain) Fetch(path string, o FetchOpts) (*Media, error) {
	abs, err := d.Resolve(path)
	if err != nil {
		return nil, err
	}
	b := d.effective()
	if o.Format != "" {
		b.Format = strings.ToLower(o.Format)
	}
	if o.MaxDim > 0 {
		b.MaxDim = o.MaxDim
	}
	if o.Quality > 0 {
		b.Quality = o.Quality
	}
	if o.MaxBlockBytes > 0 {
		b.MaxBlockBytes = o.MaxBlockBytes
	}
	if o.MaxDuration > 0 {
		b.MaxDuration = o.MaxDuration
	}
	seek := o.Seek
	if seek == "" {
		seek = "0"
	}
	if o.Rate > 0 {
		b.Rate = o.Rate
	}
	if o.Channels > 0 {
		b.Channels = o.Channels
	}
	if o.Bitrate != "" {
		b.Bitrate = o.Bitrate
	}
	mime := sniffMime(abs)
	switch {
	case strings.HasPrefix(mime, "image/"):
		return d.fetchImage(abs, mime, b)
	case strings.HasPrefix(mime, "audio/"), isAudioExt(abs):
		return d.fetchAudio(abs, b, seek)
	case strings.HasPrefix(mime, "video/"), isVideoExt(abs):
		return d.fetchVideo(abs, b, seek)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	if len(data) > b.MaxBlockBytes {
		return &Media{Note: fmt.Sprintf(
			"raw media too large to inline: %d bytes (budget %d, %s) — fetch via the artifact URL", len(data), b.MaxBlockBytes, mime)}, nil
	}
	return &Media{Data: data, Mime: mime}, nil
}

func (d *Domain) fetchImage(abs, mime string, b Budget) (*Media, error) {
	raw, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	img, _, derr := image.Decode(bytes.NewReader(raw))
	if derr != nil {
		// Undecodable format: raw passthrough within budget.
		if len(raw) > b.MaxBlockBytes {
			return &Media{Note: fmt.Sprintf("image too large to inline: %d bytes (budget %d, %s)", len(raw), b.MaxBlockBytes, mime)}, nil
		}
		return &Media{Data: raw, Mime: mime}, nil
	}
	box := img.Bounds()
	w, h := fit(box.Dx(), box.Dy(), b.MaxDim)
	out, outMime, ok := encodeFitting(img, w, h, b.Quality, b.Format, b.MaxBlockBytes)
	if out == nil {
		return nil, fmt.Errorf("encode image: %w", derr)
	}
	return &Media{Data: out, Mime: outMime, Degraded: !ok}, nil
}

// encodeFitting scales and encodes, degrading into budget: quality steps
// down first, then the scale shrinks. The last attempt is returned even
// over budget (a degraded preview beats an error), ok=false.
func encodeFitting(img image.Image, w, h, qual int, format string, budget int) ([]byte, string, bool) {
	q, sc := qual, 1.0
	var last []byte
	var lastMime string
	for attempt := 0; attempt < 8; attempt++ {
		nw, nh := max(1, int(float64(w)*sc)), max(1, int(float64(h)*sc))
		out, outMime, err := encodeImage(img, nw, nh, q, format)
		if err != nil {
			return last, lastMime, false
		}
		last, lastMime = out, outMime
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
	return last, lastMime, last != nil && len(last) <= budget
}

func encodeImage(img image.Image, w, h, qual int, format string) ([]byte, string, error) {
	dst := scaleNearest(img, w, h)
	var buf bytes.Buffer
	var err error
	switch format {
	case "png":
		err = png.Encode(&buf, dst)
		return buf.Bytes(), "image/png", err
	default:
		err = jpeg.Encode(&buf, dst, &jpeg.Options{Quality: qual})
		return buf.Bytes(), "image/jpeg", err
	}
}

// scaleNearest downscales (or passes through) without external deps:
// previews do not deserve resampling libraries.
func scaleNearest(img image.Image, w, h int) image.Image {
	box := img.Bounds()
	if w == box.Dx() && h == box.Dy() {
		return img
	}
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		sy := box.Min.Y + y*box.Dy()/h
		for x := 0; x < w; x++ {
			sx := box.Min.X + x*box.Dx()/w
			dst.Set(x, y, img.At(sx, sy))
		}
	}
	return dst
}

// fit scales (w,h) down so the longest side is at most maxDim, keeping aspect.
func fit(w, h, maxDim int) (int, int) {
	if w <= maxDim && h <= maxDim {
		return w, h
	}
	scale := float64(maxDim) / float64(max(w, h))
	return max(1, int(0.5+float64(w)*scale)), max(1, int(0.5+float64(h)*scale))
}
