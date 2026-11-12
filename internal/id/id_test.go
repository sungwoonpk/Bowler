package id

import "testing"

func TestNewIsUniqueAndWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for range 5000 {
		v := New(Message)
		if !HasPrefix(v, Message) {
			t.Fatalf("%q is not a well formed message id", v)
		}
		if seen[v] {
			t.Fatalf("duplicate id %q", v)
		}
		seen[v] = true
	}
}

func TestHasPrefixRefusesOtherKindsAndJunk(t *testing.T) {
	good := New(Endpoint)
	for _, bad := range []string{"", "ep_", good + "x", good[:len(good)-1], "msg" + good[2:], "ep_" + "UPPERCASEUPPERCASEUPP", "ep_!!!!!!!!!!!!!!!!!!!!", "ep-" + good[3:]} {
		if HasPrefix(bad, Endpoint) {
			t.Errorf("%q should not count as an endpoint id", bad)
		}
	}
	if HasPrefix(good, Message) {
		t.Error("an endpoint id must not pass as a message id")
	}
}
