package kling

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Audio support levels.
const (
	AudioNone     = "none"     // model can't generate audio
	AudioOptional = "optional" // settings.audio native|off
	AudioAlways   = "always"   // audio is always generated
)

// ModelSpec describes a Kling text-to-video model: valid durations, audio
// support and the per-second unit price (from Kling's published pricing table,
// snapshot 2026-09; override via config if Kling changes it).
type ModelSpec struct {
	ID          string // new-standard path id, e.g. "kling-2.6"
	LegacyName  string // legacy model_name, e.g. "kling-v2-6" ("" if not available)
	Label       string
	Durations   []int // allowed clip lengths in seconds
	Audio       string
	Resolutions []string
	PromptMax   int
	// UnitsPerSecond[resolution][audio] → units/second
	UnitsPerSecond map[string]map[bool]float64
}

// Models is the registry of supported models.
var Models = map[string]ModelSpec{
	"kling-3.0-turbo": {
		ID: "kling-3.0-turbo", Label: "Kling 3.0 Turbo (native audio, 3–15s clips)",
		Durations: rangeInts(3, 15), Audio: AudioAlways, Resolutions: []string{"720p", "1080p"}, PromptMax: 3072,
		UnitsPerSecond: map[string]map[bool]float64{"720p": {true: 0.8}, "1080p": {true: 1.0}},
	},
	"kling-3.0": {
		ID: "kling-3.0", Label: "Kling 3.0 (optional native audio, 3–15s clips)",
		Durations: rangeInts(3, 15), Audio: AudioOptional, Resolutions: []string{"720p", "1080p"}, PromptMax: 2500,
		UnitsPerSecond: map[string]map[bool]float64{"720p": {false: 0.6, true: 0.9}, "1080p": {false: 0.8, true: 1.2}},
	},
	"kling-2.6": {
		ID: "kling-2.6", LegacyName: "kling-v2-6", Label: "Kling 2.6 (native audio @1080p, 5/10s clips)",
		Durations: []int{5, 10}, Audio: AudioOptional, Resolutions: []string{"720p", "1080p"}, PromptMax: 2500,
		UnitsPerSecond: map[string]map[bool]float64{"720p": {false: 0.3}, "1080p": {false: 0.5, true: 1.0}},
	},
	"kling-2.5-turbo": {
		ID: "kling-2.5-turbo", LegacyName: "kling-v2-5-turbo", Label: "Kling 2.5 Turbo (silent, 5/10s clips)",
		Durations: []int{5, 10}, Audio: AudioNone, Resolutions: []string{"720p", "1080p"}, PromptMax: 2500,
		UnitsPerSecond: map[string]map[bool]float64{"720p": {false: 0.3}, "1080p": {false: 0.5}},
	},
}

// ModelIDs returns registry keys sorted.
func ModelIDs() []string {
	ids := make([]string, 0, len(Models))
	for id := range Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Lookup finds a model by new-standard id or legacy name.
func Lookup(id string) (ModelSpec, bool) {
	if m, ok := Models[id]; ok {
		return m, true
	}
	for _, m := range Models {
		if m.LegacyName != "" && strings.EqualFold(m.LegacyName, id) {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// EffectiveAudio resolves whether audio will be generated given a request.
func (m ModelSpec) EffectiveAudio(want bool) bool {
	switch m.Audio {
	case AudioAlways:
		return true
	case AudioNone:
		return false
	}
	return want
}

// Validate checks a resolution/audio combination.
func (m ModelSpec) Validate(resolution string, audio bool) error {
	r, ok := m.UnitsPerSecond[resolution]
	if !ok {
		return fmt.Errorf("%s does not support %s", m.ID, resolution)
	}
	if _, ok := r[m.EffectiveAudio(audio)]; !ok {
		return fmt.Errorf("%s does not support audio=%v at %s (native audio requires 1080p)", m.ID, audio, resolution)
	}
	return nil
}

// UnitsFor estimates the unit cost of one clip.
func (m ModelSpec) UnitsFor(seconds int, resolution string, audio bool) float64 {
	rate := m.UnitsPerSecond[resolution][m.EffectiveAudio(audio)]
	return math.Round(rate*float64(seconds)*100) / 100
}

// SnapDuration returns the closest allowed clip length.
func (m ModelSpec) SnapDuration(sec int) int {
	best := m.Durations[0]
	for _, d := range m.Durations {
		if abs(d-sec) < abs(best-sec) || (abs(d-sec) == abs(best-sec) && d > best) {
			best = d
		}
	}
	return best
}

func (m ModelSpec) MinDuration() int { return m.Durations[0] }
func (m ModelSpec) MaxDuration() int { return m.Durations[len(m.Durations)-1] }

func rangeInts(a, b int) []int {
	out := make([]int, 0, b-a+1)
	for i := a; i <= b; i++ {
		out = append(out, i)
	}
	return out
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
