package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ArchiveRoot is the rel_path prefix of the archive (§3).
const ArchiveRoot = "Archive/"

// ArchiveEntry is an archived or missing asset under Archive/: what a scan compares the disk with.
type ArchiveEntry struct {
	SHA256    string
	RelPath   string
	Size      int64
	Mtime     int64
	State     AssetState // archived | missing
	UpdatedAt time.Time
	CheckedAt time.Time // zero: never read by the indexer
}

// ArchiveCopy is another archive path holding an asset's content.
type ArchiveCopy struct {
	RelPath    string
	SHA256     string
	Size       int64
	Mtime      int64
	CheckedAt  time.Time
	AssetState AssetState // the asset's state now; "" when no asset has this hash any more
}

// ArchiveChange is an archive file whose content changed in place, until accepted.
type ArchiveChange struct {
	ID        int64     `json:"id"`
	RelPath   string    `json:"rel_path"`
	OldSHA256 string    `json:"old_sha256"`
	SHA256    string    `json:"sha256"`
	OldSize   int64     `json:"old_size"`
	Size      int64     `json:"size"`
	IndexedAt time.Time `json:"indexed_at"`
	ChangedAt time.Time `json:"changed_at"`
}

// MissingFile is an archive file the index knows but the disk no longer
// has, or one of those the owner ignored.
type MissingFile struct {
	SHA256  string    `json:"sha256"`
	RelPath string    `json:"rel_path"`
	Size    int64     `json:"size"`
	Since   time.Time `json:"since"` // updated_at: when it was marked missing
}

// RecentFolder is a top-level archive folder promoted to lately.
type RecentFolder struct {
	Folder string    `json:"folder"` // "Archive/<first path element>"
	Files  int       `json:"files"`
	At     time.Time `json:"at"` // newest promoted_at of its files
}

// ArchiveCounts are the numbers on the archive index page.
type ArchiveCounts struct {
	Files    int   // archived assets under Archive/
	Bytes    int64 // their sizes
	Missing  int
	Ignored  int // missing files the owner ignored
	Changed  int // archive_changes rows
	Copies   int
	Imported int // assets with a first_ingest, any state ("files in ingest history")
}

// ArchiveScan is one indexer pass (or one backup) as recorded in archive_scans.
type ArchiveScan struct {
	ID           int64
	Kind         string // manual | nightly | backup
	Reread       bool
	RereadBefore time.Time // zero unless Reread
	StartedAt    time.Time
	FinishedAt   time.Time // zero: unfinished
	Complete     bool
	Result       string // JSON written by internal/indexer
}

// scanKeep is how many finished pass rows, and how many backup rows, are kept.
const scanKeep = 30

func optTime(ns sql.NullString) (time.Time, error) {
	if !ns.Valid {
		return time.Time{}, nil
	}
	return parseTime(ns.String)
}

func optTimeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return formatTime(t)
}

const archiveEntryCols = "sha256, size, mtime, state, rel_path, updated_at, checked_at"

func scanArchiveEntry(s scanner) (ArchiveEntry, error) {
	var (
		e                ArchiveEntry
		sha              []byte
		mtime            sql.NullInt64
		state            string
		updated, checked sql.NullString
	)
	var rel sql.NullString
	if err := s.Scan(&sha, &e.Size, &mtime, &state, &rel, &updated, &checked); err != nil {
		return ArchiveEntry{}, err
	}
	e.SHA256 = blobToHex(sha)
	e.RelPath = rel.String
	e.Mtime = mtime.Int64
	e.State = AssetState(state)
	var err error
	if e.UpdatedAt, err = optTime(updated); err != nil {
		return ArchiveEntry{}, fmt.Errorf("asset %s: parse updated_at: %w", e.SHA256, err)
	}
	if e.CheckedAt, err = optTime(checked); err != nil {
		return ArchiveEntry{}, fmt.Errorf("asset %s: parse checked_at: %w", e.SHA256, err)
	}
	return e, nil
}

