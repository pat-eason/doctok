package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/pat-eason/doctok/engine"
)

type evMsg engine.Event
type finishedMsg struct {
	gen *engine.Generation
	err error
}
type tickMsg time.Time

type progressModel struct {
	title    string
	gen      *engine.Generation
	logs     []string
	spin     spinner.Model
	bar      progress.Model
	cancel   context.CancelFunc
	stopping bool
	done     bool
	err      error
	width    int
	unitUSD  float64
}

func newProgressModel(title string, g *engine.Generation, cancel context.CancelFunc, unitUSD float64) progressModel {
	sp := spinner.New()
	sp.Spinner = spinner.MiniDot
	sp.Style = lipgloss.NewStyle().Foreground(pink)
	bar := progress.New(progress.WithGradient("#FF4FD8", "#00E5FF"), progress.WithWidth(44))
	return progressModel{title: title, gen: g, spin: sp, bar: bar, cancel: cancel, width: 100, unitUSD: unitUSD}
}

func (m progressModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, tick())
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q", "esc":
			if !m.stopping {
				m.stopping = true
				m.logs = append(m.logs, time.Now().Format("15:04:05")+" stopping… Kling tasks keep running server-side; `doctok resume` picks them up")
				m.cancel()
			}
		}
		return m, nil
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.bar.Width = min(60, max(20, msg.Width-30))
		return m, nil
	case evMsg:
		if msg.Gen != nil {
			m.gen = msg.Gen
		}
		if msg.Message != "" && msg.Kind != engine.EventProgress {
			m.logs = append(m.logs, msg.Time.Format("15:04:05")+" "+msg.Message)
			if len(m.logs) > 6 {
				m.logs = m.logs[len(m.logs)-6:]
			}
		}
		return m, nil
	case finishedMsg:
		m.done = true
		m.err = msg.err
		if msg.gen != nil {
			m.gen = msg.gen
		}
		return m, tea.Quit
	case tickMsg:
		return m, tick()
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m progressModel) View() string {
	g := m.gen
	if g == nil {
		return m.spin.View() + " starting…\n"
	}
	var b strings.Builder
	style := engine.GenerationStyle(g)
	b.WriteString(sTitle.Render("doctok ✨ ") + sBold.Render(g.Name) + sDim.Render("  ("+g.Key+")") + "\n")
	fmt.Fprintf(&b, "%s%s %s\n", sLabel.Render("style"), style.Emoji, style.Name)
	src := g.Source.Ref
	if g.Source.Kind == engine.SourceFile {
		src = homeShort(src)
	}
	via := g.Source.Kind
	if g.Source.Via != "" {
		via = g.Source.Via
	}
	fmt.Fprintf(&b, "%s%s %s\n", sLabel.Render("source"), truncate(src, 70), sDim.Render("("+via+")"))
	started := "—"
	if g.StartedAt != nil {
		started = g.StartedAt.Local().Format("Jan 2 15:04:05")
	}
	fmt.Fprintf(&b, "%s%s %s %s\n\n", sLabel.Render("started"), started, sDim.Render("· elapsed"), sAccent.Render(fmtDuration(g.Elapsed(time.Now()))))

	for _, st := range engine.Stages {
		label := engine.StageLabels[st]
		timing := g.StageTimes[st]
		var icon, dur string
		switch {
		case timing != nil && timing.FinishedAt != nil:
			icon = sOK.Render("✓")
			dur = sDim.Render(fmtSecs(timing.Seconds))
		case g.Stage == st && (g.Status == engine.StatusRunning) && !m.done:
			icon = m.spin.View()
			if timing != nil && timing.StartedAt != nil {
				dur = sAccent.Render(fmtDuration(time.Since(*timing.StartedAt)))
			}
			label = sBold.Render(label)
		case g.Stage == st && g.Status == engine.StatusFailed:
			icon = sErr.Render("✗")
		default:
			icon = sDim.Render("·")
			label = sDim.Render(label)
		}
		fmt.Fprintf(&b, "  %s %-36s %s\n", icon, label, dur)
	}
	b.WriteString("\n  " + m.bar.ViewAs(g.Progress) + "\n")

	if len(g.Kling.Clips) > 0 {
		b.WriteString("  " + clipGrid(g.Kling.Clips) + "\n")
	}
	b.WriteString("  " + costLine(g, m.unitUSD) + "\n")
	if len(m.logs) > 0 {
		b.WriteString("\n")
		width := m.width
		if width <= 0 {
			width = 100
		}
		for _, l := range m.logs {
			l = truncate(l, max(40, width-4))
			stamp, rest, _ := strings.Cut(l, " ")
			b.WriteString("  " + sDim.Render(stamp) + " " + rest + "\n")
		}
	}
	if !m.done {
		b.WriteString(sDim.Render("\n  ctrl+c to stop (resumable)") + "\n")
	}
	return b.String()
}

