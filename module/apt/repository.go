package apt

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/vt100io/smed/module"
	"github.com/vt100io/smed/shellquote"
)

// Repository ensures an apt repository is configured the way Debian
// recommends today: a deb822 file in /etc/apt/sources.list.d/<Name>.sources
// whose Signed-By points at its own key in /etc/apt/keyrings/<Name>.asc, so
// the key is trusted for this repository only, not for every repository
// the way apt-key used to. When either changes, it runs apt-get update.
//
//	apt.Repository{
//		Name:       "docker",
//		URIs:       "https://download.docker.com/linux/debian",
//		Suites:     "trixie",
//		Components: "stable",
//		Key:        dockerKey, // //go:embed docker.asc
//	}
//
// The key is Key, the ASCII-armored key itself, or KeyURL, fetched once on
// the target with curl. Key is the better one: the key is reviewed and
// pinned in your repo, and Plan sees when it changes. A KeyURL key is
// fetched only when the keyring is missing; it must be armored ("-----BEGIN
// PGP PUBLIC KEY BLOCK-----"), since apt reads a .asc keyring as text.
//
// An old-style <Name>.list for the same repository is removed: apt refuses
// two sources for one repository with different Signed-By values.
type Repository struct {
	Name          string // the file names; letters, digits, '.', '_', '-'
	URIs          string // "https://download.docker.com/linux/debian"
	Suites        string // "trixie", or "./" for a flat repository
	Components    string // "stable", "main contrib"; empty for a flat repository
	Architectures string // "amd64"; "" means apt's own default
	Key           string
	KeyURL        string
}

const (
	sourcesDir  = "/etc/apt/sources.list.d"
	keyringsDir = "/etc/apt/keyrings"
	armorHeader = "-----BEGIN PGP PUBLIC KEY BLOCK-----"
)

func (r Repository) sourcesPath() string { return sourcesDir + "/" + r.Name + ".sources" }
func (r Repository) listPath() string    { return sourcesDir + "/" + r.Name + ".list" }
func (r Repository) keyPath() string     { return keyringsDir + "/" + r.Name + ".asc" }

// FilePath implements module.Pathed: Owns("/etc/apt/sources.list.d/*")
// keeps the repositories you declare and removes the rest.
func (r Repository) FilePath() string { return r.sourcesPath() }

// sources is the deb822 file.
func (r Repository) sources() string {
	var b strings.Builder
	b.WriteString("Types: deb\n")
	fmt.Fprintf(&b, "URIs: %s\n", r.URIs)
	fmt.Fprintf(&b, "Suites: %s\n", r.Suites)
	if r.Components != "" {
		fmt.Fprintf(&b, "Components: %s\n", r.Components)
	}
	if r.Architectures != "" {
		fmt.Fprintf(&b, "Architectures: %s\n", r.Architectures)
	}
	fmt.Fprintf(&b, "Signed-By: %s\n", r.keyPath())
	return b.String()
}

var repoName = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// check refuses a repository apt would reject, or one smed can't keep
// safely, so Plan says so rather than Apply.
func (r Repository) check() error {
	switch {
	case !repoName.MatchString(r.Name):
		return fmt.Errorf("apt repository %q: Name can only hold letters, digits, '.', '_' and '-'", r.Name)
	case r.URIs == "" || r.Suites == "":
		return fmt.Errorf("apt repository %s: needs URIs and Suites", r.Name)
	case strings.ContainsAny(r.URIs+r.Suites+r.Components+r.Architectures, "\n"):
		return fmt.Errorf("apt repository %s: a field holds a newline", r.Name)
	case strings.HasSuffix(r.Suites, "/") && r.Components != "":
		return fmt.Errorf("apt repository %s: a flat repository (Suites %q) has no Components", r.Name, r.Suites)
	case (r.Key == "") == (r.KeyURL == ""):
		return fmt.Errorf("apt repository %s: set exactly one of Key and KeyURL; an unsigned repository isn't supported", r.Name)
	case r.Key != "" && !strings.Contains(r.Key, armorHeader):
		return fmt.Errorf("apt repository %s: Key isn't an ASCII-armored public key", r.Name)
	case r.KeyURL != "" && !strings.HasPrefix(r.KeyURL, "https://"):
		return fmt.Errorf("apt repository %s: KeyURL must be https", r.Name)
	}
	return nil
}

type repoState struct {
	Sources    string
	HasSources bool
	Key        string // compared only when Key is set
	HasKey     bool
	OldList    bool // an old-style <Name>.list is there
}

func (r Repository) Desired() module.State {
	return repoState{Sources: r.sources(), HasSources: true, Key: r.Key, HasKey: true}
}

