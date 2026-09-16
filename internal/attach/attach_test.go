package attach

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func pngBytes(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	img.Set(1, 1, color.White)
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestDetectClassifiesByContent(t *testing.T) {
	if kind, err := Detect(pngBytes(t), "shot.png"); err != nil || kind != KindImage {
		t.Fatalf("png = %q, %v", kind, err)
	}
	mp4 := append(make([]byte, 4), []byte("ftypisom")...)
	if kind, err := Detect(mp4, "clip.mp4"); err != nil || kind != KindVideo {
		t.Fatalf("mp4 = %q, %v", kind, err)
	}
	// A name alone must not make a file an image.
	if _, err := Detect([]byte("not an image at all"), "evil.png"); err == nil {
		t.Fatal("text accepted as image")
	}
}

func TestStoreWritesImagesAndKeepsOriginalName(t *testing.T) {
	dir := t.TempDir()
	out, err := Store(context.Background(), dir, []File{{Name: "결제 화면.png", Data: pngBytes(t)}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Kind != KindImage || out[0].Name != "결제 화면.png" {
		t.Fatalf("stored = %+v", out)
	}
	if _, err := os.Stat(out[0].Path); err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if base := filepath.Base(out[0].Path); strings.ContainsAny(base, " /") {
		t.Fatalf("disk name not sanitised: %q", base)
	}
	if got := out[0].Readable(); len(got) != 1 || got[0] != out[0].Path {
		t.Fatalf("readable = %v", got)
	}
}

func TestStoreRejectsUnsupportedAndOversized(t *testing.T) {
	if _, err := Store(context.Background(), t.TempDir(), []File{{Name: "notes.txt", Data: []byte("hello")}}); err == nil {
		t.Fatal("text file accepted")
	}
	big := make([]File, MaxFiles+1)
	for i := range big {
		big[i] = File{Name: "s.png", Data: pngBytes(t)}
	}
	if _, err := Store(context.Background(), t.TempDir(), big); err == nil {
		t.Fatal("too many files accepted")
	}
}

func TestExtractFramesSplitsRecording(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	video := filepath.Join(dir, "clip.mp4")
	cmd := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=320x240:rate=10", "-t", "4", "-pix_fmt", "yuv420p", video)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg could not build a fixture: %v: %s", err, out)
	}
	data, err := os.ReadFile(video)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := Store(context.Background(), filepath.Join(dir, "store"), []File{{Name: "clip.mp4", Data: data}})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 1 || stored[0].Kind != KindVideo {
		t.Fatalf("stored = %+v", stored)
	}
	frames := stored[0].Readable()
	if len(frames) < 2 || len(frames) > MaxFrames {
		t.Fatalf("frames = %d (%v)", len(frames), frames)
	}
	for _, f := range frames {
		st, err := os.Stat(f)
		if err != nil || st.Size() == 0 {
			t.Fatalf("frame %s: %v", f, err)
		}
	}
	if stored[0].Note == "" {
		t.Fatal("video note is empty")
	}
}
