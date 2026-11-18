//go:build integration

package test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/getkin/kin-openapi/openapi3"
	"github.com/moby/moby/client"

	"github.com/backproofhq/runner/internal/agent"
	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

// agentAPI hands out jobs and records results, checking every result against the contract.
type agentAPI struct {
	t      *testing.T
	schema *openapi3.Schema

	mu      sync.Mutex
	queue   []api.Job
	results map[string]string
}

func (a *agentAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	defer a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/v1/runners/poll":
		w.Header().Set("Backproof-Latest-Version", "dev")
		w.Header().Set("Backproof-Min-Version", "0.0.0")
		w.Header().Set("Backproof-Next-Poll-After", "1")
		if len(a.queue) == 0 {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next := a.queue[0]
		a.queue = a.queue[1:]
		_ = json.NewEncoder(w).Encode(next)
	case strings.HasSuffix(r.URL.Path, "/heartbeat"):
		_, _ = fmt.Fprintf(w, `{"lease_expires_at":%q,"cancel":false}`, time.Now().Add(10*time.Minute).Format(time.RFC3339))
	case strings.HasSuffix(r.URL.Path, "/result"):
		body, err := io.ReadAll(r.Body)
		if err != nil {
			a.t.Errorf("read result: %v", err)
		}
		var value any
		if err := json.Unmarshal(body, &value); err != nil {
			a.t.Errorf("decode result: %v", err)
		}
		if err := a.schema.VisitJSON(value); err != nil {
			a.t.Errorf("result breaks the contract: %v", err)
		}
		a.results[strings.Split(r.URL.Path, "/")[3]] = string(body)
		w.WriteHeader(http.StatusAccepted)
	}
}

func (a *agentAPI) submit(assigned api.Job) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queue = append(a.queue, assigned)
}

