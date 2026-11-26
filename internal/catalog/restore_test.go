package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

func TestMigrateV4ToV5(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{schemaV1, schemaV2, schemaV3, schemaV4, "PRAGMA user_version = 4",
		`INSERT INTO assets (sha256, size, kind, state, rel_path, updated_at)
			VALUES (X'` + hexN(1) + `', 1, 'image', 'missing', 'Archive/a.JPG', '2026-02-02T10:00:00.000Z')`} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	ctx := t.Context()
	if v, err := d.Version(ctx); err != nil || v != SchemaVersion || SchemaVersion != 5 {
		t.Fatalf("version = %d, %v; want 5", v, err)
	}
	if a, err := d.AssetBySHA(ctx, hexN(1)); err != nil || a.State != StateMissing {
		t.Fatalf("asset = %+v, %v; want it kept, missing", a, err)
	}
	var n int
	if err := d.db.QueryRowContext(ctx, "SELECT count(*) FROM restore_requests").Scan(&n); err != nil || n != 0 {
		t.Fatalf("restore_requests: count %d, %v; want an empty table", n, err)
	}
	if _, err := d.db.ExecContext(ctx, "INSERT INTO restore_requests (sha256, requested_at) VALUES (X'00', '2026-02-02T10:00:00.000Z')"); err == nil {
		t.Error("restore_requests accepted a hash that is not 32 bytes")
	}
}

// missingFixture records an import whose files A, B and C (hexN(1..3), each
// with fingerprint hexN(100+n)) matched assets missing from the archive;
// since then A and B are still missing (updated_at t0) and C is archived
// again. A has two card files.
func missingFixture(t *testing.T, d *DB) (in Ingest, a, b, c string) {
	t.Helper()
	in = newIngest(t, d)
	var files []IngestFile
	for n, state := range map[byte]AssetState{1: StateMissing, 2: StateMissing, 3: StateArchived} {
		sha := hexN(n)
		as := Asset{SHA256: sha, Size: int64(n), Kind: media.KindImage, State: state,
			RelPath: fmt.Sprintf("Archive/%d.JPG", n), UpdatedAt: t0}
		if err := d.InsertAsset(t.Context(), as, hexN(100+n)); err != nil {
			t.Fatal(err)
		}
		files = append(files, IngestFile{CardPath: fmt.Sprintf("DCIM/%d.JPG", n), Size: int64(n),
			QuickFP: hexN(100 + n), SHA256: sha, Outcome: OutcomeMissing})
	}
	files = append(files, IngestFile{CardPath: "DCIM/copy-of-1.JPG", Size: 1, QuickFP: hexN(101), SHA256: hexN(1), Outcome: OutcomeMissing})
	if err := d.AddIngestFiles(t.Context(), in.ID, files); err != nil {
		t.Fatal(err)
	}
	return in, hexN(1), hexN(2), hexN(3)
}

// putRequest writes a restore request for sha directly.
func putRequest(t *testing.T, d *DB, sha string, at time.Time) {
	t.Helper()
	_, err := d.db.ExecContext(t.Context(), "INSERT OR REPLACE INTO restore_requests (sha256, requested_at) VALUES (?, ?)",
		mustBlob(t, sha), formatTime(at))
	if err != nil {
		t.Fatal(err)
	}
}

