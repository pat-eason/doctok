// Package engine is doctok's generation engine: it ingests a document (local
// Markdown or Notion), distills and scripts it with a local Claude Code install,
// renders each scene with Kling, and assembles a 1080x1920 TikTok-style video.
//
// The engine has no CLI or UI code; frontends drive it through Create / Plan /
// Render / Run and observe progress through Events.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pat-eason/doctok/engine/claude"
	"github.com/pat-eason/doctok/engine/kling"
	"github.com/pat-eason/doctok/engine/media"
)

// Event kinds.
const (
	EventCreated  = "created"
	EventStage    = "stage"
	EventLog      = "log"
	EventProgress = "progress"
	EventClip     = "clip"
	EventApproval = "approval" // storyboard ready; waiting for render approval
	EventDone     = "done"
	EventError    = "error"
)

// Event is emitted to observers; Gen is a snapshot after the change.
type Event struct {
	Key     string      `json:"key"`
	Time    time.Time   `json:"time"`
	Kind    string      `json:"kind"`
	Stage   string      `json:"stage,omitempty"`
	Message string      `json:"message,omitempty"`
	Gen     *Generation `json:"generation,omitempty"`
}

// Observer receives events. It must not block for long.
type Observer func(Event)

// KlingAPI is the subset of the Kling client the engine uses (mockable).
type KlingAPI interface {
	CreateTextToVideo(ctx context.Context, r kling.CreateRequest) (*kling.Task, error)
	GetTask(ctx context.Context, id string) (*kling.Task, error)
	Download(ctx context.Context, url, path string) (int64, error)
	AccountCosts(ctx context.Context) (*kling.Account, error)
}

// Engine runs generations.
type Engine struct {
	Home   string
	Config Config
	Store  *Store

	// NewKling builds the Kling client; override in tests.
	NewKling func(cfg KlingConfig) (KlingAPI, string, error)
	// Now is the clock; override in tests.
	Now func() time.Time
	// MaxClipAttempts bounds Kling retries per clip.
	MaxClipAttempts int
	// PollInterval overrides kling.poll_seconds (tests).
	PollInterval time.Duration
	// RetryBase is the first backoff after a retryable Kling error.
	RetryBase time.Duration

	billingOnce sync.Once
	billing     string
	billingErr  error
}

// Open loads ~/.doctok (or $DOCTOK_HOME) and its config.
func Open() (*Engine, error) {
	home, err := HomeDir()
	if err != nil {
		return nil, err
	}
	cfg, err := LoadConfig(home)
	if err != nil {
		return nil, err
	}
	return New(home, cfg), nil
}

// New builds an engine over an explicit home and config.
func New(home string, cfg Config) *Engine {
	return &Engine{
		Home:            home,
		Config:          cfg,
		Store:           NewStore(home),
		NewKling:        defaultKling,
		Now:             time.Now,
		MaxClipAttempts: 3,
		RetryBase:       5 * time.Second,
	}
}

func defaultKling(c KlingConfig) (KlingAPI, string, error) {
	cl, err := kling.New(c.BaseURL, c.APIStyle, c.APIKey, c.AccessKey, c.SecretKey)
	if err != nil {
		return nil, "", err
	}
	return cl, cl.Style, nil
}

// GenerateRequest describes a new generation.
type GenerateRequest struct {
	Source        string
	Name          string
	Style         string
	DryRun        bool
	TargetSeconds int
	KlingModel    string
	Resolution    string
	Audio         *bool
	Captions      *bool
	Music         string
}

// Estimate is the projected cost of rendering.
type Estimate struct {
	Scenes         int     `json:"scenes"`
	Seconds        int     `json:"seconds"`
	Model          string  `json:"model"`
	Resolution     string  `json:"resolution"`
	Audio          bool    `json:"audio"`
	Units          float64 `json:"units"`
	USD            float64 `json:"usd"`
	UnitPriceUSD   float64 `json:"unit_price_usd"`
	DryRun         bool    `json:"dry_run"`
	FromStoryboard bool    `json:"from_storyboard"`
}

