// Package tui is vakt's control center in the terminal: panels on the left
// drive the content on the right, in the style of lazygit. It asks every
// machine every few seconds and keeps the answers only in memory, so the
// graphs show what happened while you watched, nothing more.
package tui

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/vakt"
)

// Every is how often the machines are asked.
const Every = 5 * time.Second

// Main runs the control center in a terminal, and prints the plain
// inventory table otherwise, or with -text. A fleet program needs one line:
//
//	tui.Main(ctx, tailnet.LocalDaemon(), vakt.Fleet{Hosts: hosts}, args)
func Main(ctx context.Context, tn tailnet.Tailnet, fleet vakt.Fleet, args []string) error {
	ask := func(ctx context.Context) (vakt.Inventory, error) { return vakt.Ask(ctx, tn, fleet) }
	if slices.Contains(args, "-text") || !isTerminal(os.Stdout) {
		inv, err := ask(ctx)
		if err != nil {
			return err
		}
		return inv.WriteText(os.Stdout)
	}
	_, err := tea.NewProgram(New(ask), tea.WithAltScreen(), tea.WithContext(ctx)).Run()
	return err
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Asker asks the fleet once.
type Asker func(ctx context.Context) (vakt.Inventory, error)

// Panels on the left, in order; 1-4 jump to them.
const (
	panelStatus = iota
	panelServices
	panelMachines
	panelChanges
	panels
)

// Model is the control center's state.
type Model struct {
	ask    Asker
	now    func() time.Time
	inv    vakt.Inventory
	err    error
	asked  bool // at least one answer, or error, has come back
	asking bool
	hist   map[string]*history

	focus  int
	sel    int // the selected machine in the Machines panel
	width  int
	height int
}

// New returns a control center that asks with ask.
func New(ask Asker) Model {
	return Model{ask: ask, now: time.Now, hist: map[string]*history{}, focus: panelStatus, width: 100, height: 30}
}

type inventoryMsg struct {
	inv vakt.Inventory
	err error
}

type tickMsg time.Time

func (m Model) Init() tea.Cmd { return tea.Batch(m.askCmd(), tick()) }

func (m Model) askCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), vakt.Timeout+2*time.Second)
		defer cancel()
		inv, err := m.ask(ctx)
		return inventoryMsg{inv, err}
	}
}

// tick redraws once a second, so "asked 3s ago" stays true.
func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case inventoryMsg:
		m.asking, m.asked = false, true
		m.err = msg.err
		if msg.err == nil {
			m.inv = msg.inv
			m.record(msg.inv)
			m.sel = min(m.sel, max(0, len(m.inv.Machines)-1))
		}
	case tickMsg:
		if !m.asking && m.asked && m.now().Sub(m.inv.AskedAt) >= Every {
			m.asking = true
			return m, tea.Batch(m.askCmd(), tick())
		}
		return m, tick()
	case tea.KeyMsg:
		return m.key(msg.String())
	}
	return m, nil
}

func (m Model) key(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "q", "ctrl+c":
		return m, tea.Quit
	case "tab":
		m.focus = (m.focus + 1) % panels
	case "shift+tab":
		m.focus = (m.focus + panels - 1) % panels
	case "1", "2", "3", "4":
		m.focus = int(k[0] - '1')
	case "down", "j":
		m.focus = panelMachines
		m.sel = min(m.sel+1, max(0, len(m.inv.Machines)-1))
	case "up", "k":
		m.focus = panelMachines
		m.sel = max(m.sel-1, 0)
	case "r":
		if !m.asking {
			m.asking = true
			return m, m.askCmd()
		}
	}
	return m, nil
}

// record keeps each machine's CPU and memory, for the graphs.
func (m Model) record(inv vakt.Inventory) {
	for _, mc := range inv.Machines {
		if mc.Report == nil {
			continue
		}
		h := m.hist[mc.Name]
		if h == nil {
			h = &history{}
			m.hist[mc.Name] = h
		}
		h.add(mc.Report.CPUPercent, mc.Report.Memory.UsedPercent)
	}
}

// history is what a machine said while you watched.
type history struct{ cpu, mem []float64 }

const keep = 40 // samples: 40 × 5s is the last 3⅓ minutes

func (h *history) add(cpu, mem float64) {
	h.cpu = append(h.cpu, cpu)
	h.mem = append(h.mem, mem)
	if len(h.cpu) > keep {
		h.cpu, h.mem = h.cpu[1:], h.mem[1:]
	}
}

func ago(d time.Duration) string {
	switch {
	case d < 2*time.Second:
		return "just now"
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	return fmt.Sprintf("%dm ago", int(d.Minutes()))
}
