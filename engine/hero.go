package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pat-eason/doctok/engine/kling"
)

// ErrHeroNotApproved is returned by Render when the presenter reference image
// hasn't been generated and approved yet.
var ErrHeroNotApproved = errors.New("the presenter reference image isn't approved yet: run `doctok resume <key>` to generate and review it")

// FileHeroBase is the hero image's base file name (extension follows the image).
const FileHeroBase = "HERO"

const heroWaitMax = 20 * time.Minute

// imageUploader is implemented by backends that can host a local image.
type imageUploader interface {
	UploadImage(ctx context.Context, path string) (string, error)
}

// heroAPI is implemented by backends that can generate the reference image.
type heroAPI interface {
	CreateImage(ctx context.Context, prompt string) (string, error)
	GetImage(ctx context.Context, id string) (status, url string, err error)
}

// NeedsHero reports whether clips for g are animated from a reference image.
// Only the Kling MCP backend supports it, and dry runs never spend credits.
func (e *Engine) NeedsHero(g *Generation) bool {
	return g != nil && !g.Options.DryRun && e.Config.Kling.Backend == KlingBackendMCP && (!e.Config.Kling.NoHero || g.Options.ActorImage != "")
}

// HeroPrompt describes the first frame shared by every clip: the cast and the
// set from the storyboard, in a neutral, closed-mouth pose that any scene can
// start from.
func HeroPrompt(sb *Storyboard, style Style) string {
	var b strings.Builder
	b.WriteString("Photorealistic vertical 9:16 portrait for a social video. ")
	for _, c := range sb.Characters {
		fmt.Fprintf(&b, "%s: %s. ", c.Label, strings.TrimSuffix(strings.TrimSpace(c.Description), "."))
	}
	setting := sb.Setting
	if setting == "" {
		setting = style.Setting
	}
	if style.ChromaKey {
		setting = "in front of a flat uniform bright green chroma-key screen"
	}
	if setting != "" {
		fmt.Fprintf(&b, "Setting: %s. ", strings.TrimSuffix(setting, "."))
	}
	b.WriteString("The presenter faces the camera at chest-up framing in a relaxed neutral pose with a slight smile and mouth closed, soft even lighting, sharp focus, ")
	b.WriteString("centered with room above the head. No text, no captions, no logos, no watermark.")
	return strings.TrimSpace(b.String())
}

