package secretbox

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func key(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealedValuesOpenAndDoNotShowTheSecret(t *testing.T) {
	box, err := New(key(1))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	sealed, err := box.Seal(secret, "ep_a:secret")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) || strings.Contains(sealed, "MfKQ9r8G") || strings.Contains(sealed, "whsec") {
		t.Fatalf("the stored text must not contain the secret: %s", sealed)
	}
	got, err := box.Open(sealed, "ep_a:secret")
	if err != nil || got != secret {
		t.Fatalf("%q %v", got, err)
	}
}

func TestTheSameSecretSealsDifferentlyEachTime(t *testing.T) {
	box, _ := New(key(1))
	a, _ := box.Seal("whsec_x", "ep_a:secret")
	b, _ := box.Seal("whsec_x", "ep_a:secret")
	if a == b {
		t.Fatal("a fresh nonce each time, or two equal secrets would be visible as equal")
	}
}

func TestAValueMovedToAnotherRowDoesNotOpen(t *testing.T) {
	box, _ := New(key(1))
	sealed, _ := box.Seal("whsec_x", "ep_a:secret")
	for _, aad := range []string{"ep_b:secret", "ep_a:prev_secret", ""} {
		if _, err := box.Open(sealed, aad); !errors.Is(err, ErrCorrupt) {
			t.Errorf("aad %q: got %v, want ErrCorrupt", aad, err)
		}
	}
}

func TestATamperedValueDoesNotOpen(t *testing.T) {
	box, _ := New(key(1))
	sealed, _ := box.Seal("whsec_x", "ep_a:secret")
	i := len(sealed) - 3
	flipped := sealed[:i] + string(rune(sealed[i]^1)) + sealed[i+1:]
	for name, v := range map[string]string{
		"a changed character": flipped, "cut short": sealed[:len(sealed)-10], "no body": prefix + "abcd1234", "no key id": prefix,
		"not base64": prefix + box.currentID + ":!!!!",
	} {
		if _, err := box.Open(v, "ep_a:secret"); err == nil {
			t.Errorf("%s: a damaged value must not open", name)
		}
	}
}

func TestTheWrongKeyDoesNotOpenAndSaysSo(t *testing.T) {
	one, _ := New(key(1))
	two, _ := New(key(2))
	sealed, _ := one.Seal("whsec_x", "ep_a:secret")
	if _, err := two.Open(sealed, "ep_a:secret"); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("got %v, want ErrUnknownKey", err)
	}
	var none *Box
	if _, err := none.Open(sealed, "ep_a:secret"); !errors.Is(err, ErrNoKey) {
		t.Fatalf("got %v, want ErrNoKey", err)
	}
}

func TestAPreviousKeyStillOpensAndTheValueNeedsSealingAgain(t *testing.T) {
	old, _ := New(key(1))
	sealed, _ := old.Seal("whsec_x", "ep_a:secret")
	rotated, err := New(key(2), key(1))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := rotated.Open(sealed, "ep_a:secret"); err != nil || got != "whsec_x" {
		t.Fatalf("%q %v", got, err)
	}
	if !rotated.NeedsSealing(sealed) {
		t.Fatal("a value under an old key must be sealed again")
	}
	fresh, _ := rotated.Seal("whsec_x", "ep_a:secret")
	if rotated.NeedsSealing(fresh) {
		t.Fatal("a value under the current key is done")
	}
}

func TestWithoutAKeyNothingIsEncryptedAndPlainValuesPassThrough(t *testing.T) {
	var none *Box
	if none.Enabled() {
		t.Fatal("a nil box is off")
	}
	if got, err := none.Seal("whsec_x", "a"); err != nil || got != "whsec_x" {
		t.Fatalf("%q %v", got, err)
	}
	if got, err := none.Open("whsec_x", "a"); err != nil || got != "whsec_x" {
		t.Fatalf("%q %v", got, err)
	}
	if none.NeedsSealing("whsec_x") {
		t.Fatal("with no key there is nothing to do")
	}
	box, _ := New(key(1))
	if got, err := box.Open("whsec_legacy", "a"); err != nil || got != "whsec_legacy" {
		t.Fatalf("a database from before encryption must still read: %q %v", got, err)
	}
	if !box.NeedsSealing("whsec_legacy") || box.NeedsSealing("") {
		t.Fatal("a plain value needs sealing once a key is set, and an empty one does not")
	}
}

func TestKeysAreChecked(t *testing.T) {
	if _, err := New(make([]byte, 16)); err == nil {
		t.Error("a short key must be refused")
	}
	if _, err := New(key(1), key(1)); err == nil {
		t.Error("the same key twice must be refused")
	}
	if _, err := New(key(1), make([]byte, 5)); err == nil {
		t.Error("a short previous key must be refused")
	}
}

func TestParseKey(t *testing.T) {
	good := "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE=" // 32 bytes of 1
	if k, err := ParseKey(good); err != nil || !bytes.Equal(k, key(1)) {
		t.Fatalf("%v %v", k, err)
	}
	if _, err := ParseKey(" " + good + "\n"); err != nil {
		t.Errorf("surrounding whitespace is ignored: %v", err)
	}
	for name, s := range map[string]string{"empty": "", "not base64": "!!!", "too short": "AQEB", "too long": good + "AQEB"} {
		if _, err := ParseKey(s); err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
}
