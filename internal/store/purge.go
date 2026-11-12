package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// PurgeResult says what one call to Purge removed.
type PurgeResult struct {
	Messages   int
	Deliveries int
	Attempts   int
	Endpoints  int
	// AuditEntries is filled in by the retention janitor, which purges the audit log separately.
	AuditEntries int
}

// Add returns the sum of two results.
func (r PurgeResult) Add(o PurgeResult) PurgeResult {
	return PurgeResult{r.Messages + o.Messages, r.Deliveries + o.Deliveries, r.Attempts + o.Attempts, r.Endpoints + o.Endpoints,
		r.AuditEntries + o.AuditEntries}
}

// purgeableMessagesSQL picks the oldest messages that are safe to delete: created before the cutoff, with nothing
// left to do and no activity since the cutoff. A message whose delivery is still waiting, or whose delivery finished
// after the cutoff (a late retry, or a replay by a person), stays, so history is never cut out from under work.
const purgeableMessagesSQL = `
SELECT m.id FROM messages m
WHERE m.created_at < ?1
  AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.message_id = m.id
                  AND (d.state IN ('pending', 'inflight') OR d.finished_at >= ?1))
ORDER BY m.created_at, m.id
LIMIT ?2`

// Purge deletes finished data older than cutoff, at most limit messages at a time, and says what it removed. A caller
// that gets back a full batch calls again. Small batches keep each write lock short, so the queue is never held up for
// long while old data is cleared.
//
// A message goes with its deliveries and their attempts. An endpoint that was deleted before the cutoff goes once
// nothing refers to it. A message's idempotency key goes with the message, so after a message is purged the same key
// is accepted as new: the key protects against repeats only within the retention period.
func (s *Store) Purge(ctx context.Context, cutoff time.Time, limit int) (PurgeResult, error) {
	var res PurgeResult
	if limit < 1 {
		return res, nil
	}
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, purgeableMessagesSQL, ms(cutoff), limit)
		if err != nil {
			return err
		}
		ids, err := collect(rows, func(r scanner) (string, error) {
			var id string
			return id, r.Scan(&id)
		})
		if err != nil {
			return err
		}
		if len(ids) > 0 {
			// Only "?" placeholders are joined into the text. Every value is passed as an argument.
			marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
			args := make([]any, len(ids))
			for i, id := range ids {
				args[i] = id
			}
			// #nosec G202 -- only "?" placeholders are joined into the text, see above.
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM deliveries WHERE message_id IN (`+marks+`)`, args...).Scan(&res.Deliveries); err != nil {
				return err
			}
			// #nosec G202 -- only "?" placeholders are joined into the text, see above.
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM attempts WHERE delivery_id IN
				(SELECT id FROM deliveries WHERE message_id IN (`+marks+`))`, args...).Scan(&res.Attempts); err != nil {
				return err
			}
			// Foreign keys carry the delete on to the deliveries and their attempts.
			// #nosec G202 -- only "?" placeholders are joined into the text, see above.
			r, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE id IN (`+marks+`)`, args...)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			res.Messages = int(n)
		}

		r, err := tx.ExecContext(ctx, `DELETE FROM endpoints WHERE deleted_at IS NOT NULL AND deleted_at < ?
			AND NOT EXISTS (SELECT 1 FROM deliveries d WHERE d.endpoint_id = endpoints.id)`, ms(cutoff))
		if err != nil {
			return err
		}
		n, _ := r.RowsAffected()
		res.Endpoints = int(n)
		return nil
	})
	if err != nil {
		return PurgeResult{}, fmt.Errorf("store: purge: %w", err)
	}
	return res, nil
}
