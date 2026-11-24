package agent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/job"
)

const (
	heartbeatInterval = 60 * time.Second
	retryDelay        = 5 * time.Second
	maxRetryDelay     = 5 * time.Minute
	resultAttempts    = 6
	resultTimeout     = 10 * time.Minute
	oncePollAttempts  = 5
)

// Exit codes of a one-shot run, which CI systems act on.
const (
	ExitPassed       = 0
	ExitBackupFailed = 1
	ExitRunnerError  = 2
)

// JobContext connects a running job to the agent.
type JobContext struct {
	OnStage     func(job.Stage)
	RunningJobs func() []string
}

// Runner tests the backup of one claimed job.
type Runner func(ctx context.Context, assigned api.Job, jobContext JobContext) api.JobResult

// Options configures an Agent.
type Options struct {
	Client       *api.RunnerClient
	Run          Runner
	Logger       *slog.Logger
	Concurrency  int
	Version      string
	Capabilities api.Capabilities
}

// Agent polls Backproof for due jobs and runs them.
type Agent struct {
	options           Options
	heartbeatInterval time.Duration
	retryDelay        time.Duration
	sleep             func(ctx context.Context, wait time.Duration)

	mu            sync.Mutex
	running       map[string]struct{}
	latestVersion string
	updateNotice  string
}

// New returns an agent.
func New(options Options) *Agent {
	return &Agent{
		options:           options,
		heartbeatInterval: heartbeatInterval,
		retryDelay:        retryDelay,
		sleep:             sleepWithJitter,
		running:           make(map[string]struct{}),
	}
}

// Run polls until ctx ends, then waits for the running jobs to finish. Ending jobs stops those
// jobs as well, so a drain can be cut short.
func (a *Agent) Run(ctx, jobs context.Context) error {
	var workers sync.WaitGroup
	defer workers.Wait()

	failures := 0
	connected := false
	for ctx.Err() == nil {
		result, err := a.options.Client.Poll(ctx, a.pollRequest())
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			var problem *api.ResponseError
			if errors.As(err, &problem) && problem.Status == http.StatusUnauthorized {
				return fmt.Errorf("the Backproof API rejected this runner's token, so it has stopped: %w", err)
			}
			failures++
			a.options.Logger.WarnContext(ctx, "check in with Backproof", slog.Any("error", err))
			a.sleep(ctx, a.backoff(failures, problem))
			continue
		}
		// The install script waits for this message to tell the user the runner is connected.
		if !connected || failures > 0 {
			a.options.Logger.InfoContext(ctx, "checked in with Backproof")
			connected = true
		}
		failures = 0
		a.noticeVersions(ctx, result)

		if result.Job != nil {
			a.claim(result.Job.Id.String())
			workers.Go(func() { a.work(jobs, *result.Job) })
			if a.hasFreeSlot() {
				continue
			}
		}
		a.sleep(ctx, result.NextPollAfter)
	}
	a.options.Logger.InfoContext(ctx, "stopped claiming jobs, waiting for running jobs to finish")
	return nil
}

// OnceSummary counts the outcomes of a one-shot run.
type OnceSummary struct {
	Passed int
	Failed int
	Errors int
}

// ExitCode reports a broken backup first, because that is the finding someone has to act on.
func (s OnceSummary) ExitCode(runErr error) int {
	switch {
	case s.Failed > 0:
		return ExitBackupFailed
	case s.Errors > 0 || runErr != nil:
		return ExitRunnerError
	default:
		return ExitPassed
	}
}

// RunOnce runs the jobs that are due, one after another, and returns when none are left.
func (a *Agent) RunOnce(ctx, jobs context.Context) (OnceSummary, error) {
	var summary OnceSummary
	failures := 0
	for {
		if err := ctx.Err(); err != nil {
			return summary, fmt.Errorf("stopped before every due job ran: %w", err)
		}

		result, err := a.options.Client.Poll(ctx, a.pollRequest())
		if err != nil {
			var problem *api.ResponseError
			failures++
			rejected := errors.As(err, &problem) && problem.Status == http.StatusUnauthorized
			if rejected || failures >= oncePollAttempts {
				return summary, fmt.Errorf("check in with Backproof: %w", err)
			}
			a.options.Logger.WarnContext(ctx, "check in with Backproof", slog.Any("error", err))
			a.sleep(ctx, a.backoff(failures, problem))
			continue
		}
		failures = 0
		a.noticeVersions(ctx, result)

		if result.UpdateRequired != "" {
			return summary, errors.New(result.UpdateRequired)
		}
		if result.Job == nil {
			return summary, nil
		}

		a.claim(result.Job.Id.String())
		outcome := a.work(jobs, *result.Job)
		switch outcome.status {
		case api.JobResultStatusPass:
			summary.Passed++
		case api.JobResultStatusFail:
			summary.Failed++
		default:
			summary.Errors++
		}
		// A result Backproof never received means the run didn't fully do its job.
		if !outcome.delivered && outcome.status != api.JobResultStatusError {
			summary.Errors++
		}
	}
}

func (a *Agent) pollRequest() api.PollRequest {
	status := api.Idle
	if !a.hasFreeSlot() {
		status = api.Busy
	}
	return api.PollRequest{Version: a.options.Version, Capabilities: a.options.Capabilities, Status: status}
}

func (a *Agent) backoff(failures int, problem *api.ResponseError) time.Duration {
	if problem != nil && problem.RetryAfter > 0 {
		return problem.RetryAfter
	}
	return min(a.retryDelay<<min(failures-1, 10), maxRetryDelay)
}

