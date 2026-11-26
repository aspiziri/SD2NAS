package staging

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

var now = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func sha(n int) string { return fmt.Sprintf("%064x", n) }

func at(h int) time.Time { return time.Date(2025, 5, 30, h, 0, 0, 0, time.Local) }

type fixture struct {
	svc    *Service
	db     *catalog.DB
	photos string
	f1, f2 catalog.StagingFolder
}

func setup(t *testing.T) fixture {
	t.Helper()
	photos, config := t.TempDir(), t.TempDir()
	db, err := catalog.Open(filepath.Join(config, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	ctx := t.Context()
	f1, err := db.StagingFolderForDate(ctx, "2025-05-30", now)
	if err != nil {
		t.Fatal(err)
	}
	f2, err := db.StagingFolderForDate(ctx, "2025-06-01", now)
	if err != nil {
		t.Fatal(err)
	}
	assets := []catalog.Asset{
		{SHA256: sha(1), Size: 100, Kind: media.KindImage, CaptureTime: at(10), State: catalog.StateStaged,
			RelPath: "Staging/2025-05-30/2025-05-30 10.00.00.JPG"},
		{SHA256: sha(2), Size: 1000, Kind: media.KindRaw, CaptureTime: at(10), State: catalog.StateStaged,
			RelPath: "Staging/2025-05-30/Capture/2025-05-30 10.00.00.ARW"},
		{SHA256: sha(3), Size: 100, Kind: media.KindImage, CaptureTime: at(11), State: catalog.StateStaged,
			RelPath: "Staging/2025-05-30/2025-05-30 11.00.00.JPG"},
		{SHA256: sha(4), Size: 5000, Kind: media.KindVideo, CaptureTime: at(12), TZOffset: "+02:00", State: catalog.StateStaged,
			RelPath: "Staging/2025-05-30/Videos/2025-05-30 12.00.00 - 3840w - 29.970 - 12sec.MP4"},
		{SHA256: sha(5), Size: 1, Kind: media.KindImage, State: catalog.StateArchived, RelPath: "Archive/2025-05-30 - Old/x.JPG"},
		{SHA256: sha(6), Size: 1, Kind: media.KindImage, State: catalog.StateStaged, RelPath: "Staging/2025-05-30 - Trip/a.JPG"},
	}
	for _, a := range assets {
		a.UpdatedAt = now
		if err := db.InsertAsset(ctx, a, ""); err != nil {
			t.Fatal(err)
		}
	}
	in, err := db.CreateIngest(ctx, "/cards/X", "X", "", now)
	if err != nil {
		t.Fatal(err)
	}
	err = db.AddClockWarnings(ctx, in.ID, []catalog.ClockWarning{
		{CardPath: "DCIM/c.JPG", SHA256: sha(3), Kind: catalog.ClockFuture, Detail: "capture time is after the NAS clock"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"2025-05-30", "2025-05-30 - Trip", ".hidden"} {
		if err := os.MkdirAll(filepath.Join(photos, Dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(photos, Dir, "readme.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	svc := NewService(Config{DB: db, PhotosRoot: photos, Now: func() time.Time { return now }})
	t.Cleanup(svc.Close)
	return fixture{svc, db, photos, f1, f2}
}

// clockAt is a second service over fx's catalog and folders, reading the
// clock at when.
func clockAt(t *testing.T, fx fixture, when time.Time) *Service {
	t.Helper()
	svc := NewService(Config{DB: fx.db, PhotosRoot: fx.photos, Now: func() time.Time { return when }})
	t.Cleanup(svc.Close)
	return svc
}

func hasAgeWarning(f Folder) bool {
	for _, w := range f.Warnings {
		if strings.HasPrefix(w, "In staging") {
			return true
		}
	}
	return false
}

func TestListGroupsByFolder(t *testing.T) {
	fx := setup(t)
	l, err := fx.svc.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Folders) != 2 || l.Folders[0].Name != "2025-05-30" || l.Folders[1].Name != "2025-06-01" {
		t.Fatalf("folders = %+v", l.Folders)
	}
	want := Folder{
		ID: fx.f1.ID, Name: "2025-05-30", Date: "2025-05-30", Title: "", Dates: []string{"2025-05-30"},
		CreatedAt: fx.f1.CreatedAt, Files: 4, Bytes: 6200, Counts: KindCounts{JPG: 2, Raw: 1, Video: 1},
		FirstCapture: "2025-05-30T10:00:00", LastCapture: "2025-05-30T12:00:00",
		Samples:       []Sample{{sha(1), "jpg"}, {sha(3), "jpg"}, {sha(4), "video"}},
		ClockWarnings: 1, Warnings: []string{"1 file has a suspicious capture time"},
	}
	if !reflect.DeepEqual(l.Folders[0], want) {
		t.Errorf("folder = %+v\nwant     %+v", l.Folders[0], want)
	}
}

func TestListUntrackedAndMissing(t *testing.T) {
	fx := setup(t)
	l, err := fx.svc.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Untracked, []string{"2025-05-30 - Trip"}) {
		t.Errorf("untracked = %v", l.Untracked)
	}
	f2 := l.Folders[1]
	if f2.Files != 0 || f2.Samples == nil || len(f2.Samples) != 0 {
		t.Errorf("f2 = %+v", f2)
	}
	if !reflect.DeepEqual(f2.Warnings, []string{"Not found on disk in Staging/"}) {
		t.Errorf("warnings = %v", f2.Warnings)
	}
}

func TestListSkipsNASMetadata(t *testing.T) {
	fx := setup(t)
	for _, name := range []string{"@eaDir", "#recycle", ".x"} {
		if err := os.MkdirAll(filepath.Join(fx.photos, "Staging", name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	l, err := fx.svc.List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(l.Untracked, []string{"2025-05-30 - Trip"}) {
		t.Errorf("untracked = %v", l.Untracked)
	}
}

func TestListNoStagingDir(t *testing.T) {
	db, err := catalog.Open(filepath.Join(t.TempDir(), "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	l, err := New(db, t.TempDir()).List(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if l.Folders == nil || len(l.Folders) != 0 || l.Untracked == nil || len(l.Untracked) != 0 {
		t.Errorf("listing = %#v", l)
	}
}

func TestPickSamplesSpread(t *testing.T) {
	var many []catalog.Asset
	for i := 1; i <= 12; i++ {
		many = append(many, catalog.Asset{SHA256: sha(i), Kind: media.KindImage, RelPath: fmt.Sprintf("x/2025-05-30 10.00.%02d.JPG", i)})
	}
	var got []string
	for _, s := range pickSamples(many, 5) {
		got = append(got, s.SHA256)
	}
	want := []string{sha(1), sha(3), sha(5), sha(8), sha(10)}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("samples = %v, want %v", got, want)
	}
	pairs := []catalog.Asset{
		{SHA256: sha(1), Kind: media.KindRaw, RelPath: "A.ARW"},
		{SHA256: sha(2), Kind: media.KindImage, RelPath: "A.JPG"},
		{SHA256: sha(3), Kind: media.KindRaw, RelPath: "B.ARW"},
	}
	if got := pickSamples(pairs, 5); !reflect.DeepEqual(got, []Sample{{sha(2), "jpg"}, {sha(3), "raw"}}) {
		t.Errorf("pairs = %v", got)
	}
}

func TestFiles(t *testing.T) {
	fx := setup(t)
	ctx := t.Context()
	l, err := fx.svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	ff, err := fx.svc.Files(ctx, fx.f1.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range ff.Files {
		paths = append(paths, f.Path)
	}
	wantPaths := []string{
		"Capture/2025-05-30 10.00.00.ARW", "2025-05-30 10.00.00.JPG", "2025-05-30 11.00.00.JPG",
		"Videos/2025-05-30 12.00.00 - 3840w - 29.970 - 12sec.MP4",
	}
	if !reflect.DeepEqual(paths, wantPaths) {
		t.Fatalf("paths = %v", paths)
	}
	if ff.Files[0].Kind != "raw" || ff.Files[0].Name != "2025-05-30 10.00.00.ARW" {
		t.Errorf("file 0 = %+v", ff.Files[0])
	}
	if v := ff.Files[3]; v.TZOffset != "+02:00" || v.CaptureTime != "2025-05-30T12:00:00" {
		t.Errorf("video = %+v", v)
	}
	if !reflect.DeepEqual(ff.Files[2].ClockWarnings, []string{"capture time is after the NAS clock"}) {
		t.Errorf("file 2 = %+v", ff.Files[2])
	}
	if !reflect.DeepEqual(ff.Folder, l.Folders[0]) {
		t.Errorf("folder = %+v, list has %+v", ff.Folder, l.Folders[0])
	}
	raw, err := fx.svc.Files(ctx, fx.f1.ID, "raw")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Files) != 1 || raw.Files[0].Kind != "raw" || raw.Folder.Files != 4 {
		t.Errorf("raw = %+v", raw)
	}
	if _, err := fx.svc.Files(ctx, fx.f1.ID, "png"); !errors.Is(err, ErrBadKind) {
		t.Errorf("png err = %v", err)
	}
	if _, err := fx.svc.Files(ctx, 999, ""); !errors.Is(err, catalog.ErrNotFound) {
		t.Errorf("999 err = %v", err)
	}
	empty, err := fx.svc.Files(ctx, fx.f2.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if empty.Files == nil || len(empty.Files) != 0 {
		t.Errorf("empty = %#v", empty.Files)
	}
}

func TestInArchive(t *testing.T) {
	fx := setup(t)
	ctx := t.Context()
	for _, c := range []catalog.ArchiveCopy{
		{RelPath: "Archive/S/b.JPG", SHA256: sha(1)},
		{RelPath: "Archive/S/z.ARW", SHA256: sha(2)},
		{RelPath: "Archive/R/a.ARW", SHA256: sha(2)},   // the first path is shown
		{RelPath: "Archive/S/old.JPG", SHA256: sha(5)}, // archived content: a duplicate, not a staged file's copy
	} {
		c.Size, c.Mtime, c.CheckedAt = 1, 1, now
		if err := fx.db.PutArchiveCopy(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	l, err := fx.svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f := l.Folders[0]
	if f.InArchive != 2 || !slices.Contains(f.Warnings, "2 files are already in the archive") {
		t.Errorf("folder = %+v", f)
	}
	if f2 := l.Folders[1]; f2.InArchive != 0 {
		t.Errorf("f2 = %+v", f2)
	}
	ff, err := fx.svc.Files(ctx, fx.f1.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, file := range ff.Files {
		got[file.SHA256] = file.InArchive
	}
	want := map[string]string{sha(1): "Archive/S/b.JPG", sha(2): "Archive/R/a.ARW", sha(3): "", sha(4): ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("in_archive = %v; want %v", got, want)
	}
	if !reflect.DeepEqual(ff.Folder, f) {
		t.Errorf("folder = %+v, list has %+v", ff.Folder, f)
	}
}

func TestSplitName(t *testing.T) {
	for _, c := range []struct{ in, date, title string }{
		{"2025-05-30", "2025-05-30", ""},
		{"2025-05-30 - Trip to Rome", "2025-05-30", "Trip to Rome"},
		{"2007-09-31 - Random Party", "2007-09-31", "Random Party"},
		{"Random", "", "Random"},
		{"2025-05-30 -", "", "2025-05-30 -"},
	} {
		if d, ti := SplitName(c.in); d != c.date || ti != c.title {
			t.Errorf("SplitName(%q) = (%q, %q), want (%q, %q)", c.in, d, ti, c.date, c.title)
		}
	}
}

func TestKindName(t *testing.T) {
	for k, want := range map[media.Kind]string{media.KindImage: "jpg", media.KindRaw: "raw", media.KindVideo: "video"} {
		if got := KindName(k); got != want {
			t.Errorf("KindName(%q) = %q, want %q", k, got, want)
		}
	}
}

func TestListAgeWarnings(t *testing.T) {
	fx := setup(t)
	ctx := t.Context()
	old := clockAt(t, fx, now.Add(40*24*time.Hour))
	l, err := old.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f1 := l.Folders[0]
	if f1.AgeDays != 40 || !f1.Stale {
		t.Errorf("f1 age = %d stale = %v, want 40 and true", f1.AgeDays, f1.Stale)
	}
	if n := len(f1.Warnings); n == 0 || f1.Warnings[n-1] != "In staging for 40 days" {
		t.Errorf("warnings = %v, want the last to be the age warning", f1.Warnings)
	}
	if l.StaleAfterDays != 30 || l.Stale != 2 {
		t.Errorf("stale_after_days = %d stale = %d, want 30 and 2", l.StaleAfterDays, l.Stale)
	}

	young := clockAt(t, fx, now.Add(29*24*time.Hour+23*time.Hour))
	l, err = young.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f1 := l.Folders[0]; f1.AgeDays != 29 || f1.Stale || hasAgeWarning(f1) {
		t.Errorf("29 days: %+v", f1)
	}
	if l.Stale != 0 {
		t.Errorf("stale = %d at 29 days, want 0", l.Stale)
	}

	if err := fx.db.SetSetting(ctx, catalog.KeyStagingWarnDays, "0"); err != nil {
		t.Fatal(err)
	}
	off := clockAt(t, fx, now.Add(40*24*time.Hour))
	l, err = off.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f1 := l.Folders[0]; f1.AgeDays != 40 || f1.Stale || hasAgeWarning(f1) {
		t.Errorf("warn off: %+v", f1)
	}
	if l.StaleAfterDays != 0 || l.Stale != 0 {
		t.Errorf("warn off: stale_after_days = %d stale = %d", l.StaleAfterDays, l.Stale)
	}

	before := clockAt(t, fx, now.Add(-72*time.Hour))
	l, err = before.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if f1 := l.Folders[0]; f1.AgeDays != 0 {
		t.Errorf("clock before created_at: age = %d, want 0", f1.AgeDays)
	}
}

func TestListAndFilesCarryVersion(t *testing.T) {
	fx := setup(t)
	ctx := t.Context()
	fx.svc.Touch()
	fx.svc.Touch()
	l, err := fx.svc.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if l.Version != 2 {
		t.Errorf("listing version = %d, want 2", l.Version)
	}
	ff, err := fx.svc.Files(ctx, fx.f1.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if ff.Version != 2 {
		t.Errorf("files version = %d, want 2", ff.Version)
	}
}
