//go:build sealinterop

package test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os/exec"
	"testing"

	"filippo.io/age"

	"github.com/backproofhq/runner/internal/config"
	"github.com/backproofhq/runner/internal/crypto"
)

type sealRequest struct {
	Secrets    map[string]string `json:"secrets"`
	Recipients []string          `json:"recipients"`
}

type sealAnswer struct {
	Sealed      string `json:"sealed"`
	Salt        string `json:"salt"`
	OwnSealed   string `json:"ownSealed"`
	Fingerprint string `json:"fingerprint"`
	Pair        struct {
		Identity  string `json:"identity"`
		Recipient string `json:"recipient"`
	} `json:"pair"`
}

// sealWithPackage runs the committed build of @backproof/seal, which is what the dashboard installs.
func sealWithPackage(t *testing.T, request sealRequest) sealAnswer {
	t.Helper()
	input, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	command := exec.CommandContext(t.Context(), "node", "../packages/seal/test/interop.mjs")
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		t.Fatalf("run @backproof/seal: %v: %s", err, stderr.String())
	}
	var answer sealAnswer
	if err := json.Unmarshal(output, &answer); err != nil {
		t.Fatalf("read answer: %v", err)
	}
	return answer
}

func newRunnerKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return identity
}

func TestRunnerOpensBundleSealedByThePackage(t *testing.T) {
	first, second, stranger := newRunnerKey(t), newRunnerKey(t), newRunnerKey(t)
	secrets := map[string]string{
		"s3_access_key":  "generated-access-key",
		"s3_secret_key":  "generated-secret-kéy-🔑",
		"name_salt":      "generated-name-salt-of-at-least-32-characters",
		"age_identity":   newRunnerKey(t).String(),
		"gpg_key":        "generated-gpg-key\nwith a second line\n",
		"gpg_passphrase": "generated passphrase",
	}

	answer := sealWithPackage(t, sealRequest{
		Secrets:    secrets,
		Recipients: []string{first.Recipient().String(), second.Recipient().String()},
	})

	for _, identity := range []*age.X25519Identity{first, second} {
		bundle, err := crypto.OpenSealedBundle(answer.Sealed, config.NewSecret(identity.String()))
		if err != nil {
			t.Fatalf("OpenSealedBundle() error = %v", err)
		}
		for name, got := range map[string]config.Secret{
			"s3_access_key":  bundle.S3AccessKey,
			"s3_secret_key":  bundle.S3SecretKey,
			"name_salt":      bundle.NameSalt,
			"age_identity":   bundle.AgeIdentity,
			"gpg_key":        bundle.GPGKey,
			"gpg_passphrase": bundle.GPGPassphrase,
		} {
			if got.Reveal() != secrets[name] {
				t.Errorf("%s doesn't match what was sealed", name)
			}
		}
	}

	_, err := crypto.OpenSealedBundle(answer.Sealed, config.NewSecret(stranger.String()))
	if !errors.Is(err, crypto.ErrWrongKey) {
		t.Errorf("another runner's key: error = %v, want ErrWrongKey", err)
	}
}

func TestRunnerUsesKeysAndSaltsFromThePackage(t *testing.T) {
	answer := sealWithPackage(t, sealRequest{
		Secrets:    map[string]string{"s3_access_key": "generated-access", "s3_secret_key": "generated-secret"},
		Recipients: []string{newRunnerKey(t).Recipient().String()},
	})

	identity, err := age.ParseX25519Identity(answer.Pair.Identity)
	if err != nil {
		t.Fatalf("parse the package's private key: %v", err)
	}
	if identity.Recipient().String() != answer.Pair.Recipient {
		t.Error("the package's public key doesn't belong to its private key")
	}
	if got := crypto.KeyFingerprint(answer.Pair.Recipient); got != answer.Fingerprint {
		t.Errorf("fingerprint = %q, the package says %q", got, answer.Fingerprint)
	}

	bundle, err := crypto.OpenSealedBundle(answer.OwnSealed, config.NewSecret(answer.Pair.Identity))
	if err != nil {
		t.Fatalf("OpenSealedBundle() error = %v", err)
	}
	if bundle.NameSalt.Reveal() != answer.Salt {
		t.Error("the name salt doesn't match what was sealed")
	}
}
