package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pat-eason/doctok/engine/kling"
	"github.com/pat-eason/doctok/engine/media"
)

// Render generates every clip with Kling (or placeholders in dry-run mode) and
// assembles VIDEO.mp4. Already-downloaded clips and in-flight Kling tasks are
// reused, so Render doubles as resume.
func (e *Engine) Render(ctx context.Context, key string, obs Observer) (*Generation, error) {
	g, err := e.Store.Get(key)
	if err != nil {
		return nil, err
	}
	if g.Status == StatusSucceeded && fileExists(filepath.Join(g.Dir, FileVideo)) {
		return g, nil
	}
	if !e.stageDone(g, StageScript) {
		return nil, fmt.Errorf("generation %s has no storyboard yet; run plan first", key)
	}
	if g, err = e.claim(key); err != nil {
		return nil, err
	}
	sb, err := e.loadStoryboard(g)
	if err != nil {
		return nil, e.fail(ctx, key, obs, err)
	}
	if !e.stageDone(g, StageRender) {
		if err := e.stageRender(ctx, g, sb, obs); err != nil {
			return nil, e.fail(ctx, key, obs, err)
		}
	}
	if g, err = e.Store.Get(key); err != nil {
		return nil, err
	}
	if !e.stageDone(g, StageAssemble) || !fileExists(filepath.Join(g.Dir, FileVideo)) {
		if err := e.stageAssemble(ctx, g, sb, obs); err != nil {
			return nil, e.fail(ctx, key, obs, err)
		}
	}
	g, err = e.Store.Update(key, func(g *Generation) error {
		now := e.Now().UTC()
		g.Status = StatusSucceeded
		g.Stage = StageDone
		g.Progress = 1
		g.FinishedAt = &now
		g.OwnerPID = 0
		if g.StartedAt != nil {
			g.DurationSeconds = now.Sub(*g.StartedAt).Seconds()
		}
		g.Message = fmt.Sprintf("Done: %s", filepath.Join(g.Dir, FileVideo))
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.emit(obs, EventDone, StageDone, g.Message, g)
	return g, nil
}

func (e *Engine) clipPath(g *Generation, idx int) string {
	return filepath.Join(g.Dir, DirClips, fmt.Sprintf("clip_%03d.mp4", idx))
}

func (e *Engine) stageRender(ctx context.Context, g *Generation, sb *Storyboard, obs Observer) error {
	if _, err := e.beginStage(g.Key, StageRender, obs); err != nil {
		return err
	}
	style, err := LookupStyle(g.Style)
	if err != nil {
		return err
	}
	spec, _ := kling.Lookup(g.Options.KlingModel)

	// Reset clips that failed last time so resume retries them; make sure the
	// clip list matches the storyboard.
	g, err = e.Store.Update(g.Key, func(g *Generation) error {
		if len(g.Kling.Clips) != len(sb.Scenes) {
			g.Kling.Clips = make([]*Clip, len(sb.Scenes))
		}
		for i, s := range sb.Scenes {
			c := g.Kling.Clips[i]
			if c == nil {
				c = &Clip{Index: i, Status: ClipPending}
				g.Kling.Clips[i] = c
			}
			c.Seconds = s.Seconds
			if c.Status == ClipFailed {
				c.Status, c.TaskID, c.Attempts, c.Error = ClipPending, "", 0, ""
			}
			if c.Status == ClipDownloaded && !fileExists(e.clipPath(g, i)) {
				c.Status = ClipPending
				c.TaskID = ""
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	var client KlingAPI
	if g.Options.DryRun {
		e.logf(obs, g.Key, StageRender, "Dry run: rendering %d local placeholder clips (no Kling credits spent)", len(sb.Scenes))
	} else {
		var apiStyle string
		client, apiStyle, err = e.NewKling(e.Config.Kling)
		if err != nil {
			return err
		}
		if _, err := e.Store.Update(g.Key, func(g *Generation) error { g.Kling.APIStyle = apiStyle; return nil }); err != nil {
			return err
		}
		e.logf(obs, g.Key, StageRender, "Submitting %d scenes to Kling %s (%s, %s, audio=%v, up to %d at a time)…",
			len(sb.Scenes), spec.ID, g.Options.Resolution, "9:16", g.Options.Audio, e.Config.Kling.Concurrency)
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	conc := e.Config.Kling.Concurrency
	if g.Options.DryRun {
		conc = 2
	}
	sem := make(chan struct{}, max(1, conc))
	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error
	for i, scene := range sb.Scenes {
		if g.Kling.Clips[i].Status == ClipDownloaded {
			continue
		}
		wg.Add(1)
		go func(i int, scene Scene) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			var err error
			if g.Options.DryRun {
				err = e.placeholderClip(ctx, g, i, scene, style, obs)
			} else {
				prompt := KlingPrompt(sb, scene, style, spec, g.Options.Audio)
				err = e.renderClip(ctx, g, i, prompt, spec, client, obs)
			}
			if err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("scene %d: %w", i+1, err))
				mu.Unlock()
				if kling.IsFatal(err) {
					cancel()
				}
			}
		}(i, scene)
	}
	wg.Wait()
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err = e.endStage(g.Key, StageRender, obs)
	return err
}

// updateClip mutates one clip, recomputes Kling totals and render progress.
func (e *Engine) updateClip(key string, idx int, obs Observer, msg string, fn func(c *Clip)) *Generation {
	g, err := e.Store.Update(key, func(g *Generation) error {
		if idx >= len(g.Kling.Clips) || g.Kling.Clips[idx] == nil {
			return fmt.Errorf("clip %d missing", idx)
		}
		fn(g.Kling.Clips[idx])
		e.recomputeKling(g)
		if msg != "" {
			g.Message = msg
		}
		return nil
	})
	if err == nil {
		e.emit(obs, EventClip, StageRender, msg, g)
	}
	return g
}

func (e *Engine) recomputeKling(g *Generation) {
	units, done, reported := 0.0, 0.0, true
	n := len(g.Kling.Clips)
	for _, c := range g.Kling.Clips {
		switch c.Status {
		case ClipDownloaded:
			done += 1
			units += c.Units
			if !c.UnitsReported {
				reported = false
			}
		case ClipSucceeded:
			done += 0.9
		case ClipProcessing:
			done += 0.4
		case ClipSubmitted:
			done += 0.1
		}
	}
	g.Kling.Units = math.Round(units*100) / 100
	g.Kling.UnitsReported = reported && units > 0
	g.Kling.CostUSD = math.Round(units*e.Config.Kling.UnitPriceUSD*100) / 100
	if n > 0 && g.Stage == StageRender {
		g.Progress = stageBase(StageRender) + stageWeights[StageRender]*done/float64(n)
	}
}

func (e *Engine) renderClip(ctx context.Context, g *Generation, idx int, prompt string, spec kling.ModelSpec, client KlingAPI, obs Observer) error {
	poll := time.Duration(e.Config.Kling.PollSeconds) * time.Second
	if e.PollInterval > 0 {
		poll = e.PollInterval
	}
	label := fmt.Sprintf("Scene %d/%d", idx+1, len(g.Kling.Clips))
	for {
		cur, err := e.Store.Get(g.Key)
		if err != nil {
			return err
		}
		clip := *cur.Kling.Clips[idx]
		if clip.Status == ClipDownloaded && fileExists(e.clipPath(g, idx)) {
			return nil
		}

		// Submit if there's no live task.
		if clip.TaskID == "" {
			if clip.Attempts >= e.MaxClipAttempts {
				msg := clip.Error
				if msg == "" {
					msg = "unknown failure"
				}
				return fmt.Errorf("gave up after %d attempts: %s", clip.Attempts, msg)
			}
			ext := fmt.Sprintf("doctok-%s-%03d-%d-%s", g.Key, idx, clip.Attempts+1, shortHex())
			if len(ext) > 64 {
				ext = ext[len(ext)-64:]
			}
			var task *kling.Task
			for backoff := e.RetryBase; ; backoff = min(backoff*2, 2*time.Minute) {
				task, err = client.CreateTextToVideo(ctx, kling.CreateRequest{
					Model: spec, Prompt: prompt, NegativePrompt: KlingNegative, Resolution: g.Options.Resolution,
					AspectRatio: "9:16", Duration: clip.Seconds, Audio: g.Options.Audio, ExternalID: ext,
				})
				if err == nil {
					break
				}
				if !kling.IsRetryable(err) {
					e.updateClip(g.Key, idx, obs, fmt.Sprintf("%s: submit failed: %v", label, err), func(c *Clip) {
						c.Status, c.Error = ClipFailed, err.Error()
						c.Attempts++
					})
					if kling.IsFatal(err) {
						return err
					}
					break
				}
				e.logf(obs, g.Key, StageRender, "%s: Kling busy (%v); retrying in %s", label, err, backoff)
				if err := sleepCtx(ctx, backoff); err != nil {
					return err
				}
			}
			if task == nil {
				continue // counted as an attempt; loop re-checks attempts
			}
			now := e.Now().UTC()
			e.updateClip(g.Key, idx, obs, fmt.Sprintf("%s submitted (task %s)", label, task.ID), func(c *Clip) {
				c.TaskID, c.ExternalID, c.Status, c.Error = task.ID, ext, ClipSubmitted, ""
				c.Attempts++
				c.SubmittedAt = &now
			})
			continue
		}

		// Poll the live task.
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
		task, err := client.GetTask(ctx, clip.TaskID)
		if err != nil {
			var ae *kling.APIError
			if errors.As(err, &ae) && ae.Code == 1203 {
				e.updateClip(g.Key, idx, obs, label+": task vanished; resubmitting", func(c *Clip) { c.TaskID, c.Status = "", ClipPending })
				continue
			}
			if kling.IsFatal(err) {
				return err
			}
			e.logf(obs, g.Key, StageRender, "%s: poll error (%v); will retry", label, err)
			continue
		}
		switch task.Status {
		case kling.TaskSubmitted, kling.TaskProcessing:
			want := ClipSubmitted
			if task.Status == kling.TaskProcessing {
				want = ClipProcessing
			}
			if clip.Status != want {
				e.updateClip(g.Key, idx, obs, fmt.Sprintf("%s %s", label, want), func(c *Clip) { c.Status = want })
			}
		case kling.TaskFailed:
			msg := orDefault(task.Message, "Kling reported failure")
			e.updateClip(g.Key, idx, obs, fmt.Sprintf("%s failed: %s", label, msg), func(c *Clip) {
				c.Status, c.Error, c.TaskID = ClipFailed, msg, ""
			})
			// loop: resubmits while attempts remain
			cur, _ := e.Store.Get(g.Key)
			if cur != nil && cur.Kling.Clips[idx].Attempts < e.MaxClipAttempts {
				e.updateClip(g.Key, idx, obs, "", func(c *Clip) { c.Status = ClipPending })
			}
		case kling.TaskSucceeded:
			if task.VideoURL == "" {
				return errors.New("kling reported success but returned no video URL")
			}
			e.updateClip(g.Key, idx, obs, label+" rendered; downloading", func(c *Clip) { c.Status, c.URL = ClipSucceeded, task.VideoURL })
			path := e.clipPath(g, idx)
			var dlErr error
			for attempt := 0; attempt < 3; attempt++ {
				if _, dlErr = client.Download(ctx, task.VideoURL, path); dlErr == nil {
					break
				}
				if err := sleepCtx(ctx, time.Duration(attempt+1)*3*time.Second); err != nil {
					return err
				}
			}
			if dlErr != nil {
				return fmt.Errorf("download: %w", dlErr)
			}
			units := task.Units
			reported := task.CostReported && task.Units > 0
			if !reported {
				units = spec.UnitsFor(clip.Seconds, g.Options.Resolution, g.Options.Audio)
			}
			now := e.Now().UTC()
			e.updateClip(g.Key, idx, obs, fmt.Sprintf("%s ✓ (%.2f units)", label, units), func(c *Clip) {
				c.Status, c.Path, c.Units, c.UnitsReported, c.CompletedAt = ClipDownloaded, path, units, reported, &now
			})
			return nil
		}
	}
}

func (e *Engine) placeholderClip(ctx context.Context, g *Generation, idx int, scene Scene, style Style, obs Observer) error {
	tools, err := e.mediaTools()
	if err != nil {
		return err
	}
	o := media.Options{Width: e.Config.Video.Width, Height: e.Config.Video.Height, FPS: e.Config.Video.FPS}
	color := ""
	if style.ChromaKey {
		color = "0x00FF00"
	}
	e.updateClip(g.Key, idx, obs, "", func(c *Clip) { c.Status = ClipProcessing; c.Attempts++ })
	if err := tools.Placeholder(ctx, e.clipPath(g, idx), scene.Seconds, idx, color, o); err != nil {
		return err
	}
	now := e.Now().UTC()
	e.updateClip(g.Key, idx, obs, fmt.Sprintf("Placeholder %d/%d ✓", idx+1, len(g.Kling.Clips)), func(c *Clip) {
		c.Status, c.Path, c.Units, c.CompletedAt = ClipDownloaded, e.clipPath(g, idx), 0, &now
	})
	return nil
}

func (e *Engine) stageAssemble(ctx context.Context, g *Generation, sb *Storyboard, obs Observer) error {
	if _, err := e.beginStage(g.Key, StageAssemble, obs); err != nil {
		return err
	}
	tools, err := e.mediaTools()
	if err != nil {
		return err
	}
	style, err := LookupStyle(g.Style)
	if err != nil {
		return err
	}
	work := filepath.Join(g.Dir, DirWork, "assemble")
	if err := os.MkdirAll(work, 0o755); err != nil {
		return err
	}
	o := media.Options{
		Width: e.Config.Video.Width, Height: e.Config.Video.Height, FPS: e.Config.Video.FPS,
		Captions: g.Options.Captions, Style: style.Captions, WorkDir: work,
	}
	n := len(sb.Scenes)
	segs := make([]string, n)
	var done int
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	sem := make(chan struct{}, 2)
	lastQuote := sb.Hook
	quotes := make([]string, n)
	for i, s := range sb.Scenes {
		if strings.TrimSpace(s.DocQuote) != "" {
			lastQuote = s.DocQuote
		}
		quotes[i] = lastQuote
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i, s := range sb.Scenes {
		segs[i] = filepath.Join(work, fmt.Sprintf("seg_%03d.mp4", i))
		wg.Add(1)
		go func(i int, s Scene) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			seg := media.Segment{
				Input: e.clipPath(g, i), Dialogue: s.Dialogue(), TopText: s.TopText, Cut: s.Cut,
				MaxSecs: float64(s.Seconds), Grade: style.Grade,
			}
			if style.SoftCuts {
				seg.Cut = "soft"
			}
			if style.AlternateZoom && i%2 == 1 && seg.Cut != "punch-in" {
				seg.Zoom = 1.12
			}
			if style.ChromaKey {
				bg := filepath.Join(work, fmt.Sprintf("bg_%03d.png", i))
				heading := orDefault(s.TopText, orDefault(sb.Title, "the docs"))
				if err := media.RenderDocCard(bg, heading, quotes[i], o.Width, o.Height, style.Captions.HighlightColor); err == nil {
					seg.Background = bg
				}
			}
			err := tools.RenderSegment(ctx, i, seg, segs[i], o)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("scene %d: %w", i+1, err)
					cancel()
				}
				return
			}
			done++
			d := done
			gg, _ := e.Store.Update(g.Key, func(g *Generation) error {
				g.Progress = stageBase(StageAssemble) + stageWeights[StageAssemble]*0.9*float64(d)/float64(n)
				g.Message = fmt.Sprintf("Cut & captioned %d/%d scenes", d, n)
				return nil
			})
			e.emit(obs, EventProgress, StageAssemble, gg.Message, gg)
		}(i, s)
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	e.logf(obs, g.Key, StageAssemble, "Stitching %d segments…", n)
	joined := filepath.Join(work, "joined.mp4")
	if err := tools.Concat(ctx, segs, joined, work); err != nil {
		return err
	}
	out := filepath.Join(g.Dir, FileVideo)
	if err := tools.Finalize(ctx, joined, g.Options.Music, out, 0.12); err != nil {
		return err
	}
	p, err := tools.ProbeFile(ctx, out)
	if err != nil {
		return err
	}
	if _, err := e.Store.Update(g.Key, func(g *Generation) error {
		g.Files["video"] = out
		g.Video.DurationSeconds = math.Round(p.Duration*100) / 100
		g.Video.Width, g.Video.Height, g.Video.SizeBytes = p.Width, p.Height, p.Size
		return nil
	}); err != nil {
		return err
	}
	// Segments are reproducible from clips; drop them to save space.
	for _, s := range segs {
		os.Remove(s)
	}
	os.Remove(joined)
	e.logf(obs, g.Key, StageAssemble, "VIDEO.mp4: %dx%d, %s, %.1f MB", p.Width, p.Height, fmtDur(p.Duration), float64(p.Size)/1e6)
	_, err = e.endStage(g.Key, StageAssemble, obs)
	return err
}

func fmtDur(sec float64) string {
	d := time.Duration(sec * float64(time.Second)).Round(time.Second)
	return fmt.Sprintf("%d:%02d", int(d.Minutes()), int(d.Seconds())%60)
}
