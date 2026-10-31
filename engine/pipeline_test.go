package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pat-eason/doctok/engine/kling"
	"github.com/pat-eason/doctok/engine/media"
)

// testStoryboard is what the fake Claude returns for the storyboard call.
func testStoryboard() Storyboard {
	return Storyboard{
		Title: "Token buckets are a whole vibe", Hook: "your retries are double-charging people",
		Characters: []Character{{Label: "Bestie", Description: "early-20s presenter in a lavender hoodie", Voice: "gossipy"}},
		Setting:    "cozy bedroom",
		Scenes: []Scene{
			{Beat: "hook", Seconds: 10, Action: "leans into lens", Camera: "selfie", Cut: "flash", TopText: "the API lore", Lines: []Line{{Speaker: "Bestie", Tone: "whisper", Text: "ok so nobody is talking about token buckets"}}},
			{Beat: "concept", Seconds: 5, Action: "counts on fingers", Camera: "selfie", Cut: "punch-in", Lines: []Line{{Speaker: "Bestie", Tone: "fast", Text: "a hundred tokens, ten per second"}}, DocQuote: "capacity of 100 tokens"},
			{Beat: "gotcha", Seconds: 10, Action: "gasps", Camera: "selfie", Cut: "hard-cut", Lines: []Line{{Speaker: "Nobody", Tone: "shocked", Text: "new idempotency key per retry? cooked."}}},
			{Beat: "outro", Seconds: 5, Action: "shimmies", Dance: "shoulder shimmy", Camera: "selfie", Cut: "bogus", Lines: nil},
		},
	}
}

