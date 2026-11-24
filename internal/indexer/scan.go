package indexer

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

const (
	msgArchiveMissing = "Archive/ is missing, so nothing was changed (is the photos share mounted?)"
	msgArchiveUnread  = "Archive/ could not be read, so nothing was changed"
	msgArchiveGone    = "Archive/ went missing during the scan, so it was stopped (is the photos share still mounted?)"
)

// beginPass makes sure Running is set (StartRescan did already) and returns
// the pass's start time.
func (s *Service) beginPass(trigger string, full bool) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status.Running == nil {
		s.status.Running = &Progress{Trigger: trigger, Full: full, StartedAt: s.now()}
		s.reportLocked(true)
	}
	return s.status.Running.StartedAt
}

// runPass runs one pass with s.pass held, records it in archive_scans and
// updates the status. It returns an error only for catalog failures; a
// cancelled ctx sets Stopped.
func (s *Service) runPass(ctx context.Context, trigger string, full bool) (ScanResult, error) {
	started := s.beginPass(trigger, full)
	dbctx := context.WithoutCancel(ctx)
	// An unfinished re-index is continued whatever the trigger; checked_at has
	// millisecond precision, so the cut-off does too (see mustReread).
	rereadBefore, err := s.db.PendingReread(dbctx)
	var id int64
	if err == nil {
		switch {
		case !rereadBefore.IsZero():
			full = true
		case full:
			rereadBefore = started.Truncate(time.Millisecond)
		}
		id, err = s.db.StartArchiveScan(dbctx, trigger, rereadBefore, started)
	}
	if err != nil {
		s.mu.Lock()
		s.userPaused = false // a Pause before the failure must not hold the next pass
		s.status.Running = nil
		s.status.Version++
		s.reportLocked(true)
		s.mu.Unlock()
		return ScanResult{}, err
	}
	s.progress(true, func(p *Progress) { p.Phase, p.Full = PhaseListing, full })

	r := ScanResult{Trigger: trigger, Full: full, StartedAt: started, Errors: []string{}}
	err = s.scanArchive(ctx, &r, rereadBefore)
	r.FinishedAt = s.now()
	if err != nil {
		if cerr := ctx.Err(); cerr != nil && errors.Is(err, cerr) {
			r.Stopped = true
			err = nil
		} else {
			r.finalError(err.Error())
			s.log.Error("archive scan failed", "err", err)
		}
	}
	complete := err == nil && !r.Stopped && r.Skipped == ""
	if b, merr := json.Marshal(r); merr != nil {
		err = errors.Join(err, fmt.Errorf("encode scan result: %w", merr))
	} else if ferr := s.db.FinishArchiveScan(dbctx, id, r.FinishedAt, complete, string(b)); ferr != nil {
		err = errors.Join(err, ferr)
	}
	if cerr := s.refreshCounts(dbctx); cerr != nil {
		err = errors.Join(err, cerr)
	}
	if cerr := s.refreshPendingFull(dbctx); cerr != nil {
		err = errors.Join(err, cerr)
	}
	s.mu.Lock()
	s.userPaused = false
	last := r
	last.log = nil
	s.status.Last = &last
	s.status.Running = nil
	s.status.Version++
	s.reportLocked(true)
	s.mu.Unlock()
	s.log.Info("archive scan", "trigger", trigger, "full", full, "stopped", r.Stopped, "skipped", r.Skipped,
		"listed", r.Listed, "unchanged", r.Unchanged, "added", r.Added, "moved", r.Moved, "changed", r.Changed,
		"reappeared", r.Reappeared, "recovered", r.Recovered, "missing", r.Missing, "copies", r.Copies,
		"in_staging", r.InStaging, "busy_folders", r.BusyFolders,
		"problems", len(r.Errors)+r.ErrorsOmitted)
	return r, err
}

// scanArchive reads the settings once, builds the scan and runs it.
func (s *Service) scanArchive(ctx context.Context, r *ScanResult, rereadBefore time.Time) error {
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
	sc.rereadBefore = rereadBefore
	return sc.run()
}

func (sc *scan) abs(rel string) string {
	return filepath.Join(sc.s.photosRoot, filepath.FromSlash(rel))
}

