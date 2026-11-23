package command

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/vt100io/smed/dag"
	"github.com/vt100io/smed/module"
)

type capturingRunner struct {
	gotCommand string
	out        string
	err        error
}

func (c *capturingRunner) Run(ctx context.Context, cmd string) (string, error) {
	c.gotCommand = cmd
	return c.out, c.err
}

func TestDiffAlwaysReportsUpdate(t *testing.T) {
	r := Run{Cmd: "echo hi"}
	change, err := r.Diff(r.Desired(), nil)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update — command has no concept of already-done", change.Action)
	}
}

func TestRunModuleAlwaysAppliesEvenOnRepeatedCalls(t *testing.T) {
	// Proves the whole point: calling RunModule (the normal
	// Read-Diff-Apply-Verify sequence) twice in a row runs the command
	// twice, unlike every other module in this repo.
	r := &capturingRunner{}
	c := Run{Cmd: "systemctl restart nginx"}

	first, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("first RunModule: %v", err)
	}
	if !first.Changed || r.gotCommand != "systemctl restart nginx" {
		t.Fatalf("first run: got Changed=%v, command=%q", first.Changed, r.gotCommand)
	}

	r.gotCommand = ""
	second, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("second RunModule: %v", err)
	}
	if !second.Changed || r.gotCommand != "systemctl restart nginx" {
		t.Fatalf("second run: want it to run again (Changed=true, command re-issued), got Changed=%v, command=%q", second.Changed, r.gotCommand)
	}
}

func TestApplyPropagatesCommandFailure(t *testing.T) {
	r := &capturingRunner{out: "unit not found", err: errors.New("exit status 5")}
	c := Run{Cmd: "systemctl restart ghost"}

	_, err := module.RunModule(context.Background(), c, r)
	if err == nil {
		t.Fatal("expected an error when the underlying command fails")
	}
}

func TestVerifyIsAlwaysNoOp(t *testing.T) {
	c := Run{Cmd: "echo hi"}
	if err := c.Verify(context.Background(), &capturingRunner{}, c.Desired()); err != nil {
		t.Fatalf("Verify: %v, want nil — nothing to re-check", err)
	}
}

func TestUnlessSkipsApplyWhenAlreadySatisfied(t *testing.T) {
	r := &capturingRunner{}
	c := Run{
		Cmd:    "tar -xzf release.tar.gz -C /opt/app",
		Unless: func(ctx context.Context, run module.Runner) (bool, error) { return true, nil },
	}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if result.Changed {
		t.Fatal("expected Changed=false — Unless reported already satisfied")
	}
	if r.gotCommand != "" {
		t.Fatalf("expected Cmd to never run, but got %q", r.gotCommand)
	}
}

func TestUnlessRunsApplyWhenNotSatisfied(t *testing.T) {
	r := &capturingRunner{}
	c := Run{
		Cmd:    "tar -xzf release.tar.gz -C /opt/app",
		Unless: func(ctx context.Context, run module.Runner) (bool, error) { return false, nil },
	}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed || r.gotCommand != "tar -xzf release.tar.gz -C /opt/app" {
		t.Fatalf("expected Cmd to run, got Changed=%v, command=%q", result.Changed, r.gotCommand)
	}
}

func TestUnlessPropagatesItsOwnError(t *testing.T) {
	c := Run{
		Cmd:    "tar -xzf release.tar.gz -C /opt/app",
		Unless: func(ctx context.Context, run module.Runner) (bool, error) { return false, errors.New("boom") },
	}

	_, err := module.RunModule(context.Background(), c, &capturingRunner{})
	if err == nil {
		t.Fatal("expected an error when Unless itself fails")
	}
}

func TestNilUnlessAlwaysRuns(t *testing.T) {
	// Same as TestRunModuleAlwaysAppliesEvenOnRepeatedCalls, but
	// explicit about the zero-value case: Unless unset must behave
	// identically to before this field existed.
	r := &capturingRunner{}
	c := Run{Cmd: "echo hi"}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — nil Unless means always run")
	}
}

func TestPathExistsReturnsTrueWhenPathIsPresent(t *testing.T) {
	r := &capturingRunner{out: "yes\n"}
	satisfied, err := PathExists("/opt/app/release")(context.Background(), r)
	if err != nil {
		t.Fatalf("PathExists check: %v", err)
	}
	if !satisfied {
		t.Fatal("expected satisfied=true when the probe prints yes")
	}
	if r.gotCommand != `test -e '/opt/app/release' && echo yes || echo no` {
		t.Fatalf("got command %q", r.gotCommand)
	}
}

