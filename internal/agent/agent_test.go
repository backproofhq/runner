package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/job"
)

// fakeBackproof answers as the contract says, and fails the test on a request that breaks it.
type fakeBackproof struct {
	t       *testing.T
	schemas openapi3.Schemas

	mu         sync.Mutex
	queue      []api.Job
	statuses   []string
	heartbeats []string
	results    []api.JobResult
	attempts   int

	pollAnswer      func() (int, string)
	heartbeatAnswer func() (int, string)
	resultAnswer    func(attempt int) int
}

func newFakeBackproof(t *testing.T) (*fakeBackproof, *api.RunnerClient) {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromFile("../../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	fake := &fakeBackproof{t: t, schemas: document.Components.Schemas}
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)

	client, err := api.NewAuthorized(server.URL, "1.2.3", config.NewSecret("bp_run_test"))
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	return fake, client
}

func (f *fakeBackproof) validated(r *http.Request, schema string) []byte {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Errorf("read request: %v", err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		f.t.Errorf("decode request: %v", err)
	}
	if err := f.schemas[schema].Value.VisitJSON(value); err != nil {
		f.t.Errorf("%s breaks the contract: %v", schema, err)
	}
	return body
}

func (f *fakeBackproof) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.Header.Get("Authorization") != "Bearer bp_run_test" {
		f.t.Errorf("Authorization header is missing on %s", r.URL.Path)
	}
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/runners/poll":
		f.poll(w, r)
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		f.heartbeat(w, r)
	case strings.HasSuffix(r.URL.Path, "/result"):
		f.result(w, r)
	default:
		f.t.Errorf("unexpected request to %s", r.URL.Path)
	}
}

func (f *fakeBackproof) poll(w http.ResponseWriter, r *http.Request) {
	var request api.PollRequest
	if err := json.Unmarshal(f.validated(r, "PollRequest"), &request); err != nil {
		f.t.Errorf("decode poll: %v", err)
	}
	f.statuses = append(f.statuses, string(request.Status))

	w.Header().Set("Backproof-Latest-Version", "1.2.3")
	w.Header().Set("Backproof-Min-Version", "1.0.0")
	w.Header().Set("Backproof-Next-Poll-After", "30")
	if f.pollAnswer != nil {
		status, body := f.pollAnswer()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	if len(f.queue) == 0 || request.Status == api.Busy {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	next := f.queue[0]
	f.queue = f.queue[1:]
	_ = json.NewEncoder(w).Encode(next)
}

func (f *fakeBackproof) heartbeat(w http.ResponseWriter, r *http.Request) {
	var request api.HeartbeatRequest
	if err := json.Unmarshal(f.validated(r, "HeartbeatRequest"), &request); err != nil {
		f.t.Errorf("decode heartbeat: %v", err)
	}
	f.heartbeats = append(f.heartbeats, string(request.Stage))
	if f.heartbeatAnswer != nil {
		status, body := f.heartbeatAnswer()
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
		return
	}
	_, _ = fmt.Fprintf(w, `{"lease_expires_at":%q,"cancel":false}`, time.Now().Add(10*time.Minute).Format(time.RFC3339))
}

func (f *fakeBackproof) result(w http.ResponseWriter, r *http.Request) {
	var result api.JobResult
	if err := json.Unmarshal(f.validated(r, "JobResult"), &result); err != nil {
		f.t.Errorf("decode result: %v", err)
	}
	f.attempts++
	status := http.StatusAccepted
	if f.resultAnswer != nil {
		status = f.resultAnswer(f.attempts)
	}
	if status == http.StatusAccepted {
		f.results = append(f.results, result)
	}
	w.WriteHeader(status)
	if status != http.StatusAccepted {
		_, _ = io.WriteString(w, `{"code":"problem","message":"Not now."}`)
	}
}

func (f *fakeBackproof) snapshot() (results []api.JobResult, heartbeats, statuses []string, attempts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]api.JobResult{}, f.results...), append([]string{}, f.heartbeats...), append([]string{}, f.statuses...), f.attempts
}

func (f *fakeBackproof) enqueue(jobs ...api.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.queue = append(f.queue, jobs...)
}

