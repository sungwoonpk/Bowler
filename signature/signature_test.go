package signature

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The published test vector from the Standard Webhooks specification, also reproduced independently with
// `openssl dgst -sha256 -hmac` using the decoded secret.
const (
	vectorSecret    = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	vectorID        = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	vectorTimestamp = 1614265330
	vectorBody      = `{"test": 2432232314}`
	vectorSignature = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func vectorTime() time.Time { return time.Unix(vectorTimestamp, 0) }

func TestSignMatchesTheSpecificationVector(t *testing.T) {
	got, err := Sign(vectorSecret, vectorID, vectorTime(), []byte(vectorBody))
	if err != nil {
		t.Fatal(err)
	}
	if got != vectorSignature {
		t.Fatalf("Sign = %q, want %q", got, vectorSignature)
	}
}

func TestSecretPrefixIsOptional(t *testing.T) {
	bare := strings.TrimPrefix(vectorSecret, SecretPrefix)
	got, err := Sign(bare, vectorID, vectorTime(), []byte(vectorBody))
	if err != nil || got != vectorSignature {
		t.Fatalf("Sign without prefix = %q, %v", got, err)
	}
}

func TestVerifyAcceptsTheVector(t *testing.T) {
	err := Verify([]string{vectorSecret}, vectorID, "1614265330", vectorSignature, []byte(vectorBody), vectorTime().Add(time.Minute), 0)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestRoundTripWithGeneratedSecret(t *testing.T) {
	secret, err := GenerateSecret()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(secret, SecretPrefix) {
		t.Fatalf("secret %q has no prefix", secret)
	}
	other, _ := GenerateSecret()
	if secret == other {
		t.Fatal("two generated secrets must differ")
	}
	now := time.Now()
	sig, err := Sign(secret, "msg_1", now, []byte("hello"))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]string{secret}, "msg_1", itoa(now.Unix()), sig, []byte("hello"), now, 0); err != nil {
		t.Fatalf("round trip: %v", err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestVerifyRejectsEveryKindOfTampering(t *testing.T) {
	at := vectorTime()
	now := at.Add(time.Second)
	ts := "1614265330"
	body := []byte(vectorBody)
	good := func() error { return Verify([]string{vectorSecret}, vectorID, ts, vectorSignature, body, now, 0) }
	if err := good(); err != nil {
		t.Fatalf("baseline must pass: %v", err)
	}

	cases := []struct {
		name string
		run  func() error
	}{
		{"changed body", func() error {
			return Verify([]string{vectorSecret}, vectorID, ts, vectorSignature, []byte(`{"test": 1}`), now, 0)
		}},
		{"changed id", func() error { return Verify([]string{vectorSecret}, "msg_other", ts, vectorSignature, body, now, 0) }},
		{"changed timestamp", func() error {
			return Verify([]string{vectorSecret}, vectorID, "1614265331", vectorSignature, body, now, 0)
		}},
		{"wrong secret", func() error {
			return Verify([]string{"whsec_" + "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}, vectorID, ts, vectorSignature, body, now, 0)
		}},
		{"truncated signature", func() error { return Verify([]string{vectorSecret}, vectorID, ts, vectorSignature[:20], body, now, 0) }},
		{"signature with unknown version", func() error {
			return Verify([]string{vectorSecret}, vectorID, ts, "v2,"+strings.TrimPrefix(vectorSignature, "v1,"), body, now, 0)
		}},
	}
	for _, c := range cases {
		if err := c.run(); !errors.Is(err, ErrNoMatch) {
			t.Errorf("%s: got %v, want ErrNoMatch", c.name, err)
		}
	}
}

func TestVerifyChecksTheClock(t *testing.T) {
	at := vectorTime()
	args := func(now time.Time, tol time.Duration) error {
		return Verify([]string{vectorSecret}, vectorID, "1614265330", vectorSignature, []byte(vectorBody), now, tol)
	}
	if err := args(at.Add(4*time.Minute), 0); err != nil {
		t.Errorf("inside the default tolerance: %v", err)
	}
	if err := args(at.Add(6*time.Minute), 0); !errors.Is(err, ErrTimestampTooOld) {
		t.Errorf("an old webhook: got %v, want ErrTimestampTooOld", err)
	}
	if err := args(at.Add(-6*time.Minute), 0); !errors.Is(err, ErrTimestampInFuture) {
		t.Errorf("a webhook from the future: got %v, want ErrTimestampInFuture", err)
	}
	if err := args(at.Add(30*time.Minute), time.Hour); err != nil {
		t.Errorf("a longer tolerance should be honored: %v", err)
	}
}

func TestVerifyRejectsBadInput(t *testing.T) {
	now := vectorTime()
	body := []byte(vectorBody)
	if err := Verify([]string{vectorSecret}, "", "1614265330", vectorSignature, body, now, 0); !errors.Is(err, ErrMissingHeaders) {
		t.Errorf("missing id: got %v", err)
	}
	if err := Verify([]string{vectorSecret}, vectorID, "", vectorSignature, body, now, 0); !errors.Is(err, ErrMissingHeaders) {
		t.Errorf("missing timestamp: got %v", err)
	}
	if err := Verify([]string{vectorSecret}, vectorID, "1614265330", "", body, now, 0); !errors.Is(err, ErrMissingHeaders) {
		t.Errorf("missing signature: got %v", err)
	}
	if err := Verify([]string{vectorSecret}, vectorID, "soon", vectorSignature, body, now, 0); !errors.Is(err, ErrBadTimestamp) {
		t.Errorf("bad timestamp: got %v", err)
	}
	if err := Verify([]string{"%%%not base64%%%"}, vectorID, "1614265330", vectorSignature, body, now, 0); !errors.Is(err, ErrBadSecret) {
		t.Errorf("bad secret: got %v", err)
	}
	if err := Verify([]string{vectorSecret}, vectorID, "1614265330", "garbage", body, now, 0); !errors.Is(err, ErrNoMatch) {
		t.Errorf("garbage signature: got %v", err)
	}
}

func TestRotationAcceptsEitherSecret(t *testing.T) {
	oldSecret, _ := GenerateSecret()
	newSecret, _ := GenerateSecret()
	now := time.Now()
	body := []byte(`{"a":1}`)

	header, err := SignAll([]string{newSecret, oldSecret}, "msg_9", now, body)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Fields(header)); got != 2 {
		t.Fatalf("expected two signatures, got %d in %q", got, header)
	}
	ts := itoa(now.Unix())
	// A receiver still on the old secret, one already on the new one, and one that has both all accept it.
	for name, secrets := range map[string][]string{"old only": {oldSecret}, "new only": {newSecret}, "both": {newSecret, oldSecret}} {
		if err := Verify(secrets, "msg_9", ts, header, body, now, 0); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	third, _ := GenerateSecret()
	if err := Verify([]string{third}, "msg_9", ts, header, body, now, 0); !errors.Is(err, ErrNoMatch) {
		t.Errorf("an unrelated secret must not verify, got %v", err)
	}
}

func TestSignRejectsBadSecrets(t *testing.T) {
	for _, bad := range []string{"", "whsec_", "whsec_***"} {
		if _, err := Sign(bad, "id", time.Now(), nil); !errors.Is(err, ErrBadSecret) {
			t.Errorf("Sign(%q): got %v, want ErrBadSecret", bad, err)
		}
	}
	if _, err := SignAll(nil, "id", time.Now(), nil); !errors.Is(err, ErrBadSecret) {
		t.Errorf("SignAll with no secrets: got %v", err)
	}
}

func TestEmptyBodyCanBeSigned(t *testing.T) {
	secret, _ := GenerateSecret()
	now := time.Now()
	sig, err := Sign(secret, "msg_e", now, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify([]string{secret}, "msg_e", itoa(now.Unix()), sig, []byte{}, now, 0); err != nil {
		t.Fatalf("empty body: %v", err)
	}
}
