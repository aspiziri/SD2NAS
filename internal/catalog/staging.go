package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// StagingFolder is a directory under Staging/ and the capture dates it covers.
type StagingFolder struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Dates     []string  `json:"dates"` // sorted YYYY-MM-DD
	CreatedAt time.Time `json:"created_at"`
}

var dateRE = regexp.MustCompile(`^[0-9]{4}-[0-9]{2}-[0-9]{2}$`)

const stagingCols = "id, name, dates, created_at"

func scanStagingFolder(s scanner) (StagingFolder, error) {
	var (
		f              StagingFolder
		dates, created string
	)
	if err := s.Scan(&f.ID, &f.Name, &dates, &created); err != nil {
		return StagingFolder{}, err
	}
	if err := json.Unmarshal([]byte(dates), &f.Dates); err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder %q: decode dates: %w", f.Name, err)
	}
	t, err := parseTime(created)
	if err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder %q: parse created_at: %w", f.Name, err)
	}
	f.CreatedAt = t
	return f, nil
}

// StagingFolderForDate returns the folder ingest files a capture on date
// (YYYY-MM-DD) into, creating or extending a row as Decision 12 says.
func (d *DB) StagingFolderForDate(ctx context.Context, date string, at time.Time) (StagingFolder, error) {
	if !dateRE.MatchString(date) {
		return StagingFolder{}, fmt.Errorf("staging folder: bad date %q", date)
	}
	var out StagingFolder
	err := d.tx(ctx, func(tx *sql.Tx) error {
		f, err := scanStagingFolder(tx.QueryRowContext(ctx, "SELECT "+stagingCols+` FROM staging_folders f
			WHERE EXISTS (SELECT 1 FROM json_each(f.dates) WHERE json_each.value = ?) ORDER BY id LIMIT 1`, date))
		if err == nil {
			out = f
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		f, err = scanStagingFolder(tx.QueryRowContext(ctx, "SELECT "+stagingCols+" FROM staging_folders WHERE name = ?", date))
		if err == nil {
			f.Dates = append(f.Dates, date)
			slices.Sort(f.Dates)
			b, err := json.Marshal(f.Dates)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, "UPDATE staging_folders SET dates = ? WHERE id = ?", string(b), f.ID); err != nil {
				return err
			}
			out = f
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO staging_folders (name, dates, created_at) VALUES (?,?,?)",
			date, `["`+date+`"]`, formatTime(at))
		if err != nil {
			return err
		}
		id, err := res.LastInsertId()
		if err != nil {
			return err
		}
		out = StagingFolder{ID: id, Name: date, Dates: []string{date}, CreatedAt: at.UTC().Truncate(time.Millisecond)}
		return nil
	})
	if err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder for %s: %w", date, err)
	}
	return out, nil
}

// StagingFolderWithDate is StagingFolderForDate's two lookups without
// writing: the folder whose dates contain date (lowest id), else the folder
// named exactly date, returned with its dates unchanged (the caller adds
// date). false when neither exists.
func (d *DB) StagingFolderWithDate(ctx context.Context, date string) (StagingFolder, bool, error) {
	if !dateRE.MatchString(date) {
		return StagingFolder{}, false, fmt.Errorf("staging folder: bad date %q", date)
	}
	for _, q := range []string{
		"SELECT " + stagingCols + ` FROM staging_folders f
			WHERE EXISTS (SELECT 1 FROM json_each(f.dates) WHERE json_each.value = ?) ORDER BY id LIMIT 1`,
		"SELECT " + stagingCols + " FROM staging_folders WHERE name = ?",
	} {
		f, err := scanStagingFolder(d.db.QueryRowContext(ctx, q, date))
		if err == nil {
			return f, true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return StagingFolder{}, false, fmt.Errorf("staging folder with %s: %w", date, err)
		}
	}
	return StagingFolder{}, false, nil
}

