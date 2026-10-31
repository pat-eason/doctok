// Package ui serves doctok's local web UI. Like the CLI, it is a frontend
// over the engine package's public API.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pat-eason/doctok/engine"
)

//go:embed static
var static embed.FS

// Server is the local UI server.
type Server struct {
	home string
	hub  *hub

	mu      sync.Mutex
	running map[string]context.CancelFunc
	baseCtx context.Context
}

// New builds a server for the doctok home directory.
func New(ctx context.Context, home string) *Server {
	return &Server{home: home, hub: newHub(), running: map[string]context.CancelFunc{}, baseCtx: ctx}
}

// engine loads a fresh engine so config edits made in the CLI are picked up.
func (s *Server) engine() (*engine.Engine, error) {
	cfg, err := engine.LoadConfig(s.home)
	if err != nil {
		return nil, err
	}
	return engine.New(s.home, cfg), nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(static, "static")
	mux.Handle("GET /", http.FileServerFS(sub))
	mux.HandleFunc("GET /api/meta", s.meta)
	mux.HandleFunc("GET /api/generations", s.list)
	mux.HandleFunc("POST /api/generations", s.create)
	mux.HandleFunc("GET /api/generations/{key}", s.get)
	mux.HandleFunc("DELETE /api/generations/{key}", s.del)
	mux.HandleFunc("POST /api/generations/{key}/render", s.render)
	mux.HandleFunc("POST /api/generations/{key}/resume", s.resume)
	mux.HandleFunc("POST /api/generations/{key}/stop", s.stop)
	mux.HandleFunc("GET /api/generations/{key}/files/{file}", s.file)
	mux.HandleFunc("GET /api/generations/{key}/storyboard", s.storyboard)
	mux.HandleFunc("POST /api/upload", s.upload)
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/doctor", s.doctor)
	return guard(mux)
}

// guard blocks DNS-rebinding and cross-site requests: only localhost hosts,
// and mutating requests must come from our own origin.
func guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "localhost" && host != "127.0.0.1" && host != "::1" && host != "[::1]" {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if o := r.Header.Get("Origin"); o != "" && o != "http://"+r.Host {
				http.Error(w, "cross-origin request blocked", http.StatusForbidden)
				return
			}
			if r.Header.Get("X-Doctok") != "1" {
				http.Error(w, "missing X-Doctok header", http.StatusForbidden)
				return
			}
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) meta(w http.ResponseWriter, r *http.Request) {
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]any{
		"home":     s.home,
		"styles":   engine.StyleCatalog,
		"defaults": eng.Config.Video,
		"kling": map[string]any{
			"model": eng.Config.Kling.Model, "resolution": eng.Config.Kling.Resolution, "audio": eng.Config.Kling.Audio,
			"unit_price_usd": eng.Config.Kling.UnitPriceUSD, "has_credentials": eng.Config.Kling.APIKey != "" || (eng.Config.Kling.AccessKey != "" && eng.Config.Kling.SecretKey != ""),
		},
		"stages":       engine.Stages,
		"stage_labels": engine.StageLabels,
	})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	gens, err := engine.NewStore(s.home).List()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, gens)
}

func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	g, err := engine.NewStore(s.home).Get(r.PathValue("key"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	eng, _ := s.engine()
	var est *engine.Estimate
	if eng != nil {
		est, _ = eng.Estimate(g.Key)
	}
	writeJSON(w, 200, map[string]any{"generation": g, "estimate": est, "running_here": s.isRunning(g.Key)})
}

type createReq struct {
	Source        string `json:"source"`
	Name          string `json:"name"`
	Style         string `json:"style"`
	TargetSeconds int    `json:"target_seconds"`
	DryRun        bool   `json:"dry_run"`
	Captions      *bool  `json:"captions"`
	Audio         *bool  `json:"audio"`
	AutoRender    bool   `json:"auto_render"`
}

func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, 400, err)
		return
	}
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if !req.DryRun && req.AutoRender {
		k := eng.Config.Kling
		if k.APIKey == "" && (k.AccessKey == "" || k.SecretKey == "") {
			writeErr(w, 400, errors.New("no Kling credentials configured — run `doctok configure` or use a dry run"))
			return
		}
	}
	g, err := eng.Create(r.Context(), engine.GenerateRequest{
		Source: req.Source, Name: req.Name, Style: req.Style, TargetSeconds: req.TargetSeconds,
		DryRun: req.DryRun, Captions: req.Captions, Audio: req.Audio,
	})
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.hub.publish(engine.Event{Key: g.Key, Time: time.Now(), Kind: engine.EventCreated, Message: "created", Gen: g})
	s.start(g.Key, func(ctx context.Context, obs engine.Observer) error {
		if _, err := eng.Plan(ctx, g.Key, obs); err != nil {
			return err
		}
		if req.AutoRender || req.DryRun {
			_, err := eng.Render(ctx, g.Key, obs)
			return err
		}
		return nil
	})
	writeJSON(w, 202, g)
}

