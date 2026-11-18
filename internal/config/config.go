// Package config loads and validates the runner's settings from its environment.
package config

import (
	"errors"
	"log/slog"
	"math"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Environment variables the runner reads. docs/config.md describes each one.
const (
	EnvToken       = "BACKPROOF_TOKEN"
	EnvRunnerKey   = "BACKPROOF_RUNNER_KEY"
	EnvAPIURL      = "BACKPROOF_API_URL"
	EnvName        = "BACKPROOF_NAME"
	EnvConcurrency = "BACKPROOF_CONCURRENCY"
	EnvWorkDir     = "BACKPROOF_WORK_DIR"
	EnvLogLevel    = "BACKPROOF_LOG_LEVEL"
	EnvMemoryLimit = "BACKPROOF_MEMORY_LIMIT"
	EnvCPULimit    = "BACKPROOF_CPU_LIMIT"
	EnvDiskMargin  = "BACKPROOF_DISK_MARGIN"
)

// Defaults for settings that are not set in the environment.
const (
	DefaultAPIURL      = "https://app.backproof.dev"
	DefaultConcurrency = 1
	DefaultWorkDir     = "/var/lib/backproof"
	DefaultLogLevel    = slog.LevelInfo
	DefaultDiskMargin  = 3.0
)

const (
	minMemoryLimit = 64 * 1024 * 1024
	minCPULimit    = 0.01
	maxCPULimit    = 4096
	minDiskMargin  = 1
	maxDiskMargin  = 1000
)

var (
	memoryLimitPattern = regexp.MustCompile(`^(\d+)([bkmgt]?)$`)
	memoryUnits        = map[string]int64{
		"":  1,
		"b": 1,
		"k": 1 << 10,
		"m": 1 << 20,
		"g": 1 << 30,
		"t": 1 << 40,
	}
)

// Config holds the validated runner settings.
type Config struct {
	Token       Secret
	RunnerKey   Secret
	APIURL      string
	Name        string
	Concurrency int
	WorkDir     string
	LogLevel    slog.Level
	// Zero means no limit.
	MemoryLimitBytes int64
	CPULimitNanos    int64
	// DiskMargin is how many times the backup's size must be free before downloading it.
	DiskMargin float64
}

// SourceSecrets holds the local decryption keys of one backup source.
type SourceSecrets struct {
	AgeIdentity   Secret
	GPGKey        Secret
	GPGPassphrase Secret
}

// EnvSourceAgeIdentity names the variable holding a source's age private key.
func EnvSourceAgeIdentity(sourceID string) string {
	return "BP_SRC_" + strings.ToUpper(sourceID) + "_AGE_IDENTITY"
}

// EnvSourceGPGKey names the variable holding a source's GPG private key.
func EnvSourceGPGKey(sourceID string) string {
	return "BP_SRC_" + strings.ToUpper(sourceID) + "_GPG_KEY"
}

// EnvSourceGPGPassphrase names the variable holding the passphrase of a source's GPG key.
func EnvSourceGPGPassphrase(sourceID string) string {
	return "BP_SRC_" + strings.ToUpper(sourceID) + "_GPG_PASSPHRASE"
}

// LoadSourceSecrets reads the local decryption keys of a backup source.
func LoadSourceSecrets(lookup LookupFunc, sourceID string) SourceSecrets {
	get := func(key string) Secret {
		value, _ := lookup(key)
		return NewSecret(strings.TrimSpace(value))
	}
	return SourceSecrets{
		AgeIdentity:   get(EnvSourceAgeIdentity(sourceID)),
		GPGKey:        get(EnvSourceGPGKey(sourceID)),
		GPGPassphrase: get(EnvSourceGPGPassphrase(sourceID)),
	}
}

// LookupFunc reads one environment variable. os.LookupEnv satisfies it.
type LookupFunc func(key string) (string, bool)

// Load reads the settings through lookup and reports every invalid setting at once.
// Error messages never contain the rejected values, because a value may be a misplaced secret.
func Load(lookup LookupFunc) (Config, error) {
	get := func(key string) string {
		value, _ := lookup(key)
		return strings.TrimSpace(value)
	}

	cfg := Config{
		Token:       NewSecret(get(EnvToken)),
		RunnerKey:   NewSecret(get(EnvRunnerKey)),
		APIURL:      DefaultAPIURL,
		Name:        get(EnvName),
		Concurrency: DefaultConcurrency,
		WorkDir:     DefaultWorkDir,
		LogLevel:    DefaultLogLevel,
		DiskMargin:  DefaultDiskMargin,
	}

	var errs []error

	if raw := get(EnvAPIURL); raw != "" {
		apiURL, err := parseAPIURL(raw)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.APIURL = apiURL
	}

	if raw := get(EnvConcurrency); raw != "" {
		concurrency, err := strconv.Atoi(raw)
		if err != nil || concurrency < 1 {
			errs = append(errs, errors.New(EnvConcurrency+" is not valid: set it to a whole number of 1 or more"))
		}
		cfg.Concurrency = concurrency
	}

	if raw := get(EnvWorkDir); raw != "" {
		if !filepath.IsAbs(raw) {
			errs = append(errs, errors.New(EnvWorkDir+" is not valid: set it to an absolute path, such as "+DefaultWorkDir))
		}
		cfg.WorkDir = raw
	}

	if raw := get(EnvLogLevel); raw != "" {
		level, err := parseLogLevel(raw)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.LogLevel = level
	}

	if raw := get(EnvMemoryLimit); raw != "" {
		limit, err := parseMemoryLimit(raw)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.MemoryLimitBytes = limit
	}

	if raw := get(EnvCPULimit); raw != "" {
		limit, err := parseCPULimit(raw)
		if err != nil {
			errs = append(errs, err)
		}
		cfg.CPULimitNanos = limit
	}

	if raw := get(EnvDiskMargin); raw != "" {
		margin, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(margin) || margin < minDiskMargin || margin > maxDiskMargin {
			errs = append(errs, errors.New(EnvDiskMargin+" is not valid: set it to a number from 1 to 1000, such as 3"))
		}
		cfg.DiskMargin = margin
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseAPIURL(raw string) (string, error) {
	invalid := errors.New(EnvAPIURL + " is not valid: set it to an https:// address without credentials, such as " + DefaultAPIURL)

	// The url.Parse error quotes its input, so it is dropped rather than wrapped.
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", invalid
	}
	if parsed.Hostname() == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", invalid
	}
	// Runner tokens must never cross a network in plain text, so http is for local development only.
	if parsed.Scheme != "https" && (parsed.Scheme != "http" || !isLoopback(parsed.Hostname())) {
		return "", invalid
	}
	return strings.TrimRight(parsed.String(), "/"), nil
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func parseMemoryLimit(raw string) (int64, error) {
	invalid := errors.New(EnvMemoryLimit + " is not valid: set it to an amount of 64m or more, such as 512m or 4g")

	match := memoryLimitPattern.FindStringSubmatch(strings.ToLower(raw))
	if match == nil {
		return 0, invalid
	}
	amount, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return 0, invalid
	}
	unit := memoryUnits[match[2]]
	if amount > math.MaxInt64/unit || amount*unit < minMemoryLimit {
		return 0, invalid
	}
	return amount * unit, nil
}

func parseCPULimit(raw string) (int64, error) {
	cpus, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(cpus) || cpus < minCPULimit || cpus > maxCPULimit {
		return 0, errors.New(EnvCPULimit + " is not valid: set it to a number of CPUs of 0.01 or more, such as 1.5")
	}
	return int64(math.Round(cpus * 1e9)), nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return DefaultLogLevel, errors.New(EnvLogLevel + " is not valid: set it to debug, info, warn or error")
	}
}
