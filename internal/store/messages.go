package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
)

const messageColumns = "id, app_id, event_type, payload, idempotency_key, created_at"

func scanMessage(r scanner) (domain.Message, error) {
	var (
		m       domain.Message
		payload string
		key     sql.NullString
		at      int64
	)
	if err := r.Scan(&m.ID, &m.AppID, &m.EventType, &payload, &key, &at); err != nil {
		return domain.Message{}, err
	}
	m.Payload = json.RawMessage(payload)
	m.IdempotencyKey = key.String
	m.CreatedAt = fromMS(at)
	return m, nil
}

// NewMessage is what a caller supplies to send an event.
type NewMessage struct {
	AppID          string
	EventType      string
	Payload        json.RawMessage
	IdempotencyKey string // optional
}

// CreateMessage records an event and creates one delivery for every endpoint that should get it. Both happen in
// one transaction, so an event is never stored without its deliveries, or the other way round.
//
// Sending the same IdempotencyKey again for the same app returns the original message and its deliveries with
// created set to false, instead of sending the event twice. That makes it safe for the caller to retry a request
// whose answer it never received.
func (s *Store) CreateMessage(ctx context.Context, n NewMessage, now time.Time) (msg domain.Message, deliveries []domain.Delivery, created bool, err error) {
	now = now.UTC().Truncate(time.Millisecond)
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		if n.IdempotencyKey != "" {
			existing, err := scanMessage(tx.QueryRowContext(ctx,
				"SELECT "+messageColumns+" FROM messages WHERE app_id = ? AND idempotency_key = ?", n.AppID, n.IdempotencyKey))
			if err == nil {
				msg = existing
				deliveries, err = deliveriesForMessage(ctx, tx, n.AppID, existing.ID)
				return err
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
		}

		msg = domain.Message{ID: id.New(id.Message), AppID: n.AppID, EventType: n.EventType, Payload: n.Payload,
			IdempotencyKey: n.IdempotencyKey, CreatedAt: now}
		key := sql.NullString{String: n.IdempotencyKey, Valid: n.IdempotencyKey != ""}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO messages (id, app_id, event_type, payload, idempotency_key, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			msg.ID, msg.AppID, msg.EventType, string(msg.Payload), key, ms(now)); err != nil {
			return err
		}

		rows, err := tx.QueryContext(ctx,
			"SELECT "+endpointColumns+" FROM endpoints WHERE app_id = ? AND deleted_at IS NULL AND disabled_at IS NULL ORDER BY id", n.AppID)
		if err != nil {
			return err
		}
		open, err := collect(rows, scanEndpoint)
		if err != nil {
			return err
		}
		var targets []domain.Endpoint
		for _, e := range open {
			if e.Wants(n.EventType) {
				targets = append(targets, e)
			}
		}

		for _, e := range targets {
			d := domain.Delivery{ID: id.New(id.Delivery), MessageID: msg.ID, EndpointID: e.ID, AppID: n.AppID,
				State: domain.Pending, NextAttemptAt: now, CreatedAt: now}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO deliveries (id, message_id, endpoint_id, app_id, state, next_attempt_at, created_at)
				 VALUES (?, ?, ?, ?, 'pending', ?, ?)`,
				d.ID, d.MessageID, d.EndpointID, d.AppID, ms(now), ms(now)); err != nil {
				return err
			}
			deliveries = append(deliveries, d)
		}
		created = true
		return nil
	})
	if err != nil {
		return domain.Message{}, nil, false, fmt.Errorf("store: create message: %w", err)
	}
	return msg, deliveries, created, nil
}

// GetMessage returns one message of an app.
func (s *Store) GetMessage(ctx context.Context, appID, messageID string) (domain.Message, error) {
	m, err := scanMessage(s.db.QueryRowContext(ctx,
		"SELECT "+messageColumns+" FROM messages WHERE id = ? AND app_id = ?", messageID, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Message{}, ErrNotFound
	}
	return m, err
}

// ListMessages returns an app's messages, newest first. eventType, when not empty, limits the list to one type.
func (s *Store) ListMessages(ctx context.Context, appID, eventType string, p Page) ([]domain.Message, string, error) {
	cur, has, err := parseCursor(p.Cursor)
	if err != nil {
		return nil, "", err
	}
	query := "SELECT " + messageColumns + " FROM messages WHERE app_id = ?"
	args := []any{appID}
	if eventType != "" {
		query += " AND event_type = ?"
		args = append(args, eventType)
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
	defer rows.Close()
	var out []domain.Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
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
