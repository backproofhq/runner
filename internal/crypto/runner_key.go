package crypto

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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

// RunnerID derives a stable, public ID from the runner's key. It labels the runner's containers.
func RunnerID(privateKey config.Secret) (string, error) {
	publicKey, err := RunnerPublicKey(privateKey)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(publicKey))
	return hex.EncodeToString(digest[:8]), nil
}

// RunnerPublicKey returns the public half of the runner's key, which keys are sealed for.
func RunnerPublicKey(privateKey config.Secret) (string, error) {
	identity, err := age.ParseX25519Identity(privateKey.Reveal())
	if err != nil {
		return "", ErrInvalidKey
	}
	return identity.Recipient().String(), nil
}

// KeyFingerprint is what users compare with the dashboard to spot a swapped key. The dashboard
// calculates the same value: SHA-256 of the public key string in unpadded base64.
func KeyFingerprint(publicKey string) string {
	digest := sha256.Sum256([]byte(publicKey))
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:])
}
