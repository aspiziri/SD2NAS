// Package catalog is SD2NAS's SQLite database: every file ever imported or
// indexed, the ingest history, staging folders and settings.
package catalog

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// DB is an open catalog. It holds exactly one SQLite connection.
type DB struct{ db *sql.DB }

var (
	// ErrNotFound reports a lookup that matched nothing.
	ErrNotFound = errors.New("catalog: not found")
	// ErrBadHash reports a hash that is not 64 lowercase hex digits.
	ErrBadHash = errors.New("catalog: hash must be 64 lowercase hex digits")
	// ErrWrongState is what every *WrongStateError matches with errors.Is.
	ErrWrongState = errors.New("catalog: the asset is in another state")
)

// WrongStateError: an asset update found the asset in a state it doesn't apply to.
type WrongStateError struct {
	Verb, SHA256 string
	State        AssetState
}

func (e *WrongStateError) Error() string {
	return fmt.Sprintf("%s %s: the asset is %s", e.Verb, e.SHA256, e.State)
}

// Is reports whether target is ErrWrongState.
func (e *WrongStateError) Is(target error) bool { return target == ErrWrongState }

// SchemaVersion is the schema version this build creates and understands.
const SchemaVersion = 4

const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
	"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"

const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// Open opens (creating if needed) the catalog at path and migrates it to the
// current schema. It refuses a database created by a newer SD2NAS.
func Open(path string) (*DB, error) {
	if strings.Contains(path, "?") {
		return nil, fmt.Errorf("open catalog %s: path must not contain '?'", path)
	}
	sdb, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("open catalog %s: %w", path, err)
	}
	sdb.SetMaxOpenConns(1)
	d := &DB{db: sdb}
	if err := d.init(context.Background()); err != nil {
		return nil, fmt.Errorf("open catalog %s: %w", path, errors.Join(err, sdb.Close()))
	}
	return d, nil
}

func (d *DB) init(ctx context.Context) error {
	var mode string
	if err := d.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err != nil {
		return fmt.Errorf("read journal mode: %w", err)
	}
	if mode != "wal" {
		return fmt.Errorf("journal mode is %q, want wal", mode)
	}
	return d.migrate(ctx)
}

func (d *DB) migrate(ctx context.Context) error {
	v, err := d.Version(ctx)
	if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("catalog schema version %d was created by a newer SD2NAS (this one knows %d)", v, len(migrations))
	}
	for i := v; i < len(migrations); i++ {
		err := d.tx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migrate to schema version %d: %w", i+1, err)
		}
	}
	return nil
}

// Close closes the database.
func (d *DB) Close() error { return d.db.Close() }

// Version returns the schema version (PRAGMA user_version).
func (d *DB) Version(ctx context.Context) (int, error) {
	var v int
	if err := d.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return v, nil
}

// tx runs fn in a transaction: commit if fn succeeds, else roll back and
// return fn's error joined with any rollback failure. The rollback is
// deferred before fn runs, so a panic in fn (or in Commit) still rolls
// back and gives the single connection back before the panic goes on
// (§16 M6). After a successful Commit the deferred Rollback is
// sql.ErrTxDone, which is ignored.
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, rbErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// formatTime renders t as UTC with millisecond precision ("Z" suffix).
func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.Parse(timeLayout, s) }

// hexToBlob converts 64 lowercase hex digits to 32 bytes.
func hexToBlob(s string) ([]byte, error) {
	if len(s) != 64 || s != strings.ToLower(s) {
		return nil, fmt.Errorf("%w: %q", ErrBadHash, s)
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("%w: %q", ErrBadHash, s)
	}
	return b, nil
}

// blobToHex renders b as lowercase hex.
func blobToHex(b []byte) string { return hex.EncodeToString(b) }
