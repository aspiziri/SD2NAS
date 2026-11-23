package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// AssetState is where an asset's content currently lives.
type AssetState string

const (
	StateStaged   AssetState = "staged"
	StateArchived AssetState = "archived"
	StateCulled   AssetState = "culled"
	StateMissing  AssetState = "missing"
)

// CaptureLayout is how assets.capture_time stores the naming wall clock.
const CaptureLayout = "2006-01-02T15:04:05"

// Asset is one unique file content.
type Asset struct {
	SHA256      string     `json:"sha256"` // lowercase hex
	Size        int64      `json:"size"`
	Kind        media.Kind `json:"kind"`
	CaptureTime time.Time  `json:"capture_time,omitzero"` // wall clock; stored without zone, read back in time.Local; zero = NULL
	TZOffset    string     `json:"tz_offset,omitempty"`
	State       AssetState `json:"state"`
	RelPath     string     `json:"rel_path,omitempty"`     // "" = NULL
	Mtime       int64      `json:"mtime,omitempty"`        // UnixNano; 0 = NULL
	FirstIngest int64      `json:"first_ingest,omitempty"` // 0 = NULL
	UpdatedAt   time.Time  `json:"updated_at"`
}

const assetCols = "sha256, size, kind, capture_time, tz_offset, state, rel_path, mtime, first_ingest, updated_at"

type scanner interface{ Scan(...any) error }

func scanAsset(s scanner) (Asset, error) {
	var (
		a       Asset
		sha     []byte
		kind    string
		state   string
		capture sql.NullString
		relPath sql.NullString
		mtime   sql.NullInt64
		first   sql.NullInt64
		updated string
	)
	if err := s.Scan(&sha, &a.Size, &kind, &capture, &a.TZOffset, &state, &relPath, &mtime, &first, &updated); err != nil {
		return Asset{}, err
	}
	a.SHA256 = blobToHex(sha)
	a.Kind = media.Kind(kind)
	a.State = AssetState(state)
	if capture.Valid {
		t, err := time.ParseInLocation(CaptureLayout, capture.String, time.Local)
		if err != nil {
			return Asset{}, fmt.Errorf("asset %s: parse capture_time: %w", a.SHA256, err)
		}
		a.CaptureTime = t
	}
	a.RelPath = relPath.String
	a.Mtime = mtime.Int64
	a.FirstIngest = first.Int64
	t, err := parseTime(updated)
	if err != nil {
		return Asset{}, fmt.Errorf("asset %s: parse updated_at: %w", a.SHA256, err)
	}
	a.UpdatedAt = t
	return a, nil
}

func insertAsset(ctx context.Context, tx *sql.Tx, a Asset) error {
	sha, err := hexToBlob(a.SHA256)
	if err != nil {
		return err
	}
	if a.UpdatedAt.IsZero() {
		return fmt.Errorf("insert asset %s: UpdatedAt is zero", a.SHA256)
	}
	var capture, relPath, mtime, first any
	if !a.CaptureTime.IsZero() {
		capture = a.CaptureTime.Format(CaptureLayout)
	}
	if a.RelPath != "" {
		relPath = a.RelPath
	}
	if a.Mtime != 0 {
		mtime = a.Mtime
	}
	if a.FirstIngest != 0 {
		first = a.FirstIngest
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO assets ("+assetCols+") VALUES (?,?,?,?,?,?,?,?,?,?)",
		sha, a.Size, string(a.Kind), capture, a.TZOffset, string(a.State), relPath, mtime, first, formatTime(a.UpdatedAt))
	if err != nil {
		return fmt.Errorf("insert asset %s: %w", a.SHA256, err)
	}
	return nil
}

func insertFingerprint(ctx context.Context, tx *sql.Tx, quickFP, sha string) error {
	fp, err := hexToBlob(quickFP)
	if err != nil {
		return err
	}
	s, err := hexToBlob(sha)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO fingerprints (quick_fp, sha256) VALUES (?,?)", fp, s); err != nil {
		return fmt.Errorf("insert fingerprint %s: %w", quickFP, err)
	}
	return nil
}

// InsertAsset stores a and, unless quickFP is empty, its fingerprint, in one
// transaction.
func (d *DB) InsertAsset(ctx context.Context, a Asset, quickFP string) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := insertAsset(ctx, tx, a); err != nil {
			return err
		}
		if quickFP == "" {
			return nil
		}
		return insertFingerprint(ctx, tx, quickFP, a.SHA256)
	})
}

// AssetsByFingerprint returns the assets sharing quickFP, ordered by sha256;
// nil when there are none.
func (d *DB) AssetsByFingerprint(ctx context.Context, quickFP string) ([]Asset, error) {
	fp, err := hexToBlob(quickFP)
	if err != nil {
		return nil, err
	}
	rows, err := d.db.QueryContext(ctx, `SELECT a.sha256, a.size, a.kind, a.capture_time, a.tz_offset, a.state,
		a.rel_path, a.mtime, a.first_ingest, a.updated_at
		FROM fingerprints f JOIN assets a ON a.sha256 = f.sha256 WHERE f.quick_fp = ? ORDER BY a.sha256`, fp)
	if err != nil {
		return nil, fmt.Errorf("assets by fingerprint %s: %w", quickFP, err)
	}
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, errors.Join(err, rows.Close())
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(err, rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return out, nil
}

// AssetBySHA returns the asset with content hash sha, following an alias one
// level. It returns ErrNotFound when neither exists.
func (d *DB) AssetBySHA(ctx context.Context, sha string) (Asset, error) {
	blob, err := hexToBlob(sha)
	if err != nil {
		return Asset{}, err
	}
	const q = "SELECT " + assetCols + " FROM assets WHERE sha256 = ?"
	a, err := scanAsset(d.db.QueryRowContext(ctx, q, blob))
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Asset{}, fmt.Errorf("asset %s: %w", sha, err)
	}
	var target []byte
	err = d.db.QueryRowContext(ctx, "SELECT sha256 FROM aliases WHERE old_sha256 = ?", blob).Scan(&target)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, fmt.Errorf("asset %s: %w", sha, ErrNotFound)
	}
	if err != nil {
		return Asset{}, fmt.Errorf("alias %s: %w", sha, err)
	}
	a, err = scanAsset(d.db.QueryRowContext(ctx, q, target))
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, fmt.Errorf("asset %s: %w", sha, ErrNotFound)
	}
	if err != nil {
		return Asset{}, fmt.Errorf("asset %s: %w", blobToHex(target), err)
	}
	return a, nil
}

// AddFingerprint links quickFP to sha; adding an existing link is a no-op.
func (d *DB) AddFingerprint(ctx context.Context, quickFP, sha string) error {
	return d.tx(ctx, func(tx *sql.Tx) error { return insertFingerprint(ctx, tx, quickFP, sha) })
}

// AddAlias records that content oldSHA now lives on as asset sha.
func (d *DB) AddAlias(ctx context.Context, oldSHA, sha string, at time.Time) error {
	o, err := hexToBlob(oldSHA)
	if err != nil {
		return err
	}
	s, err := hexToBlob(sha)
	if err != nil {
		return err
	}
	_, err = d.db.ExecContext(ctx, "INSERT INTO aliases (old_sha256, sha256, seen_at) VALUES (?,?,?)", o, s, formatTime(at))
	if err != nil {
		return fmt.Errorf("add alias %s: %w", oldSHA, err)
	}
	return nil
}
