// vakt is the control center for the tailnet: it asks every tagged machine
// what it looks like right now, every few seconds, in a terminal UI.
//
//	vakt          # the control center
//	vakt -text    # the inventory as a table, once
//
// On its own it only knows the tailnet. To compare with what you declared,
// call tui.Main from your fleet program with your HostSpecs, the way
// vt100-server does with `go run . vakt`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/vakt"
	"github.com/vt100io/smed/vakt/tui"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := tui.Main(ctx, tailnet.LocalDaemon(), vakt.Fleet{}, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "vakt:", err)
		os.Exit(1)
	}
}
