package catalog

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// stagedAt inserts staged asset hexN(n) at rel (fingerprint hexN(100+n)).
func stagedAt(t *testing.T, d *DB, n byte, rel string) string {
	t.Helper()
	sha := hexN(n)
	a := Asset{SHA256: sha, Size: int64(n), Kind: media.KindImage, State: StateStaged, RelPath: rel, UpdatedAt: t0}
	if err := d.InsertAsset(t.Context(), a, hexN(100+n)); err != nil {
		t.Fatal(err)
	}
	return sha
}

func pathState(t *testing.T, d *DB, sha string) (string, AssetState) {
	t.Helper()
	a, err := d.AssetBySHA(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	return a.RelPath, a.State
}

func wantPath(t *testing.T, d *DB, sha, rel string, state AssetState) {
	t.Helper()
	if p, s := pathState(t, d, sha); p != rel || s != state {
		t.Errorf("asset %s = (%q, %s), want (%q, %s)", sha[:4], p, s, rel, state)
	}
}

func TestCreateStagingFolder(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	f, err := d.CreateStagingFolder(ctx, "2025-05-30 - Trip", []string{"2025-05-31", "2025-05-30", "2025-05-31"}, t0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.Dates, []string{"2025-05-30", "2025-05-31"}) || !f.CreatedAt.Equal(t0) {
		t.Errorf("folder = %+v", f)
	}
	if _, err := d.CreateStagingFolder(ctx, "2025-05-30 - Trip", nil, t0); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate: err = %v, want ErrNameTaken", err)
	}
	if _, err := d.CreateStagingFolder(ctx, "Bad", []string{"2025-5-30"}, t0); err == nil {
		t.Error("bad date: want error")
	}
	e, err := d.CreateStagingFolder(ctx, "Empty", nil, t0)
	if err != nil || !reflect.DeepEqual(e.Dates, []string{}) {
		t.Errorf("nil dates = %+v, %v; want []string{}", e, err)
	}
}

func TestRenameStagingFolderRewritesPaths(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	id := insertFolder(t, d, "2025-05-30", `["2025-05-30"]`)
	insertFolder(t, d, "2025-05-30 - Trip", `["2025-05-30"]`)
	a := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")
	b := stagedAt(t, d, 2, "Staging/2025-05-30/Capture/a.ARW")
	trap := stagedAt(t, d, 3, "Staging/2025-05-30 - Trip/b.JPG")
	culled := stagedAt(t, d, 4, "Staging/2025-05-30/c.JPG")
	if err := d.CullAsset(ctx, culled, t0); err != nil {
		t.Fatal(err)
	}

	if err := d.RenameStagingFolder(ctx, id, "2025-05-30 - Zürich", "Staging/2025-05-30/", "Staging/2025-05-30 - Zürich/", t0); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, a, "Staging/2025-05-30 - Zürich/a.JPG", StateStaged)
	wantPath(t, d, b, "Staging/2025-05-30 - Zürich/Capture/a.ARW", StateStaged)
	wantPath(t, d, trap, "Staging/2025-05-30 - Trip/b.JPG", StateStaged)
	wantPath(t, d, culled, "", StateCulled)
	if f, err := d.StagingFolderByID(ctx, id); err != nil || f.Name != "2025-05-30 - Zürich" {
		t.Errorf("folder = %+v, %v", f, err)
	}

	if err := d.RenameStagingFolder(ctx, id, "2025-05-30 - Zug", "Staging/2025-05-30 - Zürich/", "Staging/2025-05-30 - Zug/", t0); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, b, "Staging/2025-05-30 - Zug/Capture/a.ARW", StateStaged)

	err := d.RenameStagingFolder(ctx, id, "2025-05-30 - Trip", "Staging/2025-05-30 - Zug/", "Staging/2025-05-30 - Trip/", t0)
	if !errors.Is(err, ErrNameTaken) {
		t.Errorf("taken: err = %v, want ErrNameTaken", err)
	}
	wantPath(t, d, b, "Staging/2025-05-30 - Zug/Capture/a.ARW", StateStaged)
	if f, _ := d.StagingFolderByID(ctx, id); f.Name != "2025-05-30 - Zug" {
		t.Errorf("name after failed rename = %q", f.Name)
	}

	if err := d.RenameStagingFolder(ctx, id, "2025-05-30 - Zug", "Staging/x/", "Staging/y/", t0); err != nil {
		t.Errorf("same name: %v", err)
	}
	if err := d.RenameStagingFolder(ctx, 999, "N", "Staging/a/", "Staging/n/", t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
	if err := d.RenameStagingFolder(ctx, id, "2025-05-30 - Bad", "Staging/2025-05-30 - Zug", "Staging/2025-05-30 - Bad", t0); err == nil {
		t.Error("prefix without trailing slash: want error")
	}
	if f, _ := d.StagingFolderByID(ctx, id); f.Name != "2025-05-30 - Zug" {
		t.Errorf("name after bad prefix = %q", f.Name)
	}
}

