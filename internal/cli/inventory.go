package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/lipgloss/table"
	"github.com/pat-eason/doctok/engine"
	"github.com/spf13/cobra"
)

func newListCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List generations in ~/.doctok/state.json",
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			gens, err := eng.Store.List()
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(gens)
			}
			if len(gens) == 0 {
				fmt.Println(sDim.Render("no generations yet — `doctok generate` to make your first one 💅"))
				return nil
			}
			rows := [][]string{}
			for _, g := range gens {
				style := engine.GenerationStyle(g)
				kl := fmt.Sprintf("%.1f u / $%.2f", g.Kling.Units, g.Kling.CostUSD)
				if g.Options.DryRun {
					kl = "dry run"
				}
				took := "—"
				if g.DurationSeconds > 0 {
					took = fmtSecs(g.DurationSeconds)
				} else if g.StartedAt != nil && g.Status == engine.StatusRunning {
					took = fmtDuration(time.Since(*g.StartedAt)) + "…"
				}
				length := "—"
				if g.Video.DurationSeconds > 0 {
					length = fmtSecs(g.Video.DurationSeconds)
				}
				rows = append(rows, []string{
					truncate(g.Key, 36), statusPlain(g), style.Emoji + " " + truncate(style.ID, 18),
					g.CreatedAt.Local().Format("Jan 2 15:04"), took, length,
					fmt.Sprintf("$%.3f", g.Claude.CostUSD), kl,
				})
			}
			t := table.New().
				Border(lipgloss.RoundedBorder()).BorderStyle(lipgloss.NewStyle().Foreground(gray)).
				Headers("KEY", "STATUS", "STYLE", "CREATED", "TOOK", "LENGTH", "CLAUDE", "KLING").
				Rows(rows...).
				StyleFunc(func(row, col int) lipgloss.Style {
					s := lipgloss.NewStyle().Padding(0, 1)
					if row == table.HeaderRow {
						return s.Bold(true).Foreground(pink)
					}
					if col == 1 {
						switch {
						case strings.HasPrefix(rows[row][1], "✓"):
							return s.Foreground(green)
						case strings.HasPrefix(rows[row][1], "✗"):
							return s.Foreground(red)
						case strings.HasPrefix(rows[row][1], "◐"):
							return s.Foreground(cyan)
						default:
							return s.Foreground(yellow)
						}
					}
					return s
				})
			fmt.Println(t)
			fmt.Println(sDim.Render(homeShort(eng.Home + "/state.json")))
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print raw JSON")
	return cmd
}

func statusPlain(g *engine.Generation) string {
	switch g.Status {
	case engine.StatusSucceeded:
		return "✓ done"
	case engine.StatusRunning:
		return fmt.Sprintf("◐ %s %.0f%%", g.Stage, g.Progress*100)
	case engine.StatusAwaitingApproval:
		return "⏸ approve"
	case engine.StatusInterrupted:
		return "⏯ paused"
	case engine.StatusFailed:
		return "✗ failed"
	}
	return "· " + g.Status
}

func newShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show [generation-key]",
		Short: "Show a generation's details, timings and costs",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			key, err := keyArg(eng, args, "Show which generation?")
			if err != nil {
				return err
			}
			g, err := eng.Store.Get(key)
			if err != nil {
				return err
			}
			if asJSON {
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(g)
			}
			style := engine.GenerationStyle(g)
			fmt.Println(sTitle.Render(g.Name) + "  " + statusBadge(g.Status))
			kv := func(k, v string) { fmt.Println(sLabel.Render(k) + v) }
			kv("key", g.Key)
			kv("dir", homeShort(g.Dir))
			kv("source", fmt.Sprintf("%s (%s via %s) %s", g.Source.Ref, g.Source.Kind, orDash(g.Source.Via), sDim.Render(g.Source.Title)))
			kv("style", style.Emoji+" "+style.Name)
			kv("options", fmt.Sprintf("%ds target · %s %s · audio=%v · captions=%v · dry-run=%v", g.Options.TargetSeconds, g.Options.KlingModel, g.Options.Resolution, g.Options.Audio, g.Options.Captions, g.Options.DryRun))
			kv("created", g.CreatedAt.Local().Format(time.RFC1123))
			if g.StartedAt != nil {
				kv("started", g.StartedAt.Local().Format(time.RFC1123))
			}
			if g.FinishedAt != nil {
				kv("finished", g.FinishedAt.Local().Format(time.RFC1123))
			}
			kv("took", fmt.Sprintf("%s wall-clock · %s active", fmtSecs(g.DurationSeconds), fmtSecs(g.ActiveSeconds)))
			for _, st := range engine.Stages {
				if t := g.StageTimes[st]; t != nil && t.Seconds > 0 {
					kv("", sDim.Render(fmt.Sprintf("%-26s %s", engine.StageLabels[st], fmtSecs(t.Seconds))))
				}
			}
			kv("claude", fmt.Sprintf("$%.4f · %s · %s · %d in / %d out tokens", g.Claude.CostUSD, orDash(g.Claude.Billing), orDash(g.Claude.Model), g.Claude.InputTokens, g.Claude.OutputTokens))
			for _, c := range g.Claude.Calls {
				kv("", sDim.Render(fmt.Sprintf("%-14s $%.4f  %5.1fs  %s", c.Purpose, c.CostUSD, float64(c.DurationMS)/1000, c.Model)))
			}
			tag := "estimated"
			if g.Kling.UnitsReported {
				tag = "reported"
			}
			kv("kling", fmt.Sprintf("%.2f units ≈ $%.2f (%s) · budget %.1f units · %s API", g.Kling.Units, g.Kling.CostUSD, tag, g.Kling.EstimatedUnits, orDash(g.Kling.APIStyle)))
			if len(g.Kling.Clips) > 0 {
				fmt.Println(sLabel.Render("") + clipGrid(g.Kling.Clips))
			}
			if g.Video.DurationSeconds > 0 {
				kv("video", fmt.Sprintf("%s · %dx%d · %.1f MB", fmtSecs(g.Video.DurationSeconds), g.Video.Width, g.Video.Height, float64(g.Video.SizeBytes)/1e6))
			}
			for _, f := range []string{"doc", "distill", "script", "video"} {
				if p := g.Files[f]; p != "" {
					kv(f, homeShort(p))
				}
			}
			if g.Error != "" {
				kv("error", sErr.Render(g.Error))
			}
			if g.Message != "" {
				kv("note", sDim.Render(g.Message))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print raw JSON")
	return cmd
}

