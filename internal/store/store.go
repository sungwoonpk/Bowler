// Package store keeps everything Bowler remembers in one SQLite file.
//
// SQLite is driven by modernc.org/sqlite, a translation of it into Go, so Bowler builds with no C compiler and
// ships as one static binary. The write path takes the database lock at the start of each transaction, so two
// workers can never both claim the same delivery.
package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

// Errors a caller can test for with errors.Is.
var (
	ErrNotFound  = errors.New("store: not found")
	ErrConflict  = errors.New("store: already exists")
	ErrLeaseLost = errors.New("store: the lease on this delivery is no longer held")
	ErrBadCursor = errors.New("store: invalid cursor")
	ErrBadState  = errors.New("store: not allowed in the delivery's current state")
)

// Store is a handle on the database. It is safe for use from many goroutines.
type Store struct {
	db *sql.DB
	// writeMu lines up the transactions inside this process. SQLite allows one writer at a time anyway, and
	// making the goroutines take turns here is cheaper and steadier than letting them all wait on the database
	// lock together, which under heavy load can run out of patience and fail with SQLITE_BUSY. The database
	// lock and busy timeout still protect against a second process using the same file.
	writeMu sync.Mutex
}

// Open opens or creates the database at path and brings its schema up to date.
func Open(path string) (*Store, error) {
	q := url.Values{}
	// busy_timeout makes a writer wait for the lock instead of failing at once.
	q.Add("_pragma", "busy_timeout(10000)")
	// WAL lets readers carry on while a write is in progress.
	q.Add("_pragma", "journal_mode(WAL)")
	// NORMAL is safe with WAL: a power cut can lose the last commits but cannot corrupt the file.
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(1)")
	// Every transaction takes the write lock when it begins. Without this, two transactions could both read the
	// same due delivery and only discover the clash when one tried to write.
	q.Set("_txlock", "immediate")

	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(8)
	db.SetConnMaxIdleTime(5 * time.Minute)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		_ = db.Close() // the migration error is the one worth reporting
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the database answers.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// migrate applies every numbered file in migrations/ that has not run yet, each in its own transaction.
func (s *Store) migrate(ctx context.Context) error {
	var current int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("store: read schema version: %w", err)
	}
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for i, name := range names {
		version := i + 1
		if version <= current {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		if err := s.apply(ctx, version, string(body)); err != nil {
			return fmt.Errorf("store: migration %s: %w", name, err)
		}
	}
	return nil
}

func (s *Store) apply(ctx context.Context, version int, body string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, body); err != nil {
		return err
	}
	// PRAGMA does not accept parameters, and version is a number we computed.
	if _, err := tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(version)); err != nil {
		return err
	}
	return tx.Commit()
}

// withTx runs fn inside a transaction and commits it, or rolls it back if fn fails.
func (s *Store) withTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// ms and fromMS convert between time.Time and the integer milliseconds kept in the database.
func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMS(n int64) time.Time { return time.UnixMilli(n).UTC() }

func optMS(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromMS(n.Int64)
	return &t
}

func nullMS(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: ms(*t), Valid: true}
}

// Page is how a list is asked for: how many, and where to continue from.
type Page struct {
	// Limit is the most to return. Values outside 1 to 250 are brought into that range.
	Limit int
	// Cursor is the Next value of the previous page. Empty for the first page.
	Cursor string
}

func (p Page) limit() int {
	switch {
	case p.Limit < 1:
		return 50
	case p.Limit > 250:
		return 250
	default:
		return p.Limit
	}
}

// cursor marks a position in a list that is ordered newest first, by creation time and then by id.
type cursor struct {
	at int64
	id string
}

func (c cursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(c.at, 10) + ":" + c.id))
}

func parseCursor(s string) (cursor, bool, error) {
	if s == "" {
		return cursor{}, false, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, false, ErrBadCursor
	}
	at, id, ok := strings.Cut(string(raw), ":")
	n, perr := strconv.ParseInt(at, 10, 64)
	if !ok || perr != nil || id == "" {
		return cursor{}, false, ErrBadCursor
	}
	return cursor{at: n, id: id}, true, nil
}

// collect reads every row of a result with scan and always closes the rows, whichever way it ends. Closing by hand in
// each early return is easy to get wrong, and a forgotten close keeps a connection busy.
func collect[T any](rows *sql.Rows, scan func(scanner) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate in a unique index.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
