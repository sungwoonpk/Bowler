package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/secretbox"
)

func boxOf(t *testing.T, current byte, previous ...byte) *secretbox.Box {
	t.Helper()
	k := func(b byte) []byte { return bytes.Repeat([]byte{b}, secretbox.KeySize) }
	var prev [][]byte
	for _, p := range previous {
		prev = append(prev, k(p))
	}
	b, err := secretbox.New(k(current), prev...)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func openAt(t *testing.T, path string, box *secretbox.Box) *Store {
	t.Helper()
	var opts []Option
	if box != nil {
		opts = append(opts, WithSecretBox(box))
	}
	s, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func addEndpoint(t *testing.T, s *Store, appID, secret string) domain.Endpoint {
	t.Helper()
	e, err := s.CreateEndpoint(ctx(), NewEndpoint{AppID: appID, URL: "https://a.example/h", Secret: secret}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func rawSecrets(t *testing.T, s *Store, id string) (secret, prev string) {
	t.Helper()
	var p *string
	if err := s.db.QueryRow("SELECT secret, prev_secret FROM endpoints WHERE id = ?", id).Scan(&secret, &p); err != nil {
		t.Fatal(err)
	}
	if p != nil {
		prev = *p
	}
	return
}

func TestSecretsAreSealedInTheFileAndPlainEverywhereElse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	s := openAt(t, path, boxOf(t, 1))
	app, _ := s.CreateApp(ctx(), "acme", nil, t0)
	e := addEndpoint(t, s, app.ID, "whsec_PLAINMARKER1")

	raw, _ := rawSecrets(t, s, e.ID)
	if !secretbox.IsSealed(raw) || strings.Contains(raw, "PLAINMARKER") {
		t.Fatalf("the file must hold the sealed value: %q", raw)
	}
	got, err := s.GetEndpoint(ctx(), app.ID, e.ID)
	if err != nil || got.Secret != "whsec_PLAINMARKER1" {
		t.Fatalf("the program sees the plain secret: %q %v", got.Secret, err)
	}
	s.CreateMessage(ctx(), NewMessage{AppID: app.ID, EventType: "x", Payload: []byte(`{}`)}, t0)
	claims, err := s.ClaimDue(ctx(), t0, time.Minute, "w", 5)
	if err != nil || len(claims) != 1 || claims[0].Endpoint.Secret != "whsec_PLAINMARKER1" {
		t.Fatalf("a claim carries the plain secret, so signing works: %+v %v", claims, err)
	}
}

func TestRotatingSealsBothTheNewAndThePreviousSecret(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "b.db"), boxOf(t, 1))
	app, _ := s.CreateApp(ctx(), "acme", nil, t0)
	e := addEndpoint(t, s, app.ID, "whsec_OLDMARKER")
	rot, err := s.RotateSecret(ctx(), app.ID, e.ID, "whsec_NEWMARKER", t0.Add(time.Hour))
	if err != nil || rot.Secret != "whsec_NEWMARKER" || rot.PrevSecret != "whsec_OLDMARKER" {
		t.Fatalf("%+v %v", rot, err)
	}
	secret, prev := rawSecrets(t, s, e.ID)
	if !secretbox.IsSealed(secret) || !secretbox.IsSealed(prev) || strings.Contains(secret+prev, "MARKER") {
		t.Fatalf("both are sealed: %q %q", secret, prev)
	}
	got, _ := s.GetEndpoint(ctx(), app.ID, e.ID)
	if got.Secret != "whsec_NEWMARKER" || got.PrevSecret != "whsec_OLDMARKER" {
		t.Fatalf("%+v", got)
	}
}

func TestADatabaseFromBeforeEncryptionIsSealedOnStartAndNoPlainTextStaysInTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	plain := openAt(t, path, nil)
	app, _ := plain.CreateApp(ctx(), "acme", nil, t0)
	var ids []string
	for i := range 5 {
		e := addEndpoint(t, plain, app.ID, "whsec_LEGACYMARKER"+string(rune('A'+i)))
		ids = append(ids, e.ID)
	}
	plain.RotateSecret(ctx(), app.ID, ids[0], "whsec_ROTATEDMARKER", t0.Add(time.Hour))
	plain.Close()

	s := openAt(t, path, boxOf(t, 1))
	n, err := s.ResealSecrets(ctx(), 2) // in batches of two
	if err != nil || n != 5 {
		t.Fatalf("all five are sealed: %d %v", n, err)
	}
	if again, err := s.ResealSecrets(ctx(), 2); err != nil || again != 0 {
		t.Fatalf("a second pass has nothing to do: %d %v", again, err)
	}
	for i, id := range ids {
		raw, _ := rawSecrets(t, s, id)
		if !secretbox.IsSealed(raw) {
			t.Fatalf("endpoint %d is still plain", i)
		}
		got, _ := s.GetEndpoint(ctx(), app.ID, id)
		want := "whsec_LEGACYMARKER" + string(rune('A'+i))
		if i == 0 {
			want = "whsec_ROTATEDMARKER"
		}
		if got.Secret != want {
			t.Fatalf("endpoint %d: %q, want %q", i, got.Secret, want)
		}
	}
	if got, _ := s.GetEndpoint(ctx(), app.ID, ids[0]); got.PrevSecret != "whsec_LEGACYMARKERA" {
		t.Fatalf("the previous secret survives: %q", got.PrevSecret)
	}

	if err := s.Compact(ctx()); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{path, path + "-wal"} {
		b, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, []byte("MARKER")) {
			t.Fatalf("plain text of a secret is still in %s after compacting", filepath.Base(f))
		}
	}
}

