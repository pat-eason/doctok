package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/huh/spinner"
	"github.com/pat-eason/doctok/engine"
	"github.com/spf13/cobra"
)

func newDoctorCmd() *cobra.Command {
	var live, skipMCP, asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Verify Claude Code (subscription or API key), Kling, Notion and ffmpeg",
		RunE: func(cmd *cobra.Command, args []string) error {
			eng, err := openEngine()
			if err != nil {
				return err
			}
			opts := engine.DoctorOptions{Live: live, SkipMCP: skipMCP}
			if asJSON {
				rep := eng.Doctor(cmd.Context(), opts, nil)
				enc := json.NewEncoder(os.Stdout)
				enc.SetIndent("", "  ")
				return enc.Encode(rep)
			}
			if interactive() && !cmd.Flags().Changed("live") {
				_ = runForm(huh.NewGroup(huh.NewConfirm().
					Title("Run a live Claude round-trip too?").
					Description("Sends a ~10-token prompt through your preferred Claude instance (fractions of a cent).").
					Affirmative("Yes, test it").Negative("Skip").Value(&live)))
				opts.Live = live
			}
			fmt.Println(logo())
			fmt.Println(sBold.Render("🩺 doctor") + sDim.Render("  checking the vibes…"))
			rep := runDoctor(cmd, eng, opts)
			return doctorFollowUp(cmd, rep)
		},
	}
	cmd.Flags().BoolVar(&live, "live", false, "send a tiny live prompt through Claude Code")
	cmd.Flags().BoolVar(&skipMCP, "skip-mcp", false, "skip the (slow) Claude Code MCP server health check")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the report as JSON")
	return cmd
}

func runDoctor(cmd *cobra.Command, eng *engine.Engine, opts engine.DoctorOptions) engine.DoctorReport {
	var rep engine.DoctorReport
	group := ""
	current := ""
	var sp *spinnerHandle
	onCheck := func(c engine.Check) {
		if c.Status == "" { // started
			if c.Group != group {
				group = c.Group
				fmt.Println()
				fmt.Println(sAccent.Render(strings.ToUpper(group)))
			}
			current = c.Name
			if interactive() {
				sp = startSpinner(current + "…")
			}
			return
		}
		if sp != nil {
			sp.Stop()
			sp = nil
		}
		fmt.Println(renderCheck(c))
	}
	rep = eng.Doctor(cmd.Context(), opts, onCheck)
	fmt.Println()
	fails, warns := 0, 0
	for _, c := range rep.Checks {
		switch c.Status {
		case engine.CheckFail:
			fails++
		case engine.CheckWarn:
			warns++
		}
	}
	switch {
	case rep.Ready && fails == 0:
		fmt.Println(sOK.Render("✨ all good — you're ready to generate. it's giving production-ready."))
	case rep.Ready:
		fmt.Println(sWarn.Render(fmt.Sprintf("ready to generate, but %d check(s) failed (optional features).", fails)))
	default:
		fmt.Println(sErr.Render(fmt.Sprintf("not ready: %d failed, %d warnings.", fails, warns)) + sDim.Render(" (dry runs still work with just Claude + ffmpeg)"))
	}
	return rep
}

func renderCheck(c engine.Check) string {
	var icon string
	switch c.Status {
	case engine.CheckOK:
		icon = sOK.Render("✓")
	case engine.CheckWarn:
		icon = sWarn.Render("!")
	case engine.CheckFail:
		icon = sErr.Render("✗")
	default:
		icon = sDim.Render("–")
	}
	line := fmt.Sprintf("  %s %-34s %s", icon, c.Name, c.Detail)
	if c.Status == engine.CheckSkip {
		line = sDim.Render(fmt.Sprintf("  – %-34s %s", c.Name, c.Detail))
	}
	if c.Hint != "" && c.Status != engine.CheckOK {
		line += "\n" + sDim.Render("      ↳ "+c.Hint)
	}
	return line
}

func doctorFollowUp(cmd *cobra.Command, rep engine.DoctorReport) error {
	if !interactive() {
		return nil
	}
	needs := false
	for _, c := range rep.Checks {
		if c.Status == engine.CheckFail && (strings.HasPrefix(c.ID, "kling") || strings.HasPrefix(c.ID, "claude") || c.ID == "config") {
			needs = true
		}
	}
	if !needs {
		return nil
	}
	fix := true
	if err := runForm(huh.NewGroup(huh.NewConfirm().Title("Fix it now with `doctok configure`?").Value(&fix))); err != nil || !fix {
		return nil
	}
	return runConfigure(cmd)
}

// spinnerHandle runs a huh spinner in the background until Stop.
type spinnerHandle struct {
	done     chan struct{}
	finished chan struct{}
}

func startSpinner(title string) *spinnerHandle {
	h := &spinnerHandle{done: make(chan struct{}), finished: make(chan struct{})}
	go func() {
		defer close(h.finished)
		_ = spinner.New().Title(" " + title).Action(func() { <-h.done }).Run()
	}()
	return h
}

func (h *spinnerHandle) Stop() {
	close(h.done)
	<-h.finished
}
