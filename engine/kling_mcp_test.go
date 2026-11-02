package engine

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/pat-eason/doctok/engine/kling"
)

// fakeMCPClaude stands in for `claude`: `mcp list` shows a Kling server, and
// -p runs append their args+stdin to a log and answer from per-tool replies.
func fakeMCPClaude(t *testing.T, replies map[string]string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	log = filepath.Join(dir, "calls.log")
	var cases strings.Builder
	for tool, body := range replies {
		env, _ := json.Marshal(map[string]any{"type": "result", "subtype": "success", "result": "", "structured_output": json.RawMessage(body), "total_cost_usd": 0.001})
		cases.WriteString("  *" + tool + "*) cat <<'EOF'\n" + string(env) + "\nEOF\n;;\n")
	}
	script := `#!/bin/sh
case "$*" in
  *"mcp list"*) echo "kling: https://kling.ai/mcp (HTTP) - ✔ Connected"; echo "claude.ai Kling: https://x - ✔ Connected"; exit 0;;
  *"auth status"*) echo '{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max"}'; exit 0;;
esac
IN=$(cat)
printf 'ARGS: %s\nSTDIN: %s\n---\n' "$*" "$IN" >> '` + log + `'
case "$IN" in
` + strings.ReplaceAll(cases.String(), "  *", "  *") + `esac
`
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func mcpEngine(t *testing.T, replies map[string]string) (*Engine, string) {
	bin, log := fakeMCPClaude(t, replies)
	cfg := DefaultConfig()
	cfg.Claude.Binary = bin
	cfg.Kling.Backend = KlingBackendMCP
	e := New(t.TempDir(), cfg)
	return e, log
}

func TestKlingMCPSubmitPollAndDownloadFlow(t *testing.T) {
	e, log := mcpEngine(t, map[string]string{
		"__text_to_video": `{"ok":true,"generation_id":"gen-123","status":"submitted","video_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`,
		"__query_tasks":   `{"ok":true,"generation_id":"gen-123","status":"succeeded","video_url":"https://cdn.example/v.mp4","credits":12,"credits_known":true,"error_kind":"none","error":""}`,
	})
	ctx := context.Background()
	c, style, err := e.newKlingBackend(ctx, e.Config.Kling)
	if err != nil {
		t.Fatal(err)
	}
	if style != KlingStyleMCP {
		t.Fatalf("style = %q", style)
	}
	spec, _ := kling.Lookup("kling-2.6")
	task, err := c.CreateTextToVideo(ctx, kling.CreateRequest{Model: spec, Prompt: "hello", Resolution: "1080p", Duration: 5, Audio: true, ExternalID: "x"})
	if err != nil || task.ID != "gen-123" {
		t.Fatalf("create: %v %+v", err, task)
	}
	got, err := c.GetTask(ctx, "gen-123")
	if err != nil || got.Status != kling.TaskSucceeded || got.VideoURL != "https://cdn.example/v.mp4" || !got.CostReported || got.Units != 12 {
		t.Fatalf("get: %v %+v", err, got)
	}

	b, _ := os.ReadFile(log)
	out := string(b)
	for _, want := range []string{
		"--allowedTools mcp__kling__text_to_video", // scoped to the one tool, server autodetected (claude.ai connector skipped)
		"--allowedTools mcp__kling__query_tasks",
		`"model":"kling-video-v2_6"`,        // registry id → MCP model id
		`{"name":"imageCount","value":"1"}`, // never multiply spend
		`{"name":"aspect_ratio","value":"9:16"}`,
		`{"name":"enable_audio","value":"true"}`,
		`{"name":"duration","value":"5"}`,
		"--model haiku",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("claude invocations missing %q:\n%s", want, out)
		}
	}
}

func TestKlingMCPErrorsMapToEngineErrorClasses(t *testing.T) {
	cases := []struct {
		kind             string
		fatal, retryable bool
	}{
		{"no_credits", true, false},
		{"auth", true, false},
		{"busy", false, true},
		{"blocked", false, false},
		{"other", false, false},
	}
	for _, tc := range cases {
		e, _ := mcpEngine(t, map[string]string{
			"__text_to_video": `{"ok":false,"generation_id":"","status":"failed","video_url":"","credits":0,"credits_known":false,"error_kind":"` + tc.kind + `","error":"boom"}`,
		})
		c, _, err := e.newKlingBackend(context.Background(), e.Config.Kling)
		if err != nil {
			t.Fatal(err)
		}
		spec, _ := kling.Lookup("kling-3.0-turbo")
		_, err = c.CreateTextToVideo(context.Background(), kling.CreateRequest{Model: spec, Prompt: "p", Resolution: "720p", Duration: 5})
		if err == nil {
			t.Fatalf("%s: expected error", tc.kind)
		}
		if kling.IsFatal(err) != tc.fatal || kling.IsRetryable(err) != tc.retryable {
			t.Errorf("%s: fatal=%v retryable=%v, err=%v", tc.kind, kling.IsFatal(err), kling.IsRetryable(err), err)
		}
	}
}

