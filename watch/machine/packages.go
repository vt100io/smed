package machine

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// PackageRun is one apt run from /var/log/apt/history.log: what it put on
// the machine, took off it or replaced.
type PackageRun struct {
	Started     time.Time `json:"started"`
	Command     string    `json:"command"`                // "/usr/bin/unattended-upgrade", "apt install jq"
	RequestedBy string    `json:"requested_by,omitempty"` // the user behind sudo, when apt says
	Installed   []Package `json:"installed,omitempty"`
	Upgraded    []Package `json:"upgraded,omitempty"`
	Removed     []Package `json:"removed,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// Package is one package in a run. From is empty for an install, To for a
// removal.
type Package struct {
	Name string `json:"name"`
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
}

// Reboot says the machine runs older code than it has installed: Debian
// writes /run/reboot-required when an upgrade needs one.
type Reboot struct {
	Required bool     `json:"required"`
	Packages []string `json:"packages,omitempty"` // the ones that asked
}

const (
	aptHistory     = "var/log/apt/history.log"
	rebootRequired = "run/reboot-required"
	maxPackageRuns = 30
)

// packageRuns reads apt's history, newest first. The rotated log is read
// too, so the history doesn't vanish the night logrotate runs. A machine
// without apt has none.
func (r Reader) packageRuns() []PackageRun {
	var runs []PackageRun
	for _, name := range []string{aptHistory + ".1", aptHistory} {
		b, err := os.ReadFile(filepath.Join(r.Root, name))
		if err == nil {
			runs = append(runs, parseAptHistory(b, time.Local)...)
		}
	}
	slices.Reverse(runs)
	return runs[:min(len(runs), maxPackageRuns)]
}

// parseAptHistory reads runs: blocks of "Key: value" lines, apart by a blank
// line. apt writes times in the machine's own zone.
func parseAptHistory(b []byte, loc *time.Location) []PackageRun {
	var out []PackageRun
	var run *PackageRun
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024) // one upgrade can list hundreds of packages
	for sc.Scan() {
		key, val, ok := strings.Cut(sc.Text(), ": ")
		if !ok {
			continue
		}
		if key == "Start-Date" {
			t, err := time.ParseInLocation("2006-01-02  15:04:05", strings.TrimSpace(val), loc)
			if err != nil {
				run = nil
				continue
			}
			out = append(out, PackageRun{Started: t.UTC()})
			run = &out[len(out)-1]
			continue
		}
		if run == nil {
			continue // a damaged run says nothing about the next
		}
		switch key {
		case "Commandline":
			run.Command = val
		case "Requested-By":
			run.RequestedBy, _, _ = strings.Cut(val, " (")
		case "Install", "Reinstall":
			run.Installed = append(run.Installed, parsePackages(val, false)...)
		case "Upgrade", "Downgrade":
			run.Upgraded = append(run.Upgraded, parsePackages(val, false)...)
		case "Remove", "Purge":
			run.Removed = append(run.Removed, parsePackages(val, true)...)
		case "Error":
			run.Error = val
		}
	}
	return out
}

// parsePackages reads "libk5crypto3:amd64 (1.21.3-5+deb13u1, 1.21.3-5+deb13u2), jq:amd64 (1.7.1-6, automatic)".
// An install gives one version, an upgrade two, a removal the one it took off.
func parsePackages(s string, removed bool) []Package {
	var out []Package
	for s != "" {
		name, rest, ok := strings.Cut(s, " (")
		if !ok {
			break
		}
		versions, after, _ := strings.Cut(rest, ")")
		s = strings.TrimPrefix(strings.TrimSpace(after), ", ")
		name, _, _ = strings.Cut(strings.TrimSpace(name), ":") // drop the architecture
		var vs []string
		for v := range strings.SplitSeq(versions, ",") {
			if v = strings.TrimSpace(v); v != "" && v != "automatic" {
				vs = append(vs, v)
			}
		}
		p := Package{Name: name}
		switch {
		case removed && len(vs) > 0:
			p.From = vs[0]
		case len(vs) == 1:
			p.To = vs[0]
		case len(vs) >= 2:
			p.From, p.To = vs[0], vs[1]
		}
		out = append(out, p)
	}
	return out
}

func (r Reader) reboot() Reboot {
	path := filepath.Join(r.Root, rebootRequired)
	if _, err := os.Stat(path); err != nil {
		return Reboot{}
	}
	rb := Reboot{Required: true}
	for line := range strings.Lines(readString(path + ".pkgs")) {
		if line = strings.TrimSpace(line); line != "" && !slices.Contains(rb.Packages, line) {
			rb.Packages = append(rb.Packages, line)
		}
	}
	return rb
}
