package config

import (
	"fmt"
	"io"
	"log/slog"
)

const redacted = "[redacted]"

// Secret holds a sensitive value and redacts it in logs, formatted output and JSON.
type Secret struct {
	value string
}

// NewSecret wraps a sensitive value.
func NewSecret(value string) Secret {
	return Secret{value: value}
}

// Reveal returns the plain value. Call it only where the value is used, never to log it.
func (s Secret) Reveal() string {
	return s.value
}

// IsSet reports whether the secret holds a value.
func (s Secret) IsSet() bool {
	return s.value != ""
}

// String returns a redacted placeholder.
func (s Secret) String() string {
	return redacted
}

// Format redacts every fmt verb, including %d and %x, which would otherwise bypass String.
func (s Secret) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, redacted)
}

// LogValue redacts the secret in slog output.
func (s Secret) LogValue() slog.Value {
	return slog.StringValue(redacted)
}

// MarshalText redacts the secret in JSON and other text encodings.
func (s Secret) MarshalText() ([]byte, error) {
	return []byte(redacted), nil
}
