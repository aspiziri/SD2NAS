package ingest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

const restoreCardJPG = "DCIM/100MSDCF/DSC00001.JPG"

// missingOnCard writes a SONY JPEG to e.cards and records asset sha (the
// file's hash when sha is "") as missing from the archive, with the file's
// fingerprint. When request is set, an earlier import that found it missing
// asks for it to be imported again. It returns the card file and the sha.
func missingOnCard(t *testing.T, e *env, sha string, request bool) (string, string) {
	t.Helper()
	ctx := t.Context()
	p := writeJPEG(t, e.cards, restoreCardJPG, mt.SonyExif)
	if sha == "" {
		sha = mustHash(t, p)
	}
	a := catalog.Asset{SHA256: sha, Size: mustSize(t, p), Kind: media.KindImage, State: catalog.StateMissing,
		RelPath: "Archive/2025-05-30 - Trip/2025-05-31 01.30.12.JPG", UpdatedAt: testNow.Add(-time.Hour)}
	if err := e.cat.InsertAsset(ctx, a, mustFP(t, p)); err != nil {
		t.Fatal(err)
	}
	if !request {
		return p, sha
	}
	in, err := e.cat.CreateIngest(ctx, e.cards, "earlier", "", testNow.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if err := e.cat.StartIngest(ctx, in.ID, testNow.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	row := catalog.IngestFile{CardPath: restoreCardJPG, Size: a.Size, QuickFP: mustFP(t, p), SHA256: sha, Outcome: catalog.OutcomeMissing}
	if err := e.cat.AddIngestFiles(ctx, in.ID, []catalog.IngestFile{row}); err != nil {
		t.Fatal(err)
	}
	if err := e.cat.FinishIngest(ctx, catalog.Ingest{ID: in.ID, Status: catalog.StatusDone, FinishedAt: testNow.Add(-time.Hour),
		Counts: catalog.Counts{Seen: 1, Missing: 1}}); err != nil {
		t.Fatal(err)
	}
	if n, err := e.cat.RequestRestore(ctx, in.ID, testNow); err != nil || n != 1 {
		t.Fatalf("RequestRestore = %d, %v; want 1", n, err)
	}
	return p, sha
}

func mustAsset(t *testing.T, e *env, sha string) catalog.Asset {
	t.Helper()
	a, err := e.cat.AssetBySHA(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func wantRestored(t *testing.T, e *env, out catalog.Ingest, sha string) {
	t.Helper()
	wantDone(t, out, catalog.Counts{Seen: 1, New: 1}, true)
	wantStaged(t, e, sonyJPEG)
	a := mustAsset(t, e, sha)
	if a.State != catalog.StateStaged || a.RelPath != sonyJPEG || a.SHA256 != sha || a.TZOffset != "+02:00" ||
		a.CaptureTime.Format(catalog.CaptureLayout) != "2025-05-31T01:30:12" {
		t.Errorf("asset = %+v, want it staged at %s", a, sonyJPEG)
	}
	if f := e.files(t, out.ID)[restoreCardJPG]; f.Outcome != catalog.OutcomeNew || f.SHA256 != sha || f.RelPath != sonyJPEG {
		t.Errorf("row = %+v, want new at %s", f, sonyJPEG)
	}
	if req, err := e.cat.RestoreRequested(t.Context()); err != nil || len(req) != 0 {
		t.Errorf("RestoreRequested = %v, %v; want none left", req, err)
	}
	_, msg := Describe(out)
	if want := CardName(out) + ": 1 new"; !strings.HasPrefix(msg, want) {
		t.Errorf("message = %q, want it to start %q", msg, want)
	}
}

func TestImportRestoresRequested(t *testing.T) {
	e := newEnv(t)
	_, sha := missingOnCard(t, e, "", true)
	wantRestored(t, e, e.run(t, e.cards), sha)
}

func TestImportRestoreFullVerify(t *testing.T) {
	e := newEnv(t)
	if err := e.cat.SetSetting(t.Context(), catalog.KeyVerifyKnown, "full"); err != nil {
		t.Fatal(err)
	}
	_, sha := missingOnCard(t, e, "", true)
	wantRestored(t, e, e.run(t, e.cards), sha)
}

func TestImportMissingWithoutRequest(t *testing.T) {
	e := newEnv(t)
	_, sha := missingOnCard(t, e, "", false)
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{Seen: 1, Missing: 1}, false)
	wantStaged(t, e)
	if a := mustAsset(t, e, sha); a.State != catalog.StateMissing {
		t.Errorf("asset = %+v, want it still missing", a)
	}
}

// The card file shares the requested asset's fingerprint, but not its
// content: it is a new asset of its own and the request stays.
func TestImportRestoreOtherContent(t *testing.T) {
	e := newEnv(t)
	other := strings.Repeat("ab", 32)
	p, _ := missingOnCard(t, e, other, true)
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{Seen: 1, New: 1}, true)
	wantStaged(t, e, sonyJPEG)
	if a := mustAsset(t, e, mustHash(t, p)); a.State != catalog.StateStaged || a.RelPath != sonyJPEG || a.FirstIngest != out.ID {
		t.Errorf("card file asset = %+v, want a new staged asset", a)
	}
	if a := mustAsset(t, e, other); a.State != catalog.StateMissing {
		t.Errorf("requested asset = %+v, want it still missing", a)
	}
	if req, err := e.cat.RestoreRequested(t.Context()); err != nil || !req[other] {
		t.Errorf("RestoreRequested = %v, %v; want the request kept", req, err)
	}
}

// The archive copy turns up again between classify and record: the fresh
// copy is a duplicate and goes.
func TestImportRestoreLostRace(t *testing.T) {
	e := newEnv(t)
	_, sha := missingOnCard(t, e, "", true)
	copyFile := e.r.copyFile
	e.r.copyFile = func(ctx context.Context, src, dst string, progress func(int64)) (string, error) {
		if err := e.cat.SetArchivePath(t.Context(), sha, "Archive/found.JPG", 5, testNow); err != nil {
			t.Error(err)
		}
		return copyFile(ctx, src, dst, progress)
	}
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{Seen: 1, InArchive: 1}, true)
	wantStaged(t, e)
	if a := mustAsset(t, e, sha); a.State != catalog.StateArchived || a.RelPath != "Archive/found.JPG" {
		t.Errorf("asset = %+v, want it archived at Archive/found.JPG, untouched", a)
	}
	if f := e.files(t, out.ID)[restoreCardJPG]; f.Outcome != catalog.OutcomeInArchive || f.SHA256 != sha {
		t.Errorf("row = %+v, want in_archive", f)
	}
}

// RestoreMissing finding the asset no longer missing (the indexer won the
// race after the lookup) drops the copy and records the asset's outcome.
func TestRestoreMissingLostRaceDropsCopy(t *testing.T) {
	e := newEnv(t)
	p, sha := missingOnCard(t, e, "", true)
	if err := e.cat.SetArchivePath(t.Context(), sha, "Archive/found.JPG", 5, testNow); err != nil {
		t.Fatal(err)
	}
	in, err := e.cat.CreateIngest(t.Context(), e.cards, "card", "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	dst := writeFile(t, e.photos, sonyJPEG, []byte("the fresh copy"))
	j := &ingestRun{r: e.r, ctx: t.Context(), dbctx: t.Context(), id: in.ID, p: NewProgress(), log: e.r.log()}
	f := newFile{CardFile: CardFile{Path: p, Rel: restoreCardJPG, Size: mustSize(t, p), Kind: media.KindImage}, fp: mustFP(t, p)}
	a := catalog.Asset{SHA256: sha, Size: f.Size, Kind: f.Kind, State: catalog.StateStaged, RelPath: sonyJPEG, Mtime: 1, UpdatedAt: testNow}
	row := catalog.IngestFile{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, SHA256: sha, Outcome: catalog.OutcomeNew, RelPath: sonyJPEG}
	res := j.restoreMissing(f, dst, media.Capture{}, a, row)
	if res.recorded || res.row.Outcome != catalog.OutcomeInArchive || res.row.SHA256 != sha {
		t.Errorf("result = %+v, want an unrecorded in_archive row", res)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Errorf("the copy is still there: %v", err)
	}
	if got := mustAsset(t, e, sha); got.State != catalog.StateArchived || got.RelPath != "Archive/found.JPG" {
		t.Errorf("asset = %+v, want it untouched", got)
	}
	if files, err := e.cat.IngestFiles(t.Context(), in.ID); err != nil || len(files) != 0 {
		t.Errorf("ingest files = %+v, %v; want none written", files, err)
	}
}
