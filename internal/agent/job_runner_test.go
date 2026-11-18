package agent

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/backproofhq/runner/internal/api"
	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/job"
)

func assertMatchesContract(t *testing.T, result api.JobResult) []byte {
	t.Helper()
	document, err := openapi3.NewLoader().LoadFromFile("../../api/v1/openapi.yaml")
	if err != nil {
		t.Fatalf("load contract: %v", err)
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	var value any
	if err := json.Unmarshal(encoded, &value); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if err := document.Components.Schemas["JobResult"].Value.VisitJSON(value); err != nil {
		t.Errorf("result breaks the contract: %v", err)
	}
	return encoded
}

func TestToResultNeverSendsHashedTableNames(t *testing.T) {
	salt := config.NewSecret("test-salt-with-enough-characters-1")
	key := job.HashedTableKey(salt)
	rows, baseline := int64(5), int64(100)
	change, tolerance := 95.0, 10.0

	result := ToResult(job.Result{
		SchemaVersion: 1,
		Status:        job.StatusFail,
		Stage:         job.StageCheck,
		ReasonCode:    "row_count_changed",
		Reason:        "Row counts moved past the allowed tolerance in 1 table since the last passing run.",
		Tables:        []job.TableCount{{Schema: "billing", Name: "private_invoices", Rows: 5}},
		Checks: []job.CheckResult{{
			Name:   job.CheckRowCounts,
			Status: job.CheckFailed,
			Code:   "row_count_changed",
			Tables: []job.TableFinding{{
				Key:              key(job.TableRef{Schema: "billing", Name: "private_invoices"}),
				Problem:          job.ProblemRowCountChanged,
				MeasuredRows:     &rows,
				ExpectedRows:     &baseline,
				ChangePercent:    &change,
				TolerancePercent: &tolerance,
			}},
		}},
	}, key)

	encoded := assertMatchesContract(t, result)
	for _, name := range []string{"private_invoices", "billing"} {
		if strings.Contains(string(encoded), name) {
			t.Errorf("the result contains the table name part %q", name)
		}
	}
	if result.Tables[0].TableKey != key(job.TableRef{Schema: "billing", Name: "private_invoices"}) {
		t.Errorf("table key = %q, want the hash", result.Tables[0].TableKey)
	}
}

func TestToResultMatchesTheContract(t *testing.T) {
	tests := map[string]job.Result{
		"failed before anything was known": job.ErrorResult(job.RunnerError("missing_credentials", "No credentials.", nil)),
		"complete": {
			SchemaVersion: 1,
			Status:        job.StatusPass,
			Backup: job.Backup{
				File:            "backup.dump.gz.gpg",
				ObjectKey:       "prod/backup.dump.gz.gpg",
				SizeBytes:       1024,
				Time:            time.Date(2026, 3, 1, 2, 0, 0, 0, time.UTC),
				Format:          archive.FormatCustom,
				Compression:     "gzip",
				Encryption:      "pgp",
				PostgresVersion: "17.4",
				PostgresMajor:   17,
				VersionSource:   job.VersionFromDump,
				RestoredMajor:   17,
			},
			Stages: []job.StageTiming{{Name: job.StageResolve, DurationMS: 5}},
			Tables: []job.TableCount{{Schema: "public", Name: "orders", Rows: 3}},
			Notes:  []string{"A note."},
		},
		"wrong key": {
			SchemaVersion: 1,
			Status:        job.StatusFail,
			Stage:         job.StageDecrypt,
			ReasonCode:    "wrong_key",
			Reason:        strings.Repeat("é", maxReasonLength+50),
			Backup:        job.Backup{ObjectKey: "a.age", Format: archive.FormatAge, Encryption: "age"},
		},
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			assertMatchesContract(t, ToResult(input, job.PlainTableKey))
		})
	}

	complete := ToResult(tests["complete"], job.PlainTableKey)
	if *complete.Backup.Encryption != api.BackupDetailsEncryptionGpg {
		t.Errorf("encryption = %s, want gpg", *complete.Backup.Encryption)
	}
	if complete.Tables[0].TableKey != "public.orders" {
		t.Errorf("table key = %q, want the readable name", complete.Tables[0].TableKey)
	}
	if format := ToResult(tests["wrong key"], job.PlainTableKey).Backup.Format; *format != api.BackupDetailsFormatUnknown {
		t.Errorf("format = %s, want unknown for a file that couldn't be opened", *format)
	}
}

