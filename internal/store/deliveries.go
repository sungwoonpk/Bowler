package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
)

const deliveryColumns = `id, message_id, endpoint_id, app_id, state, attempt_count, next_attempt_at,
	last_status, last_failure, created_at, finished_at`

func scanDelivery(r scanner) (domain.Delivery, error) {
	var (
		d              domain.Delivery
		state          string
		next, created  int64
		status, finish sql.NullInt64
		failure        sql.NullString
	)
	if err := r.Scan(&d.ID, &d.MessageID, &d.EndpointID, &d.AppID, &state, &d.AttemptCount, &next,
		&status, &failure, &created, &finish); err != nil {
		return domain.Delivery{}, err
	}
	d.State = domain.State(state)
	d.NextAttemptAt = fromMS(next)
	d.LastStatus = int(status.Int64)
	d.LastFailure = failure.String
	d.CreatedAt = fromMS(created)
	d.FinishedAt = optMS(finish)
	return d, nil
}

type querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func deliveriesForMessage(ctx context.Context, q querier, appID, messageID string) ([]domain.Delivery, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT "+deliveryColumns+" FROM deliveries WHERE app_id = ? AND message_id = ? ORDER BY id", appID, messageID)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanDelivery)
}

// ListDeliveriesForMessage returns every delivery of one message.
func (s *Store) ListDeliveriesForMessage(ctx context.Context, appID, messageID string) ([]domain.Delivery, error) {
	if _, err := s.GetMessage(ctx, appID, messageID); err != nil {
		return nil, err
	}
	return deliveriesForMessage(ctx, s.db, appID, messageID)
}

// ListDeliveriesForEndpoint returns the deliveries to one endpoint, newest first. state, when not empty, limits
// the list to one state.
func (s *Store) ListDeliveriesForEndpoint(ctx context.Context, appID, endpointID string, state domain.State, p Page) ([]domain.Delivery, string, error) {
	cur, has, err := parseCursor(p.Cursor)
	if err != nil {
		return nil, "", err
	}
	// The endpoint may have been deleted, and its history is still readable, so this does not use GetEndpoint.
	var exists int
	if err := s.db.QueryRowContext(ctx, "SELECT 1 FROM endpoints WHERE id = ? AND app_id = ?", endpointID, appID).Scan(&exists); errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	} else if err != nil {
		return nil, "", err
	}

	query := "SELECT " + deliveryColumns + " FROM deliveries WHERE app_id = ? AND endpoint_id = ?"
	args := []any{appID, endpointID}
	if state != "" {
		query += " AND state = ?"
		args = append(args, string(state))
	}
	if has {
		query += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, cur.at, cur.at, cur.id)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, p.limit()+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	out, err := collect(rows, scanDelivery)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > p.limit() {
		out = out[:p.limit()]
		last := out[len(out)-1]
		next = cursor{at: ms(last.CreatedAt), id: last.ID}.String()
	}
	return out, next, nil
}

// GetDelivery returns one delivery of an app.
func (s *Store) GetDelivery(ctx context.Context, appID, deliveryID string) (domain.Delivery, error) {
	d, err := scanDelivery(s.db.QueryRowContext(ctx,
		"SELECT "+deliveryColumns+" FROM deliveries WHERE id = ? AND app_id = ?", deliveryID, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Delivery{}, ErrNotFound
	}
	return d, err
}

// ListAttempts returns every attempt at a delivery, oldest first.
func (s *Store) ListAttempts(ctx context.Context, appID, deliveryID string) ([]domain.Attempt, error) {
	if _, err := s.GetDelivery(ctx, appID, deliveryID); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, delivery_id, started_at, duration_ms, status_code, failure, response_snippet, trigger
		 FROM attempts WHERE delivery_id = ? ORDER BY started_at, id`, deliveryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.Attempt
	for rows.Next() {
		var (
			a       domain.Attempt
			started int64
			dur     int64
			status  sql.NullInt64
			failure sql.NullString
			snippet sql.NullString
			trigger string
		)
		if err := rows.Scan(&a.ID, &a.DeliveryID, &started, &dur, &status, &failure, &snippet, &trigger); err != nil {
			return nil, err
		}
		a.StartedAt = fromMS(started)
		a.Duration = time.Duration(dur) * time.Millisecond
		a.StatusCode = int(status.Int64)
		a.Failure = domain.Failure(failure.String)
		a.ResponseSnippet = snippet.String
		a.Trigger = domain.Trigger(trigger)
		out = append(out, a)
	}
	return out, rows.Err()
}

// Replay sends a finished delivery again, as if for the first time: it gets a fresh set of attempts. A delivery
// that is still waiting or being tried cannot be replayed, because it is already on its way.
func (s *Store) Replay(ctx context.Context, appID, deliveryID string, now time.Time) (domain.Delivery, error) {
	var out domain.Delivery
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		d, err := scanDelivery(tx.QueryRowContext(ctx,
			"SELECT "+deliveryColumns+" FROM deliveries WHERE id = ? AND app_id = ?", deliveryID, appID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if !d.State.Final() {
			return ErrBadState
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE deliveries SET state = 'pending', attempt_count = 0, next_attempt_at = ?, manual = 1, finished_at = NULL,
			        lease_until = NULL, lease_owner = NULL WHERE id = ?`, ms(now), d.ID)
		if err != nil {
			return err
		}
		d.State, d.AttemptCount, d.NextAttemptAt, d.FinishedAt = domain.Pending, 0, now.UTC().Truncate(time.Millisecond), nil
		out = d
		return nil
	})
	return out, err
}

