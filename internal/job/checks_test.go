package job

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/backproofhq/runner/internal/config"
)

func TestCheckTables(t *testing.T) {
	orders := TableCount{Schema: "sales", Name: "orders", Rows: 10}
	users := TableCount{Schema: "public", Name: "users", Rows: 5}
	ordersBaseline := KeyedCount{Key: "sales.orders", Rows: 10}
	usersBaseline := KeyedCount{Key: "public.users", Rows: 5}

	tests := []struct {
		name     string
		checks   Checks
		tables   []TableCount
		wantCode string
		missing  []string
	}{
		{name: "first run with tables", tables: []TableCount{orders}},
		{name: "first run without tables", wantCode: "no_tables"},
		{name: "new tables are fine", checks: Checks{Baseline: []KeyedCount{ordersBaseline}}, tables: []TableCount{orders, users}},
		{name: "baseline table gone", checks: Checks{Baseline: []KeyedCount{ordersBaseline, usersBaseline}}, tables: []TableCount{orders}, wantCode: "table_missing", missing: []string{"public.users"}},
		{name: "expected table gone", checks: Checks{ExpectedTables: []string{"public.users"}}, tables: []TableCount{orders}, wantCode: "table_missing", missing: []string{"public.users"}},
		{name: "same name in another schema doesn't count", checks: Checks{ExpectedTables: []string{"archive.orders"}}, tables: []TableCount{orders}, wantCode: "table_missing", missing: []string{"archive.orders"}},
		{name: "table listed twice is reported once", checks: Checks{ExpectedTables: []string{"public.users"}, Baseline: []KeyedCount{usersBaseline}}, tables: []TableCount{orders}, wantCode: "table_missing", missing: []string{"public.users"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := checkTables(test.checks, test.tables)
			if got.Code != test.wantCode {
				t.Errorf("code = %q, want %q", got.Code, test.wantCode)
			}
			if (got.Status == CheckFailed) != (test.wantCode != "") {
				t.Errorf("status = %s with code %q", got.Status, got.Code)
			}
			if len(got.Tables) != len(test.missing) {
				t.Fatalf("findings = %+v, want %v", got.Tables, test.missing)
			}
			for index, name := range test.missing {
				if got.Tables[index].Key != name || got.Tables[index].Problem != ProblemMissing {
					t.Errorf("finding %d = %+v, want %s missing", index, got.Tables[index], name)
				}
			}
		})
	}
}

func TestCheckRowCounts(t *testing.T) {
	const schema, table, key = "public", "orders", "public.orders"

	tests := []struct {
		name      string
		baseline  int64
		measured  int64
		tolerance float64
		overrides map[string]float64
		wantFail  bool
	}{
		{name: "unchanged", baseline: 100, measured: 100, tolerance: 0},
		{name: "exactly at the tolerance", baseline: 100, measured: 90, tolerance: 10},
		{name: "just past the tolerance", baseline: 100, measured: 89, tolerance: 10, wantFail: true},
		{name: "growth past the tolerance", baseline: 100, measured: 111, tolerance: 10, wantFail: true},
		{name: "all rows lost", baseline: 100, measured: 0, tolerance: 50, wantFail: true},
		{name: "override allows more", baseline: 100, measured: 50, tolerance: 10, overrides: map[string]float64{key: 60}},
		{name: "override allows less", baseline: 100, measured: 95, tolerance: 10, overrides: map[string]float64{key: 1}, wantFail: true},
		{name: "empty table that filled up", baseline: 0, measured: 500, tolerance: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := checkRowCounts(Checks{
				Baseline:              []KeyedCount{{Key: key, Rows: test.baseline}},
				TolerancePercent:      test.tolerance,
				TableTolerancePercent: test.overrides,
			}, []TableCount{{Schema: schema, Name: table, Rows: test.measured}})

			if (got.Status == CheckFailed) != test.wantFail {
				t.Fatalf("status = %s, want failure %t: %+v", got.Status, test.wantFail, got)
			}
			if !test.wantFail {
				return
			}
			finding := got.Tables[0]
			if *finding.MeasuredRows != test.measured || *finding.ExpectedRows != test.baseline {
				t.Errorf("finding reports %d rows, expected %d, want %d and %d", *finding.MeasuredRows, *finding.ExpectedRows, test.measured, test.baseline)
			}
		})
	}
}

func TestCheckRowCountsWithoutBaselinePasses(t *testing.T) {
	got := checkRowCounts(Checks{}, []TableCount{{Schema: "public", Name: "orders", Rows: 1}})
	if got.Status != CheckPassed {
		t.Errorf("status = %s, want a first run to pass", got.Status)
	}
}

