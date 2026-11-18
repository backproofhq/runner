// Package job runs the backup test pipeline and builds its result.
package job

import (
	"context"
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
func RunLocal(ctx context.Context, engine Engine, logger *slog.Logger, jobID, path string, checks Checks) (result Result) {
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
	removeOrphans(ctx, engine, logger, jobID)

	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		err := result.timed(StageCleanup, func() error {
			removeOrphans(cleanupCtx, engine, logger, jobID)
			return engine.Cleanup(cleanupCtx, jobID)
		})
		if err == nil {
			return
		}
		logger.ErrorContext(cleanupCtx, "clean up job", slog.String("job", jobID), slog.Any("error", err))
		if result.Status == StatusPass {
			result.stop(cleanupCtx, logger, StageCleanup, RunnerError(
				"cleanup_failed",
				"The backup passed, but its container could not be removed. Remove containers labelled dev.backproof.job="+jobID+".",
				err,
			))
		}
	}()

	var dump Dump
	err := result.timed(StageDecompress, func() error {
		var err error
		dump, err = inspectDump(path, &result.Backup)
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
func removeOrphans(ctx context.Context, engine Engine, logger *slog.Logger, jobID string) {
	if err := engine.RemoveOrphans(ctx, jobID); err != nil {
		logger.WarnContext(ctx, "remove leftover containers", slog.Any("error", err))
	}
}

func inspectDump(path string, backup *Backup) (Dump, error) {
	unreadable := func(err error) error {
		return RunnerError("file_unreadable", "The backup file could not be read. Check the path and its permissions.", err)
	}

	file, err := os.Open(path) //nolint:gosec // The person running the command chooses this file.
	if err != nil {
		return Dump{}, unreadable(err)
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return Dump{}, unreadable(err)
	}
	if !info.Mode().IsRegular() {
		return Dump{}, unreadable(fmt.Errorf("%s is not a regular file", filepath.Base(path)))
	}
	backup.SizeBytes = info.Size()
	backup.Time = info.ModTime().UTC()

	format, err := archive.Sniff(file)
	if err != nil {
		return Dump{}, unreadable(err)
	}
	backup.Format = format

	switch format {
	case archive.FormatPlain:
		truncated, err := archive.PlainDumpTruncated(file, info.Size())
		if err != nil {
			return Dump{}, unreadable(err)
		}
		if truncated {
			return Dump{}, Fail("truncated_backup", "The backup file ends before pg_dump finished writing it. Check the job that creates your backups.")
		}
		return Dump{Path: path, Format: format}, nil
	case archive.FormatCustom:
		return Dump{Path: path, Format: format}, nil
	case archive.FormatEmpty:
		return Dump{}, Fail("empty_backup", "The backup file is empty. Check the job that creates your backups.")
	case archive.FormatGzip, archive.FormatZstd, archive.FormatAge, archive.FormatPGP:
		return Dump{}, RunnerError(
			"format_not_supported",
			fmt.Sprintf("This build can't open %s backups yet. Decompress or decrypt the file first.", format),
			nil,
		)
	default:
		return Dump{}, Fail("unknown_format", "The file isn't a Postgres plain SQL or custom-format dump. Check the job that creates your backups.")
	}
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
