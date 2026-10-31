package media

import (
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildCues(t *testing.T) {
	st := DefaultCaptionStyle()
	st.WordsPerChunk = 3
	cues := BuildCues("one two three four five", 10, st)
	if len(cues) != 5 { // highlight on → one cue per word
		t.Fatalf("cues=%d", len(cues))
	}
	if cues[0].Start < 0.2 || cues[len(cues)-1].End != 10 {
		t.Fatalf("timing %+v", cues)
	}
	for i := 1; i < len(cues); i++ {
		if cues[i].Start < cues[i-1].Start || cues[i-1].End > cues[i].Start+1e-9 {
			t.Fatalf("overlap at %d: %+v", i, cues)
		}
	}
	if len(cues[3].Words) != 2 || cues[3].Active != 0 {
		t.Fatalf("second chunk %+v", cues[3])
	}
	st.HighlightColor = ""
	if got := BuildCues("one two three four five", 10, st); len(got) != 2 {
		t.Fatalf("chunked cues=%d", len(got))
	}
	if BuildCues("", 5, st) != nil {
		t.Fatal("empty dialogue should have no cues")
	}
}

func TestRenderImages(t *testing.T) {
	dir := t.TempDir()
	st := DefaultCaptionStyle()
	h, err := RenderCue(filepath.Join(dir, "c.png"), Cue{Words: []string{"no", "cap", "🔥", "fr"}, Active: 1}, 1080, st)
	if err != nil || h <= 0 {
		t.Fatal(h, err)
	}
	f, _ := os.Open(filepath.Join(dir, "c.png"))
	img, err := png.Decode(f)
	f.Close()
	if err != nil || img.Bounds().Dx() != 1080 || img.Bounds().Dy() != h {
		t.Fatalf("bad png: %v %v", img.Bounds(), err)
	}
	st.TopBoxColor = NoBox
	if _, err := RenderTopText(filepath.Join(dir, "t.png"), "#1 connection pooling", 1080, st); err != nil {
		t.Fatal(err)
	}
	st.TopBoxColor = "#00000066"
	if _, err := RenderTopText(filepath.Join(dir, "t2.png"), "soft launch", 1080, st); err != nil {
		t.Fatal(err)
	}
	if err := RenderDocCard(filepath.Join(dir, "bg.png"), "Retries", "Clients MUST retry 429 and 503 responses with exponential backoff and full jitter.", 1080, 1920, "#00E5FF"); err != nil {
		t.Fatal(err)
	}
}

func TestParseHexPremultiplied(t *testing.T) {
	c := parseHex("#FF000080", color.RGBA{})
	if c.A != 0x80 || c.R != 0x80 {
		t.Fatalf("%+v", c)
	}
}
