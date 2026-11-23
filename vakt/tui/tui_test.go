package tui

import (
	"context"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/vt100io/smed/vakt"
	"github.com/vt100io/smed/vakt/machine"
)

var t0 = time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

func healthy(name string, cpu, mem, disk float64) vakt.Machine {
	return vakt.Machine{Name: name, Tags: []string{"tag:" + name}, Declared: []string{`Host("` + name + `")`}, State: vakt.Answered,
		Report: &machine.Report{Version: 1, OS: "Debian GNU/Linux 13 (trixie)", Kernel: "6.12", CPUs: 4, Uptime: 8 * 86400,
			CPUPercent: cpu, Load: [3]float64{0.04, 0.02, 0},
			Memory: machine.Memory{TotalBytes: 8 << 30, AvailableBytes: 7 << 30, UsedPercent: mem},
			Disks:  []machine.Disk{{Mount: "/", FS: "ext4", TotalBytes: 80 << 30, FreeBytes: 75 << 30, UsedPercent: disk}}}}
}

func model(inv vakt.Inventory) Model {
	m := New(func(context.Context) (vakt.Inventory, error) { return inv, nil })
	m.now = func() time.Time { return t0.Add(3 * time.Second) }
	next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: 30})
	next, _ = next.Update(inventoryMsg{inv: inv})
	return next.(Model)
}

func lines(v string) []string { return strings.Split(v, "\n") }

// The view fills the window exactly: no line wider, no line more.
func checkFits(t *testing.T, m Model) {
	t.Helper()
	ls := lines(m.View())
	if len(ls) != m.height {
		t.Errorf("%d lines, window is %d", len(ls), m.height)
	}
	for i, l := range ls {
		if w := ansi.StringWidth(l); w != m.width {
			t.Errorf("line %d is %d wide, window is %d: %q", i, w, m.width, ansi.Strip(l))
		}
	}
}

func TestQuietWhenNothingNeedsYou(t *testing.T) {
	m := model(vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{healthy("vt100", 2, 7, 6)}})
	checkFits(t, m)
	v := ansi.Strip(m.View())
	for _, s := range []string{"Nothing needs you.", "● all quiet", "asked 3s ago", "vt100", "1 machine · 1 answered"} {
		if !strings.Contains(v, s) {
			t.Errorf("view lacks %q:\n%s", s, v)
		}
	}
}

func TestSaysWhatNeedsYou(t *testing.T) {
	inv := vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{
		healthy("vt100", 2, 7, 6),
		healthy("dwh", 30, 60, 94),
		{Name: "gone", Declared: []string{`Host("gone")`}, State: vakt.Missing},
		{Name: "web-3", Tags: []string{"tag:web"}, State: vakt.NoResponder}, // undeclared: not a problem yet
	}}
	m := model(inv)
	checkFits(t, m)
	v := ansi.Strip(m.View())
	for _, s := range []string{"2 things need you.", "✗ 2 need you", "disk / 94% full", "declared, but not in the tailnet"} {
		if !strings.Contains(v, s) {
			t.Errorf("view lacks %q:\n%s", s, v)
		}
	}
	if strings.Contains(v, "web-3          declared") {
		t.Error("an undeclared machine without a responder is listed as needing you")
	}
	// Worst first.
	a := attention(inv)
	if len(a) != 2 || a[0].level != alarm {
		t.Errorf("attention %+v", a)
	}
}

func TestMachineScreen(t *testing.T) {
	m := model(vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{healthy("a", 1, 1, 1), healthy("vt100", 2, 7, 6)}})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.focus != panelMachines || m.sel != 1 {
		t.Fatalf("focus %d sel %d", m.focus, m.sel)
	}
	checkFits(t, m)
	v := ansi.Strip(m.View())
	for _, s := range []string{"─ vt100 ─", "▸● vt100", `declared by Host("vt100")`, "load 0.04", "DISKS", "75 GiB free of 80 GiB"} {
		if !strings.Contains(v, s) {
			t.Errorf("machine screen lacks %q:\n%s", s, v)
		}
	}
	// Selection stops at the last machine.
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	if next.(Model).sel != 1 {
		t.Error("selection ran past the last machine")
	}
}

// Graphs show what happened while you watched: one sample per answer.
func TestHistoryGrows(t *testing.T) {
	m := model(vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{healthy("vt100", 2, 7, 6)}})
	for i := range keep + 5 {
		next, _ := m.Update(inventoryMsg{inv: vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{healthy("vt100", float64(i), 7, 6)}}})
		m = next.(Model)
	}
	if h := m.hist["vt100"]; len(h.cpu) != keep || h.cpu[keep-1] != keep+4 {
		t.Errorf("history %d samples, last %v", len(h.cpu), h.cpu[len(h.cpu)-1])
	}
}

