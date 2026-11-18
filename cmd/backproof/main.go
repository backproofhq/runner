// Command backproof proves that Postgres backups restore, inside the customer's infrastructure.
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/backproofhq/runner/internal/config"
)

const (
	exitOK = 0
	// exitRunnerError means the runner could not do its job. Exit code 1 is reserved for failed backups.
	exitRunnerError = 2
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr, os.LookupEnv)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, lookup config.LookupFunc) int {
	root := newRootCommand(stdout, stderr, lookup)
	root.SetArgs(args)
	if err := root.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintf(stderr, "backproof: %v\n", err)
		return exitRunnerError
	}
	return exitOK
}
