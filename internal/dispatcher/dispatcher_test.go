package dispatcher

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sungwoonpk/bowler/internal/deliver"
	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/retry"
)

// quick is a schedule that runs in milliseconds: three attempts after the first.
var quick = retry.Policy{Schedule: []time.Duration{0, 30 * time.Millisecond, 30 * time.Millisecond, 30 * time.Millisecond}}

type fakeSender func(ctx context.Context, c store.Claim) deliver.Outcome

func (f fakeSender) Send(ctx context.Context, c store.Claim) deliver.Outcome { return f(ctx, c) }

func ok(c store.Claim) deliver.Outcome {
	return deliver.Outcome{Verdict: deliver.Success, Attempt: domain.Attempt{DeliveryID: c.Delivery.ID, StartedAt: time.Now(), StatusCode: 200, Trigger: domain.Scheduled}}
}

func failing(c store.Claim, status int) deliver.Outcome {
	return deliver.Outcome{Verdict: deliver.Retry, Attempt: domain.Attempt{DeliveryID: c.Delivery.ID, StartedAt: time.Now(), StatusCode: status, Failure: domain.FailServerError, Trigger: domain.Scheduled}}
}

type env struct {
	s   *store.Store
	app domain.App
}

func newEnv(t *testing.T) env {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "bowler.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	app, err := s.CreateApp(context.Background(), "acme", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return env{s: s, app: app}
}

