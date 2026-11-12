package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
)

var t0 = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bowler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type world struct {
	s   *Store
	app domain.App
}

func newWorld(t *testing.T) world {
	t.Helper()
	s := newStore(t)
	app, err := s.CreateApp(context.Background(), "acme", nil, t0)
	if err != nil {
		t.Fatal(err)
	}
	return world{s: s, app: app}
}

func (w world) endpoint(t *testing.T, url string, types []string, concurrency int) domain.Endpoint {
	t.Helper()
	e, err := w.s.CreateEndpoint(context.Background(), NewEndpoint{AppID: w.app.ID, URL: url, EventTypes: types, Concurrency: concurrency, Secret: "whsec_c2VjcmV0"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (w world) send(t *testing.T, eventType string, at time.Time) (domain.Message, []domain.Delivery) {
	t.Helper()
	m, d, _, err := w.s.CreateMessage(context.Background(), NewMessage{AppID: w.app.ID, EventType: eventType, Payload: json.RawMessage(`{"n":1}`)}, at)
	if err != nil {
		t.Fatal(err)
	}
	return m, d
}

func ctx() context.Context { return context.Background() }

func TestOpenIsRepeatable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	app, _ := s.CreateApp(ctx(), "keep me", nil, t0)
	s.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening must work: %v", err)
	}
	defer s2.Close()
	got, err := s2.ResolveApp(ctx(), app.ID)
	if err != nil || got.Name != "keep me" {
		t.Fatalf("data must survive a reopen: %v %v", got, err)
	}
	var v int
	s2.db.QueryRow("PRAGMA user_version").Scan(&v)
	files, _ := migrationFiles.ReadDir("migrations")
	if v != len(files) {
		t.Fatalf("schema version = %d, want %d (one per migration file)", v, len(files))
	}
}

func TestAppsAndPagination(t *testing.T) {
	s := newStore(t)
	uid := "customer-42"
	a, err := s.CreateApp(ctx(), "one", &uid, t0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateApp(ctx(), "dup", &uid, t0); !errors.Is(err, ErrConflict) {
		t.Fatalf("a repeated uid must conflict, got %v", err)
	}
	for _, ref := range []string{a.ID, uid} {
		got, err := s.ResolveApp(ctx(), ref)
		if err != nil || got.ID != a.ID {
			t.Fatalf("resolve %q: %v %v", ref, got, err)
		}
	}
	if _, err := s.ResolveApp(ctx(), "app_nothere"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}

	for i := range 6 {
		s.CreateApp(ctx(), fmt.Sprintf("app-%d", i), nil, t0.Add(time.Duration(i+1)*time.Second))
	}
	var seen []string
	cursor := ""
	for pages := 0; ; pages++ {
		if pages > 10 {
			t.Fatal("pagination does not end")
		}
		items, next, err := s.ListApps(ctx(), Page{Limit: 3, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range items {
			seen = append(seen, it.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 7 {
		t.Fatalf("saw %d apps across pages, want 7", len(seen))
	}
	unique := map[string]bool{}
	for _, id := range seen {
		unique[id] = true
	}
	if len(unique) != 7 {
		t.Fatal("a page repeated an app")
	}
	if _, _, err := s.ListApps(ctx(), Page{Cursor: "%%%"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("a bad cursor must be refused, got %v", err)
	}
}

func TestFanOutRespectsFiltersAndState(t *testing.T) {
	w := newWorld(t)
	all := w.endpoint(t, "https://a.example/hook", nil, 1)
	paid := w.endpoint(t, "https://b.example/hook", []string{"invoice.paid"}, 1)
	off := w.endpoint(t, "https://c.example/hook", nil, 1)
	gone := w.endpoint(t, "https://d.example/hook", nil, 1)
	disabled := true
	if _, err := w.s.UpdateEndpoint(ctx(), w.app.ID, off.ID, EndpointUpdate{Disabled: &disabled}, t0); err != nil {
		t.Fatal(err)
	}
	if err := w.s.DeleteEndpoint(ctx(), w.app.ID, gone.ID, t0); err != nil {
		t.Fatal(err)
	}

	_, ds := w.send(t, "invoice.paid", t0)
	got := map[string]bool{}
	for _, d := range ds {
		got[d.EndpointID] = true
	}
	if !got[all.ID] || !got[paid.ID] || got[off.ID] || got[gone.ID] || len(ds) != 2 {
		t.Fatalf("an invoice.paid event should go to the open endpoint and the matching filter only, got %v", got)
	}

	_, ds = w.send(t, "invoice.created", t0)
	if len(ds) != 1 || ds[0].EndpointID != all.ID {
		t.Fatalf("a filtered endpoint must not get other events, got %+v", ds)
	}
}

func TestMessageWithNoEndpointsIsStillRecorded(t *testing.T) {
	w := newWorld(t)
	m, ds := w.send(t, "x", t0)
	if len(ds) != 0 {
		t.Fatalf("no endpoints means no deliveries, got %d", len(ds))
	}
	if _, err := w.s.GetMessage(ctx(), w.app.ID, m.ID); err != nil {
		t.Fatalf("the message must be kept: %v", err)
	}
}

func TestIdempotencyKeyReturnsTheSameMessage(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	n := NewMessage{AppID: w.app.ID, EventType: "x", Payload: json.RawMessage(`{"a":1}`), IdempotencyKey: "order-7"}
	m1, d1, created1, err := w.s.CreateMessage(ctx(), n, t0)
	if err != nil || !created1 {
		t.Fatalf("first send: %v created=%v", err, created1)
	}
	m2, d2, created2, err := w.s.CreateMessage(ctx(), n, t0.Add(time.Minute))
	if err != nil || created2 {
		t.Fatalf("second send: %v created=%v", err, created2)
	}
	if m1.ID != m2.ID || len(d1) != 1 || len(d2) != 1 || d1[0].ID != d2[0].ID {
		t.Fatalf("a repeat must return the original: %v %v / %v %v", m1.ID, m2.ID, d1, d2)
	}
	var count int
	w.s.db.QueryRow("SELECT COUNT(*) FROM deliveries").Scan(&count)
	if count != 1 {
		t.Fatalf("a repeat must not create another delivery, have %d", count)
	}

	// The same key in another app is a different message.
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	m3, _, created3, err := w.s.CreateMessage(ctx(), NewMessage{AppID: other.ID, EventType: "x", Payload: json.RawMessage(`{}`), IdempotencyKey: "order-7"}, t0)
	if err != nil || !created3 || m3.ID == m1.ID {
		t.Fatalf("keys are per app: %v %v", m3.ID, err)
	}
}

func TestClaimOnlyWhenDueAndOnlyOnce(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	w.send(t, "x", t0.Add(time.Minute))

	if c, err := w.s.ClaimDue(ctx(), t0, 30*time.Second, "w1", 10); err != nil || len(c) != 0 {
		t.Fatalf("nothing is due yet: %v %v", c, err)
	}
	c, err := w.s.ClaimDue(ctx(), t0.Add(time.Minute), 30*time.Second, "w1", 10)
	if err != nil || len(c) != 1 {
		t.Fatalf("one delivery is due: %v %v", c, err)
	}
	if c[0].Delivery.AttemptCount != 1 || c[0].Delivery.State != domain.InFlight || c[0].Endpoint.URL != "https://a.example/hook" || string(c[0].Message.Payload) != `{"n":1}` {
		t.Fatalf("the claim should carry everything needed: %+v", c[0])
	}
	if c2, _ := w.s.ClaimDue(ctx(), t0.Add(time.Minute+time.Second), 30*time.Second, "w2", 10); len(c2) != 0 {
		t.Fatalf("a leased delivery must not be given to a second worker: %v", c2)
	}
}

func TestExpiredLeaseLetsAnotherWorkerTakeOver(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	w.send(t, "x", t0)
	first, _ := w.s.ClaimDue(ctx(), t0, 30*time.Second, "dies", 1)
	if len(first) != 1 {
		t.Fatal("setup")
	}
	// The worker never reports back, as if it was killed.
	at := t0.Add(31 * time.Second)
	again, err := w.s.ClaimDue(ctx(), at, 30*time.Second, "rescuer", 1)
	if err != nil || len(again) != 1 {
		t.Fatalf("an abandoned delivery must be picked up again: %v %v", again, err)
	}
	if again[0].Delivery.AttemptCount != 2 {
		t.Fatalf("the lost attempt still counts, attempt_count = %d", again[0].Delivery.AttemptCount)
	}
}

func TestLeaseLostWorkerCannotOverwriteTheNewHolder(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	w.s.ClaimDue(ctx(), t0, 30*time.Second, "slow", 1)
	at := t0.Add(time.Minute)
	w.s.ClaimDue(ctx(), at, 30*time.Second, "fast", 1)

	// The slow worker finally reports, after its lease was given away.
	err := w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "slow", Result: ResultSucceeded, Now: at,
		Attempt: domain.Attempt{StartedAt: t0, StatusCode: 200, Trigger: domain.Scheduled}})
	if !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("got %v, want ErrLeaseLost", err)
	}
	d, _ := w.s.GetDelivery(ctx(), w.app.ID, ds[0].ID)
	if d.State != domain.InFlight {
		t.Fatalf("the delivery now belongs to the other worker and must stay in flight, state = %s", d.State)
	}
	attempts, _ := w.s.ListAttempts(ctx(), w.app.ID, ds[0].ID)
	if len(attempts) != 1 {
		t.Fatal("the attempt really happened, so it must still be recorded")
	}
}

func TestConcurrencyLimitPerEndpoint(t *testing.T) {
	w := newWorld(t)
	one := w.endpoint(t, "https://one.example/hook", nil, 1)
	three := w.endpoint(t, "https://three.example/hook", nil, 3)
	for range 10 {
		w.send(t, "x", t0)
	}
	claims, err := w.s.ClaimDue(ctx(), t0, time.Minute, "w", 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, c := range claims {
		counts[c.Endpoint.ID]++
	}
	if counts[one.ID] != 1 || counts[three.ID] != 3 {
		t.Fatalf("an endpoint may have at most its concurrency in flight: %v", counts)
	}
	// While those are in flight, nothing more for those endpoints.
	more, _ := w.s.ClaimDue(ctx(), t0, time.Minute, "w", 100)
	if len(more) != 0 {
		t.Fatalf("slots are full, nothing more may be claimed: %d", len(more))
	}
}

func TestSlowEndpointDoesNotStarveOthers(t *testing.T) {
	w := newWorld(t)
	slow := w.endpoint(t, "https://slow.example/hook", nil, 1)
	// A large backlog for the slow endpoint, all older than anything else.
	for i := range 200 {
		w.send(t, "backlog", t0.Add(time.Duration(i)*time.Millisecond))
	}
	healthy := w.endpoint(t, "https://healthy.example/hook", nil, 5)
	// Newer events go to both endpoints.
	for range 5 {
		w.send(t, "fresh", t0.Add(time.Minute))
	}
	claims, err := w.s.ClaimDue(ctx(), t0.Add(2*time.Minute), time.Minute, "w", 10)
	if err != nil {
		t.Fatal(err)
	}
	var slowCount, healthyCount int
	for _, c := range claims {
		switch c.Endpoint.ID {
		case slow.ID:
			slowCount++
		case healthy.ID:
			healthyCount++
		}
	}
	if slowCount != 1 {
		t.Fatalf("the slow endpoint holds one slot, got %d", slowCount)
	}
	if healthyCount != 5 {
		t.Fatalf("the healthy endpoint must not wait behind the slow one's backlog, got %d", healthyCount)
	}
}

func TestDisabledAndDeletedEndpointsAreNotClaimed(t *testing.T) {
	w := newWorld(t)
	e1 := w.endpoint(t, "https://a.example/hook", nil, 1)
	e2 := w.endpoint(t, "https://b.example/hook", nil, 1)
	w.send(t, "x", t0)
	yes := true
	w.s.UpdateEndpoint(ctx(), w.app.ID, e1.ID, EndpointUpdate{Disabled: &yes}, t0)
	if c, _ := w.s.ClaimDue(ctx(), t0, time.Minute, "w", 10); len(c) != 1 || c[0].Endpoint.ID != e2.ID {
		t.Fatalf("a disabled endpoint must be skipped: %v", c)
	}
	w.s.DeleteEndpoint(ctx(), w.app.ID, e2.ID, t0)
	var cancelled int
	w.s.db.QueryRow("SELECT COUNT(*) FROM deliveries WHERE state = 'cancelled'").Scan(&cancelled)
	if cancelled != 1 {
		t.Fatalf("deleting an endpoint cancels its unfinished deliveries, cancelled = %d", cancelled)
	}
	// Turning the first back on makes its waiting delivery claimable again.
	no := false
	w.s.UpdateEndpoint(ctx(), w.app.ID, e1.ID, EndpointUpdate{Disabled: &no}, t0)
	if c, _ := w.s.ClaimDue(ctx(), t0, time.Minute, "w", 10); len(c) != 1 || c[0].Endpoint.ID != e1.ID {
		t.Fatalf("a re-enabled endpoint resumes: %v", c)
	}
}

// Many workers asking at once must never be handed the same delivery. Each worker does what a real one does: it
// claims, then reports success, which frees the endpoint's slot for the next. Run with -race.
func TestNoDeliveryIsClaimedTwiceUnderContention(t *testing.T) {
	w := newWorld(t)
	const endpoints, messages = 25, 16
	for range endpoints {
		w.endpoint(t, "https://x.example/hook", nil, 4)
	}
	for range messages {
		w.send(t, "x", t0)
	}
	var (
		mu      sync.Mutex
		claimed = map[string]string{}
		wg      sync.WaitGroup
	)
	for i := range 12 {
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			idle := 0
			for idle < 3 {
				cs, err := w.s.ClaimDue(ctx(), t0, time.Hour, worker, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(cs) == 0 {
					idle++
					continue
				}
				idle = 0
				for _, c := range cs {
					mu.Lock()
					if prev, dup := claimed[c.Delivery.ID]; dup {
						t.Errorf("delivery %s claimed by %s and by %s", c.Delivery.ID, prev, worker)
					}
					claimed[c.Delivery.ID] = worker
					mu.Unlock()
					err := w.s.Complete(ctx(), Completion{DeliveryID: c.Delivery.ID, Owner: worker, Result: ResultSucceeded, Now: t0,
						Attempt: domain.Attempt{StartedAt: t0, StatusCode: 200, Trigger: domain.Scheduled}})
					if err != nil {
						t.Error(err)
						return
					}
				}
			}
		}(fmt.Sprintf("worker-%d", i))
	}
	wg.Wait()
	if len(claimed) != endpoints*messages {
		t.Fatalf("every delivery should have been handled exactly once, got %d of %d", len(claimed), endpoints*messages)
	}
	counts, _ := w.s.CountByState(ctx())
	if counts[domain.Succeeded] != endpoints*messages || counts[domain.Pending]+counts[domain.InFlight] != 0 {
		t.Fatalf("everything should be succeeded and nothing left over: %v", counts)
	}
}

func TestCompleteMovesTheDeliveryOn(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)

	run := func(result Result, att domain.Attempt, mutate func(*Completion)) domain.Delivery {
		_, ds := w.send(t, "x", t0)
		w.s.ClaimDue(ctx(), t0, time.Minute, "w", 1)
		c := Completion{DeliveryID: ds[0].ID, Owner: "w", Result: result, Attempt: att, Now: t0.Add(time.Second), NextAttemptAt: t0.Add(time.Hour)}
		if mutate != nil {
			mutate(&c)
		}
		if err := w.s.Complete(ctx(), c); err != nil {
			t.Fatal(err)
		}
		d, _ := w.s.GetDelivery(ctx(), w.app.ID, ds[0].ID)
		return d
	}

	ok := run(ResultSucceeded, domain.Attempt{StartedAt: t0, Duration: 120 * time.Millisecond, StatusCode: 200, Trigger: domain.Scheduled}, nil)
	if ok.State != domain.Succeeded || ok.FinishedAt == nil || ok.LastStatus != 200 {
		t.Fatalf("success: %+v", ok)
	}

	retry := run(ResultRetry, domain.Attempt{StartedAt: t0, StatusCode: 503, Failure: domain.FailServerError, ResponseSnippet: "overloaded", Trigger: domain.Scheduled}, nil)
	if retry.State != domain.Pending || !retry.NextAttemptAt.Equal(t0.Add(time.Hour)) || retry.FinishedAt != nil || retry.LastFailure != "http_5xx" {
		t.Fatalf("retry: %+v", retry)
	}

	failed := run(ResultFailed, domain.Attempt{StartedAt: t0, Failure: domain.FailTimeout, Trigger: domain.Scheduled}, nil)
	if failed.State != domain.Failed || failed.FinishedAt == nil || failed.LastFailure != "timeout" {
		t.Fatalf("failed: %+v", failed)
	}
}

func TestGoneDisablesTheEndpoint(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	w.s.ClaimDue(ctx(), t0, time.Minute, "w", 1)
	err := w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "w", Result: ResultFailed, Now: t0, DisableEndpoint: "the receiver answered 410 Gone",
		Attempt: domain.Attempt{StartedAt: t0, StatusCode: 410, Failure: domain.FailGone, Trigger: domain.Scheduled}})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := w.s.GetEndpoint(ctx(), w.app.ID, e.ID)
	if !got.Disabled() || got.DisabledReason != "the receiver answered 410 Gone" {
		t.Fatalf("the endpoint should be off: %+v", got)
	}
	_, later := w.send(t, "y", t0)
	if len(later) != 0 {
		t.Fatal("a disabled endpoint must not get new messages")
	}
}

func TestAttemptsAreListedInOrderWithTheirDetail(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	for i := range 3 {
		at := t0.Add(time.Duration(i) * time.Minute)
		w.s.ClaimDue(ctx(), at, 30*time.Second, "w", 1)
		w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "w", Result: ResultRetry, Now: at, NextAttemptAt: at.Add(time.Minute),
			Attempt: domain.Attempt{StartedAt: at, Duration: time.Duration(100*(i+1)) * time.Millisecond, StatusCode: 500 + i, Failure: domain.FailServerError, ResponseSnippet: fmt.Sprintf("try %d", i), Trigger: domain.Scheduled}})
	}
	as, err := w.s.ListAttempts(ctx(), w.app.ID, ds[0].ID)
	if err != nil || len(as) != 3 {
		t.Fatalf("%v %d", err, len(as))
	}
	for i, a := range as {
		if a.StatusCode != 500+i || a.ResponseSnippet != fmt.Sprintf("try %d", i) || a.Duration != time.Duration(100*(i+1))*time.Millisecond {
			t.Errorf("attempt %d: %+v", i, a)
		}
	}
	if _, err := w.s.ListAttempts(ctx(), "app_other", ds[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another app must not read these attempts, got %v", err)
	}
}

func TestReplayGivesAFreshStart(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	if _, err := w.s.Replay(ctx(), w.app.ID, ds[0].ID, t0); !errors.Is(err, ErrBadState) {
		t.Fatalf("a delivery that is still waiting cannot be replayed, got %v", err)
	}
	w.s.ClaimDue(ctx(), t0, time.Minute, "w", 1)
	w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "w", Result: ResultFailed, Now: t0, Attempt: domain.Attempt{StartedAt: t0, Failure: domain.FailTimeout, Trigger: domain.Scheduled}})

	at := t0.Add(time.Hour)
	d, err := w.s.Replay(ctx(), w.app.ID, ds[0].ID, at)
	if err != nil || d.State != domain.Pending || d.AttemptCount != 0 || d.FinishedAt != nil {
		t.Fatalf("replay: %+v %v", d, err)
	}
	claims, _ := w.s.ClaimDue(ctx(), at, time.Minute, "w", 1)
	if len(claims) != 1 || !claims[0].Manual || claims[0].Delivery.AttemptCount != 1 {
		t.Fatalf("a replay is claimed as a manual attempt with a fresh count: %+v", claims)
	}
	if _, err := w.s.Replay(ctx(), "app_other", ds[0].ID, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("another app cannot replay this delivery, got %v", err)
	}
}

