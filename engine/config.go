package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Claude billing / auth modes.
const (
	ClaudeModeAuto         = "auto"
	ClaudeModeSubscription = "subscription"
	ClaudeModeAPI          = "api"
)

// Notion retrieval modes.
const (
	NotionModeAuto = "auto"
	NotionModeAPI  = "api"
	NotionModeMCP  = "mcp"
)

// Kling API styles.
const (
	KlingStyleAuto     = "auto"
	KlingStyleStandard = "standard" // POST /text-to-video/<model>, GET /tasks (API key)
	KlingStyleLegacy   = "legacy"   // POST /v1/videos/text2video (AK/SK JWT)
)

// Supported video lengths. The old defaults were 180–300s; short formats
// (30s–2min) are supported now.
const (
	MinVideoSeconds = 30
	MaxVideoSeconds = 300
)

// Kling backends: how clips are generated.
const (
	KlingBackendAPI = "api" // Kling Open Platform API (API key or AK/SK), pay-per-unit
	KlingBackendMCP = "mcp" // Kling MCP server via headless Claude Code, billed to the Kling account's credits
)

// Config is persisted at ~/.doctok/config.json. Environment variables override
// file values at load time (see applyEnv) but are never written back.
type Config struct {
	Claude ClaudeConfig `json:"claude"`
	Kling  KlingConfig  `json:"kling"`
	Notion NotionConfig `json:"notion"`
	Video  VideoConfig  `json:"video"`

	// envOverrides records which fields came from the environment so Save()
	// doesn't persist secrets the user only exported in their shell.
	envOverrides map[string]bool
}

type ClaudeConfig struct {
	Mode         string  `json:"mode"`              // auto | subscription | api
	APIKey       string  `json:"api_key,omitempty"` // only used in api mode
	Model        string  `json:"model,omitempty"`   // alias or full id; empty = Claude Code default
	Binary       string  `json:"binary,omitempty"`  // path to `claude`; empty = $PATH lookup
	MaxBudgetUSD float64 `json:"max_budget_usd,omitempty"`
}

type KlingConfig struct {
	Backend   string `json:"backend,omitempty"`    // api (default) | mcp
	MCPServer string `json:"mcp_server,omitempty"` // name from `claude mcp list`; empty = autodetect "kling"
	// CreditsPerSecond prices MCP renders for the estimate (Kling doesn't publish
	// per-clip credit prices through the MCP). 0 = unknown: no credit estimate is shown.
	CreditsPerSecond float64 `json:"credits_per_second,omitempty"`
	NoHero           bool    `json:"no_hero,omitempty"` // mcp: skip the presenter reference image (clips are then independent text-to-video)
	APIKey           string  `json:"api_key,omitempty"`
	AccessKey        string  `json:"access_key,omitempty"`
	SecretKey        string  `json:"secret_key,omitempty"`
	BaseURL          string  `json:"base_url"`
	APIStyle         string  `json:"api_style"` // auto | standard | legacy
	Model            string  `json:"model"`
	Resolution       string  `json:"resolution"` // 720p | 1080p
	Audio            bool    `json:"audio"`      // native audio (spoken dialogue)
	Concurrency      int     `json:"concurrency"`
	PollSeconds      int     `json:"poll_seconds"`
	UnitPriceUSD     float64 `json:"unit_price_usd"`
}

type NotionConfig struct {
	Mode      string `json:"mode"` // auto | api | mcp
	Token     string `json:"token,omitempty"`
	MCPServer string `json:"mcp_server,omitempty"` // name from `claude mcp list`; empty = autodetect
}

type VideoConfig struct {
	Style         string `json:"style"`
	TargetSeconds int    `json:"target_seconds"`
	MinSeconds    int    `json:"min_seconds"`
	MaxSeconds    int    `json:"max_seconds"`
	Captions      bool   `json:"captions"`
	Width         int    `json:"width"`
	Height        int    `json:"height"`
	FPS           int    `json:"fps"`
}

