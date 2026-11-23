package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrNameTaken reports a staging folder name another folder already has.
var ErrNameTaken = errors.New("catalog: a staging folder with that name already exists")

// hashChunk bounds the number of ? in one IN (…) list.
const hashChunk = 500

// normalizeDates sorts and de-duplicates dates (each YYYY-MM-DD) and
// returns them as the JSON the dates column holds.
func normalizeDates(dates []string) (string, error) {
	out := []string{}
	for _, d := range dates {
		if !dateRE.MatchString(d) {
			return "", fmt.Errorf("bad date %q", d)
		}
		out = append(out, d)
	}
	slices.Sort(out)
	b, err := json.Marshal(slices.Compact(out))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// nameTaken returns ErrNameTaken when a row other than except is named name.
func nameTaken(ctx context.Context, tx *sql.Tx, name string, except int64) error {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM staging_folders WHERE name = ? AND id != ?", name, except).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("%w: %q", ErrNameTaken, name)
}

// CreateStagingFolder adds a folder row. ErrNameTaken when the name exists.
func (d *DB) CreateStagingFolder(ctx context.Context, name string, dates []string, createdAt time.Time) (StagingFolder, error) {
	ds, err := normalizeDates(dates)
	if err != nil {
		return StagingFolder{}, fmt.Errorf("create staging folder %q: %w", name, err)
	}
	var id int64
	err = d.tx(ctx, func(tx *sql.Tx) error {
		if err := nameTaken(ctx, tx, name, 0); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO staging_folders (name, dates, created_at) VALUES (?,?,?)",
			name, ds, formatTime(createdAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return StagingFolder{}, fmt.Errorf("create staging folder %q: %w", name, err)
	}
	return d.StagingFolderByID(ctx, id)
}

// rewritePrefix moves every asset whose rel_path lies below from to the
// same place below to, setting state unless it is "", and only for assets
// in onlyState unless that is "". SQLite's substr and length count
// characters, not bytes, so the old prefix is measured in SQL: folder names
// may hold any UTF-8.
func rewritePrefix(ctx context.Context, tx *sql.Tx, from, to string, state, onlyState AssetState, at time.Time) (int64, error) {
	lo, hi, err := prefixRange(from)
	if err != nil {
		return 0, err
	}
	if _, _, err := prefixRange(to); err != nil {
		return 0, err
	}
	q := "UPDATE assets SET rel_path = ? || substr(rel_path, length(?) + 1), updated_at = ?"
	args := []any{to, from, formatTime(at)}
	if state != "" {
		q += ", state = ?"
		args = append(args, string(state))
	}
	q += " WHERE rel_path >= ? AND rel_path < ?"
	args = append(args, lo, hi)
	if onlyState != "" {
		q += " AND state = ?"
		args = append(args, string(onlyState))
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func folderInTx(ctx context.Context, tx *sql.Tx, id int64) (StagingFolder, error) {
	f, err := scanStagingFolder(tx.QueryRowContext(ctx, "SELECT "+stagingCols+" FROM staging_folders WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return StagingFolder{}, ErrNotFound
	}
	return f, err
}

// RenameStagingFolder renames folder id to name and, in the same
// transaction, moves every asset below oldPrefix (Staging/<old>/) to the
// same place below newPrefix (Staging/<name>/). ErrNameTaken when another
// folder already has name.
func (d *DB) RenameStagingFolder(ctx context.Context, id int64, name, oldPrefix, newPrefix string, at time.Time) error {
	err := d.tx(ctx, func(tx *sql.Tx) error {
		f, err := folderInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if f.Name == name {
			return nil
		}
		if err := nameTaken(ctx, tx, name, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE staging_folders SET name = ? WHERE id = ?", name, id); err != nil {
			return err
		}
		_, err = rewritePrefix(ctx, tx, oldPrefix, newPrefix, "", "", at)
		return err
	})
	if err != nil {
		return fmt.Errorf("rename staging folder %d to %q: %w", id, name, err)
	}
	return nil
}

// SetStagingFolderInfo replaces folder id's dates and, unless createdAt is
// zero, its creation time.
func (d *DB) SetStagingFolderInfo(ctx context.Context, id int64, dates []string, createdAt time.Time) error {
	ds, err := normalizeDates(dates)
	if err != nil {
		return fmt.Errorf("staging folder %d: %w", id, err)
	}
	q, args := "UPDATE staging_folders SET dates = ? WHERE id = ?", []any{ds, id}
	if !createdAt.IsZero() {
		q, args = "UPDATE staging_folders SET dates = ?, created_at = ? WHERE id = ?", []any{ds, formatTime(createdAt), id}
	}
	return d.exactlyOne(ctx, fmt.Sprintf("staging folder %d", id), q, args...)
}

// DeleteStagingFolder removes folder id's row (not its assets).
func (d *DB) DeleteStagingFolder(ctx context.Context, id int64) error {
	return d.exactlyOne(ctx, fmt.Sprintf("delete staging folder %d", id), "DELETE FROM staging_folders WHERE id = ?", id)
}

// exactlyOne runs q, which must change one row; none is ErrNotFound.
func (d *DB) exactlyOne(ctx context.Context, what, q string, args ...any) error {
	res, err := d.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

// AssetsByHash returns the assets with exactly these hashes (aliases are
// not followed), keyed by hash. Unknown hashes are absent.
func (d *DB) AssetsByHash(ctx context.Context, shas []string) (map[string]Asset, error) {
	out := map[string]Asset{}
	for start := 0; start < len(shas); start += hashChunk {
		chunk := shas[start:min(start+hashChunk, len(shas))]
		args := make([]any, len(chunk))
		for i, s := range chunk {
			b, err := hexToBlob(s)
			if err != nil {
				return nil, err
			}
			args[i] = b
		}
		q := "SELECT " + assetCols + " FROM assets WHERE sha256 IN (?" + strings.Repeat(",?", len(chunk)-1) + ")"
		rows, err := d.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("assets by hash: %w", err)
		}
		for rows.Next() {
			a, err := scanAsset(rows)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("assets by hash: %w", err), rows.Close())
			}
			out[a.SHA256] = a
		}
		if err := rows.Err(); err != nil {
			return nil, errors.Join(fmt.Errorf("assets by hash: %w", err), rows.Close())
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("assets by hash: %w", err)
		}
	}
	return out, nil
}

// changeAsset updates one asset that must be in one of states (the IN list
// is built from AssetState constants only, never from input). It names the
// state it found when the asset is in another, and returns ErrNotFound for
// an unknown hash.
func (d *DB) changeAsset(ctx context.Context, verb, sha string, states []AssetState, set string, args ...any) error {
	b, err := hexToBlob(sha)
	if err != nil {
		return err
	}
	in := make([]string, len(states))
	for i, s := range states {
		in[i] = "'" + string(s) + "'"
	}
	q := "UPDATE assets SET " + set + " WHERE sha256 = ? AND state IN (" + strings.Join(in, ",") + ")"
	res, err := d.db.ExecContext(ctx, q, append(args, b)...)
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	if n == 1 {
		return nil
	}
	var state string
	err = d.db.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", b).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s %s: %w", verb, sha, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	return fmt.Errorf("%s %s: the asset is %s", verb, sha, state)
}

var onlyStaged = []AssetState{StateStaged}

// MoveAsset records that staged asset sha now lives at relPath.
func (d *DB) MoveAsset(ctx context.Context, sha, relPath string, at time.Time) error {
	return d.changeAsset(ctx, "move", sha, onlyStaged, "rel_path = ?, updated_at = ?", relPath, formatTime(at))
}

// CullAsset records that staged asset sha was deleted on purpose (§6
// "Culled stays remembered"): state culled, no path; fingerprints stay.
func (d *DB) CullAsset(ctx context.Context, sha string, at time.Time) error {
	return d.changeAsset(ctx, "cull", sha, onlyStaged, "state = 'culled', rel_path = NULL, updated_at = ?", formatTime(at))
}

// ArchiveAsset records that staged asset sha was promoted to relPath.
func (d *DB) ArchiveAsset(ctx context.Context, sha, relPath string, at time.Time) error {
	return d.changeAsset(ctx, "archive", sha, onlyStaged, "state = 'archived', rel_path = ?, updated_at = ?", relPath, formatTime(at))
}

// RestageAsset records that the content of a culled or missing asset is in
// staging again, at relPath.
func (d *DB) RestageAsset(ctx context.Context, sha, relPath string, mtime int64, at time.Time) error {
	return d.changeAsset(ctx, "restage", sha, []AssetState{StateCulled, StateMissing},
		"state = 'staged', rel_path = ?, mtime = ?, updated_at = ?", relPath, mtime, formatTime(at))
}

// ArchiveFolder records a promoted folder in one transaction: every staged
// asset below fromPrefix becomes archived at the same place below toPrefix,
// and row id is deleted. It returns how many assets it archived.
func (d *DB) ArchiveFolder(ctx context.Context, id int64, fromPrefix, toPrefix string, at time.Time) (int, error) {
	var n int64
	err := d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := folderInTx(ctx, tx, id); err != nil {
			return err
		}
		var err error
		if n, err = rewritePrefix(ctx, tx, fromPrefix, toPrefix, StateArchived, StateStaged, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM staging_folders WHERE id = ?", id)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("archive staging folder %d: %w", id, err)
	}
	return int(n), nil
}
