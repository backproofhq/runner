//go:build integration

package test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

// fixtureSchema covers what real dumps contain and a naive restore trips over: roles that exist
// only on the source server, row-level security, partitions, foreign keys and awkward names.
const fixtureSchema = `
CREATE ROLE app_owner;
CREATE ROLE app_reader;
CREATE SCHEMA sales AUTHORIZATION app_owner;

CREATE TABLE public.customers (id bigserial PRIMARY KEY, name text NOT NULL);
ALTER TABLE public.customers OWNER TO app_owner;
INSERT INTO public.customers (name) VALUES ('Ada'), ('O''Brien'), (E'Line\nBreak\tTab');
GRANT SELECT ON public.customers TO app_reader;
ALTER TABLE public.customers ENABLE ROW LEVEL SECURITY;
CREATE POLICY reader_rows ON public.customers FOR SELECT TO app_reader USING (true);

CREATE TABLE sales.orders (
  id serial PRIMARY KEY,
  customer_id bigint NOT NULL REFERENCES public.customers (id),
  total numeric(10, 2) NOT NULL,
  placed_at timestamptz NOT NULL
);
ALTER TABLE sales.orders OWNER TO app_owner;
INSERT INTO sales.orders (customer_id, total, placed_at) VALUES
  (1, 10.50, '2026-01-10 12:00:00+00'),
  (2, 99.99, '2026-01-15 12:00:00+00');

CREATE TABLE public.events (id int NOT NULL, happened_on date NOT NULL) PARTITION BY RANGE (happened_on);
CREATE TABLE public.events_2025 PARTITION OF public.events FOR VALUES FROM ('2025-01-01') TO ('2026-01-01');
CREATE TABLE public.events_2026 PARTITION OF public.events FOR VALUES FROM ('2026-01-01') TO ('2027-01-01');
INSERT INTO public.events VALUES (1, '2025-03-01'), (2, '2025-04-01'), (3, '2026-05-01'), (4, '2026-06-01');

CREATE TABLE public."Odd Name" (id int);
INSERT INTO public."Odd Name" VALUES (1);

CREATE TABLE public.audit_log (happened_at timestamp NOT NULL);
INSERT INTO public.audit_log VALUES ('2026-01-14 08:00:00');

CREATE TABLE public.empty_log (happened_at timestamptz);
`

const fixtureJobPrefix = "fixture-"

var fixtureTables = map[string]int64{
	"public.audit_log": 1,
	"public.customers": 3,
	"public.empty_log": 0,
	"public.events":    4,
	"public.Odd Name":  1,
	"sales.orders":     2,
}

// testRunner gives each test its own runner label, so tests can't see each other's containers.
type testRunner struct {
	id      string
	docker  *docker.Client
	api     *client.Client
	workDir string
}

func newTestRunner(t *testing.T) *testRunner {
	t.Helper()
	id := "test-" + randomID(t)

	dockerClient, err := docker.New(id)
	if err != nil {
		t.Fatalf("create docker client: %v", err)
	}
	api, err := client.New(client.FromEnv)
	if err != nil {
		t.Fatalf("create api client: %v", err)
	}
	t.Cleanup(func() {
		_ = dockerClient.Close()
		_ = api.Close()
	})
	return &testRunner{id: id, docker: dockerClient, api: api, workDir: t.TempDir()}
}

func (r *testRunner) restore(t *testing.T, path string) job.Result {
	t.Helper()
	return r.restoreWithChecks(t, path, job.Checks{})
}

func (r *testRunner) restoreWithChecks(t *testing.T, path string, checks job.Checks) job.Result {
	t.Helper()
	return r.restoreWithContext(t.Context(), t, path, checks)
}

func (r *testRunner) restoreWithContext(ctx context.Context, t *testing.T, path string, checks job.Checks) job.Result {
	t.Helper()
	return r.restoreWithOptions(ctx, t, path, job.Options{Checks: checks})
}

func (r *testRunner) restoreWithOptions(ctx context.Context, t *testing.T, path string, options job.Options) job.Result {
	t.Helper()
	options.WorkDir = r.workDir
	options.SourceID = "LOCAL"
	logger := slog.New(slog.DiscardHandler)
	return job.RunLocal(ctx, postgres.New(r.docker, docker.Limits{}), logger, randomID(t), path, options)
}

func (r *testRunner) labels(jobID string) map[string]string {
	return map[string]string{docker.LabelRunner: r.id, docker.LabelJob: jobID}
}

func (r *testRunner) assertNothingLeft(t *testing.T) {
	t.Helper()
	ctx := context.WithoutCancel(t.Context())
	filters := make(client.Filters).Add("label", docker.LabelRunner+"="+r.id)

	containers, err := r.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
	if err != nil {
		t.Fatalf("list containers: %v", err)
	}
	if len(containers.Items) != 0 {
		t.Errorf("%d containers left behind", len(containers.Items))
	}
	volumes, err := r.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
	if err != nil {
		t.Fatalf("list volumes: %v", err)
	}
	if len(volumes.Items) != 0 {
		t.Errorf("%d volumes left behind", len(volumes.Items))
	}
}

