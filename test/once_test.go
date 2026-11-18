//go:build integration

package test

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/getkin/kin-openapi/openapi3"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
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

func TestRunOnceNeedsOnlyACIToken(t *testing.T) {
	storage := startStorage(t)
	fixtures := newTestRunner(t)
	dumps := generateDumps(t, fixtures, postgres.NewestMajor)
	storage.put(t, "good/backup.dump", readFixture(t, dumps.healthy.custom))

	document, err := openapi3.NewLoader().LoadFromFile("../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	runner := runnerForIdentity(t, identity)

	secretBytes := make([]byte, 32)
	if _, err := rand.Read(secretBytes); err != nil {
		t.Fatalf("create token secret: %v", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)

	assigned := agentJob(t, 1, storage, "PROD_DB", "good/", true, emptyChecks())
	bundle := sealBundle(t, identity.Recipient(), map[string]any{
		"version":       1,
		"s3_access_key": storage.accessKey,
		"s3_secret_key": storage.secretKey,
		"name_salt":     randomID(t) + randomID(t) + randomID(t),
	})
	assigned.SealedBundle = &bundle

	backproof := &agentAPI{t: t, schema: document.Components.Schemas["JobResult"].Value, results: make(map[string]string)}
	backproof.submit(assigned)

	var mu sync.Mutex
	var sent bytes.Buffer
	authorizations := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		authorizations[r.Header.Get("Authorization")] = true
		_ = r.Header.Write(&sent)
		sent.Write(body)
		mu.Unlock()
		backproof.ServeHTTP(w, r)
	}))
	defer server.Close()

	command := exec.CommandContext(t.Context(), buildBackproof(t), "run", "--once") //nolint:gosec // The test built this binary.
	command.Env = append(os.Environ(),
		config.EnvToken+"=bp_ci_"+secret+"."+identity.String(),
		config.EnvAPIURL+"="+server.URL,
		config.EnvWorkDir+"="+runner.workDir,
	)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("run --once: %v\n%s", err, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != "1 passed, 0 failed, 0 errors" {
		t.Errorf("summary = %q", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if want := "Bearer bp_run_" + secret; len(authorizations) != 1 || !authorizations[want] {
		t.Errorf("the runner authenticated with %d other credentials, want only its runner token", len(authorizations))
	}
	if bytes.Contains(sent.Bytes(), []byte(identity.String())) {
		t.Error("the private key was sent to Backproof")
	}
	if strings.Contains(stdout.String()+stderr.String(), identity.String()) {
		t.Error("the private key was logged")
	}
	runner.assertNothingLeft(t)
	runner.assertNoWorkFiles(t)
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
