package cli

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/pat-eason/doctok/engine"
	"github.com/pat-eason/doctok/engine/kling"
	"github.com/pat-eason/doctok/engine/notion"
	"github.com/spf13/cobra"
)

type genFlags struct {
	name, style, length, model, resolution, music string
	noAudio, noCaptions, dryRun, yes, plain       bool
}

func newGenerateCmd() *cobra.Command {
	var f genFlags
	cmd := &cobra.Command{
		Use:     "generate [notion-url | path/to/doc.md]",
		Aliases: []string{"gen", "g"},
		Short:   "Turn a Notion page or Markdown doc into a TikTok-style video",
		Example: `  doctok generate ./ARCHITECTURE.md --name k8s-tea
  doctok generate https://www.notion.so/acme/Payments-RFC-1a2b3c... --style pov-skit
  doctok generate docs/api.md --dry-run          # storyboard + placeholder video, no Kling spend`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src := ""
			if len(args) == 1 {
				src = args[0]
			}
			return runGenerate(cmd, src, f)
		},
	}
	fl := cmd.Flags()
	fl.StringVarP(&f.name, "name", "n", "", "name for this generation (default: auto-generated)")
	fl.StringVarP(&f.style, "style", "s", "", "video style preset (see `doctok styles`)")
	fl.StringVarP(&f.length, "length", "l", "", "target length, e.g. 210, 3m30s, 4m (180–300s)")
	fl.StringVar(&f.model, "model", "", "Kling model ("+strings.Join(kling.ModelIDs(), ", ")+")")
	fl.StringVar(&f.resolution, "resolution", "", "Kling clip resolution: 1080p | 720p")
	fl.StringVar(&f.music, "music", "", "optional music bed (audio file you have rights to) mixed under the dialogue")
	fl.BoolVar(&f.noAudio, "no-audio", false, "silent clips (captions only)")
	fl.BoolVar(&f.noCaptions, "no-captions", false, "don't burn in captions")
	fl.BoolVar(&f.dryRun, "dry-run", false, "distill + storyboard with Claude, render placeholder clips locally (no Kling credits)")
	fl.BoolVarP(&f.yes, "yes", "y", false, "don't ask for confirmation before spending Kling credits")
	fl.BoolVar(&f.plain, "plain", false, "plain log output instead of the live TUI")
	return cmd
}

func parseLength(s string) (int, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("bad length %q (use seconds like 210 or a duration like 3m30s)", s)
	}
	return int(d.Seconds()), nil
}

func validateSource(s string) error {
	_, err := engine.ClassifySource(s)
	return err
}

