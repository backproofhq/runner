package main

import (
	"strings"
	"testing"
)

func TestOnlyOneLocalRestoreRunsAtATime(t *testing.T) {
	name := "@backproof-test-" + strings.NewReplacer("/", "-").Replace(t.Name())

	release, err := lockLocalRestore(t.Context(), name)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	_, err = lockLocalRestore(t.Context(), name)
	if err == nil {
		t.Fatal("second restore got the lock while the first was running")
	}
	if !strings.Contains(err.Error(), "already running") {
		t.Errorf("error %q does not say what is wrong", err)
	}

	release()
	again, err := lockLocalRestore(t.Context(), name)
	if err != nil {
		t.Fatalf("lock after the first restore finished: %v", err)
	}
	again()
}
