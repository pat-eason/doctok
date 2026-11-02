package media

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"
)

func actorTools(t *testing.T) Tools {
	t.Helper()
	tools, err := FindTools()
	if err != nil {
		t.Skip("ffmpeg/ffprobe not installed")
	}
	return tools
}

func testPhoto(t *testing.T, tools Tools, size string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "photo.jpg")
	if out, err := exec.Command(tools.FFmpeg, "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size="+size+":rate=1", "-frames:v", "1", p).CombinedOutput(); err != nil {
		t.Fatalf("make photo: %v %s", err, out)
	}
	return p
}

func TestActorCrop(t *testing.T) {
	for _, c := range []struct {
		w, h, cw, ch int
		ok           bool
	}{
		{1600, 900, 506, 900, true},   // landscape: trim the sides
		{1000, 1000, 562, 1000, true}, // square
		{1080, 1920, 1080, 1920, true},
		{900, 1600, 900, 1600, true},
		{1000, 3000, 1000, 1777, true}, // taller than 9:16: trim the bottom
		{300, 400, 225, 400, false},    // too narrow once cropped
	} {
		cw, ch, err := ActorCrop(c.w, c.h)
		if (err == nil) != c.ok || (c.ok && (cw != c.cw || ch != c.ch)) {
			t.Errorf("%dx%d → %dx%d, %v (want %dx%d ok=%v)", c.w, c.h, cw, ch, err, c.cw, c.ch, c.ok)
		}
	}
}

func TestPrepareActorMakes916(t *testing.T) {
	tools := actorTools(t)
	for _, size := range []string{"1600x900", "800x800", "900x1600"} {
		out := filepath.Join(t.TempDir(), "hero.jpg")
		if err := tools.PrepareActor(context.Background(), testPhoto(t, tools, size), out); err != nil {
			t.Fatalf("%s: %v", size, err)
		}
		w, h, err := tools.ImageSize(context.Background(), out)
		if err != nil || w != 1080 || h != 1920 {
			t.Errorf("%s → %dx%d, %v; want 1080x1920", size, w, h, err)
		}
	}
	if err := tools.PrepareActor(context.Background(), testPhoto(t, tools, "320x320"), filepath.Join(t.TempDir(), "x.jpg")); err == nil {
		t.Error("a photo too small for Kling must be rejected")
	}
	if _, _, err := tools.ImageSize(context.Background(), filepath.Join(t.TempDir(), "missing.jpg")); err == nil {
		t.Error("missing file must error")
	}
}
