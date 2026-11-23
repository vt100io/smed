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
	for _, s := range []string{"Nothing needs you.", "● all quiet", "asked 3s ago", "vt100", "1 machines · 1 answered"} {
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
	for _, s := range []string{"─ vt100 ─", `declared by Host("vt100")`, "load 0.04", "DISKS", "75 GiB free of 80 GiB"} {
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
