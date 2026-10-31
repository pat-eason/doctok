package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseMCPList(t *testing.T) {
	out := `Checking MCP server health…

claude.ai Notion: https://mcp.notion.com/mcp - ! Needs authentication
notion: https://mcp.notion.com/mcp (HTTP) - ✔ Connected
plugin:linear:linear: https://mcp.linear.app/mcp (HTTP) - ✗ Failed to connect
`
	s := ParseMCPList(out)
	if len(s) != 3 {
		t.Fatalf("got %d servers", len(s))
	}
	if s[0].Status != "needs-auth" || !s[0].IsClaudeAIConnector() || s[0].ToolScope != "mcp__claude_ai_Notion" {
		t.Errorf("%+v", s[0])
	}
	if s[1].Status != "connected" || s[1].ToolScope != "mcp__notion" {
		t.Errorf("%+v", s[1])
	}
	if s[2].Status != "failed" || s[2].ToolScope != "mcp__plugin_linear_linear" {
		t.Errorf("%+v", s[2])
	}
}

func TestParseEnvelopeFallbacks(t *testing.T) {
	line := `{"type":"result","subtype":"success","result":"hi","total_cost_usd":0.5}`
	for _, in := range []string{line, "warning: stuff\n" + line + "\n", "[" + `{"type":"system"},` + line + "]"} {
		env, err := parseEnvelope([]byte(in))
		if err != nil || env.Result != "hi" || env.TotalCostUSD != 0.5 {
			t.Errorf("%q → %+v %v", in, env, err)
		}
	}
	if got := extractJSON("here you go:\n```json\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("extractJSON=%q", got)
	}
}

// fakeClaude writes a shell script that records its args/env and prints a canned envelope.
func fakeClaude(t *testing.T, envelope string) (bin, argsFile, envFile string) {
	dir := t.TempDir()
	argsFile = filepath.Join(dir, "args")
	envFile = filepath.Join(dir, "env")
	bin = filepath.Join(dir, "claude")
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > " + argsFile + "\nenv > " + envFile + "\ncat > /dev/null\ncat <<'EOF'\n" + envelope + "\nEOF\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

func TestRunSubscriptionVsAPI(t *testing.T) {
	env := `{"type":"result","subtype":"success","is_error":false,"result":"{\"x\":1}","structured_output":{"x":1},"total_cost_usd":0.0123,"duration_ms":42,
"modelUsage":{"claude-sonnet-5-5":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":5,"cacheCreationInputTokens":1,"costUSD":0.0123}}}`
	bin, argsFile, envFile := fakeClaude(t, env)
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-shell")

	r := &Runner{Binary: bin, Billing: BillingSubscription, Model: "sonnet"}
	res, err := r.Run(context.Background(), Request{Prompt: "doc", Schema: json.RawMessage(`{"type":"object"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if res.CostUSD != 0.0123 || res.InputTokens != 16 || res.OutputTokens != 20 || res.Model != "claude-sonnet-5-5" || string(res.Structured) != `{"x":1}` {
		t.Fatalf("%+v", res)
	}
	envDump, _ := os.ReadFile(envFile)
	if strings.Contains(string(envDump), "ANTHROPIC_API_KEY") {
		t.Fatal("subscription mode must strip ANTHROPIC_API_KEY")
	}
	args, _ := os.ReadFile(argsFile)
	for _, want := range []string{"-p", "--output-format\njson", "--model\nsonnet", "--json-schema", "--tools\n\n", "--strict-mcp-config", "--no-session-persistence"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %q:\n%s", want, args)
		}
	}

	r = &Runner{Binary: bin, Billing: BillingAPI, APIKey: "sk-configured"}
	if _, err := r.Run(context.Background(), Request{Prompt: "x", AllowMCP: []string{"mcp__notion"}}); err != nil {
		t.Fatal(err)
	}
	envDump, _ = os.ReadFile(envFile)
	if !strings.Contains(string(envDump), "ANTHROPIC_API_KEY=sk-configured") || strings.Contains(string(envDump), "sk-from-shell") {
		t.Fatalf("api mode should pass the configured key only:\n%s", envDump)
	}
	args, _ = os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--allowedTools\nmcp__notion") || strings.Contains(string(args), "--strict-mcp-config") {
		t.Fatalf("mcp args wrong:\n%s", args)
	}
}

func TestRunError(t *testing.T) {
	bin, _, _ := fakeClaude(t, `{"type":"result","subtype":"error_max_budget_usd","is_error":true,"result":"budget exceeded","total_cost_usd":1}`)
	r := &Runner{Binary: bin, Billing: BillingSubscription}
	res, err := r.Run(context.Background(), Request{Prompt: "x"})
	if err == nil || !strings.Contains(err.Error(), "budget exceeded") {
		t.Fatalf("want error, got %v", err)
	}
	if res == nil || res.CostUSD != 1 {
		t.Fatal("cost should still be reported on error")
	}
}
