// Package kling is a small client for the Kling AI Open Platform video API.
//
// Two API surfaces are supported:
//
//   - standard (current): Bearer API key, POST /text-to-video/<model>,
//     GET /tasks?task_ids=…, billing reported per task.
//   - legacy: AK/SK HS256 JWT, POST /v1/videos/text2video with model_name,
//     GET /v1/videos/text2video/<id>.
package kling

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	StyleStandard = "standard"
	StyleLegacy   = "legacy"
)

// Task statuses (normalized across API styles).
const (
	TaskSubmitted  = "submitted"
	TaskProcessing = "processing"
	TaskSucceeded  = "succeeded"
	TaskFailed     = "failed"
)

// Client talks to Kling.
type Client struct {
	BaseURL   string
	Style     string
	APIKey    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client
	UserAgent string
	now       func() time.Time
}

// New builds a client; style "auto" picks standard when an API key is set,
// otherwise legacy with AK/SK.
func New(baseURL, style, apiKey, accessKey, secretKey string) (*Client, error) {
	c := &Client{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		APIKey:    apiKey,
		AccessKey: accessKey,
		SecretKey: secretKey,
		HTTP:      &http.Client{Timeout: 60 * time.Second},
		UserAgent: "doctok/1.0",
		now:       time.Now,
	}
	switch style {
	case StyleStandard, StyleLegacy:
		c.Style = style
	case "", "auto":
		if apiKey != "" {
			c.Style = StyleStandard
		} else {
			c.Style = StyleLegacy
		}
	default:
		return nil, fmt.Errorf("unknown kling api style %q", style)
	}
	if c.BaseURL == "" {
		return nil, errors.New("kling base URL is empty")
	}
	if !c.HasCredentials() {
		return nil, errors.New("no Kling credentials: set an API key (KLING_API_KEY) or access/secret keys (KLING_ACCESS_KEY / KLING_SECRET_KEY)")
	}
	return c, nil
}

// HasCredentials reports whether the client can authenticate.
func (c *Client) HasCredentials() bool {
	return c.APIKey != "" || (c.AccessKey != "" && c.SecretKey != "")
}

// AuthMode describes the credential in use.
func (c *Client) AuthMode() string {
	if c.APIKey != "" {
		return "api-key"
	}
	return "access-key/secret-key (JWT)"
}

func (c *Client) token() (string, error) {
	if c.APIKey != "" {
		return c.APIKey, nil
	}
	return SignJWT(c.AccessKey, c.SecretKey, c.now())
}

// SignJWT creates the HS256 token Kling expects for AK/SK auth.
func SignJWT(accessKey, secretKey string, now time.Time) (string, error) {
	if accessKey == "" || secretKey == "" {
		return "", errors.New("missing access key or secret key")
	}
	enc := base64.RawURLEncoding
	header, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"iss": accessKey,
		"exp": now.Add(30 * time.Minute).Unix(),
		"nbf": now.Add(-5 * time.Second).Unix(),
	})
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secretKey))
	mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// APIError is a non-zero Kling response code.
type APIError struct {
	HTTPStatus int
	Code       int
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	hint := ""
	switch e.Code {
	case 1000, 1001, 1002, 1003, 1004:
		hint = " (check your Kling credentials / API style)"
	case 1102:
		hint = " (resource pack exhausted or expired — top up Kling units)"
	case 1103:
		hint = " (your account isn't authorized for this model)"
	case 1301:
		hint = " (prompt blocked by Kling content safety)"
	case 1302, 1303:
		hint = " (rate/concurrency limit — lower kling.concurrency)"
	}
	return fmt.Sprintf("kling: HTTP %d code %d: %s%s", e.HTTPStatus, e.Code, e.Message, hint)
}

