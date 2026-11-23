package catalog

import (
	"errors"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

var (
	t0      = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	capture = time.Date(2025, 5, 31, 1, 30, 12, 0, time.Local)
)

func sampleAsset() Asset {
	return Asset{
		SHA256: hexN(1), Size: 598, Kind: media.KindImage, CaptureTime: capture, TZOffset: "+02:00",
		State: StateStaged, RelPath: "Staging/2025-05-30/2025-05-31 01.30.12.JPG",
		Mtime: 1748647812000000000, UpdatedAt: t0,
	}
}

func assetCount(t *testing.T, d *DB) int {
	t.Helper()
	var n int
	if err := d.db.QueryRowContext(t.Context(), "SELECT count(*) FROM assets").Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func checkAsset(t *testing.T, got, want Asset) {
	t.Helper()
	if got.SHA256 != want.SHA256 || got.Size != want.Size || got.Kind != want.Kind ||
		got.TZOffset != want.TZOffset || got.State != want.State || got.RelPath != want.RelPath ||
		got.Mtime != want.Mtime || got.FirstIngest != want.FirstIngest {
		t.Errorf("asset = %+v, want %+v", got, want)
	}
	if got.CaptureTime.Format(CaptureLayout) != want.CaptureTime.Format(CaptureLayout) {
		t.Errorf("capture = %v, want %v", got.CaptureTime, want.CaptureTime)
	}
	if want.CaptureTime.IsZero() != got.CaptureTime.IsZero() {
		t.Errorf("capture zero-ness differs: got %v want %v", got.CaptureTime, want.CaptureTime)
	}
	if !got.UpdatedAt.Equal(want.UpdatedAt) {
		t.Errorf("updated = %v, want %v", got.UpdatedAt, want.UpdatedAt)
	}
}

func TestInsertAndLookupAsset(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	want := sampleAsset()
	if err := d.InsertAsset(ctx, want, hexN(2)); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetsByFingerprint(ctx, hexN(2))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d assets, want 1", len(got))
	}
	checkAsset(t, got[0], want)
	if got[0].CaptureTime.Format(CaptureLayout) != "2025-05-31T01:30:12" {
		t.Errorf("capture = %s", got[0].CaptureTime.Format(CaptureLayout))
	}
	bySHA, err := d.AssetBySHA(ctx, hexN(1))
	if err != nil {
		t.Fatal(err)
	}
	checkAsset(t, bySHA, want)

	none, err := d.AssetsByFingerprint(ctx, hexN(9))
	if err != nil || none != nil {
		t.Errorf("unknown fingerprint = %v, %v; want nil, nil", none, err)
	}
	if _, err := d.AssetBySHA(ctx, hexN(9)); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown sha err = %v, want ErrNotFound", err)
	}
}

func TestInsertAssetNullables(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	want := Asset{SHA256: hexN(1), Size: 5, Kind: media.KindRaw, State: StateCulled, UpdatedAt: t0}
	if err := d.InsertAsset(ctx, want, ""); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetBySHA(ctx, hexN(1))
	if err != nil {
		t.Fatal(err)
	}
	checkAsset(t, got, want)
	var n int
	err = d.db.QueryRowContext(ctx, `SELECT count(*) FROM assets WHERE rel_path IS NULL AND mtime IS NULL
		AND first_ingest IS NULL AND capture_time IS NULL`).Scan(&n)
	if err != nil || n != 1 {
		t.Errorf("null row count = %d, err %v; want 1", n, err)
	}
}

func TestInsertAssetRejects(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	if err := d.InsertAsset(ctx, sampleAsset(), ""); err != nil {
		t.Fatal(err)
	}
	mod := func(f func(*Asset)) Asset {
		a := sampleAsset()
		a.SHA256 = hexN(5)
		f(&a)
		return a
	}
	cases := map[string]Asset{
		"duplicate": sampleAsset(),
		"bad hash":  mod(func(a *Asset) { a.SHA256 = "abc" }),
		"bad kind":  mod(func(a *Asset) { a.Kind = "photo" }),
		"bad state": mod(func(a *Asset) { a.State = "deleted" }),
		"zero time": mod(func(a *Asset) { a.UpdatedAt = time.Time{} }),
	}
	for name, a := range cases {
		err := d.InsertAsset(ctx, a, hexN(6))
		if err == nil {
			t.Errorf("%s: want error", name)
		}
		if name == "bad hash" && !errors.Is(err, ErrBadHash) {
			t.Errorf("bad hash err = %v, want ErrBadHash", err)
		}
	}
	if n := assetCount(t, d); n != 1 {
		t.Errorf("assets = %d, want 1", n)
	}
}

func TestFingerprintManyToOne(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	a := sampleAsset()
	if err := d.InsertAsset(ctx, a, hexN(10)); err != nil {
		t.Fatal(err)
	}
	if err := d.AddFingerprint(ctx, hexN(11), a.SHA256); err != nil {
		t.Fatal(err)
	}
	for _, fp := range []string{hexN(10), hexN(11)} {
		got, err := d.AssetsByFingerprint(ctx, fp)
		if err != nil || len(got) != 1 || got[0].SHA256 != a.SHA256 {
			t.Errorf("fp %s = %v, %v", fp[:4], got, err)
		}
	}
	b := sampleAsset()
	b.SHA256 = hexN(3)
	b.RelPath = "Staging/x.JPG"
	if err := d.InsertAsset(ctx, b, hexN(10)); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetsByFingerprint(ctx, hexN(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].SHA256 != hexN(1) || got[1].SHA256 != hexN(3) {
		t.Errorf("collision = %+v", got)
	}
}

func TestAddFingerprintIdempotent(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	if err := d.InsertAsset(ctx, sampleAsset(), ""); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := d.AddFingerprint(ctx, hexN(2), hexN(1)); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddFingerprint(ctx, hexN(2), hexN(8)); err == nil {
		t.Error("unknown sha: want foreign key error")
	}
}

func TestAssetBySHAFollowsAlias(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	want := sampleAsset()
	if err := d.InsertAsset(ctx, want, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAlias(ctx, hexN(7), hexN(1), t0); err != nil {
		t.Fatal(err)
	}
	got, err := d.AssetBySHA(ctx, hexN(7))
	if err != nil {
		t.Fatal(err)
	}
	checkAsset(t, got, want)
	if err := d.AddAlias(ctx, hexN(7), hexN(9), t0); err == nil {
		t.Error("alias to unknown sha: want error")
	}
	if err := d.AddAlias(ctx, hexN(4), hexN(9), t0); err == nil {
		t.Error("alias to unknown sha: want error")
	}
}
