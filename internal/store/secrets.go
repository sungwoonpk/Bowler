package store

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sungwoonpk/bowler/internal/secretbox"
)

// ResealSecrets brings every stored signing secret in line with the key the store was opened with, and returns how many
// endpoints it changed.
//
// With a key, a secret that is still plain (a database from before encryption) or sealed under an older key (after a
// rotation) is written again under the current key. Without a key, nothing is changed, and a database that holds sealed
// secrets is refused, since it could not sign anything and the reason should be clear at once.
//
// It opens every secret on the way, so a wrong key, a damaged value or a value moved to another row is found here, when
// the program starts, instead of failing later when that endpoint is first used.
func (s *Store) ResealSecrets(ctx context.Context, batch int) (int, error) {
	if batch < 1 {
		batch = 100
	}
	if !s.box.Enabled() {
		var sealed int
		err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM endpoints WHERE secret LIKE 'enc:v1:%' OR prev_secret LIKE 'enc:v1:%'`).Scan(&sealed)
		if err != nil {
			return 0, err
		}
		if sealed > 0 {
			return 0, fmt.Errorf("store: %d endpoints have encrypted secrets and no encryption key is set: %w", sealed, secretbox.ErrNoKey)
		}
		return 0, nil
	}

	changed, last := 0, ""
	for {
		n, next, err := s.resealBatch(ctx, last, batch)
		if err != nil {
			return changed, err
		}
		changed += n
		if next == "" {
			return changed, nil
		}
		last = next
	}
}

// resealBatch handles the endpoints after the id "after", at most limit of them. It returns how many it changed and the
// id to continue from, which is empty when there are no more.
func (s *Store) resealBatch(ctx context.Context, after string, limit int) (changed int, next string, err error) {
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, secret, prev_secret FROM endpoints WHERE id > ? ORDER BY id LIMIT ?`, after, limit)
		if err != nil {
			return err
		}
		type row struct {
			id, secret string
			prev       sql.NullString
		}
		found, err := collect(rows, func(r scanner) (row, error) {
			var v row
			return v, r.Scan(&v.id, &v.secret, &v.prev)
		})
		if err != nil {
			return err
		}
		for _, v := range found {
			next = v.id
			// Every secret is opened, changed or not, so a wrong key or a damaged value is found now.
			secret, err := s.box.Open(v.secret, secretAAD(v.id, "secret"))
			if err != nil {
				return fmt.Errorf("store: the signing secret of endpoint %s: %w", v.id, err)
			}
			prev, err := s.box.Open(v.prev.String, secretAAD(v.id, "prev_secret"))
			if err != nil {
				return fmt.Errorf("store: the previous signing secret of endpoint %s: %w", v.id, err)
			}
			if !s.box.NeedsSealing(v.secret) && !s.box.NeedsSealing(v.prev.String) {
				continue
			}
			sealedSecret, err := s.box.Seal(secret, secretAAD(v.id, "secret"))
			if err != nil {
				return err
			}
			sealedPrev := sql.NullString{}
			if prev != "" {
				p, err := s.box.Seal(prev, secretAAD(v.id, "prev_secret"))
				if err != nil {
					return err
				}
				sealedPrev = sql.NullString{String: p, Valid: true}
			}
			if _, err := tx.ExecContext(ctx, `UPDATE endpoints SET secret = ?, prev_secret = ? WHERE id = ?`, sealedSecret, sealedPrev, v.id); err != nil {
				return err
			}
			changed++
		}
		if len(found) < limit {
			next = ""
		}
		return nil
	})
	return changed, next, err
}

// Compact rewrites the database file so that nothing deleted or replaced stays in it. Replacing a plain secret with a
// sealed one leaves the plain text in unused pages of the file and in the write-ahead log until they are reused, so after
// secrets are sealed the file is compacted to remove it. It needs free disk space about the size of the database, and it
// holds the write lock for as long as it takes, so it is for start-up and not for a busy service.
func (s *Store) Compact(ctx context.Context) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "VACUUM"); err != nil {
		return fmt.Errorf("store: compact: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("store: checkpoint: %w", err)
	}
	return nil
}
