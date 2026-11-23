package user

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/vt100io/smed/module"
)

type scriptedRunner struct {
	responses []scriptedResponse
	calls     []string
}

type scriptedResponse struct {
	out string
	err error
}

func (s *scriptedRunner) Run(ctx context.Context, cmd string) (string, error) {
	s.calls = append(s.calls, cmd)
	if len(s.calls) > len(s.responses) {
		return "", errors.New("scriptedRunner: ran out of scripted responses")
	}
	r := s.responses[len(s.calls)-1]
	return r.out, r.err
}

func TestReadParsesExistingUser(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "deploy:x:1001:1001:Deploy user:/home/deploy:/bin/bash\n" + sep + "\ndeploy\n" + sep + "\ndeploy sudo docker\n"},
	}}
	got, err := Account{Name: "deploy"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := state{
		Exists: true, UID: "1001", Group: "deploy", Groups: []string{"docker", "sudo"},
		Comment: "Deploy user", Home: "/home/deploy", Shell: "/bin/bash",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestReadParsesNoSupplementaryGroups(t *testing.T) {
	// id -Gn lists every group including the primary — when that's
	// the only one, the user has no supplementary groups at all.
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "lonely:x:1002:1002::/home/lonely:/bin/bash\n" + sep + "\nlonely\n" + sep + "\nlonely\n"},
	}}
	got, err := Account{Name: "lonely"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(state).Groups != nil {
		t.Fatalf("got Groups=%v, want nil (no supplementary groups)", got.(state).Groups)
	}
}

func TestReadTreatsUnknownUserAsNotExisting(t *testing.T) {
	// The real command always ends in "; true", so its exit code is
	// always 0 regardless of getent's own result — err is never
	// actually non-nil here in practice; only the captured text tells
	// you the user doesn't exist.
	r := &scriptedRunner{responses: []scriptedResponse{
		{out: "" + sep + "\nid: 'ghost': no such user\n" + sep + "\nid: 'ghost': no such user\n"},
	}}
	got, err := Account{Name: "ghost"}.Read(context.Background(), r)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.(state).Exists {
		t.Fatal("expected Exists=false for an unknown user")
	}
}

func TestDiffCreateWhenMissing(t *testing.T) {
	a := Account{Name: "deploy"}
	change, err := a.Diff(a.Desired(), state{Exists: false})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Create {
		t.Fatalf("Action = %v, want Create", change.Action)
	}
}

func TestDiffNoOpWhenUnmanagedAttributesDiffer(t *testing.T) {
	// UID/Group/Groups/Home/Shell/Comment all unset on Account —
	// nothing is managed, so an existing account with arbitrary values
	// must be NoOp, same as file.Content ignores an unmanaged
	// Owner/Group.
	a := Account{Name: "deploy"}
	current := state{
		Exists: true, UID: "1001", Group: "sudo", Groups: []string{"docker", "wheel"},
		Home: "/home/deploy", Shell: "/bin/zsh", Comment: "whatever",
	}
	change, err := a.Diff(a.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp", change.Action)
	}
}

func TestDiffUpdateWhenManagedShellDiffers(t *testing.T) {
	a := Account{Name: "deploy", Shell: "/bin/bash"}
	current := state{Exists: true, Shell: "/bin/sh"}
	change, err := a.Diff(a.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update", change.Action)
	}
}

