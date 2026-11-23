package tui

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/vt100io/smed/watch"
	"github.com/vt100io/smed/watch/machine"
)

// change is one thing that happened to the fleet: a deploy or an apt run.
// Commits and smed's applies join the same timeline later.
type change struct {
	at      time.Time
	machine string
	level   level  // fine, or alarm when it didn't go as asked
	short   string // for the left panel: "deploy vt100", "6 packages"
	title   string // for the timeline
	detail  []string
}

const (
	maxChanges   = 60
	runsTogether = 10 * time.Minute // apt runs this close, by the same command, are one change
	maxPkgLines  = 12
)

// changesOf is the fleet's timeline, newest first.
func changesOf(inv watch.Inventory) []change {
	var out []change
	for _, mc := range inv.Machines {
		if mc.Report == nil {
			continue
		}
		for _, a := range mc.Report.Apps {
			for _, d := range a.Deploys {
				out = append(out, deployChange(mc.Name, a.Name, d))
			}
		}
		out = append(out, packageChanges(mc.Name, mc.Report.Packages)...)
	}
	slices.SortStableFunc(out, func(a, b change) int { return b.at.Compare(a.at) })
	return out[:min(len(out), maxChanges)]
}

func deployChange(mc, app string, d machine.Deploy) change {
	c := change{at: d.Finished, machine: mc, short: "deploy " + app}
	if c.at.IsZero() {
		c.at = d.Started
	}
	c.title = styleText.Render("deploy "+app+" "+d.Version+" ") + resultText(d.Result) + styleMuted.Render(" · ") + ciText(d)
	if d.Result != "live" {
		c.level = alarm
	}
	line := "by " + d.Who + from(d) + " · took " + took(d)
	if d.Previous != "" {
		line += " · before " + d.Previous
	}
	c.detail = append(c.detail, styleMuted.Render(line))
	if d.Error != "" {
		c.detail = append(c.detail, styleBad.Render(d.Error))
	}
	return c
}

// packageChanges groups apt's runs: unattended-upgrade upgrades one package
// set at a time, a dozen runs in a minute, and that is one night's change.
func packageChanges(mc string, runs []machine.PackageRun) []change {
	runs = slices.Clone(runs)
	slices.SortStableFunc(runs, func(a, b machine.PackageRun) int { return a.Started.Compare(b.Started) })
	var groups [][]machine.PackageRun
	for _, r := range runs {
		if n := len(groups); n > 0 {
			last := groups[n-1][len(groups[n-1])-1]
			if r.Command == last.Command && r.RequestedBy == last.RequestedBy && r.Started.Sub(last.Started) < runsTogether {
				groups[n-1] = append(groups[n-1], r)
				continue
			}
		}
		groups = append(groups, []machine.PackageRun{r})
	}
	var out []change
	for _, g := range groups {
		out = append(out, packageChange(mc, g))
	}
	return out
}

