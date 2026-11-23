package watch

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/watch/machine"
)

// Punchcard is what Punchcard said about builds and audits. smed joins it
// with what the machines run: an audit is of a binary, and unit deploys
// only binaries byte-identical to Punchcard's, so the checksum in a deploy
// record names the audit of what runs.
type Punchcard struct {
	AskedAt time.Time
	Builds  []Build // newest first
	Audits  []Audit // the newest audit of each artifact
}

// Build and Audit are the parts of Punchcard's records smed uses; its
// /api/builds and /api/audits send more.
type Build struct {
	Repo     string            `json:"repo"`
	Event    string            `json:"event"`
	Branch   string            `json:"branch,omitempty"`
	SHA      string            `json:"sha,omitempty"`
	Who      string            `json:"who,omitempty"`
	Result   string            `json:"result,omitempty"`
	Finished time.Time         `json:"finished"`
	Binaries map[string]string `json:"binaries,omitempty"`
}

type Audit struct {
	Repo     string    `json:"repo"`
	SHA      string    `json:"sha"`
	Artifact string    `json:"artifact"`
	SHA256   string    `json:"sha256"`
	Scanner  string    `json:"scanner,omitempty"`
	Checked  time.Time `json:"checked"`
	Findings []Finding `json:"findings,omitempty"`
	Error    string    `json:"error,omitempty"`
}

// Finding is one known vulnerability, shaped after OSV whatever the
// language: Reach says how close the binary comes to it.
type Finding struct {
	ID        string   `json:"id"`
	Aliases   []string `json:"aliases,omitempty"`
	Summary   string   `json:"summary,omitempty"`
	Ecosystem string   `json:"ecosystem"`
	Package   string   `json:"package"`
	Version   string   `json:"version"`
	Fixed     string   `json:"fixed,omitempty"`
	Reach     string   `json:"reach"` // called, imported or required
}

// Called is a vulnerability the running code reaches: the only kind that
// asks something of you. Imported and required ones are context.
func (f Finding) Called() bool { return f.Reach == "called" }

const punchcardAPIVersion = 1

// AskPunchcard reads builds and audits from Punchcard at addr
// ("machine:port") over the tailnet.
func AskPunchcard(ctx context.Context, tn tailnet.Tailnet, addr string) (Punchcard, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return Punchcard{}, fmt.Errorf("punchcard %q: %w", addr, err)
	}
	peers, err := tn.Peers(ctx)
	if err != nil {
		return Punchcard{}, err
	}
	i := slices.IndexFunc(peers, func(p tailnet.Peer) bool { return strings.EqualFold(p.Hostname, host) })
	if i < 0 {
		return Punchcard{}, fmt.Errorf("punchcard: no machine %q in the tailnet", host)
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return tn.Dial(ctx, peers[i], port) },
	}}
	pc := Punchcard{AskedAt: time.Now()}
	var b struct {
		Version int
		Builds  []Build
	}
	if err := getJSON(ctx, client, "http://"+addr+"/api/builds?n=500", &b.Version, &b); err != nil {
		return pc, err
	}
	var a struct {
		Version int
		Audits  []Audit
	}
	if err := getJSON(ctx, client, "http://"+addr+"/api/audits", &a.Version, &a); err != nil {
		return pc, err
	}
	pc.Builds, pc.Audits = b.Builds, a.Audits
	return pc, nil
}

func getJSON(ctx context.Context, c *http.Client, url string, version *int, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := c.Do(req)
	if err != nil {
		return fmt.Errorf("punchcard: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("punchcard: %s says %s", url, resp.Status)
	}
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		return fmt.Errorf("punchcard: reading %s: %w", url, err)
	}
	if *version != punchcardAPIVersion {
		return fmt.Errorf("punchcard: %s is version %d, smed reads %d", url, *version, punchcardAPIVersion)
	}
	return nil
}

