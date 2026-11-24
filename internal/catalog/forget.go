package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IngestAssets returns every asset import ingestID is linked to: those it
// brought in (first_ingest) and those any of its ingest_files rows names
// (already in staging or the archive, seen before), ordered by hash.
func (d *DB) IngestAssets(ctx context.Context, ingestID int64) ([]Asset, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+assetCols+" FROM assets WHERE first_ingest = ?"+
		" UNION SELECT "+assetCols+" FROM assets WHERE sha256 IN"+
		" (SELECT sha256 FROM ingest_files WHERE ingest_id = ? AND sha256 IS NOT NULL) ORDER BY sha256", ingestID, ingestID)
	if err != nil {
		return nil, fmt.Errorf("assets of ingest %d: %w", ingestID, err)
	}
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("assets of ingest %d: %w", ingestID, err), rows.Close())
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("assets of ingest %d: %w", ingestID, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("assets of ingest %d: %w", ingestID, err)
	}
	return out, nil
}

// ForgetAssets deletes the assets among shas whose state is in states
// (staged and/or culled; anything else is refused), with their
// fingerprints, aliases and clock warnings, in one transaction, so a card
// holding that content imports it as new again (§8 "Forget an import").
// Archived and missing assets are never deleted; unknown hashes are
// skipped. It returns the hashes it deleted, in the order given.
func (d *DB) ForgetAssets(ctx context.Context, shas []string, states []AssetState) ([]string, error) {
	allowed := map[string]bool{}
	for _, st := range states {
		if st != StateStaged && st != StateCulled {
			return nil, fmt.Errorf("forget assets: %s assets are never forgotten", st)
		}
		allowed[string(st)] = true
	}
	var out []string
	err := d.tx(ctx, func(tx *sql.Tx) error {
		out = nil
		for _, sha := range shas {
			b, err := hexToBlob(sha)
			if err != nil {
				return err
			}
			var state string
			err = tx.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", b).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if !allowed[state] {
				continue
			}
			for _, q := range []string{
				"DELETE FROM fingerprints WHERE sha256 = ?",
				"DELETE FROM aliases WHERE sha256 = ?",
				"DELETE FROM clock_warnings WHERE sha256 = ?",
				"DELETE FROM assets WHERE sha256 = ?",
			} {
				if _, err := tx.ExecContext(ctx, q, b); err != nil {
					return fmt.Errorf("forget %s: %w", sha, err)
				}
			}
			out = append(out, sha)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("forget assets: %w", err)
	}
	return out, nil
}

// CulledSeenBefore returns the hashes of import ingestID's "seen before"
// files whose asset is still culled, sorted.
func (d *DB) CulledSeenBefore(ctx context.Context, ingestID int64) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT DISTINCT a.sha256 FROM ingest_files f JOIN assets a ON a.sha256 = f.sha256
		WHERE f.ingest_id = ? AND f.outcome = 'seen_before' AND a.state = 'culled' ORDER BY a.sha256`, ingestID)
	if err != nil {
		return nil, fmt.Errorf("culled files of ingest %d: %w", ingestID, err)
	}
	var out []string
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, errors.Join(fmt.Errorf("culled files of ingest %d: %w", ingestID, err), rows.Close())
		}
		out = append(out, blobToHex(b))
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("culled files of ingest %d: %w", ingestID, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("culled files of ingest %d: %w", ingestID, err)
	}
	return out, nil
}
