package engine

import (
	"context"
	"encoding/json"
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