// Hero generates the presenter reference image (or reuses the existing one) and
// leaves the generation awaiting approval. regenerate discards the current image
// and pays for a new one. The Kling task id is saved right after submitting, so
// a resumed run polls the in-flight image instead of paying twice.
func (e *Engine) Hero(ctx context.Context, key string, regenerate bool, obs Observer) (*Generation, error) {
	g, err := e.Store.Get(key)
	if err != nil {
		return nil, err
	}
	if !e.NeedsHero(g) {
		return g, nil
	}
	if !e.stageDone(g, StageScript) {
		return nil, fmt.Errorf("generation %s has no storyboard yet; run plan first", key)
	}
	if !regenerate && g.Hero != nil && g.Hero.Path != "" && fileExists(g.Hero.Path) {
		return g, nil
	}
	if g, err = e.claim(key); err != nil {
		return nil, err
	}
	if err := e.heroRun(ctx, g, regenerate, obs); err != nil {
		return nil, e.fail(ctx, key, obs, err)
	}
	g, err = e.Store.Update(key, func(g *Generation) error {
		g.Status = StatusAwaitingApproval
		g.OwnerPID = 0
		g.Message = fmt.Sprintf("Presenter image ready for review: %s", g.Hero.Path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	e.emit(obs, EventApproval, StageRender, g.Message, g)
	return g, nil
}

func (e *Engine) heroRun(ctx context.Context, g *Generation, regenerate bool, obs Observer) error {
	sb, err := e.loadStoryboard(g)
	if err != nil {
		return err
	}
	style, err := e.styleFor(g)
	if err != nil {
		return err
	}
	client, _, err := e.newKlingBackend(ctx, e.Config.Kling)
	if err != nil {
		return err
	}
	if g.Options.ActorImage != "" {
		return e.heroFromPhoto(ctx, g, client, obs)
	}
	ha, ok := client.(heroAPI)
	if !ok {
		return errors.New("this Kling backend can't generate a reference image")
	}

	inflight := !regenerate && g.Hero != nil && g.Hero.TaskID != "" && g.Hero.Path == ""
	if !inflight {
		prompt := HeroPrompt(sb, style)
		e.logf(obs, g.Key, StageRender, "Generating the presenter reference image (a few Kling credits)…")
		var id string
		for backoff := e.RetryBase; ; backoff = min(backoff*2, 2*time.Minute) {
			id, err = ha.CreateImage(ctx, prompt)
			if err == nil {
				break
			}
			if !kling.IsRetryable(err) {
				return err
			}
			e.logf(obs, g.Key, StageRender, "Kling busy (%v); retrying in %s", err, backoff)
			if err := sleepCtx(ctx, backoff); err != nil {
				return err
			}
		}
		// Persist the id immediately: from here on, resume polls instead of paying again.
		if _, err := e.Store.Update(g.Key, func(g *Generation) error {
			attempts := 0
			if g.Hero != nil {
				attempts = g.Hero.Attempts
			}
			g.Hero = &Hero{TaskID: id, Prompt: prompt, Source: "generated", Attempts: attempts + 1, CreatedAt: e.Now().UTC()}
			return nil
		}); err != nil {
			return err
		}
		e.logf(obs, g.Key, StageRender, "Presenter image submitted (task %s)", id)
	} else {
		e.logf(obs, g.Key, StageRender, "Resuming in-flight presenter image (task %s)", g.Hero.TaskID)
	}

	cur, err := e.Store.Get(g.Key)
	if err != nil {
		return err
	}
	taskID := cur.Hero.TaskID
	poll := 15 * time.Second
	if e.PollInterval > 0 {
		poll = e.PollInterval
	}
	deadline := e.Now().Add(heroWaitMax)
	var url string
	for url == "" {
		if err := sleepCtx(ctx, poll); err != nil {
			return err
		}
		status, u, err := ha.GetImage(ctx, taskID)
		switch {
		case err != nil && kling.IsFatal(err):
			return err
		case err != nil:
			e.logf(obs, g.Key, StageRender, "Presenter image: poll error (%v); will retry", err)
		case status == "failed":
			return fmt.Errorf("Kling failed to generate the presenter image (task %s); `doctok resume %s` to try again", taskID, g.Key)
		case status == "succeeded" && u != "":
			url = u
		}
		if url == "" && e.Now().After(deadline) {
			return fmt.Errorf("presenter image (task %s) still not ready after %s; `doctok resume %s` keeps waiting on it without paying again", taskID, heroWaitMax, g.Key)
		}
	}

	tmp := filepath.Join(g.Dir, FileHeroBase+".download")
	if _, err := kling.DownloadFile(ctx, url, tmp, "doctok/1.0"); err != nil {
		return fmt.Errorf("download presenter image: %w", err)
	}
	path := filepath.Join(g.Dir, FileHeroBase+imageExt(tmp))
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	_, err = e.Store.Update(g.Key, func(g *Generation) error {
		g.Hero.URL, g.Hero.Path, g.Hero.Approved = url, path, false
		return nil
	})
	return err
}

// heroFromPhoto uses the user's photo as the reference image: crop to 9:16,
// upload to Kling. No credits are spent.
func (e *Engine) heroFromPhoto(ctx context.Context, g *Generation, client KlingAPI, obs Observer) error {
	up, ok := client.(imageUploader)
	if !ok {
		return errors.New("this Kling backend can't host an actor photo")
	}
	tools, err := e.mediaTools()
	if err != nil {
		return err
	}
	e.logf(obs, g.Key, StageRender, "Preparing your photo as the presenter (cropping to 9:16)…")
	out := filepath.Join(g.Dir, FileHeroBase+".jpg")
	if err := tools.PrepareActor(ctx, g.Options.ActorImage, out); err != nil {
		return err
	}
	e.logf(obs, g.Key, StageRender, "Uploading your photo to Kling…")
	url, err := up.UploadImage(ctx, out)
	if err != nil {
		return err
	}
	_, err = e.Store.Update(g.Key, func(g *Generation) error {
		attempts := 0
		if g.Hero != nil {
			attempts = g.Hero.Attempts
		}
		g.Hero = &Hero{URL: url, Path: out, Source: "upload", Attempts: attempts + 1, CreatedAt: e.Now().UTC()}
		return nil
	})
	return err
}

// ApproveHero marks the current reference image as the one to render with.
func (e *Engine) ApproveHero(key string) (*Generation, error) {
	return e.Store.Update(key, func(g *Generation) error {
		if g.Hero == nil || g.Hero.Path == "" || !fileExists(g.Hero.Path) {
			return errors.New("no presenter image to approve yet")
		}
		g.Hero.Approved = true
		return nil
	})
}

// refreshHeroURL re-reads the hero image's URL from Kling (hosted URLs can
// expire between a first run and a later resume). It keeps the saved URL if
// the lookup fails.
func (e *Engine) refreshHeroURL(ctx context.Context, g *Generation, client KlingAPI, obs Observer) string {
	h := g.Hero
	if h == nil {
		return ""
	}
	if h.Source == "upload" {
		// An uploaded photo has no Kling task to re-query; check the URL still
		// resolves and re-host the saved crop if it doesn't.
		if heroURLAlive(ctx, h.URL) {
			return h.URL
		}
		if up, ok := client.(imageUploader); ok && fileExists(h.Path) {
			if u, err := up.UploadImage(ctx, h.Path); err == nil {
				_, _ = e.Store.Update(g.Key, func(g *Generation) error {
					if g.Hero != nil {
						g.Hero.URL = u
					}
					return nil
				})
				e.logf(obs, g.Key, StageRender, "Re-uploaded your photo (the earlier Kling URL had expired)")
				return u
			} else {
				e.logf(obs, g.Key, StageRender, "Couldn't re-upload your photo (%v); using the saved URL", err)
			}
		}
		return h.URL
	}
	if ha, ok := client.(heroAPI); ok && h.TaskID != "" {
		if status, u, err := ha.GetImage(ctx, h.TaskID); err == nil && status == "succeeded" && u != "" {
			if u != h.URL {
				_, _ = e.Store.Update(g.Key, func(g *Generation) error {
					if g.Hero != nil {
						g.Hero.URL = u
					}
					return nil
				})
			}
			return u
		} else if err != nil {
			e.logf(obs, g.Key, StageRender, "Couldn't refresh the presenter image URL (%v); using the saved one", err)
		}
	}
	return h.URL
}

func imageExt(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ".png"
	}
	defer f.Close()
	head := make([]byte, 4)
	n, _ := f.Read(head)
	if n >= 2 && bytes.HasPrefix(head, []byte{0xFF, 0xD8}) {
		return ".jpg"
	}
	return ".png"
}

// heroURLAlive reports whether a hosted image URL still resolves.
func heroURLAlive(ctx context.Context, u string) bool {
	if u == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, u, nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 400
}
