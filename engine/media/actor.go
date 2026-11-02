package media

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// MinActorEdge is the smallest short edge Kling accepts for a first frame.
const MinActorEdge = 300

// ImageSize returns an image's pixel size via ffprobe.
func (t Tools) ImageSize(ctx context.Context, path string) (w, h int, err error) {
	out, err := exec.CommandContext(ctx, t.FFprobe, "-v", "error", "-select_streams", "v:0",
		"-show_entries", "stream=width,height", "-of", "json", path).Output()
	if err != nil {
		return 0, 0, fmt.Errorf("can't read %s as an image: %w", path, err)
	}
	var d struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &d); err != nil || len(d.Streams) == 0 || d.Streams[0].Width == 0 {
		return 0, 0, fmt.Errorf("can't read %s as an image", path)
	}
	return d.Streams[0].Width, d.Streams[0].Height, nil
}

// ActorCrop returns the 9:16 crop size for a w×h photo and rejects photos that
// would be too small for Kling after cropping.
func ActorCrop(w, h int) (cw, ch int, err error) {
	cw, ch = w, h
	if w*16 > h*9 { // wider than 9:16: trim the sides
		cw = h * 9 / 16
	} else { // taller than 9:16: trim the bottom (keep the head)
		ch = w * 16 / 9
	}
	if cw < MinActorEdge || ch < MinActorEdge {
		return 0, 0, fmt.Errorf("photo is too small: %dx%d leaves only %dx%d after cropping to 9:16, Kling needs at least %dpx on the short edge", w, h, cw, ch, MinActorEdge)
	}
	return cw, ch, nil
}

// PrepareActor turns a photo into the 1080x1920 (9:16) JPEG every clip starts
// from. It crops rather than pads (padding would put a blurred frame inside
// the video) and keeps the top of the picture, because the head is nearly
// always near the top of a portrait. EXIF rotation is applied by ffmpeg.
func (t Tools) PrepareActor(ctx context.Context, in, out string) error {
	w, h, err := t.ImageSize(ctx, in)
	if err != nil {
		return err
	}
	cw, ch, err := ActorCrop(w, h)
	if err != nil {
		return err
	}
	vf := strings.Join([]string{
		fmt.Sprintf("crop=%d:%d:(iw-%d)/2:(ih-%d)*0.2", cw, ch, cw, ch),
		"scale=1080:1920:flags=lanczos",
	}, ",")
	return t.run(ctx, "-i", in, "-vf", vf, "-frames:v", "1", "-q:v", "2", out)
}
