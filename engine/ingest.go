package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/pat-eason/doctok/engine/claude"
	"github.com/pat-eason/doctok/engine/notion"
)

// Source kinds.
const (
	SourceFile   = "file"
	SourceNotion = "notion"
)

// Ingestion routes.
const (
	ViaFile      = "file"
	ViaNotionAPI = "notion-api"
	ViaClaudeMCP = "claude-mcp"
)

// MaxDocBytes caps how much of a document is sent to Claude.
const MaxDocBytes = 1_500_000

// ClassifySource figures out what the user handed us.
func ClassifySource(ref string) (SourceInfo, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return SourceInfo{}, errors.New("no source given: pass a Notion URL or a path to a Markdown file")
	}
	if notion.IsNotionURL(ref) {
		if _, err := notion.PageID(ref); err != nil {
			return SourceInfo{}, err
		}
		return SourceInfo{Kind: SourceNotion, Ref: ref}, nil
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return SourceInfo{}, fmt.Errorf("%q is a URL but not a Notion page; download it to a .md file first", ref)
	}
	p := expandHome(ref)
	abs, err := filepath.Abs(p)
	if err != nil {
		return SourceInfo{}, err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return SourceInfo{}, fmt.Errorf("can't read %s: %w", ref, err)
	}
	if st.IsDir() {
		return SourceInfo{}, fmt.Errorf("%s is a directory; point at a Markdown file", ref)
	}
	return SourceInfo{Kind: SourceFile, Ref: abs}, nil
}

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

var h1RE = regexp.MustCompile(`(?m)^#\s+(.+)$`)

