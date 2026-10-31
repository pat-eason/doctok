package cli

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/pat-eason/doctok/engine"
	"github.com/pat-eason/doctok/engine/kling"
	"github.com/spf13/cobra"
)

func newConfigureCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "configure",
		Aliases: []string{"config", "setup"},
		Short:   "Interactively configure Claude, Kling, Notion and video defaults",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !interactive() {
				return errors.New("configure is interactive; edit ~/.doctok/config.json or use env vars (KLING_API_KEY, NOTION_TOKEN, ANTHROPIC_API_KEY)")
			}
			return runConfigure(cmd)
		},
	}
}

func runConfigure(cmd *cobra.Command) error {
	eng, err := openEngine()
	if err != nil {
		return err
	}
	cfg := eng.Config
	fmt.Println(logo())
	fmt.Println(sBold.Render("⚙️  configure") + sDim.Render("  secrets are stored in ~/.doctok/config.json (0600); leave blank to keep current / use env"))
	fmt.Println()

	// --- Claude
	claudeMode := cfg.Claude.Mode
	claudeKey := ""
	claudeModel := cfg.Claude.Model
	// --- Kling
	klingAuth := "apikey"
	if cfg.Kling.APIKey == "" && cfg.Kling.AccessKey != "" {
		klingAuth = "aksk"
	}
	klingKey, klingAK, klingSK := "", "", ""
	klingModel := cfg.Kling.Model
	klingRes := cfg.Kling.Resolution
	klingAudio := cfg.Kling.Audio
	klingConc := strconv.Itoa(cfg.Kling.Concurrency)
	klingBase := cfg.Kling.BaseURL
	// --- Notion
	notionMode := cfg.Notion.Mode
	notionToken := ""
	// --- Video
	style := cfg.Video.Style
	length := strconv.Itoa(cfg.Video.TargetSeconds)
	captions := cfg.Video.Captions

	keyNote := func(set, env bool) string {
		switch {
		case env:
			return "currently set from environment"
		case set:
			return "currently set — leave blank to keep"
		}
		return "not set"
	}

	modelOpts := []huh.Option[string]{}
	for _, id := range kling.ModelIDs() {
		m := kling.Models[id]
		modelOpts = append(modelOpts, huh.NewOption(m.Label, id))
	}
	styleOpts := []huh.Option[string]{}
	for _, s := range eng.Styles() {
		styleOpts = append(styleOpts, huh.NewOption(fmt.Sprintf("%s  %s — %s", s.Emoji, s.Name, s.Description), s.ID))
	}

	err = runForm(
		huh.NewGroup(
			huh.NewNote().Title("Claude").Description("Claude Code reads & distills your docs. Use your Claude subscription login or an Anthropic API key."),
			huh.NewSelect[string]().Title("Preferred Claude instance").Options(
				huh.NewOption("Auto — subscription if logged in, else API key", engine.ClaudeModeAuto),
				huh.NewOption("Claude subscription (claude auth login)", engine.ClaudeModeSubscription),
				huh.NewOption("Anthropic API key", engine.ClaudeModeAPI),
			).Value(&claudeMode),
			huh.NewInput().Title("Anthropic API key").Description(keyNote(cfg.Claude.APIKey != "", cfg.FromEnv("claude.api_key"))).
				EchoMode(huh.EchoModePassword).Value(&claudeKey),
			huh.NewInput().Title("Claude model (optional)").Description("alias like opus / sonnet / fable, or blank for Claude Code's default").Value(&claudeModel),
		),
		huh.NewGroup(
			huh.NewNote().Title("Kling").Description("Kling renders every scene. Get credentials at https://kling.ai/dev"),
			huh.NewSelect[string]().Title("Kling auth").Options(
				huh.NewOption("API key (current standard API)", "apikey"),
				huh.NewOption("Access key + secret key (legacy JWT API)", "aksk"),
			).Value(&klingAuth),
		),
		huh.NewGroup(
			huh.NewInput().Title("Kling API key").Description(keyNote(cfg.Kling.APIKey != "", cfg.FromEnv("kling.api_key"))).
				EchoMode(huh.EchoModePassword).Value(&klingKey),
		).WithHideFunc(func() bool { return klingAuth != "apikey" }),
		huh.NewGroup(
			huh.NewInput().Title("Kling access key").Description(keyNote(cfg.Kling.AccessKey != "", cfg.FromEnv("kling.access_key"))).Value(&klingAK),
			huh.NewInput().Title("Kling secret key").Description(keyNote(cfg.Kling.SecretKey != "", cfg.FromEnv("kling.secret_key"))).
				EchoMode(huh.EchoModePassword).Value(&klingSK),
		).WithHideFunc(func() bool { return klingAuth != "aksk" }),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Kling model").Options(modelOpts...).Value(&klingModel),
			huh.NewSelect[string]().Title("Clip resolution").Options(
				huh.NewOption("1080p (recommended; needed for native audio)", "1080p"),
				huh.NewOption("720p (cheaper; upscaled to 1080x1920)", "720p"),
			).Value(&klingRes),
			huh.NewConfirm().Title("Native audio (the influencer actually talks)?").Value(&klingAudio),
			huh.NewInput().Title("Parallel Kling tasks").Description("match your resource pack's concurrency").Value(&klingConc).Validate(posInt),
			huh.NewInput().Title("Kling API base URL").Value(&klingBase),
		),
		huh.NewGroup(
			huh.NewNote().Title("Notion").Description("Fetch pages with a Notion integration token, or through a Notion MCP server configured in Claude Code."),
			huh.NewSelect[string]().Title("Notion retrieval").Options(
				huh.NewOption("Auto — API token if set, else Claude Code MCP", engine.NotionModeAuto),
				huh.NewOption("Notion API token only", engine.NotionModeAPI),
				huh.NewOption("Claude Code MCP only", engine.NotionModeMCP),
			).Value(&notionMode),
			huh.NewInput().Title("Notion integration token (optional)").Description(keyNote(cfg.Notion.Token != "", cfg.FromEnv("notion.token"))).
				EchoMode(huh.EchoModePassword).Value(&notionToken),
		),
		huh.NewGroup(
			huh.NewNote().Title("Video defaults"),
			huh.NewSelect[string]().Title("Default style").Options(styleOpts...).Value(&style).Height(12),
			huh.NewInput().Title("Target length (seconds, 180–300)").Value(&length).Validate(func(s string) error {
				n, err := strconv.Atoi(strings.TrimSpace(s))
				if err != nil || n < cfg.Video.MinSeconds || n > cfg.Video.MaxSeconds {
					return fmt.Errorf("enter %d–%d", cfg.Video.MinSeconds, cfg.Video.MaxSeconds)
				}
				return nil
			}),
			huh.NewConfirm().Title("Burn in TikTok-style captions?").Value(&captions),
		),
	)
	if err != nil {
		return err
	}

	cfg.Claude.Mode = claudeMode
	cfg.Claude.Model = strings.TrimSpace(claudeModel)
	if k := strings.TrimSpace(claudeKey); k != "" {
		cfg.Claude.APIKey = k
	}
	if klingAuth == "apikey" {
		if k := strings.TrimSpace(klingKey); k != "" {
			cfg.Kling.APIKey = k
		}
		cfg.Kling.APIStyle = engine.KlingStyleStandard
	} else {
		if k := strings.TrimSpace(klingAK); k != "" {
			cfg.Kling.AccessKey = k
		}
		if k := strings.TrimSpace(klingSK); k != "" {
			cfg.Kling.SecretKey = k
		}
		cfg.Kling.APIStyle = engine.KlingStyleLegacy
	}
	cfg.Kling.Model = klingModel
	cfg.Kling.Resolution = klingRes
	cfg.Kling.Audio = klingAudio
	cfg.Kling.Concurrency, _ = strconv.Atoi(strings.TrimSpace(klingConc))
	cfg.Kling.BaseURL = strings.TrimSpace(klingBase)
	cfg.Notion.Mode = notionMode
	if k := strings.TrimSpace(notionToken); k != "" {
		cfg.Notion.Token = k
	}
	cfg.Video.Style = style
	cfg.Video.TargetSeconds, _ = strconv.Atoi(strings.TrimSpace(length))
	cfg.Video.Captions = captions

	if spec, ok := kling.Lookup(cfg.Kling.Model); ok {
		if err := spec.Validate(cfg.Kling.Resolution, cfg.Kling.Audio); err != nil {
			return fmt.Errorf("%w — pick 1080p or turn off audio", err)
		}
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := engine.SaveConfig(eng.Home, cfg); err != nil {
		return err
	}
	fmt.Println(sOK.Render("✓ saved ") + homeShort(eng.Home+"/config.json"))

	check := true
	if err := runForm(huh.NewGroup(huh.NewConfirm().Title("Run doctor now?").Value(&check))); err == nil && check {
		eng2, err := openEngine()
		if err != nil {
			return err
		}
		runDoctor(cmd, eng2, engine.DoctorOptions{})
	}
	return nil
}

func posInt(s string) error {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 {
		return errors.New("enter a positive number")
	}
	return nil
}
