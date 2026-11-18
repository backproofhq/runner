package job

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/klauspost/compress/zstd"

	"github.com/backproofhq/runner/internal/archive"
	"github.com/backproofhq/runner/internal/config"
)

const plainDump = "--\n-- PostgreSQL database dump\n--\n\nCREATE TABLE t (id int);\n\n--\n-- PostgreSQL database dump complete\n--\n"

type testKeys struct {
	ageIdentity  *age.X25519Identity
	gpgEntity    *openpgp.Entity
	gpgPrivate   string
	otherAge     string
	otherGPG     string
	lockedGPG    string
	lockedEntity *openpgp.Entity
}

func gpgKey(t *testing.T, passphrase string) (*openpgp.Entity, string) {
	t.Helper()
	entity, err := openpgp.NewEntity("Test", "", "test@example.invalid", nil)
	if err != nil {
		t.Fatalf("create gpg key: %v", err)
	}

	var exported bytes.Buffer
	armored, err := pgparmor.Encode(&exported, openpgp.PrivateKeyType, nil)
	if err != nil {
		t.Fatalf("armor gpg key: %v", err)
	}
	// Serializing first re-signs the identities, which needs the unlocked key.
	var signed bytes.Buffer
	if err := entity.SerializePrivate(&signed, nil); err != nil {
		t.Fatalf("sign gpg key: %v", err)
	}
	if passphrase != "" {
		if err := entity.EncryptPrivateKeys([]byte(passphrase), nil); err != nil {
			t.Fatalf("lock gpg key: %v", err)
		}
		if err := entity.SerializePrivateWithoutSigning(armored, nil); err != nil {
			t.Fatalf("export gpg key: %v", err)
		}
	} else if _, err := armored.Write(signed.Bytes()); err != nil {
		t.Fatalf("export gpg key: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("close gpg armor: %v", err)
	}
	return entity, exported.String()
}

func newTestKeys(t *testing.T) testKeys {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create age key: %v", err)
	}
	other, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create age key: %v", err)
	}
	entity, private := gpgKey(t, "")
	_, otherPrivate := gpgKey(t, "")
	locked, lockedPrivate := gpgKey(t, "test-passphrase")
	return testKeys{
		ageIdentity:  identity,
		gpgEntity:    entity,
		gpgPrivate:   private,
		otherAge:     other.String(),
		otherGPG:     otherPrivate,
		lockedGPG:    lockedPrivate,
		lockedEntity: locked,
	}
}

func gzipped(t *testing.T, data []byte) []byte {
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

func zstandard(t *testing.T, data []byte) []byte {
	t.Helper()
	writer, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatalf("zstd: %v", err)
	}
	defer func() { _ = writer.Close() }()
	return writer.EncodeAll(data, nil)
}