// requests returns every restore request: hash -> requested_at.
func requests(t *testing.T, d *DB) map[string]string {
	t.Helper()
	rows, err := d.db.QueryContext(t.Context(), "SELECT sha256, requested_at FROM restore_requests")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var (
			b  []byte
			at string
		)
		if err := rows.Scan(&b, &at); err != nil {
			t.Fatal(err)
		}
		out[blobToHex(b)] = at
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// restoreRefused fails unless err is ErrWrongState naming state.
func restoreRefused(t *testing.T, err error, state string) {
	t.Helper()
	wrongState(t, err, state)
	if !errors.Is(err, ErrWrongState) {
		t.Errorf("err = %v, want ErrWrongState", err)
	}
}

func TestRequestRestore(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in, a, b, c := missingFixture(t, d)
	putRequest(t, d, c, t0) // stale: C is archived again
	other := hexN(9)        // another import's missing file, requested earlier
	if err := d.InsertAsset(ctx, Asset{SHA256: other, Size: 9, Kind: media.KindImage, State: StateMissing, RelPath: "Archive/9.JPG", UpdatedAt: t0}, ""); err != nil {
		t.Fatal(err)
	}
	putRequest(t, d, other, t1)

	n, err := d.RequestRestore(ctx, in.ID, t1)
	if err != nil || n != 2 {
		t.Fatalf("RequestRestore = %d, %v; want 2", n, err)
	}
	want := map[string]string{a: formatTime(t1), b: formatTime(t1), other: formatTime(t1)}
	if got := requests(t, d); !reflect.DeepEqual(got, want) {
		t.Fatalf("requests = %v, want %v", got, want)
	}
	n, err = d.RequestRestore(ctx, in.ID, t2)
	if err != nil || n != 2 {
		t.Fatalf("again: RequestRestore = %d, %v; want 2", n, err)
	}
	if got := requests(t, d); len(got) != 3 {
		t.Errorf("after asking again, requests = %v, want the same 3", got)
	}
	if n, err := d.RequestRestore(ctx, 999, t2); err != nil || n != 0 {
		t.Errorf("unknown import: RequestRestore = %d, %v; want 0", n, err)
	}
}

// A request made before the asset went missing again no longer counts:
// asking again renews it.
func TestRequestRestoreRenewsStale(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in, a, _, _ := missingFixture(t, d)
	putRequest(t, d, a, t0.Add(-time.Hour))
	n, err := d.RequestRestore(ctx, in.ID, t1)
	if err != nil || n != 2 {
		t.Fatalf("RequestRestore = %d, %v; want 2", n, err)
	}
	if got := requests(t, d)[a]; got != formatTime(t1) {
		t.Errorf("A requested_at = %q, want %q", got, formatTime(t1))
	}
}

func TestRestoreMissing(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	first := newIngest(t, d)
	sha := hexN(1)
	if err := d.InsertAsset(ctx, Asset{SHA256: sha, Size: 598, Kind: media.KindImage, State: StateMissing,
		RelPath: "Archive/a.JPG", Mtime: 7, FirstIngest: first.ID, UpdatedAt: t0}, hexN(101)); err != nil {
		t.Fatal(err)
	}
	setCols(t, d, sha, t0, t0)
	putRequest(t, d, sha, t1)
	in := newIngest(t, d)
	staged := Asset{SHA256: sha, Size: 598, Kind: media.KindImage, CaptureTime: capture, TZOffset: "+02:00",
		State: StateStaged, RelPath: "Staging/2025-05-30/2025-05-31 01.30.12.JPG", Mtime: 42, UpdatedAt: t2}
	fp := hexN(200)
	row := IngestFile{CardPath: "DCIM/DSC00001.JPG", Size: 598, Outcome: OutcomeNew}

	// One transaction: a fingerprint that cannot be written (after the
	// asset update) leaves the asset and its request alone.
	if err := d.RestoreMissing(ctx, in.ID, staged, "not hex", row); err == nil {
		t.Fatal("RestoreMissing accepted an invalid fingerprint")
	}
	if a, err := d.AssetBySHA(ctx, sha); err != nil || a.State != StateMissing || a.RelPath != "Archive/a.JPG" {
		t.Fatalf("after a failed restore, asset = %+v, %v; want it untouched", a, err)
	}
	if got := requests(t, d); len(got) != 1 {
		t.Fatalf("after a failed restore, requests = %v; want it kept", got)
	}

	if err := d.RestoreMissing(ctx, in.ID, staged, fp, row); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetBySHA(ctx, sha)
	if err != nil {
		t.Fatal(err)
	}
	want := staged
	want.FirstIngest = first.ID
	checkAsset(t, got, want)
	if checked, _ := rawCols(t, d, sha); !checked.IsZero() {
		t.Errorf("checked_at = %v, want NULL", checked)
	}
	if r := requests(t, d); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
	if as, err := d.AssetsByFingerprint(ctx, fp); err != nil || len(as) != 1 || as[0].SHA256 != sha {
		t.Errorf("assets by the card fingerprint = %+v, %v", as, err)
	}
	files, err := d.IngestFiles(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantFiles := []IngestFile{{CardPath: row.CardPath, Size: 598, QuickFP: fp, SHA256: sha, Outcome: OutcomeNew, RelPath: staged.RelPath}}
	if !reflect.DeepEqual(files, wantFiles) {
		t.Errorf("ingest files = %+v, want %+v", files, wantFiles)
	}

	again := staged
	again.RelPath = "Staging/2025-05-30/other.JPG"
	restoreRefused(t, d.RestoreMissing(ctx, in.ID, again, fp, row), "staged")
	if files, err := d.IngestFiles(ctx, in.ID); err != nil || len(files) != 1 {
		t.Errorf("after the refused restore, ingest files = %+v, %v; want 1", files, err)
	}
	if a, err := d.AssetBySHA(ctx, sha); err != nil || a.RelPath != staged.RelPath {
		t.Errorf("after the refused restore, asset = %+v, %v", a, err)
	}

	for n, state := range map[byte]AssetState{2: StateCulled, 3: StateArchived} {
		s := hexN(n)
		if err := d.InsertAsset(ctx, Asset{SHA256: s, Size: 1, Kind: media.KindImage, State: state, UpdatedAt: t0}, ""); err != nil {
			t.Fatal(err)
		}
		a := staged
		a.SHA256 = s
		restoreRefused(t, d.RestoreMissing(ctx, in.ID, a, fp, row), string(state))
	}
	unknown := staged
	unknown.SHA256 = hexN(4)
	if err := d.RestoreMissing(ctx, in.ID, unknown, fp, row); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown asset: err = %v, want ErrNotFound", err)
	}
	if files, err := d.IngestFiles(ctx, in.ID); err != nil || len(files) != 1 {
		t.Errorf("after the refused restores, ingest files = %+v, %v; want 1", files, err)
	}
}

func TestRestoreRequestedFresh(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in, a, b, _ := missingFixture(t, d)
	if n, err := d.RequestRestore(ctx, in.ID, t1); err != nil || n != 2 {
		t.Fatalf("RequestRestore = %d, %v; want 2", n, err)
	}
	got, err := d.RestoreRequested(ctx)
	if err != nil || !reflect.DeepEqual(got, map[string]bool{a: true, b: true}) {
		t.Fatalf("RestoreRequested = %v, %v; want A and B", got, err)
	}
	if err := d.SetArchivePath(ctx, a, "Archive/back.JPG", 1, t2); err != nil { // A reappears ...
		t.Fatal(err)
	}
	if err := d.MarkMissing(ctx, a, t2.Add(time.Minute)); err != nil { // ... and goes missing again
		t.Fatal(err)
	}
	got, err = d.RestoreRequested(ctx)
	if err != nil || !reflect.DeepEqual(got, map[string]bool{b: true}) {
		t.Fatalf("RestoreRequested = %v, %v; want only B", got, err)
	}
	if r := requests(t, d); len(r) != 1 || r[b] == "" {
		t.Errorf("requests = %v, want only B's", r)
	}
	if err := d.SetArchivePath(ctx, b, "Archive/back2.JPG", 1, t2); err != nil {
		t.Fatal(err)
	}
	if got, err := d.RestoreRequested(ctx); err != nil || len(got) != 0 {
		t.Errorf("RestoreRequested = %v, %v; want none", got, err)
	}
	if r := requests(t, d); len(r) != 0 {
		t.Errorf("requests = %v, want none", r)
	}
}

func TestRestorableMissing(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	in, a, b, c := missingFixture(t, d)
	putRequest(t, d, a, t1)
	putRequest(t, d, b, t0.Add(-time.Hour)) // stale: B went missing after it
	putRequest(t, d, c, t1)                 // C is archived again
	restorable, requested, err := d.RestorableMissing(ctx, in.ID)
	if err != nil || restorable != 2 || requested != 1 {
		t.Errorf("RestorableMissing = %d, %d, %v; want 2, 1", restorable, requested, err)
	}
	if r, q, err := d.RestorableMissing(ctx, 999); err != nil || r != 0 || q != 0 {
		t.Errorf("unknown import: RestorableMissing = %d, %d, %v; want 0, 0", r, q, err)
	}
}
