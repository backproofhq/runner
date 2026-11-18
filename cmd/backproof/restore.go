package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

const (
	localRunnerID = "local"
	localSourceID = "LOCAL"
	// An abstract socket disappears with its process, so a killed run can't leave the lock held.
	localRestoreLock = "@backproof-local-restore"
)

func newRestoreCommand(stdout, stderr io.Writer, lookup config.LookupFunc) *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:    "restore",
		Short:  "Test a local dump file and print the result as JSON",
		Hidden: true, // A development command until the agent can run real jobs.
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(lookup)
			if err != nil {
				return fmt.Errorf("load settings: %w", err)
			}
			// Each run removes the containers of its runner label, so two runs would stop each other.
			release, err := lockLocalRestore(cmd.Context(), localRestoreLock)
			if err != nil {
				return err
			}
			defer release()

			client, err := docker.New(localRunnerID)
			if err != nil {
				return fmt.Errorf("connect to docker: %w", err)
			}
			defer func() { _ = client.Close() }()

			id := make([]byte, 8)
			if _, err := rand.Read(id); err != nil {
				return fmt.Errorf("create job id: %w", err)
			}

			logger := newLogger(stderr, cfg.LogLevel)
			limits := docker.Limits{MemoryBytes: cfg.MemoryLimitBytes, NanoCPUs: cfg.CPULimitNanos}
			result := job.RunLocal(cmd.Context(), postgres.New(client, limits), logger, hex.EncodeToString(id), file, job.Options{
				WorkDir:  cfg.WorkDir,
				SourceID: localSourceID,
				Secrets:  config.LoadSourceSecrets(lookup, localSourceID),
			})

			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			if err := encoder.Encode(result); err != nil {
				return fmt.Errorf("print result: %w", err)
			}
			switch result.Status {
			case job.StatusPass:
				return nil
			case job.StatusFail:
				return exitCodeError(exitBackupFailed)
			default:
				return exitCodeError(exitRunnerError)
			}
		},
	}
	cmd.Flags().StringVar(&file, "file", "", "path to the dump file")
	if err := cmd.MarkFlagRequired("file"); err != nil {
		panic(err) // The flag is defined on the line above, so this is a programming error.
	}
	return cmd
}

func lockLocalRestore(ctx context.Context, name string) (func(), error) {
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", name)
	if errors.Is(err, syscall.EADDRINUSE) {
		return nil, errors.New("another restore is already running on this machine: wait for it to finish, then try again")
	}
	if err != nil {
		return nil, fmt.Errorf("lock local restore: %w", err)
	}
	return func() { _ = listener.Close() }, nil
}
