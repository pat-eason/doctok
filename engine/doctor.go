package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pat-eason/doctok/engine/claude"
	"github.com/pat-eason/doctok/engine/kling"
	"github.com/pat-eason/doctok/engine/media"
	"github.com/pat-eason/doctok/engine/notion"
)

// Check statuses.
const (
	CheckOK   = "ok"
	CheckWarn = "warn"
	CheckFail = "fail"
	CheckSkip = "skip"
)

// Check is one doctor result.
type Check struct {
	ID       string `json:"id"`
	Group    string `json:"group"`
	Name     string `json:"name"`
	Status   string `json:"status"`
	Detail   string `json:"detail"`
	Hint     string `json:"hint,omitempty"`
	Duration string `json:"duration,omitempty"`
}

// DoctorOptions tunes which checks run.
type DoctorOptions struct {
	Live    bool // run a tiny live Claude prompt (costs a fraction of a cent)
	SkipMCP bool // skip `claude mcp list` (slow: it health-checks every server)
	// AnthropicBaseURL overrides the API used to validate keys (tests).
	AnthropicBaseURL string
}

// DoctorReport summarizes all checks.
type DoctorReport struct {
	Checks        []Check `json:"checks"`
	ClaudeBilling string  `json:"claude_billing,omitempty"`
	Ready         bool    `json:"ready"` // can run a full (non-dry) generation from a local file
}

// DoctorCheckNames lists the checks in run order (for UIs that pre-render rows).
var DoctorCheckNames = []struct{ ID, Group, Name string }{
	{"home", "doctok", "Artifact directory"},
	{"config", "doctok", "Configuration"},
	{"ffmpeg", "doctok", "ffmpeg / ffprobe"},
	{"claude-bin", "claude", "Claude Code CLI"},
	{"claude-sub", "claude", "Claude subscription login"},
	{"claude-key", "claude", "Anthropic API key"},
	{"claude-pick", "claude", "Claude instance for distillation"},
	{"claude-live", "claude", "Live Claude round-trip"},
	{"kling-creds", "kling", "Kling credentials"},
	{"kling-api", "kling", "Kling API & balance"},
	{"kling-model", "kling", "Kling model"},
	{"notion-api", "notion", "Notion API token"},
	{"notion-mcp", "notion", "Notion via Claude Code MCP"},
}

