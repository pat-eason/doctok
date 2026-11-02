package ui

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pat-eason/doctok/engine"
)

func TestGuardAndAPI(t *testing.T) {
	home := t.TempDir()
	h := New(context.Background(), home).Handler()
	do := func(method, target, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/api/generations", "evil.example.com", nil, ""); w.Code != 403 {
		t.Fatalf("foreign host allowed: %d", w.Code)
	}
	if w := do("GET", "/", "localhost:7878", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "doctok") {
		t.Fatalf("index: %d", w.Code)
	}
	if w := do("POST", "/api/generations", "localhost:7878", map[string]string{"Content-Type": "application/json"}, `{}`); w.Code != 403 {
		t.Fatalf("missing X-Doctok header allowed: %d", w.Code)
	}
	if w := do("POST", "/api/generations", "localhost:7878", map[string]string{"X-Doctok": "1", "Origin": "https://evil.example.com"}, `{}`); w.Code != 403 {
		t.Fatalf("cross-origin allowed: %d", w.Code)
	}
	// Valid request with a bad source → 400 from the engine, not a guard error.
	w := do("POST", "/api/generations", "localhost:7878", map[string]string{"X-Doctok": "1", "Origin": "http://localhost:7878"}, `{"source":"/nope.md","dry_run":true}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "can't read") {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w = do("GET", "/api/meta", "127.0.0.1:7878", nil, "")
	var meta map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil || len(meta["styles"].([]any)) < 5 {
		t.Fatalf("meta: %s", w.Body.String())
	}
	// File endpoint only serves the allow-listed artifacts.
	_ = os.WriteFile(filepath.Join(home, "secret.txt"), []byte("x"), 0o600)
	if w := do("GET", "/api/generations/x/files/..%2Fsecret.txt", "localhost", nil, ""); w.Code != 404 {
		t.Fatalf("file traversal: %d", w.Code)
	}
	_ = http.StatusOK
}

// The Kling MCP backend has no API key. The UI must not treat that as "no
// credentials", must explain the presenter-image gate instead of failing
// silently, and must serve the presenter image for review.
func TestMCPBackendRenderGateAndHeroImage(t *testing.T) {
	home := t.TempDir()
	cfg := engine.DefaultConfig()
	cfg.Kling.Backend = engine.KlingBackendMCP
	if err := engine.SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	doc := filepath.Join(t.TempDir(), "doc.md")
	_ = os.WriteFile(doc, []byte("# hi\n\nsome doc"), 0o644)
	eng := engine.New(home, cfg)
	g, err := eng.Create(context.Background(), engine.GenerateRequest{Source: doc, Name: "mcp ui"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if _, err := eng.Store.Update(g.Key, func(g *engine.Generation) error {
		g.Status = engine.StatusAwaitingApproval
		g.StageTimes = map[string]*engine.StageTiming{engine.StageScript: {FinishedAt: &now}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	h := New(context.Background(), home).Handler()
	do := func(method, target string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader("{}"))
		r.Host = "localhost:7878"
		r.Header.Set("X-Doctok", "1")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}

	var meta struct {
		Kling struct {
			HasCredentials bool   `json:"has_credentials"`
			Backend        string `json:"backend"`
		} `json:"kling"`
	}
	if err := json.Unmarshal(do("GET", "/api/meta").Body.Bytes(), &meta); err != nil || !meta.Kling.HasCredentials || meta.Kling.Backend != "mcp" {
		t.Fatalf("MCP backend must count as ready (no API key needed): %+v %v", meta, err)
	}

	// Render before the presenter is approved: a clear 409, never "no credentials".
	w := do("POST", "/api/generations/"+g.Key+"/render")
	if w.Code != 409 || !strings.Contains(w.Body.String(), "presenter") || strings.Contains(w.Body.String(), "credentials") {
		t.Fatalf("render gate: %d %s", w.Code, w.Body.String())
	}
	// Nothing to approve yet.
	if w := do("POST", "/api/generations/"+g.Key+"/hero-approve"); w.Code != 400 {
		t.Fatalf("approve without an image: %d %s", w.Code, w.Body.String())
	}
	// With an image on disk: it is served, approval works, and the gate opens.
	img := filepath.Join(g.Dir, engine.FileHeroBase+".jpg")
	_ = os.WriteFile(img, []byte{0xFF, 0xD8, 0xFF, 0xE0}, 0o644)
	if _, err := eng.Store.Update(g.Key, func(g *engine.Generation) error {
		g.Hero = &engine.Hero{TaskID: "IMG-1", URL: "https://cdn.example/h.jpg", Path: img, Attempts: 1}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := do("GET", "/api/generations/"+g.Key+"/files/HERO.jpg"); w.Code != 200 || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("hero image: %d %s", w.Code, w.Header().Get("Content-Type"))
	}
	if w := do("POST", "/api/generations/"+g.Key+"/hero-approve"); w.Code != 200 {
		t.Fatalf("approve: %d %s", w.Code, w.Body.String())
	}
	cur, _ := eng.Store.Get(g.Key)
	if cur.Hero == nil || !cur.Hero.Approved {
		t.Fatalf("hero not approved: %+v", cur.Hero)
	}
}

func TestUploadActorPhoto(t *testing.T) {
	home := t.TempDir()
	h := New(context.Background(), home).Handler()
	post := func(filename string, content []byte) *httptest.ResponseRecorder {
		var body bytes.Buffer
		mw := multipart.NewWriter(&body)
		fw, _ := mw.CreateFormFile("file", filename)
		_, _ = fw.Write(content)
		_ = mw.Close()
		r := httptest.NewRequest("POST", "/api/upload-actor", &body)
		r.Host = "localhost:7878"
		r.Header.Set("X-Doctok", "1")
		r.Header.Set("Content-Type", mw.FormDataContentType())
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	for _, bad := range []string{"notes.md", "me.gif", "me.jpg.exe", "../../etc/passwd"} {
		if w := post(bad, []byte("x")); w.Code != 400 {
			t.Errorf("%s accepted: %d %s", bad, w.Code, w.Body.String())
		}
	}
	w := post("Me.JPEG", []byte{0xFF, 0xD8, 0xFF, 0xE0})
	var out struct{ Path string }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if filepath.Dir(out.Path) != filepath.Join(home, "uploads") || filepath.Ext(out.Path) != ".jpg" {
		t.Fatalf("stored at %q", out.Path)
	}
	if st, err := os.Stat(out.Path); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("photo must be private (0600): %v %v", err, st)
	}
}

func TestMetaOffersShortLengthsWithEstimates(t *testing.T) {
	home := t.TempDir()
	cfg := engine.DefaultConfig()
	cfg.Kling.Backend = engine.KlingBackendMCP
	cfg.Kling.CreditsPerSecond = 10
	if err := engine.SaveConfig(home, cfg); err != nil {
		t.Fatal(err)
	}
	h := New(context.Background(), home).Handler()
	r := httptest.NewRequest("GET", "/api/meta", nil)
	r.Host = "localhost:7878"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var meta struct {
		Lengths []struct {
			Seconds int     `json:"seconds"`
			Label   string  `json:"label"`
			Credits float64 `json:"credits"`
		} `json:"lengths"`
		Kling struct {
			Model string `json:"model"`
		} `json:"kling"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil {
		t.Fatal(err)
	}
	got := map[int]float64{}
	for _, l := range meta.Lengths {
		got[l.Seconds] = l.Credits
	}
	for sec, credits := range map[int]float64{30: 300, 45: 450, 60: 600, 90: 900, 120: 1200, 210: 2100} {
		if got[sec] != credits {
			t.Errorf("%ds → %v credits, want %v (all: %v)", sec, got[sec], credits, got)
		}
	}
	if meta.Kling.Model != "kling-3.0" {
		t.Errorf("default model = %q, want kling-3.0", meta.Kling.Model)
	}

	// The page builds its options from the server, not from a hard-coded list.
	r = httptest.NewRequest("GET", "/", nil)
	r.Host = "localhost:7878"
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	page := w.Body.String()
	if strings.Contains(page, `<option value="180">`) || !strings.Contains(page, "buildLengths") || !strings.Contains(page, `id="lengthHint"`) {
		t.Error("length options must be built from /api/meta")
	}
}

// The page re-renders every second (live timers) and on every server event.
// Rebuilding the DOM with innerHTML on each pass replaced the buttons under the
// cursor, so a click whose mousedown and mouseup straddled a rebuild was dropped
// ("clicking the button does nothing"). Rendering must patch in place instead.
func TestPageRendersInPlaceSoClicksAreNotDropped(t *testing.T) {
	h := New(context.Background(), t.TempDir()).Handler()
	r := httptest.NewRequest("GET", "/", nil)
	r.Host = "localhost:7878"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	page := w.Body.String()
	for _, want := range []string{"function morph(", "function patchChildren(", "morph(box, html)", "morph($('#gens'),"} {
		if !strings.Contains(page, want) {
			t.Errorf("page must patch the DOM in place; missing %q", want)
		}
	}
	for _, bad := range []string{"box.innerHTML = html", "$('#gens').innerHTML = list.map"} {
		if strings.Contains(page, bad) {
			t.Errorf("page must not rebuild the detail panel or list wholesale (found %q)", bad)
		}
	}
}