func TestRotatingTheKeyReSealsUnderTheNewOneAndTheOldOneCanBeDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	one := openAt(t, path, boxOf(t, 1))
	app, _ := one.CreateApp(ctx(), "acme", nil, t0)
	e := addEndpoint(t, one, app.ID, "whsec_KEYMARKER")
	one.Close()

	// Both keys are given: the new one seals, the old one still opens.
	two := openAt(t, path, boxOf(t, 2, 1))
	if n, err := two.ResealSecrets(ctx(), 10); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	two.Close()

	// Now the old key is not needed.
	only := openAt(t, path, boxOf(t, 2))
	if n, err := only.ResealSecrets(ctx(), 10); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if got, _ := only.GetEndpoint(ctx(), app.ID, e.ID); got.Secret != "whsec_KEYMARKER" {
		t.Fatalf("%q", got.Secret)
	}
	only.Close()

	// And the old key alone no longer works.
	stale := openAt(t, path, boxOf(t, 1))
	if _, err := stale.ResealSecrets(ctx(), 10); !errors.Is(err, secretbox.ErrUnknownKey) {
		t.Fatalf("got %v, want ErrUnknownKey", err)
	}
}

func TestStartingWithoutTheKeyOrWithTheWrongOneIsRefusedAtOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	sealed := openAt(t, path, boxOf(t, 1))
	app, _ := sealed.CreateApp(ctx(), "acme", nil, t0)
	addEndpoint(t, sealed, app.ID, "whsec_x")
	sealed.Close()

	none := openAt(t, path, nil)
	if _, err := none.ResealSecrets(ctx(), 10); !errors.Is(err, secretbox.ErrNoKey) {
		t.Fatalf("no key: got %v", err)
	}
	wrong := openAt(t, path, boxOf(t, 9))
	if _, err := wrong.ResealSecrets(ctx(), 10); !errors.Is(err, secretbox.ErrUnknownKey) {
		t.Fatalf("wrong key: got %v", err)
	}
	// Without a key and without sealed secrets there is nothing to refuse.
	clean := openAt(t, filepath.Join(t.TempDir(), "c.db"), nil)
	capp, _ := clean.CreateApp(ctx(), "acme", nil, t0)
	addEndpoint(t, clean, capp.ID, "whsec_plain")
	if n, err := clean.ResealSecrets(ctx(), 10); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestACiphertextMovedToAnotherEndpointIsRefused(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "b.db"), boxOf(t, 1))
	app, _ := s.CreateApp(ctx(), "acme", nil, t0)
	a := addEndpoint(t, s, app.ID, "whsec_AAAA")
	b := addEndpoint(t, s, app.ID, "whsec_BBBB")
	rawA, _ := rawSecrets(t, s, a.ID)
	if _, err := s.db.Exec("UPDATE endpoints SET secret = ? WHERE id = ?", rawA, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetEndpoint(ctx(), app.ID, b.ID); !errors.Is(err, secretbox.ErrCorrupt) {
		t.Fatalf("an endpoint must not sign with another's secret: got %v", err)
	}
	if _, err := s.ResealSecrets(ctx(), 10); !errors.Is(err, secretbox.ErrCorrupt) {
		t.Fatalf("start-up must notice: got %v", err)
	}
}

func TestWithoutAKeyNothingChangesForExistingUsers(t *testing.T) {
	s := openAt(t, filepath.Join(t.TempDir(), "b.db"), nil)
	app, _ := s.CreateApp(ctx(), "acme", nil, t0)
	e := addEndpoint(t, s, app.ID, "whsec_PLAINMARKER")
	if raw, _ := rawSecrets(t, s, e.ID); raw != "whsec_PLAINMARKER" {
		t.Fatalf("with no key the secret is stored as before: %q", raw)
	}
}