// DefaultConfig returns sane defaults: 1080x1920 vertical, ~3.5 minutes,
// Kling 2.6 at 1080p with native audio so the influencer actually talks.
func DefaultConfig() Config {
	return Config{
		Claude: ClaudeConfig{Mode: ClaudeModeAuto},
		Kling: KlingConfig{
			Backend:      KlingBackendAPI,
			BaseURL:      "https://api-singapore.klingai.com",
			APIStyle:     KlingStyleAuto,
			Model:        "kling-3.0",
			Resolution:   "1080p",
			Audio:        true,
			Concurrency:  4,
			PollSeconds:  10,
			UnitPriceUSD: 0.14,
		},
		Notion: NotionConfig{Mode: NotionModeAuto},
		Video: VideoConfig{
			Style:         DefaultStyleID,
			TargetSeconds: 210,
			MinSeconds:    MinVideoSeconds,
			MaxSeconds:    MaxVideoSeconds,
			Captions:      true,
			Width:         1080,
			Height:        1920,
			FPS:           30,
		},
	}
}

// HomeDir returns the doctok artifact directory (~/.doctok or $DOCTOK_HOME).
func HomeDir() (string, error) {
	if h := os.Getenv("DOCTOK_HOME"); h != "" {
		return filepath.Abs(h)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".doctok"), nil
}

func configPath(home string) string { return filepath.Join(home, "config.json") }

// LoadConfig reads config.json (if present) over defaults, then applies env.
func LoadConfig(home string) (Config, error) {
	cfg := DefaultConfig()
	b, err := os.ReadFile(configPath(home))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return cfg, err
	default:
		if err := json.Unmarshal(b, &cfg); err != nil {
			return cfg, fmt.Errorf("parse %s: %w", configPath(home), err)
		}
	}
	cfg.fillDefaults()
	cfg.applyEnv()
	return cfg, nil
}

