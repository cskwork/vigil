// Package attach stores the screenshots and screen recordings a QA requester
// uploads with a request, and turns every video into the still frames a vision
// model can actually read. The agent never receives a container file: it
// receives image paths it can open, plus the note that says where they came
// from.
package attach

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Kinds of uploaded evidence.
const (
	KindImage = "image"
	KindVideo = "video"
)

// Limits bound one request's uploads. They are deliberately small: this is
// reproduction evidence, not a media library.
const (
	MaxFiles      = 8
	MaxImageBytes = 12 << 20 // 12 MB per screenshot
	MaxVideoBytes = 64 << 20 // 64 MB per recording
	MaxTotalBytes = 128 << 20
	// MaxFrames is how many stills one recording contributes. Eight evenly
	// spaced frames describe a short reproduction without flooding the context.
	MaxFrames = 8
)

// File is one uploaded file held in memory before it is stored.
type File struct {
	Name string
	Data []byte
}

// Attachment is one stored piece of request evidence. Frames is set for a
// video: the agent opens those images, never the container.
type Attachment struct {
	Name   string   `json:"name" yaml:"name"`
	Kind   string   `json:"kind" yaml:"kind"`
	Path   string   `json:"path" yaml:"path"`
	Frames []string `json:"frames,omitempty" yaml:"frames,omitempty"`
	Note   string   `json:"note,omitempty" yaml:"note,omitempty"`
}

// Readable lists the image paths the agent should open for this attachment.
func (a Attachment) Readable() []string {
	if a.Kind == KindVideo {
		return a.Frames
	}
	if a.Path == "" {
		return nil
	}
	return []string{a.Path}
}

// ErrUnsupported marks a file vigil will not accept, so the HTTP layer can
// answer 415 instead of 500.
var ErrUnsupported = errors.New("unsupported attachment")

var unsafeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// diskName is the sanitised on-disk file name; Attachment.Name keeps the original.
func diskName(name string, i int) string {
	base := unsafeName.ReplaceAllString(filepath.Base(name), "_")
	base = strings.Trim(base, "._-")
	if len(base) > 64 {
		base = base[len(base)-64:]
	}
	if base == "" {
		base = "upload"
	}
	return fmt.Sprintf("%02d-%s", i+1, base)
}

// Detect classifies a file by content, not by the name the browser sent.
func Detect(data []byte, name string) (string, error) {
	switch {
	case len(data) >= 8 && bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return KindImage, nil
	case len(data) >= 3 && bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return KindImage, nil
	case len(data) >= 6 && (bytes.HasPrefix(data, []byte("GIF87a")) || bytes.HasPrefix(data, []byte("GIF89a"))):
		return KindImage, nil
	case len(data) >= 12 && bytes.Equal(data[0:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return KindImage, nil
	case len(data) >= 12 && bytes.Equal(data[4:8], []byte("ftyp")):
		return KindVideo, nil // mp4 / m4v / mov
	case len(data) >= 4 && bytes.Equal(data[0:4], []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return KindVideo, nil // webm / mkv
	}
	return "", fmt.Errorf("%w: %s는 PNG, JPEG, GIF, WebP 이미지이거나 MP4, MOV, WebM 영상이어야 합니다", ErrUnsupported, name)
}

// Store writes files under dir and returns what the agent can open. Videos are
// expanded into frames when ffmpeg is available; without ffmpeg the recording
// is kept and reported as unreadable rather than silently dropped.
func Store(ctx context.Context, dir string, files []File) ([]Attachment, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if len(files) > MaxFiles {
		return nil, fmt.Errorf("%w: 첨부는 %d개까지 올릴 수 있습니다", ErrUnsupported, MaxFiles)
	}
	total := 0
	for _, f := range files {
		total += len(f.Data)
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("%w: 첨부 전체 용량은 %d MB를 넘을 수 없습니다", ErrUnsupported, MaxTotalBytes>>20)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	out := make([]Attachment, 0, len(files))
	for i, f := range files {
		kind, err := Detect(f.Data, f.Name)
		if err != nil {
			return nil, err
		}
		limit := MaxImageBytes
		if kind == KindVideo {
			limit = MaxVideoBytes
		}
		if len(f.Data) > limit {
			return nil, fmt.Errorf("%w: %s가 너무 큽니다 (최대 %d MB)", ErrUnsupported, f.Name, limit>>20)
		}
		path := filepath.Join(dir, diskName(f.Name, i))
		if err := os.WriteFile(path, f.Data, 0o644); err != nil {
			return nil, err
		}
		a := Attachment{Name: f.Name, Kind: kind, Path: path}
		if kind == KindVideo {
			frames, note, err := ExtractFrames(ctx, path, filepath.Join(dir, fmt.Sprintf("frames-%02d", i+1)))
			if err != nil {
				return nil, err
			}
			a.Frames, a.Note = frames, note
		}
		out = append(out, a)
	}
	return out, nil
}

// ExtractFrames turns a recording into evenly spaced JPEG stills. It returns a
// human-readable note (frame count and interval, or why no frame exists).
func ExtractFrames(ctx context.Context, video, dir string) ([]string, string, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, "ffmpeg가 설치되어 있지 않아 영상에서 화면을 추출하지 못했습니다. 화면 캡처 이미지를 함께 올려 주세요.", nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, "", err
	}
	seconds := Duration(ctx, video)
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-i", video}
	if seconds > 0 {
		// Evenly spaced stills: MaxFrames over the whole recording.
		args = append(args, "-vf", fmt.Sprintf("fps=%.6f,scale=1280:-2", float64(MaxFrames)/seconds))
	} else {
		args = append(args, "-vf", "fps=1,scale=1280:-2")
	}
	args = append(args, "-frames:v", strconv.Itoa(MaxFrames), "-q:v", "4", filepath.Join(dir, "frame-%02d.jpg"))
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(cctx, ffmpeg, args...)
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return nil, "", fmt.Errorf("%w: 영상에서 화면을 추출하지 못했습니다: %s", ErrUnsupported, firstLine(errBuf.String()))
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "frame-*.jpg"))
	if len(matches) == 0 {
		return nil, "", fmt.Errorf("%w: 영상에서 화면을 한 장도 추출하지 못했습니다", ErrUnsupported)
	}
	note := fmt.Sprintf("녹화 영상에서 화면 %d장을 시간 순서대로 추출했습니다", len(matches))
	if seconds > 0 {
		note = fmt.Sprintf("%.1f초 길이의 녹화 영상에서 화면 %d장을 약 %.1f초 간격으로 추출했습니다", seconds, len(matches), seconds/float64(len(matches)))
	}
	return matches, note, nil
}

// Duration reads a media file's length in seconds; 0 when ffprobe cannot tell.
func Duration(ctx context.Context, path string) float64 {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		return 0
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, ffprobe, "-v", "error", "-show_entries", "format=duration", "-of", "json", path).Output()
	if err != nil {
		return 0
	}
	var probe struct {
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if json.Unmarshal(out, &probe) != nil {
		return 0
	}
	d, err := strconv.ParseFloat(strings.TrimSpace(probe.Format.Duration), 64)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