func (a *agentAPI) waitForResult(t *testing.T, id string) (api.JobResult, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		body, found := a.results[id]
		a.mu.Unlock()
		if found {
			var result api.JobResult
			if err := json.Unmarshal([]byte(body), &result); err != nil {
				t.Fatalf("decode result: %v", err)
			}
			return result, body
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the agent never reported a result")
	return api.JobResult{}, ""
}

func agentJob(t *testing.T, number int, storage *testStorage, sourceID, prefix string, showNames bool, checks api.CheckConfig) api.Job {
	t.Helper()
	var id api.JobId
	if err := id.UnmarshalText(fmt.Appendf(nil, "7b1f6c1e-3f0a-4a53-9a55-%012d", number)); err != nil {
		t.Fatalf("create job id: %v", err)
	}
	return api.Job{
		Id:             id,
		LeaseExpiresAt: time.Now().Add(10 * time.Minute),
		TimeoutSeconds: 600,
		Source: api.Source{
			Id:             sourceID,
			Endpoint:       storage.endpoint,
			Bucket:         testBucket,
			Prefix:         prefix,
			ExpectedFormat: api.SourceExpectedFormatAuto,
			Encryption:     api.SourceEncryptionNone,
			ShowTableNames: showNames,
		},
		Checks: checks,
	}
}

func emptyChecks() api.CheckConfig {
	return api.CheckConfig{
		ExpectedTables:   []string{},
		Baseline:         []api.TableRowCount{},
		TableTolerances:  []api.TableTolerance{},
		RowCountMode:     api.Exact,
		TolerancePercent: 10,
	}
}

func TestAgentRunsJobsFromStorageToResult(t *testing.T) {
	storage := startStorage(t)
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, postgres.NewestMajor)
	storage.put(t, "prod/backup.dump", encodeGzip(t, readFixture(t, dumps.healthy.custom)))

	document, err := openapi3.NewLoader().LoadFromFile("../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	backproof := &agentAPI{t: t, schema: document.Components.Schemas["JobResult"].Value, results: make(map[string]string)}
	server := httptest.NewServer(backproof)
	t.Cleanup(server.Close)
	client, err := api.NewAuthorized(server.URL, "dev", config.NewSecret("bp_run_"+randomID(t)))
	if err != nil {
		t.Fatalf("create client: %v", err)
	}

	runnerIdentity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	salt := randomID(t) + randomID(t) + randomID(t)
	secrets := map[string]string{
		"BP_SRC_PROD_DB_S3_ACCESS_KEY": storage.accessKey,
		"BP_SRC_PROD_DB_S3_SECRET_KEY": storage.secretKey,
		"BP_SRC_PROD_DB_NAME_SALT":     salt,
	}
	logger := slog.New(slog.DiscardHandler)
	worker := agent.New(agent.Options{
		Client: client,
		Run: agent.NewJobRunner(agent.JobRunnerOptions{
			Engine:      postgres.New(runner.docker, docker.Limits{}),
			Logger:      logger,
			WorkDir:     runner.workDir,
			Lookup:      func(key string) (string, bool) { value, ok := secrets[key]; return value, ok },
			SourcesFile: "/nonexistent/sources.env",
			RunnerKey:   config.NewSecret(runnerIdentity.String()),
		}),
		Logger:       logger,
		Concurrency:  1,
		Version:      "dev",
		Capabilities: agent.DefaultCapabilities(),
	})

	ctx, drain := context.WithCancel(t.Context())
	jobs, abort := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := worker.Run(ctx, jobs); err != nil {
			t.Errorf("Run() error = %v", err)
		}
	}()
	t.Cleanup(func() {
		drain()
		abort()
		<-done
	})

	hashed := job.HashedTableKey(config.NewSecret(salt))
	customers := hashed(job.TableRef{Schema: "public", Name: "customers"})

	t.Run("hashed table names", func(t *testing.T) {
		assigned := agentJob(t, 1, storage, "PROD_DB", "prod/", false, emptyChecks())
		backproof.submit(assigned)

		result, body := backproof.waitForResult(t, assigned.Id.String())

		if result.Status != api.JobResultStatusPass {
			t.Fatalf("result = %s (%v): %v", result.Status, result.ReasonCode, result.Reason)
		}
		for name := range fixtureTables {
			_, table, _ := strings.Cut(name, ".")
			if strings.Contains(body, table) {
				t.Errorf("the result contains the table name %q", table)
			}
		}
		found := false
		for _, table := range result.Tables {
			found = found || (table.TableKey == customers && table.Rows == fixtureTables["public.customers"])
		}
		if !found {
			t.Errorf("tables = %+v, want the customers table under its hash", result.Tables)
		}
		if *result.Backup.ObjectKey != "prod/backup.dump" || *result.Backup.Compression != api.BackupDetailsCompressionGzip {
			t.Errorf("backup = %+v", result.Backup)
		}
	})

	t.Run("baseline with hashed names", func(t *testing.T) {
		checks := emptyChecks()
		checks.Baseline = []api.TableRowCount{{TableKey: customers, Rows: 300}}
		assigned := agentJob(t, 2, storage, "PROD_DB", "prod/", false, checks)
		backproof.submit(assigned)

		result, _ := backproof.waitForResult(t, assigned.Id.String())

		if result.Status != api.JobResultStatusFail || *result.ReasonCode != "row_count_changed" {
			t.Fatalf("result = %s (%v), want a row count failure", result.Status, result.ReasonCode)
		}
		if *result.FailedStage != api.JobResultFailedStageCheck {
			t.Errorf("failed stage = %s, want check", *result.FailedStage)
		}
	})

	t.Run("readable table names", func(t *testing.T) {
		assigned := agentJob(t, 3, storage, "PROD_DB", "prod/", true, emptyChecks())
		backproof.submit(assigned)

		result, body := backproof.waitForResult(t, assigned.Id.String())

		if result.Status != api.JobResultStatusPass || !strings.Contains(body, "public.customers") {
			t.Errorf("result = %s, want a pass with readable names", result.Status)
		}
	})

	t.Run("keys sealed for this runner", func(t *testing.T) {
		backupKey, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatalf("create backup key: %v", err)
		}
		storage.put(t, "sealed/backup.dump.age", encodeAge(t, backupKey.Recipient(), readFixture(t, dumps.healthy.custom)))

		assigned := agentJob(t, 5, storage, "SEALED_DB", "sealed/", false, emptyChecks())
		bundle := sealBundle(t, runnerIdentity.Recipient(), map[string]any{
			"version":       1,
			"s3_access_key": storage.accessKey,
			"s3_secret_key": storage.secretKey,
			"name_salt":     salt,
			"age_identity":  backupKey.String(),
		})
		assigned.SealedBundle = &bundle
		backproof.submit(assigned)

		result, body := backproof.waitForResult(t, assigned.Id.String())

		if result.Status != api.JobResultStatusPass {
			t.Fatalf("result = %s (%v): %v", result.Status, result.ReasonCode, result.Reason)
		}
		if *result.Backup.Encryption != api.BackupDetailsEncryptionAge {
			t.Errorf("encryption = %s, want age", *result.Backup.Encryption)
		}
		for _, secret := range []string{storage.secretKey, backupKey.String(), salt} {
			if strings.Contains(body, secret) {
				t.Error("the result contains a sealed key")
			}
		}
	})

	t.Run("keys sealed for another runner", func(t *testing.T) {
		stranger, err := age.GenerateX25519Identity()
		if err != nil {
			t.Fatalf("create other key: %v", err)
		}
		assigned := agentJob(t, 6, storage, "SEALED_DB", "sealed/", false, emptyChecks())
		bundle := sealBundle(t, stranger.Recipient(), map[string]any{
			"version":       1,
			"s3_access_key": storage.accessKey,
			"s3_secret_key": storage.secretKey,
			"name_salt":     salt,
		})
		assigned.SealedBundle = &bundle
		backproof.submit(assigned)

		result, _ := backproof.waitForResult(t, assigned.Id.String())

		if result.Status != api.JobResultStatusError || *result.ReasonCode != "missing_credentials" || result.FailedStage != nil {
			t.Errorf("result = %s (%v), want an error before the job starts", result.Status, result.ReasonCode)
		}
		if !strings.Contains(*result.Reason, "SEALED_DB") {
			t.Errorf("reason %q does not name the source", *result.Reason)
		}
	})

	t.Run("source without credentials", func(t *testing.T) {
		assigned := agentJob(t, 4, storage, "OTHER_DB", "prod/", true, emptyChecks())
		backproof.submit(assigned)

		result, _ := backproof.waitForResult(t, assigned.Id.String())

		if result.Status != api.JobResultStatusError || *result.ReasonCode != "missing_credentials" || result.FailedStage != nil {
			t.Errorf("result = %s (%v), want an error before the job starts", result.Status, result.ReasonCode)
		}
		if !strings.Contains(*result.Reason, "OTHER_DB") {
			t.Errorf("reason %q does not name the source", *result.Reason)
		}
	})

	runner.assertNothingLeft(t)
	runner.assertNoWorkFiles(t)
}

