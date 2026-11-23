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

// Every is how often the machines are asked, and PunchcardEvery how often
// Punchcard: builds and audits change by the hour, not the second.
const (
	Every          = 5 * time.Second
	PunchcardEvery = time.Minute
)

// Main runs the control center in a terminal, and prints the plain
// inventory table otherwise, or with -text. A fleet program needs one line:
//
//	tui.Main(ctx, tailnet.LocalDaemon(), vakt.Fleet{Hosts: hosts}, args)
func Main(ctx context.Context, tn tailnet.Tailnet, fleet vakt.Fleet, args []string) error {
	ask := func(ctx context.Context) (vakt.Inventory, error) { return vakt.Ask(ctx, tn, fleet) }
	m := New(ask)
	if fleet.Punchcard != "" {
		m = m.WithPunchcard(fleet.Punchcard, func(ctx context.Context) (vakt.Punchcard, error) {
			return vakt.AskPunchcard(ctx, tn, fleet.Punchcard)
		})
	}
	if slices.Contains(args, "-text") || !isTerminal(os.Stdout) {
		inv, err := ask(ctx)
		if err != nil {
			return err
		}
		return inv.WriteText(os.Stdout)
	}
	_, err := tea.NewProgram(m, tea.WithAltScreen(), tea.WithContext(ctx)).Run()
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

// Asker asks the fleet once, and PunchcardAsker asks Punchcard once.
type (
	Asker          func(ctx context.Context) (vakt.Inventory, error)
	PunchcardAsker func(ctx context.Context) (vakt.Punchcard, error)
)

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

	// Punchcard, when the fleet has one. pc is its last answer; it's kept
	// when a later ask fails, and pcErr says that one failed.
	askPC    PunchcardAsker
	pcAddr   string
	pc       *vakt.Punchcard
	pcErr    error
	pcAt     time.Time // when Punchcard last answered or failed
	pcAsking bool

	focus  int
	sel    int // the selected machine in the Machines panel
	appSel int // the selected app in the Services panel
	// changeSel is the selected change in the Changes panel. It counts
	// from the newest, so it moves when something new happens: the
	// timeline is about now.
	changeSel int
	width     int
	height    int
}

// New returns a control center that asks with ask.
func New(ask Asker) Model {
	return Model{ask: ask, now: time.Now, hist: map[string]*history{}, focus: panelStatus, width: 100, height: 30}
}

// WithPunchcard has m ask Punchcard at addr too, every PunchcardEvery.
func (m Model) WithPunchcard(addr string, ask PunchcardAsker) Model {
	m.askPC, m.pcAddr = ask, addr
	return m
}

// pcView is what an app's screen needs to know about Punchcard.
type pcView struct {
	on  bool // the fleet has a Punchcard
	sec secFunc
	err error
}

func (m Model) pcView() pcView { return pcView{m.askPC != nil, m.sec(), m.pcErr} }

type punchcardMsg struct {
	pc  vakt.Punchcard
	err error
}

func (m Model) askPCCmd() tea.Cmd {
	if m.askPC == nil {
		return nil
	}
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		pc, err := m.askPC(ctx)
		return punchcardMsg{pc, err}
	}
}

type inventoryMsg struct {
	inv vakt.Inventory
	err error
}

type tickMsg time.Time

func (m Model) Init() tea.Cmd { return tea.Batch(m.askCmd(), m.askPCCmd(), tick()) }

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
			m.appSel = min(m.appSel, max(0, len(m.apps())-1))
			m.changeSel = min(m.changeSel, max(0, len(changesOf(m.inv))-1))
		}
	case punchcardMsg:
		m.pcAsking, m.pcAt, m.pcErr = false, m.now(), msg.err
		if msg.err == nil {
			m.pc = &msg.pc
			m.appSel = min(m.appSel, max(0, len(m.apps())-1))
		}
	case tickMsg:
		cmds := []tea.Cmd{tick()}
		if !m.asking && m.asked && m.now().Sub(m.inv.AskedAt) >= Every {
			m.asking = true
			cmds = append(cmds, m.askCmd())
		}
		if m.askPC != nil && !m.pcAsking && !m.pcAt.IsZero() && m.now().Sub(m.pcAt) >= PunchcardEvery {
			m.pcAsking = true
			cmds = append(cmds, m.askPCCmd())
		}
		return m, tea.Batch(cmds...)
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
		sel, n := m.list()
		*sel = min(*sel+1, max(0, n-1))
	case "up", "k":
		sel, _ := m.list()
		*sel = max(*sel-1, 0)
	case "r":
		var cmds []tea.Cmd
		if !m.asking {
			m.asking = true
			cmds = append(cmds, m.askCmd())
		}
		if m.askPC != nil && !m.pcAsking {
			m.pcAsking = true
			cmds = append(cmds, m.askPCCmd())
		}
		return m, tea.Batch(cmds...)
	}
	return m, nil
}

// list is the selection ↑↓ moves and how many it chooses from. Services
// and Changes have their own; from Status, the arrows go to the machines.
func (m *Model) list() (*int, int) {
	switch m.focus {
	case panelServices:
		return &m.appSel, len(m.apps())
	case panelChanges:
		return &m.changeSel, len(changesOf(m.inv))
	}
	m.focus = panelMachines
	return &m.sel, len(m.inv.Machines)
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
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
	return fmt.Sprintf("%dd ago", int(d.Hours()/24))
}
