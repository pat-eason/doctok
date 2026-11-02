package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/pat-eason/doctok/engine/claude"
	"github.com/pat-eason/doctok/engine/kling"
)

// KlingStyleMCP is recorded as the "API style" of generations rendered through
// the Kling MCP server.
const KlingStyleMCP = "mcp"

// mcpPollFloor is the minimum poll interval for the MCP backend: every poll is
// a headless Claude run, so polling at the API cadence (10s) would burn Claude
// usage for no benefit.
const mcpPollFloor = 30 * time.Second

// mcpModel is the cheap model used to relay MCP calls. The calls are pure
// plumbing (one tool call, copy fields out), so a small model is plenty.
const mcpModel = "haiku"

// klingMCP implements KlingAPI over the Kling MCP server. The MCP server is
// OAuth-only, so it can't be called with an API key from Go; instead each call
// is one headless `claude -p` run that is only allowed to use the Kling tools
// and returns a small structured result.
type klingMCP struct {
	runner *claude.Runner
	scope  string // tool prefix, e.g. "mcp__kling"
	server string
	trace  string // UUIDv7 grouping every call of this render
	ua     string
}

// Result envelope returned by the relay runs.
type mcpResult struct {
	OK           bool    `json:"ok"`
	GenerationID string  `json:"generation_id"`
	Status       string  `json:"status"` // submitted | processing | succeeded | failed
	VideoURL     string  `json:"video_url"`
	ImageURL     string  `json:"image_url"`
	UploadURL    string  `json:"upload_url"`
	UploadTicket string  `json:"upload_ticket"`
	Credits      float64 `json:"credits"`
	CreditsKnown bool    `json:"credits_known"`
	ErrorKind    string  `json:"error_kind"` // none | auth | no_credits | busy | blocked | not_found | other
	Error        string  `json:"error"`
}

const mcpSchema = `{"type":"object","required":["ok","generation_id","status","video_url","credits","credits_known","error_kind","error"],"properties":{` +
	`"ok":{"type":"boolean"},"generation_id":{"type":"string"},` +
	`"status":{"type":"string","enum":["submitted","processing","succeeded","failed"]},` +
	`"video_url":{"type":"string"},"image_url":{"type":"string"},"upload_url":{"type":"string"},"upload_ticket":{"type":"string"},"credits":{"type":"number"},"credits_known":{"type":"boolean"},` +
	`"error_kind":{"type":"string","enum":["none","auth","no_credits","busy","blocked","not_found","other"]},` +
	`"error":{"type":"string"}}}`

const mcpSystem = `You are a thin relay between a program and the Kling MCP server. Make exactly the tool calls you are told to make, copy the outcome into the required JSON fields, and nothing else.
Rules: never call a generation tool (text_to_video, image_to_video, motion_control, omni_ref_video, text_to_image, image_to_image) more than once, never retry a failed generation call, never call tools you were not told to call, and never invent values: if a field is not in the tool result, leave it empty/0/false. The user has already approved the credit spend for the generation you are asked to submit.
On a tool error set ok=false and classify error_kind: auth (not signed in / token expired), no_credits (insufficient credits or membership), busy (rate/concurrency limit, try again later), blocked (content policy), not_found (unknown generation id), other. Put the tool's error text in "error".`

func (e *Engine) newKlingBackend(ctx context.Context, cfg KlingConfig) (KlingAPI, string, error) {
	if cfg.Backend == KlingBackendMCP {
		return e.newKlingMCP(ctx, cfg)
	}
	return e.NewKling(cfg)
}

func (e *Engine) newKlingMCP(ctx context.Context, cfg KlingConfig) (KlingAPI, string, error) {
	srv, err := e.findKlingMCP(ctx, cfg.MCPServer)
	if err != nil {
		return nil, "", err
	}
	if _, err := e.resolveBilling(ctx); err != nil {
		return nil, "", err
	}
	r := e.claudeRunner(e.Home)
	r.Model = mcpModel
	return &klingMCP{runner: r, scope: srv.ToolScope, server: srv.Name, trace: uuidV7(), ua: "doctok/1.0"}, KlingStyleMCP, nil
}

