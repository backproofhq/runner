package job

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/backproofhq/runner/internal/config"
)

// CheckName identifies a built-in check.
type CheckName string

// Built-in checks.
const (
	CheckTablesExist     CheckName = "tables_exist"
	CheckRowCounts       CheckName = "row_counts"
	CheckLatestTimestamp CheckName = "latest_timestamp"
)

// CheckStatus is the outcome of one check.
type CheckStatus string

// Check outcomes.
const (
	CheckPassed  CheckStatus = "pass"
	CheckFailed  CheckStatus = "fail"
	CheckSkipped CheckStatus = "skipped"
)

// Problems reported for a single table.
const (
	ProblemMissing         = "missing"
	ProblemRowCountChanged = "row_count_changed"
)

// TableRef names a table.
type TableRef struct {
	Schema string
	Name   string
}

// TableKeyFunc turns a table into the key that identifies it outside the restored database.
type TableKeyFunc func(TableRef) string

// PlainTableKey identifies a table by its readable name.
func PlainTableKey(table TableRef) string {
	return table.Schema + "." + table.Name
}

// HashedTableKey identifies tables without revealing their names. The salt stops anyone from
// recovering names by hashing common ones, and the NUL byte keeps schema and table apart.
func HashedTableKey(salt config.Secret) TableKeyFunc {
	return func(table TableRef) string {
		mac := hmac.New(sha256.New, []byte(salt.Reveal()))
		mac.Write([]byte(table.Schema))
		mac.Write([]byte{0})
		mac.Write([]byte(table.Name))
		return hex.EncodeToString(mac.Sum(nil))
	}
}

// KeyedCount is the number of rows in the table with this key.
type KeyedCount struct {
	Key  string
	Rows int64
}

// Checks configures how a restored backup is judged.
type Checks struct {
	// ExpectedTables, Baseline and TableTolerancePercent identify tables by table key.
	ExpectedTables []string
	// Baseline holds the row counts of the last passing run. It is empty on a source's first run.
	Baseline              []KeyedCount
	TolerancePercent      float64
	TableTolerancePercent map[string]float64
	EstimateRows          bool
	LatestTimestamp       *TimestampCheck
	// TableKey defaults to PlainTableKey.
	TableKey TableKeyFunc
	// BackupTime is when the backup was made. The file's modification time is used when it is zero.
	BackupTime time.Time
}

// TimestampCheck requires a recent row in the chosen column.
type TimestampCheck struct {
	Table  TableRef
	Column string
	MaxAge time.Duration
}

// TimestampState says what was found in the chosen column.
type TimestampState int

// States of the chosen timestamp column.
const (
	TimestampFound TimestampState = iota
	TimestampNoColumn
	TimestampNoRows
)

// TimestampReading is the newest value in the chosen column.
type TimestampReading struct {
	State TimestampState
	Value time.Time
}

