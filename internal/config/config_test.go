package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
)

func lookupFrom(env map[string]string) LookupFunc {
	return func(key string) (string, bool) {
		value, ok := env[key]
		return value, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.APIURL != DefaultAPIURL {
		t.Errorf("APIURL = %q, want %q", cfg.APIURL, DefaultAPIURL)
	}
	if cfg.Concurrency != 1 {
		t.Errorf("Concurrency = %d, want 1", cfg.Concurrency)
	}
	if cfg.WorkDir != "/var/lib/backproof" {
		t.Errorf("WorkDir = %q, want /var/lib/backproof", cfg.WorkDir)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.Token.IsSet() || cfg.RunnerKey.IsSet() {
		t.Error("secrets are set without any environment")
	}
}

func TestLoadReadsEverySetting(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		EnvToken:       "test-token",
		EnvRunnerKey:   "test-runner-key",
		EnvAPIURL:      "https://api.example.test/",
		EnvName:        " build-box ",
		EnvConcurrency: "3",
		EnvWorkDir:     "/srv/backproof",
		EnvLogLevel:    "DEBUG",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Token.Reveal() != "test-token" || cfg.RunnerKey.Reveal() != "test-runner-key" {
		t.Error("secrets were not loaded")
	}
	if cfg.APIURL != "https://api.example.test" {
		t.Errorf("APIURL = %q, want the trailing slash removed", cfg.APIURL)
	}
	if cfg.Name != "build-box" {
		t.Errorf("Name = %q, want surrounding space removed", cfg.Name)
	}
	if cfg.Concurrency != 3 {
		t.Errorf("Concurrency = %d, want 3", cfg.Concurrency)
	}
	if cfg.WorkDir != "/srv/backproof" {
		t.Errorf("WorkDir = %q, want /srv/backproof", cfg.WorkDir)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug", cfg.LogLevel)
	}
}

func TestLoadTreatsEmptyValuesAsUnset(t *testing.T) {
	cfg, err := Load(lookupFrom(map[string]string{
		EnvAPIURL:      "",
		EnvConcurrency: "  ",
		EnvWorkDir:     "",
		EnvLogLevel:    "",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.APIURL != DefaultAPIURL || cfg.Concurrency != DefaultConcurrency || cfg.WorkDir != DefaultWorkDir {
		t.Errorf("Load() = %+v, want defaults", cfg)
	}
}

func TestLoadAPIURL(t *testing.T) {
	tests := []struct {
		name  string
		value string
		valid bool
	}{
		{"https", "https://app.backproof.dev", true},
		{"https with port", "https://backproof.example.test:8443", true},
		{"http on localhost", "http://localhost:3000", true},
		{"http on loopback ipv4", "http://127.0.0.1:3000", true},
		{"http on loopback ipv6", "http://[::1]:3000", true},
		{"http on a remote host", "http://app.backproof.dev", false},
		{"http on a lookalike host", "http://localhost.example.test", false},
		{"other scheme", "ftp://app.backproof.dev", false},
		{"no scheme", "app.backproof.dev", false},
		{"no host", "https://", false},
		{"credentials", "https://user:test-password@app.backproof.dev", false},
		{"query", "https://app.backproof.dev?token=test-token", false},
		{"fragment", "https://app.backproof.dev#test-token", false},
		{"unparsable", "https://exa mple.test", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvAPIURL: test.value}))
			if test.valid && err != nil {
				t.Errorf("Load() error = %v, want none", err)
			}
			if !test.valid && err == nil {
				t.Error("Load() accepted an invalid address")
			}
		})
	}
}

func TestLoadContainerLimits(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.MemoryLimitBytes != 0 || cfg.CPULimitNanos != 0 {
		t.Errorf("limits = %d bytes and %d nanos, want no limits by default", cfg.MemoryLimitBytes, cfg.CPULimitNanos)
	}

	memory := map[string]int64{
		"64m":       64 << 20,
		"512M":      512 << 20,
		"4g":        4 << 30,
		"1t":        1 << 40,
		"65536k":    64 << 20,
		"67108864":  64 << 20,
		"67108864b": 64 << 20,
	}
	for value, want := range memory {
		cfg, err := Load(lookupFrom(map[string]string{EnvMemoryLimit: value}))
		if err != nil {
			t.Errorf("memory limit %q: %v", value, err)
		}
		if cfg.MemoryLimitBytes != want {
			t.Errorf("memory limit %q = %d, want %d", value, cfg.MemoryLimitBytes, want)
		}
	}

	cpus := map[string]int64{"0.01": 10_000_000, "0.5": 500_000_000, "1.5": 1_500_000_000, "8": 8_000_000_000}
	for value, want := range cpus {
		cfg, err := Load(lookupFrom(map[string]string{EnvCPULimit: value}))
		if err != nil {
			t.Errorf("cpu limit %q: %v", value, err)
		}
		if cfg.CPULimitNanos != want {
			t.Errorf("cpu limit %q = %d, want %d", value, cfg.CPULimitNanos, want)
		}
	}
}

func TestLoadDiskMargin(t *testing.T) {
	cfg, err := Load(lookupFrom(nil))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DiskMargin != 3 {
		t.Errorf("DiskMargin = %g, want 3 by default", cfg.DiskMargin)
	}

	for value, want := range map[string]float64{"1": 1, "1.5": 1.5, "10": 10, "1000": 1000} {
		cfg, err := Load(lookupFrom(map[string]string{EnvDiskMargin: value}))
		if err != nil {
			t.Errorf("disk margin %q: %v", value, err)
		}
		if cfg.DiskMargin != want {
			t.Errorf("disk margin %q = %g, want %g", value, cfg.DiskMargin, want)
		}
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		key   string
		value string
	}{
		{EnvConcurrency, "0"},
		{EnvConcurrency, "-1"},
		{EnvConcurrency, "two"},
		{EnvConcurrency, "1.5"},
		{EnvConcurrency, "99999999999999999999"},
		{EnvWorkDir, "relative/path"},
		{EnvLogLevel, "verbose"},
		{EnvLogLevel, "info+2"},
		{EnvMemoryLimit, "63m"},
		{EnvMemoryLimit, "0"},
		{EnvMemoryLimit, "-1g"},
		{EnvMemoryLimit, "1.5g"},
		{EnvMemoryLimit, "4gb"},
		{EnvMemoryLimit, "lots"},
		{EnvMemoryLimit, "99999999999999999999t"},
		{EnvMemoryLimit, "9000000000t"},
		{EnvCPULimit, "0"},
		{EnvCPULimit, "0.001"},
		{EnvCPULimit, "-1"},
		{EnvCPULimit, "two"},
		{EnvCPULimit, "NaN"},
		{EnvCPULimit, "Inf"},
		{EnvDiskMargin, "0.5"},
		{EnvDiskMargin, "0"},
		{EnvDiskMargin, "-3"},
		{EnvDiskMargin, "1001"},
		{EnvDiskMargin, "three"},
		{EnvDiskMargin, "NaN"},
		{EnvDiskMargin, "Inf"},
	}
	for _, test := range tests {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{test.key: test.value}))
			if err == nil {
				t.Fatal("Load() accepted an invalid value")
			}
			if !strings.Contains(err.Error(), test.key) {
				t.Errorf("error %q does not name %s", err, test.key)
			}
		})
	}
}

