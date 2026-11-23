package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/vt100io/smed/vakt"
	"github.com/vt100io/smed/vakt/machine"
)

const leftWidth = 30

func (m Model) View() string {
	w, h := max(m.width, 60), max(m.height, 16)
	body := h - 2 // header and footer
	left := m.left(body)
	right := box(m.rightTitle(), m.right(w-leftWidth-4), w-leftWidth, body, false)
	rows := make([]string, 0, h)
	rows = append(rows, m.header(w))
	for i := range body {
		rows = append(rows, left[i]+right[i])
	}
	rows = append(rows, m.footer(w))
	return strings.Join(rows, "\n")
}

func (m Model) header(w int) string {
	s := styleBrand.Render(" vakt")
	switch {
	case !m.asked:
		s += styleMuted.Render(" · asking every machine…")
	case m.err != nil:
		s += styleBad.Render(" · " + m.err.Error())
	default:
		s += styleMuted.Render(fmt.Sprintf(" · asked %s · every %ds", ago(m.now().Sub(m.inv.AskedAt)), int(Every.Seconds())))
		if m.asking {
			s += styleAccent.Render(" · asking…")
		}
	}
	return fit(s, w)
}

func (m Model) footer(w int) string {
	keys := []struct{ k, what string }{
		{"tab/1-4", "panel"}, {"↑↓", "machine"}, {"r", "ask now"}, {"q", "quit"},
	}
	var parts []string
	for _, k := range keys {
		parts = append(parts, styleAccent.Render(k.k)+" "+styleMuted.Render(k.what))
	}
	return fit(" "+strings.Join(parts, "   "), w)
}

// left stacks the four panels; Machines takes the height the others leave.
func (m Model) left(height int) []string {
	attn := attention(m.inv)
	status := []string{styleOK.Render("● all quiet")}
	switch {
	case !m.asked:
		status = []string{styleMuted.Render("○ asking…")}
	case len(attn) > 0:
		status = []string{attn.worst().style().Render(fmt.Sprintf("✗ %d need%s you", len(attn), plural(len(attn), "s", "")))}
	}
	status = append(status, styleMuted.Render(fmt.Sprintf("%d machines · %d answered", len(m.inv.Machines), answered(m.inv))))

	services := []string{styleDim.Render("next: apps from unit's"), styleDim.Render("deploy records")}
	changes := []string{styleDim.Render("next: commits, deploys,"), styleDim.Render("package upgrades")}

	fixed := 3 * 4 // three panels of two lines plus borders
	var machines []string
	for i, mc := range m.inv.Machines {
		line := stateDot(mc) + " " + styleText.Render(mc.Name)
		if mc.Report != nil {
			line = fitLeft(line, 18) + styleMuted.Render("up "+upFor(mc.Report.Uptime))
		}
		if m.focus == panelMachines && i == m.sel {
			line = styleSelected.Render(fit(line, leftWidth-3))
		}
		machines = append(machines, line)
	}
	if len(machines) == 0 && m.asked {
		machines = []string{styleMuted.Render("no tagged machines")}
	}

	var out []string
	out = append(out, box("[1] Status", status, leftWidth, 4, m.focus == panelStatus)...)
	out = append(out, box("[2] Services", services, leftWidth, 4, m.focus == panelServices)...)
	out = append(out, box("[3] Machines", machines, leftWidth, height-fixed, m.focus == panelMachines)...)
	out = append(out, box("[4] Changes", changes, leftWidth, 4, m.focus == panelChanges)...)
	return out
}

func (m Model) rightTitle() string {
	switch m.focus {
	case panelMachines:
		if mc, ok := m.selected(); ok {
			return mc.Name
		}
		return "Machines"
	case panelServices:
		return "Services"
	case panelChanges:
		return "Changes"
	}
	return "Overview"
}

func (m Model) right(w int) []string {
	switch m.focus {
	case panelMachines:
		if mc, ok := m.selected(); ok {
			return m.machine(mc, w)
		}
	case panelServices:
		return []string{
			styleMuted.Render("What unit deployed, per app: the release that runs, whether it is the"),
			styleMuted.Render("binary CI built, its sandbox, and logs and history a key away."),
			"",
			styleDim.Render("Comes when the responder reads /var/log/unit/deploys.jsonl."),
		}
	case panelChanges:
		return []string{
			styleMuted.Render("Has something changed? Commits, deploys and package upgrades on one"),
			styleMuted.Render("timeline, newest first."),
			"",
			styleDim.Render("Comes with deploy records, smed's apply records and GitHub."),
		}
	}
	return m.overview(w)
}

