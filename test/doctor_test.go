//go:build integration

package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"filippo.io/age"
	"github.com/getkin/kin-openapi/openapi3"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
)

// statusAPI answers only GET /v1/runners/me, so any other request shows that doctor did more
// than read the runner's status.
type statusAPI struct {
	t      *testing.T
	schema *openapi3.Schema
	token  string
	status api.RunnerStatus
	latest string
	min    string

	mu       sync.Mutex
	requests []string
}

func (s *statusAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.requests = append(s.requests, r.Method+" "+r.URL.Path)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodGet || r.URL.Path != "/v1/runners/me" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+s.token {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"code":"invalid_token","message":"This runner's token was revoked. Register the runner again with a new install token."}`))
		return
	}

	body, err := json.Marshal(s.status)
	if err != nil {
		s.t.Errorf("encode status: %v", err)
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		s.t.Errorf("decode status: %v", err)
	}
	if err := s.schema.VisitJSON(value); err != nil {
		s.t.Errorf("the test's status breaks the contract: %v", err)
	}
	w.Header().Set("Backproof-Latest-Version", s.latest)
	w.Header().Set("Backproof-Min-Version", s.min)
	_, _ = w.Write(body)
}

func TestDoctorReportsTheRunnersSetup(t *testing.T) {
	document, err := openapi3.NewLoader().LoadFromFile("../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	binary := buildBackproof(t, "-ldflags", "-X github.com/backproofhq/runner/internal/version.version=0.2.0")
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	stranger, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create other key: %v", err)
	}
	fingerprint := crypto.KeyFingerprint(identity.Recipient().String())
	var runnerID openapi_types.UUID
	if err := runnerID.UnmarshalText([]byte("7b1f6c1e-3f0a-4a53-9a55-2f4f0e0a1c11")); err != nil {
		t.Fatalf("parse runner id: %v", err)
	}

	token := "bp_run_" + randomID(t)
	accessKey, secretKey := "local-access-"+randomID(t), "local-secret-"+randomID(t)
	localKeys := []string{
		config.EnvSourceS3AccessKey("LOCAL_DB") + "=" + accessKey,
		config.EnvSourceS3SecretKey("LOCAL_DB") + "=" + secretKey,
		config.EnvSourceS3AccessKey("HALF_DB") + "=" + accessKey,
	}
	sources := []api.RunnerSource{
		{Id: "PROD_DB", HasSealedKeys: true},
		{Id: "LOCAL_DB", HasSealedKeys: false},
		{Id: "HALF_DB", HasSealedKeys: true},
		{Id: "STAGING_DB", HasSealedKeys: false},
	}

	tests := []struct {
		name         string
		token        string
		fingerprint  string
		latest, min  string
		env          []string
		wantCode     int
		wantLines    []string
		wantRequests int
	}{
		{
			name:        "a healthy runner behind the latest release",
			token:       token,
			fingerprint: fingerprint,
			latest:      "0.3.0", min: "0.1.0",
			env:      localKeys,
			wantCode: 0,
			wantLines: []string{
				"ok    Settings: valid, using the API at ",
				"ok    Docker: reachable",
				`ok    Backproof API: connected as runner "build-box"`,
				"ok    Runner key: " + fingerprint + " matches the dashboard",
				"warn  Version: 0.2.0 is installed and 0.3.0 is available. Update with: curl -fsSL https://get.backproof.dev | sh",
				"ok    Source PROD_DB: keys entered in the dashboard, sealed for this runner",
				"ok    Source LOCAL_DB: local keys",
				"warn  Source HALF_DB: only one of BP_SRC_HALF_DB_S3_ACCESS_KEY and BP_SRC_HALF_DB_S3_SECRET_KEY is set",
				"warn  Source STAGING_DB: no keys on this runner",
				"No checks failed, 3 need attention.",
			},
			wantRequests: 1,
		},
		{
			name:        "a runner below the minimum version",
			token:       token,
			fingerprint: fingerprint,
			latest:      "0.4.0", min: "0.3.0",
			wantCode:     2,
			wantLines:    []string{"fail  Version: 0.2.0 is below the minimum version 0.3.0, so this runner gets no jobs."},
			wantRequests: 1,
		},
		{
			name:        "Backproof holds a different key",
			token:       token,
			fingerprint: crypto.KeyFingerprint(stranger.Recipient().String()),
			latest:      "0.2.0", min: "0.1.0",
			wantCode:     2,
			wantLines:    []string{"fail  Runner key: this runner's key " + fingerprint + " doesn't match the key", "ok    Version: 0.2.0 is up to date"},
			wantRequests: 1,
		},
		{
			name:         "a revoked token",
			token:        "bp_run_" + randomID(t),
			fingerprint:  fingerprint,
			wantCode:     2,
			wantLines:    []string{"fail  Backproof API: This runner's token was revoked.", "1 of 3 checks failed."},
			wantRequests: 1,
		},
		{
			name:        "Docker is unreachable",
			token:       token,
			fingerprint: fingerprint,
			latest:      "0.2.0", min: "0.1.0",
			env:          []string{"DOCKER_HOST=unix:///nonexistent/docker.sock"},
			wantCode:     2,
			wantLines:    []string{"fail  Docker: can't reach Docker", `ok    Backproof API: connected as runner "build-box"`},
			wantRequests: 1,
		},
		{
			name:         "invalid settings",
			token:        token,
			fingerprint:  fingerprint,
			env:          []string{config.EnvConcurrency + "=0"},
			wantCode:     2,
			wantLines:    []string{"fail  Settings: " + config.EnvConcurrency + " is not valid", "1 of 2 checks failed."},
			wantRequests: 0,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			backproof := &statusAPI{
				t:      t,
				schema: document.Components.Schemas["RunnerStatus"].Value,
				token:  test.token,
				status: api.RunnerStatus{
					RunnerId:       runnerID,
					Name:           "build-box",
					KeyFingerprint: test.fingerprint,
					Sources:        sources,
				},
				latest: test.latest,
				min:    test.min,
			}
			server := httptest.NewServer(backproof)
			defer server.Close()

			command := exec.CommandContext(t.Context(), binary, "doctor")
			// exec keeps the last value of a repeated variable, so these replace processEnv's.
			command.Env = append(processEnv(t, server.URL, identity, t.TempDir()), config.EnvToken+"="+token)
			command.Env = append(command.Env, test.env...)
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			err := command.Run()

			code := 0
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatalf("doctor: %v", err)
			}
			output := stdout.String() + stderr.String()
			if code != test.wantCode {
				t.Errorf("exit code = %d, want %d\n%s", code, test.wantCode, output)
			}
			for _, line := range test.wantLines {
				if !strings.Contains(output, line) {
					t.Errorf("output is missing %q\n%s", line, output)
				}
			}
			for _, secret := range []string{token, identity.String(), accessKey, secretKey} {
				if strings.Contains(output, secret) {
					t.Errorf("output contains a secret\n%s", output)
				}
			}

			backproof.mu.Lock()
			defer backproof.mu.Unlock()
			if len(backproof.requests) != test.wantRequests {
				t.Errorf("requests = %v, want %d GET /v1/runners/me", backproof.requests, test.wantRequests)
			}
			for _, request := range backproof.requests {
				if request != "GET /v1/runners/me" {
					t.Errorf("doctor sent %s, but it may only read the runner's status", request)
				}
			}
		})
	}
}
