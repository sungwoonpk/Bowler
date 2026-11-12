package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
)

// Claim is a delivery a worker has been given, with everything needed to make the attempt.
type Claim struct {
	Delivery domain.Delivery
	Endpoint domain.Endpoint
	Message  domain.Message
	// Manual is true when a person asked for this attempt, as opposed to the retry schedule.
	Manual bool
}

// A delivery is ready when it is pending and its time has come, or when it is in flight but the lease on it has run
// out, which means the worker that held it died or stalled.
//
// Choosing work is where fairness is decided. Each endpoint may have at most its concurrency in flight, and a claim takes
// only as many from an endpoint as it has free slots. Without that, an endpoint with a thousand deliveries waiting would
// fill every worker, and a healthy endpoint behind it would wait its turn.
//
// The work is found one endpoint at a time, through indexes that start with the endpoint. An earlier version ranked every
// due delivery with a window function, which was simple and read all of them on every claim: with 50,000 waiting, one
// claim took 190ms, and a service that fell behind fell further behind. This version reads about as many rows as it
// returns. TestClaimCostDoesNotGrowWithTheBacklog guards it.

// endpointsWithWorkSQL lists the endpoints that have something ready, the ones waiting longest first.
const endpointsWithWorkSQL = `
SELECT e.id, e.concurrency
FROM endpoints e
WHERE e.deleted_at IS NULL AND e.disabled_at IS NULL
  AND (EXISTS (SELECT 1 FROM deliveries d WHERE d.endpoint_id = e.id AND d.state = 'pending'  AND d.next_attempt_at <= ?1)
    OR EXISTS (SELECT 1 FROM deliveries d WHERE d.endpoint_id = e.id AND d.state = 'inflight' AND d.lease_until     <= ?1))
ORDER BY (SELECT MIN(d.next_attempt_at) FROM deliveries d WHERE d.endpoint_id = e.id AND d.state = 'pending' AND d.next_attempt_at <= ?1), e.id`

// busySQL counts the deliveries of one endpoint that a worker is working on right now.
const busySQL = `SELECT COUNT(*) FROM deliveries WHERE endpoint_id = ?1 AND state = 'inflight' AND lease_until > ?2`

// readySQL takes the oldest ready deliveries of one endpoint, at most ?3 of them.
const readySQL = `
SELECT id FROM (
    SELECT id, due FROM (SELECT id, next_attempt_at AS due FROM deliveries
                         WHERE endpoint_id = ?1 AND state = 'pending' AND next_attempt_at <= ?2
                         ORDER BY next_attempt_at, id LIMIT ?3)
    UNION ALL
    SELECT id, due FROM (SELECT id, lease_until AS due FROM deliveries
                         WHERE endpoint_id = ?1 AND state = 'inflight' AND lease_until <= ?2
                         ORDER BY lease_until, id LIMIT ?3)
)
ORDER BY due, id
LIMIT ?3`

// readyIDs picks up to limit ready deliveries, taking no more from one endpoint than it has free slots.
func readyIDs(ctx context.Context, tx *sql.Tx, now time.Time, limit int) ([]string, error) {
	rows, err := tx.QueryContext(ctx, endpointsWithWorkSQL, ms(now))
	if err != nil {
		return nil, err
	}
	type endpointSlots struct {
		id          string
		concurrency int
	}
	endpoints, err := collect(rows, func(r scanner) (endpointSlots, error) {
		var e endpointSlots
		return e, r.Scan(&e.id, &e.concurrency)
	})
	if err != nil {
		return nil, err
	}

	var ids []string
	for _, e := range endpoints {
		if len(ids) >= limit {
			break
		}
		var busy int
		if err := tx.QueryRowContext(ctx, busySQL, e.id, ms(now)).Scan(&busy); err != nil {
			return nil, err
		}
		free := e.concurrency - busy
		if free <= 0 {
			continue
		}
		rows, err := tx.QueryContext(ctx, readySQL, e.id, ms(now), min(free, limit-len(ids)))
		if err != nil {
			return nil, err
		}
		got, err := collect(rows, func(r scanner) (string, error) {
			var id string
			return id, r.Scan(&id)
		})
		if err != nil {
			return nil, err
		}
		ids = append(ids, got...)
	}
	return ids, nil
}

