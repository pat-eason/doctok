// Package cli is doctok's command-line frontend. It only talks to the
// generation engine through the engine package's public API.
package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/charmbracelet/huh"
	"github.com/pat-eason/doctok/engine"
	"github.com/spf13/cobra"
)

// Version is set at build time.
var Version = "dev"

// Execute runs the CLI.
func Execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	root := newRoot()
	if err := root.ExecuteContext(ctx); err != nil {
		if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, context.Canceled) {
			fmt.Fprintln(os.Stderr, sDim.Render("bye bestie 👋"))
			return 130
		}
		fmt.Fprintln(os.Stderr, sErr.Render("✗ ")+err.Error())
		return 1
	}
	return 0
}

func openEngine() (*engine.Engine, error) { return engine.Open() }

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "doctok",
		Short:         "Turn dense technical docs into 3–5 minute TikTok-influencer explainer videos",
		Long:          logo() + "\n\nReads a Notion page or Markdown file with your local Claude Code, writes a\nGen-Z storyboard, renders every scene with Kling and cuts a 1080x1920 video.\nArtifacts live in ~/.doctok/<generation-key>/ and state in ~/.doctok/state.json.",
		Version:       Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() {
				return cmd.Help()
			}
			return mainMenu(cmd)
		},
	}
	root.AddCommand(
		newDoctorCmd(),
		newConfigureCmd(),
		newGenerateCmd(),
		newResumeCmd(),
		newListCmd(),
		newShowCmd(),
		newOpenCmd(),
		newRmCmd(),
		newStylesCmd(),
		newTrendsCmd(),
		newUICmd(),
	)
	return root
}

func mainMenu(cmd *cobra.Command) error {
	fmt.Println(logo())
	fmt.Println()
	var choice string
	err := runForm(huh.NewGroup(
		huh.NewSelect[string]().Title("What are we doing today?").Options(
			huh.NewOption("🎬  Generate a video from a doc", "generate"),
			huh.NewOption("🩺  Doctor — check Claude, Kling, Notion & ffmpeg", "doctor"),
			huh.NewOption("⚙️   Configure", "configure"),
			huh.NewOption("📼  List generations", "list"),
			huh.NewOption("💅  Browse styles", "styles"),
			huh.NewOption("📈  Refresh TikTok trends", "trends"),
			huh.NewOption("🖥️   Open the local UI", "ui"),
		).Value(&choice),
	))
	if err != nil {
		return err
	}
	sub, _, err := cmd.Root().Find([]string{choice})
	if err != nil {
		return err
	}
	sub.SetContext(cmd.Context())
	return sub.RunE(sub, nil)
}
