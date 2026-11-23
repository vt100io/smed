// Package ufw implements module.Module for Ubuntu's firewall frontend:
// smed's version of community.general.ufw. Two separate types, matching
// the project's taste for small focused modules over one that branches
// on "what kind of thing is this": Rule (an allow/deny rule — only ever
// added, never removed, same no-Absent pattern as file/apt/service/user)
// and Default (enabled state + default incoming/outgoing policy).
//
// ufw has no machine-readable status output, so Read parses
// `ufw status` / `ufw status verbose` text directly — a pragmatic
// heuristic grounded in real captured output (see ufw_test.go), not a
// formal parser. Known limitations, stated rather than hidden: outbound
// rules aren't matched (the role this was built against only ever adds
// inbound allow rules), and an app-profile rule (e.g. "OpenSSH") is
// matched by its name appearing in the line, not by independently
// verifying the port it resolves to.
//
// Read the safety note on Default before using it — getting the
// enable/policy ordering wrong on a real host can lock out SSH access
// entirely, including over Tailscale (traffic still passes through the
// normal netfilter INPUT chain on tailscale0; it isn't exempted).
package ufw

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/vt100io/smed/module"
	"github.com/vt100io/smed/shellquote"
)

// Rule is a single inbound allow/deny rule: a named application profile
// (ufw app list), or any mix of port, source and interface. App can't be
// mixed with the others, Proto needs a Port, and a rule must say at least
// one thing, since a bare "ufw allow" isn't a rule:
//
//	Rule{Allow: true, Port: "22", Proto: "tcp"}             // ufw allow 22/tcp
//	Rule{Allow: true, From: "100.64.0.0/10"}                // every port, from the tailnet
//	Rule{Allow: true, In: "tailscale0"}                     // everything that comes in over Tailscale
//	Rule{Allow: true, In: "tailscale0", Port: "5432"}       // one port, over Tailscale only
type Rule struct {
	Allow bool   // true = allow, false = deny
	Port  string // e.g. "5432"; "" means every port
	Proto string // e.g. "tcp"; "" means unspecified (matches any proto for this port)
	From  string // source CIDR/IP; "" means "Anywhere" (ufw's own term for any source)
	In    string // the interface it comes in on, e.g. "tailscale0"; "" means any
	App   string // a named profile from `ufw app list`, e.g. "OpenSSH"
}

// check refuses a rule ufw would reject or misread, so Plan says so
// rather than Apply failing on the host.
func (r Rule) check() error {
	switch {
	case r.App != "" && (r.Port != "" || r.Proto != "" || r.From != "" || r.In != ""):
		return fmt.Errorf("ufw rule %s: App can't be combined with Port, Proto, From or In", r.App)
	case r.App != "":
		return nil
	case r.Proto != "" && r.Port == "":
		return fmt.Errorf("ufw rule: Proto %s needs a Port", r.Proto)
	case r.Port == "" && r.From == "" && r.In == "":
		return fmt.Errorf("ufw rule: says nothing to match; set Port, From, In or App")
	}
	return nil
}

type ruleState struct {
	Exists bool
}

func (r Rule) Desired() module.State { return ruleState{Exists: true} }

// Read checks whether a matching rule is already active. Never mutates
// — `ufw status` is purely informational.
func (r Rule) Read(ctx context.Context, run module.Runner) (module.State, error) {
	out, err := run.Run(ctx, "ufw status 2>&1")
	if err != nil {
		return nil, fmt.Errorf("read ufw rule: %w", err)
	}
	for _, line := range strings.Split(out, "\n") {
		if r.matches(line) {
			return ruleState{Exists: true}, nil
		}
	}
	return ruleState{Exists: false}, nil
}