func (e env) endpoint(t *testing.T, concurrency int) domain.Endpoint {
	t.Helper()
	ep, err := e.s.CreateEndpoint(context.Background(), store.NewEndpoint{AppID: e.app.ID, URL: "https://example.com/hook", Concurrency: concurrency, Secret: "whsec_c2VjcmV0"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func (e env) send(t *testing.T) []domain.Delivery {
	t.Helper()
	_, ds, _, err := e.s.CreateMessage(context.Background(), store.NewMessage{AppID: e.app.ID, EventType: "x", Payload: json.RawMessage(`{}`)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func (e env) delivery(t *testing.T, id string) domain.Delivery {
	t.Helper()
	d, err := e.s.GetDelivery(context.Background(), e.app.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func (e env) attempts(t *testing.T, id string) []domain.Attempt {
	t.Helper()
	a, err := e.s.ListAttempts(context.Background(), e.app.ID, id)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// start runs a dispatcher until the test ends. It returns the dispatcher and a function that stops it and waits.
func start(t *testing.T, e env, sender Sender, cfg Config) (*Dispatcher, func()) {
	t.Helper()
	if cfg.Retry.Schedule == nil {
		cfg.Retry = quick
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Millisecond
	}
	if cfg.AttemptTimeout == 0 {
		cfg.AttemptTimeout = time.Second
		cfg.LeaseFor = 6 * time.Second
	}
	d, err := New(e.s, sender, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { d.Run(ctx); close(done) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("Run did not return")
		}
	}
	t.Cleanup(stop)
	return d, stop
}

func TestDeliversAndRecordsTheAttempt(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome { return ok(c) }), Config{})

	eventually(t, 3*time.Second, "delivery to succeed", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
	if as := e.attempts(t, ds[0].ID); len(as) != 1 || as[0].StatusCode != 200 {
		t.Fatalf("one successful attempt should be recorded: %+v", as)
	}
}

func TestRetriesUntilItWorks(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	var calls atomic.Int32
	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome {
		if calls.Add(1) < 3 {
			return failing(c, 503)
		}
		return ok(c)
	}), Config{})

	eventually(t, 3*time.Second, "delivery to succeed on the third try", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
	as := e.attempts(t, ds[0].ID)
	if len(as) != 3 || as[0].StatusCode != 503 || as[1].StatusCode != 503 || as[2].StatusCode != 200 {
		t.Fatalf("expected two failures and a success: %+v", as)
	}
	if got := e.delivery(t, ds[0].ID).AttemptCount; got != 3 {
		t.Fatalf("attempt_count = %d, want 3", got)
	}
}

func TestGivesUpWhenTheScheduleIsUsedUp(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome { return failing(c, 500) }), Config{})

	eventually(t, 3*time.Second, "delivery to fail for good", func() bool { return e.delivery(t, ds[0].ID).State == domain.Failed })
	if got := len(e.attempts(t, ds[0].ID)); got != quick.MaxAttempts() {
		t.Fatalf("it should try exactly %d times, tried %d", quick.MaxAttempts(), got)
	}
	time.Sleep(150 * time.Millisecond)
	if got := len(e.attempts(t, ds[0].ID)); got != quick.MaxAttempts() {
		t.Fatalf("a failed delivery must not be tried again, now %d attempts", got)
	}
}

func TestRetryAfterPushesTheNextAttemptBack(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	var calls atomic.Int32
	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome {
		if calls.Add(1) == 1 {
			o := failing(c, 429)
			o.RetryAfter = 400 * time.Millisecond
			return o
		}
		return ok(c)
	}), Config{})

	eventually(t, 4*time.Second, "delivery to succeed", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
	as := e.attempts(t, ds[0].ID)
	if gap := as[1].StartedAt.Sub(as[0].StartedAt); gap < 350*time.Millisecond {
		t.Fatalf("the receiver asked for 400ms and the next attempt came after %v", gap)
	}
}

func TestPermanentFailureSwitchesTheEndpointOff(t *testing.T) {
	e := newEnv(t)
	ep := e.endpoint(t, 1)
	ds := e.send(t)
	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome {
		return deliver.Outcome{Verdict: deliver.Fail, Disable: "the receiver answered 410 Gone",
			Attempt: domain.Attempt{DeliveryID: c.Delivery.ID, StartedAt: time.Now(), StatusCode: 410, Failure: domain.FailGone, Trigger: domain.Scheduled}}
	}), Config{})

	eventually(t, 3*time.Second, "delivery to fail", func() bool { return e.delivery(t, ds[0].ID).State == domain.Failed })
	got, _ := e.s.GetEndpoint(context.Background(), e.app.ID, ep.ID)
	if !got.Disabled() {
		t.Fatal("a 410 should switch the endpoint off")
	}
	if n := len(e.attempts(t, ds[0].ID)); n != 1 {
		t.Fatalf("it must not retry after a permanent failure, made %d attempts", n)
	}
}

func TestSlowEndpointDoesNotBlockAHealthyOne(t *testing.T) {
	e := newEnv(t)
	slow := e.endpoint(t, 1)
	healthy := e.endpoint(t, 4)
	const messages = 40
	var all []domain.Delivery
	for range messages {
		all = append(all, e.send(t)...)
	}

	gate := make(chan struct{})
	var slowInFlight, slowMax atomic.Int32
	start(t, e, fakeSender(func(ctx context.Context, c store.Claim) deliver.Outcome {
		if c.Endpoint.ID != slow.ID {
			return ok(c)
		}
		n := slowInFlight.Add(1)
		for {
			m := slowMax.Load()
			if n <= m || slowMax.CompareAndSwap(m, n) {
				break
			}
		}
		defer slowInFlight.Add(-1)
		select {
		case <-gate:
			return ok(c)
		case <-ctx.Done():
			return deliver.Outcome{Verdict: deliver.Abort}
		}
	}), Config{Workers: 8})

	healthyDone := func() int {
		n := 0
		for _, d := range all {
			if d.EndpointID == healthy.ID && e.delivery(t, d.ID).State == domain.Succeeded {
				n++
			}
		}
		return n
	}
	eventually(t, 5*time.Second, "every healthy delivery to finish while the slow endpoint is stuck", func() bool { return healthyDone() == messages })
	if slowMax.Load() != 1 {
		t.Fatalf("the slow endpoint may hold only its own concurrency (1), peak was %d", slowMax.Load())
	}
	close(gate)
	eventually(t, 10*time.Second, "the slow endpoint to catch up once it recovers", func() bool {
		n := 0
		for _, d := range all {
			if d.EndpointID == slow.ID && e.delivery(t, d.ID).State == domain.Succeeded {
				n++
			}
		}
		return n == messages
	})
}

func TestWorkersAreCapped(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 100)
	for range 30 {
		e.send(t)
	}
	gate := make(chan struct{})
	var running, peak atomic.Int32
	d, _ := start(t, e, fakeSender(func(ctx context.Context, c store.Claim) deliver.Outcome {
		n := running.Add(1)
		for {
			m := peak.Load()
			if n <= m || peak.CompareAndSwap(m, n) {
				break
			}
		}
		defer running.Add(-1)
		select {
		case <-gate:
		case <-ctx.Done():
		}
		return ok(c)
	}), Config{Workers: 3})

	eventually(t, 3*time.Second, "three workers to be busy", func() bool { return running.Load() == 3 })
	time.Sleep(100 * time.Millisecond)
	if peak.Load() != 3 || d.Stats().InFlight != 3 {
		t.Fatalf("no more than 3 attempts may run at once: peak=%d inflight=%d", peak.Load(), d.Stats().InFlight)
	}
	close(gate)
}

func TestACrashedWorkerLosesNothing(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)

	// A worker takes the delivery and dies without a word. Only the lease is left behind.
	claims, err := e.s.ClaimDue(context.Background(), time.Now(), 300*time.Millisecond, "doomed-worker", 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("setup: %v %v", claims, err)
	}

	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome { return ok(c) }), Config{Owner: "rescuer"})
	eventually(t, 5*time.Second, "the survivor to deliver after the lease runs out", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
	if got := e.delivery(t, ds[0].ID).AttemptCount; got != 2 {
		t.Fatalf("the lost attempt still counts, so attempt_count should be 2, got %d", got)
	}
}

