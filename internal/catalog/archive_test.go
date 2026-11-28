package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// archivedAt inserts archived asset hexN(n) at rel with fingerprint
// hexN(100+n), first ingest firstIngest (0 = none; the ingest must exist),
// and, when firstIngest != 0, one ingest-file row naming it.
func archivedAt(t *testing.T, d *DB, n byte, rel string, firstIngest int64) string {
	t.Helper()
	sha := hexN(n)
	a := Asset{SHA256: sha, Size: int64(n), Kind: media.KindImage, State: StateArchived, RelPath: rel,
		Mtime: int64(n) * 1000, FirstIngest: firstIngest, UpdatedAt: t0}
	if err := d.InsertAsset(t.Context(), a, hexN(100+n)); err != nil {
		t.Fatal(err)
	}
	if firstIngest != 0 {
		f := IngestFile{CardPath: fmt.Sprintf("DCIM/%d.JPG", n), Size: int64(n), SHA256: sha, Outcome: OutcomeNew}
		if err := d.AddIngestFiles(t.Context(), firstIngest, []IngestFile{f}); err != nil {
			t.Fatal(err)
		}
	}
	return sha
}

// setCols sets checked_at and promoted_at (zero = leave NULL) of asset sha.
func setCols(t *testing.T, d *DB, sha string, checked, promoted time.Time) {
	t.Helper()
	var c, p any
	if !checked.IsZero() {
		c = formatTime(checked)
	}
	if !promoted.IsZero() {
		p = formatTime(promoted)
	}
	if _, err := d.db.ExecContext(t.Context(), "UPDATE assets SET checked_at = ?, promoted_at = ? WHERE sha256 = ?", c, p, mustBlob(t, sha)); err != nil {
		t.Fatal(err)
	}
}

// rawCols reads checked_at and promoted_at of asset sha (zero = NULL).
func rawCols(t *testing.T, d *DB, sha string) (checked, promoted time.Time) {
	t.Helper()
	var c, p sql.NullString
	if err := d.db.QueryRowContext(t.Context(), "SELECT checked_at, promoted_at FROM assets WHERE sha256 = ?", mustBlob(t, sha)).Scan(&c, &p); err != nil {
		t.Fatal(err)
	}
	var err error
	if c.Valid {
		if checked, err = parseTime(c.String); err != nil {
			t.Fatal(err)
		}
	}
	if p.Valid {
		if promoted, err = parseTime(p.String); err != nil {
			t.Fatal(err)
		}
	}
	return checked, promoted
}

