package crypto

import (
	"testing"

	"filippo.io/age"
)

func TestKeyFingerprintMatchesTheDashboard(t *testing.T) {
	// The test vector from the web app's docs/phase-2/runner-registration.md.
	const want = "SHA256:4m4FJEkQ1+4bsddQHkzIqik856xUVFF2r27Q/fZPLno"
	if got := KeyFingerprint("age1test"); got != want {
		t.Errorf("KeyFingerprint() = %q, want %q", got, want)
	}
}

func TestGenerateRunnerKeyReturnsAMatchingPair(t *testing.T) {
	private, public, err := GenerateRunnerKey()
	if err != nil {
		t.Fatalf("GenerateRunnerKey() error = %v", err)
	}
	identity, err := age.ParseX25519Identity(private.Reveal())
	if err != nil {
		t.Fatalf("parse private key: %v", err)
	}
	if identity.Recipient().String() != public {
		t.Error("the public key doesn't belong to the private key")
	}
}
