package indexer

import (
	"context"
	"errors"
	"io/fs"
	"path"
	"path/filepath"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// metadataNames are NAS and OS housekeeping names that never hold the
// owner's photos: the same list as staging's skipName (internal/staging/
// reconcile.go); keep them in sync. Dot-names are skipped as well.
var metadataNames = []string{"@eaDir", "@Recycle", "@Recently-Snapshot", "#recycle", "#snapshot",
	"$RECYCLE.BIN", "System Volume Information"}

// builtinSkip: names no setting can make the indexer look at.
func builtinSkip(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, m := range metadataNames {
		if strings.EqualFold(name, m) {
			return true
		}
	}
	return false
}

// skipRules are the built-in rules plus archive_skip, fixed for one pass
// (Decision 6).
type skipRules struct {
	media media.Rules
	dirs  []string // lower-cased directory-name patterns ("output"), from entries ending in "/"
	files []string // lower-cased file-name patterns ("*.cosessiondb")
}

// newSkipRules splits the entries from catalog.ParseArchiveSkip into
// directory-name and file-name patterns.
func newSkipRules(rules media.Rules, entries []string) skipRules {
	k := skipRules{media: rules}
	for _, e := range entries {
		if p, ok := strings.CutSuffix(e, "/"); ok {
			k.dirs = append(k.dirs, strings.ToLower(p))
		} else {
			k.files = append(k.files, strings.ToLower(e))
		}
	}
	return k
}

// dir reports a directory the listing never walks into; depth is its depth
// below Archive/ (1 = a session folder: only built-in rules apply).
func (k skipRules) dir(name string, depth int) bool {
	if builtinSkip(name) {
		return true
	}
	if depth >= 2 {
		n := strings.ToLower(name)
		for _, p := range k.dirs {
			if ok, _ := path.Match(p, n); ok { // patterns were validated by the catalog
				return true
			}
		}
	}
	return false
}

// file reports whether a regular file named name is indexed, and its kind.
func (k skipRules) file(name string) (media.Kind, bool) {
	if builtinSkip(name) {
		return "", false
	}
	n := strings.ToLower(name)
	for _, p := range k.files {
		if ok, _ := path.Match(p, n); ok {
			return "", false
		}
	}
	return k.media.KindOf(name)
}

// reaches reports whether the listing could reach rel_path rel ("Archive/S/Output/x.JPG")
// under these rules; a catalog row it can't reach is left alone (never missing).
func (k skipRules) reaches(rel string) bool {
	parts := strings.Split(strings.TrimPrefix(rel, ArchiveDir+"/"), "/")
	for i, p := range parts {
		if i == len(parts)-1 {
			_, ok := k.file(p)
			return ok
		}
		if k.dir(p, i+1) {
			return false
		}
	}
	return false
}

// file is the per-path value the pass carries.
type file struct {
	rel         string
	size, mtime int64
	kind        media.Kind
	sha, fp     string                // known content (a copy whose size and time match) / after reading
	editedFrom  *catalog.ArchiveEntry // the archived content this path held (its size or time changed, or a re-index)
}

// scan is one pass's working state.
type scan struct {
	s       *Service
	ctx     context.Context
	r       *ScanResult
	skip    skipRules
	skipped map[string]bool // rels the listing didn't read: held, unreadable
	fresh   []file

	dbctx      context.Context                 // context.WithoutCancel(ctx): what was read is recorded
	byPath     map[string]catalog.ArchiveEntry // archived rows
	bySHA      map[string]catalog.ArchiveEntry // the same rows by hash
	copies     map[string]catalog.ArchiveCopy  // by rel_path
	keepCopy   map[string]bool                 // copy rows confirmed or written this pass
	seenCopies map[string]catalog.ArchiveCopy  // by sha: a copy of archived content seen this pass
	seen       map[string]bool                 // sha of archived rows accounted for this pass
	readOther  map[string]bool                 // rel_paths of archived rows read this pass with other content
	changed    []file
}

// newScan builds the pass state; settings are read once, at the start of a pass.
func (s *Service) newScan(ctx context.Context, set catalog.Settings, r *ScanResult) (*scan, error) {
	entries, err := catalog.ParseArchiveSkip(set.ArchiveSkip)
	if err != nil {
		return nil, err
	}
	if r.Errors == nil {
		r.Errors = []string{}
	}
	r.log = s.log
	return &scan{s: s, ctx: ctx, r: r, skip: newSkipRules(s.rules, entries), skipped: map[string]bool{},
		dbctx:  context.WithoutCancel(ctx),
		byPath: map[string]catalog.ArchiveEntry{}, bySHA: map[string]catalog.ArchiveEntry{},
		copies: map[string]catalog.ArchiveCopy{}, keepCopy: map[string]bool{}, seenCopies: map[string]catalog.ArchiveCopy{},
		seen: map[string]bool{}, readOther: map[string]bool{}}, nil
}

// held reports whether a promote is writing into directory rel.
func (s *Service) held(rel string) bool { return s.locks != nil && s.locks.NameHeld(rel) }

// walk lists dir (rel is its slash path, depth its depth below Archive/,
// 0 = Archive/ itself), handing each indexed file to found. Symlinks are
// never followed.
func (sc *scan) walk(dir, rel string, depth int) error {
	if depth > 0 {
		if err := sc.ctx.Err(); err != nil {
			return err
		}
		if sc.s.held(rel) {
			sc.skipped[rel] = true
			sc.r.BusyFolders++
			return nil
		}
	}
	entries, err := sc.s.readDir(dir)
	if err != nil {
		if depth == 0 {
			return err
		}
		sc.r.errorf("%s: %v", rel, err)
		sc.skipped[rel] = true
		return nil
	}
	for _, e := range entries { // os.ReadDir sorts by name; a replacement must too
		name := e.Name()
		child := rel + "/" + name
		switch {
		case e.IsDir():
			if sc.skip.dir(name, depth+1) {
				continue
			}
			if err := sc.walk(filepath.Join(dir, name), child, depth+1); err != nil {
				return err
			}
		case e.Type().IsRegular():
			kind, ok := sc.skip.file(name)
			if !ok {
				continue
			}
			info, err := e.Info()
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				sc.r.errorf("%s: %v", child, err)
				sc.skipped[child] = true
				continue
			}
			sc.found(file{rel: child, size: info.Size(), mtime: info.ModTime().UnixNano(), kind: kind})
		}
	}
	return nil
}