func keyArg(eng *engine.Engine, args []string, title string) (string, error) {
	if len(args) == 1 {
		return args[0], nil
	}
	return pickGeneration(eng, title, nil)
}

func newOpenCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "open [generation-key]",
		Short: "Open a finished VIDEO.mp4 (or the generation folder)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			key, err := keyArg(eng, args, "Open which generation?")
			if err != nil {
				return err
			}
			g, err := eng.Store.Get(key)
			if err != nil {
				return err
			}
			target := g.Files["video"]
			if target == "" {
				target = g.Dir
			}
			return openPath(target)
		},
	}
}

func openPath(p string) error {
	var c *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		c = exec.Command("open", p)
	case "windows":
		c = exec.Command("cmd", "/c", "start", "", p)
	default:
		c = exec.Command("xdg-open", p)
	}
	return c.Start()
}

func newRmCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:     "rm [generation-key]",
		Aliases: []string{"delete"},
		Short:   "Delete a generation and its artifacts",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			key, err := keyArg(eng, args, "Delete which generation?")
			if err != nil {
				return err
			}
			g, err := eng.Store.Get(key)
			if err != nil {
				return err
			}
			if !force {
				if !interactive() {
					return errors.New("pass --force to delete non-interactively")
				}
				ok := false
				if err := runForm(huh.NewGroup(huh.NewConfirm().
					Title(fmt.Sprintf("Delete %s and everything in %s?", g.Key, homeShort(g.Dir))).
					Affirmative("Delete").Negative("Keep").Value(&ok))); err != nil || !ok {
					return err
				}
			}
			if err := eng.Delete(key); err != nil {
				return err
			}
			fmt.Println(sOK.Render("✓ deleted ") + key)
			return nil
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "don't ask")
	return cmd
}

func newStylesCmd() *cobra.Command {
	var verbose bool
	cmd := &cobra.Command{
		Use:   "styles",
		Short: "List video style presets",
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			def := eng.DefaultStyle().ID
			for _, s := range eng.Styles() {
				mark := "  "
				if s.ID == def {
					mark = sOK.Render("★ ")
				}
				fmt.Printf("%s%s %s %s\n", mark, s.Emoji, sBold.Render(s.Name), sDim.Render("("+s.ID+")"))
				fmt.Println("     " + s.Description)
				if verbose {
					fmt.Println(sDim.Render("     persona: " + truncate(s.Persona, 110)))
					beats := make([]string, len(s.Structure))
					for i, b := range s.Structure {
						beats[i] = fmt.Sprintf("%s %d%%", b.Name, b.Percent)
					}
					fmt.Println(sDim.Render("     arc:     " + strings.Join(beats, " → ")))
					fmt.Println(sDim.Render("     moves:   " + strings.Join(s.Moves, ", ")))
					fmt.Println(sDim.Render("     slang:   " + truncate(strings.Join(s.Slang, ", "), 110)))
				}
				fmt.Println()
			}
			info := eng.TrendsInfo()
			age := fmt.Sprintf("%s catalog from %s (%d days old)", info.Source, info.GeneratedAt.Format("Jan 2 2006"), info.AgeDays)
			if info.Stale {
				age = sWarn.Render(age + " — run `doctok trends refresh`")
			} else {
				age = sDim.Render(age)
			}
			fmt.Println(age)
			fmt.Println(sDim.Render("use: doctok generate <doc> --style <id>   ·   -v for details   ·   doctok trends refresh to re-research"))
			return nil
		},
	}
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show persona, arc, moves and slang")
	return cmd
}