type dumpPair struct {
	plain  string
	custom string
}

type dumpFiles struct {
	healthy   dumpPair
	empty     dumpPair
	extension dumpPair
}

// fixtureExtension installs a stand-in for postgis on the fixture server only, so its dumps
// really need an extension that the restore image doesn't have.
const fixtureExtension = `
directory=/usr/share/postgresql/%d/extension
printf "comment = 'fixture'\ndefault_version = '1.0'\nrelocatable = true\n" > "$directory/postgis.control"
echo "CREATE FUNCTION fixture_extension_marker() RETURNS int LANGUAGE sql AS 'SELECT 1';" > "$directory/postgis--1.0.sql"
`

// generateDumps creates real dumps with the pg_dump of the given major.
func generateDumps(t *testing.T, runner *testRunner, major int) dumpFiles {
	t.Helper()
	ctx := t.Context()
	jobID := fixtureJobPrefix + randomID(t)
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := runner.docker.RemoveJob(cleanupCtx, jobID); err != nil {
			t.Errorf("remove fixture container: %v", err)
		}
	})

	dataPath := "/var/lib/postgresql/data"
	if major >= 18 {
		dataPath = "/var/lib/postgresql"
	}
	labels := runner.labels(jobID)

	server, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:  fmt.Sprintf("postgres:%d", major),
			Env:    map[string]string{"POSTGRES_HOST_AUTH_METHOD": "trust", "POSTGRES_DB": "fixture"},
			Labels: labels,
			HostConfigModifier: func(config *container.HostConfig) {
				config.NetworkMode = "none"
				config.Mounts = []mount.Mount{{
					Type:          mount.TypeVolume,
					Source:        jobID,
					Target:        dataPath,
					VolumeOptions: &mount.VolumeOptions{Labels: labels},
				}}
			},
			WaitingFor: wait.ForExec([]string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "fixture"}).
				WithStartupTimeout(2 * time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start postgres %d: %v", major, err)
	}

	run := func(command ...string) {
		t.Helper()
		code, output, err := server.Exec(ctx, command)
		if err != nil {
			t.Fatalf("%s: %v", command[0], err)
		}
		if code != 0 {
			logs, _ := io.ReadAll(output)
			t.Fatalf("%s exited with %d: %s", command[0], code, logs)
		}
	}
	run("psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "fixture", "-c", fixtureSchema)
	run("createdb", "-U", "postgres", "empty_database")
	run("sh", "-c", fmt.Sprintf(fixtureExtension, major))
	run("createdb", "-U", "postgres", "with_extension")
	run("psql", "-X", "-v", "ON_ERROR_STOP=1", "-U", "postgres", "-d", "with_extension", "-c", "CREATE EXTENSION postgis")

	dump := func(database, name string) dumpPair {
		t.Helper()
		plain, custom := "/tmp/"+name+".sql", "/tmp/"+name+".dump"
		run("pg_dump", "-U", "postgres", "-d", database, "--format=plain", "-f", plain)
		run("pg_dump", "-U", "postgres", "-d", database, "--format=custom", "-f", custom)
		return dumpPair{plain: plain, custom: custom}
	}
	healthy := dump("fixture", "healthy")
	empty := dump("empty_database", "empty")
	extension := dump("with_extension", "extension")

	directory := t.TempDir()
	copyOut := func(source, name string) string {
		t.Helper()
		reader, err := server.CopyFileFromContainer(ctx, source)
		if err != nil {
			t.Fatalf("copy %s: %v", source, err)
		}
		defer func() { _ = reader.Close() }()

		path := filepath.Join(directory, name)
		file, err := os.Create(path) //nolint:gosec // The path is inside the test's temp directory.
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		defer func() { _ = file.Close() }()
		if _, err := io.Copy(file, reader); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return path
	}
	copyPair := func(pair dumpPair) dumpPair {
		t.Helper()
		return dumpPair{
			plain:  copyOut(pair.plain, filepath.Base(pair.plain)),
			custom: copyOut(pair.custom, filepath.Base(pair.custom)),
		}
	}
	files := dumpFiles{healthy: copyPair(healthy), empty: copyPair(empty), extension: copyPair(extension)}

	// The server is finished, and a runner sweeps up anything else carrying its label.
	if err := runner.docker.RemoveJob(ctx, jobID); err != nil {
		t.Fatalf("remove fixture server: %v", err)
	}
	return files
}

func randomID(t *testing.T) string {
	t.Helper()
	id := make([]byte, 6)
	if _, err := rand.Read(id); err != nil {
		t.Fatalf("create id: %v", err)
	}
	return hex.EncodeToString(id)
}
