package notion

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPageID(t *testing.T) {
	want := "1a2b3c4d-5e6f-4a1b-8c9d-0e1f2a3b4c5d"
	for _, u := range []string{
		"https://www.notion.so/acme/Payments-RFC-1a2b3c4d5e6f4a1b8c9d0e1f2a3b4c5d",
		"https://www.notion.so/1a2b3c4d5e6f4a1b8c9d0e1f2a3b4c5d?pvs=4",
		"https://acme.notion.site/Doc-1a2b3c4d-5e6f-4a1b-8c9d-0e1f2a3b4c5d",
		"https://www.notion.so/acme/ffffffffffffffffffffffffffffffff?p=1a2b3c4d5e6f4a1b8c9d0e1f2a3b4c5d&pm=s",
	} {
		got, err := PageID(u)
		if err != nil || got != want {
			t.Errorf("%s → %q, %v", u, got, err)
		}
		if !IsNotionURL(u) {
			t.Errorf("%s not detected as notion", u)
		}
	}
	if IsNotionURL("https://example.com/notion.so") || IsNotionURL("./docs/notion.md") {
		t.Error("false positive")
	}
	if _, err := PageID("https://www.notion.so/acme/no-id-here"); err == nil {
		t.Error("expected error")
	}
}

func TestFetchMarkdown(t *testing.T) {
	const pid = "1a2b3c4d-5e6f-4a1b-8c9d-0e1f2a3b4c5d"
	rt := func(s string) string { return `[{"plain_text":"` + s + `","annotations":{}}]` }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Notion-Version") == "" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			io.WriteString(w, `{"code":"unauthorized","message":"bad"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/pages/" + pid:
			io.WriteString(w, `{"id":"`+pid+`","url":"u","properties":{"Name":{"type":"title","title":`+rt("Payments RFC")+`}}}`)
		case "/v1/blocks/" + pid + "/children":
			if r.URL.Query().Get("start_cursor") == "" {
				io.WriteString(w, `{"has_more":true,"next_cursor":"c2","results":[
					{"id":"b1","type":"heading_2","heading_2":{"rich_text":`+rt("Overview")+`}},
					{"id":"b2","type":"paragraph","paragraph":{"rich_text":[{"plain_text":"Use ","annotations":{}},{"plain_text":"tokens","annotations":{"bold":true}}]}},
					{"id":"b3","type":"bulleted_list_item","has_children":true,"bulleted_list_item":{"rich_text":`+rt("parent")+`}}]}`)
				return
			}
			io.WriteString(w, `{"has_more":false,"results":[
				{"id":"b4","type":"code","code":{"language":"go","rich_text":`+rt("x := 1")+`}},
				{"id":"b5","type":"table","has_children":true,"table":{}}]}`)
		case "/v1/blocks/b3/children":
			io.WriteString(w, `{"has_more":false,"results":[{"id":"b31","type":"bulleted_list_item","bulleted_list_item":{"rich_text":`+rt("child")+`}}]}`)
		case "/v1/blocks/b5/children":
			io.WriteString(w, `{"has_more":false,"results":[
				{"id":"r1","type":"table_row","table_row":{"cells":[`+rt("k")+`,`+rt("v")+`]}},
				{"id":"r2","type":"table_row","table_row":{"cells":[`+rt("a|b")+`,`+rt("1")+`]}}]}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"code":"object_not_found","message":"nope"}`)
		}
	}))
	defer srv.Close()
	c := New("tok")
	c.BaseURL = srv.URL
	page, err := c.FetchMarkdown(context.Background(), "https://www.notion.so/acme/Payments-RFC-1a2b3c4d5e6f4a1b8c9d0e1f2a3b4c5d")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"# Payments RFC", "## Overview", "Use **tokens**", "- parent", "  - child", "```go\nx := 1\n```", "| k | v |", "| --- | --- |", `| a\|b | 1 |`} {
		if !strings.Contains(page.Markdown, want) {
			t.Errorf("missing %q in:\n%s", want, page.Markdown)
		}
	}
	bad := New("nope")
	bad.BaseURL = srv.URL
	if _, err := bad.FetchMarkdown(context.Background(), "https://notion.so/"+strings.ReplaceAll(pid, "-", "")); err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("want unauthorized, got %v", err)
	}
}
