package engine

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"
)

var (
	nameAdjectives = []string{"slay", "bussin", "unhinged", "delulu", "rizzed", "goated", "yassified", "feral", "iconic", "lowkey", "highkey", "tuff", "aura", "chopped", "cooked", "maxxing", "unc-coded", "no-crumbs"}
	nameNouns      = []string{"era", "arc", "core", "drop", "moment", "serve", "tea", "lore", "glowup", "speedrun", "explainer", "storytime"}
	slugRE         = regexp.MustCompile(`[^a-z0-9]+`)
)

// Slugify makes a filesystem-safe generation key.
func Slugify(s string) string {
	s = slugRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = strings.Trim(s[:60], "-")
	}
	return s
}

func randIndex(n int) int {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return (int(b[0])<<8 | int(b[1])) % n
}

func shortHex() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// AutoName invents a name like "yassified-kubernetes-networking-era-3f9a".
func AutoName(hint string) string {
	adj := nameAdjectives[randIndex(len(nameAdjectives))]
	noun := nameNouns[randIndex(len(nameNouns))]
	words := strings.Fields(slugRE.ReplaceAllString(strings.ToLower(hint), " "))
	if len(words) > 3 {
		words = words[:3]
	}
	parts := []string{adj}
	parts = append(parts, words...)
	parts = append(parts, noun, shortHex())
	return strings.Join(parts, "-")
}
