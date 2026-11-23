package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// IngestStatus is where an ingest is in its life.
type IngestStatus string

const (
	StatusQueued    IngestStatus = "queued"
	StatusRunning   IngestStatus = "running"
	StatusDone      IngestStatus = "done"
	StatusFailed    IngestStatus = "failed"
	StatusRefused   IngestStatus = "refused"
	StatusCancelled IngestStatus = "cancelled"
)

// Terminal reports whether s is a final status.
func (s IngestStatus) Terminal() bool {
	switch s {
	case StatusDone, StatusFailed, StatusRefused, StatusCancelled:
		return true
	}
	return false
}

// Outcome is what happened to one file on the card.
type Outcome string

const (
	OutcomeNew         Outcome = "new"
	OutcomeInArchive   Outcome = "in_archive"
	OutcomeInStaging   Outcome = "in_staging"
	OutcomeSeenBefore  Outcome = "seen_before"
	OutcomeFailed      Outcome = "failed"
	OutcomeUnsupported Outcome = "unsupported"
)

// Counts are the outcomes of the ingest_files rows written for one ingest.
type Counts struct {
	Seen        int `json:"seen"` // every outcome except unsupported
	New         int `json:"new"`
	InArchive   int `json:"in_archive"`
	InStaging   int `json:"in_staging"`
	SeenBefore  int `json:"seen_before"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported"`
}

// Add counts one file with outcome o.
func (c *Counts) Add(o Outcome) {
	if o != OutcomeUnsupported {
		c.Seen++
	}
	switch o {
	case OutcomeNew:
		c.New++
	case OutcomeInArchive:
		c.InArchive++
	case OutcomeInStaging:
		c.InStaging++
	case OutcomeSeenBefore:
		c.SeenBefore++
	case OutcomeFailed:
		c.Failed++
	case OutcomeUnsupported:
		c.Unsupported++
	}
}

// Ingest is one import of one card.
type Ingest struct {
	ID         int64        `json:"id"`
	CardPath   string       `json:"card_path"`
	CardLabel  string       `json:"card_label"`
	CardUUID   string       `json:"card_uuid"`
	CardSize   int64        `json:"card_size"`
	StartedAt  time.Time    `json:"started_at"`
	FinishedAt time.Time    `json:"finished_at,omitzero"`
	Status     IngestStatus `json:"status"`
	Counts     Counts       `json:"counts"`
	// UnsupportedExts are the distinct extensions of the unsupported files,
	// lowercase with the dot ("" for a file with no extension), sorted with
	// "" last. nil when there are none.
	UnsupportedExts []string `json:"unsupported_exts,omitempty"`
	ClockWarnings   int      `json:"clock_warnings"`
	SafeToFormat    bool     `json:"safe_to_format"`
	Error           string   `json:"error,omitempty"`
}

// IngestFile is what happened to one file on the card.
type IngestFile struct {
	CardPath string  `json:"card_path"` // slash-separated, relative to the card root
	Size     int64   `json:"size"`
	QuickFP  string  `json:"quick_fp,omitempty"` // "" = NULL
	SHA256   string  `json:"sha256,omitempty"`   // "" = NULL
	Outcome  Outcome `json:"outcome"`
	Error    string  `json:"error,omitempty"`
	RelPath  string  `json:"rel_path,omitempty"` // read only: rel_path of the asset with SHA256 (LEFT JOIN); ignored on write
}

// ClockWarningKind says what looked wrong with a file's timestamp.
type ClockWarningKind string

const (
	ClockFuture       ClockWarningKind = "future"
	ClockResetDefault ClockWarningKind = "reset_default"
	ClockGPSMismatch  ClockWarningKind = "gps_mismatch"
)

// ClockWarning flags a file whose capture time looks wrong.
type ClockWarning struct {
	CardPath string           `json:"card_path"`
	SHA256   string           `json:"sha256,omitempty"`
	Kind     ClockWarningKind `json:"kind"`
	Detail   string           `json:"detail"`
}

const interruptedMessage = "interrupted: SD2NAS stopped before this import finished"

const ingestCols = "id, card_path, card_label, card_uuid, card_size, started_at, finished_at, status, " +
	"seen, new, in_archive, in_staging, seen_before, failed, unsupported, unsupported_exts, clock_warnings, safe_to_format, error"

