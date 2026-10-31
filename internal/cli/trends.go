package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/pat-eason/doctok/engine"
	"github.com/spf13/cobra"
)

func newTrendsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "trends",
		Short: "Show, refresh (re-research) or reset the TikTok trend style catalog",
		Long: "TikTok formats and slang change constantly. `doctok trends refresh` has your local Claude Code\n" +
			"research current trends on the web and rebuilds the style presets; you review the diff before\n" +
			"anything is applied. Generations keep the style snapshot they were created with.",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() {
				return runTrendsShow(false)
			}
			eng, err := openEngine()
			if err != nil {
				return err
			}
			info := eng.TrendsInfo()
			fmt.Println(logo())
			fmt.Printf("%s %d styles · %s catalog from %s (%d days old)\n\n", sBold.Render("📈 trends"), info.Styles, info.Source, info.GeneratedAt.Format("Jan 2 2006"), info.AgeDays)
			choice := "refresh"
			opts := []huh.Option[string]{
				huh.NewOption("🔎 Refresh — re-research current TikTok trends with Claude", "refresh"),
				huh.NewOption("📋 Show the current catalog, notes & sources", "show"),
			}
			if info.Source == "refreshed" {
				opts = append(opts, huh.NewOption("↩️  Reset to the built-in catalog", "reset"))
			}
			if err := runForm(huh.NewGroup(huh.NewSelect[string]().Title("Trends").Options(opts...).Value(&choice))); err != nil {
				return err
			}
			switch choice {
			case "show":
				return runTrendsShow(false)
			case "reset":
				return runTrendsReset(false)
			}
			return runTrendsRefresh(cmd, trendsFlags{})
		},
	}
	var asJSON bool
	show := &cobra.Command{
		Use:   "show",
		Short: "Show the active catalog, its age, trend notes and sources",
		RunE:  func(cmd *cobra.Command, args []string) error { return runTrendsShow(asJSON) },
	}
	show.Flags().BoolVar(&asJSON, "json", false, "print the trends file as JSON")

	var f trendsFlags
	refresh := &cobra.Command{
		Use:   "refresh",
		Short: "Re-research current TikTok trends with Claude Code (web search) and rebuild the presets",
		Example: `  doctok trends refresh
  doctok trends refresh --focus "more duet and green-screen formats"
  doctok trends refresh --replace --yes`,
		RunE: func(cmd *cobra.Command, args []string) error { return runTrendsRefresh(cmd, f) },
	}
	refresh.Flags().BoolVar(&f.replace, "replace", false, "replace the whole catalog instead of merging into the built-ins")
	refresh.Flags().StringVar(&f.focus, "focus", "", "extra direction for the research (e.g. a niche or vibe)")
	refresh.Flags().StringVar(&f.model, "model", "", "Claude model for the research (default: your configured model)")
	refresh.Flags().BoolVarP(&f.yes, "yes", "y", false, "apply without asking")

	var yes bool
	reset := &cobra.Command{
		Use:   "reset",
		Short: "Revert to the built-in catalog (the current one is archived)",
		RunE:  func(cmd *cobra.Command, args []string) error { return runTrendsReset(yes) },
	}
	reset.Flags().BoolVarP(&yes, "yes", "y", false, "don't ask")
	cmd.AddCommand(show, refresh, reset)
	return cmd
}

type trendsFlags struct {
	replace, yes bool
	focus, model string
}