// findKlingMCP locates the Kling MCP server configured in Claude Code.
func (e *Engine) findKlingMCP(ctx context.Context, want string) (*claude.MCPServer, error) {
	// List from the same directory the relay runs use, so project-scoped servers
	// are only reported when headless runs can actually load them.
	servers, err := e.claudeRunner(e.Home).MCPList(ctx)
	if err != nil {
		return nil, fmt.Errorf("claude mcp list: %w", err)
	}
	want = strings.ToLower(want)
	var best *claude.MCPServer
	for i, s := range servers {
		name := strings.ToLower(s.Name)
		switch {
		case want != "" && name != want:
			continue
		case want == "" && !strings.Contains(name, "kling") && !strings.Contains(strings.ToLower(s.Target), "kling"):
			continue
		case s.IsClaudeAIConnector():
			continue // not loaded in headless runs
		}
		if best == nil || (s.Status == "connected" && best.Status != "connected") {
			best = &servers[i]
		}
	}
	if best == nil {
		return nil, errors.New("no Kling MCP server in Claude Code: add one with `claude mcp add --scope user --transport http kling https://kling.ai/mcp`, then run `claude` → `/mcp` to sign in (or set kling.mcp_server)")
	}
	if best.Status == "needs-auth" {
		return nil, fmt.Errorf("Kling MCP server %q needs authentication — run `claude` then `/mcp` to sign in", best.Name)
	}
	return best, nil
}

func (k *klingMCP) call(ctx context.Context, tools []string, prompt string) (*mcpResult, error) {
	allow := make([]string, len(tools))
	for i, t := range tools {
		allow[i] = k.scope + "__" + t
	}
	res, err := k.runner.Run(ctx, claude.Request{
		Prompt:       prompt,
		SystemPrompt: mcpSystem,
		Schema:       json.RawMessage(mcpSchema),
		AllowMCP:     allow,
		Timeout:      6 * time.Minute,
	})
	if err != nil {
		return nil, fmt.Errorf("kling mcp: %w", err)
	}
	var out mcpResult
	if err := json.Unmarshal(res.Structured, &out); err != nil {
		return nil, fmt.Errorf("kling mcp: parse result: %w", err)
	}
	if !out.OK {
		return nil, out.apiError()
	}
	return &out, nil
}

// apiError maps a relay failure onto the engine's existing error classes so
// Fatal/Retryable handling (abort on money/auth, back off on busy) just works.
func (r mcpResult) apiError() error {
	code, status := -1, 0
	switch r.ErrorKind {
	case "auth":
		code = 1001
	case "no_credits":
		code = 1102
	case "busy":
		code, status = 1303, http.StatusTooManyRequests
	case "blocked":
		code = 1301
	case "not_found":
		code = 1203
	}
	msg := r.Error
	if msg == "" {
		msg = "unspecified failure"
	}
	return &kling.APIError{HTTPStatus: status, Code: code, Message: msg}
}

func (k *klingMCP) CreateTextToVideo(ctx context.Context, r kling.CreateRequest) (*kling.Task, error) {
	if r.Model.MCPName == "" {
		return nil, fmt.Errorf("model %s isn't available on the Kling MCP server; pick kling-2.6, kling-3.0 or kling-3.0-turbo", r.Model.ID)
	}
	if r.AspectRatio == "" {
		r.AspectRatio = "9:16"
	}
	prompt := r.Prompt
	if r.Model.PromptMax > 0 && len([]rune(prompt)) > r.Model.PromptMax {
		prompt = string([]rune(prompt)[:r.Model.PromptMax])
	}
	tool := "text_to_video"
	args := []map[string]string{
		{"name": "prompt", "value": prompt},
		{"name": "duration", "value": strconv.Itoa(r.Duration)},
		{"name": "resolution", "value": r.Resolution},
		{"name": "imageCount", "value": "1"}, // one video per clip; the default can multiply the spend
	}
	payload := map[string]any{
		"model":       r.Model.MCPName,
		"taskTraceId": k.trace,
		"rationale":   "doctok renders one vertical clip of an explainer video; settings are fixed by the user's doctok config",
	}
	if r.FirstImageURL != "" {
		// Image-to-video follows the first frame's aspect ratio (the tool has no
		// aspect_ratio argument), so the hero image itself must already be 9:16.
		tool = "image_to_video"
		payload["inputs"] = []map[string]string{{"name": "first_image", "inputType": "URL", "url": r.FirstImageURL}}
	} else {
		args = append(args, map[string]string{"name": "aspect_ratio", "value": r.AspectRatio})
	}
	if r.Model.Audio == kling.AudioOptional {
		args = append(args, map[string]string{"name": "enable_audio", "value": strconv.FormatBool(r.Audio)})
	}
	if r.Model.MCPName == "kling-video-v3_0" {
		// 3.0 defaults to smart multi-shot splitting, which would cut inside a clip.
		args = append(args, map[string]string{"name": "prefer_multi_shots", "value": "false"})
	}
	payload["arguments"] = args
	call, _ := json.Marshal(payload)
	res, err := k.call(ctx, []string{tool}, fmt.Sprintf(`Call %s__%s EXACTLY ONCE with precisely this JSON as its input (do not add, drop or change anything, and do not call who_am_i first):
%s

Then return generation_id = the generation id from the result (the value query_tasks takes as generationId), status = "submitted" (or the status the result reports), ok=true.
If the call errors, return ok=false and do NOT call it again.`, k.scope, tool, call))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(res.GenerationID) == "" {
		return nil, &kling.APIError{Code: -1, Message: "submit succeeded but the Kling MCP returned no generation id (check the Kling web app before retrying: a clip may have been charged)"}
	}
	return &kling.Task{ID: strings.TrimSpace(res.GenerationID), ExternalID: r.ExternalID, Status: kling.TaskSubmitted}, nil
}

