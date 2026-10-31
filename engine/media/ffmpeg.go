// Package media normalizes Kling clips to 1080x1920, burns in TikTok-style
// captions and cuts, and concatenates everything into the final VIDEO.mp4.
// It only requires a stock ffmpeg/ffprobe (no libass/freetype): captions are
// rendered to PNG in Go and composited with the overlay filter.
package media

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Tools locates ffmpeg binaries.
type Tools struct {
	FFmpeg  string
	FFprobe string
}

// FindTools resolves ffmpeg/ffprobe from $PATH (or DOCTOK_FFMPEG/DOCTOK_FFPROBE).
func FindTools() (Tools, error) {
	var t Tools
	var err error
	ff := os.Getenv("DOCTOK_FFMPEG")
	if ff == "" {
		ff = "ffmpeg"
	}
	fp := os.Getenv("DOCTOK_FFPROBE")
	if fp == "" {
		fp = "ffprobe"
	}
	if t.FFmpeg, err = exec.LookPath(ff); err != nil {
		return t, fmt.Errorf("ffmpeg not found (brew install ffmpeg): %w", err)
	}
	if t.FFprobe, err = exec.LookPath(fp); err != nil {
		return t, fmt.Errorf("ffprobe not found (brew install ffmpeg): %w", err)
	}
	return t, nil
}

// Version returns ffmpeg's first version line.
func (t Tools) Version(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, t.FFmpeg, "-hide_banner", "-version").Output()
	if err != nil {
		return "", err
	}
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line), nil
}

// Probe describes a media file.
type Probe struct {
	Duration float64
	Width    int
	Height   int
	HasAudio bool
	Size     int64
}

