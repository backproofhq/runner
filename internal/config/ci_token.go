package config

import (
	"encoding/base64"
	"errors"
	"strings"

	"filippo.io/age"
)

const (
	ciTokenPrefix      = "bp_ci_"
	runnerTokenPrefix  = "bp_run_"
	ciTokenSecretBytes = 32
)

var (
	errInvalidCIToken = errors.New(EnvToken + " is not a valid CI token: copy it again from the dashboard, or create a new one on the Add runner page")
	errCITokenWithKey = errors.New(EnvRunnerKey + " can't be used with a CI token, which already contains the runner's key: remove " + EnvRunnerKey)
)

// splitCIToken separates the credential sent to Backproof from the private key, which must never
// leave the runner. A CI token is bp_ci_<secret>.<age private key>, and Backproof only knows
// bp_run_<secret>.
func splitCIToken(token string) (runnerToken, runnerKey Secret, err error) {
	secret, key, found := strings.Cut(strings.TrimPrefix(token, ciTokenPrefix), ".")
	if !found {
		return Secret{}, Secret{}, errInvalidCIToken
	}
	// Encoding it again rejects line breaks, which the decoder skips but an HTTP header can't carry.
	decoded, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(decoded) != ciTokenSecretBytes || base64.RawURLEncoding.EncodeToString(decoded) != secret {
		return Secret{}, Secret{}, errInvalidCIToken
	}
	if _, err := age.ParseX25519Identity(key); err != nil {
		return Secret{}, Secret{}, errInvalidCIToken
	}
	return NewSecret(runnerTokenPrefix + secret), NewSecret(key), nil
}
