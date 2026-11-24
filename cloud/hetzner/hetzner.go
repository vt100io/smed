// Package hetzner declares Hetzner Cloud servers and creates the missing
// ones, the smed way: read what exists, show a plan, then apply it.
//
// There is no state file. Hetzner's API is the only source of truth, and
// smed only ever looks at servers and firewalls carrying its label
// (smed=<Account.Project>), so servers made by hand or by other tools are
// never touched.
//
// A server's first boot can join it to the tailnet (JoinTailnet). From then
// on it's an ordinary smed host: smed.Host(name) reaches it over Tailscale.
//
// This layer sits before hosts, not among the modules: a module reads and
// changes a machine through a Runner, and a machine being created has
// nothing to run commands on yet.
package hetzner

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/vt100io/smed/tailnet"
)

// DefaultBaseURL is the Hetzner Cloud API.
const DefaultBaseURL = "https://api.hetzner.cloud/v1"

const (
	labelProject = "smed"        // on servers and firewalls: Account.Project
	labelServer  = "smed-server" // on firewalls: the server it guards
)

// Account is a Hetzner Cloud project and how smed labels what it creates
// there.
type Account struct {
	// Token returns the API token, for example secret.Lazy(op, "op://…").
	Token func(context.Context) (string, error)

	// Project is the label value smed puts on everything it creates
	// (smed=<Project>), and the only thing it will ever look at or change.
	Project string

	// PruneUndeclared deletes servers labelled with Project that are no
	// longer declared. Off by default: smed never deletes a server unless
	// asked to.
	PruneUndeclared bool

	// Tailnet, if set, makes Apply wait until each new server that joins
	// the tailnet shows up online there, so the next smed.Host step can
	// reach it.
	Tailnet tailnet.Tailnet

	BaseURL      string        // "" means DefaultBaseURL
	HTTPClient   *http.Client  // nil means a client with a 30s timeout
	PollInterval time.Duration // 0 means 2s
	Timeout      time.Duration // per created server; 0 means 10 minutes
}

// ServerSpec is one declared server. Build it with Server and its setters.
type ServerSpec struct {
	name       string
	serverType string
	location   string
	image      string
	sshKeys    []Key
	ports      []int
	authKey    func(context.Context) (string, error)
	tags       []string
}

// Server declares a server called name. Type, Location and Image are
// required.
func Server(name string) *ServerSpec { return &ServerSpec{name: name} }

// Type sets the server type, for example "cx22".
func (s *ServerSpec) Type(t string) *ServerSpec { s.serverType = t; return s }

// Location sets the location, for example "hel1".
func (s *ServerSpec) Location(l string) *ServerSpec { s.location = l; return s }

// Image sets the image, for example "debian-13".
func (s *ServerSpec) Image(i string) *ServerSpec { s.image = i; return s }

// Key is a public SSH key in the Hetzner project, uploaded if it's missing.
type Key struct {
	Name      string // its name in the project
	PublicKey string // the line from a .pub file, e.g. "ssh-ed25519 AAAA… me@example.com"
}

// SSHKeys gives root these public keys when the server is created. With a
// key, Hetzner doesn't email a root password, and its rescue system lets
// you in with the key if Tailscale ever doesn't. Keys only apply at
// creation; an existing server keeps what it was created with.
func (s *ServerSpec) SSHKeys(keys ...Key) *ServerSpec { s.sshKeys = keys; return s }

// OpenPorts lists the TCP ports the server's firewall lets in from the
// internet. Everything else is closed, SSH included: with JoinTailnet, SSH
// goes over Tailscale. A server without OpenPorts gets a firewall that
// lets nothing in.
func (s *ServerSpec) OpenPorts(ports ...int) *ServerSpec { s.ports = ports; return s }

// JoinTailnet makes the server's first boot install Tailscale and join the
// tailnet with authKey, as its own hostname, with Tailscale SSH and the
// given tags ("tag:web"). The key is read only when the server is created.
// It stays readable to root on the server, so use a single-use key with a
// short expiry.
func (s *ServerSpec) JoinTailnet(authKey func(context.Context) (string, error), tags ...string) *ServerSpec {
	s.authKey, s.tags = authKey, tags
	return s
}

