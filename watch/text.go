package watch

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/vt100io/smed/watch/machine"
)

// WriteText prints the inventory as a plain table: the first view, before
// the TUI exists, and what a script or a nightly report can read.
func (inv Inventory) WriteText(w io.Writer) error {
	answered, declared := 0, 0
	for _, m := range inv.Machines {
		if m.State == Answered {
			answered++
		}
		if len(m.Declared) > 0 {
			declared++
		}
	}
	head := fmt.Sprintf("smed watch · asked %s · %d of %d answered", inv.AskedAt.Format("15:04:05"), answered, len(inv.Machines))
	if declared > 0 {
		head += fmt.Sprintf(" · %d declared", declared)
	}
	if inv.Personal > 0 {
		head += fmt.Sprintf(" · %d personal %s not asked", inv.Personal, plural(inv.Personal, "device", "devices"))
	}
	if _, err := fmt.Fprintf(w, "%s\n\n", head); err != nil {
		return err
	}
	if len(inv.Machines) == 0 {
		_, err := fmt.Fprintln(w, "No tagged or declared machines in this tailnet.")
		return err
	}

	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "MACHINE\tDECLARED\tSTATE\tLOAD\tCPU\tMEM\tDISK\tUP\tOS")
	for _, m := range inv.Machines {
		decl := "–"
		if len(m.Declared) > 0 {
			decl = strings.Join(m.Declared, ", ")
		}
		if m.Report == nil {
			// Every row has every cell: tabwriter ends a column at a row
			// with fewer. The reason goes last, where the OS would be.
			fmt.Fprintf(tw, "%s\t%s\t%s\t\t\t\t\t\t%s\n", m.Name, decl, m.State, m.Err)
			continue
		}
		r := m.Report
		fmt.Fprintf(tw, "%s\t%s\t%s\t%.2f\t%.0f%%\t%.0f%%\t%s\t%s\t%s\n",
			m.Name, decl, m.State, r.Load[0], r.CPUPercent, r.Memory.UsedPercent,
			fullest(r), uptime(r.Uptime), r.OS)
	}
	return tw.Flush()
}

// fullest is the disk closest to full, the one worth a glance.
func fullest(r *machine.Report) string {
	if len(r.Disks) == 0 {
		return "–"
	}
	d := r.Disks[0]
	for _, x := range r.Disks[1:] {
		if x.UsedPercent > d.UsedPercent {
			d = x
		}
	}
	return fmt.Sprintf("%s %.0f%%", d.Mount, d.UsedPercent)
}

func uptime(seconds float64) string {
	d := time.Duration(seconds) * time.Second
	switch {
	case d >= 48*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
