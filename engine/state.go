package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"
	"time"
)

// Generation statuses.
const (
	StatusPending          = "pending"
	StatusRunning          = "running"
	StatusAwaitingApproval = "awaiting_approval" // storyboard ready, render not yet approved
	StatusSucceeded        = "succeeded"
	StatusFailed           = "failed"
	StatusCanceled         = "canceled"
	StatusInterrupted      = "interrupted" // owning process died mid-run; resumable
)

// Pipeline stages, in order.
const (
	StageIngest   = "ingest"
	StageDistill  = "distill"
	StageScript   = "script"
	StageRender   = "render"
	StageAssemble = "assemble"
	StageDone     = "done"
)

// Stages is the ordered pipeline.
var Stages = []string{StageIngest, StageDistill, StageScript, StageRender, StageAssemble}

// StageLabels are human-friendly names for UIs.
var StageLabels = map[string]string{
	StageIngest:   "Ingest document",
	StageDistill:  "Distill with Claude",
	StageScript:   "Write storyboard",
	StageRender:   "Render clips with Kling",
	StageAssemble: "Cut, caption & assemble",
}

// Artifact file names inside a generation directory.
const (
	FileDoc     = "DOC.md"
	FileDistill = "DISTILL.md"
	FileScript  = "SCRIPT.json"
	FileVideo   = "VIDEO.mp4"
	DirClips    = "clips"
	DirWork     = "work"
)

// State is the on-disk inventory at ~/.doctok/state.json.
type State struct {
	Version     int                    `json:"version"`
	UpdatedAt   time.Time              `json:"updated_at"`
	Generations map[string]*Generation `json:"generations"`
}

// Generation is one doc → video run, with all its metrics.
type Generation struct {
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	CreatedAt time.Time `json:"created_at"`

	Source SourceInfo `json:"source"`
	Style  string     `json:"style"`

	Status   string  `json:"status"`
	Stage    string  `json:"stage"`
	Progress float64 `json:"progress"` // 0..1 across the whole pipeline
	Message  string  `json:"message,omitempty"`
	Error    string  `json:"error,omitempty"`

	StartedAt       *time.Time              `json:"started_at,omitempty"`
	FinishedAt      *time.Time              `json:"finished_at,omitempty"`
	DurationSeconds float64                 `json:"duration_seconds,omitempty"` // wall-clock, start → finish
	ActiveSeconds   float64                 `json:"active_seconds,omitempty"`   // time spent working (excludes waiting for approval)
	StageTimes      map[string]*StageTiming `json:"stage_times,omitempty"`

	Options GenerationOptions `json:"options"`
	Claude  ClaudeMetrics     `json:"claude"`
	Kling   KlingMetrics      `json:"kling"`
	Video   VideoMetrics      `json:"video"`
	Files   map[string]string `json:"files,omitempty"`

	OwnerPID  int    `json:"owner_pid,omitempty"`
	OwnerHost string `json:"owner_host,omitempty"`
}

type SourceInfo struct {
	Kind  string `json:"kind"` // file | notion
	Ref   string `json:"ref"`  // path or URL as given
	Via   string `json:"via,omitempty"`
	Title string `json:"title,omitempty"`
	Bytes int    `json:"bytes,omitempty"`
}

type StageTiming struct {
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Seconds    float64    `json:"seconds,omitempty"`
}

type GenerationOptions struct {
	DryRun        bool   `json:"dry_run,omitempty"` // skip Kling; render placeholder clips locally
	TargetSeconds int    `json:"target_seconds"`
	Captions      bool   `json:"captions"`
	KlingModel    string `json:"kling_model"`
	Resolution    string `json:"resolution"`
	Audio         bool   `json:"audio"`
	Music         string `json:"music,omitempty"` // optional music bed mixed under dialogue
}

type ClaudeMetrics struct {
	Billing      string       `json:"billing,omitempty"` // subscription (cost is API-equivalent, covered by plan) | api (billed)
	Model        string       `json:"model,omitempty"`
	CostUSD      float64      `json:"cost_usd"`
	InputTokens  int          `json:"input_tokens"`
	OutputTokens int          `json:"output_tokens"`
	Calls        []ClaudeCall `json:"calls,omitempty"`
}