// writeFakeClaude creates a `claude` stand-in that answers distill, storyboard
// and auth calls like Claude Code's JSON output.
func writeFakeClaude(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	sb, _ := json.Marshal(testStoryboard())
	sbEnv, _ := json.Marshal(map[string]any{
		"type": "result", "subtype": "success", "result": string(sb), "structured_output": json.RawMessage(sb),
		"total_cost_usd": 0.02, "duration_ms": 900,
		"modelUsage": map[string]any{"claude-sonnet-5-5": map[string]any{"inputTokens": 100, "outputTokens": 200, "costUSD": 0.02}},
	})
	distill := "# Token Buckets\\n\\n## TL;DR\\nBuckets refill."
	distEnv := `{"type":"result","subtype":"success","result":"` + distill + `","total_cost_usd":0.01,"duration_ms":500,"modelUsage":{"claude-sonnet-5-5":{"inputTokens":50,"outputTokens":60,"costUSD":0.01}}}`
	script := `#!/bin/sh
case "$*" in
  *"auth status"*) echo '{"loggedIn":true,"authMethod":"claude.ai","subscriptionType":"max","email":"t@example.com"}'; exit 0;;
  *--version*) echo "9.9.9 (Claude Code)"; exit 0;;
esac
cat > /dev/null
case "$*" in
  *--json-schema*) cat <<'EOF'
` + string(sbEnv) + `
EOF
;;
  *) cat <<'EOF'
` + distEnv + `
EOF
;;
esac
`
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

type fakeKling struct {
	srv      *httptest.Server
	video    string
	mu       sync.Mutex
	creates  int32
	polls    map[string]int
	failOnce map[int]bool // scene index (from external id) → fail first attempt
	failed   map[int]bool
	onPoll   func()
	prompts  []string
}

func newFakeKling(t *testing.T, video string) *fakeKling {
	f := &fakeKling{video: video, polls: map[string]int{}, failOnce: map[int]bool{}, failed: map[int]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeKling) handle(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/text-to-video/"):
		var body struct {
			Prompt  string `json:"prompt"`
			Options struct {
				ExternalTaskID string `json:"external_task_id"`
			} `json:"options"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := atomic.AddInt32(&f.creates, 1)
		var scene int
		parts := strings.Split(body.Options.ExternalTaskID, "-")
		for i, p := range parts {
			if len(p) == 3 && i+1 < len(parts) {
				fmt.Sscanf(p, "%d", &scene)
			}
		}
		f.mu.Lock()
		f.prompts = append(f.prompts, body.Prompt)
		id := fmt.Sprintf("task-%d-s%d", n, scene)
		f.mu.Unlock()
		fmt.Fprintf(w, `{"code":0,"data":{"id":%q,"status":"submitted"}}`, id)
	case r.Method == "GET" && r.URL.Path == "/tasks":
		if f.onPoll != nil {
			f.onPoll()
		}
		id := r.URL.Query().Get("task_ids")
		var scene int
		fmt.Sscanf(id[strings.LastIndex(id, "-s")+2:], "%d", &scene)
		f.mu.Lock()
		f.polls[id]++
		n := f.polls[id]
		shouldFail := f.failOnce[scene] && !f.failed[scene]
		if shouldFail && n >= 2 {
			f.failed[scene] = true
		}
		f.mu.Unlock()
		switch {
		case n < 2:
			fmt.Fprintf(w, `{"code":0,"data":[{"id":%q,"status":"processing"}]}`, id)
		case shouldFail:
			fmt.Fprintf(w, `{"code":0,"data":[{"id":%q,"status":"failed","message":"model overloaded"}]}`, id)
		default:
			fmt.Fprintf(w, `{"code":0,"data":[{"id":%q,"status":"succeeded","outputs":[{"type":"video","url":"%s/files/clip.mp4","duration":"10"}],"billing":[{"charge_type":"unit","amount":"7.5"}]}]}`, id, f.srv.URL)
		}
	case r.URL.Path == "/files/clip.mp4":
		http.ServeFile(w, r, f.video)
	case r.URL.Path == "/account/costs":
		io.WriteString(w, `{"code":0,"data":{"code":0,"resource_pack_subscribe_infos":[{"resource_pack_name":"p","total_quantity":1000,"remaining_quantity":900,"status":"online"}]}}`)
	default:
		w.WriteHeader(404)
		io.WriteString(w, `{"code":1202,"message":"not found"}`)
	}
}

func needFFmpeg(t *testing.T) media.Tools {
	t.Helper()
	tools, err := media.FindTools()
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	return tools
}

func makeClipVideo(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "clip.mp4")
	cmd := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "testsrc2=s=540x960:r=15:d=10", "-f", "lavfi", "-i", "sine=frequency=330:duration=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", p)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	return p
}

func testEngine(t *testing.T, fk *fakeKling) *Engine {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Claude.Binary = writeFakeClaude(t)
	cfg.Claude.Mode = ClaudeModeAuto
	cfg.Kling.APIKey = "test-key"
	cfg.Kling.APIStyle = KlingStyleStandard
	if fk != nil {
		cfg.Kling.BaseURL = fk.srv.URL
	}
	cfg.Kling.Concurrency = 2
	cfg.Video = VideoConfig{Style: "green-screen-yapper", TargetSeconds: 30, MinSeconds: 20, MaxSeconds: 40, Captions: true, Width: 540, Height: 960, FPS: 15}
	e := New(t.TempDir(), cfg)
	e.PollInterval = 20 * time.Millisecond
	e.RetryBase = 10 * time.Millisecond
	return e
}

func writeDoc(t *testing.T) string {
	p := filepath.Join(t.TempDir(), "rate-limits.md")
	if err := os.WriteFile(p, []byte("# Rate limits\n\nBuckets hold 100 tokens and refill at 10/s.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPipelineEndToEnd(t *testing.T) {
	tools := needFFmpeg(t)
	fk := newFakeKling(t, makeClipVideo(t))
	fk.failOnce[1] = true // scene 2 fails once, must be retried
	e := testEngine(t, fk)
	ctx := context.Background()

	g, err := e.Create(ctx, GenerateRequest{Source: writeDoc(t), Name: "Rate Limit Tea!"})
	if err != nil {
		t.Fatal(err)
	}
	if g.Key != "rate-limit-tea" {
		t.Fatalf("key=%q", g.Key)
	}
	var events []Event
	var mu sync.Mutex
	obs := func(ev Event) { mu.Lock(); events = append(events, ev); mu.Unlock() }

	g, err = e.Plan(ctx, g.Key, obs)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusAwaitingApproval {
		t.Fatalf("status after plan = %s", g.Status)
	}
	est, err := e.Estimate(g.Key)
	if err != nil || !est.FromStoryboard || est.Scenes != 4 || est.Seconds != 30 || est.Units != 30 {
		t.Fatalf("estimate %+v %v", est, err)
	}
	if atomic.LoadInt32(&fk.creates) != 0 {
		t.Fatal("plan must not submit Kling tasks")
	}

	g, err = e.Render(ctx, g.Key, obs)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusSucceeded || g.Progress != 1 {
		t.Fatalf("status=%s progress=%v err=%s", g.Status, g.Progress, g.Error)
	}
	// Artifacts.
	for _, f := range []string{FileDoc, FileDistill, FileScript, FileVideo} {
		if !fileExists(filepath.Join(g.Dir, f)) {
			t.Errorf("missing %s", f)
		}
	}
	p, err := tools.ProbeFile(ctx, filepath.Join(g.Dir, FileVideo))
	if err != nil {
		t.Fatal(err)
	}
	if p.Width != 540 || p.Height != 960 || !p.HasAudio || p.Duration < 29 || p.Duration > 31.5 {
		t.Fatalf("video probe %+v", p)
	}
	// Metrics persisted in state.json.
	st, _ := e.Store.Load()
	sg := st.Generations[g.Key]
	if sg.Claude.Billing != "subscription" || sg.Claude.CostUSD < 0.0299 || len(sg.Claude.Calls) != 2 || sg.Claude.InputTokens != 150 {
		t.Fatalf("claude metrics %+v", sg.Claude)
	}
	if !sg.Kling.UnitsReported || sg.Kling.Units != 30 || sg.Kling.CostUSD != 4.2 || sg.Kling.APIStyle != "standard" {
		t.Fatalf("kling metrics %+v", sg.Kling)
	}
	if sg.Kling.Clips[1].Attempts != 2 {
		t.Fatalf("scene 2 should have been retried, attempts=%d", sg.Kling.Clips[1].Attempts)
	}
	if got := atomic.LoadInt32(&fk.creates); got != 5 {
		t.Fatalf("creates=%d want 5 (4 scenes + 1 retry)", got)
	}
	if sg.StartedAt == nil || sg.FinishedAt == nil || sg.DurationSeconds <= 0 || sg.ActiveSeconds <= 0 {
		t.Fatalf("timings %+v", sg)
	}
	for _, s := range Stages {
		if !StageDoneFor(sg, s) {
			t.Errorf("stage %s not finished", s)
		}
	}
	// Normalization: unknown speaker → first character; bogus cut → hard-cut.
	sb, _ := e.LoadStoryboard(g.Key)
	if sb.Scenes[2].Lines[0].Speaker != "Bestie" || sb.Scenes[3].Cut != "hard-cut" {
		t.Fatalf("normalize: %+v", sb.Scenes)
	}
	// Green-screen style forces the chroma backdrop in prompts.
	found := false
	for _, p := range fk.prompts {
		if !strings.Contains(p, "chroma-key") {
			t.Fatalf("green-screen prompt without chroma backdrop: %s", p)
		}
		if strings.Contains(p, `[Bestie, whisper] looks into the camera and says: "ok so nobody`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("hook dialogue missing from prompts: %v", fk.prompts)
	}
	// Event stream covered every stage and the approval gate.
	kinds := map[string]bool{}
	for _, ev := range events {
		kinds[ev.Kind] = true
	}
	for _, k := range []string{EventStage, EventApproval, EventClip, EventDone} {
		if !kinds[k] {
			t.Errorf("no %s event", k)
		}
	}
	// Re-running a finished generation is a no-op.
	before := atomic.LoadInt32(&fk.creates)
	if _, err := e.Render(ctx, g.Key, nil); err != nil || atomic.LoadInt32(&fk.creates) != before {
		t.Fatal("render of a finished generation should be a no-op")
	}
}

func TestRenderInterruptAndResume(t *testing.T) {
	needFFmpeg(t)
	fk := newFakeKling(t, makeClipVideo(t))
	e := testEngine(t, fk)
	e.Config.Video.Style = "bestie-explainer"
	ctx := context.Background()
	g, err := e.Create(ctx, GenerateRequest{Source: writeDoc(t), Name: "resume me"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Plan(ctx, g.Key, nil); err != nil {
		t.Fatal(err)
	}
	cctx, cancel := context.WithCancel(ctx)
	var once sync.Once
	fk.onPoll = func() { once.Do(cancel) } // ctrl+c as soon as Kling is polled
	_, err = e.Render(cctx, g.Key, nil)
	if err == nil {
		t.Fatal("expected interruption")
	}
	cur, _ := e.Store.Get(g.Key)
	if cur.Status != StatusInterrupted {
		t.Fatalf("status=%s", cur.Status)
	}
	submitted := 0
	for _, c := range cur.Kling.Clips {
		if c.TaskID != "" {
			submitted++
		}
	}
	if submitted == 0 {
		t.Fatal("expected in-flight task ids to be persisted")
	}
	creates := atomic.LoadInt32(&fk.creates)
	fk.onPoll = nil
	g, err = e.Render(ctx, g.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusSucceeded {
		t.Fatalf("status=%s", g.Status)
	}
	if got := atomic.LoadInt32(&fk.creates) - creates; int(got) != 4-submitted {
		t.Fatalf("resume created %d new tasks, want %d (in-flight tasks must be reused)", got, 4-submitted)
	}
}

func TestDryRunNeedsNoKling(t *testing.T) {
	needFFmpeg(t)
	e := testEngine(t, nil)
	e.Config.Kling.APIKey = ""
	e.NewKling = func(KlingConfig) (KlingAPI, string, error) {
		t.Fatal("dry run must not build a Kling client")
		return nil, "", nil
	}
	ctx := context.Background()
	g, err := e.Create(ctx, GenerateRequest{Source: writeDoc(t), DryRun: true, Style: "throwback-2016"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.Name, "rate-limits") {
		t.Fatalf("auto name %q should include the file name", g.Name)
	}
	g, err = e.Run(ctx, g.Key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if g.Status != StatusSucceeded || g.Kling.Units != 0 {
		t.Fatalf("%+v", g)
	}
}

func TestFatalKlingErrorStopsRender(t *testing.T) {
	needFFmpeg(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
		io.WriteString(w, `{"code":1102,"message":"Resource pack exhausted"}`)
	}))
	defer srv.Close()
	e := testEngine(t, nil)
	e.Config.Kling.BaseURL = srv.URL
	ctx := context.Background()
	g, _ := e.Create(ctx, GenerateRequest{Source: writeDoc(t)})
	if _, err := e.Plan(ctx, g.Key, nil); err != nil {
		t.Fatal(err)
	}
	_, err := e.Render(ctx, g.Key, nil)
	if err == nil || !kling.IsFatal(err) {
		t.Fatalf("want fatal kling error, got %v", err)
	}
	cur, _ := e.Store.Get(g.Key)
	if cur.Status != StatusFailed || !strings.Contains(cur.Error, "exhausted") {
		t.Fatalf("status=%s err=%s", cur.Status, cur.Error)
	}
}
