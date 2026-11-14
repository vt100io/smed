// Package command resolves secrets by running a command you choose, with
// the ref as its last argument. It's how smed works with any secret store
// that has a command line, without depending on one:
//
//	command.Resolver{Command: []string{"pass", "show"}, FirstLine: true} // pass, gopass
//	command.Resolver{Command: []string{"secret-tool", "lookup", "name"}} // GNOME Keyring, KWallet
//	command.Resolver{Command: []string{"bw", "get", "password"}}         // Bitwarden
//	command.Resolver{Command: []string{"op", "read"}}                    // 1Password
//
// Like the other resolvers, it runs on the control side as a local
// subprocess, never on a target. The secret is the command's standard
// output; it never appears in an error, which only carries the command's
// standard error.
package command

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/vt100io/smed/secret"
)

// Resolver runs Command with the ref appended.
type Resolver struct {
	Command []string

	// FirstLine keeps only the first line of the output. pass stores the
	// secret on the first line and anything else (usernames, notes) below
	// it; turn this off for secrets that span lines, like a PEM key.
	FirstLine bool
}

func (r Resolver) Resolve(ctx context.Context, ref secret.Ref) (string, error) {
	if len(r.Command) == 0 {
		return "", errors.New("command: Resolver.Command is empty")
	}
	if ref == "" {
		return "", errors.New("command: empty ref")
	}
	name := strings.Join(r.Command, " ") + " " + string(ref)

	cmd := exec.CommandContext(ctx, r.Command[0], append(slices.Clone(r.Command[1:]), string(ref))...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("%s: %s", name, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("%s: %w", name, err)
	}

	value := strings.TrimSuffix(string(out), "\n")
	if r.FirstLine {
		value, _, _ = strings.Cut(value, "\n")
	}
	if value == "" {
		return "", fmt.Errorf("%s: printed nothing", name)
	}
	return value, nil
}