func ageEncrypted(t *testing.T, recipient age.Recipient, data []byte) []byte {
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

func ageArmored(t *testing.T, recipient age.Recipient, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	armored := armor.NewWriter(&output)
	if _, err := armored.Write(ageEncrypted(t, recipient, data)); err != nil {
		t.Fatalf("age armor: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("age armor: %v", err)
	}
	return output.Bytes()
}

func gpgEncrypted(t *testing.T, entity *openpgp.Entity, data []byte) []byte {
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

func gpgArmored(t *testing.T, entity *openpgp.Entity, data []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	armored, err := pgparmor.Encode(&output, "PGP MESSAGE", nil)
	if err != nil {
		t.Fatalf("gpg armor: %v", err)
	}
	if _, err := armored.Write(gpgEncrypted(t, entity, data)); err != nil {
		t.Fatalf("gpg armor: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("gpg armor: %v", err)
	}
	return output.Bytes()
}

func unpack(t *testing.T, data []byte, secrets config.SourceSecrets) (Dump, Backup, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.bin")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	var backup Backup
	dump, err := prepareDump(t.Context(), path, "job", Options{WorkDir: t.TempDir(), SourceID: "LOCAL", Secrets: secrets}, &backup)
	return dump, backup, err
}

func TestPrepareDumpOpensEveryLayerCombination(t *testing.T) {
	keys := newTestKeys(t)
	dump := []byte(plainDump)
	secrets := config.SourceSecrets{
		AgeIdentity: config.NewSecret(keys.ageIdentity.String()),
		GPGKey:      config.NewSecret(keys.gpgPrivate),
	}
	recipient := keys.ageIdentity.Recipient()

	tests := []struct {
		name            string
		data            []byte
		secrets         config.SourceSecrets
		wantCompression string
		wantEncryption  string
	}{
		{name: "gzip", data: gzipped(t, dump), wantCompression: "gzip"},
		{name: "zstd", data: zstandard(t, dump), wantCompression: "zstd"},
		{name: "age", data: ageEncrypted(t, recipient, dump), secrets: secrets, wantEncryption: "age"},
		{name: "armored age", data: ageArmored(t, recipient, dump), secrets: secrets, wantEncryption: "age"},
		{name: "gpg", data: gpgEncrypted(t, keys.gpgEntity, dump), secrets: secrets, wantEncryption: "pgp"},
		{name: "armored gpg", data: gpgArmored(t, keys.gpgEntity, dump), secrets: secrets, wantEncryption: "pgp"},
		{name: "gzip inside age", data: ageEncrypted(t, recipient, gzipped(t, dump)), secrets: secrets, wantCompression: "gzip", wantEncryption: "age"},
		{name: "zstd inside gpg", data: gpgEncrypted(t, keys.gpgEntity, zstandard(t, dump)), secrets: secrets, wantCompression: "zstd", wantEncryption: "pgp"},
		{name: "age inside gzip", data: gzipped(t, ageEncrypted(t, recipient, dump)), secrets: secrets, wantCompression: "gzip", wantEncryption: "age"},
		{
			name:           "gpg key with a passphrase",
			data:           gpgEncrypted(t, keys.lockedEntity, dump),
			secrets:        config.SourceSecrets{GPGKey: config.NewSecret(keys.lockedGPG), GPGPassphrase: config.NewSecret("test-passphrase")},
			wantEncryption: "pgp",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, backup, err := unpack(t, test.data, test.secrets)
			if err != nil {
				t.Fatalf("prepareDump() error = %v", err)
			}
			if backup.Compression != test.wantCompression || backup.Encryption != test.wantEncryption {
				t.Errorf("layers = %q and %q, want %q and %q", backup.Compression, backup.Encryption, test.wantCompression, test.wantEncryption)
			}
			if got.Format != archive.FormatPlain {
				t.Errorf("format = %s, want plain", got.Format)
			}
			contents, err := os.ReadFile(got.Path)
			if err != nil {
				t.Fatalf("read unpacked dump: %v", err)
			}
			if string(contents) != plainDump {
				t.Errorf("unpacked dump differs from the original")
			}
			info, err := os.Stat(got.Path)
			if err != nil {
				t.Fatalf("stat unpacked dump: %v", err)
			}
			if info.Mode().Perm() != 0o600 {
				t.Errorf("unpacked dump has mode %o, want 600", info.Mode().Perm())
			}
		})
	}
}

func TestPrepareDumpReportsWhatWentWrong(t *testing.T) {
	keys := newTestKeys(t)
	dump := []byte(plainDump)
	recipient := keys.ageIdentity.Recipient()
	right := config.SourceSecrets{AgeIdentity: config.NewSecret(keys.ageIdentity.String()), GPGKey: config.NewSecret(keys.gpgPrivate)}

	tests := []struct {
		name       string
		data       []byte
		secrets    config.SourceSecrets
		wantStatus Status
		wantCode   string
		wantStage  Stage
	}{
		{"wrong age key", ageEncrypted(t, recipient, dump), config.SourceSecrets{AgeIdentity: config.NewSecret(keys.otherAge)}, StatusFail, "wrong_key", StageDecrypt},
		{"wrong gpg key", gpgEncrypted(t, keys.gpgEntity, dump), config.SourceSecrets{GPGKey: config.NewSecret(keys.otherGPG)}, StatusFail, "wrong_key", StageDecrypt},
		{"no age key", ageEncrypted(t, recipient, dump), config.SourceSecrets{}, StatusError, "missing_key", StageDecrypt},
		{"no gpg key", gpgEncrypted(t, keys.gpgEntity, dump), config.SourceSecrets{}, StatusError, "missing_key", StageDecrypt},
		{"malformed age key", ageEncrypted(t, recipient, dump), config.SourceSecrets{AgeIdentity: config.NewSecret("not-a-key")}, StatusError, "invalid_key", StageDecrypt},
		{"malformed gpg key", gpgEncrypted(t, keys.gpgEntity, dump), config.SourceSecrets{GPGKey: config.NewSecret("not-a-key")}, StatusError, "invalid_key", StageDecrypt},
		{"wrong gpg passphrase", gpgEncrypted(t, keys.lockedEntity, dump), config.SourceSecrets{GPGKey: config.NewSecret(keys.lockedGPG), GPGPassphrase: config.NewSecret("wrong")}, StatusError, "invalid_key", StageDecrypt},
		{"missing gpg passphrase", gpgEncrypted(t, keys.lockedEntity, dump), config.SourceSecrets{GPGKey: config.NewSecret(keys.lockedGPG)}, StatusError, "invalid_key", StageDecrypt},
		{"gzip of nothing", gzipped(t, nil), config.SourceSecrets{}, StatusFail, "empty_backup", ""},
		{"zstd of nothing", zstandard(t, nil), config.SourceSecrets{}, StatusFail, "empty_backup", ""},
		{"encrypted nothing", ageEncrypted(t, recipient, nil), right, StatusFail, "empty_backup", ""},
		{"truncated dump inside gzip", gzipped(t, dump[:len(dump)/2]), config.SourceSecrets{}, StatusFail, "truncated_backup", ""},
		{"binary data inside gzip", gzipped(t, []byte{0x00, 0x01, 0x02}), config.SourceSecrets{}, StatusFail, "unknown_format", ""},
		{"endless gzip layers", gzipped(t, gzipped(t, gzipped(t, gzipped(t, gzipped(t, gzipped(t, dump)))))), config.SourceSecrets{}, StatusFail, "unknown_format", ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := unpack(t, test.data, test.secrets)
			if err == nil {
				t.Fatal("prepareDump() succeeded")
			}
			failure := asFailure(context.Background(), err)
			if failure.Status != test.wantStatus || failure.Code != test.wantCode || failure.Stage != test.wantStage {
				t.Errorf("failure = %s/%s at %q, want %s/%s at %q", failure.Status, failure.Code, failure.Stage, test.wantStatus, test.wantCode, test.wantStage)
			}
			if failure.Reason == "" {
				t.Error("failure has no reason")
			}
		})
	}
}

func TestPrepareDumpDetectsDamageAtEveryLength(t *testing.T) {
	keys := newTestKeys(t)
	dump := []byte(strings.Replace(plainDump, "CREATE TABLE t (id int);\n", strings.Repeat("INSERT INTO t VALUES (1);\n", 400), 1))
	recipient := keys.ageIdentity.Recipient()
	secrets := config.SourceSecrets{AgeIdentity: config.NewSecret(keys.ageIdentity.String()), GPGKey: config.NewSecret(keys.gpgPrivate)}

	tests := []struct {
		name string
		data []byte
	}{
		{"gzip", gzipped(t, dump)},
		{"zstd", zstandard(t, dump)},
		{"age", ageEncrypted(t, recipient, dump)},
		{"gpg", gpgEncrypted(t, keys.gpgEntity, dump)},
		{"gzip inside age", ageEncrypted(t, recipient, gzipped(t, dump))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Shorter files have lost the format's signature, so they are judged as plain SQL.
			for length := 32; length < len(test.data); length += max(1, len(test.data)/97) {
				assertBackupFails(t, test.data[:length], secrets, length)
			}
			for removed := 1; removed <= 32; removed++ {
				assertBackupFails(t, test.data[:len(test.data)-removed], secrets, len(test.data)-removed)
			}
		})
	}
}

func assertBackupFails(t *testing.T, data []byte, secrets config.SourceSecrets, length int) {
	t.Helper()
	_, _, err := unpack(t, data, secrets)
	if err == nil {
		t.Errorf("a backup cut to %d bytes was accepted", length)
		return
	}
	if failure := asFailure(context.Background(), err); failure.Status != StatusFail {
		t.Errorf("a backup cut to %d bytes = %s (%s), want a failed backup", length, failure.Status, failure.Code)
	}
}

func TestPrepareDumpDetectsFlippedBits(t *testing.T) {
	keys := newTestKeys(t)
	dump := []byte(strings.Replace(plainDump, "CREATE TABLE t (id int);\n", strings.Repeat("INSERT INTO t VALUES (1);\n", 400), 1))
	secrets := config.SourceSecrets{AgeIdentity: config.NewSecret(keys.ageIdentity.String()), GPGKey: config.NewSecret(keys.gpgPrivate)}

	for name, data := range map[string][]byte{
		"gzip": gzipped(t, dump),
		"age":  ageEncrypted(t, keys.ageIdentity.Recipient(), dump),
		"gpg":  gpgEncrypted(t, keys.gpgEntity, dump),
	} {
		t.Run(name, func(t *testing.T) {
			damaged := bytes.Clone(data)
			damaged[len(damaged)*3/4] ^= 0xff
			got, _, err := unpack(t, damaged, secrets)
			if err == nil {
				contents, _ := os.ReadFile(got.Path)
				t.Fatalf("damaged backup was accepted, identical to the original: %t", bytes.Equal(contents, dump))
			}
			if failure := asFailure(context.Background(), err); failure.Status != StatusFail {
				t.Errorf("failure = %s (%s), want a failed backup", failure.Status, failure.Code)
			}
		})
	}
}

func TestPrepareDumpErrorsNeverContainKeys(t *testing.T) {
	const canary = "AGE-SECRET-KEY-CANARY-VALUE"
	keys := newTestKeys(t)
	_, _, err := unpack(t, ageEncrypted(t, keys.ageIdentity.Recipient(), []byte(plainDump)), config.SourceSecrets{AgeIdentity: config.NewSecret(canary)})
	if err == nil {
		t.Fatal("prepareDump() accepted a malformed key")
	}
	failure := asFailure(context.Background(), err)
	for _, text := range []string{err.Error(), failure.Reason} {
		if strings.Contains(text, canary) {
			t.Errorf("error leaked the key: %q", text)
		}
	}
	if failure.cause != nil && strings.Contains(failure.cause.Error(), canary) {
		t.Errorf("logged cause leaked the key: %q", failure.cause)
	}
}

func TestUnwritableWorkDirIsARunnerError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "backup.gz")
	if err := os.WriteFile(path, gzipped(t, []byte(plainDump)), 0o600); err != nil {
		t.Fatalf("write backup: %v", err)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	var backup Backup
	_, err := prepareDump(t.Context(), path, "job", Options{WorkDir: blocked}, &backup)

	failure := asFailure(context.Background(), err)
	if failure.Status != StatusError || failure.Code != "work_dir_unavailable" {
		t.Errorf("failure = %s (%s), want a runner error", failure.Status, failure.Code)
	}
	if !strings.Contains(failure.Reason, config.EnvWorkDir) {
		t.Errorf("reason %q does not say what to change", failure.Reason)
	}
}

