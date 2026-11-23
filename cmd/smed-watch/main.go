// smed-watch is smed in watch mode without a fleet program: it asks every
// tagged machine in the tailnet what it looks like right now, every few
// seconds, in a terminal UI.
//
//	smed-watch          # the control center
//	smed-watch -text    # the inventory as a table, once
//
// On its own it only knows the tailnet. To compare with what you declared,
// call tui.Main from your fleet program with your HostSpecs, the way
// vt100-server does with `go run . watch`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/watch"
	"github.com/vt100io/smed/watch/tui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := tui.Main(ctx, tailnet.LocalDaemon(), watch.Fleet{}, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "smed watch:", err)
		os.Exit(1)
	}
}
