// Package secretbox encrypts the signing secrets Bowler keeps, so a copy of the database file does not hand out the
// power to forge webhooks.
//
// A secret is sealed with AES-256-GCM. The stored text is "enc:v1:" + the id of the key + ":" + base64 of the nonce
// followed by the ciphertext. The id is the start of a hash of the key, so it says which key to use and reveals nothing
// about it. A sealed value is tied to the row it belongs to through "associated data", so moving the ciphertext of one
// endpoint into another endpoint's row makes it fail to open instead of signing with the wrong secret.
//
// The key is not stored anywhere Bowler writes. It comes from outside, and the type is small enough that a key manager
// can be put behind it later.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of a key in bytes.
const KeySize = 32

const prefix = "enc:v1:"

// Errors a caller can test for with errors.Is.
var (
	// ErrNoKey means a value is sealed and no key was given.
	ErrNoKey = errors.New("secretbox: the value is encrypted and no encryption key is set")
	// ErrUnknownKey means a value is sealed under a key that was not given, such as one that was rotated out and removed.
	ErrUnknownKey = errors.New("secretbox: the value is encrypted under a key that is not available")
	// ErrCorrupt means a sealed value did not open: it was changed, moved to another row, or the key is wrong.
	ErrCorrupt = errors.New("secretbox: the encrypted value cannot be opened")
)

// Box seals and opens values. A nil *Box is valid and does not encrypt: Seal returns its input and Open refuses
// anything that is sealed.
type Box struct {
	currentID string
	keys      map[string]cipher.AEAD
}

// New makes a Box that seals with current and can also open what was sealed with any of previous, which lets a key be
// rotated: start with the new key as current and the old one as previous, and Bowler re-seals everything.
func New(current []byte, previous ...[]byte) (*Box, error) {
	b := &Box{keys: map[string]cipher.AEAD{}}
	for i, k := range append([][]byte{current}, previous...) {
		if len(k) != KeySize {
			return nil, fmt.Errorf("secretbox: a key must be %d bytes, got %d", KeySize, len(k))
		}
		block, err := aes.NewCipher(k)
		if err != nil {
			return nil, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		id := keyID(k)
		if _, dup := b.keys[id]; dup {
			return nil, errors.New("secretbox: the same key was given twice")
		}
		b.keys[id] = aead
		if i == 0 {
			b.currentID = id
		}
	}
	return b, nil
}

// ParseKey reads a key written as base64, as `openssl rand -base64 32` makes it.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil {
			if len(k) != KeySize {
				return nil, fmt.Errorf("the key must be %d bytes (for example from `openssl rand -base64 32`), this one is %d", KeySize, len(k))
			}
			return k, nil
		}
	}
	return nil, errors.New("the key must be base64, for example from `openssl rand -base64 32`")
}

func keyID(k []byte) string {
	sum := sha256.Sum256(append([]byte("bowler secretbox key id\x00"), k...))
	return hex.EncodeToString(sum[:4])
}

// Enabled reports whether new values are encrypted.
func (b *Box) Enabled() bool { return b != nil }

// Seal encrypts plain for the row named by aad. Without a key it returns plain unchanged.
func (b *Box) Seal(plain, aad string) (string, error) {
	if b == nil {
		return plain, nil
	}
	aead := b.keys[b.currentID]
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: cannot read randomness: %w", err)
	}
	sealed := aead.Seal(nonce, nonce, []byte(plain), []byte(aad))
	return prefix + b.currentID + ":" + base64.RawStdEncoding.EncodeToString(sealed), nil
}

// IsSealed reports whether a stored value is encrypted.
func IsSealed(stored string) bool { return strings.HasPrefix(stored, prefix) }

// Open returns the plain value of a stored one. A value that was never sealed is returned as it is, which is how a
// database from before encryption is read.
func (b *Box) Open(stored, aad string) (string, error) {
	if !IsSealed(stored) {
		return stored, nil
	}
	if b == nil {
		return "", ErrNoKey
	}
	id, body, ok := strings.Cut(strings.TrimPrefix(stored, prefix), ":")
	if !ok {
		return "", ErrCorrupt
	}
	aead, known := b.keys[id]
	if !known {
		return "", ErrUnknownKey
	}
	raw, err := base64.RawStdEncoding.DecodeString(body)
	if err != nil || len(raw) < aead.NonceSize() {
		return "", ErrCorrupt
	}
	plain, err := aead.Open(nil, raw[:aead.NonceSize()], raw[aead.NonceSize():], []byte(aad))
	if err != nil {
		return "", ErrCorrupt
	}
	return string(plain), nil
}

// NeedsSealing reports whether a stored value should be written again: it is plain and a key is set, or it is sealed
// under a key that is not the current one. It is false for an empty value and when no key is set.
func (b *Box) NeedsSealing(stored string) bool {
	if b == nil || stored == "" {
		return false
	}
	return !strings.HasPrefix(stored, prefix+b.currentID+":")
}
