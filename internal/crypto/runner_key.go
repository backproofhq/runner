package crypto

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"filippo.io/age"

	"github.com/backproofhq/runner/internal/config"
)

// GenerateRunnerKey creates the key pair that keys are sealed for. The private key stays on the runner.
func GenerateRunnerKey() (config.Secret, string, error) {
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		return config.Secret{}, "", fmt.Errorf("generate runner key: %w", err)
	}
	return config.NewSecret(identity.String()), identity.Recipient().String(), nil
}

// KeyFingerprint is what users compare with the dashboard to spot a swapped key. The dashboard
// calculates the same value: SHA-256 of the public key string in unpadded base64.
func KeyFingerprint(publicKey string) string {
	digest := sha256.Sum256([]byte(publicKey))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:])
}