func runGenerate(cmd *cobra.Command, src string, f genFlags) error {
	eng, err := openEngine()
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	fmt.Println(logo())
	fmt.Println()

	// Interactive prompts for anything missing.
	if interactive() {
		lengthStr := f.length
		if lengthStr == "" {
			lengthStr = strconv.Itoa(eng.Config.Video.TargetSeconds)
		}
		style := f.style
		if style == "" {
			style = eng.Config.Video.Style
		}
		var groups []*huh.Group
		if src == "" {
			groups = append(groups, huh.NewGroup(
				huh.NewInput().Title("What doc are we yassifying?").
					Description("A Notion page URL or a path to a local Markdown file").
					Placeholder("https://www.notion.so/…  or  ./docs/ARCHITECTURE.md").
					Value(&src).Validate(validateSource),
			))
		}
		if !cmd.Flags().Changed("name") {
			groups = append(groups, huh.NewGroup(
				huh.NewInput().Title("Name this generation").Description("Leave blank for an auto-generated name").
					Placeholder("e.g. k8s-networking-tea").Value(&f.name),
			))
		}
		if !cmd.Flags().Changed("style") {
			opts := make([]huh.Option[string], 0, len(engine.StyleCatalog))
			for _, s := range engine.StyleCatalog {
				opts = append(opts, huh.NewOption(fmt.Sprintf("%s  %-26s %s", s.Emoji, s.Name, sDim.Render(s.Description)), s.ID))
			}
			groups = append(groups, huh.NewGroup(
				huh.NewSelect[string]().Title("Pick a style").Options(opts...).Value(&style).Height(11),
			))
		}
		if !cmd.Flags().Changed("length") {
			groups = append(groups, huh.NewGroup(
				huh.NewSelect[string]().Title("How long?").Options(
					huh.NewOption("3:00 — quick serve", "180"),
					huh.NewOption("3:30 — the sweet spot", "210"),
					huh.NewOption("4:00 — deep lore", "240"),
					huh.NewOption("5:00 — full documentary era", "300"),
				).Value(&lengthStr),
			))
		}
		if !cmd.Flags().Changed("dry-run") {
			groups = append(groups, huh.NewGroup(
				huh.NewConfirm().Title("Dry run?").
					Description("Dry run = Claude writes the storyboard, placeholder clips are rendered locally, no Kling credits spent.").
					Affirmative("Dry run").Negative("Full send 🚀").Value(&f.dryRun),
			))
		}
		if len(groups) > 0 {
			if err := runForm(groups...); err != nil {
				return err
			}
		}
		f.length, f.style = lengthStr, style
	}
	if src == "" {
		return errors.New("missing source: pass a Notion URL or Markdown path")
	}
	if notion.IsNotionURL(src) {
		fmt.Println(sDim.Render("Notion source — will use " + notionRoute(eng)))
	}
	target, err := parseLength(f.length)
	if err != nil {
		return err
	}
	req := engine.GenerateRequest{
		Source: src, Name: f.name, Style: f.style, DryRun: f.dryRun, TargetSeconds: target,
		KlingModel: f.model, Resolution: f.resolution, Music: f.music,
	}
	if f.noAudio {
		v := false
		req.Audio = &v
	}
	if f.noCaptions {
		v := false
		req.Captions = &v
	}
	if !f.dryRun {
		if err := preflightKling(eng); err != nil {
			return err
		}
	}
	g, err := eng.Create(ctx, req)
	if err != nil {
		return err
	}
	pre := eng.PreEstimate(g)
	fmt.Printf("%s %s → %s\n", sOK.Render("✓"), sBold.Render(g.Name), sDim.Render(homeShort(g.Dir)))
	if !g.Options.DryRun {
		fmt.Println(sDim.Render(fmt.Sprintf("  rough budget: ~%d scenes · ~%.0f Kling units (~$%.2f) on %s %s; exact estimate after the storyboard",
			pre.Scenes, pre.Units, pre.USD, pre.Model, pre.Resolution)))
	}
	fmt.Println()
	return drive(ctx, eng, g, f.yes, f.plain)
}

// drive runs plan → approval → render for a generation, from wherever it is.
func drive(ctx context.Context, eng *engine.Engine, g *engine.Generation, yes, plain bool) error {
	unit := eng.Config.Kling.UnitPriceUSD
	var err error
	if g.Status != engine.StatusAwaitingApproval && g.Status != engine.StatusSucceeded {
		g, err = runWithProgress(ctx, "plan", g, unit, plain, func(ctx context.Context, obs engine.Observer) (*engine.Generation, error) {
			return eng.Plan(ctx, g.Key, obs)
		})
		if err != nil {
			return reportFailure(eng, g, err)
		}
	}
	if g.Status == engine.StatusSucceeded {
		printSummary(eng, g)
		return nil
	}

	// Approval gate.
	if !engine.StageDoneFor(g, engine.StageRender) {
		est, err := eng.Estimate(g.Key)
		if err != nil {
			return err
		}
		if sb, err := eng.LoadStoryboard(g.Key); err == nil {
			printStoryboard(sb, est)
		}
		if !g.Options.DryRun && !yes {
			if !interactive() {
				return fmt.Errorf("storyboard ready; re-run with --yes to spend ~%.1f Kling units, or `doctok resume %s --yes`", est.Units, g.Key)
			}
			choice := "render"
			err := runForm(huh.NewGroup(huh.NewSelect[string]().
				Title(fmt.Sprintf("Render %d scenes with Kling for ~%.1f units (~$%.2f)?", est.Scenes, est.Units, est.USD)).
				Options(
					huh.NewOption("🚀 Send it", "render"),
					huh.NewOption("⏸  Not now — resume later with `doctok resume "+g.Key+"`", "later"),
				).Value(&choice)))
			if err != nil {
				return err
			}
			if choice != "render" {
				fmt.Println(sDim.Render("saved. storyboard: " + homeShort(g.Dir+"/"+engine.FileScript)))
				return nil
			}
		}
	}
	g, err = runWithProgress(ctx, "render", g, unit, plain, func(ctx context.Context, obs engine.Observer) (*engine.Generation, error) {
		return eng.Render(ctx, g.Key, obs)
	})
	if err != nil {
		return reportFailure(eng, g, err)
	}
	printSummary(eng, g)
	return nil
}

