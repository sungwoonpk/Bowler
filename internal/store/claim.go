package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

// dueSQL picks the deliveries that are ready, taking no more from one endpoint than it has free slots for.
//
// This is what keeps one slow receiver from starving the rest. Without the per-endpoint limit, an endpoint with a
// thousand deliveries waiting would fill every worker, and a healthy endpoint behind it would wait its turn. With
// it, each endpoint can occupy at most its own concurrency, and the remaining workers go to everyone else.
//
// A delivery is ready when it is pending and its time has come, or when it is in flight but the lease on it has
// run out, which means the worker that held it died or stalled.
const dueSQL = `
WITH busy AS (
    SELECT endpoint_id, COUNT(*) AS n
    FROM deliveries
    WHERE state = 'inflight' AND lease_until > ?1
    GROUP BY endpoint_id
), due AS (
    SELECT d.id, d.endpoint_id, d.next_attempt_at, e.concurrency,
           ROW_NUMBER() OVER (PARTITION BY d.endpoint_id ORDER BY d.next_attempt_at, d.id) AS rank
    FROM deliveries d
    JOIN endpoints e ON e.id = d.endpoint_id
    WHERE e.deleted_at IS NULL AND e.disabled_at IS NULL
      AND ((d.state = 'pending' AND d.next_attempt_at <= ?1)
        OR (d.state = 'inflight' AND d.lease_until <= ?1))
)
SELECT due.id
FROM due
LEFT JOIN busy ON busy.endpoint_id = due.endpoint_id
WHERE due.rank <= due.concurrency - COALESCE(busy.n, 0)
ORDER BY due.next_attempt_at, due.id
LIMIT ?2`

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
		rows, err := tx.QueryContext(ctx, dueSQL, ms(now), limit)
		if err != nil {
			return err
		}
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
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

		marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		args := make([]any, len(ids))
		for i, id := range ids {
			args[i] = id
		}
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
	return claims, err
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
	now := c.Now.UTC().Truncate(time.Millisecond)
	lost := false
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		a := c.Attempt
		_, err := tx.ExecContext(ctx,
			`INSERT INTO attempts (id, delivery_id, started_at, duration_ms, status_code, failure, response_snippet, trigger)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			id.New(id.Attempt), c.DeliveryID, ms(a.StartedAt), a.Duration.Milliseconds(),
			sql.NullInt64{Int64: int64(a.StatusCode), Valid: a.StatusCode != 0},
			sql.NullString{String: string(a.Failure), Valid: a.Failure != ""},
			sql.NullString{String: a.ResponseSnippet, Valid: a.ResponseSnippet != ""},
			string(a.Trigger))
		if err != nil {
			return err
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
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			lost = true
			return nil
		}
		if c.DisableEndpoint != "" {
			_, err = tx.ExecContext(ctx,
				`UPDATE endpoints SET disabled_at = ?, disabled_reason = ?
				 WHERE id = (SELECT endpoint_id FROM deliveries WHERE id = ?) AND disabled_at IS NULL`,
				ms(now), c.DisableEndpoint, c.DeliveryID)
		}
		return err
	})
	if err != nil {
		return err
	}
	if lost {
		return ErrLeaseLost
	}
	return nil
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