// Retryable reports transient errors (rate limits, overload, 5xx).
func (e *APIError) Retryable() bool {
	if e.Fatal() {
		return false
	}
	switch e.Code {
	case 1302, 1303, 5000, 5001, 5002:
		return true
	}
	return e.HTTPStatus >= 500 || e.HTTPStatus == http.StatusTooManyRequests
}

// Fatal reports errors that won't fix themselves (auth, money, policy).
func (e *APIError) Fatal() bool {
	switch e.Code {
	case 1000, 1001, 1002, 1003, 1004, 1100, 1101, 1102, 1103, 1304:
		return true
	}
	return false
}

// IsRetryable is a helper for arbitrary errors.
func IsRetryable(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Retryable()
	}
	var ne interface{ Timeout() bool }
	return errors.As(err, &ne) && ne.Timeout()
}

// IsFatal reports errors that should abort the whole render.
func IsFatal(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Fatal()
}

type envelope struct {
	Code      int             `json:"code"`
	Message   string          `json:"message"`
	RequestID string          `json:"request_id"`
	Data      json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any) (json.RawMessage, error) {
	u := c.BaseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	tok, err := c.token()
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	var env envelope
	if jerr := json.Unmarshal(raw, &env); jerr != nil {
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: -1, Message: fmt.Sprintf("non-JSON response: %s", snippet(raw))}
	}
	if resp.StatusCode >= 300 || env.Code != 0 {
		msg := env.Message
		if msg == "" {
			msg = snippet(raw)
		}
		return nil, &APIError{HTTPStatus: resp.StatusCode, Code: env.Code, Message: msg, RequestID: env.RequestID}
	}
	return env.Data, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// CreateRequest is a text-to-video job.
type CreateRequest struct {
	Model          ModelSpec
	Prompt         string
	NegativePrompt string // legacy only
	Resolution     string // 720p | 1080p
	AspectRatio    string // 9:16
	Duration       int
	Audio          bool
	ExternalID     string
}

// Task is a normalized task snapshot.
type Task struct {
	ID            string
	ExternalID    string
	Status        string
	Message       string
	VideoURL      string
	VideoDuration float64
	Units         float64 // units charged, if reported
	CashAmount    float64 // cash charged, if reported
	Currency      string
	CostReported  bool
}

// CreateTextToVideo submits a job and returns its task id.
func (c *Client) CreateTextToVideo(ctx context.Context, r CreateRequest) (*Task, error) {
	if r.AspectRatio == "" {
		r.AspectRatio = "9:16"
	}
	prompt := r.Prompt
	if r.Model.PromptMax > 0 && len([]rune(prompt)) > r.Model.PromptMax {
		prompt = string([]rune(prompt)[:r.Model.PromptMax])
	}
	if c.Style == StyleLegacy {
		return c.createLegacy(ctx, r, prompt)
	}
	settings := map[string]any{
		"resolution":   r.Resolution,
		"aspect_ratio": r.AspectRatio,
		"duration":     r.Duration,
	}
	if r.Model.Audio == AudioOptional {
		if r.Audio {
			settings["audio"] = "native"
		} else {
			settings["audio"] = "off"
		}
	}
	body := map[string]any{
		"prompt":   prompt,
		"settings": settings,
		"options":  map[string]any{"external_task_id": r.ExternalID},
	}
	data, err := c.do(ctx, http.MethodPost, "/text-to-video/"+url.PathEscape(r.Model.ID), nil, body)
	if err != nil {
		return nil, err
	}
	var d struct {
		ID         string `json:"id"`
		TaskID     string `json:"task_id"`
		Status     string `json:"status"`
		ExternalID string `json:"external_id"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("kling create: %w", err)
	}
	id := d.ID
	if id == "" {
		id = d.TaskID
	}
	if id == "" {
		return nil, errors.New("kling create: response had no task id")
	}
	return &Task{ID: id, ExternalID: d.ExternalID, Status: normStatus(d.Status)}, nil
}

func (c *Client) createLegacy(ctx context.Context, r CreateRequest, prompt string) (*Task, error) {
	name := r.Model.LegacyName
	if name == "" {
		return nil, fmt.Errorf("model %s is not available on the legacy Kling API; use api_style=standard with an API key", r.Model.ID)
	}
	mode := "std"
	if r.Resolution == "1080p" {
		mode = "pro"
	}
	body := map[string]any{
		"model_name":       name,
		"prompt":           prompt,
		"mode":             mode,
		"aspect_ratio":     r.AspectRatio,
		"duration":         strconv.Itoa(r.Duration),
		"external_task_id": r.ExternalID,
	}
	if r.NegativePrompt != "" {
		body["negative_prompt"] = r.NegativePrompt
	}
	if r.Model.Audio != AudioNone {
		if r.Model.EffectiveAudio(r.Audio) {
			body["sound"] = "on"
		} else {
			body["sound"] = "off"
		}
	}
	data, err := c.do(ctx, http.MethodPost, "/v1/videos/text2video", nil, body)
	if err != nil {
		return nil, err
	}
	var d struct {
		TaskID     string `json:"task_id"`
		TaskStatus string `json:"task_status"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("kling create: %w", err)
	}
	if d.TaskID == "" {
		return nil, errors.New("kling create: response had no task_id")
	}
	return &Task{ID: d.TaskID, ExternalID: r.ExternalID, Status: normStatus(d.TaskStatus)}, nil
}