func runTrendsShow(asJSON bool) error {
	eng, err := openEngine()
	if err != nil {
		return err
	}
	tf, err := eng.LoadTrends()
	if err != nil {
		return err
	}
	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if tf == nil {
			return enc.Encode(map[string]any{"source": "built-in", "generated_at": engine.BuiltinTrendsDate, "styles": eng.Styles()})
		}
		return enc.Encode(tf)
	}
	info := eng.TrendsInfo()
	fmt.Println(sTitle.Render("📈 TikTok trend catalog"))
	kv := func(k, v string) { fmt.Println(sLabel.Render(k) + v) }
	age := fmt.Sprintf("%s (%d days ago)", info.GeneratedAt.Local().Format("Mon Jan 2 2006"), info.AgeDays)
	if info.Stale {
		age = sWarn.Render(age + " — stale, run `doctok trends refresh`")
	}
	kv("source", info.Source)
	kv("updated", age)
	kv("mode", info.Mode)
	if tf != nil {
		kv("research", fmt.Sprintf("$%.4f · %s · %s · %d sources", tf.CostUSD, orDash(tf.Billing), orDash(tf.Model), len(tf.Sources)))
		if tf.Focus != "" {
			kv("focus", tf.Focus)
		}
		kv("report", homeShort(eng.Home+"/TRENDS.md"))
	}
	fmt.Println()
	for _, s := range eng.Styles() {
		fmt.Printf("  %s %-28s %s\n", s.Emoji, sBold.Render(s.Name), sDim.Render(s.ID))
	}
	if tf != nil && strings.TrimSpace(tf.Notes) != "" {
		fmt.Println()
		fmt.Println(sAccent.Render("TREND NOTES"))
		lines := strings.Split(strings.TrimSpace(tf.Notes), "\n")
		if len(lines) > 30 {
			lines = append(lines[:30], sDim.Render(fmt.Sprintf("… %d more lines in TRENDS.md", len(lines)-30)))
		}
		for _, l := range lines {
			fmt.Println("  " + l)
		}
		fmt.Println()
		fmt.Println(sAccent.Render("SOURCES"))
		for i, s := range tf.Sources {
			if i == 15 {
				fmt.Println(sDim.Render(fmt.Sprintf("  … %d more", len(tf.Sources)-15)))
				break
			}
			fmt.Printf("  %s %s\n", truncate(orDash(s.Title), 60), sDim.Render(s.URL))
		}
	}
	if tf == nil {
		fmt.Println(sDim.Render("\nbuilt-in catalog — `doctok trends refresh` to re-research with Claude"))
	}
	return nil
}

func runTrendsRefresh(cmd *cobra.Command, f trendsFlags) error {
	eng, err := openEngine()
	if err != nil {
		return err
	}
	fmt.Println(logo())
	fmt.Println(sBold.Render("📈 trends refresh") + sDim.Render("  Claude Code researches TikTok on the web (WebSearch/WebFetch). Usually 3–10 minutes."))
	if interactive() && !f.yes && !cmd.Flags().Changed("replace") {
		mode := "merge"
		if err := runForm(huh.NewGroup(
			huh.NewSelect[string]().Title("How should results be applied?").Options(
				huh.NewOption("Merge — keep built-ins, add new & update stale presets", "merge"),
				huh.NewOption("Replace — the research defines the entire catalog", "replace"),
			).Value(&mode),
			huh.NewInput().Title("Any focus? (optional)").Placeholder("e.g. more duet formats, B2B-friendly tone").Value(&f.focus),
		)); err != nil {
			return err
		}
		f.replace = mode == "replace"
	}
	fmt.Println()
	start := time.Now()
	var sp *spinnerHandle
	if interactive() {
		sp = startSpinner("researching…")
	}
	log := func(msg string) {
		if sp != nil {
			sp.Stop()
		}
		icon := "  "
		switch {
		case strings.HasPrefix(msg, "search:"):
			icon = "🔎"
		case strings.HasPrefix(msg, "fetch:"):
			icon = "📄"
		}
		fmt.Printf("%s %s %s\n", sDim.Render(fmtDuration(time.Since(start))), icon, truncate(msg, 110))
		if interactive() {
			sp = startSpinner("researching…")
		}
	}
	p, err := eng.RefreshTrends(cmd.Context(), engine.TrendsRefreshOptions{Replace: f.replace, Focus: f.focus, Model: f.model, Log: log})
	if sp != nil {
		sp.Stop()
	}
	if err != nil {
		return err
	}
	fmt.Println()
	printTrendsDiff(p)
	if !f.yes {
		if !interactive() {
			return fmt.Errorf("proposal saved to %s; re-run with --yes to apply", homeShort(p.Path))
		}
		apply := true
		if err := runForm(huh.NewGroup(huh.NewConfirm().Title("Apply this catalog?").Affirmative("Apply ✨").Negative("Discard").Value(&apply))); err != nil {
			return err
		}
		if !apply {
			fmt.Println(sDim.Render("not applied. the proposal is saved at " + homeShort(p.Path)))
			return nil
		}
	}
	if err := eng.ApplyTrends(p); err != nil {
		return err
	}
	fmt.Println(sOK.Render("✓ catalog updated") + sDim.Render(" · report: "+homeShort(eng.Home+"/TRENDS.md")+" · previous version archived in trends-history/"))
	if def := eng.Config.Video.Style; def != "" {
		if _, err := eng.Style(def); err != nil {
			fmt.Println(sWarn.Render(fmt.Sprintf("your default style %q no longer exists; `doctok configure` to pick a new one (falling back to %s)", def, eng.DefaultStyle().ID)))
		}
	}
	return nil
}

