package catalog

import (
	"errors"
	"reflect"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// forgetFixture is ingest 1 (staged A, B new; culled C seen before; archived
// D in the archive; E staged by ingest 2 and found in staging; a failed file),
// a clock warning for A and an alias whose target is B.
type forgetFixture struct {
	d              *DB
	a, b, c, dd, e string // dd is the archived asset D
	aliasOld       string
}

func newForgetFixture(t *testing.T) forgetFixture {
	t.Helper()
	d := openTest(t)
	ctx := t.Context()
	in1 := newIngest(t, d)
	in2 := newIngest(t, d)
	f := forgetFixture{d: d, a: hexN(1), b: hexN(2), c: hexN(3), dd: hexN(4), e: hexN(5), aliasOld: hexN(50)}
	rec := func(ingest int64, sha string, n byte, rel string, path string) {
		t.Helper()
		a := Asset{SHA256: sha, Size: int64(n), Kind: media.KindImage, State: StateStaged, RelPath: rel, UpdatedAt: t0}
		err := d.RecordNew(ctx, ingest, a, hexN(100+n), IngestFile{CardPath: path, Size: int64(n), Outcome: OutcomeNew})
		if err != nil {
			t.Fatal(err)
		}
	}
	rec(in1.ID, f.a, 1, "Staging/x/a.jpg", "DCIM/a.jpg")
	rec(in1.ID, f.b, 2, "Staging/x/b.jpg", "DCIM/b.jpg")
	rec(in2.ID, f.e, 5, "Staging/x/e.jpg", "DCIM/e.jpg")
	for _, x := range []struct {
		sha   string
		n     byte
		state AssetState
		rel   string
	}{{f.c, 3, StateCulled, ""}, {f.dd, 4, StateArchived, "Archive/d.jpg"}} {
		a := Asset{SHA256: x.sha, Size: int64(x.n), Kind: media.KindImage, State: x.state, RelPath: x.rel, UpdatedAt: t0}
		if err := d.InsertAsset(ctx, a, hexN(100+x.n)); err != nil {
			t.Fatal(err)
		}
	}
	err := d.AddIngestFiles(ctx, in1.ID, []IngestFile{
		{CardPath: "DCIM/c.jpg", Size: 3, QuickFP: hexN(103), SHA256: f.c, Outcome: OutcomeSeenBefore},
		{CardPath: "DCIM/d.jpg", Size: 4, QuickFP: hexN(104), SHA256: f.dd, Outcome: OutcomeInArchive},
		{CardPath: "DCIM/e.jpg", Size: 5, QuickFP: hexN(105), SHA256: f.e, Outcome: OutcomeInStaging},
		{CardPath: "DCIM/bad.jpg", Size: 6, Outcome: OutcomeFailed, Error: "unreadable"},
	})
	if err != nil {
		t.Fatal(err)
	}
	ws := []ClockWarning{{CardPath: "DCIM/a.jpg", SHA256: f.a, Kind: ClockFuture, Detail: "x"}}
	if err := d.AddClockWarnings(ctx, in1.ID, ws); err != nil {
		t.Fatal(err)
	}
	if err := d.AddAlias(ctx, f.aliasOld, f.b, t0); err != nil {
		t.Fatal(err)
	}
	return f
}

func shasOf(as []Asset) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.SHA256)
	}
	return out
}

