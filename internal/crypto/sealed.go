package crypto

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"filippo.io/age/armor"

	"github.com/backproofhq/runner/internal/config"
)

// ErrInvalidBundle replaces every reason a bundle can't be read, because parser messages can
// quote the decrypted contents.
var ErrInvalidBundle = errors.New("the sealed keys can't be read, enter them again in the dashboard")

const (
	sealedBundleVersion = 1
	// The maxLength of Job.sealed_bundle in api/v1/openapi.yaml.
	maxSealedBundleLength = 1 << 20
	minNameSaltLength     = 32
)

// SealedBundle holds the keys from SealedBundleContents in api/v1/openapi.yaml. It lives in
// memory for one job.
type SealedBundle struct {
	S3AccessKey   config.Secret
	S3SecretKey   config.Secret
	NameSalt      config.Secret
	AgeIdentity   config.Secret
	GPGKey        config.Secret
	GPGPassphrase config.Secret
}

// OpenSealedBundle decrypts a job's sealed bundle in memory with the runner's private key. A
// bundle sealed for other runners returns ErrWrongKey.
func OpenSealedBundle(armored string, identity config.Secret) (SealedBundle, error) {
	if len(armored) > maxSealedBundleLength || !strings.HasPrefix(armored, armor.Header) {
		return SealedBundle{}, ErrInvalidBundle
	}
	decrypted, err := OpenAge(strings.NewReader(armored), identity)
	if errors.Is(err, ErrNoKey) || errors.Is(err, ErrInvalidKey) || errors.Is(err, ErrWrongKey) {
		return SealedBundle{}, err
	}
	if err != nil {
		return SealedBundle{}, ErrInvalidBundle
	}

	// Decrypted contents are never longer than the armored file around them.
	plain, err := io.ReadAll(io.LimitReader(decrypted, maxSealedBundleLength))
	defer clear(plain)
	if err != nil {
		return SealedBundle{}, ErrInvalidBundle
	}
	return parseSealedBundle(plain)
}

func parseSealedBundle(plain []byte) (SealedBundle, error) {
	var contents struct {
		Version       *int   `json:"version"`
		S3AccessKey   string `json:"s3_access_key"`
		S3SecretKey   string `json:"s3_secret_key"`
		NameSalt      string `json:"name_salt"`
		AgeIdentity   string `json:"age_identity"`
		GPGKey        string `json:"gpg_key"`
		GPGPassphrase string `json:"gpg_passphrase"`
	}
	if err := json.Unmarshal(plain, &contents); err != nil {
		return SealedBundle{}, ErrInvalidBundle
	}
	if contents.Version == nil || *contents.Version != sealedBundleVersion ||
		contents.S3AccessKey == "" || contents.S3SecretKey == "" ||
		utf8.RuneCountInString(contents.NameSalt) < minNameSaltLength {
		return SealedBundle{}, ErrInvalidBundle
	}
	return SealedBundle{
		S3AccessKey:   config.NewSecret(contents.S3AccessKey),
		S3SecretKey:   config.NewSecret(contents.S3SecretKey),
		NameSalt:      config.NewSecret(contents.NameSalt),
		AgeIdentity:   config.NewSecret(contents.AgeIdentity),
		GPGKey:        config.NewSecret(contents.GPGKey),
		GPGPassphrase: config.NewSecret(contents.GPGPassphrase),
	}, nil
}
