// Package id makes the identifiers Bowler hands out, such as "msg_k3j9...".
package id

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// Prefixes tell a reader what an identifier refers to, and let the API refuse an id of the wrong kind early.
const (
	App      = "app"
	Endpoint = "ep"
	Message  = "msg"
	Delivery = "dlv"
	Attempt  = "att"
)

var enc = base32.StdEncoding.WithPadding(base32.NoPadding)

// New returns prefix + "_" + 20 random lowercase characters, which is 100 bits of randomness. Random rather than
// sequential, so an id reveals nothing about how many things exist and cannot be guessed.
func New(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The system source of randomness failing is not something a service can carry on through.
		panic("id: cannot read randomness: " + err.Error())
	}
	return prefix + "_" + strings.ToLower(enc.EncodeToString(b[:]))[:20]
}

// HasPrefix reports whether s looks like an id of the given kind.
func HasPrefix(s, prefix string) bool {
	if len(s) != len(prefix)+1+20 || !strings.HasPrefix(s, prefix+"_") {
		return false
	}
	for _, c := range s[len(prefix)+1:] {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}