// sealBundle builds a sealed bundle in the format of SealedBundleContents.
func sealBundle(t *testing.T, recipient age.Recipient, contents map[string]any) string {
	t.Helper()
	plain, err := json.Marshal(contents)
	if err != nil {
		t.Fatalf("encode bundle: %v", err)
	}
	var sealed bytes.Buffer
	armored := armor.NewWriter(&sealed)
	if _, err := armored.Write(encodeAge(t, recipient, plain)); err != nil {
		t.Fatalf("armor bundle: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("armor bundle: %v", err)
	}
	return sealed.String()
}

// sleepingCopy makes a real dump take the given time to restore.
func sleepingCopy(t *testing.T, path string, seconds int) []byte {
	t.Helper()
	footer := []byte("-- PostgreSQL database dump complete")
	sleeping := bytes.Replace(readFixture(t, path), footer, append(fmt.Appendf(nil, "SELECT pg_sleep(%d);\n", seconds), footer...), 1)
	if !bytes.Contains(sleeping, []byte("pg_sleep")) {
		t.Fatal("dump has no footer")
	}
	return sleeping
}

type agentProcess struct {
	command *exec.Cmd
	runner  *testRunner
	exited  chan struct{}
	err     error
}

// startAgentProcess runs the real binary, so the test covers how it handles signals.
func startAgentProcess(t *testing.T, apiURL string, identity *age.X25519Identity) *agentProcess {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "backproof")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "./cmd/backproof") //nolint:gosec // The output path is the test's own temp directory.
	build.Dir = ".."
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, output)
	}

	runnerID, err := crypto.RunnerID(config.NewSecret(identity.String()))
	if err != nil {
		t.Fatalf("derive runner id: %v", err)
	}
	dockerClient, err := docker.New(runnerID)
	if err != nil {
		t.Fatalf("create docker client: %v", err)
	}
	inspector, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("create api client: %v", err)
	}
	t.Cleanup(func() {
		_ = dockerClient.RemoveOrphans(context.WithoutCancel(t.Context()))
		_ = dockerClient.Close()
		_ = inspector.Close()
	})

	workDir := t.TempDir()
	command := exec.CommandContext(context.WithoutCancel(t.Context()), binary, "agent") //nolint:gosec // The test built this binary.
	command.Env = append(os.Environ(),
		config.EnvToken+"=bp_run_"+randomID(t),
		config.EnvRunnerKey+"="+identity.String(),
		config.EnvAPIURL+"="+apiURL,
		config.EnvWorkDir+"="+workDir,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	process := &agentProcess{
		command: command,
		runner:  &testRunner{id: runnerID, docker: dockerClient, api: inspector, workDir: workDir},
		exited:  make(chan struct{}),
	}
	go func() {
		process.err = command.Wait()
		close(process.exited)
	}()
	t.Cleanup(func() {
		_ = command.Process.Kill()
		<-process.exited
	})
	return process
}

