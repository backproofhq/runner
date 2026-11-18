package job

import (
	"context"
	"errors"
	"time"

	"github.com/backproofhq/runner/internal/archive"
)

const schemaVersion = 1

// Status is the outcome of a job.
type Status string

// A failed backup and a runner that couldn't do its job trigger different alerts.
const (
	StatusPass  Status = "pass"
	StatusFail  Status = "fail"
	StatusError Status = "error"
)

// Stage names a step of the pipeline.
type Stage string

// Pipeline stages, in the order they run.
const (
	StageDecrypt       Stage = "decrypt"
	StageDecompress    Stage = "decompress"
	StageDetectVersion Stage = "detect_version"
	StageRestore       Stage = "restore"
	StageCheck         Stage = "check"
	StageCleanup       Stage = "cleanup"
)

// VersionSource says where the detected Postgres version came from.
type VersionSource string

// Places a Postgres version can come from.
const (
	VersionFromDump VersionSource = "dump"
	VersionDefault  VersionSource = "default"
)

// Result is the report of one job.
type Result struct {
	SchemaVersion int           `json:"schema_version"`
	Status        Status        `json:"status"`
	Stage         Stage         `json:"stage,omitempty"`
	ReasonCode    string        `json:"reason_code,omitempty"`
	Reason        string        `json:"reason,omitempty"`
	Backup        Backup        `json:"backup"`
	Stages        []StageTiming `json:"stages"`
	Tables        []TableCount  `json:"tables"`
	Checks        []CheckResult `json:"checks"`
	Notes         []string      `json:"notes,omitempty"`
}

// Backup describes the file that was tested.
type Backup struct {
	File            string         `json:"file"`
	SizeBytes       int64          `json:"size_bytes"`
	Time            time.Time      `json:"time,omitzero"`
	Format          archive.Format `json:"format,omitempty"`
	Compression     string         `json:"compression,omitempty"`
	Encryption      string         `json:"encryption,omitempty"`
	PostgresVersion string         `json:"postgres_version,omitempty"`
	PostgresMajor   int            `json:"postgres_major,omitempty"`
	VersionSource   VersionSource  `json:"version_source,omitempty"`
	RestoredMajor   int            `json:"restored_major,omitempty"`
}

// StageTiming records how long a stage took.
type StageTiming struct {
	Name       Stage `json:"name"`
	DurationMS int64 `json:"duration_ms"`
}

// TableCount is the number of rows in a restored table.
type TableCount struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Rows   int64  `json:"rows"`
}

// Failure explains why a job didn't pass. Code and Reason must never contain SQL or row contents.
type Failure struct {
	Status Status
	Code   string
	Reason string
	// Stage overrides the running stage when one stage covers several steps.
	Stage Stage
	cause error
}

// Fail reports that the backup is bad.
func Fail(code, reason string) *Failure {
	return &Failure{Status: StatusFail, Code: code, Reason: reason}
}

// RunnerError reports that the runner couldn't do its job.
func RunnerError(code, reason string, cause error) *Failure {
	return &Failure{Status: StatusError, Code: code, Reason: reason, cause: cause}
}

// Error returns the reason shown to the user.
func (f *Failure) Error() string {
	return f.Reason
}

// Unwrap returns the underlying cause, if there is one.
func (f *Failure) Unwrap() error {
	return f.cause
}

func asFailure(ctx context.Context, err error) *Failure {
	if ctx.Err() != nil {
		return RunnerError("cancelled", "The restore was stopped before it finished. Run it again to test this backup.", err)
	}
	var failure *Failure
	if errors.As(err, &failure) {
		return failure
	}
	return RunnerError("runner_failed", "The runner hit an unexpected problem. Check its logs and try again.", err)
}