func (s *Server) render(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	g, err := eng.Store.Get(key)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	if !g.Options.DryRun {
		k := eng.Config.Kling
		if k.APIKey == "" && (k.AccessKey == "" || k.SecretKey == "") {
			writeErr(w, 400, errors.New("no Kling credentials configured"))
			return
		}
	}
	if !s.start(key, func(ctx context.Context, obs engine.Observer) error {
		_, err := eng.Render(ctx, key, obs)
		return err
	}) {
		writeErr(w, 409, errors.New("already running"))
		return
	}
	writeJSON(w, 202, map[string]string{"status": "rendering"})
}

func (s *Server) resume(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	g, err := eng.Store.Get(key)
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	scripted := engine.StageDoneFor(g, engine.StageScript)
	if !s.start(key, func(ctx context.Context, obs engine.Observer) error {
		if !scripted {
			if _, err := eng.Plan(ctx, key, obs); err != nil {
				return err
			}
			if !g.Options.DryRun {
				return nil // stop at approval
			}
		}
		_, err := eng.Render(ctx, key, obs)
		return err
	}) {
		writeErr(w, 409, errors.New("already running"))
		return
	}
	writeJSON(w, 202, map[string]string{"status": "resuming"})
}

func (s *Server) stop(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	s.mu.Lock()
	cancel, ok := s.running[key]
	s.mu.Unlock()
	if !ok {
		writeErr(w, 404, errors.New("not running in this UI"))
		return
	}
	cancel()
	writeJSON(w, 202, map[string]string{"status": "stopping"})
}

func (s *Server) del(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if s.isRunning(key) {
		writeErr(w, 409, errors.New("stop it first"))
		return
	}
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if err := eng.Delete(key); err != nil {
		writeErr(w, 400, err)
		return
	}
	s.hub.publish(engine.Event{Key: key, Time: time.Now(), Kind: "deleted"})
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

func (s *Server) isRunning(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.running[key]
	return ok
}

// start runs a job in the background; false if one is already running.
func (s *Server) start(key string, fn func(ctx context.Context, obs engine.Observer) error) bool {
	s.mu.Lock()
	if _, ok := s.running[key]; ok {
		s.mu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.running[key] = cancel
	s.mu.Unlock()
	go func() {
		defer func() {
			s.mu.Lock()
			delete(s.running, key)
			s.mu.Unlock()
			cancel()
			if g, err := engine.NewStore(s.home).Get(key); err == nil {
				s.hub.publish(engine.Event{Key: key, Time: time.Now(), Kind: "idle", Gen: g})
			}
		}()
		_ = fn(ctx, s.hub.publish)
	}()
	return true
}

var allowedFiles = map[string]string{
	engine.FileDoc: "text/markdown; charset=utf-8", engine.FileDistill: "text/markdown; charset=utf-8",
	engine.FileScript: "application/json", engine.FileVideo: "video/mp4",
}

func (s *Server) file(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("file")
	ct, ok := allowedFiles[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	g, err := engine.NewStore(s.home).Get(r.PathValue("key"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(g.Dir, name)
	f, err := os.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", ct)
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", g.Key+"-"+name))
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) storyboard(w http.ResponseWriter, r *http.Request) {
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	sb, err := eng.LoadStoryboard(r.PathValue("key"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, sb)
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		writeErr(w, 400, err)
		return
	}
	f, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	defer f.Close()
	dir := filepath.Join(s.home, "uploads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, 500, err)
		return
	}
	base := engine.Slugify(strings.TrimSuffix(filepath.Base(hdr.Filename), filepath.Ext(hdr.Filename)))
	if base == "" {
		base = "doc"
	}
	p := filepath.Join(dir, fmt.Sprintf("%s-%s.md", time.Now().Format("20060102-150405"), base))
	out, err := os.Create(p)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if _, err := io.Copy(out, io.LimitReader(f, 32<<20)); err != nil {
		out.Close()
		writeErr(w, 500, err)
		return
	}
	out.Close()
	writeJSON(w, 200, map[string]string{"path": p})
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	ch := s.hub.subscribe()
	defer s.hub.unsubscribe(ch)
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, ": hello\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	eng, err := s.engine()
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	opts := engine.DoctorOptions{Live: r.URL.Query().Get("live") == "1", SkipMCP: r.URL.Query().Get("skip_mcp") == "1"}
	send := func(kind string, v any) {
		b, _ := json.Marshal(v)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind, b)
		fl.Flush()
	}
	rep := eng.Doctor(r.Context(), opts, func(c engine.Check) { send("check", c) })
	send("report", rep)
}

// hub fans events out to SSE subscribers.
type hub struct {
	mu   sync.Mutex
	subs map[chan engine.Event]struct{}
}

func newHub() *hub { return &hub{subs: map[chan engine.Event]struct{}{}} }

func (h *hub) subscribe() chan engine.Event {
	ch := make(chan engine.Event, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan engine.Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (h *hub) publish(ev engine.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- ev:
		default: // slow client: drop (the UI re-syncs from /api/generations)
		}
	}
}

// Serve listens on addr until ctx is done.
func Serve(ctx context.Context, home, addr string, ready func(url string)) error {
	s := New(ctx, home)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	url := "http://" + strings.Replace(ln.Addr().String(), "127.0.0.1", "localhost", 1)
	if ready != nil {
		ready(url)
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		// Give running jobs a moment to record their interruption.
		time.Sleep(500 * time.Millisecond)
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