func TestSmallAndEmptyWindows(t *testing.T) {
	m := New(func(context.Context) (vakt.Inventory, error) { return vakt.Inventory{}, nil })
	m.now = func() time.Time { return t0 }
	next, _ := m.Update(tea.WindowSizeMsg{Width: 60, Height: 16})
	checkFits(t, next.(Model)) // before the first answer
	if v := ansi.Strip(next.(Model).View()); !strings.Contains(v, "asking") {
		t.Errorf("first view doesn't say it's asking:\n%s", v)
	}
}

func TestSpark(t *testing.T) {
	if got := ansi.Strip(spark([]float64{0, 50, 100}, 5, fine)); got != "··▁▄█" {
		t.Errorf("spark %q", got)
	}
}

func withApps(mc vakt.Machine, apps ...machine.App) vakt.Machine {
	mc.Report.Apps = apps
	return mc
}

func app(name string, running bool, result string) machine.App {
	at := t0.Add(-2 * time.Hour)
	return machine.App{Name: name, Release: "9463b1f", Running: running, MemoryBytes: 12 << 20, MemoryMaxBytes: 128 << 20, Tasks: 7,
		Port: 8080, Domains: []string{name + ".io"},
		Deploys: []machine.Deploy{
			{Version: "9463b1f", Previous: "4e30d97", CI: "success", SameAsCI: true, Who: "stianh@github", From: "elcap", Started: at.Add(-3 * time.Second), Finished: at, Result: result},
			{Version: "4e30d97", CI: "skipped", Who: "stianh@github", Started: at.Add(-48 * time.Hour), Finished: at.Add(-48 * time.Hour), Result: "live"},
		}}
}

func TestServices(t *testing.T) {
	inv := vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{
		withApps(healthy("vt100", 2, 7, 6), app("vt100", true, "live"), app("punchcard", true, "live")),
	}}
	m := model(inv)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Nothing needs you.") {
		t.Errorf("healthy apps need you:\n%s", v)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	next, _ = next.Update(tea.KeyMsg{Type: tea.KeyDown}) // punchcard, then vt100: by name
	m = next.(Model)
	if m.focus != panelServices || m.appSel != 1 {
		t.Fatalf("focus %d appSel %d", m.focus, m.appSel)
	}
	checkFits(t, m)
	v := ansi.Strip(m.View())
	for _, s := range []string{
		"─ vt100 on vt100 ─", "▸✓ vt100", "release 9463b1f", "vt100.io · :8080 · memory 12 MiB of 128 MiB · 7 tasks",
		"LATEST DEPLOY", "9463b1f live · CI ✓ · same as CI · before 4e30d97", "by stianh@github from elcap · took 3s",
		"HISTORY", "CI skipped", "unit logs",
	} {
		if !strings.Contains(v, s) {
			t.Errorf("app screen lacks %q:\n%s", s, v)
		}
	}
}

func TestAppsThatNeedYou(t *testing.T) {
	inv := vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{
		withApps(healthy("vt100", 2, 7, 6), app("vt100", true, "rolled-back"), app("splash", false, "live")),
	}}
	a := attention(inv)
	if len(a) != 2 || a[0].level != alarm || a[1].level != notice {
		t.Fatalf("attention %+v", a)
	}
	v := ansi.Strip(model(inv).View())
	for _, s := range []string{"app vt100: 9463b1f rolled back to 4e30d97", "app splash is not running"} {
		if !strings.Contains(v, s) {
			t.Errorf("overview lacks %q:\n%s", s, v)
		}
	}
	// The machine screen lists its apps too.
	m := model(inv)
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("3")})
	if v := ansi.Strip(next.(Model).View()); !strings.Contains(v, "APPS from unit") || !strings.Contains(v, "rolled-back") {
		t.Errorf("machine screen lacks its apps:\n%s", v)
	}
}

func TestAgo(t *testing.T) {
	for d, want := range map[time.Duration]string{
		time.Second: "just now", 21 * time.Second: "21s ago", 5 * time.Minute: "5m ago",
		5807 * time.Minute: "4d ago", 3 * time.Hour: "3h ago", 47 * time.Hour: "47h ago",
	} {
		if got := ago(d); got != want {
			t.Errorf("ago(%v) = %q, want %q", d, got, want)
		}
	}
}

func withPackages(mc vakt.Machine, reboot bool, runs ...machine.PackageRun) vakt.Machine {
	mc.Report.Packages = runs
	if reboot {
		mc.Report.Reboot = machine.Reboot{Required: true, Packages: []string{"linux-image-6.12.118"}}
	}
	return mc
}

// Night upgrades come as a dozen apt runs a minute apart: one change.
func nightly(at time.Time) []machine.PackageRun {
	u := "/usr/bin/unattended-upgrade"
	return []machine.PackageRun{ // newest first, as the responder sends them
		{Started: at.Add(11 * time.Second), Command: u, Removed: []machine.Package{{Name: "linux-image-6.12.117", From: "6.12.117-1"}}},
		{Started: at.Add(time.Second), Command: u,
			Installed: []machine.Package{{Name: "linux-image-6.12.118", To: "6.12.118-1"}},
			Upgraded:  []machine.Package{{Name: "linux-image-cloud-amd64", From: "6.12.117-1", To: "6.12.118-1"}}},
		{Started: at, Command: u, Upgraded: []machine.Package{{Name: "libk5crypto3", From: "1.21.3-5+deb13u1", To: "1.21.3-5+deb13u2"}}},
	}
}

