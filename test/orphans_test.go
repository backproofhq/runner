//go:build integration

package test

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/client"

	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

const (
	orphanTestMajor = 17

	helperRunnerEnv = "BACKPROOF_TEST_HELPER_RUNNER"
	helperDumpEnv   = "BACKPROOF_TEST_HELPER_DUMP"
)

// slowCopy makes a real dump take minutes to restore, so a test can interrupt it at any moment.
func slowCopy(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	footer := []byte("-- PostgreSQL database dump complete")
	slow := bytes.Replace(contents, footer, append([]byte("SELECT pg_sleep(600);\n"), footer...), 1)
	if bytes.Equal(slow, contents) {
		t.Fatal("dump has no footer")
	}
	target := filepath.Join(t.TempDir(), "slow.sql")
	if err := os.WriteFile(target, slow, 0o600); err != nil { //nolint:gosec // The path is inside the test's temp directory.
		t.Fatalf("write dump: %v", err)
	}
	return target
}

func (r *testRunner) resources(t *testing.T) (containers, volumes int) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	filters := make(client.Filters).Add("label", docker.LabelRunner+"="+r.id)

	listed, err := r.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	found, err := r.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	return len(listed.Items), len(found.Items)
}

func (r *testRunner) waitForRestore(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if containers, _ := r.resources(t); containers > 0 {
			// Gives the restore time to reach the slow statement.
			time.Sleep(3 * time.Second)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the restore container never started")
}

// jobIDs returns every job that still has a container or volume on this runner.
func (r *testRunner) jobIDs(t *testing.T) map[string]bool {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	filters := make(client.Filters).Add("label", docker.LabelRunner+"="+r.id)

	containers, err := r.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	volumes, err := r.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}

	jobs := make(map[string]bool)
	for _, item := range containers.Items {
		jobs[item.Labels[docker.LabelJob]] = true
	}
	for _, item := range volumes.Items {
		jobs[item.Labels[docker.LabelJob]] = true
	}
	return jobs
}

// backgroundRestore runs a restore that the test can stop, and waits for it when stopped.
func (r *testRunner) backgroundRestore(t *testing.T, path string) (stop func() job.Result) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	results := make(chan job.Result, 1)
	go func() { results <- r.restoreWithContext(ctx, t, path, job.Checks{}) }()

	return func() job.Result {
		t.Helper()
		cancel()
		select {
		case result := <-results:
			return result
		case <-time.After(2 * time.Minute):
			t.Fatal("the restore did not stop after it was cancelled")
			return job.Result{}
		}
	}
}

// TestKilledRunnerHelper is the runner process that TestNextRunRemovesWhatAKilledRunLeft kills.
func TestKilledRunnerHelper(t *testing.T) {
	runnerID := os.Getenv(helperRunnerEnv)
	if runnerID == "" {
		t.Skip("only runs as a child process of TestNextRunRemovesWhatAKilledRunLeft")
	}
	dockerClient, err := docker.New(runnerID)
	if err != nil {
		t.Fatalf("create docker client: %v", err)
	}
	logger := slog.New(slog.DiscardHandler)
	job.RunLocal(context.Background(), postgres.New(dockerClient, docker.Limits{}), logger, randomID(t), os.Getenv(helperDumpEnv), job.Checks{})
}

