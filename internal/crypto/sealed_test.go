package crypto

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/backproofhq/runner/internal/config"
)

const testNameSalt = "test-salt-that-is-at-least-32-characters"

func validContents() map[string]any {
	return map[string]any{
		"version":       1,
		"s3_access_key": "test-access-key",
		"s3_secret_key": "test-secret-key",
		"name_salt":     testNameSalt,
	}
}

func newTestKey(t *testing.T) *age.X25519Identity {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return identity
}

func sealText(t *testing.T, plain []byte, recipients ...age.Recipient) string {
	t.Helper()
	var sealed bytes.Buffer
	armored := armor.NewWriter(&sealed)
	encrypted, err := age.Encrypt(armored, recipients...)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if _, err := encrypted.Write(plain); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := encrypted.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := armored.Close(); err != nil {
		t.Fatalf("close armor: %v", err)
	}
	return sealed.String()
}

func sealContents(t *testing.T, contents map[string]any, recipients ...age.Recipient) string {
	t.Helper()
	plain, err := json.Marshal(contents)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return sealText(t, plain, recipients...)
}

func secretOf(identity *age.X25519Identity) config.Secret {
	return config.NewSecret(identity.String())
}

func TestOpenSealedBundleReturnsEveryKey(t *testing.T) {
	identity := newTestKey(t)
	contents := validContents()
	contents["age_identity"] = "test-age-identity"
	contents["gpg_key"] = "test-gpg-key"
	contents["gpg_passphrase"] = "test-gpg-passphrase"

	bundle, err := OpenSealedBundle(sealContents(t, contents, identity.Recipient()), secretOf(identity))
	if err != nil {
		t.Fatalf("OpenSealedBundle() error = %v", err)
	}

	for name, got := range map[string][2]string{
		"s3_access_key":  {bundle.S3AccessKey.Reveal(), "test-access-key"},
		"s3_secret_key":  {bundle.S3SecretKey.Reveal(), "test-secret-key"},
		"name_salt":      {bundle.NameSalt.Reveal(), testNameSalt},
		"age_identity":   {bundle.AgeIdentity.Reveal(), "test-age-identity"},
		"gpg_key":        {bundle.GPGKey.Reveal(), "test-gpg-key"},
		"gpg_passphrase": {bundle.GPGPassphrase.Reveal(), "test-gpg-passphrase"},
	} {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", name, got[0], got[1])
		}
	}
}

func TestOpenSealedBundleLeavesOptionalKeysUnset(t *testing.T) {
	identity := newTestKey(t)

	bundle, err := OpenSealedBundle(sealContents(t, validContents(), identity.Recipient()), secretOf(identity))
	if err != nil {
		t.Fatalf("OpenSealedBundle() error = %v", err)
	}
	if bundle.AgeIdentity.IsSet() || bundle.GPGKey.IsSet() || bundle.GPGPassphrase.IsSet() {
		t.Error("an optional key is set although the bundle doesn't hold it")
	}
}

func TestOpenSealedBundleIgnoresFieldsAddedLater(t *testing.T) {
	identity := newTestKey(t)
	contents := validContents()
	contents["added_later"] = "value"

	if _, err := OpenSealedBundle(sealContents(t, contents, identity.Recipient()), secretOf(identity)); err != nil {
		t.Fatalf("OpenSealedBundle() error = %v", err)
	}
}

func TestOpenSealedBundleOpensForEachRecipient(t *testing.T) {
	first, second := newTestKey(t), newTestKey(t)
	sealed := sealContents(t, validContents(), first.Recipient(), second.Recipient())

	for _, identity := range []*age.X25519Identity{first, second} {
		if _, err := OpenSealedBundle(sealed, secretOf(identity)); err != nil {
			t.Errorf("OpenSealedBundle() error = %v", err)
		}
	}
}

func TestOpenSealedBundleRefusesAnotherRunnersBundle(t *testing.T) {
	sealed := sealContents(t, validContents(), newTestKey(t).Recipient())

	_, err := OpenSealedBundle(sealed, secretOf(newTestKey(t)))
	if !errors.Is(err, ErrWrongKey) {
		t.Errorf("OpenSealedBundle() error = %v, want ErrWrongKey", err)
	}
}

