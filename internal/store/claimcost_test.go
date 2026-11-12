package store

import (
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/id"
)

// seedBacklog adds messages, each with one delivery per endpoint, all due now. It writes them in one transaction,
// because going through CreateMessage thousands of times would make the test slow without making it any truer.
func seedBacklog(t *testing.T, w world, endpoints []string, messages int) {
	t.Helper()
	tx, err := w.s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	msg, err := tx.Prepare("INSERT INTO messages (id, app_id, event_type, payload, created_at) VALUES (?, ?, 'x', '{}', ?)")
	if err != nil {
		t.Fatal(err)
	}
	dlv, err := tx.Prepare("INSERT INTO deliveries (id, message_id, endpoint_id, app_id, state, next_attempt_at, created_at) VALUES (?, ?, ?, ?, 'pending', ?, ?)")
	if err != nil {
		t.Fatal(err)
	}
	for range messages {
		mid := id.New(id.Message)
		if _, err := msg.Exec(mid, w.app.ID, ms(t0)); err != nil {
			t.Fatal(err)
		}
		for _, ep := range endpoints {
			if _, err := dlv.Exec(id.New(id.Delivery), mid, ep, w.app.ID, ms(t0), ms(t0)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// TestClaimCostDoesNotGrowWithTheBacklog times one ClaimDue call while different numbers of deliveries are waiting. If the
// cost grew with the backlog, a service that fell behind would fall further behind. An earlier version ranked every due
// delivery on each claim: one claim took 2ms with 1,000 waiting and 190ms with 50,000.
func TestClaimCostDoesNotGrowWithTheBacklog(t *testing.T) {
	w := newWorld(t)
	var endpoints []string
	for range 10 {
		endpoints = append(endpoints, w.endpoint(t, "https://x.example/hook", nil, 20).ID)
	}
	measure := func() time.Duration {
		const calls = 20
		start := time.Now()
		for range calls {
			// A lease of zero length expires at once, so the same work stays ready and every call does the same job.
			if _, err := w.s.ClaimDue(ctx(), t0.Add(time.Hour), 0, "probe", 1); err != nil {
				t.Fatal(err)
			}
		}
		return time.Since(start) / calls
	}

	var costs []time.Duration
	seeded := 0
	for _, target := range []int{100, 1000, 3000} {
		seedBacklog(t, w, endpoints, target-seeded)
		seeded = target
		cost := measure()
		costs = append(costs, cost)
		t.Logf("%5d messages waiting (%6d deliveries): one claim takes %v", target, target*len(endpoints), cost)
	}
	if costs[2] > costs[0]*5+2*time.Millisecond {
		t.Errorf("a claim got %.1fx slower as the backlog grew 30x", float64(costs[2])/float64(costs[0]))
	}
}