// ProbeFile runs ffprobe.
func (t Tools) ProbeFile(ctx context.Context, path string) (*Probe, error) {
	out, err := exec.CommandContext(ctx, t.FFprobe, "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path).Output()
	if err != nil {
		return nil, fmt.Errorf("ffprobe %s: %w", filepath.Base(path), err)
	}
	var d struct {
		Format struct {
			Duration string `json:"duration"`
			Size     string `json:"size"`
		} `json:"format"`
		Streams []struct {
			CodecType string `json:"codec_type"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
			Duration  string `json:"duration"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, err
	}
	p := &Probe{}
	p.Duration, _ = strconv.ParseFloat(d.Format.Duration, 64)
	p.Size, _ = strconv.ParseInt(d.Format.Size, 10, 64)
	for _, s := range d.Streams {
		switch s.CodecType {
		case "video":
			if p.Width == 0 {
				p.Width, p.Height = s.Width, s.Height
			}
			if p.Duration == 0 {
				p.Duration, _ = strconv.ParseFloat(s.Duration, 64)
			}
		case "audio":
			p.HasAudio = true
		}
	}
	return p, nil
}

// Segment is one scene to normalize.
type Segment struct {
	Input    string
	Dialogue string // used for captions
	TopText  string // sticker overlay
	Cut      string // hard-cut | jump-cut | punch-in | flash | soft
	MaxSecs  float64
	Zoom     float64 // extra scale factor (>1 punches in); 0 = none
	// Background, when set, is composited behind a chroma-keyed (green) clip.
	Background string
	Grade      string // "" | "faded"
}

// Options for assembly.
type Options struct {
	Width, Height, FPS int
	Captions           bool
	Style              CaptionStyle
	WorkDir            string
	CRF                int
}

func (o Options) withDefaults() Options {
	if o.Width == 0 {
		o.Width = 1080
	}
	if o.Height == 0 {
		o.Height = 1920
	}
	if o.FPS == 0 {
		o.FPS = 30
	}
	if o.CRF == 0 {
		o.CRF = 18
	}
	return o
}

func (t Tools) run(ctx context.Context, args ...string) error {
	cmd := exec.CommandContext(ctx, t.FFmpeg, append([]string{"-hide_banner", "-loglevel", "error", "-y"}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 1500 {
			msg = msg[len(msg)-1500:]
		}
		return fmt.Errorf("ffmpeg: %v: %s", err, msg)
	}
	return nil
}

// RenderSegment normalizes one clip into a uniform 1080x1920/30fps/H.264/AAC
// segment with captions and cut effects applied.
func (t Tools) RenderSegment(ctx context.Context, idx int, seg Segment, out string, o Options) error {
	o = o.withDefaults()
	p, err := t.ProbeFile(ctx, seg.Input)
	if err != nil {
		return err
	}
	dur := p.Duration
	if seg.MaxSecs > 0 && dur > seg.MaxSecs+0.5 {
		dur = seg.MaxSecs
	}
	if dur <= 0 {
		return fmt.Errorf("clip %s has no duration", filepath.Base(seg.Input))
	}
	dir := filepath.Join(o.WorkDir, fmt.Sprintf("seg_%03d", idx))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	args := []string{"-i", seg.Input}
	inputs := 1
	audioIn := "0:a"
	if !p.HasAudio {
		args = append(args, "-f", "lavfi", "-t", fmtF(dur), "-i", "anullsrc=r=48000:cl=stereo")
		audioIn = fmt.Sprintf("%d:a", inputs)
		inputs++
	}

	W, H := o.Width, o.Height
	var fc []string
	// Fill the 9:16 frame (cover); punch-ins scale up before the center crop.
	zoom := seg.Zoom
	if zoom < 1 {
		zoom = 1
	}
	if seg.Cut == "punch-in" {
		zoom = max(zoom, 1.18)
	}
	sw, sh := even(float64(W)*zoom), even(float64(H)*zoom)
	cover := fmt.Sprintf("scale=%d:%d:force_original_aspect_ratio=increase:flags=lanczos,crop=%d:%d,setsar=1,fps=%d", sw, sh, W, H, o.FPS)
	var base string
	if seg.Background != "" {
		// Green-screen composite: key the creator out and lay them over the doc card.
		args = append(args, "-loop", "1", "-t", fmtF(dur), "-i", seg.Background)
		bg := inputs
		inputs++
		fc = append(fc,
			fmt.Sprintf("[%d:v]scale=%d:%d:force_original_aspect_ratio=increase,crop=%d:%d,setsar=1,fps=%d,format=yuv420p[bg]", bg, W, H, W, H, o.FPS),
			fmt.Sprintf("[0:v]%s,format=yuva420p,chromakey=0x00FF00:0.16:0.08,despill=type=green[fg]", cover),
		)
		base = "[bg][fg]overlay=0:0:shortest=1,format=yuv420p"
	} else {
		base = "[0:v]" + cover + ",format=yuv420p"
	}
	switch seg.Grade {
	case "faded":
		base += ",eq=contrast=0.86:brightness=0.045:saturation=0.72:gamma_r=1.04:gamma_b=0.96"
	}
	switch seg.Cut {
	case "flash":
		base += ",fade=t=in:st=0:d=0.18:color=white"
	case "soft":
		base += ",fade=t=in:st=0:d=0.3:color=black"
	}
	fc = append(fc, base+"[v0]")
	last := "v0"
	n := 0
	overlay := func(png string, y int, enable string) {
		args = append(args, "-i", png)
		in := inputs
		inputs++
		n++
		lbl := fmt.Sprintf("v%d", n)
		f := fmt.Sprintf("[%s][%d:v]overlay=0:%d", last, in, y)
		if enable != "" {
			f += ":enable='" + enable + "'"
		}
		fc = append(fc, f+"["+lbl+"]")
		last = lbl
	}

	if o.Captions && strings.TrimSpace(seg.TopText) != "" {
		png := filepath.Join(dir, "top.png")
		if h, err := RenderTopText(png, seg.TopText, W, o.Style); err == nil {
			overlay(png, int(float64(H)*o.Style.withDefaults().TopPosition)-h/2, "")
		}
	}
	if o.Captions {
		cues := BuildCues(seg.Dialogue, dur, o.Style)
		for ci, cue := range cues {
			png := filepath.Join(dir, fmt.Sprintf("cue_%03d.png", ci))
			h, err := RenderCue(png, cue, W, o.Style)
			if err != nil {
				return fmt.Errorf("render caption: %w", err)
			}
			y := int(float64(H)*o.Style.withDefaults().Position) - h/2
			overlay(png, y, fmt.Sprintf("between(t,%s,%s)", fmtF(cue.Start), fmtF(cue.End-0.001)))
		}
	}
	fc = append(fc, fmt.Sprintf("[%s]format=yuv420p[vout]", last))
	fc = append(fc, fmt.Sprintf("[%s]aresample=48000,aformat=sample_fmts=fltp:channel_layouts=stereo,apad[aout]", audioIn))

	graph := strings.Join(fc, ";")
	_ = os.WriteFile(filepath.Join(dir, "filter.txt"), []byte(strings.Join(fc, ";\n")), 0o644) // debugging aid
	args = append(args,
		"-filter_complex", graph,
		"-map", "[vout]", "-map", "[aout]",
		"-t", fmtF(dur),
		"-c:v", "libx264", "-preset", "medium", "-crf", strconv.Itoa(o.CRF), "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-r", strconv.Itoa(o.FPS), "-g", strconv.Itoa(o.FPS*2),
		"-c:a", "aac", "-b:a", "192k", "-ar", "48000", "-ac", "2",
		"-video_track_timescale", "15360",
		out)
	return t.run(ctx, args...)
}

// Finalize lays a looping music bed under the dialogue and tags the file as
// AI-generated. With music == "" it only writes metadata (stream copy).
func (t Tools) Finalize(ctx context.Context, in, music, out string, musicVolume float64) error {
	meta := []string{
		"-metadata", "comment=AI-generated with doctok (Claude + Kling). Label as AI-generated content when posting.",
		"-metadata", "title=" + strings.TrimSuffix(filepath.Base(out), filepath.Ext(out)),
	}
	tmp := out + ".tmp.mp4"
	var args []string
	if music == "" {
		args = append([]string{"-i", in, "-map", "0", "-c", "copy"}, meta...)
	} else {
		if musicVolume <= 0 {
			musicVolume = 0.12
		}
		args = []string{"-i", in, "-stream_loop", "-1", "-i", music,
			"-filter_complex", fmt.Sprintf("[1:a]aresample=48000,aformat=channel_layouts=stereo,volume=%.3f[m];[0:a][m]amix=inputs=2:duration=first:dropout_transition=0:normalize=0[a]", musicVolume),
			"-map", "0:v", "-map", "[a]", "-c:v", "copy", "-c:a", "aac", "-b:a", "192k", "-shortest"}
		args = append(args, meta...)
	}
	args = append(args, "-movflags", "+faststart", tmp)
	if err := t.run(ctx, args...); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// Concat joins uniform segments losslessly into the final mp4.
func (t Tools) Concat(ctx context.Context, segments []string, out, workDir string) error {
	list := filepath.Join(workDir, "concat.txt")
	var sb strings.Builder
	for _, s := range segments {
		abs, _ := filepath.Abs(s)
		sb.WriteString("file '" + strings.ReplaceAll(abs, "'", `'\''`) + "'\n")
	}
	if err := os.WriteFile(list, []byte(sb.String()), 0o644); err != nil {
		return err
	}
	tmp := out + ".tmp.mp4"
	if err := t.run(ctx, "-f", "concat", "-safe", "0", "-i", list, "-c", "copy", "-movflags", "+faststart", tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, out)
}

// Placeholder renders a local stand-in clip (used by --dry-run): a colored
// card with a tone so timing and captions can be previewed for free.
func (t Tools) Placeholder(ctx context.Context, out string, seconds int, idx int, color string, o Options) error {
	o = o.withDefaults()
	palette := []string{"0xFF2D95", "0x00E5FF", "0xB388FF", "0xFFE135", "0x1DE9B6", "0xFF6E40"}
	c := palette[idx%len(palette)]
	if color != "" {
		c = color
	}
	freq := 220 + (idx%6)*55
	return t.run(ctx,
		"-f", "lavfi", "-i", fmt.Sprintf("color=c=%s:s=%dx%d:r=%d:d=%d", c, o.Width, o.Height, o.FPS, seconds),
		"-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=%d:sample_rate=48000:duration=%d", freq, seconds),
		"-filter_complex", "[1:a]volume=0.08[a]",
		"-map", "0:v", "-map", "[a]",
		"-c:v", "libx264", "-preset", "veryfast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-shortest", out)
}

func fmtF(f float64) string { return strconv.FormatFloat(f, 'f', 3, 64) }

func even(f float64) int {
	n := int(f)
	if n%2 == 1 {
		n++
	}
	return n
}
