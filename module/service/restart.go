package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/vt100io/smed/module"
	"github.com/vt100io/smed/shellquote"
)

// Restart restarts a unit every time it runs: it's an action, not a state
// to converge to. It's what a handler wants. Unit{Active: true} as a
// handler only starts a unit that's stopped, so a running nginx would never
// see its new config:
//
//	Run(file.Content{Path: "/etc/nginx/conf.d/app.conf", Data: conf}).
//	Notify("restart-nginx").
//	Handler("restart-nginx", service.Restart{Name: "nginx"})
//
// With Reload, it asks the unit to reload its config instead, and restarts
// it only if the unit can't reload (systemctl reload-or-restart).
type Restart struct {
	Name   string
	Reload bool
}

func (r Restart) verb() string {
	if r.Reload {
		return "reload-or-restart"
	}
	return "restart"
}

func (r Restart) Desired() module.State { return nil }

// Read has nothing to read: there's no "already restarted".
func (r Restart) Read(ctx context.Context, run module.Runner) (module.State, error) {
	return nil, nil
}

// Diff always reports Update: reached means run. As a handler, the gate
// is whether a notifier changed something, not this module.
func (r Restart) Diff(desired, current module.State) (module.Change, error) {
	return module.Change{Action: module.Update, Description: fmt.Sprintf("systemctl %s %s", r.verb(), r.Name)}, nil
}

func (r Restart) Apply(ctx context.Context, run module.Runner, change module.Change) (module.Result, error) {
	out, err := run.Run(ctx, fmt.Sprintf("systemctl %s %s 2>&1", r.verb(), shellquote.Quote(r.Name)))
	if err != nil {
		return module.Result{Output: out}, fmt.Errorf("%s %s: %w", r.verb(), r.Name, err)
	}
	return module.Result{Changed: true, Output: out}, nil
}

// Verify checks the unit came back up: a restart into a broken config is
// a failure, not a change.
func (r Restart) Verify(ctx context.Context, run module.Runner, desired module.State) error {
	out, err := run.Run(ctx, "systemctl is-active "+shellquote.Quote(r.Name))
	if err != nil {
		return fmt.Errorf("%s is %s after %s", r.Name, strings.TrimSpace(out), r.verb())
	}
	return nil
}
