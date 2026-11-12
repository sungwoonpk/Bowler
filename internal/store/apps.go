package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/sungwoonpk/bowler/internal/domain"
	"github.com/sungwoonpk/bowler/internal/id"
)

const appColumns = "id, name, uid, created_at"

type scanner interface{ Scan(dest ...any) error }

func scanApp(r scanner) (domain.App, error) {
	var (
		a   domain.App
		uid sql.NullString
		at  int64
	)
	if err := r.Scan(&a.ID, &a.Name, &uid, &at); err != nil {
		return domain.App{}, err
	}
	if uid.Valid {
		a.UID = &uid.String
	}
	a.CreatedAt = fromMS(at)
	return a, nil
}

// CreateApp adds an app. uid is the caller's own identifier for it and must be unique when given.
func (s *Store) CreateApp(ctx context.Context, name string, uid *string, now time.Time) (domain.App, error) {
	a := domain.App{ID: id.New(id.App), Name: name, UID: uid, CreatedAt: now.UTC().Truncate(time.Millisecond)}
	var u sql.NullString
	if uid != nil {
		u = sql.NullString{String: *uid, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO apps (id, name, uid, created_at) VALUES (?, ?, ?, ?)", a.ID, a.Name, u, ms(a.CreatedAt))
	if isUniqueViolation(err) {
		return domain.App{}, ErrConflict
	}
	return a, err
}

// ResolveApp finds an app by its id or by the caller's own uid.
func (s *Store) ResolveApp(ctx context.Context, ref string) (domain.App, error) {
	a, err := scanApp(s.db.QueryRowContext(ctx, "SELECT "+appColumns+" FROM apps WHERE id = ? OR uid = ?", ref, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return domain.App{}, ErrNotFound
	}
	return a, err
}

// ListApps returns apps, newest first.
func (s *Store) ListApps(ctx context.Context, p Page) ([]domain.App, string, error) {
	cur, has, err := parseCursor(p.Cursor)
	if err != nil {
		return nil, "", err
	}
	query := "SELECT " + appColumns + " FROM apps"
	args := []any{}
	if has {
		query += " WHERE (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, cur.at, cur.at, cur.id)
	}
	query += " ORDER BY created_at DESC, id DESC LIMIT ?"
	args = append(args, p.limit()+1)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []domain.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, a)
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
