package store

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/token"
)

func TestATokenCanBeFoundByItsHashUntilItIsRevoked(t *testing.T) {
	w := newWorld(t)
	tok := token.New()
	made, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: tok.Hash, Prefix: tok.Short}, t0)
	if err != nil || made.AppID != w.app.ID || made.Prefix != tok.Short {
		t.Fatalf("%+v %v", made, err)
	}
	found, err := w.s.AppTokenByHash(ctx(), token.Hash(tok.Secret), t0)
	if err != nil || found.ID != made.ID {
		t.Fatalf("the token must be found by the hash of its secret: %+v %v", found, err)
	}
	if _, err := w.s.AppTokenByHash(ctx(), token.Hash("bwl_wrong"), t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a hash nobody has is not found: %v", err)
	}
	if _, err := w.s.RevokeAppToken(ctx(), w.app.ID, made.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AppTokenByHash(ctx(), tok.Hash, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked token must not be found: %v", err)
	}
}

func TestOnlyAHashIsStored(t *testing.T) {
	w := newWorld(t)
	tok := token.New()
	w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: tok.Hash, Prefix: tok.Short}, t0)
	var dump string
	rows, _ := w.s.db.Query("SELECT id || name || token_hash || token_prefix FROM app_tokens")
	for rows.Next() {
		var s string
		rows.Scan(&s)
		dump += s
	}
	rows.Close()
	if contains(dump, tok.Secret) || contains(dump, tok.Secret[len(token.Prefix)+6:]) {
		t.Fatal("the token itself must never be written to the database")
	}
}

func contains(s, sub string) bool { return len(sub) > 0 && len(s) >= len(sub) && indexOf(s, sub) >= 0 }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestRevokingTwiceKeepsTheFirstTimeAndAWrongAppFindsNothing(t *testing.T) {
	w := newWorld(t)
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	tok := token.New()
	made, _ := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: tok.Hash, Prefix: tok.Short}, t0)

	if _, err := w.s.RevokeAppToken(ctx(), other.ID, made.ID, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another app must not revoke this token: %v", err)
	}
	if _, err := w.s.GetAppToken(ctx(), other.ID, made.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another app must not see this token: %v", err)
	}
	first, _ := w.s.RevokeAppToken(ctx(), w.app.ID, made.ID, t0.Add(time.Hour))
	again, err := w.s.RevokeAppToken(ctx(), w.app.ID, made.ID, t0.Add(2*time.Hour))
	if err != nil || !again.RevokedAt.Equal(*first.RevokedAt) {
		t.Fatalf("the first revocation time stays: %v %v", first.RevokedAt, again.RevokedAt)
	}
}

func TestAnAppHoldsAtMostFiftyWorkingTokensAndRevokingMakesRoom(t *testing.T) {
	w := newWorld(t)
	var first domain.AppToken
	for i := range MaxActiveTokensPerApp {
		tok := token.New()
		made, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "t" + strconv.Itoa(i), Hash: tok.Hash, Prefix: tok.Short}, t0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = made
		}
	}
	tok := token.New()
	if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "one too many", Hash: tok.Hash, Prefix: tok.Short}, t0); !errors.Is(err, ErrTooManyTokens) {
		t.Fatalf("got %v", err)
	}
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: other.ID, Name: "fine", Hash: tok.Hash, Prefix: tok.Short}, t0); err != nil {
		t.Fatalf("the limit is per app: %v", err)
	}
	w.s.RevokeAppToken(ctx(), w.app.ID, first.ID, t0)
	tok2 := token.New()
	if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "now fits", Hash: tok2.Hash, Prefix: tok2.Short}, t0); err != nil {
		t.Fatalf("a revoked token no longer counts: %v", err)
	}
}

func TestTokensListNewestFirstWithRevokedOnesAndPages(t *testing.T) {
	w := newWorld(t)
	for i := range 5 {
		tok := token.New()
		w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "t" + strconv.Itoa(i), Hash: tok.Hash, Prefix: tok.Short}, t0.Add(time.Duration(i)*time.Second))
	}
	page1, next, err := w.s.ListAppTokens(ctx(), w.app.ID, Page{Limit: 3})
	if err != nil || len(page1) != 3 || next == "" || page1[0].Name != "t4" {
		t.Fatalf("%+v %q %v", page1, next, err)
	}
	page2, next2, _ := w.s.ListAppTokens(ctx(), w.app.ID, Page{Limit: 3, Cursor: next})
	if len(page2) != 2 || next2 != "" || page2[1].Name != "t0" {
		t.Fatalf("%+v %q", page2, next2)
	}
	if _, _, err := w.s.ListAppTokens(ctx(), w.app.ID, Page{Cursor: "garbage!"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("got %v", err)
	}
}

