package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/vt100io/smed/vakt"
	"github.com/vt100io/smed/vakt/machine"
)

// appRef is an app and the machine it runs on.
type appRef struct {
	machine string
	app     machine.App
}

// apps lists every app on every machine that answered, by name.
func (m Model) apps() []appRef {
	return appsOf(m.inv)
}

func appsOf(inv vakt.Inventory) []appRef {
	var out []appRef
	for _, mc := range inv.Machines {
		if mc.Report == nil {
			continue
		}
		for _, a := range mc.Report.Apps {
			out = append(out, appRef{mc.Name, a})
		}
	}
	slices.SortStableFunc(out, func(a, b appRef) int {
		return strings.Compare(a.app.Name+"\x00"+a.machine, b.app.Name+"\x00"+b.machine)
	})
	return out
}

func (m Model) selectedApp() (appRef, bool) {
	apps := m.apps()
	if m.appSel < len(apps) {
		return apps[m.appSel], true
	}
	return appRef{}, false
}

// appLevel is how much an app asks of you, the worst of how it runs and
// what's known about its vulnerabilities.
func appLevel(a machine.App, s *vakt.Security, now time.Time) (level, string) {
	l, why := runLevel(a)
	if sl, swhy := securityLevel(a, s, now); sl > l {
		return sl, swhy
	}
	return l, why
}

// runLevel is how much running an app asks of you: a deploy that didn't
// go live is an alarm, an app that doesn't run or is near its memory limit
// a notice.
func runLevel(a machine.App) (level, string) {
	if len(a.Deploys) > 0 {
		switch d := a.Deploys[0]; d.Result {
		case "live":
		case "rolled-back":
			return alarm, fmt.Sprintf("app %s: %s rolled back to %s", a.Name, d.Version, d.Previous)
		default:
			return alarm, fmt.Sprintf("app %s: %s %s", a.Name, d.Version, d.Result)
		}
	}
	if !a.Running {
		return notice, fmt.Sprintf("app %s is not running", a.Name)
	}
	if a.MemoryMaxBytes > 0 && float64(a.MemoryBytes) >= 0.9*float64(a.MemoryMaxBytes) {
		return notice, fmt.Sprintf("app %s uses %s of its %s", a.Name, size(a.MemoryBytes), size(a.MemoryMaxBytes))
	}
	return fine, ""
}

func appMark(a machine.App, s *vakt.Security, now time.Time) string {
	switch l, _ := appLevel(a, s, now); l {
	case alarm:
		return styleBad.Render("✗")
	case notice:
		return styleWarn.Render("!")
	}
	return styleOK.Render("✓")
}

// appScreen is one app: what runs, how it got there, what it uses, and,
// with Punchcard, what's known to be vulnerable in it.
func appScreen(r appRef, pc pcView, now time.Time) []string {
	a := r.app
	state := styleOK.Render("running")
	if !a.Running {
		state = styleWarn.Render("not running")
	}
	release := a.Release
	if release == "" {
		release = "unknown"
	}
	out := []string{
		styleName.Render(a.Name) + styleMuted.Render(" · on "+r.machine+" · ") + state + styleMuted.Render(" · release ") + styleText.Render(release),
	}
	var where []string
	if len(a.Domains) > 0 {
		where = append(where, strings.Join(a.Domains, ", "))
	}
	if a.Port > 0 {
		where = append(where, fmt.Sprintf(":%d", a.Port))
	}
	mem := "memory " + size(a.MemoryBytes)
	if a.MemoryMaxBytes > 0 {
		mem += " of " + size(a.MemoryMaxBytes)
	}
	where = append(where, mem, fmt.Sprintf("%d tasks", a.Tasks))
	out = append(out, styleMuted.Render(strings.Join(where, " · ")))

	s := pc.sec.of(a)
	if l, why := appLevel(a, s, now); l > fine {
		out = append(out, "", l.style().Render(why))
	}

	out = append(out, "", styleHeading.Render("LATEST DEPLOY"))
	if len(a.Deploys) == 0 {
		out = append(out, styleMuted.Render("No deploy records. Deployed before unit kept them?"))
	} else {
		d := a.Deploys[0]
		line := styleText.Render(d.Version) + " " + resultText(d.Result) + styleMuted.Render(" · ") + ciText(d) + styleMuted.Render(" · ") + binaryText(d)
		if d.Previous != "" {
			line += styleMuted.Render(" · before " + d.Previous)
		}
		out = append(out, line,
			styleMuted.Render(fmt.Sprintf("%s by %s%s · took %s", when(d.Finished), d.Who, from(d), took(d))))
		if d.Error != "" {
			out = append(out, styleBad.Render(d.Error))
		}
	}

	if pc.on {
		out = append(out, securityLines(a, s, pc.err, now)...)
	}

	if len(a.Deploys) > 1 {
		out = append(out, "", styleHeading.Render("HISTORY"))
		for _, d := range a.Deploys {
			out = append(out, styleMuted.Render(fitLeft(when(d.Finished), 14))+
				styleText.Render(fitLeft(d.Version, 9))+resultText(fitLeft(d.Result, 12))+
				ciText(d)+styleMuted.Render("  "+d.Who+from(d)))
		}
	}

	out = append(out, "", styleHeading.Render("IN ITS REPO"),
		styleAccent.Render("unit logs")+styleMuted.Render("      stream the journal"),
		styleAccent.Render("unit history")+styleMuted.Render("   every deploy, from the server"),
		styleAccent.Render("unit audit")+styleMuted.Render("     systemd-analyze security"),
	)
	return out
}

// appLines is the APPS section of a machine's screen.
func appLines(r *machine.Report, sec secFunc, now time.Time) []string {
	if len(r.Apps) == 0 {
		return nil
	}
	out := []string{"", styleHeading.Render("APPS") + styleMuted.Render(" from unit")}
	for _, a := range r.Apps {
		line := appMark(a, sec.of(a), now) + " " + styleText.Render(fitLeft(a.Name, 12)) + styleMuted.Render(fitLeft(a.Release, 9))
		if len(a.Deploys) > 0 {
			d := a.Deploys[0]
			line += resultText(d.Result) + styleMuted.Render(" · "+ago(time.Since(d.Finished))+" · ") + binaryText(d)
		}
		out = append(out, line)
	}
	return out
}

func resultText(r string) string {
	switch strings.TrimSpace(r) {
	case "live":
		return styleOK.Render(r)
	case "rolled-back", "failed":
		return styleBad.Render(r)
	}
	return styleText.Render(r)
}

func ciText(d machine.Deploy) string {
	switch d.CI {
	case "success":
		return styleOK.Render("CI ✓")
	case "":
		return styleMuted.Render("no CI gate")
	case "skipped":
		return styleWarn.Render("CI skipped")
	}
	return styleBad.Render("CI " + d.CI)
}

func binaryText(d machine.Deploy) string {
	if d.SameAsCI {
		return styleOK.Render("same as CI")
	}
	return styleMuted.Render("binary not compared")
}

func from(d machine.Deploy) string {
	if d.From == "" {
		return ""
	}
	return " from " + d.From
}

func when(t time.Time) string {
	if t.IsZero() {
		return "–"
	}
	return t.Local().Format("02 Jan 15:04")
}

func took(d machine.Deploy) string {
	if d.Started.IsZero() || d.Finished.Before(d.Started) {
		return "–"
	}
	return d.Finished.Sub(d.Started).Round(time.Second).String()
}

func size(b uint64) string {
	if b >= 1<<30 {
		return gib(b)
	}
	return fmt.Sprintf("%d MiB", b>>20)
}
