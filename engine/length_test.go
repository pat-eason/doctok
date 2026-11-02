package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pat-eason/doctok/engine/kling"
)

func TestTargetWindow(t *testing.T) {
	for _, c := range []struct{ target, min, max, lo, hi int }{
		{30, 30, 300, 30, 35}, // 85% of 30 is below the 30s minimum, so it clamps
		{45, 30, 300, 38, 52},
		{60, 30, 300, 51, 69},
		{90, 30, 300, 77, 104},
		{120, 30, 300, 102, 138},
		{210, 30, 300, 179, 242},
		{300, 30, 300, 255, 300}, // clamped to the configured max
		{30, 20, 40, 26, 35},
		{35, 34, 36, 34, 36}, // tight config: target still inside the window
	} {
		lo, hi := TargetWindow(c.target, c.min, c.max)
		if lo != c.lo || hi != c.hi || lo > c.target || hi < c.target {
			t.Errorf("TargetWindow(%d,%d,%d) = %d..%d, want %d..%d", c.target, c.min, c.max, lo, hi, c.lo, c.hi)
		}
	}
}

func TestLengthLabel(t *testing.T) {
	for in, want := range map[int]string{30: "30s", 45: "45s", 60: "1 min", 90: "1 min 30s", 120: "2 min", 210: "3 min 30s"} {
		if got := LengthLabel(in); got != want {
			t.Errorf("%d → %q, want %q", in, got, want)
		}
	}
}

// The storyboard must be told what a video of this length can carry.
func TestStoryboardPromptScalesWithLength(t *testing.T) {
	st, _ := LookupStyle("bestie-explainer")
	spec := kling.Models["kling-3.0"]
	build := func(target int) string {
		lo, hi := TargetWindow(target, MinVideoSeconds, MaxVideoSeconds)
		return StoryboardPrompt("notes", st, spec, true, target, lo, hi)
	}
	for _, c := range []struct {
		target     int
		must, none []string
	}{
		{30, []string{"between 30 and 35 seconds", "SHORT video (~30s)", "ONE core idea", "No chapters"}, []string{"3–5 minute", "every ~25–40 s", "mini-chapters, each opening"}},
		{60, []string{"between 51 and 69 seconds", "~1 min video", "ONE chapter"}, []string{"3–5 minute", "every ~25–40 s"}},
		{90, []string{"between 77 and 104 seconds", "~1 min 30s video", "TWO mini-chapters"}, []string{"3–5 minute", "No chapters"}},
		{120, []string{"~2 min video", "TWO mini-chapters"}, []string{"3–5 minute"}},
		{210, []string{"between 179 and 242 seconds", "3–5 minute video keeps retention", "every ~25–40 s"}, []string{"SHORT video", "ONE core idea"}},
	} {
		p := build(c.target)
		for _, m := range c.must {
			if !strings.Contains(p, m) {
				t.Errorf("%ds prompt missing %q", c.target, m)
			}
		}
		for _, n := range c.none {
			if strings.Contains(p, n) {
				t.Errorf("%ds prompt must not contain %q", c.target, n)
			}
		}
	}
}

func TestDistillPromptForScalesWithLength(t *testing.T) {
	if p := DistillPromptFor("doc", 30); !strings.Contains(p, "short ~30s video") || strings.Contains(p, "3–5 minute") {
		t.Errorf("30s distill prompt:\n%.300s", p)
	}
	if p := DistillPromptFor("doc", 210); !strings.Contains(p, "3–5 minute video") {
		t.Errorf("long distill prompt:\n%.300s", p)
	}
	if DistillPrompt("doc") != DistillPromptFor("doc", 210) {
		t.Error("DistillPrompt must stay the long-form default")
	}
}

func TestDefaultModelIsKling30(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Kling.Model != "kling-3.0" {
		t.Fatalf("default model = %q, want kling-3.0", cfg.Kling.Model)
	}
	spec, ok := kling.Lookup(cfg.Kling.Model)
	if !ok || spec.MCPName != "kling-video-v3_0" {
		t.Fatalf("kling-3.0 must exist with an MCP model: %+v", spec)
	}
	if err := spec.Validate(cfg.Kling.Resolution, cfg.Kling.Audio); err != nil {
		t.Fatalf("defaults must be a valid combination: %v", err)
	}
	if cfg.Validate() != nil {
		t.Fatal(cfg.Validate())
	}
}

