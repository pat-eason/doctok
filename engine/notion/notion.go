// Package notion fetches a Notion page through the official REST API and
// renders it as Markdown.
package notion

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const apiVersion = "2022-06-28"

var idRE = regexp.MustCompile(`([0-9a-fA-F]{32}|[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`)

// IsNotionURL reports whether s looks like a Notion page link.
func IsNotionURL(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return false
	}
	h := strings.ToLower(u.Host)
	return h == "notion.so" || strings.HasSuffix(h, ".notion.so") || strings.HasSuffix(h, ".notion.site") || h == "notion.site"
}

// PageID extracts the page UUID from a Notion URL (prefers the ?p= peek param).
func PageID(raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if p := u.Query().Get("p"); p != "" {
		if m := idRE.FindString(p); m != "" {
			return hyphenate(m), nil
		}
	}
	matches := idRE.FindAllString(u.Path, -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("no Notion page id in %q", raw)
	}
	return hyphenate(matches[len(matches)-1]), nil
}

func hyphenate(id string) string {
	id = strings.ReplaceAll(strings.ToLower(id), "-", "")
	if len(id) != 32 {
		return id
	}
	return id[0:8] + "-" + id[8:12] + "-" + id[12:16] + "-" + id[16:20] + "-" + id[20:]
}

// Client is a minimal Notion API client.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
	// MaxDepth bounds recursion into nested blocks.
	MaxDepth int
}

func New(token string) *Client {
	return &Client{Token: token, BaseURL: "https://api.notion.com", HTTP: &http.Client{Timeout: 30 * time.Second}, MaxDepth: 6}
}

// APIError is a Notion error response.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	hint := ""
	switch e.Code {
	case "object_not_found":
		hint = " — share the page with your Notion integration (••• → Connections)"
	case "unauthorized":
		hint = " — check your Notion integration token"
	}
	return fmt.Sprintf("notion: %d %s: %s%s", e.Status, e.Code, e.Message, hint)
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	u := c.BaseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.Token)
		req.Header.Set("Notion-Version", apiVersion)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		resp.Body.Close()
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusTooManyRequests && attempt < 5 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt+1) * time.Second):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			var e struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			}
			_ = json.Unmarshal(b, &e)
			return &APIError{Status: resp.StatusCode, Code: e.Code, Message: e.Message}
		}
		return json.Unmarshal(b, out)
	}
}

// Me validates the token (GET /v1/users/me) and returns the bot name.
func (c *Client) Me(ctx context.Context) (string, error) {
	var u struct {
		Name string `json:"name"`
		Bot  struct {
			WorkspaceName string `json:"workspace_name"`
		} `json:"bot"`
	}
	if err := c.get(ctx, "/v1/users/me", nil, &u); err != nil {
		return "", err
	}
	if u.Bot.WorkspaceName != "" {
		return u.Name + " @ " + u.Bot.WorkspaceName, nil
	}
	return u.Name, nil
}

// Page is a fetched page.
type Page struct {
	ID       string
	Title    string
	URL      string
	Markdown string
}

