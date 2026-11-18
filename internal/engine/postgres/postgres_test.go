package postgres

import (
	"errors"
	"strings"
	"testing"

	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/job"
)

func TestParseVersion(t *testing.T) {
	tests := []struct {
		name      string
		format    archive.Format
		header    string
		wantFull  string
		wantMajor int
	}{
		{"plain", archive.FormatPlain, "--\n-- Dumped from database version 17.2 (Debian 17.2-1.pgdg120+1)\n-- Dumped by pg_dump version 17.2\n", "17.2", 17},
		{"plain from an old server", archive.FormatPlain, "-- Dumped from database version 9.6.24\n", "9.6.24", 9},
		{"custom", archive.FormatCustom, ";\n; Archive created at 2026-11-18\n;     Dumped from database version: 14.13\n;     Dumped by pg_restore\n", "14.13", 14},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := parseVersion(test.format, []byte(test.header))
			if !ok {
				t.Fatal("parseVersion() found no version")
			}
			if got.Full != test.wantFull || got.Major != test.wantMajor || got.Source != job.VersionFromDump {
				t.Errorf("parseVersion() = %+v, want %s (major %d) from the dump", got, test.wantFull, test.wantMajor)
			}
		})
	}
}

func TestParseVersionIgnoresDumpContents(t *testing.T) {
	header := "CREATE TABLE notes (body text);\nINSERT INTO notes VALUES ('-- Dumped from database version 99.1');\n"
	if got, ok := parseVersion(archive.FormatPlain, []byte(header)); ok {
		t.Errorf("parseVersion() = %+v, want no version from row data", got)
	}
}

func TestRestoreMajor(t *testing.T) {
	for dump, want := range map[int]int{9: 14, 13: 14, 14: 14, 16: 16, 18: 18} {
		got, err := restoreMajor(dump)
		if err != nil || got != want {
			t.Errorf("restoreMajor(%d) = %d, %v, want %d", dump, got, err, want)
		}
	}

	_, err := restoreMajor(19)
	var failure *job.Failure
	if !errors.As(err, &failure) || failure.Status != job.StatusFail {
		t.Fatalf("restoreMajor(19) error = %v, want a failed backup", err)
	}
	for _, version := range []string{"19", "18"} {
		if !strings.Contains(failure.Reason, version) {
			t.Errorf("reason %q does not name Postgres %s", failure.Reason, version)
		}
	}
}

func TestRestoreOutputClassifiesFailures(t *testing.T) {
	tests := []struct {
		name       string
		line       string
		wantStatus job.Status
		wantCode   string
		wantReason string
	}{
		{"missing extension", `psql:<stdin>:12: ERROR:  extension "postgis" is not available`, job.StatusFail, "missing_extension", "postgis"},
		{"missing control file", `pg_restore: error: could not open extension control file "/usr/share/postgresql/17/extension/vector.control": No such file or directory`, job.StatusFail, "missing_extension", "vector"},
		{"newer archive", `pg_restore: error: unsupported version (1.99) in file header`, job.StatusFail, "dump_version_unsupported", "1.99"},
		{"disk full", `psql:<stdin>:9: ERROR:  could not extend file "base/1/2": No space left on device`, job.StatusError, "disk_full", "disk space"},
		{"server crash", `server closed the connection unexpectedly`, job.StatusError, "database_stopped", "stopped"},
		{"any other error", `psql:<stdin>:3: ERROR:  syntax error at or near "x"`, job.StatusFail, "restore_failed", "error"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output := &restoreOutput{}
			output.observe(test.line)

			var failure *job.Failure
			if !errors.As(output.failure(17), &failure) {
				t.Fatal("failure() did not return a job failure")
			}
			if failure.Status != test.wantStatus || failure.Code != test.wantCode {
				t.Errorf("failure = %s/%s, want %s/%s", failure.Status, failure.Code, test.wantStatus, test.wantCode)
			}
			if !strings.Contains(failure.Reason, test.wantReason) {
				t.Errorf("reason %q does not mention %q", failure.Reason, test.wantReason)
			}
		})
	}
}

func TestRestoreOutputNeverRepeatsDumpContents(t *testing.T) {
	const canary = "private-row-value"
	output := &restoreOutput{}
	lines := newLineWriter(output.observe)
	_, _ = lines.Write([]byte(strings.Join([]string{
		`psql:<stdin>:7: ERROR:  duplicate key value violates unique constraint "users_pkey"`,
		`DETAIL:  Key (email)=(` + canary + `) already exists.`,
		`LINE 1: INSERT INTO users VALUES ('` + canary + `');`,
		`Command was: INSERT INTO users VALUES ('` + canary + `', 'extension "bait" is not available');`,
		`CONTEXT:  COPY users, line 1: "` + canary + ` No space left on device"`,
	}, "\n")))
	lines.flush()

	failure := output.failure(17)
	if strings.Contains(failure.Error(), canary) {
		t.Errorf("reason leaked dump contents: %q", failure.Error())
	}
	var got *job.Failure
	if !errors.As(failure, &got) || got.Code != "restore_failed" {
		t.Errorf("failure = %+v, want restore_failed because echoed lines must not be classified", got)
	}
}

func TestRoleScanner(t *testing.T) {
	scanner := newRoleScanner()
	for _, line := range []string{
		`ALTER TABLE public.orders OWNER TO app_owner;`,
		`ALTER SCHEMA billing OWNER TO "Billing Team";`,
		`GRANT SELECT ON TABLE public.orders TO reader, "odd""name";`,
		`GRANT ALL ON SCHEMA public TO PUBLIC;`,
		`REVOKE ALL ON TABLE public.orders FROM auditor;`,
		`CREATE POLICY own_rows ON public.orders TO authenticated USING ((owner = CURRENT_USER));`,
		`ALTER DEFAULT PRIVILEGES FOR ROLE deployer IN SCHEMA public GRANT SELECT ON TABLES TO reader;`,
		`SET SESSION AUTHORIZATION 'migrator';`,
		`ALTER TABLE public.jobs OWNER TO postgres;`,
		`GRANT pg_read_all_data TO pg_monitor;`,
		`INSERT INTO notes VALUES ('GRANT ALL ON x TO row_data');`,
	} {
		scanner.observe(line)
	}

	want := strings.Join([]string{
		`CREATE ROLE "Billing Team";`,
		`CREATE ROLE "app_owner";`,
		`CREATE ROLE "auditor";`,
		`CREATE ROLE "authenticated";`,
		`CREATE ROLE "deployer";`,
		`CREATE ROLE "migrator";`,
		`CREATE ROLE "odd""name";`,
		`CREATE ROLE "reader";`,
	}, "\n") + "\n"
	if got := string(scanner.script()); got != want {
		t.Errorf("script() =\n%s\nwant\n%s", got, want)
	}
}

func TestParseTables(t *testing.T) {
	output := []byte("public\x00orders\x003\x00sales\x00odd name\x000\x00")
	got, err := parseTables(output)
	if err != nil {
		t.Fatalf("parseTables() error = %v", err)
	}
	want := []job.TableCount{
		{Schema: "public", Name: "orders", Rows: 3},
		{Schema: "sales", Name: "odd name", Rows: 0},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("parseTables() = %+v, want %+v", got, want)
	}
}