func TestRecoverEndpointReenablesAndReplaysRecentFailures(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 5)
	fail := func(at time.Time) string {
		_, ds := w.send(t, "x", at)
		w.s.ClaimDue(ctx(), at, time.Minute, "w", 100)
		w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "w", Result: ResultFailed, Now: at, Attempt: domain.Attempt{StartedAt: at, Failure: domain.FailTimeout, Trigger: domain.Scheduled}})
		return ds[0].ID
	}
	old := fail(t0)
	recent1 := fail(t0.Add(48 * time.Hour))
	recent2 := fail(t0.Add(49 * time.Hour))
	yes := true
	w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{Disabled: &yes}, t0)

	n, err := w.s.RecoverEndpoint(ctx(), w.app.ID, e.ID, t0.Add(24*time.Hour), t0.Add(50*time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("two recent failures should be replayed, got %d %v", n, err)
	}
	got, _ := w.s.GetEndpoint(ctx(), w.app.ID, e.ID)
	if got.Disabled() {
		t.Fatal("recovering switches the endpoint back on")
	}
	states := map[string]domain.State{}
	for _, id := range []string{old, recent1, recent2} {
		d, _ := w.s.GetDelivery(ctx(), w.app.ID, id)
		states[id] = d.State
	}
	if states[old] != domain.Failed || states[recent1] != domain.Pending || states[recent2] != domain.Pending {
		t.Fatalf("only failures since the cut-off are replayed: %v", states)
	}
	if _, err := w.s.RecoverEndpoint(ctx(), w.app.ID, "ep_nothere00000000000000", t0, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestReleaseReturnsAnUnstartedClaimWithoutCountingIt(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	w.s.ClaimDue(ctx(), t0, time.Hour, "w", 1)
	if err := w.s.Release(ctx(), ds[0].ID, "w", t0); err != nil {
		t.Fatal(err)
	}
	d, _ := w.s.GetDelivery(ctx(), w.app.ID, ds[0].ID)
	if d.State != domain.Pending || d.AttemptCount != 0 {
		t.Fatalf("a released delivery is waiting again with no attempt used: %+v", d)
	}
	if c, _ := w.s.ClaimDue(ctx(), t0, time.Hour, "other", 1); len(c) != 1 {
		t.Fatal("another worker can take it at once")
	}
	// Releasing something you do not hold changes nothing.
	w.s.Release(ctx(), ds[0].ID, "stranger", t0)
	d, _ = w.s.GetDelivery(ctx(), w.app.ID, ds[0].ID)
	if d.State != domain.InFlight {
		t.Fatalf("a stranger cannot release someone else's delivery, state = %s", d.State)
	}
}

func TestEndpointUpdateAndSecretRotation(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", []string{"a"}, 1)

	newURL, desc, conc := "https://b.example/hook", "billing", 4
	var nilTypes *[]string
	types := &nilTypes
	got, err := w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{URL: &newURL, Description: &desc, Concurrency: &conc, EventTypes: types}, t0)
	if err != nil || got.URL != newURL || got.Description != "billing" || got.Concurrency != 4 || got.EventTypes != nil {
		t.Fatalf("update: %+v %v", got, err)
	}
	if !got.Wants("anything") {
		t.Fatal("clearing the filter means every event")
	}

	until := t0.Add(24 * time.Hour)
	rotated, err := w.s.RotateSecret(ctx(), w.app.ID, e.ID, "whsec_bmV3", until)
	if err != nil || rotated.Secret != "whsec_bmV3" || rotated.PrevSecret != "whsec_c2VjcmV0" {
		t.Fatalf("rotate: %+v %v", rotated, err)
	}
	if s := rotated.SigningSecrets(t0.Add(time.Hour)); len(s) != 2 {
		t.Fatalf("both secrets sign during the grace period: %v", s)
	}
	if s := rotated.SigningSecrets(t0.Add(25 * time.Hour)); len(s) != 1 || s[0] != "whsec_bmV3" {
		t.Fatalf("only the new secret signs afterwards: %v", s)
	}
	if _, err := w.s.UpdateEndpoint(ctx(), "app_other", e.ID, EndpointUpdate{URL: &newURL}, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an app cannot change another app's endpoint, got %v", err)
	}
}

func TestEndpointSecretsNeverAppearInJSON(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 1)
	b, _ := json.Marshal(e)
	if string(b) == "" || containsAny(string(b), "whsec_", "secret") {
		t.Fatalf("a secret leaked into JSON: %s", b)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

func TestAttemptJSONUsesWholeMilliseconds(t *testing.T) {
	b, _ := json.Marshal(domain.Attempt{Duration: 1234 * time.Millisecond, Trigger: domain.Scheduled})
	var m map[string]any
	json.Unmarshal(b, &m)
	if m["duration_ms"] != float64(1234) {
		t.Fatalf("duration_ms = %v in %s", m["duration_ms"], b)
	}
}

func TestMessagesAndDeliveriesAreScopedToTheirApp(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	m, ds := w.send(t, "x", t0)
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	if _, err := w.s.GetMessage(ctx(), other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := w.s.GetDelivery(ctx(), other.ID, ds[0].ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, err := w.s.ListDeliveriesForMessage(ctx(), other.ID, m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if _, _, err := w.s.ListDeliveriesForEndpoint(ctx(), other.ID, ds[0].EndpointID, "", Page{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestListingMessagesAndDeliveries(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 5)
	for i := range 7 {
		typ := "a"
		if i%2 == 1 {
			typ = "b"
		}
		w.send(t, typ, t0.Add(time.Duration(i)*time.Second))
	}
	all, next, err := w.s.ListMessages(ctx(), w.app.ID, "", Page{Limit: 4})
	if err != nil || len(all) != 4 || next == "" {
		t.Fatalf("first page: %d %q %v", len(all), next, err)
	}
	rest, next2, err := w.s.ListMessages(ctx(), w.app.ID, "", Page{Limit: 4, Cursor: next})
	if err != nil || len(rest) != 3 || next2 != "" {
		t.Fatalf("second page: %d %q %v", len(rest), next2, err)
	}
	onlyB, _, _ := w.s.ListMessages(ctx(), w.app.ID, "b", Page{})
	if len(onlyB) != 3 {
		t.Fatalf("filter by event type: %d", len(onlyB))
	}
	ds, _, err := w.s.ListDeliveriesForEndpoint(ctx(), w.app.ID, e.ID, domain.Pending, Page{})
	if err != nil || len(ds) != 7 {
		t.Fatalf("pending deliveries: %d %v", len(ds), err)
	}
	if ds2, _, _ := w.s.ListDeliveriesForEndpoint(ctx(), w.app.ID, e.ID, domain.Failed, Page{}); len(ds2) != 0 {
		t.Fatalf("no failed deliveries yet: %d", len(ds2))
	}
	counts, err := w.s.CountByState(ctx())
	if err != nil || counts[domain.Pending] != 7 {
		t.Fatalf("counts: %v %v", counts, err)
	}
}

func TestAnAttemptWithoutATriggerIsStillRecorded(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 1)
	_, ds := w.send(t, "x", t0)
	w.s.ClaimDue(ctx(), t0, time.Minute, "w", 1)
	err := w.s.Complete(ctx(), Completion{DeliveryID: ds[0].ID, Owner: "w", Result: ResultSucceeded, Now: t0,
		Attempt: domain.Attempt{StartedAt: t0, StatusCode: 200}})
	if err != nil {
		t.Fatalf("a missing trigger must not stop the result from being recorded: %v", err)
	}
	as, _ := w.s.ListAttempts(ctx(), w.app.ID, ds[0].ID)
	if len(as) != 1 || as[0].Trigger != domain.Scheduled {
		t.Fatalf("it should default to scheduled: %+v", as)
	}
	if d, _ := w.s.GetDelivery(ctx(), w.app.ID, ds[0].ID); d.State != domain.Succeeded {
		t.Fatalf("state = %s", d.State)
	}
}

func TestPermanentFailureFinishesTheOtherWaitingDeliveries(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 1)
	var ds []domain.Delivery
	for range 4 {
		_, d := w.send(t, "x", t0)
		ds = append(ds, d...)
	}
	// One delivery is tried and the receiver says it is gone for good.
	claimed, _ := w.s.ClaimDue(ctx(), t0, time.Minute, "w", 1)
	err := w.s.Complete(ctx(), Completion{DeliveryID: claimed[0].Delivery.ID, Owner: "w", Result: ResultFailed, Now: t0, DisableEndpoint: "gone",
		Attempt: domain.Attempt{StartedAt: t0, StatusCode: 410, Failure: domain.FailGone, Trigger: domain.Scheduled}})
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := w.s.CountByState(ctx())
	if counts[domain.Failed] != 4 || counts[domain.Pending] != 0 {
		t.Fatalf("all four should be finished as failed, none left waiting: %v", counts)
	}
	for _, d := range ds {
		got, _ := w.s.GetDelivery(ctx(), w.app.ID, d.ID)
		if got.ID != claimed[0].Delivery.ID && got.LastFailure != string(domain.FailEndpointDisabled) {
			t.Fatalf("a delivery finished because its endpoint was disabled says so: %+v", got)
		}
	}
	// Once the receiver is fixed, one call brings them all back.
	n, err := w.s.RecoverEndpoint(ctx(), w.app.ID, e.ID, t0.Add(-time.Hour), t0.Add(time.Minute))
	if err != nil || n != 4 {
		t.Fatalf("recover should replay all four, got %d %v", n, err)
	}
}

func TestSwitchingAnEndpointOffByHandIsAPauseNotAFailure(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 1)
	w.send(t, "x", t0)
	off := true
	w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{Disabled: &off}, t0)
	counts, _ := w.s.CountByState(ctx())
	if counts[domain.Pending] != 1 || counts[domain.Failed] != 0 {
		t.Fatalf("a paused endpoint keeps its waiting deliveries: %v", counts)
	}
}

func TestRecoverAlsoSendsWhatWasMissedWhileTheEndpointWasOff(t *testing.T) {
	w := newWorld(t)
	// An old message from before this endpoint existed must never be sent to it.
	w.send(t, "before.endpoint", t0.Add(-time.Hour))
	e := w.endpoint(t, "https://a.example/hook", []string{"invoice.paid", "invoice.refunded"}, 1)

	// The endpoint is switched off for good, as after a 410. Messages arriving now skip it.
	off := true
	w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{Disabled: &off}, t0)
	_, skipped := w.send(t, "invoice.paid", t0.Add(time.Minute))
	w.send(t, "invoice.refunded", t0.Add(2*time.Minute))
	w.send(t, "something.else", t0.Add(3*time.Minute)) // not a type this endpoint subscribes to
	if len(skipped) != 0 {
		t.Fatalf("a disabled endpoint gets no new deliveries: %v", skipped)
	}

	n, err := w.s.RecoverEndpoint(ctx(), w.app.ID, e.ID, t0, t0.Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("the two subscribed messages it missed should be created, got %d %v", n, err)
	}
	ds, _, _ := w.s.ListDeliveriesForEndpoint(ctx(), w.app.ID, e.ID, domain.Pending, Page{})
	if len(ds) != 2 {
		t.Fatalf("expected two pending deliveries, got %d", len(ds))
	}
	claims, _ := w.s.ClaimDue(ctx(), t0.Add(time.Hour), time.Minute, "w", 10)
	if len(claims) != 1 {
		t.Fatalf("a concurrency of 1 hands out one at a time, got %d", len(claims))
	}
	if !claims[0].Manual {
		t.Fatal("recovered deliveries are manual attempts")
	}

	// Recovering again must not make a second delivery for the same messages.
	again, err := w.s.RecoverEndpoint(ctx(), w.app.ID, e.ID, t0, t0.Add(2*time.Hour))
	if err != nil || again != 0 {
		t.Fatalf("nothing is missing the second time, got %d %v", again, err)
	}
	var total int
	w.s.db.QueryRow("SELECT COUNT(*) FROM deliveries").Scan(&total)
	if total != 2 {
		t.Fatalf("there must be exactly two deliveries, found %d", total)
	}
}

func TestRecoverRespectsTheCutoffForMissedMessages(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 1)
	off := true
	w.s.UpdateEndpoint(ctx(), w.app.ID, e.ID, EndpointUpdate{Disabled: &off}, t0)
	w.send(t, "x", t0.Add(time.Hour))
	w.send(t, "x", t0.Add(5*time.Hour))
	n, _ := w.s.RecoverEndpoint(ctx(), w.app.ID, e.ID, t0.Add(3*time.Hour), t0.Add(6*time.Hour))
	if n != 1 {
		t.Fatalf("only the message after the cutoff is recovered, got %d", n)
	}
}

func TestPageLimitIsKeptInRange(t *testing.T) {
	for in, want := range map[int]int{-5: 50, 0: 50, 1: 1, 100: 100, 250: 250, 251: 250, 99999: 250} {
		if got := (Page{Limit: in}).limit(); got != want {
			t.Errorf("limit %d became %d, want %d", in, got, want)
		}
	}
}

func TestListingEndpointsAndDeliveriesAcrossPages(t *testing.T) {
	w := newWorld(t)
	var created []string
	for i := range 5 {
		e, err := w.s.CreateEndpoint(ctx(), NewEndpoint{AppID: w.app.ID, URL: fmt.Sprintf("https://e%d.example/h", i), Secret: "whsec_c2VjcmV0"}, t0.Add(time.Duration(i)*time.Second))
		if err != nil {
			t.Fatal(err)
		}
		created = append(created, e.ID)
	}
	var seen []string
	cursor := ""
	for range 10 {
		page, next, err := w.s.ListEndpoints(ctx(), w.app.ID, Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page {
			seen = append(seen, e.ID)
		}
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 || seen[0] != created[4] || seen[4] != created[0] {
		t.Fatalf("pages should list all five, newest first: %v", seen)
	}
	if _, _, err := w.s.ListEndpoints(ctx(), w.app.ID, Page{Cursor: "!!"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("got %v", err)
	}

	// Deliveries to one endpoint, in pages.
	for i := range 5 {
		w.send(t, "x", t0.Add(time.Duration(i)*time.Second))
	}
	var dseen int
	cursor = ""
	for range 10 {
		page, next, err := w.s.ListDeliveriesForEndpoint(ctx(), w.app.ID, created[0], "", Page{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		dseen += len(page)
		if next == "" {
			break
		}
		cursor = next
	}
	if dseen != 5 {
		t.Fatalf("pages of deliveries should add up to 5, got %d", dseen)
	}
	if _, _, err := w.s.ListDeliveriesForEndpoint(ctx(), w.app.ID, created[0], "", Page{Cursor: "!!"}); !errors.Is(err, ErrBadCursor) {
		t.Fatalf("got %v", err)
	}
}

func TestDeletingSomeoneElsesEndpointFails(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 1)
	other, _ := w.s.CreateApp(ctx(), "other", nil, t0)
	if err := w.s.DeleteEndpoint(ctx(), other.ID, e.ID, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := w.s.DeleteEndpoint(ctx(), w.app.ID, e.ID, t0); err != nil {
		t.Fatal(err)
	}
	if err := w.s.DeleteEndpoint(ctx(), w.app.ID, e.ID, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleting twice: %v", err)
	}
	// The history of a deleted endpoint stays readable.
	if _, _, err := w.s.ListDeliveriesForEndpoint(ctx(), w.app.ID, e.ID, "", Page{}); err != nil {
		t.Fatalf("history of a deleted endpoint should still be readable: %v", err)
	}
}