// Doctor verifies everything doctok depends on. onCheck is called as each
// check starts (Status "") and finishes.
func (e *Engine) Doctor(ctx context.Context, opts DoctorOptions, onCheck func(Check)) DoctorReport {
	var rep DoctorReport
	emit := func(c Check) {
		if onCheck != nil {
			onCheck(c)
		}
	}
	run := func(id, group, name string, fn func() Check) Check {
		emit(Check{ID: id, Group: group, Name: name})
		start := time.Now()
		c := fn()
		c.ID, c.Group, c.Name = id, group, name
		c.Duration = time.Since(start).Round(10 * time.Millisecond).String()
		rep.Checks = append(rep.Checks, c)
		emit(c)
		return c
	}

	run("home", "doctok", "Artifact directory", func() Check {
		if err := os.MkdirAll(e.Home, 0o755); err != nil {
			return Check{Status: CheckFail, Detail: err.Error()}
		}
		f, err := os.CreateTemp(e.Home, ".doctor-*")
		if err != nil {
			return Check{Status: CheckFail, Detail: "not writable: " + err.Error()}
		}
		f.Close()
		os.Remove(f.Name())
		n := 0
		if gens, err := e.Store.List(); err == nil {
			n = len(gens)
		}
		return Check{Status: CheckOK, Detail: fmt.Sprintf("%s (%d generations in state.json)", e.Home, n)}
	})

	run("config", "doctok", "Configuration", func() Check {
		if err := e.Config.Validate(); err != nil {
			return Check{Status: CheckFail, Detail: err.Error(), Hint: "run `doctok configure`"}
		}
		where := filepath.Join(e.Home, "config.json")
		if !fileExists(where) {
			return Check{Status: CheckWarn, Detail: "no config.json yet — using defaults + environment", Hint: "run `doctok configure` to save your choices"}
		}
		return Check{Status: CheckOK, Detail: fmt.Sprintf("%s · style=%s · %ds target", where, e.Config.Video.Style, e.Config.Video.TargetSeconds)}
	})

	ffOK := run("ffmpeg", "doctok", "ffmpeg / ffprobe", func() Check {
		t, err := media.FindTools()
		if err != nil {
			return Check{Status: CheckFail, Detail: err.Error(), Hint: "brew install ffmpeg"}
		}
		v, err := t.Version(ctx)
		if err != nil {
			return Check{Status: CheckFail, Detail: err.Error()}
		}
		return Check{Status: CheckOK, Detail: v}
	}).Status == CheckOK

	r := &claude.Runner{Binary: e.Config.Claude.Binary}
	binOK := run("claude-bin", "claude", "Claude Code CLI", func() Check {
		p, err := r.LookPath()
		if err != nil {
			return Check{Status: CheckFail, Detail: "`claude` not found on PATH", Hint: "install Claude Code: https://claude.com/claude-code"}
		}
		v, err := r.Version(ctx)
		if err != nil {
			return Check{Status: CheckFail, Detail: p + ": " + err.Error()}
		}
		return Check{Status: CheckOK, Detail: v + " (" + p + ")"}
	}).Status == CheckOK

	var auth *claude.AuthStatus
	subOK := false
	run("claude-sub", "claude", "Claude subscription login", func() Check {
		if !binOK {
			return Check{Status: CheckSkip, Detail: "Claude Code not installed"}
		}
		st, err := r.Auth(ctx)
		if err != nil {
			return Check{Status: CheckWarn, Detail: err.Error(), Hint: "run `claude auth login`"}
		}
		auth = st
		if !st.HasSubscription() {
			return Check{Status: CheckWarn, Detail: fmt.Sprintf("not logged in with a Claude subscription (method=%s)", orDefault(st.AuthMethod, "none")), Hint: "run `claude auth login` to use your Pro/Max plan"}
		}
		subOK = true
		plan := orDefault(st.SubscriptionType, "subscription")
		who := st.Email
		if who == "" {
			who = st.OrgName
		}
		return Check{Status: CheckOK, Detail: fmt.Sprintf("%s plan · %s", plan, who)}
	})
	_ = auth

	keyOK := false
	run("claude-key", "claude", "Anthropic API key", func() Check {
		key := e.Config.Claude.APIKey
		if key == "" {
			st := CheckSkip
			if e.Config.Claude.Mode == ClaudeModeAPI {
				st = CheckFail
			}
			return Check{Status: st, Detail: "no API key configured", Hint: "set ANTHROPIC_API_KEY or `doctok configure` (only needed for API billing)"}
		}
		src := "config.json"
		if e.Config.FromEnv("claude.api_key") {
			src = "environment"
		}
		if err := validateAnthropicKey(ctx, key, opts.AnthropicBaseURL); err != nil {
			return Check{Status: CheckFail, Detail: fmt.Sprintf("key %s from %s rejected: %v", maskKey(key), src, err)}
		}
		keyOK = true
		return Check{Status: CheckOK, Detail: fmt.Sprintf("key %s from %s is valid", maskKey(key), src)}
	})

	pickOK := run("claude-pick", "claude", "Claude instance for distillation", func() Check {
		mode := e.Config.Claude.Mode
		switch {
		case !binOK:
			return Check{Status: CheckFail, Detail: "Claude Code is required for reading & distilling docs"}
		case mode == ClaudeModeSubscription && !subOK:
			return Check{Status: CheckFail, Detail: "mode=subscription but not logged in", Hint: "`claude auth login` or switch mode with `doctok configure`"}
		case mode == ClaudeModeAPI && !keyOK:
			return Check{Status: CheckFail, Detail: "mode=api but no valid API key"}
		case mode == ClaudeModeSubscription || (mode == ClaudeModeAuto && subOK):
			rep.ClaudeBilling = claude.BillingSubscription
		case mode == ClaudeModeAPI || (mode == ClaudeModeAuto && keyOK):
			rep.ClaudeBilling = claude.BillingAPI
		default:
			return Check{Status: CheckFail, Detail: "neither a subscription login nor an API key is available", Hint: "`claude auth login` or set ANTHROPIC_API_KEY"}
		}
		model := orDefault(e.Config.Claude.Model, "Claude Code default model")
		note := "costs are reported as API-equivalent; usage counts against your plan"
		if rep.ClaudeBilling == claude.BillingAPI {
			note = "billed to your Anthropic API account"
		}
		return Check{Status: CheckOK, Detail: fmt.Sprintf("%s (mode=%s) · %s · %s", rep.ClaudeBilling, mode, model, note)}
	}).Status == CheckOK

	run("claude-live", "claude", "Live Claude round-trip", func() Check {
		if !opts.Live {
			return Check{Status: CheckSkip, Detail: "skipped (use --live to send a tiny test prompt)"}
		}
		if !pickOK {
			return Check{Status: CheckSkip, Detail: "no usable Claude instance"}
		}
		lr := &claude.Runner{Binary: e.Config.Claude.Binary, Billing: rep.ClaudeBilling, APIKey: e.Config.Claude.APIKey, Model: e.Config.Claude.Model, Dir: os.TempDir()}
		res, err := lr.Run(ctx, claude.Request{Prompt: "Reply with exactly: slay", Timeout: 2 * time.Minute})
		if err != nil {
			return Check{Status: CheckFail, Detail: err.Error()}
		}
		return Check{Status: CheckOK, Detail: fmt.Sprintf("%q from %s in %dms ($%.5f)", strings.TrimSpace(res.Text), orDefault(res.Model, "claude"), res.DurationMS, res.CostUSD)}
	})

	kc := e.Config.Kling
	credsOK := run("kling-creds", "kling", "Kling credentials", func() Check {
		switch {
		case kc.APIKey != "":
			return Check{Status: CheckOK, Detail: fmt.Sprintf("API key %s · %s", maskKey(kc.APIKey), kc.BaseURL)}
		case kc.AccessKey != "" && kc.SecretKey != "":
			return Check{Status: CheckOK, Detail: fmt.Sprintf("access key %s + secret (JWT) · %s", maskKey(kc.AccessKey), kc.BaseURL)}
		case kc.AccessKey != "" || kc.SecretKey != "":
			return Check{Status: CheckFail, Detail: "only one of access key / secret key is set"}
		}
		return Check{Status: CheckFail, Detail: "no Kling credentials", Hint: "create an API key at https://kling.ai/dev, then `doctok configure` or export KLING_API_KEY (dry runs work without it)"}
	}).Status == CheckOK

	klingOK := run("kling-api", "kling", "Kling API & balance", func() Check {
		if !credsOK {
			return Check{Status: CheckSkip, Detail: "no credentials"}
		}
		client, style, err := e.NewKling(kc)
		if err != nil {
			return Check{Status: CheckFail, Detail: err.Error()}
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		acct, err := client.AccountCosts(cctx)
		if err != nil {
			return Check{Status: CheckFail, Detail: err.Error(), Hint: "check credentials, kling.base_url and kling.api_style"}
		}
		packs := make([]string, 0, len(acct.Packs))
		for _, p := range acct.Packs {
			if p.Status == "" || p.Status == "online" {
				packs = append(packs, fmt.Sprintf("%s %.0f/%.0f", orDefault(p.Name, "pack"), p.Remaining, p.Total))
			}
		}
		detail := fmt.Sprintf("authenticated (%s API) · %.1f units remaining (~$%.2f)", style, acct.RemainingUnits, acct.RemainingUnits*kc.UnitPriceUSD)
		if len(packs) > 0 {
			detail += " · " + strings.Join(packs, ", ")
		}
		spec, _ := kling.Lookup(kc.Model)
		need := spec.UnitsFor(e.Config.Video.TargetSeconds, kc.Resolution, kc.Audio)
		if acct.RemainingUnits < need {
			return Check{Status: CheckWarn, Detail: detail, Hint: fmt.Sprintf("a %ds video needs ~%.0f units; balance may be low (Kling reports balances with up to ~12h delay)", e.Config.Video.TargetSeconds, need)}
		}
		return Check{Status: CheckOK, Detail: detail}
	}).Status == CheckOK

	run("kling-model", "kling", "Kling model", func() Check {
		spec, ok := kling.Lookup(kc.Model)
		if !ok {
			return Check{Status: CheckFail, Detail: fmt.Sprintf("unknown model %q", kc.Model), Hint: "known: " + strings.Join(kling.ModelIDs(), ", ")}
		}
		if err := spec.Validate(kc.Resolution, kc.Audio); err != nil {
			return Check{Status: CheckFail, Detail: err.Error()}
		}
		style := kc.APIStyle
		if style == KlingStyleAuto {
			style = KlingStyleStandard
			if kc.APIKey == "" {
				style = KlingStyleLegacy
			}
		}
		if style == KlingStyleLegacy && spec.LegacyName == "" {
			return Check{Status: CheckFail, Detail: spec.ID + " isn't available on the legacy (AK/SK) API", Hint: "use an API key (standard API) or pick kling-2.6"}
		}
		units := spec.UnitsFor(e.Config.Video.TargetSeconds, kc.Resolution, kc.Audio)
		return Check{Status: CheckOK, Detail: fmt.Sprintf("%s · %s · audio=%v · ~%.0f units (~$%.2f) per %ds video",
			spec.Label, kc.Resolution, spec.EffectiveAudio(kc.Audio), units, units*kc.UnitPriceUSD, e.Config.Video.TargetSeconds)}
	})

	notionAPIOK := run("notion-api", "notion", "Notion API token", func() Check {
		if e.Config.Notion.Token == "" {
			return Check{Status: CheckSkip, Detail: "no token (Notion pages will be fetched via Claude Code MCP)", Hint: "optional: create an integration at https://www.notion.so/my-integrations and set NOTION_TOKEN"}
		}
		cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		who, err := notion.New(e.Config.Notion.Token).Me(cctx)
		if err != nil {
			return Check{Status: CheckFail, Detail: err.Error()}
		}
		return Check{Status: CheckOK, Detail: "integration: " + who + " (share pages with it via ••• → Connections)"}
	}).Status == CheckOK

	run("notion-mcp", "notion", "Notion via Claude Code MCP", func() Check {
		if opts.SkipMCP {
			return Check{Status: CheckSkip, Detail: "skipped"}
		}
		if !binOK {
			return Check{Status: CheckSkip, Detail: "Claude Code not installed"}
		}
		srv, all, err := e.FindNotionMCP(ctx)
		if err != nil {
			return Check{Status: CheckWarn, Detail: "claude mcp list: " + err.Error()}
		}
		level := CheckWarn
		if notionAPIOK {
			level = CheckSkip // API covers Notion; MCP is optional
		}
		if srv == nil {
			return Check{Status: level, Detail: fmt.Sprintf("no Notion MCP server among %d configured", len(all)),
				Hint: "claude mcp add --transport http notion https://mcp.notion.com/mcp  (then /mcp in claude to sign in)"}
		}
		switch srv.Status {
		case "connected":
			if srv.IsClaudeAIConnector() {
				return Check{Status: CheckWarn, Detail: fmt.Sprintf("%q is connected, but claude.ai connectors aren't loaded by headless Claude Code", srv.Name),
					Hint: "add a local server: claude mcp add --transport http notion https://mcp.notion.com/mcp"}
			}
			return Check{Status: CheckOK, Detail: fmt.Sprintf("%q connected (%s)", srv.Name, srv.ToolScope)}
		case "needs-auth":
			return Check{Status: level, Detail: fmt.Sprintf("%q needs authentication", srv.Name), Hint: "run `claude`, then `/mcp` and sign in to Notion"}
		}
		return Check{Status: level, Detail: fmt.Sprintf("%q status: %s", srv.Name, srv.Status)}
	})

	rep.Ready = ffOK && pickOK && klingOK
	return rep
}

func validateAnthropicKey(ctx context.Context, key, base string) error {
	if base == "" {
		base = "https://api.anthropic.com"
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/v1/models?limit=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set("x-api-key", key)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	return fmt.Errorf("HTTP %d %s", resp.StatusCode, e.Error.Message)
}

func maskKey(k string) string {
	if len(k) <= 8 {
		return "••••"
	}
	return k[:4] + "…" + k[len(k)-4:]
}