func TestPathExistsReturnsFalseWhenPathIsMissing(t *testing.T) {
	r := &capturingRunner{out: "no\n"}
	satisfied, err := PathExists("/opt/app/release")(context.Background(), r)
	if err != nil {
		t.Fatalf("PathExists check: %v", err)
	}
	if satisfied {
		t.Fatal("expected satisfied=false when the probe prints no")
	}
}

func TestPathExistsAsUnlessIntegratesWithRunModule(t *testing.T) {
	r := &scriptedCommandRunner{responses: []string{"no\n", ""}} // test -e -> missing, then the real command
	c := Run{Cmd: "tar -xzf release.tar.gz -C /opt/app/release", Unless: PathExists("/opt/app/release")}

	result, err := module.RunModule(context.Background(), c, r)
	if err != nil {
		t.Fatalf("RunModule: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true — the path doesn't exist yet")
	}
	if len(r.calls) != 2 {
		t.Fatalf("expected 2 commands (the check, then the real one), got %d: %v", len(r.calls), r.calls)
	}
}

type scriptedCommandRunner struct {
	responses []string
	calls     []string
}

func (s *scriptedCommandRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.calls = append(s.calls, cmd)
	if len(s.calls) > len(s.responses) {
		return "", errors.New("scriptedCommandRunner: ran out of scripted responses")
	}
	return s.responses[len(s.calls)-1], nil
}

const password = "hunter2-s3cr3t"

func lazy(v string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return v, nil }
}

// The whole point of Env: the secret reaches the command, and nothing smed
// prints, returns or keeps contains it.
func TestEnvNeverShowsTheSecret(t *testing.T) {
	c := Run{Cmd: `psql -c "ALTER USER app PASSWORD '$PGPASS'"`, Env: map[string]func(context.Context) (string, error){"PGPASS": lazy(password)}}

	change, err := c.Diff(c.Desired(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := `psql -c "ALTER USER app PASSWORD '$PGPASS'" (with $PGPASS)`; change.Description != want {
		t.Errorf("plan says %q, want %q", change.Description, want)
	}

	// The command echoes the password back, and fails.
	r := &capturingRunner{out: "ERROR: bad password " + password + "\n", err: errors.New("exit 1: " + password)}
	res, err := c.Apply(context.Background(), r, change)
	if !strings.HasPrefix(r.gotCommand, "export PGPASS='"+password+"'; psql") {
		t.Errorf("the command didn't get the secret: %q", r.gotCommand)
	}
	for what, s := range map[string]string{"output": res.Output, "error": err.Error()} {
		if strings.Contains(s, password) {
			t.Errorf("%s shows the secret: %q", what, s)
		}
		if !strings.Contains(s, "[secret PGPASS]") {
			t.Errorf("%s doesn't say a secret was there: %q", what, s)
		}
	}
}

// Plan reads and diffs; it must never fetch a secret, which for 1Password
// would also mean a prompt on every plan.
func TestPlanNeverResolves(t *testing.T) {
	c := Run{Cmd: "true", Env: map[string]func(context.Context) (string, error){
		"X": func(context.Context) (string, error) { t.Error("resolved during plan"); return "", nil },
	}}
	var out module.Change
	g, err := dag.NewGraph([]dag.Step{module.PlanStep("s", c, &capturingRunner{}, &out)})
	if err != nil {
		t.Fatal(err)
	}
	g.Run(context.Background(), 0)
	if out.Action != module.Update {
		t.Errorf("plan %+v", out)
	}
}

func TestEnvNamesAreChecked(t *testing.T) {
	for _, env := range []map[string]func(context.Context) (string, error){
		{"NOT-A-NAME": lazy("x")},
		{"1X": lazy("x")},
		{"X": nil},
	} {
		c := Run{Cmd: "true", Env: env}
		if _, err := c.Diff(nil, nil); err == nil {
			t.Errorf("Diff accepted %v", env)
		}
	}
}

func TestResolveFailureNamesTheVariable(t *testing.T) {
	c := Run{Cmd: "true", Env: map[string]func(context.Context) (string, error){
		"TOKEN": func(context.Context) (string, error) { return "", errors.New("op: not signed in") },
	}}
	r := &capturingRunner{}
	_, err := c.Apply(context.Background(), r, module.Change{})
	if err == nil || !strings.Contains(err.Error(), "$TOKEN: op: not signed in") || r.gotCommand != "" {
		t.Errorf("err %v, ran %q", err, r.gotCommand)
	}
}

// A secret that contains another is replaced whole, not half.
func TestRedactLongestFirst(t *testing.T) {
	got := redact("abc and abcdef", map[string]string{"SHORT": "abc", "LONG": "abcdef", "EMPTY": ""})
	if got != "[secret SHORT] and [secret LONG]" {
		t.Errorf("%q", got)
	}
}
