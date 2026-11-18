package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"testing"
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