func testCIToken(t *testing.T) (token, runnerToken, runnerKey string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("create runner key: %v", err)
	}
	secret := base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, 32))
	return "bp_ci_" + secret + "." + identity.String(), "bp_run_" + secret, identity.String()
}

func TestLoadSplitsACIToken(t *testing.T) {
	token, wantToken, wantKey := testCIToken(t)

	cfg, err := Load(lookupFrom(map[string]string{EnvToken: token + "\n"}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if got := cfg.Token.Reveal(); got != wantToken {
		t.Error("the token sent to Backproof is not the runner token inside the CI token")
	}
	if got := cfg.RunnerKey.Reveal(); got != wantKey {
		t.Error("the runner key is not the private key inside the CI token")
	}
}

func TestLoadRejectsBadCITokens(t *testing.T) {
	token, _, key := testCIToken(t)
	secret, _, _ := strings.Cut(strings.TrimPrefix(token, "bp_ci_"), ".")

	encoded := func(size int) string {
		return base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0x5a}, size))
	}
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	last := strings.IndexByte(alphabet, secret[len(secret)-1])

	tests := map[string]string{
		"no key":          "bp_ci_" + secret,
		"empty key":       "bp_ci_" + secret + ".",
		"not an age key":  "bp_ci_" + secret + ".AGE-SECRET-KEY-1CANARY",
		"short secret":    "bp_ci_" + encoded(31) + "." + key,
		"long secret":     "bp_ci_" + encoded(33) + "." + key,
		"invalid secret":  "bp_ci_" + secret[:42] + "!." + key,
		"altered secret":  "bp_ci_" + secret[:42] + string(alphabet[last+1]) + "." + key,
		"line in secret":  "bp_ci_" + secret[:20] + "\n" + secret[20:] + "." + key,
		"nothing after":   "bp_ci_",
		"two separators":  token + ".extra",
		"secret only dot": "bp_ci_." + key,
	}
	for name, value := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{EnvToken: value}))
			if err == nil {
				t.Fatal("Load() accepted an invalid CI token")
			}
			if !strings.Contains(err.Error(), EnvToken) {
				t.Errorf("error %q does not name %s", err, EnvToken)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), key) {
				t.Error("the error contains part of the rejected token")
			}
		})
	}
}

