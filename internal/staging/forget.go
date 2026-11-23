package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

var (
	ErrIngestRunning    = errors.New("staging: that import has not finished")
	ErrAlreadyForgotten = errors.New("staging: that import was already forgotten")
)

// ForgetSummary says what forgetting an import would do.
type ForgetSummary struct {
	Staged       int   `json:"staged"` // files of this import still in staging
	StagedBytes  int64 `json:"staged_bytes"`
	Culled       int   `json:"culled"`       // culled files of this import (the card brings them back after a forget)
	Archived     int   `json:"archived"`     // already in the archive: stay known
	Reimportable int   `json:"reimportable"` // its "seen before" files whose asset is culled ("Re-import anyway")
	Forgotten    bool  `json:"forgotten"`
}

// ForgetResult is what Forget did.
type ForgetResult struct {
	Forgotten    int     `json:"forgotten"` // asset rows deleted
	DeletedFiles int     `json:"deleted_files"`
	KeptArchived int     `json:"kept_archived"`
	Removed      []int64 `json:"removed"` // staging folders left empty and removed
}

// ForgetSummary says what forgetting import ingestID would do.
func (s *Service) ForgetSummary(ctx context.Context, ingestID int64) (ForgetSummary, error) {
	in, err := s.db.Ingest(ctx, ingestID)
	if err != nil {
		return ForgetSummary{}, err
	}
	assets, err := s.db.IngestAssets(ctx, ingestID)
	if err != nil {
		return ForgetSummary{}, err
	}
	culled, err := s.db.CulledSeenBefore(ctx, ingestID)
	if err != nil {
		return ForgetSummary{}, err
	}
	out := ForgetSummary{Reimportable: len(culled), Forgotten: !in.ForgottenAt.IsZero()}
	for _, a := range assets {
		switch a.State {
		case catalog.StateStaged:
			out.Staged++
			out.StagedBytes += a.Size
		case catalog.StateCulled:
			out.Culled++
		case catalog.StateArchived:
			out.Archived++
		}
	}
	return out, nil
}

// Forget forgets import ingestID (§8, Decision 10): its staged and culled
// assets are deleted from the catalog with their fingerprints, after their
// files in Staging/ are deleted when deleteFiles is set, so the same card
// imports them again. Archived assets stay known. The ingest stays in the
// history, marked forgotten. The caller keeps imports from running
// meanwhile (ingest.Manager.Exclusive).
func (s *Service) Forget(ctx context.Context, ingestID int64, deleteFiles bool) (ForgetResult, error) {
	in, err := s.db.Ingest(ctx, ingestID)
	if err != nil {
		return ForgetResult{}, err
	}
	if !in.Status.Terminal() {
		return ForgetResult{}, ErrIngestRunning
	}
	if !in.ForgottenAt.IsZero() {
		return ForgetResult{}, ErrAlreadyForgotten
	}
	forget, staged, ids, err := s.forgetPlan(ctx, ingestID)
	if err != nil {
		return ForgetResult{}, err
	}
	if forgetAfterRead != nil {
		forgetAfterRead()
	}
	rows, release, err := s.lockFolders(ctx, uniqueIDs(ids))
	if err != nil {
		return ForgetResult{}, err
	}
	defer release()
	defer s.changed()
	// Read again under the locks: a rename or move that landed since the
	// first read changed the paths and may have left a folder unlocked.
	in, err = s.db.Ingest(ctx, ingestID)
	if err != nil {
		return ForgetResult{}, err
	}
	if !in.ForgottenAt.IsZero() {
		return ForgetResult{}, ErrAlreadyForgotten
	}
	forget, staged, ids, err = s.forgetPlan(ctx, ingestID)
	if err != nil {
		return ForgetResult{}, err
	}
	locked := map[int64]bool{}
	for _, r := range rows {
		locked[r.ID] = true
	}
	for _, id := range ids {
		if !locked[id] {
			return ForgetResult{}, ErrChanged
		}
	}
	res := ForgetResult{Removed: []int64{}}
	for _, a := range forget {
		if a.State == catalog.StateArchived {
			res.KeptArchived++
		}
	}
	forget = slices.DeleteFunc(forget, func(a catalog.Asset) bool { return a.State == catalog.StateArchived })
	// Files first, then the catalog (global rule): a delete that fails half
	// way leaves the import unforgotten, so Forget can simply run again
	// (a file already gone counts as deleted); the rescan meanwhile culls
	// the vanished ones, which the retry forgets too.
	touched := map[string]bool{}
	if deleteFiles {
		for _, a := range staged {
			if !strings.HasPrefix(a.RelPath, Dir+"/") {
				continue // never delete outside Staging/
			}
			p := s.abs(a.RelPath)
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return res, errors.Join(fmt.Errorf("deleted %d of %d files, the import is not forgotten yet: %w",
					res.DeletedFiles, len(staged), err), syncDirs(touched))
			}
			touched[filepath.Dir(p)] = true
			res.DeletedFiles++
		}
	}
	shas := make([]string, len(forget))
	for i, a := range forget {
		shas[i] = a.SHA256
	}
	gone, err := s.db.ForgetAssets(ctx, shas, []catalog.AssetState{catalog.StateStaged, catalog.StateCulled})
	if err != nil {
		return res, errors.Join(err, syncDirs(touched))
	}
	res.Forgotten = len(gone)
	s.removed(gone)
	if err := s.db.MarkIngestForgotten(ctx, ingestID, s.now()); err != nil {
		// Another forget of this import may have won the race.
		if in, rerr := s.db.Ingest(ctx, ingestID); rerr == nil && !in.ForgottenAt.IsZero() {
			err = errors.Join(ErrAlreadyForgotten, err)
		}
		return res, errors.Join(err, syncDirs(touched))
	}
	if deleteFiles {
		for _, row := range rows {
			removed, err := s.dropIfEmpty(ctx, row, touched)
			if err != nil {
				return res, errors.Join(err, syncDirs(touched))
			}
			if removed {
				res.Removed = append(res.Removed, row.ID)
			}
		}
	}
	return res, syncDirs(touched)
}

