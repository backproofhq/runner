//go:build integration

package test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/klauspost/compress/zstd"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/engine/postgres"
	"github.com/backproofhq/runner/internal/job"
)

type fixtureKeys struct {
	identity *age.X25519Identity
	other    *age.X25519Identity
	entity   *openpgp.Entity
	private  string
	stranger string
}

func exportedGPGKey(t *testing.T) (*openpgp.Entity, string) {
	t.Helper()
	entity, err := openpgp.NewEntity("Fixture", "", "fixture@example.invalid", nil)
	if err != nil {
		t.Fatalf("create gpg key: %v", err)
	}
	var exported bytes.Buffer
	armored, err := pgparmor.Encode(&exported, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatalf("armor gpg key: %v", err)
	}
	if err := entity.SerializePrivate(armored, nil); err != nil {
		t.Fatalf("export gpg key: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("close gpg armor: %v", err)
	}
	return entity, exported.String()
}

func newFixtureKeys(t *testing.T) fixtureKeys {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create age key: %v", err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create age key: %v", err)
	}
	entity, private := exportedGPGKey(t)
	_, stranger := exportedGPGKey(t)
	return fixtureKeys{identity: identity, other: other, entity: entity, private: private, stranger: stranger}
}

func (k fixtureKeys) secrets() config.SourceSecrets {
	return config.SourceSecrets{
		AgeIdentity: config.NewSecret(k.identity.String()),
		GPGKey:      config.NewSecret(k.private),
	}
}

func encodeGzip(t *testing.T, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	return output.Bytes()
}

func encodeZstd(t *testing.T, data []byte) []byte {
	t.Helper()
	writer, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer func() { _ = writer.Close() }()
	return writer.EncodeAll(data, nil)
}

