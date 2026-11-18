package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCommitNeverReplacesARunnerThatAppearedMeanwhile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runner.env")
	file, err := ReserveRunnerFile(path)
	if err != nil {
		t.Fatalf("ReserveRunnerFile() error = %v", err)
	}
	defer file.Discard()

	const existing = "BACKPROOF_NAME=first-runner\n"
	if err := os.WriteFile(path, []byte(existing), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}

	err = file.Commit(RunnerSettings{Token: NewSecret("test-token"), RunnerKey: NewSecret("test-key"), Name: "second-runner"})
	if err == nil {
		t.Error("Commit() replaced an existing runner")
	}
	contents, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if string(contents) != existing {
		t.Error("the existing runner's settings were changed")
	}
}
