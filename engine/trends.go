package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pat-eason/doctok/engine/claude"
	"github.com/pat-eason/doctok/engine/media"
)

// BuiltinTrendsDate is when the compiled-in StyleCatalog was researched.
var BuiltinTrendsDate = time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)

// TrendsStaleAfter is when doctor starts nagging about a refresh.
const TrendsStaleAfter = 30 * 24 * time.Hour

const (
	TrendsModeMerge   = "merge"   // built-ins + refreshed presets (refreshed wins on id clash)
	TrendsModeReplace = "replace" // refreshed presets only
)

const (
	fileTrends        = "trends.json"
	fileTrendsReport  = "TRENDS.md"
	dirTrendsHistory  = "trends-history"
	fileStyleSnapshot = "STYLE.json"
)

// TrendSource is one cited source.
type TrendSource struct {
	Title string `json:"title"`
	URL   string `json:"url"`
	Date  string `json:"date,omitempty"`
}

// TrendsFile is ~/.doctok/trends.json: a researched style catalog.
type TrendsFile struct {
	Version     int           `json:"version"`
	GeneratedAt time.Time     `json:"generated_at"`
	Mode        string        `json:"mode"`
	Model       string        `json:"model,omitempty"`
	Billing     string        `json:"billing,omitempty"`
	CostUSD     float64       `json:"cost_usd"`
	Focus       string        `json:"focus,omitempty"`
	Notes       string        `json:"notes"`
	Sources     []TrendSource `json:"sources"`
	Styles      []Style       `json:"styles"`
}

// TrendsInfo summarizes the active catalog.
type TrendsInfo struct {
	Source      string    `json:"source"` // built-in | refreshed
	GeneratedAt time.Time `json:"generated_at"`
	AgeDays     int       `json:"age_days"`
	Stale       bool      `json:"stale"`
	Mode        string    `json:"mode"`
	Styles      int       `json:"styles"`
	Sources     int       `json:"sources"`
	Path        string    `json:"path,omitempty"`
}

func (e *Engine) trendsPath() string { return filepath.Join(e.Home, fileTrends) }

