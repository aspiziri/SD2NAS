package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

const (
	msgArchiveMissing = "Archive/ is missing, so nothing was changed (is the photos share mounted?)"
	msgArchiveUnread  = "Archive/ could not be read, so nothing was changed"
)

// runPass runs one pass with s.pass held. It returns an error only for
// catalog failures; a cancelled ctx sets Stopped.
func (s *Service) runPass(ctx context.Context, trigger string, full bool) (ScanResult, error) {
	// full (a re-index) has no effect until the pass can re-read files.
	_ = full
	r := ScanResult{Trigger: trigger, StartedAt: s.now(), Errors: []string{}}
	err := s.scanArchive(ctx, &r)
	r.FinishedAt = s.now()
	if err != nil {
		if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
			r.Stopped = true
			return r, nil
		}
		return r, err
	}
	return r, nil
}

// scanArchive reads the settings once, builds the scan and runs it.
func (s *Service) scanArchive(ctx context.Context, r *ScanResult) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	set, err := s.db.Settings(ctx)
	if err != nil {
		return err
	}
	sc, err := s.newScan(ctx, set, r)
	if err != nil {
		return err
	}
	return sc.run()
}

func (sc *scan) abs(rel string) string {
	return filepath.Join(sc.s.photosRoot, filepath.FromSlash(rel))
}

// mustReread reports whether a file last read at checkedAt is due for a
// re-read whatever its size and time say (a re-index).
func (sc *scan) mustReread(checkedAt time.Time) bool { return false }

// wait blocks while an import or promote runs, and ends with the context.
func (sc *scan) wait() error { return sc.ctx.Err() }

// before runs the test hook ahead of touching a file, then checks for a stop.
func (sc *scan) before(rel string) error {
	if h := sc.s.beforeRead; h != nil {
		h(sc.ctx, rel)
	}
	return sc.ctx.Err()
}

func (sc *scan) run() error {
	s, r := sc.s, sc.r
	if _, err := os.Stat(filepath.Join(s.photosRoot, ArchiveDir)); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			r.Skipped = msgArchiveMissing
			return nil
		}
		r.errorf("%s: %v", ArchiveDir, err)
		r.Skipped = msgArchiveUnread
		return nil
	}
	entries, err := s.db.ArchiveEntries(sc.ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.State == catalog.StateArchived { // missing rows are found by hash
			sc.byPath[e.RelPath] = e
			sc.bySHA[e.SHA256] = e
		}
	}
	copies, err := s.db.ArchiveCopies(sc.ctx)
	if err != nil {
		return err
	}
	for _, c := range copies {
		sc.copies[c.RelPath] = c
	}

	if err := sc.walk(filepath.Join(s.photosRoot, ArchiveDir), ArchiveDir, 0); err != nil {
		if cerr := sc.ctx.Err(); cerr != nil {
			return cerr
		}
		if errors.Is(err, fs.ErrNotExist) { // went missing between the check and the listing
			r.Skipped = msgArchiveMissing
			return nil
		}
		r.errorf("%s: %v", ArchiveDir, err)
		r.Skipped = msgArchiveUnread
		return nil
	}

	for _, f := range sc.changed {
		if err := sc.handle(f, true); err != nil {
			return err
		}
	}
	for _, f := range sc.fresh {
		if err := sc.handle(f, false); err != nil {
			return err
		}
	}
	return nil
}

// found takes one indexed file from the listing.
func (sc *scan) found(f file) {
	sc.r.Listed++
	if a, ok := sc.byPath[f.rel]; ok {
		sc.seen[a.SHA256] = true
		if a.Size == f.size && a.Mtime == f.mtime && !sc.mustReread(a.CheckedAt) {
			sc.r.Unchanged++
			return
		}
		f.editedFrom = &a
		sc.changed = append(sc.changed, f)
		return
	}
	if c, ok := sc.copies[f.rel]; ok && c.Size == f.size && c.Mtime == f.mtime && !sc.mustReread(c.CheckedAt) {
		if c.AssetState == catalog.StateArchived {
			sc.keepCopy[f.rel] = true
			sc.seenCopies[c.SHA256] = c
			sc.r.Unchanged++
			return
		}
		f.sha = c.SHA256 // known content whose asset left the archive: resolved again without a full read (still fingerprinted)
	}
	sc.fresh = append(sc.fresh, f) // includes paths of missing rows and changed copies
}

// handle reads and records one file; changed is true for a path the catalog
// holds with other size or time. Only cancellation and catalog failures are
// returned.
func (sc *scan) handle(f file, changed bool) error {
	if err := sc.wait(); err != nil {
		return err
	}
	if sc.s.held(f.rel) {
		sc.r.BusyFolders++
		return nil
	}
	if err := sc.before(f.rel); err != nil {
		return err
	}
	if changed {
		return sc.handleChanged(f)
	}

	// A promote that finished after the listing has recorded this path already (pre-flight C5).
	a, err := sc.s.db.ArchiveEntryAt(sc.dbctx, f.rel)
	switch {
	case err == nil:
		sc.byPath[f.rel] = a
		sc.bySHA[a.SHA256] = a
		if a.Size == f.size && a.Mtime == f.mtime && !sc.mustReread(a.CheckedAt) {
			sc.seen[a.SHA256] = true
			sc.r.Unchanged++
			return nil
		}
		f.editedFrom = &a
		return sc.handleChanged(f)
	case !errors.Is(err, catalog.ErrNotFound):
		return err
	}

	ok, err := sc.read(&f, f.sha == "") // always fingerprinted, even a copy with a known hash
	if err != nil || !ok {
		return err
	}
	return sc.resolve(f)
}

