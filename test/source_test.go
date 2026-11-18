//go:build integration

package test

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/docker"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
	"github.com/backproofhq/runner/internal/source"
)

// Chainguard only publishes a free latest tag, so the image is pinned by digest and bumped by hand.
const minioImage = "cgr.dev/chainguard/minio@sha256:7618b82bba99aadd77fcf59eb6cfd89b4b8b8e01ac796c474d57a1c1abf17e79"

const testBucket = "backups"

type testStorage struct {
	endpoint  string
	accessKey string
	secretKey string
	api       *minio.Client
}

// startStorage labels MinIO with its own runner ID, because a runner removes leftovers with its label.
func startStorage(t *testing.T) *testStorage {
	t.Helper()
	ctx := t.Context()
	accessKey, secretKey := "test"+randomID(t), "test"+randomID(t)+randomID(t)

	server, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Image:        minioImage,
			Cmd:          []string{"server", "/tmp/data"},
			Env:          map[string]string{"MINIO_ROOT_USER": accessKey, "MINIO_ROOT_PASSWORD": secretKey},
			ExposedPorts: []string{"9000/tcp"},
			Labels:       map[string]string{docker.LabelRunner: "test-storage-" + randomID(t), docker.LabelJob: "storage-" + randomID(t)},
			WaitingFor:   wait.ForHTTP("/minio/health/ready").WithPort("9000/tcp").WithStartupTimeout(time.Minute),
		},
		Started: true,
	})
	if err != nil {
		t.Fatalf("start storage: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Terminate(context.WithoutCancel(ctx)); err != nil {
			t.Errorf("stop storage: %v", err)
		}
	})

	address, err := server.PortEndpoint(ctx, "9000/tcp", "")
	if err != nil {
		t.Fatalf("find storage address: %v", err)
	}
	api, err := minio.New(address, &minio.Options{Creds: credentials.NewStaticV4(accessKey, secretKey, ""), TrailingHeaders: true})
	if err != nil {
		t.Fatalf("connect to storage: %v", err)
	}
	if err := api.MakeBucket(ctx, testBucket, minio.MakeBucketOptions{}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}
	return &testStorage{endpoint: "http://" + address, accessKey: accessKey, secretKey: secretKey, api: api}
}

func (s *testStorage) put(t *testing.T, key string, data []byte) {
	t.Helper()
	_, err := s.api.PutObject(t.Context(), testBucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		Checksum: minio.ChecksumSHA256,
	})
	if err != nil {
		t.Fatalf("upload %s: %v", key, err)
	}
	// Listings report modification times, so uploads need to be distinguishable.
	time.Sleep(1100 * time.Millisecond)
}

func (s *testStorage) config(prefix, pattern string) source.Config {
	return source.Config{
		ID:        "PROD_DB",
		Endpoint:  s.endpoint,
		Bucket:    testBucket,
		Prefix:    prefix,
		Pattern:   pattern,
		AccessKey: config.NewSecret(s.accessKey),
		SecretKey: config.NewSecret(s.secretKey),
	}
}

func (r *testRunner) restoreFromSource(t *testing.T, cfg source.Config, options job.Options) job.Result {
	t.Helper()
	options.WorkDir = r.workDir
	options.SourceID = cfg.ID
	logger := slog.New(slog.DiscardHandler)
	return job.RunSource(t.Context(), postgres.New(r.docker, docker.Limits{}), source.New(cfg), logger, randomID(t), options)
}

func randomBytes(t *testing.T, size int) []byte {
	t.Helper()
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		t.Fatalf("create random data: %v", err)
	}
	return data
}

func TestNewestMatchingObjectIsDownloadedAndRestored(t *testing.T) {
	storage := startStorage(t)
	keys := newFixtureKeys(t)
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, postgres.NewestMajor)
	healthy := encodeAge(t, keys.identity.Recipient(), encodeGzip(t, readFixture(t, dumps.healthy.custom)))

	// Picking any of the others ends in a failure, so a pass proves the right object was chosen.
	storage.put(t, "prod/daily-1.dump.gz.age", randomBytes(t, 4096))
	storage.put(t, "prod/daily-2.dump.gz.age", randomBytes(t, 4096))
	storage.put(t, "prod/daily-3.dump.gz.age", healthy)
	storage.put(t, "prod/notes.txt", randomBytes(t, 4096))
	storage.put(t, "staging/daily-4.dump.gz.age", randomBytes(t, 4096))

	result := runner.restoreFromSource(t, storage.config("prod/", "*.dump.gz.age"), job.Options{Secrets: keys.secrets()})

	if result.Status != job.StatusPass {
		t.Fatalf("result = %s at %q (%s): %s", result.Status, result.Stage, result.ReasonCode, result.Reason)
	}
	if result.Backup.ObjectKey != "prod/daily-3.dump.gz.age" {
		t.Errorf("object key = %q, want the newest matching object", result.Backup.ObjectKey)
	}
	if result.Backup.File != "daily-3.dump.gz.age" {
		t.Errorf("file = %q, want the object's file name", result.Backup.File)
	}
	if result.Backup.SizeBytes != int64(len(healthy)) {
		t.Errorf("size = %d, want %d", result.Backup.SizeBytes, len(healthy))
	}
	if age := time.Since(result.Backup.Time); age < 0 || age > 5*time.Minute {
		t.Errorf("backup time = %s, want the time of the upload", result.Backup.Time)
	}
	if len(result.Tables) != len(fixtureTables) {
		t.Errorf("restored %d tables, want %d", len(result.Tables), len(fixtureTables))
	}

	stages := make([]string, 0, len(result.Stages))
	for _, stage := range result.Stages {
		stages = append(stages, string(stage.Name))
	}
	if got, want := strings.Join(stages[:3], ","), "resolve,preflight,download"; got != want {
		t.Errorf("first stages = %s, want %s", got, want)
	}
	runner.assertNothingLeft(t)
	runner.assertNoWorkFiles(t)
}

