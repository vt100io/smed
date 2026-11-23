package apt

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/vt100io/smed/module"
)

const testKey = "-----BEGIN PGP PUBLIC KEY BLOCK-----\n\nmQINBFit2ioBEADhWpZ8/wvZ6hUTiXOwQHXMAlaFHcPH9hAtr4F1y2+OYdbtMuth\n-----END PGP PUBLIC KEY BLOCK-----\n"

var docker = Repository{
	Name: "docker", URIs: "https://download.docker.com/linux/debian", Suites: "trixie",
	Components: "stable", Architectures: "amd64", Key: testKey,
}

// shRunner runs smed's real commands with sh, with /etc/apt moved into a
// temporary directory, apt-get update recorded instead of run, and curl
// reading local files: the shell, base64, mv and quoting are all real.
type shRunner struct {
	root    string
	urls    map[string]string // KeyURL -> local file
	updates int
	cmds    []string
}

func (s *shRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.cmds = append(s.cmds, cmd)
	if cmd == "apt-get update 2>&1" {
		s.updates++
		return "Reading package lists... Done\n", nil
	}
	cmd = strings.ReplaceAll(cmd, "/etc/apt/", s.root+"/etc/apt/")
	for url, file := range s.urls {
		cmd = strings.ReplaceAll(cmd, "curl -fsSL '"+url+"' -o", "cp "+file)
	}
	out, err := exec.CommandContext(ctx, "sh", "-c", cmd).CombinedOutput()
	return string(out), err
}

func newSh(t *testing.T) *shRunner {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "etc/apt/sources.list.d"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &shRunner{root: root, urls: map[string]string{}}
}

func (s *shRunner) read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s.root, path))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestRepositoryFromNothing(t *testing.T) {
	sh := newSh(t)
	// A Docker setup done by hand before: the old .list must go, or apt
	// refuses two sources for one repository with different Signed-By.
	os.WriteFile(filepath.Join(sh.root, "etc/apt/sources.list.d/docker.list"), []byte("deb [signed-by=/usr/share/keyrings/docker.gpg] https://download.docker.com/linux/debian trixie stable\n"), 0o644)

	res, err := module.RunModule(context.Background(), docker, sh)
	if err != nil {
		t.Fatalf("%v\n%s", err, res.Output)
	}
	want := "Types: deb\nURIs: https://download.docker.com/linux/debian\nSuites: trixie\nComponents: stable\nArchitectures: amd64\nSigned-By: /etc/apt/keyrings/docker.asc\n"
	if got := sh.read(t, "etc/apt/sources.list.d/docker.sources"); got != want {
		t.Errorf("sources:\n%s\nwant:\n%s", got, want)
	}
	if got := sh.read(t, "etc/apt/keyrings/docker.asc"); got != testKey {
		t.Errorf("key %q", got)
	}
	if _, err := os.Stat(filepath.Join(sh.root, "etc/apt/sources.list.d/docker.list")); !os.IsNotExist(err) {
		t.Error("the old docker.list is still there")
	}
	if sh.updates != 1 {
		t.Errorf("%d apt-get updates", sh.updates)
	}
	// Run again: nothing to do, and no second update.
	res, err = module.RunModule(context.Background(), docker, sh)
	if err != nil || res.Changed || sh.updates != 1 {
		t.Errorf("second run: changed %v, %d updates, %v", res.Changed, sh.updates, err)
	}
}

func TestRepositoryNoticesChanges(t *testing.T) {
	sh := newSh(t)
	if _, err := module.RunModule(context.Background(), docker, sh); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		r    Repository
		want string
	}{
		{func() Repository { r := docker; r.Suites = "forky"; return r }(), "apt repository docker: update /etc/apt/sources.list.d/docker.sources"},
		{func() Repository { r := docker; r.Key = strings.Replace(testKey, "mQ", "mX", 1); return r }(), "apt repository docker: replace key /etc/apt/keyrings/docker.asc"},
	} {
		cur, err := c.r.Read(context.Background(), sh)
		if err != nil {
			t.Fatal(err)
		}
		change, err := c.r.Diff(c.r.Desired(), cur)
		if err != nil || change.Action != module.Update || change.Description != c.want {
			t.Errorf("%+v %v, want %q", change, err, c.want)
		}
	}
}

