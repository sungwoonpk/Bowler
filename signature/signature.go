// Package signature signs and verifies webhooks as described by the Standard Webhooks specification
// (https://www.standardwebhooks.com).
//
// Bowler uses Sign to sign what it sends. A service that receives webhooks from Bowler, or from any other sender
// that follows the same specification, can import this package to check them. It has no dependencies outside the
// standard library.
//
// A signed webhook carries three headers:
//
//	webhook-id         the message id, the same on every retry, so the receiver can drop duplicates
//	webhook-timestamp  seconds since the Unix epoch, when this attempt was made
//	webhook-signature  one or more space separated signatures, each written as "v1,<base64 HMAC-SHA256>"
//
// The signature covers "<id>.<timestamp>.<body>", so changing the id, the time or the body breaks it.
package signature

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Header names defined by the specification.
const (
	HeaderID        = "webhook-id"
	HeaderTimestamp = "webhook-timestamp"
	HeaderSignature = "webhook-signature"
)

// SecretPrefix starts every signing secret that Bowler generates.
const SecretPrefix = "whsec_"

// DefaultTolerance is how far a timestamp may be from the receiver's clock. It limits how long a captured webhook
// can be replayed.
const DefaultTolerance = 5 * time.Minute

const (
	secretBytes = 24
	version     = "v1"
)

// Errors returned by Verify. They can be compared with errors.Is.
var (
	ErrMissingHeaders    = errors.New("signature: missing webhook headers")
	ErrBadTimestamp      = errors.New("signature: timestamp is not a number")
	ErrTimestampTooOld   = errors.New("signature: timestamp is too old")
	ErrTimestampInFuture = errors.New("signature: timestamp is too far in the future")
	ErrNoMatch           = errors.New("signature: no signature matches")
	ErrBadSecret         = errors.New("signature: secret is not valid")
)

// GenerateSecret returns a new random signing secret, "whsec_" followed by 24 random bytes in base64.
func GenerateSecret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("signature: read randomness: %w", err)
	}
	return SecretPrefix + base64.StdEncoding.EncodeToString(b), nil
}

// key turns a secret in text form into the bytes used for the HMAC. The "whsec_" prefix is optional.
func key(secret string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(secret, SecretPrefix))
	if err != nil || len(raw) == 0 {
		return nil, ErrBadSecret
	}
	return raw, nil
}

func mac(k []byte, id string, timestamp int64, body []byte) []byte {
	h := hmac.New(sha256.New, k)
	h.Write([]byte(id))
	h.Write([]byte("."))
	h.Write([]byte(strconv.FormatInt(timestamp, 10)))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}

// Sign returns one signature in the form "v1,<base64>".
func Sign(secret, id string, timestamp time.Time, body []byte) (string, error) {
	k, err := key(secret)
	if err != nil {
		return "", err
	}
	sum := mac(k, id, timestamp.Unix(), body)
	return version + "," + base64.StdEncoding.EncodeToString(sum), nil
}

// SignAll signs with every secret and returns the signatures joined by spaces, ready to use as the
// webhook-signature header. Sending two signatures while a secret is being rotated lets receivers move from the
// old secret to the new one without dropping a message.
func SignAll(secrets []string, id string, timestamp time.Time, body []byte) (string, error) {
	if len(secrets) == 0 {
		return "", ErrBadSecret
	}
	parts := make([]string, 0, len(secrets))
	for _, s := range secrets {
		sig, err := Sign(s, id, timestamp, body)
		if err != nil {
			return "", err
		}
		parts = append(parts, sig)
	}
	return strings.Join(parts, " "), nil
}

// Verify checks a received webhook against one or more secrets. Pass every secret that is currently valid, for
// example the new and the old one during a rotation. It returns nil when any signature matches.
//
// idHeader, timestampHeader and signatureHeader are the values of the three webhook headers, and body is the raw
// request body exactly as received, before any parsing. now is the receiver's clock. tolerance is how far the
// timestamp may be from now, or zero for DefaultTolerance.
func Verify(secrets []string, idHeader, timestampHeader, signatureHeader string, body []byte, now time.Time, tolerance time.Duration) error {
	if idHeader == "" || timestampHeader == "" || signatureHeader == "" {
		return ErrMissingHeaders
	}
	if tolerance <= 0 {
		tolerance = DefaultTolerance
	}
	seconds, err := strconv.ParseInt(timestampHeader, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	sent := time.Unix(seconds, 0)
	if now.Sub(sent) > tolerance {
		return ErrTimestampTooOld
	}
	if sent.Sub(now) > tolerance {
		return ErrTimestampInFuture
	}

	provided := parseSignatures(signatureHeader)
	if len(provided) == 0 {
		return ErrNoMatch
	}
	for _, secret := range secrets {
		k, err := key(secret)
		if err != nil {
			return err
		}
		want := mac(k, idHeader, seconds, body)
		for _, got := range provided {
			// hmac.Equal compares in constant time, so a wrong guess does not reveal how much of it was right.
			if hmac.Equal(want, got) {
				return nil
			}
		}
	}
	return ErrNoMatch
}

// parseSignatures reads a webhook-signature header and returns the decoded v1 signatures. Entries with another
// version, or that are not valid base64, are skipped, so a sender can add a new version without breaking old
// receivers.
func parseSignatures(header string) [][]byte {
	var out [][]byte
	for _, entry := range strings.Fields(header) {
		v, encoded, ok := strings.Cut(entry, ",")
		if !ok || v != version {
			continue
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			continue
		}
		out = append(out, raw)
	}
	return out
}
