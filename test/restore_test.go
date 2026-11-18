//go:build integration

package test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

func TestDumpsFromEverySupportedMajorRestore(t *testing.T) {
	for major := postgres.OldestMajor; major <= postgres.NewestMajor; major++ {
		t.Run(fmt.Sprintf("postgres_%d", major), func(t *testing.T) {
			t.Parallel()
			runner := newTestRunner(t)
			dumps := generateDumps(t, runner, major)

			formats := []struct {
				format archive.Format
				path   string
			}{
				{archive.FormatPlain, dumps.healthy.plain},
				{archive.FormatCustom, dumps.healthy.custom},
			}
			for _, test := range formats {
				t.Run(string(test.format), func(t *testing.T) {
					result := runner.restore(t, test.path)

					if result.Status != job.StatusPass {
						t.Fatalf("status = %s at %s: %s (%s)", result.Status, result.Stage, result.Reason, result.ReasonCode)
					}
					backup := result.Backup
					if backup.Format != test.format {
						t.Errorf("format = %s, want %s", backup.Format, test.format)
					}
					if backup.PostgresMajor != major || backup.RestoredMajor != major {
						t.Errorf("detected %d and restored into %d, want %d", backup.PostgresMajor, backup.RestoredMajor, major)
					}
					if backup.VersionSource != job.VersionFromDump {
						t.Errorf("version source = %s, want the dump", backup.VersionSource)
					}

					got := make(map[string]int64, len(result.Tables))
					for _, table := range result.Tables {
						got[table.Schema+"."+table.Name] = table.Rows
					}
					if len(got) != len(fixtureTables) {
						t.Errorf("tables = %v, want %v", got, fixtureTables)
					}
					for name, rows := range fixtureTables {
						if got[name] != rows {
							t.Errorf("%s has %d rows, want %d", name, got[name], rows)
						}
					}
					runner.assertNothingLeft(t)
				})
			}

			truncated := []struct {
				name      string
				path      string
				wantStage job.Stage
				wantCode  string
			}{
				{"truncated_plain", truncatedCopy(t, dumps.healthy.plain), job.StageDecompress, "truncated_backup"},
				{"truncated_custom", truncatedCopy(t, dumps.healthy.custom), job.StageRestore, "restore_failed"},
			}
			for _, test := range truncated {
				t.Run(test.name, func(t *testing.T) {
					result := runner.restore(t, test.path)

					if result.Status != job.StatusFail || result.Stage != test.wantStage || result.ReasonCode != test.wantCode {
						t.Errorf("result = %s at %q (%s), want fail at %q (%s)", result.Status, result.Stage, result.ReasonCode, test.wantStage, test.wantCode)
					}
					if result.Reason == "" {
						t.Error("result has no reason")
					}
					runner.assertNothingLeft(t)
				})
			}
		})
	}
}

// truncatedCopy keeps two thirds of a dump, like an upload or disk that failed partway.
func truncatedCopy(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	target := filepath.Join(t.TempDir(), "truncated-"+filepath.Base(path))
	if err := os.WriteFile(target, contents[:len(contents)*2/3], 0o600); err != nil { //nolint:gosec // The path is inside the test's temp directory.
		t.Fatalf("write truncated dump: %v", err)
	}
	return target
}

func TestFailedRestoreKeepsDumpContentsOutOfTheResult(t *testing.T) {
	const canary = "private-row-value"
	path := filepath.Join(t.TempDir(), "broken.sql")
	dump := "-- Dumped from database version 17.4\n" +
		"CREATE TABLE people (email text UNIQUE);\n" +
		"INSERT INTO people VALUES ('" + canary + "');\n" +
		"INSERT INTO people VALUES ('" + canary + "');\n"
	if err := os.WriteFile(path, []byte(dump), 0o600); err != nil {
		t.Fatalf("write dump: %v", err)
	}

	runner := newTestRunner(t)
	result := runner.restore(t, path)

	if result.Status != job.StatusFail || result.Stage != job.StageRestore || result.ReasonCode != "restore_failed" {
		t.Errorf("result = %s at %s (%s), want a failed restore", result.Status, result.Stage, result.ReasonCode)
	}
	if result.Reason == "" {
		t.Error("result has no reason")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	if strings.Contains(string(encoded), canary) {
		t.Errorf("result leaked dump contents: %s", encoded)
	}
	runner.assertNothingLeft(t)
}

func TestThrowawayContainersAreIsolatedAndLabelled(t *testing.T) {
	// Postgres 18 declares a different data volume from earlier majors.
	for _, major := range []int{postgres.NewestMajor - 1, postgres.NewestMajor} {
		t.Run(fmt.Sprintf("postgres_%d", major), func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			runner := newTestRunner(t)
			jobID := randomID(t)

			limits := docker.Limits{MemoryBytes: 512 << 20, NanoCPUs: 1_500_000_000}
			started, err := postgres.New(runner.docker, limits).Start(ctx, jobID, job.Version{Major: major, Source: job.VersionFromDump})
			if err != nil {
				t.Fatalf("start: %v", err)
			}
			if started.Major() != major {
				t.Errorf("major = %d, want %d", started.Major(), major)
			}
			t.Cleanup(func() {
				cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
				defer cancel()
				if err := runner.docker.RemoveJob(cleanupCtx, jobID); err != nil {
					t.Errorf("remove job: %v", err)
				}
				runner.assertNothingLeft(t)
			})

			filters := make(client.Filters).Add("label", docker.LabelRunner+"="+runner.id, docker.LabelJob+"="+jobID)
			containers, err := runner.api.ContainerList(ctx, client.ContainerListOptions{All: true, Filters: filters})
			if err != nil || len(containers.Items) != 1 {
				t.Fatalf("list containers: %d found, error %v", len(containers.Items), err)
			}
			inspected, err := runner.api.ContainerInspect(ctx, containers.Items[0].ID, client.ContainerInspectOptions{})
			if err != nil {
				t.Fatalf("inspect container: %v", err)
			}

			config := inspected.Container.HostConfig
			if config.NetworkMode != "none" {
				t.Errorf("network = %s, want none", config.NetworkMode)
			}
			if len(config.PortBindings) != 0 || config.PublishAllPorts {
				t.Errorf("container publishes ports: %v", config.PortBindings)
			}
			if config.Memory != limits.MemoryBytes || config.MemorySwap != limits.MemoryBytes || config.NanoCPUs != limits.NanoCPUs {
				t.Errorf("limits = %d memory, %d swap, %d cpus, want %+v without swap", config.Memory, config.MemorySwap, config.NanoCPUs, limits)
			}

			volumes, err := runner.api.VolumeList(ctx, client.VolumeListOptions{Filters: filters})
			if err != nil {
				t.Fatalf("list volumes: %v", err)
			}
			labelled := make(map[string]bool, len(volumes.Items))
			for _, volume := range volumes.Items {
				labelled[volume.Name] = true
			}
			if len(inspected.Container.Mounts) == 0 {
				t.Error("container has no data volume")
			}
			for _, attached := range inspected.Container.Mounts {
				if attached.Type != mount.TypeVolume || !labelled[attached.Name] {
					t.Errorf("mount %s at %s is not a labelled volume", attached.Name, attached.Destination)
				}
			}
		})
	}
}