func TestShutdownGivesBackWhatItCouldNotFinish(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	started := make(chan struct{})
	d, stop := start(t, e, fakeSender(func(ctx context.Context, c store.Claim) deliver.Outcome {
		close(started)
		<-ctx.Done() // never finishes on its own
		return deliver.Outcome{Verdict: deliver.Abort}
	}), Config{DrainTimeout: 100 * time.Millisecond})
	<-started
	stop()

	got := e.delivery(t, ds[0].ID)
	if got.State != domain.Pending || got.AttemptCount != 0 {
		t.Fatalf("an attempt cut off by shutdown is handed back and not counted: %+v", got)
	}
	if n := len(e.attempts(t, ds[0].ID)); n != 0 {
		t.Fatalf("no attempt should be recorded for one that never completed, got %d", n)
	}
	if d.Stats().Aborted != 1 {
		t.Fatalf("stats: %+v", d.Stats())
	}

	// Another dispatcher takes it at once, without waiting for any lease.
	start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome { return ok(c) }), Config{Owner: "next"})
	eventually(t, 3*time.Second, "the next dispatcher to deliver it", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
}

func TestShutdownWaitsForAttemptsThatAreAboutToFinish(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	started := make(chan struct{})
	_, stop := start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome {
		close(started)
		time.Sleep(250 * time.Millisecond)
		return ok(c)
	}), Config{DrainTimeout: 3 * time.Second})
	<-started
	stop()
	if got := e.delivery(t, ds[0].ID); got.State != domain.Succeeded {
		t.Fatalf("an attempt that finishes within the drain time is kept: %+v", got)
	}
}

func TestSeveralDispatchersShareOneQueueWithoutRepeats(t *testing.T) {
	e := newEnv(t)
	const endpoints, messages = 4, 60
	for range endpoints {
		e.endpoint(t, 3)
	}
	var all []domain.Delivery
	for range messages {
		all = append(all, e.send(t)...)
	}
	var (
		mu    sync.Mutex
		count = map[string]int{}
	)
	sender := fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome {
		mu.Lock()
		count[c.Delivery.ID]++
		mu.Unlock()
		time.Sleep(2 * time.Millisecond)
		return ok(c)
	})
	for i := range 3 {
		start(t, e, sender, Config{Owner: "d" + string(rune('a'+i)), Workers: 6})
	}
	eventually(t, 15*time.Second, "everything to be delivered", func() bool {
		counts, _ := e.s.CountByState(context.Background())
		return counts[domain.Succeeded] == len(all)
	})
	mu.Lock()
	defer mu.Unlock()
	for _, d := range all {
		if count[d.ID] != 1 {
			t.Fatalf("delivery %s was sent %d times, want exactly once", d.ID, count[d.ID])
		}
	}
}

func TestNotifyWakesTheLoop(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	d, _ := start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome { return ok(c) }), Config{PollInterval: time.Minute})
	time.Sleep(50 * time.Millisecond) // let the first, empty poll pass
	ds := e.send(t)
	d.Notify()
	eventually(t, 2*time.Second, "a notified dispatcher to deliver at once, not after the poll", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
}

func TestASenderThatPanicsDoesNotKillTheDispatcher(t *testing.T) {
	e := newEnv(t)
	e.endpoint(t, 1)
	ds := e.send(t)
	var calls atomic.Int32
	d, _ := start(t, e, fakeSender(func(_ context.Context, c store.Claim) deliver.Outcome {
		if calls.Add(1) == 1 {
			panic("boom")
		}
		return ok(c)
	}), Config{})
	eventually(t, 3*time.Second, "the delivery to succeed after the panic", func() bool { return e.delivery(t, ds[0].ID).State == domain.Succeeded })
	if d.Stats().Panics != 1 {
		t.Fatalf("stats: %+v", d.Stats())
	}
}

func TestLeaseMustOutlastAnAttempt(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "x.db"))
	defer s.Close()
	if _, err := New(s, fakeSender(nil), Config{AttemptTimeout: 30 * time.Second, LeaseFor: 31 * time.Second}); err == nil {
		t.Fatal("a lease that is not clearly longer than an attempt would hand a running attempt to a second worker")
	}
	if _, err := New(s, fakeSender(nil), Config{AttemptTimeout: 15 * time.Second, LeaseFor: 60 * time.Second}); err != nil {
		t.Fatalf("the defaults must be valid: %v", err)
	}
	if _, err := New(s, fakeSender(nil), Config{}); err != nil {
		t.Fatalf("an empty config must be valid: %v", err)
	}
}

func TestOwnersDifferByDefault(t *testing.T) {
	s, _ := store.Open(filepath.Join(t.TempDir(), "x.db"))
	defer s.Close()
	a, _ := New(s, fakeSender(nil), Config{})
	b, _ := New(s, fakeSender(nil), Config{})
	if a.cfg.Owner == b.cfg.Owner {
		t.Fatal("two dispatchers must not share an owner name, or one could finish the other's work")
	}
}