// Create validates a request and registers a pending generation.
func (e *Engine) Create(ctx context.Context, req GenerateRequest) (*Generation, error) {
	if err := e.Config.Validate(); err != nil {
		return nil, err
	}
	src, err := ClassifySource(req.Source)
	if err != nil {
		return nil, err
	}
	styleID := req.Style
	if styleID == "" {
		styleID = e.Config.Video.Style
	}
	if _, err := LookupStyle(styleID); err != nil {
		return nil, err
	}
	opts := GenerationOptions{
		DryRun:        req.DryRun,
		TargetSeconds: req.TargetSeconds,
		Captions:      e.Config.Video.Captions,
		KlingModel:    req.KlingModel,
		Resolution:    req.Resolution,
		Audio:         e.Config.Kling.Audio,
		Music:         req.Music,
	}
	if opts.TargetSeconds == 0 {
		opts.TargetSeconds = e.Config.Video.TargetSeconds
	}
	if opts.TargetSeconds < e.Config.Video.MinSeconds || opts.TargetSeconds > e.Config.Video.MaxSeconds {
		return nil, fmt.Errorf("target length %ds must be between %ds and %ds", opts.TargetSeconds, e.Config.Video.MinSeconds, e.Config.Video.MaxSeconds)
	}
	if opts.KlingModel == "" {
		opts.KlingModel = e.Config.Kling.Model
	}
	if opts.Resolution == "" {
		opts.Resolution = e.Config.Kling.Resolution
	}
	if req.Audio != nil {
		opts.Audio = *req.Audio
	}
	if req.Captions != nil {
		opts.Captions = *req.Captions
	}
	if opts.Music != "" {
		m, err := filepath.Abs(expandHome(opts.Music))
		if err != nil {
			return nil, err
		}
		if _, err := os.Stat(m); err != nil {
			return nil, fmt.Errorf("music file: %w", err)
		}
		opts.Music = m
	}
	spec, ok := kling.Lookup(opts.KlingModel)
	if !ok {
		return nil, fmt.Errorf("unknown Kling model %q (known: %s)", opts.KlingModel, strings.Join(kling.ModelIDs(), ", "))
	}
	opts.KlingModel = spec.ID
	opts.Audio = spec.EffectiveAudio(opts.Audio)
	if err := spec.Validate(opts.Resolution, opts.Audio); err != nil {
		return nil, err
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		hint := ""
		if src.Kind == SourceFile {
			hint = strings.TrimSuffix(filepath.Base(src.Ref), filepath.Ext(src.Ref))
		}
		name = AutoName(hint)
	}
	base := Slugify(name)
	if base == "" {
		base = Slugify(AutoName(""))
	}
	g := &Generation{
		Name:      name,
		CreatedAt: e.Now().UTC(),
		Source:    src,
		Style:     styleID,
		Status:    StatusPending,
		Stage:     StageIngest,
		Options:   opts,
		Kling: KlingMetrics{
			Model: spec.ID, Resolution: opts.Resolution, Audio: opts.Audio,
			EstimatedUnits: spec.UnitsFor(opts.TargetSeconds, opts.Resolution, opts.Audio),
		},
		StageTimes: map[string]*StageTiming{},
		Files:      map[string]string{},
	}
	if _, err := e.Store.ReserveKey(base, g); err != nil {
		return nil, err
	}
	for _, d := range []string{g.Dir, filepath.Join(g.Dir, DirClips), filepath.Join(g.Dir, DirWork)} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return g, nil
}

// PreEstimate projects cost before a storyboard exists.
func (e *Engine) PreEstimate(g *Generation) Estimate {
	spec, _ := kling.Lookup(g.Options.KlingModel)
	units := spec.UnitsFor(g.Options.TargetSeconds, g.Options.Resolution, g.Options.Audio)
	est := Estimate{
		Seconds: g.Options.TargetSeconds, Model: spec.ID, Resolution: g.Options.Resolution, Audio: g.Options.Audio,
		Units: units, UnitPriceUSD: e.Config.Kling.UnitPriceUSD, USD: units * e.Config.Kling.UnitPriceUSD, DryRun: g.Options.DryRun,
	}
	if spec.MaxDuration() > 0 {
		est.Scenes = (g.Options.TargetSeconds + min(spec.MaxDuration(), 10) - 1) / min(spec.MaxDuration(), 10)
	}
	if g.Options.DryRun {
		est.Units, est.USD = 0, 0
	}
	return est
}

// Estimate returns the storyboard-based projection.
func (e *Engine) Estimate(key string) (*Estimate, error) {
	g, err := e.Store.Get(key)
	if err != nil {
		return nil, err
	}
	sb, err := e.loadStoryboard(g)
	if err != nil {
		est := e.PreEstimate(g)
		return &est, nil
	}
	spec, _ := kling.Lookup(g.Options.KlingModel)
	est := &Estimate{
		Scenes: len(sb.Scenes), Seconds: sb.TotalSeconds(), Model: spec.ID, Resolution: g.Options.Resolution,
		Audio: g.Options.Audio, UnitPriceUSD: e.Config.Kling.UnitPriceUSD, DryRun: g.Options.DryRun, FromStoryboard: true,
	}
	for _, s := range sb.Scenes {
		est.Units += spec.UnitsFor(s.Seconds, g.Options.Resolution, g.Options.Audio)
	}
	est.USD = est.Units * e.Config.Kling.UnitPriceUSD
	if g.Options.DryRun {
		est.Units, est.USD = 0, 0
	}
	return est, nil
}

