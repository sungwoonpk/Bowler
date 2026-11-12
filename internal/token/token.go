// Package token makes the tokens an app uses to call the API, and the hashes that are kept in their place.
package token

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// Prefix starts every token, so a leaked one is easy to recognise and to search for, and so a token can be told
// from the operator's own key without asking the database.
const Prefix = "bwl_"

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// Token is a freshly made token. Secret is shown to the person once and never stored.
type Token struct {
	Secret string // what the caller sends: Prefix plus 52 characters, 256 bits of randomness
	Hash   string // what is stored
	Short  string // the first characters, safe to show
}

// New makes a token.
func New() Token {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("token: cannot read randomness: " + err.Error())
	}
	secret := Prefix + strings.ToLower(enc.EncodeToString(b[:]))
	return Token{Secret: secret, Hash: Hash(secret), Short: secret[:len(Prefix)+6]}
}

// Hash is what is stored for a token. The token has 256 bits of randomness, so there is nothing to guess and a slow
// hash would add nothing but cost on every request.
func Hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// Looks reports whether s has the shape of a token. It does not say whether it is a real one.
func Looks(s string) bool {
	return len(s) == len(Prefix)+52 && strings.HasPrefix(s, Prefix)
}
