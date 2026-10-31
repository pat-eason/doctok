package engine

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/pat-eason/doctok/engine/kling"
)

// Character is a reusable, byte-identical character block for every clip.
type Character struct {
	Label       string `json:"label"`       // e.g. "Bestie", "The Server"
	Description string `json:"description"` // appearance + wardrobe, identical every clip
	Voice       string `json:"voice"`       // tone label used in dialogue tags
}

// Line is one spoken line.
type Line struct {
	Speaker string `json:"speaker"` // must match a Character.Label
	Tone    string `json:"tone"`
	Text    string `json:"text"`
}

// Scene is one Kling clip.
type Scene struct {
	Beat     string `json:"beat"`
	Seconds  int    `json:"seconds"`
	Action   string `json:"action"` // what happens visually: gestures, movement, dance
	Dance    string `json:"dance"`  // optional dance move for this scene
	Camera   string `json:"camera"`
	Cut      string `json:"cut"`      // hard-cut | punch-in | flash | soft
	TopText  string `json:"top_text"` // on-screen sticker / chapter card
	Lines    []Line `json:"lines"`
	DocQuote string `json:"doc_quote,omitempty"` // excerpt shown behind green-screen creators
}

// Storyboard is Claude's script for the whole video.
type Storyboard struct {
	Title      string      `json:"title"`
	Hook       string      `json:"hook"`
	Characters []Character `json:"characters"`
	Setting    string      `json:"setting"`
	Scenes     []Scene     `json:"scenes"`
}

// TotalSeconds sums scene durations.
func (sb *Storyboard) TotalSeconds() int {
	t := 0
	for _, s := range sb.Scenes {
		t += s.Seconds
	}
	return t
}

