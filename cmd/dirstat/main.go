// Command dirstat is the entrypoint binary. It does nothing but hand control to
// the cli package, keeping main tiny and all behavior importable/testable.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/phillipod/go-dirstat/internal/cli"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := cli.New().ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "dirstat:", err)
		os.Exit(1)
	}
}
