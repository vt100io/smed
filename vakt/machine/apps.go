package machine

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// App is one app unit put on the machine: what runs, how it got there, and
// what it uses. Everything comes from files unit and systemd already keep;
// the responder needs no D-Bus and no rights beyond reading.
type App struct {
	Name           string   `json:"name"`
	Release        string   `json:"release"` // what /opt/unit/<name>/current points at
	Running        bool     `json:"running"` // the service has processes
	MemoryBytes    uint64   `json:"memory_bytes"`
	MemoryMaxBytes uint64   `json:"memory_max_bytes,omitempty"` // 0 means no limit
	Tasks          int      `json:"tasks"`
	Port           int      `json:"port,omitempty"`
	Domains        []string `json:"domains,omitempty"`
	// Deploys is the app's deploy history from unit's records, newest
	// first, at most maxDeploys.
	Deploys []Deploy `json:"deploys,omitempty"`
}

// Deploy is one line of unit's deploy log, for one app.
type Deploy struct {
	Version  string    `json:"version"`
	Previous string    `json:"previous,omitempty"`
	CI       string    `json:"ci,omitempty"` // the CI check's state, or "skipped"
	SameAsCI bool      `json:"same_as_ci,omitempty"`
	Who      string    `json:"who"`
	From     string    `json:"from,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Result   string    `json:"result"` // live, rolled-back, failed
	Error    string    `json:"error,omitempty"`
}

const maxDeploys = 10

// Where unit and systemd keep what Apps reads. Reader.Root moves them all,
// for tests.
const (
	unitDir     = "opt/unit"
	recordsPath = "var/log/unit/deploys.jsonl"
	systemdDir  = "etc/systemd/system"
	caddyDir    = "etc/caddy/conf.d"
	cgroupDir   = "sys/fs/cgroup/system.slice"
)

// apps lists what unit installed. A machine without unit has none, and
// that's not an error. A file about one app that can't be read leaves that
// field empty: the rest of the answer is still true.
func (r Reader) apps() []App {
	entries, err := os.ReadDir(filepath.Join(r.Root, unitDir))
	if err != nil {
		return nil
	}
	deploys := r.deploys()
	var out []App
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		a := App{Name: e.Name(), Deploys: deploys[e.Name()]}
		if target, err := os.Readlink(filepath.Join(r.Root, unitDir, a.Name, "current")); err == nil {
			a.Release = filepath.Base(target)
		}
		cg := filepath.Join(r.Root, cgroupDir, a.Name+".service")
		a.Running = strings.Contains(readString(filepath.Join(cg, "cgroup.events")), "populated 1")
		a.MemoryBytes = readUint(filepath.Join(cg, "memory.current"))
		a.MemoryMaxBytes = readUint(filepath.Join(cg, "memory.max")) // "max" reads as 0: no limit
		a.Tasks = int(readUint(filepath.Join(cg, "pids.current")))
		a.Port = listenPort(readString(filepath.Join(r.Root, systemdDir, a.Name+".socket")))
		a.Domains = caddyDomains(readString(filepath.Join(r.Root, caddyDir, a.Name+".conf")))
		out = append(out, a)
	}
	return out
}

// deploys reads unit's log once, newest first per app. Uninstalls end an
// app's history: a later install starts a new one.
func (r Reader) deploys() map[string][]Deploy {
	f, err := os.Open(filepath.Join(r.Root, recordsPath))
	if err != nil {
		return nil
	}
	defer f.Close()
	out := map[string][]Deploy{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		var rec struct {
			App    string `json:"app"`
			Action string `json:"action"`
			Deploy
		}
		if json.Unmarshal(sc.Bytes(), &rec) != nil || rec.App == "" {
			continue // a damaged line says nothing about the others
		}
		if rec.Action == "uninstall" {
			delete(out, rec.App)
			continue
		}
		out[rec.App] = append(out[rec.App], rec.Deploy)
	}
	for app, ds := range out {
		slices.Reverse(ds)
		out[app] = ds[:min(len(ds), maxDeploys)]
	}
	return out
}

func readString(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

func readUint(path string) uint64 {
	v, _ := strconv.ParseUint(strings.TrimSpace(readString(path)), 10, 64)
	return v
}

// listenPort reads ListenStream= from a socket unit: "8080" or "127.0.0.1:8080".
func listenPort(unit string) int {
	for line := range strings.Lines(unit) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "ListenStream="); ok {
			if i := strings.LastIndex(v, ":"); i >= 0 {
				v = v[i+1:]
			}
			p, _ := strconv.Atoi(v)
			return p
		}
	}
	return 0
}

// caddyDomains reads the site addresses from the first line of unit's Caddy
// snippet: "vt100.io, www.vt100.io {".
func caddyDomains(conf string) []string {
	first, _, _ := strings.Cut(conf, "{")
	var out []string
	for d := range strings.SplitSeq(first, ",") {
		if d = strings.TrimSpace(d); d != "" {
			out = append(out, d)
		}
	}
	return out
}
