package staging

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// importOf records a finished ingest that staged files (path inside folder
// 2025-05-30 → content; written with e.write and recorded with RecordNew,
// outcome new) and saw culled and archived ones (assets inserted directly
// in that state, linked by AddIngestFiles rows with outcome seen_before and
// in_archive). It returns the ingest id, the folder, and the hashes of
// each group in the order given.
func importOf(t *testing.T, e *opsEnv, staged map[string]string, culled, archived []string) (id int64, folder catalog.StagingFolder, stagedSHAs, culledSHAs, archivedSHAs []string) {
	t.Helper()
	ctx := t.Context()
	folder = e.folder(t, "2025-05-30", "2025-05-30")
	in, err := e.db.CreateIngest(ctx, "/cards/X", "X", "", opsNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartIngest(ctx, in.ID, opsNow); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range staged {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		content := staged[name]
		rel := prefixOf(folder.Name) + name
		e.write(t, rel, content)
		sha := shaOf(content)
		a := catalog.Asset{SHA256: sha, Size: int64(len(content)), Kind: media.KindImage, State: catalog.StateStaged,
			RelPath: rel, UpdatedAt: opsNow}
		f := catalog.IngestFile{CardPath: "DCIM/" + name, Size: a.Size, Outcome: catalog.OutcomeNew}
		if err := e.db.RecordNew(ctx, in.ID, a, shaOf("fp:"+content), f); err != nil {
			t.Fatal(err)
		}
		stagedSHAs = append(stagedSHAs, sha)
	}
	var files []catalog.IngestFile
	for i, content := range culled {
		sha := shaOf(content)
		a := catalog.Asset{SHA256: sha, Size: int64(len(content)), Kind: media.KindImage, State: catalog.StateCulled, UpdatedAt: opsNow}
		if err := e.db.InsertAsset(ctx, a, shaOf("fp:"+content)); err != nil {
			t.Fatal(err)
		}
		files = append(files, catalog.IngestFile{CardPath: "DCIM/culled" + string(rune('0'+i)) + ".jpg", Size: a.Size,
			QuickFP: shaOf("fp:" + content), SHA256: sha, Outcome: catalog.OutcomeSeenBefore})
		culledSHAs = append(culledSHAs, sha)
	}
	for i, content := range archived {
		sha := shaOf(content)
		a := catalog.Asset{SHA256: sha, Size: int64(len(content)), Kind: media.KindImage, State: catalog.StateArchived,
			RelPath: "Archive/kept" + string(rune('0'+i)) + ".jpg", UpdatedAt: opsNow}
		if err := e.db.InsertAsset(ctx, a, shaOf("fp:"+content)); err != nil {
			t.Fatal(err)
		}
		files = append(files, catalog.IngestFile{CardPath: "DCIM/kept" + string(rune('0'+i)) + ".jpg", Size: a.Size,
			QuickFP: shaOf("fp:" + content), SHA256: sha, Outcome: catalog.OutcomeInArchive})
		archivedSHAs = append(archivedSHAs, sha)
	}
	if err := e.db.AddIngestFiles(ctx, in.ID, files); err != nil {
		t.Fatal(err)
	}
	in.Status = catalog.StatusDone
	in.FinishedAt = opsNow
	if err := e.db.FinishIngest(ctx, in); err != nil {
		t.Fatal(err)
	}
	return in.ID, folder, stagedSHAs, culledSHAs, archivedSHAs
}

var forgetStaged = map[string]string{"a.jpg": "abc", "b.jpg": "defgh"}

// addMissing records that import id found each content's archive copy
// missing (assets inserted in state missing, linked by rows with outcome
// missing). It returns their hashes.
func addMissing(t *testing.T, e *opsEnv, id int64, contents ...string) []string {
	t.Helper()
	var shas []string
	for i, content := range contents {
		sha := shaOf(content)
		a := catalog.Asset{SHA256: sha, Size: int64(len(content)), Kind: media.KindImage, State: catalog.StateMissing,
			RelPath: "Archive/lost" + string(rune('0'+i)) + ".jpg", UpdatedAt: opsNow}
		if err := e.db.InsertAsset(t.Context(), a, shaOf("fp:"+content)); err != nil {
			t.Fatal(err)
		}
		f := catalog.IngestFile{CardPath: "DCIM/" + content + ".jpg", Size: a.Size, QuickFP: shaOf("fp:" + content), SHA256: sha,
			Outcome: catalog.OutcomeMissing}
		if err := e.db.AddIngestFiles(t.Context(), id, []catalog.IngestFile{f}); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	return shas
}

func TestForgetSummary(t *testing.T) {
	e := newOpsEnv(t)
	id, _, _, _, _ := importOf(t, e, forgetStaged, []string{"culled"}, []string{"archived"})
	addMissing(t, e, id, "lost")
	if n, err := e.db.RequestRestore(t.Context(), id, opsNow); err != nil || n != 1 {
		t.Fatalf("RequestRestore = %d, %v; want 1", n, err)
	}
	addMissing(t, e, id, "lost again")
	got, err := e.svc.ForgetSummary(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	want := ForgetSummary{Staged: 2, StagedBytes: 8, Culled: 1, Archived: 1, Reimportable: 1,
		Missing: 2, Restorable: 2, RestoreRequested: 1}
	if got != want {
		t.Errorf("summary = %+v, want %+v", got, want)
	}
	if _, err := e.svc.ForgetSummary(t.Context(), 99); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("unknown ingest: err = %v, want ErrNotFound", err)
	}
}

// addIgnored is addMissing plus an ignore of each (the Archive page's Ignore).
func addIgnored(t *testing.T, e *opsEnv, id int64, contents ...string) []string {
	t.Helper()
	shas := addMissing(t, e, id, contents...)
	for _, sha := range shas {
		if err := e.db.IgnoreMissing(t.Context(), sha); err != nil {
			t.Fatal(err)
		}
	}
	return shas
}

func TestForgetKeepsIgnored(t *testing.T) {
	e := newOpsEnv(t)
	id, _, _, culled, _ := importOf(t, e, forgetStaged, []string{"culled"}, []string{"archived"})
	ignored := addIgnored(t, e, id, "deleted on purpose")
	sum, err := e.svc.ForgetSummary(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Ignored != 1 || sum.Culled != 1 {
		t.Errorf("summary = %+v, want 1 ignored and 1 culled (the ignored file is not counted as culled)", sum)
	}
	got, err := e.svc.Forget(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	if got.Forgotten != 3 || got.KeptIgnored != 1 || got.KeptArchived != 1 {
		t.Errorf("result = %+v, want 3 forgotten (2 staged, 1 culled), 1 ignored and 1 archived kept", got)
	}
	a := e.asset(t, ignored[0])
	if !a.Ignored() || a.RelPath != "Archive/lost0.jpg" {
		t.Errorf("ignored asset = %+v, want it kept, culled at its archive path", a)
	}
	if _, err := e.db.AssetBySHA(t.Context(), culled[0]); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("culled asset: err = %v, want ErrNotFound", err)
	}
	if slices.Contains(e.removedHashes(), ignored[0]) {
		t.Errorf("removed hashes %v include the ignored file", e.removedHashes())
	}
}

// "Re-import anyway" still forgets an ignored file the card saw: asking to
// import the card's culled files again is what it is for.
func TestForgetCulledForgetsIgnored(t *testing.T) {
	e := newOpsEnv(t)
	id, _, _, _, _ := importOf(t, e, nil, nil, nil)
	sha := addIgnored(t, e, id, "deleted on purpose")[0]
	f := catalog.IngestFile{CardPath: "DCIM/again.jpg", Size: 18, QuickFP: shaOf("fp:deleted on purpose"), SHA256: sha,
		Outcome: catalog.OutcomeSeenBefore}
	if err := e.db.AddIngestFiles(t.Context(), id, []catalog.IngestFile{f}); err != nil {
		t.Fatal(err)
	}
	if got, err := e.svc.ForgetCulled(t.Context(), id); err != nil || got != 1 {
		t.Fatalf("ForgetCulled = %d, %v; want 1", got, err)
	}
	if _, err := e.db.AssetBySHA(t.Context(), sha); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("ignored asset: err = %v, want ErrNotFound", err)
	}
}

func TestForgetDeletesFiles(t *testing.T) {
	e := newOpsEnv(t)
	id, f, staged, culled, archived := importOf(t, e, forgetStaged, []string{"culled"}, []string{"archived"})
	missing := addMissing(t, e, id, "lost")
	v := e.version()
	got, err := e.svc.Forget(t.Context(), id, true)
	if err != nil {
		t.Fatal(err)
	}
	want := ForgetResult{Forgotten: 3, DeletedFiles: 2, KeptArchived: 1, KeptMissing: 1, Removed: []int64{f.ID}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	if a := e.asset(t, missing[0]); a.State != catalog.StateMissing {
		t.Errorf("missing asset = %+v, want it kept", a)
	}
	e.mustNotExist(t, "Staging/2025-05-30/a.jpg", "Staging/2025-05-30/b.jpg", "Staging/2025-05-30")
	for _, sha := range append(slices.Clone(staged), culled...) {
		if _, err := e.db.AssetBySHA(t.Context(), sha); !errors.Is(err, catalog.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", sha[:4], err)
		}
	}
	if a := e.asset(t, archived[0]); a.State != catalog.StateArchived {
		t.Errorf("archived asset = %+v", a)
	}
	if _, err := e.db.StagingFolderByID(t.Context(), f.ID); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("folder row: err = %v, want ErrNotFound", err)
	}
	in, err := e.db.Ingest(t.Context(), id)
	if err != nil || in.ForgottenAt.IsZero() {
		t.Errorf("ingest = %+v, %v; want ForgottenAt set", in, err)
	}
	if want := slices.Sorted(slices.Values(append(slices.Clone(staged), culled...))); !reflect.DeepEqual(e.removedHashes(), want) {
		t.Errorf("removed hashes = %v, want %v", e.removedHashes(), want)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
}

func TestForgetKeepsFiles(t *testing.T) {
	e := newOpsEnv(t)
	id, f, staged, culled, _ := importOf(t, e, forgetStaged, []string{"culled"}, []string{"archived"})
	v := e.version()
	got, err := e.svc.Forget(t.Context(), id, false)
	if err != nil {
		t.Fatal(err)
	}
	want := ForgetResult{Forgotten: 3, KeptArchived: 1, Removed: []int64{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("result = %+v, want %+v", got, want)
	}
	e.mustExist(t, "Staging/2025-05-30/a.jpg", "Staging/2025-05-30/b.jpg")
	for _, sha := range append(slices.Clone(staged), culled...) {
		if _, err := e.db.AssetBySHA(t.Context(), sha); !errors.Is(err, catalog.ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", sha[:4], err)
		}
	}
	if _, err := e.db.StagingFolderByID(t.Context(), f.ID); err != nil {
		t.Errorf("folder row: %v", err)
	}
	e.bumped(t, v)
	e.lockFree(t, f.ID)
}

func TestForgetRefuses(t *testing.T) {
	e := newOpsEnv(t)
	ctx := t.Context()
	running, err := e.db.CreateIngest(ctx, "/cards/R", "R", "", opsNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.db.StartIngest(ctx, running.ID, opsNow); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Forget(ctx, running.ID, true); !errors.Is(err, ErrIngestRunning) {
		t.Errorf("running ingest: err = %v, want ErrIngestRunning", err)
	}
	if _, err := e.svc.ForgetCulled(ctx, running.ID); !errors.Is(err, ErrIngestRunning) {
		t.Errorf("running ingest, culled: err = %v, want ErrIngestRunning", err)
	}
	id, _, _, _, _ := importOf(t, e, forgetStaged, nil, nil)
	if _, err := e.svc.Forget(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Forget(ctx, id, true); !errors.Is(err, ErrAlreadyForgotten) {
		t.Errorf("forgotten ingest: err = %v, want ErrAlreadyForgotten", err)
	}
	if s, err := e.svc.ForgetSummary(ctx, id); err != nil || !s.Forgotten {
		t.Errorf("summary of a forgotten import = %+v, %v; want Forgotten", s, err)
	}
	if _, err := e.svc.Forget(ctx, 99, true); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("unknown ingest: err = %v, want ErrNotFound", err)
	}
}

// forgetUntouched fails unless the import still has its files, rows and no
// forgotten mark.
func forgetUntouched(t *testing.T, e *opsEnv, id int64, staged []string) {
	t.Helper()
	e.mustExist(t, "Staging/2025-05-30/a.jpg", "Staging/2025-05-30/b.jpg")
	for _, sha := range staged {
		if a := e.asset(t, sha); a.State != catalog.StateStaged {
			t.Errorf("asset %s = %s, want staged", sha[:4], a.State)
		}
	}
	in, err := e.db.Ingest(t.Context(), id)
	if err != nil || !in.ForgottenAt.IsZero() {
		t.Errorf("ingest = %+v, %v; want not forgotten", in, err)
	}
}

func TestForgetBusyFolder(t *testing.T) {
	e := newOpsEnv(t)
	id, f, staged, _, _ := importOf(t, e, forgetStaged, []string{"culled"}, nil)
	release, err := e.locks.TryLock(HolderIngest, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Forget(t.Context(), id, true)
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder != HolderIngest || busy.ID != f.ID {
		t.Fatalf("err = %v, want *BusyError held by the ingest", err)
	}
	forgetUntouched(t, e, id, staged)
	if shas, err := e.db.CulledSeenBefore(t.Context(), id); err != nil || len(shas) != 1 {
		t.Errorf("culled files = %v, %v; want 1", shas, err)
	}
	release()
	e.lockFree(t, f.ID)
}

func TestForgetBusyPromote(t *testing.T) {
	e := newOpsEnv(t)
	id, f, staged, _, _ := importOf(t, e, forgetStaged, nil, nil)
	release, err := e.locks.TryLock(HolderPromote, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.svc.Forget(t.Context(), id, false)
	var busy *BusyError
	if !errors.As(err, &busy) || busy.Holder != HolderPromote {
		t.Fatalf("err = %v, want *BusyError held by a promote", err)
	}
	forgetUntouched(t, e, id, staged)
	release()
	e.lockFree(t, f.ID)
}

func TestForgetCulled(t *testing.T) {
	e := newOpsEnv(t)
	id, f, staged, culled, _ := importOf(t, e, forgetStaged, []string{"culled"}, []string{"archived"})
	before := e.files(t)
	got, err := e.svc.ForgetCulled(t.Context(), id)
	if err != nil || got != 1 {
		t.Fatalf("ForgetCulled = %d, %v; want 1", got, err)
	}
	if _, err := e.db.AssetBySHA(t.Context(), culled[0]); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("culled asset: err = %v, want ErrNotFound", err)
	}
	forgetUntouched(t, e, id, staged)
	if after := e.files(t); !reflect.DeepEqual(after, before) {
		t.Errorf("files = %v, want %v", after, before)
	}
	if want := culled; !reflect.DeepEqual(e.removedHashes(), want) {
		t.Errorf("removed hashes = %v, want %v", e.removedHashes(), want)
	}
	got, err = e.svc.ForgetCulled(t.Context(), id)
	if err != nil || got != 0 {
		t.Errorf("second ForgetCulled = %d, %v; want 0", got, err)
	}
	e.lockFree(t, f.ID)
}

// A move that lands between Forget's first read and its locks leaves a file
// in a folder Forget did not lock: nothing is deleted or forgotten.
func TestForgetChangedUnderLock(t *testing.T) {
	e := newOpsEnv(t)
	id, f, staged, _, _ := importOf(t, e, forgetStaged, []string{"culled"}, nil)
	forgetAfterRead = func() {
		if _, err := e.svc.Move(t.Context(), MoveRequest{SHAs: staged[:1], NewTitle: "Other"}); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { forgetAfterRead = nil })
	_, err := e.svc.Forget(t.Context(), id, true)
	if !errors.Is(err, ErrChanged) {
		t.Fatalf("err = %v, want ErrChanged", err)
	}
	forgetAfterRead = nil
	e.mustExist(t, "Staging/2025-05-30/b.jpg", "Staging/2025-05-30 - Other/a.jpg")
	for _, sha := range staged {
		if a := e.asset(t, sha); a.State != catalog.StateStaged {
			t.Errorf("asset %s = %s, want staged", sha[:4], a.State)
		}
	}
	if in, err := e.db.Ingest(t.Context(), id); err != nil || !in.ForgottenAt.IsZero() {
		t.Errorf("ingest = %+v, %v; want not forgotten", in, err)
	}
	if shas, err := e.db.CulledSeenBefore(t.Context(), id); err != nil || len(shas) != 1 {
		t.Errorf("culled files = %v, %v; want 1", shas, err)
	}
	e.lockFree(t, f.ID)
	// Retrying after the reload works and deletes both files at their new places.
	got, err := e.svc.Forget(t.Context(), id, true)
	if err != nil || got.DeletedFiles != 2 {
		t.Fatalf("retry = %+v, %v; want 2 deleted files", got, err)
	}
	e.mustNotExist(t, "Staging/2025-05-30/b.jpg", "Staging/2025-05-30 - Other/a.jpg")
}

// A rename that lands in the gap keeps the folder id: Forget deletes the
// files at their new paths.
func TestForgetRenamedUnderLock(t *testing.T) {
	e := newOpsEnv(t)
	id, f, _, _, _ := importOf(t, e, forgetStaged, nil, nil)
	forgetAfterRead = func() {
		if _, err := e.svc.Rename(t.Context(), f.ID, "Trip"); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { forgetAfterRead = nil })
	got, err := e.svc.Forget(t.Context(), id, true)
	if err != nil || got.DeletedFiles != 2 || got.Forgotten != 2 {
		t.Fatalf("Forget = %+v, %v; want 2 deleted and forgotten", got, err)
	}
	if files := e.files(t); len(files) != 0 {
		t.Errorf("files left: %v", files)
	}
}

// Forgets of an import with no staged files take no folder lock; the loser
// of the race still gets ErrAlreadyForgotten.
func TestForgetTwiceWithoutStagedFiles(t *testing.T) {
	e := newOpsEnv(t)
	id, _, _, _, _ := importOf(t, e, nil, []string{"culled"}, nil)
	forgetAfterRead = func() {
		forgetAfterRead = nil
		if _, err := e.svc.Forget(t.Context(), id, false); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { forgetAfterRead = nil })
	_, err := e.svc.Forget(t.Context(), id, false)
	if !errors.Is(err, ErrAlreadyForgotten) {
		t.Fatalf("err = %v, want ErrAlreadyForgotten", err)
	}
}
