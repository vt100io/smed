package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/vt100io/smed/vakt"
	"github.com/vt100io/smed/vakt/machine"
)

// staleAudit is when an audit of a running binary is too old to trust:
// Punchcard audits what it keeps every night, so an old one means the
// nightly run stopped, or the running release is older than what
// Punchcard keeps.
const staleAudit = 3 * 24 * time.Hour

// secFunc finds what's known about the vulnerabilities in what an app
// runs; nil when vakt has nothing from Punchcard.
type secFunc func(machine.App) *vakt.Security

// sec is m's secFunc: nil until Punchcard has answered once.
func (m Model) sec() secFunc {
	if m.pc == nil {
		return nil
	}
	pc := *m.pc
	return func(a machine.App) *vakt.Security {
		s := pc.SecurityOf(a)
		return &s
	}
}

func (f secFunc) of(a machine.App) *vakt.Security {
	if f == nil {
		return nil
	}
	return f(a)
}

// securityLevel is how much an app's vulnerabilities ask of you. Only what
// the running code reaches is an alarm; a stale or failed audit is a
// notice, since then nobody is watching. Not audited at all is quiet:
// apps deployed before Punchcard kept binaries are, until their next
// deploy.
func securityLevel(a machine.App, s *vakt.Security, now time.Time) (level, string) {
	if s == nil || s.Audit == nil {
		return fine, ""
	}
	switch {
	case len(s.Called) > 0 && s.Fix != nil:
		return alarm, fmt.Sprintf("app %s reaches %s; %s fixes it", a.Name, vulnNames(s.Called), short(s.Fix.SHA))
	case len(s.Called) > 0:
		return alarm, fmt.Sprintf("app %s reaches %s, not fixed yet", a.Name, vulnNames(s.Called))
	case s.Audit.Error != "":
		return notice, fmt.Sprintf("app %s: its last audit failed", a.Name)
	case now.Sub(s.Audit.Checked) > staleAudit:
		return notice, fmt.Sprintf("app %s: audited %s", a.Name, ago(now.Sub(s.Audit.Checked)))
	}
	return fine, ""
}

func vulnNames(fs []vakt.Finding) string {
	if len(fs) == 1 {
		return fs[0].ID
	}
	return fmt.Sprintf("%d vulnerabilities", len(fs))
}

// bumpText says what to upgrade: "bump golang.org/x/net to v0.55.0".
func bumpText(bs []vakt.Finding) string {
	var parts []string
	for _, b := range bs {
		name, fixed := b.Package, b.Fixed
		if b.Ecosystem == "Go" && name == "stdlib" {
			name, fixed = "Go", strings.TrimPrefix(fixed, "v") // "Go to 1.27.5"
		}
		if fixed == "" {
			parts = append(parts, name+" (no fix yet)")
		} else {
			parts = append(parts, name+" to "+fixed)
		}
	}
	return "bump " + strings.Join(parts, ", ")
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// securityLines is the SECURITY section of an app's screen.
func securityLines(a machine.App, s *vakt.Security, pcErr error, now time.Time) []string {
	out := []string{"", styleHeading.Render("SECURITY") + styleMuted.Render(" from Punchcard")}
	switch {
	case s == nil && pcErr != nil:
		return append(out, styleWarn.Render("Punchcard didn't answer: "+pcErr.Error()))
	case s == nil:
		return append(out, styleMuted.Render("Asking Punchcard…"))
	case s.Audit == nil:
		return append(out, styleMuted.Render("Not audited: Punchcard has no audit of this binary."),
			styleDim.Render("Deployed before Punchcard kept binaries, or not built by it. The next deploy will be."))
	}
	au := s.Audit
	when := styleMuted.Render(fmt.Sprintf("audited %s · %s · %s %s", ago(now.Sub(au.Checked)), au.Scanner, au.Artifact, short(au.SHA)))
	if now.Sub(au.Checked) > staleAudit {
		when = styleWarn.Render(fmt.Sprintf("audited %s · %s", ago(now.Sub(au.Checked)), au.Scanner))
	}
	out = append(out, when)
	if au.Error != "" {
		return append(out, styleWarn.Render("The audit failed: "+au.Error), styleDim.Render("No findings then means unknown, not clean."))
	}
	if len(s.Called) == 0 {
		out = append(out, styleOK.Render("Nothing known that the code reaches."))
	} else {
		for _, f := range s.Called {
			fix := "no fix yet"
			if f.Fixed != "" {
				fix = "→ " + f.Fixed
			}
			out = append(out, styleBad.Render("✗ ")+styleText.Render(fitLeft(f.ID, 14))+styleMuted.Render(f.Package+" "+f.Version+" "+fix))
			if f.Summary != "" {
				out = append(out, "                "+styleText.Render(f.Summary))
			}
		}
		if s.Fix != nil {
			out = append(out, styleOK.Render(short(s.Fix.SHA)+" fixes it: passed CI, audited without it")+
				styleMuted.Render(" → ")+styleAccent.Render("unit deploy"))
		} else {
			out = append(out, styleWarn.Render("No build without them yet: "+bumpText(s.Bumps())+"."))
		}
	}
	if rest := len(au.Findings) - len(s.Called); rest > 0 {
		out = append(out, styleDim.Render(fmt.Sprintf("%d more known in what it includes, not reached by its code.", rest)))
	}
	return out
}