func testJob(t *testing.T, number int) api.Job {
	t.Helper()
	var id api.JobId
	if err := id.UnmarshalText(fmt.Appendf(nil, "7b1f6c1e-3f0a-4a53-9a55-%012d", number)); err != nil {
		t.Fatalf("create job id: %v", err)
	}
	return api.Job{
		Id:             id,
		LeaseExpiresAt: time.Now().Add(10 * time.Minute),
		TimeoutSeconds: 3600,
		Source: api.Source{
			Id:             "PROD_DB",
			Endpoint:       "https://s3.example.test",
			Bucket:         "backups",
			ExpectedFormat: api.SourceExpectedFormatAuto,
			Encryption:     api.SourceEncryptionNone,
		},
		Checks: api.CheckConfig{
			ExpectedTables:  []string{},
			Baseline:        []api.TableRowCount{},
			TableTolerances: []api.TableTolerance{},
			RowCountMode:    api.Exact,
		},
	}
}

func passed() api.JobResult {
	return ToResult(job.Result{SchemaVersion: 1, Status: job.StatusPass}, job.PlainTableKey)
}

type testAgent struct {
	agent *Agent
	logs  *syncBuffer
	drain context.CancelFunc
	abort context.CancelFunc
	done  chan struct{}
	err   error
}

type syncBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func startAgent(t *testing.T, client *api.RunnerClient, concurrency int, run Runner) *testAgent {
	t.Helper()
	logs := &syncBuffer{}
	agent := New(Options{
		Client:       client,
		Run:          run,
		Logger:       slog.New(slog.NewJSONHandler(logs, nil)),
		Concurrency:  concurrency,
		Version:      "1.2.3",
		Capabilities: DefaultCapabilities(),
	})
	agent.heartbeatInterval = 5 * time.Millisecond
	agent.retryDelay = time.Millisecond
	agent.sleep = func(ctx context.Context, _ time.Duration) {
		select {
		case <-ctx.Done():
		case <-time.After(time.Millisecond):
		}
	}

	ctx, drain := context.WithCancel(context.Background())
	jobs, abort := context.WithCancel(context.Background())
	running := &testAgent{agent: agent, logs: logs, drain: drain, abort: abort, done: make(chan struct{})}
	go func() {
		running.err = agent.Run(ctx, jobs)
		close(running.done)
	}()
	t.Cleanup(func() {
		drain()
		abort()
		select {
		case <-running.done:
		case <-time.After(10 * time.Second):
			t.Error("the agent did not stop")
		}
	})
	return running
}

func waitUntil(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting until %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAgentRunsAJobAndReportsItsStageAndResult(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1))
	release := make(chan struct{})

	startAgent(t, client, 1, func(_ context.Context, _ api.Job, jobContext JobContext) api.JobResult {
		jobContext.OnStage(job.StageRestore)
		<-release
		return passed()
	})

	waitUntil(t, "a heartbeat reports the restore stage", func() bool {
		_, heartbeats, _, _ := fake.snapshot()
		for _, stage := range heartbeats {
			if stage == "restore" {
				return true
			}
		}
		return false
	})
	close(release)
	waitUntil(t, "the result arrives", func() bool {
		results, _, _, _ := fake.snapshot()
		return len(results) == 1
	})
}

func TestAgentReportsBusyAndClaimsNothingWhileItIsFull(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1), testJob(t, 2))
	release := make(chan struct{})
	var started atomic.Int32

	startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult {
		started.Add(1)
		<-release
		return passed()
	})

	waitUntil(t, "the agent reports busy", func() bool {
		_, _, statuses, _ := fake.snapshot()
		return len(statuses) > 3 && statuses[len(statuses)-1] == "busy"
	})
	if started.Load() != 1 {
		t.Errorf("%d jobs started, want only 1", started.Load())
	}
	close(release)
	waitUntil(t, "both jobs finish", func() bool {
		results, _, _, _ := fake.snapshot()
		return len(results) == 2
	})
}