// ClaimDue hands up to limit ready deliveries to owner, each under a lease that lasts leaseFor.
//
// While the lease holds, no other worker is given the delivery. If the worker dies, the lease runs out and the
// delivery becomes ready again, so it is tried by someone else. That is what makes delivery at-least-once: a
// crash can cause a repeat, never a loss.
//
// The attempt is counted when it is claimed, not when it ends. If a worker dies mid-attempt, the attempt still
// counts, so a delivery that crashes its worker every time cannot be retried forever.
func (s *Store) ClaimDue(ctx context.Context, now time.Time, leaseFor time.Duration, owner string, limit int) ([]Claim, error) {
	if limit < 1 {
		return nil, nil
	}
	var claims []Claim
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ids, err := readyIDs(ctx, tx, now, limit)
		if err != nil {
			return err
		}
		if len(ids) == 0 {
			return nil
		}

		lease := ms(now.Add(leaseFor))
		for _, id := range ids {
			if _, err := tx.ExecContext(ctx,
				`UPDATE deliveries SET state = 'inflight', lease_until = ?, lease_owner = ?, attempt_count = attempt_count + 1
				 WHERE id = ?`, lease, owner, id); err != nil {
				return err
			}
		}

		// The only thing built into the SQL text is a run of "?" placeholders. Every value is passed as an argument.
		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
		// #nosec G202 -- only "?" placeholders are joined into the text, see above. The ids are bound as arguments.
		joined, err := tx.QueryContext(ctx, `
			SELECT d.id, d.message_id, d.endpoint_id, d.app_id, d.state, d.attempt_count, d.next_attempt_at,
			       d.last_status, d.last_failure, d.created_at, d.finished_at, d.manual,
			       e.id, e.app_id, e.url, e.description, e.secret, e.prev_secret, e.prev_secret_expires_at, e.event_types,
			       e.concurrency, e.disabled_at, e.disabled_reason, e.created_at,
			       m.id, m.app_id, m.event_type, m.payload, m.idempotency_key, m.created_at
			FROM deliveries d
			JOIN endpoints e ON e.id = d.endpoint_id
			JOIN messages m ON m.id = d.message_id
			WHERE d.id IN (`+marks+`)
			ORDER BY d.next_attempt_at, d.id`, args...)
		if err != nil {
			return err
		}
		defer joined.Close()
		for joined.Next() {
			c, err := scanClaim(joined)
			if err != nil {
				return err
			}
			claims = append(claims, c)
		}
		return joined.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("store: claim due deliveries: %w", err)
	}
	return claims, nil
}

func scanClaim(r scanner) (Claim, error) {
	var (
		d                       domain.Delivery
		state                   string
		next, dCreated          int64
		lastStatus, finished    sql.NullInt64
		lastFailure             sql.NullString
		manual                  int
		e                       domain.Endpoint
		prev, types, reason     sql.NullString
		prevExpires, disabledAt sql.NullInt64
		eCreated                int64
		m                       domain.Message
		payload                 string
		key                     sql.NullString
		mCreated                int64
	)
	if err := r.Scan(&d.ID, &d.MessageID, &d.EndpointID, &d.AppID, &state, &d.AttemptCount, &next,
		&lastStatus, &lastFailure, &dCreated, &finished, &manual,
		&e.ID, &e.AppID, &e.URL, &e.Description, &e.Secret, &prev, &prevExpires, &types,
		&e.Concurrency, &disabledAt, &reason, &eCreated,
		&m.ID, &m.AppID, &m.EventType, &payload, &key, &mCreated); err != nil {
		return Claim{}, err
	}
	d.State = domain.State(state)
	d.NextAttemptAt = fromMS(next)
	d.LastStatus = int(lastStatus.Int64)
	d.LastFailure = lastFailure.String
	d.CreatedAt = fromMS(dCreated)
	d.FinishedAt = optMS(finished)

	e.PrevSecret = prev.String
	e.PrevSecretExpiresAt = optMS(prevExpires)
	e.DisabledAt = optMS(disabledAt)
	e.DisabledReason = reason.String
	e.CreatedAt = fromMS(eCreated)
	if types.Valid {
		if err := json.Unmarshal([]byte(types.String), &e.EventTypes); err != nil {
			return Claim{}, err
		}
	}

	m.Payload = json.RawMessage(payload)
	m.IdempotencyKey = key.String
	m.CreatedAt = fromMS(mCreated)
	return Claim{Delivery: d, Endpoint: e, Message: m, Manual: manual == 1}, nil
}

// Result is how an attempt ended, as far as the queue is concerned.
type Result int

const (
	// ResultSucceeded: the receiver accepted it. The delivery is done.
	ResultSucceeded Result = iota + 1
	// ResultRetry: it failed and will be tried again at NextAttemptAt.
	ResultRetry
	// ResultFailed: it failed and no more attempts will be made.
	ResultFailed
)

// Completion reports the outcome of an attempt.
type Completion struct {
	DeliveryID string
	// Owner must be the worker that claimed the delivery. A worker whose lease ran out and was given to another
	// cannot overwrite the new holder's result.
	Owner   string
	Attempt domain.Attempt
	Result  Result
	// NextAttemptAt is when to try again. It is used only for ResultRetry.
	NextAttemptAt time.Time
	// DisableEndpoint, when not empty, switches the endpoint off for this reason.
	DisableEndpoint string
	Now             time.Time
}

// Complete records an attempt and moves the delivery on.
//
// The attempt is always kept, even if the lease was lost, because it really happened and a customer looking at the
// history should see it. In that case Complete returns ErrLeaseLost and leaves the delivery's state alone.
func (s *Store) Complete(ctx context.Context, c Completion) error {
	return s.CompleteBatch(ctx, []Completion{c})[0]
}