// RecoverEndpoint is what to do after a receiver has been fixed. It switches the endpoint back on, if it was off,
// and sends it everything it should have received since the given time:
//
//   - deliveries that failed at or after since are replayed, and
//   - messages created at or after since that this endpoint never got a delivery for are created and sent. That
//     covers the time the endpoint was switched off, when new messages skipped it. Without this, an event that
//     happened during an outage would be lost to that customer for good.
//
// Messages from before the endpoint existed, and messages whose type the endpoint does not subscribe to, are left
// out. A message that already has a delivery is never given a second one. It returns how many deliveries were
// replayed or created.
func (s *Store) RecoverEndpoint(ctx context.Context, appID, endpointID string, since, now time.Time) (int, error) {
	now = now.UTC().Truncate(time.Millisecond)
	var replayed int
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		ep, err := scanEndpoint(tx.QueryRowContext(ctx,
			"SELECT "+endpointColumns+" FROM endpoints WHERE id = ? AND app_id = ? AND deleted_at IS NULL", endpointID, appID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE endpoints SET disabled_at = NULL, disabled_reason = NULL WHERE id = ?", ep.ID); err != nil {
			return err
		}

		// Messages the endpoint missed because it was off.
		from := since
		if ep.CreatedAt.After(from) {
			from = ep.CreatedAt
		}
		rows, err := tx.QueryContext(ctx,
			`SELECT m.id, m.event_type FROM messages m
			 WHERE m.app_id = ? AND m.created_at >= ?
			   AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = m.id AND d.endpoint_id = ?)
			 ORDER BY m.created_at, m.id`, appID, ms(from), ep.ID)
		if err != nil {
			return err
		}
		type candidate struct{ id, eventType string }
		candidates, err := collect(rows, func(r scanner) (candidate, error) {
			var c candidate
			return c, r.Scan(&c.id, &c.eventType)
		})
		if err != nil {
			return err
		}
		var missed []string
		for _, c := range candidates {
			if ep.Wants(c.eventType) {
				missed = append(missed, c.id)
			}
		}
		for _, mid := range missed {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO deliveries (id, message_id, endpoint_id, app_id, state, next_attempt_at, manual, created_at)
				 VALUES (?, ?, ?, ?, 'pending', ?, 1, ?)`, id.New(id.Delivery), mid, ep.ID, appID, ms(now), ms(now)); err != nil {
				return err
			}
		}
		replayed = len(missed)

		res, err := tx.ExecContext(ctx,
			`UPDATE deliveries SET state = 'pending', attempt_count = 0, next_attempt_at = ?, manual = 1, finished_at = NULL,
			        lease_until = NULL, lease_owner = NULL
			 WHERE endpoint_id = ? AND app_id = ? AND state = 'failed' AND finished_at >= ?`,
			ms(now), endpointID, appID, ms(since))
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		replayed += int(n)
		return nil
	})
	return replayed, err
}

// CountByState returns how many deliveries are in each state, for metrics.
func (s *Store) CountByState(ctx context.Context) (map[domain.State]int, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT state, COUNT(*) FROM deliveries GROUP BY state")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[domain.State]int{}
	for rows.Next() {
		var (
			state string
			n     int
		)
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[domain.State(state)] = n
	}
	return out, rows.Err()
}