func TestAgentRunsUpToItsConcurrency(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1), testJob(t, 2), testJob(t, 3), testJob(t, 4))
	release := make(chan struct{})
	var running, most atomic.Int32
	var others sync.Map

	startAgent(t, client, 2, func(_ context.Context, assigned api.Job, jobContext JobContext) api.JobResult {
		current := running.Add(1)
		defer running.Add(-1)
		for {
			seen := most.Load()
			if current <= seen || most.CompareAndSwap(seen, current) {
				break
			}
		}
		others.Store(assigned.Id.String(), jobContext.RunningJobs())
		<-release
		return passed()
	})

	waitUntil(t, "two jobs run at once", func() bool { return running.Load() == 2 })
	time.Sleep(20 * time.Millisecond)
	close(release)
	waitUntil(t, "all jobs finish", func() bool {
		results, _, _, _ := fake.snapshot()
		return len(results) == 4
	})
	if most.Load() != 2 {
		t.Errorf("%d jobs ran at once, want at most 2", most.Load())
	}
}

func TestAgentStopsAJobItNoLongerHolds(t *testing.T) {
	answers := map[string]func() (int, string){
		"lease lost": func() (int, string) { return http.StatusConflict, `{"code":"lease_lost","message":"Gone."}` },
		"job gone":   func() (int, string) { return http.StatusNotFound, `{"code":"not_found","message":"Gone."}` },
		"cancelled": func() (int, string) {
			return http.StatusOK, fmt.Sprintf(`{"lease_expires_at":%q,"cancel":true}`, time.Now().Format(time.RFC3339))
		},
	}
	for name, answer := range answers {
		t.Run(name, func(t *testing.T) {
			fake, client := newFakeBackproof(t)
			fake.heartbeatAnswer = answer
			fake.enqueue(testJob(t, 1))
			stopped := make(chan struct{})

			startAgent(t, client, 1, func(ctx context.Context, _ api.Job, _ JobContext) api.JobResult {
				<-ctx.Done()
				close(stopped)
				return passed()
			})

			select {
			case <-stopped:
			case <-time.After(10 * time.Second):
				t.Fatal("the job kept running")
			}
			time.Sleep(20 * time.Millisecond)
			if _, _, _, attempts := fake.snapshot(); attempts != 0 {
				t.Errorf("%d results sent for a job this runner no longer holds", attempts)
			}
		})
	}
}

func TestDrainFinishesTheRunningJobAndClaimsNoMore(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1))
	started := make(chan struct{})
	release := make(chan struct{})
	var cancelled atomic.Bool

	running := startAgent(t, client, 1, func(ctx context.Context, _ api.Job, _ JobContext) api.JobResult {
		close(started)
		select {
		case <-ctx.Done():
			cancelled.Store(true)
		case <-release:
		}
		return passed()
	})
	<-started

	running.drain()
	fake.enqueue(testJob(t, 2))
	time.Sleep(20 * time.Millisecond)
	_, _, before, _ := fake.snapshot()
	time.Sleep(20 * time.Millisecond)
	_, _, after, _ := fake.snapshot()
	if len(after) != len(before) {
		t.Error("the agent kept polling while draining")
	}
	select {
	case <-running.done:
		t.Fatal("the agent stopped before its job finished")
	default:
	}

	close(release)
	select {
	case <-running.done:
		if running.err != nil {
			t.Errorf("Run() error = %v", running.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent did not stop after its job finished")
	}
	results, _, _, _ := fake.snapshot()
	if len(results) != 1 {
		t.Errorf("%d results sent, want the running job's result", len(results))
	}
	if cancelled.Load() {
		t.Error("draining cancelled the running job")
	}
}

func TestAbortStopsTheRunningJobAndStillReportsIt(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1))
	started := make(chan struct{})

	running := startAgent(t, client, 1, func(ctx context.Context, _ api.Job, _ JobContext) api.JobResult {
		close(started)
		<-ctx.Done()
		return ToResult(job.ErrorResult(job.RunnerError("cancelled", "Stopped.", nil)), job.PlainTableKey)
	})
	<-started

	running.drain()
	running.abort()

	waitUntil(t, "the stopped job is reported", func() bool {
		results, _, _, _ := fake.snapshot()
		return len(results) == 1 && results[0].Status == api.JobResultStatusError
	})
}

