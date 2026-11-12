package token

import (
	"strings"
	"testing"
)

func TestNewMakesALongRandomTokenAndAMatchingHash(t *testing.T) {
	a, b := New(), New()
	if a.Secret == b.Secret || a.Hash == b.Hash {
		t.Fatal("two tokens must differ")
	}
	if !strings.HasPrefix(a.Secret, Prefix) || len(a.Secret) != 56 || !Looks(a.Secret) {
		t.Fatalf("shape: %q", a.Secret)
	}
	if Hash(a.Secret) != a.Hash {
		t.Fatal("the stored hash must be the hash of the secret")
	}
	if strings.Contains(a.Hash, a.Secret[len(Prefix):]) || len(a.Hash) != 64 {
		t.Fatalf("the hash must not contain the secret: %q", a.Hash)
	}
	if a.Short != a.Secret[:10] || strings.Count(a.Short, "") > 11 {
		t.Fatalf("only the first characters are shown: %q", a.Short)
	}
}

func TestLooks(t *testing.T) {
	good := New().Secret
	for s, want := range map[string]bool{
		good: true, "": false, "bwl_": false, good + "x": false, good[:55]: false,
		"abc_" + good[4:]: false, "0123456789abcdef0123456789abcdef": false,
	} {
		if got := Looks(s); got != want {
			t.Errorf("Looks(%q) = %v, want %v", s, got, want)
		}
	}
}
