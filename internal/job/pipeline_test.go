package job

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// recordingEngine stops a job when it would start Postgres, and records what it was asked to do.
type recordingEngine struct {
	detected Version
	started  Version
	kept     [][]string
}

func (e *recordingEngine) Ready(context.Context) error { return nil }

func (e *recordingEngine) DetectVersion(context.Context, string, Dump) (Version, error) {
	return e.detected, nil
}

func (e *recordingEngine) Start(_ context.Context, _ string, version Version) (Instance, error) {
	e.started = version
	return nil, Fail("stopped", "Stopped by the test.")
}

func (e *recordingEngine) Cleanup(context.Context, string) error { return nil }

func (e *recordingEngine) RemoveOrphans(_ context.Context, keepJobIDs ...string) error {
	e.kept = append(e.kept, keepJobIDs)
	return nil
}

func writePlainDump(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE t (id int);\n"), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}
	return path
}

func TestCleanupKeepsOtherRunningJobs(t *testing.T) {
	engine := &recordingEngine{detected: Version{Major: 17, Source: VersionFromDump}}
	workDir := t.TempDir()
	other := jobDirectory(workDir, "other-job")
	if err := os.MkdirAll(other, 0o700); err != nil {
		t.Fatalf("create job directory: %v", err)
	}

	RunLocal(t.Context(), engine, slog.New(slog.DiscardHandler), "this-job", writePlainDump(t), Options{
		WorkDir:     workDir,
		RunningJobs: func() []string { return []string{"other-job"} },
	})

	if len(engine.kept) != 2 {
		t.Fatalf("leftovers were removed %d times, want at the start and the end", len(engine.kept))
	}
	for _, kept := range engine.kept {
		if !slices.Contains(kept, "other-job") || !slices.Contains(kept, "this-job") {
			t.Errorf("kept %v, want both running jobs", kept)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("the other job lost its files: %v", err)
	}
}

func TestPostgresHintIsUsedWhenTheDumpDoesNotSay(t *testing.T) {
	engine := &recordingEngine{detected: Version{Major: 18, Source: VersionDefault}}

	result := RunLocal(t.Context(), engine, slog.New(slog.DiscardHandler), "job", writePlainDump(t), Options{
		WorkDir:      t.TempDir(),
		PostgresHint: 15,
	})

	if engine.started.Major != 15 || engine.started.Source != VersionFromHint {
		t.Errorf("started %+v, want the hinted major", engine.started)
	}
	if result.Backup.VersionSource != VersionFromHint || result.Backup.PostgresMajor != 15 {
		t.Errorf("backup = %+v, want the hint reported", result.Backup)
	}
}

func TestPostgresHintNeverOverridesTheDump(t *testing.T) {
	engine := &recordingEngine{detected: Version{Full: "17.4", Major: 17, Source: VersionFromDump}}

	RunLocal(t.Context(), engine, slog.New(slog.DiscardHandler), "job", writePlainDump(t), Options{
		WorkDir:      t.TempDir(),
		PostgresHint: 15,
	})

	if engine.started.Major != 17 {
		t.Errorf("started Postgres %d, want the dump's version", engine.started.Major)
	}
}

func TestStagesAreAnnouncedAsTheyStart(t *testing.T) {
	engine := &recordingEngine{detected: Version{Major: 17, Source: VersionFromDump}}
	var stages []Stage

	RunLocal(t.Context(), engine, slog.New(slog.DiscardHandler), "job", writePlainDump(t), Options{
		WorkDir: t.TempDir(),
		OnStage: func(stage Stage) { stages = append(stages, stage) },
	})

	want := []Stage{StageDecompress, StageDetectVersion, StageRestore, StageCleanup}
	if !slices.Equal(stages, want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

func TestAJobOverItsTimeLimitIsARunnerError(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()

	failure := asFailure(ctx, errors.New("stopped"))

	if failure.Status != StatusError || failure.Code != "job_timeout" {
		t.Errorf("failure = %s (%s), want a job timeout", failure.Status, failure.Code)
	}
}