// handleChanged reads a path the catalog holds with other size or time.
func (sc *scan) handleChanged(f file) error {
	ok, err := sc.read(&f, true)
	if err != nil || !ok {
		return err
	}
	old := *f.editedFrom
	if f.sha == old.SHA256 { // only touched
		err := sc.s.db.SetArchivePath(sc.dbctx, old.SHA256, f.rel, f.mtime, sc.s.now())
		if sc.entryChanged(f.rel, err) {
			return nil
		}
		if err != nil {
			return err
		}
		sc.r.Unchanged++
		return nil
	}
	delete(sc.seen, old.SHA256)
	sc.readOther[f.rel] = true
	return sc.resolve(f)
}

// entryChanged records a catalog entry that moved on under the pass and
// reports whether err was that (the next pass looks again).
func (sc *scan) entryChanged(rel string, err error) bool {
	if errors.Is(err, catalog.ErrWrongState) || errors.Is(err, catalog.ErrNotFound) {
		sc.r.errorf("%s: its catalog entry changed during the scan; the next scan looks again", rel)
		return true
	}
	return false
}

// read fingerprints the file (and hashes it when full) into f, and takes
// its size and time from after the read. ok is false when the file is to be
// skipped: gone, unreadable, or changing while read.
func (sc *scan) read(f *file, full bool) (ok bool, err error) {
	s, p := sc.s, sc.abs(f.rel)
	before, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		sc.r.errorf("%s: %v", f.rel, err)
		return false, nil
	}
	if !before.Mode().IsRegular() {
		sc.r.errorf("%s: no longer a regular file", f.rel)
		return false, nil
	}
	if !full && !f.matches(before) {
		full = true // the hash taken from a copy row is for a file that has since been replaced
		f.sha = ""
	}
	if f.fp, err = s.fingerprint(p); err != nil {
		return sc.readFailed(f.rel, err)
	}
	if full {
		if f.sha, err = s.hash(sc.ctx, p); err != nil {
			return sc.readFailed(f.rel, err)
		}
	}
	if s.afterHash != nil {
		s.afterHash(p)
	}
	after, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		sc.r.errorf("%s: %v", f.rel, err)
		return false, nil
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		sc.r.errorf("%s changed while it was read (still being copied?); the next scan picks it up", f.rel)
		return false, nil
	}
	f.size, f.mtime = after.Size(), after.ModTime().UnixNano()
	return true, nil
}

// matches reports whether info has the size and time the listing saw: a hash
// taken from the catalog for f.rel is only good for a file that still does.
func (f file) matches(info os.FileInfo) bool {
	return info.Size() == f.size && info.ModTime().UnixNano() == f.mtime
}

// readFailed sorts a failed read: cancellation stops the pass, a vanished
// file is skipped silently, anything else is listed.
func (sc *scan) readFailed(rel string, err error) (bool, error) {
	if cerr := sc.ctx.Err(); cerr != nil {
		return false, cerr
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	sc.r.errorf("%s: %v", rel, err)
	return false, nil
}

// resolve records what the content of f is to the catalog.
func (sc *scan) resolve(f file) error {
	db, now := sc.s.db, sc.s.now()
	found, err := db.AssetsByHash(sc.dbctx, []string{f.sha})
	if err != nil {
		return err
	}
	a, known := found[f.sha]
	switch {
	case known && a.State == catalog.StateArchived && a.RelPath == f.rel:
		// already recorded here (a promote finished during the pass): never a copy of itself
		sc.seen[a.SHA256] = true
		if a.Mtime != f.mtime {
			err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now)
		}
		if err == nil {
			sc.r.Unchanged++
		}
	case !known && f.editedFrom != nil:
		// the file at this path now holds new content; Task 8 adds the cases around it
		err = db.RecordArchiveChange(sc.dbctx, *f.editedFrom, f.sha, f.fp, f.size, f.mtime, now)
		if err == nil {
			sc.r.Changed++
			sc.seen[f.editedFrom.SHA256] = true // the old row no longer exists
			sc.seen[f.sha] = true
		}
	case !known:
		err = db.IndexAsset(sc.dbctx, catalog.Asset{SHA256: f.sha, Size: f.size, Kind: f.kind,
			State: catalog.StateArchived, RelPath: f.rel, Mtime: f.mtime, UpdatedAt: now}, f.fp, now)
		if err == nil {
			sc.r.Added++
			sc.seen[f.sha] = true
		}
	case a.State == catalog.StateCulled:
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now) // M4 Decision 9: a promote stopped before its catalog write
		if err == nil {
			sc.r.Recovered++
		}
	case a.State == catalog.StateStaged:
		sc.r.InStaging++ // a promote is moving it, or a copy of a staging file: the staging side settles it
		return nil
	}
	if sc.entryChanged(f.rel, err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("record %s: %w", f.rel, err)
	}
	if f.fp != "" && known {
		err = db.AddFingerprint(sc.dbctx, f.fp, a.SHA256) // insert-or-ignore: later moves of this file take the shortcut
	}
	return err
}