type ClaudeCall struct {
	Purpose      string  `json:"purpose"`
	Model        string  `json:"model,omitempty"`
	CostUSD      float64 `json:"cost_usd"`
	DurationMS   int     `json:"duration_ms"`
	InputTokens  int     `json:"input_tokens"`
	OutputTokens int     `json:"output_tokens"`
	SessionID    string  `json:"session_id,omitempty"`
}

type KlingMetrics struct {
	APIStyle       string  `json:"api_style,omitempty"`
	Model          string  `json:"model,omitempty"`
	Resolution     string  `json:"resolution,omitempty"`
	Audio          bool    `json:"audio"`
	EstimatedUnits float64 `json:"estimated_units"`
	Units          float64 `json:"units"`          // sum over clips (reported where available)
	UnitsReported  bool    `json:"units_reported"` // true if every clip's units came from Kling billing
	CostUSD        float64 `json:"cost_usd"`       // units × unit price (or reported cash)
	Clips          []*Clip `json:"clips,omitempty"`
}

// Clip statuses.
const (
	ClipPending    = "pending"
	ClipSubmitted  = "submitted"
	ClipProcessing = "processing"
	ClipSucceeded  = "succeeded"
	ClipDownloaded = "downloaded"
	ClipFailed     = "failed"
)

type Clip struct {
	Index         int        `json:"index"`
	Seconds       int        `json:"seconds"`
	TaskID        string     `json:"task_id,omitempty"`
	ExternalID    string     `json:"external_id,omitempty"`
	Status        string     `json:"status"`
	Attempts      int        `json:"attempts"`
	Units         float64    `json:"units"`
	UnitsReported bool       `json:"units_reported"`
	URL           string     `json:"url,omitempty"`
	Path          string     `json:"path,omitempty"`
	Error         string     `json:"error,omitempty"`
	SubmittedAt   *time.Time `json:"submitted_at,omitempty"`
	CompletedAt   *time.Time `json:"completed_at,omitempty"`
}

type VideoMetrics struct {
	DurationSeconds float64 `json:"duration_seconds,omitempty"`
	Width           int     `json:"width,omitempty"`
	Height          int     `json:"height,omitempty"`
	SizeBytes       int64   `json:"size_bytes,omitempty"`
	Scenes          int     `json:"scenes,omitempty"`
}

// Clone returns a deep copy (via JSON) so callers can't mutate stored state.
func (g *Generation) Clone() *Generation {
	b, _ := json.Marshal(g)
	var out Generation
	_ = json.Unmarshal(b, &out)
	return &out
}

// Elapsed returns wall-clock time since start (or total duration when done).
func (g *Generation) Elapsed(now time.Time) time.Duration {
	if g.StartedAt == nil {
		return 0
	}
	if g.FinishedAt != nil {
		return g.FinishedAt.Sub(*g.StartedAt)
	}
	return now.Sub(*g.StartedAt)
}

// IsTerminal reports whether the generation will not progress without user action.
func (g *Generation) IsTerminal() bool {
	switch g.Status {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted, StatusAwaitingApproval:
		return true
	}
	return false
}

// Store guards state.json with an in-process mutex plus an flock so the CLI and
// the local UI can safely run at the same time.
type Store struct {
	home string
	mu   sync.Mutex
}

func NewStore(home string) *Store { return &Store{home: home} }

func (s *Store) path() string     { return filepath.Join(s.home, "state.json") }
func (s *Store) lockPath() string { return filepath.Join(s.home, ".state.lock") }

func (s *Store) withLock(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.home, 0o755); err != nil {
		return err
	}
	lf, err := os.OpenFile(s.lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock state: %w", err)
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return fn()
}