func (k *klingMCP) GetTask(ctx context.Context, id string) (*kling.Task, error) {
	res, err := k.call(ctx, []string{"query_tasks"}, fmt.Sprintf(`Call %s__query_tasks once with generationId %q and taskTraceId %q. Do not call any other tool.
Return: generation_id (same id), status = submitted/processing while it is queued or running, succeeded when a finished video is available, failed if it failed; video_url = the direct https URL of the finished VIDEO file (empty unless succeeded; never a page/share link or a thumbnail); credits/credits_known = credits charged if the result says so; on failure put the reason in "error".`,
		k.scope, id, k.trace))
	if err != nil {
		return nil, err
	}
	t := &kling.Task{ID: id, Status: kling.TaskSubmitted, Message: res.Error, VideoURL: strings.TrimSpace(res.VideoURL)}
	switch res.Status {
	case "processing":
		t.Status = kling.TaskProcessing
	case "succeeded":
		t.Status = kling.TaskSucceeded
	case "failed":
		t.Status = kling.TaskFailed
	}
	if t.Status == kling.TaskSucceeded && t.VideoURL == "" {
		t.Status = kling.TaskProcessing // finished but no URL yet; poll again
	}
	if res.CreditsKnown && res.Credits > 0 {
		t.Units, t.CostReported = res.Credits, true
	}
	return t, nil
}

// heroImageModel renders the presenter reference image. kling-image-v3_0
// supports 9:16 and is built for consistency; one image costs a few credits.
const heroImageModel = "kling-image-v3_0"

// CreateImage submits one text-to-image job and returns its generation id.
func (k *klingMCP) CreateImage(ctx context.Context, prompt string) (string, error) {
	call, _ := json.Marshal(map[string]any{
		"model": heroImageModel,
		"arguments": []map[string]string{
			{"name": "prompt", "value": prompt},
			{"name": "aspect_ratio", "value": "9:16"},
			{"name": "img_resolution", "value": "2k"},
			{"name": "imageCount", "value": "1"},
		},
		"taskTraceId": k.trace,
		"rationale":   "doctok presenter reference image: every clip of the video is animated from this one frame so the presenter and set stay consistent",
	})
	res, err := k.call(ctx, []string{"text_to_image"}, fmt.Sprintf(`Call %s__text_to_image EXACTLY ONCE with precisely this JSON as its input (do not add, drop or change anything, and do not call who_am_i first):
%s

Then return generation_id = the generation id from the result (the value query_tasks takes as generationId), status = "submitted", ok=true.
If the call errors, return ok=false and do NOT call it again.`, k.scope, call))
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(res.GenerationID) == "" {
		return "", &kling.APIError{Code: -1, Message: "image submit succeeded but the Kling MCP returned no generation id (check the Kling web app before retrying: an image may have been charged)"}
	}
	return strings.TrimSpace(res.GenerationID), nil
}

// GetImage polls an image job. url is empty until the image is ready.
func (k *klingMCP) GetImage(ctx context.Context, id string) (status, url string, err error) {
	res, err := k.call(ctx, []string{"query_tasks"}, fmt.Sprintf(`Call %s__query_tasks once with generationId %q and taskTraceId %q. Do not call any other tool.
Return: generation_id (same id), status = submitted/processing while queued or running, succeeded when the finished IMAGE is available, failed if it failed; image_url = the direct https URL of the finished IMAGE file (png/jpg; empty unless succeeded; never a page/share link or a thumbnail; leave video_url empty); on failure put the reason in "error".`,
		k.scope, id, k.trace))
	if err != nil {
		return "", "", err
	}
	status = res.Status
	url = strings.TrimSpace(res.ImageURL)
	if status == "succeeded" && url == "" {
		status = "processing" // finished but no URL yet; poll again
	}
	return status, url, nil
}

