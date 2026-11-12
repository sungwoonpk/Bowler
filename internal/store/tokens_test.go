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
	made, err := w.s.CreateAppToken(ctx(), w.app.ID, "portal", tok.Hash, tok.Short, t0)
	if err != nil || made.AppID != w.app.ID || made.Prefix != tok.Short {
		t.Fatalf("%+v %v", made, err)
	}
	found, err := w.s.AppTokenByHash(ctx(), token.Hash(tok.Secret))
	if err != nil || found.ID != made.ID {
		t.Fatalf("the token must be found by the hash of its secret: %+v %v", found, err)
	}
	if _, err := w.s.AppTokenByHash(ctx(), token.Hash("bwl_wrong")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a hash nobody has is not found: %v", err)
	}
	if _, err := w.s.RevokeAppToken(ctx(), w.app.ID, made.ID, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := w.s.AppTokenByHash(ctx(), tok.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a revoked token must not be found: %v", err)
	}
}

func TestOnlyAHashIsStored(t *testing.T) {
	w := newWorld(t)
	tok := token.New()
	w.s.CreateAppToken(ctx(), w.app.ID, "portal", tok.Hash, tok.Short, t0)
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
	made, _ := w.s.CreateAppToken(ctx(), w.app.ID, "portal", tok.Hash, tok.Short, t0)

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
		made, err := w.s.CreateAppToken(ctx(), w.app.ID, "t"+strconv.Itoa(i), tok.Hash, tok.Short, t0)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = made
		}
	}
	tok := token.New()
	if _, err := w.s.CreateAppToken(ctx(), w.app.ID, "one too many", tok.Hash, tok.Short, t0); !errors.Is(err, ErrTooManyTokens) {
		t.Fatalf("got %v", err)
	}
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	if _, err := w.s.CreateAppToken(ctx(), other.ID, "fine", tok.Hash, tok.Short, t0); err != nil {
		t.Fatalf("the limit is per app: %v", err)
	}
	w.s.RevokeAppToken(ctx(), w.app.ID, first.ID, t0)
	tok2 := token.New()
	if _, err := w.s.CreateAppToken(ctx(), w.app.ID, "now fits", tok2.Hash, tok2.Short, t0); err != nil {
		t.Fatalf("a revoked token no longer counts: %v", err)
	}
}

func TestTokensListNewestFirstWithRevokedOnesAndPages(t *testing.T) {
	w := newWorld(t)
	for i := range 5 {
		tok := token.New()
		w.s.CreateAppToken(ctx(), w.app.ID, "t"+strconv.Itoa(i), tok.Hash, tok.Short, t0.Add(time.Duration(i)*time.Second))
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
