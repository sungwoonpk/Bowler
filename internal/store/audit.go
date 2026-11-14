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

const auditColumns = "seq, id, at, actor_kind, actor_id, app_id, action, resource_kind, resource_id, detail, request_id"

// auditRow is an entry with its place in the log, which the API does not show but the paging needs.
type auditRow struct {
	domain.AuditEntry
	seq int64
}

func scanAudit(r scanner) (auditRow, error) {
	var (
		e                                      auditRow
		at                                     int64
		appID, resKind, resID, detail, request sql.NullString
	)
	if err := r.Scan(&e.seq, &e.ID, &at, &e.ActorKind, &e.ActorID, &appID, &e.Action, &resKind, &resID, &detail, &request); err != nil {
		return auditRow{}, err
	}
	e.At = fromMS(at)
	e.AppID, e.ResourceKind, e.ResourceID, e.RequestID = appID.String, resKind.String, resID.String, request.String
	if detail.Valid {
		e.Detail = json.RawMessage(detail.String)
	}
	return e, nil
}

// ErrAuditWrite means a change was not made because its audit entry could not be written. The change and the entry are
// one transaction, so neither exists.
var ErrAuditWrite = errors.New("store: the audit entry could not be written")

// WriteOption changes how a write is made.
type WriteOption func(*writeOpts)

type writeOpts struct{ audit *domain.AuditEntry }

func optsOf(opts []WriteOption) writeOpts {
	var o writeOpts
	for _, f := range opts {
		f(&o)
	}
	return o
}

// Audited makes a write record e in the audit log in the same transaction as the change, so that the change and its
// entry exist together or not at all. The store fills in the id and the time, and the app and the resource it changed
// wherever e leaves them empty, which is the only way to name a resource that the change itself creates. A write that
// changes nothing (revoking a token that is already revoked) writes no entry.
func Audited(e domain.AuditEntry) WriteOption { return func(o *writeOpts) { o.audit = &e } }

// fill returns have when it is set and otherwise the value the write made.
func fill(have, made string) string {
	if have != "" {
		return have
	}
	return made
}

// about completes an entry with the app and the resource the write is about, wherever the caller left them empty. The
// store knows these better than its caller does, since it has just changed them.
func about(appID, resourceID string, then func(*domain.AuditEntry)) func(*domain.AuditEntry) {
	return func(e *domain.AuditEntry) {
		e.AppID, e.ResourceID = fill(e.AppID, appID), fill(e.ResourceID, resourceID)
		if then != nil {
			then(e)
		}
	}
}

// withCount adds a number to a JSON object of details, or makes the object when there are none yet.
func withCount(detail json.RawMessage, key string, n int) json.RawMessage {
	m := map[string]any{}
	if len(detail) > 0 {
		_ = json.Unmarshal(detail, &m) // details are made by our own code, and if they are not an object they are replaced
	}
	m[key] = n
	b, _ := json.Marshal(m) // a map of plain values always marshals
	return b
}

// record writes the entry inside tx, if there is one. fill is given the entry first, to complete what only the write
// knows.
func (o writeOpts) record(ctx context.Context, tx *sql.Tx, now time.Time, fill func(*domain.AuditEntry)) error {
	if o.audit == nil {
		return nil
	}
	e := *o.audit
	if fill != nil {
		fill(&e)
	}
	if _, err := insertAudit(ctx, tx, e, now); err != nil {
		return fmt.Errorf("%w: %w", ErrAuditWrite, err)
	}
	return nil
}

func insertAudit(ctx context.Context, tx *sql.Tx, e domain.AuditEntry, now time.Time) (domain.AuditEntry, error) {
	e.ID = id.New(id.Audit)
	e.At = now.UTC().Truncate(time.Millisecond)
	nul := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	var detail sql.NullString
	if len(e.Detail) > 0 {
		detail = sql.NullString{String: string(e.Detail), Valid: true}
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_log (id, at, actor_kind, actor_id, app_id, action, resource_kind, resource_id, detail, request_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, ms(e.At), e.ActorKind, e.ActorID, nul(e.AppID), e.Action, nul(e.ResourceKind), nul(e.ResourceID), detail, nul(e.RequestID))
	return e, err
}

// RecordAudit adds an entry to the audit log on its own and returns it. Entries are never changed afterwards. Use it for
// something that changes nothing, such as reading a secret. A change is recorded with the Audited option instead.
func (s *Store) RecordAudit(ctx context.Context, e domain.AuditEntry, now time.Time) (domain.AuditEntry, error) {
	var out domain.AuditEntry
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var err error
		out, err = insertAudit(ctx, tx, e, now)
		return err
	})
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("%w: %w", ErrAuditWrite, err)
	}
	return out, nil
}

// AuditFilter narrows the audit log. Empty fields match everything.
type AuditFilter struct {
	AppID  string
	Action string
}

// ListAudit returns audit entries, newest first.
func (s *Store) ListAudit(ctx context.Context, f AuditFilter, p Page) ([]domain.AuditEntry, string, error) {
	cur, has, err := parseCursor(p.Cursor)
	if err != nil {
		return nil, "", err
	}
	query := "SELECT " + auditColumns + " FROM audit_log WHERE 1 = 1"
	args := []any{}
	if f.AppID != "" {
		query += " AND app_id = ?"
		args = append(args, f.AppID)
	}
	if f.Action != "" {
		query += " AND action = ?"
		args = append(args, f.Action)
	}
	if has {
		query += " AND seq < ?" // the cursor's number is the position of the last entry of the previous page
		args = append(args, cur.at)
	}
	query += " ORDER BY seq DESC LIMIT ?"
	args = append(args, p.limit()+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	found, err := collect(rows, scanAudit)
	if err != nil {
		return nil, "", err
	}
	next := ""
	if len(found) > p.limit() {
		found = found[:p.limit()]
		last := found[len(found)-1]
		next = cursor{at: last.seq, id: last.ID}.String()
	}
	out := make([]domain.AuditEntry, len(found))
	for i, f := range found {
		out[i] = f.AuditEntry
	}
	return out, next, nil
}

// PurgeAudit deletes up to limit entries older than cutoff, oldest first, and returns how many it removed.
func (s *Store) PurgeAudit(ctx context.Context, cutoff time.Time, limit int) (int, error) {
	if limit < 1 {
		return 0, nil
	}
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := tx.ExecContext(ctx, `DELETE FROM audit_log WHERE id IN
			(SELECT id FROM audit_log WHERE at < ? ORDER BY at, seq LIMIT ?)`, ms(cutoff), limit)
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("store: purge audit log: %w", err)
	}
	return int(n), nil
}