func TestLoadRefusesACITokenWithASeparateRunnerKey(t *testing.T) {
	token, _, _ := testCIToken(t)
	_, _, otherKey := testCIToken(t)

	_, err := Load(lookupFrom(map[string]string{EnvToken: token, EnvRunnerKey: otherKey}))
	if err == nil {
		t.Fatal("Load() accepted two runner keys")
	}
	if !strings.Contains(err.Error(), EnvRunnerKey) {
		t.Errorf("error %q does not name %s", err, EnvRunnerKey)
	}
	if strings.Contains(err.Error(), otherKey) {
		t.Error("the error contains the runner key")
	}
}

func TestLoadErrorsNeverContainRejectedValues(t *testing.T) {
	const canary = "test-canary-value"
	for _, key := range []string{EnvAPIURL, EnvConcurrency, EnvWorkDir, EnvLogLevel} {
		t.Run(key, func(t *testing.T) {
			_, err := Load(lookupFrom(map[string]string{key: canary}))
			if err == nil {
				t.Fatal("Load() accepted an invalid value")
			}
			if strings.Contains(err.Error(), canary) {
				t.Errorf("error %q contains the rejected value", err)
			}
		})
	}

	t.Run("unparsable address", func(t *testing.T) {
		_, err := Load(lookupFrom(map[string]string{EnvAPIURL: "https://" + canary + " host"}))
		if err == nil {
			t.Fatal("Load() accepted an invalid address")
		}
		if strings.Contains(err.Error(), canary) {
			t.Errorf("error %q contains the rejected value", err)
		}
	})
}

func TestLoadSourceSecretsUsesTheUpperCaseSourceID(t *testing.T) {
	got := LoadSourceSecrets(lookupFrom(map[string]string{
		"BP_SRC_PROD_DB_AGE_IDENTITY":   " test-age-key\n",
		"BP_SRC_PROD_DB_GPG_KEY":        "test-gpg-key",
		"BP_SRC_PROD_DB_GPG_PASSPHRASE": "test-passphrase",
		"BP_SRC_OTHER_AGE_IDENTITY":     "test-other-key",
	}), "prod_db")

	if got.AgeIdentity.Reveal() != "test-age-key" || got.GPGKey.Reveal() != "test-gpg-key" || got.GPGPassphrase.Reveal() != "test-passphrase" {
		t.Errorf("LoadSourceSecrets() did not read the keys of this source")
	}
}