func clipGrid(clips []*engine.Clip) string {
	var cells strings.Builder
	var done, active, queued, failed int
	for _, c := range clips {
		switch c.Status {
		case engine.ClipDownloaded:
			cells.WriteString(sOK.Render("▮"))
			done++
		case engine.ClipSucceeded, engine.ClipProcessing:
			cells.WriteString(sWarn.Render("▮"))
			active++
		case engine.ClipSubmitted:
			cells.WriteString(lipgloss.NewStyle().Foreground(blue).Render("▮"))
			active++
		case engine.ClipFailed:
			cells.WriteString(sErr.Render("▮"))
			failed++
		default:
			cells.WriteString(sDim.Render("▯"))
			queued++
		}
	}
	s := fmt.Sprintf("%s %s %d/%d done", sLabel.Render("clips"), cells.String(), done, len(clips))
	if active > 0 {
		s += sWarn.Render(fmt.Sprintf(" · %d rendering", active))
	}
	if queued > 0 {
		s += sDim.Render(fmt.Sprintf(" · %d queued", queued))
	}
	if failed > 0 {
		s += sErr.Render(fmt.Sprintf(" · %d failed", failed))
	}
	return s
}

func costLine(g *engine.Generation, unitUSD float64) string {
	billing := g.Claude.Billing
	if billing == "" {
		billing = "—"
	}
	claudeStr := fmt.Sprintf("Claude $%.4f %s", g.Claude.CostUSD, sDim.Render("("+billing+")"))
	var klingStr string
	if g.Options.DryRun {
		klingStr = sDim.Render("Kling 0 units (dry run)")
	} else {
		tag := "est."
		if g.Kling.UnitsReported {
			tag = "reported"
		}
		klingStr = fmt.Sprintf("Kling %.1f units ≈ $%.2f %s", g.Kling.Units, g.Kling.Units*unitUSD, sDim.Render(fmt.Sprintf("(%s; budget ~%.0f)", tag, g.Kling.EstimatedUnits)))
	}
	return sLabel.Render("cost") + claudeStr + sDim.Render(" · ") + klingStr
}

// runWithProgress runs fn with a live TUI (or plain log lines when not a TTY).
func runWithProgress(ctx context.Context, title string, g *engine.Generation, unitUSD float64, plain bool,
	fn func(ctx context.Context, obs engine.Observer) (*engine.Generation, error)) (*engine.Generation, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if plain || !interactive() {
		obs := func(ev engine.Event) {
			if ev.Message == "" || ev.Kind == engine.EventProgress {
				return
			}
			pct := ""
			if ev.Gen != nil {
				pct = fmt.Sprintf("%3.0f%% ", ev.Gen.Progress*100)
			}
			fmt.Printf("%s %s%s\n", ev.Time.Format("15:04:05"), pct, ev.Message)
		}
		return fn(ctx, obs)
	}
	m := newProgressModel(title, g, cancel, unitUSD)
	p := tea.NewProgram(m)
	go func() {
		gen, err := fn(ctx, func(ev engine.Event) { p.Send(evMsg(ev)) })
		p.Send(finishedMsg{gen: gen, err: err})
	}()
	final, err := p.Run()
	if err != nil {
		cancel()
		return nil, err
	}
	fm := final.(progressModel)
	if !fm.done {
		// Program exited without the engine finishing (shouldn't happen).
		cancel()
		return fm.gen, context.Canceled
	}
	if fm.err != nil {
		return fm.gen, fm.err
	}
	return fm.gen, nil
}
