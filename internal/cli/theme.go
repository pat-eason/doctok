package cli

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

var (
	pink   = lipgloss.Color("#FF4FD8")
	cyan   = lipgloss.Color("#00E5FF")
	yellow = lipgloss.Color("#FFE135")
	green  = lipgloss.Color("#3DDC97")
	red    = lipgloss.Color("#FF5A5F")
	gray   = lipgloss.Color("#8A8A99")
	blue   = lipgloss.Color("#7AA2FF")

	sTitle  = lipgloss.NewStyle().Bold(true).Foreground(pink)
	sAccent = lipgloss.NewStyle().Foreground(cyan)
	sDim    = lipgloss.NewStyle().Foreground(gray)
	sOK     = lipgloss.NewStyle().Foreground(green)
	sWarn   = lipgloss.NewStyle().Foreground(yellow)
	sErr    = lipgloss.NewStyle().Foreground(red)
	sBold   = lipgloss.NewStyle().Bold(true)
	sBox    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(pink).Padding(0, 2)
	sLabel  = lipgloss.NewStyle().Foreground(gray).Width(10)
)

func logo() string {
	return sTitle.Render("doctok") + sDim.Render(" ✨ docs → TikToks, no cap")
}

// interactive reports whether we can prompt the user.
func interactive() bool {
	return isatty.IsTerminal(os.Stdin.Fd()) && isatty.IsTerminal(os.Stdout.Fd()) && os.Getenv("DOCTOK_NONINTERACTIVE") == ""
}

func formTheme() *huh.Theme { return huh.ThemeCharm() }

func runForm(groups ...*huh.Group) error {
	return huh.NewForm(groups...).WithTheme(formTheme()).Run()
}

func fmtDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

func fmtSecs(sec float64) string { return fmtDuration(time.Duration(sec * float64(time.Second))) }

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}

func statusBadge(status string) string {
	switch status {
	case "succeeded":
		return sOK.Render("✓ succeeded")
	case "running":
		return sAccent.Render("◐ running")
	case "awaiting_approval":
		return sWarn.Render("⏸ awaiting approval")
	case "interrupted":
		return sWarn.Render("⏯ interrupted")
	case "failed":
		return sErr.Render("✗ failed")
	case "canceled":
		return sDim.Render("⊘ canceled")
	default:
		return sDim.Render("· " + status)
	}
}

func homeShort(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		return "~" + strings.TrimPrefix(p, h)
	}
	return p
}