func reportFailure(eng *engine.Engine, g *engine.Generation, err error) error {
	if g != nil {
		if cur, gerr := eng.Store.Get(g.Key); gerr == nil {
			g = cur
		}
	}
	if errors.Is(err, context.Canceled) {
		if g != nil {
			fmt.Println(sWarn.Render("⏯  paused. resume with: ") + sBold.Render("doctok resume "+g.Key))
		}
		return context.Canceled
	}
	if g != nil {
		fmt.Println(sErr.Render("✗ generation failed") + sDim.Render(" — fix the issue then `doctok resume "+g.Key+"` (finished work is kept)"))
	}
	return err
}

func printStoryboard(sb *engine.Storyboard, est *engine.Estimate) {
	fmt.Println(sBold.Render("🎬 " + sb.Title))
	if sb.Hook != "" {
		fmt.Println(sDim.Render("   hook: ") + truncate(sb.Hook, 100))
	}
	for _, c := range sb.Characters {
		fmt.Println(sDim.Render("   cast: ") + sAccent.Render(c.Label) + sDim.Render(" — "+truncate(c.Description, 90)))
	}
	fmt.Println()
	t := 0
	for i, s := range sb.Scenes {
		line := truncate(s.Dialogue(), 70)
		if line == "" {
			line = sDim.Render(truncate(s.Action, 70))
		}
		extra := ""
		if s.Dance != "" {
			extra = sWarn.Render(" 🕺 " + truncate(s.Dance, 24))
		}
		fmt.Printf("  %s %s %-16s %s%s\n", sDim.Render(fmt.Sprintf("%2d", i+1)), sDim.Render(fmtSecs(float64(t))), sAccent.Render(truncate(s.Beat, 16)), line, extra)
		t += s.Seconds
	}
	fmt.Println()
	msg := fmt.Sprintf("%d scenes · %s runtime · %s %s · audio=%v", est.Scenes, fmtSecs(float64(est.Seconds)), est.Model, est.Resolution, est.Audio)
	if est.DryRun {
		msg += " · dry run (no Kling spend)"
	} else {
		msg += fmt.Sprintf(" · est. %.1f units ≈ $%.2f", est.Units, est.USD)
	}
	fmt.Println(sBox.Render(msg))
	fmt.Println()
}

