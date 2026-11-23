// vakt asks every tagged machine in the tailnet what it looks like right
// now, and prints the answers.
//
//	vakt
//
// On its own it only knows the tailnet. To compare with what you declared,
// call vakt.Ask from your fleet program with your HostSpecs, the way
// vt100-server does with `go run . vakt`.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/vt100io/smed/tailnet"
	"github.com/vt100io/smed/vakt"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	inv, err := vakt.Ask(ctx, tailnet.LocalDaemon(), vakt.Fleet{})
	if err != nil {
		fmt.Fprintln(os.Stderr, "vakt:", err)
		os.Exit(1)
	}
	if err := inv.WriteText(os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "vakt:", err)
		os.Exit(1)
	}
}
