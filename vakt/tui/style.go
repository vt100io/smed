package tui

import "github.com/charmbracelet/lipgloss"

// The palette is vt100.io's own (Nord), so the site and the tool feel like
// one thing. Green, yellow and red are Nord's aurora, used only to
// say something: ok, look at this, act on this.
var (
	night3 = lipgloss.Color("#4c566a") // borders, empty bars, missing samples
	muted  = lipgloss.Color("#b4bdcc") // secondary text
	snow   = lipgloss.Color("#eceff4") // names, headings
	snow2  = lipgloss.Color("#d8dee9") // text
	frost  = lipgloss.Color("#88c0d0") // accent: focus, keys, healthy graphs
	green  = lipgloss.Color("#a3be8c")
	yellow = lipgloss.Color("#ebcb8b")
	red    = lipgloss.Color("#bf616a")
)

var (
	styleText     = lipgloss.NewStyle().Foreground(snow2)
	styleName     = lipgloss.NewStyle().Foreground(snow).Bold(true)
	styleMuted    = lipgloss.NewStyle().Foreground(muted)
	styleDim      = lipgloss.NewStyle().Foreground(night3)
	styleAccent   = lipgloss.NewStyle().Foreground(frost)
	styleOK       = lipgloss.NewStyle().Foreground(green)
	styleWarn     = lipgloss.NewStyle().Foreground(yellow)
	styleBad      = lipgloss.NewStyle().Foreground(red)
	styleTitle    = lipgloss.NewStyle().Foreground(muted)
	styleTitleOn  = lipgloss.NewStyle().Foreground(frost).Bold(true)
	styleBorder   = lipgloss.NewStyle().Foreground(night3)
	styleBorderOn = lipgloss.NewStyle().Foreground(frost)
	styleHeading  = lipgloss.NewStyle().Foreground(muted).Bold(true)
	styleBrand    = lipgloss.NewStyle().Foreground(frost).Bold(true)
)

// level is how much a value asks of you.
type level int

const (
	fine level = iota
	notice
	alarm
)

func (l level) style() lipgloss.Style {
	switch l {
	case alarm:
		return styleBad
	case notice:
		return styleWarn
	}
	return styleAccent
}

// textStyle is for words rather than graphs: plain when all is well.
func (l level) textStyle() lipgloss.Style {
	if l == fine {
		return styleText
	}
	return l.style()
}

// The thresholds live here, in the control center, not on the machines:
// machines report facts, the viewer decides what is worth your attention.
func diskLevel(p float64) level {
	switch {
	case p >= 90:
		return alarm
	case p >= 80:
		return notice
	}
	return fine
}

func memLevel(p float64) level {
	switch {
	case p >= 95:
		return alarm
	case p >= 90:
		return notice
	}
	return fine
}

func cpuLevel(p float64) level {
	if p >= 90 {
		return notice // busy is not broken; it only becomes alarm with history
	}
	return fine
}
