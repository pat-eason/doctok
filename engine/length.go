package engine

import (
	"fmt"

	"github.com/pat-eason/doctok/engine/kling"
)

// TargetWindow is the runtime window the storyboard must land in for a target
// length: ±15% of the target, clamped to the configured limits. (The old fixed
// 180–300s window would have forced a 45s video up to three minutes.)
func TargetWindow(target, cfgMin, cfgMax int) (lo, hi int) {
	lo = (target*85 + 50) / 100 // integer math, rounding half up: no float surprises
	hi = (target*115 + 50) / 100
	if lo < cfgMin {
		lo = cfgMin
	}
	if hi > cfgMax {
		hi = cfgMax
	}
	if lo > target {
		lo = target
	}
	if hi < target {
		hi = target
	}
	return lo, hi
}

// LengthLabel renders seconds the way people say them: "45s", "1 min", "1 min 30s".
func LengthLabel(sec int) string {
	switch {
	case sec < 60:
		return fmt.Sprintf("%ds", sec)
	case sec%60 == 0:
		return fmt.Sprintf("%d min", sec/60)
	default:
		return fmt.Sprintf("%d min %ds", sec/60, sec%60)
	}
}

// LengthGuidance is the storyboard instruction about narrative structure, scaled
// to the target length: a 30-second video can't carry chapters, a recap
// speed-run and a lore section, and a 4-minute one needs all of them.
func LengthGuidance(target int) string {
	switch {
	case target <= 45:
		return fmt.Sprintf(`- This is a SHORT video (~%s): hook in the first 2–3 seconds, then ONE core idea only
  (the single most important takeaway from the notes, with one analogy), a punchy
  payoff, and a quick outro. No chapters, no chapter cards, no lore/context section
  and no recap speed-run: skip those beats of the style's arc. "top_text" may be ""
  or one short reaction sticker (≤ 6 words).`, LengthLabel(target))
	case target <= 75:
		return fmt.Sprintf(`- This is a ~%s video: hook in the first 3 seconds, then ONE chapter of 2–3 beats
  (the idea → an analogy → the gotcha), then a short recap and an outro. Skip the
  lore section. At most one chapter card in "top_text" (≤ 6 words); otherwise "" or
  a short reaction sticker.`, LengthLabel(target))
	case target <= 130:
		return fmt.Sprintf(`- This is a ~%s video: hook, then TWO mini-chapters, each opening with its own hook
  line → an analogy → one twist, then a quick recap and an outro. Keep the lore to a
  single line. Put a short chapter card in "top_text" at each chapter start
  (≤ 6 words); otherwise "" or a short reaction sticker.`, LengthLabel(target))
	default:
		return `- Structure it as a chain of mini-chapters, each opening with its own hook, so a
  3–5 minute video keeps retention. Put a short chapter card in "top_text" at each
  chapter start (every ~25–40 s); otherwise top_text may be "" or a short reaction
  sticker. top_text ≤ 6 words.`
	}
}

// lengthPhrase describes the video for the distill step.
func lengthPhrase(target int) string {
	if target >= 150 {
		return "3–5 minute video"
	}
	return fmt.Sprintf("short ~%s video (so favor the few most important ideas over completeness)", LengthLabel(target))
}

// LengthOption is one selectable video length with its projected cost.
type LengthOption struct {
	Seconds int    `json:"seconds"`
	Name    string `json:"name"` // "snack", "one minute", …
	Label   string `json:"label"`
	// Cost projection for the current Kling settings. API backend: Units/USD from
	// the published price table. MCP backend: Credits = seconds × kling.credits_per_second
	// (0 when no rate is configured, because Kling doesn't publish per-clip credit prices).
	Units   float64 `json:"units,omitempty"`
	USD     float64 `json:"usd,omitempty"`
	Credits float64 `json:"credits,omitempty"`
}

var lengthChoices = []struct {
	sec  int
	name string
}{
	{30, "snack"}, {45, "quick hit"}, {60, "one minute"}, {90, "minute and a half"}, {120, "two minutes"},
	{180, "quick serve"}, {210, "sweet spot"}, {240, "deep lore"}, {300, "documentary era"},
}

// LengthOptions lists the selectable lengths within the configured limits, each
// with its cost projection for the configured model, resolution and backend.
func (e *Engine) LengthOptions() []LengthOption {
	k := e.Config.Kling
	spec, _ := kling.Lookup(k.Model)
	var out []LengthOption
	for _, c := range lengthChoices {
		if c.sec < e.Config.Video.MinSeconds || c.sec > e.Config.Video.MaxSeconds {
			continue
		}
		o := LengthOption{Seconds: c.sec, Name: c.name, Label: fmt.Sprintf("%d:%02d — %s", c.sec/60, c.sec%60, c.name)}
		if k.Backend == KlingBackendMCP {
			o.Credits = float64(c.sec) * k.CreditsPerSecond
		} else if spec.ID != "" {
			o.Units = spec.UnitsFor(c.sec, k.Resolution, k.Audio)
			o.USD = o.Units * k.UnitPriceUSD
		}
		out = append(out, o)
	}
	return out
}