func TestLoadSourceSecretsReadsEveryKey(t *testing.T) {
	got := LoadSourceSecrets(lookupFrom(map[string]string{
		"BP_SRC_PROD_DB_S3_ACCESS_KEY": "test-access",
		"BP_SRC_PROD_DB_S3_SECRET_KEY": "test-secret",
		"BP_SRC_PROD_DB_NAME_SALT":     "test-salt",
		"BP_SRC_PROD_DB_GPG_KEY":       `first-line\n\nlast-line`,
	}), "PROD_DB")

	if got.S3AccessKey.Reveal() != "test-access" || got.S3SecretKey.Reveal() != "test-secret" || got.NameSalt.Reveal() != "test-salt" {
		t.Error("LoadSourceSecrets() did not read the storage keys and salt")
	}
	if want := "first-line\n\nlast-line"; got.GPGKey.Reveal() != want {
		t.Error("LoadSourceSecrets() did not restore the GPG key's line breaks")
	}
}

func TestWithEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sources.env")
	contents := "# local secrets\n\nBP_SRC_A_S3_ACCESS_KEY=from-file\nBP_SRC_A_S3_SECRET_KEY=with=equals\nBP_SRC_A_NAME_SALT=from-file\r\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	lookup, err := WithEnvFile(lookupFrom(map[string]string{"BP_SRC_A_NAME_SALT": "from-environment"}), path)
	if err != nil {
		t.Fatalf("WithEnvFile() error = %v", err)
	}

	for key, want := range map[string]string{
		"BP_SRC_A_S3_ACCESS_KEY": "from-file",
		"BP_SRC_A_S3_SECRET_KEY": "with=equals",
		"BP_SRC_A_NAME_SALT":     "from-environment",
	} {
		if got, _ := lookup(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestWithEnvFileAllowsAMissingFile(t *testing.T) {
	lookup, err := WithEnvFile(lookupFrom(map[string]string{"KEY": "value"}), filepath.Join(t.TempDir(), "missing.env"))
	if err != nil {
		t.Fatalf("WithEnvFile() error = %v", err)
	}
	if got, _ := lookup("KEY"); got != "value" {
		t.Errorf("KEY = %q, want the environment's value", got)
	}
}

func TestWithEnvFileErrorsNeverContainValues(t *testing.T) {
	const canary = "test-secret-canary"
	path := filepath.Join(t.TempDir(), "sources.env")
	if err := os.WriteFile(path, []byte(canary+"\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	_, err := WithEnvFile(lookupFrom(nil), path)

	if err == nil {
		t.Fatal("WithEnvFile() accepted a line without a key")
	}
	if strings.Contains(err.Error(), canary) {
		t.Errorf("error %q contains the line's value", err)
	}
}

func TestSecretIsRedactedEverywhere(t *testing.T) {
	const value = "test-secret-value"
	secret := NewSecret(value)
	cfg := Config{Token: secret, RunnerKey: secret}

	for _, format := range []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%X"} {
		for _, operand := range []any{secret, &secret, cfg, &cfg} {
			if got := fmt.Sprintf(format, operand); strings.Contains(got, value) || strings.Contains(got, fmt.Sprintf("%x", value)) {
				t.Errorf("Sprintf(%q, %T) leaked the secret: %s", format, operand, got)
			}
		}
	}

	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), value) {
		t.Errorf("JSON leaked the secret: %s", encoded)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	logger.InfoContext(t.Context(), "loaded", slog.Any("token", secret), slog.Any("config", cfg))
	if strings.Contains(logs.String(), value) {
		t.Errorf("log leaked the secret: %s", logs.String())
	}

	if secret.Reveal() != value {
		t.Errorf("Reveal() = %q, want the plain value", secret.Reveal())
	}
}