func (k *klingMCP) Download(ctx context.Context, url, path string) (int64, error) {
	return kling.DownloadFile(ctx, url, path, k.ua)
}

// AccountCosts reports the account's remaining credits (free, read-only). It
// doubles as the auth check for `doctok doctor`.
func (k *klingMCP) AccountCosts(ctx context.Context) (*kling.Account, error) {
	res, err := k.call(ctx, []string{"query_membership_and_credits"}, fmt.Sprintf(`Call %s__query_membership_and_credits once (no other tool). Return credits = availableRemainCredits, credits_known = true, status = "succeeded", generation_id = "", video_url = "". Put the membership type in "error" as plain text like "membership: Pro" (this is informational, ok stays true).`, k.scope))
	if err != nil {
		return nil, err
	}
	return &kling.Account{
		Packs:          []kling.ResourcePack{{Name: strings.TrimPrefix(strings.TrimSpace(res.Error), "membership: "), Remaining: res.Credits, Status: "online"}},
		RemainingUnits: res.Credits,
	}, nil
}

// uuidV7 returns an RFC 9562 UUIDv7 (the Kling MCP groups calls by one).
func uuidV7() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	ms := uint64(time.Now().UnixMilli())
	b[0], b[1], b[2], b[3], b[4], b[5] = byte(ms>>40), byte(ms>>32), byte(ms>>24), byte(ms>>16), byte(ms>>8), byte(ms)
	b[6] = b[6]&0x0f | 0x70
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// allowUploadHost gates where a photo may be sent. The upload URL comes from a
// Claude relay run, so it is checked here instead of trusted: only Kling's own
// hosts, over https. (Tests override this.)
var allowUploadHost = func(u *url.URL) bool {
	h := strings.ToLower(u.Hostname())
	return u.Scheme == "https" && (h == "kling.ai" || strings.HasSuffix(h, ".kling.ai") || h == "klingai.com" || strings.HasSuffix(h, ".klingai.com"))
}

// UploadImage sends a local JPG/PNG to Kling and returns its hosted URL (usable
// as an image_to_video first frame). Uploading is free; the one-time ticket
// comes from the MCP's file_upload tool and the bytes are POSTed by doctok.
func (k *klingMCP) UploadImage(ctx context.Context, path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	ctype := "image/jpeg"
	if strings.EqualFold(filepath.Ext(path), ".png") {
		ctype = "image/png"
	}
	call, _ := json.Marshal(map[string]any{
		"filename": filepath.Base(path), "contentType": ctype, "size": st.Size(), "taskTraceId": k.trace,
	})
	res, err := k.call(ctx, []string{"file_upload"}, fmt.Sprintf(`Call %s__file_upload EXACTLY ONCE with precisely this JSON as its input:
%s

Then return upload_ticket = the "ticket" string and upload_url = the "uploadUrl" string from the result, exactly as given, ok=true, status="succeeded", generation_id="". Do not try to upload anything yourself and do not call any other tool.`, k.scope, call))
	if err != nil {
		return "", err
	}
	u, err := url.Parse(strings.TrimSpace(res.UploadURL))
	if err != nil || !allowUploadHost(u) {
		return "", fmt.Errorf("refusing to upload your photo to an unexpected address (%q); expected a Kling https host", res.UploadURL)
	}
	if strings.TrimSpace(res.UploadTicket) == "" {
		return "", errors.New("kling mcp: file_upload returned no ticket")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("ticket", strings.TrimSpace(res.UploadTicket))
	hdr := make(map[string][]string)
	hdr["Content-Disposition"] = []string{fmt.Sprintf(`form-data; name="file"; filename=%q`, filepath.Base(path))}
	hdr["Content-Type"] = []string{ctype}
	part, err := mw.CreatePart(hdr)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(part, f); err != nil {
		return "", err
	}
	if err := mw.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("User-Agent", k.ua)
	resp, err := (&http.Client{Timeout: 2 * time.Minute}).Do(req)
	if err != nil {
		return "", fmt.Errorf("upload photo: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out struct {
		Status  int    `json:"status"`
		Result  int    `json:"result"`
		Message string `json:"message"`
		Data    struct {
			URL string `json:"url"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || resp.StatusCode != http.StatusOK || out.Data.URL == "" {
		msg := out.Message
		if msg == "" {
			msg = snippetStr(raw)
		}
		return "", fmt.Errorf("upload photo: HTTP %d: %s", resp.StatusCode, msg)
	}
	return out.Data.URL, nil
}

func snippetStr(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
