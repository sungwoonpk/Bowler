package store

import (
	"context"
	"database/sql"
	"encoding/json"
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

// RecordAudit adds an entry to the audit log and returns it. Entries are never changed afterwards.
func (s *Store) RecordAudit(ctx context.Context, e domain.AuditEntry, now time.Time) (domain.AuditEntry, error) {
	e.ID = id.New(id.Audit)
	e.At = now.UTC().Truncate(time.Millisecond)
	nul := func(v string) sql.NullString { return sql.NullString{String: v, Valid: v != ""} }
	var detail sql.NullString
	if len(e.Detail) > 0 {
		detail = sql.NullString{String: string(e.Detail), Valid: true}
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO audit_log (id, at, actor_kind, actor_id, app_id, action, resource_kind, resource_id, detail, request_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			e.ID, ms(e.At), e.ActorKind, e.ActorID, nul(e.AppID), e.Action, nul(e.ResourceKind), nul(e.ResourceID), detail, nul(e.RequestID))
		return err
	})
	if err != nil {
		return domain.AuditEntry{}, fmt.Errorf("store: record audit entry: %w", err)
	}
	return e, nil
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