// StagingFolders returns every folder ordered by name.
func (d *DB) StagingFolders(ctx context.Context) ([]StagingFolder, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+stagingCols+" FROM staging_folders ORDER BY name")
	if err != nil {
		return nil, fmt.Errorf("staging folders: %w", err)
	}
	var out []StagingFolder
	for rows.Next() {
		f, err := scanStagingFolder(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// StagingFolderByID returns the folder with id, or ErrNotFound.
func (d *DB) StagingFolderByID(ctx context.Context, id int64) (StagingFolder, error) {
	f, err := scanStagingFolder(d.db.QueryRowContext(ctx, "SELECT "+stagingCols+" FROM staging_folders WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return StagingFolder{}, fmt.Errorf("staging folder %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return StagingFolder{}, fmt.Errorf("staging folder %d: %w", id, err)
	}
	return f, nil
}

// prefixRange is [prefix, prefix with its final "/" replaced by "0").
// "0" is the byte after "/", so the range holds exactly the paths below
// prefix, as a range scan on the assets_path index with no LIKE wildcards.
func prefixRange(prefix string) (lo, hi string, err error) {
	if len(prefix) < 2 || !strings.HasSuffix(prefix, "/") {
		return "", "", fmt.Errorf("catalog: path prefix %q must be a folder ending in /", prefix)
	}
	return prefix, prefix[:len(prefix)-1] + "0", nil
}

// AssetsUnder returns the assets in state whose rel_path lies below prefix (a
// folder path ending in "/"), ordered by rel_path in byte order.
func (d *DB) AssetsUnder(ctx context.Context, prefix string, state AssetState) ([]Asset, error) {
	lo, hi, err := prefixRange(prefix)
	if err != nil {
		return nil, fmt.Errorf("assets under %s: %w", prefix, err)
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+assetCols+
		" FROM assets WHERE rel_path >= ? AND rel_path < ? AND state = ? ORDER BY rel_path", lo, hi, string(state))
	if err != nil {
		return nil, fmt.Errorf("assets under %s: %w", prefix, err)
	}
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("assets under %s: %w", prefix, err), rows.Close())
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("assets under %s: %w", prefix, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("assets under %s: %w", prefix, err)
	}
	return out, nil
}

// ClockWarningsUnder returns the clock warnings of the assets AssetsUnder
// would return, ordered by the asset's rel_path and the warning kind.
func (d *DB) ClockWarningsUnder(ctx context.Context, prefix string, state AssetState) ([]ClockWarning, error) {
	lo, hi, err := prefixRange(prefix)
	if err != nil {
		return nil, fmt.Errorf("clock warnings under %s: %w", prefix, err)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT w.card_path, w.sha256, w.kind, w.detail
		FROM clock_warnings w JOIN assets a ON a.sha256 = w.sha256
		WHERE a.state = ? AND a.rel_path >= ? AND a.rel_path < ? ORDER BY a.rel_path, w.kind`, string(state), lo, hi)
	if err != nil {
		return nil, fmt.Errorf("clock warnings under %s: %w", prefix, err)
	}
	var out []ClockWarning
	for rows.Next() {
		var (
			w    ClockWarning
			sha  []byte
			kind string
		)
		if err := rows.Scan(&w.CardPath, &sha, &kind, &w.Detail); err != nil {
			return nil, errors.Join(fmt.Errorf("clock warnings under %s: %w", prefix, err), rows.Close())
		}
		w.Kind = ClockWarningKind(kind)
		w.SHA256 = blobToHex(sha)
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("clock warnings under %s: %w", prefix, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("clock warnings under %s: %w", prefix, err)
	}
	return out, nil
}

// ArchivedCopiesUnder returns, for the staged assets under prefix whose
// content the archive indexer also found in Archive/, the first such
// archive path, by hash.
func (d *DB) ArchivedCopiesUnder(ctx context.Context, prefix string) (map[string]string, error) {
	lo, hi, err := prefixRange(prefix)
	if err != nil {
		return nil, fmt.Errorf("archive copies under %s: %w", prefix, err)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT a.sha256, MIN(c.rel_path)
		FROM archive_copies c JOIN assets a ON a.sha256 = c.sha256
		WHERE a.state = 'staged' AND a.rel_path >= ? AND a.rel_path < ? GROUP BY a.sha256`, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("archive copies under %s: %w", prefix, err)
	}
	out := map[string]string{}
	for rows.Next() {
		var (
			sha []byte
			rel string
		)
		if err := rows.Scan(&sha, &rel); err != nil {
			return nil, errors.Join(fmt.Errorf("archive copies under %s: %w", prefix, err), rows.Close())
		}
		out[blobToHex(sha)] = rel
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("archive copies under %s: %w", prefix, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("archive copies under %s: %w", prefix, err)
	}
	return out, nil
}
