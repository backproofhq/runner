package main

import (
	"fmt"
	"io"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/version"
)

func newRootCommand(stdout, stderr io.Writer, lookup config.LookupFunc) *cobra.Command {
	root := &cobra.Command{
		Use:           "backproof",
		Short:         "Prove that your latest Postgres backup restores",
		Long:          "The Backproof runner restores your latest Postgres backup into a throwaway container, runs checks on it and reports metadata to Backproof. Your data never leaves your infrastructure.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetOut(stdout)
	root.SetErr(stderr)

	stub := func(use, short string) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				cfg, err := config.Load(lookup)
				if err != nil {
					return fmt.Errorf("load settings: %w", err)
				}
				logger := newLogger(stderr, cfg.LogLevel)
				logger.InfoContext(cmd.Context(), "command not implemented", slog.String("command", use))
				return fmt.Errorf("the %s command is not available in this build: update to a newer runner release", use)
			},
		}
	}

	runCmd := stub("run", "Run every due job once, then exit")
	runCmd.Flags().Bool("once", false, "run the jobs that are due, then exit")
	if err := runCmd.MarkFlagRequired("once"); err != nil {
		panic(err) // The flag is defined on the line above, so this is a programming error.
	}

	root.AddCommand(
		stub("agent", "Poll Backproof and run jobs as they come due"),
		runCmd,
		stub("register", "Register this runner with an install token"),
		stub("doctor", "Check this runner's setup"),
		newVersionCommand(),
	)
	return root
}

// newVersionCommand does not load settings, so it still works when they are invalid.
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the runner's version and commit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if _, err := fmt.Fprintln(cmd.OutOrStdout(), version.Get()); err != nil {
				return fmt.Errorf("print version: %w", err)
			}
			return nil
		},
	}
}

func newLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level}))
}