// SaveConfig writes the config with 0600 perms. Values that came only from the
// environment are not persisted.
func SaveConfig(home string, cfg Config) error {
	if err := os.MkdirAll(home, 0o755); err != nil {
		return err
	}
	out := cfg
	// Re-read the on-disk values for env-overridden fields so we don't leak them.
	disk := DefaultConfig()
	if b, err := os.ReadFile(configPath(home)); err == nil {
		_ = json.Unmarshal(b, &disk)
	}
	for k := range cfg.envOverrides {
		switch k {
		case "claude.api_key":
			out.Claude.APIKey = disk.Claude.APIKey
		case "kling.api_key":
			out.Kling.APIKey = disk.Kling.APIKey
		case "kling.access_key":
			out.Kling.AccessKey = disk.Kling.AccessKey
		case "kling.secret_key":
			out.Kling.SecretKey = disk.Kling.SecretKey
		case "kling.base_url":
			out.Kling.BaseURL = disk.Kling.BaseURL
		case "kling.backend":
			out.Kling.Backend = disk.Kling.Backend
		case "kling.mcp_server":
			out.Kling.MCPServer = disk.Kling.MCPServer
		case "notion.token":
			out.Notion.Token = disk.Notion.Token
		}
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(configPath(home), b, 0o600)
}

// FromEnv reports whether a config field's current value came from the environment.
func (c Config) FromEnv(field string) bool { return c.envOverrides[field] }

func (c *Config) fillDefaults() {
	d := DefaultConfig()
	if c.Claude.Mode == "" {
		c.Claude.Mode = d.Claude.Mode
	}
	if c.Kling.BaseURL == "" {
		c.Kling.BaseURL = d.Kling.BaseURL
	}
	if c.Kling.Backend == "" {
		c.Kling.Backend = d.Kling.Backend
	}
	if c.Kling.APIStyle == "" {
		c.Kling.APIStyle = d.Kling.APIStyle
	}
	if c.Kling.Model == "" {
		c.Kling.Model = d.Kling.Model
	}
	if c.Kling.Resolution == "" {
		c.Kling.Resolution = d.Kling.Resolution
	}
	if c.Kling.Concurrency <= 0 {
		c.Kling.Concurrency = d.Kling.Concurrency
	}
	if c.Kling.PollSeconds <= 0 {
		c.Kling.PollSeconds = d.Kling.PollSeconds
	}
	if c.Kling.UnitPriceUSD <= 0 {
		c.Kling.UnitPriceUSD = d.Kling.UnitPriceUSD
	}
	if c.Notion.Mode == "" {
		c.Notion.Mode = d.Notion.Mode
	}
	if c.Video.Style == "" {
		c.Video.Style = d.Video.Style
	}
	if c.Video.TargetSeconds <= 0 {
		c.Video.TargetSeconds = d.Video.TargetSeconds
	}
	if c.Video.MinSeconds <= 0 {
		c.Video.MinSeconds = d.Video.MinSeconds
	}
	if c.Video.MaxSeconds <= 0 {
		c.Video.MaxSeconds = d.Video.MaxSeconds
	}
	// Configs saved by older versions pin the old 180–300s defaults, which would
	// reject every short video. That exact pair is treated as "never customized".
	if c.Video.MinSeconds == 180 && c.Video.MaxSeconds == 300 {
		c.Video.MinSeconds = MinVideoSeconds
	}
	if c.Video.Width <= 0 || c.Video.Height <= 0 {
		c.Video.Width, c.Video.Height = d.Video.Width, d.Video.Height
	}
	if c.Video.FPS <= 0 {
		c.Video.FPS = d.Video.FPS
	}
}

func (c *Config) applyEnv() {
	c.envOverrides = map[string]bool{}
	set := func(field string, dst *string, keys ...string) {
		for _, k := range keys {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				*dst = v
				c.envOverrides[field] = true
				return
			}
		}
	}
	set("claude.api_key", &c.Claude.APIKey, "DOCTOK_ANTHROPIC_API_KEY", "ANTHROPIC_API_KEY")
	set("kling.api_key", &c.Kling.APIKey, "KLING_API_KEY")
	set("kling.access_key", &c.Kling.AccessKey, "KLING_ACCESS_KEY")
	set("kling.secret_key", &c.Kling.SecretKey, "KLING_SECRET_KEY")
	set("kling.base_url", &c.Kling.BaseURL, "KLING_BASE_URL")
	set("kling.backend", &c.Kling.Backend, "KLING_BACKEND")
	set("kling.mcp_server", &c.Kling.MCPServer, "KLING_MCP_SERVER")
	set("notion.token", &c.Notion.Token, "NOTION_TOKEN", "NOTION_API_KEY")
}

// Validate checks the config for obviously bad values.
func (c Config) Validate() error {
	var errs []string
	switch c.Claude.Mode {
	case ClaudeModeAuto, ClaudeModeSubscription, ClaudeModeAPI:
	default:
		errs = append(errs, fmt.Sprintf("claude.mode %q must be auto|subscription|api", c.Claude.Mode))
	}
	switch c.Notion.Mode {
	case NotionModeAuto, NotionModeAPI, NotionModeMCP:
	default:
		errs = append(errs, fmt.Sprintf("notion.mode %q must be auto|api|mcp", c.Notion.Mode))
	}
	switch c.Kling.Backend {
	case KlingBackendAPI, KlingBackendMCP:
	default:
		errs = append(errs, fmt.Sprintf("kling.backend %q must be api|mcp", c.Kling.Backend))
	}
	switch c.Kling.APIStyle {
	case KlingStyleAuto, KlingStyleStandard, KlingStyleLegacy:
	default:
		errs = append(errs, fmt.Sprintf("kling.api_style %q must be auto|standard|legacy", c.Kling.APIStyle))
	}
	if c.Video.MinSeconds > c.Video.MaxSeconds {
		errs = append(errs, "video.min_seconds must be <= video.max_seconds")
	}
	if c.Video.TargetSeconds < c.Video.MinSeconds || c.Video.TargetSeconds > c.Video.MaxSeconds {
		errs = append(errs, fmt.Sprintf("video.target_seconds %d must be within [%d,%d]", c.Video.TargetSeconds, c.Video.MinSeconds, c.Video.MaxSeconds))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(perm); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
