package catalog

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/media"
)

func insertFolder(t *testing.T, d *DB, name, dates string) int64 {
	t.Helper()
	res, err := d.db.ExecContext(t.Context(),
		"INSERT INTO staging_folders (name, dates, created_at) VALUES (?,?,?)", name, dates, formatTime(t0))
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestStagingFolderForDateCreates(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	f, err := d.StagingFolderForDate(ctx, "2025-05-30", t0)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID <= 0 || f.Name != "2025-05-30" || !reflect.DeepEqual(f.Dates, []string{"2025-05-30"}) || !f.CreatedAt.Equal(t0) {
		t.Errorf("folder = %+v", f)
	}
	again, err := d.StagingFolderForDate(ctx, "2025-05-30", t0)
	if err != nil || again.ID != f.ID {
		t.Errorf("second call = %+v, %v; want id %d", again, err, f.ID)
	}
	all, err := d.StagingFolders(ctx)
	if err != nil || len(all) != 1 {
		t.Errorf("folders = %+v, %v; want 1", all, err)
	}
}

func TestStagingFolderForDateUsesCoveringFolder(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	id := insertFolder(t, d, "2025-05-29 - Trip", `["2025-05-29","2025-05-30"]`)
	f, err := d.StagingFolderForDate(ctx, "2025-05-30", t0)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID != id || f.Name != "2025-05-29 - Trip" {
		t.Errorf("folder = %+v", f)
	}
	if all, _ := d.StagingFolders(ctx); len(all) != 1 {
		t.Errorf("folders = %d, want 1", len(all))
	}
}

func TestStagingFolderForDateLowestID(t *testing.T) {
	d := openTest(t)
	first := insertFolder(t, d, "b", `["2025-05-30"]`)
	insertFolder(t, d, "a", `["2025-05-30"]`)
	f, err := d.StagingFolderForDate(t.Context(), "2025-05-30", t0)
	if err != nil || f.ID != first {
		t.Errorf("folder = %+v, %v; want id %d", f, err, first)
	}
}

func TestStagingFolderForDateAdoptsSameName(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	id := insertFolder(t, d, "2025-05-31", `["2025-05-30"]`)
	f, err := d.StagingFolderForDate(ctx, "2025-05-31", t0)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"2025-05-30", "2025-05-31"}
	if f.ID != id || !reflect.DeepEqual(f.Dates, want) {
		t.Errorf("folder = %+v", f)
	}
	all, err := d.StagingFolders(ctx)
	if err != nil || len(all) != 1 || !reflect.DeepEqual(all[0].Dates, want) {
		t.Errorf("persisted = %+v, %v", all, err)
	}
}

func TestStagingFolderForDateRejectsBadDate(t *testing.T) {
	d := openTest(t)
	for _, date := range []string{"2025-5-30", "../x", ""} {
		if _, err := d.StagingFolderForDate(t.Context(), date, t0); err == nil {
			t.Errorf("date %q: want error", date)
		}
	}
}

func TestStagingFolders(t *testing.T) {
	d := openTest(t)
	insertFolder(t, d, "b", `["2025-01-02"]`)
	insertFolder(t, d, "a", `["2025-01-01","2025-01-03"]`)
	all, err := d.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "a" || all[1].Name != "b" ||
		!reflect.DeepEqual(all[0].Dates, []string{"2025-01-01", "2025-01-03"}) {
		t.Errorf("folders = %+v", all)
	}
}