// ArchiveEntries returns every archived or missing asset under Archive/, by rel_path.
func (d *DB) ArchiveEntries(ctx context.Context) ([]ArchiveEntry, error) {
	lo, hi, err := prefixRange(ArchiveRoot)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, "SELECT "+archiveEntryCols+
		" FROM assets WHERE state IN ('archived','missing') AND rel_path >= ? AND rel_path < ? ORDER BY rel_path", lo, hi)
	if err != nil {
		return nil, fmt.Errorf("archive entries: %w", err)
	}
	var out []ArchiveEntry
	for rows.Next() {
		e, err := scanArchiveEntry(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("archive entries: %w", err), rows.Close())
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("archive entries: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("archive entries: %w", err)
	}
	return out, nil
}

// ArchiveEntryAt returns the archived asset at exactly relPath; ErrNotFound when none.
func (d *DB) ArchiveEntryAt(ctx context.Context, relPath string) (ArchiveEntry, error) {
	e, err := scanArchiveEntry(d.db.QueryRowContext(ctx, "SELECT "+archiveEntryCols+
		" FROM assets WHERE state = 'archived' AND rel_path = ?", relPath))
	if errors.Is(err, sql.ErrNoRows) {
		return ArchiveEntry{}, fmt.Errorf("archive entry %s: %w", relPath, ErrNotFound)
	}
	if err != nil {
		return ArchiveEntry{}, fmt.Errorf("archive entry %s: %w", relPath, err)
	}
	return e, nil
}