func TestKlingMCPSuccessWithoutURLKeepsPolling(t *testing.T) {
	e, _ := mcpEngine(t, map[string]string{
		"__query_tasks": `{"ok":true,"generation_id":"g","status":"succeeded","video_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`,
	})
	c, _, _ := e.newKlingBackend(context.Background(), e.Config.Kling)
	task, err := c.GetTask(context.Background(), "g")
	if err != nil || task.Status != kling.TaskProcessing {
		t.Fatalf("want processing, got %+v %v", task, err)
	}
}

func TestKlingMCPMissingServer(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\necho 'other: https://example.com/mcp (HTTP) - ✔ Connected'\n"), 0o755)
	cfg := DefaultConfig()
	cfg.Claude.Binary = bin
	cfg.Kling.Backend = KlingBackendMCP
	e := New(t.TempDir(), cfg)
	_, _, err := e.newKlingBackend(context.Background(), cfg.Kling)
	if err == nil || !strings.Contains(err.Error(), "claude mcp add") {
		t.Fatalf("want actionable add-server error, got %v", err)
	}
}

func TestKlingBackendConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Kling.Backend != KlingBackendAPI {
		t.Fatalf("default backend = %q", cfg.Kling.Backend)
	}
	cfg.Kling.Backend = "carrier-pigeon"
	if cfg.Validate() == nil {
		t.Fatal("bad backend should fail validation")
	}
	for _, id := range kling.ModelIDs() {
		if kling.Models[id].MCPName == "" {
			t.Errorf("%s has no MCP model name", id)
		}
	}
}

func TestUUIDV7(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	a, b := uuidV7(), uuidV7()
	if !re.MatchString(a) || a == b {
		t.Fatalf("bad uuidv7 %q / %q", a, b)
	}
}

// TestPipelineViaKlingMCP renders a whole video through the MCP backend: the
// fake `claude` answers Kling MCP relay runs itself and delegates everything
// else (distill, storyboard, auth) to the standard fake.
func TestPipelineViaKlingMCP(t *testing.T) {
	needFFmpeg(t)
	fk := newFakeKling(t, makeClipVideo(t))
	e := testEngine(t, fk)
	orig := e.Config.Claude.Binary

	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	submits := filepath.Join(dir, "submits.log")
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
  *__text_to_video*)
    n=$(( $(cat '` + counter + `' 2>/dev/null || echo 0) + 1 )); echo $n > '` + counter + `'
    echo "$IN" >> '` + submits + `'
    cat <<EOF