func TestIngestAssets(t *testing.T) {
	f := newForgetFixture(t)
	ctx := t.Context()
	got, err := f.d.IngestAssets(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{f.a, f.b, f.c, f.dd, f.e}; !reflect.DeepEqual(shasOf(got), want) {
		t.Errorf("ingest 1 assets = %v, want %v", shasOf(got), want)
	}
	got, err = f.d.IngestAssets(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{f.e}; !reflect.DeepEqual(shasOf(got), want) {
		t.Errorf("ingest 2 assets = %v, want %v", shasOf(got), want)
	}
	got, err = f.d.IngestAssets(ctx, 99)
	if err != nil || len(got) != 0 {
		t.Errorf("ingest 99 assets = %v, %v; want none", shasOf(got), err)
	}
}

func TestForgetAssets(t *testing.T) {
	f := newForgetFixture(t)
	ctx := t.Context()
	unknown := hexN(99)
	files, err := f.d.IngestFiles(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.d.ForgetAssets(ctx, []string{f.a, f.b, f.c, f.dd, unknown}, []AssetState{StateStaged, StateCulled})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{f.a, f.b, f.c}; !reflect.DeepEqual(got, want) {
		t.Fatalf("forgot %v, want %v", got, want)
	}
	for i, sha := range []string{f.a, f.b, f.c} {
		if _, err := f.d.AssetBySHA(ctx, sha); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", sha[:4], err)
		}
		fp, err := f.d.AssetsByFingerprint(ctx, hexN(byte(101+i)))
		if err != nil || len(fp) != 0 {
			t.Errorf("fingerprint of %s: %v, %v; want none", sha[:4], fp, err)
		}
	}
	if _, err := f.d.AssetBySHA(ctx, f.aliasOld); !errors.Is(err, ErrNotFound) {
		t.Errorf("alias to B: err = %v, want ErrNotFound", err)
	}
	if ws, err := f.d.ClockWarnings(ctx, 1); err != nil || len(ws) != 0 {
		t.Errorf("clock warnings = %v, %v; want none", ws, err)
	}
	var aliases int
	if err := f.d.db.QueryRowContext(ctx, "SELECT count(*) FROM aliases").Scan(&aliases); err != nil || aliases != 0 {
		t.Errorf("alias rows = %d, %v; want 0", aliases, err)
	}
	wantPath(t, f.d, f.dd, "Archive/d.jpg", StateArchived)
	wantPath(t, f.d, f.e, "Staging/x/e.jpg", StateStaged)
	after, err := f.d.IngestFiles(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(files) {
		t.Errorf("ingest_files rows = %d, want %d", len(after), len(files))
	}
}

func TestForgetAssetsBadHashChangesNothing(t *testing.T) {
	f := newForgetFixture(t)
	ctx := t.Context()
	got, err := f.d.ForgetAssets(ctx, []string{f.a, "nothex"}, []AssetState{StateStaged})
	if !errors.Is(err, ErrBadHash) || got != nil {
		t.Fatalf("got %v, %v; want ErrBadHash", got, err)
	}
	wantPath(t, f.d, f.a, "Staging/x/a.jpg", StateStaged)
	if ws, err := f.d.ClockWarnings(ctx, 1); err != nil || len(ws) != 1 {
		t.Errorf("clock warnings = %v, %v; want 1", ws, err)
	}
}

func TestForgetAssetsStates(t *testing.T) {
	f := newForgetFixture(t)
	ctx := t.Context()
	got, err := f.d.ForgetAssets(ctx, []string{f.a, f.b, f.c}, []AssetState{StateCulled})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{f.c}; !reflect.DeepEqual(got, want) {
		t.Errorf("forgot %v, want %v", got, want)
	}
	wantPath(t, f.d, f.a, "Staging/x/a.jpg", StateStaged)
	wantPath(t, f.d, f.b, "Staging/x/b.jpg", StateStaged)
	for _, st := range []AssetState{StateArchived, StateMissing} {
		got, err := f.d.ForgetAssets(ctx, []string{f.a, f.dd}, []AssetState{StateStaged, st})
		if err == nil || got != nil {
			t.Errorf("states with %s: got %v, %v; want an error", st, got, err)
		}
	}
	wantPath(t, f.d, f.a, "Staging/x/a.jpg", StateStaged)
	wantPath(t, f.d, f.dd, "Archive/d.jpg", StateArchived)
}

func TestCulledSeenBefore(t *testing.T) {
	f := newForgetFixture(t)
	ctx := t.Context()
	got, err := f.d.CulledSeenBefore(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{f.c}; !reflect.DeepEqual(got, want) {
		t.Errorf("culled seen before = %v, want %v", got, want)
	}
	if err := f.d.RestageAsset(ctx, f.c, "Staging/x/c.jpg", 1, t1); err != nil {
		t.Fatal(err)
	}
	got, err = f.d.CulledSeenBefore(ctx, 1)
	if err != nil || len(got) != 0 {
		t.Errorf("after restage = %v, %v; want none", got, err)
	}
}
