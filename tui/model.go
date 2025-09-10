package tui

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/yansol0/aperture/runner"
)

type ModelInit struct {
	SpecPath   string
	ConfigPath string
	BaseURL    string
	Events     <-chan runner.Event
}

type UI struct {
	mdl      model
	program  *tea.Program
	results  []runner.ResultLog
	runErr   error
	doneOnce sync.Once
}

func NewModel(init ModelInit) *UI {
	return &UI{mdl: newModel(init)}
}

func (u *UI) Run() error {
	p := tea.NewProgram(u.mdl, tea.WithoutSignalHandler())
	u.program = p
	m, err := p.Run()
	if mm, ok := m.(model); ok {
		u.mdl = mm
	}
	u.runErr = err
	return err
}

func (u *UI) Done(results []runner.ResultLog, err error) {
	u.doneOnce.Do(func() {
		u.results = results
		if u.program != nil {
			u.program.Send(doneMsg{results: results, err: err})
		}
	})
}

func (u *UI) Results() []runner.ResultLog {
	return u.results
}

type model struct {
	init ModelInit

	spin      spinner.Model
	prog      progress.Model
	percent   float64
	completed int
	total     int

	pathsCount      int
	currentMethod   string
	currentEndpoint string
	lastBodyJSON    string

	width    int
	height   int
	quitting bool

	err error
}

type evMsg struct{ ev runner.Event }

type eventsClosedMsg struct{}

type doneMsg struct {
	results []runner.ResultLog
	err     error
}

func newModel(init ModelInit) model {
	sp := spinner.New()
	sp.Spinner = spinner.Dot
	sp.Style = lipgloss.NewStyle().Foreground(lipgloss.Color("205"))
	pg := progress.New(progress.WithDefaultGradient())
	return model{
		init: init,
		spin: sp,
		prog: pg,
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spin.Tick,
		waitForEvent(m.init.Events),
	)
}

func waitForEvent(ch <-chan runner.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return eventsClosedMsg{}
		}
		return evMsg{ev: ev}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.prog.Width = max(20, (m.width-10)/2)
		return m, nil
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC, tea.KeyEsc:
			m.quitting = true
			return m, tea.Quit
		}
		return m, nil
	case evMsg:
		e := msg.ev
		switch e.Kind {
		case runner.EventPathsDiscovered:
			m.pathsCount = e.PathsCount
		case runner.EventTotalRequests:
			m.total = e.Total
			m.percent = percent(m.completed, m.total)
			return m, tea.Batch(m.prog.SetPercent(m.percent), waitForEvent(m.init.Events))
		case runner.EventEndpointStarting:
			m.currentEndpoint = e.Endpoint
			m.currentMethod = e.Method
		case runner.EventRequestPrepared:
			m.currentEndpoint = e.Endpoint
			m.currentMethod = e.Method
			m.completed = e.Completed
			m.total = e.Total
			m.percent = percent(m.completed, m.total)
			m.lastBodyJSON = marshalPretty(e.Request.Body)
			return m, tea.Batch(m.prog.SetPercent(m.percent), waitForEvent(m.init.Events))
		case runner.EventRequestCompleted:
			m.completed = e.Completed
			m.total = e.Total
			m.percent = percent(m.completed, m.total)
			return m, tea.Batch(m.prog.SetPercent(m.percent), waitForEvent(m.init.Events))
		}
		return m, waitForEvent(m.init.Events)
	case eventsClosedMsg:
		// Wait for doneMsg from controller
		return m, nil
	case doneMsg:
		m.err = msg.err
		m.quitting = true
		return m, tea.Quit
	default:
		return m, nil
	}
}

func (m model) View() string {
	if m.quitting {
		return ""
	}
	bannerString := `
 █████╗ ██████╗ ███████╗██████╗ ████████╗██╗   ██╗██████╗ ███████╗
██╔══██╗██╔══██╗██╔════╝██╔══██╗╚══██╔══╝██║   ██║██╔══██╗██╔════╝
███████║██████╔╝█████╗  ██████╔╝   ██║   ██║   ██║██████╔╝█████╗  
██╔══██║██╔═══╝ ██╔══╝  ██╔══██╗   ██║   ██║   ██║██╔══██╗██╔══╝  
██║  ██║██║     ███████╗██║  ██║   ██║   ╚██████╔╝██║  ██║███████╗
╚═╝  ╚═╝╚═╝     ╚══════╝╚═╝  ╚═╝   ╚═╝    ╚═════╝ ╚═╝  ╚═╝╚══════╝
	`
	banner := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("69")).Render(bannerString)
	meta := lipgloss.NewStyle().Faint(true).Render(fmt.Sprintf("Spec: %s  |  Config: %s  |  Base: %s", m.init.SpecPath, m.init.ConfigPath, m.init.BaseURL))
	paths := fmt.Sprintf("Parsed endpoints: %d", m.pathsCount)
	title := lipgloss.NewStyle().Bold(true).Render("Testing endpoints ") + m.spin.View()
	current := fmt.Sprintf("%s %s", m.currentMethod, m.currentEndpoint)
	bodyTitle := lipgloss.NewStyle().Faint(true).Render("Current request body:")
	body := m.lastBodyJSON
	if body == "" {
		body = "(none)"
	}
	progressLine := fmt.Sprintf("%d/%d", m.completed, m.total)
	return lipgloss.JoinVertical(lipgloss.Left,
		banner,
		meta,
		paths,
		"",
		title,
		current,
		"",
		bodyTitle,
		body,
		"",
		m.prog.ViewAs(m.percent),
		progressLine,
	)
}

func marshalPretty(v any) string {
	if v == nil {
		return "(none)"
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("(unserializable body: %v)", err)
	}
	return string(b)
}

func percent(completed, total int) float64 {
	if total <= 0 {
		return 0
	}
	p := float64(completed) / float64(total)
	if p < 0 {
		p = 0
	}
	if p > 1 {
		p = 1
	}
	return p
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
