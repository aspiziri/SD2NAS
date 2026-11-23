package catalog

import (
	"reflect"
	"testing"
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