func (p *agentProcess) signal(t *testing.T) {
	t.Helper()
	if err := p.command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal agent: %v", err)
	}
}

func (p *agentProcess) waitForExit(t *testing.T, limit time.Duration) {
	t.Helper()
	select {
	case <-p.exited:
		var exit *exec.ExitError
		if errors.As(p.err, &exit) || p.err != nil {
			t.Errorf("the agent exited with %v, want a clean exit", p.err)
		}
	case <-time.After(limit):
		t.Fatal("the agent did not exit in time")
	}
}

func startSignalTest(t *testing.T, seconds int) (*agentAPI, *agentProcess, api.Job) {
	t.Helper()
	storage := startStorage(t)
	fixtures := newTestRunner(t)
	dumps := generateDumps(t, fixtures, postgres.NewestMajor)
	storage.put(t, "prod/backup.sql", sleepingCopy(t, dumps.healthy.plain, seconds))

	document, err := openapi3.NewLoader().LoadFromFile("../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	backproof := &agentAPI{t: t, schema: document.Components.Schemas["JobResult"].Value, results: make(map[string]string)}
	server := httptest.NewServer(backproof)
	t.Cleanup(server.Close)

	// The binary gets no local secrets, so these tests also prove it opens sealed keys.
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	assigned := agentJob(t, 1, storage, "PROD_DB", "prod/", true, emptyChecks())
	bundle := sealBundle(t, identity.Recipient(), map[string]any{
		"version":       1,
		"s3_access_key": storage.accessKey,
		"s3_secret_key": storage.secretKey,
		"name_salt":     randomID(t) + randomID(t) + randomID(t),
	})
	assigned.SealedBundle = &bundle
	backproof.submit(assigned)
	process := startAgentProcess(t, server.URL, identity)

	deadline := time.Now().Add(2 * time.Minute)
	for {
		if containers, _ := process.runner.resources(t); containers > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the restore never started")
		}
		time.Sleep(100 * time.Millisecond)
	}
	// Gives the restore time to reach the slow statement.
	time.Sleep(3 * time.Second)
	return backproof, process, assigned
}

func TestFirstSignalLetsTheRunningJobFinish(t *testing.T) {
	backproof, process, assigned := startSignalTest(t, 12)

	process.signal(t)

	select {
	case <-process.exited:
		t.Fatal("the agent stopped while its job was still running")
	case <-time.After(2 * time.Second):
	}
	process.waitForExit(t, 2*time.Minute)

	result, _ := backproof.waitForResult(t, assigned.Id.String())
	if result.Status != api.JobResultStatusPass {
		t.Errorf("result = %s (%v): %v, want the job to finish", result.Status, result.ReasonCode, result.Reason)
	}
	process.runner.assertNothingLeft(t)
	process.runner.assertNoWorkFiles(t)
}

func TestSecondSignalStopsTheRunningJob(t *testing.T) {
	backproof, process, assigned := startSignalTest(t, 600)

	process.signal(t)
	time.Sleep(time.Second)
	process.signal(t)

	process.waitForExit(t, time.Minute)

	result, _ := backproof.waitForResult(t, assigned.Id.String())
	if result.Status != api.JobResultStatusError || *result.ReasonCode != "cancelled" {
		t.Errorf("result = %s (%v), want a cancelled job", result.Status, result.ReasonCode)
	}
	process.runner.assertNothingLeft(t)
	process.runner.assertNoWorkFiles(t)
}