// Run plans and renders in one go (no approval pause).
func (e *Engine) Run(ctx context.Context, key string, obs Observer) (*Generation, error) {
	if _, err := e.Plan(ctx, key, obs); err != nil {
		return nil, err
	}
	return e.Render(ctx, key, obs)
}

// ---- internals -------------------------------------------------------------

var stageWeights = map[string]float64{
	StageIngest: 0.03, StageDistill: 0.12, StageScript: 0.10, StageRender: 0.65, StageAssemble: 0.10,
}

func stageBase(stage string) float64 {
	t := 0.0
	for _, s := range Stages {
		if s == stage {
			return t
		}
		t += stageWeights[s]
	}
	return t
}

func (e *Engine) emit(obs Observer, kind, stage, msg string, g *Generation) {
	if obs == nil {
		return
	}
	var key string
	if g != nil {
		key = g.Key
	}
	obs(Event{Key: key, Time: e.Now(), Kind: kind, Stage: stage, Message: msg, Gen: g})
}

func (e *Engine) logf(obs Observer, key, stage, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	g, _ := e.Store.Update(key, func(g *Generation) error { g.Message = msg; return nil })
	e.emit(obs, EventLog, stage, msg, g)
}

// claim marks the generation as running in this process.
func (e *Engine) claim(key string) (*Generation, error) {
	host, _ := os.Hostname()
	return e.Store.Update(key, func(g *Generation) error {
		if g.Status == StatusRunning && g.OwnerPID != os.Getpid() && pidAlive(g.OwnerPID) && (g.OwnerHost == "" || g.OwnerHost == host) {
			return fmt.Errorf("generation %s is already running (pid %d)", key, g.OwnerPID)
		}
		now := e.Now().UTC()
		if g.StartedAt == nil {
			g.StartedAt = &now
		}
		g.FinishedAt = nil
		g.Status = StatusRunning
		g.Error = ""
		g.OwnerPID = os.Getpid()
		g.OwnerHost = host
		if g.StageTimes == nil {
			g.StageTimes = map[string]*StageTiming{}
		}
		if g.Files == nil {
			g.Files = map[string]string{}
		}
		return nil
	})
}

func (e *Engine) stageDone(g *Generation, stage string) bool {
	st := g.StageTimes[stage]
	return st != nil && st.FinishedAt != nil
}

func (e *Engine) beginStage(key, stage string, obs Observer) (*Generation, error) {
	g, err := e.Store.Update(key, func(g *Generation) error {
		now := e.Now().UTC()
		g.Stage = stage
		g.Status = StatusRunning
		g.Progress = stageBase(stage)
		g.Message = StageLabels[stage] + "…"
		st := g.StageTimes[stage]
		if st == nil {
			st = &StageTiming{}
			g.StageTimes[stage] = st
		}
		st.StartedAt = &now
		st.FinishedAt = nil
		return nil
	})
	if err == nil {
		e.emit(obs, EventStage, stage, StageLabels[stage], g)
	}
	return g, err
}

func (e *Engine) endStage(key, stage string, obs Observer) (*Generation, error) {
	g, err := e.Store.Update(key, func(g *Generation) error {
		now := e.Now().UTC()
		st := g.StageTimes[stage]
		if st == nil {
			st = &StageTiming{StartedAt: &now}
			g.StageTimes[stage] = st
		}
		st.FinishedAt = &now
		if st.StartedAt != nil {
			secs := now.Sub(*st.StartedAt).Seconds()
			st.Seconds += secs
			g.ActiveSeconds += secs
		}
		g.Progress = stageBase(stage) + stageWeights[stage]
		return nil
	})
	if err == nil {
		e.emit(obs, EventStage, stage, StageLabels[stage]+" ✓", g)
	}
	return g, err
}