func (s *Store) read() (*State, error) {
	st := &State{Version: 1, Generations: map[string]*Generation{}}
	b, err := os.ReadFile(s.path())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if len(b) == 0 {
		return st, nil
	}
	if err := json.Unmarshal(b, st); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.path(), err)
	}
	if st.Generations == nil {
		st.Generations = map[string]*Generation{}
	}
	for _, g := range st.Generations {
		if g.StageTimes == nil {
			g.StageTimes = map[string]*StageTiming{}
		}
		if g.Files == nil {
			g.Files = map[string]string{}
		}
	}
	return st, nil
}

func (s *Store) write(st *State) error {
	st.UpdatedAt = time.Now().UTC()
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(s.path(), b, 0o644)
}

// Load returns a snapshot of the whole state, marking dead-owner runs interrupted.
func (s *Store) Load() (*State, error) {
	var out *State
	err := s.withLock(func() error {
		st, err := s.read()
		if err != nil {
			return err
		}
		markInterrupted(st)
		out = st
		return nil
	})
	return out, err
}

// Get returns a copy of one generation.
func (s *Store) Get(key string) (*Generation, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	g, ok := st.Generations[key]
	if !ok {
		return nil, fmt.Errorf("no generation %q", key)
	}
	return g, nil
}

// List returns generations newest first.
func (s *Store) List() ([]*Generation, error) {
	st, err := s.Load()
	if err != nil {
		return nil, err
	}
	out := make([]*Generation, 0, len(st.Generations))
	for _, g := range st.Generations {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

// Put inserts or replaces a generation.
func (s *Store) Put(g *Generation) error {
	return s.withLock(func() error {
		st, err := s.read()
		if err != nil {
			return err
		}
		st.Generations[g.Key] = g.Clone()
		return s.write(st)
	})
}

// Update applies fn to a stored generation atomically and returns the new copy.
func (s *Store) Update(key string, fn func(g *Generation) error) (*Generation, error) {
	var out *Generation
	err := s.withLock(func() error {
		st, err := s.read()
		if err != nil {
			return err
		}
		g, ok := st.Generations[key]
		if !ok {
			return fmt.Errorf("no generation %q", key)
		}
		if err := fn(g); err != nil {
			return err
		}
		if err := s.write(st); err != nil {
			return err
		}
		out = g.Clone()
		return nil
	})
	return out, err
}

// Delete removes a generation from the inventory.
func (s *Store) Delete(key string) error {
	return s.withLock(func() error {
		st, err := s.read()
		if err != nil {
			return err
		}
		if _, ok := st.Generations[key]; !ok {
			return fmt.Errorf("no generation %q", key)
		}
		delete(st.Generations, key)
		return s.write(st)
	})
}

// ReserveKey picks a unique generation key derived from base and inserts the
// generation under it in one locked step.
func (s *Store) ReserveKey(base string, g *Generation) (string, error) {
	var key string
	err := s.withLock(func() error {
		st, err := s.read()
		if err != nil {
			return err
		}
		key = base
		for i := 2; ; i++ {
			_, taken := st.Generations[key]
			_, statErr := os.Stat(filepath.Join(s.home, key))
			if !taken && errors.Is(statErr, os.ErrNotExist) && !reservedName(key) {
				break
			}
			key = fmt.Sprintf("%s-%d", base, i)
		}
		g.Key = key
		g.Dir = filepath.Join(s.home, key)
		st.Generations[key] = g.Clone()
		return s.write(st)
	})
	return key, err
}

func reservedName(k string) bool {
	switch k {
	case "state.json", "config.json", "uploads", "logs":
		return true
	}
	return false
}

func markInterrupted(st *State) {
	host, _ := os.Hostname()
	for _, g := range st.Generations {
		if g.Status != StatusRunning || g.OwnerPID == 0 {
			continue
		}
		if g.OwnerHost != "" && g.OwnerHost != host {
			continue
		}
		if !pidAlive(g.OwnerPID) {
			g.Status = StatusInterrupted
			g.Message = "process exited mid-run — `doctok resume " + g.Key + "` to continue"
		}
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if pid == os.Getpid() {
		return true
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// StageDoneFor reports whether a stage has finished for g.
func StageDoneFor(g *Generation, stage string) bool {
	st := g.StageTimes[stage]
	return st != nil && st.FinishedAt != nil
}
