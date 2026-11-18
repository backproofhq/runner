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
			runner, logger, closeRunner, err := newRunner(stderr, lookup)
			if err != nil {
				return err
			}
			defer closeRunner()

			jobs, stopJobs := context.WithCancel(context.WithoutCancel(cmd.Context()))
			defer stopJobs()
			stopWatching := watchForSecondSignal(jobs, stopJobs, logger)
			defer stopWatching()

			logger.InfoContext(cmd.Context(), "runner started", slog.String("version", version.Get().Version))
			return runner.Run(cmd.Context(), jobs)
		},
	}
}

func newRunCommand(stdout, stderr io.Writer, lookup config.LookupFunc) *cobra.Command {
	var once bool
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run every due job once, then exit",
		Long:  "Runs the jobs that are due, then exits with 0 when all passed, 1 when a backup failed and 2 when the runner couldn't do its job.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			runner, logger, closeRunner, err := newRunner(stderr, lookup)
			if err != nil {
				return err
			}
			defer closeRunner()

			jobs, stopJobs := context.WithCancel(context.WithoutCancel(cmd.Context()))
			defer stopJobs()
			stopWatching := watchForSecondSignal(jobs, stopJobs, logger)
			defer stopWatching()

			summary, runErr := runner.RunOnce(cmd.Context(), jobs)
			if runErr != nil {
				_, _ = fmt.Fprintf(stderr, "backproof: %v\n", runErr)
			}
			if _, err := fmt.Fprintf(stdout, "%d passed, %d failed, %d errors\n", summary.Passed, summary.Failed, summary.Errors); err != nil {
				return fmt.Errorf("print summary: %w", err)
			}
			if code := summary.ExitCode(runErr); code != agent.ExitPassed {
				return exitCodeError(code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "run the jobs that are due, then exit")
	if err := cmd.MarkFlagRequired("once"); err != nil {
		panic(err) // The flag is defined on the line above, so this is a programming error.
	}
	return cmd
}

// newRunner connects a registered runner to Backproof and Docker.
func newRunner(stderr io.Writer, lookup config.LookupFunc) (*agent.Agent, *slog.Logger, func(), error) {
	settings, err := config.WithEnvFile(lookup, config.RunnerEnvFile)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load settings: %w", err)
	}
	cfg, err := config.Load(settings)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load settings: %w", err)
	}
	if !cfg.Token.IsSet() || !cfg.RunnerKey.IsSet() {
		return nil, nil, nil, errors.New("this runner isn't registered: run backproof register with an install token from the dashboard's Add runner page")
	}
	runnerID, err := crypto.RunnerID(cfg.RunnerKey)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%s is not valid: register this runner again", config.EnvRunnerKey)
	}

	dockerClient, err := docker.New(runnerID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to docker: %w", err)
	}
	runnerVersion := version.Get().Version
	client, err := api.NewAuthorized(cfg.APIURL, runnerVersion, cfg.Token)
	if err != nil {
		_ = dockerClient.Close()
		return nil, nil, nil, fmt.Errorf("connect to Backproof: %w", err)
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
	return runner, logger, func() { _ = dockerClient.Close() }, nil
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