func seedUnder(t *testing.T, d *DB) {
	t.Helper()
	rows := []struct {
		rel   string
		kind  media.Kind
		state AssetState
	}{
		{"Staging/2025-05-30/2025-05-30 10.00.00.JPG", media.KindImage, StateStaged},
		{"Staging/2025-05-30/Capture/2025-05-30 10.00.00.ARW", media.KindRaw, StateStaged},
		{"Staging/2025-05-30 - Trip/a.JPG", media.KindImage, StateStaged},
		{"Staging/50%_off/b.JPG", media.KindImage, StateStaged},
		{"Archive/2025-05-30 - Old/c.JPG", media.KindImage, StateArchived},
		{"Staging/2025-05-30/d.JPG", media.KindImage, StateArchived},
	}
	for i, r := range rows {
		err := d.InsertAsset(t.Context(), Asset{SHA256: hexN(byte(i + 1)), Size: 10, Kind: r.kind, State: r.state,
			RelPath: r.rel, UpdatedAt: time.Now()}, "")
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestAssetsUnderPrefixTrap(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	seedUnder(t, d)
	shas := func(prefix string) []string {
		t.Helper()
		as, err := d.AssetsUnder(ctx, prefix, StateStaged)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, a := range as {
			out = append(out, a.SHA256)
		}
		return out
	}
	for _, c := range []struct {
		prefix string
		want   []string
	}{
		{"Staging/2025-05-30/", []string{hexN(1), hexN(2)}},
		{"Staging/50%_off/", []string{hexN(4)}},
		{"Staging/", []string{hexN(3), hexN(1), hexN(2), hexN(4)}},
	} {
		if got := shas(c.prefix); !reflect.DeepEqual(got, c.want) {
			t.Errorf("AssetsUnder(%q) = %v, want %v", c.prefix, got, c.want)
		}
	}
	for _, bad := range []string{"Staging", "/"} {
		if _, err := d.AssetsUnder(ctx, bad, StateStaged); err == nil {
			t.Errorf("AssetsUnder(%q) succeeded", bad)
		}
	}
}

func TestStagingFolderByID(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	f, err := d.StagingFolderForDate(ctx, "2025-05-30", t0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.StagingFolderByID(ctx, f.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, f) {
		t.Errorf("got %+v, want %+v", got, f)
	}
	if _, err := d.StagingFolderByID(ctx, 999); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestClockWarningsUnder(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	seedUnder(t, d)
	in, err := d.CreateIngest(ctx, "/cards/X", "X", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = d.AddClockWarnings(ctx, in.ID, []ClockWarning{
		{CardPath: "DCIM/a.JPG", SHA256: hexN(1), Kind: ClockFuture, Detail: "in the future"},
		{CardPath: "DCIM/c.JPG", SHA256: hexN(5), Kind: ClockFuture, Detail: "x"},
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.ClockWarningsUnder(ctx, "Staging/2025-05-30/", StateStaged)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SHA256 != hexN(1) || got[0].Detail != "in the future" {
		t.Errorf("got %+v", got)
	}
}

func TestStagingFolderIDsNeverReused(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	var top int64
	for _, date := range []string{"2025-05-30", "2025-05-31", "2025-06-01"} {
		f, err := d.StagingFolderForDate(ctx, date, t0)
		if err != nil {
			t.Fatal(err)
		}
		top = max(top, f.ID)
	}
	if err := d.DeleteStagingFolder(ctx, top); err != nil {
		t.Fatal(err)
	}
	f, err := d.StagingFolderForDate(ctx, "2025-06-01", t0)
	if err != nil {
		t.Fatal(err)
	}
	if f.ID <= top {
		t.Fatalf("new folder id %d, want above every earlier id (%d)", f.ID, top)
	}
}

func TestStagingFolderWithDate(t *testing.T) {
	d := openTest(t)
	ctx := t.Context()
	trip := insertFolder(t, d, "2025-05-30 - Trip", `["2025-05-30","2025-05-31"]`)
	insertFolder(t, d, "2025-05-31", `["2025-05-31"]`)
	named := insertFolder(t, d, "2025-06-02", `[]`)

	f, ok, err := d.StagingFolderWithDate(ctx, "2025-05-31")
	if err != nil || !ok || f.ID != trip || f.Name != "2025-05-30 - Trip" {
		t.Errorf("covering folder = %+v, %v, %v; want id %d", f, ok, err, trip)
	}
	f, ok, err = d.StagingFolderWithDate(ctx, "2025-06-02")
	if err != nil || !ok || f.ID != named || len(f.Dates) != 0 {
		t.Errorf("folder named for the date = %+v, %v, %v; want id %d with its dates unchanged", f, ok, err, named)
	}
	if row, err := d.StagingFolderByID(ctx, named); err != nil || len(row.Dates) != 0 {
		t.Errorf("the lookup wrote dates: %+v, %v", row, err)
	}
	if f, ok, err := d.StagingFolderWithDate(ctx, "2025-06-03"); err != nil || ok {
		t.Errorf("no folder: %+v, %v, %v; want false", f, ok, err)
	}
	if _, _, err := d.StagingFolderWithDate(ctx, "2025-6-3"); err == nil {
		t.Error("a bad date was accepted")
	}
	if all, _ := d.StagingFolders(ctx); len(all) != 3 {
		t.Errorf("folders = %d, want 3 (the lookup must not create one)", len(all))
	}
}
