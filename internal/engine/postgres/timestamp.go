package postgres

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/job"
)

var timestampTypes = []string{"timestamptz", "timestamp", "date"}

// LatestTimestamp reads the newest value in the chosen column.
func (i *Instance) LatestTimestamp(ctx context.Context, check job.TimestampCheck) (job.TimestampReading, error) {
	table := quoteIdentifier(check.Table.Schema) + "." + quoteIdentifier(check.Table.Name)
	column := quoteIdentifier(check.Column)

	columnType, err := i.queryValue(ctx, fmt.Sprintf(
		`SELECT t.typname FROM pg_attribute a JOIN pg_type t ON t.oid = a.atttypid
		 WHERE a.attrelid = to_regclass(%s) AND a.attname = %s AND NOT a.attisdropped`,
		quoteLiteral(table), quoteLiteral(check.Column),
	))
	if err != nil {
		return job.TimestampReading{}, err
	}
	if columnType == "" {
		return job.TimestampReading{State: job.TimestampNoColumn}, nil
	}
	if !slices.Contains(timestampTypes, columnType) {
		return job.TimestampReading{}, job.RunnerError(
			"timestamp_column_unsupported",
			"The column chosen for the latest timestamp check isn't a date or timestamp column. Choose another column for this check.",
			nil,
		)
	}

	// Values without a time zone count as UTC, the time zone of the throwaway server.
	value, err := i.queryValue(ctx, fmt.Sprintf(
		`SELECT to_char(max(%[1]s::timestamptz) FILTER (WHERE isfinite(%[1]s)) AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"') FROM %[2]s`,
		column, table,
	))
	if err != nil {
		return job.TimestampReading{}, err
	}
	if value == "" {
		return job.TimestampReading{State: job.TimestampNoRows}, nil
	}

	newest, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return job.TimestampReading{}, job.RunnerError("timestamp_failed", "The runner couldn't read the newest timestamp. Check the chosen column and try again.", err)
	}
	return job.TimestampReading{State: job.TimestampFound, Value: newest}, nil
}

func (i *Instance) queryValue(ctx context.Context, query string) (string, error) {
	var output bytes.Buffer
	code, err := i.exec(ctx, docker.ExecSpec{
		Cmd:    []string{"psql", "-X", "-q", "-A", "-t", "-v", "ON_ERROR_STOP=1", "-U", superuser, "-d", database, "-c", query},
		Stdout: &output,
	})
	if err != nil {
		return "", dockerFailure(err)
	}
	if code != 0 {
		return "", job.RunnerError("timestamp_failed", "The runner couldn't read the newest timestamp. Check the chosen column and try again.", nil)
	}
	return strings.TrimSpace(output.String()), nil
}

func quoteIdentifier(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func quoteLiteral(value string) string {
	return `'` + strings.ReplaceAll(value, `'`, `''`) + `'`
}