// Security is what's known about the vulnerabilities in what an app runs.
type Security struct {
	// Audit is the newest audit of the running binary; nil when Punchcard
	// has none: built before Punchcard kept binaries, or not by Punchcard.
	Audit *Audit
	// Called is the audit's findings the running code reaches.
	Called []Finding
	// Fix is the newest passed build of the same repo, newer than what
	// runs, whose audit has none of Called: deploying it fixes them. Nil
	// when there's no such build yet.
	Fix *Build
}

// SecurityOf finds the audit of what app runs, by the checksum of the
// binary unit deployed, and a newer build that doesn't have its called
// findings. Only by checksum: a commit doesn't say which repo it's in, and
// a binary not built by Punchcard isn't the binary Punchcard audited. A
// deploy record without a checksum (from before unit's CI gate) is
// simply not audited.
func (pc Punchcard) SecurityOf(app machine.App) Security {
	var s Security
	sum := runningSHA256(app)
	if sum == "" {
		return s
	}
	i := slices.IndexFunc(pc.Audits, func(a Audit) bool { return a.SHA256 == sum })
	if i < 0 {
		return s
	}
	run := pc.Audits[i]
	s.Audit = &run
	for _, f := range run.Findings {
		if f.Called() {
			s.Called = append(s.Called, f)
		}
	}
	if len(s.Called) == 0 {
		return s
	}
	// The builds of the same repo that finished after the running one,
	// newest first, whose audit of the same artifact is clean of Called.
	ranAt := time.Time{}
	for _, b := range pc.Builds {
		if b.Repo == run.Repo && b.SHA == run.SHA {
			ranAt = b.Finished
			break
		}
	}
	for _, b := range pc.Builds {
		if b.Repo != run.Repo || b.Result != "passed" || b.Event == "pull_request" || !b.Finished.After(ranAt) || b.SHA == run.SHA {
			continue
		}
		j := slices.IndexFunc(pc.Audits, func(a Audit) bool { return a.Repo == b.Repo && a.SHA == b.SHA && a.Artifact == run.Artifact })
		if j < 0 || pc.Audits[j].Error != "" || hasAny(pc.Audits[j].Findings, s.Called) {
			continue
		}
		fix := b
		s.Fix = &fix
		break
	}
	return s
}

// runningSHA256 is the checksum unit recorded for the release that runs.
func runningSHA256(app machine.App) string {
	for _, d := range app.Deploys {
		if d.Version == app.Release && d.Result == "live" {
			return d.SHA256
		}
	}
	return ""
}

func hasAny(fs, of []Finding) bool {
	for _, f := range fs {
		if slices.ContainsFunc(of, func(o Finding) bool { return o.ID == f.ID }) {
			return true
		}
	}
	return false
}

// Bumps is what to upgrade for Called when no build fixes them yet: each
// package once, to the highest version that fixes all of its findings.
// A finding without a fixed version yet is listed with Fixed "".
func (s Security) Bumps() []Finding {
	var out []Finding
	for _, f := range s.Called {
		i := slices.IndexFunc(out, func(o Finding) bool { return o.Ecosystem == f.Ecosystem && o.Package == f.Package })
		switch {
		case i < 0:
			out = append(out, f)
		case f.Fixed == "" || out[i].Fixed == "":
			out[i].Fixed = ""
		case compareVersions(f.Fixed, out[i].Fixed) > 0:
			out[i].Fixed = f.Fixed
		}
	}
	return out
}

// compareVersions compares "v0.45.0", "1.27.5", "2.31.0rc1" number by
// number, and what isn't a number as text.
func compareVersions(a, b string) int {
	split := func(s string) []string {
		return strings.FieldsFunc(strings.TrimPrefix(s, "v"), func(r rune) bool { return r == '.' || r == '-' || r == '+' })
	}
	pa, pb := split(a), split(b)
	for i := range max(len(pa), len(pb)) {
		var x, y string
		if i < len(pa) {
			x = pa[i]
		}
		if i < len(pb) {
			y = pb[i]
		}
		nx, ex := strconv.Atoi(x)
		ny, ey := strconv.Atoi(y)
		var c int
		if ex == nil && ey == nil {
			c = cmp.Compare(nx, ny)
		} else {
			c = strings.Compare(x, y)
		}
		if c != 0 {
			return c
		}
	}
	return 0
}