func TestSetStagingFolderInfo(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	later := t0.Add(48 * time.Hour)
	id := insertFolder(t, d, "2025-05-30", `["2025-05-30"]`)
	if err := d.SetStagingFolderInfo(ctx, id, []string{"2025-06-02", "2025-06-01", "2025-06-02"}, later); err != nil {
		t.Fatal(err)
	}
	f, err := d.StagingFolderByID(ctx, id)
	if err != nil || !reflect.DeepEqual(f.Dates, []string{"2025-06-01", "2025-06-02"}) || !f.CreatedAt.Equal(later) {
		t.Fatalf("folder = %+v, %v", f, err)
	}
	if err := d.SetStagingFolderInfo(ctx, id, []string{"2025-07-01"}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	f, _ = d.StagingFolderByID(ctx, id)
	if !reflect.DeepEqual(f.Dates, []string{"2025-07-01"}) || !f.CreatedAt.Equal(later) {
		t.Errorf("zero createdAt changed it: %+v", f)
	}
	if err := d.SetStagingFolderInfo(ctx, 999, nil, time.Time{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown id: err = %v, want ErrNotFound", err)
	}
}

func TestDeleteStagingFolder(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	id := insertFolder(t, d, "2025-05-30", "[]")
	if err := d.DeleteStagingFolder(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := d.DeleteStagingFolder(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: err = %v, want ErrNotFound", err)
	}
}

func TestAssetsByHash(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	var shas []string
	for i := range 600 {
		sha := fmt.Sprintf("%064x", i+1)
		a := Asset{SHA256: sha, Size: 1, Kind: media.KindImage, State: StateStaged, RelPath: fmt.Sprintf("Staging/x/%d.JPG", i), UpdatedAt: t0}
		if err := d.InsertAsset(ctx, a, fmt.Sprintf("%064x", 10000+i)); err != nil {
			t.Fatal(err)
		}
		shas = append(shas, sha)
	}
	old := hexN(250)
	if err := d.AddAlias(ctx, old, shas[0], t0); err != nil {
		t.Fatal(err)
	}
	ask := append(append([]string{}, shas...), hexN(251), old)
	got, err := d.AssetsByHash(ctx, ask)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 600 {
		t.Fatalf("got %d assets, want 600", len(got))
	}
	if _, ok := got[old]; ok {
		t.Error("alias was followed")
	}
	if _, err := d.AssetsByHash(ctx, []string{"nothex"}); !errors.Is(err, ErrBadHash) {
		t.Errorf("bad hash: err = %v, want ErrBadHash", err)
	}
	if got, err := d.AssetsByHash(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("empty list = %v, %v", got, err)
	}
}

func wrongState(t *testing.T, err error, state string) {
	t.Helper()
	if err == nil || errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), state) {
		t.Errorf("err = %v, want a non-NotFound error mentioning %q", err, state)
	}
}

func TestAssetStateChanges(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	sha := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")

	if err := d.MoveAsset(ctx, sha, "Staging/2025-05-31/a.JPG", t0); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, sha, "Staging/2025-05-31/a.JPG", StateStaged)

	if err := d.CullAsset(ctx, sha, t0); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, sha, "", StateCulled)
	if as, err := d.AssetsByFingerprint(ctx, hexN(101)); err != nil || len(as) != 1 {
		t.Errorf("fingerprint after cull = %v, %v", as, err)
	}
	wrongState(t, d.MoveAsset(ctx, sha, "Staging/x/a.JPG", t0), "culled")
	wrongState(t, d.ArchiveAsset(ctx, sha, "Archive/x/a.JPG", t0), "culled")

	if err := d.RestageAsset(ctx, sha, "Staging/2025-06-01/a.JPG", 12345, t0); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, sha, "Staging/2025-06-01/a.JPG", StateStaged)
	if a, _ := d.AssetBySHA(ctx, sha); a.Mtime != 12345 {
		t.Errorf("mtime = %d, want 12345", a.Mtime)
	}
	wrongState(t, d.RestageAsset(ctx, sha, "Staging/x/a.JPG", 1, t0), "staged")

	if err := d.ArchiveAsset(ctx, sha, "Archive/2025/a.JPG", t0); err != nil {
		t.Fatal(err)
	}
	wantPath(t, d, sha, "Archive/2025/a.JPG", StateArchived)
	wrongState(t, d.CullAsset(ctx, sha, t0), "archived")

	unknown := hexN(9)
	for name, err := range map[string]error{
		"move":    d.MoveAsset(ctx, unknown, "Staging/x", t0),
		"cull":    d.CullAsset(ctx, unknown, t0),
		"archive": d.ArchiveAsset(ctx, unknown, "Archive/x", t0),
		"restage": d.RestageAsset(ctx, unknown, "Staging/x", 1, t0),
	} {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s unknown: err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestArchiveFolder(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	id := insertFolder(t, d, "2025-05-30", `["2025-05-30"]`)
	insertFolder(t, d, "2025-05-30 - Trip", `["2025-05-30"]`)
	a := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")
	b := stagedAt(t, d, 2, "Staging/2025-05-30/Capture/a.ARW")
	trap := stagedAt(t, d, 3, "Staging/2025-05-30 - Trip/b.JPG")

	if n, err := d.ArchiveFolder(ctx, 999, "Staging/2025-05-30/", "Archive/2025-05-30/", t0); !errors.Is(err, ErrNotFound) || n != 0 {
		t.Errorf("unknown id = %d, %v; want ErrNotFound", n, err)
	}
	wantPath(t, d, a, "Staging/2025-05-30/a.JPG", StateStaged)

	n, err := d.ArchiveFolder(ctx, id, "Staging/2025-05-30/", "Archive/2025-05-30/", t0)
	if err != nil || n != 2 {
		t.Fatalf("ArchiveFolder = %d, %v; want 2", n, err)
	}
	wantPath(t, d, a, "Archive/2025-05-30/a.JPG", StateArchived)
	wantPath(t, d, b, "Archive/2025-05-30/Capture/a.ARW", StateArchived)
	wantPath(t, d, trap, "Staging/2025-05-30 - Trip/b.JPG", StateStaged)
	if _, err := d.StagingFolderByID(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("row still there: %v", err)
	}
}

func TestChangeAssetWrongState(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	sha := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")
	if err := d.ArchiveAsset(ctx, sha, "Archive/2025/a.JPG", t0); err != nil {
		t.Fatal(err)
	}
	err := d.CullAsset(ctx, sha, t0)
	if !errors.Is(err, ErrWrongState) {
		t.Fatalf("err = %v, want ErrWrongState", err)
	}
	var ws *WrongStateError
	if !errors.As(err, &ws) || ws.State != StateArchived {
		t.Fatalf("errors.As = %v, %+v; want state archived", errors.As(err, &ws), ws)
	}
	if want := "cull " + sha + ": the asset is archived"; err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
	if err := d.CullAsset(ctx, hexN(9), t0); !errors.Is(err, ErrNotFound) || errors.Is(err, ErrWrongState) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound only", err)
	}
}

