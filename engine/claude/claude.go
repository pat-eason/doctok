// Package claude drives a local Claude Code install (`claude -p`) headlessly,
// using either the user's Claude subscription login or an Anthropic API key.
package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

const (
	BillingSubscription = "subscription"
	BillingAPI          = "api"
)

// Runner invokes the Claude Code CLI.
type Runner struct {
	Binary       string // path to claude; "" → lookup "claude"
	Billing      string // subscription | api
	APIKey       string // used when Billing == api
	Model        string // optional model alias/id
	MaxBudgetUSD float64
	Dir          string // working directory for the subprocess
}

// Request is one headless prompt.
type Request struct {
	Prompt       string          // sent on stdin (safe for very large docs)
	SystemPrompt string          // appended to Claude Code's system prompt
	Schema       json.RawMessage // optional JSON Schema → structured output
	// MCP: when non-empty, the run may use these MCP tool prefixes
	// (e.g. "mcp__notion"); otherwise all tools and MCP servers are disabled.
	AllowMCP []string
	// Tools enables specific built-in tools (e.g. WebSearch, WebFetch) with no
	// MCP servers. Mutually exclusive with AllowMCP.
	Tools   []string
	Timeout time.Duration
	// OnTool, when set, streams the run (stream-json) and reports each tool call.
	OnTool func(ToolCall)
}

// ToolCall is a tool invocation observed while streaming.
type ToolCall struct {
	Name  string
	Input map[string]any
}

// Summary renders a short human description ("search: …", "fetch: …").
func (t ToolCall) Summary() string {
	str := func(k string) string { v, _ := t.Input[k].(string); return v }
	switch t.Name {
	case "WebSearch":
		return "search: " + str("query")
	case "WebFetch":
		return "fetch: " + str("url")
	}
	return t.Name
}

// Result is the parsed `--output-format json` envelope.
type Result struct {
	Text         string
	Structured   json.RawMessage
	CostUSD      float64
	DurationMS   int
	InputTokens  int
	OutputTokens int
	Model        string
	SessionID    string
	Turns        int
}