func TestCheckLatestTimestamp(t *testing.T) {
	backup := time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)
	check := &TimestampCheck{Table: TableRef{Schema: "public", Name: "orders"}, Column: "placed_at", MaxAge: 24 * time.Hour}

	tests := []struct {
		name     string
		reading  TimestampReading
		wantCode string
	}{
		{name: "recent", reading: TimestampReading{Value: backup.Add(-time.Hour)}},
		{name: "exactly at the limit", reading: TimestampReading{Value: backup.Add(-24 * time.Hour)}},
		{name: "newer than the backup", reading: TimestampReading{Value: backup.Add(time.Hour)}},
		{name: "too old", reading: TimestampReading{Value: backup.Add(-24*time.Hour - time.Second)}, wantCode: "timestamp_too_old"},
		{name: "column gone", reading: TimestampReading{State: TimestampNoColumn}, wantCode: "timestamp_missing"},
		{name: "no values", reading: TimestampReading{State: TimestampNoRows}, wantCode: "timestamp_no_rows"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := checkLatestTimestamp(check, &test.reading, backup)
			if got.Code != test.wantCode {
				t.Errorf("code = %q, want %q", got.Code, test.wantCode)
			}
			if (got.Status == CheckFailed) != (test.wantCode != "") {
				t.Errorf("status = %s with code %q", got.Status, got.Code)
			}
		})
	}

	if got := checkLatestTimestamp(nil, nil, backup); got.Status != CheckSkipped {
		t.Errorf("status = %s, want an unconfigured check to be skipped", got.Status)
	}
}

func TestFailureReasonsNeverNameTables(t *testing.T) {
	const secretName = "private_table_name"
	reading := TimestampReading{State: TimestampNoColumn}
	results := evaluateChecks(Checks{
		ExpectedTables:  []string{"public." + secretName},
		Baseline:        []KeyedCount{{Key: "public.orders", Rows: 100}},
		LatestTimestamp: &TimestampCheck{Table: TableRef{Schema: "public", Name: secretName}, Column: secretName, MaxAge: time.Hour},
	}, []TableCount{{Schema: "public", Name: "orders", Rows: 1}}, &reading, time.Now())

	for _, result := range results {
		if result.Status != CheckFailed {
			t.Errorf("%s status = %s, want fail", result.Name, result.Status)
		}
		if strings.Contains(result.Reason, secretName) || strings.Contains(result.Reason, "orders") {
			t.Errorf("%s reason names a table: %q", result.Name, result.Reason)
		}
	}

	var failure *Failure
	if !errors.As(firstFailure(results), &failure) || failure.Code != "table_missing" {
		t.Errorf("firstFailure() = %+v, want the first failed check", failure)
	}
}

func TestChecksCompareHashedTableKeys(t *testing.T) {
	key := HashedTableKey(config.NewSecret("test-salt-with-enough-characters-1"))
	orders := TableRef{Schema: "public", Name: "orders"}

	results := evaluateChecks(Checks{
		TableKey:         key,
		Baseline:         []KeyedCount{{Key: key(orders), Rows: 100}},
		TolerancePercent: 10,
	}, []TableCount{{Schema: "public", Name: "orders", Rows: 50}}, nil, time.Now())

	if results[0].Status != CheckPassed {
		t.Errorf("tables check = %s, want the hashed table to be found", results[0].Status)
	}
	if results[1].Code != "row_count_changed" || results[1].Tables[0].Key != key(orders) {
		t.Errorf("row count check = %+v, want the change reported under the hashed key", results[1])
	}
}

func TestHashedTableKey(t *testing.T) {
	salt := config.NewSecret("test-salt-with-enough-characters-1")
	key := HashedTableKey(salt)

	// Calculated separately: HMAC-SHA256 of "public\x00orders" with the salt as the key.
	const want = "388d47b3e69f29d66413028fae1ad9ba9c89eb2a50878cb99bbec7f6d013ec71"
	if got := key(TableRef{Schema: "public", Name: "orders"}); got != want {
		t.Errorf("HashedTableKey() = %s, want %s", got, want)
	}

	other := HashedTableKey(config.NewSecret("another-salt-with-enough-characters"))
	if key(TableRef{Schema: "public", Name: "orders"}) == other(TableRef{Schema: "public", Name: "orders"}) {
		t.Error("two salts gave the same key")
	}
	if key(TableRef{Schema: "a.b", Name: "c"}) == key(TableRef{Schema: "a", Name: "b.c"}) {
		t.Error("two different tables gave the same key")
	}
}