// GetTask polls one task.
func (c *Client) GetTask(ctx context.Context, id string) (*Task, error) {
	if c.Style == StyleLegacy {
		return c.getLegacy(ctx, id)
	}
	data, err := c.do(ctx, http.MethodGet, "/tasks", url.Values{"task_ids": {id}}, nil)
	if err != nil {
		return nil, err
	}
	var arr []standardTask
	if err := json.Unmarshal(data, &arr); err != nil {
		// Some deployments may wrap as {result:[…]} — accept that too.
		var wrapped struct {
			Result []standardTask `json:"result"`
		}
		if err2 := json.Unmarshal(data, &wrapped); err2 != nil {
			return nil, fmt.Errorf("kling get task: %w", err)
		}
		arr = wrapped.Result
	}
	for _, t := range arr {
		if t.ID == id {
			return t.normalize(), nil
		}
	}
	if len(arr) == 1 {
		return arr[0].normalize(), nil
	}
	return nil, &APIError{HTTPStatus: 404, Code: 1203, Message: "task " + id + " not found"}
}

type standardTask struct {
	ID         string `json:"id"`
	Status     string `json:"status"`
	Message    string `json:"message"`
	ExternalID string `json:"external_id"`
	Outputs    []struct {
		Type     string          `json:"type"`
		URL      string          `json:"url"`
		Duration json.RawMessage `json:"duration"`
	} `json:"outputs"`
	Billing []struct {
		ChargeType string          `json:"charge_type"`
		Amount     json.RawMessage `json:"amount"`
		Currency   string          `json:"currency"`
	} `json:"billing"`
}

func (t standardTask) normalize() *Task {
	out := &Task{ID: t.ID, ExternalID: t.ExternalID, Status: normStatus(t.Status), Message: t.Message}
	for _, o := range t.Outputs {
		if o.Type == "" || o.Type == "video" {
			out.VideoURL = o.URL
			out.VideoDuration = flexFloat(o.Duration)
			break
		}
	}
	for _, b := range t.Billing {
		amt := flexFloat(b.Amount)
		switch b.ChargeType {
		case "unit":
			out.Units += amt
			out.CostReported = true
		case "cash":
			out.CashAmount += amt
			out.Currency = b.Currency
			out.CostReported = true
		}
	}
	return out
}

