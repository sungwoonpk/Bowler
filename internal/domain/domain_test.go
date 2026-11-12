package domain

import (
	"encoding/json"
	"testing"
	"time"
)

func TestEndpointWants(t *testing.T) {
	everything := Endpoint{}
	if !everything.Wants("anything.at.all") {
		t.Error("an endpoint with no filter wants every event")
	}
	none := Endpoint{EventTypes: []string{}}
	if none.Wants("x") {
		t.Error("an empty filter is not the same as no filter: it wants nothing")
	}
	some := Endpoint{EventTypes: []string{"invoice.paid", "invoice.refunded"}}
	for typ, want := range map[string]bool{"invoice.paid": true, "invoice.refunded": true, "invoice.created": false, "": false, "INVOICE.PAID": false} {
		if got := some.Wants(typ); got != want {
			t.Errorf("Wants(%q) = %v, want %v", typ, got, want)
		}
	}
}

func TestSigningSecretsFollowTheGracePeriod(t *testing.T) {
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)

	plain := Endpoint{Secret: "new"}
	if got := plain.SigningSecrets(now); len(got) != 1 || got[0] != "new" {
		t.Errorf("without a rotation only the secret signs: %v", got)
	}
	rotated := Endpoint{Secret: "new", PrevSecret: "old", PrevSecretExpiresAt: &until}
	if got := rotated.SigningSecrets(now); len(got) != 2 || got[0] != "new" || got[1] != "old" {
		t.Errorf("during the grace period the new secret comes first and the old one still signs: %v", got)
	}
	if got := rotated.SigningSecrets(until); len(got) != 1 {
		t.Errorf("at the moment the grace period ends the old secret stops signing: %v", got)
	}
	if got := rotated.SigningSecrets(until.Add(time.Second)); len(got) != 1 || got[0] != "new" {
		t.Errorf("after the grace period only the new secret signs: %v", got)
	}
	noExpiry := Endpoint{Secret: "new", PrevSecret: "old"}
	if got := noExpiry.SigningSecrets(now); len(got) != 1 {
		t.Errorf("a previous secret without an expiry must never keep signing: %v", got)
	}
}

func TestStateFinal(t *testing.T) {
	for state, want := range map[State]bool{Pending: false, InFlight: false, Succeeded: true, Failed: true, Cancelled: true} {
		if got := state.Final(); got != want {
			t.Errorf("%s.Final() = %v, want %v", state, got, want)
		}
	}
}

func TestDisabled(t *testing.T) {
	at := time.Now()
	if (Endpoint{}).Disabled() || !(Endpoint{DisabledAt: &at}).Disabled() {
		t.Error("an endpoint is disabled exactly when it has a time it was switched off")
	}
}

func TestEndpointJSONNeverContainsASecret(t *testing.T) {
	at := time.Now()
	b, err := json.Marshal(Endpoint{ID: "ep_x", URL: "https://a.example", Secret: "whsec_abc", PrevSecret: "whsec_old", PrevSecretExpiresAt: &at})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	for k := range m {
		if k == "secret" || k == "prev_secret" || k == "prev_secret_expires_at" {
			t.Fatalf("a secret field %q must not be part of the normal JSON: %s", k, b)
		}
	}
}

func TestAttemptJSONReportsWholeMilliseconds(t *testing.T) {
	b, err := json.Marshal(Attempt{Duration: 1234 * time.Millisecond, Trigger: Manual, Failure: FailTimeout})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m["duration_ms"] != float64(1234) || m["trigger"] != "manual" || m["failure"] != "timeout" {
		t.Fatalf("%s", b)
	}
}
