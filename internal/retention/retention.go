// Package retention deletes data Bowler no longer needs to keep. Message payloads often carry customer data, so the
// service does not hold them for ever by default.
package retention

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/sungwoonpk/bowler/internal/store"
)

// Config sets how long data is kept and how the clearing is paced.
type Config struct {
	Store *store.Store
	// Keep is how long finished data stays. Messages older than this, with nothing left to do, are deleted.
	Keep time.Duration
	// Every is how often to look for data to delete. Default 1 hour.
	Every time.Duration
	// Batch is how many messages are deleted in one transaction. Default 500.
	Batch int
	// Pause is the wait between two batches of one sweep, which leaves room for the queue to write. Default 50ms.
	Pause time.Duration
	Now   func() time.Time
	Log   *slog.Logger
}

// Stats counts what has been deleted since the program started.
type Stats struct {
	Sweeps     int64
	Messages   int64
	Deliveries int64
	Attempts   int64
	Endpoints  int64
	Errors     int64
}

// Janitor deletes old data on a schedule.
type Janitor struct {
	cfg                                                         Config
	sweeps, messages, deliveries, attempts, endpoints, failures atomic.Int64
}

// New checks the settings and returns a janitor.
func New(cfg Config) (*Janitor, error) {
	if cfg.Store == nil {
		return nil, errors.New("retention: a store is required")
	}
	if cfg.Keep <= 0 {
		return nil, errors.New("retention: Keep must be positive. To keep everything, do not start a janitor")
	}
	if cfg.Every <= 0 {
		cfg.Every = time.Hour
	}
	if cfg.Batch <= 0 {
		cfg.Batch = 500
	}
	if cfg.Pause <= 0 {
		cfg.Pause = 50 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Janitor{cfg: cfg}, nil
}

// Stats returns the counters.
func (j *Janitor) Stats() Stats {
	return Stats{
		Sweeps: j.sweeps.Load(), Messages: j.messages.Load(), Deliveries: j.deliveries.Load(),
		Attempts: j.attempts.Load(), Endpoints: j.endpoints.Load(), Errors: j.failures.Load(),
	}
}

// Run sweeps once at start and then every Every, until ctx ends.
func (j *Janitor) Run(ctx context.Context) {
	ticker := time.NewTicker(j.cfg.Every)
	defer ticker.Stop()
	for {
		if _, err := j.Sweep(ctx); err != nil && ctx.Err() == nil {
			j.cfg.Log.Error("retention sweep failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Sweep deletes everything that is past its retention, a batch at a time, and returns the total. It stops early if ctx
// ends or a batch fails. What was deleted before that stays deleted, since each batch is its own transaction.
func (j *Janitor) Sweep(ctx context.Context) (store.PurgeResult, error) {
	cutoff := j.cfg.Now().Add(-j.cfg.Keep)
	var total store.PurgeResult
	defer func() {
		j.sweeps.Add(1)
		if total != (store.PurgeResult{}) {
			j.cfg.Log.Info("retention removed old data", "messages", total.Messages, "deliveries", total.Deliveries,
				"attempts", total.Attempts, "endpoints", total.Endpoints, "older_than", cutoff.UTC().Format(time.RFC3339))
		}
	}()
	for {
		res, err := j.cfg.Store.Purge(ctx, cutoff, j.cfg.Batch)
		j.messages.Add(int64(res.Messages))
		j.deliveries.Add(int64(res.Deliveries))
		j.attempts.Add(int64(res.Attempts))
		j.endpoints.Add(int64(res.Endpoints))
		total = total.Add(res)
		if err != nil {
			j.failures.Add(1)
			return total, err
		}
		if res.Messages < j.cfg.Batch {
			return total, nil
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		case <-time.After(j.cfg.Pause):
		}
	}
}
