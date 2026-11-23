// Package vakt keeps watch over what you declared with smed. It asks every
// machine in the tailnet what it looks like right now, and compares the
// answers with your HostSpecs: which declared machines answer, which are
// missing, and which machines nobody declared.
//
// It asks; it never acts. It needs no SSH and no secrets, only the tailnet
// and each machine's read-only responder (cmd/vakt-responder).
package vakt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/vt100io/smed"
	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/vakt/machine"
)

// Fleet is what you declared. It may be empty: then vakt shows the tailnet
// as it is, with nothing to compare against.
type Fleet struct {
	Hosts []*smed.HostSpec
	// Punchcard is where Punchcard serves builds and audits on the
	// tailnet, as "machine:port": "vt100:8081". Empty means no Punchcard.
	Punchcard string
}

// State is what vakt learned about one machine.
type State string

const (
	Answered    State = "answered"
	Failing     State = "error"             // the responder answered, with an error
	NoResponder State = "no responder"      // the machine refused the connection
	Silent      State = "silent"            // no answer in time
	Offline     State = "offline"           // in the tailnet, but offline
	Missing     State = "declared, missing" // declared, but no such machine in the tailnet
)

// Machine is one row of the inventory.
type Machine struct {
	Name     string
	Tags     []string
	Declared []string // the HostSpecs that select it, as Host("x") or Tag("y")
	State    State
	Report   *machine.Report
	Err      string
}

// Inventory is the whole answer.
type Inventory struct {
	AskedAt  time.Time
	Machines []Machine
	// Personal is how many untagged, undeclared devices were left alone.
	// Tailscale tags mark servers; laptops and phones aren't asked.
	Personal int
}

// Timeout is how long a machine gets to answer.
const Timeout = 3 * time.Second

// Ask asks every machine that matters: tagged ones, and any you declared.
func Ask(ctx context.Context, tn tailnet.Tailnet, fleet Fleet) (Inventory, error) {
	inv := Inventory{AskedAt: time.Now()}
	peers, err := tn.Peers(ctx)
	if err != nil {
		return inv, err
	}
	matched := map[int]bool{} // index into fleet.Hosts that matched at least one peer
	for _, p := range peers {
		if p.DNSName == "" {
			continue // not a machine: Tailscale's own funnel ingress nodes
		}
		m := Machine{Name: p.Hostname, Tags: p.Tags}
		for i, h := range fleet.Hosts {
			if selects(h, p) {
				m.Declared = append(m.Declared, describe(h))
				matched[i] = true
			}
		}
		if len(p.Tags) == 0 && len(m.Declared) == 0 {
			inv.Personal++
			continue
		}
		if !p.Online {
			m.State = Offline
		}
		inv.Machines = append(inv.Machines, m)
	}
	for i, h := range fleet.Hosts {
		if matched[i] {
			continue
		}
		name, tag := h.Selector()
		if name == "" {
			name = "tag:" + tag
		}
		inv.Machines = append(inv.Machines, Machine{Name: name, Declared: []string{describe(h)}, State: Missing})
	}

	byName := map[string]tailnet.Peer{}
	for _, p := range peers {
		byName[p.Hostname] = p
	}
	var wg sync.WaitGroup
	for i := range inv.Machines {
		m := &inv.Machines[i]
		if m.State != "" {
			continue
		}
		wg.Go(func() { ask(ctx, tn, byName[m.Name], m) })
	}
	wg.Wait()
	slices.SortStableFunc(inv.Machines, func(a, b Machine) int { return strings.Compare(a.Name, b.Name) })
	return inv, nil
}

func ask(ctx context.Context, tn tailnet.Tailnet, p tailnet.Peer, m *Machine) {
	ctx, cancel := context.WithTimeout(ctx, Timeout)
	defer cancel()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return tn.Dial(ctx, p, machine.Port)
		},
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+p.Hostname+"/", nil)
	if err != nil {
		m.State, m.Err = Failing, err.Error()
		return
	}
	resp, err := client.Do(req)
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		m.State = NoResponder
		return
	case err != nil:
		m.State, m.Err = Silent, err.Error()
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		m.State, m.Err = Failing, resp.Status
		return
	}
	var rep machine.Report
	if err := json.NewDecoder(resp.Body).Decode(&rep); err != nil {
		m.State, m.Err = Failing, "unreadable answer: "+err.Error()
		return
	}
	if rep.Version != machine.Version {
		m.State, m.Err = Failing, fmt.Sprintf("speaks version %d, vakt reads %d", rep.Version, machine.Version)
		return
	}
	m.State, m.Report = Answered, &rep
}

func selects(h *smed.HostSpec, p tailnet.Peer) bool {
	name, tag := h.Selector()
	if name != "" {
		return p.Hostname == name
	}
	return slices.Contains(p.Tags, "tag:"+tag)
}

func describe(h *smed.HostSpec) string {
	name, tag := h.Selector()
	if name != "" {
		return fmt.Sprintf("Host(%q)", name)
	}
	return fmt.Sprintf("Tag(%q)", tag)
}
