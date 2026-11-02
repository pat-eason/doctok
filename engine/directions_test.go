package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/pat-eason/doctok/engine/kling"
)

func TestCleanDirection(t *testing.T) {
	got, err := cleanDirection("voice direction", "  warm,\n  unhurried \t dry   humor ")
	if err != nil || got != "warm, unhurried dry humor" {
		t.Fatalf("%q %v", got, err)
	}
	if _, err := cleanDirection("stage direction", strings.Repeat("x", MaxDirectionRunes+1)); err == nil {
		t.Fatal("over-long direction must be rejected")
	}
	if got, err := cleanDirection("x", "   "); err != nil || got != "" {
		t.Fatalf("blank → %q %v", got, err)
	}
}

func TestStoryboardDirectionsAndPlacement(t *testing.T) {
	if StoryboardDirections(GenerationOptions{}) != "" {
		t.Fatal("no direction → no text")
	}
	d := StoryboardDirections(GenerationOptions{VoiceDirection: "warm and dry", StageDirection: "always at a desk", ActorImage: "/x/ACTOR.jpg"})
	for _, want := range []string{"VOICE DIRECTION", "warm and dry", "STAGE DIRECTION", "always at a desk", "ACTOR", "reference photo", "Do NOT invent"} {
		if !strings.Contains(d, want) {
			t.Errorf("missing %q in:\n%s", want, d)
		}
	}
	p := withDirections("rules\n<distilled_notes>\nthe doc\n</distilled_notes>\n\nReturn only the JSON storyboard.", d)
	if i, j := strings.Index(p, "USER DIRECTION"), strings.Index(p, "<distilled_notes>"); i < 0 || i > j {
		t.Fatalf("direction must come before the notes:\n%s", p)
	}
}

func TestKlingPromptWithActorVoiceAndStage(t *testing.T) {
	sb := testStoryboard()
	st, _ := LookupStyle("bestie-explainer")
	spec := kling.Models["kling-2.6"]

	plain := KlingPromptWith(&sb, sb.Scenes[0], st, spec, true, PromptExtras{})
	if plain != KlingPrompt(&sb, sb.Scenes[0], st, spec, true) || !strings.Contains(plain, "lavender hoodie") || !strings.Contains(plain, "cozy bedroom") {
		t.Fatalf("no extras must not change the prompt:\n%s", plain)
	}

	p := KlingPromptWith(&sb, sb.Scenes[0], st, spec, true, PromptExtras{Actor: true, Voice: "warm and dry.", Stage: "always at a desk"})
	for _, want := range []string{
		"[Bestie]: the person in the reference image", "exact face",
		"Setting: the same place as in the reference image.",
		"Stage direction: always at a desk.", "Voice direction: warm and dry.",
		"[Bestie, whisper] looks into the camera and says:",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in:\n%s", want, p)
		}
	}
	for _, bad := range []string{"lavender hoodie", "cozy bedroom"} {
		if strings.Contains(p, bad) {
			t.Errorf("an actor photo must replace the invented look (%q still in prompt):\n%s", bad, p)
		}
	}
	// A silent model has no voice to direct, but the stage direction still applies.
	silent := KlingPromptWith(&sb, sb.Scenes[0], st, kling.Models["kling-2.5-turbo"], true, PromptExtras{Voice: "warm", Stage: "at a desk"})
	if strings.Contains(silent, "Voice direction") || !strings.Contains(silent, "Stage direction: at a desk.") {
		t.Errorf("silent model:\n%s", silent)
	}
}

func TestAllowUploadHost(t *testing.T) {
	for raw, want := range map[string]bool{
		"https://kling.ai/api/mcp/files":         true,
		"https://upload.klingai.com/x":           true,
		"http://kling.ai/api/mcp/files":          false, // not https
		"https://evil.example.com/kling.ai":      false,
		"https://kling.ai.evil.com/files":        false,
		"https://notkling.ai/files":              false,
		"https://klingai.com.attacker.io/upload": false,
	} {
		u, _ := url.Parse(raw)
		if got := allowUploadHost(u); got != want {
			t.Errorf("%s → %v, want %v", raw, got, want)
		}
	}
}