func printSummary(eng *engine.Engine, g *engine.Generation) {
	if cur, err := eng.Store.Get(g.Key); err == nil {
		g = cur
	}
	style, _ := engine.LookupStyle(g.Style)
	rows := [][2]string{
		{"video", homeShort(g.Files["video"])},
		{"length", fmt.Sprintf("%s · %dx%d · %.1f MB · %d scenes", fmtSecs(g.Video.DurationSeconds), g.Video.Width, g.Video.Height, float64(g.Video.SizeBytes)/1e6, g.Video.Scenes)},
		{"style", style.Emoji + " " + style.Name},
	}
	if g.StartedAt != nil {
		rows = append(rows, [2]string{"started", g.StartedAt.Local().Format("Mon Jan 2 15:04:05")})
	}
	rows = append(rows,
		[2]string{"took", fmt.Sprintf("%s wall-clock · %s active", fmtSecs(g.DurationSeconds), fmtSecs(g.ActiveSeconds))},
		[2]string{"claude", fmt.Sprintf("$%.4f · %s · %s · %d in / %d out tokens", g.Claude.CostUSD, orDash(g.Claude.Billing), orDash(g.Claude.Model), g.Claude.InputTokens, g.Claude.OutputTokens)},
	)
	if g.Options.DryRun {
		rows = append(rows, [2]string{"kling", "0 units (dry run — placeholders)"})
	} else {
		tag := "estimated from Kling's price table"
		if g.Kling.UnitsReported {
			tag = "reported by Kling"
		}
		rows = append(rows, [2]string{"kling", fmt.Sprintf("%.2f units ≈ $%.2f (%s) · %s", g.Kling.Units, g.Kling.CostUSD, tag, g.Kling.Model)})
	}
	var b strings.Builder
	b.WriteString(sOK.Render("✨ it's giving finished video ✨") + "\n\n")
	for _, r := range rows {
		b.WriteString(sLabel.Render(r[0]) + r[1] + "\n")
	}
	b.WriteString("\n" + sDim.Render("open: doctok open "+g.Key+"   ·   AI-generated: label it as such when posting"))
	fmt.Println(sBox.Render(b.String()))
}

func orDash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func notionRoute(eng *engine.Engine) string {
	switch eng.Config.Notion.Mode {
	case engine.NotionModeAPI:
		return "the Notion API"
	case engine.NotionModeMCP:
		return "Claude Code's Notion MCP server"
	}
	if eng.Config.Notion.Token != "" {
		return "the Notion API (falls back to Claude Code MCP)"
	}
	return "Claude Code's Notion MCP server (no NOTION_TOKEN set)"
}

func preflightKling(eng *engine.Engine) error {
	k := eng.Config.Kling
	if k.APIKey == "" && (k.AccessKey == "" || k.SecretKey == "") {
		return errors.New("no Kling credentials — run `doctok configure`, export KLING_API_KEY, or use --dry-run")
	}
	return nil
}

func newResumeCmd() *cobra.Command {
	var yes, plain bool
	cmd := &cobra.Command{
		Use:   "resume [generation-key]",
		Short: "Resume an interrupted, failed or awaiting-approval generation",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			key := ""
			if len(args) == 1 {
				key = args[0]
			} else {
				key, err = pickGeneration(eng, "Resume which generation?", func(g *engine.Generation) bool {
					return g.Status != engine.StatusSucceeded && g.Status != engine.StatusRunning
				})
				if err != nil {
					return err
				}
			}
			g, err := eng.Store.Get(key)
			if err != nil {
				return err
			}
			if g.Status == engine.StatusSucceeded {
				printSummary(eng, g)
				return nil
			}
			if !g.Options.DryRun {
				if err := preflightKling(eng); err != nil && engine.StageDoneFor(g, engine.StageScript) {
					return err
				}
			}
			fmt.Println(logo())
			fmt.Println()
			return drive(cmd.Context(), eng, g, yes, plain)
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask before spending Kling credits")
	cmd.Flags().BoolVar(&plain, "plain", false, "plain log output")
	return cmd
}

func pickGeneration(eng *engine.Engine, title string, filter func(*engine.Generation) bool) (string, error) {
	gens, err := eng.Store.List()
	if err != nil {
		return "", err
	}
	var opts []huh.Option[string]
	for _, g := range gens {
		if filter != nil && !filter(g) {
			continue
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-40s %s  %s", truncate(g.Name, 40), statusBadge(g.Status), sDim.Render(g.CreatedAt.Local().Format("Jan 2 15:04"))), g.Key))
	}
	if len(opts) == 0 {
		return "", errors.New("no matching generations")
	}
	if !interactive() {
		return "", errors.New("pass a generation key")
	}
	var key string
	if err := runForm(huh.NewGroup(huh.NewSelect[string]().Title(title).Options(opts...).Value(&key).Height(min(12, len(opts)+2)))); err != nil {
		return "", err
	}
	return key, nil
}
