package engine

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/pat-eason/doctok/engine/media"
)

// MaxDirectionRunes bounds each free-text direction. Kling truncates long
// prompts from the end (which is where the "no on-screen text" suffix lives),
// so directions stay short.
const MaxDirectionRunes = 400

// maxActorBytes is Kling's image size limit.
const maxActorBytes = 30 << 20

// cleanDirection normalizes whitespace and enforces the length limit.
func cleanDirection(what, s string) (string, error) {
	s = strings.Join(strings.Fields(s), " ")
	if n := utf8.RuneCountInString(s); n > MaxDirectionRunes {
		return "", fmt.Errorf("%s is %d characters; keep it under %d", what, n, MaxDirectionRunes)
	}
	return s, nil
}

// checkActor validates a requested actor photo and returns its absolute path
// ("" when none is used). Dry runs ignore it: they never touch Kling.
func (e *Engine) checkActor(ctx context.Context, req GenerateRequest, style Style) (string, error) {
	if strings.TrimSpace(req.ActorImage) == "" || req.DryRun {
		return "", nil
	}
	switch {
	case e.Config.Kling.Backend != KlingBackendMCP:
		return "", fmt.Errorf("an actor photo needs the Kling MCP backend (set kling.backend to mcp); the API backend can't animate a reference image")
	case e.Config.Kling.NoHero:
		return "", fmt.Errorf("an actor photo can't be combined with kling.no_hero (the photo is the reference image)")
	case style.ChromaKey:
		return "", fmt.Errorf("style %q keys out a green screen, which your photo's background isn't; pick a different style", style.ID)
	}
	p, err := filepath.Abs(expandHome(strings.TrimSpace(req.ActorImage)))
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", fmt.Errorf("actor photo: %w", err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("actor photo %s is a directory", p)
	}
	if ext := strings.ToLower(filepath.Ext(p)); ext != ".jpg" && ext != ".jpeg" && ext != ".png" {
		return "", fmt.Errorf("actor photo must be a JPG or PNG (got %q)", filepath.Ext(p))
	}
	if st.Size() > maxActorBytes {
		return "", fmt.Errorf("actor photo is %.1f MB; Kling's limit is %d MB", float64(st.Size())/1e6, maxActorBytes>>20)
	}
	tools, err := e.mediaTools()
	if err != nil {
		return "", err
	}
	w, h, err := tools.ImageSize(ctx, p)
	if err != nil {
		return "", err
	}
	if _, _, err := media.ActorCrop(w, h); err != nil {
		return "", err
	}
	return p, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// StoryboardDirections is appended to the storyboard prompt when the user gave
// any direction or an actor photo; "" otherwise.
func StoryboardDirections(o GenerationOptions) string {
	if o.VoiceDirection == "" && o.StageDirection == "" && o.ActorImage == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nUSER DIRECTION (from the person commissioning the video; apply it throughout. It beats the style's defaults where they conflict, but never the hard requirements above or the doc's facts)\n")
	if o.VoiceDirection != "" {
		fmt.Fprintf(&b, "- VOICE DIRECTION: %q. Reflect it in how every line is written (rhythm, word choice, register) and in each line's \"tone\".\n", o.VoiceDirection)
	}
	if o.StageDirection != "" {
		fmt.Fprintf(&b, "- STAGE DIRECTION: %q. Reflect it in every scene's \"action\", \"camera\" and \"dance\" (blocking, props, framing, mood, pacing of beats).\n", o.StageDirection)
	}
	if o.ActorImage != "" {
		b.WriteString("- ACTOR: the presenter is a real person supplied as a reference photo; every clip starts from that photo. Define exactly ONE character (label \"Presenter\"), and set its \"description\" to just: the presenter from the reference photo, with face, hair and outfit exactly as in the photo. Do NOT invent age, hair, clothing or accessories. Leave \"setting\" as: the same place as in the reference photo. Write actions the person could plausibly do in that setting.\n")
	}
	return b.String()
}

// PromptExtras are per-generation additions to every Kling clip prompt.
type PromptExtras struct {
	Actor bool   // clips start from the user's photo: refer to the person, not a described character
	Voice string // voice direction (only used when the clip has audio)
	Stage string // stage direction
}

// ExtrasFor derives the clip prompt extras for a generation. Actor is only set
// once the reference image is actually in use.
func ExtrasFor(o GenerationOptions, usingReferenceImage bool) PromptExtras {
	return PromptExtras{Actor: o.ActorImage != "" && usingReferenceImage, Voice: o.VoiceDirection, Stage: o.StageDirection}
}

// withDirections inserts direction text just before the distilled notes, so it
// reads as instructions rather than as part of the document.
func withDirections(prompt, dir string) string {
	if dir == "" {
		return prompt
	}
	const marker = "\n<distilled_notes>\n"
	if i := strings.Index(prompt, marker); i >= 0 {
		return prompt[:i] + dir + prompt[i:]
	}
	return prompt + dir
}
