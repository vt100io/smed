// Package command implements module.Module for running an arbitrary
// shell command: smed's version of Ansible's command/shell modules.
// There's no meaningful Read/Diff for "has this command already run" —
// a command isn't a resource with state to converge toward — so by
// default this module is non-idempotent: Apply runs every single time
// it's reached, same as Ansible's own command module.
//
// The main reason this exists at all rather than staying hand-rolled
// inline (every demo so far had done exactly that) is handlers: a
// restart action ("systemctl restart X") isn't a convergent resource
// state the way service.Unit{Active: true} is — starting an
// already-active unit is a no-op, not a restart — so a handler usually
// needs something action-shaped to wrap. For a systemd unit that's
// service.Restart; this is the generic one for everything else.
//
// Unless is the escape hatch for the other real case this module
// covers: a command that genuinely does have an "already done" state,
// but no module exists for it yet (installing a GPG keyring via a
// shell pipeline, joining a tailnet, unpacking a release tarball into
// a venv) — the same job Ansible's creates:/removes: do for an
// otherwise-unconditional command/shell task.
//
// Cmd is printed: in Plan, in errors, and in any record of what ran. A
// secret goes in Env, never in Cmd.
package command

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/vt100io/smed/module"
	"github.com/vt100io/smed/shellquote"
)

// Run executes Cmd whenever it's reached in the graph — unconditionally
// if Unless is nil (the default, unchanged from before Unless existed),
// or only if Unless reports the condition isn't already satisfied.
type Run struct {
	Cmd string

	// Env sets environment variables for Cmd, from values resolved only
	// when Cmd runs: secret.Lazy fits. Plan never resolves them, and
	// what smed prints names the variable, never its value; a value the
	// command prints back is replaced with [secret NAME] in its output.
	//
	//	command.Run{
	//		Cmd: `psql -c "ALTER USER app PASSWORD '$PGPASS'"`,
	//		Env: map[string]func(context.Context) (string, error){"PGPASS": secret.Lazy(op, "op://infra/db/app")},
	//	}
	//
	// The value still reaches the target inside the command smed sends,
	// like every secret smed delivers, so the target's root can see it
	// while the command runs.
	Env map[string]func(context.Context) (string, error)

	// Unless, checked once during Read, is this command's idempotency
	// gate: if it reports true (whatever it checks is already true on
	// the real target), Diff reports NoOp and Apply never runs. nil
	// means always run — correct for something like a restart, which
	// has no "already done" state to check at all. PathExists below
	// covers the single most common case (Ansible's creates: path) as
	// a one-line constructor.
	Unless func(ctx context.Context, r module.Runner) (bool, error)
}

// state carries Read's one finding — whether Unless was already
// satisfied — forward to Diff. Doing the actual check in Read, not
// Diff, keeps Diff a pure function of two States, same as every other
// module: Unless needs a Runner to do real I/O, and Read is the only
// method in this contract allowed to have one.
type state struct {
	satisfied bool
}

func (r Run) Desired() module.State { return nil }

// Read runs Unless, if set, and carries its result forward as state —
// the only thing this module's state exists for. No Unless means
// nothing to check, so Diff always reports Update, unchanged from
// before this field existed.
func (r Run) Read(ctx context.Context, run module.Runner) (module.State, error) {
	if r.Unless == nil {
		return state{}, nil
	}
	satisfied, err := r.Unless(ctx, run)
	if err != nil {
		return nil, fmt.Errorf("check unless: %w", err)
	}
	return state{satisfied: satisfied}, nil
}

// Diff reports NoOp if Read found Unless already satisfied; otherwise
// Update, the same unconditional "run it" this module always reported
// before Unless existed.
func (r Run) Diff(desired, current module.State) (module.Change, error) {
	if err := r.checkEnv(); err != nil {
		return module.Change{}, err
	}
	if cur, ok := current.(state); ok && cur.satisfied {
		return module.Change{Action: module.NoOp}, nil
	}
	return module.Change{Action: module.Update, Description: r.describe()}, nil
}

func (r Run) Apply(ctx context.Context, run module.Runner, change module.Change) (module.Result, error) {
	if err := r.checkEnv(); err != nil {
		return module.Result{}, err
	}
	var prefix strings.Builder
	values := map[string]string{}
	for _, name := range r.envNames() {
		v, err := r.Env[name](ctx)
		if err != nil {
			return module.Result{}, fmt.Errorf("run %q: resolve $%s: %w", r.Cmd, name, err)
		}
		values[name] = v
		fmt.Fprintf(&prefix, "export %s=%s; ", name, shellquote.Quote(v))
	}
	out, err := run.Run(ctx, prefix.String()+r.Cmd)
	out = redact(out, values)
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("run %q: %s", r.describe(), redact(err.Error(), values))
	}
	return module.Result{Changed: true, Output: out}, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// checkEnv refuses a name the shell can't export, so Plan says so rather
// than Apply failing halfway.
func (r Run) checkEnv() error {
	for _, name := range r.envNames() {
		if !envName.MatchString(name) {
			return fmt.Errorf("run %q: %q is not a variable name", r.Cmd, name)
		}
		if r.Env[name] == nil {
			return fmt.Errorf("run %q: $%s has no value", r.Cmd, name)
		}
	}
	return nil
}

func (r Run) envNames() []string {
	names := make([]string, 0, len(r.Env))
	for name := range r.Env {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// describe is Cmd and the names of its secrets: what Plan and errors show.
func (r Run) describe() string {
	if len(r.Env) == 0 {
		return r.Cmd
	}
	return r.Cmd + " (with $" + strings.Join(r.envNames(), ", $") + ")"
}

// redact replaces every secret value in s with its name. Longest first, so
// a value that contains another is replaced whole.
func redact(s string, values map[string]string) string {
	names := make([]string, 0, len(values))
	for name, v := range values {
		if v != "" {
			names = append(names, name)
		}
	}
	slices.SortFunc(names, func(a, b string) int { return len(values[b]) - len(values[a]) })
	for _, name := range names {
		s = strings.ReplaceAll(s, values[name], "[secret "+name+"]")
	}
	return s
}

// Verify is a no-op: there's no desired state to re-check after the
// fact — running the command was the entire point, not a means to some
// other convergent state.
func (r Run) Verify(ctx context.Context, run module.Runner, desired module.State) error {
	return nil
}

// PathExists returns an Unless check satisfied once path exists on
// the target — the direct equivalent of Ansible's creates: path, and
// by far the most common shape an idempotency gate needs: "only run
// this if it hasn't already produced the thing it produces."
func PathExists(path string) func(context.Context, module.Runner) (bool, error) {
	return func(ctx context.Context, r module.Runner) (bool, error) {
		out, err := r.Run(ctx, fmt.Sprintf("test -e %s && echo yes || echo no", shellquote.Quote(path)))
		if err != nil {
			return false, fmt.Errorf("check path exists %s: %w", path, err)
		}
		return strings.TrimSpace(out) == "yes", nil
	}
}