// row is one rule in `ufw status`: To, the action with an optional
// direction, From, and an optional comment. ufw pads the columns with
// spaces; To and From can hold spaces themselves ("Anywhere on
// tailscale0", "22/tcp (OpenSSH)").
var row = regexp.MustCompile(`^(.+?)\s+(ALLOW|DENY|REJECT|LIMIT)(?: (IN|OUT|FWD))?\s+(.+?)\s*(?:#.*)?$`)

// matches reads one line of `ufw status` and says whether it is this
// rule: same action, and the To and From columns exactly as ufw prints
// them for it (see to and from). Exact, not a prefix: "22/tcp" must not
// match "22/tcp on tailscale0", or the narrower rule would hide the
// missing wider one.
//
// Caught against a real host: ufw prints "ALLOW IN" for a rule added
// as a plain port/proto spec, but just "ALLOW" (no direction) for one
// added via the "from X to any port Y" form, so the direction is
// optional. Outbound and routed rules are never this module's.
func (r Rule) matches(line string) bool {
	m := row.FindStringSubmatch(strings.TrimSpace(line))
	if m == nil {
		return false
	}
	to, action, dir, from := m[1], m[2], m[3], m[4]
	if action != r.action() || dir == "OUT" || dir == "FWD" {
		return false
	}
	if r.App != "" {
		return strings.Contains(to, "("+r.App+")")
	}
	return to == r.to() && from == r.from()
}

func (r Rule) action() string {
	if r.Allow {
		return "ALLOW"
	}
	return "DENY"
}

// to is the To column ufw prints for this rule: the port, or Anywhere,
// and the interface. From ufw's backend_iptables.py, get_status.
func (r Rule) to() string {
	to := "Anywhere"
	if r.Port != "" {
		to = r.Port
		if r.Proto != "" {
			to += "/" + r.Proto
		}
	}
	if r.In != "" {
		to += " on " + r.In
	}
	return to
}

func (r Rule) from() string {
	if r.From == "" {
		return "Anywhere"
	}
	return r.From
}

func (r Rule) Diff(desired, current module.State) (module.Change, error) {
	if err := r.check(); err != nil {
		return module.Change{}, err
	}
	cur, ok := current.(ruleState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not ufw.ruleState", current)
	}
	if cur.Exists {
		return module.Change{Action: module.NoOp}, nil
	}
	verb := "deny"
	if r.Allow {
		verb = "allow"
	}
	return module.Change{Action: module.Create, Description: fmt.Sprintf("ufw %s %s", verb, r.describe())}, nil
}

func (r Rule) describe() string {
	if r.App != "" {
		return r.App
	}
	d := r.to()
	if r.From != "" {
		d += " from " + r.From
	}
	return d
}

func (r Rule) Apply(ctx context.Context, run module.Runner, change module.Change) (module.Result, error) {
	if err := r.check(); err != nil {
		return module.Result{}, err
	}
	out, err := run.Run(ctx, r.applyCmd())
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apply ufw rule: %w", err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

func (r Rule) applyCmd() string {
	action := strings.ToLower(r.action())
	if r.App != "" {
		return fmt.Sprintf("ufw %s %s 2>&1", action, shellquote.Quote(r.App))
	}
	if r.From == "" && r.In == "" {
		spec := r.Port
		if r.Proto != "" {
			spec += "/" + r.Proto
		}
		return fmt.Sprintf("ufw %s %s 2>&1", action, shellquote.Quote(spec))
	}
	// The full form: ufw allow [in on IF] [from X] [to any port P [proto T]].
	cmd := "ufw " + action
	if r.In != "" {
		cmd += " in on " + shellquote.Quote(r.In)
	}
	if r.From != "" {
		cmd += " from " + shellquote.Quote(r.From)
	}
	if r.Port != "" {
		cmd += " to any port " + shellquote.Quote(r.Port)
		if r.Proto != "" {
			cmd += " proto " + shellquote.Quote(r.Proto)
		}
	}
	return cmd + " 2>&1"
}

func (r Rule) Verify(ctx context.Context, run module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, r, run, desired)
}
