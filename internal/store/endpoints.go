package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
)

const endpointColumns = `id, app_id, url, description, secret, prev_secret, prev_secret_expires_at, event_types,
	concurrency, rate_per_second, disabled_at, disabled_reason, created_at`

func scanEndpoint(r scanner) (domain.Endpoint, error) {
	var (
		e                       domain.Endpoint
		prev, types, reason     sql.NullString
		prevExpires, disabledAt sql.NullInt64
		created                 int64
	)
	if err := r.Scan(&e.ID, &e.AppID, &e.URL, &e.Description, &e.Secret, &prev, &prevExpires, &types,
		&e.Concurrency, &e.RatePerSecond, &disabledAt, &reason, &created); err != nil {
		return domain.Endpoint{}, err
	}
	e.PrevSecret = prev.String
	e.PrevSecretExpiresAt = optMS(prevExpires)
	e.DisabledAt = optMS(disabledAt)
	e.DisabledReason = reason.String
	e.CreatedAt = fromMS(created)
	if types.Valid {
		if err := json.Unmarshal([]byte(types.String), &e.EventTypes); err != nil {
			return domain.Endpoint{}, err
		}
	}
	return e, nil
}

func encodeEventTypes(types []string) (sql.NullString, error) {
	if types == nil {
		return sql.NullString{}, nil
	}
	b, err := json.Marshal(types)
	return sql.NullString{String: string(b), Valid: true}, err
}

// NewEndpoint is what a caller supplies to create an endpoint.
type NewEndpoint struct {
	AppID       string
	URL         string
	Description string
	EventTypes  []string // nil for every event
	Concurrency int
	// RatePerSecond is zero for no limit.
	RatePerSecond int
	Secret        string
}

// CreateEndpoint adds an endpoint to an app.
func (s *Store) CreateEndpoint(ctx context.Context, n NewEndpoint, now time.Time) (domain.Endpoint, error) {
	types, err := encodeEventTypes(n.EventTypes)
	if err != nil {
		return domain.Endpoint{}, err
	}
	if n.Concurrency < 1 {
		n.Concurrency = 1
	}
	e := domain.Endpoint{
		ID: id.New(id.Endpoint), AppID: n.AppID, URL: n.URL, Description: n.Description, EventTypes: n.EventTypes,
		Concurrency: n.Concurrency, RatePerSecond: n.RatePerSecond, Secret: n.Secret, CreatedAt: now.UTC().Truncate(time.Millisecond),
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO endpoints (id, app_id, url, description, secret, event_types, concurrency, rate_per_second, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.AppID, e.URL, e.Description, e.Secret, types, e.Concurrency, e.RatePerSecond, ms(e.CreatedAt))
	return e, err
}

// GetEndpoint returns one endpoint of an app. A deleted endpoint is not found.
func (s *Store) GetEndpoint(ctx context.Context, appID, endpointID string) (domain.Endpoint, error) {
	e, err := scanEndpoint(s.db.QueryRowContext(ctx,
		"SELECT "+endpointColumns+" FROM endpoints WHERE id = ? AND app_id = ? AND deleted_at IS NULL", endpointID, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Endpoint{}, ErrNotFound
	}
	return e, err
}

// ListEndpoints returns an app's endpoints, newest first.
func (s *Store) ListEndpoints(ctx context.Context, appID string, p Page) ([]domain.Endpoint, string, error) {
	cur, has, err := parseCursor(p.Cursor)
	if err != nil {
		return nil, "", err
	}
	query := "SELECT " + endpointColumns + " FROM endpoints WHERE app_id = ? AND deleted_at IS NULL"
	args := []any{appID}
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
	var out []domain.Endpoint
	for rows.Next() {
		e, err := scanEndpoint(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, e)
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

// EndpointUpdate lists the fields to change. A nil field is left alone.
type EndpointUpdate struct {
	URL         *string
	Description *string
	// EventTypes is a pointer to a list so that "no change" (nil) and "every event" (pointer to nil) differ.
	EventTypes  **[]string
	Concurrency *int
	// RatePerSecond changes the limit. Zero removes it.
	RatePerSecond *int
	// Disabled switches the endpoint off or back on. Switching it on clears the reason.
	Disabled *bool
}

// UpdateEndpoint changes an endpoint and returns it.
func (s *Store) UpdateEndpoint(ctx context.Context, appID, endpointID string, u EndpointUpdate, now time.Time) (domain.Endpoint, error) {
	var out domain.Endpoint
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEndpoint(tx.QueryRowContext(ctx,
			"SELECT "+endpointColumns+" FROM endpoints WHERE id = ? AND app_id = ? AND deleted_at IS NULL", endpointID, appID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if u.URL != nil {
			e.URL = *u.URL
		}
		if u.Description != nil {
			e.Description = *u.Description
		}
		if u.EventTypes != nil {
			if *u.EventTypes == nil {
				e.EventTypes = nil
			} else {
				e.EventTypes = **u.EventTypes
			}
		}
		if u.Concurrency != nil {
			e.Concurrency = *u.Concurrency
		}
		if u.RatePerSecond != nil {
			e.RatePerSecond = *u.RatePerSecond
		}
		if u.Disabled != nil {
			if *u.Disabled && e.DisabledAt == nil {
				t := now.UTC().Truncate(time.Millisecond)
				e.DisabledAt, e.DisabledReason = &t, "disabled by a person"
			} else if !*u.Disabled {
				e.DisabledAt, e.DisabledReason = nil, ""
			}
		}
		types, err := encodeEventTypes(e.EventTypes)
		if err != nil {
			return err
		}
		reason := sql.NullString{String: e.DisabledReason, Valid: e.DisabledReason != ""}
		_, err = tx.ExecContext(ctx,
			`UPDATE endpoints SET url = ?, description = ?, event_types = ?, concurrency = ?, rate_per_second = ?, disabled_at = ?, disabled_reason = ?
			 WHERE id = ?`,
			e.URL, e.Description, types, e.Concurrency, e.RatePerSecond, nullMS(e.DisabledAt), reason, e.ID)
		out = e
		return err
	})
	return out, err
}

// DeleteEndpoint removes an endpoint from view and cancels the deliveries still waiting to go to it. Finished
// deliveries and their attempts are kept so history stays readable.
func (s *Store) DeleteEndpoint(ctx context.Context, appID, endpointID string, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE endpoints SET deleted_at = ? WHERE id = ? AND app_id = ? AND deleted_at IS NULL", ms(now), endpointID, appID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE deliveries SET state = 'cancelled', finished_at = ?, lease_until = NULL, lease_owner = NULL
			 WHERE endpoint_id = ? AND state IN ('pending', 'inflight')`, ms(now), endpointID)
		return err
	})
}

// RotateSecret makes newSecret the signing secret. The old one keeps signing until graceUntil, so receivers have
// time to switch.
func (s *Store) RotateSecret(ctx context.Context, appID, endpointID, newSecret string, graceUntil time.Time) (domain.Endpoint, error) {
	var out domain.Endpoint
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		e, err := scanEndpoint(tx.QueryRowContext(ctx,
			"SELECT "+endpointColumns+" FROM endpoints WHERE id = ? AND app_id = ? AND deleted_at IS NULL", endpointID, appID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "UPDATE endpoints SET secret = ?, prev_secret = ?, prev_secret_expires_at = ? WHERE id = ?",
			newSecret, e.Secret, ms(graceUntil), e.ID)
		e.PrevSecret, e.Secret = e.Secret, newSecret
		g := graceUntil.UTC()
		e.PrevSecretExpiresAt = &g
		out = e
		return err
	})
	return out, err
}
