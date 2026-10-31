package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeTrendsClaude emits stream-json: optional WebSearch tool calls, then a
// result whose structured_output is the given research payload.
func fakeTrendsClaude(t *testing.T, payload any, searches int) string {
	t.Helper()
	body, _ := json.Marshal(payload)
	var lines []string
	lines = append(lines, `{"type":"system","subtype":"init"}`)
	for i := 0; i < searches; i++ {
		lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"WebSearch","input":{"query":"tiktok trends october 2026"}}]}}`)
	}
	lines = append(lines, `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"WebFetch","input":{"url":"https://example.com/trends"}}]}}`)
	res, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "result": string(body), "structured_output": json.RawMessage(body),
		"total_cost_usd": 0.42, "modelUsage": map[string]any{"claude-sonnet-5-5": map[string]any{"inputTokens": 1, "outputTokens": 1, "costUSD": 0.42}},
	})
	lines = append(lines, string(res))
	script := "#!/bin/sh\ncase \"$*\" in *\"auth status\"*) echo '{\"loggedIn\":true,\"authMethod\":\"claude.ai\",\"subscriptionType\":\"max\"}'; exit 0;; esac\n" +
		"case \"$*\" in *stream-json*) ;; *) echo 'expected stream-json' >&2; exit 3;; esac\n" +
		"case \"$*\" in *WebSearch,WebFetch*) ;; *) echo 'expected web tools' >&2; exit 4;; esac\n" +
		"cat > /dev/null\ncat <<'EOF'\n" + strings.Join(lines, "\n") + "\nEOF\n"
	bin := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

func researchedStyle(id string) Style {
	s, _ := LookupStyle("top5-hype")
	s.ID = id
	s.Name = "Fresh " + id
	s.Slang = []string{"maxxing", "huzz", "aura*", "Aura*"}
	s.PromptSuffix = "full-body parking garage, see https://evil.example.com"
	s.Captions.Position = 0.95
	s.Captions.TextColor = "white"
	s.Structure = []Beat{{"hook", 10, "x"}, {"body", 60, "y"}, {"outro", 10, "z"}}
	return s
}

func TestRefreshApplyReset(t *testing.T) {
	updated, _ := LookupStyle("bestie-explainer")
	updated.Slang = append([]string{"brand-new-term"}, updated.Slang...)
	payload := map[string]any{
		"notes":   "## What's hot\n- duet formats are back (Oct 2026)",
		"sources": []map[string]string{{"title": "Trends", "url": "https://example.com/trends", "date": "2026-10-20"}, {"title": "bad", "url": "javascript:alert(1)"}},
		"styles":  []Style{researchedStyle("duet-chaos"), updated},
	}
	e := testEngine(t, nil)
	e.Config.Claude.Binary = fakeTrendsClaude(t, payload, 3)
	var logs []string
	p, err := e.RefreshTrends(context.Background(), TrendsRefreshOptions{Log: func(s string) { logs = append(logs, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "search: tiktok trends october 2026") {
		t.Fatalf("tool calls not streamed: %v", logs)
	}
	if len(p.Added) != 1 || p.Added[0] != "duet-chaos" || len(p.Updated) != 1 || p.Updated[0] != "bestie-explainer" || len(p.Removed) != 0 {
		t.Fatalf("diff added=%v updated=%v removed=%v", p.Added, p.Updated, p.Removed)
	}
	if len(p.File.Sources) != 1 || p.File.CostUSD != 0.42 {
		t.Fatalf("sources/cost %+v", p.File)
	}
	if _, err := os.Stat(p.Path); err != nil {
		t.Fatal("proposal not saved")
	}
	// Not active until applied.
	if _, err := e.Style("duet-chaos"); err == nil {
		t.Fatal("proposal leaked into active catalog before apply")
	}
	// A generation created now keeps its style even after the catalog changes.
	g, err := e.Create(context.Background(), GenerateRequest{Source: writeDoc(t), Style: "bestie-explainer", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ApplyTrends(p); err != nil {
		t.Fatal(err)
	}
	s, err := e.Style("duet-chaos")
	if err != nil {
		t.Fatal(err)
	}
	for _, sl := range s.Slang {
		if strings.EqualFold(sl, "huzz") {
			t.Fatal("banned slang survived")
		}
	}
	if len(s.Slang) != 2 || strings.Contains(s.PromptSuffix, "http") || !strings.Contains(s.PromptSuffix, "9:16") || !strings.Contains(s.PromptSuffix, "no background music") {
		t.Fatalf("sanitize: %+v / %q", s.Slang, s.PromptSuffix)
	}
	if s.Captions.Position > 0.76 || s.Captions.TextColor != "#FFFFFF" {
		t.Fatalf("captions not clamped: %+v", s.Captions)
	}
	if got := len(e.Styles()); got != len(StyleCatalog)+1 {
		t.Fatalf("merge catalog size %d", got)
	}
	if snap := GenerationStyle(g); snap.ID != "bestie-explainer" || snap.Slang[0] == "brand-new-term" {
		t.Fatal("generation style snapshot changed after refresh")
	}
	if _, err := os.Stat(filepath.Join(e.Home, "TRENDS.md")); err != nil {
		t.Fatal("TRENDS.md not written")
	}
	info := e.TrendsInfo()
	if info.Source != "refreshed" || info.Stale {
		t.Fatalf("info %+v", info)
	}
	// A second refresh in merge mode carries forward the earlier refreshed preset.
	payload["styles"] = []Style{researchedStyle("pov-remix")}
	e.Config.Claude.Binary = fakeTrendsClaude(t, payload, 1)
	p2, err := e.RefreshTrends(context.Background(), TrendsRefreshOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, s := range p2.Catalog {
		ids[s.ID] = true
	}
	if !ids["duet-chaos"] || !ids["pov-remix"] {
		t.Fatalf("merge lost presets: %v", ids)
	}
	if err := e.ApplyTrends(p2); err != nil {
		t.Fatal(err)
	}
	hist, _ := os.ReadDir(filepath.Join(e.Home, "trends-history"))
	archived := 0
	for _, h := range hist {
		if strings.HasPrefix(h.Name(), "trends-") {
			archived++
		}
	}
	if archived < 1 {
		t.Fatal("previous catalog not archived")
	}
	if err := e.ResetTrends(); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Style("duet-chaos"); err == nil || len(e.Styles()) != len(StyleCatalog) {
		t.Fatal("reset didn't restore built-ins")
	}
}

func TestRefreshRequiresWebResearch(t *testing.T) {
	e := testEngine(t, nil)
	e.Config.Claude.Binary = fakeTrendsClaude(t, map[string]any{"notes": "", "sources": []any{}, "styles": []Style{researchedStyle("x-y")}}, 0)
	if _, err := e.RefreshTrends(context.Background(), TrendsRefreshOptions{}); err == nil || !strings.Contains(err.Error(), "web searches") {
		t.Fatalf("want no-search refusal, got %v", err)
	}
}

func TestReplaceModeAndStaleness(t *testing.T) {
	var styles []Style
	for _, id := range []string{"a-one", "b-two", "c-three", "d-four"} {
		styles = append(styles, researchedStyle(id))
	}
	e := testEngine(t, nil)
	e.Config.Claude.Binary = fakeTrendsClaude(t, map[string]any{"notes": "n", "sources": []any{}, "styles": styles}, 2)
	p, err := e.RefreshTrends(context.Background(), TrendsRefreshOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Catalog) != 4 || len(p.Removed) != len(StyleCatalog) {
		t.Fatalf("replace: catalog=%d removed=%d", len(p.Catalog), len(p.Removed))
	}
	if err := e.ApplyTrends(p); err != nil {
		t.Fatal(err)
	}
	// Configured default style vanished → fall back gracefully.
	if e.DefaultStyle().ID != "a-one" {
		t.Fatalf("fallback default %s", e.DefaultStyle().ID)
	}
	e.Now = func() time.Time { return time.Now().Add(45 * 24 * time.Hour) }
	if !e.TrendsInfo().Stale {
		t.Fatal("45-day-old catalog should be stale")
	}
	// Corrupt trends file never blocks generation.
	_ = os.WriteFile(filepath.Join(e.Home, "trends.json"), []byte("{nope"), 0o644)
	e.resetStyleCache()
	if len(e.Styles()) != len(StyleCatalog) {
		t.Fatal("corrupt trends.json should fall back to built-ins")
	}
}

func TestSanitizeStyleRejects(t *testing.T) {
	if _, _, err := SanitizeStyle(Style{ID: "x", Name: "x", Persona: "p", PromptSuffix: "s", Structure: []Beat{{"a", 50, ""}}}); err == nil {
		t.Fatal("too few beats should be rejected")
	}
	if _, _, err := SanitizeStyle(Style{ID: "", Name: ""}); err == nil {
		t.Fatal("missing id should be rejected")
	}
	s, w, err := SanitizeStyle(researchedStyle("Weird ID!!"))
	if err != nil || s.ID != "weird-id" || len(w) == 0 {
		t.Fatalf("%v %v %v", s.ID, w, err)
	}
	sum := 0
	for _, b := range s.Structure {
		sum += b.Percent
	}
	if sum != 100 {
		t.Fatalf("beats sum %d", sum)
	}
}
