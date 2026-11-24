package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/backproofhq/runner/internal/config"
)

func TestInvalidSettingsStopTheRunnerWithoutLeakingSecrets(t *testing.T) {
	const token = "test-token-canary"
	env := map[string]string{
		config.EnvToken:       token,
		config.EnvConcurrency: "none",
	}
	lookup := func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}

	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"agent"}, &stdout, &stderr, lookup)

	if code != exitRunnerError {
		t.Errorf("exit code = %d, want %d", code, exitRunnerError)
	}
	if !strings.Contains(stderr.String(), config.EnvConcurrency) {
		t.Errorf("stderr %q does not name the invalid setting", stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), token) {
		t.Error("output leaked the token")
	}
}