// Read reads the sources file and the keyring in one round trip, as
// base64 so any content survives line-based parsing, and whether an old
// .list file is there. Never writes.
func (r Repository) Read(ctx context.Context, run module.Runner) (module.State, error) {
	cmd := fmt.Sprintf(
		`for f in %s %s; do if [ -f "$f" ]; then printf 'FILE '; base64 -w0 "$f"; echo; else echo MISSING; fi; done; if [ -e %s ]; then echo LIST; else echo NOLIST; fi`,
		shellquote.Quote(r.sourcesPath()), shellquote.Quote(r.keyPath()), shellquote.Quote(r.listPath()),
	)
	out, err := run.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("read apt repository %s: %w", r.Name, err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(lines) != 3 {
		return nil, fmt.Errorf("read apt repository %s: unexpected output %q", r.Name, out)
	}
	var st repoState
	var files [2]string
	var has [2]bool
	for i := range 2 {
		line := strings.TrimSpace(lines[i])
		switch {
		case line == "MISSING":
		case strings.HasPrefix(line, "FILE"):
			b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(strings.TrimPrefix(line, "FILE")))
			if err != nil {
				return nil, fmt.Errorf("read apt repository %s: decode: %w", r.Name, err)
			}
			files[i], has[i] = string(b), true
		default:
			return nil, fmt.Errorf("read apt repository %s: unexpected output %q", r.Name, out)
		}
	}
	st.Sources, st.HasSources = files[0], has[0]
	st.Key, st.HasKey = files[1], has[1]
	st.OldList = strings.TrimSpace(lines[2]) == "LIST"
	return st, nil
}

func (r Repository) Diff(desired, current module.State) (module.Change, error) {
	if err := r.check(); err != nil {
		return module.Change{}, err
	}
	d, ok := desired.(repoState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: desired state is %T, not apt.repoState", desired)
	}
	cur, ok := current.(repoState)
	if !ok {
		return module.Change{}, fmt.Errorf("diff: current state is %T, not apt.repoState", current)
	}
	if !cur.HasSources && !cur.HasKey {
		desc := fmt.Sprintf("add apt repository %s (%s %s)", r.Name, r.URIs, r.Suites)
		if cur.OldList {
			desc += ", replacing " + r.listPath()
		}
		return module.Change{Action: module.Create, Description: desc}, nil
	}
	var diffs []string
	if !cur.HasSources {
		diffs = append(diffs, "write "+r.sourcesPath())
	} else if cur.Sources != d.Sources {
		diffs = append(diffs, "update "+r.sourcesPath())
	}
	switch {
	case !cur.HasKey:
		diffs = append(diffs, "write key "+r.keyPath())
	case r.Key != "" && cur.Key != d.Key:
		diffs = append(diffs, "replace key "+r.keyPath())
	}
	if cur.OldList {
		diffs = append(diffs, "remove "+r.listPath())
	}
	if len(diffs) == 0 {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: fmt.Sprintf("apt repository %s: %s", r.Name, strings.Join(diffs, ", "))}, nil
}

// Apply writes the key and the sources file, each through a temporary file
// and mv so apt never reads half of one, removes an old .list, then
// updates the package lists so the next apt.Installed sees the repository.
func (r Repository) Apply(ctx context.Context, run module.Runner, change module.Change) (module.Result, error) {
	if err := r.check(); err != nil {
		return module.Result{}, err
	}
	key, tmpKey := shellquote.Quote(r.keyPath()), shellquote.Quote(r.keyPath()+".smed-tmp")
	src, tmpSrc := shellquote.Quote(r.sourcesPath()), shellquote.Quote(r.sourcesPath()+".smed-tmp")
	steps := []string{"install -d -m 0755 " + shellquote.Quote(keyringsDir)}
	if r.Key != "" {
		steps = append(steps, fmt.Sprintf("echo %s | base64 -d > %s", shellquote.Quote(base64.StdEncoding.EncodeToString([]byte(r.Key))), tmpKey))
	} else {
		// Fetched once: an existing keyring is kept, so Plan stays quiet and
		// the key doesn't change behind your back.
		steps = append(steps, fmt.Sprintf(
			"if [ -f %s ]; then cp %s %s; else curl -fsSL %s -o %s; fi",
			key, key, tmpKey, shellquote.Quote(r.KeyURL), tmpKey))
		// -e: the header starts with "-", which grep would read as options.
		steps = append(steps, fmt.Sprintf(
			"{ grep -q -e %s %s || { rm -f %s; echo %s; false; }; }",
			shellquote.Quote(armorHeader), tmpKey, tmpKey, shellquote.Quote("the key at "+r.KeyURL+" is not ASCII-armored")))
	}
	steps = append(steps,
		fmt.Sprintf("chmod 0644 %s && mv %s %s", tmpKey, tmpKey, key),
		fmt.Sprintf("echo %s | base64 -d > %s && chmod 0644 %s && mv %s %s",
			shellquote.Quote(base64.StdEncoding.EncodeToString([]byte(r.sources()))), tmpSrc, tmpSrc, tmpSrc, src),
		"rm -f "+shellquote.Quote(r.listPath()),
	)
	out, err := run.Run(ctx, strings.Join(steps, " && ")+" 2>&1")
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("apt repository %s: %w", r.Name, err)
	}

	for attempt := 1; ; attempt++ {
		upd, err := run.Run(ctx, "apt-get update 2>&1")
		out += upd
		if err == nil {
			return module.Result{Changed: true, Output: out}, nil
		}
		if !looksLikeDpkgLockHeld(upd) || attempt >= maxApplyAttempts {
			return module.Result{Output: out}, fmt.Errorf("apt repository %s: apt-get update: %w", r.Name, err)
		}
		select {
		case <-time.After(lockRetryDelay):
		case <-ctx.Done():
			return module.Result{Output: out}, ctx.Err()
		}
	}
}

func (r Repository) Verify(ctx context.Context, run module.Runner, desired module.State) error {
	return module.VerifyByReread(ctx, r, run, desired)
}
