package indexer

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func fixedNow(s *Service) time.Time {
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.Local)
	s.now = func() time.Time { return now }
	return now
}

func dirNames(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}

func writeOldBackups(t *testing.T, dir string, now time.Time, n int) []string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	var names []string
	for i := 1; i <= n; i++ {
		name := "catalog-" + now.AddDate(0, 0, -i).Format("2006-01-02") + ".db"
		names = append(names, name)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return names // newest first
}

func TestBackupWritesAndPrunes(t *testing.T) {
	s, _, photos := setup(t)
	now := fixedNow(s)
	put(t, photos, "Archive/T/a.JPG", "content a", t0)
	rescan(t, s)
	dir := filepath.Join(photos, "Management", "SD2NAS")
	olds := writeOldBackups(t, dir, now, 16)
	others := []string{"notes.txt", "catalog-old.db", "catalog-2026-01-01.db.bak"}
	for _, n := range others {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("keep"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	res := s.backup(t.Context())
	if res.Error != "" {
		t.Fatalf("backup error: %s", res.Error)
	}
	today := "catalog-" + now.Format("2006-01-02") + ".db"
	if res.Path != filepath.Join(dir, today) || res.Bytes <= 0 {
		t.Errorf("result = %+v", res)
	}
	wantRemoved := []string{olds[15], olds[14], olds[13]}
	slices.Sort(wantRemoved)
	if !slices.Equal(res.Removed, wantRemoved) {
		t.Errorf("Removed = %v; want %v", res.Removed, wantRemoved)
	}
	want := append([]string{today}, olds[:13]...)
	want = append(want, others...)
	slices.Sort(want)
	if got := dirNames(t, dir); !slices.Equal(got, want) {
		t.Errorf("folder = %v; want %v", got, want)
	}
	b, err := catalog.Open(filepath.Join(dir, today))
	if err != nil {
		t.Fatal(err)
	}
	if v, err := b.Version(t.Context()); err != nil || v != 3 {
		t.Errorf("backup version = %d, %v; want 3", v, err)
	}
	asset(t, b, "content a")
	if err := b.Close(); err != nil { // Windows cannot rename over an open file
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, today), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	again := s.backup(t.Context())
	if again.Error != "" || len(again.Removed) != 0 {
		t.Errorf("second backup = %+v", again)
	}
	head, err := os.ReadFile(filepath.Join(dir, today))
	if err != nil || !bytes.HasPrefix(head, []byte("SQLite format 3")) {
		t.Errorf("same-day file was not replaced: %v", err)
	}
}

// A directory and a symlink named like a backup are not backups: prune keeps them.
func TestPruneIgnoresNonRegularBackupNames(t *testing.T) {
	s, _, photos := setup(t)
	now := fixedNow(s)
	dir := filepath.Join(photos, "Management", "SD2NAS")
	writeOldBackups(t, dir, now, 16)
	if err := os.Mkdir(filepath.Join(dir, "catalog-2020-01-01.db"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := []string{"catalog-2020-01-01.db"}
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "catalog-2020-01-02.db")); err == nil {
		keep = append(keep, "catalog-2020-01-02.db")
	}
	res := s.backup(t.Context())
	if res.Error != "" {
		t.Fatalf("backup error: %s", res.Error)
	}
	got := dirNames(t, dir)
	for _, n := range keep {
		if !slices.Contains(got, n) {
			t.Errorf("%s was removed; folder = %v", n, got)
		}
	}
	if _, err := os.Stat(target); err != nil {
		t.Errorf("symlink target touched: %v", err)
	}
}

// Close in the middle of a backup is not a failed backup: nothing is recorded.
func TestBackupInterruptedRecordsNothing(t *testing.T) {
	s, db, _ := setup(t)
	fixedNow(s)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.backupNow(ctx)
	if st := s.Status(); st.LastBackup != nil || st.BackingUp {
		t.Errorf("status = %+v; want no LastBackup and not backing up", st)
	}
	if _, err := db.LastArchiveBackup(t.Context()); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("LastArchiveBackup = %v; want ErrNotFound", err)
	}
}

func TestBackupFailureKeepsOldBackups(t *testing.T) {
	s, db, photos := setup(t)
	now := fixedNow(s)
	// (a) backup_dir is an existing regular file.
	file := filepath.Join(t.TempDir(), "afile")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting(t.Context(), catalog.KeyBackupDir, file); err != nil {
		t.Fatal(err)
	}
	s.backupNow(t.Context())
	st := s.Status()
	if st.LastBackup == nil || st.LastBackup.Error == "" || st.BackingUp {
		t.Errorf("LastBackup = %+v, backing up %v; want an error", st.LastBackup, st.BackingUp)
	}
	if fi, err := os.Stat(file); err != nil || !fi.Mode().IsRegular() {
		t.Errorf("the file was disturbed: %v", err)
	}
	if rec, err := db.LastArchiveBackup(t.Context()); err != nil || rec.Complete {
		t.Errorf("recorded backup = %+v, %v; want incomplete", rec, err)
	}

	// (b) the stale .partial cannot be removed: it is a non-empty directory.
	dir := filepath.Join(photos, "Management", "SD2NAS")
	if err := db.SetSetting(t.Context(), catalog.KeyBackupDir, dir); err != nil {
		t.Fatal(err)
	}
	olds := writeOldBackups(t, dir, now, 16)
	part := filepath.Join(dir, "catalog-"+now.Format("2006-01-02")+".db.partial")
	if err := os.MkdirAll(filepath.Join(part, "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	res := s.backup(t.Context())
	if res.Error == "" || len(res.Removed) != 0 {
		t.Errorf("result = %+v; want an error and nothing removed", res)
	}
	for _, n := range olds {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("old backup %s: %v", n, err)
		}
	}
}

func TestBackupInsideArchiveRefused(t *testing.T) {
	s, db, photos := setup(t)
	fixedNow(s)
	dir := filepath.Join(photos, "Archive", "x")
	if err := db.SetSetting(t.Context(), catalog.KeyBackupDir, dir); err != nil {
		t.Fatalf("the catalog now refuses it (a contract change): %v", err)
	}
	if res := s.backup(t.Context()); res.Error == "" {
		t.Errorf("backup into Archive/ succeeded: %+v", res)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the folder inside Archive/ was created")
	}
}

func TestCheckBackupDir(t *testing.T) {
	root := filepath.Join(t.TempDir(), "photos")
	bad := []string{
		filepath.Join(root, "Archive"),
		filepath.Join(root, "Archive", "x"),
		filepath.Join(root, "Staging", "y"),
		filepath.Join(root, "Staging"),
		filepath.Join(root, "Management", "..", "Archive", "z"),
	}
	for _, d := range bad {
		if err := CheckBackupDir(root, d); err != ErrBackupInside {
			t.Errorf("CheckBackupDir(%q) = %v; want ErrBackupInside", d, err)
		}
	}
	good := []string{
		filepath.Join(root, "Management", "SD2NAS"),
		filepath.Join(root, "ArchiveBackups"),
		filepath.Join(root, "Archive", "..", "Other"),
		"relative",
	}
	for _, d := range good {
		if err := CheckBackupDir(root, d); err != nil {
			t.Errorf("CheckBackupDir(%q) = %v; want nil", d, err)
		}
	}
}