// FetchMarkdown downloads a page and all nested blocks as Markdown.
func (c *Client) FetchMarkdown(ctx context.Context, pageURL string) (*Page, error) {
	id, err := PageID(pageURL)
	if err != nil {
		return nil, err
	}
	var p struct {
		ID         string                     `json:"id"`
		URL        string                     `json:"url"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := c.get(ctx, "/v1/pages/"+id, nil, &p); err != nil {
		return nil, err
	}
	title := pageTitle(p.Properties)
	var sb strings.Builder
	if title != "" {
		sb.WriteString("# " + title + "\n\n")
	}
	if err := c.renderChildren(ctx, id, 0, &sb); err != nil {
		return nil, err
	}
	md := strings.TrimSpace(sb.String()) + "\n"
	if strings.TrimSpace(strings.TrimPrefix(md, "# "+title)) == "" {
		return nil, errors.New("notion page is empty (or the integration can't see its blocks)")
	}
	return &Page{ID: id, Title: title, URL: p.URL, Markdown: md}, nil
}

func pageTitle(props map[string]json.RawMessage) string {
	for _, raw := range props {
		var p struct {
			Type  string     `json:"type"`
			Title []richText `json:"title"`
		}
		if json.Unmarshal(raw, &p) == nil && p.Type == "title" {
			return plain(p.Title)
		}
	}
	return ""
}

type richText struct {
	PlainText   string `json:"plain_text"`
	Href        string `json:"href"`
	Annotations struct {
		Bold          bool `json:"bold"`
		Italic        bool `json:"italic"`
		Strikethrough bool `json:"strikethrough"`
		Code          bool `json:"code"`
	} `json:"annotations"`
}

func plain(rt []richText) string {
	var sb strings.Builder
	for _, t := range rt {
		sb.WriteString(t.PlainText)
	}
	return sb.String()
}

func md(rt []richText) string {
	var sb strings.Builder
	for _, t := range rt {
		s := t.PlainText
		if s == "" {
			continue
		}
		if t.Annotations.Code {
			s = "`" + s + "`"
		}
		if t.Annotations.Bold {
			s = "**" + s + "**"
		}
		if t.Annotations.Italic {
			s = "*" + s + "*"
		}
		if t.Annotations.Strikethrough {
			s = "~~" + s + "~~"
		}
		if t.Href != "" {
			s = "[" + s + "](" + t.Href + ")"
		}
		sb.WriteString(s)
	}
	return sb.String()
}

type block struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	HasChildren bool   `json:"has_children"`
	raw         map[string]json.RawMessage
}

type blockBody struct {
	RichText []richText   `json:"rich_text"`
	Caption  []richText   `json:"caption"`
	Language string       `json:"language"`
	Checked  bool         `json:"checked"`
	Title    string       `json:"title"`
	URL      string       `json:"url"`
	Expr     string       `json:"expression"`
	Cells    [][]richText `json:"cells"`
	Icon     *struct {
		Emoji string `json:"emoji"`
	} `json:"icon"`
	External *struct {
		URL string `json:"url"`
	} `json:"external"`
	File *struct {
		URL string `json:"url"`
	} `json:"file"`
}

func (c *Client) children(ctx context.Context, id string) ([]block, error) {
	var out []block
	cursor := ""
	for {
		q := url.Values{"page_size": {"100"}}
		if cursor != "" {
			q.Set("start_cursor", cursor)
		}
		var resp struct {
			Results    []map[string]json.RawMessage `json:"results"`
			HasMore    bool                         `json:"has_more"`
			NextCursor string                       `json:"next_cursor"`
		}
		if err := c.get(ctx, "/v1/blocks/"+id+"/children", q, &resp); err != nil {
			return nil, err
		}
		for _, r := range resp.Results {
			var b block
			_ = json.Unmarshal(r["id"], &b.ID)
			_ = json.Unmarshal(r["type"], &b.Type)
			_ = json.Unmarshal(r["has_children"], &b.HasChildren)
			b.raw = r
			out = append(out, b)
		}
		if !resp.HasMore || resp.NextCursor == "" {
			return out, nil
		}
		cursor = resp.NextCursor
	}
}

func (c *Client) renderChildren(ctx context.Context, id string, depth int, sb *strings.Builder) error {
	blocks, err := c.children(ctx, id)
	if err != nil {
		return err
	}
	indent := strings.Repeat("  ", depth)
	num := 0
	for _, b := range blocks {
		var body blockBody
		_ = json.Unmarshal(b.raw[b.Type], &body)
		text := md(body.RichText)
		if b.Type != "numbered_list_item" {
			num = 0
		}
		descend := b.HasChildren && depth < c.MaxDepth
		switch b.Type {
		case "paragraph":
			sb.WriteString(indent + text + "\n\n")
		case "heading_1":
			sb.WriteString("# " + text + "\n\n")
		case "heading_2":
			sb.WriteString("## " + text + "\n\n")
		case "heading_3":
			sb.WriteString("### " + text + "\n\n")
		case "bulleted_list_item":
			sb.WriteString(indent + "- " + text + "\n")
		case "numbered_list_item":
			num++
			sb.WriteString(fmt.Sprintf("%s%d. %s\n", indent, num, text))
		case "to_do":
			mark := " "
			if body.Checked {
				mark = "x"
			}
			sb.WriteString(indent + "- [" + mark + "] " + text + "\n")
		case "toggle":
			sb.WriteString(indent + "- **" + text + "**\n")
		case "quote":
			sb.WriteString(indent + "> " + text + "\n\n")
		case "callout":
			icon := ""
			if body.Icon != nil && body.Icon.Emoji != "" {
				icon = body.Icon.Emoji + " "
			}
			sb.WriteString(indent + "> " + icon + text + "\n\n")
		case "code":
			sb.WriteString("```" + body.Language + "\n" + plain(body.RichText) + "\n```\n\n")
		case "equation":
			sb.WriteString("$$" + body.Expr + "$$\n\n")
		case "divider":
			sb.WriteString("---\n\n")
		case "child_page":
			sb.WriteString(indent + "📄 " + body.Title + "\n\n")
			descend = false // don't inline whole sub-pages
		case "child_database":
			sb.WriteString(indent + "🗃️ " + body.Title + "\n\n")
			descend = false
		case "image", "video", "file", "pdf":
			u := ""
			if body.External != nil {
				u = body.External.URL
			} else if body.File != nil {
				u = body.File.URL
			}
			cap := plain(body.Caption)
			if cap == "" {
				cap = b.Type
			}
			sb.WriteString(fmt.Sprintf("%s[%s: %s](%s)\n\n", indent, b.Type, cap, u))
		case "bookmark", "embed", "link_preview":
			sb.WriteString(indent + "<" + body.URL + ">\n\n")
		case "table":
			rows, err := c.children(ctx, b.ID)
			if err != nil {
				return err
			}
			for i, r := range rows {
				var rb blockBody
				_ = json.Unmarshal(r.raw["table_row"], &rb)
				cells := make([]string, len(rb.Cells))
				for j, cell := range rb.Cells {
					cells[j] = strings.ReplaceAll(md(cell), "|", "\\|")
				}
				sb.WriteString("| " + strings.Join(cells, " | ") + " |\n")
				if i == 0 {
					sep := make([]string, len(cells))
					for j := range sep {
						sep[j] = "---"
					}
					sb.WriteString("| " + strings.Join(sep, " | ") + " |\n")
				}
			}
			sb.WriteString("\n")
			descend = false
		case "column_list", "column", "synced_block", "template":
			// containers: just descend
		default:
			if text != "" {
				sb.WriteString(indent + text + "\n\n")
			}
		}
		if descend {
			childDepth := depth
			if strings.HasSuffix(b.Type, "list_item") || b.Type == "to_do" || b.Type == "toggle" {
				childDepth = depth + 1
			}
			if err := c.renderChildren(ctx, b.ID, childDepth, sb); err != nil {
				return err
			}
		}
	}
	if num > 0 || (len(blocks) > 0 && strings.HasSuffix(blocks[len(blocks)-1].Type, "list_item")) {
		sb.WriteString("\n")
	}
	return nil
}
