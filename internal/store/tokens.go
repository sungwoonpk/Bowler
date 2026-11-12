package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
)

// MaxActiveTokensPerApp bounds how many working tokens one app may hold, so a loop that keeps creating them cannot
// fill the table.
const MaxActiveTokensPerApp = 50

// ErrTooManyTokens is returned when an app already holds the most working tokens.
var ErrTooManyTokens = errors.New("store: the app already has the most tokens it may hold")

const tokenColumns = "id, app_id, name, token_prefix, created_at, revoked_at" // #nosec G101 -- a list of column names, not a credential

func scanToken(r scanner) (domain.AppToken, error) {
	var (
		t       domain.AppToken
		created int64
		revoked sql.NullInt64
	)
	if err := r.Scan(&t.ID, &t.AppID, &t.Name, &t.Prefix, &created, &revoked); err != nil {
		return domain.AppToken{}, err
	}
	t.CreatedAt = fromMS(created)
	t.RevokedAt = optMS(revoked)
	return t, nil
}

// CreateAppToken stores the hash of a new token for an app. The token itself is never stored.
func (s *Store) CreateAppToken(ctx context.Context, appID, name, hash, prefix string, now time.Time) (domain.AppToken, error) {
	t := domain.AppToken{ID: id.New(id.Token), AppID: appID, Name: name, Prefix: prefix, CreatedAt: now.UTC().Truncate(time.Millisecond)}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var active int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM app_tokens WHERE app_id = ? AND revoked_at IS NULL", appID).Scan(&active); err != nil {
			return err
		}
		if active >= MaxActiveTokensPerApp {
			return ErrTooManyTokens
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO app_tokens (id, app_id, name, token_hash, token_prefix, created_at) VALUES (?, ?, ?, ?, ?, ?)",
			t.ID, t.AppID, t.Name, hash, t.Prefix, ms(t.CreatedAt))
		return err
	})
	return t, err
}

// AppTokenByHash finds a token that still works. A revoked token is not found.
func (s *Store) AppTokenByHash(ctx context.Context, hash string) (domain.AppToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, "SELECT "+tokenColumns+" FROM app_tokens WHERE token_hash = ? AND revoked_at IS NULL", hash))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AppToken{}, ErrNotFound
	}
	return t, err
}

// GetAppToken returns one token of an app, revoked or not.
func (s *Store) GetAppToken(ctx context.Context, appID, tokenID string) (domain.AppToken, error) {
	t, err := scanToken(s.db.QueryRowContext(ctx, "SELECT "+tokenColumns+" FROM app_tokens WHERE id = ? AND app_id = ?", tokenID, appID))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.AppToken{}, ErrNotFound
	}
	return t, err
}

// ListAppTokens returns an app's tokens, newest first, revoked ones included so the history stays readable.
func (s *Store) ListAppTokens(ctx context.Context, appID string, p Page) ([]domain.AppToken, string, error) {
	cur, has, err := parseCursor(p.Cursor)
	if err != nil {
		return nil, "", err
	}
	query := "SELECT " + tokenColumns + " FROM app_tokens WHERE app_id = ?"
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
	out, err := collect(rows, func(r scanner) (domain.AppToken, error) { return scanToken(r) })
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

// RevokeAppToken stops a token from working. Revoking one that is already revoked changes nothing and keeps the
// original time.
func (s *Store) RevokeAppToken(ctx context.Context, appID, tokenID string, now time.Time) (domain.AppToken, error) {
	var out domain.AppToken
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		t, err := scanToken(tx.QueryRowContext(ctx, "SELECT "+tokenColumns+" FROM app_tokens WHERE id = ? AND app_id = ?", tokenID, appID))
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if t.RevokedAt == nil {
			at := now.UTC().Truncate(time.Millisecond)
			if _, err := tx.ExecContext(ctx, "UPDATE app_tokens SET revoked_at = ? WHERE id = ?", ms(at), t.ID); err != nil {
				return err
			}
			t.RevokedAt = &at
		}
		out = t
		return nil
	})
	return out, err
}