func packageChange(mc string, g []machine.PackageRun) change {
	var ins, up, rm []machine.Package
	var errs []string
	for _, r := range g {
		ins, up, rm = append(ins, r.Installed...), append(up, r.Upgraded...), append(rm, r.Removed...)
		if r.Error != "" {
			errs = append(errs, r.Error)
		}
	}
	n := len(ins) + len(up) + len(rm)
	c := change{at: g[0].Started, machine: mc, short: fmt.Sprintf("%d package%s", n, plural(n, "", "s"))}
	var what []string
	for _, p := range []struct {
		n    int
		verb string
	}{{len(up), "upgraded"}, {len(ins), "installed"}, {len(rm), "removed"}} {
		if p.n > 0 {
			what = append(what, fmt.Sprintf("%d %s", p.n, p.verb))
		}
	}
	by := command(g[0].Command)
	if g[0].RequestedBy != "" {
		by += " by " + g[0].RequestedBy
	}
	// Unattended upgrades are the quiet normal and say so in the detail; a
	// person running apt is news and says so in the title.
	c.title = styleText.Render("packages ") + styleMuted.Render(strings.Join(what, " · "))
	var lines []string
	if g[0].Command == "/usr/bin/unattended-upgrade" {
		lines = append(lines, styleMuted.Render("by "+by))
	} else {
		c.title += styleMuted.Render(" · ") + styleText.Render(by)
	}
	if len(errs) > 0 {
		c.level = alarm
		c.title += styleBad.Render(" · failed")
	}
	for _, p := range up {
		lines = append(lines, styleAccent.Render("↑ ")+styleText.Render(p.Name)+styleMuted.Render(" "+p.From+" → "+p.To))
	}
	for _, p := range ins {
		lines = append(lines, styleOK.Render("+ ")+styleText.Render(p.Name)+styleMuted.Render(" "+p.To))
	}
	for _, p := range rm {
		lines = append(lines, styleWarn.Render("− ")+styleText.Render(p.Name)+styleMuted.Render(" "+p.From))
	}
	if len(lines) > maxPkgLines+1 {
		more := len(lines) - maxPkgLines
		lines = append(lines[:maxPkgLines], styleMuted.Render(fmt.Sprintf("and %d more", more)))
	}
	for _, e := range errs {
		lines = append(lines, styleBad.Render(e))
	}
	c.detail = lines
	return c
}

// command shortens apt's command line: "/usr/bin/unattended-upgrade" is
// unattended-upgrade, "apt install jq" stays as it is.
func command(c string) string {
	first, rest, _ := strings.Cut(c, " ")
	if c == "" {
		return "apt"
	}
	return strings.TrimSpace(path.Base(first) + " " + rest)
}

// changeLines is the left panel: the newest changes, one line each.
func (m Model) changeLines(cs []change) []string {
	var out []string
	for i, c := range cs {
		mark, short := " ", c.level.textStyle().Render(c.short)
		if m.focus == panelChanges && i == m.changeSel {
			mark, short = styleAccent.Render("▸"), styleName.Render(c.short)
			if c.level > fine {
				short = c.level.style().Bold(true).Render(c.short)
			}
		}
		out = append(out, mark+styleMuted.Render(fitLeft(ago(m.now().Sub(c.at)), 9))+short)
	}
	if len(out) == 0 {
		out = []string{styleMuted.Render("nothing recorded yet")}
	}
	return out
}

// changesScreen is the timeline by day, newest first, with the selected
// change opened. It scrolls so the selected change is always in view.
func (m Model) changesScreen(cs []change, height int) []string {
	if len(cs) == 0 {
		return []string{styleMuted.Render("No deploys or package runs on the machines that answered.")}
	}
	multi := len(m.inv.Machines) > 1
	var out []string
	selStart, selEnd := 0, 0
	day := ""
	for i, c := range cs {
		at := c.at.Local()
		if d := dayName(at, m.now()); d != day {
			if day != "" {
				out = append(out, "")
			}
			out = append(out, styleHeading.Render(d))
			day = d
		}
		mark := " "
		if i == m.changeSel {
			mark = styleAccent.Render("▸")
			selStart = len(out)
		}
		dot := styleDim.Render("·")
		if c.level > fine {
			dot = c.level.style().Render("✗")
		}
		row := mark + dot + " " + styleMuted.Render(at.Format("15:04")+" ")
		if multi {
			row += styleMuted.Render(fitLeft(c.machine, 10))
		}
		out = append(out, row+c.title)
		if i == m.changeSel {
			for _, d := range c.detail {
				out = append(out, "         "+d)
			}
			selEnd = len(out) - 1
		}
	}
	if selEnd >= height {
		out = out[min(selStart, selEnd-height+1):]
	}
	return out
}

func dayName(t, now time.Time) string {
	now = now.Local()
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	today := time.Date(y2, m2, d2, 0, 0, 0, 0, time.Local)
	switch day := time.Date(y1, m1, d1, 0, 0, 0, 0, time.Local); {
	case day.Equal(today):
		return "TODAY"
	case day.Equal(today.AddDate(0, 0, -1)):
		return "YESTERDAY"
	}
	return strings.ToUpper(t.Format("Mon 02 Jan"))
}