// Dialogue returns all spoken text of a scene.
func (s Scene) Dialogue() string {
	parts := make([]string, 0, len(s.Lines))
	for _, l := range s.Lines {
		if t := strings.TrimSpace(l.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

var validCuts = map[string]bool{"hard-cut": true, "punch-in": true, "flash": true, "soft": true}

// StoryboardSchema returns the JSON schema handed to `claude --json-schema`.
func StoryboardSchema(spec kling.ModelSpec) json.RawMessage {
	durs := make([]any, len(spec.Durations))
	for i, d := range spec.Durations {
		durs[i] = d
	}
	str := map[string]any{"type": "string"}
	schema := map[string]any{
		"type":     "object",
		"required": []string{"title", "hook", "characters", "setting", "scenes"},
		"properties": map[string]any{
			"title":   str,
			"hook":    str,
			"setting": str,
			"characters": map[string]any{
				"type": "array", "minItems": 1, "maxItems": 2,
				"items": map[string]any{
					"type": "object", "required": []string{"label", "description", "voice"},
					"properties": map[string]any{"label": str, "description": str, "voice": str},
				},
			},
			"scenes": map[string]any{
				"type": "array", "minItems": 8, "maxItems": 60,
				"items": map[string]any{
					"type":     "object",
					"required": []string{"beat", "seconds", "action", "camera", "cut", "top_text", "lines"},
					"properties": map[string]any{
						"beat":      str,
						"seconds":   map[string]any{"type": "integer", "enum": durs},
						"action":    str,
						"dance":     str,
						"camera":    str,
						"cut":       map[string]any{"type": "string", "enum": []string{"hard-cut", "punch-in", "flash", "soft"}},
						"top_text":  str,
						"doc_quote": str,
						"lines": map[string]any{
							"type": "array", "maxItems": 2,
							"items": map[string]any{
								"type": "object", "required": []string{"speaker", "tone", "text"},
								"properties": map[string]any{"speaker": str, "tone": str, "text": str},
							},
						},
					},
				},
			},
		},
	}
	b, _ := json.Marshal(schema)
	return b
}

// DistillPrompt asks Claude to boil the doc down to teachable material.
func DistillPrompt(doc string) string {
	return `You are the research brain behind a comedic Gen-Z TikTok explainer channel.
Read the technical document below and produce DISTILL.md: a faithful, accurate
distillation that a short-form video writer can turn into a 3–5 minute video.

Write GitHub-flavored Markdown with exactly these sections:

# <punchy title for the topic>
## TL;DR
(3 sentences max, plain English)
## Who this is for / why it matters
## Key concepts
(4–7 concepts, each: **name** — one-sentence plain explanation — one everyday-life
analogy a 16-year-old would get — the single most important detail to get right)
## How it works
(the core flow / mechanism as a numbered list)
## Gotchas & footguns
(the mistakes people actually make, and the fix)
## Numbers, names & commands worth quoting
(exact identifiers, limits, defaults, CLI commands or API names from the doc — verbatim)
## Spicy takes
(2–4 opinionated, funny-but-true observations about this tech)
## Quotable lines
(3–6 short verbatim excerpts from the doc, ≤ 200 characters each, that could be shown on screen)

Rules: be technically correct above all — never invent facts, APIs, numbers or
features that aren't in the document. If the doc is ambiguous, say so. Output only
the Markdown, no preamble.

<document>
` + doc + `
</document>`
}

// StoryboardPrompt asks Claude for the scene-by-scene script.
func StoryboardPrompt(distill string, style Style, spec kling.ModelSpec, audio bool, target, minS, maxS int) string {
	wps := style.WordsPerSec
	if wps <= 0 {
		wps = 2.3
	}
	durs := make([]string, len(spec.Durations))
	for i, d := range spec.Durations {
		durs[i] = fmt.Sprint(d)
	}
	durList := strings.Join(durs, ", ")
	if len(spec.Durations) > 4 {
		durList = fmt.Sprintf("any integer from %d to %d", spec.MinDuration(), spec.MaxDuration())
	}
	avgClip := float64(spec.MaxDuration())
	if avgClip > 10 {
		avgClip = 10
	}
	approxScenes := int(math.Round(float64(target) / avgClip))

	var b strings.Builder
	fmt.Fprintf(&b, `You write scripts for a viral, funny, Gen-Z / Gen-Alpha "EduTok" creator who
turns dense technical docs into TikToks people actually finish. Write a storyboard
for ONE vertical 9:16 video based on the distilled notes below.

HARD REQUIREMENTS
- Total runtime: sum of scene "seconds" between %d and %d seconds; aim for ~%d.
- Allowed scene lengths (seconds): %s. Roughly %d scenes.
- Each scene becomes a separate AI-generated video clip, so each scene must be
  self-contained: describe exactly what is visible in "action" and "camera".
- Structure it as a chain of mini-chapters, each opening with its own hook, so a
  3–5 minute video keeps retention. Put a short chapter card in "top_text" at each
  chapter start (every ~25–40 s); otherwise top_text may be "" or a short reaction
  sticker. top_text ≤ 6 words.
- Characters: define 1 character (2 only if the style is a duo/skit) with a unique
  "label" and a detailed "description" (age range, hair, outfit, accessories, vibe)
  that will be pasted byte-for-byte into every clip prompt. Fictional people only.
- Teach correctly: every technical claim must come from the notes. The comedy is in
  the delivery and analogies, never in getting facts wrong.
- Use dances/movement as short punctuation beats (outro, transitions, celebrating a
  concept), not the main content. Put the move in "dance" (else "").
- "cut" is how the clip starts: "punch-in" (zoomed jump cut — use on punchlines),
  "flash" (white flash — chapter starts / reveals), "soft" (gentle fade), or "hard-cut".
`, minS, maxS, target, durList, approxScenes)
	if audio {
		fmt.Fprintf(&b, `- Dialogue is voiced by the video model. Each scene has 0–2 "lines" (max 2
  speakers per scene). Keep dialogue to about %.1f words per second of scene
  (e.g. ~%d words for a 10 s scene, ~%d for 5 s) — shorter is better than longer.
  Write ordinary words in lowercase and ACRONYMS / Proper Nouns in caps. Spell out
  symbols and code the way you'd say them out loud.
`, wps, int(wps*10), int(wps*5))
	} else {
		fmt.Fprintf(&b, `- The clips are silent; "lines" become burned-in captions only. Keep each scene's
  lines to about %.1f words per second so captions are readable.
`, wps)
	}
	b.WriteString(`- Set "doc_quote" to a short verbatim excerpt from the notes' "Quotable lines" or
  "Numbers, names & commands" when the scene is reacting to the doc (≤ 200 chars),
  otherwise "".
- Avoid: ` + strings.Join(GlobalAvoid, "; ") + `.

`)
	b.WriteString(style.Brief())
	b.WriteString("\n<distilled_notes>\n")
	b.WriteString(distill)
	b.WriteString("\n</distilled_notes>\n\nReturn only the JSON storyboard.")
	return b.String()
}

// Normalize snaps durations to the model's allowed set, validates cuts and
// speakers, and nudges the runtime into [minS, maxS]. Returns warnings.
func (sb *Storyboard) Normalize(spec kling.ModelSpec, style Style, minS, maxS int) ([]string, error) {
	var warns []string
	if len(sb.Characters) == 0 {
		sb.Characters = []Character{{Label: "Creator", Description: style.Persona, Voice: style.Voice}}
		warns = append(warns, "storyboard had no characters; used the style persona")
	}
	if len(sb.Characters) > 2 {
		sb.Characters = sb.Characters[:2]
	}
	labels := map[string]bool{}
	for _, c := range sb.Characters {
		labels[c.Label] = true
	}
	var scenes []Scene
	for _, s := range sb.Scenes {
		if strings.TrimSpace(s.Action) == "" && len(s.Lines) == 0 {
			continue
		}
		s.Seconds = spec.SnapDuration(s.Seconds)
		if !validCuts[s.Cut] {
			s.Cut = "hard-cut"
		}
		if len(s.Lines) > 2 {
			s.Lines = s.Lines[:2]
		}
		for i := range s.Lines {
			if !labels[s.Lines[i].Speaker] {
				s.Lines[i].Speaker = sb.Characters[0].Label
			}
		}
		scenes = append(scenes, s)
	}
	sb.Scenes = scenes
	if len(sb.Scenes) == 0 {
		return warns, fmt.Errorf("storyboard has no usable scenes")
	}
	// Too short: lengthen the shortest scenes with the most dialogue first.
	for sb.TotalSeconds() < minS {
		i := sb.pickToResize(spec, true)
		if i < 0 {
			return warns, fmt.Errorf("storyboard runs %ds, below the %ds minimum even at max clip length", sb.TotalSeconds(), minS)
		}
		sb.Scenes[i].Seconds = nextDuration(spec, sb.Scenes[i].Seconds, true)
		warns = append(warns, fmt.Sprintf("lengthened scene %d to %ds to reach the minimum runtime", i+1, sb.Scenes[i].Seconds))
	}
	// Too long: shorten scenes with the least dialogue, then drop from the middle.
	for sb.TotalSeconds() > maxS {
		if i := sb.pickToResize(spec, false); i >= 0 {
			sb.Scenes[i].Seconds = nextDuration(spec, sb.Scenes[i].Seconds, false)
			continue
		}
		mid := len(sb.Scenes) / 2
		warns = append(warns, fmt.Sprintf("dropped scene %d (%q) to stay under %ds", mid+1, sb.Scenes[mid].Beat, maxS))
		sb.Scenes = append(sb.Scenes[:mid], sb.Scenes[mid+1:]...)
	}
	return warns, nil
}

func nextDuration(spec kling.ModelSpec, cur int, up bool) int {
	ds := append([]int(nil), spec.Durations...)
	sort.Ints(ds)
	if up {
		for _, d := range ds {
			if d > cur {
				return d
			}
		}
		return cur
	}
	for i := len(ds) - 1; i >= 0; i-- {
		if ds[i] < cur {
			return ds[i]
		}
	}
	return cur
}

// pickToResize chooses a scene to lengthen (up) or shorten (down); -1 if none.
func (sb *Storyboard) pickToResize(spec kling.ModelSpec, up bool) int {
	best, bestScore := -1, math.Inf(-1)
	for i, s := range sb.Scenes {
		next := nextDuration(spec, s.Seconds, up)
		if next == s.Seconds {
			continue
		}
		words := float64(len(strings.Fields(s.Dialogue())))
		density := words / float64(s.Seconds)
		score := density // lengthen the most crowded scenes
		if !up {
			// Shortening must not cram dialogue: prefer sparse scenes that still fit.
			if words/float64(next) > 3.0 {
				continue
			}
			score = -density
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	return best
}

// KlingPrompt builds the per-clip text-to-video prompt.
func KlingPrompt(sb *Storyboard, s Scene, style Style, spec kling.ModelSpec, audio bool) string {
	var b strings.Builder
	// Identical character blocks in every clip keep the creator consistent.
	for _, c := range sb.Characters {
		fmt.Fprintf(&b, "[%s]: %s. ", c.Label, strings.TrimSuffix(strings.TrimSpace(c.Description), "."))
	}
	setting := sb.Setting
	if setting == "" {
		setting = style.Setting
	}
	if style.ChromaKey {
		setting = "in front of a flat uniform bright green chroma-key screen"
	}
	if setting != "" {
		fmt.Fprintf(&b, "Setting: %s. ", strings.TrimSuffix(setting, "."))
	}
	if a := strings.TrimSpace(s.Action); a != "" {
		fmt.Fprintf(&b, "%s. ", strings.TrimSuffix(a, "."))
	}
	if d := strings.TrimSpace(s.Dance); d != "" {
		fmt.Fprintf(&b, "Dance move: %s. ", strings.TrimSuffix(d, "."))
	}
	if c := strings.TrimSpace(s.Camera); c != "" {
		fmt.Fprintf(&b, "Camera: %s. ", strings.TrimSuffix(c, "."))
	}
	if spec.EffectiveAudio(audio) {
		for _, l := range s.Lines {
			if strings.TrimSpace(l.Text) == "" {
				continue
			}
			tone := l.Tone
			if tone == "" {
				tone = "energetic"
			}
			fmt.Fprintf(&b, "[%s, %s] looks into the camera and says: \"%s\" ", l.Speaker, tone, strings.TrimSpace(l.Text))
		}
	} else if len(s.Lines) > 0 {
		b.WriteString("The creator talks expressively to the camera. ")
	}
	b.WriteString("Style: ")
	b.WriteString(style.PromptSuffix)
	b.WriteString(". No on-screen text, no subtitles, no logos, no watermark, consistent face and outfit.")
	return strings.TrimSpace(b.String())
}
