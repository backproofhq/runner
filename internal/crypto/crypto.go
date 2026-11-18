// Package crypto opens backups encrypted with age or GPG.
package crypto

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"
	"github.com/ProtonMail/go-crypto/openpgp"
	pgparmor "github.com/ProtonMail/go-crypto/openpgp/armor"
	pgperrors "github.com/ProtonMail/go-crypto/openpgp/errors"

	"github.com/backproofhq/runner/internal/config"
)

// Reasons a backup can't be opened. Key parsing errors are replaced by these because library
// messages can quote the key they were given.
var (
	ErrNoKey      = errors.New("no decryption key is set")
	ErrInvalidKey = errors.New("the decryption key can't be used")
	ErrWrongKey   = errors.New("the decryption key doesn't match this backup")
)

const pgpMessageHeader = "-----BEGIN PGP MESSAGE-----"

// OpenAge returns the decrypted contents of an age file. Damage is reported while reading.
func OpenAge(encrypted io.Reader, identity config.Secret) (io.Reader, error) {
	if !identity.IsSet() {
		return nil, ErrNoKey
	}
	identities, err := age.ParseIdentities(strings.NewReader(identity.Reveal()))
	if err != nil {
		return nil, ErrInvalidKey
	}

	buffered := bufio.NewReader(encrypted)
	source := io.Reader(buffered)
	if header, err := buffered.Peek(len(armor.Header)); err == nil && string(header) == armor.Header {
		source = armor.NewReader(buffered)
	}

	decrypted, err := age.Decrypt(source, identities...)
	var noMatch *age.NoIdentityMatchError
	if errors.As(err, &noMatch) {
		return nil, ErrWrongKey
	}
	if err != nil {
		return nil, fmt.Errorf("read age header: %w", err)
	}
	return decrypted, nil
}

// OpenGPG returns the decrypted contents of a GPG message. Damage is reported while reading.
func OpenGPG(encrypted io.Reader, key, passphrase config.Secret) (io.Reader, error) {
	if !key.IsSet() {
		return nil, ErrNoKey
	}
	keyring, err := readKeyring(key, passphrase)
	if err != nil {
		return nil, err
	}

	buffered := bufio.NewReader(encrypted)
	source := io.Reader(buffered)
	if header, err := buffered.Peek(len(pgpMessageHeader)); err == nil && string(header) == pgpMessageHeader {
		block, err := pgparmor.Decode(buffered)
		if err != nil {
			return nil, fmt.Errorf("read gpg armor: %w", err)
		}
		source = block.Body
	}

	message, err := openpgp.ReadMessage(source, keyring, nil, nil)
	if errors.Is(err, pgperrors.ErrKeyIncorrect) {
		return nil, ErrWrongKey
	}
	if err != nil {
		return nil, fmt.Errorf("read gpg message: %w", err)
	}
	return &verifiedMessage{message: message}, nil
}

func readKeyring(key, passphrase config.Secret) (openpgp.EntityList, error) {
	material := []byte(key.Reveal())

	read := openpgp.ReadKeyRing
	if bytes.Contains(material, []byte("-----BEGIN PGP")) {
		read = openpgp.ReadArmoredKeyRing
	}
	keyring, err := read(bytes.NewReader(material))
	if err != nil || len(keyring) == 0 {
		return nil, ErrInvalidKey
	}

	for _, entity := range keyring {
		if !hasEncryptedKey(entity) {
			continue
		}
		if !passphrase.IsSet() || entity.DecryptPrivateKeys([]byte(passphrase.Reveal())) != nil {
			return nil, ErrInvalidKey
		}
	}
	return keyring, nil
}

func hasEncryptedKey(entity *openpgp.Entity) bool {
	if entity.PrivateKey != nil && entity.PrivateKey.Encrypted {
		return true
	}
	for _, subkey := range entity.Subkeys {
		if subkey.PrivateKey != nil && subkey.PrivateKey.Encrypted {
			return true
		}
	}
	return false
}

// verifiedMessage fails at the end of the message when a signature from a known key doesn't match.
type verifiedMessage struct {
	message *openpgp.MessageDetails
}

func (m *verifiedMessage) Read(p []byte) (int, error) {
	n, err := m.message.UnverifiedBody.Read(p)
	if errors.Is(err, io.EOF) && m.message.SignedBy != nil && m.message.SignatureError != nil {
		return n, fmt.Errorf("verify gpg signature: %w", m.message.SignatureError)
	}
	return n, err
}