// mustReread reports whether a file last read at checkedAt is due for a
// re-read whatever its size and time say (a re-index). A file read in the
// re-index's starting millisecond is read again: both times are cut to the
// millisecond, so it may have been read just before the re-index started.
// Only a resumed re-index pays for that, with an extra read.
func (sc *scan) mustReread(checkedAt time.Time) bool {
	return !sc.rereadBefore.IsZero() && !checkedAt.After(sc.rereadBefore)
}

// reindex reports a re-index pass: a file never read before must be read
// in full, so no move is taken on its fingerprint alone.
func (sc *scan) reindex() bool { return sc.mustReread(time.Time{}) }

// isGone: archived content not seen at its path this pass, and inside what the listing could read.
func (sc *scan) isGone(sha string) bool {
	e, ok := sc.bySHA[sha]
	return ok && !sc.seen[sha] && sc.inScope(e.RelPath)
}

// inScope: this pass's listing could have seen rel: the rules reach it, and
// neither it nor a parent directory was skipped (held, unreadable).
func (sc *scan) inScope(rel string) bool {
	if !sc.skip.reaches(rel) {
		return false
	}
	for p := rel; p != "." && p != "/" && p != ArchiveDir; p = path.Dir(p) {
		if sc.skipped[p] {
			return false
		}
	}
	return true
}

// replaced reports whether e's path is known to hold other content this pass,
// so a file there is no sign that e is back: the path was read with other
// content, or another row is confirmed there (two rows can share a path after
// a pass stopped between a move onto it and its check).
func (sc *scan) replaced(e catalog.ArchiveEntry) bool {
	if sc.readOther[e.RelPath] {
		return true
	}
	p, ok := sc.byPath[e.RelPath]
	return ok && p.SHA256 != e.SHA256 && sc.seen[p.SHA256]
}

// unresolved records a listed file the pass left unresolved though it is
// still there (held, unreadable, changing while read, its catalog entry
// moving on): an archived row not seen this pass may have moved to it, and a
// move keeps the size, so check leaves rows of that size, and a copy row at
// that path, for the next pass.
func (sc *scan) unresolved(f *file) {
	sc.unread[f.size] = true
	sc.unreadAt[f.rel] = true
}

// hasCopy reports whether a copy of content sha was seen at another path this pass.
func (sc *scan) hasCopy(sha string) bool {
	_, ok := sc.seenCopies[sha]
	return ok
}

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

	toHash, bytes := len(sc.fresh)+len(sc.changed), int64(0)
	for _, f := range sc.fresh {
		bytes += f.size
	}
	for _, f := range sc.changed {
		bytes += f.size
	}
	s.progress(true, func(p *Progress) {
		p.Phase, p.Folder, p.Current = PhaseHashing, "", ""
		p.Listed, p.ToHash, p.BytesToHash = r.Listed, toHash, bytes
	})

	// New paths first: a copy of an edited file's old content, made in the
	// same pass, is then seen before the edited path is resolved, so the old
	// content moves to the copy instead of becoming an alias of the edit.
	for _, f := range sc.fresh {
		if err := sc.handle(f, false); err != nil {
			return err
		}
	}
	for _, f := range sc.changed {
		if err := sc.handle(f, true); err != nil {
			return err
		}
	}
	sc.s.progress(true, func(p *Progress) { p.Phase, p.Folder, p.Current = PhaseChecking, "", "" })
	return sc.check()
}