func TestAuditEntriesAreRecordedFilteredAndPaged(t *testing.T) {
	w := newWorld(t)
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	rec := func(app, action string, at time.Duration) {
		_, err := w.s.RecordAudit(ctx(), domain.AuditEntry{ActorKind: domain.ActorAdmin, ActorID: "admin", AppID: app, Action: action,
			ResourceKind: "endpoint", ResourceID: "ep_x", Detail: json.RawMessage(`{"changed":["url"]}`), RequestID: "req_1"}, t0.Add(at))
		if err != nil {
			t.Fatal(err)
		}
	}
	rec(w.app.ID, "endpoint.create", 0)
	rec(w.app.ID, "endpoint.update", time.Second)
	rec(other.ID, "endpoint.create", 2*time.Second)
	rec(w.app.ID, "endpoint.update", 3*time.Second)

	all, _, _ := w.s.ListAudit(ctx(), AuditFilter{}, Page{})
	if len(all) != 4 || all[0].Action != "endpoint.update" || !all[0].At.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("newest first: %+v", all)
	}
	if e := all[0]; e.ActorKind != domain.ActorAdmin || e.ResourceID != "ep_x" || string(e.Detail) != `{"changed":["url"]}` || e.RequestID != "req_1" || e.ID == "" {
		t.Fatalf("every field is kept: %+v", e)
	}
	mine, _, _ := w.s.ListAudit(ctx(), AuditFilter{AppID: w.app.ID}, Page{})
	if len(mine) != 3 {
		t.Fatalf("one app sees only its own entries: %d", len(mine))
	}
	updates, _, _ := w.s.ListAudit(ctx(), AuditFilter{AppID: w.app.ID, Action: "endpoint.update"}, Page{})
	if len(updates) != 2 {
		t.Fatalf("filter by action: %d", len(updates))
	}
	p1, next, _ := w.s.ListAudit(ctx(), AuditFilter{}, Page{Limit: 3})
	p2, next2, _ := w.s.ListAudit(ctx(), AuditFilter{}, Page{Limit: 3, Cursor: next})
	if len(p1) != 3 || len(p2) != 1 || next2 != "" {
		t.Fatalf("paging: %d %d %q", len(p1), len(p2), next2)
	}
}

func TestAuditEntryRefusesAnUnknownActorKind(t *testing.T) {
	w := newWorld(t)
	if _, err := w.s.RecordAudit(ctx(), domain.AuditEntry{ActorKind: "robot", ActorID: "x", Action: "a"}, t0); err == nil {
		t.Fatal("the database must refuse an actor kind it does not know")
	}
}

func TestAuditEntriesAreNeverChangedByTheStore(t *testing.T) {
	w := newWorld(t)
	w.s.RecordAudit(ctx(), domain.AuditEntry{ActorKind: domain.ActorAdmin, ActorID: "admin", Action: "a"}, t0)
	// The store offers no way to update or delete one entry, only to purge by age.
	if n, _ := w.s.PurgeAudit(ctx(), t0.Add(-time.Hour), 10); n != 0 {
		t.Fatalf("a recent entry is kept: %d", n)
	}
	if n, _ := w.s.PurgeAudit(ctx(), t0.Add(time.Hour), 10); n != 1 {
		t.Fatalf("an old entry is purged: %d", n)
	}
	if n, err := w.s.PurgeAudit(ctx(), t0, 0); n != 0 || err != nil {
		t.Fatalf("limit zero does nothing: %d %v", n, err)
	}
}

func TestPurgeAuditGoesOldestFirstInBatches(t *testing.T) {
	w := newWorld(t)
	for i := range 10 {
		w.s.RecordAudit(ctx(), domain.AuditEntry{ActorKind: domain.ActorAdmin, ActorID: "admin", Action: "a"}, t0.Add(time.Duration(i)*time.Hour))
	}
	if n, _ := w.s.PurgeAudit(ctx(), t0.Add(100*time.Hour), 4); n != 4 {
		t.Fatalf("got %d", n)
	}
	left, _, _ := w.s.ListAudit(ctx(), AuditFilter{}, Page{})
	if len(left) != 6 || !left[len(left)-1].At.Equal(t0.Add(4*time.Hour)) {
		t.Fatalf("the oldest four go first: %d left, oldest %v", len(left), left[len(left)-1].At)
	}
}

