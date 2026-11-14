package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
)

// A change and its audit entry are one transaction. These tests break the audit log on purpose and look for a change that
// still happened, which is the gap this design closes.

func breakAuditLog(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.db.Exec(`CREATE TRIGGER audit_down BEFORE INSERT ON audit_log BEGIN SELECT RAISE(ABORT, 'audit log unavailable'); END`)
	if err != nil {
		t.Fatal(err)
	}
}

func allAudit(t *testing.T, s *Store) []domain.AuditEntry {
	t.Helper()
	got, _, err := s.ListAudit(ctx(), AuditFilter{}, Page{Limit: 250})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func note(action, kind string) domain.AuditEntry {
	return domain.AuditEntry{ActorKind: "admin", ActorID: "admin", Action: action, ResourceKind: kind, RequestID: "req_1"}
}

// auditedWrite is one audited change: do makes it with the given options, and applied says whether it took effect.
type auditedWrite struct {
	name    string
	action  string
	prepare func(t *testing.T) (w world, do func(...WriteOption) error, applied func() bool)
}

func finishedDelivery(t *testing.T, w world) domain.Delivery {
	t.Helper()
	w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	claims, err := w.s.ClaimDue(ctx(), t0, time.Minute, "w", 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %v %v", claims, err)
	}
	err = w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "w", Result: ResultFailed, Now: t0,
		Attempt: domain.Attempt{StartedAt: t0, Failure: domain.FailTimeout, Trigger: domain.Scheduled}})
	if err != nil {
		t.Fatal(err)
	}
	return ds[0]
}

func auditedWrites() []auditedWrite {
	return []auditedWrite{
		{"create an app", "app.create", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			uid := "fresh"
			return w,
				func(o ...WriteOption) error { _, err := w.s.CreateApp(ctx(), "fresh", &uid, t0, o...); return err },
				func() bool { _, err := w.s.ResolveApp(ctx(), uid); return err == nil }
		}},
		{"create an endpoint", "endpoint.create", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.CreateEndpoint(ctx(), NewEndpoint{AppID: w.app.ID, URL: "https://new.example/h", Secret: "whsec_c2VjcmV0"}, t0, o...)
					return err
				},
				func() bool { l, _, _ := w.s.ListEndpoints(ctx(), w.app.ID, Page{}); return len(l) == 1 }
		}},
		{"update an endpoint", "endpoint.update", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			e := w.endpoint(t, "https://a.example/h", nil, 1)
			d := "changed"
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{Description: &d}, t0, o...)
					return err
				},
				func() bool { got, _ := w.s.GetEndpoint(ctx(), w.app.ID, e.ID); return got.Description == d }
		}},
		{"delete an endpoint", "endpoint.delete", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			e := w.endpoint(t, "https://a.example/h", nil, 1)
			return w,
				func(o ...WriteOption) error { return w.s.DeleteEndpoint(ctx(), w.app.ID, e.ID, t0, o...) },
				func() bool { _, err := w.s.GetEndpoint(ctx(), w.app.ID, e.ID); return errors.Is(err, ErrNotFound) }
		}},
		{"rotate a secret", "endpoint.secret_rotate", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			e := w.endpoint(t, "https://a.example/h", nil, 1)
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.RotateSecret(ctx(), w.app.ID, e.ID, "whsec_bmV3", t0.Add(time.Hour), t0, o...)
					return err
				},
				func() bool { got, _ := w.s.GetEndpoint(ctx(), w.app.ID, e.ID); return got.Secret == "whsec_bmV3" }
		}},
		{"recover an endpoint", "endpoint.recover", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			e := w.endpoint(t, "https://a.example/h", nil, 1)
			off := true
			if _, err := w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{Disabled: &off}, t0); err != nil {
				t.Fatal(err)
			}
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.RecoverEndpoint(ctx(), w.app.ID, e.ID, t0.Add(-time.Hour), t0, o...)
					return err
				},
				func() bool { got, _ := w.s.GetEndpoint(ctx(), w.app.ID, e.ID); return got.DisabledAt == nil }
		}},
		{"replay a delivery", "delivery.replay", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			d := finishedDelivery(t, w)
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.Replay(ctx(), w.app.ID, d.ID, t0.Add(time.Hour), o...)
					return err
				},
				func() bool { got, _ := w.s.GetDelivery(ctx(), w.app.ID, d.ID); return got.State == domain.Pending }
		}},
		{"create a token", "token.create", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: "h1", Prefix: "bwl_aaaa"}, t0, o...)
					return err
				},
				func() bool { l, _, _ := w.s.ListAppTokens(ctx(), w.app.ID, Page{}); return len(l) == 1 }
		}},
		{"revoke a token", "token.revoke", func(t *testing.T) (world, func(...WriteOption) error, func() bool) {
			w := newWorld(t)
			tok, err := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: "h1", Prefix: "bwl_aaaa"}, t0)
			if err != nil {
				t.Fatal(err)
			}
			return w,
				func(o ...WriteOption) error {
					_, err := w.s.RevokeAppToken(ctx(), w.app.ID, tok.ID, t0, o...)
					return err
				},
				func() bool { got, _ := w.s.GetAppToken(ctx(), w.app.ID, tok.ID); return got.Revoked() }
		}},
	}
}