func TestNextRunRemovesWhatAKilledRunLeft(t *testing.T) {
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, orphanTestMajor)

	helper := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestKilledRunnerHelper$") //nolint:gosec // The test runs its own binary.
	helper.Env = append(os.Environ(), helperRunnerEnv+"="+runner.id, helperDumpEnv+"="+slowCopy(t, dumps.healthy.plain))
	if err := helper.Start(); err != nil {
		t.Fatalf("start runner: %v", err)
	}
	t.Cleanup(func() { _ = runner.docker.RemoveOrphans(context.WithoutCancel(t.Context())) })

	runner.waitForRestore(t)
	if err := helper.Process.Kill(); err != nil {
		t.Fatalf("kill runner: %v", err)
	}
	_ = helper.Wait()

	containers, volumes := runner.resources(t)
	if containers == 0 || volumes == 0 {
		t.Fatalf("the killed run left %d containers and %d volumes, want some to prove the cleanup", containers, volumes)
	}

	orphans := runner.jobIDs(t)
	stop := runner.backgroundRestore(t, slowCopy(t, dumps.healthy.plain))
	deadline := time.Now().Add(2 * time.Minute)
	for {
		current := runner.jobIDs(t)
		started := false
		for id := range current {
			started = started || !orphans[id]
		}
		if started {
			// The next run clears the leftovers before it creates anything of its own.
			for id := range orphans {
				if current[id] {
					t.Errorf("job %s was still there when the next run started", id)
				}
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the next run never started")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()

	result := runner.restore(t, dumps.healthy.custom)

	if result.Status != job.StatusPass {
		t.Errorf("next run = %s (%s): %s", result.Status, result.ReasonCode, result.Reason)
	}
	runner.assertNothingLeft(t)
}

func TestFinishedJobRemovesOrphansThatAppearedDuringIt(t *testing.T) {
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, orphanTestMajor)

	stop := runner.backgroundRestore(t, slowCopy(t, dumps.healthy.plain))
	runner.waitForRestore(t)
	if _, err := runner.docker.Start(t.Context(), docker.ContainerSpec{
		JobID:      randomID(t),
		Image:      "postgres:17",
		Entrypoint: []string{"sleep"},
		Cmd:        []string{"infinity"},
	}); err != nil {
		t.Fatalf("start orphan: %v", err)
	}

	stop()
	runner.assertNothingLeft(t)
}

func TestCancelledRestoreRemovesEverything(t *testing.T) {
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, orphanTestMajor)

	stop := runner.backgroundRestore(t, slowCopy(t, dumps.healthy.plain))
	runner.waitForRestore(t)

	result := stop()

	if result.Status != job.StatusError || result.ReasonCode != "cancelled" {
		t.Errorf("result = %s (%s), want a cancelled runner error", result.Status, result.ReasonCode)
	}
	runner.assertNothingLeft(t)
}

func TestOrphanCleanupKeepsRunningJobsAndOtherRunners(t *testing.T) {
	ctx := t.Context()
	runner, other := newTestRunner(t), newTestRunner(t)
	t.Cleanup(func() {
		cleanupCtx := context.WithoutCancel(ctx)
		_ = runner.docker.RemoveOrphans(cleanupCtx)
		_ = other.docker.RemoveOrphans(cleanupCtx)
	})

	start := func(target *testRunner, jobID string) {
		t.Helper()
		_, err := target.docker.Start(ctx, docker.ContainerSpec{
			JobID:      jobID,
			Image:      "postgres:17",
			Entrypoint: []string{"sleep"},
			Cmd:        []string{"infinity"},
		})
		if err != nil {
			t.Fatalf("start container: %v", err)
		}
	}
	running, orphan := randomID(t), randomID(t)
	start(runner, running)
	start(runner, orphan)
	start(other, randomID(t))

	if err := runner.docker.RemoveOrphans(ctx, running); err != nil {
		t.Fatalf("remove orphans: %v", err)
	}

	filters := make(client.Filters).Add("label", docker.LabelRunner+"="+runner.id)
	containers, err := runner.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers.Items) != 1 || containers.Items[0].Labels[docker.LabelJob] != running {
		t.Errorf("%d containers remain, want only the running job", len(containers.Items))
	}
	volumes, err := runner.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(volumes.Items) != 1 || volumes.Items[0].Labels[docker.LabelJob] != running {
		t.Errorf("%d volumes remain, want only the running job's volume", len(volumes.Items))
	}
	if containers, volumes := other.resources(t); containers != 1 || volumes != 1 {
		t.Errorf("the other runner has %d containers and %d volumes, want both kept", containers, volumes)
	}
}