// overview is the first screen: quiet when nothing needs you.
func (m Model) overview(w int) []string {
	if !m.asked {
		return []string{styleMuted.Render("Asking every machine…")}
	}
	var out []string
	attn := attention(m.inv)
	if len(attn) == 0 {
		out = append(out, styleOK.Render("Nothing needs you."))
	} else {
		out = append(out, styleName.Render(fmt.Sprintf("%d thing%s need%s you.", len(attn), plural(len(attn), "", "s"), plural(len(attn), "s", ""))))
		out = append(out, "")
		for _, a := range attn {
			out = append(out, a.level.style().Render(a.mark())+" "+styleName.Render(fitLeft(a.machine, 15))+" "+styleText.Render(a.text))
		}
	}
	out = append(out, "")
	for _, mc := range m.inv.Machines {
		out = append(out, m.overviewRow(mc))
	}
	out = append(out, "", styleDim.Render("cpu and mem: sampled every 5s while you watch · disk: the fullest"))
	return out
}

func (m Model) overviewRow(mc vakt.Machine) string {
	name := styleText.Render(fitLeft(mc.Name, 15))
	if mc.Report == nil {
		return name + stateStyle(mc).Render(string(mc.State))
	}
	r := mc.Report
	h := m.hist[mc.Name]
	cpu, mem := []float64{}, []float64{}
	if h != nil {
		cpu, mem = h.cpu, h.mem
	}
	d := fullestDisk(r)
	return name +
		styleMuted.Render("cpu ") + spark(cpu, 8, cpuLevel(r.CPUPercent)) + pct(r.CPUPercent, cpuLevel(r.CPUPercent)) +
		styleMuted.Render("  mem ") + spark(mem, 8, memLevel(r.Memory.UsedPercent)) + pct(r.Memory.UsedPercent, memLevel(r.Memory.UsedPercent)) +
		styleMuted.Render("  disk ") + pct(d.UsedPercent, diskLevel(d.UsedPercent))
}

// machine is one machine's screen.
func (m Model) machine(mc vakt.Machine, w int) []string {
	var out []string
	tags := strings.Join(mc.Tags, " ")
	decl := "not declared"
	if len(mc.Declared) > 0 {
		decl = "declared by " + strings.Join(mc.Declared, ", ")
	}
	if mc.Report == nil {
		out = append(out,
			styleName.Render(mc.Name)+styleMuted.Render(" · "+tags),
			styleMuted.Render(decl),
			"",
			stateStyle(mc).Render(string(mc.State)),
		)
		if mc.Err != "" {
			out = append(out, styleText.Render(mc.Err))
		}
		if mc.State == vakt.NoResponder {
			out = append(out, "", styleDim.Render("Nothing answers on :"+machine.Port+". Deploy cmd/vakt-responder there with unit."))
		}
		return out
	}
	r := mc.Report
	out = append(out,
		styleName.Render(mc.Name)+styleMuted.Render(fmt.Sprintf(" · %s · %s · %d CPUs · %s · up %s", tags, r.OS, r.CPUs, gib(r.Memory.TotalBytes), upFor(r.Uptime))),
		styleMuted.Render(decl+" · kernel "+r.Kernel),
		"",
	)
	h := m.hist[mc.Name]
	if h == nil {
		h = &history{}
	}
	graph := max(10, min(40, w-40))
	out = append(out,
		styleMuted.Render("cpu  ")+spark(h.cpu, graph, cpuLevel(r.CPUPercent))+pct(r.CPUPercent, cpuLevel(r.CPUPercent))+
			styleMuted.Render(fmt.Sprintf("   load %.2f %.2f %.2f", r.Load[0], r.Load[1], r.Load[2])),
		styleMuted.Render("mem  ")+spark(h.mem, graph, memLevel(r.Memory.UsedPercent))+pct(r.Memory.UsedPercent, memLevel(r.Memory.UsedPercent))+
			styleMuted.Render(fmt.Sprintf("   %s of %s", gib(r.Memory.TotalBytes-r.Memory.AvailableBytes), gib(r.Memory.TotalBytes))),
		"",
		styleHeading.Render("DISKS"),
	)
	for _, d := range r.Disks {
		out = append(out, styleText.Render(fitLeft(d.Mount, 12))+bar(d.UsedPercent, 20, diskLevel(d.UsedPercent))+pct(d.UsedPercent, diskLevel(d.UsedPercent))+
			styleMuted.Render(fmt.Sprintf("   %s free of %s", gib(d.FreeBytes), gib(d.TotalBytes))))
	}
	return out
}

func (m Model) selected() (vakt.Machine, bool) {
	if m.sel < len(m.inv.Machines) {
		return m.inv.Machines[m.sel], true
	}
	return vakt.Machine{}, false
}

// attention is what needs you, worst first. Thresholds live in style.go.
type item struct {
	level   level
	machine string
	text    string
}

type items []item

func (a items) worst() level {
	l := fine
	for _, i := range a {
		l = max(l, i.level)
	}
	return l
}

func (i item) mark() string {
	if i.level == alarm {
		return "✗"
	}
	return "!"
}