func (c *Client) getLegacy(ctx context.Context, id string) (*Task, error) {
	data, err := c.do(ctx, http.MethodGet, "/v1/videos/text2video/"+url.PathEscape(id), nil, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		TaskID             string          `json:"task_id"`
		TaskStatus         string          `json:"task_status"`
		TaskStatusMsg      string          `json:"task_status_msg"`
		FinalUnitDeduction json.RawMessage `json:"final_unit_deduction"`
		TaskInfo           struct {
			ExternalTaskID string `json:"external_task_id"`
		} `json:"task_info"`
		TaskResult struct {
			Videos []struct {
				URL      string          `json:"url"`
				Duration json.RawMessage `json:"duration"`
			} `json:"videos"`
		} `json:"task_result"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("kling get task: %w", err)
	}
	t := &Task{ID: d.TaskID, ExternalID: d.TaskInfo.ExternalTaskID, Status: normStatus(d.TaskStatus), Message: d.TaskStatusMsg}
	if len(d.TaskResult.Videos) > 0 {
		t.VideoURL = d.TaskResult.Videos[0].URL
		t.VideoDuration = flexFloat(d.TaskResult.Videos[0].Duration)
	}
	if len(d.FinalUnitDeduction) > 0 && string(d.FinalUnitDeduction) != "null" {
		t.Units = flexFloat(d.FinalUnitDeduction)
		t.CostReported = true
	}
	return t, nil
}

func normStatus(s string) string {
	switch strings.ToLower(s) {
	case "succeed", "succeeded", "success", "completed":
		return TaskSucceeded
	case "failed", "fail", "error":
		return TaskFailed
	case "processing", "running", "in_progress":
		return TaskProcessing
	default:
		return TaskSubmitted
	}
}

func flexFloat(raw json.RawMessage) float64 {
	if len(raw) == 0 {
		return 0
	}
	var f float64
	if json.Unmarshal(raw, &f) == nil {
		return f
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		f, _ = strconv.ParseFloat(strings.TrimSpace(s), 64)
	}
	return f
}

// ResourcePack is one prepaid unit package.
type ResourcePack struct {
	Name      string  `json:"resource_pack_name"`
	ID        string  `json:"resource_pack_id"`
	Type      string  `json:"resource_pack_type"`
	Total     float64 `json:"total_quantity"`
	Remaining float64 `json:"remaining_quantity"`
	Status    string  `json:"status"`
	InvalidMS int64   `json:"invalid_time"`
}

// Account is the account/resource-pack summary.
type Account struct {
	Packs          []ResourcePack
	RemainingUnits float64 // sum of online packs (Kling notes up to ~12h reporting delay)
}

// AccountCosts queries GET /account/costs (free; QPS ≤ 1). Doubles as an auth check.
func (c *Client) AccountCosts(ctx context.Context) (*Account, error) {
	end := c.now()
	start := end.AddDate(0, 0, -365)
	q := url.Values{
		"start_time": {strconv.FormatInt(start.UnixMilli(), 10)},
		"end_time":   {strconv.FormatInt(end.UnixMilli(), 10)},
	}
	data, err := c.do(ctx, http.MethodGet, "/account/costs", q, nil)
	if err != nil {
		return nil, err
	}
	var d struct {
		Code  int            `json:"code"`
		Msg   string         `json:"msg"`
		Packs []ResourcePack `json:"resource_pack_subscribe_infos"`
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, fmt.Errorf("kling account: %w", err)
	}
	if d.Code != 0 {
		return nil, &APIError{HTTPStatus: 200, Code: d.Code, Message: d.Msg}
	}
	acct := &Account{Packs: d.Packs}
	for _, p := range d.Packs {
		if p.Status == "" || p.Status == "online" {
			acct.RemainingUnits += p.Remaining
		}
	}
	return acct, nil
}

// Download saves a finished video to path.
func (c *Client) Download(ctx context.Context, videoURL, path string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, videoURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	hc := &http.Client{Timeout: 10 * time.Minute}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: HTTP %d", videoURL, resp.StatusCode)
	}
	tmp := path + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return n, os.Rename(tmp, path)
}