func TestAnAuditedChangeWritesExactlyOneEntry(t *testing.T) {
	for _, c := range auditedWrites() {
		t.Run(c.name, func(t *testing.T) {
			w, do, applied := c.prepare(t)
			if err := do(Audited(note(c.action, "thing"))); err != nil {
				t.Fatal(err)
			}
			if !applied() {
				t.Fatal("the change did not happen")
			}
			got := allAudit(t, w.s)
			if len(got) != 1 {
				t.Fatalf("want one entry, got %d: %+v", len(got), got)
			}
			e := got[0]
			if e.Action != c.action || e.ActorKind != "admin" || e.RequestID != "req_1" || e.AppID == "" || e.ResourceID == "" || e.ID == "" || e.At.IsZero() {
				t.Fatalf("the entry is incomplete: %+v", e)
			}
		})
	}
}

func TestAChangeWithoutTheOptionWritesNoEntry(t *testing.T) {
	for _, c := range auditedWrites() {
		t.Run(c.name, func(t *testing.T) {
			w, do, applied := c.prepare(t)
			if err := do(); err != nil || !applied() {
				t.Fatalf("the plain change must still work: %v", err)
			}
			if got := allAudit(t, w.s); len(got) != 0 {
				t.Fatalf("no entry was asked for: %+v", got)
			}
		})
	}
}

func TestAChangeIsNotMadeWhenItsEntryCannotBeWritten(t *testing.T) {
	for _, c := range auditedWrites() {
		t.Run(c.name, func(t *testing.T) {
			w, do, applied := c.prepare(t)
			breakAuditLog(t, w.s)
			err := do(Audited(note(c.action, "thing")))
			if !errors.Is(err, ErrAuditWrite) {
				t.Fatalf("want ErrAuditWrite, got %v", err)
			}
			if applied() {
				t.Fatal("the change survived the failure of its audit entry, so the log has a gap")
			}
		})
	}
}

func TestAnUnauditedChangeStillWorksWhenTheAuditLogIsBroken(t *testing.T) {
	// The log is only in the way of changes that ask for an entry. The delivery path never does.
	w := newWorld(t)
	breakAuditLog(t, w.s)
	w.endpoint(t, "https://a.example/h", nil, 1)
	if _, ds := w.send(t, "x", t0); len(ds) != 1 {
		t.Fatalf("a message must still be accepted: %v", ds)
	}
}

func TestRevokingTwiceLeavesOneEntry(t *testing.T) {
	w := newWorld(t)
	tok, _ := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: "h1", Prefix: "bwl_aaaa"}, t0)
	for range 2 {
		if _, err := w.s.RevokeAppToken(ctx(), w.app.ID, tok.ID, t0, Audited(note("token.revoke", "token"))); err != nil {
			t.Fatal(err)
		}
	}
	if got := allAudit(t, w.s); len(got) != 1 {
		t.Fatalf("the second call changed nothing, so it records nothing: %+v", got)
	}
}

func TestRevokingAtTheSameTimeLeavesOneEntry(t *testing.T) {
	w := newWorld(t)
	tok, _ := w.s.CreateAppToken(ctx(), NewAppToken{AppID: w.app.ID, Name: "portal", Hash: "h1", Prefix: "bwl_aaaa"}, t0)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := w.s.RevokeAppToken(ctx(), w.app.ID, tok.ID, t0, Audited(note("token.revoke", "token"))); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := allAudit(t, w.s); len(got) != 1 {
		t.Fatalf("eight callers, one revocation, so one entry, got %d", len(got))
	}
}

func TestRecoveringRecordsHowManyWereReplayed(t *testing.T) {
	w := newWorld(t)
	d := finishedDelivery(t, w)
	eps, _, _ := w.s.ListEndpoints(ctx(), w.app.ID, Page{})
	e := domain.AuditEntry{ActorKind: "admin", ActorID: "admin", Action: "endpoint.recover", ResourceKind: "endpoint", ResourceID: eps[0].ID, AppID: w.app.ID,
		Detail: json.RawMessage(`{"note":"kept"}`)}
	n, err := w.s.RecoverEndpoint(ctx(), w.app.ID, eps[0].ID, t0.Add(-time.Hour), t0.Add(time.Minute), Audited(e))
	if err != nil || n != 1 {
		t.Fatalf("replayed %d (%s): %v", n, d.ID, err)
	}
	got := allAudit(t, w.s)
	if len(got) != 1 {
		t.Fatalf("%+v", got)
	}
	var detail map[string]any
	if err := json.Unmarshal(got[0].Detail, &detail); err != nil || detail["replayed"] != float64(1) || detail["note"] != "kept" {
		t.Fatalf("the count joins the details the caller gave: %s %v", got[0].Detail, err)
	}
}

func TestAResourceIDTheCallerGaveIsKept(t *testing.T) {
	w := newWorld(t)
	e := note("endpoint.create", "endpoint")
	e.AppID, e.ResourceID = w.app.ID, "ep_given"
	if _, err := w.s.CreateEndpoint(ctx(), NewEndpoint{AppID: w.app.ID, URL: "https://x.example/h", Secret: "whsec_c2VjcmV0"}, t0, Audited(e)); err != nil {
		t.Fatal(err)
	}
	if got := allAudit(t, w.s); len(got) != 1 || got[0].ResourceID != "ep_given" {
		t.Fatalf("the store fills only what is empty: %+v", got)
	}
}

func TestRecordAuditOnItsOwnReportsAFailure(t *testing.T) {
	s := newStore(t)
	breakAuditLog(t, s)
	_, err := s.RecordAudit(ctx(), note("endpoint.secret_read", "endpoint"), t0)
	if !errors.Is(err, ErrAuditWrite) {
		t.Fatalf("want ErrAuditWrite, got %v", err)
	}
}
