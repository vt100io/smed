package ufw

import (
	"context"
	"strings"
	"testing"

	"github.com/vt100io/smed/module"
)

// realStatusOutput is `ufw status` captured verbatim from a real host
// (db-t-01) that's actually running the config this package was built
// against — not a hand-constructed fixture. Trailing spaces on some
// lines are real; ufw pads columns.
const realStatusOutput = `Status: active

To                         Action      From
--                         ------      ----
22/tcp (OpenSSH)           ALLOW IN    Anywhere
5432/tcp                   ALLOW IN    10.10.0.0/16
22/tcp (OpenSSH (v6))      ALLOW IN    Anywhere (v6)             `

const realVerboseOutput = `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp (OpenSSH)           ALLOW IN    Anywhere
5432/tcp                   ALLOW IN    10.10.0.0/16
22/tcp (OpenSSH (v6))      ALLOW IN    Anywhere (v6)             `

type scriptedRunner struct{ out string }

func (s scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	return s.out, nil
}

type capturingRunner struct {
	out        string
	gotCommand string
}

func (c *capturingRunner) Run(ctx context.Context, cmd string) (string, error) {
	c.gotCommand = cmd
	return c.out, nil
}

func TestRuleReadFindsExistingAppProfileRule(t *testing.T) {
	r := Rule{Allow: true, App: "OpenSSH"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.(ruleState).Exists {
		t.Fatal("expected Exists=true for the already-present OpenSSH rule")
	}
}

func TestRuleReadFindsExistingPortRule(t *testing.T) {
	r := Rule{Allow: true, Port: "5432", Proto: "tcp", From: "10.10.0.0/16"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.(ruleState).Exists {
		t.Fatal("expected Exists=true for the already-present 5432/tcp rule")
	}
}

func TestRuleReadMissesRuleWithDifferentSource(t *testing.T) {
	// Same port, different CIDR — must not match.
	r := Rule{Allow: true, Port: "5432", Proto: "tcp", From: "192.168.1.0/24"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(ruleState).Exists {
		t.Fatal("expected Exists=false — the real rule is from a different CIDR")
	}
}

func TestRuleReadMissesAbsentRule(t *testing.T) {
	r := Rule{Allow: true, Port: "9999", Proto: "tcp"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(ruleState).Exists {
		t.Fatal("expected Exists=false for a port with no rule at all")
	}
}

func TestRuleReadFindsFromToStyleRuleWithNoDirectionSuffix(t *testing.T) {
	// Captured for real against db-t-01: a rule added via
	// "ufw allow from X to any port Y proto Z" (what Apply uses
	// whenever From is set) displays as plain "ALLOW" with no " IN"
	// suffix — unlike the plain port/proto form, which does print
	// "ALLOW IN". A matcher requiring the literal "ALLOW IN" string
	// would never recognize a rule this module's own Apply had just
	// created — exactly the bug this regression test is named for.
	const out = `Status: active

To                         Action      From
--                         ------      ----
19999/tcp                  ALLOW       192.0.2.0/24              `

	r := Rule{Allow: true, Port: "19999", Proto: "tcp", From: "192.0.2.0/24"}
	got, err := r.Read(context.Background(), scriptedRunner{out: out})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !got.(ruleState).Exists {
		t.Fatal("expected Exists=true — this exact line is how ufw really displays a from/to rule")
	}
}

func TestRuleReadDistinguishesAllowFromDeny(t *testing.T) {
	// The real output only has ALLOW IN rules; a DENY rule for the
	// same port must not match just because the port text matches.
	r := Rule{Allow: false, Port: "5432", Proto: "tcp", From: "10.10.0.0/16"}
	got, err := r.Read(context.Background(), scriptedRunner{out: realStatusOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(ruleState).Exists {
		t.Fatal("expected Exists=false — the real rule is ALLOW, not DENY")
	}
}

func TestRuleDiffNoOpWhenExists(t *testing.T) {
	r := Rule{Allow: true, Port: "5432", Proto: "tcp"}
	change, err := r.Diff(r.Desired(), ruleState{Exists: true})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestRuleDiffCreateWhenMissing(t *testing.T) {
	r := Rule{Allow: true, Port: "5432", Proto: "tcp"}
	change, err := r.Diff(r.Desired(), ruleState{Exists: false})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Create {
		t.Fatalf("Action = %v, want Create", change.Action)
	}
}

func TestRuleApplyBuildsPortRuleWithSource(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: true, Port: "5432", Proto: "tcp", From: "10.10.0.0/16"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	want := "ufw allow from '10.10.0.0/16' to any port '5432' proto 'tcp' 2>&1"
	if r.gotCommand != want {
		t.Fatalf("got command %q, want %q", r.gotCommand, want)
	}
}

func TestRuleApplyBuildsPlainPortRule(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: true, Port: "22", Proto: "tcp"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(r.gotCommand, "ufw allow '22/tcp'") {
		t.Fatalf("got command %q, want it to start with \"ufw allow '22/tcp'\"", r.gotCommand)
	}
}

func TestRuleApplyBuildsAppProfileRule(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: true, App: "OpenSSH"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if r.gotCommand != "ufw allow 'OpenSSH' 2>&1" {
		t.Fatalf("got command %q", r.gotCommand)
	}
}

func TestRuleApplyDeny(t *testing.T) {
	r := &capturingRunner{}
	rule := Rule{Allow: false, Port: "8080", Proto: "tcp"}
	if _, err := rule.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(r.gotCommand, "ufw deny") {
		t.Fatalf("got command %q, want it to start with 'ufw deny'", r.gotCommand)
	}
}

func TestDefaultReadParsesRealVerboseOutput(t *testing.T) {
	d := Default{}
	got, err := d.Read(context.Background(), scriptedRunner{out: realVerboseOutput})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := defaultState{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "allow"}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestDefaultReadParsesInactive(t *testing.T) {
	d := Default{}
	got, err := d.Read(context.Background(), scriptedRunner{out: "Status: inactive\n"})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(defaultState).Enabled {
		t.Fatal("expected Enabled=false for Status: inactive")
	}
}

func TestDefaultDiffNoOpWhenMatching(t *testing.T) {
	d := Default{Enabled: true, IncomingPolicy: "deny"}
	current := defaultState{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "allow"}
	change, err := d.Diff(d.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestDefaultDiffIgnoresUnmanagedOutgoingPolicy(t *testing.T) {
	d := Default{Enabled: true, IncomingPolicy: "deny"} // OutgoingPolicy unmanaged
	current := defaultState{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "reject"}
	change, err := d.Diff(d.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (outgoing policy unmanaged)", change.Action)
	}
}

func TestDefaultApplySetsPolicyBeforeEnabling(t *testing.T) {
	r := &capturingRunner{}
	d := Default{Enabled: true, IncomingPolicy: "deny", OutgoingPolicy: "allow"}
	if _, err := d.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	policyIdx := strings.Index(r.gotCommand, "ufw default 'deny' incoming")
	enableIdx := strings.Index(r.gotCommand, "ufw --force enable")
	if policyIdx == -1 || enableIdx == -1 || policyIdx > enableIdx {
		t.Fatalf("got command %q, want policy set before enabling", r.gotCommand)
	}
}

func TestDefaultApplyDisableUsesForce(t *testing.T) {
	r := &capturingRunner{}
	d := Default{Enabled: false}
	if _, err := d.Apply(context.Background(), r, module.Change{}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.Contains(r.gotCommand, "ufw --force disable") {
		t.Fatalf("got command %q, want 'ufw --force disable'", r.gotCommand)
	}
}

// Rows as ufw prints them: built with its own "%-26s %-12s%-26s" from
// backend_iptables.py. A plain inbound rule's direction is blank, so the
// action is "ALLOW ".
const interfaceStatus = `Status: active

To                         Action      From
--                         ------      ----
Anywhere on tailscale0     ALLOW       Anywhere                  
Anywhere                   ALLOW       100.64.0.0/10             
5432 on tailscale0         DENY        10.0.0.0/8                
22/tcp on tailscale0       ALLOW       Anywhere                  
Anywhere (v6) on tailscale0 ALLOW       Anywhere (v6)             
80/tcp                     ALLOW OUT   Anywhere                   (out)`

func exists(t *testing.T, r Rule, status string) bool {
	t.Helper()
	got, err := r.Read(context.Background(), scriptedRunner{out: status})
	if err != nil {
		t.Fatal(err)
	}
	return got.(ruleState).Exists
}

func TestRuleReadsInterfaceAndPortlessRules(t *testing.T) {
	for _, c := range []struct {
		r    Rule
		want bool
	}{
		{Rule{Allow: true, In: "tailscale0"}, true},
		{Rule{Allow: true, From: "100.64.0.0/10"}, true},
		{Rule{Allow: false, In: "tailscale0", Port: "5432", From: "10.0.0.0/8"}, true},
		{Rule{Allow: true, In: "tailscale0", Port: "22", Proto: "tcp"}, true},
		{Rule{Allow: true, In: "eth0"}, false},
		{Rule{Allow: true, From: "10.0.0.0/8"}, false},
		// The prefix bug: the rule over Tailscale only is not the one for
		// every interface.
		{Rule{Allow: true, Port: "22", Proto: "tcp"}, false},
		// Nor is an outbound rule an inbound one.
		{Rule{Allow: true, Port: "80", Proto: "tcp"}, false},
	} {
		if got := exists(t, c.r, interfaceStatus); got != c.want {
			t.Errorf("%+v: exists %v, want %v", c.r, got, c.want)
		}
	}
}

func TestRuleCommands(t *testing.T) {
	for _, c := range []struct {
		r    Rule
		want string
	}{
		// The finding: From without Port made "to any port ''".
		{Rule{Allow: true, From: "100.64.0.0/10"}, "ufw allow from '100.64.0.0/10' 2>&1"},
		{Rule{Allow: true, In: "tailscale0"}, "ufw allow in on 'tailscale0' 2>&1"},
		{Rule{Allow: true, In: "tailscale0", Port: "22", Proto: "tcp"}, "ufw allow in on 'tailscale0' to any port '22' proto 'tcp' 2>&1"},
		{Rule{Allow: false, In: "tailscale0", From: "10.0.0.0/8", Port: "5432"}, "ufw deny in on 'tailscale0' from '10.0.0.0/8' to any port '5432' 2>&1"},
		{Rule{Allow: true, Port: "22", Proto: "tcp"}, "ufw allow '22/tcp' 2>&1"},
	} {
		run := &capturingRunner{}
		if _, err := c.r.Apply(context.Background(), run, module.Change{}); err != nil {
			t.Fatal(err)
		}
		if run.gotCommand != c.want {
			t.Errorf("%+v ran %q, want %q", c.r, run.gotCommand, c.want)
		}
	}
}

func TestRuleChecksAtPlan(t *testing.T) {
	for _, r := range []Rule{
		{Allow: true},                                  // ufw allow, alone
		{Allow: true, Proto: "tcp"},                    // a protocol of what?
		{Allow: true, App: "OpenSSH", From: "1.2.3.4"}, // used to drop From silently
	} {
		if _, err := r.Diff(r.Desired(), ruleState{}); err == nil {
			t.Errorf("%+v accepted", r)
		}
		run := &capturingRunner{}
		if _, err := r.Apply(context.Background(), run, module.Change{}); err == nil || run.gotCommand != "" {
			t.Errorf("%+v applied: %q", r, run.gotCommand)
		}
	}
}

func TestRuleDescribe(t *testing.T) {
	r := Rule{Allow: true, In: "tailscale0", From: "100.64.0.0/10"}
	change, err := r.Diff(r.Desired(), ruleState{})
	if err != nil || change.Description != "ufw allow Anywhere on tailscale0 from 100.64.0.0/10" {
		t.Errorf("%+v %v", change, err)
	}
}
