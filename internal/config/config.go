// Package config loads and validates the runner's settings from its environment.
package config

import (
	"errors"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
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
)

// Defaults for settings that are not set in the environment.
const (
	DefaultAPIURL      = "https://app.backproof.dev"
	DefaultConcurrency = 1
	DefaultWorkDir     = "/var/lib/backproof"
	DefaultLogLevel    = slog.LevelInfo
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
