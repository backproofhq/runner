package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/backproofhq/runner/internal/agent"
	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/version"
)

func newAgentCommand(stderr io.Writer, lookup config.LookupFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "agent",
		Short: "Poll Backproof and run jobs as they come due",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := config.WithEnvFile(lookup, config.RunnerEnvFile)
			if err != nil {
				return fmt.Errorf("load settings: %w", err)
			}
			cfg, err := config.Load(settings)
			if err != nil {
				return fmt.Errorf("load settings: %w", err)
			}
			if !cfg.Token.IsSet() || !cfg.RunnerKey.IsSet() {
				return errors.New("this runner isn't registered: run backproof register with an install token from the dashboard's Add runner page")
			}
			runnerID, err := crypto.RunnerID(cfg.RunnerKey)
			if err != nil {
				return fmt.Errorf("%s is not valid: register this runner again", config.EnvRunnerKey)
			}

			dockerClient, err := docker.New(runnerID)
			if err != nil {
				return fmt.Errorf("connect to docker: %w", err)
			}
			defer func() { _ = dockerClient.Close() }()

			runnerVersion := version.Get().Version
			client, err := api.NewAuthorized(cfg.APIURL, runnerVersion, cfg.Token)
			if err != nil {
				return fmt.Errorf("connect to Backproof: %w", err)
			}

			logger := newLogger(stderr, cfg.LogLevel)
			limits := docker.Limits{MemoryBytes: cfg.MemoryLimitBytes, NanoCPUs: cfg.CPULimitNanos}
			runner := agent.New(agent.Options{
				Client: client,
				Run: agent.NewJobRunner(agent.JobRunnerOptions{
					Engine:      postgres.New(dockerClient, limits),
					Logger:      logger,
					WorkDir:     cfg.WorkDir,
					DiskMargin:  cfg.DiskMargin,
					Lookup:      settings,
					SourcesFile: config.SourcesEnvFile,
					RunnerKey:   cfg.RunnerKey,
				}),
				Logger:       logger,
				Concurrency:  cfg.Concurrency,
				Version:      runnerVersion,
				Capabilities: agent.DefaultCapabilities(),
			})

			jobs, stopJobs := context.WithCancel(context.WithoutCancel(cmd.Context()))
			defer stopJobs()
			stopWatching := watchForSecondSignal(jobs, stopJobs, logger)
			defer stopWatching()

			logger.InfoContext(cmd.Context(), "runner started", slog.String("version", runnerVersion), slog.String("name", cfg.Name))
			return runner.Run(cmd.Context(), jobs)
		},
	}
}

// watchForSecondSignal lets a first signal drain the runner and a second one stop running jobs.
func watchForSecondSignal(jobs context.Context, stopJobs context.CancelFunc, logger *slog.Logger) (stop func()) {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	go func() {
		received := 0
		for {
			select {
			case <-jobs.Done():
				return
			case <-signals:
				received++
				if received == 1 {
					logger.InfoContext(jobs, "finishing running jobs before stopping, signal again to stop them now")
					continue
				}
				logger.WarnContext(jobs, "stopping running jobs")
				stopJobs()
				return
			}
		}
	}()
	return func() { signal.Stop(signals) }
}