` + reply(`{"ok":true,"generation_id":"GEN-$n","status":"submitted","video_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *__query_tasks*)
    cat <<'EOF'
` + reply(`{"ok":true,"generation_id":"x","status":"succeeded","video_url":"`+fk.srv.URL+`/files/clip.mp4","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *) printf '%s' "$IN" | '` + orig + `' "$@";;
esac
`
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	e.Config.Claude.Binary = bin
	e.Config.Kling.Backend = KlingBackendMCP
	e.Config.Kling.APIKey, e.Config.Kling.APIStyle = "", KlingStyleAuto // prove the API creds are not needed
	e.Config.Kling.NoHero = true                                        // independent text-to-video clips; the hero flow has its own test

	ctx := context.Background()
	g, err := e.Create(ctx, GenerateRequest{Source: writeDoc(t), Name: "mcp tea"})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = e.Plan(ctx, g.Key, nil); err != nil {
		t.Fatal(err)
	}
	est, _ := e.Estimate(g.Key)
	if est.Backend != KlingBackendMCP || est.USD != 0 || est.Units != 0 {
		t.Fatalf("mcp estimate must not project API dollars: %+v", est)
	}
	g, err = e.Render(ctx, g.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusSucceeded || !fileExists(filepath.Join(g.Dir, FileVideo)) {
		t.Fatalf("status=%s err=%s", g.Status, g.Error)
	}
	if g.Kling.APIStyle != KlingStyleMCP || g.Kling.CostUSD != 0 || g.Kling.Units != 0 {
		t.Fatalf("kling metrics %+v", g.Kling)
	}
	b, _ := os.ReadFile(submits)
	if n := strings.Count(string(b), "taskTraceId"); n != 4 {
		t.Fatalf("want exactly one submit per scene (4), got %d", n)
	}
	for i, c := range g.Kling.Clips {
		if !strings.HasPrefix(c.TaskID, "GEN-") || c.Attempts != 1 || c.Status != ClipDownloaded {
			t.Errorf("clip %d: %+v", i, c)
		}
	}
}

func TestKlingMCPImageToVideoRequestShape(t *testing.T) {
	ok := `{"ok":true,"generation_id":"gen-9","status":"submitted","video_url":"","image_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`
	e, log := mcpEngine(t, map[string]string{"__image_to_video": ok, "__text_to_video": ok})
	ctx := context.Background()
	c, _, err := e.newKlingBackend(ctx, e.Config.Kling)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := kling.Lookup("kling-3.0")
	if _, err := c.CreateTextToVideo(ctx, kling.CreateRequest{Model: spec, Prompt: "p", Resolution: "1080p", Duration: 5, Audio: true, FirstImageURL: "https://cdn.example/hero.jpg"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CreateTextToVideo(ctx, kling.CreateRequest{Model: spec, Prompt: "p", Resolution: "1080p", Duration: 5, Audio: true}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	var runs []string
	for _, r := range strings.Split(string(b), "\n---\n") {
		if strings.TrimSpace(r) != "" {
			runs = append(runs, r)
		}
	}
	if len(runs) != 2 {
		t.Fatalf("want 2 relay runs, got %d:\n%s", len(runs), b)
	}
	i2v, t2v := runs[0], runs[1]
	for _, want := range []string{
		"--allowedTools mcp__kling__image_to_video",
		`"inputs":[{"inputType":"URL","name":"first_image","url":"https://cdn.example/hero.jpg"}]`,
		`{"name":"prefer_multi_shots","value":"false"}`, // 3.0 defaults to multi-shot, which cuts inside a clip
		`{"name":"imageCount","value":"1"}`,
	} {
		if !strings.Contains(i2v, want) {
			t.Errorf("image_to_video run missing %q:\n%s", want, i2v)
		}
	}
	if strings.Contains(i2v, "aspect_ratio") {
		t.Errorf("image_to_video has no aspect_ratio argument (the first frame decides):\n%s", i2v)
	}
	if !strings.Contains(t2v, "--allowedTools mcp__kling__text_to_video") || !strings.Contains(t2v, `{"name":"aspect_ratio","value":"9:16"}`) || strings.Contains(t2v, "first_image") {
		t.Errorf("text_to_video run wrong:\n%s", t2v)
	}
}

func TestHeroPromptUsesCastAndSet(t *testing.T) {
	sb := &Storyboard{Characters: []Character{{Label: "Bestie", Description: "early-20s presenter in a lavender hoodie"}}, Setting: "a cozy bedroom studio"}
	p := HeroPrompt(sb, Style{})
	for _, want := range []string{"9:16", "Bestie: early-20s presenter in a lavender hoodie.", "Setting: a cozy bedroom studio.", "mouth closed", "No text"} {
		if !strings.Contains(p, want) {
			t.Errorf("hero prompt missing %q: %s", want, p)
		}
	}
	if cp := HeroPrompt(sb, Style{ChromaKey: true}); !strings.Contains(cp, "chroma-key") || strings.Contains(cp, "bedroom") {
		t.Errorf("chroma-key styles must put the presenter on green: %s", cp)
	}
}

// TestPipelineViaKlingMCPWithHero: the reference image is generated once, gated
// on approval, and every clip is animated from it.
func TestPipelineViaKlingMCPWithHero(t *testing.T) {
	needFFmpeg(t)
	fk := newFakeKling(t, makeClipVideo(t))
	e := testEngine(t, fk)
	orig := e.Config.Claude.Binary

	heroSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte{0xFF, 0xD8, 0xFF, 0xE0, 0, 0x10, 'J', 'F', 'I', 'F'})
	}))
	t.Cleanup(heroSrv.Close)
	heroURL := heroSrv.URL + "/hero.jpg"

	dir := t.TempDir()
	vcount, icount := filepath.Join(dir, "v"), filepath.Join(dir, "i")
	submits, images := filepath.Join(dir, "submits.log"), filepath.Join(dir, "images.log")
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
  *__text_to_image*)
    n=$(( $(cat '` + icount + `' 2>/dev/null || echo 0) + 1 )); echo $n > '` + icount + `'
    echo "$IN" >> '` + images + `'
    cat <<EOF
` + reply(`{"ok":true,"generation_id":"IMG-$n","status":"submitted","video_url":"","image_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *__image_to_video*|*__text_to_video*)
    n=$(( $(cat '` + vcount + `' 2>/dev/null || echo 0) + 1 )); echo $n > '` + vcount + `'
    echo "$IN" >> '` + submits + `'
    cat <<EOF
` + reply(`{"ok":true,"generation_id":"GEN-$n","status":"submitted","video_url":"","image_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *__query_tasks*IMG-*)
    cat <<'EOF'
` + reply(`{"ok":true,"generation_id":"x","status":"succeeded","video_url":"","image_url":"`+heroURL+`","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *__query_tasks*)
    cat <<'EOF'
` + reply(`{"ok":true,"generation_id":"x","status":"succeeded","video_url":"`+fk.srv.URL+`/files/clip.mp4","image_url":"","credits":0,"credits_known":false,"error_kind":"none","error":""}`) + `
EOF
    ;;
  *) printf '%s' "$IN" | '` + orig + `' "$@";;
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

	ctx := context.Background()
	g, err := e.Create(ctx, GenerateRequest{Source: writeDoc(t), Name: "hero tea"})
	if err != nil {
		t.Fatal(err)
	}
	if g, err = e.Plan(ctx, g.Key, nil); err != nil {
		t.Fatal(err)
	}
	if !e.NeedsHero(g) {
		t.Fatal("mcp backend should use a hero image by default")
	}

	// 1. No clips may be paid for before the presenter is approved.
	if _, err := e.Render(ctx, g.Key, nil); !errors.Is(err, ErrHeroNotApproved) {
		t.Fatalf("render before approval: %v", err)
	}
	if read(submits) != "" {
		t.Fatal("a clip was submitted without an approved presenter image")
	}

	// 2. Generate the image once; asking again must not pay again.
	g, err = e.Hero(ctx, g.Key, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusAwaitingApproval || g.Hero == nil || g.Hero.TaskID != "IMG-1" || g.Hero.Approved || !fileExists(g.Hero.Path) || filepath.Ext(g.Hero.Path) != ".jpg" {
		t.Fatalf("hero state %+v status=%s", g.Hero, g.Status)
	}
	if _, err = e.Hero(ctx, g.Key, false, nil); err != nil || strings.Count(read(images), "taskTraceId") != 1 {
		t.Fatalf("re-running Hero must reuse the image: %v\n%s", err, read(images))
	}
	if !strings.Contains(read(images), `"kling-image-v3_0"`) || !strings.Contains(read(images), `{"name":"aspect_ratio","value":"9:16"}`) {
		t.Fatalf("image request wrong:\n%s", read(images))
	}

	// 3. Regenerating pays for a second image and resets approval.
	if g, err = e.Hero(ctx, g.Key, true, nil); err != nil || g.Hero.TaskID != "IMG-2" || g.Hero.Attempts != 2 {
		t.Fatalf("regenerate: %v %+v", err, g.Hero)
	}
	if _, err := e.Render(ctx, g.Key, nil); !errors.Is(err, ErrHeroNotApproved) {
		t.Fatalf("still unapproved: %v", err)
	}

	// 4. Approve, render: every clip is image_to_video from the hero URL.
	if _, err := e.ApproveHero(g.Key); err != nil {
		t.Fatal(err)
	}
	g, err = e.Render(ctx, g.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusSucceeded || !fileExists(filepath.Join(g.Dir, FileVideo)) {
		t.Fatalf("status=%s err=%s", g.Status, g.Error)
	}
	sub := read(submits)
	if n := strings.Count(sub, "taskTraceId"); n != 4 {
		t.Fatalf("want 4 clip submits, got %d", n)
	}
	if n := strings.Count(sub, `"name":"first_image"`); n != 4 || !strings.Contains(sub, heroURL) {
		t.Fatalf("every clip must start from the hero image (%d/4):\n%s", n, sub)
	}
	if strings.Contains(sub, "__text_to_video") || strings.Contains(sub, `"name":"aspect_ratio"`) {
		t.Fatalf("clips must be image_to_video without aspect_ratio:\n%s", sub)
	}
	if got := strings.Count(read(images), "taskTraceId"); got != 2 {
		t.Fatalf("exactly the two requested images should have been generated, got %d", got)
	}
}
