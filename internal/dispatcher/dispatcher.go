// Package dispatcher runs the queue: it takes deliveries that are due, hands them to a sender, and records what
// happened.
//
// Several dispatchers can share one database, in one process or in several. The store's leases make sure each
// delivery is worked on by one of them at a time, and that a delivery whose worker has died is picked up by another.
package dispatcher

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sungwoonpk/bowler/internal/deliver"
	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/store"
	"github.com/sungwoonpk/bowler/retry"
)

// Sender makes one attempt. deliver.Sender is the real one, tests use a fake.
type Sender interface {
	Send(ctx context.Context, c store.Claim) deliver.Outcome
}

// Config controls a dispatcher. Zero values get defaults.
type Config struct {
	// Owner names this dispatcher in leases. It must be different for every running dispatcher. Default is random.
	Owner string
	// Workers is how many attempts may be in progress at once. Default 128.
	Workers int
	// ClaimBatch is the most deliveries taken from the queue in one go. Default 64.
	ClaimBatch int
	// PollInterval is how often to look for due work when nothing has woken the dispatcher. Default 250ms.
	PollInterval time.Duration
	// LeaseFor is how long a worker holds a delivery before others may take it. It must be longer than
	// AttemptTimeout, or a slow attempt would be handed to a second worker while the first still runs. Default 60s.
	LeaseFor time.Duration
	// AttemptTimeout is the longest one attempt takes, as set on the sender. It is used to check LeaseFor. Default 15s.
	AttemptTimeout time.Duration
	// DrainTimeout is how long shutdown waits for attempts in progress before cutting them off. Default 10s.
	DrainTimeout time.Duration
	// Retry is the retry schedule. Default retry.Default().
	Retry retry.Policy
	// Now and Rand are the clock and random source, replaceable for tests.
	Now  func() time.Time
	Rand func() float64
	Log  *slog.Logger
}

func (c *Config) setDefaults() {
	if c.Owner == "" {
		c.Owner = newOwner()
	}
	if c.Workers <= 0 {
		c.Workers = 128
	}
	if c.ClaimBatch <= 0 {
		c.ClaimBatch = 64
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 250 * time.Millisecond
	}
	if c.AttemptTimeout <= 0 {
		c.AttemptTimeout = 15 * time.Second
	}
	if c.LeaseFor <= 0 {
		c.LeaseFor = 60 * time.Second
	}
	if c.DrainTimeout <= 0 {
		c.DrainTimeout = 10 * time.Second
	}
	if c.Retry.Schedule == nil {
		c.Retry = retry.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Rand == nil {
		c.Rand = rand.Float64
	}
	if c.Log == nil {
		c.Log = slog.New(slog.DiscardHandler)
	}
}

// newOwner makes a name for a dispatcher. It has to be different for every dispatcher that ever shares a database,
// across processes and machines, so it comes from the system's secure randomness: 64 bits is enough that two will not
// collide, which a seeded generator with 32 bits would not guarantee.
func newOwner() string {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		panic("dispatcher: cannot read randomness: " + err.Error())
	}
	return "dispatcher-" + hex.EncodeToString(b[:])
}

// Stats are running totals, for metrics.
type Stats struct {
	InFlight   int64
	Claimed    uint64
	Succeeded  uint64
	Retried    uint64
	Failed     uint64
	Aborted    uint64
	LeaseLost  uint64
	StoreError uint64
	Panics     uint64
}

// Dispatcher runs the queue.
type Dispatcher struct {
	store  *store.Store
	sender Sender
	cfg    Config

	wake chan struct{} // new work may be ready
	done chan struct{} // an attempt finished, so a worker slot is free

	inFlight                                     atomic.Int64
	claimed, succeeded, retried, failed, aborted atomic.Uint64
	leaseLost, storeErrors, panics               atomic.Uint64
}

// New builds a dispatcher. It returns an error when the configuration cannot work.
func New(s *store.Store, sender Sender, cfg Config) (*Dispatcher, error) {
	cfg.setDefaults()
	if cfg.LeaseFor < cfg.AttemptTimeout+5*time.Second {
		return nil, fmt.Errorf("dispatcher: the lease (%v) must be at least 5s longer than an attempt may take (%v), "+
			"or a slow attempt would be given to a second worker while it is still running", cfg.LeaseFor, cfg.AttemptTimeout)
	}
	return &Dispatcher{store: s, sender: sender, cfg: cfg, wake: make(chan struct{}, 1), done: make(chan struct{}, 1)}, nil
}

