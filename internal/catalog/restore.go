package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// "Re-import missing files" (M7): a restore request asks the next import of
// a card holding an asset's content to copy it into Staging again. A
// request counts only while its asset is missing and was marked missing no
// later than the request (a file that reappeared and then went missing
// again needs a new request); the others are deleted as they are found.

// freshRequest holds for a request r whose asset a is still missing and
// was marked missing before the request.
const freshRequest = "a.state = 'missing' AND a.updated_at <= r.requested_at"

// dropStaleRequests deletes every request that no longer counts.
func dropStaleRequests(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM restore_requests AS r WHERE NOT EXISTS
		(SELECT 1 FROM assets a WHERE a.sha256 = r.sha256 AND `+freshRequest+`)`)
	if err != nil {
		return fmt.Errorf("drop stale restore requests: %w", err)
	}
	return nil
}

// RequestRestore asks the next import of import ingestID's card to copy
// again the files it found missing from the archive whose asset is still
// missing, in one transaction. Asking again is harmless. It returns how many
// of the import's missing files are now requested (0 when there are none).
func (d *DB) RequestRestore(ctx context.Context, ingestID int64, at time.Time) (int, error) {
	var n int
	err := d.tx(ctx, func(tx *sql.Tx) error {
		if err := dropStaleRequests(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO restore_requests (sha256, requested_at)
			SELECT DISTINCT f.sha256, ? FROM ingest_files f JOIN assets a ON a.sha256 = f.sha256
			WHERE f.ingest_id = ? AND f.outcome = 'missing' AND a.state = 'missing'`, formatTime(at), ingestID)
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(DISTINCT f.sha256) FROM ingest_files f
			JOIN assets a ON a.sha256 = f.sha256 JOIN restore_requests r ON r.sha256 = f.sha256
			WHERE f.ingest_id = ? AND f.outcome = 'missing' AND `+freshRequest, ingestID).Scan(&n)
	})
	if err != nil {
		return 0, fmt.Errorf("request restore for ingest %d: %w", ingestID, err)
	}
	return n, nil
}

// RestoreRequested returns the hashes of the assets an import should copy
// again, after deleting the requests that no longer count, in one
// transaction.
func (d *DB) RestoreRequested(ctx context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	err := d.tx(ctx, func(tx *sql.Tx) error {
		clear(out)
		if err := dropStaleRequests(ctx, tx); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT r.sha256 FROM restore_requests r JOIN assets a ON a.sha256 = r.sha256
			WHERE `+freshRequest)
		if err != nil {
			return err
		}
		for rows.Next() {
			var b []byte
			if err := rows.Scan(&b); err != nil {
				return errors.Join(err, rows.Close())
			}
			out[blobToHex(b)] = true
		}
		if err := rows.Err(); err != nil {
			return errors.Join(err, rows.Close())
		}
		return rows.Close()
	})
	if err != nil {
		return nil, fmt.Errorf("restore requests: %w", err)
	}
	return out, nil
}

// RestorableMissing counts import ingestID's files whose archive copy was
// missing and whose asset still is (restorable), and how many of those have
// a request that counts (requested).
func (d *DB) RestorableMissing(ctx context.Context, ingestID int64) (restorable, requested int, err error) {
	const missing = `FROM ingest_files f JOIN assets a ON a.sha256 = f.sha256
		WHERE f.ingest_id = ? AND f.outcome = 'missing' AND a.state = 'missing'`
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(DISTINCT f.sha256) "+missing, ingestID).Scan(&restorable); err != nil {
		return 0, 0, fmt.Errorf("restorable files of ingest %d: %w", ingestID, err)
	}
	err = d.db.QueryRowContext(ctx, `SELECT COUNT(DISTINCT f.sha256) FROM ingest_files f
		JOIN assets a ON a.sha256 = f.sha256 JOIN restore_requests r ON r.sha256 = f.sha256
		WHERE f.ingest_id = ? AND f.outcome = 'missing' AND `+freshRequest, ingestID).Scan(&requested)
	if err != nil {
		return 0, 0, fmt.Errorf("requested files of ingest %d: %w", ingestID, err)
	}
	return restorable, requested, nil
}

// RestoreMissing records, in one transaction, that the content of missing
// asset a.SHA256 was copied into Staging again by import ingestID: the
// asset becomes staged at a.RelPath with a's mtime, capture time and offset
// (first_ingest stays; checked_at is cleared), its request goes, fp ->
// a.SHA256 is added, and row is written (QuickFP and SHA256 taken from fp
// and a; row.Outcome must be OutcomeNew). An asset that is no longer
// missing is ErrWrongState and nothing is written; an unknown one is
// ErrNotFound.
func (d *DB) RestoreMissing(ctx context.Context, ingestID int64, a Asset, fp string, row IngestFile) error {
	if row.Outcome != OutcomeNew {
		return fmt.Errorf("restore %s: outcome %q must be %q", a.SHA256, row.Outcome, OutcomeNew)
	}
	if a.State != StateStaged || a.RelPath == "" {
		return fmt.Errorf("restore %s: the asset must be staged at a path", a.SHA256)
	}
	if a.UpdatedAt.IsZero() {
		return fmt.Errorf("restore %s: UpdatedAt is zero", a.SHA256)
	}
	sha, err := hexToBlob(a.SHA256)
	if err != nil {
		return err
	}
	var capture, mtime any
	if !a.CaptureTime.IsZero() {
		capture = a.CaptureTime.Format(CaptureLayout)
	}
	if a.Mtime != 0 {
		mtime = a.Mtime
	}
	row.QuickFP, row.SHA256 = fp, a.SHA256
	err = d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE assets SET state = 'staged', rel_path = ?, mtime = ?, capture_time = ?,
			tz_offset = ?, checked_at = NULL, updated_at = ? WHERE sha256 = ? AND state = 'missing'`,
			a.RelPath, mtime, capture, a.TZOffset, formatTime(a.UpdatedAt), sha)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var state string
			err := tx.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", sha).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			return &WrongStateError{Verb: "restore", SHA256: a.SHA256, State: AssetState(state)}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM restore_requests WHERE sha256 = ?", sha); err != nil {
			return err
		}
		if err := insertFingerprint(ctx, tx, fp, a.SHA256); err != nil {
			return err
		}
		return insertIngestFile(ctx, tx, ingestID, row)
	})
	if err != nil {
		return fmt.Errorf("restore %s: %w", a.SHA256, err)
	}
	return nil
}