func TestArchiveEntriesAndCopies(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := archivedAt(t, d, 1, "Archive/2025/a.JPG", 0)
	m := archivedAt(t, d, 2, "Archive/2025/b.JPG", 0)
	if err := d.MarkMissing(ctx, m, t1); err != nil {
		t.Fatal(err)
	}
	stagedAt(t, d, 3, "Staging/x/c.JPG")
	archivedAt(t, d, 4, "Archive2/d.JPG", 0)
	setCols(t, d, a, t2, time.Time{})

	es, err := d.ArchiveEntries(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 || es[0].SHA256 != a || es[1].SHA256 != m {
		t.Fatalf("entries = %+v", es)
	}
	if e := es[0]; e.RelPath != "Archive/2025/a.JPG" || e.Size != 1 || e.Mtime != 1000 || e.State != StateArchived ||
		!e.UpdatedAt.Equal(t0) || !e.CheckedAt.Equal(t2) {
		t.Errorf("entry 0 = %+v", e)
	}
	if e := es[1]; e.State != StateMissing || !e.UpdatedAt.Equal(t1) || !e.CheckedAt.IsZero() {
		t.Errorf("entry 1 = %+v", e)
	}

	e, err := d.ArchiveEntryAt(ctx, "Archive/2025/a.JPG")
	if err != nil || e.SHA256 != a {
		t.Errorf("ArchiveEntryAt = %+v, %v", e, err)
	}
	if _, err := d.ArchiveEntryAt(ctx, "Archive/2025/b.JPG"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing one: err = %v, want ErrNotFound", err)
	}

	for _, c := range []ArchiveCopy{
		{RelPath: "Archive/copy/a.JPG", SHA256: a, Size: 1, Mtime: 5, CheckedAt: t1},
		{RelPath: "Archive/copy/z.JPG", SHA256: hexN(77), Size: 7, Mtime: 6, CheckedAt: t2},
	} {
		if err := d.PutArchiveCopy(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	cs, err := d.ArchiveCopies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 || cs[0].AssetState != StateArchived || cs[1].AssetState != "" || cs[1].SHA256 != hexN(77) ||
		cs[0].Size != 1 || cs[0].Mtime != 5 || !cs[0].CheckedAt.Equal(t1) {
		t.Errorf("copies = %+v", cs)
	}
	// Upsert by rel_path, and deleting an absent row is fine.
	if err := d.PutArchiveCopy(ctx, ArchiveCopy{RelPath: "Archive/copy/a.JPG", SHA256: a, Size: 9, Mtime: 9, CheckedAt: t2}); err != nil {
		t.Fatal(err)
	}
	if cs, _ = d.ArchiveCopies(ctx); len(cs) != 2 || cs[0].Size != 9 {
		t.Errorf("after upsert = %+v", cs)
	}
	if err := d.DeleteArchiveCopy(ctx, "Archive/copy/a.JPG"); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteArchiveCopy(ctx, "Archive/nope.JPG"); err != nil {
		t.Errorf("delete absent: %v", err)
	}
	if cs, _ = d.ArchiveCopies(ctx); len(cs) != 1 {
		t.Errorf("after delete = %+v", cs)
	}
}

func TestIndexAssetSetsCheckedAt(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := Asset{SHA256: hexN(1), Size: 5, Kind: media.KindRaw, State: StateArchived, RelPath: "Archive/s/a.ARW", Mtime: 42, UpdatedAt: t0}
	if err := d.IndexAsset(ctx, a, hexN(101), t2); err != nil {
		t.Fatal(err)
	}
	if c, p := rawCols(t, d, a.SHA256); !c.Equal(t2) || !p.IsZero() {
		t.Errorf("checked, promoted = %v, %v", c, p)
	}
	got, err := d.AssetsByFingerprint(ctx, hexN(101))
	if err != nil || len(got) != 1 || got[0].SHA256 != a.SHA256 {
		t.Errorf("fingerprint = %+v, %v", got, err)
	}
	// A failing insert leaves nothing behind.
	if err := d.IndexAsset(ctx, a, hexN(102), t2); err == nil {
		t.Fatal("duplicate: want error")
	}
	if got, _ := d.AssetsByFingerprint(ctx, hexN(102)); got != nil {
		t.Errorf("fingerprint of the failed insert: %+v", got)
	}
}

// An earlier version of an edited asset found as a file of its own becomes an
// asset again: its alias goes, the edited asset and its fingerprints stay.
func TestIndexAssetOfAliasKey(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	b := archivedAt(t, d, 2, "Archive/s/a.JPG", 0)
	a := hexN(1)
	if err := d.AddAlias(ctx, a, b, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.AddFingerprint(ctx, hexN(101), b); err != nil { // A's fingerprint, re-pointed by the change
		t.Fatal(err)
	}
	if err := d.IndexAsset(ctx, Asset{SHA256: a, Size: 1, Kind: media.KindImage, State: StateArchived,
		RelPath: "Archive/s/c.JPG", Mtime: 1000, UpdatedAt: t1}, hexN(101), t1); err != nil {
		t.Fatal(err)
	}
	if got, err := d.AssetBySHA(ctx, a); err != nil || got.SHA256 != a || got.RelPath != "Archive/s/c.JPG" {
		t.Errorf("AssetBySHA(A) = %+v, %v; want A itself", got, err)
	}
	var n int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM aliases").Scan(&n); err != nil || n != 0 {
		t.Errorf("aliases left = %d, %v", n, err)
	}
	if got, err := d.AssetBySHA(ctx, b); err != nil || got.RelPath != "Archive/s/a.JPG" || got.State != StateArchived {
		t.Errorf("B = %+v, %v", got, err)
	}
	got, err := d.AssetsByFingerprint(ctx, hexN(101))
	if err != nil || len(got) != 2 || got[0].SHA256 != a || got[1].SHA256 != b {
		t.Errorf("fingerprint finds %+v, %v; want A and B", got, err)
	}
	// A later change in place of A's file no longer trips over the old alias.
	e := ArchiveEntry{SHA256: a, RelPath: "Archive/s/c.JPG", Size: 1, Mtime: 1000, State: StateArchived, UpdatedAt: t1}
	if err := d.RecordArchiveChange(ctx, e, hexN(3), "", 3, 3000, t2); err != nil {
		t.Errorf("change of the re-indexed file: %v", err)
	}
}

func TestSetArchivePath(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	moved := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	setCols(t, d, moved, t0, t1)
	if err := d.SetArchivePath(ctx, moved, "Archive/s/sub/a.JPG", 777, t2); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, moved, "Archive/s/sub/a.JPG", StateArchived)
	if a, _ := d.AssetBySHA(ctx, moved); a.Mtime != 777 || !a.UpdatedAt.Equal(t2) {
		t.Errorf("moved = %+v", a)
	}
	if c, p := rawCols(t, d, moved); !c.Equal(t2) || !p.Equal(t1) {
		t.Errorf("checked, promoted = %v, %v; want %v, %v", c, p, t2, t1)
	}

	gone := archivedAt(t, d, 2, "Archive/s/b.JPG", 0)
	if err := d.MarkMissing(ctx, gone, t1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetArchivePath(ctx, gone, "Archive/s/b.JPG", 5, t2); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, gone, "Archive/s/b.JPG", StateArchived)

	culled := stagedAt(t, d, 3, "Staging/x/c.JPG")
	if err := d.CullAsset(ctx, culled, t1); err != nil {
		t.Fatal(err)
	}
	if err := d.SetArchivePath(ctx, culled, "Archive/s/c.JPG", 6, t2); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, culled, "Archive/s/c.JPG", StateArchived)
	if a, _ := d.AssetBySHA(ctx, culled); a.Mtime != 6 {
		t.Errorf("recovered mtime = %d", a.Mtime)
	}

	staged := stagedAt(t, d, 4, "Staging/x/d.JPG")
	wrongState(t, d.SetArchivePath(ctx, staged, "Archive/s/d.JPG", 7, t2), "staged")
	wantPath(t, d, staged, "Staging/x/d.JPG", StateStaged)
	if err := d.SetArchivePath(ctx, hexN(99), "Archive/s/z.JPG", 7, t2); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown: %v", err)
	}
}