// Two actions in the same millisecond have the same time and random ids. The log must still come out in the order they
// happened, and paging through it must not skip or repeat an entry.
func TestAuditEntriesFromTheSameMomentKeepTheirOrder(t *testing.T) {
	w := newWorld(t)
	for i := range 30 {
		_, err := w.s.RecordAudit(ctx(), domain.AuditEntry{ActorKind: domain.ActorAdmin, ActorID: "admin", Action: "a" + strconv.Itoa(i)}, t0)
		if err != nil {
			t.Fatal(err)
		}
	}
	var seen []string
	cur := ""
	for {
		page, next, err := w.s.ListAudit(ctx(), AuditFilter{}, Page{Limit: 7, Cursor: cur})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			seen = append(seen, e.Action)
		}
		if next == "" {
			break
		}
		cur = next
	}
	if len(seen) != 30 {
		t.Fatalf("paging must return every entry once, got %d", len(seen))
	}
	for i, a := range seen {
		if want := "a" + strconv.Itoa(29-i); a != want {
			t.Fatalf("entry %d is %s, want %s: the newest comes first, in the order written", i, a, want)
		}
	}
}

func TestATokenHasAScopeThatDefaultsToFull(t *testing.T) {
	w := newWorld(t)
	a := token.New()
	full, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "default", Hash: a.Hash, Prefix: a.Short}, t0)
	if err != nil || full.Scope != domain.ScopeFull {
		t.Fatalf("%+v %v", full, err)
	}
	b := token.New()
	read, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "viewer", Hash: b.Hash, Prefix: b.Short, Scope: domain.ScopeRead}, t0)
	if err != nil || read.Scope != domain.ScopeRead {
		t.Fatalf("%+v %v", read, err)
	}
	got, _ := w.s.AppTokenByHash(ctx(), b.Hash, t0)
	if got.Scope != domain.ScopeRead {
		t.Fatalf("the scope is kept: %+v", got)
	}
	c := token.New()
	if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "x", Hash: c.Hash, Prefix: c.Short, Scope: "admin"}, t0); err == nil {
		t.Fatal("the database must refuse a scope it does not know")
	}
}

func TestATokenStopsWorkingWhenItExpires(t *testing.T) {
	w := newWorld(t)
	tok := token.New()
	exp := t0.Add(time.Hour)
	made, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "short", Hash: tok.Hash, Prefix: tok.Short, ExpiresAt: &exp}, t0)
	if err != nil || made.ExpiresAt == nil || !made.ExpiresAt.Equal(exp) {
		t.Fatalf("%+v %v", made, err)
	}
	if _, err := w.s.AppTokenByHash(ctx(), tok.Hash, t0.Add(59*time.Minute)); err != nil {
		t.Fatalf("before it expires it works: %v", err)
	}
	for _, at := range []time.Duration{time.Hour, 2 * time.Hour} {
		if _, err := w.s.AppTokenByHash(ctx(), tok.Hash, t0.Add(at)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("at +%s it must not work: %v", at, err)
		}
	}
	if !made.Expired(t0.Add(time.Hour)) || made.Expired(t0) {
		t.Fatal("Expired disagrees with the lookup")
	}
	// An expired token is still listed, so the history stays readable.
	list, _, _ := w.s.ListAppTokens(ctx(), w.app.ID, Page{})
	if len(list) != 1 || list[0].ExpiresAt == nil {
		t.Fatalf("%+v", list)
	}
}

func TestExpiredTokensDoNotCountTowardTheLimit(t *testing.T) {
	w := newWorld(t)
	exp := t0.Add(time.Hour)
	for i := range MaxActiveTokensPerApp {
		tok := token.New()
		if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: strconv.Itoa(i), Hash: tok.Hash, Prefix: tok.Short, ExpiresAt: &exp}, t0); err != nil {
			t.Fatal(err)
		}
	}
	tok := token.New()
	if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "full", Hash: tok.Hash, Prefix: tok.Short}, t0); !errors.Is(err, ErrTooManyTokens) {
		t.Fatalf("got %v", err)
	}
	if _, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "later", Hash: tok.Hash, Prefix: tok.Short}, t0.Add(2*time.Hour)); err != nil {
		t.Fatalf("once they have expired there is room: %v", err)
	}
}

func TestTouchRecordsWhenATokenWasLastUsed(t *testing.T) {
	w := newWorld(t)
	tok := token.New()
	made, _ := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "x", Hash: tok.Hash, Prefix: tok.Short}, t0)
	if made.LastUsedAt != nil {
		t.Fatal("a new token has not been used")
	}
	if err := w.s.TouchAppToken(ctx(), made.ID, t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	got, _ := w.s.GetAppToken(ctx(), w.app.ID, made.ID)
	if got.LastUsedAt == nil || !got.LastUsedAt.Equal(t0.Add(time.Minute)) {
		t.Fatalf("%+v", got)
	}
}