// ArchiveCopies returns every archive_copies row with its asset's current state, by rel_path.
func (d *DB) ArchiveCopies(ctx context.Context) ([]ArchiveCopy, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT c.rel_path, c.sha256, c.size, c.mtime, c.checked_at, a.state
		FROM archive_copies c LEFT JOIN assets a ON a.sha256 = c.sha256 ORDER BY c.rel_path`)
	if err != nil {
		return nil, fmt.Errorf("archive copies: %w", err)
	}
	var out []ArchiveCopy
	for rows.Next() {
		var (
			c       ArchiveCopy
			sha     []byte
			checked string
			state   sql.NullString
		)
		if err := rows.Scan(&c.RelPath, &sha, &c.Size, &c.Mtime, &checked, &state); err != nil {
			return nil, errors.Join(fmt.Errorf("archive copies: %w", err), rows.Close())
		}
		c.SHA256 = blobToHex(sha)
		c.AssetState = AssetState(state.String)
		t, err := parseTime(checked)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("archive copy %s: parse checked_at: %w", c.RelPath, err), rows.Close())
		}
		c.CheckedAt = t
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("archive copies: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("archive copies: %w", err)
	}
	return out, nil
}

// IndexAsset is InsertAsset plus checked_at = checkedAt, in one transaction.
// Content that was an alias (an earlier version of an asset changed in place)
// is an asset of its own from now on: its alias row goes. Fingerprints stay,
// so the edited asset still matches the old content's fingerprint.
func (d *DB) IndexAsset(ctx context.Context, a Asset, quickFP string, checkedAt time.Time) error {
	sha, err := hexToBlob(a.SHA256)
	if err != nil {
		return err
	}
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := insertAsset(ctx, tx, a); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE assets SET checked_at = ? WHERE sha256 = ?", formatTime(checkedAt), sha); err != nil {
			return fmt.Errorf("index asset %s: %w", a.SHA256, err)
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM aliases WHERE old_sha256 = ?", sha); err != nil {
			return fmt.Errorf("index asset %s: %w", a.SHA256, err)
		}
		if quickFP == "" {
			return nil
		}
		return insertFingerprint(ctx, tx, quickFP, a.SHA256)
	})
}

// SetArchivePath records that the content of asset sha is at relPath: a move,
// a touch, a file that reappeared or one recovered after a cull. It never
// touches promoted_at. A staged asset is ErrWrongState.
func (d *DB) SetArchivePath(ctx context.Context, sha, relPath string, mtime int64, at time.Time) error {
	return d.changeAsset(ctx, "set archive path", sha, []AssetState{StateArchived, StateMissing, StateCulled},
		"state = 'archived', rel_path = ?, mtime = ?, updated_at = ?, checked_at = ?",
		relPath, mtime, formatTime(at), formatTime(at))
}

// MarkMissing records that archived asset sha is no longer on disk; rel_path stays.
func (d *DB) MarkMissing(ctx context.Context, sha string, at time.Time) error {
	return d.changeAsset(ctx, "mark missing", sha, []AssetState{StateArchived}, "state = 'missing', updated_at = ?", formatTime(at))
}

// RecordArchiveChange records that the file behind old changed in place to
// content sha (quickFP: its fingerprint, "" for none): the new content becomes
// the asset, the old one an alias, and the change is listed until accepted.
func (d *DB) RecordArchiveChange(ctx context.Context, old ArchiveEntry, sha, quickFP string, size, mtime int64, at time.Time) error {
	oldB, err := hexToBlob(old.SHA256)
	if err != nil {
		return err
	}
	newB, err := hexToBlob(sha)
	if err != nil {
		return err
	}
	var fpB []byte
	if quickFP != "" {
		if fpB, err = hexToBlob(quickFP); err != nil {
			return err
		}
	}
	if old.SHA256 == sha {
		return fmt.Errorf("record archive change %s: the content did not change", old.RelPath)
	}
	rel, ts := old.RelPath, formatTime(at)
	err = d.tx(ctx, func(tx *sql.Tx) error {
		// 1. The new content becomes the asset.
		res, err := tx.ExecContext(ctx, `INSERT INTO assets (sha256, size, kind, capture_time, tz_offset, state, rel_path, mtime,
				first_ingest, updated_at, checked_at, promoted_at)
			SELECT ?, ?, kind, capture_time, tz_offset, 'archived', ?, ?, first_ingest, ?, ?, promoted_at
			FROM assets WHERE sha256 = ? AND state = 'archived'`, newB, size, rel, mtime, ts, ts, oldB)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var state string
			err := tx.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", oldB).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			return &WrongStateError{Verb: "old content", SHA256: old.SHA256, State: AssetState(state)}
		}
		// 2. Fingerprints: the old content's values find the new asset.
		if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO fingerprints (quick_fp, sha256) SELECT quick_fp, ? FROM fingerprints WHERE sha256 = ?", newB, oldB); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM fingerprints WHERE sha256 = ?", oldB); err != nil {
			return err
		}
		if fpB != nil {
			if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO fingerprints (quick_fp, sha256) VALUES (?, ?)", fpB, newB); err != nil {
				return err
			}
		}
		// 3. Aliases: the new hash may be an earlier version of some asset (a revert).
		for _, s := range []struct {
			q    string
			args []any
		}{
			{"DELETE FROM aliases WHERE old_sha256 = ?", []any{newB}},
			{"UPDATE aliases SET sha256 = ? WHERE sha256 = ?", []any{newB, oldB}},
			{"INSERT INTO aliases (old_sha256, sha256, seen_at) VALUES (?, ?, ?)", []any{oldB, newB, ts}},
			// 4. History and warnings follow the content.
			{"UPDATE ingest_files SET sha256 = ? WHERE sha256 = ?", []any{newB, oldB}},
			{"UPDATE clock_warnings SET sha256 = ? WHERE sha256 = ?", []any{newB, oldB}},
			{"DELETE FROM archive_copies WHERE sha256 = ?", []any{oldB}},
			// 5. The old row.
			{"DELETE FROM assets WHERE sha256 = ?", []any{oldB}},
		} {
			if _, err := tx.ExecContext(ctx, s.q, s.args...); err != nil {
				return err
			}
		}
		// 6. The change entry: a re-change keeps the first old hash, size and indexed time.
		firstOld, firstSize := old.SHA256, old.Size
		indexed := old.CheckedAt
		if indexed.IsZero() {
			indexed = old.UpdatedAt
		}
		var exOld []byte
		var exSize int64
		var exIndexed string
		err = tx.QueryRowContext(ctx, "SELECT old_sha256, old_size, indexed_at FROM archive_changes WHERE rel_path = ?", rel).
			Scan(&exOld, &exSize, &exIndexed)
		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			firstOld, firstSize = blobToHex(exOld), exSize
			if indexed, err = parseTime(exIndexed); err != nil {
				return fmt.Errorf("parse indexed_at: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM archive_changes WHERE rel_path = ?", rel); err != nil {
			return err
		}
		if firstOld == sha { // changed and then reverted
			return nil
		}
		firstB, err := hexToBlob(firstOld)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO archive_changes (rel_path, old_sha256, sha256, old_size, size, indexed_at, changed_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, rel, firstB, newB, firstSize, size, formatTime(indexed), ts)
		return err
	})
	if err != nil {
		return fmt.Errorf("record archive change %s: %w", rel, err)
	}
	return nil
}

// PutArchiveCopy inserts or replaces the copy at c.RelPath.
func (d *DB) PutArchiveCopy(ctx context.Context, c ArchiveCopy) error {
	sha, err := hexToBlob(c.SHA256)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx, `INSERT INTO archive_copies (rel_path, sha256, size, mtime, checked_at) VALUES (?,?,?,?,?)
		ON CONFLICT(rel_path) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size,
		mtime = excluded.mtime, checked_at = excluded.checked_at`,
		c.RelPath, sha, c.Size, c.Mtime, formatTime(c.CheckedAt))
	if err != nil {
		return fmt.Errorf("put archive copy %s: %w", c.RelPath, err)
	}
	return nil
}

// DeleteArchiveCopy removes the copy at relPath; an absent one is not an error.
func (d *DB) DeleteArchiveCopy(ctx context.Context, relPath string) error {
	if _, err := d.db.ExecContext(ctx, "DELETE FROM archive_copies WHERE rel_path = ?", relPath); err != nil {
		return fmt.Errorf("delete archive copy %s: %w", relPath, err)
	}
	return nil
}

// ArchiveChanges returns up to limit changes ordered by id, the total number
// of changes, and the highest id among the returned ones (0 when none).
func (d *DB) ArchiveChanges(ctx context.Context, limit int) (changes []ArchiveChange, total int, latestID int64, err error) {
	err = d.tx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM archive_changes").Scan(&total); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT id, rel_path, old_sha256, sha256, old_size, size, indexed_at, changed_at
			FROM archive_changes ORDER BY id LIMIT ?`, limit)
		if err != nil {
			return err
		}
		for rows.Next() {
			var (
				c                ArchiveChange
				oldSHA, sha      []byte
				indexed, changed string
			)
			if err := rows.Scan(&c.ID, &c.RelPath, &oldSHA, &sha, &c.OldSize, &c.Size, &indexed, &changed); err != nil {
				return errors.Join(err, rows.Close())
			}
			c.OldSHA256, c.SHA256 = blobToHex(oldSHA), blobToHex(sha)
			if c.IndexedAt, err = parseTime(indexed); err != nil {
				return errors.Join(fmt.Errorf("%s: parse indexed_at: %w", c.RelPath, err), rows.Close())
			}
			if c.ChangedAt, err = parseTime(changed); err != nil {
				return errors.Join(fmt.Errorf("%s: parse changed_at: %w", c.RelPath, err), rows.Close())
			}
			changes = append(changes, c)
			latestID = c.ID
		}
		if err := rows.Err(); err != nil {
			return errors.Join(err, rows.Close())
		}
		return rows.Close()
	})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("archive changes: %w", err)
	}
	return changes, total, latestID, nil
}