func TestOutdatedRunnerGetsNoJobsAndLogsWhyOnce(t *testing.T) {
	const message = "This runner is too old to receive jobs. Update it with the install script."
	fake, client := newFakeBackproof(t)
	fake.pollAnswer = func() (int, string) {
		return http.StatusUpgradeRequired, fmt.Sprintf(`{"code":"update_required","message":%q}`, message)
	}
	var started atomic.Int32

	running := startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult {
		started.Add(1)
		return passed()
	})

	waitUntil(t, "the agent has polled several times", func() bool {
		_, _, statuses, _ := fake.snapshot()
		return len(statuses) > 3
	})
	if started.Load() != 0 {
		t.Error("an outdated runner started a job")
	}
	if count := strings.Count(running.logs.String(), message); count != 1 {
		t.Errorf("the update notice was logged %d times, want once", count)
	}
}

func TestRejectedTokenStopsTheAgent(t *testing.T) {
	const message = "This runner was revoked. Register it again."
	fake, client := newFakeBackproof(t)
	fake.pollAnswer = func() (int, string) {
		return http.StatusUnauthorized, fmt.Sprintf(`{"code":"invalid_token","message":%q}`, message)
	}

	running := startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult { return passed() })

	select {
	case <-running.done:
		if running.err == nil || !strings.Contains(running.err.Error(), message) {
			t.Errorf("Run() error = %v, want the API's explanation", running.err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the agent kept running with a rejected token")
	}
}

func TestAgentKeepsPollingWhenTheAPIFails(t *testing.T) {
	fake, client := newFakeBackproof(t)
	var failures atomic.Int32
	fake.pollAnswer = func() (int, string) {
		if failures.Add(1) <= 3 {
			return http.StatusServiceUnavailable, `{"code":"down","message":"Try later."}`
		}
		fake.pollAnswer = nil
		return http.StatusNoContent, ""
	}
	fake.enqueue(testJob(t, 1))

	startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult { return passed() })

	waitUntil(t, "the job runs after the API recovers", func() bool {
		results, _, _, _ := fake.snapshot()
		return len(results) == 1
	})
}

func TestResultIsSentAgainAfterAFailure(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.resultAnswer = func(attempt int) int {
		if attempt < 3 {
			return http.StatusServiceUnavailable
		}
		return http.StatusAccepted
	}
	fake.enqueue(testJob(t, 1))

	startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult { return passed() })

	waitUntil(t, "the result is accepted", func() bool {
		results, _, _, attempts := fake.snapshot()
		return len(results) == 1 && attempts == 3
	})
}

func TestResultIsNotRepeatedWhenTheAPIRefusesIt(t *testing.T) {
	for name, status := range map[string]int{"lease lost": http.StatusConflict, "invalid": http.StatusBadRequest} {
		t.Run(name, func(t *testing.T) {
			fake, client := newFakeBackproof(t)
			fake.resultAnswer = func(int) int { return status }
			fake.enqueue(testJob(t, 1))

			startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult { return passed() })

			waitUntil(t, "the result is sent", func() bool {
				_, _, _, attempts := fake.snapshot()
				return attempts > 0
			})
			time.Sleep(20 * time.Millisecond)
			if _, _, _, attempts := fake.snapshot(); attempts != 1 {
				t.Errorf("the result was sent %d times, want once", attempts)
			}
		})
	}
}

func TestJobStopsAtItsTimeLimit(t *testing.T) {
	fake, client := newFakeBackproof(t)
	assigned := testJob(t, 1)
	assigned.TimeoutSeconds = 1
	fake.enqueue(assigned)
	var limit atomic.Int64

	startAgent(t, client, 1, func(ctx context.Context, _ api.Job, _ JobContext) api.JobResult {
		deadline, _ := ctx.Deadline()
		limit.Store(int64(time.Until(deadline)))
		return passed()
	})

	waitUntil(t, "the job runs", func() bool { return limit.Load() != 0 })
	if got := time.Duration(limit.Load()); got <= 0 || got > time.Second {
		t.Errorf("the job's time limit is %s, want at most 1s", got)
	}
}