func TestRepositoryKeyURL(t *testing.T) {
	sh := newSh(t)
	keyFile := filepath.Join(t.TempDir(), "gpg")
	os.WriteFile(keyFile, []byte(testKey), 0o644)
	sh.urls["https://download.docker.com/linux/debian/gpg"] = keyFile
	r := docker
	r.Key, r.KeyURL = "", "https://download.docker.com/linux/debian/gpg"

	if res, err := module.RunModule(context.Background(), r, sh); err != nil {
		t.Fatalf("%v\n%s\n%q", err, res.Output, sh.cmds)
	}
	if got := sh.read(t, "etc/apt/keyrings/docker.asc"); got != testKey {
		t.Errorf("key %q", got)
	}
	// Fetched once: the key at the URL changing doesn't change the machine.
	os.WriteFile(keyFile, []byte("something else"), 0o644)
	if res, err := module.RunModule(context.Background(), r, sh); err != nil || res.Changed {
		t.Errorf("refetched: %+v %v", res, err)
	}
}

func TestRepositoryRefusesABinaryKeyFromURL(t *testing.T) {
	sh := newSh(t)
	keyFile := filepath.Join(t.TempDir(), "key.gpg")
	os.WriteFile(keyFile, []byte{0x99, 0x02, 0x0d, 0x04}, 0o644)
	sh.urls["https://example.com/key.gpg"] = keyFile
	r := docker
	r.Key, r.KeyURL = "", "https://example.com/key.gpg"

	res, err := module.RunModule(context.Background(), r, sh)
	if err == nil || !strings.Contains(res.Output, "not ASCII-armored") {
		t.Fatalf("accepted a binary key: %v %q", err, res.Output)
	}
	for _, f := range []string{"etc/apt/keyrings/docker.asc", "etc/apt/keyrings/docker.asc.smed-tmp", "etc/apt/sources.list.d/docker.sources"} {
		if _, err := os.Stat(filepath.Join(sh.root, f)); err == nil {
			t.Errorf("%s left behind", f)
		}
	}
	if sh.updates != 0 {
		t.Error("apt-get update ran")
	}
}

func TestRepositoryChecksAtPlan(t *testing.T) {
	for name, r := range map[string]Repository{
		"name with a slash": {Name: "../x", URIs: "https://a", Suites: "s", Key: testKey},
		"no suites":         {Name: "x", URIs: "https://a", Key: testKey},
		"no key":            {Name: "x", URIs: "https://a", Suites: "s"},
		"both keys":         {Name: "x", URIs: "https://a", Suites: "s", Key: testKey, KeyURL: "https://k"},
		"key isn't armored": {Name: "x", URIs: "https://a", Suites: "s", Key: "not a key"},
		"key over http":     {Name: "x", URIs: "https://a", Suites: "s", KeyURL: "http://k"},
		"flat with comps":   {Name: "x", URIs: "https://a", Suites: "./", Components: "main", Key: testKey},
		"newline":           {Name: "x", URIs: "https://a\nevil", Suites: "s", Key: testKey},
	} {
		if _, err := r.Diff(r.Desired(), repoState{}); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRepositoryUpdateWaitsForTheLock(t *testing.T) {
	run := &scriptedRunner{responses: []scriptedResponse{
		{out: ""}, // the files
		{out: "E: Could not get lock /var/lib/apt/lists/lock", err: errors.New("exit 100")},
		{out: "Done"},
	}}
	if _, err := docker.Apply(context.Background(), run, module.Change{Action: module.Create}); err != nil || len(run.calls) != 3 {
		t.Errorf("%v, %d calls", err, len(run.calls))
	}
}
