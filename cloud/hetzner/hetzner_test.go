package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vt100io/smed/tailnet"
)

// fakeHetzner is an in-memory Hetzner Cloud API: just the endpoints this
// package uses, with pages of two and actions that run for one poll.
type fakeHetzner struct {
	mu        sync.Mutex
	nextID    int64
	servers   map[int64]*apiServer
	firewalls map[int64]*apiFirewall
	actions   map[int64]int // polls left before success
	created   map[string]map[string]any
	applied   map[int64][]int64 // firewall ID -> server IDs
	sshKeys   map[string]string // name -> public key
	writes    []string          // "POST /servers", "DELETE /firewalls/3", …
}

const testToken = "test-token"

func newFake(t *testing.T) (*fakeHetzner, Account) {
	t.Helper()
	f := &fakeHetzner{
		nextID:    100,
		servers:   map[int64]*apiServer{},
		firewalls: map[int64]*apiFirewall{},
		actions:   map[int64]int{},
		created:   map[string]map[string]any{},
		applied:   map[int64][]int64{},
		sshKeys:   map[string]string{},
	}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, Account{
		Token:        func(context.Context) (string, error) { return testToken, nil },
		Project:      "vt100",
		BaseURL:      srv.URL,
		PollInterval: time.Millisecond,
		Timeout:      5 * time.Second,
	}
}

func (f *fakeHetzner) id() int64 { f.nextID++; return f.nextID }

func (f *fakeHetzner) action() apiAction {
	a := apiAction{ID: f.id(), Status: "running"}
	f.actions[a.ID] = 1
	return a
}

// addServer puts a server in the fake as if created earlier, by smed if
// labels say so.
func (f *fakeHetzner) addServer(name, serverType, location, image string, labels map[string]string) *apiServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &apiServer{ID: f.id(), Name: name, Status: "running", Labels: labels}
	s.ServerType.Name = serverType
	s.Location.Name = location // the current API; older ones used datacenter.location
	s.Image = &struct {
		Name string `json:"name"`
	}{Name: image}
	s.PublicNet.IPv4.IP = "203.0.113." + strconv.FormatInt(s.ID%250, 10)
	f.servers[s.ID] = s
	return s
}

func (f *fakeHetzner) addFirewall(server string, ports ...int) *apiFirewall {
	f.mu.Lock()
	defer f.mu.Unlock()
	fw := &apiFirewall{ID: f.id(), Name: "smed-" + server, Labels: map[string]string{labelProject: "vt100", labelServer: server}, Rules: rules(ports)}
	f.firewalls[fw.ID] = fw
	return fw
}

func matches(labels map[string]string, selector string) bool {
	if selector == "" {
		return true
	}
	k, v, _ := strings.Cut(selector, "=")
	return labels[k] == v
}