func attention(inv vakt.Inventory) items {
	var out items
	for _, mc := range inv.Machines {
		declared := len(mc.Declared) > 0
		switch mc.State {
		case vakt.Missing:
			out = append(out, item{alarm, mc.Name, "declared, but not in the tailnet"})
		case vakt.Offline:
			out = append(out, item{alarm, mc.Name, "offline in the tailnet"})
		case vakt.Silent:
			out = append(out, item{alarm, mc.Name, "silent: no answer in time"})
		case vakt.Failing:
			out = append(out, item{alarm, mc.Name, "answers with an error: " + mc.Err})
		case vakt.NoResponder:
			if declared { // undeclared machines just haven't got one yet
				out = append(out, item{notice, mc.Name, "declared, but no responder on :" + machine.Port})
			}
		}
		if r := mc.Report; r != nil {
			for _, d := range r.Disks {
				if l := diskLevel(d.UsedPercent); l > fine {
					out = append(out, item{l, mc.Name, fmt.Sprintf("disk %s %.0f%% full · %s left", d.Mount, d.UsedPercent, gib(d.FreeBytes))})
				}
			}
			if l := memLevel(r.Memory.UsedPercent); l > fine {
				out = append(out, item{l, mc.Name, fmt.Sprintf("memory %.0f%% used", r.Memory.UsedPercent)})
			}
		}
	}
	// Worst first, otherwise in inventory order.
	var sorted items
	for _, l := range []level{alarm, notice} {
		for _, i := range out {
			if i.level == l {
				sorted = append(sorted, i)
			}
		}
	}
	return sorted
}

// box draws a panel with its title in the top border, like lazygit.
func box(title string, lines []string, width, height int, focus bool) []string {
	bs, ts := styleBorder, styleTitle
	if focus {
		bs, ts = styleBorderOn, styleTitleOn
	}
	inner := width - 2
	top := bs.Render("╭─ ") + ts.Render(title) + bs.Render(" "+strings.Repeat("─", max(0, inner-3-ansi.StringWidth(title)))+"╮")
	out := []string{top}
	for i := range height - 2 {
		line := ""
		if i < len(lines) {
			line = lines[i]
		}
		out = append(out, bs.Render("│")+" "+fit(line, inner-1)+bs.Render("│"))
	}
	return append(out, bs.Render("╰"+strings.Repeat("─", inner)+"╯"))
}

// fit cuts or pads s to exactly w columns, counting what a terminal shows.
func fit(s string, w int) string {
	if ansi.StringWidth(s) > w {
		s = ansi.Truncate(s, w, "…")
	}
	return s + strings.Repeat(" ", max(0, w-ansi.StringWidth(s)))
}

func fitLeft(s string, w int) string { return fit(s, w) }

const ticks = "▁▂▃▄▅▆▇█"

// spark draws the last n samples, oldest left; samples not taken yet are dots.
func spark(vals []float64, n int, l level) string {
	if len(vals) > n {
		vals = vals[len(vals)-n:]
	}
	var b strings.Builder
	for _, v := range vals {
		i := int(v / 100 * float64(len([]rune(ticks))-1))
		i = max(0, min(i, len([]rune(ticks))-1))
		b.WriteRune([]rune(ticks)[i])
	}
	return styleDim.Render(strings.Repeat("·", n-len(vals))) + l.style().Render(b.String())
}

func bar(p float64, n int, l level) string {
	full := int(p/100*float64(n) + 0.5)
	full = max(0, min(full, n))
	return l.style().Render(strings.Repeat("█", full)) + styleDim.Render(strings.Repeat("░", n-full))
}

func pct(p float64, l level) string {
	s := fmt.Sprintf(" %3.0f%%", p)
	if l == fine {
		return styleText.Render(s)
	}
	return l.style().Render(s)
}

func stateDot(mc vakt.Machine) string { return stateStyle(mc).Render("●") }

func stateStyle(mc vakt.Machine) lipgloss.Style {
	switch mc.State {
	case vakt.Answered:
		return styleOK
	case vakt.NoResponder:
		if len(mc.Declared) == 0 {
			return styleDim
		}
		return styleWarn
	}
	return styleBad
}

func fullestDisk(r *machine.Report) machine.Disk {
	var d machine.Disk
	for _, x := range r.Disks {
		if x.UsedPercent >= d.UsedPercent {
			d = x
		}
	}
	return d
}

func answered(inv vakt.Inventory) int {
	n := 0
	for _, m := range inv.Machines {
		if m.State == vakt.Answered {
			n++
		}
	}
	return n
}

func upFor(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dm", int(d.Minutes()))
}

func gib(b uint64) string {
	g := float64(b) / (1 << 30)
	if g >= 10 {
		return fmt.Sprintf("%.0f GiB", g)
	}
	return fmt.Sprintf("%.1f GiB", g)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
