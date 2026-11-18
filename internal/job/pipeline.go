// Package job runs the backup test pipeline and builds its result.
package job

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/backproofhq/runner/internal/archive"
)

const cleanupTimeout = 2 * time.Minute

// Dump is a backup file that is ready to restore.
type Dump struct {
	Path   string
	Format archive.Format
}

// Version is the Postgres version a dump was made with.
type Version struct {
	Full   string
	Major  int
	Source VersionSource
}

// Engine restores dumps of one database type.
type Engine interface {
	Ready(ctx context.Context) error
	DetectVersion(ctx context.Context, jobID string, dump Dump) (Version, error)
	Start(ctx context.Context, jobID string, version Version) (Instance, error)
	Cleanup(ctx context.Context, jobID string) error
	RemoveOrphans(ctx context.Context, keepJobIDs ...string) error
}

// Instance is a throwaway database server.
type Instance interface {
	Major() int
	Restore(ctx context.Context, dump Dump) error
	Tables(ctx context.Context, estimate bool) ([]TableCount, error)
	LatestTimestamp(ctx context.Context, check TimestampCheck) (TimestampReading, error)
}

// RunLocal tests a backup file on this machine and always removes what it created.
func RunLocal(ctx context.Context, engine Engine, logger *slog.Logger, jobID, path string, options Options) (result Result) {
	checks := options.Checks
	result = Result{
		SchemaVersion: schemaVersion,
		Status:        StatusPass,
		Backup:        Backup{File: filepath.Base(path)},
		Stages:        []StageTiming{},
		Tables:        []TableCount{},
		Checks:        []CheckResult{},
	}

	if err := engine.Ready(ctx); err != nil {
		result.stop(ctx, logger, "", err)
		return result
	}

	// A killed runner can't clean up after itself, so the next run does it.
	removeOrphans(ctx, engine, logger, options.WorkDir, jobID)

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		err := result.timed(StageCleanup, func() error {
			removeOrphans(cleanupCtx, engine, logger, options.WorkDir, jobID)
			var removeFiles error
			if options.WorkDir != "" {
				removeFiles = os.RemoveAll(jobDirectory(options.WorkDir, jobID))
			}
			return errors.Join(removeFiles, engine.Cleanup(cleanupCtx, jobID))
		})
		if err == nil {
			return
		}
		logger.ErrorContext(cleanupCtx, "clean up job", slog.String("job", jobID), slog.Any("error", err))
		if result.Status == StatusPass {
			result.stop(cleanupCtx, logger, StageCleanup, RunnerError(
				"cleanup_failed",
				"The backup passed, but its files or container could not be removed. Remove containers labelled dev.backproof.job="+jobID+" and clear the work directory.",
				err,
			))
		}
	}()

	var dump Dump
	err := result.timed(StageDecompress, func() error {
		var err error
		dump, err = prepareDump(ctx, path, jobID, options, &result.Backup)
		return err
	})
	if err != nil {
		result.stop(ctx, logger, StageDecompress, err)
		return result
	}
	if !checks.BackupTime.IsZero() {
		result.Backup.Time = checks.BackupTime.UTC()
	}

	var version Version
	err = result.timed(StageDetectVersion, func() error {
		var err error
		version, err = engine.DetectVersion(ctx, jobID, dump)
		return err
	})
	if err != nil {
		result.stop(ctx, logger, StageDetectVersion, err)
		return result
	}
	result.Backup.PostgresVersion = version.Full
	result.Backup.VersionSource = version.Source
	if version.Source == VersionFromDump {
		result.Backup.PostgresMajor = version.Major
	}

	var instance Instance
	err = result.timed(StageRestore, func() error {
		var err error
		instance, err = engine.Start(ctx, jobID, version)
		if err != nil {
			return err
		}
		result.Backup.RestoredMajor = instance.Major()
		return instance.Restore(ctx, dump)
	})
	if err != nil {
		result.stop(ctx, logger, StageRestore, err)
		return result
	}
	result.addVersionNotes(version)

	err = result.timed(StageCheck, func() error {
		tables, err := instance.Tables(ctx, checks.EstimateRows)
		if err != nil {
			return err
		}
		result.Tables = tables

		var reading *TimestampReading
		if checks.LatestTimestamp != nil {
			latest, err := instance.LatestTimestamp(ctx, *checks.LatestTimestamp)
			if err != nil {
				return err
			}
			reading = &latest
		}
		result.Checks = evaluateChecks(checks, tables, reading, result.Backup.Time)
		if len(checks.Baseline) == 0 {
			result.Notes = append(result.Notes, "There is no earlier run to compare with, so row counts were recorded without being judged.")
		}
		return firstFailure(result.Checks)
	})
	if err != nil {
		result.stop(ctx, logger, StageCheck, err)
	}
	return result
}

// removeOrphans only logs failures, because leftovers from another run say nothing about this backup.
func removeOrphans(ctx context.Context, engine Engine, logger *slog.Logger, workDir, jobID string) {
	if err := engine.RemoveOrphans(ctx, jobID); err != nil {
		logger.WarnContext(ctx, "remove leftover containers", slog.Any("error", err))
	}
	removeStaleWorkFiles(ctx, logger, workDir, jobID)
}

func (r *Result) addVersionNotes(version Version) {
	switch {
	case version.Source == VersionDefault:
		r.Notes = append(r.Notes, fmt.Sprintf("The backup doesn't say which Postgres version made it, so it was restored into Postgres %d.", r.Backup.RestoredMajor))
	case version.Major < r.Backup.RestoredMajor:
		r.Notes = append(r.Notes, fmt.Sprintf("The backup was made with Postgres %d and restored into Postgres %d, the oldest supported version.", version.Major, r.Backup.RestoredMajor))
	}
}

func (r *Result) timed(stage Stage, run func() error) error {
	start := time.Now()
	err := run()
	r.Stages = append(r.Stages, StageTiming{Name: stage, DurationMS: time.Since(start).Milliseconds()})
	return err
}

func (r *Result) stop(ctx context.Context, logger *slog.Logger, stage Stage, err error) {
	failure := asFailure(ctx, err)
	if failure.Stage != "" {
		stage = failure.Stage
	}
	r.Status = failure.Status
	r.Stage = stage
	r.ReasonCode = failure.Code
	r.Reason = failure.Reason
	if failure.cause != nil {
		logger.ErrorContext(ctx, "job stopped",
			slog.String("stage", string(stage)),
			slog.String("code", failure.Code),
			slog.Any("error", failure.cause),
		)
	}
}