// Notify tells the dispatcher that new work may be ready, so it need not wait for the next poll. It never blocks.
func (d *Dispatcher) Notify() {
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// Stats returns the running totals.
func (d *Dispatcher) Stats() Stats {
	return Stats{
		InFlight: d.inFlight.Load(), Claimed: d.claimed.Load(), Succeeded: d.succeeded.Load(), Retried: d.retried.Load(),
		Failed: d.failed.Load(), Aborted: d.aborted.Load(), LeaseLost: d.leaseLost.Load(),
		StoreError: d.storeErrors.Load(), Panics: d.panics.Load(),
	}
}

// Run works the queue until ctx ends, then shuts down in an orderly way: it stops taking new work, gives attempts
// already in progress DrainTimeout to finish, and hands back any it had to cut off so another worker can take them
// at once, without counting the interrupted attempt.
func (d *Dispatcher) Run(ctx context.Context) error {
	// Attempts run under their own context, which outlives ctx by the drain time, so a shutdown does not abandon
	// attempts that are about to finish.
	work, cancelWork := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelWork()
	var wg sync.WaitGroup

	timer := time.NewTimer(0)
	defer timer.Stop()
loop:
	for {
		for ctx.Err() == nil {
			free := d.cfg.Workers - int(d.inFlight.Load())
			if free <= 0 {
				break
			}
			batch := min(free, d.cfg.ClaimBatch)
			claims, err := d.store.ClaimDue(ctx, d.cfg.Now(), d.cfg.LeaseFor, d.cfg.Owner, batch)
			if err != nil {
				if ctx.Err() == nil {
					d.storeErrors.Add(1)
					d.cfg.Log.Error("claim failed", "error", err)
				}
				break
			}
			for _, c := range claims {
				d.claimed.Add(1)
				d.inFlight.Add(1)
				wg.Add(1)
				go func() {
					defer wg.Done()
					defer func() { d.inFlight.Add(-1); d.signalDone() }()
					d.handle(work, c)
				}()
			}
			// A full batch means there may be more waiting, so ask again straight away.
			if len(claims) < batch {
				break
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d.cfg.PollInterval)
		select {
		case <-ctx.Done():
			break loop
		case <-d.wake:
		case <-d.done:
		case <-timer.C:
		}
	}

	// Orderly shutdown.
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(d.cfg.DrainTimeout):
		d.cfg.Log.Warn("drain time is over, cutting off attempts in progress", "in_flight", d.inFlight.Load())
		cancelWork()
		<-finished
	}
	return nil
}

func (d *Dispatcher) signalDone() {
	select {
	case d.done <- struct{}{}:
	default:
	}
}

// handle makes one attempt and records the result.
func (d *Dispatcher) handle(ctx context.Context, c store.Claim) {
	var out deliver.Outcome
	func() {
		// A bug in the sender must not take the whole process down, and must not leave the delivery stuck.
		defer func() {
			if r := recover(); r != nil {
				d.panics.Add(1)
				d.cfg.Log.Error("sender panicked", "delivery", c.Delivery.ID, "panic", fmt.Sprint(r))
				trigger := domain.Scheduled
				if c.Manual {
					trigger = domain.Manual
				}
				out = deliver.Outcome{Verdict: deliver.Retry, Attempt: domain.Attempt{
					DeliveryID: c.Delivery.ID, StartedAt: d.cfg.Now(), Failure: domain.FailConnection,
					ResponseSnippet: "internal error", Trigger: trigger}}
			}
		}()
		out = d.sender.Send(ctx, c)
	}()

	// Recording the result must succeed even while shutting down, so it does not use the cancellable context.
	rec, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	now := d.cfg.Now()

	if out.Verdict == deliver.Abort {
		d.aborted.Add(1)
		if err := d.store.Release(rec, c.Delivery.ID, d.cfg.Owner, now); err != nil {
			d.storeErrors.Add(1)
			d.cfg.Log.Error("release failed", "delivery", c.Delivery.ID, "error", err)
		}
		return
	}

	comp := store.Completion{DeliveryID: c.Delivery.ID, Owner: d.cfg.Owner, Attempt: out.Attempt, Now: now}
	switch out.Verdict {
	case deliver.Success:
		comp.Result = store.ResultSucceeded
		d.succeeded.Add(1)
	case deliver.Fail:
		comp.Result = store.ResultFailed
		comp.DisableEndpoint = out.Disable
		d.failed.Add(1)
	default: // deliver.Retry
		wait, ok := d.cfg.Retry.Next(c.Delivery.AttemptCount, d.cfg.Rand, out.RetryAfter)
		if ok {
			comp.Result = store.ResultRetry
			comp.NextAttemptAt = now.Add(wait)
			d.retried.Add(1)
		} else {
			comp.Result = store.ResultFailed
			d.failed.Add(1)
		}
	}

	if err := d.store.Complete(rec, comp); err != nil {
		if errors.Is(err, store.ErrLeaseLost) {
			d.leaseLost.Add(1)
			d.cfg.Log.Warn("lease was lost before the result could be recorded", "delivery", c.Delivery.ID)
			return
		}
		d.storeErrors.Add(1)
		d.cfg.Log.Error("recording the attempt failed", "delivery", c.Delivery.ID, "error", err)
	}
}