// CompleteBatch records several results in one transaction and returns one error per result, in the same order, nil for
// each that was recorded.
//
// Every result used to be its own transaction, and the number of write transactions a second is what limits a single
// SQLite writer. Recording many in one makes the cost of the commit shared.
//
// Each result runs inside its own savepoint, so one that fails (a bad value, say) is undone alone and the others are
// kept. Without that, one bad result would roll back all the rest, and every delivery in the batch would wait for its
// lease to run out.
func (s *Store) CompleteBatch(ctx context.Context, batch []Completion) []error {
	errs := make([]error, len(batch))
	if len(batch) == 0 {
		return errs
	}
	txErr := s.withTx(ctx, func(tx *sql.Tx) error {
		for i, c := range batch {
			if _, err := tx.ExecContext(ctx, "SAVEPOINT result"); err != nil {
				return err
			}
			lost, err := completeOne(ctx, tx, c)
			switch {
			case err != nil:
				errs[i] = fmt.Errorf("store: complete delivery %s: %w", c.DeliveryID, err)
				if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO result"); rerr != nil {
					return rerr
				}
			case lost:
				errs[i] = ErrLeaseLost
			}
			if _, err := tx.ExecContext(ctx, "RELEASE result"); err != nil {
				return err
			}
		}
		return nil
	})
	if txErr != nil {
		// The transaction itself failed, so nothing was recorded and every result shares the cause.
		for i, c := range batch {
			errs[i] = fmt.Errorf("store: complete delivery %s: %w", c.DeliveryID, txErr)
		}
	}
	return errs
}

// completeOne records one result inside a transaction. lost is true when the delivery's lease had been given away, in
// which case only the attempt is kept.
func completeOne(ctx context.Context, tx *sql.Tx, c Completion) (lost bool, err error) {
	now := c.Now.UTC().Truncate(time.Millisecond)
	a := c.Attempt
	// An attempt without a trigger would break a constraint and leave the delivery stuck until its lease ran out.
	// Scheduled is the safe default.
	if a.Trigger == "" {
		a.Trigger = domain.Scheduled
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO attempts (id, delivery_id, started_at, duration_ms, status_code, failure, response_snippet, trigger)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		id.New(id.Attempt), c.DeliveryID, ms(a.StartedAt), a.Duration.Milliseconds(),
		sql.NullInt64{Int64: int64(a.StatusCode), Valid: a.StatusCode != 0},
		sql.NullString{String: string(a.Failure), Valid: a.Failure != ""},
		sql.NullString{String: a.ResponseSnippet, Valid: a.ResponseSnippet != ""},
		string(a.Trigger)); err != nil {
		return false, err
	}

	var (
		state  domain.State
		finish sql.NullInt64
		next   = ms(now)
	)
	switch c.Result {
	case ResultSucceeded:
		state, finish = domain.Succeeded, sql.NullInt64{Int64: ms(now), Valid: true}
	case ResultFailed:
		state, finish = domain.Failed, sql.NullInt64{Int64: ms(now), Valid: true}
	default:
		state, next = domain.Pending, ms(c.NextAttemptAt)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE deliveries
		 SET state = ?, next_attempt_at = ?, finished_at = ?, lease_until = NULL, lease_owner = NULL, manual = 0,
		     last_status = ?, last_failure = ?
		 WHERE id = ? AND state = 'inflight' AND lease_owner = ?`,
		string(state), next, finish,
		sql.NullInt64{Int64: int64(a.StatusCode), Valid: a.StatusCode != 0},
		sql.NullString{String: string(a.Failure), Valid: a.Failure != ""},
		c.DeliveryID, c.Owner)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return true, nil
	}
	if c.DisableEndpoint == "" {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE endpoints SET disabled_at = ?, disabled_reason = ?
		 WHERE id = (SELECT endpoint_id FROM deliveries WHERE id = ?) AND disabled_at IS NULL`,
		ms(now), c.DisableEndpoint, c.DeliveryID); err != nil {
		return false, err
	}
	// The endpoint was switched off because the receiver refused for good, so the deliveries still waiting for it are
	// finished too, as failed. Left pending they would never end and never show up as something to recover.
	// RecoverEndpoint replays them once the receiver is fixed. An endpoint a person switched off by hand is different:
	// that is a pause, and its deliveries wait.
	_, err = tx.ExecContext(ctx,
		`UPDATE deliveries SET state = 'failed', finished_at = ?, last_failure = ?, lease_until = NULL, lease_owner = NULL, manual = 0
		 WHERE endpoint_id = (SELECT endpoint_id FROM deliveries WHERE id = ?) AND state = 'pending'`,
		ms(now), string(domain.FailEndpointDisabled), c.DeliveryID)
	return false, err
}

// Release hands a claimed delivery back without counting an attempt. A worker that is shutting down uses it for
// deliveries it was given but has not started, so they go to someone else straight away instead of waiting for
// the lease to run out.
func (s *Store) Release(ctx context.Context, deliveryID, owner string, now time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE deliveries
		 SET state = 'pending', next_attempt_at = ?, attempt_count = attempt_count - 1, lease_until = NULL, lease_owner = NULL
		 WHERE id = ? AND state = 'inflight' AND lease_owner = ?`, ms(now), deliveryID, owner)
	return err
}
