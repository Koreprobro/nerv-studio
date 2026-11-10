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
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// fetchAudio cuts a preview clip (mp3) within max_duration; an over-budget
// mp3 (~linear size in duration) is re-cut proportionally shorter, once.
func (d *Domain) fetchAudio(abs string, b Budget, seek string) (*Media, error) {
	out, err := runFFmpeg(abs, b, seek)
	if err != nil {
		return &Media{Note: "audio preview unavailable: " + err.Error() + "; raw via the artifact URL"}, nil
	}
	if len(out) > b.MaxBlockBytes {
		shorter := max(1, int(int64(b.MaxDuration)*int64(b.MaxBlockBytes)/int64(len(out))))
		if shorter < b.MaxDuration {
			b.MaxDuration = shorter
			if out2, err2 := runFFmpeg(abs, b, seek); err2 == nil && len(out2) > 0 {
				return &Media{Data: out2, Mime: "audio/mpeg", Degraded: true}, nil
			}
		}
		return &Media{Data: out, Mime: "audio/mpeg", Degraded: true}, nil
	}
	return &Media{Data: out, Mime: "audio/mpeg"}, nil
}

func runFFmpeg(abs string, b Budget, seek string) ([]byte, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("nerv-audio-%d.mp3", os.Getpid()))
	defer os.Remove(tmp)
	args := []string{"-y", "-loglevel", "error", "-ss", seek, "-i", abs,
		"-t", strconv.Itoa(b.MaxDuration),
		"-ar", strconv.Itoa(b.Rate), "-ac", strconv.Itoa(b.Channels),
		"-b:a", b.Bitrate, "-f", "mp3", tmp}
	if err := runCmd(exec.Command("ffmpeg", args...)); err != nil {
		return nil, err
	}
	return os.ReadFile(tmp)
}

// fetchVideo collapses a video to one thumbnail frame at seek.
func (d *Domain) fetchVideo(abs string, b Budget, seek string) (*Media, error) {
	tmp := filepath.Join(os.TempDir(), fmt.Sprintf("nerv-vid-%d.jpg", os.Getpid()))
	defer os.Remove(tmp)
	vf := fmt.Sprintf("scale=%d:%d", b.VideoMaxDim, b.VideoMaxDim)
	if w, h, ok := probeVideoDims(abs); ok {
		sw, sh := fit(w, h, b.VideoMaxDim)
		vf = fmt.Sprintf("scale=%d:%d", sw, sh)
	}
	args := []string{"-y", "-loglevel", "error", "-ss", seek, "-i", abs,
		"-frames:v", "1", "-vf", vf,
		"-codec:v", "mjpeg", "-pix_fmt", "yuv420p", "-q:v", "8", tmp}
	if err := runCmd(exec.Command("ffmpeg", args...)); err != nil {
		return &Media{Note: "video preview unavailable: " + err.Error() + "; raw via the artifact URL"}, nil
	}
	data, err := os.ReadFile(tmp)
	if err != nil {
		return &Media{Note: "video preview unavailable: " + err.Error()}, nil
	}
	if len(data) == 0 {
		return &Media{Note: "empty thumbnail (seek past end?)"}, nil
	}
	if len(data) > b.MaxBlockBytes {
		if img, _, derr := image.Decode(bytes.NewReader(data)); derr == nil {
			box := img.Bounds()
			if fitOut, mime, _ := encodeFitting(img, box.Dx(), box.Dy(), b.Quality, "jpeg", b.MaxBlockBytes); fitOut != nil {
				return &Media{Data: fitOut, Mime: mime, Degraded: true}, nil
			}
		}
	}
	return &Media{Data: data, Mime: "image/jpeg"}, nil
}

// ffprobeMedia returns a flat map of relevant stream/format fields;
// it degrades to nil when ffprobe is missing or fails.
func ffprobeMedia(abs string) map[string]string {
	out, err := exec.Command("ffprobe", "-v", "error",
		"-show_format", "-show_streams",
		"-of", "default=noprint_wrappers=1", abs).CombinedOutput()
	if err != nil {
		return nil
	}
	m := map[string]string{}
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
	if len(m) == 0 {
		return nil
	}
	return m
}

func probeVideoDims(abs string) (int, int, bool) {
	m := ffprobeMedia(abs)
	if m == nil {
		return 0, 0, false
	}
	w, err1 := strconv.Atoi(m["width"])
	h, err2 := strconv.Atoi(m["height"])
	if err1 == nil && err2 == nil && w > 0 && h > 0 {
		return w, h, true
	}
	return 0, 0, false
}

func imageDims(abs string) (int, int, bool) {
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

func isAudioExt(abs string) bool {
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".mp3", ".wav", ".ogg", ".flac", ".m4a", ".aac", ".opus":
		return true
	}
	return false
}

func isVideoExt(abs string) bool {
	switch strings.ToLower(filepath.Ext(abs)) {
	case ".mp4", ".mov", ".mkv", ".webm", ".avi", ".mpeg", ".mpg":
		return true
	}
	return false
}

func runCmd(cmd *exec.Cmd) error {
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
