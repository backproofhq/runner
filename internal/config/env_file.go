package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// SourcesEnvFile holds local secrets for backup sources. It is read again before each job.
const SourcesEnvFile = "/etc/backproof/sources.env"

// WithEnvFile adds the values of an env file to lookup. Real environment variables win, and a
// missing file adds nothing. Lines are KEY=VALUE, taken literally like docker run --env-file.
func WithEnvFile(lookup LookupFunc, path string) (LookupFunc, error) {
	contents, err := os.ReadFile(path) //nolint:gosec // The path is one of the runner's own settings files.
	if errors.Is(err, os.ErrNotExist) {
		return lookup, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	values := make(map[string]string)
	for number, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) == "" {
			// The line itself stays out of the error, because it can hold a secret.
			return nil, fmt.Errorf("line %d of %s is not valid: write it as NAME=value", number+1, path)
		}
		values[strings.TrimSpace(key)] = value
	}

	return func(key string) (string, bool) {
		if value, ok := lookup(key); ok && value != "" {
			return value, true
		}
		value, ok := values[key]
		return value, ok
	}, nil
}