func TestPreflightRefusesABackupThatCannotFit(t *testing.T) {
	workDir := t.TempDir()

	if err := preflight(workDir, "fits", 1024, 3); err != nil {
		t.Errorf("preflight() error = %v, want a small backup to fit", err)
	}

	err := preflight(workDir, "too-big", 1<<62, 3)
	failure := asFailure(context.Background(), err)
	if failure.Status != StatusError || failure.Code != "disk_too_small" {
		t.Fatalf("failure = %s (%s), want a runner error", failure.Status, failure.Code)
	}
	if !strings.Contains(failure.Reason, config.EnvDiskMargin) {
		t.Errorf("reason %q does not say what to change", failure.Reason)
	}

	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	failure = asFailure(context.Background(), preflight(blocked, "job", 1024, 3))
	if failure.Status != StatusError || failure.Code != "work_dir_unavailable" {
		t.Errorf("failure = %s (%s), want an unavailable work directory", failure.Status, failure.Code)
	}
}

func TestFullDiskIsARunnerError(t *testing.T) {
	failure := asFailure(context.Background(), WorkFileFailure(fmt.Errorf("write backup: %w", syscall.ENOSPC)))
	if failure.Status != StatusError || failure.Code != "disk_full" {
		t.Errorf("failure = %s (%s), want a disk full runner error", failure.Status, failure.Code)
	}
}

func TestRemoveStaleWorkFilesKeepsTheRunningJob(t *testing.T) {
	workDir := t.TempDir()
	for _, job := range []string{"killed-run", "running"} {
		directory := jobDirectory(workDir, job)
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create job directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(directory, "backup"), []byte("decrypted"), 0o600); err != nil {
			t.Fatalf("write file: %v", err)
		}
	}

	removeStaleWorkFiles(t.Context(), slog.New(slog.DiscardHandler), workDir, "running")

	if _, err := os.Stat(jobDirectory(workDir, "killed-run")); !os.IsNotExist(err) {
		t.Errorf("the killed run's files are still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(jobDirectory(workDir, "running"), "backup")); err != nil {
		t.Errorf("the running job lost its files: %v", err)
	}
}