func (s *ServerSpec) validate() error {
	var missing []string
	for _, f := range []struct{ name, value string }{{"Type", s.serverType}, {"Location", s.location}, {"Image", s.image}} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if s.name == "" {
		return errors.New("hetzner: a server needs a name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("hetzner: server %s: missing %s", s.name, strings.Join(missing, ", "))
	}
	for _, p := range s.ports {
		if p < 1 || p > 65535 {
			return fmt.Errorf("hetzner: server %s: port %d out of range", s.name, p)
		}
	}
	for _, k := range s.sshKeys {
		if k.Name == "" {
			return fmt.Errorf("hetzner: server %s: an SSH key needs a name", s.name)
		}
		if _, err := keyBody(k.PublicKey); err != nil {
			return fmt.Errorf("hetzner: server %s: SSH key %s: %w", s.name, k.Name, err)
		}
	}
	for _, t := range s.tags {
		if !strings.HasPrefix(t, "tag:") {
			return fmt.Errorf("hetzner: server %s: tailnet tag %q must start with tag:", s.name, t)
		}
	}
	return nil
}

func (s *ServerSpec) describe() string {
	return fmt.Sprintf("%s, %s, %s", s.serverType, s.location, s.image)
}

// Action is what Apply would do to one server.
type Action string

const (
	Unchanged Action = "unchanged"
	Create    Action = "create"
	Update    Action = "update" // only the firewall's open ports change
	Drift     Action = "drift"  // differs from its declaration; Apply leaves it
	Delete    Action = "delete" // only with PruneUndeclared
)

// Change is one line of a plan.
type Change struct {
	Action Action
	Server string
	Detail string

	spec     *ServerSpec
	serverID int64
	firewall *apiFirewall // the server's smed firewall, if it has one
}

// Plan is what Apply would do, one Change per declared or pruned server,
// in declaration order.
type Plan []Change

// Pending reports whether Apply would change anything. Drift doesn't
// count: Apply never acts on it.
func (p Plan) Pending() bool {
	for _, c := range p {
		if c.Action == Create || c.Action == Update || c.Action == Delete {
			return true
		}
	}
	return false
}

func (p Plan) String() string {
	var b strings.Builder
	for _, c := range p {
		mark := map[Action]string{Unchanged: " ", Create: "+", Update: "~", Drift: "!", Delete: "-"}[c.Action]
		fmt.Fprintf(&b, "%s %s %s", mark, c.Action, c.Server)
		if c.Detail != "" {
			b.WriteString(": " + c.Detail)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// Plan reads the project's servers and firewalls and compares them with
// specs. It changes nothing.
func (a Account) Plan(ctx context.Context, specs ...*ServerSpec) (Plan, error) {
	c, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	return a.plan(ctx, c, specs)
}

func (a Account) plan(ctx context.Context, c *client, specs []*ServerSpec) (Plan, error) {
	if a.Project == "" {
		return nil, errors.New("hetzner: Account.Project is required")
	}
	seen := map[string]bool{}
	for _, s := range specs {
		if err := s.validate(); err != nil {
			return nil, err
		}
		if seen[s.name] {
			return nil, fmt.Errorf("hetzner: server %s declared twice", s.name)
		}
		seen[s.name] = true
	}

	selector := url.Values{"label_selector": {labelProject + "=" + a.Project}}
	servers, err := c.listServers(ctx, selector)
	if err != nil {
		return nil, err
	}
	firewalls, err := c.listFirewalls(ctx, selector)
	if err != nil {
		return nil, err
	}
	byName := map[string]apiServer{}
	for _, s := range servers {
		byName[s.Name] = s
	}
	firewallOf := map[string]*apiFirewall{}
	for i, f := range firewalls {
		firewallOf[f.Labels[labelServer]] = &firewalls[i]
	}

	var plan Plan
	for _, spec := range specs {
		fw := firewallOf[spec.name]
		server, exists := byName[spec.name]
		if !exists {
			// A server of that name smed didn't create would make the
			// create fail; say so now, in the plan.
			others, err := c.listServers(ctx, url.Values{"name": {spec.name}})
			if err != nil {
				return nil, err
			}
			if len(others) > 0 {
				return nil, fmt.Errorf("hetzner: server %s exists but isn't smed's (no %s=%s label); rename one of them", spec.name, labelProject, a.Project)
			}
			price, err := checkNew(ctx, c, spec)
			if err != nil {
				return nil, err
			}
			keys, err := checkKeys(ctx, c, spec.sshKeys)
			if err != nil {
				return nil, err
			}
			detail := spec.describe() + ", " + price + ", " + portsDetail(spec.ports) + keys
			if spec.authKey != nil {
				detail += ", joins the tailnet"
				if len(spec.tags) > 0 {
					detail += " as " + strings.Join(spec.tags, ",")
				}
			}
			plan = append(plan, Change{Action: Create, Server: spec.name, Detail: detail, spec: spec, firewall: fw})
			continue
		}

		change := Change{Action: Unchanged, Server: spec.name, Detail: server.PublicNet.IPv4.IP, spec: spec, serverID: server.ID, firewall: fw}
		if d := drift(spec, server); d != "" {
			change.Action, change.Detail = Drift, d+" (recreate it yourself; smed won't)"
		} else if have, want := firewallPorts(fw), spec.ports; fw == nil || !samePorts(have, want) {
			change.Action = Update
			if fw == nil {
				change.Detail = "add firewall, " + portsDetail(want)
			} else {
				change.Detail = "firewall " + portsDetail(have) + " → " + portsDetail(want)
			}
		}
		plan = append(plan, change)
	}

	if a.PruneUndeclared {
		for _, s := range servers {
			if !seen[s.Name] {
				plan = append(plan, Change{Action: Delete, Server: s.Name, Detail: "no longer declared", serverID: s.ID, firewall: firewallOf[s.Name]})
			}
		}
	}
	return plan, nil
}

// checkNew confirms with Hetzner that spec's type, location and image
// exist and fit together, so a typo fails the plan instead of the create.
// It returns the type's monthly price in that location.
func checkNew(ctx context.Context, c *client, spec *ServerSpec) (string, error) {
	fail := func(format string, args ...any) (string, error) {
		return "", fmt.Errorf("hetzner: server %s: "+format, append([]any{spec.name}, args...)...)
	}
	ok, err := c.locationExists(ctx, spec.location)
	if err != nil {
		return "", err
	}
	if !ok {
		return fail("there is no location %q", spec.location)
	}
	st, err := c.serverType(ctx, spec.serverType)
	if err != nil {
		return "", err
	}
	if st == nil {
		return fail("there is no server type %q", spec.serverType)
	}
	offeredHere := false
	var offered []string
	for _, l := range st.Locations {
		offered = append(offered, l.Name)
		if l.Name != spec.location {
			continue
		}
		offeredHere = true
		if l.Deprecation != nil {
			return fail("server type %s is deprecated in %s and can't be ordered after %s", st.Name, l.Name, l.Deprecation.date())
		}
		if !l.Available {
			return fail("server type %s is sold out in %s right now", st.Name, l.Name)
		}
	}
	price := ""
	for _, p := range st.Prices {
		if p.Location == spec.location {
			price = monthly(p.PriceMonthly.Gross)
		}
	}
	if !offeredHere || price == "" {
		return fail("server type %s isn't offered in %s (it is in %s)", st.Name, spec.location, strings.Join(offered, ", "))
	}
	img, err := c.systemImage(ctx, spec.image, st.Architecture)
	if err != nil {
		return "", err
	}
	if img == nil {
		return fail("there is no %s image %q", st.Architecture, spec.image)
	}
	if img.Deprecation != nil {
		return fail("image %s is deprecated and can't be used after %s", spec.image, img.Deprecation.date())
	}
	return price, nil
}

// checkKeys confirms that each key either exists in the project with the
// same public key or can be uploaded, and describes that for the plan.
func checkKeys(ctx context.Context, c *client, keys []Key) (string, error) {
	if len(keys) == 0 {
		return ", no SSH key (Hetzner emails a root password)", nil
	}
	var parts []string
	for _, k := range keys {
		existing, err := c.sshKey(ctx, k.Name)
		if err != nil {
			return "", err
		}
		switch {
		case existing == nil:
			parts = append(parts, k.Name+" (uploaded)")
		case !sameKey(existing.PublicKey, k.PublicKey):
			return "", fmt.Errorf("hetzner: SSH key %s already exists in the project with a different public key", k.Name)
		default:
			parts = append(parts, k.Name)
		}
	}
	return ", SSH key " + strings.Join(parts, ", "), nil
}

// keyBody returns the type and key of a public key line, without the
// comment.
func keyBody(line string) (string, error) {
	f := strings.Fields(line)
	if len(f) < 2 || !(strings.HasPrefix(f[0], "ssh-") || strings.HasPrefix(f[0], "ecdsa-")) {
		return "", errors.New("not a public key line like \"ssh-ed25519 AAAA… comment\"")
	}
	return f[0] + " " + f[1], nil
}

func sameKey(a, b string) bool {
	ka, errA := keyBody(a)
	kb, errB := keyBody(b)
	return errA == nil && errB == nil && ka == kb
}

// monthly formats Hetzner's gross monthly price ("7.5900000000") as
// "€7.59/month". Hetzner bills in euros.
func monthly(gross string) string {
	f, err := strconv.ParseFloat(gross, 64)
	if err != nil {
		return gross + "/month"
	}
	return fmt.Sprintf("€%.2f/month", f)
}

// drift describes how server differs from spec in ways smed won't fix,
// because fixing them means recreating the server. "" means no drift.
func drift(spec *ServerSpec, server apiServer) string {
	var diffs []string
	if server.ServerType.Name != spec.serverType {
		diffs = append(diffs, "type "+server.ServerType.Name+", declared "+spec.serverType)
	}
	if loc := server.location(); loc != spec.location {
		diffs = append(diffs, "location "+loc+", declared "+spec.location)
	}
	if server.Image != nil && server.Image.Name != spec.image {
		diffs = append(diffs, "image "+server.Image.Name+", declared "+spec.image)
	}
	return strings.Join(diffs, "; ")
}

// Apply plans, then creates missing servers, updates firewalls and, with
// PruneUndeclared, deletes undeclared servers. It stops at the first
// error. Afterwards it plans again and fails if anything is still pending,
// so a reported success means Hetzner matches the declarations.
func (a Account) Apply(ctx context.Context, specs ...*ServerSpec) (Plan, error) {
	c, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	plan, err := a.plan(ctx, c, specs)
	if err != nil {
		return nil, err
	}
	for _, ch := range plan {
		switch ch.Action {
		case Create:
			err = a.create(ctx, c, ch)
		case Update:
			err = a.updateFirewall(ctx, c, ch)
		case Delete:
			err = a.delete(ctx, c, ch)
		}
		if err != nil {
			return plan, fmt.Errorf("%s %s: %w", ch.Action, ch.Server, err)
		}
	}

	after, err := a.plan(ctx, c, specs)
	if err != nil {
		return plan, fmt.Errorf("verifying: %w", err)
	}
	if after.Pending() {
		return plan, fmt.Errorf("hetzner: still pending after apply:\n%s", after)
	}
	return plan, nil
}

func (a Account) create(ctx context.Context, c *client, ch Change) error {
	spec := ch.spec
	ctx, cancel := context.WithTimeout(ctx, a.timeout())
	defer cancel()

	// The firewall first, so the server is never reachable on a closed
	// port, not even while booting. A firewall left over from a failed
	// attempt is reused.
	fw := ch.firewall
	if fw == nil {
		var err error
		if fw, err = a.createFirewall(ctx, c, spec, 0); err != nil {
			return err
		}
	} else if !samePorts(firewallPorts(fw), spec.ports) {
		if err := a.setRules(ctx, c, fw.ID, spec.ports); err != nil {
			return err
		}
	}

	// Keys are uploaded before the server that needs them.
	for _, k := range spec.sshKeys {
		existing, err := c.sshKey(ctx, k.Name)
		if err != nil {
			return err
		}
		if existing == nil {
			body := map[string]any{"name": k.Name, "public_key": k.PublicKey, "labels": map[string]string{labelProject: a.Project}}
			if err := c.do(ctx, http.MethodPost, "/ssh_keys", body, nil); err != nil {
				return err
			}
		}
	}

	authKey := ""
	if spec.authKey != nil {
		var err error
		if authKey, err = spec.authKey(ctx); err != nil {
			return fmt.Errorf("reading the tailnet auth key: %w", err)
		}
	}
	body := map[string]any{
		"name":               spec.name,
		"server_type":        spec.serverType,
		"location":           spec.location,
		"image":              spec.image,
		"labels":             map[string]string{labelProject: a.Project},
		"firewalls":          []apiFirewallRef{{Firewall: fw.ID}},
		"user_data":          cloudInit(spec.name, authKey, spec.tags),
		"start_after_create": true,
	}
	if len(spec.sshKeys) > 0 {
		var names []string
		for _, k := range spec.sshKeys {
			names = append(names, k.Name)
		}
		body["ssh_keys"] = names
	}
	var resp struct {
		Server      apiServer   `json:"server"`
		Action      apiAction   `json:"action"`
		NextActions []apiAction `json:"next_actions"`
	}
	if err := c.do(ctx, http.MethodPost, "/servers", body, &resp); err != nil {
		return err
	}
	if err := c.waitAll(ctx, append([]apiAction{resp.Action}, resp.NextActions...), a.poll()); err != nil {
		return err
	}
	if a.Tailnet != nil && spec.authKey != nil {
		return a.waitOnline(ctx, spec.name)
	}
	return nil
}

// waitOnline waits for name to appear online in the tailnet.
func (a Account) waitOnline(ctx context.Context, name string) error {
	for {
		peers, err := a.Tailnet.Peers(ctx)
		if err != nil {
			return fmt.Errorf("listing tailnet peers: %w", err)
		}
		for _, p := range peers {
			if p.Hostname == name && p.Online {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("server %s was created but hasn't come online in the tailnet: %w (if a machine of that name already existed, Tailscale may have named it %s-1)", name, ctx.Err(), name)
		case <-time.After(a.poll()):
		}
	}
}

func (a Account) updateFirewall(ctx context.Context, c *client, ch Change) error {
	if ch.firewall == nil {
		_, err := a.createFirewall(ctx, c, ch.spec, ch.serverID)
		return err
	}
	return a.setRules(ctx, c, ch.firewall.ID, ch.spec.ports)
}

// createFirewall creates the firewall for spec, applied to serverID if it
// isn't 0.
func (a Account) createFirewall(ctx context.Context, c *client, spec *ServerSpec, serverID int64) (*apiFirewall, error) {
	body := map[string]any{
		"name":   "smed-" + spec.name,
		"labels": map[string]string{labelProject: a.Project, labelServer: spec.name},
		"rules":  rules(spec.ports),
	}
	if serverID != 0 {
		to := apiApplyTo{Type: "server"}
		to.Server.ID = serverID
		body["apply_to"] = []apiApplyTo{to}
	}
	var resp struct {
		Firewall apiFirewall `json:"firewall"`
		Actions  []apiAction `json:"actions"`
	}
	if err := c.do(ctx, http.MethodPost, "/firewalls", body, &resp); err != nil {
		return nil, err
	}
	return &resp.Firewall, c.waitAll(ctx, resp.Actions, a.poll())
}

func (a Account) setRules(ctx context.Context, c *client, firewallID int64, ports []int) error {
	var resp struct {
		Actions []apiAction `json:"actions"`
	}
	path := "/firewalls/" + strconv.FormatInt(firewallID, 10) + "/actions/set_rules"
	if err := c.do(ctx, http.MethodPost, path, map[string]any{"rules": rules(ports)}, &resp); err != nil {
		return err
	}
	return c.waitAll(ctx, resp.Actions, a.poll())
}

func (a Account) delete(ctx context.Context, c *client, ch Change) error {
	var resp struct {
		Action apiAction `json:"action"`
	}
	if err := c.do(ctx, http.MethodDelete, "/servers/"+strconv.FormatInt(ch.serverID, 10), nil, &resp); err != nil {
		return err
	}
	if err := c.wait(ctx, resp.Action.ID, a.poll()); err != nil {
		return err
	}
	// The server is gone, so its firewall is no longer applied anywhere.
	if ch.firewall != nil {
		return c.do(ctx, http.MethodDelete, "/firewalls/"+strconv.FormatInt(ch.firewall.ID, 10), nil, nil)
	}
	return nil
}

func (a Account) client(ctx context.Context) (*client, error) {
	if a.Token == nil {
		return nil, errors.New("hetzner: Account.Token is required")
	}
	token, err := a.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("hetzner: reading the API token: %w", err)
	}
	base := a.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	hc := a.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &client{base: strings.TrimSuffix(base, "/"), http: hc, token: token}, nil
}

func (a Account) poll() time.Duration {
	if a.PollInterval > 0 {
		return a.PollInterval
	}
	return 2 * time.Second
}

func (a Account) timeout() time.Duration {
	if a.Timeout > 0 {
		return a.Timeout
	}
	return 10 * time.Minute
}

// rules lets the given TCP ports in from anywhere, IPv4 and IPv6.
func rules(ports []int) []apiRule {
	out := []apiRule{}
	for _, p := range sortedPorts(ports) {
		out = append(out, apiRule{Direction: "in", Protocol: "tcp", Port: strconv.Itoa(p), SourceIPs: []string{"0.0.0.0/0", "::/0"}})
	}
	return out
}

// firewallPorts reads back the inbound TCP ports of an smed firewall.
func firewallPorts(fw *apiFirewall) []int {
	if fw == nil {
		return nil
	}
	var ports []int
	for _, r := range fw.Rules {
		if r.Direction != "in" || r.Protocol != "tcp" {
			continue
		}
		if p, err := strconv.Atoi(r.Port); err == nil {
			ports = append(ports, p)
		}
	}
	return ports
}

func samePorts(a, b []int) bool { return slices.Equal(sortedPorts(a), sortedPorts(b)) }

func sortedPorts(ports []int) []int {
	out := slices.Clone(ports)
	slices.Sort(out)
	return slices.Compact(out)
}

func portsDetail(ports []int) string {
	if len(ports) == 0 {
		return "no ports open"
	}
	var s []string
	for _, p := range sortedPorts(ports) {
		s = append(s, strconv.Itoa(p))
	}
	return "ports " + strings.Join(s, ", ") + " open"
}