func encodeAge(t *testing.T, recipient age.Recipient, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := age.Encrypt(&output, recipient)
	if err != nil {
		t.Fatalf("age: %v", err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("age: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("age: %v", err)
	}
	return output.Bytes()
}

func encodeGPG(t *testing.T, entity *openpgp.Entity, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	writer, err := openpgp.Encrypt(&output, []*openpgp.Entity{entity}, nil, nil, nil)
	if err != nil {
		t.Fatalf("gpg: %v", err)
	}
	if _, err := writer.Write(data); err != nil {
		t.Fatalf("gpg: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gpg: %v", err)
	}
	return output.Bytes()
}

func writeFixture(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func readFixture(t *testing.T, path string) []byte {
	t.Helper()
	contents, err := os.ReadFile(path) //nolint:gosec // The path is inside the test's temp directory.
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return contents
}

func (r *testRunner) assertNoWorkFiles(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(r.workDir, "jobs"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read work directory: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("%d job directories left in the work directory", len(entries))
	}
}

func TestCompressedAndEncryptedDumpsRestore(t *testing.T) {
	keys := newFixtureKeys(t)
	recipient := keys.identity.Recipient()

	encodings := []struct {
		name            string
		encode          func(*testing.T, []byte) []byte
		wantCompression string
		wantEncryption  string
	}{
		{"gzip", encodeGzip, "gzip", ""},
		{"zstd", encodeZstd, "zstd", ""},
		{"age", func(t *testing.T, data []byte) []byte { return encodeAge(t, recipient, data) }, "", "age"},
		{"gpg", func(t *testing.T, data []byte) []byte { return encodeGPG(t, keys.entity, data) }, "", "pgp"},
		{"gzip_then_age", func(t *testing.T, data []byte) []byte { return encodeAge(t, recipient, encodeGzip(t, data)) }, "gzip", "age"},
		{"zstd_then_gpg", func(t *testing.T, data []byte) []byte { return encodeGPG(t, keys.entity, encodeZstd(t, data)) }, "zstd", "pgp"},
	}

	for major := postgres.OldestMajor; major <= postgres.NewestMajor; major++ {
		t.Run(fmt.Sprintf("postgres_%d", major), func(t *testing.T) {
			t.Parallel()
			runner := newTestRunner(t)
			dumps := generateDumps(t, runner, major)

			for format, path := range map[string]string{"plain": dumps.healthy.plain, "custom": dumps.healthy.custom} {
				for _, encoding := range encodings {
					// Unpacking doesn't depend on the Postgres version, so older majors get one combined case.
					if major != postgres.NewestMajor && encoding.name != "gzip_then_age" {
						continue
					}
					t.Run(format+"/"+encoding.name, func(t *testing.T) {
						fixture := writeFixture(t, "backup.bin", encoding.encode(t, readFixture(t, path)))

						result := runner.restoreWithOptions(t.Context(), t, fixture, job.Options{Secrets: keys.secrets()})

						if result.Status != job.StatusPass {
							t.Fatalf("result = %s at %q (%s): %s", result.Status, result.Stage, result.ReasonCode, result.Reason)
						}
						if result.Backup.Compression != encoding.wantCompression || result.Backup.Encryption != encoding.wantEncryption {
							t.Errorf("layers = %q and %q, want %q and %q", result.Backup.Compression, result.Backup.Encryption, encoding.wantCompression, encoding.wantEncryption)
						}
						if string(result.Backup.Format) != format {
							t.Errorf("format = %s, want %s", result.Backup.Format, format)
						}
						if len(result.Tables) != len(fixtureTables) {
							t.Errorf("restored %d tables, want %d", len(result.Tables), len(fixtureTables))
						}
						runner.assertNothingLeft(t)
						runner.assertNoWorkFiles(t)
					})
				}
			}
		})
	}
}

func TestBackupCutToItsFirstBytesFails(t *testing.T) {
	keys := newFixtureKeys(t)
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, postgres.NewestMajor)
	custom := readFixture(t, dumps.healthy.custom)

	for name, data := range map[string][]byte{
		"gzip": encodeGzip(t, custom),
		"zstd": encodeZstd(t, custom),
		"age":  encodeAge(t, keys.identity.Recipient(), custom),
		"gpg":  encodeGPG(t, keys.entity, custom),
	} {
		for _, length := range []int{1, 3, 8, 16} {
			t.Run(fmt.Sprintf("%s_%d_bytes", name, length), func(t *testing.T) {
				result := runner.restoreWithOptions(t.Context(), t, writeFixture(t, "backup.bin", data[:length]), job.Options{Secrets: keys.secrets()})

				if result.Status != job.StatusFail {
					t.Errorf("result = %s at %q (%s), want a failed backup", result.Status, result.Stage, result.ReasonCode)
				}
				runner.assertNothingLeft(t)
				runner.assertNoWorkFiles(t)
			})
		}
	}
}

func TestBrokenCompressedAndEncryptedBackups(t *testing.T) {
	keys := newFixtureKeys(t)
	recipient := keys.identity.Recipient()
	runner := newTestRunner(t)
	dumps := generateDumps(t, runner, postgres.NewestMajor)
	custom := readFixture(t, dumps.healthy.custom)
	cut := func(data []byte) []byte { return data[:len(data)*2/3] }

	strangerSecrets := config.SourceSecrets{
		AgeIdentity: config.NewSecret(keys.other.String()),
		GPGKey:      config.NewSecret(keys.stranger),
	}

	tests := []struct {
		name       string
		data       []byte
		secrets    config.SourceSecrets
		wantStatus job.Status
		wantStage  job.Stage
		wantCode   string
	}{
		{"truncated gzip", cut(encodeGzip(t, custom)), keys.secrets(), job.StatusFail, job.StageDecompress, "damaged_archive"},
		{"truncated zstd", cut(encodeZstd(t, custom)), keys.secrets(), job.StatusFail, job.StageDecompress, "damaged_archive"},
		{"truncated age", cut(encodeAge(t, recipient, custom)), keys.secrets(), job.StatusFail, job.StageDecrypt, "damaged_encryption"},
		{"truncated gpg", cut(encodeGPG(t, keys.entity, custom)), keys.secrets(), job.StatusFail, job.StageDecrypt, "damaged_encryption"},
		{"age with a different key", encodeAge(t, recipient, custom), strangerSecrets, job.StatusFail, job.StageDecrypt, "wrong_key"},
		{"gpg with a different key", encodeGPG(t, keys.entity, custom), strangerSecrets, job.StatusFail, job.StageDecrypt, "wrong_key"},
		{"age without a key", encodeAge(t, recipient, custom), config.SourceSecrets{}, job.StatusError, job.StageDecrypt, "missing_key"},
		{"gpg without a key", encodeGPG(t, keys.entity, custom), config.SourceSecrets{}, job.StatusError, job.StageDecrypt, "missing_key"},
		{"gzip of an empty file", encodeGzip(t, nil), keys.secrets(), job.StatusFail, job.StageDecompress, "empty_backup"},
		{"gzip of an empty database", encodeGzip(t, readFixture(t, dumps.empty.custom)), keys.secrets(), job.StatusFail, job.StageCheck, "no_tables"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runner.restoreWithOptions(t.Context(), t, writeFixture(t, "backup.bin", test.data), job.Options{Secrets: test.secrets})

			if result.Status != test.wantStatus || result.Stage != test.wantStage || result.ReasonCode != test.wantCode {
				t.Errorf("result = %s at %q (%s), want %s at %q (%s)", result.Status, result.Stage, result.ReasonCode, test.wantStatus, test.wantStage, test.wantCode)
			}
			if result.Reason == "" {
				t.Error("result has no reason")
			}

			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("encode result: %v", err)
			}
			for _, key := range []string{keys.identity.String(), keys.other.String(), "PRIVATE KEY"} {
				if strings.Contains(string(encoded), key) {
					t.Error("result contains a private key")
				}
			}
			runner.assertNothingLeft(t)
			runner.assertNoWorkFiles(t)
		})
	}
}