func TestUploadImageRefusesUnexpectedHost(t *testing.T) {
	e, _ := mcpEngine(t, map[string]string{
		"__file_upload": `{"ok":true,"generation_id":"","status":"succeeded","video_url":"","image_url":"","upload_url":"https://attacker.example.com/steal","upload_ticket":"T","credits":0,"credits_known":false,"error_kind":"none","error":""}`,
	})
	c, _, err := e.newKlingBackend(context.Background(), e.Config.Kling)
	if err != nil {
		t.Fatal(err)
	}
	photo := filepath.Join(t.TempDir(), "me.jpg")
	_ = os.WriteFile(photo, []byte{0xFF, 0xD8, 0xFF, 0xE0}, 0o644)
	_, err = c.(imageUploader).UploadImage(context.Background(), photo)
	if err == nil || !strings.Contains(err.Error(), "unexpected address") {
		t.Fatalf("a photo must never be sent to a non-Kling host: %v", err)
	}
}

func TestCreateValidatesActorPhoto(t *testing.T) {
	tools := needFFmpeg(t)
	mk := func(size, name string) string {
		p := filepath.Join(t.TempDir(), name)
		if out, err := exec.Command(tools.FFmpeg, "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size="+size+":rate=1", "-frames:v", "1", p).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
		return p
	}
	good := mk("1200x800", "me.jpg")
	doc := writeDoc(t)

	newEng := func(mut func(*Config)) *Engine {
		e := testEngine(t, nil)
		e.Config.Kling.Backend = KlingBackendMCP
		if mut != nil {
			mut(&e.Config)
		}
		return e
	}
	create := func(e *Engine, req GenerateRequest) (*Generation, error) {
		req.Source, req.Style = doc, "bestie-explainer"
		return e.Create(context.Background(), req)
	}

	// Happy path: copied into the generation folder; directions are stored trimmed.
	e := newEng(nil)
	g, err := create(e, GenerateRequest{ActorImage: good, VoiceDirection: "  warm   and dry ", StageDirection: "at a desk"})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(g.Options.ActorImage) != "ACTOR.jpg" || !fileExists(g.Options.ActorImage) || filepath.Dir(g.Options.ActorImage) != g.Dir {
		t.Fatalf("actor not snapshotted into the generation: %q", g.Options.ActorImage)
	}
	if g.Options.VoiceDirection != "warm and dry" || g.Options.StageDirection != "at a desk" {
		t.Fatalf("directions: %+v", g.Options)
	}
	if !e.NeedsHero(g) {
		t.Fatal("an actor photo needs the reference-image step")
	}

	// Dry runs never touch Kling, so the photo is ignored rather than rejected.
	if g, err := create(e, GenerateRequest{ActorImage: good, DryRun: true}); err != nil || g.Options.ActorImage != "" {
		t.Fatalf("dry run: %v %+v", err, g)
	}

	for name, tc := range map[string]struct {
		eng *Engine
		req GenerateRequest
		sub string
	}{
		"api backend":  {newEng(func(c *Config) { c.Kling.Backend = KlingBackendAPI }), GenerateRequest{ActorImage: good}, "MCP backend"},
		"no_hero":      {newEng(func(c *Config) { c.Kling.NoHero = true }), GenerateRequest{ActorImage: good}, "no_hero"},
		"missing":      {e, GenerateRequest{ActorImage: filepath.Join(t.TempDir(), "nope.jpg")}, "actor photo"},
		"not an image": {e, GenerateRequest{ActorImage: doc}, "JPG or PNG"},
		"too small":    {e, GenerateRequest{ActorImage: mk("320x320", "tiny.jpg")}, "too small"},
		"long voice":   {e, GenerateRequest{VoiceDirection: strings.Repeat("v", 401)}, "voice direction"},
		"long stage":   {e, GenerateRequest{StageDirection: strings.Repeat("s", 401)}, "stage direction"},
		"green screen": {e, GenerateRequest{ActorImage: good, Style: "green-screen-yapper"}, "green screen"},
		"corrupt image": {e, GenerateRequest{ActorImage: func() string {
			p := filepath.Join(t.TempDir(), "x.png")
			_ = os.WriteFile(p, []byte("not an image"), 0o644)
			return p
		}()}, "image"},
	} {
		req := tc.req
		req.Source = doc
		if req.Style == "" {
			req.Style = "bestie-explainer"
		}
		if _, err := tc.eng.Create(context.Background(), req); err == nil || !strings.Contains(err.Error(), tc.sub) {
			t.Errorf("%s: want error containing %q, got %v", name, tc.sub, err)
		}
	}
}