// AcceptArchiveChanges deletes every change with id <= upTo and returns how many.
func (d *DB) AcceptArchiveChanges(ctx context.Context, upTo int64) (int, error) {
	if upTo < 1 {
		return 0, fmt.Errorf("accept archive changes: upTo must be positive, got %d", upTo)
	}
	res, err := d.db.ExecContext(ctx, "DELETE FROM archive_changes WHERE id <= ?", upTo)
	if err != nil {
		return 0, fmt.Errorf("accept archive changes: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("accept archive changes: %w", err)
	}
	return int(n), nil
}

// ArchiveCounts returns the numbers for the archive index page.
func (d *DB) ArchiveCounts(ctx context.Context) (ArchiveCounts, error) {
	lo, hi, err := prefixRange(ArchiveRoot)
	if err != nil {
		return ArchiveCounts{}, err
	}
	var c ArchiveCounts
	for _, q := range []struct {
		dst  any
		sql  string
		args []any
	}{
		{&c.Files, "SELECT COUNT(*) FROM assets WHERE state = 'archived' AND rel_path >= ? AND rel_path < ?", []any{lo, hi}},
		{&c.Bytes, "SELECT COALESCE(SUM(size), 0) FROM assets WHERE state = 'archived' AND rel_path >= ? AND rel_path < ?", []any{lo, hi}},
		{&c.Missing, "SELECT COUNT(*) FROM assets WHERE state = 'missing' AND rel_path >= ? AND rel_path < ?", []any{lo, hi}},
		{&c.Ignored, "SELECT COUNT(*) FROM assets WHERE state = 'culled' AND rel_path >= ? AND rel_path < ?", []any{lo, hi}},
		{&c.Changed, "SELECT COUNT(*) FROM archive_changes", nil},
		// duplicates of archived content only: a staged file's copy is shown in Staging
		{&c.Copies, "SELECT COUNT(*) FROM archive_copies c JOIN assets a ON a.sha256 = c.sha256 WHERE a.state = 'archived'", nil},
		{&c.Imported, "SELECT COUNT(*) FROM assets WHERE first_ingest IS NOT NULL", nil},
	} {
		if err := d.db.QueryRowContext(ctx, q.sql, q.args...).Scan(q.dst); err != nil {
			return ArchiveCounts{}, fmt.Errorf("archive counts: %w", err)
		}
	}
	return c, nil
}

// MissingSince counts the assets marked missing after after (updated_at,
// still missing) and returns the newest such updated_at (zero when n is 0).
func (d *DB) MissingSince(ctx context.Context, after time.Time) (n int, latest time.Time, err error) {
	var last string
	// formatTime is fixed width, so text order is time order
	if err := d.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(updated_at), '') FROM assets
		WHERE state = 'missing' AND updated_at > ?`, formatTime(after)).Scan(&n, &last); err != nil {
		return 0, time.Time{}, fmt.Errorf("count missing since %s: %w", formatTime(after), err)
	}
	if last != "" {
		if latest, err = parseTime(last); err != nil {
			return 0, time.Time{}, fmt.Errorf("count missing since %s: parse updated_at %q: %w", formatTime(after), last, err)
		}
	}
	return n, latest, nil
}

// MissingFiles returns up to limit missing files under Archive/, by rel_path.
func (d *DB) MissingFiles(ctx context.Context, limit int) ([]MissingFile, error) {
	return d.archiveFilesIn(ctx, "missing files", StateMissing, limit)
}

// IgnoredFiles returns up to limit ignored missing files under Archive/, by rel_path.
func (d *DB) IgnoredFiles(ctx context.Context, limit int) ([]MissingFile, error) {
	return d.archiveFilesIn(ctx, "ignored files", StateCulled, limit)
}

func (d *DB) archiveFilesIn(ctx context.Context, what string, state AssetState, limit int) ([]MissingFile, error) {
	lo, hi, err := prefixRange(ArchiveRoot)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT sha256, rel_path, size, updated_at FROM assets
		WHERE state = ? AND rel_path >= ? AND rel_path < ? ORDER BY rel_path LIMIT ?`, string(state), lo, hi, limit)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	var out []MissingFile
	for rows.Next() {
		var (
			m     MissingFile
			sha   []byte
			since string
		)
		if err := rows.Scan(&sha, &m.RelPath, &m.Size, &since); err != nil {
			return nil, errors.Join(fmt.Errorf("%s: %w", what, err), rows.Close())
		}
		m.SHA256 = blobToHex(sha)
		if m.Since, err = parseTime(since); err != nil {
			return nil, errors.Join(fmt.Errorf("%s: %s: parse updated_at: %w", what, m.RelPath, err), rows.Close())
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("%s: %w", what, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// IgnoreMissing records that missing archive file sha was deleted on purpose:
// it becomes culled, as a cull would have left it, but keeps its rel_path, so
// it is listed as ignored and UnignoreMissing can undo it. A card holding the
// content counts it as seen before, and a pending "Re-import missing files"
// request for it goes. updated_at stays the time it was marked missing, so
// undoing it raises no new missing-files notice. An asset in another state,
// or missing outside Archive/, is a *WrongStateError.
func (d *DB) IgnoreMissing(ctx context.Context, sha string) error {
	return d.setIgnored(ctx, "ignore missing", sha, StateMissing, StateCulled)
}

// UnignoreMissing makes ignored file sha missing again.
func (d *DB) UnignoreMissing(ctx context.Context, sha string) error {
	return d.setIgnored(ctx, "un-ignore missing", sha, StateCulled, StateMissing)
}

func (d *DB) setIgnored(ctx context.Context, verb, sha string, from, to AssetState) error {
	b, err := hexToBlob(sha)
	if err != nil {
		return err
	}
	lo, hi, err := prefixRange(ArchiveRoot)
	if err != nil {
		return err
	}
	err = d.tx(ctx, func(tx *sql.Tx) error {
		// A cull from Staging has no rel_path, so only an ignored file matches culled here.
		res, err := tx.ExecContext(ctx, "UPDATE assets SET state = ? WHERE sha256 = ? AND state = ? AND rel_path >= ? AND rel_path < ?",
			string(to), b, string(from), lo, hi)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var state string
			err := tx.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", b).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			return &WrongStateError{Verb: verb, SHA256: sha, State: AssetState(state)}
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM restore_requests WHERE sha256 = ?", b)
		return err
	})
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	return nil
}

// RecentArchiveFolders returns the top-level archive folders with the newest
// promoted files, newest first. 'Archive/' is 8 characters; substr and instr
// count characters, so non-ASCII names are fine.
func (d *DB) RecentArchiveFolders(ctx context.Context, limit int) ([]RecentFolder, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT substr(rel_path, 1, 8 + instr(substr(rel_path, 9), '/') - 1) AS folder, COUNT(*), MAX(promoted_at)
		FROM assets
		WHERE state = 'archived' AND promoted_at IS NOT NULL AND rel_path >= 'Archive/' AND rel_path < 'Archive0'
		  AND instr(substr(rel_path, 9), '/') > 0
		GROUP BY folder ORDER BY MAX(promoted_at) DESC, folder LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("recent archive folders: %w", err)
	}
	var out []RecentFolder
	for rows.Next() {
		var (
			f  RecentFolder
			at string
		)
		if err := rows.Scan(&f.Folder, &f.Files, &at); err != nil {
			return nil, errors.Join(fmt.Errorf("recent archive folders: %w", err), rows.Close())
		}
		if f.At, err = parseTime(at); err != nil {
			return nil, errors.Join(fmt.Errorf("recent archive folder %s: parse promoted_at: %w", f.Folder, err), rows.Close())
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("recent archive folders: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("recent archive folders: %w", err)
	}
	return out, nil
}

// StartArchiveScan records a new pass and returns its id. kind is manual or
// nightly; a non-zero rereadBefore makes it a re-index of files not checked
// since then.
func (d *DB) StartArchiveScan(ctx context.Context, kind string, rereadBefore time.Time, at time.Time) (int64, error) {
	if kind != "manual" && kind != "nightly" {
		return 0, fmt.Errorf("start archive scan: kind must be manual or nightly, got %q", kind)
	}
	res, err := d.db.ExecContext(ctx, "INSERT INTO archive_scans (kind, reread, reread_before, started_at) VALUES (?,?,?,?)",
		kind, !rereadBefore.IsZero(), optTimeArg(rereadBefore), formatTime(at))
	if err != nil {
		return 0, fmt.Errorf("start archive scan: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("start archive scan: %w", err)
	}
	return id, nil
}

// FinishArchiveScan closes pass id and keeps only the newest 30 passes.
func (d *DB) FinishArchiveScan(ctx context.Context, id int64, at time.Time, complete bool, result string) error {
	err := d.tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, "UPDATE archive_scans SET finished_at = ?, complete = ?, result = ? WHERE id = ? AND kind != 'backup'",
			formatTime(at), complete, result, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrNotFound
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM archive_scans WHERE kind != 'backup' AND id NOT IN
			(SELECT id FROM archive_scans WHERE kind != 'backup' ORDER BY id DESC LIMIT ?)`, scanKeep)
		return err
	})
	if err != nil {
		return fmt.Errorf("finish archive scan %d: %w", id, err)
	}
	return nil
}

const archiveScanCols = "id, kind, reread, reread_before, started_at, finished_at, complete, result"

func scanArchiveScan(s scanner) (ArchiveScan, error) {
	var (
		r                       ArchiveScan
		before, started, finish sql.NullString
	)
	if err := s.Scan(&r.ID, &r.Kind, &r.Reread, &before, &started, &finish, &r.Complete, &r.Result); err != nil {
		return ArchiveScan{}, err
	}
	var err error
	if r.RereadBefore, err = optTime(before); err != nil {
		return ArchiveScan{}, fmt.Errorf("scan %d: parse reread_before: %w", r.ID, err)
	}
	if r.StartedAt, err = optTime(started); err != nil {
		return ArchiveScan{}, fmt.Errorf("scan %d: parse started_at: %w", r.ID, err)
	}
	if r.FinishedAt, err = optTime(finish); err != nil {
		return ArchiveScan{}, fmt.Errorf("scan %d: parse finished_at: %w", r.ID, err)
	}
	return r, nil
}

// LastArchiveScan returns the newest finished pass; ErrNotFound when none.
func (d *DB) LastArchiveScan(ctx context.Context) (ArchiveScan, error) {
	r, err := scanArchiveScan(d.db.QueryRowContext(ctx, "SELECT "+archiveScanCols+
		" FROM archive_scans WHERE kind != 'backup' AND finished_at IS NOT NULL ORDER BY id DESC LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return ArchiveScan{}, fmt.Errorf("last archive scan: %w", ErrNotFound)
	}
	if err != nil {
		return ArchiveScan{}, fmt.Errorf("last archive scan: %w", err)
	}
	return r, nil
}

// UnfinishedArchiveScans returns the passes with no finish time, oldest first.
func (d *DB) UnfinishedArchiveScans(ctx context.Context) ([]ArchiveScan, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+archiveScanCols+
		" FROM archive_scans WHERE kind != 'backup' AND finished_at IS NULL ORDER BY id")
	if err != nil {
		return nil, fmt.Errorf("unfinished archive scans: %w", err)
	}
	var out []ArchiveScan
	for rows.Next() {
		r, err := scanArchiveScan(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("unfinished archive scans: %w", err), rows.Close())
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("unfinished archive scans: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("unfinished archive scans: %w", err)
	}
	return out, nil
}

// PendingReread returns the reread_before of an unfinished re-index, or the
// zero time when there is none. The newest pass with reread set decides.
func (d *DB) PendingReread(ctx context.Context) (time.Time, error) {
	var (
		complete bool
		before   sql.NullString
	)
	err := d.db.QueryRowContext(ctx, `SELECT complete, reread_before FROM archive_scans
		WHERE kind != 'backup' AND reread = 1 ORDER BY id DESC LIMIT 1`).Scan(&complete, &before)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("pending re-index: %w", err)
	}
	if complete {
		return time.Time{}, nil
	}
	t, err := optTime(before)
	if err != nil {
		return time.Time{}, fmt.Errorf("pending re-index: parse reread_before: %w", err)
	}
	return t, nil
}

// RecordArchiveBackup records a finished backup and keeps the newest 30.
func (d *DB) RecordArchiveBackup(ctx context.Context, at time.Time, ok bool, result string) error {
	err := d.tx(ctx, func(tx *sql.Tx) error {
		ts := formatTime(at)
		if _, err := tx.ExecContext(ctx, "INSERT INTO archive_scans (kind, started_at, finished_at, complete, result) VALUES ('backup',?,?,?,?)",
			ts, ts, ok, result); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM archive_scans WHERE kind = 'backup' AND id NOT IN
			(SELECT id FROM archive_scans WHERE kind = 'backup' ORDER BY id DESC LIMIT ?)`, scanKeep)
		return err
	})
	if err != nil {
		return fmt.Errorf("record archive backup: %w", err)
	}
	return nil
}

// LastArchiveBackup returns the newest backup row; ErrNotFound when none.
func (d *DB) LastArchiveBackup(ctx context.Context) (ArchiveScan, error) {
	r, err := scanArchiveScan(d.db.QueryRowContext(ctx, "SELECT "+archiveScanCols+
		" FROM archive_scans WHERE kind = 'backup' ORDER BY id DESC LIMIT 1"))
	if errors.Is(err, sql.ErrNoRows) {
		return ArchiveScan{}, fmt.Errorf("last archive backup: %w", ErrNotFound)
	}
	if err != nil {
		return ArchiveScan{}, fmt.Errorf("last archive backup: %w", err)
	}
	return r, nil
}

// Backup writes a consistent copy of the catalog to path with VACUUM INTO.
// SQLite refuses a path that already holds a non-empty file.
func (d *DB) Backup(ctx context.Context, path string) error {
	if _, err := d.db.ExecContext(ctx, "VACUUM INTO ?", path); err != nil {
		return fmt.Errorf("back up catalog to %s: %w", path, err)
	}
	return nil
}