// check (PhaseChecking) runs once every listed file was handled. An archived
// row not seen this pass moves to a copy of its content seen this pass, or is
// marked missing after its path and Archive/ are checked once more. Copy rows
// the pass did not confirm are then dropped. Rows the listing could not see
// (skip rules, held or unreadable folders) are left alone.
func (sc *scan) check() error {
	s, r, db := sc.s, sc.r, sc.s.db
	var rows []catalog.ArchiveEntry
	for sha, e := range sc.bySHA { // by hash: two rows can share a path while an edit settles
		if !sc.seen[sha] && sc.inScope(e.RelPath) {
			rows = append(rows, e)
		}
	}
	slices.SortFunc(rows, func(a, b catalog.ArchiveEntry) int {
		return cmp.Or(strings.Compare(a.RelPath, b.RelPath), strings.Compare(a.SHA256, b.SHA256))
	})
	for _, e := range rows {
		if err := sc.wait(); err != nil {
			return err
		}
		if c, ok := sc.seenCopies[e.SHA256]; ok {
			err := db.SetArchivePath(sc.dbctx, e.SHA256, c.RelPath, c.Mtime, s.now())
			if sc.entryChanged(e.RelPath, err) {
				continue
			}
			if err != nil {
				return err
			}
			if err := db.DeleteArchiveCopy(sc.dbctx, c.RelPath); err != nil {
				return err
			}
			r.Moved++
			if _, had := sc.copies[c.RelPath]; !had {
				r.Copies-- // counted as a new copy during hashing; it is the primary now
			}
			delete(sc.seenCopies, e.SHA256)
			continue
		}
		if sc.unread[e.Size] || s.held(e.RelPath) {
			continue // it may be a file this pass could not read: the next pass looks again
		}
		if h := s.beforeMissing; h != nil {
			h(e.RelPath)
		}
		if !sc.replaced(e) {
			_, err := os.Lstat(sc.abs(e.RelPath))
			if err == nil {
				continue // it is back: the next pass reads it
			}
			if !errors.Is(err, fs.ErrNotExist) {
				r.errorf("%s: %v", e.RelPath, err)
				continue
			}
		}
		// A dropped share reads as "not found" too: nothing is marked once Archive/ itself is gone.
		if _, err := os.Stat(filepath.Join(s.photosRoot, ArchiveDir)); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				r.Skipped = msgArchiveGone
				return nil
			}
			r.errorf("%s: %v; nothing more was marked missing", ArchiveDir, err)
			return nil
		}
		err := db.MarkMissing(sc.dbctx, e.SHA256, s.now())
		if sc.entryChanged(e.RelPath, err) {
			continue
		}
		if err != nil {
			return err
		}
		r.Missing++
	}

	// keepCopy holds every copy seen this pass, including those just made primary.
	for _, rel := range slices.Sorted(maps.Keys(sc.copies)) {
		if sc.keepCopy[rel] || sc.unreadAt[rel] || !sc.inScope(rel) {
			continue
		}
		if err := db.DeleteArchiveCopy(sc.dbctx, rel); err != nil {
			return err
		}
	}
	return nil
}