func TestLegacyLengthLimitsAreMigrated(t *testing.T) {
	home := t.TempDir()
	// What older versions saved: the old 180–300s defaults, pinned in the file.
	old := `{"claude":{"mode":"auto"},"kling":{"model":"kling-3.0","resolution":"720p"},"video":{"style":"bestie-explainer","target_seconds":210,"min_seconds":180,"max_seconds":300,"captions":true,"width":1080,"height":1920,"fps":30}}`
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Video.MinSeconds != MinVideoSeconds || cfg.Video.MaxSeconds != 300 {
		t.Fatalf("legacy limits not migrated: %d–%d", cfg.Video.MinSeconds, cfg.Video.MaxSeconds)
	}
	// A deliberately customized range is respected.
	custom := strings.Replace(old, `"min_seconds":180`, `"min_seconds":120`, 1)
	_ = os.WriteFile(filepath.Join(home, "config.json"), []byte(custom), 0o600)
	if cfg, _ = LoadConfig(home); cfg.Video.MinSeconds != 120 {
		t.Fatalf("custom minimum overwritten: %d", cfg.Video.MinSeconds)
	}
}

func TestCreateAcceptsShortLengths(t *testing.T) {
	doc := writeDoc(t)
	e := New(t.TempDir(), DefaultConfig())
	for _, n := range []int{30, 45, 60, 90, 120, 210, 300} {
		g, err := e.Create(context.Background(), GenerateRequest{Source: doc, Name: "len", TargetSeconds: n, DryRun: true})
		if err != nil {
			t.Errorf("%ds rejected: %v", n, err)
			continue
		}
		if g.Options.TargetSeconds != n {
			t.Errorf("%ds stored as %d", n, g.Options.TargetSeconds)
		}
		if est := e.PreEstimate(g); est.Seconds != n {
			t.Errorf("%ds estimate seconds = %d", n, est.Seconds)
		}
	}
	for _, n := range []int{29, 301} {
		if _, err := e.Create(context.Background(), GenerateRequest{Source: doc, Name: "bad", TargetSeconds: n, DryRun: true}); err == nil || !strings.Contains(err.Error(), "must be between") {
			t.Errorf("%ds should be rejected with the allowed range, got %v", n, err)
		}
	}
}

func TestLengthOptionsCarryCostProjections(t *testing.T) {
	byLen := func(opts []LengthOption) map[int]LengthOption {
		m := map[int]LengthOption{}
		for _, o := range opts {
			m[o.Seconds] = o
		}
		return m
	}

	// API backend: Kling's published price table (kling-3.0, 1080p, audio = 1.2 units/s).
	api := byLen(New(t.TempDir(), DefaultConfig()).LengthOptions())
	for _, sec := range []int{30, 45, 60, 90, 120, 180, 210, 240, 300} {
		if _, ok := api[sec]; !ok {
			t.Errorf("missing the %ds option", sec)
		}
	}
	if o := api[30]; o.Units != 36 || o.USD < 5.03 || o.USD > 5.05 || o.Credits != 0 || o.Label != "0:30 — snack" {
		t.Errorf("30s API option: %+v", o)
	}
	if o := api[120]; o.Units != 144 || o.Label != "2:00 — two minutes" {
		t.Errorf("2:00 API option: %+v", o)
	}

	// MCP backend: credits only exist once a rate is configured. Never a fake $0.
	cfg := DefaultConfig()
	cfg.Kling.Backend = KlingBackendMCP
	none := byLen(New(t.TempDir(), cfg).LengthOptions())
	if o := none[60]; o.Credits != 0 || o.Units != 0 || o.USD != 0 {
		t.Errorf("no rate configured → no estimate, got %+v", o)
	}
	cfg.Kling.CreditsPerSecond = 10
	rated := byLen(New(t.TempDir(), cfg).LengthOptions())
	if rated[30].Credits != 300 || rated[90].Credits != 900 || rated[120].Credits != 1200 || rated[60].Units != 0 || rated[60].USD != 0 {
		t.Errorf("credits = seconds × rate: %+v %+v %+v", rated[30], rated[90], rated[120])
	}

	// Options respect the configured limits.
	cfg.Video.MinSeconds, cfg.Video.MaxSeconds = 120, 210
	limited := New(t.TempDir(), cfg).LengthOptions()
	if len(limited) != 3 || limited[0].Seconds != 120 || limited[len(limited)-1].Seconds != 210 {
		t.Errorf("limited options: %+v", limited)
	}
}
