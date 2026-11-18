package postgres

import (
	"bytes"
	"context"
	"fmt"
	"strconv"

	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/job"
)

const (
	exactRows     = `(xpath('/row/count/text()', query_to_xml(format('SELECT count(*) FROM %I.%I', n.nspname, c.relname), false, true, '')))[1]::text::bigint`
	estimatedRows = `greatest(c.reltuples, 0)::bigint`
)

// Partitions are left out because counting a partitioned table already includes their rows.
const tablesFrom = `
FROM pg_class c
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND NOT c.relispartition
  AND n.nspname <> 'information_schema'
  AND n.nspname NOT LIKE 'pg\_%'
  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
ORDER BY 1, 2`

// Tables counts the rows of every restored table, or estimates them for very large tables.
func (i *Instance) Tables(ctx context.Context, estimate bool) ([]job.TableCount, error) {
	// NUL separators keep unusual table names from breaking the parsing.
	command := []string{"psql", "-X", "-q", "-A", "-t", "-z", "-0", "-v", "ON_ERROR_STOP=1", "-U", superuser, "-d", database}
	rows := exactRows
	if estimate {
		command = append(command, "-c", "ANALYZE")
		rows = estimatedRows
	}
	command = append(command, "-c", "SELECT n.nspname, c.relname, "+rows+tablesFrom)

	var output bytes.Buffer
	code, err := i.exec(ctx, docker.ExecSpec{Cmd: command, Stdout: &output})
	if err != nil {
		return nil, dockerFailure(err)
	}
	if code != 0 {
		return nil, job.RunnerError("count_failed", "The runner couldn't count the restored rows. Check the Docker host and try again.", nil)
	}
	return parseTables(output.Bytes())
}

func parseTables(output []byte) ([]job.TableCount, error) {
	tables := []job.TableCount{}
	fields := bytes.Split(bytes.TrimRight(output, "\x00"), []byte{0})
	if len(output) == 0 {
		return tables, nil
	}
	if len(fields)%3 != 0 {
		return nil, job.RunnerError("count_failed", "The runner couldn't read the restored row counts. Try again.", fmt.Errorf("got %d fields", len(fields)))
	}
	for index := 0; index < len(fields); index += 3 {
		rows, err := strconv.ParseInt(string(fields[index+2]), 10, 64)
		if err != nil {
			return nil, job.RunnerError("count_failed", "The runner couldn't read the restored row counts. Try again.", err)
		}
		tables = append(tables, job.TableCount{Schema: string(fields[index]), Name: string(fields[index+1]), Rows: rows})
	}
	return tables, nil
}