// fail records an error (or an interruption if ctx was canceled).
func (e *Engine) fail(ctx context.Context, key string, obs Observer, cause error) error {
	interrupted := ctx.Err() != nil || errors.Is(cause, context.Canceled)
	g, _ := e.Store.Update(key, func(g *Generation) error {
		now := e.Now().UTC()
		if st := g.StageTimes[g.Stage]; st != nil && st.StartedAt != nil && st.FinishedAt == nil {
			secs := now.Sub(*st.StartedAt).Seconds()
			st.Seconds += secs
			g.ActiveSeconds += secs
			st.StartedAt = nil
		}
		g.OwnerPID = 0
		if interrupted {
			g.Status = StatusInterrupted
			g.Message = "interrupted — `doctok resume " + g.Key + "` to pick up where it left off"
			return nil
		}
		g.Status = StatusFailed
		g.Error = cause.Error()
		g.FinishedAt = &now
		if g.StartedAt != nil {
			g.DurationSeconds = now.Sub(*g.StartedAt).Seconds()
		}
		return nil
	})
	if interrupted {
		e.emit(obs, EventError, "", "interrupted", g)
		return context.Canceled
	}
	e.emit(obs, EventError, "", cause.Error(), g)
	return cause
}

// resolveBilling decides subscription vs API key for Claude.
func (e *Engine) resolveBilling(ctx context.Context) (string, error) {
	e.billingOnce.Do(func() {
		c := e.Config.Claude
		switch c.Mode {
		case ClaudeModeAPI:
			if c.APIKey == "" {
				e.billingErr = errors.New("claude.mode=api but no Anthropic API key configured (ANTHROPIC_API_KEY or `doctok configure`)")
				return
			}
			e.billing = claude.BillingAPI
		case ClaudeModeSubscription:
			e.billing = claude.BillingSubscription
		default:
			r := &claude.Runner{Binary: c.Binary}
			if st, err := r.Auth(ctx); err == nil && st.HasSubscription() {
				e.billing = claude.BillingSubscription
				return
			}
			if c.APIKey != "" {
				e.billing = claude.BillingAPI
				return
			}
			e.billingErr = errors.New("no Claude access: log in with `claude auth login` (subscription) or set ANTHROPIC_API_KEY")
		}
	})
	return e.billing, e.billingErr
}

func (e *Engine) claudeRunner(dir string) *claude.Runner {
	c := e.Config.Claude
	return &claude.Runner{Binary: c.Binary, Billing: e.billing, APIKey: c.APIKey, Model: c.Model, MaxBudgetUSD: c.MaxBudgetUSD, Dir: dir}
}

func (e *Engine) recordClaude(key, purpose string, res *claude.Result, obs Observer) {
	if res == nil {
		return
	}
	g, _ := e.Store.Update(key, func(g *Generation) error {
		g.Claude.Billing = e.billing
		if res.Model != "" {
			g.Claude.Model = res.Model
		}
		g.Claude.CostUSD += res.CostUSD
		g.Claude.InputTokens += res.InputTokens
		g.Claude.OutputTokens += res.OutputTokens
		g.Claude.Calls = append(g.Claude.Calls, ClaudeCall{
			Purpose: purpose, Model: res.Model, CostUSD: res.CostUSD, DurationMS: res.DurationMS,
			InputTokens: res.InputTokens, OutputTokens: res.OutputTokens, SessionID: res.SessionID,
		})
		return nil
	})
	e.emit(obs, EventProgress, "", fmt.Sprintf("Claude %s: $%.4f", purpose, res.CostUSD), g)
}

func (e *Engine) loadStoryboard(g *Generation) (*Storyboard, error) {
	b, err := os.ReadFile(filepath.Join(g.Dir, FileScript))
	if err != nil {
		return nil, err
	}
	var sb Storyboard
	if err := json.Unmarshal(b, &sb); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileScript, err)
	}
	return &sb, nil
}

// LoadStoryboard exposes the saved storyboard for frontends.
func (e *Engine) LoadStoryboard(key string) (*Storyboard, error) {
	g, err := e.Store.Get(key)
	if err != nil {
		return nil, err
	}
	return e.loadStoryboard(g)
}

// Delete removes a generation and its directory.
func (e *Engine) Delete(key string) error {
	g, err := e.Store.Get(key)
	if err != nil {
		return err
	}
	if g.Status == StatusRunning && g.OwnerPID != 0 && pidAlive(g.OwnerPID) {
		return fmt.Errorf("generation %s is running (pid %d); stop it first", key, g.OwnerPID)
	}
	if err := e.Store.Delete(key); err != nil {
		return err
	}
	// Only remove directories inside our home.
	if rel, err := filepath.Rel(e.Home, g.Dir); err == nil && !strings.HasPrefix(rel, "..") && rel != "." && g.Dir != "" {
		return os.RemoveAll(g.Dir)
	}
	return nil
}

func (e *Engine) mediaTools() (media.Tools, error) { return media.FindTools() }

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
