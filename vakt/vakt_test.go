package vakt

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vt100io/smed"
	"github.com/vt100io/smed/module/command"
	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/vakt/machine"
)

// fakeTailnet routes each peer to a local address instead of its tailnet IP.
type fakeTailnet struct {
	peers []tailnet.Peer
	addrs map[string]string // hostname → host:port
}

func (f fakeTailnet) Peers(context.Context) ([]tailnet.Peer, error) { return f.peers, nil }
func (f fakeTailnet) Self(context.Context) (tailnet.Peer, error)    { return tailnet.Peer{}, nil }
func (f fakeTailnet) Dial(ctx context.Context, p tailnet.Peer, _ string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "tcp", f.addrs[p.Hostname])
}

func responder(t *testing.T, rep machine.Report, status int) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != http.StatusOK {
			http.Error(w, "read meminfo: permission denied", status)
			return
		}
		_ = json.NewEncoder(w).Encode(rep)
	}))
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

// closedPort is an address where nothing listens: a machine without a responder.
func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func peer(name string, online bool, tags ...string) tailnet.Peer {
	return tailnet.Peer{Hostname: name, DNSName: name + ".example.ts.net.", Online: online, Tags: tags}
}

func steps(h *smed.HostSpec) *smed.HostSpec { return h.Run(command.Run{Cmd: "true"}) }

func TestAsk(t *testing.T) {
	healthy := machine.Report{Version: 1, OS: "Debian GNU/Linux 13 (trixie)", Uptime: 4 * 86400,
		Load: [3]float64{0.02, 0, 0}, Memory: machine.Memory{UsedPercent: 7.2},
		Disks: []machine.Disk{{Mount: "/", UsedPercent: 5.7}, {Mount: "/data", UsedPercent: 91}}}
	tn := fakeTailnet{
		peers: []tailnet.Peer{
			peer("vt100", true, "tag:vt100"),
			peer("web-2", true, "tag:web"),     // declared by tag, nothing listens
			peer("db-1", true, "tag:db"),       // undeclared, answers with an error
			peer("old", false, "tag:web"),      // declared by tag, offline
			peer("phone", true),                // personal: untagged, undeclared
			{Hostname: "funnel-ingress-node"},  // not a machine
			peer("future", true, "tag:vt100x"), // a version vakt can't read
		},
		addrs: map[string]string{
			"vt100":  responder(t, healthy, http.StatusOK),
			"web-2":  closedPort(t),
			"db-1":   responder(t, machine.Report{}, http.StatusInternalServerError),
			"future": responder(t, machine.Report{Version: 2}, http.StatusOK),
		},
	}
	fleet := Fleet{Hosts: []*smed.HostSpec{
		steps(smed.Host("vt100")),
		steps(smed.Tag("web")),
		steps(smed.Host("gone")), // declared, never joined
	}}
	inv, err := Ask(context.Background(), tn, fleet)
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]State{
		"vt100": Answered, "web-2": NoResponder, "db-1": Failing,
		"old": Offline, "gone": Missing, "future": Failing,
	}
	got := map[string]Machine{}
	for _, m := range inv.Machines {
		got[m.Name] = m
	}
	if len(got) != len(want) {
		t.Errorf("machines %v", inv.Machines)
	}
	for name, state := range want {
		if got[name].State != state {
			t.Errorf("%s: %q (%s), want %q", name, got[name].State, got[name].Err, state)
		}
	}
	if inv.Personal != 1 {
		t.Errorf("personal %d, want 1 (the phone)", inv.Personal)
	}
	if d := got["old"].Declared; len(d) != 1 || d[0] != `Tag("web")` {
		t.Errorf("old declared by %v", d)
	}
	if d := got["db-1"].Declared; len(d) != 0 {
		t.Errorf("db-1 is undeclared, got %v", d)
	}

	var out bytes.Buffer
	if err := inv.WriteText(&out); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, s := range []string{"1 of 6 answered", "1 personal device not asked"} {
		if !strings.Contains(text, s) {
			t.Errorf("header lacks %q:\n%s", s, text)
		}
	}
	rows := map[string][]string{ // what each machine's row must say
		"vt100":  {`Host("vt100")`, "answered", "/data 91%", "4d", "Debian GNU/Linux 13 (trixie)"},
		"gone":   {`Host("gone")`, "declared, missing"},
		"future": {"–", "error", "speaks version 2, vakt reads 1"},
		"db-1":   {"error", "500 Internal Server Error"},
		"web-2":  {`Tag("web")`, "no responder"},
	}
	lines := strings.Split(text, "\n")
	for name, parts := range rows {
		var line string
		for _, l := range lines {
			if strings.HasPrefix(l, name+" ") {
				line = l
			}
		}
		for _, part := range parts {
			if !strings.Contains(line, part) {
				t.Errorf("%s row %q lacks %q", name, line, part)
			}
		}
	}
	// The columns line up: every row's STATE starts where the header's does.
	// Counted in characters, as a terminal shows them: "–" is three bytes.
	col := len([]rune(lines[2][:strings.Index(lines[2], "STATE")]))
	for _, l := range lines[3:] {
		r := []rune(l)
		if len(r) == 0 {
			continue
		}
		if r[col-1] != ' ' || r[col] == ' ' {
			t.Errorf("misaligned row: %q", l)
		}
	}
}