func scanIngest(s scanner) (Ingest, error) {
	var (
		in       Ingest
		started  string
		finished sql.NullString
		status   string
		exts     string
		safe     int
	)
	err := s.Scan(&in.ID, &in.CardPath, &in.CardLabel, &in.CardUUID, &in.CardSize, &started, &finished, &status,
		&in.Counts.Seen, &in.Counts.New, &in.Counts.InArchive, &in.Counts.InStaging, &in.Counts.SeenBefore,
		&in.Counts.Failed, &in.Counts.Unsupported, &exts, &in.ClockWarnings, &safe, &in.Error)
	if err != nil {
		return Ingest{}, err
	}
	if exts != "" {
		if err := json.Unmarshal([]byte(exts), &in.UnsupportedExts); err != nil {
			return Ingest{}, fmt.Errorf("ingest %d: parse unsupported_exts: %w", in.ID, err)
		}
		if len(in.UnsupportedExts) == 0 {
			in.UnsupportedExts = nil
		}
	}
	in.Status = IngestStatus(status)
	in.SafeToFormat = safe != 0
	if in.StartedAt, err = parseTime(started); err != nil {
		return Ingest{}, fmt.Errorf("ingest %d: parse started_at: %w", in.ID, err)
	}
	if finished.Valid {
		if in.FinishedAt, err = parseTime(finished.String); err != nil {
			return Ingest{}, fmt.Errorf("ingest %d: parse finished_at: %w", in.ID, err)
		}
	}
	return in, nil
}

// CreateIngest adds a queued ingest for the card at cardPath.
func (d *DB) CreateIngest(ctx context.Context, cardPath, label, uuid string, at time.Time) (Ingest, error) {
	res, err := d.db.ExecContext(ctx,
		"INSERT INTO ingests (card_path, card_label, card_uuid, started_at, status) VALUES (?,?,?,?,?)",
		cardPath, label, uuid, formatTime(at), string(StatusQueued))
	if err != nil {
		return Ingest{}, fmt.Errorf("create ingest: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Ingest{}, fmt.Errorf("create ingest: %w", err)
	}
	return d.Ingest(ctx, id)
}

// StartIngest moves a queued ingest to running and resets StartedAt to at.
func (d *DB) StartIngest(ctx context.Context, id int64, at time.Time) error {
	res, err := d.db.ExecContext(ctx,
		"UPDATE ingests SET status = 'running', started_at = ? WHERE id = ? AND status = 'queued'", formatTime(at), id)
	if err != nil {
		return fmt.Errorf("start ingest %d: %w", id, err)
	}
	return d.requireChanged(ctx, res, id, "start", "is not queued")
}

// requireChanged turns a zero-row UPDATE into ErrNotFound (no such ingest) or
// an error saying why the row was not eligible.
func (d *DB) requireChanged(ctx context.Context, res sql.Result, id int64, verb, why string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s ingest %d: %w", verb, id, err)
	}
	if n > 0 {
		return nil
	}
	var exists int
	err = d.db.QueryRowContext(ctx, "SELECT 1 FROM ingests WHERE id = ?", id).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s ingest %d: %w", verb, id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("%s ingest %d: %w", verb, id, err)
	}
	return fmt.Errorf("%s ingest %d: ingest %s", verb, id, why)
}

// FinishIngest writes in.Status (must be Terminal), FinishedAt (must be
// non-zero), Counts, UnsupportedExts, ClockWarnings, CardSize, SafeToFormat and Error to row
// in.ID, which must be queued or running (ErrNotFound when there is no row).
func (d *DB) FinishIngest(ctx context.Context, in Ingest) error {
	if !in.Status.Terminal() {
		return fmt.Errorf("finish ingest %d: status %q is not terminal", in.ID, in.Status)
	}
	if in.FinishedAt.IsZero() {
		return fmt.Errorf("finish ingest %d: FinishedAt is zero", in.ID)
	}
	safe := 0
	if in.SafeToFormat {
		safe = 1
	}
	exts := ""
	if len(in.UnsupportedExts) > 0 {
		b, err := json.Marshal(in.UnsupportedExts)
		if err != nil {
			return fmt.Errorf("finish ingest %d: unsupported extensions: %w", in.ID, err)
		}
		exts = string(b)
	}
	c := in.Counts
	res, err := d.db.ExecContext(ctx, `UPDATE ingests SET status = ?, finished_at = ?, seen = ?, new = ?,
		in_archive = ?, in_staging = ?, seen_before = ?, failed = ?, unsupported = ?, unsupported_exts = ?,
		clock_warnings = ?, card_size = ?, safe_to_format = ?, error = ?
		WHERE id = ? AND status IN ('queued','running')`,
		string(in.Status), formatTime(in.FinishedAt), c.Seen, c.New, c.InArchive, c.InStaging, c.SeenBefore,
		c.Failed, c.Unsupported, exts, in.ClockWarnings, in.CardSize, safe, in.Error, in.ID)
	if err != nil {
		return fmt.Errorf("finish ingest %d: %w", in.ID, err)
	}
	return d.requireChanged(ctx, res, in.ID, "finish", "is already finished")
}

