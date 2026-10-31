package ui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuardAndAPI(t *testing.T) {
	home := t.TempDir()
	h := New(context.Background(), home).Handler()
	do := func(method, target, host string, hdr map[string]string, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, target, strings.NewReader(body))
		r.Host = host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := do("GET", "/api/generations", "evil.example.com", nil, ""); w.Code != 403 {
		t.Fatalf("foreign host allowed: %d", w.Code)
	}
	if w := do("GET", "/", "localhost:7878", nil, ""); w.Code != 200 || !strings.Contains(w.Body.String(), "doctok") {
		t.Fatalf("index: %d", w.Code)
	}
	if w := do("POST", "/api/generations", "localhost:7878", map[string]string{"Content-Type": "application/json"}, `{}`); w.Code != 403 {
		t.Fatalf("missing X-Doctok header allowed: %d", w.Code)
	}
	if w := do("POST", "/api/generations", "localhost:7878", map[string]string{"X-Doctok": "1", "Origin": "https://evil.example.com"}, `{}`); w.Code != 403 {
		t.Fatalf("cross-origin allowed: %d", w.Code)
	}
	// Valid request with a bad source → 400 from the engine, not a guard error.
	w := do("POST", "/api/generations", "localhost:7878", map[string]string{"X-Doctok": "1", "Origin": "http://localhost:7878"}, `{"source":"/nope.md","dry_run":true}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "can't read") {
		t.Fatalf("create: %d %s", w.Code, w.Body.String())
	}
	w = do("GET", "/api/meta", "127.0.0.1:7878", nil, "")
	var meta map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil || len(meta["styles"].([]any)) < 5 {
		t.Fatalf("meta: %s", w.Body.String())
	}
	// File endpoint only serves the allow-listed artifacts.
	_ = os.WriteFile(filepath.Join(home, "secret.txt"), []byte("x"), 0o600)
	if w := do("GET", "/api/generations/x/files/..%2Fsecret.txt", "localhost", nil, ""); w.Code != 404 {
		t.Fatalf("file traversal: %d", w.Code)
	}
	_ = http.StatusOK
}
