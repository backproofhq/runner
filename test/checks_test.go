//go:build integration

package test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

// fixtureBackupTime is one day after the newest row in sales.orders.
var fixtureBackupTime = time.Date(2026, 1, 16, 12, 0, 0, 0, time.UTC)

func fixtureBaseline() []job.KeyedCount {
	baseline := make([]job.KeyedCount, 0, len(fixtureTables))
	for name, rows := range fixtureTables {
		baseline = append(baseline, job.KeyedCount{Key: name, Rows: rows})
	}
	return baseline
}

func baselineWith(schema, table string, rows int64) []job.KeyedCount {
	key := schema + "." + table
	baseline := slices.DeleteFunc(fixtureBaseline(), func(entry job.KeyedCount) bool {
		return entry.Key == key
	})
	return append(baseline, job.KeyedCount{Key: key, Rows: rows})
}

func timestampCheck(schema, table, column string, maxAge time.Duration) job.Checks {
	return job.Checks{
		BackupTime: fixtureBackupTime,
		LatestTimestamp: &job.TimestampCheck{
			Table:  job.TableRef{Schema: schema, Name: table},
			Column: column,
			MaxAge: maxAge,
		},
	}
}

func TestChecksJudgeRestoredBackups(t *testing.T) {
	const customers = "public.customers"

	tests := []struct {
		name       string
		checks     job.Checks
		wantStatus job.Status
		wantCode   string
		wantCheck  job.CheckName
		wantTable  string
	}{
		{name: "matching baseline", checks: job.Checks{Baseline: fixtureBaseline()}, wantStatus: job.StatusPass},
		{name: "estimated row counts", checks: job.Checks{Baseline: fixtureBaseline(), EstimateRows: true}, wantStatus: job.StatusPass},
		{
			name:       "table from the baseline is gone",
			checks:     job.Checks{Baseline: baselineWith("public", "deleted_table", 10)},
			wantStatus: job.StatusFail, wantCode: "table_missing", wantCheck: job.CheckTablesExist, wantTable: "deleted_table",
		},
		{
			name:       "expected table is gone",
			checks:     job.Checks{ExpectedTables: []string{"sales.refunds"}},
			wantStatus: job.StatusFail, wantCode: "table_missing", wantCheck: job.CheckTablesExist, wantTable: "refunds",
		},
		{
			name:       "table lost more rows than the tolerance",
			checks:     job.Checks{Baseline: baselineWith("public", "customers", 100), TolerancePercent: 10},
			wantStatus: job.StatusFail, wantCode: "row_count_changed", wantCheck: job.CheckRowCounts, wantTable: "customers",
		},
		{
			name:       "row change within the tolerance",
			checks:     job.Checks{Baseline: baselineWith("public", "customers", 4), TolerancePercent: 30},
			wantStatus: job.StatusPass,
		},
		{
			name: "per-table tolerance",
			checks: job.Checks{
				Baseline:              baselineWith("public", "customers", 100),
				TolerancePercent:      10,
				TableTolerancePercent: map[string]float64{customers: 99},
			},
			wantStatus: job.StatusPass,
		},
		{name: "recent timestamptz", checks: timestampCheck("sales", "orders", "placed_at", 48*time.Hour), wantStatus: job.StatusPass},
		{name: "recent timestamp", checks: timestampCheck("public", "audit_log", "happened_at", 72*time.Hour), wantStatus: job.StatusPass},
		{name: "recent date", checks: timestampCheck("public", "events", "happened_on", time.Hour), wantStatus: job.StatusPass},
		{
			name:       "latest timestamp is too old",
			checks:     timestampCheck("sales", "orders", "placed_at", time.Hour),
			wantStatus: job.StatusFail, wantCode: "timestamp_too_old", wantCheck: job.CheckLatestTimestamp,
		},
		{
			name:       "timestamp column is gone",
			checks:     timestampCheck("sales", "orders", "no_such_column", time.Hour),
			wantStatus: job.StatusFail, wantCode: "timestamp_missing", wantCheck: job.CheckLatestTimestamp,
		},
		{
			name:       "timestamp table is gone",
			checks:     timestampCheck("sales", "no_such_table", "placed_at", time.Hour),
			wantStatus: job.StatusFail, wantCode: "timestamp_missing", wantCheck: job.CheckLatestTimestamp,
		},
		{
			name:       "timestamp table is empty",
			checks:     timestampCheck("public", "empty_log", "happened_at", time.Hour),
			wantStatus: job.StatusFail, wantCode: "timestamp_no_rows", wantCheck: job.CheckLatestTimestamp,
		},
		{
			name:       "chosen column is not a timestamp",
			checks:     timestampCheck("public", "customers", "name", time.Hour),
			wantStatus: job.StatusError, wantCode: "timestamp_column_unsupported",
		},
	}

	for major := postgres.OldestMajor; major <= postgres.NewestMajor; major++ {
		t.Run(fmt.Sprintf("postgres_%d", major), func(t *testing.T) {
			t.Parallel()
			runner := newTestRunner(t)
			dumps := generateDumps(t, runner, major)

			for _, format := range []struct {
				name string
				path string
			}{{"plain", dumps.healthy.plain}, {"custom", dumps.healthy.custom}} {
				for _, test := range tests {
					t.Run(format.name+"/"+test.name, func(t *testing.T) {
						result := runner.restoreWithChecks(t, format.path, test.checks)

						if result.Status != test.wantStatus || result.ReasonCode != test.wantCode {
							t.Fatalf("result = %s (%s): %s, want %s (%s)", result.Status, result.ReasonCode, result.Reason, test.wantStatus, test.wantCode)
						}
						if test.wantStatus == job.StatusPass {
							runner.assertNothingLeft(t)
							return
						}
						if result.Stage != job.StageCheck {
							t.Errorf("stage = %q, want %q", result.Stage, job.StageCheck)
						}
						if result.Reason == "" {
							t.Error("result has no reason")
						}
						if test.wantCheck != "" {
							assertFailedCheck(t, result, test.wantCheck, test.wantTable)
						}
						runner.assertNothingLeft(t)
					})
				}
			}
		})
	}
}

