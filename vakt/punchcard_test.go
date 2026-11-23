package vakt

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/vakt/machine"
)

var p0 = time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)

func finding(id, pkg, version, fixed, reach string) Finding {
	return Finding{ID: id, Ecosystem: "Go", Package: pkg, Version: version, Fixed: fixed, Reach: reach}
}

var (
	xnetCalled   = finding("GO-2026-4441", "golang.org/x/net", "v0.17.0", "v0.45.0", "called")
	xnetCalled2  = finding("GO-2026-5030", "golang.org/x/net", "v0.17.0", "v0.55.0", "called")
	xnetRequired = finding("GO-2024-2687", "golang.org/x/net", "v0.17.0", "v0.23.0", "required")
)

// fleetPunchcard: vt100 runs a111111. a222222 passed but still has the
// called finding; a333333 passed later and is clean; a444444 is a pull
// request. Another repo's build is newer than all of them.
func fleetPunchcard() Punchcard {
	b := func(repo, sha, event, result string, at time.Duration) Build {
		return Build{Repo: repo, SHA: sha, Event: event, Branch: "main", Result: result, Finished: p0.Add(at), Binaries: map[string]string{"bin/vt100": sha + "-sum"}}
	}
	au := func(repo, sha string, fs ...Finding) Audit {
		return Audit{Repo: repo, SHA: sha, Artifact: "bin/vt100", SHA256: sha + "-sum", Scanner: "govulncheck v1.10.0", Checked: p0.Add(10 * time.Hour), Findings: fs}
	}
	return Punchcard{
		Builds: []Build{ // newest first
			b("vt100io/other", "o999999", "push", "passed", 6*time.Hour),
			b("vt100io/vt100", "a444444", "pull_request", "passed", 5*time.Hour),
			b("vt100io/vt100", "a333333", "push", "passed", 4*time.Hour),
			b("vt100io/vt100", "a222222", "push", "passed", 3*time.Hour),
			b("vt100io/vt100", "a111111", "push", "passed", 1*time.Hour),
		},
		Audits: []Audit{
			au("vt100io/other", "o999999"),
			au("vt100io/vt100", "a444444"),
			au("vt100io/vt100", "a333333", xnetRequired),
			au("vt100io/vt100", "a222222", xnetCalled),
			au("vt100io/vt100", "a111111", xnetCalled, xnetCalled2, xnetRequired),
		},
	}
}

func running(release, sum string) machine.App {
	return machine.App{Name: "vt100", Release: release, Deploys: []machine.Deploy{{Version: release, SHA256: sum, Result: "live"}}}
}

func TestFixReadyNotDeployed(t *testing.T) {
	s := fleetPunchcard().SecurityOf(running("a111111", "a111111-sum"))
	if s.Audit == nil || s.Audit.SHA != "a111111" {
		t.Fatalf("audit %+v", s.Audit)
	}
	// Required findings ask nothing of you.
	if len(s.Called) != 2 {
		t.Errorf("called %+v", s.Called)
	}
	// Not a222222 (still has GO-2026-4441), not the pull request: a333333.
	if s.Fix == nil || s.Fix.SHA != "a333333" {
		t.Errorf("fix %+v", s.Fix)
	}
}

func TestNoFixYet(t *testing.T) {
	pc := fleetPunchcard()
	pc.Audits[2].Findings = []Finding{xnetCalled2} // a333333 still reaches one
	pc.Audits[1].Findings = []Finding{xnetCalled2}
	s := pc.SecurityOf(running("a111111", "a111111-sum"))
	if s.Fix != nil {
		t.Errorf("fix %+v", s.Fix)
	}
	// One bump per package, to the version that fixes both.
	if b := s.Bumps(); len(b) != 1 || b[0].Package != "golang.org/x/net" || b[0].Fixed != "v0.55.0" {
		t.Errorf("bumps %+v", b)
	}
}

func TestCleanAndUnaudited(t *testing.T) {
	pc := fleetPunchcard()
	if s := pc.SecurityOf(running("a333333", "a333333-sum")); s.Audit == nil || len(s.Called) != 0 || s.Fix != nil {
		t.Errorf("clean: %+v", s)
	}
	// Deployed before Punchcard kept binaries: nothing known.
	if s := pc.SecurityOf(running("f000000", "f000000-sum")); s.Audit != nil {
		t.Errorf("unaudited: %+v", s)
	}
	// A deploy record without a checksum isn't matched by its commit: a
	// short commit doesn't say which repo it's in.
	if s := pc.SecurityOf(running("a111111", "")); s.Audit != nil {
		t.Errorf("by commit: %+v", s.Audit)
	}
	// The checksum wins over the commit: a binary not built by Punchcard
	// isn't Punchcard's audit, whatever its commit.
	if s := pc.SecurityOf(running("a111111", "built-on-a-laptop")); s.Audit != nil {
		t.Errorf("laptop build matched: %+v", s.Audit)
	}
}

func TestBumpsWithoutAFixedVersion(t *testing.T) {
	s := Security{Called: []Finding{
		finding("GO-1", "stdlib", "v1.27.4", "v1.27.5", "called"),
		finding("GO-2", "golang.org/x/crypto", "v0.40.0", "", "called"),
		finding("GO-3", "golang.org/x/crypto", "v0.40.0", "v0.44.0", "called"),
	}}
	b := s.Bumps()
	if len(b) != 2 || b[0].Fixed != "v1.27.5" || b[1].Fixed != "" {
		t.Errorf("%+v", b)
	}
}

func TestCompareVersions(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want int
	}{
		{"v0.45.0", "v0.9.0", 1}, {"v1.27.5", "1.27.5", 0}, {"2.31.0", "2.31.0rc1", -1}, {"v0.55.0", "v0.55.1", -1},
	} {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compare(%s, %s) = %d", c.a, c.b, got)
		}
	}
}

func TestAskPunchcard(t *testing.T) {
	pc := fleetPunchcard()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/builds":
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "builds": pc.Builds})
		case "/api/audits":
			json.NewEncoder(w).Encode(map[string]any{"version": 1, "audits": pc.Audits})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	tn := fakeTailnet{peers: []tailnet.Peer{peer("vt100", true, "tag:vt100")}, addrs: map[string]string{"vt100": srv.Listener.Addr().String()}}

	got, err := AskPunchcard(context.Background(), tn, "vt100:8081")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Builds) != 5 || len(got.Audits) != 5 || got.Audits[4].Findings[0].ID != "GO-2026-4441" {
		t.Errorf("%+v", got)
	}
	if _, err := AskPunchcard(context.Background(), tn, "nowhere:8081"); err == nil || !strings.Contains(err.Error(), `no machine "nowhere"`) {
		t.Errorf("missing machine: %v", err)
	}
}
