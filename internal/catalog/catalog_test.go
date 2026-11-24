package catalog

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOpenCreatesSchema(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	v, err := d.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Fatalf("version = %d, want %d", v, SchemaVersion)
	}
	rows, err := d.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		got = append(got, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"aliases", "archive_changes", "archive_copies", "archive_scans", "assets", "clock_warnings", "fingerprints", "ingest_files", "ingests", "settings", "staging_folders"}
	if !slices.Equal(got, want) {
		t.Fatalf("tables = %v, want %v", got, want)
	}
}

func TestOpenPragmas(t *testing.T) {
	d := openTest(t)
	ctx := context.Background()
	var s string
	if err := d.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&s); err != nil {
		t.Fatal(err)
	}
	if s != "wal" {
		t.Errorf("journal_mode = %q, want wal", s)
	}
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "busy_timeout": 5000} {
		var n int
		if err := d.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != want {
			t.Errorf("%s = %d, want %d", pragma, n, want)
		}
	}
}

func TestOpenTwice(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES ('k', 'v')`); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	v, err := d.Version(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion {
		t.Errorf("version = %d, want %d", v, SchemaVersion)
	}
	var val string
	if err := d.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = 'k'`).Scan(&val); err != nil {
		t.Fatal(err)
	}
	if val != "v" {
		t.Errorf("value = %q, want v", val)
	}
}

func TestOpenRefusesNewerSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err == nil {
		d.Close()
		t.Fatal("expected error opening a newer schema")
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("error %q does not mention \"newer\"", err)
	}
	if d != nil {
		t.Error("handle should be nil on error")
	}
}

func TestOpenRejectsQuestionMark(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "a?b.db"))
	if err == nil {
		d.Close()
		t.Fatal("expected error for a path containing '?'")
	}
}

func TestForeignKeysEnforced(t *testing.T) {
	d := openTest(t)
	h, err := hexToBlob(hexN(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.db.Exec(`INSERT INTO fingerprints(quick_fp, sha256) VALUES (?, ?)`, h, h); err == nil {
		t.Fatal("expected foreign key violation")
	}
}

func TestTimeRoundTrip(t *testing.T) {
	in := time.Date(2025, 5, 31, 1, 30, 12, 123456789, time.FixedZone("CEST", 2*3600))
	s := formatTime(in)
	if s != "2025-05-30T23:30:12.123Z" {
		t.Fatalf("formatTime = %q", s)
	}
	got, err := parseTime(s)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2025, 5, 30, 23, 30, 12, 123000000, time.UTC); !got.Equal(want) {
		t.Errorf("parseTime = %v, want %v", got, want)
	}
}

func TestHexBlob(t *testing.T) {
	b, err := hexToBlob(hexN(0xab))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b, bytes.Repeat([]byte{0xab}, 32)) {
		t.Errorf("blob = %x", b)
	}
	if got := blobToHex(b); got != hexN(0xab) {
		t.Errorf("blobToHex = %q", got)
	}
	for _, bad := range []string{"abc", strings.Repeat("g", 64), strings.ToUpper(hexN(0xab))} {
		if _, err := hexToBlob(bad); !errors.Is(err, ErrBadHash) {
			t.Errorf("hexToBlob(%q) error = %v, want ErrBadHash", bad, err)
		}
	}
}
