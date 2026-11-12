package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
)

func (w world) count(t *testing.T, table string) int {
	t.Helper()
	var n int
	if err := w.s.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// deliver claims and completes every due delivery at the given time, as a worker would.
func (w world) deliverAll(t *testing.T, at time.Time, result Result) {
	t.Helper()
	claims, err := w.s.ClaimDue(ctx(), at, time.Minute, "w", 1000)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range claims {
		err := w.s.Complete(ctx(), Completion{DeliveryID: c.Delivery.ID, Owner: "w", Result: result, Now: at, NextAttemptAt: at.Add(time.Hour),
			Attempt: domain.Attempt{StartedAt: at, StatusCode: 200, Trigger: domain.Scheduled}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPurgeRemovesOldFinishedMessagesWithTheirDeliveriesAndAttempts(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 5)
	w.endpoint(t, "https://b.example/hook", nil, 5)
	old := t0.Add(-40 * 24 * time.Hour)
	w.send(t, "old", old)
	w.deliverAll(t, old, ResultSucceeded)
	w.send(t, "new", t0)
	w.deliverAll(t, t0, ResultSucceeded)

	res, err := w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100)
	if err != nil {
		t.Fatal(err)
	}
	if res.Messages != 1 || res.Deliveries != 2 || res.Attempts != 2 {
		t.Fatalf("one old message, its two deliveries and two attempts: %+v", res)
	}
	if w.count(t, "messages") != 1 || w.count(t, "deliveries") != 2 || w.count(t, "attempts") != 2 {
		t.Fatalf("the recent message and everything of it must remain: %d %d %d", w.count(t, "messages"), w.count(t, "deliveries"), w.count(t, "attempts"))
	}
}

func TestPurgeKeepsAMessageThatStillHasWorkToDo(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 5)
	old := t0.Add(-40 * 24 * time.Hour)
	w.send(t, "waiting", old) // never delivered: still pending
	if res, _ := w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100); res.Messages != 0 {
		t.Fatalf("a message with a pending delivery must not be purged: %+v", res)
	}
	// In flight is unfinished too.
	w.s.ClaimDue(ctx(), t0, time.Hour, "w", 10)
	if res, _ := w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100); res.Messages != 0 {
		t.Fatalf("a message with a delivery in flight must not be purged: %+v", res)
	}
}

func TestPurgeKeepsAMessageWhoseDeliveryFinishedRecently(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 5)
	w.send(t, "late", t0.Add(-40*24*time.Hour))
	// The delivery was retried and only finished a day ago, long after the message was created.
	w.deliverAll(t, t0.Add(-24*time.Hour), ResultSucceeded)
	if res, _ := w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100); res.Messages != 0 {
		t.Fatalf("recent activity keeps the message: %+v", res)
	}
	// Once that too is older than the cutoff it goes.
	if res, _ := w.s.Purge(ctx(), t0.Add(time.Hour), 100); res.Messages != 1 {
		t.Fatalf("it goes once everything about it is old: %+v", res)
	}
}

func TestPurgeRemovesAnOldMessageThatNobodyWasSentTo(t *testing.T) {
	w := newWorld(t) // no endpoints, so the message has no deliveries
	w.send(t, "nobody", t0.Add(-40*24*time.Hour))
	if res, _ := w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100); res.Messages != 1 || res.Deliveries != 0 {
		t.Fatalf("%+v", res)
	}
}