func assertFailedCheck(t *testing.T, result job.Result, name job.CheckName, table string) {
	t.Helper()
	index := slices.IndexFunc(result.Checks, func(check job.CheckResult) bool { return check.Name == name })
	if index < 0 {
		t.Fatalf("result has no %s check: %+v", name, result.Checks)
	}
	check := result.Checks[index]
	if check.Status != job.CheckFailed {
		t.Errorf("%s status = %s, want fail", name, check.Status)
	}
	if table == "" {
		return
	}
	if !slices.ContainsFunc(check.Tables, func(finding job.TableFinding) bool { return strings.HasSuffix(finding.Key, "."+table) }) {
		t.Errorf("%s findings = %+v, want table %s", name, check.Tables, table)
	}
	if strings.Contains(result.Reason, table) {
		t.Errorf("reason names table %s: %q", table, result.Reason)
	}
}

func TestBrokenBackupFixtures(t *testing.T) {
	for major := postgres.OldestMajor; major <= postgres.NewestMajor; major++ {
		t.Run(fmt.Sprintf("postgres_%d", major), func(t *testing.T) {
			t.Parallel()
			runner := newTestRunner(t)
			dumps := generateDumps(t, runner, major)

			tests := []struct {
				name       string
				path       string
				wantStage  job.Stage
				wantCode   string
				wantReason []string
			}{
				{"empty database plain", dumps.empty.plain, job.StageCheck, "no_tables", []string{"no tables"}},
				{"empty database custom", dumps.empty.custom, job.StageCheck, "no_tables", []string{"no tables"}},
				{"missing extension plain", dumps.extension.plain, job.StageRestore, "missing_extension", []string{"postgis"}},
				{"missing extension custom", dumps.extension.custom, job.StageRestore, "missing_extension", []string{"postgis"}},
				{"newer major plain", newerMajorCopy(t, dumps.healthy.plain, major), job.StageRestore, "dump_newer_than_supported", []string{"19", "18"}},
				{"newer major custom", newerMajorCopy(t, dumps.healthy.custom, major), job.StageRestore, "dump_newer_than_supported", []string{"19", "18"}},
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					result := runner.restoreWithChecks(t, test.path, job.Checks{})

					if result.Status != job.StatusFail || result.Stage != test.wantStage || result.ReasonCode != test.wantCode {
						t.Errorf("result = %s at %q (%s), want fail at %q (%s)", result.Status, result.Stage, result.ReasonCode, test.wantStage, test.wantCode)
					}
					for _, want := range test.wantReason {
						if !strings.Contains(result.Reason, want) {
							t.Errorf("reason %q does not mention %q", result.Reason, want)
						}
					}
					runner.assertNothingLeft(t)
				})
			}
		})
	}
}

// newerMajorCopy changes the version a real dump says it came from. The replacement has the same
// length, so the custom format's binary header stays valid.
func newerMajorCopy(t *testing.T, path string, major int) string {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	patched := bytes.Replace(contents, fmt.Appendf(nil, "%d.", major), []byte("19."), 1)
	if strings.HasSuffix(path, ".sql") {
		patched = bytes.Replace(contents, fmt.Appendf(nil, "database version %d.", major), []byte("database version 19."), 1)
	}
	if bytes.Equal(patched, contents) {
		t.Fatalf("no Postgres %d version found in the dump", major)
	}
	target := filepath.Join(t.TempDir(), "newer-"+filepath.Base(path))
	if err := os.WriteFile(target, patched, 0o600); err != nil { //nolint:gosec // The path is inside the test's temp directory.
		t.Fatalf("write dump: %v", err)
	}
	return target
}

func TestEmptyFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sql")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	runner := newTestRunner(t)
	result := runner.restoreWithChecks(t, path, job.Checks{})

	if result.Status != job.StatusFail || result.Stage != job.StageDecompress || result.ReasonCode != "empty_backup" {
		t.Errorf("result = %s at %q (%s), want an empty backup failure", result.Status, result.Stage, result.ReasonCode)
	}
	if result.Reason == "" {
		t.Error("result has no reason")
	}
	runner.assertNothingLeft(t)
}

func TestUnreachableDockerIsARunnerError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///nonexistent/docker.sock")
	client, err := docker.New("test-" + randomID(t))
	if err != nil {
		t.Fatalf("create docker client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	runner := &testRunner{docker: client}
	result := runner.restoreWithChecks(t, filepath.Join(t.TempDir(), "unused.sql"), job.Checks{})

	if result.Status != job.StatusError || result.ReasonCode != "docker_unreachable" {
		t.Errorf("result = %s (%s), want a runner error", result.Status, result.ReasonCode)
	}
	if result.Stage != "" || len(result.Stages) != 0 {
		t.Errorf("stage = %q after %d stages, want an error before the job starts", result.Stage, len(result.Stages))
	}
	if !strings.Contains(result.Reason, "Docker") {
		t.Errorf("reason %q does not mention Docker", result.Reason)
	}
}