// Ingest returns the ingest with the given id, or ErrNotFound.
func (d *DB) Ingest(ctx context.Context, id int64) (Ingest, error) {
	in, err := scanIngest(d.db.QueryRowContext(ctx, "SELECT "+ingestCols+" FROM ingests WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return Ingest{}, fmt.Errorf("ingest %d: %w", id, ErrNotFound)
	}
	if err != nil {
		return Ingest{}, fmt.Errorf("ingest %d: %w", id, err)
	}
	return in, nil
}

// Ingests lists ingests newest first. limit <= 0 means 50 and is capped at
// 500; beforeID > 0 returns only ids below it.
func (d *DB) Ingests(ctx context.Context, limit int, beforeID int64) ([]Ingest, error) {
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 500)
	q := "SELECT " + ingestCols + " FROM ingests"
	var args []any
	if beforeID > 0 {
		q += " WHERE id < ?"
		args = append(args, beforeID)
	}
	q += " ORDER BY id DESC LIMIT ?"
	args = append(args, limit)
	rows, err := d.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list ingests: %w", err)
	}
	var out []Ingest
	for rows.Next() {
		in, err := scanIngest(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("list ingests: %w", err), rows.Close())
		}
		out = append(out, in)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("list ingests: %w", err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("list ingests: %w", err)
	}
	return out, nil
}

// FailInterrupted marks every queued or running ingest failed, as at startup
// after a crash. It returns how many rows it changed.
func (d *DB) FailInterrupted(ctx context.Context, at time.Time) (int64, error) {
	res, err := d.db.ExecContext(ctx,
		"UPDATE ingests SET status = 'failed', finished_at = ?, error = ? WHERE status IN ('queued','running')",
		formatTime(at), interruptedMessage)
	if err != nil {
		return 0, fmt.Errorf("fail interrupted ingests: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("fail interrupted ingests: %w", err)
	}
	return n, nil
}

// hexOrNil converts a hex hash to a blob, or nil (SQL NULL) when empty.
func hexOrNil(s string) (any, error) {
	if s == "" {
		return nil, nil
	}
	return hexToBlob(s)
}

func validOutcome(o Outcome) bool {
	switch o {
	case OutcomeNew, OutcomeInArchive, OutcomeInStaging, OutcomeSeenBefore, OutcomeFailed, OutcomeUnsupported:
		return true
	}
	return false
}

func insertIngestFile(ctx context.Context, tx *sql.Tx, ingestID int64, f IngestFile) error {
	if !validOutcome(f.Outcome) {
		return fmt.Errorf("ingest file %s: invalid outcome %q", f.CardPath, f.Outcome)
	}
	fp, err := hexOrNil(f.QuickFP)
	if err != nil {
		return fmt.Errorf("ingest file %s: %w", f.CardPath, err)
	}
	sha, err := hexOrNil(f.SHA256)
	if err != nil {
		return fmt.Errorf("ingest file %s: %w", f.CardPath, err)
	}
	_, err = tx.ExecContext(ctx,
		"INSERT INTO ingest_files (ingest_id, card_path, size, quick_fp, sha256, outcome, error) VALUES (?,?,?,?,?,?,?)",
		ingestID, f.CardPath, f.Size, fp, sha, string(f.Outcome), f.Error)
	if err != nil {
		return fmt.Errorf("ingest file %s: %w", f.CardPath, err)
	}
	return nil
}

// AddIngestFiles records files for ingestID in one transaction.
func (d *DB) AddIngestFiles(ctx context.Context, ingestID int64, files []IngestFile) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		for _, f := range files {
			if err := insertIngestFile(ctx, tx, ingestID, f); err != nil {
				return err
			}
		}
		return nil
	})
}