func TestPurgeWorksInBatchesOldestFirst(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://a.example/hook", nil, 5)
	for i := range 25 {
		at := t0.Add(-time.Duration(60-i) * 24 * time.Hour)
		w.send(t, "x", at)
		w.deliverAll(t, at, ResultSucceeded)
	}
	cutoff := t0.Add(-40 * 24 * time.Hour) // messages 0..19 are older than this

	// The first batch takes the eight oldest, so the oldest one left is the ninth.
	if res, err := w.s.Purge(ctx(), cutoff, 8); err != nil || res.Messages != 8 {
		t.Fatalf("%+v %v", res, err)
	}
	var oldest int64
	w.s.db.QueryRow("SELECT MIN(created_at) FROM messages").Scan(&oldest)
	if want := ms(t0.Add(-time.Duration(60-8) * 24 * time.Hour)); oldest != want {
		t.Fatalf("a batch must remove the oldest first: oldest left is %d, want %d", oldest, want)
	}
	total := PurgeResult{Messages: 8, Deliveries: 8, Attempts: 8}
	calls := 1
	for {
		res, err := w.s.Purge(ctx(), cutoff, 8)
		if err != nil {
			t.Fatal(err)
		}
		calls++
		total = total.Add(res)
		if res.Messages < 8 {
			break
		}
	}
	if total.Messages != 20 || calls != 3 || w.count(t, "messages") != 5 {
		t.Fatalf("20 messages in batches of 8 take three calls: total %+v, calls %d, left %d", total, calls, w.count(t, "messages"))
	}
}

func TestPurgeRemovesAnOldDeletedEndpointOnlyWhenNothingRefersToIt(t *testing.T) {
	w := newWorld(t)
	e := w.endpoint(t, "https://a.example/hook", nil, 5)
	old := t0.Add(-40 * 24 * time.Hour)
	w.send(t, "x", old)
	w.deliverAll(t, old, ResultSucceeded)
	w.s.DeleteEndpoint(ctx(), w.app.ID, e.ID, old)

	cutoff := t0.Add(-30 * 24 * time.Hour)
	// The delivery history still refers to the endpoint in the first pass, which deletes the message and so the delivery.
	res, err := w.s.Purge(ctx(), cutoff, 100)
	if err != nil || res.Messages != 1 || res.Endpoints != 1 || w.count(t, "endpoints") != 0 {
		t.Fatalf("%+v %v endpoints left %d", res, err, w.count(t, "endpoints"))
	}
}

func TestPurgeKeepsALiveEndpointAndAnEndpointDeletedRecently(t *testing.T) {
	w := newWorld(t)
	w.endpoint(t, "https://live.example/hook", nil, 1)
	recent := w.endpoint(t, "https://gone.example/hook", nil, 1)
	w.s.DeleteEndpoint(ctx(), w.app.ID, recent.ID, t0.Add(-time.Hour))
	if res, _ := w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100); res.Endpoints != 0 || w.count(t, "endpoints") != 2 {
		t.Fatalf("%+v", res)
	}
}

func TestPurgeAllowsAnOldIdempotencyKeyToBeUsedAgain(t *testing.T) {
	w := newWorld(t)
	n := NewMessage{AppID: w.app.ID, EventType: "x", Payload: json.RawMessage(`{}`), IdempotencyKey: "order-1"}
	first, _, created, err := w.s.CreateMessage(ctx(), n, t0.Add(-40*24*time.Hour))
	if err != nil || !created {
		t.Fatal(err)
	}
	if _, _, again, _ := w.s.CreateMessage(ctx(), n, t0.Add(-40*24*time.Hour)); again {
		t.Fatal("setup: the repeat should be recognised")
	}
	w.s.Purge(ctx(), t0.Add(-30*24*time.Hour), 100)
	second, _, created, err := w.s.CreateMessage(ctx(), n, t0)
	if err != nil || !created || second.ID == first.ID {
		t.Fatalf("after the purge the key is new again: created=%v err=%v", created, err)
	}
}

func TestPurgeWithNothingToDo(t *testing.T) {
	w := newWorld(t)
	if res, err := w.s.Purge(ctx(), t0, 100); err != nil || res != (PurgeResult{}) {
		t.Fatalf("%+v %v", res, err)
	}
	if res, err := w.s.Purge(ctx(), t0, 0); err != nil || res != (PurgeResult{}) {
		t.Fatalf("a limit of zero does nothing: %+v %v", res, err)
	}
}