// forgetAfterRead is a test seam: it runs between Forget's first read and
// its folder locks.
var forgetAfterRead func()

// forgetPlan reads import ingestID's assets: those to forget (staged and
// culled) and archived ones (kept) in forget, the staged ones in staged,
// and the ids of the folders holding the staged ones. A staged asset that
// is not inside a known staging folder is ErrChanged.
func (s *Service) forgetPlan(ctx context.Context, ingestID int64) (forget, staged []catalog.Asset, ids []int64, err error) {
	assets, err := s.db.IngestAssets(ctx, ingestID)
	if err != nil {
		return nil, nil, nil, err
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return nil, nil, nil, err
	}
	byName := map[string]catalog.StagingFolder{}
	for _, r := range all {
		byName[r.Name] = r
	}
	seen := map[int64]bool{}
	for _, a := range assets {
		switch a.State {
		case catalog.StateStaged:
			name, _, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/")
			r, known := byName[name]
			if !strings.HasPrefix(a.RelPath, Dir+"/") || !ok || !known {
				return nil, nil, nil, ErrChanged
			}
			staged = append(staged, a)
			forget = append(forget, a)
			if !seen[r.ID] {
				seen[r.ID] = true
				ids = append(ids, r.ID)
			}
		case catalog.StateCulled, catalog.StateArchived:
			forget = append(forget, a)
		}
	}
	return forget, staged, ids, nil
}

// ForgetCulled forgets that import ingestID's culled "seen before" files
// were culled, so the next import of that card copies them again
// ("Re-import anyway", Decision 11). It touches no files and returns how
// many it forgot.
func (s *Service) ForgetCulled(ctx context.Context, ingestID int64) (int, error) {
	in, err := s.db.Ingest(ctx, ingestID)
	if err != nil {
		return 0, err
	}
	if !in.Status.Terminal() {
		return 0, ErrIngestRunning
	}
	shas, err := s.db.CulledSeenBefore(ctx, ingestID)
	if err != nil {
		return 0, err
	}
	// Culled only: a rescan may have restaged one of them meanwhile, and a
	// staged asset must not be forgotten here (pre-flight D7).
	gone, err := s.db.ForgetAssets(ctx, shas, []catalog.AssetState{catalog.StateCulled})
	if err != nil {
		return 0, err
	}
	s.removed(gone)
	return len(gone), nil
}