// IngestFiles returns the files recorded for ingestID in insertion order.
func (d *DB) IngestFiles(ctx context.Context, ingestID int64) ([]IngestFile, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT f.card_path, f.size, f.quick_fp, f.sha256, f.outcome, f.error,
		COALESCE(a.rel_path, '') FROM ingest_files f LEFT JOIN assets a ON a.sha256 = f.sha256
		WHERE f.ingest_id = ? ORDER BY f.rowid`, ingestID)
	if err != nil {
		return nil, fmt.Errorf("ingest files %d: %w", ingestID, err)
	}
	var out []IngestFile
	for rows.Next() {
		var (
			f       IngestFile
			fp, sha []byte
			outcome string
		)
		if err := rows.Scan(&f.CardPath, &f.Size, &fp, &sha, &outcome, &f.Error, &f.RelPath); err != nil {
			return nil, errors.Join(fmt.Errorf("ingest files %d: %w", ingestID, err), rows.Close())
		}
		f.Outcome = Outcome(outcome)
		if fp != nil {
			f.QuickFP = blobToHex(fp)
		}
		if sha != nil {
			f.SHA256 = blobToHex(sha)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("ingest files %d: %w", ingestID, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("ingest files %d: %w", ingestID, err)
	}
	return out, nil
}

// RecordNew, in one transaction, inserts a with FirstIngest = ingestID, adds
// quickFP -> a.SHA256, and writes f (QuickFP and SHA256 taken from quickFP and
// a; f.Outcome must be OutcomeNew or OutcomeInStaging).
func (d *DB) RecordNew(ctx context.Context, ingestID int64, a Asset, quickFP string, f IngestFile) error {
	if f.Outcome != OutcomeNew && f.Outcome != OutcomeInStaging {
		return fmt.Errorf("record new %s: outcome %q must be %q or %q", a.SHA256, f.Outcome, OutcomeNew, OutcomeInStaging)
	}
	a.FirstIngest = ingestID
	f.QuickFP = quickFP
	f.SHA256 = a.SHA256
	return d.tx(ctx, func(tx *sql.Tx) error {
		if err := insertAsset(ctx, tx, a); err != nil {
			return err
		}
		if err := insertFingerprint(ctx, tx, quickFP, a.SHA256); err != nil {
			return err
		}
		return insertIngestFile(ctx, tx, ingestID, f)
	})
}

// AddClockWarnings records ws for ingestID in one transaction.
func (d *DB) AddClockWarnings(ctx context.Context, ingestID int64, ws []ClockWarning) error {
	return d.tx(ctx, func(tx *sql.Tx) error {
		for _, w := range ws {
			sha, err := hexOrNil(w.SHA256)
			if err != nil {
				return fmt.Errorf("clock warning %s: %w", w.CardPath, err)
			}
			_, err = tx.ExecContext(ctx,
				"INSERT INTO clock_warnings (ingest_id, card_path, sha256, kind, detail) VALUES (?,?,?,?,?)",
				ingestID, w.CardPath, sha, string(w.Kind), w.Detail)
			if err != nil {
				return fmt.Errorf("clock warning %s: %w", w.CardPath, err)
			}
		}
		return nil
	})
}

// ClockWarnings returns the warnings recorded for ingestID in insertion order.
func (d *DB) ClockWarnings(ctx context.Context, ingestID int64) ([]ClockWarning, error) {
	rows, err := d.db.QueryContext(ctx,
		"SELECT card_path, sha256, kind, detail FROM clock_warnings WHERE ingest_id = ? ORDER BY rowid", ingestID)
	if err != nil {
		return nil, fmt.Errorf("clock warnings %d: %w", ingestID, err)
	}
	var out []ClockWarning
	for rows.Next() {
		var (
			w    ClockWarning
			sha  []byte
			kind string
		)
		if err := rows.Scan(&w.CardPath, &sha, &kind, &w.Detail); err != nil {
			return nil, errors.Join(fmt.Errorf("clock warnings %d: %w", ingestID, err), rows.Close())
		}
		w.Kind = ClockWarningKind(kind)
		if sha != nil {
			w.SHA256 = blobToHex(sha)
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("clock warnings %d: %w", ingestID, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("clock warnings %d: %w", ingestID, err)
	}
	return out, nil
}