func TestArchiveFolderSetsPromotedAt(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	id := insertFolder(t, d, "2025-05-30", `["2025-05-30"]`)
	a := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")
	b := stagedAt(t, d, 2, "Staging/2025-05-30/Capture/a.ARW")
	if _, err := d.ArchiveFolder(ctx, id, "Staging/2025-05-30/", "Archive/2025-05-30/", t2); err != nil {
		t.Fatal(err)
	}
	for _, sha := range []string{a, b} {
		if _, p := rawCols(t, d, sha); !p.Equal(t2) {
			t.Errorf("promoted_at of %s = %v, want %v", sha[:4], p, t2)
		}
	}
}

func TestArchiveAssetSetsPromotedAt(t *testing.T) {
	d := openTest(t)
	sha := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")
	if err := d.ArchiveAsset(t.Context(), sha, "Archive/2025/a.JPG", t2); err != nil {
		t.Fatal(err)
	}
	if _, p := rawCols(t, d, sha); !p.Equal(t2) {
		t.Errorf("promoted_at = %v, want %v", p, t2)
	}
}

func TestRenameStagingFolderLeavesPromotedAt(t *testing.T) {
	d := openTest(t)
	id := insertFolder(t, d, "2025-05-30", `["2025-05-30"]`)
	sha := stagedAt(t, d, 1, "Staging/2025-05-30/a.JPG")
	if err := d.RenameStagingFolder(t.Context(), id, "New", "Staging/2025-05-30/", "Staging/New/", t2); err != nil {
		t.Fatal(err)
	}
	if _, p := rawCols(t, d, sha); !p.IsZero() {
		t.Errorf("promoted_at = %v, want NULL", p)
	}
}