func TestChecksFromJob(t *testing.T) {
	assigned := testJob(t, 1)
	assigned.Source.ShowTableNames = true
	assigned.Checks = api.CheckConfig{
		ExpectedTables:   []string{"public.orders"},
		Baseline:         []api.TableRowCount{{TableKey: "public.orders", Rows: 10}},
		TolerancePercent: 15,
		TableTolerances:  []api.TableTolerance{{TableKey: "public.orders", TolerancePercent: 50}},
		RowCountMode:     api.Estimate,
		LatestTimestamp:  &api.LatestTimestampCheck{Schema: "public", Table: "orders", Column: "placed_at", MaxAgeSeconds: 3600},
	}

	got := checksFromJob(assigned, job.PlainTableKey)

	if len(got.ExpectedTables) != 1 || got.Baseline[0] != (job.KeyedCount{Key: "public.orders", Rows: 10}) {
		t.Errorf("tables = %v and %v", got.ExpectedTables, got.Baseline)
	}
	if got.TolerancePercent != 15 || got.TableTolerancePercent["public.orders"] != 50 || !got.EstimateRows {
		t.Errorf("row count settings = %+v", got)
	}
	if got.LatestTimestamp.MaxAge != time.Hour || got.LatestTimestamp.Table.Name != "orders" || got.LatestTimestamp.Column != "placed_at" {
		t.Errorf("timestamp check = %+v", got.LatestTimestamp)
	}

	assigned.Source.ShowTableNames = false
	if hashed := checksFromJob(assigned, job.PlainTableKey); len(hashed.ExpectedTables) != 0 {
		t.Error("readable table names were compared with hashed keys")
	}
}

// stoppedEngine ends a job as soon as it starts, which is all these tests need.
type stoppedEngine struct {
	job.Engine
}

func (stoppedEngine) Ready(context.Context) error {
	return job.RunnerError("docker_unreachable", "Docker isn't reachable.", errors.New("stopped"))
}

func TestJobRunnerNeedsCredentialsAndReadsThemForEveryJob(t *testing.T) {
	sources := filepath.Join(t.TempDir(), "sources.env")
	run := NewJobRunner(JobRunnerOptions{
		Engine:      stoppedEngine{},
		Logger:      slog.New(slog.DiscardHandler),
		WorkDir:     t.TempDir(),
		Lookup:      func(string) (string, bool) { return "", false },
		SourcesFile: sources,
	})
	assigned := testJob(t, 1)

	before := run(t.Context(), assigned, JobContext{})
	if before.Status != api.JobResultStatusError || *before.ReasonCode != "missing_credentials" || before.FailedStage != nil {
		t.Fatalf("without keys = %s (%v), want an error before the job starts", before.Status, *before.ReasonCode)
	}
	if !strings.Contains(*before.Reason, "PROD_DB") {
		t.Errorf("reason %q does not name the source", *before.Reason)
	}

	contents := "BP_SRC_PROD_DB_S3_ACCESS_KEY=test-access\nBP_SRC_PROD_DB_S3_SECRET_KEY=test-secret\nBP_SRC_PROD_DB_NAME_SALT=test-salt-with-enough-characters-1\n"
	if err := os.WriteFile(sources, []byte(contents), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}

	after := run(t.Context(), assigned, JobContext{})
	if *after.ReasonCode != "docker_unreachable" {
		t.Errorf("with keys = %s, want the job to start without a restart", *after.ReasonCode)
	}
}

func TestJobRunnerRequiresASaltForHashedNames(t *testing.T) {
	secrets := map[string]string{"BP_SRC_PROD_DB_S3_ACCESS_KEY": "test-access", "BP_SRC_PROD_DB_S3_SECRET_KEY": "test-secret"}
	run := NewJobRunner(JobRunnerOptions{
		Engine:      stoppedEngine{},
		Logger:      slog.New(slog.DiscardHandler),
		WorkDir:     t.TempDir(),
		Lookup:      func(key string) (string, bool) { value, ok := secrets[key]; return value, ok },
		SourcesFile: filepath.Join(t.TempDir(), "missing.env"),
	})
	assigned := testJob(t, 1)

	if got := run(t.Context(), assigned, JobContext{}); *got.ReasonCode != "missing_credentials" {
		t.Errorf("hashed names without a salt = %s, want missing credentials", *got.ReasonCode)
	}

	assigned.Source.ShowTableNames = true
	if got := run(t.Context(), assigned, JobContext{}); *got.ReasonCode != "docker_unreachable" {
		t.Errorf("readable names without a salt = %s, want the job to start", *got.ReasonCode)
	}
}

func TestJobRunnerReportsAnInvalidSecretsFile(t *testing.T) {
	const canary = "test-secret-canary"
	sources := filepath.Join(t.TempDir(), "sources.env")
	if err := os.WriteFile(sources, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatalf("write secrets: %v", err)
	}
	run := NewJobRunner(JobRunnerOptions{
		Engine:      stoppedEngine{},
		Logger:      slog.New(slog.DiscardHandler),
		WorkDir:     t.TempDir(),
		Lookup:      func(string) (string, bool) { return "", false },
		SourcesFile: sources,
	})

	got := run(t.Context(), testJob(t, 1), JobContext{})

	if *got.ReasonCode != "sources_file_invalid" {
		t.Errorf("result = %s, want an invalid secrets file", *got.ReasonCode)
	}
	if strings.Contains(*got.Reason, canary) {
		t.Error("the reason contains a line from the secrets file")
	}
}