func TestDiffUpdateWhenGroupsDiffer(t *testing.T) {
	a := Account{Name: "deploy", Groups: []string{"docker"}}
	current := state{Exists: true, Groups: []string{"sudo"}}
	change, err := a.Diff(a.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.Update {
		t.Fatalf("Action = %v, want Update", change.Action)
	}
}

func TestDiffGroupsIgnoresOrderAndDuplicates(t *testing.T) {
	a := Account{Name: "deploy", Groups: []string{"sudo", "docker", "docker"}}
	current := state{Exists: true, Groups: []string{"docker", "sudo"}}
	change, err := a.Diff(a.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (same set, different order/dupes)", change.Action)
	}
}

func TestDiffGroupsNilCurrentMatchesExplicitEmptyDesired(t *testing.T) {
	// A user with zero supplementary groups reads back as Groups=nil
	// (see TestReadParsesNoSupplementaryGroups). Desired Groups: []
	// explicitly means "manage to no groups" — those two must compare
	// equal, not as a difference.
	a := Account{Name: "deploy", Groups: []string{}}
	current := state{Exists: true, Groups: nil}
	change, err := a.Diff(a.Desired(), current)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	if change.Action != module.NoOp {
		t.Fatalf("Action = %v, want NoOp (nil and [] both mean zero groups)", change.Action)
	}
}

func TestApplyCreateUsesUseraddWithDashM(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	a := Account{Name: "deploy", Shell: "/bin/bash", Home: "/home/deploy"}
	result, err := a.Apply(context.Background(), r, module.Change{Action: module.Create})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Changed {
		t.Fatal("expected Changed=true")
	}
	got := r.calls[0]
	if !strings.HasPrefix(got, "useradd -m") {
		t.Fatalf("got command %q, want it to start with 'useradd -m'", got)
	}
	if !strings.Contains(got, "-s '/bin/bash'") || !strings.Contains(got, "-d '/home/deploy'") {
		t.Fatalf("got command %q, want both -s and -d flags", got)
	}
}

func TestApplyCreateSystemAccountUsesDashR(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	a := Account{Name: "postgres", System: true}
	if _, err := a.Apply(context.Background(), r, module.Change{Action: module.Create}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	if !strings.HasPrefix(got, "useradd -r") {
		t.Fatalf("got command %q, want it to start with 'useradd -r'", got)
	}
	if strings.Contains(got, "-m") {
		t.Fatalf("got command %q, system accounts shouldn't pass -m", got)
	}
}

func TestApplyUpdateUsesUsermod(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	a := Account{Name: "deploy", Shell: "/bin/bash"}
	if _, err := a.Apply(context.Background(), r, module.Change{Action: module.Update}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	if !strings.HasPrefix(got, "usermod") || strings.Contains(got, "useradd") {
		t.Fatalf("got command %q, want usermod not useradd", got)
	}
	if !strings.Contains(got, "-s '/bin/bash'") {
		t.Fatalf("got command %q, want -s flag", got)
	}
}

func TestApplyIncludesGroupsFlagWhenManaged(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	a := Account{Name: "deploy", Groups: []string{"docker", "sudo"}}
	if _, err := a.Apply(context.Background(), r, module.Change{Action: module.Update}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	if !strings.Contains(got, "-G 'docker,sudo'") {
		t.Fatalf("got command %q, want -G 'docker,sudo'", got)
	}
}

func TestApplyEmptyGroupsClearsSupplementaryMembership(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	a := Account{Name: "deploy", Groups: []string{}} // explicit: manage to zero groups
	if _, err := a.Apply(context.Background(), r, module.Change{Action: module.Update}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	if !strings.Contains(got, "-G ''") {
		t.Fatalf("got command %q, want -G '' (clear supplementary groups)", got)
	}
}

func TestApplyOnlyPassesManagedFlags(t *testing.T) {
	r := &scriptedRunner{responses: []scriptedResponse{{out: ""}}}
	a := Account{Name: "deploy", Shell: "/bin/bash"} // only Shell managed
	if _, err := a.Apply(context.Background(), r, module.Change{Action: module.Update}); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	got := r.calls[0]
	for _, flag := range []string{"-u ", "-g ", "-G ", "-d ", "-c "} {
		if strings.Contains(got, flag) {
			t.Fatalf("got command %q, unmanaged flag %q should not appear", got, flag)
		}
	}
}

// stian is a human account in sudo, the way it looks before you add docker.
var stian = state{Exists: true, UID: "1000", Group: "stian", Groups: []string{"sudo"}, Home: "/home/stian", Shell: "/bin/bash"}

// The finding this exists for: adding yourself to docker must not take
// you out of sudo.
func TestMemberOfAddsAndKeepsTheRest(t *testing.T) {
	a := Account{Name: "stian", MemberOf: []string{"docker"}}
	change, err := a.Diff(a.Desired(), stian)
	if err != nil || change.Action != module.Update || change.Description != "stian: add to docker" {
		t.Fatalf("diff %+v %v", change, err)
	}
	r := &scriptedRunner{responses: []scriptedResponse{{}}}
	if _, err := a.Apply(context.Background(), r, change); err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "usermod -a -G 'docker' 'stian' 2>&1" {
		t.Errorf("ran %q", r.calls[0])
	}
	// Once in docker, and in more groups than asked, there's nothing to do.
	after := stian
	after.Groups = []string{"docker", "plugdev", "sudo"}
	if change, _ := a.Diff(a.Desired(), after); change.Action != module.NoOp {
		t.Errorf("after: %+v", change)
	}
	// The primary group counts as being in it.
	own := Account{Name: "stian", MemberOf: []string{"stian"}}
	if change, _ := own.Diff(own.Desired(), stian); change.Action != module.NoOp {
		t.Errorf("primary: %+v", change)
	}
}

func TestMemberOfOnCreate(t *testing.T) {
	a := Account{Name: "ci", MemberOf: []string{"docker", "adm"}}
	r := &scriptedRunner{responses: []scriptedResponse{{}}}
	if _, err := a.Apply(context.Background(), r, module.Change{Action: module.Create}); err != nil {
		t.Fatal(err)
	}
	if r.calls[0] != "useradd -m -G 'adm,docker' 'ci' 2>&1" {
		t.Errorf("ran %q", r.calls[0])
	}
}

func TestGroupsAndMemberOfTogetherIsAnError(t *testing.T) {
	a := Account{Name: "stian", Groups: []string{"sudo"}, MemberOf: []string{"docker"}}
	if _, err := a.Diff(a.Desired(), stian); err == nil {
		t.Error("both accepted")
	}
}

// Groups still replaces, but Plan says what it takes away.
func TestGroupsSaysWhatItRemoves(t *testing.T) {
	a := Account{Name: "stian", Groups: []string{"docker"}}
	change, err := a.Diff(a.Desired(), stian)
	if err != nil || !strings.Contains(change.Description, "(removes sudo)") {
		t.Errorf("diff %+v %v", change, err)
	}
}