// found takes one indexed file from the listing.
func (sc *scan) found(f file) {
	sc.r.Listed++
	listed := sc.r.Listed
	sc.s.progress(false, func(p *Progress) { p.Listed = listed })
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
func (sc *scan) handle(f file, changed bool) (err error) {
	if err := sc.wait(); err != nil {
		return err
	}
	if sc.s.held(f.rel) {
		sc.r.BusyFolders++
		sc.unresolved(&f)
		return nil
	}
	folder, _, _ := strings.Cut(strings.TrimPrefix(f.rel, ArchiveDir+"/"), "/")
	sc.s.progress(false, func(p *Progress) { p.Folder, p.Current = ArchiveDir+"/"+folder, f.rel })
	defer func() {
		if err == nil {
			sc.s.progress(false, func(p *Progress) { p.Hashed++; p.BytesHashed += f.size })
		}
	}()
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

	// Always fingerprinted, even a copy with a known hash; read in full unless
	// the fingerprint finds a move (not in a re-index).
	ok, err := sc.read(&f, f.sha == "" && sc.reindex())
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

// read fingerprints the file into f and hashes it when full, or when f.sha
// is unknown and the fingerprint finds no move; it takes the size and time
// from after the read. ok is false when the file is to be skipped: gone,
// unreadable, or changing while read. err is a stop or a catalog failure.
func (sc *scan) read(f *file, full bool) (ok bool, err error) {
	s, p := sc.s, sc.abs(f.rel)
	before, err := os.Lstat(p)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		sc.r.errorf("%s: %v", f.rel, err)
		sc.unresolved(f)
		return false, nil
	}
	if !before.Mode().IsRegular() {
		sc.r.errorf("%s: no longer a regular file", f.rel)
		sc.unresolved(f)
		return false, nil
	}
	if !full && !f.matches(before) {
		full = true // replaced since the listing: neither a copy row's hash nor a move by fingerprint holds
		f.sha = ""
	}
	if f.fp, err = s.fingerprint(p); err != nil {
		return sc.readFailed(f, err)
	}
	if !full && f.sha == "" {
		if f.sha, err = sc.movedContent(*f); err != nil {
			return false, err
		}
		full = f.sha == ""
	}
	if full {
		if f.sha, err = s.hash(sc.ctx, p); err != nil {
			return sc.readFailed(f, err)
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
		sc.unresolved(f)
		return false, nil
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		sc.r.errorf("%s changed while it was read (still being copied?); the next scan picks it up", f.rel)
		sc.unresolved(f)
		return false, nil
	}
	f.size, f.mtime = after.Size(), after.ModTime().UnixNano()
	return true, nil
}

// movedContent returns the hash of known content f (fingerprinted) is a move
// of, or "" when it must be read in full. A candidate shares the fingerprint
// and has the listed size and time (a move keeps them; a change in place hands
// the old content's fingerprint to the new asset, so a same-size edit would
// match on size alone), and is missing or archived and gone this pass.
func (sc *scan) movedContent(f file) (string, error) {
	cands, err := sc.s.db.AssetsByFingerprint(sc.dbctx, f.fp)
	if err != nil {
		return "", err
	}
	for _, a := range cands {
		if a.Size != f.size || a.Mtime != f.mtime {
			continue
		}
		if a.State == catalog.StateMissing || a.State == catalog.StateArchived && sc.isGone(a.SHA256) {
			return a.SHA256, nil
		}
	}
	return "", nil
}

// matches reports whether info has the size and time the listing saw: a hash
// taken from the catalog for f.rel is only good for a file that still does.
func (f file) matches(info os.FileInfo) bool {
	return info.Size() == f.size && info.ModTime().UnixNano() == f.mtime
}

// readFailed sorts a failed read: cancellation stops the pass, a vanished
// file is skipped silently, anything else is listed and left unresolved.
func (sc *scan) readFailed(f *file, err error) (bool, error) {
	if cerr := sc.ctx.Err(); cerr != nil {
		return false, cerr
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	sc.r.errorf("%s: %v", f.rel, err)
	sc.unresolved(f)
	return false, nil
}

// resolve records what the content of f is to the catalog.
func (sc *scan) resolve(f file) error {
	db, now := sc.s.db, sc.s.now()
	found, err := db.AssetsByHash(sc.dbctx, []string{f.sha})
	if err != nil {
		return err
	}
	// Not an asset; it may still be an alias (an earlier version of an asset
	// changed in place): RecordArchiveChange and IndexAsset both drop that alias.
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
	case !known && f.editedFrom != nil && !sc.hasCopy(f.editedFrom.SHA256):
		// the file at this path now holds new content
		err = db.RecordArchiveChange(sc.dbctx, *f.editedFrom, f.sha, f.fp, f.size, f.mtime, now)
		if err == nil {
			sc.r.Changed++
			sc.seen[f.editedFrom.SHA256] = true // the old row no longer exists
			sc.seen[f.sha] = true
		}
	case !known:
		// New content; an edited path whose old content survives as a copy
		// lands here too, and the old content moves to that copy in check.
		err = db.IndexAsset(sc.dbctx, catalog.Asset{SHA256: f.sha, Size: f.size, Kind: f.kind,
			State: catalog.StateArchived, RelPath: f.rel, Mtime: f.mtime, UpdatedAt: now}, f.fp, now)
		if err == nil {
			sc.r.Added++
			sc.seen[f.sha] = true
		}
	case a.State == catalog.StateArchived && sc.isGone(a.SHA256):
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now)
		if err == nil {
			sc.r.Moved++
			sc.seen[a.SHA256] = true
		}
	case a.State == catalog.StateArchived:
		// still at its own path, or below a folder this pass skipped: a duplicate in the archive
		c := catalog.ArchiveCopy{RelPath: f.rel, SHA256: a.SHA256, Size: f.size, Mtime: f.mtime, CheckedAt: now, AssetState: a.State}
		err = db.PutArchiveCopy(sc.dbctx, c)
		if err == nil {
			if _, had := sc.copies[f.rel]; !had {
				sc.r.Copies++
			} else {
				sc.r.Unchanged++ // a known copy read again (a re-index)
			}
			sc.keepCopy[f.rel] = true
			sc.seenCopies[a.SHA256] = c
		}
	case a.State == catalog.StateMissing:
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now)
		if err == nil {
			sc.r.Reappeared++
			sc.seen[a.SHA256] = true
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
		sc.unresolved(&f)
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
