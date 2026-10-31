package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pat-eason/doctok/engine/kling"
)

func TestNormalizeRuntime(t *testing.T) {
	spec := kling.Models["kling-2.6"]
	st, _ := LookupStyle(DefaultStyleID)
	mk := func(n, secs int, words int) *Storyboard {
		sb := &Storyboard{Characters: []Character{{Label: "A", Description: "d"}}}
		for i := 0; i < n; i++ {
			sb.Scenes = append(sb.Scenes, Scene{Beat: "b", Seconds: secs, Action: "x", Cut: "hard-cut",
				Lines: []Line{{Speaker: "A", Text: strings.Repeat("word ", words)}}})
		}
		return sb
	}
	// 20 × 5s = 100s → must be lengthened to ≥180 by bumping scenes to 10s.
	sb := mk(20, 5, 12)
	if _, err := sb.Normalize(spec, st, 180, 300); err != nil {
		t.Fatal(err)
	}
	if got := sb.TotalSeconds(); got < 180 || got > 300 {
		t.Fatalf("total %d", got)
	}
	// 40 × 10s = 400s → shortened/dropped to ≤300.
	sb = mk(40, 10, 5)
	if _, err := sb.Normalize(spec, st, 180, 300); err != nil {
		t.Fatal(err)
	}
	if got := sb.TotalSeconds(); got > 300 || got < 180 {
		t.Fatalf("total %d", got)
	}
	// Impossible: 5 scenes even at 10s = 50s.
	sb = mk(5, 10, 5)
	if _, err := sb.Normalize(spec, st, 180, 300); err == nil {
		t.Fatal("expected error for too-short storyboard")
	}
	// Odd durations snap to the allowed set.
	sb = mk(20, 7, 5)
	sb.Scenes[0].Seconds = 3
	_, _ = sb.Normalize(spec, st, 100, 300)
	for _, s := range sb.Scenes {
		if s.Seconds != 5 && s.Seconds != 10 {
			t.Fatalf("unsnapped duration %d", s.Seconds)
		}
	}
}

func TestStoryboardSchemaUsesModelDurations(t *testing.T) {
	var schema map[string]any
	_ = json.Unmarshal(StoryboardSchema(kling.Models["kling-2.6"]), &schema)
	secs := schema["properties"].(map[string]any)["scenes"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)["seconds"].(map[string]any)
	if enum := secs["enum"].([]any); len(enum) != 2 || enum[0].(float64) != 5 || enum[1].(float64) != 10 {
		t.Fatalf("enum %v", secs["enum"])
	}
}

func TestKlingPromptAudioOff(t *testing.T) {
	sb := testStoryboard()
	st, _ := LookupStyle("cozymaxxing-study")
	p := KlingPrompt(&sb, sb.Scenes[0], st, kling.Models["kling-2.5-turbo"], true)
	if strings.Contains(p, "says:") || !strings.Contains(p, "talks expressively") {
		t.Fatalf("silent model should not get dialogue: %s", p)
	}
	if !strings.Contains(p, "[Bestie]: early-20s") || !strings.Contains(p, st.PromptSuffix) {
		t.Fatalf("prompt missing character/style: %s", p)
	}
}

func TestStylesCatalog(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range StyleCatalog {
		if seen[s.ID] {
			t.Fatalf("duplicate style %s", s.ID)
		}
		seen[s.ID] = true
		total := 0
		for _, b := range s.Structure {
			total += b.Percent
		}
		if total < 95 || total > 105 {
			t.Errorf("%s: arc sums to %d%%", s.ID, total)
		}
		if s.PromptSuffix == "" || s.Persona == "" || !strings.Contains(s.PromptSuffix, "9:16") || !strings.Contains(s.PromptSuffix, "no background music") && s.ID != "cozymaxxing-study" {
			t.Errorf("%s: incomplete prompt suffix", s.ID)
		}
	}
	if _, err := LookupStyle(DefaultStyleID); err != nil {
		t.Fatal(err)
	}
	if _, err := LookupStyle("nope"); err == nil {
		t.Fatal("expected unknown style error")
	}
}

func TestStoreReserveAndConcurrency(t *testing.T) {
	home := t.TempDir()
	s := NewStore(home)
	k1, err := s.ReserveKey("tea", &Generation{Name: "tea", CreatedAt: time.Now()})
	if err != nil || k1 != "tea" {
		t.Fatal(k1, err)
	}
	k2, _ := s.ReserveKey("tea", &Generation{Name: "tea"})
	if k2 != "tea-2" {
		t.Fatalf("k2=%s", k2)
	}
	k3, _ := s.ReserveKey("state.json", &Generation{})
	if k3 == "state.json" {
		t.Fatal("reserved name used as key")
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = s.Update("tea", func(g *Generation) error { g.Claude.CostUSD += 1; return nil })
		}()
	}
	wg.Wait()
	g, _ := s.Get("tea")
	if g.Claude.CostUSD != 50 {
		t.Fatalf("lost updates: %v", g.Claude.CostUSD)
	}
	// A running generation owned by a dead pid reads back as interrupted.
	_, _ = s.Update("tea", func(g *Generation) error { g.Status = StatusRunning; g.OwnerPID = 999999; return nil })
	host, _ := os.Hostname()
	_, _ = s.Update("tea", func(g *Generation) error { g.OwnerHost = host; return nil })
	if g, _ := s.Get("tea"); g.Status != StatusInterrupted {
		t.Fatalf("status=%s", g.Status)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "state.json")); !json.Valid(b) {
		t.Fatal("state.json is not valid JSON")
	}
}

func TestConfigEnvNotPersisted(t *testing.T) {
	home := t.TempDir()
	t.Setenv("KLING_API_KEY", "env-secret")
	t.Setenv("NOTION_TOKEN", "")
	cfg, err := LoadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Kling.APIKey != "env-secret" || !cfg.FromEnv("kling.api_key") {
		t.Fatal("env override not applied")
	}
	cfg.Video.Style = "pov-skit"
	if err := SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(home, "config.json"))
	if strings.Contains(string(b), "env-secret") {
		t.Fatal("env secret leaked into config.json")
	}
	if !strings.Contains(string(b), "pov-skit") {
		t.Fatal("config change not saved")
	}
	st, _ := os.Stat(filepath.Join(home, "config.json"))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perm %v", st.Mode().Perm())
	}
}

func TestClassifySource(t *testing.T) {
	if s, err := ClassifySource("https://www.notion.so/acme/Doc-1a2b3c4d5e6f4a1b8c9d0e1f2a3b4c5d"); err != nil || s.Kind != SourceNotion {
		t.Fatal(s, err)
	}
	if _, err := ClassifySource("https://example.com/doc.md"); err == nil {
		t.Fatal("non-notion URL should be rejected")
	}
	if _, err := ClassifySource(t.TempDir()); err == nil {
		t.Fatal("directory should be rejected")
	}
	if _, err := ClassifySource("/definitely/not/here.md"); err == nil {
		t.Fatal("missing file should be rejected")
	}
}

func TestSlugAndAutoName(t *testing.T) {
	if Slugify("  Rate Limit Tea!! 💅 ") != "rate-limit-tea" {
		t.Fatal(Slugify("  Rate Limit Tea!! 💅 "))
	}
	n := AutoName("Kubernetes Networking Deep Dive")
	if !strings.Contains(n, "kubernetes-networking-deep") || Slugify(n) != n {
		t.Fatalf("auto name %q", n)
	}
}