func TestMarkMissing(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	sha := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	if err := d.MarkMissing(ctx, sha, t2); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, sha, "Archive/s/a.JPG", StateMissing)
	if a, _ := d.AssetBySHA(ctx, sha); !a.UpdatedAt.Equal(t2) {
		t.Errorf("updated_at = %v", a.UpdatedAt)
	}
	wrongState(t, d.MarkMissing(ctx, sha, t2), "missing")
}

// changeSetup is the asset A of the change tests: raw, captured, first
// ingest 1, promoted, fingerprint FA, alias X -> A, an ingest-file row
// and a clock warning.
func changeSetup(t *testing.T, d *DB) (in Ingest, a string, entry ArchiveEntry) {
	t.Helper()
	ctx := t.Context()
	in = newIngest(t, d)
	a = hexN(1)
	asset := Asset{SHA256: a, Size: 10, Kind: media.KindRaw, State: StateArchived, RelPath: "Archive/s/a.ARW", Mtime: 111,
		CaptureTime: time.Date(2025, 5, 30, 10, 0, 0, 0, time.Local), TZOffset: "+02:00", FirstIngest: in.ID, UpdatedAt: t0}
	if err := d.InsertAsset(ctx, asset, hexN(101)); err != nil {
		t.Fatal(err)
	}
	setCols(t, d, a, t1, t2)
	if err := d.AddAlias(ctx, hexN(50), a, t0); err != nil {
		t.Fatal(err)
	}
	if err := d.AddIngestFiles(ctx, in.ID, []IngestFile{{CardPath: "DCIM/a.ARW", Size: 10, SHA256: a, Outcome: OutcomeNew}}); err != nil {
		t.Fatal(err)
	}
	if err := d.AddClockWarnings(ctx, in.ID, []ClockWarning{{CardPath: "DCIM/a.ARW", SHA256: a, Kind: ClockFuture, Detail: "x"}}); err != nil {
		t.Fatal(err)
	}
	entry, err := d.ArchiveEntryAt(ctx, "Archive/s/a.ARW")
	if err != nil {
		t.Fatal(err)
	}
	return in, a, entry
}