// LoadTrends reads trends.json; (nil, nil) when it doesn't exist.
func (e *Engine) LoadTrends() (*TrendsFile, error) {
	b, err := os.ReadFile(e.trendsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var tf TrendsFile
	if err := json.Unmarshal(b, &tf); err != nil {
		return nil, fmt.Errorf("parse %s: %w", e.trendsPath(), err)
	}
	return &tf, nil
}

// composeCatalog applies a trends file over the built-in catalog.
func composeCatalog(tf *TrendsFile) []Style {
	if tf == nil || len(tf.Styles) == 0 {
		return append([]Style(nil), StyleCatalog...)
	}
	if tf.Mode == TrendsModeReplace {
		return append([]Style(nil), tf.Styles...)
	}
	out := append([]Style(nil), StyleCatalog...)
	idx := map[string]int{}
	for i, s := range out {
		idx[s.ID] = i
	}
	for _, s := range tf.Styles {
		if i, ok := idx[s.ID]; ok {
			out[i] = s
		} else {
			idx[s.ID] = len(out)
			out = append(out, s)
		}
	}
	return out
}

// Styles returns the active catalog (built-ins merged with any refresh).
func (e *Engine) Styles() []Style {
	e.stylesMu.Lock()
	defer e.stylesMu.Unlock()
	if e.styles == nil {
		tf, err := e.LoadTrends()
		if err != nil {
			tf = nil // a corrupt trends file must never block generation
		}
		e.styles = composeCatalog(tf)
	}
	return e.styles
}

// Style finds a style in the active catalog.
func (e *Engine) Style(id string) (Style, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	ids := make([]string, 0, len(e.Styles()))
	for _, s := range e.Styles() {
		if s.ID == id {
			return s, nil
		}
		ids = append(ids, s.ID)
	}
	return Style{}, fmt.Errorf("unknown style %q (available: %s)", id, strings.Join(ids, ", "))
}

// DefaultStyle returns the configured default if it still exists, else a fallback.
func (e *Engine) DefaultStyle() Style {
	if s, err := e.Style(e.Config.Video.Style); err == nil {
		return s
	}
	if s, err := e.Style(DefaultStyleID); err == nil {
		return s
	}
	return e.Styles()[0]
}

// TrendsInfo describes the active catalog.
func (e *Engine) TrendsInfo() TrendsInfo {
	info := TrendsInfo{Source: "built-in", GeneratedAt: BuiltinTrendsDate, Mode: TrendsModeMerge, Styles: len(e.Styles())}
	if tf, err := e.LoadTrends(); err == nil && tf != nil {
		info.Source = "refreshed"
		info.GeneratedAt = tf.GeneratedAt
		info.Mode = tf.Mode
		info.Sources = len(tf.Sources)
		info.Path = e.trendsPath()
	}
	age := e.Now().Sub(info.GeneratedAt)
	info.AgeDays = int(age.Hours() / 24)
	info.Stale = age > TrendsStaleAfter
	return info
}

// snapshotStyle freezes a generation's style so later catalog refreshes can't
// change (or remove) it mid-flight.
func snapshotStyle(dir string, s Style) error {
	b, _ := json.MarshalIndent(s, "", "  ")
	return os.WriteFile(filepath.Join(dir, fileStyleSnapshot), b, 0o644)
}

// GenerationStyle returns the style a generation was created with.
func GenerationStyle(g *Generation) Style {
	if g != nil && g.Dir != "" {
		if b, err := os.ReadFile(filepath.Join(g.Dir, fileStyleSnapshot)); err == nil {
			var s Style
			if json.Unmarshal(b, &s) == nil && s.ID != "" {
				return s
			}
		}
	}
	if g == nil {
		return Style{}
	}
	if s, err := LookupStyle(g.Style); err == nil {
		return s
	}
	return Style{ID: g.Style, Name: g.Style, Emoji: "🎬"}
}

func (e *Engine) styleFor(g *Generation) (Style, error) {
	s := GenerationStyle(g)
	if s.PromptSuffix != "" {
		return s, nil
	}
	return e.Style(g.Style)
}

// ---- refresh -----------------------------------------------------------------

// TrendsRefreshOptions tunes a refresh.
type TrendsRefreshOptions struct {
	Replace bool   // drop built-ins, use only the refreshed presets
	Focus   string // optional steer, e.g. "more dance-heavy formats"
	Model   string // Claude model override
	Log     func(string)
}

// TrendsProposal is a validated, not-yet-applied refresh.
type TrendsProposal struct {
	File     *TrendsFile `json:"file"`
	Catalog  []Style     `json:"catalog"` // resulting active catalog if applied
	Added    []string    `json:"added"`
	Updated  []string    `json:"updated"`
	Removed  []string    `json:"removed"`
	Kept     []string    `json:"kept"`
	Warnings []string    `json:"warnings"`
	Path     string      `json:"path"` // where the proposal was saved
}

type trendsOut struct {
	Notes   string        `json:"notes"`
	Sources []TrendSource `json:"sources"`
	Styles  []Style       `json:"styles"`
}

// TrendsSchema is the JSON schema for the research output.
func TrendsSchema() json.RawMessage {
	str := map[string]any{"type": "string"}
	strs := map[string]any{"type": "array", "items": str}
	num := map[string]any{"type": "number"}
	boolean := map[string]any{"type": "boolean"}
	captions := map[string]any{
		"type": "object",
		"required": []string{"text_color", "stroke_color", "highlight_color", "position", "words_per_chunk", "uppercase",
			"top_box_color", "top_text_color", "top_position"},
		"properties": map[string]any{
			"text_color": str, "stroke_color": str, "highlight_color": str,
			"position": num, "words_per_chunk": map[string]any{"type": "integer"}, "uppercase": boolean,
			"font_scale": num, "top_box_color": str, "top_text_color": str, "top_position": num,
			"top_uppercase": boolean, "top_scale": num,
		},
	}
	style := map[string]any{
		"type": "object",
		"required": []string{"id", "name", "emoji", "description", "persona", "setting", "structure", "camera", "prompt_suffix",
			"edit_grammar", "moves", "voice", "slang", "example_lines", "avoid", "words_per_sec", "captions"},
		"properties": map[string]any{
			"id": str, "name": str, "emoji": str, "description": str, "persona": str, "setting": str,
			"structure": map[string]any{"type": "array", "minItems": 3, "maxItems": 10, "items": map[string]any{
				"type": "object", "required": []string{"name", "percent", "direction"},
				"properties": map[string]any{"name": str, "percent": map[string]any{"type": "integer"}, "direction": str},
			}},
			"camera": str, "prompt_suffix": str, "edit_grammar": str, "moves": strs, "voice": str,
			"slang": strs, "example_lines": strs, "avoid": strs, "words_per_sec": num,
			"alternate_zoom": boolean, "chroma_key": boolean, "soft_cuts": boolean,
			"grade":    map[string]any{"type": "string", "enum": []string{"", "faded"}},
			"captions": captions,
		},
	}
	schema := map[string]any{
		"type":     "object",
		"required": []string{"notes", "sources", "styles"},
		"properties": map[string]any{
			"notes": str,
			"sources": map[string]any{"type": "array", "items": map[string]any{
				"type": "object", "required": []string{"title", "url"},
				"properties": map[string]any{"title": str, "url": str, "date": str},
			}},
			"styles": map[string]any{"type": "array", "minItems": 4, "maxItems": 14, "items": style},
		},
	}
	b, _ := json.Marshal(schema)
	return b
}

// TrendsPrompt builds the research brief.
func TrendsPrompt(today time.Time, current []Style, replace bool, focus string) string {
	cur, _ := json.Marshal(current)
	mode := "MERGE: return presets to ADD or UPDATE (reuse an existing id to update it; a new id adds a preset). Omit presets that are still accurate and need no change — they are kept automatically. Also include updated versions of any preset whose slang/format has gone stale."
	if replace {
		mode = "REPLACE: return the COMPLETE new catalog (6–12 presets). Anything you omit is removed. Reuse ids of presets you keep."
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Today is %s. You maintain the style catalog for "doctok", a tool that turns long technical
docs into 3–5 minute vertical (9:16, 1080x1920) TikTok-influencer-style explainer videos.
Claude writes a storyboard of ~18–30 scenes; each scene is rendered as a separate AI video clip
by Kling text-to-video (native audio voices the dialogue); ffmpeg cuts them together and burns
in word-chunk captions plus a top text sticker. Tone: humorous Gen-Z / Gen-Alpha, dances as
punctuation beats, jump cuts, current slang.

TASK
1. Use WebSearch (and WebFetch for promising pages) to research what is trending on TikTok RIGHT
   NOW — prioritize sources from the last 4–6 weeks: formats, edit styles, creator archetypes,
   EduTok/#LearnOnTikTok formats that perform, caption styles, dance/movement trends, and the
   most-searched current slang. Run several searches; prefer dated sources. Flag older/evergreen
   items as such. Never invent trends — if you can't verify recency, say so in notes.
2. Compare against the current catalog below and produce updated presets.
   Mode — %s
`, today.Format("January 2, 2006"), mode)
	if strings.TrimSpace(focus) != "" {
		fmt.Fprintf(&b, "   Extra direction from the user: %s\n", strings.TrimSpace(focus))
	}
	b.WriteString(`
PRESET RULES (each field is pasted into prompts, so be concrete)
- Build presets on recurring FORMATS, not single sounds/memes that die in days.
- id: kebab-case. persona: a fictional creator archetype with concrete appearance/wardrobe cues
  (no real people, celebrities, creators, brands, shows, songs, logos or copyrighted characters;
  no minors in romantic/sexual contexts; family-friendly).
- structure: beats with percent of runtime summing to ~100, structured as chained mini-chapters.
- prompt_suffix: a reusable Kling style string; MUST include "vertical 9:16" and "no background music".
- moves: generic, renderable movement/dance descriptions (no named choreography tied to a song).
- slang: 12–30 CURRENT terms (mark older-but-alive ones with *). Never include misogynistic,
  hateful or harassment slang (e.g. foid, huzz, chud, bird/bop as insults) or slurs.
- example_lines: 2–3 lines explaining a technical concept in the voice of the preset.
- words_per_sec: 1.8–2.6 (dialogue pacing for ~5/10 s clips).
- captions: hex colors (#RRGGBB, or #RRGGBBAA for translucent boxes), position = vertical center of
  the caption band as a fraction of height (keep 0.55–0.74: TikTok UI covers the bottom ~20%),
  top_position 0.10–0.14, words_per_chunk 1–4, highlight_color = active-word color ("" = none),
  top_box_color "none" for stroked meme-style text instead of a sticker box.
- Edit treatments doctok can actually do: alternate_zoom (punch in every other clip),
  chroma_key (creator on green screen composited over doc excerpt cards), soft_cuts (fades),
  grade "faded" (washed-out look), and per-scene cuts (hard-cut, punch-in, white flash, soft fade).
  Don't promise effects outside this list in edit_grammar.

OUTPUT
- notes: Markdown "trend notes" summarizing what you found, with dates, and what changed vs the
  current catalog and why.
- sources: every URL you relied on (title, url, date if known).
- styles: the presets per the mode above.

CURRENT CATALOG (JSON):
`)
	b.Write(cur)
	b.WriteString("\n\nReturn only the JSON object.")
	return b.String()
}

// RefreshTrends researches current TikTok trends with Claude Code (web tools)
// and returns a validated proposal. Nothing is written to the active catalog
// until ApplyTrends; the proposal itself is saved under trends-history/.
func (e *Engine) RefreshTrends(ctx context.Context, opts TrendsRefreshOptions) (*TrendsProposal, error) {
	log := opts.Log
	if log == nil {
		log = func(string) {}
	}
	billing, err := e.resolveBilling(ctx)
	if err != nil {
		return nil, err
	}
	current := e.Styles()
	r := e.claudeRunner(os.TempDir())
	if opts.Model != "" {
		r.Model = opts.Model
	}
	log(fmt.Sprintf("Claude (%s) is researching TikTok trends on the web…", billing))
	searches := 0
	res, err := r.Run(ctx, claude.Request{
		Prompt:       TrendsPrompt(e.Now(), current, opts.Replace, opts.Focus),
		SystemPrompt: "You are a meticulous social-media trend researcher. Cite sources, verify recency, never fabricate.",
		Schema:       TrendsSchema(),
		Tools:        []string{"WebSearch", "WebFetch"},
		Timeout:      40 * time.Minute,
		OnTool: func(tc claude.ToolCall) {
			switch tc.Name {
			case "WebSearch":
				searches++
			case "WebFetch":
			default:
				return // internal tools (e.g. StructuredOutput) aren't research progress
			}
			log(tc.Summary())
		},
	})
	if err != nil {
		return nil, fmt.Errorf("trend research: %w", err)
	}
	var out trendsOut
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return nil, fmt.Errorf("parse trend research: %w", err)
	}
	if searches == 0 {
		return nil, errors.New("Claude didn't run any web searches — refusing to publish trends from memory (is WebSearch available to your Claude Code?)")
	}

	var warns []string
	var styles []Style
	seen := map[string]bool{}
	for _, s := range out.Styles {
		clean, w, err := SanitizeStyle(s)
		for _, x := range w {
			warns = append(warns, clean.ID+": "+x)
		}
		if err != nil {
			warns = append(warns, fmt.Sprintf("dropped %q: %v", s.ID, err))
			continue
		}
		if seen[clean.ID] {
			warns = append(warns, "dropped duplicate id "+clean.ID)
			continue
		}
		seen[clean.ID] = true
		styles = append(styles, clean)
	}
	minStyles := 1
	if opts.Replace {
		minStyles = 4
	}
	if len(styles) < minStyles {
		return nil, fmt.Errorf("research produced only %d valid presets (need ≥%d); warnings: %s", len(styles), minStyles, strings.Join(warns, "; "))
	}
	var sources []TrendSource
	for _, s := range out.Sources {
		if strings.HasPrefix(s.URL, "http://") || strings.HasPrefix(s.URL, "https://") {
			sources = append(sources, TrendSource{Title: clip(s.Title, 200), URL: clip(s.URL, 500), Date: clip(s.Date, 40)})
		}
	}
	mode := TrendsModeMerge
	if opts.Replace {
		mode = TrendsModeReplace
	}
	tf := &TrendsFile{
		Version: 1, GeneratedAt: e.Now().UTC(), Mode: mode, Model: res.Model, Billing: billing,
		CostUSD: res.CostUSD, Focus: opts.Focus, Notes: clip(out.Notes, 20000), Sources: sources, Styles: styles,
	}
	// In merge mode, carry forward presets from a previous refresh that this one didn't touch.
	if mode == TrendsModeMerge {
		if prev, _ := e.LoadTrends(); prev != nil && prev.Mode == TrendsModeMerge {
			for _, s := range prev.Styles {
				if !seen[s.ID] {
					tf.Styles = append(tf.Styles, s)
				}
			}
		}
	}
	p := &TrendsProposal{File: tf, Catalog: composeCatalog(tf), Warnings: warns}
	p.diff(current)
	if err := os.MkdirAll(filepath.Join(e.Home, dirTrendsHistory), 0o755); err != nil {
		return nil, err
	}
	p.Path = filepath.Join(e.Home, dirTrendsHistory, "proposal-"+tf.GeneratedAt.Format("20060102-150405")+".json")
	b, _ := json.MarshalIndent(tf, "", "  ")
	if err := os.WriteFile(p.Path, b, 0o644); err != nil {
		return nil, err
	}
	log(fmt.Sprintf("Research done: %d searches, %d sources, %d presets ($%.4f)", searches, len(sources), len(styles), res.CostUSD))
	return p, nil
}

func (p *TrendsProposal) diff(current []Style) {
	cur := map[string]Style{}
	for _, s := range current {
		cur[s.ID] = s
	}
	next := map[string]bool{}
	for _, s := range p.Catalog {
		next[s.ID] = true
		old, ok := cur[s.ID]
		switch {
		case !ok:
			p.Added = append(p.Added, s.ID)
		case !reflect.DeepEqual(old, s):
			p.Updated = append(p.Updated, s.ID)
		default:
			p.Kept = append(p.Kept, s.ID)
		}
	}
	for _, s := range current {
		if !next[s.ID] {
			p.Removed = append(p.Removed, s.ID)
		}
	}
	sort.Strings(p.Removed)
}

// ApplyTrends activates a proposal (archiving the previous trends.json).
func (e *Engine) ApplyTrends(p *TrendsProposal) error {
	if p == nil || p.File == nil {
		return errors.New("nothing to apply")
	}
	if err := e.archiveTrends(); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(p.File, "", "  ")
	if err := writeFileAtomic(e.trendsPath(), b, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(e.Home, fileTrendsReport), []byte(TrendsReport(p.File, p.Catalog)), 0o644); err != nil {
		return err
	}
	e.resetStyleCache()
	return nil
}

// ResetTrends reverts to the built-in catalog (archiving the current one).
func (e *Engine) ResetTrends() error {
	if err := e.archiveTrends(); err != nil {
		return err
	}
	for _, f := range []string{e.trendsPath(), filepath.Join(e.Home, fileTrendsReport)} {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	e.resetStyleCache()
	return nil
}

func (e *Engine) archiveTrends() error {
	b, err := os.ReadFile(e.trendsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	dir := filepath.Join(e.Home, dirTrendsHistory)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "trends-"+e.Now().UTC().Format("20060102-150405")+".json"), b, 0o644)
}

func (e *Engine) resetStyleCache() {
	e.stylesMu.Lock()
	e.styles = nil
	e.stylesMu.Unlock()
}

// TrendsReport renders TRENDS.md.
func TrendsReport(tf *TrendsFile, catalog []Style) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# TikTok trends — doctok style catalog\n\nRefreshed %s · mode %s · %d presets · Claude %s ($%.4f, %s)\n\n",
		tf.GeneratedAt.Format("2006-01-02 15:04 MST"), tf.Mode, len(catalog), orDefault(tf.Model, "default"), tf.CostUSD, orDefault(tf.Billing, "—"))
	if tf.Focus != "" {
		fmt.Fprintf(&b, "Focus: %s\n\n", tf.Focus)
	}
	b.WriteString("## Trend notes\n\n" + strings.TrimSpace(tf.Notes) + "\n\n## Presets\n\n| id | name | description |\n|---|---|---|\n")
	for _, s := range catalog {
		fmt.Fprintf(&b, "| `%s` | %s %s | %s |\n", s.ID, s.Emoji, s.Name, strings.ReplaceAll(s.Description, "|", "\\|"))
	}
	b.WriteString("\n## Sources\n\n")
	for _, s := range tf.Sources {
		date := ""
		if s.Date != "" {
			date = " (" + s.Date + ")"
		}
		fmt.Fprintf(&b, "- [%s](%s)%s\n", orDefault(s.Title, s.URL), s.URL, date)
	}
	return b.String()
}

// ---- validation ----------------------------------------------------------------

// bannedSlang is never allowed in presets regardless of what the web says.
var bannedSlang = []string{"foid", "huzz", "chud", "bird", "bop", "gyatt"}

var (
	hexRE     = regexp.MustCompile(`^#([0-9A-Fa-f]{6}|[0-9A-Fa-f]{8})$`)
	urlRE     = regexp.MustCompile(`https?://\S+`)
	controlRE = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f]`)
)

func clip(s string, n int) string {
	s = strings.TrimSpace(controlRE.ReplaceAllString(s, ""))
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

// cleanText strips URLs/markup-ish noise from fields that flow into prompts.
func cleanText(s string, n int) string {
	s = urlRE.ReplaceAllString(s, "")
	s = strings.NewReplacer("<", "", ">", "", "```", "").Replace(s)
	return clip(strings.Join(strings.Fields(s), " "), n)
}

func cleanList(in []string, maxItems, maxLen int) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		s = cleanText(s, maxLen)
		k := strings.ToLower(s)
		if s == "" || seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
		if len(out) == maxItems {
			break
		}
	}
	return out
}