type envelope struct {
	Type             string          `json:"type"`
	Subtype          string          `json:"subtype"`
	IsError          bool            `json:"is_error"`
	Result           string          `json:"result"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	TotalCostUSD     float64         `json:"total_cost_usd"`
	DurationMS       int             `json:"duration_ms"`
	NumTurns         int             `json:"num_turns"`
	SessionID        string          `json:"session_id"`
	Usage            struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]struct {
		InputTokens              int     `json:"inputTokens"`
		OutputTokens             int     `json:"outputTokens"`
		CacheReadInputTokens     int     `json:"cacheReadInputTokens"`
		CacheCreationInputTokens int     `json:"cacheCreationInputTokens"`
		CostUSD                  float64 `json:"costUSD"`
	} `json:"modelUsage"`
	PermissionDenials []json.RawMessage `json:"permission_denials"`
	APIErrorStatus    any               `json:"api_error_status"`
}

func (r *Runner) bin() string {
	if r.Binary != "" {
		return r.Binary
	}
	return "claude"
}

// LookPath resolves the claude binary.
func (r *Runner) LookPath() (string, error) { return exec.LookPath(r.bin()) }

// Env builds the subprocess environment for the chosen billing mode.
func (r *Runner) Env() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT":
			// Billing is decided explicitly below; CLAUDECODE would make a nested
			// session think it's running inside another Claude Code session.
			continue
		}
		env = append(env, kv)
	}
	if r.Billing == BillingAPI && r.APIKey != "" {
		env = append(env, "ANTHROPIC_API_KEY="+r.APIKey)
	}
	return env
}

// Args builds the CLI arguments for a request.
func (r *Runner) Args(req Request) []string {
	format := []string{"--output-format", "json"}
	if req.OnTool != nil {
		format = []string{"--output-format", "stream-json", "--verbose"}
	}
	args := append([]string{"-p"}, format...)
	args = append(args, "--no-session-persistence", "--permission-prompts", "none")
	if r.Model != "" {
		args = append(args, "--model", r.Model)
	}
	if r.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", fmt.Sprintf("%.2f", r.MaxBudgetUSD))
	}
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", req.SystemPrompt)
	}
	if len(req.Schema) > 0 {
		args = append(args, "--json-schema", string(req.Schema))
	}
	switch {
	case len(req.Tools) > 0:
		// Only the named built-ins (e.g. web research), no MCP servers.
		list := strings.Join(req.Tools, ",")
		args = append(args, "--tools", list, "--allowedTools", list, "--strict-mcp-config")
	case len(req.AllowMCP) == 0:
		// Pure text-in/text-out: no tools, no MCP servers.
		args = append(args, "--tools", "", "--strict-mcp-config")
	default:
		// Only the named MCP servers are auto-approved; everything that would
		// prompt is denied. Mutating built-ins are removed outright.
		args = append(args, "--allowedTools", strings.Join(req.AllowMCP, ","))
		args = append(args, "--disallowedTools", "Bash,Edit,Write,NotebookEdit,WebFetch,WebSearch,Agent")
	}
	return args
}

// Run executes the request and parses the JSON envelope.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	if req.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, req.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, r.bin(), r.Args(req)...)
	cmd.Env = r.Env()
	cmd.Dir = r.Dir
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.WaitDelay = 5 * time.Second
	var runErr error
	if req.OnTool == nil {
		cmd.Stdout = &stdout
		runErr = cmd.Run()
	} else {
		pipe, err := cmd.StdoutPipe()
		if err != nil {
			return nil, err
		}
		if err := cmd.Start(); err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(pipe)
		sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
		for sc.Scan() {
			line := sc.Bytes()
			stdout.Write(line)
			stdout.WriteByte('\n')
			for _, tc := range parseToolCalls(line) {
				req.OnTool(tc)
			}
		}
		runErr = cmd.Wait()
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	env, perr := parseEnvelope(stdout.Bytes())
	if perr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if runErr != nil {
			return nil, fmt.Errorf("claude: %v: %s", runErr, truncate(msg, 600))
		}
		return nil, fmt.Errorf("claude: unparseable output: %v: %s", perr, truncate(msg, 600))
	}
	res := &Result{
		Text:       env.Result,
		Structured: env.StructuredOutput,
		CostUSD:    env.TotalCostUSD,
		DurationMS: env.DurationMS,
		SessionID:  env.SessionID,
		Turns:      env.NumTurns,
	}
	// Token totals: prefer the per-model breakdown (includes cache tokens).
	var bestCost float64 = -1
	for model, mu := range env.ModelUsage {
		res.InputTokens += mu.InputTokens + mu.CacheReadInputTokens + mu.CacheCreationInputTokens
		res.OutputTokens += mu.OutputTokens
		if mu.CostUSD > bestCost {
			bestCost, res.Model = mu.CostUSD, model
		}
	}
	if len(env.ModelUsage) == 0 {
		res.InputTokens = env.Usage.InputTokens + env.Usage.CacheReadInputTokens + env.Usage.CacheCreationInputTokens
		res.OutputTokens = env.Usage.OutputTokens
	}
	if env.IsError || (env.Subtype != "" && env.Subtype != "success") {
		msg := env.Result
		if msg == "" {
			msg = env.Subtype
		}
		return res, fmt.Errorf("claude returned an error (%s): %s", env.Subtype, truncate(msg, 600))
	}
	if len(req.Schema) > 0 && len(res.Structured) == 0 {
		// Fall back to parsing JSON out of the text result.
		if js := extractJSON(res.Text); js != "" {
			res.Structured = json.RawMessage(js)
		} else {
			return res, errors.New("claude did not return structured output")
		}
	}
	return res, nil
}

// parseEnvelope finds the result object; tolerates stray non-JSON lines.
func parseEnvelope(b []byte) (*envelope, error) {
	b = bytes.TrimSpace(b)
	var env envelope
	if err := json.Unmarshal(b, &env); err == nil && env.Type != "" {
		return &env, nil
	}
	// Some versions may print a JSON array of messages or extra lines; scan
	// for the last line that parses as a result.
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	var found *envelope
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e envelope
		if json.Unmarshal(line, &e) == nil && e.Type == "result" {
			ec := e
			found = &ec
		}
	}
	if found != nil {
		return found, nil
	}
	var arr []envelope
	if json.Unmarshal(b, &arr) == nil {
		for i := len(arr) - 1; i >= 0; i-- {
			if arr[i].Type == "result" {
				return &arr[i], nil
			}
		}
	}
	return nil, errors.New("no result object in claude output")
}

// parseToolCalls extracts tool_use blocks from one stream-json line.
func parseToolCalls(line []byte) []ToolCall {
	var msg struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type  string         `json:"type"`
				Name  string         `json:"name"`
				Input map[string]any `json:"input"`
			} `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal(line, &msg) != nil || msg.Type != "assistant" {
		return nil
	}
	var out []ToolCall
	for _, c := range msg.Message.Content {
		if c.Type == "tool_use" || c.Type == "server_tool_use" {
			out = append(out, ToolCall{Name: c.Name, Input: c.Input})
		}
	}
	return out
}

