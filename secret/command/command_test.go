package command

import (
	"context"
	"strings"
	"testing"

	"github.com/vt100io/smed/secret"
)

// sh runs script with the ref as $1, standing in for a real secret store.
func sh(script string) []string { return []string{"sh", "-c", script, "sh"} }

func TestResolvePassesTheRefAndReturnsStdout(t *testing.T) {
	r := Resolver{Command: sh(`printf 'value-of-%s\n' "$1"`)}
	got, err := r.Resolve(context.Background(), "vt100/hetzner-token")
	if err != nil || got != "value-of-vt100/hetzner-token" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestMultiLineSecretsStayWhole(t *testing.T) {
	pem := "-----BEGIN KEY-----\nabc\n-----END KEY-----"
	r := Resolver{Command: sh(`printf '%s\n' "` + pem + `"`)}
	if got, _ := r.Resolve(context.Background(), "key"); got != pem {
		t.Errorf("got %q", got)
	}
}

// pass keeps the secret on the first line and notes below it.
func TestFirstLine(t *testing.T) {
	r := Resolver{Command: sh(`printf 'tskey-auth-abc\nexpires: 1 day\n'`), FirstLine: true}
	if got, _ := r.Resolve(context.Background(), "x"); got != "tskey-auth-abc" {
		t.Errorf("got %q", got)
	}
}

func TestErrorsCarryStderrButNeverTheSecret(t *testing.T) {
	// The value printed is built from the ref, so it appears nowhere in
	// the command line that the error does include.
	r := Resolver{Command: sh(`printf 'leaked-%s\n' "$1"; echo "$1 is not in the password store." >&2; exit 1`)}
	_, err := r.Resolve(context.Background(), "vt100/nope")
	if err == nil || !strings.Contains(err.Error(), "is not in the password store") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "leaked-vt100/nope") {
		t.Errorf("error contains stdout: %v", err)
	}
}

func TestBadSetups(t *testing.T) {
	for _, tc := range []struct {
		r    Resolver
		ref  secret.Ref
		want string
	}{
		{Resolver{}, "x", "Command is empty"},
		{Resolver{Command: []string{"echo"}}, "", "empty ref"},
		{Resolver{Command: []string{"smed-no-such-command"}}, "x", "executable file not found"},
		{Resolver{Command: sh(`true`)}, "x", "printed nothing"},
	} {
		if _, err := tc.r.Resolve(context.Background(), tc.ref); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("err = %v, want %q", err, tc.want)
		}
	}
}

func TestLazyWorksWithIt(t *testing.T) {
	get := secret.Lazy(Resolver{Command: []string{"echo"}}, "hello")
	if got, err := get(context.Background()); err != nil || got != "hello" {
		t.Errorf("got %q, %v", got, err)
	}
}