func page[T any](items []T, r *http.Request) ([]T, *int) {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	p = max(p, 1)
	start := min((p-1)*2, len(items))
	end := min(start+2, len(items))
	if end < len(items) {
		next := p + 1
		return items[start:end], &next
	}
	return items[start:end], nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (f *fakeHetzner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testToken {
		apiError(w, 401, "unauthorized", "unable to authenticate")
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	id := int64(0)
	if len(parts) > 1 {
		id, _ = strconv.ParseInt(parts[1], 10, 64)
	}
	q := r.URL.Query()

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/servers":
		var list []apiServer
		for _, s := range f.sorted() {
			if matches(s.Labels, q.Get("label_selector")) && (q.Get("name") == "" || s.Name == q.Get("name")) {
				list = append(list, *s)
			}
		}
		items, next := page(list, r)
		writeJSON(w, 200, map[string]any{"servers": items, "meta": map[string]any{"pagination": map[string]any{"next_page": next}}})

	case r.Method == http.MethodPost && r.URL.Path == "/servers":
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		name := body["name"].(string)
		for _, s := range f.servers {
			if s.Name == name {
				apiError(w, 409, "uniqueness_error", "server name is already used")
				return
			}
		}
		f.created[name] = body
		labels := map[string]string{}
		for k, v := range body["labels"].(map[string]any) {
			labels[k] = v.(string)
		}
		f.mu.Unlock()
		s := f.addServer(name, body["server_type"].(string), body["location"].(string), body["image"].(string), labels)
		f.mu.Lock()
		for _, ref := range body["firewalls"].([]any) {
			fwID := int64(ref.(map[string]any)["firewall"].(float64))
			f.applied[fwID] = append(f.applied[fwID], s.ID)
		}
		writeJSON(w, 201, map[string]any{"server": s, "action": f.action(), "next_actions": []apiAction{f.action()}})

	case r.Method == http.MethodDelete && parts[0] == "servers":
		delete(f.servers, id)
		writeJSON(w, 200, map[string]any{"action": f.action()})

	case r.Method == http.MethodGet && parts[0] == "actions":
		left, ok := f.actions[id]
		if !ok {
			apiError(w, 404, "not_found", "action not found")
			return
		}
		status := "success"
		if left > 0 {
			status = "running"
			f.actions[id] = left - 1
		}
		writeJSON(w, 200, map[string]any{"action": apiAction{ID: id, Status: status}})

	case r.Method == http.MethodGet && r.URL.Path == "/firewalls":
		var list []apiFirewall
		for _, fw := range f.firewalls {
			if matches(fw.Labels, q.Get("label_selector")) {
				list = append(list, *fw)
			}
		}
		slices.SortFunc(list, func(a, b apiFirewall) int { return int(a.ID - b.ID) })
		items, next := page(list, r)
		writeJSON(w, 200, map[string]any{"firewalls": items, "meta": map[string]any{"pagination": map[string]any{"next_page": next}}})

	case r.Method == http.MethodPost && r.URL.Path == "/firewalls":
		var body struct {
			Name    string            `json:"name"`
			Labels  map[string]string `json:"labels"`
			Rules   []apiRule         `json:"rules"`
			ApplyTo []apiApplyTo      `json:"apply_to"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		fw := &apiFirewall{ID: f.id(), Name: body.Name, Labels: body.Labels, Rules: body.Rules}
		f.firewalls[fw.ID] = fw
		for _, to := range body.ApplyTo {
			f.applied[fw.ID] = append(f.applied[fw.ID], to.Server.ID)
		}
		writeJSON(w, 201, map[string]any{"firewall": fw, "actions": []apiAction{f.action()}})

	case r.Method == http.MethodPost && len(parts) == 4 && parts[0] == "firewalls" && parts[3] == "set_rules":
		var body struct {
			Rules []apiRule `json:"rules"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.firewalls[id].Rules = body.Rules
		writeJSON(w, 201, map[string]any{"actions": []apiAction{f.action()}})

	case r.Method == http.MethodDelete && parts[0] == "firewalls":
		delete(f.firewalls, id)
		w.WriteHeader(204)

	case r.Method == http.MethodGet && r.URL.Path == "/ssh_keys":
		var list []apiSSHKey
		if k, ok := f.sshKeys[q.Get("name")]; ok {
			list = append(list, apiSSHKey{Name: q.Get("name"), PublicKey: k})
		}
		writeJSON(w, 200, map[string]any{"ssh_keys": list})

	case r.Method == http.MethodPost && r.URL.Path == "/ssh_keys":
		var body apiSSHKey
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := f.sshKeys[body.Name]; ok {
			apiError(w, 409, "uniqueness_error", "SSH key with the same name already exists")
			return
		}
		f.sshKeys[body.Name] = body.PublicKey
		writeJSON(w, 201, map[string]any{"ssh_key": body})

	case r.Method == http.MethodGet && r.URL.Path == "/locations":
		var list []map[string]string
		for _, l := range []string{"hel1", "fsn1"} {
			if q.Get("name") == l {
				list = append(list, map[string]string{"name": l})
			}
		}
		writeJSON(w, 200, map[string]any{"locations": list})

	case r.Method == http.MethodGet && r.URL.Path == "/server_types":
		var list []any
		for _, st := range fakeServerTypes {
			if st["name"] == q.Get("name") {
				list = append(list, st)
			}
		}
		writeJSON(w, 200, map[string]any{"server_types": list})

	case r.Method == http.MethodGet && r.URL.Path == "/images":
		var list []any
		for _, img := range fakeImages {
			if img["name"] == q.Get("name") && img["architecture"] == q.Get("architecture") && q.Get("type") == "system" {
				list = append(list, img)
			}
		}
		writeJSON(w, 200, map[string]any{"images": list})

	default:
		apiError(w, 404, "not_found", r.Method+" "+r.URL.Path)
	}
}

func price(location, gross string) map[string]any {
	return map[string]any{"location": location, "price_monthly": map[string]string{"gross": gross}}
}

var fakeServerTypes = []map[string]any{
	{"name": "cx22", "architecture": "x86", "deprecated": false, "prices": []any{price("hel1", "4.5900000000"), price("fsn1", "4.5900000000")}},
	{"name": "cx32", "architecture": "x86", "deprecated": false, "prices": []any{price("hel1", "7.5900000000")}},
	{"name": "cpx32", "architecture": "x86", "deprecated": false, "prices": []any{price("fsn1", "13.9900000000")}},
	{"name": "cax11", "architecture": "arm", "deprecated": false, "prices": []any{price("hel1", "4.4900000000")}},
	{"name": "cx11", "architecture": "x86", "deprecated": true, "prices": []any{price("hel1", "4.0000000000")}},
}

var fakeImages = []map[string]any{
	{"name": "debian-13", "architecture": "x86", "deprecated": nil},
	{"name": "debian-13", "architecture": "arm", "deprecated": nil},
	{"name": "ubuntu-24.04", "architecture": "x86", "deprecated": nil},
	{"name": "debian-11", "architecture": "x86", "deprecated": "2026-08-01T00:00:00Z"},
	{"name": "fedora-42", "architecture": "x86", "deprecated": nil},
}

func (f *fakeHetzner) sorted() []*apiServer {
	var list []*apiServer
	for _, s := range f.servers {
		list = append(list, s)
	}
	slices.SortFunc(list, func(a, b *apiServer) int { return int(a.ID - b.ID) })
	return list
}

func (f *fakeHetzner) serverNamed(name string) *apiServer {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.servers {
		if s.Name == name {
			return s
		}
	}
	return nil
}

// fakeTailnet reports a peer online once it has been asked a few times,
// like a server finishing its first boot.
type fakeTailnet struct {
	mu       sync.Mutex
	name     string
	askedFor int
}

func (n *fakeTailnet) Peers(context.Context) ([]tailnet.Peer, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.askedFor++
	return []tailnet.Peer{{Hostname: n.name, Online: n.askedFor >= 3}}, nil
}
func (n *fakeTailnet) Self(context.Context) (tailnet.Peer, error) { return tailnet.Peer{}, nil }
func (n *fakeTailnet) Dial(context.Context, tailnet.Peer, string) (net.Conn, error) {
	return nil, errors.New("not in tests")
}

const testPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyMaterialForTestsOnly0123456789ab stian@hegglund.com"

func vt100() *ServerSpec {
	key := func(context.Context) (string, error) { return "tskey-auth-secret", nil }
	return Server("vt100").Type("cx22").Location("hel1").Image("debian-13").
		SSHKeys(Key{Name: "stian", PublicKey: testPublicKey}).
		OpenPorts(443, 80).JoinTailnet(key, "tag:vt100", "tag:ci")
}

func TestPlanCreatesMissingServer(t *testing.T) {
	_, acct := newFake(t)
	plan, err := acct.Plan(context.Background(), vt100())
	if err != nil {
		t.Fatal(err)
	}
	want := "+ create vt100: cx22, hel1, debian-13, €4.59/month, ports 80, 443 open, SSH key stian (uploaded), joins the tailnet as tag:vt100,tag:ci\n"
	if plan.String() != want || !plan.Pending() {
		t.Errorf("plan =\n%s\nwant\n%s", plan, want)
	}
}

func TestApplyCreatesFirewallThenServerAndWaitsForTailnet(t *testing.T) {
	f, acct := newFake(t)
	net := &fakeTailnet{name: "vt100"}
	acct.Tailnet = net
	if _, err := acct.Apply(context.Background(), vt100()); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(f.writes, ", "); got != "POST /firewalls, POST /ssh_keys, POST /servers" {
		t.Errorf("writes = %s, want the firewall and key created before the server", got)
	}
	if f.sshKeys["stian"] != testPublicKey {
		t.Errorf("uploaded key = %q", f.sshKeys["stian"])
	}
	if names := f.created["vt100"]["ssh_keys"]; fmt.Sprint(names) != "[stian]" {
		t.Errorf("server created with ssh_keys %v", names)
	}
	s := f.serverNamed("vt100")
	if s == nil || s.Labels[labelProject] != "vt100" {
		t.Fatalf("server = %+v", s)
	}
	var fw *apiFirewall
	for _, x := range f.firewalls {
		fw = x
	}
	if fw.Name != "smed-vt100" || fw.Labels[labelServer] != "vt100" || !samePorts(firewallPorts(fw), []int{80, 443}) {
		t.Errorf("firewall = %+v", fw)
	}
	if !slices.Contains(f.applied[fw.ID], s.ID) {
		t.Error("firewall not attached to the server at creation")
	}
	userData := f.created["vt100"]["user_data"].(string)
	for _, want := range []string{
		"ssh_pwauth: false",
		tailscaleInstall,
		`tailscale up --ssh --auth-key='tskey-auth-secret' --hostname='vt100' --advertise-tags='tag:vt100,tag:ci'`,
	} {
		if !strings.Contains(userData, want) {
			t.Errorf("user_data lacks %q:\n%s", want, userData)
		}
	}
	if net.askedFor < 3 {
		t.Errorf("Apply returned before the server was online in the tailnet (asked %d times)", net.askedFor)
	}

	again, err := acct.Plan(context.Background(), vt100())
	if err != nil {
		t.Fatal(err)
	}
	if again.Pending() || again[0].Action != Unchanged || again[0].Detail != s.PublicNet.IPv4.IP {
		t.Errorf("after apply:\n%s", again)
	}
}

func TestOnlySmedsServersAreConsidered(t *testing.T) {
	f, acct := newFake(t)
	f.addServer("basicbooking", "cpx21", "fsn1", "ubuntu-24.04", nil)
	f.addServer("vt100", "cx22", "hel1", "debian-13", nil) // same name, not smed's

	acct.PruneUndeclared = true
	_, err := acct.Plan(context.Background(), vt100())
	if err == nil || !strings.Contains(err.Error(), "isn't smed's") {
		t.Fatalf("err = %v, want a clash with the unlabelled vt100", err)
	}

	plan, err := acct.Plan(context.Background(), Server("web").Type("cx22").Location("hel1").Image("debian-13"))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].Server != "web" {
		t.Errorf("plan touches servers smed didn't create:\n%s", plan)
	}
}

func TestDriftIsReportedNotApplied(t *testing.T) {
	f, acct := newFake(t)
	f.addServer("vt100", "cx32", "hel1", "debian-13", map[string]string{labelProject: "vt100"})
	f.addFirewall("vt100", 80, 443)

	plan, err := acct.Apply(context.Background(), vt100())
	if err != nil {
		t.Fatal(err)
	}
	if plan[0].Action != Drift || !strings.Contains(plan[0].Detail, "type cx32, declared cx22") {
		t.Errorf("plan =\n%s", plan)
	}
	if len(f.writes) != 0 {
		t.Errorf("Apply changed something for a drifted server: %v", f.writes)
	}
}

func TestFirewallPortsAreUpdated(t *testing.T) {
	f, acct := newFake(t)
	f.addServer("vt100", "cx22", "hel1", "debian-13", map[string]string{labelProject: "vt100"})
	fw := f.addFirewall("vt100", 22)

	plan, err := acct.Plan(context.Background(), vt100())
	if err != nil {
		t.Fatal(err)
	}
	if want := "~ update vt100: firewall ports 22 open → ports 80, 443 open\n"; plan.String() != want {
		t.Errorf("plan =\n%swant\n%s", plan, want)
	}
	if _, err := acct.Apply(context.Background(), vt100()); err != nil {
		t.Fatal(err)
	}
	if !samePorts(firewallPorts(f.firewalls[fw.ID]), []int{80, 443}) {
		t.Errorf("rules = %+v", f.firewalls[fw.ID].Rules)
	}
}

func TestExistingServerWithoutFirewallGetsOne(t *testing.T) {
	f, acct := newFake(t)
	s := f.addServer("vt100", "cx22", "hel1", "debian-13", map[string]string{labelProject: "vt100"})
	if _, err := acct.Apply(context.Background(), vt100()); err != nil {
		t.Fatal(err)
	}
	for id, fw := range f.firewalls {
		if fw.Labels[labelServer] == "vt100" && slices.Contains(f.applied[id], s.ID) {
			return
		}
	}
	t.Error("no firewall applied to the existing server")
}

func TestPruneOnlyWhenAsked(t *testing.T) {
	f, acct := newFake(t)
	old := f.addServer("old", "cx22", "hel1", "debian-13", map[string]string{labelProject: "vt100"})
	oldFW := f.addFirewall("old", 80)

	plan, err := acct.Plan(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(plan) != 0 {
		t.Fatalf("without PruneUndeclared, plan =\n%s", plan)
	}

	acct.PruneUndeclared = true
	plan, err = acct.Apply(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if plan.String() != "- delete old: no longer declared\n" {
		t.Errorf("plan =\n%s", plan)
	}
	if f.servers[old.ID] != nil || f.firewalls[oldFW.ID] != nil {
		t.Error("server or its firewall still there")
	}
}

func TestManyServersAcrossPages(t *testing.T) {
	f, acct := newFake(t)
	var specs []*ServerSpec
	for i := range 5 {
		name := fmt.Sprintf("web-%d", i)
		f.addServer(name, "cx22", "hel1", "debian-13", map[string]string{labelProject: "vt100"})
		f.addFirewall(name)
		specs = append(specs, Server(name).Type("cx22").Location("hel1").Image("debian-13"))
	}
	plan, err := acct.Plan(context.Background(), specs...)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Pending() || len(plan) != 5 {
		t.Errorf("plan =\n%s", plan)
	}
}

func TestErrorsSurface(t *testing.T) {
	_, acct := newFake(t)
	acct.Token = func(context.Context) (string, error) { return "wrong", nil }
	if _, err := acct.Plan(context.Background(), vt100()); err == nil || !strings.Contains(err.Error(), "unable to authenticate") {
		t.Errorf("bad token: err = %v", err)
	}

	_, acct = newFake(t)
	for _, tc := range []struct {
		specs []*ServerSpec
		want  string
	}{
		{[]*ServerSpec{Server("a").Location("hel1")}, "missing Type, Image"},
		{[]*ServerSpec{vt100(), vt100()}, "declared twice"},
		{[]*ServerSpec{vt100().OpenPorts(70000)}, "out of range"},
		{[]*ServerSpec{vt100().JoinTailnet(nil, "vt100")}, `must start with tag:`},
	} {
		if _, err := acct.Plan(context.Background(), tc.specs...); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
}

func TestCloudInitWithoutTailnet(t *testing.T) {
	if got := cloudInit("web", "", nil); got != "#cloud-config\nssh_pwauth: false\n" {
		t.Errorf("cloud-init = %q", got)
	}
}

// A typo or a combination Hetzner doesn't sell fails the plan, before
// anything is created.
func TestPlanChecksTypeLocationAndImage(t *testing.T) {
	f, acct := newFake(t)
	for _, tc := range []struct {
		spec *ServerSpec
		want string
	}{
		{vt100().Location("hel9"), `there is no location "hel9"`},
		{vt100().Type("cx99"), `there is no server type "cx99"`},
		{vt100().Type("cx11"), "cx11 is deprecated"},
		{vt100().Type("cpx32"), "cpx32 isn't offered in hel1 (it is in fsn1)"},
		{vt100().Image("debian-14"), `there is no x86 image "debian-14"`},
		{vt100().Image("debian-11"), "image debian-11 is deprecated"},
		{vt100().Type("cax11").Image("ubuntu-24.04"), `there is no arm image "ubuntu-24.04"`},
	} {
		if _, err := acct.Plan(context.Background(), tc.spec); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
	if len(f.writes) != 0 {
		t.Errorf("a failed plan wrote: %v", f.writes)
	}

	// An ARM type with an image that exists for ARM is fine.
	plan, err := acct.Plan(context.Background(), vt100().Type("cax11"))
	if err != nil || !strings.Contains(plan.String(), "€4.49/month") {
		t.Errorf("cax11: %v\n%s", err, plan)
	}
}

func TestSSHKeys(t *testing.T) {
	// A key already in the project is used, not uploaded again; a
	// different comment doesn't make it a different key.
	f, acct := newFake(t)
	f.sshKeys["stian"] = strings.Replace(testPublicKey, "stian@hegglund.com", "laptop", 1)
	plan, err := acct.Plan(context.Background(), vt100())
	if err != nil || !strings.Contains(plan.String(), "SSH key stian,") {
		t.Fatalf("existing key: %v\n%s", err, plan)
	}
	if _, err := acct.Apply(context.Background(), vt100()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(f.writes, "POST /ssh_keys") {
		t.Error("an existing key was uploaded again")
	}

	// A different key under the same name is refused, not overwritten.
	f, acct = newFake(t)
	f.sshKeys["stian"] = "ssh-ed25519 AAAAsomeoneElsesKey other@example.com"
	if _, err := acct.Plan(context.Background(), vt100()); err == nil || !strings.Contains(err.Error(), "different public key") {
		t.Errorf("mismatched key: err = %v", err)
	}

	// Without a key the plan says what that means.
	_, acct = newFake(t)
	plan, err = acct.Plan(context.Background(), Server("web").Type("cx22").Location("hel1").Image("debian-13"))
	if err != nil || !strings.Contains(plan.String(), "no SSH key (Hetzner emails a root password)") {
		t.Errorf("no key: %v\n%s", err, plan)
	}

	// Something that isn't a public key fails validation.
	_, err = acct.Plan(context.Background(), vt100().SSHKeys(Key{Name: "oops", PublicKey: "-----BEGIN OPENSSH PRIVATE KEY-----"}))
	if err == nil || !strings.Contains(err.Error(), "not a public key line") {
		t.Errorf("private key pasted: err = %v", err)
	}
}

// Hetzner moved a server's location from datacenter.location to location;
// both are read.
func TestServerLocationFromEitherField(t *testing.T) {
	var current, older apiServer
	if err := json.Unmarshal([]byte(`{"location":{"name":"hel1"}}`), &current); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"datacenter":{"location":{"name":"fsn1"}}}`), &older); err != nil {
		t.Fatal(err)
	}
	if current.location() != "hel1" || older.location() != "fsn1" {
		t.Errorf("locations = %q, %q", current.location(), older.location())
	}
}