func isBanned(term string) bool {
	t := strings.ToLower(strings.Trim(term, " *"))
	for _, b := range bannedSlang {
		if t == b || strings.HasPrefix(t, b+" ") {
			return true
		}
	}
	return false
}

func clampF(v, lo, hi, def float64) float64 {
	if v == 0 || math.IsNaN(v) {
		return def
	}
	return math.Max(lo, math.Min(hi, v))
}

// SanitizeStyle validates and normalizes a researched preset. It returns
// warnings for anything it fixed and an error if the preset is unusable.
func SanitizeStyle(s Style) (Style, []string, error) {
	var w []string
	s.ID = Slugify(s.ID)
	if s.ID == "" {
		s.ID = Slugify(s.Name)
	}
	if s.ID == "" {
		return s, nil, errors.New("missing id")
	}
	s.Name = cleanText(s.Name, 60)
	s.Emoji = clip(s.Emoji, 8)
	if s.Emoji == "" {
		s.Emoji = "🎬"
	}
	s.Description = cleanText(s.Description, 200)
	s.Persona = cleanText(s.Persona, 700)
	s.Setting = cleanText(s.Setting, 300)
	s.Camera = cleanText(s.Camera, 400)
	s.PromptSuffix = cleanText(s.PromptSuffix, 450)
	s.EditGrammar = cleanText(s.EditGrammar, 400)
	s.Voice = cleanText(s.Voice, 300)
	if s.Name == "" || s.Persona == "" || s.PromptSuffix == "" {
		return s, w, errors.New("missing name, persona or prompt_suffix")
	}
	low := strings.ToLower(s.PromptSuffix)
	if !strings.Contains(low, "9:16") {
		s.PromptSuffix = "vertical 9:16, " + s.PromptSuffix
		w = append(w, "added 9:16 to prompt_suffix")
	}
	if !strings.Contains(low, "no background music") {
		s.PromptSuffix += ", no background music"
		w = append(w, "added 'no background music' to prompt_suffix")
	}
	// Beats: keep 3–10, rescale to 100%.
	var beats []Beat
	total := 0
	for _, b := range s.Structure {
		b.Name, b.Direction = cleanText(b.Name, 40), cleanText(b.Direction, 300)
		if b.Name == "" || b.Percent <= 0 {
			continue
		}
		beats = append(beats, b)
		total += b.Percent
		if len(beats) == 10 {
			break
		}
	}
	if len(beats) < 3 {
		return s, w, errors.New("needs at least 3 structure beats")
	}
	if total != 100 {
		acc := 0
		for i := range beats {
			beats[i].Percent = int(math.Round(float64(beats[i].Percent) * 100 / float64(total)))
			acc += beats[i].Percent
		}
		beats[len(beats)-1].Percent += 100 - acc
		w = append(w, fmt.Sprintf("rescaled beats from %d%% to 100%%", total))
	}
	s.Structure = beats
	s.Moves = cleanList(s.Moves, 10, 80)
	s.ExampleLines = cleanList(s.ExampleLines, 4, 300)
	s.Avoid = cleanList(s.Avoid, 8, 160)
	var slang []string
	for _, t := range cleanList(s.Slang, 40, 40) {
		if isBanned(t) {
			w = append(w, "removed banned slang "+t)
			continue
		}
		slang = append(slang, t)
	}
	if len(slang) > 30 {
		slang = slang[:30]
	}
	s.Slang = slang
	s.WordsPerSec = clampF(s.WordsPerSec, 1.6, 2.8, 2.3)
	if s.Grade != "" && s.Grade != "faded" {
		w = append(w, "unknown grade "+s.Grade+" removed")
		s.Grade = ""
	}
	d := media.DefaultCaptionStyle()
	c := &s.Captions
	fixColor := func(v *string, def string, allowEmpty, allowNone bool) {
		switch {
		case allowEmpty && *v == "":
		case allowNone && strings.EqualFold(*v, media.NoBox):
			*v = media.NoBox
		case !hexRE.MatchString(*v):
			if *v != "" {
				w = append(w, "invalid color "+*v)
			}
			*v = def
		}
	}
	fixColor(&c.TextColor, d.TextColor, false, false)
	fixColor(&c.StrokeColor, d.StrokeColor, false, false)
	fixColor(&c.HighlightColor, "", true, false)
	fixColor(&c.TopBoxColor, d.TopBoxColor, false, true)
	fixColor(&c.TopTextColor, d.TopTextColor, false, false)
	c.Position = clampF(c.Position, 0.5, 0.76, d.Position)
	c.TopPosition = clampF(c.TopPosition, 0.08, 0.16, d.TopPosition)
	if c.WordsPerChunk < 1 || c.WordsPerChunk > 5 {
		c.WordsPerChunk = d.WordsPerChunk
	}
	c.FontScale = clampF(c.FontScale, 0.7, 1.4, 1)
	c.TopScale = clampF(c.TopScale, 0.8, 1.6, 1)
	return s, w, nil
}