// CheckResult reports one check. Table names stay in Tables, so they can be hashed before the result
// leaves the runner, and never appear in Reason.
type CheckResult struct {
	Name     CheckName      `json:"name"`
	Status   CheckStatus    `json:"status"`
	Code     string         `json:"code,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Measured string         `json:"measured,omitempty"`
	Expected string         `json:"expected,omitempty"`
	Tables   []TableFinding `json:"tables,omitempty"`
}

// TableFinding describes what is wrong with one table.
type TableFinding struct {
	Key              string   `json:"table_key"`
	Problem          string   `json:"problem"`
	MeasuredRows     *int64   `json:"measured_rows,omitempty"`
	ExpectedRows     *int64   `json:"expected_rows,omitempty"`
	ChangePercent    *float64 `json:"change_percent,omitempty"`
	TolerancePercent *float64 `json:"tolerance_percent,omitempty"`
}

func evaluateChecks(checks Checks, tables []TableCount, reading *TimestampReading, backupTime time.Time) []CheckResult {
	return []CheckResult{
		checkTables(checks, tables),
		checkRowCounts(checks, tables),
		checkLatestTimestamp(checks.LatestTimestamp, reading, backupTime),
	}
}

func (c Checks) keyOf(table TableCount) string {
	key := c.TableKey
	if key == nil {
		key = PlainTableKey
	}
	return key(TableRef{Schema: table.Schema, Name: table.Name})
}

func firstFailure(results []CheckResult) error {
	for _, result := range results {
		if result.Status == CheckFailed {
			return Fail(result.Code, result.Reason)
		}
	}
	return nil
}

func checkTables(checks Checks, tables []TableCount) CheckResult {
	result := CheckResult{Name: CheckTablesExist, Status: CheckPassed, Measured: tableCount(len(tables))}

	present := make(map[string]struct{}, len(tables))
	for _, table := range tables {
		present[checks.keyOf(table)] = struct{}{}
	}

	required := slices.Clone(checks.ExpectedTables)
	for _, table := range checks.Baseline {
		required = append(required, table.Key)
	}
	slices.Sort(required)
	required = slices.Compact(required)
	if len(required) > 0 {
		result.Expected = tableCount(len(required))
	}

	for _, table := range required {
		if _, ok := present[table]; !ok {
			result.Tables = append(result.Tables, TableFinding{Key: table, Problem: ProblemMissing})
		}
	}

	switch {
	case len(tables) == 0:
		result.Status = CheckFailed
		result.Code = "no_tables"
		result.Reason = "The restored backup contains no tables. Check the job that creates your backups."
	case len(result.Tables) > 0:
		result.Status = CheckFailed
		result.Code = "table_missing"
		result.Reason = fmt.Sprintf("Missing from the restored backup: %d of %s.", len(result.Tables), tableCount(len(required)))
	}
	return result
}

func checkRowCounts(checks Checks, tables []TableCount) CheckResult {
	result := CheckResult{Name: CheckRowCounts, Status: CheckPassed, Measured: tableCount(len(tables)) + " counted"}
	if len(checks.Baseline) == 0 {
		return result
	}
	result.Expected = fmt.Sprintf("within %g%% of the last passing run", checks.TolerancePercent)

	measured := make(map[string]int64, len(tables))
	for _, table := range tables {
		measured[checks.keyOf(table)] = table.Rows
	}

	for _, baseline := range checks.Baseline {
		rows, ok := measured[baseline.Key]
		// A table that was empty last time has no percentage to compare, and growing isn't damage.
		if !ok || baseline.Rows <= 0 {
			continue
		}

		tolerance := checks.TolerancePercent
		if override, ok := checks.TableTolerancePercent[baseline.Key]; ok {
			tolerance = override
		}
		change := math.Abs(float64(rows-baseline.Rows)) / float64(baseline.Rows) * 100
		if change > tolerance {
			result.Tables = append(result.Tables, TableFinding{
				Key:              baseline.Key,
				Problem:          ProblemRowCountChanged,
				MeasuredRows:     &rows,
				ExpectedRows:     &baseline.Rows,
				ChangePercent:    &change,
				TolerancePercent: &tolerance,
			})
		}
	}

	if len(result.Tables) > 0 {
		result.Status = CheckFailed
		result.Code = "row_count_changed"
		result.Reason = fmt.Sprintf("Row counts moved past the allowed tolerance in %s since the last passing run.", tableCount(len(result.Tables)))
	}
	return result
}

func checkLatestTimestamp(check *TimestampCheck, reading *TimestampReading, backupTime time.Time) CheckResult {
	result := CheckResult{Name: CheckLatestTimestamp, Status: CheckSkipped}
	if check == nil || reading == nil {
		return result
	}
	result.Status = CheckPassed
	result.Expected = "at most " + readableDuration(check.MaxAge) + " older than the backup"

	switch reading.State {
	case TimestampNoColumn:
		result.Status = CheckFailed
		result.Code = "timestamp_missing"
		result.Reason = "The column chosen for the latest timestamp check isn't in the restored backup."
	case TimestampNoRows:
		result.Status = CheckFailed
		result.Code = "timestamp_no_rows"
		result.Reason = "The column chosen for the latest timestamp check has no values in the restored backup."
	case TimestampFound:
		result.Measured = reading.Value.UTC().Format(time.RFC3339)
		if age := backupTime.Sub(reading.Value); age > check.MaxAge {
			result.Status = CheckFailed
			result.Code = "timestamp_too_old"
			result.Reason = fmt.Sprintf("The newest row is %s older than the backup, and at most %s is allowed.", readableDuration(age), readableDuration(check.MaxAge))
		}
	}
	return result
}

func tableCount(count int) string {
	if count == 1 {
		return "1 table"
	}
	return fmt.Sprintf("%d tables", count)
}

func readableDuration(duration time.Duration) string {
	return duration.Round(time.Second).String()
}