// TitleFromMarkdown returns the first H1, if any.
func TitleFromMarkdown(md string) string {
	if m := h1RE.FindStringSubmatch(md); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ingest fetches the document text. Returns markdown, the route used, the
// title and any Claude usage incurred (MCP route).
func (e *Engine) ingest(ctx context.Context, src SourceInfo, workDir string, log func(string)) (string, string, string, *claude.Result, error) {
	switch src.Kind {
	case SourceFile:
		b, err := os.ReadFile(src.Ref)
		if err != nil {
			return "", "", "", nil, err
		}
		if !utf8.Valid(b) {
			return "", "", "", nil, fmt.Errorf("%s is not UTF-8 text", src.Ref)
		}
		md := string(b)
		title := TitleFromMarkdown(md)
		if title == "" {
			title = strings.TrimSuffix(filepath.Base(src.Ref), filepath.Ext(src.Ref))
		}
		return md, ViaFile, title, nil, nil
	case SourceNotion:
		mode := e.Config.Notion.Mode
		tryAPI := mode == NotionModeAPI || (mode == NotionModeAuto && e.Config.Notion.Token != "")
		var apiErr error
		if tryAPI {
			if e.Config.Notion.Token == "" {
				return "", "", "", nil, errors.New("notion.mode=api but no Notion token configured (NOTION_TOKEN or `doctok configure`)")
			}
			log("Fetching Notion page via the Notion API…")
			page, err := notion.New(e.Config.Notion.Token).FetchMarkdown(ctx, src.Ref)
			if err == nil {
				return page.Markdown, ViaNotionAPI, page.Title, nil, nil
			}
			if mode == NotionModeAPI {
				return "", "", "", nil, err
			}
			apiErr = err
			log("Notion API failed (" + err.Error() + "); trying Claude Code MCP…")
		}
		md, title, res, err := e.fetchNotionViaMCP(ctx, src.Ref, workDir, log)
		if err != nil {
			if apiErr != nil {
				return "", "", "", res, fmt.Errorf("notion API: %v; Claude MCP: %w", apiErr, err)
			}
			return "", "", "", res, err
		}
		return md, ViaClaudeMCP, title, res, nil
	}
	return "", "", "", nil, fmt.Errorf("unknown source kind %q", src.Kind)
}

// FindNotionMCP locates a Notion MCP server configured in Claude Code.
func (e *Engine) FindNotionMCP(ctx context.Context) (*claude.MCPServer, []claude.MCPServer, error) {
	servers, err := e.claudeRunner("").MCPList(ctx)
	if err != nil {
		return nil, nil, err
	}
	want := strings.ToLower(e.Config.Notion.MCPServer)
	var candidates []claude.MCPServer
	for _, s := range servers {
		name := strings.ToLower(s.Name)
		if want != "" && name != want {
			continue
		}
		if want == "" && !strings.Contains(name, "notion") && !strings.Contains(strings.ToLower(s.Target), "notion") {
			continue
		}
		candidates = append(candidates, s)
	}
	// Prefer: connected local servers > connected claude.ai connectors > anything.
	rank := func(s claude.MCPServer) int {
		r := 0
		if s.Status == "connected" {
			r += 10
		}
		if !s.IsClaudeAIConnector() {
			r += 5
		}
		return r
	}
	var best *claude.MCPServer
	for i := range candidates {
		if best == nil || rank(candidates[i]) > rank(*best) {
			best = &candidates[i]
		}
	}
	return best, servers, nil
}

type mcpFetch struct {
	OK       bool   `json:"ok"`
	Title    string `json:"title"`
	Markdown string `json:"markdown"`
	Error    string `json:"error"`
}

func (e *Engine) fetchNotionViaMCP(ctx context.Context, url, workDir string, log func(string)) (string, string, *claude.Result, error) {
	log("Looking for a Notion MCP server in Claude Code…")
	srv, _, err := e.FindNotionMCP(ctx)
	if err != nil {
		return "", "", nil, fmt.Errorf("claude mcp list: %w", err)
	}
	if srv == nil {
		return "", "", nil, errors.New("no Notion access: set a Notion integration token (NOTION_TOKEN) or add a Notion MCP server to Claude Code (`claude mcp add --transport http notion https://mcp.notion.com/mcp`)")
	}
	if srv.Status == "needs-auth" {
		return "", "", nil, fmt.Errorf("Notion MCP server %q needs authentication — run `claude` then `/mcp` to sign in", srv.Name)
	}
	if srv.IsClaudeAIConnector() {
		log(fmt.Sprintf("Note: %q is a claude.ai connector; headless Claude Code may not load it. A local `claude mcp add` server is more reliable.", srv.Name))
	}
	log(fmt.Sprintf("Fetching via Claude Code + MCP server %q…", srv.Name))
	schema := json.RawMessage(`{"type":"object","required":["ok","title","markdown","error"],"properties":{"ok":{"type":"boolean"},"title":{"type":"string"},"markdown":{"type":"string"},"error":{"type":"string"}}}`)
	prompt := fmt.Sprintf(`Use the Notion MCP tools available to you (server %q) to fetch this Notion page:
%s

Return the page's COMPLETE content converted to GitHub-flavored Markdown in "markdown"
(headings, lists, code blocks, tables, callouts as blockquotes). Do not summarize,
shorten or editorialize — reproduce the content. Put the page title in "title".
If you cannot access the page or no Notion tools are available, set ok=false and
explain why in "error" (leave markdown empty). Never make up content.`, srv.Name, url)
	r := e.claudeRunner(workDir)
	res, err := r.Run(ctx, claude.Request{
		Prompt:   prompt,
		Schema:   schema,
		AllowMCP: []string{srv.ToolScope},
		Timeout:  10 * time.Minute,
	})
	if err != nil {
		return "", "", res, err
	}
	var out mcpFetch
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return "", "", res, fmt.Errorf("parse MCP fetch result: %w", err)
	}
	if !out.OK || strings.TrimSpace(out.Markdown) == "" {
		msg := out.Error
		if msg == "" {
			msg = "Claude returned no content"
		}
		return "", "", res, fmt.Errorf("Notion via MCP failed: %s", msg)
	}
	md := out.Markdown
	if out.Title != "" && TitleFromMarkdown(md) == "" {
		md = "# " + out.Title + "\n\n" + md
	}
	return md, out.Title, res, nil
}
