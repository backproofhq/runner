// Command backproof proves that Postgres backups restore, inside the customer's infrastructure.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/backproofhq/runner/internal/config"
)

const (
	exitOK           = 0
	exitBackupFailed = 1
	exitRunnerError  = 2
)

// exitCodeError ends the command with an exit code after it has already reported the outcome.
type exitCodeError int

func (e exitCodeError) Error() string {
	return fmt.Sprintf("exit code %d", int(e))
}

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
		var exitCode exitCodeError
		if errors.As(err, &exitCode) {
			return int(exitCode)
		}
		_, _ = fmt.Fprintf(stderr, "backproof: %v\n", err)
		return exitRunnerError
	}
	return exitOK
}