// noticeVersions logs each version notice once, because the agent polls every 30 seconds.
func (a *Agent) noticeVersions(ctx context.Context, result api.PollResult) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if result.UpdateRequired != a.updateNotice {
		a.updateNotice = result.UpdateRequired
		if result.UpdateRequired != "" {
			a.options.Logger.ErrorContext(ctx, "this runner gets no jobs until it is updated",
				slog.String("reason", result.UpdateRequired),
				slog.String("version", a.options.Version),
				slog.String("min_version", result.MinVersion),
			)
		}
	}
	if result.LatestVersion != "" && result.LatestVersion != a.latestVersion {
		a.latestVersion = result.LatestVersion
		if result.LatestVersion != a.options.Version {
			a.options.Logger.InfoContext(ctx, "a different runner version is available",
				slog.String("version", a.options.Version),
				slog.String("latest_version", result.LatestVersion),
			)
		}
	}
}

func (a *Agent) hasFreeSlot() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.running) < max(a.options.Concurrency, 1)
}

func (a *Agent) claim(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.running[jobID] = struct{}{}
}

func (a *Agent) release(jobID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.running, jobID)
}

func (a *Agent) runningJobs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	jobs := make([]string, 0, len(a.running))
	for id := range a.running {
		jobs = append(jobs, id)
	}
	return jobs
}

// jobOutcome says how a job ended and whether Backproof received its result.
type jobOutcome struct {
	status    api.JobResultStatus
	delivered bool
}

func (a *Agent) work(jobs context.Context, assigned api.Job) jobOutcome {
	jobID := assigned.Id.String()
	defer a.release(jobID)
	logger := a.options.Logger.With(slog.String("job", jobID), slog.String("source", assigned.Source.Id))
	logger.InfoContext(jobs, "job started")

	ctx, cancel := context.WithTimeout(jobs, time.Duration(assigned.TimeoutSeconds)*time.Second)
	defer cancel()

	var stage atomic.Value
	stage.Store(api.StageResolve)
	var abandoned atomic.Bool
	heartbeatCtx, stopHeartbeat := context.WithCancel(context.WithoutCancel(ctx))
	var heartbeats sync.WaitGroup
	heartbeats.Go(func() {
		a.heartbeat(heartbeatCtx, assigned.Id, &stage, logger, func() {
			abandoned.Store(true)
			cancel()
		})
	})

	result := a.execute(ctx, assigned, &stage, logger)
	stopHeartbeat()
	heartbeats.Wait()

	if abandoned.Load() {
		logger.WarnContext(jobs, "job stopped without a result, because this runner no longer holds it")
		return jobOutcome{status: api.JobResultStatusError}
	}
	delivered := a.submit(jobs, assigned.Id, result, logger)
	logger.InfoContext(jobs, "job finished", slog.String("status", string(result.Status)))
	return jobOutcome{status: result.Status, delivered: delivered}
}

func (a *Agent) execute(ctx context.Context, assigned api.Job, stage *atomic.Value, logger *slog.Logger) (result api.JobResult) {
	// A panic's value can hold anything the job was working on, so it stays out of the logs.
	defer func() {
		if recover() != nil {
			logger.ErrorContext(ctx, "job crashed")
			result = ToResult(job.ErrorResult(job.RunnerError("runner_failed", "The runner hit an unexpected problem. Check its logs and try again.", nil)), job.PlainTableKey)
		}
	}()
	return a.options.Run(ctx, assigned, JobContext{
		OnStage:     func(current job.Stage) { stage.Store(api.Stage(current)) },
		RunningJobs: a.runningJobs,
	})
}

func (a *Agent) heartbeat(ctx context.Context, jobID api.JobId, stage *atomic.Value, logger *slog.Logger, abandon func()) {
	ticker := time.NewTicker(a.heartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		current, _ := stage.Load().(api.Stage)
		response, err := a.options.Client.Heartbeat(ctx, jobID, current)
		switch {
		case errors.Is(err, api.ErrLeaseLost):
			abandon()
			return
		case err != nil:
			if ctx.Err() == nil {
				logger.WarnContext(ctx, "send heartbeat", slog.Any("error", err))
			}
		case response.Cancel:
			logger.InfoContext(ctx, "Backproof cancelled this job")
			abandon()
			return
		}
	}
}

// submit keeps trying after a drain or an abort, because the job's work is already done.
func (a *Agent) submit(jobs context.Context, jobID api.JobId, result api.JobResult, logger *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(jobs), resultTimeout)
	defer cancel()

	for attempt := 1; attempt <= resultAttempts; attempt++ {
		err := a.options.Client.SubmitResult(ctx, jobID, result)
		if err == nil {
			return true
		}
		if errors.Is(err, api.ErrLeaseLost) {
			logger.WarnContext(ctx, "result not accepted, because this runner no longer holds the job")
			return false
		}
		var problem *api.ResponseError
		if errors.As(err, &problem) && !problem.Retryable() {
			logger.ErrorContext(ctx, "result not accepted", slog.Int("status", problem.Status), slog.String("code", problem.Code))
			return false
		}
		logger.WarnContext(ctx, "send result", slog.Int("attempt", attempt), slog.Any("error", err))
		if attempt < resultAttempts {
			a.sleep(ctx, a.backoff(attempt, problem))
		}
	}
	logger.ErrorContext(ctx, "gave up sending the result, so Backproof will run this job again")
	return false
}

// sleepWithJitter spreads runners out, so they don't all call the API at the same moment.
func sleepWithJitter(ctx context.Context, wait time.Duration) {
	jitter := time.Duration(rand.Int64N(int64(wait/10) + 1)) //nolint:gosec // Timing only, nothing secret depends on it.
	timer := time.NewTimer(wait + jitter)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