// TestPipelineWithActorPhotoAndDirections: the user's photo (not a generated
// image) is the presenter, directions reach the storyboard and every clip, and
// nothing is generated or paid for before approval.
func TestPipelineWithActorPhotoAndDirections(t *testing.T) {
	tools := needFFmpeg(t)
	fk := newFakeKling(t, makeClipVideo(t))
	e := testEngine(t, fk)
	orig := e.Config.Claude.Binary

	// Stand-in for kling.ai's upload endpoint + CDN.
	var mu sync.Mutex
	var gotTicket, gotType string
	var gotBytes int
	var cdn *httptest.Server
	cdn = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/mcp/files":
			if err := r.ParseMultipartForm(32 << 20); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
			b, _ := io.ReadAll(f)
			mu.Lock()
			gotTicket, gotType, gotBytes = r.FormValue("ticket"), hdr.Header.Get("Content-Type"), len(b)
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"status": 200, "result": 1, "message": "", "data": map[string]any{"url": cdn.URL + "/kimg/actor.jpg", "fileType": "image", "fileSize": len(b)}})
		case r.URL.Path == "/kimg/actor.jpg":
			w.WriteHeader(200)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cdn.Close)
	cdnURL := cdn.URL + "/kimg/actor.jpg"
	prev := allowUploadHost
	allowUploadHost = func(u *url.URL) bool { return u.Host == strings.TrimPrefix(cdn.URL, "http://") }
	t.Cleanup(func() { allowUploadHost = prev })

	dir := t.TempDir()
	vcount, submits, dirlog := filepath.Join(dir, "v"), filepath.Join(dir, "submits.log"), filepath.Join(dir, "direction.log")
	imagesLog := filepath.Join(dir, "images.log")
	reply := func(body string) string {
		env, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": "", "structured_output": json.RawMessage(body), "total_cost_usd": 0.001})
		return string(env)
	}
	script := `#!/bin/sh
case "$*" in
  *"mcp list"*) echo "kling: https://kling.ai/mcp (HTTP) - ✔ Connected"; exit 0;;
  *"auth status"*|*--version*) exec '` + orig + `' "$@";;
esac
IN=$(cat)
case "$IN" in
  *__text_to_image*) echo "$IN" >> '` + imagesLog + `'; echo "unexpected text_to_image" >&2; exit 1;;
  *__file_upload*)
    cat <<'EOF'
` + reply(`{"ok":true,"generation_id":"","status":"succeeded","video_url":"","image_url":"","upload_url":"`+cdn.URL+`/api/mcp/files","upload_ticket":"TICKET-1","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *__image_to_video*|*__text_to_video*)
    n=$(( $(cat '` + vcount + `' 2>/dev/null || echo 0) + 1 )); echo $n > '` + vcount + `'
    echo "$IN" >> '` + submits + `'
    cat <<EOF
` + reply(`{"ok":true,"generation_id":"GEN-$n","status":"submitted","video_url":"","image_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *__query_tasks*)
    cat <<'EOF'
` + reply(`{"ok":true,"generation_id":"x","status":"succeeded","video_url":"`+fk.srv.URL+`/files/clip.mp4","image_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *) case "$IN" in *"USER DIRECTION"*) printf '%s' "$IN" > '` + dirlog + `';; esac
     printf '%s' "$IN" | '` + orig + `' "$@";;
esac
`
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.Config.Claude.Binary = bin
	e.Config.Kling.Backend = KlingBackendMCP
	e.Config.Kling.APIKey, e.Config.Kling.APIStyle = "", KlingStyleAuto
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	photo := filepath.Join(t.TempDir(), "me.jpg")
	if out, err := exec.Command(tools.FFmpeg, "-loglevel", "error", "-y", "-f", "lavfi", "-i", "testsrc2=size=1600x900:rate=1", "-frames:v", "1", photo).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}

	ctx := context.Background()
	g, err := e.Create(ctx, GenerateRequest{
		Source: writeDoc(t), Name: "me tea", Style: "bestie-explainer", ActorImage: photo,
		VoiceDirection: "warm, unhurried, dry humor", StageDirection: "always at a desk with a mug",
	})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = e.Plan(ctx, g.Key, nil); err != nil {
		t.Fatal(err)
	}
	// The storyboard writer was told about the actor and both directions.
	dl := read(dirlog)
	for _, want := range []string{"USER DIRECTION", "warm, unhurried, dry humor", "always at a desk with a mug", "reference photo"} {
		if !strings.Contains(dl, want) {
			t.Errorf("storyboard prompt missing %q", want)
		}
	}
	if strings.Index(dl, "USER DIRECTION") > strings.Index(dl, "<distilled_notes>") {
		t.Error("direction must precede the distilled notes")
	}

	// No clip before the photo is approved.
	if _, err := e.Render(ctx, g.Key, nil); err == nil || !strings.Contains(err.Error(), "presenter") {
		t.Fatalf("render before approval: %v", err)
	}
	// The photo path: crop → upload; no image generation, no credits.
	if g, err = e.Hero(ctx, g.Key, false, nil); err != nil {
		t.Fatal(err)
	}
	if g.Hero == nil || g.Hero.Source != "upload" || g.Hero.URL != cdnURL || g.Hero.TaskID != "" || g.Hero.Approved || filepath.Base(g.Hero.Path) != "HERO.jpg" {
		t.Fatalf("hero %+v", g.Hero)
	}
	if w, h, err := tools.ImageSize(ctx, g.Hero.Path); err != nil || w != 1080 || h != 1920 {
		t.Fatalf("hero must be the 9:16 crop, got %dx%d %v", w, h, err)
	}
	mu.Lock()
	ticket, ctype, nbytes := gotTicket, gotType, gotBytes
	mu.Unlock()
	if ticket != "TICKET-1" || ctype != "image/jpeg" || nbytes < 1000 {
		t.Fatalf("upload: ticket=%q type=%q bytes=%d", ticket, ctype, nbytes)
	}
	if read(imagesLog) != "" {
		t.Fatal("an actor photo must not trigger paid image generation")
	}

	if _, err := e.ApproveHero(g.Key); err != nil {
		t.Fatal(err)
	}
	g, err = e.Render(ctx, g.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusSucceeded {
		t.Fatalf("status=%s err=%s", g.Status, g.Error)
	}
	sub := read(submits)
	if n := strings.Count(sub, `"name":"first_image"`); n != 4 || strings.Count(sub, cdnURL) != 4 {
		t.Fatalf("every clip must start from the uploaded photo (%d/4):\n%s", n, sub)
	}
	for _, want := range []string{"the person in the reference image", "Voice direction: warm, unhurried, dry humor.", "Stage direction: always at a desk with a mug."} {
		if n := strings.Count(sub, want); n != 4 {
			t.Errorf("%q should be in all 4 clip prompts, found %d", want, n)
		}
	}
	if strings.Contains(sub, "lavender hoodie") || strings.Contains(sub, "cozy bedroom") {
		t.Errorf("the invented look must not reach Kling when a photo is used:\n%s", sub)
	}
}
