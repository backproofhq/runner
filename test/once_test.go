//go:build integration

package test

import (
	"bytes"
	"errors"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/engine/postgres"
)

func TestRunOnceExitCodesTellCIWhatHappened(t *testing.T) {
	storage := startStorage(t)
	fixtures := newTestRunner(t)
	dumps := generateDumps(t, fixtures, postgres.NewestMajor)
	custom := readFixture(t, dumps.healthy.custom)
	storage.put(t, "good/backup.dump", custom)
	storage.put(t, "broken/backup.dump", custom[:len(custom)*2/3])

	document, err := openapi3.NewLoader().LoadFromFile("../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	binary := buildBackproof(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}
	runner := runnerForIdentity(t, identity)

	sealedFor := func(recipient age.Recipient) *string {
		bundle := sealBundle(t, recipient, map[string]any{
			"version":       1,
			"s3_access_key": storage.accessKey,
			"s3_secret_key": storage.secretKey,
			"name_salt":     randomID(t) + randomID(t) + randomID(t),
		})
		return &bundle
	}
	job := func(number int, prefix string, recipient age.Recipient) api.Job {
		assigned := agentJob(t, number, storage, "PROD_DB", prefix, true, emptyChecks())
		assigned.SealedBundle = sealedFor(recipient)
		return assigned
	}

	tests := []struct {
		name        string
		jobs        []api.Job
		wantCode    int
		wantSummary string
	}{
		{"nothing is due", nil, 0, "0 passed, 0 failed, 0 errors"},
		{"a good backup", []api.Job{job(1, "good/", identity.Recipient())}, 0, "1 passed, 0 failed, 0 errors"},
		{"a broken backup", []api.Job{job(2, "good/", identity.Recipient()), job(3, "broken/", identity.Recipient())}, 1, "1 passed, 1 failed, 0 errors"},
		{"keys this runner can't open", []api.Job{job(4, "good/", stranger.Recipient())}, 2, "0 passed, 0 failed, 1 errors"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backproof := &agentAPI{t: t, schema: document.Components.Schemas["JobResult"].Value, results: make(map[string]string)}
			server := httptest.NewServer(backproof)
			defer server.Close()
			for _, assigned := range test.jobs {
				backproof.submit(assigned)
			}

			command := exec.CommandContext(t.Context(), binary, "run", "--once")
			command.Env = processEnv(t, server.URL, identity, runner.workDir)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()

			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("run --once: %v", err)
			}
			if code != test.wantCode {
				t.Errorf("exit code = %d, want %d\n%s", code, test.wantCode, stderr.String())
			}
			if got := strings.TrimSpace(stdout.String()); got != test.wantSummary {
				t.Errorf("summary = %q, want %q", got, test.wantSummary)
			}
			backproof.mu.Lock()
			reported := len(backproof.results)
			backproof.mu.Unlock()
			if reported != len(test.jobs) {
				t.Errorf("%d results reported, want %d", reported, len(test.jobs))
			}
			runner.assertNothingLeft(t)
			runner.assertNoWorkFiles(t)
		})
	}
}

func TestRunOnceReportsAnUnreachableAPI(t *testing.T) {
	binary := buildBackproof(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	server := httptest.NewServer(nil)
	address := server.URL
	server.Close()

	command := exec.CommandContext(t.Context(), binary, "run", "--once") //nolint:gosec // The test built this binary.
	command.Env = processEnv(t, address, identity, t.TempDir())
	var stderr bytes.Buffer
	command.Stderr = &stderr
	err = command.Run()

	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 2 {
		t.Errorf("run --once = %v, want exit code 2", err)
	}
	if !strings.Contains(stderr.String(), address) {
		t.Errorf("stderr %q does not name the address", stderr.String())
	}
}