func printTrendsDiff(p *engine.TrendsProposal) {
	byID := map[string]engine.Style{}
	for _, s := range p.Catalog {
		byID[s.ID] = s
	}
	line := func(sym, id string, style func(...string) string) {
		s, ok := byID[id]
		if !ok {
			fmt.Printf("  %s %s\n", style(sym), style(id))
			return
		}
		fmt.Printf("  %s %s %-28s %s\n", style(sym), s.Emoji, style(s.Name), sDim.Render(truncate(s.Description, 70)))
	}
	fmt.Println(sBold.Render(fmt.Sprintf("Proposed catalog: %d styles (%s mode, %d sources, $%.4f)", len(p.Catalog), p.File.Mode, len(p.File.Sources), p.File.CostUSD)))
	for _, id := range p.Added {
		line("+", id, sOK.Render)
	}
	for _, id := range p.Updated {
		line("~", id, sWarn.Render)
	}
	for _, id := range p.Removed {
		line("-", id, sErr.Render)
	}
	if len(p.Kept) > 0 {
		fmt.Println(sDim.Render(fmt.Sprintf("  = unchanged: %s", strings.Join(p.Kept, ", "))))
	}
	if len(p.Warnings) > 0 {
		fmt.Println(sDim.Render(fmt.Sprintf("  (%d fixes applied during validation, e.g. %s)", len(p.Warnings), truncate(p.Warnings[0], 80))))
	}
	if notes := strings.TrimSpace(p.File.Notes); notes != "" {
		fmt.Println()
		fmt.Println(sAccent.Render("TREND NOTES (excerpt)"))
		lines := strings.Split(notes, "\n")
		if len(lines) > 14 {
			lines = append(lines[:14], "…")
		}
		for _, l := range lines {
			fmt.Println("  " + truncate(l, 120))
		}
	}
	fmt.Println()
}

func runTrendsReset(yes bool) error {
	eng, err := openEngine()
	if err != nil {
		return err
	}
	if tf, _ := eng.LoadTrends(); tf == nil {
		fmt.Println(sDim.Render("already using the built-in catalog"))
		return nil
	}
	if !yes {
		if !interactive() {
			return errors.New("pass --yes to reset non-interactively")
		}
		ok := false
		if err := runForm(huh.NewGroup(huh.NewConfirm().Title("Revert to the built-in catalog? (current one is archived)").Value(&ok))); err != nil || !ok {
			return err
		}
	}
	if err := eng.ResetTrends(); err != nil {
		return err
	}
	fmt.Println(sOK.Render("✓ reverted to the built-in catalog"))
	return nil
}
