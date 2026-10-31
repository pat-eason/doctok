package kling

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSignJWT(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tok, err := SignJWT("ak", "sk", now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		t.Fatalf("want 3 parts, got %d", len(parts))
	}
	mac := hmac.New(sha256.New, []byte("sk"))
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if got := base64.RawURLEncoding.EncodeToString(mac.Sum(nil)); got != parts[2] {
		t.Fatal("bad signature")
	}
	var claims map[string]any
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	_ = json.Unmarshal(b, &claims)
	if claims["iss"] != "ak" || int64(claims["exp"].(float64)) != now.Add(30*time.Minute).Unix() || int64(claims["nbf"].(float64)) != now.Unix()-5 {
		t.Fatalf("claims: %v", claims)
	}
}

func TestStandardAPI(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer key123" {
			w.WriteHeader(401)
			io.WriteString(w, `{"code":1002,"message":"Authorization is invalid"}`)
			return
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/text-to-video/kling-2.6":
			_ = json.NewDecoder(r.Body).Decode(&gotBody)
			io.WriteString(w, `{"code":0,"message":"ok","request_id":"r1","data":{"id":"task-1","status":"submitted","external_id":"ext"}}`)
		case r.Method == "GET" && r.URL.Path == "/tasks":
			if r.URL.Query().Get("task_ids") != "task-1" {
				t.Errorf("task_ids=%q", r.URL.Query().Get("task_ids"))
			}
			io.WriteString(w, `{"code":0,"data":[{"id":"task-1","status":"succeeded","outputs":[{"type":"video","url":"https://cdn/x.mp4","duration":"10"}],
				"billing":[{"charge_type":"unit","amount":"10.0","package_type":"video"}]}]}`)
		case r.URL.Path == "/account/costs":
			io.WriteString(w, `{"code":0,"data":{"code":0,"msg":"ok","resource_pack_subscribe_infos":[
				{"resource_pack_name":"Video-500","total_quantity":500,"remaining_quantity":321.5,"status":"online"},
				{"resource_pack_name":"Old","total_quantity":100,"remaining_quantity":50,"status":"expired"}]}}`)
		default:
			w.WriteHeader(404)
			io.WriteString(w, `{"code":1202,"message":"nope"}`)
		}
	}))
	defer srv.Close()

	c, err := New(srv.URL, "auto", "key123", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Style != StyleStandard {
		t.Fatalf("style=%s", c.Style)
	}
	spec := Models["kling-2.6"]
	task, err := c.CreateTextToVideo(context.Background(), CreateRequest{Model: spec, Prompt: "hi", Resolution: "1080p", Duration: 10, Audio: true, ExternalID: "ext"})
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "task-1" {
		t.Fatalf("id=%s", task.ID)
	}
	settings := gotBody["settings"].(map[string]any)
	if settings["audio"] != "native" || settings["aspect_ratio"] != "9:16" || settings["resolution"] != "1080p" || settings["duration"].(float64) != 10 {
		t.Fatalf("settings=%v", settings)
	}
	got, err := c.GetTask(context.Background(), "task-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskSucceeded || got.VideoURL != "https://cdn/x.mp4" || got.VideoDuration != 10 || got.Units != 10 || !got.CostReported {
		t.Fatalf("task=%+v", got)
	}
	acct, err := c.AccountCosts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.RemainingUnits != 321.5 {
		t.Fatalf("remaining=%v", acct.RemainingUnits)
	}

	bad, _ := New(srv.URL, "standard", "wrong", "", "")
	_, err = bad.AccountCosts(context.Background())
	if !IsFatal(err) || IsRetryable(err) {
		t.Fatalf("auth error should be fatal, got %v", err)
	}
}

func TestLegacyAPI(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
			t.Errorf("expected JWT, got %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.Method == "POST" && r.URL.Path == "/v1/videos/text2video":
			_ = json.NewDecoder(r.Body).Decode(&body)
			io.WriteString(w, `{"code":0,"data":{"task_id":"L1","task_status":"submitted"}}`)
		case r.URL.Path == "/v1/videos/text2video/L1":
			io.WriteString(w, `{"code":0,"data":{"task_id":"L1","task_status":"succeed","final_unit_deduction":"3.5",
				"task_result":{"videos":[{"url":"https://cdn/l.mp4","duration":"5.0"}]}}}`)
		}
	}))
	defer srv.Close()
	c, err := New(srv.URL, "auto", "", "ak", "sk")
	if err != nil {
		t.Fatal(err)
	}
	if c.Style != StyleLegacy {
		t.Fatal("expected legacy")
	}
	if _, err := c.CreateTextToVideo(context.Background(), CreateRequest{Model: Models["kling-2.6"], Prompt: "p", NegativePrompt: "n", Resolution: "1080p", Duration: 5, Audio: true}); err != nil {
		t.Fatal(err)
	}
	if body["model_name"] != "kling-v2-6" || body["mode"] != "pro" || body["duration"] != "5" || body["sound"] != "on" || body["aspect_ratio"] != "9:16" {
		t.Fatalf("body=%v", body)
	}
	task, err := c.GetTask(context.Background(), "L1")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskSucceeded || task.Units != 3.5 || task.VideoURL != "https://cdn/l.mp4" {
		t.Fatalf("task=%+v", task)
	}
	if _, err := c.CreateTextToVideo(context.Background(), CreateRequest{Model: Models["kling-3.0-turbo"], Prompt: "p", Resolution: "1080p", Duration: 5}); err == nil {
		t.Fatal("3.0 turbo should be rejected on legacy API")
	}
}

func TestErrorClassification(t *testing.T) {
	cases := []struct {
		e            *APIError
		retry, fatal bool
	}{
		{&APIError{HTTPStatus: 429, Code: 1303}, true, false},
		{&APIError{HTTPStatus: 429, Code: 1302}, true, false},
		{&APIError{HTTPStatus: 429, Code: 1102}, false, true},
		{&APIError{HTTPStatus: 401, Code: 1004}, false, true},
		{&APIError{HTTPStatus: 503, Code: 5001}, true, false},
		{&APIError{HTTPStatus: 400, Code: 1301}, false, false},
	}
	for _, c := range cases {
		if c.e.Retryable() != c.retry || c.e.Fatal() != c.fatal {
			t.Errorf("code %d: retry=%v fatal=%v", c.e.Code, c.e.Retryable(), c.e.Fatal())
		}
	}
}

func TestModelSpecs(t *testing.T) {
	m := Models["kling-2.6"]
	if m.SnapDuration(7) != 5 && m.SnapDuration(7) != 10 {
		t.Fatal("snap")
	}
	if m.SnapDuration(8) != 10 || m.SnapDuration(3) != 5 {
		t.Fatalf("snap 8→%d 3→%d", m.SnapDuration(8), m.SnapDuration(3))
	}
	if u := m.UnitsFor(10, "1080p", true); u != 10 {
		t.Fatalf("units=%v", u)
	}
	if err := m.Validate("720p", true); err == nil {
		t.Fatal("2.6 audio at 720p should be invalid")
	}
	if !Models["kling-3.0-turbo"].EffectiveAudio(false) || Models["kling-2.5-turbo"].EffectiveAudio(true) {
		t.Fatal("effective audio")
	}
	if s, ok := Lookup("kling-v2-6"); !ok || s.ID != "kling-2.6" {
		t.Fatal("legacy lookup")
	}
}
