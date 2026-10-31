package engine

import (
	"fmt"
	"sort"
	"strings"

	"github.com/pat-eason/doctok/engine/media"
)

// Beat is one section of a style's narrative arc.
type Beat struct {
	Name      string `json:"name"`
	Percent   int    `json:"percent"` // approx share of runtime
	Direction string `json:"direction"`
}

// Style is a selectable video style preset. Everything here flows into the
// storyboard prompt (Claude) and the per-clip prompts (Kling), and into the
// caption renderer.
type Style struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Emoji       string `json:"emoji"`
	Description string `json:"description"`

	Persona      string   `json:"persona"`       // creator archetype & appearance cues
	Setting      string   `json:"setting"`       // default location
	Structure    []Beat   `json:"structure"`     // narrative arc
	Camera       string   `json:"camera"`        // visual / camera grammar
	PromptSuffix string   `json:"prompt_suffix"` // appended to every Kling prompt
	EditGrammar  string   `json:"edit_grammar"`  // how to use cuts
	Moves        []string `json:"moves"`         // dance / movement vocabulary
	Voice        string   `json:"voice"`         // delivery direction for native audio
	Slang        []string `json:"slang"`
	ExampleLines []string `json:"example_lines"`
	Avoid        []string `json:"avoid"`
	WordsPerSec  float64  `json:"words_per_sec"` // speaking pace for dialogue sizing

	// Edit treatments applied by the assembler.
	AlternateZoom bool   `json:"alternate_zoom,omitempty"` // punch in on every other clip (jump-cut feel)
	ChromaKey     bool   `json:"chroma_key,omitempty"`     // key out green and composite doc excerpt cards
	SoftCuts      bool   `json:"soft_cuts,omitempty"`      // fade-in from black instead of punchy cuts
	Grade         string `json:"grade,omitempty"`          // color grade preset ("faded")

	Captions media.CaptionStyle `json:"captions"`
}

// LookupStyle finds a preset by id.
func LookupStyle(id string) (Style, error) {
	if s, ok := styleIndex()[strings.ToLower(strings.TrimSpace(id))]; ok {
		return s, nil
	}
	return Style{}, fmt.Errorf("unknown style %q (available: %s)", id, strings.Join(StyleIDs(), ", "))
}

// StyleIDs lists preset ids in catalog order.
func StyleIDs() []string {
	ids := make([]string, len(StyleCatalog))
	for i, s := range StyleCatalog {
		ids[i] = s.ID
	}
	return ids
}

func styleIndex() map[string]Style {
	m := make(map[string]Style, len(StyleCatalog))
	for _, s := range StyleCatalog {
		m[s.ID] = s
	}
	return m
}

// SortedStyles returns the catalog sorted by name (for pickers that want it).
func SortedStyles() []Style {
	out := append([]Style(nil), StyleCatalog...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Brief renders the style as prompt text for the storyboard writer.
func (s Style) Brief() string {
	var b strings.Builder
	fmt.Fprintf(&b, "STYLE: %s %s — %s\n", s.Emoji, s.Name, s.Description)
	fmt.Fprintf(&b, "Creator persona: %s\n", s.Persona)
	if s.Setting != "" {
		fmt.Fprintf(&b, "Default setting: %s\n", s.Setting)
	}
	if len(s.Structure) > 0 {
		b.WriteString("Narrative arc (approx % of runtime):\n")
		for _, beat := range s.Structure {
			fmt.Fprintf(&b, "  - %s (%d%%): %s\n", beat.Name, beat.Percent, beat.Direction)
		}
	}
	fmt.Fprintf(&b, "Camera & visual grammar: %s\n", s.Camera)
	fmt.Fprintf(&b, "Edit grammar: %s\n", s.EditGrammar)
	if len(s.Moves) > 0 {
		fmt.Fprintf(&b, "Movement / dance vocabulary: %s\n", strings.Join(s.Moves, "; "))
	}
	fmt.Fprintf(&b, "Voice & delivery: %s\n", s.Voice)
	if len(s.Slang) > 0 {
		fmt.Fprintf(&b, "Slang word bank (use naturally, never more than 2–3 per line): %s\n", strings.Join(s.Slang, ", "))
	}
	if len(s.ExampleLines) > 0 {
		b.WriteString("Example lines:\n")
		for _, l := range s.ExampleLines {
			fmt.Fprintf(&b, "  - %q\n", l)
		}
	}
	if len(s.Avoid) > 0 {
		fmt.Fprintf(&b, "Avoid: %s\n", strings.Join(s.Avoid, "; "))
	}
	return b.String()
}