func TestShiftAsset(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	capture := time.Date(2025, 5, 30, 14, 2, 11, 0, time.Local)
	a := Asset{SHA256: hexN(1), Size: 1, Kind: media.KindImage, CaptureTime: capture, State: StateStaged,
		RelPath: "Staging/2025-05-30/2025-05-30 14.02.11.JPG", UpdatedAt: t0}
	if err := d.InsertAsset(ctx, a, hexN(101)); err != nil {
		t.Fatal(err)
	}
	other := stagedAt(t, d, 2, "Staging/2025-05-30/b.JPG")
	in, err := d.CreateIngest(ctx, "/cards/X", "X", "", t0)
	if err != nil {
		t.Fatal(err)
	}
	err = d.AddClockWarnings(ctx, in.ID, []ClockWarning{
		{CardPath: "DCIM/a.JPG", SHA256: a.SHA256, Kind: ClockFuture, Detail: "in the future"},
		{CardPath: "DCIM/a.JPG", SHA256: a.SHA256, Kind: ClockGPSMismatch, Detail: "gps"},
		{CardPath: "DCIM/b.JPG", SHA256: other, Kind: ClockFuture, Detail: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}

	shifted := capture.Add(time.Hour)
	if err := d.ShiftAsset(ctx, a.SHA256, "Staging/2025-05-30/2025-05-30 15.02.11.JPG", shifted, t2); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetBySHA(ctx, a.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if got.CaptureTime.Format(CaptureLayout) != "2025-05-30T15:02:11" || got.RelPath != "Staging/2025-05-30/2025-05-30 15.02.11.JPG" ||
		!got.UpdatedAt.Equal(t2) || got.State != StateStaged {
		t.Errorf("asset = %+v", got)
	}
	ws, err := d.ClockWarnings(ctx, in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 1 || ws[0].SHA256 != other {
		t.Errorf("clock warnings = %+v, want only the other file's", ws)
	}

	if err := d.ArchiveAsset(ctx, other, "Archive/b.JPG", t0); err != nil {
		t.Fatal(err)
	}
	err = d.ShiftAsset(ctx, other, "Staging/2025-05-30/c.JPG", shifted, t2)
	if !errors.Is(err, ErrWrongState) {
		t.Errorf("archived: err = %v, want ErrWrongState", err)
	}
	wantPath(t, d, other, "Archive/b.JPG", StateArchived)
	if ws, _ := d.ClockWarnings(ctx, in.ID); len(ws) != 1 {
		t.Errorf("a refused shift deleted clock warnings: %+v", ws)
	}
	if err := d.ShiftAsset(ctx, hexN(9), "Staging/x.JPG", shifted, t2); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown hash: err = %v, want ErrNotFound", err)
	}
}