func TestACrashedJobIsReportedAndTheAgentCarriesOn(t *testing.T) {
	const canary = "private-row-value"
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1), testJob(t, 2))
	var runs atomic.Int32

	running := startAgent(t, client, 1, func(context.Context, api.Job, JobContext) api.JobResult {
		if runs.Add(1) == 1 {
			panic(canary)
		}
		return passed()
	})

	waitUntil(t, "both jobs are reported", func() bool {
		results, _, _, _ := fake.snapshot()
		return len(results) == 2
	})
	results, _, _, _ := fake.snapshot()
	if results[0].Status != api.JobResultStatusError {
		t.Errorf("crashed job = %s, want error", results[0].Status)
	}
	if strings.Contains(running.logs.String(), canary) {
		t.Error("the logs contain what the job crashed on")
	}
}

func onceAgent(t *testing.T, client *api.RunnerClient, run Runner) *Agent {
	t.Helper()
	agent := New(Options{
		Client:       client,
		Run:          run,
		Logger:       slog.New(slog.DiscardHandler),
		Concurrency:  1,
		Version:      "1.2.3",
		Capabilities: DefaultCapabilities(),
	})
	agent.heartbeatInterval = 5 * time.Millisecond
	agent.retryDelay = time.Millisecond
	agent.sleep = func(context.Context, time.Duration) {}
	return agent
}

func withStatus(status job.Status) api.JobResult {
	return ToResult(job.Result{SchemaVersion: 1, Status: status}, job.PlainTableKey)
}

func TestRunOnceRunsEveryDueJobOneAfterAnother(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1), testJob(t, 2), testJob(t, 3))
	statuses := []job.Status{job.StatusPass, job.StatusFail, job.StatusError}
	var running, runs atomic.Int32

	summary, err := onceAgent(t, client, func(context.Context, api.Job, JobContext) api.JobResult {
		if running.Add(1) != 1 {
			t.Error("two jobs ran at the same time")
		}
		defer running.Add(-1)
		return withStatus(statuses[runs.Add(1)-1])
	}).RunOnce(t.Context(), t.Context())

	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if summary != (OnceSummary{Passed: 1, Failed: 1, Errors: 1}) {
		t.Errorf("summary = %+v", summary)
	}
	if results, _, _, _ := fake.snapshot(); len(results) != 3 {
		t.Errorf("%d results reported, want 3", len(results))
	}
}

func TestRunOnceWithNothingDueExitsCleanly(t *testing.T) {
	fake, client := newFakeBackproof(t)

	summary, err := onceAgent(t, client, func(context.Context, api.Job, JobContext) api.JobResult {
		t.Error("a job ran although none was due")
		return passed()
	}).RunOnce(t.Context(), t.Context())

	if err != nil || summary != (OnceSummary{}) || summary.ExitCode(err) != ExitPassed {
		t.Errorf("RunOnce() = %+v, %v, want a clean exit", summary, err)
	}
	if _, _, statuses, _ := fake.snapshot(); len(statuses) != 1 {
		t.Errorf("%d polls, want 1", len(statuses))
	}
}

func TestOnceExitCodes(t *testing.T) {
	problem := fmt.Errorf("the API is down")
	tests := []struct {
		name    string
		summary OnceSummary
		err     error
		want    int
	}{
		{"nothing due", OnceSummary{}, nil, ExitPassed},
		{"all passed", OnceSummary{Passed: 3}, nil, ExitPassed},
		{"a backup failed", OnceSummary{Passed: 2, Failed: 1}, nil, ExitBackupFailed},
		{"the runner hit an error", OnceSummary{Passed: 2, Errors: 1}, nil, ExitRunnerError},
		{"the run stopped early", OnceSummary{Passed: 2}, problem, ExitRunnerError},
		{"a failed backup outweighs an error", OnceSummary{Failed: 1, Errors: 1}, nil, ExitBackupFailed},
		{"a failed backup outweighs a stopped run", OnceSummary{Failed: 1}, problem, ExitBackupFailed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.summary.ExitCode(test.err); got != test.want {
				t.Errorf("ExitCode() = %d, want %d", got, test.want)
			}
		})
	}
}