func TestPinnedObjectIsRestoredInsteadOfTheNewest(t *testing.T) {
	storage := startStorage(t)
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, postgres.NewestMajor)

	storage.put(t, "prod/chosen.dump", readFixture(t, dumps.healthy.custom))
	storage.put(t, "prod/newer.dump", randomBytes(t, 4096))

	cfg := storage.config("prod/", "*.dump")
	cfg.ObjectKey = "prod/chosen.dump"
	result := runner.restoreFromSource(t, cfg, job.Options{})

	if result.Status != job.StatusPass || result.Backup.ObjectKey != cfg.ObjectKey {
		t.Errorf("result = %s for %q (%s), want a pass for the pinned object", result.Status, result.Backup.ObjectKey, result.ReasonCode)
	}
	runner.assertNothingLeft(t)
	runner.assertNoWorkFiles(t)
}

func TestBrokenBackupsInStorage(t *testing.T) {
	storage := startStorage(t)
	keys := newFixtureKeys(t)
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, postgres.NewestMajor)
	custom := readFixture(t, dumps.healthy.custom)
	cut := func(data []byte) []byte { return data[:len(data)*2/3] }

	stranger := config.SourceSecrets{
		AgeIdentity: config.NewSecret(keys.other.String()),
		GPGKey:      config.NewSecret(keys.stranger),
	}

	storage.put(t, "truncated-gzip/backup.dump.gz", cut(encodeGzip(t, custom)))
	storage.put(t, "truncated-custom/backup.dump", cut(custom))
	storage.put(t, "wrong-age/backup.dump.age", encodeAge(t, keys.identity.Recipient(), custom))
	storage.put(t, "wrong-gpg/backup.dump.gpg", encodeGPG(t, keys.entity, custom))
	storage.put(t, "healthy/backup.dump", custom)

	wrongSecret := storage.config("healthy/", "")
	wrongSecret.SecretKey = config.NewSecret("wrong" + storage.secretKey)
	wrongAccess := storage.config("healthy/", "")
	wrongAccess.AccessKey = config.NewSecret("wrong" + storage.accessKey)
	missingBucket := storage.config("healthy/", "")
	missingBucket.Bucket = "no-such-bucket"
	noCredentials := storage.config("healthy/", "")
	noCredentials.AccessKey = config.Secret{}

	tests := []struct {
		name       string
		config     source.Config
		options    job.Options
		wantStatus job.Status
		wantStage  job.Stage
		wantCode   string
	}{
		{name: "no objects under the prefix", config: storage.config("nothing-here/", ""), wantStatus: job.StatusFail, wantStage: job.StageResolve, wantCode: "no_backup_found"},
		{name: "no object matches the pattern", config: storage.config("healthy/", "*.sql"), wantStatus: job.StatusFail, wantStage: job.StageResolve, wantCode: "no_backup_found"},
		{name: "truncated compressed backup", config: storage.config("truncated-gzip/", ""), wantStatus: job.StatusFail, wantStage: job.StageDecompress, wantCode: "damaged_archive"},
		{name: "truncated custom dump", config: storage.config("truncated-custom/", ""), wantStatus: job.StatusFail, wantStage: job.StageRestore, wantCode: "restore_failed"},
		{name: "wrong age key", config: storage.config("wrong-age/", ""), options: job.Options{Secrets: stranger}, wantStatus: job.StatusFail, wantStage: job.StageDecrypt, wantCode: "wrong_key"},
		{name: "wrong gpg key", config: storage.config("wrong-gpg/", ""), options: job.Options{Secrets: stranger}, wantStatus: job.StatusFail, wantStage: job.StageDecrypt, wantCode: "wrong_key"},
		{name: "wrong secret key", config: wrongSecret, wantStatus: job.StatusError, wantStage: job.StageResolve, wantCode: "source_credentials"},
		{name: "wrong access key", config: wrongAccess, wantStatus: job.StatusError, wantStage: job.StageResolve, wantCode: "source_credentials"},
		{name: "missing bucket", config: missingBucket, wantStatus: job.StatusError, wantStage: job.StageResolve, wantCode: "source_bucket_missing"},
		{name: "no credentials on this runner", config: noCredentials, wantStatus: job.StatusError, wantStage: job.StageResolve, wantCode: "missing_credentials"},
		{name: "work volume too small", config: storage.config("healthy/", ""), options: job.Options{DiskMargin: 1e15}, wantStatus: job.StatusError, wantStage: job.StagePreflight, wantCode: "disk_too_small"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runner.restoreFromSource(t, test.config, test.options)

			if result.Status != test.wantStatus || result.Stage != test.wantStage || result.ReasonCode != test.wantCode {
				t.Errorf("result = %s at %q (%s), want %s at %q (%s)", result.Status, result.Stage, result.ReasonCode, test.wantStatus, test.wantStage, test.wantCode)
			}
			if result.Reason == "" {
				t.Error("result has no reason")
			}
			if strings.Contains(fmt.Sprintf("%+v", result), storage.secretKey) {
				t.Error("result contains the storage secret key")
			}
			runner.assertNothingLeft(t)
			runner.assertNoWorkFiles(t)
		})
	}
}