func TestOpenSealedBundleNeedsAUsableKey(t *testing.T) {
	sealed := sealContents(t, validContents(), newTestKey(t).Recipient())

	if _, err := OpenSealedBundle(sealed, config.Secret{}); !errors.Is(err, ErrNoKey) {
		t.Errorf("without a key: error = %v, want ErrNoKey", err)
	}
	if _, err := OpenSealedBundle(sealed, config.NewSecret("not-a-key")); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("with a bad key: error = %v, want ErrInvalidKey", err)
	}
}

func TestOpenSealedBundleRefusesInvalidContents(t *testing.T) {
	change := func(key string, value any) map[string]any {
		contents := validContents()
		if value == nil {
			delete(contents, key)
		} else {
			contents[key] = value
		}
		return contents
	}
	tests := map[string]map[string]any{
		"no version":        change("version", nil),
		"unknown version":   change("version", 2),
		"text version":      change("version", "1"),
		"no access key":     change("s3_access_key", nil),
		"empty access key":  change("s3_access_key", ""),
		"no secret key":     change("s3_secret_key", nil),
		"empty secret key":  change("s3_secret_key", ""),
		"no name salt":      change("name_salt", nil),
		"short name salt":   change("name_salt", strings.Repeat("x", 31)),
		"non-text key":      change("s3_secret_key", 42),
		"non-text optional": change("gpg_key", true),
	}
	identity := newTestKey(t)
	for name, contents := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := OpenSealedBundle(sealContents(t, contents, identity.Recipient()), secretOf(identity))
			if !errors.Is(err, ErrInvalidBundle) {
				t.Errorf("OpenSealedBundle() error = %v, want ErrInvalidBundle", err)
			}
		})
	}
}

func TestOpenSealedBundleCountsSaltCharacters(t *testing.T) {
	identity := newTestKey(t)
	contents := validContents()
	contents["name_salt"] = strings.Repeat("é", minNameSaltLength)

	if _, err := OpenSealedBundle(sealContents(t, contents, identity.Recipient()), secretOf(identity)); err != nil {
		t.Errorf("OpenSealedBundle() error = %v", err)
	}
}

func TestOpenSealedBundleRefusesDamagedFiles(t *testing.T) {
	identity := newTestKey(t)
	sealed := sealContents(t, validContents(), identity.Recipient())
	lines := strings.Split(sealed, "\n")
	lines[1] = strings.Repeat("A", len(lines[1]))

	tests := map[string]string{
		"empty":          "",
		"not armored":    "plain text",
		"bad armor":      armor.Header + "\nnot base64!\n" + armor.Footer + "\n",
		"changed":        strings.Join(lines, "\n"),
		"cut off":        sealed[:len(sealed)/2],
		"over the limit": sealed + strings.Repeat("x", maxSealedBundleLength),
	}
	for name, bundle := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := OpenSealedBundle(bundle, secretOf(identity))
			if !errors.Is(err, ErrInvalidBundle) {
				t.Errorf("OpenSealedBundle() error = %v, want ErrInvalidBundle", err)
			}
		})
	}
}

func TestOpenSealedBundleKeepsContentsOutOfErrors(t *testing.T) {
	const secret = "test-secret-that-must-stay-out"
	identity := newTestKey(t)
	bundles := [][]byte{
		[]byte(secret),
		[]byte(`{"version":"` + secret + `"}`),
		[]byte(`{"version":1,"s3_access_key":"` + secret + `",`),
		[]byte(`{"version":2,"s3_secret_key":"` + secret + `"}`),
	}
	for _, plain := range bundles {
		_, err := OpenSealedBundle(sealText(t, plain, identity.Recipient()), secretOf(identity))
		if err == nil {
			t.Fatal("OpenSealedBundle() succeeded on invalid contents")
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", err, err, err), secret[:12]) {
			t.Error("the error holds the bundle's contents")
		}
	}
}

func TestSealedBundleRedactsItsKeys(t *testing.T) {
	identity := newTestKey(t)
	bundle, err := OpenSealedBundle(sealContents(t, validContents(), identity.Recipient()), secretOf(identity))
	if err != nil {
		t.Fatalf("OpenSealedBundle() error = %v", err)
	}
	if printed := fmt.Sprintf("%v %+v", bundle, bundle); strings.Contains(printed, "test-secret-key") {
		t.Error("printing the bundle shows a key")
	}
}