func TestRecordArchiveChange(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in, a, entry := changeSetup(t, d)
	b := hexN(2)
	if err := d.PutArchiveCopy(ctx, ArchiveCopy{RelPath: "Archive/s/copy.ARW", SHA256: a, Size: 10, Mtime: 1, CheckedAt: t1}); err != nil {
		t.Fatal(err)
	}

	at := t2.Add(time.Hour)
	if err := d.RecordArchiveChange(ctx, entry, b, hexN(102), 20, 222, at); err != nil {
		t.Fatal(err)
	}
	if m, _ := d.AssetsByHash(ctx, []string{a}); len(m) != 0 {
		t.Errorf("A is still an asset: %+v", m)
	}
	nb, err := d.AssetBySHA(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if nb.State != StateArchived || nb.RelPath != "Archive/s/a.ARW" || nb.Size != 20 || nb.Mtime != 222 ||
		nb.Kind != media.KindRaw || !nb.CaptureTime.Equal(time.Date(2025, 5, 30, 10, 0, 0, 0, time.Local)) ||
		nb.TZOffset != "+02:00" || nb.FirstIngest != in.ID || !nb.UpdatedAt.Equal(at) {
		t.Errorf("B = %+v", nb)
	}
	if c, p := rawCols(t, d, b); !c.Equal(at) || !p.Equal(t2) {
		t.Errorf("B checked, promoted = %v, %v", c, p)
	}
	for _, old := range []string{a, hexN(50)} {
		if got, err := d.AssetBySHA(ctx, old); err != nil || got.SHA256 != b {
			t.Errorf("AssetBySHA(%s) = %+v, %v; want B", old[:4], got, err)
		}
	}
	for _, fp := range []string{hexN(101), hexN(102)} {
		if got, _ := d.AssetsByFingerprint(ctx, fp); len(got) != 1 || got[0].SHA256 != b {
			t.Errorf("AssetsByFingerprint(%s) = %+v", fp[:4], got)
		}
	}
	files, err := d.IngestFiles(ctx, in.ID)
	if err != nil || len(files) != 1 || files[0].SHA256 != b || files[0].RelPath != "Archive/s/a.ARW" {
		t.Errorf("ingest files = %+v, %v", files, err)
	}
	var warn []byte
	if err := d.db.QueryRowContext(ctx, "SELECT sha256 FROM clock_warnings").Scan(&warn); err != nil || blobToHex(warn) != b {
		t.Errorf("clock warning sha = %x, %v", warn, err)
	}
	if cs, _ := d.ArchiveCopies(ctx); len(cs) != 0 {
		t.Errorf("stale copy kept: %+v", cs)
	}

	cs, total, latest, err := d.ArchiveChanges(ctx, 10)
	if err != nil || total != 1 || len(cs) != 1 || latest != cs[0].ID {
		t.Fatalf("changes = %+v, %d, %d, %v", cs, total, latest, err)
	}
	if c := cs[0]; c.RelPath != "Archive/s/a.ARW" || c.OldSHA256 != a || c.SHA256 != b || c.OldSize != 10 || c.Size != 20 ||
		!c.IndexedAt.Equal(t1) || !c.ChangedAt.Equal(at) {
		t.Errorf("change = %+v", c)
	}
}

func TestRecordArchiveChangeIndexedAtFallsBackToUpdatedAt(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	entry, err := d.ArchiveEntryAt(ctx, "Archive/s/a.JPG")
	if err != nil || !entry.CheckedAt.IsZero() {
		t.Fatal(entry, err)
	}
	if err := d.RecordArchiveChange(ctx, entry, hexN(2), "", 20, 2, t2); err != nil {
		t.Fatal(err)
	}
	cs, _, _, _ := d.ArchiveChanges(ctx, 10)
	if len(cs) != 1 || cs[0].OldSHA256 != a || !cs[0].IndexedAt.Equal(t0) {
		t.Errorf("changes = %+v", cs)
	}
	if err := d.RecordArchiveChange(ctx, entry, "xyz", "", 1, 1, t2); !errors.Is(err, ErrBadHash) {
		t.Errorf("bad hash: %v", err)
	}
	if err := d.RecordArchiveChange(ctx, entry, entry.SHA256, "", 1, 1, t2); err == nil {
		t.Error("same hash: want error")
	}
}

func TestRecordArchiveChangeTwiceAndRevert(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	_, a, entry := changeSetup(t, d)
	b, c := hexN(2), hexN(3)

	if err := d.RecordArchiveChange(ctx, entry, b, hexN(102), 20, 2, t2); err != nil {
		t.Fatal(err)
	}
	first, _, _, _ := d.ArchiveChanges(ctx, 10)
	entryB, err := d.ArchiveEntryAt(ctx, "Archive/s/a.ARW")
	if err != nil || entryB.SHA256 != b {
		t.Fatal(entryB, err)
	}
	if err := d.RecordArchiveChange(ctx, entryB, c, hexN(103), 30, 3, t2.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	cs, total, _, _ := d.ArchiveChanges(ctx, 10)
	if total != 1 || len(cs) != 1 || cs[0].OldSHA256 != a || cs[0].SHA256 != c || cs[0].OldSize != 10 || cs[0].Size != 30 ||
		!cs[0].IndexedAt.Equal(t1) || cs[0].ID <= first[0].ID {
		t.Fatalf("after A->B->C: %+v (first id %d)", cs, first[0].ID)
	}

	entryC, _ := d.ArchiveEntryAt(ctx, "Archive/s/a.ARW")
	if err := d.RecordArchiveChange(ctx, entryC, a, hexN(101), 10, 111, t2.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if m, _ := d.AssetsByHash(ctx, []string{a}); len(m) != 1 || m[a].State != StateArchived {
		t.Errorf("A is not an asset again: %+v", m)
	}
	var n int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM aliases WHERE old_sha256 = ?", mustBlob(t, a)).Scan(&n); err != nil || n != 0 {
		t.Errorf("aliases with old = A: %d, %v", n, err)
	}
	if got, err := d.AssetBySHA(ctx, c); err != nil || got.SHA256 != a {
		t.Errorf("AssetBySHA(C) = %+v, %v; want A", got, err)
	}
	if got, err := d.AssetBySHA(ctx, b); err != nil || got.SHA256 != a {
		t.Errorf("AssetBySHA(B) = %+v, %v; want A", got, err)
	}
	if cs, total, latest, _ := d.ArchiveChanges(ctx, 10); len(cs) != 0 || total != 0 || latest != 0 {
		t.Errorf("entry survived the revert: %+v", cs)
	}
}

func TestRecordArchiveChangeWrongState(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	_, a, entry := changeSetup(t, d)
	if err := d.MarkMissing(ctx, a, t2); err != nil {
		t.Fatal(err)
	}
	wrongState(t, d.RecordArchiveChange(ctx, entry, hexN(2), hexN(102), 20, 2, t2), "missing")

	staged := stagedAt(t, d, 5, "Staging/x/e.JPG")
	se := ArchiveEntry{SHA256: staged, RelPath: "Archive/x/e.JPG", Size: 5, UpdatedAt: t0}
	wrongState(t, d.RecordArchiveChange(ctx, se, hexN(6), hexN(106), 6, 6, t2), "staged")

	unknown := ArchiveEntry{SHA256: hexN(88), RelPath: "Archive/q.JPG", UpdatedAt: t0}
	if err := d.RecordArchiveChange(ctx, unknown, hexN(6), "", 6, 6, t2); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown old: %v", err)
	}

	// Nothing changed: the old rows, fingerprints and aliases are intact.
	if m, _ := d.AssetsByHash(ctx, []string{a, staged, hexN(2), hexN(6)}); len(m) != 2 || m[a].State != StateMissing || m[staged].State != StateStaged {
		t.Errorf("assets = %+v", m)
	}
	if got, _ := d.AssetsByFingerprint(ctx, hexN(101)); len(got) != 1 || got[0].SHA256 != a {
		t.Errorf("fingerprint FA = %+v", got)
	}
	if got, _ := d.AssetsByFingerprint(ctx, hexN(102)); got != nil {
		t.Errorf("fingerprint FB = %+v", got)
	}
	var n int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM aliases WHERE sha256 = ?", mustBlob(t, a)).Scan(&n); err != nil || n != 1 {
		t.Errorf("aliases of A: %d, %v", n, err)
	}
	if _, total, _, _ := d.ArchiveChanges(ctx, 10); total != 0 {
		t.Errorf("changes = %d", total)
	}
}