var fenceRE = regexp.MustCompile("(?s)```(?:json)?\\s*(\\{.*\\})\\s*```")

func extractJSON(s string) string {
	if m := fenceRE.FindStringSubmatch(s); m != nil && json.Valid([]byte(m[1])) {
		return m[1]
	}
	start := strings.Index(s, "{")
	end := strings.LastIndex(s, "}")
	if start >= 0 && end > start && json.Valid([]byte(s[start:end+1])) {
		return s[start : end+1]
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// AuthStatus is `claude auth status --json`.
type AuthStatus struct {
	LoggedIn         bool   `json:"loggedIn"`
	AuthMethod       string `json:"authMethod"`
	APIProvider      string `json:"apiProvider"`
	Email            string `json:"email"`
	OrgName          string `json:"orgName"`
	SubscriptionType string `json:"subscriptionType"`
}

// HasSubscription reports a claude.ai (Pro/Max/Team/Enterprise) login.
func (a AuthStatus) HasSubscription() bool {
	return a.LoggedIn && (a.AuthMethod == "claude.ai" || a.SubscriptionType != "")
}

// Version returns `claude --version`.
func (r *Runner) Version(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), "--version")
	cmd.Env = r.Env()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// Auth returns the subscription login status (API key env is stripped so we
// see the OAuth/keychain login, not the key).
func (r *Runner) Auth(ctx context.Context) (*AuthStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	sub := *r
	sub.Billing = BillingSubscription
	cmd := exec.CommandContext(ctx, r.bin(), "auth", "status", "--json")
	cmd.Env = sub.Env()
	out, err := cmd.Output()
	var st AuthStatus
	if jerr := json.Unmarshal(bytes.TrimSpace(out), &st); jerr != nil {
		if err != nil {
			return nil, fmt.Errorf("claude auth status: %w", err)
		}
		return nil, fmt.Errorf("claude auth status: %w", jerr)
	}
	return &st, nil
}

// MCPServer is one line of `claude mcp list`.
type MCPServer struct {
	Name      string
	Target    string
	Status    string // connected | needs-auth | failed | pending | unknown
	Raw       string
	ToolScope string // e.g. "mcp__claude_ai_Notion"
}

var mcpLineRE = regexp.MustCompile(`^(.+?): (.+?) - (.+)$`)

// MCPList runs `claude mcp list` (performs health checks; may take a while).
func (r *Runner) MCPList(ctx context.Context) ([]MCPServer, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, r.bin(), "mcp", "list")
	cmd.Env = r.Env()
	out, err := cmd.CombinedOutput()
	if err != nil && len(out) == 0 {
		return nil, err
	}
	return ParseMCPList(string(out)), nil
}

// ParseMCPList parses `claude mcp list` output.
func ParseMCPList(out string) []MCPServer {
	var servers []MCPServer
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		m := mcpLineRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		s := MCPServer{Name: m[1], Target: m[2], Raw: line, ToolScope: ToolScope(m[1])}
		st := strings.ToLower(m[3])
		switch {
		case strings.Contains(st, "connected") && !strings.Contains(st, "not connected"):
			s.Status = "connected"
		case strings.Contains(st, "auth"):
			s.Status = "needs-auth"
		case strings.Contains(st, "pending"):
			s.Status = "pending"
		case strings.Contains(st, "fail"), strings.Contains(st, "error"):
			s.Status = "failed"
		default:
			s.Status = "unknown"
		}
		servers = append(servers, s)
	}
	return servers
}

var nonToolChar = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// ToolScope maps an MCP server display name to Claude Code's tool prefix,
// e.g. "claude.ai Notion" → "mcp__claude_ai_Notion".
func ToolScope(server string) string {
	return "mcp__" + nonToolChar.ReplaceAllString(server, "_")
}

// IsClaudeAIConnector reports servers proxied from claude.ai, which are not
// loaded in headless (-p) runs.
func (s MCPServer) IsClaudeAIConnector() bool {
	return strings.HasPrefix(s.Name, "claude.ai ")
}