func TestChanges(t *testing.T) {
	jq := machine.PackageRun{Started: t0.Add(-3 * 24 * time.Hour), Command: "apt install jq", RequestedBy: "stianh",
		Installed: []machine.Package{{Name: "jq", To: "1.7.1-6"}}}
	inv := vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{
		withPackages(withApps(healthy("vt100", 2, 7, 6), app("vt100", true, "live")), false, append(nightly(t0.Add(-3*time.Hour)), jq)...),
	}}
	cs := changesOf(inv)
	// The nightly runs, the deploy 2h ago and the one 2 days before, jq.
	if len(cs) != 4 {
		for _, c := range cs {
			t.Log(c.at, c.short)
		}
		t.Fatalf("%d changes", len(cs))
	}
	if cs[0].short != "deploy vt100" || cs[1].short != "4 packages" || cs[3].short != "1 package" {
		t.Errorf("order: %q %q %q", cs[0].short, cs[1].short, cs[3].short)
	}

	m := model(inv)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "Nothing needs you.") || !strings.Contains(v, "3h ago   4 packages") {
		t.Errorf("changes ask for attention, or aren't listed:\n%s", v)
	}
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})
	next, _ = next.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(Model)
	if m.focus != panelChanges || m.changeSel != 1 {
		t.Fatalf("focus %d changeSel %d", m.focus, m.changeSel)
	}
	checkFits(t, m)
	v := ansi.Strip(m.View())
	for _, s := range []string{
		"TODAY", "deploy vt100 9463b1f live · CI ✓",
		"packages 2 upgraded · 1 installed · 1 removed", "by unattended-upgrade",
		"↑ linux-image-cloud-amd64 6.12.117-1 → 6.12.118-1", "+ linux-image-6.12.118 6.12.118-1", "− linux-image-6.12.117 6.12.117-1",
		"packages 1 installed · apt install jq by stianh",
	} {
		if !strings.Contains(v, s) {
			t.Errorf("timeline lacks %q:\n%s", s, v)
		}
	}
	if strings.Contains(v, "by stianh@github from elcap") {
		t.Error("an unselected change is opened")
	}
}

// The timeline scrolls so the selected change stays in view.
func TestChangesScroll(t *testing.T) {
	var runs []machine.PackageRun
	for i := range 30 {
		runs = append(runs, machine.PackageRun{Started: t0.Add(-time.Duration(i) * time.Hour), Command: "apt install x" + string(rune('a'+i)),
			Installed: []machine.Package{{Name: "x", To: "1"}}})
	}
	m := model(vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{withPackages(healthy("vt100", 2, 7, 6), false, runs...)}})
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})
	for range 29 {
		next, _ = next.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	m = next.(Model)
	checkFits(t, m)
	if v := ansi.Strip(m.View()); !strings.Contains(v, "▸· 05:00 packages 1 installed · apt install x~") || !strings.Contains(v, "▸29h ago  1 package") {
		t.Errorf("the last change is out of view:\n%s", v)
	}
}

func TestRebootAndFailedApt(t *testing.T) {
	runs := nightly(t0.Add(-time.Hour))
	runs[0].Error = "Sub-process /usr/bin/dpkg returned an error code (1)"
	inv := vakt.Inventory{AskedAt: t0, Machines: []vakt.Machine{withPackages(healthy("vt100", 2, 7, 6), true, runs...)}}
	a := attention(inv)
	if len(a) != 2 || a[0].level != alarm || a[1].level != notice {
		t.Fatalf("attention %+v", a)
	}
	v := ansi.Strip(model(inv).View())
	for _, s := range []string{"apt failed: Sub-process", "waits for a reboot for linux-image-6.12.118"} {
		if !strings.Contains(v, s) {
			t.Errorf("overview lacks %q:\n%s", s, v)
		}
	}
	// A failure apt got past since is history, not a problem.
	runs = append([]machine.PackageRun{{Started: t0, Command: "apt upgrade", Upgraded: []machine.Package{{Name: "a", From: "1", To: "2"}}}}, runs...)
	if a := attention(vakt.Inventory{Machines: []vakt.Machine{withPackages(healthy("vt100", 2, 7, 6), false, runs...)}}); len(a) != 0 {
		t.Errorf("an old apt failure needs you: %+v", a)
	}
}

func TestCommand(t *testing.T) {
	for in, want := range map[string]string{"/usr/bin/unattended-upgrade": "unattended-upgrade", "apt install jq": "apt install jq", "": "apt"} {
		if got := command(in); got != want {
			t.Errorf("command(%q) = %q", in, got)
		}
	}
}