// changeOne changes the archived asset at rel to hexN(newN).
func changeOne(t *testing.T, d *DB, rel string, newN byte) {
	t.Helper()
	e, err := d.ArchiveEntryAt(t.Context(), rel)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordArchiveChange(t.Context(), e, hexN(newN), "", 1, 1, t2); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveChangesOrderAndLatest(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	var rels []string
	for i, name := range []string{"c.JPG", "a.JPG", "b.JPG"} {
		n := byte(10 + i)
		rel := "Archive/s/" + name
		archivedAt(t, d, n, rel, 0)
		changeOne(t, d, rel, n+20)
		rels = append(rels, rel)
	}
	cs, total, latest, err := d.ArchiveChanges(ctx, 10)
	if err != nil || total != 3 || len(cs) != 3 {
		t.Fatalf("changes = %+v, %d, %v", cs, total, err)
	}
	for i, c := range cs {
		if c.RelPath != rels[i] {
			t.Errorf("change %d = %s, want %s", i, c.RelPath, rels[i])
		}
	}
	if latest != cs[2].ID || !(cs[0].ID < cs[1].ID && cs[1].ID < cs[2].ID) {
		t.Errorf("ids = %d %d %d, latest %d", cs[0].ID, cs[1].ID, cs[2].ID, latest)
	}
	cs2, total, latest, err := d.ArchiveChanges(ctx, 2)
	if err != nil || len(cs2) != 2 || total != 3 || latest != cs2[1].ID || latest != cs[1].ID {
		t.Errorf("limit 2 = %+v, total %d, latest %d, %v", cs2, total, latest, err)
	}
}

func TestAcceptArchiveChanges(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	for i := range 3 {
		rel := fmt.Sprintf("Archive/s/%d.JPG", i)
		archivedAt(t, d, byte(10+i), rel, 0)
		changeOne(t, d, rel, byte(30+i))
	}
	cs, _, _, _ := d.ArchiveChanges(ctx, 10)
	n, err := d.AcceptArchiveChanges(ctx, cs[1].ID)
	if err != nil || n != 2 {
		t.Fatalf("accept = %d, %v; want 2", n, err)
	}
	left, total, _, _ := d.ArchiveChanges(ctx, 10)
	if total != 1 || len(left) != 1 || left[0].ID != cs[2].ID {
		t.Errorf("left = %+v", left)
	}
	if _, err := d.AcceptArchiveChanges(ctx, 0); err == nil {
		t.Error("upTo 0: want error")
	}
}

func TestArchiveCounts(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in := newIngest(t, d)
	archivedAt(t, d, 1, "Archive/s/a.JPG", in.ID)
	archivedAt(t, d, 2, "Archive/s/b.JPG", 0)
	m := archivedAt(t, d, 3, "Archive/s/c.JPG", in.ID)
	if err := d.MarkMissing(ctx, m, t1); err != nil {
		t.Fatal(err)
	}
	archivedAt(t, d, 4, "Archive2/x.JPG", in.ID)
	stagedAt(t, d, 5, "Staging/x/e.JPG")
	e, _ := d.ArchiveEntryAt(ctx, "Archive/s/b.JPG")
	if err := d.RecordArchiveChange(ctx, e, hexN(9), "", 90, 1, t2); err != nil {
		t.Fatal(err)
	}
	if err := d.PutArchiveCopy(ctx, ArchiveCopy{RelPath: "Archive/s/copy.JPG", SHA256: hexN(1), Size: 1, Mtime: 1, CheckedAt: t1}); err != nil {
		t.Fatal(err)
	}
	got, err := d.ArchiveCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Archived under Archive/: a (size 1) and the changed b -> 9 (size 90).
	// Imported: a, c, x (first_ingest set); the changed b has none.
	want := ArchiveCounts{Files: 2, Bytes: 91, Missing: 1, Changed: 1, Copies: 1, Imported: 3}
	if got != want {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

func TestMissingFiles(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	for i, name := range []string{"c.JPG", "a.JPG", "b.JPG"} {
		sha := archivedAt(t, d, byte(1+i), "Archive/s/"+name, 0)
		if err := d.MarkMissing(ctx, sha, t1.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	archivedAt(t, d, 9, "Archive/s/ok.JPG", 0)
	got, err := d.MissingFiles(ctx, 2)
	if err != nil || len(got) != 2 || got[0].RelPath != "Archive/s/a.JPG" || got[1].RelPath != "Archive/s/b.JPG" ||
		got[0].Size != 2 || !got[0].Since.Equal(t1.Add(time.Minute)) {
		t.Errorf("missing = %+v, %v", got, err)
	}
	if all, _ := d.MissingFiles(ctx, 10); len(all) != 3 {
		t.Errorf("all = %+v", all)
	}
}

func TestIgnoreMissing(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	b := archivedAt(t, d, 2, "Archive/s/b.JPG", 0)
	for _, sha := range []string{a, b} {
		if err := d.MarkMissing(ctx, sha, t1); err != nil {
			t.Fatal(err)
		}
	}
	putRequest(t, d, a, t2)
	if err := d.IgnoreMissing(ctx, a); err != nil {
		t.Fatal(err)
	}
	// culled, as a cull would leave it, but the path stays to list it by
	wantPath(t, d, a, "Archive/s/a.JPG", StateCulled)
	if got, _ := d.AssetBySHA(ctx, a); !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at = %v, want the missing time %v", got.UpdatedAt, t1)
	}
	if r := requests(t, d); len(r) != 0 {
		t.Errorf("restore requests = %v, want the ignored file's gone", r)
	}
	c, err := d.ArchiveCounts(ctx)
	if err != nil || c.Missing != 1 || c.Ignored != 1 {
		t.Errorf("counts = %+v, %v; want 1 missing, 1 ignored", c, err)
	}
	if m, _ := d.MissingFiles(ctx, 10); len(m) != 1 || m[0].SHA256 != b {
		t.Errorf("missing = %+v, want only b", m)
	}
	ig, err := d.IgnoredFiles(ctx, 10)
	if err != nil || len(ig) != 1 || ig[0].SHA256 != a || ig[0].RelPath != "Archive/s/a.JPG" || ig[0].Size != 1 || !ig[0].Since.Equal(t1) {
		t.Errorf("ignored = %+v, %v", ig, err)
	}
	if n, _, _ := d.MissingSince(ctx, t0); n != 1 {
		t.Errorf("MissingSince = %d, want 1 (ignored files are not missing)", n)
	}
	wrongState(t, d.IgnoreMissing(ctx, a), "culled")

	if err := d.UnignoreMissing(ctx, a); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, a, "Archive/s/a.JPG", StateMissing)
	if got, _ := d.AssetBySHA(ctx, a); !got.UpdatedAt.Equal(t1) {
		t.Errorf("updated_at after un-ignore = %v, want %v", got.UpdatedAt, t1)
	}
	if n, _, _ := d.MissingSince(ctx, t1); n != 0 {
		t.Errorf("MissingSince(t1) = %d after un-ignore, want 0 (no new notice)", n)
	}
	wrongState(t, d.UnignoreMissing(ctx, a), "missing")
}

func TestIgnoreMissingOnlyArchiveFiles(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	arch := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	wrongState(t, d.IgnoreMissing(ctx, arch), "archived")
	wrongState(t, d.UnignoreMissing(ctx, arch), "archived")
	// A cull from Staging is not an ignored file: it has no path to go back to.
	culled := stagedAt(t, d, 2, "Staging/x/b.JPG")
	if err := d.CullAsset(ctx, culled, t1); err != nil {
		t.Fatal(err)
	}
	wrongState(t, d.UnignoreMissing(ctx, culled), "culled")
	wantPath(t, d, culled, "", StateCulled)
	if ig, _ := d.IgnoredFiles(ctx, 10); len(ig) != 0 {
		t.Errorf("ignored = %+v, want none", ig)
	}
	if c, _ := d.ArchiveCounts(ctx); c.Ignored != 0 {
		t.Errorf("ignored count = %d, want 0", c.Ignored)
	}
	if err := d.IgnoreMissing(ctx, hexN(9)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}
	if err := d.IgnoreMissing(ctx, "nope"); !errors.Is(err, ErrBadHash) {
		t.Errorf("bad hash: err = %v, want ErrBadHash", err)
	}
}

func TestMissingSince(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	b := archivedAt(t, d, 2, "Archive/s/b.JPG", 0)
	back := archivedAt(t, d, 3, "Archive/s/c.JPG", 0)
	if err := d.MarkMissing(ctx, a, t1); err != nil {
		t.Fatal(err)
	}
	if err := d.MarkMissing(ctx, b, t2); err != nil {
		t.Fatal(err)
	}
	// missing, then found again: archived, so not counted
	if err := d.MarkMissing(ctx, back, t2); err != nil {
		t.Fatal(err)
	}
	if err := d.SetArchivePath(ctx, back, "Archive/s/c.JPG", 3, t2.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		after  time.Time
		n      int
		latest time.Time
	}{
		{t1.Add(-time.Millisecond), 2, t2},
		{t1, 1, t2},
		{t2, 0, time.Time{}},
	} {
		n, latest, err := d.MissingSince(ctx, tc.after)
		if err != nil || n != tc.n || !latest.Equal(tc.latest) {
			t.Errorf("MissingSince(%v) = %d, %v, %v; want %d, %v", tc.after, n, latest, err, tc.n, tc.latest)
		}
	}
}

func TestRecentArchiveFolders(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := archivedAt(t, d, 1, "Archive/2025-05-30 - Zürich/a.JPG", 0)
	b := archivedAt(t, d, 2, "Archive/2025-05-30 - Zürich/Capture/b.JPG", 0)
	c := archivedAt(t, d, 3, "Archive/2025-06-01 - Trip/c.JPG", 0)
	root := archivedAt(t, d, 4, "Archive/loose.JPG", 0)
	archivedAt(t, d, 5, "Archive/2025-07-01 - Old/d.JPG", 0) // no promoted_at
	trap := archivedAt(t, d, 6, "Archive2/x/e.JPG", 0)
	setCols(t, d, a, time.Time{}, t1)
	setCols(t, d, b, time.Time{}, t2)
	setCols(t, d, c, time.Time{}, t1.Add(time.Minute))
	setCols(t, d, root, time.Time{}, t2.Add(time.Hour))
	setCols(t, d, trap, time.Time{}, t2.Add(time.Hour))

	got, err := d.RecentArchiveFolders(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	want := []RecentFolder{
		{Folder: "Archive/2025-05-30 - Zürich", Files: 2, At: t2},
		{Folder: "Archive/2025-06-01 - Trip", Files: 1, At: t1.Add(time.Minute)},
	}
	if len(got) != 2 {
		t.Fatalf("folders = %+v", got)
	}
	for i := range want {
		if got[i].Folder != want[i].Folder || got[i].Files != want[i].Files || !got[i].At.Equal(want[i].At) {
			t.Errorf("folder %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A move does not change promoted_at.
	if err := d.SetArchivePath(ctx, c, "Archive/2025-06-01 - Trip/sub/c.JPG", 1, t2.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	again, _ := d.RecentArchiveFolders(ctx, 10)
	if len(again) != 2 || again[1].Folder != want[1].Folder || !again[1].At.Equal(want[1].At) {
		t.Errorf("after move = %+v", again)
	}
	if one, _ := d.RecentArchiveFolders(ctx, 1); len(one) != 1 || one[0].Folder != want[0].Folder {
		t.Errorf("limit 1 = %+v", one)
	}
}

func TestArchiveScans(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	if _, err := d.LastArchiveScan(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty: %v", err)
	}
	if _, err := d.StartArchiveScan(ctx, "backup", time.Time{}, t0); err == nil {
		t.Error("kind backup: want error")
	}
	id1, err := d.StartArchiveScan(ctx, "manual", time.Time{}, t0)
	if err != nil {
		t.Fatal(err)
	}
	id2, err := d.StartArchiveScan(ctx, "nightly", t0, t1)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.RecordArchiveBackup(ctx, t2, true, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishArchiveScan(ctx, id1, t1, true, `{"a":1}`); err != nil {
		t.Fatal(err)
	}
	if err := d.FinishArchiveScan(ctx, 9999, t1, true, ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: %v", err)
	}
	last, err := d.LastArchiveScan(ctx)
	if err != nil || last.ID != id1 || last.Kind != "manual" || last.Reread || !last.RereadBefore.IsZero() ||
		!last.StartedAt.Equal(t0) || !last.FinishedAt.Equal(t1) || !last.Complete || last.Result != `{"a":1}` {
		t.Errorf("last = %+v, %v", last, err)
	}
	un, err := d.UnfinishedArchiveScans(ctx)
	if err != nil || len(un) != 1 || un[0].ID != id2 || !un[0].Reread || !un[0].RereadBefore.Equal(t0) || !un[0].FinishedAt.IsZero() {
		t.Errorf("unfinished = %+v, %v", un, err)
	}

	// 31 more finished passes keep 30 pass rows and every backup row.
	for i := range 31 {
		at := t2.Add(time.Duration(i) * time.Minute)
		id, err := d.StartArchiveScan(ctx, "nightly", time.Time{}, at)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.FinishArchiveScan(ctx, id, at, true, ""); err != nil {
			t.Fatal(err)
		}
	}
	var passes, backups int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM archive_scans WHERE kind != 'backup'").Scan(&passes); err != nil {
		t.Fatal(err)
	}
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM archive_scans WHERE kind = 'backup'").Scan(&backups); err != nil {
		t.Fatal(err)
	}
	if passes != 30 || backups != 1 {
		t.Errorf("passes = %d, backups = %d; want 30, 1", passes, backups)
	}
}

func TestPendingReread(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	pending := func() time.Time {
		t.Helper()
		got, err := d.PendingReread(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := pending(); !got.IsZero() {
		t.Errorf("no re-index: %v", got)
	}
	plain, _ := d.StartArchiveScan(ctx, "manual", time.Time{}, t0)
	if err := d.FinishArchiveScan(ctx, plain, t0, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := pending(); !got.IsZero() {
		t.Errorf("plain pass: %v", got)
	}
	id, err := d.StartArchiveScan(ctx, "manual", t1, t1)
	if err != nil {
		t.Fatal(err)
	}
	if got := pending(); !got.Equal(t1) {
		t.Errorf("unfinished re-index: %v, want %v", got, t1)
	}
	if err := d.FinishArchiveScan(ctx, id, t1, false, ""); err != nil {
		t.Fatal(err)
	}
	if got := pending(); !got.Equal(t1) {
		t.Errorf("incomplete re-index: %v, want %v", got, t1)
	}
	id, _ = d.StartArchiveScan(ctx, "nightly", t1, t2)
	if err := d.FinishArchiveScan(ctx, id, t2, true, ""); err != nil {
		t.Fatal(err)
	}
	if got := pending(); !got.IsZero() {
		t.Errorf("completed: %v", got)
	}
}

func TestArchiveBackups(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	if _, err := d.LastArchiveBackup(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty: %v", err)
	}
	if err := d.RecordArchiveBackup(ctx, t0, true, "first"); err != nil {
		t.Fatal(err)
	}
	if err := d.RecordArchiveBackup(ctx, t1, false, "second"); err != nil {
		t.Fatal(err)
	}
	b, err := d.LastArchiveBackup(ctx)
	if err != nil || b.Result != "second" || b.Complete || b.Kind != "backup" || !b.StartedAt.Equal(t1) || !b.FinishedAt.Equal(t1) {
		t.Errorf("last backup = %+v, %v", b, err)
	}
	for i := range 31 {
		if err := d.RecordArchiveBackup(ctx, t2.Add(time.Duration(i)*time.Minute), true, ""); err != nil {
			t.Fatal(err)
		}
	}
	var n int
	if err := d.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM archive_scans WHERE kind = 'backup'").Scan(&n); err != nil || n != 30 {
		t.Errorf("backup rows = %d, %v; want 30", n, err)
	}
	if _, err := d.LastArchiveScan(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("backups leaked into LastArchiveScan: %v", err)
	}
}

func TestBackupVacuumInto(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	sha := archivedAt(t, d, 1, "Archive/s/a.JPG", 0)
	path := filepath.Join(t.TempDir(), "catalog-2026-01-01.db")
	if err := d.Backup(ctx, path); err != nil {
		t.Fatal(err)
	}
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	}()
	if v, err := b.Version(ctx); err != nil || v != SchemaVersion {
		t.Errorf("version = %d, %v", v, err)
	}
	if a, err := b.AssetBySHA(ctx, sha); err != nil || a.RelPath != "Archive/s/a.JPG" {
		t.Errorf("asset in backup = %+v, %v", a, err)
	}
	if err := d.Backup(ctx, path); err == nil {
		t.Error("second backup onto a non-empty file: want error")
	}
}
