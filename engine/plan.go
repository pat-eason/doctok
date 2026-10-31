package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pat-eason/doctok/engine/claude"
	"github.com/pat-eason/doctok/engine/kling"
)

// Plan runs ingest → distill → storyboard and stops at awaiting_approval with
// a cost estimate. Completed stages are skipped, so Plan is resumable.
func (e *Engine) Plan(ctx context.Context, key string, obs Observer) (*Generation, error) {
	if cur, err := e.Store.Get(key); err != nil {
		return nil, err
	} else if cur.Status == StatusSucceeded || (cur.Status == StatusAwaitingApproval && e.stageDone(cur, StageScript)) {
		return cur, nil
	}
	g, err := e.claim(key)
	if err != nil {
		return nil, err
	}
	e.emit(obs, EventCreated, g.Stage, "started", g)
	if _, err := e.resolveBilling(ctx); err != nil {
		return nil, e.fail(ctx, key, obs, err)
	}

	if !e.stageDone(g, StageIngest) || !fileExists(filepath.Join(g.Dir, FileDoc)) {
		if err := e.stageIngest(ctx, g, obs); err != nil {
			return nil, e.fail(ctx, key, obs, err)
		}
	}
	if g, err = e.Store.Get(key); err != nil {
		return nil, err
	}
	if !e.stageDone(g, StageDistill) || !fileExists(filepath.Join(g.Dir, FileDistill)) {
		if err := e.stageDistill(ctx, g, obs); err != nil {
			return nil, e.fail(ctx, key, obs, err)
		}
	}
	if g, err = e.Store.Get(key); err != nil {
		return nil, err
	}
	if !e.stageDone(g, StageScript) || !fileExists(filepath.Join(g.Dir, FileScript)) {
		if err := e.stageScript(ctx, g, obs); err != nil {
			return nil, e.fail(ctx, key, obs, err)
		}
	}
	est, _ := e.Estimate(key)
	g, err = e.Store.Update(key, func(g *Generation) error {
		g.Status = StatusAwaitingApproval
		g.OwnerPID = 0
		g.Stage = StageRender
		if est != nil {
			g.Kling.EstimatedUnits = est.Units
			g.Message = fmt.Sprintf("Storyboard ready: %d scenes, %ds — est. %.1f Kling units (~$%.2f)", est.Scenes, est.Seconds, est.Units, est.USD)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.emit(obs, EventApproval, StageRender, g.Message, g)
	return g, nil
}

func (e *Engine) stageIngest(ctx context.Context, g *Generation, obs Observer) error {
	if _, err := e.beginStage(g.Key, StageIngest, obs); err != nil {
		return err
	}
	log := func(m string) { e.logf(obs, g.Key, StageIngest, "%s", m) }
	md, via, title, res, err := e.ingest(ctx, g.Source, g.Dir, log)
	e.recordClaude(g.Key, "notion-fetch", res, obs)
	if err != nil {
		return err
	}
	if strings.TrimSpace(md) == "" {
		return errors.New("document is empty")
	}
	if len(md) > MaxDocBytes {
		log(fmt.Sprintf("Document is %d bytes; truncating to %d for distillation", len(md), MaxDocBytes))
		md = md[:MaxDocBytes]
	}
	if err := os.WriteFile(filepath.Join(g.Dir, FileDoc), []byte(md), 0o644); err != nil {
		return err
	}
	if _, err := e.Store.Update(g.Key, func(g *Generation) error {
		g.Source.Via = via
		g.Source.Title = title
		g.Source.Bytes = len(md)
		g.Files["doc"] = filepath.Join(g.Dir, FileDoc)
		return nil
	}); err != nil {
		return err
	}
	log(fmt.Sprintf("Saved %s (%s, %d KB via %s)", FileDoc, orDefault(title, "untitled"), len(md)/1024, via))
	_, err = e.endStage(g.Key, StageIngest, obs)
	return err
}

func (e *Engine) stageDistill(ctx context.Context, g *Generation, obs Observer) error {
	if _, err := e.beginStage(g.Key, StageDistill, obs); err != nil {
		return err
	}
	doc, err := os.ReadFile(filepath.Join(g.Dir, FileDoc))
	if err != nil {
		return err
	}
	e.logf(obs, g.Key, StageDistill, "Claude (%s) is reading %d KB of docs…", e.billing, len(doc)/1024)
	res, err := e.claudeRunner(filepath.Join(g.Dir, DirWork)).Run(ctx, claude.Request{
		Prompt:       DistillPrompt(string(doc)),
		SystemPrompt: "You are a meticulous technical editor. Accuracy beats entertainment.",
		Timeout:      20 * time.Minute,
	})
	e.recordClaude(g.Key, "distill", res, obs)
	if err != nil {
		return fmt.Errorf("distill: %w", err)
	}
	text := strings.TrimSpace(res.Text)
	if text == "" {
		return errors.New("distill: Claude returned an empty distillation")
	}
	if err := os.WriteFile(filepath.Join(g.Dir, FileDistill), []byte(text+"\n"), 0o644); err != nil {
		return err
	}
	if _, err := e.Store.Update(g.Key, func(g *Generation) error {
		g.Files["distill"] = filepath.Join(g.Dir, FileDistill)
		if g.Source.Title == "" {
			g.Source.Title = TitleFromMarkdown(text)
		}
		return nil
	}); err != nil {
		return err
	}
	e.logf(obs, g.Key, StageDistill, "Wrote %s (%d words)", FileDistill, len(strings.Fields(text)))
	_, err = e.endStage(g.Key, StageDistill, obs)
	return err
}

func (e *Engine) stageScript(ctx context.Context, g *Generation, obs Observer) error {
	if _, err := e.beginStage(g.Key, StageScript, obs); err != nil {
		return err
	}
	style, err := e.styleFor(g)
	if err != nil {
		return err
	}
	spec, ok := kling.Lookup(g.Options.KlingModel)
	if !ok {
		return fmt.Errorf("unknown Kling model %q", g.Options.KlingModel)
	}
	distill, err := os.ReadFile(filepath.Join(g.Dir, FileDistill))
	if err != nil {
		return err
	}
	minS, maxS := e.Config.Video.MinSeconds, e.Config.Video.MaxSeconds
	prompt := StoryboardPrompt(string(distill), style, spec, g.Options.Audio, g.Options.TargetSeconds, minS, maxS)
	schema := StoryboardSchema(spec)

	var sb *Storyboard
	var warns []string
	feedback := ""
	for attempt := 1; attempt <= 2; attempt++ {
		e.logf(obs, g.Key, StageScript, "Writing a %s %s storyboard (attempt %d)…", style.Emoji, style.Name, attempt)
		res, err := e.claudeRunner(filepath.Join(g.Dir, DirWork)).Run(ctx, claude.Request{
			Prompt:       prompt + feedback,
			Schema:       schema,
			SystemPrompt: "You are a short-form video showrunner. Funny, fast, and technically correct.",
			Timeout:      20 * time.Minute,
		})
		e.recordClaude(g.Key, fmt.Sprintf("storyboard#%d", attempt), res, obs)
		if err != nil {
			return fmt.Errorf("storyboard: %w", err)
		}
		var cand Storyboard
		if err := json.Unmarshal(res.Structured, &cand); err != nil {
			feedback = fmt.Sprintf("\n\nYour previous answer was not valid storyboard JSON (%v). Return valid JSON matching the schema.", err)
			continue
		}
		w, nerr := cand.Normalize(spec, style, minS, maxS)
		if nerr != nil {
			feedback = fmt.Sprintf("\n\nYour previous storyboard was rejected: %v. Fix it — total seconds must be between %d and %d.", nerr, minS, maxS)
			continue
		}
		sb, warns = &cand, w
		break
	}
	if sb == nil {
		return errors.New("storyboard: Claude could not produce a valid storyboard after 2 attempts")
	}
	for _, w := range warns {
		e.logf(obs, g.Key, StageScript, "Storyboard: %s", w)
	}
	b, _ := json.MarshalIndent(sb, "", "  ")
	if err := os.WriteFile(filepath.Join(g.Dir, FileScript), b, 0o644); err != nil {
		return err
	}
	clips := make([]*Clip, len(sb.Scenes))
	for i, s := range sb.Scenes {
		clips[i] = &Clip{Index: i, Seconds: s.Seconds, Status: ClipPending}
	}
	if _, err := e.Store.Update(g.Key, func(g *Generation) error {
		g.Files["script"] = filepath.Join(g.Dir, FileScript)
		g.Video.Scenes = len(sb.Scenes)
		g.Kling.Clips = clips
		g.Kling.Units, g.Kling.CostUSD, g.Kling.UnitsReported = 0, 0, false
		if sb.Title != "" && g.Source.Title == "" {
			g.Source.Title = sb.Title
		}
		return nil
	}); err != nil {
		return err
	}
	e.logf(obs, g.Key, StageScript, "Storyboard “%s”: %d scenes, %ds", sb.Title, len(sb.Scenes), sb.TotalSeconds())
	_, err = e.endStage(g.Key, StageScript, obs)
	return err
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