func TestRunOnceStopsWhenTheRunnerIsOutdatedOrRevoked(t *testing.T) {
	tests := map[string]struct {
		status  int
		message string
	}{
		"outdated": {http.StatusUpgradeRequired, "This runner is too old to receive jobs. Update it with the install script."},
		"revoked":  {http.StatusUnauthorized, "This runner was revoked. Register it again."},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fake, client := newFakeBackproof(t)
			fake.pollAnswer = func() (int, string) {
				return test.status, fmt.Sprintf(`{"code":"refused","message":%q}`, test.message)
			}

			summary, err := onceAgent(t, client, func(context.Context, api.Job, JobContext) api.JobResult {
				t.Error("a refused runner ran a job")
				return passed()
			}).RunOnce(t.Context(), t.Context())

			if err == nil || !strings.Contains(err.Error(), test.message) {
				t.Errorf("error = %v, want the API's explanation", err)
			}
			if summary.ExitCode(err) != ExitRunnerError {
				t.Errorf("exit code = %d, want %d", summary.ExitCode(err), ExitRunnerError)
			}
			if _, _, statuses, _ := fake.snapshot(); len(statuses) != 1 {
				t.Errorf("%d polls, want it to stop at once", len(statuses))
			}
		})
	}
}

func TestRunOnceRetriesBrieflyWhenTheAPIFails(t *testing.T) {
	down := func() (int, string) { return http.StatusServiceUnavailable, `{"code":"down","message":"Try later."}` }

	t.Run("recovers", func(t *testing.T) {
		fake, client := newFakeBackproof(t)
		var calls atomic.Int32
		fake.pollAnswer = func() (int, string) {
			if calls.Add(1) < oncePollAttempts {
				return down()
			}
			return http.StatusNoContent, ""
		}
		if _, err := onceAgent(t, client, nil).RunOnce(t.Context(), t.Context()); err != nil {
			t.Errorf("RunOnce() error = %v, want it to recover", err)
		}
	})

	t.Run("gives up", func(t *testing.T) {
		fake, client := newFakeBackproof(t)
		fake.pollAnswer = down
		summary, err := onceAgent(t, client, nil).RunOnce(t.Context(), t.Context())
		if err == nil || summary.ExitCode(err) != ExitRunnerError {
			t.Errorf("RunOnce() = %+v, %v, want a runner error", summary, err)
		}
		if _, _, statuses, _ := fake.snapshot(); len(statuses) != oncePollAttempts {
			t.Errorf("%d polls, want %d", len(statuses), oncePollAttempts)
		}
	})
}

func TestRunOnceCountsAResultBackproofNeverReceived(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.resultAnswer = func(int) int { return http.StatusConflict }
	fake.enqueue(testJob(t, 1))

	summary, err := onceAgent(t, client, func(context.Context, api.Job, JobContext) api.JobResult { return passed() }).RunOnce(t.Context(), t.Context())

	if err != nil {
		t.Fatalf("RunOnce() error = %v", err)
	}
	if summary.ExitCode(err) != ExitRunnerError {
		t.Errorf("summary = %+v, want a runner error for the lost result", summary)
	}
}

func TestRunOnceCountsAJobItLost(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.heartbeatAnswer = func() (int, string) { return http.StatusConflict, `{"code":"lease_lost","message":"Gone."}` }
	fake.enqueue(testJob(t, 1))

	summary, err := onceAgent(t, client, func(ctx context.Context, _ api.Job, _ JobContext) api.JobResult {
		<-ctx.Done()
		return passed()
	}).RunOnce(t.Context(), t.Context())

	if err != nil || summary != (OnceSummary{Errors: 1}) {
		t.Errorf("RunOnce() = %+v, %v, want one error", summary, err)
	}
}

func TestRunOnceClaimsNothingAfterAStopSignal(t *testing.T) {
	fake, client := newFakeBackproof(t)
	fake.enqueue(testJob(t, 1), testJob(t, 2))
	ctx, drain := context.WithCancel(t.Context())
	defer drain()
	var runs atomic.Int32

	summary, err := onceAgent(t, client, func(context.Context, api.Job, JobContext) api.JobResult {
		runs.Add(1)
		drain()
		return passed()
	}).RunOnce(ctx, t.Context())

	if runs.Load() != 1 {
		t.Errorf("%d jobs ran, want the running job to finish and no more", runs.Load())
	}
	if summary.Passed != 1 || err == nil || summary.ExitCode(err) != ExitRunnerError {
		t.Errorf("RunOnce() = %+v, %v, want the stopped run reported", summary, err)
	}
}
