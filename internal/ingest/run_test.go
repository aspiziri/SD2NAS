package ingest

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

const (
	sonyJPEG = "Staging/2025-05-30/2025-05-31 01.30.12.JPG"
	sonyARW  = "Staging/2025-05-30/Capture/2025-05-31 01.30.12.ARW"

	cardJPG = "DCIM/100MSDCF/DSC00001.JPG"
	cardARW = "DCIM/100MSDCF/DSC00001.ARW"
)

func cardPath(card, rel string) string { return filepath.Join(card, filepath.FromSlash(rel)) }

func mustHash(t *testing.T, p string) string {
	t.Helper()
	h, err := media.HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func mustFP(t *testing.T, p string) string {
	t.Helper()
	fp, err := media.QuickFingerprint(p)
	if err != nil {
		t.Fatal(err)
	}
	return fp
}

func mustSize(t *testing.T, p string) int64 {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func wantStaged(t *testing.T, e *env, want ...string) {
	t.Helper()
	got := e.staged(t)
	if len(want) == 0 {
		want = nil
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("staged = %q, want %q", got, want)
	}
}

func wantDone(t *testing.T, out catalog.Ingest, counts catalog.Counts, safe bool) {
	t.Helper()
	if out.Status != catalog.StatusDone {
		t.Fatalf("status = %q (error %q), want done", out.Status, out.Error)
	}
	if out.Counts != counts {
		t.Errorf("counts = %+v, want %+v", out.Counts, counts)
	}
	if out.SafeToFormat != safe {
		t.Errorf("safe_to_format = %v, want %v", out.SafeToFormat, safe)
	}
}

func TestRunFixtureCard(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	jpg, arw := cardPath(card, cardJPG), cardPath(card, cardARW)
	fpJ, fpA := mustFP(t, jpg), mustFP(t, arw)
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
	if out.ClockWarnings != 0 || out.FinishedAt.IsZero() || out.Error != "" {
		t.Errorf("ingest = %+v", out)
	}
	if want := mustSize(t, jpg) + mustSize(t, arw); out.CardSize != want {
		t.Errorf("card size = %d, want %d", out.CardSize, want)
	}
	wantStaged(t, e, sonyJPEG, sonyARW)
	for rel, src := range map[string]string{sonyJPEG: jpg, sonyARW: arw} {
		got, err := os.ReadFile(filepath.Join(e.photos, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		want, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s differs from its source", rel)
		}
	}
	folders, err := e.cat.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 || folders[0].Name != "2025-05-30" || !reflect.DeepEqual(folders[0].Dates, []string{"2025-05-30"}) {
		t.Errorf("staging folders = %+v", folders)
	}
	a, err := e.cat.AssetBySHA(t.Context(), mustHash(t, jpg))
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(e.photos, filepath.FromSlash(sonyJPEG)))
	if err != nil {
		t.Fatal(err)
	}
	if a.State != catalog.StateStaged || a.RelPath != sonyJPEG || a.TZOffset != "+02:00" || a.Kind != media.KindImage ||
		a.FirstIngest != out.ID || a.CaptureTime.Format(catalog.CaptureLayout) != "2025-05-31T01:30:12" ||
		a.Mtime != info.ModTime().UnixNano() {
		t.Errorf("asset = %+v", a)
	}
	files := e.files(t, out.ID)
	if len(files) != 2 {
		t.Fatalf("ingest files = %+v", files)
	}
	for cp, f := range files {
		if f.Outcome != catalog.OutcomeNew || f.RelPath == "" {
			t.Errorf("%s row = %+v", cp, f)
		}
	}
	if got := mustFP(t, jpg); got != fpJ {
		t.Errorf("card JPG fingerprint changed")
	}
	if got := mustFP(t, arw); got != fpA {
		t.Errorf("card ARW fingerprint changed")
	}
}

func TestRunGoProCard(t *testing.T) {
	e := newEnv(t)
	out := e.run(t, e.fixtureCard(t, "GOPRO"))
	wantDone(t, out, catalog.Counts{Seen: 1, New: 1}, true)
	local := time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC).In(time.Local)
	wantStaged(t, e, path.Join("Staging", media.FolderDate(local, media.DefaultDayStartsAt), "Videos",
		local.Format("2006-01-02 15.04.05")+" - 3840w - 29.970 - 12sec.MP4"))
}

func TestRunRerunImportsNothing(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	e.run(t, card)
	foldersBefore, err := e.cat.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, InStaging: 2}, true)
	wantStaged(t, e, sonyJPEG, sonyARW)
	foldersAfter, err := e.cat.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(foldersBefore, foldersAfter) {
		t.Errorf("staging folders changed: %+v -> %+v", foldersBefore, foldersAfter)
	}
	files := e.files(t, out.ID)
	for cp, want := range map[string]string{cardJPG: sonyJPEG, cardARW: sonyARW} {
		if f := files[cp]; f.Outcome != catalog.OutcomeInStaging || f.RelPath != want {
			t.Errorf("%s row = %+v, want in_staging at %s", cp, f, want)
		}
	}
}

func TestRunKnownOutcomes(t *testing.T) {
	e := newEnv(t)
	card := e.cards
	specs := []struct {
		rel, dto string
		state    catalog.AssetState
		relPath  string
	}{
		{"DCIM/100MSDCF/DSC00001.JPG", "2025:06:01 10:00:00", catalog.StateArchived, "Archive/2025-06-01 - Hike/2025-06-01 10.00.00.JPG"},
		{"DCIM/100MSDCF/DSC00002.JPG", "2025:06:01 10:00:01", catalog.StateCulled, ""},
		{"DCIM/100MSDCF/DSC00003.JPG", "2025:06:01 10:00:02", catalog.StateMissing, ""},
	}
	var archivedSHA string
	for _, s := range specs {
		p := writeJPEG(t, card, s.rel, mt.ExifSpec{DateTimeOriginal: s.dto})
		sha := mustHash(t, p)
		a := catalog.Asset{SHA256: sha, Size: mustSize(t, p), Kind: media.KindImage, State: s.state, RelPath: s.relPath, UpdatedAt: testNow}
		if err := e.cat.InsertAsset(t.Context(), a, mustFP(t, p)); err != nil {
			t.Fatal(err)
		}
		if s.state == catalog.StateArchived {
			archivedSHA = sha
		}
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 3, InArchive: 1, SeenBefore: 2}, true)
	wantStaged(t, e)
	f := e.files(t, out.ID)["DCIM/100MSDCF/DSC00001.JPG"]
	if f.Outcome != catalog.OutcomeInArchive || f.SHA256 != archivedSHA || f.RelPath != specs[0].relPath {
		t.Errorf("archived row = %+v", f)
	}
}

func TestRunVerifyFull(t *testing.T) {
	seed := func(t *testing.T, e *env, card string) {
		t.Helper()
		a := catalog.Asset{SHA256: strings.Repeat("bb", 32), Size: 1, Kind: media.KindImage, State: catalog.StateArchived,
			RelPath: "Archive/x.JPG", UpdatedAt: testNow}
		if err := e.cat.InsertAsset(t.Context(), a, mustFP(t, cardPath(card, cardJPG))); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("quick", func(t *testing.T) {
		e := newEnv(t)
		card := e.fixtureCard(t, "SONY_A7IV")
		seed(t, e, card)
		out := e.run(t, card)
		wantDone(t, out, catalog.Counts{Seen: 2, New: 1, InArchive: 1}, true)
		wantStaged(t, e, sonyARW)
	})
	t.Run("full", func(t *testing.T) {
		e := newEnv(t)
		if err := e.cat.SetSetting(t.Context(), catalog.KeyVerifyKnown, "full"); err != nil {
			t.Fatal(err)
		}
		card := e.fixtureCard(t, "SONY_A7IV")
		seed(t, e, card)
		out := e.run(t, card)
		wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
		wantStaged(t, e, sonyJPEG, sonyARW)
	})
}

func TestRunDuplicateOnCard(t *testing.T) {
	e := newEnv(t)
	data, err := mt.JPEG(mt.SonyExif)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, e.cards, "DCIM/100MSDCF/DSC00001.JPG", data)
	writeFile(t, e.cards, "DCIM/101MSDCF/DSC00001.JPG", data)
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, InStaging: 1}, true)
	wantStaged(t, e, sonyJPEG)
}

func TestRunCopyMatchesExistingAsset(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	jpg := cardPath(card, cardJPG)
	sha := mustHash(t, jpg)
	a := catalog.Asset{SHA256: sha, Size: mustSize(t, jpg), Kind: media.KindImage, State: catalog.StateArchived,
		RelPath: "Archive/a.JPG", UpdatedAt: testNow}
	if err := e.cat.InsertAsset(t.Context(), a, strings.Repeat("aa", 32)); err != nil {
		t.Fatal(err)
	}
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 1, InArchive: 1}, true)
	wantStaged(t, e, sonyARW)
	got, err := e.cat.AssetsByFingerprint(t.Context(), mustFP(t, jpg))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].SHA256 != sha || got[0].State != catalog.StateArchived {
		t.Errorf("assets by fingerprint = %+v", got)
	}
}

func TestRunBurstOrder(t *testing.T) {
	e := newEnv(t)
	for _, c := range []struct{ file, model string }{{"DSC00003.JPG", "A"}, {"DSC00001.JPG", "B"}, {"DSC00002.JPG", "C"}} {
		writeJPEG(t, e.cards, "DCIM/100MSDCF/"+c.file, mt.ExifSpec{Model: c.model, DateTimeOriginal: "2025:06:01 10:00:00"})
	}
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{Seen: 3, New: 3}, true)
	files := e.files(t, out.ID)
	for file, want := range map[string]string{
		"DSC00001.JPG": "Staging/2025-06-01/2025-06-01 10.00.00.JPG",
		"DSC00002.JPG": "Staging/2025-06-01/2025-06-01 10.00.00-1.JPG",
		"DSC00003.JPG": "Staging/2025-06-01/2025-06-01 10.00.00-2.JPG",
	} {
		if got := files["DCIM/100MSDCF/"+file].RelPath; got != want {
			t.Errorf("%s -> %q, want %q", file, got, want)
		}
	}
}

func TestRunDayCutoffSetting(t *testing.T) {
	e := newEnv(t)
	if err := e.cat.SetSetting(t.Context(), catalog.KeyDayStartsAt, "00:00"); err != nil {
		t.Fatal(err)
	}
	out := e.run(t, e.fixtureCard(t, "SONY_A7IV"))
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
	if got := e.files(t, out.ID)[cardJPG].RelPath; got != "Staging/2025-05-31/2025-05-31 01.30.12.JPG" {
		t.Errorf("JPG staged at %q", got)
	}
}

func TestRunRefusedBelowReserve(t *testing.T) {
	e := newEnv(t)
	e.free = 50e9 + 100
	card := e.fixtureCard(t, "SONY_A7IV")
	out := e.run(t, card)
	if out.Status != catalog.StatusRefused || out.Counts != (catalog.Counts{}) || out.SafeToFormat {
		t.Errorf("ingest = %+v", out)
	}
	const want = "staging would drop below the 50.0 GB free-space reserve (2 new files need 0.0 GB, 50.0 GB free)"
	if out.Error != want {
		t.Errorf("error = %q, want %q", out.Error, want)
	}
	wantStaged(t, e)
	if err := e.cat.SetSetting(t.Context(), catalog.KeyFreeSpaceReserveGB, "0"); err != nil {
		t.Fatal(err)
	}
	out = e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2}, true)
}

func TestRunNoSpaceCheckWithoutNewFiles(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	e.run(t, card)
	e.free = 0
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, InStaging: 2}, true)
}

func TestRunUnsupportedFormatsNotSafe(t *testing.T) {
	e := newEnv(t)
	card := e.fixtureCard(t, "SONY_A7IV")
	hif := writeFile(t, card, "DCIM/100MSDCF/DSC00002.HIF", []byte("hif data"))
	out := e.run(t, card)
	wantDone(t, out, catalog.Counts{Seen: 2, New: 2, Unsupported: 1}, false)
	f := e.files(t, out.ID)["DCIM/100MSDCF/DSC00002.HIF"]
	if f.Outcome != catalog.OutcomeUnsupported || f.Size != mustSize(t, hif) {
		t.Errorf("unsupported row = %+v", f)
	}
}

func TestRunEmptyCardNotSafe(t *testing.T) {
	e := newEnv(t)
	writeFile(t, e.cards, "DCIM/100MSDCF/._DSC00001.JPG", []byte("junk"))
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{}, false)
}

func TestRunClockWarnings(t *testing.T) {
	e := newEnv(t)
	reset := writeJPEG(t, e.cards, "DCIM/100MSDCF/DSC00001.JPG", mt.ExifSpec{DateTimeOriginal: "2000:01:01 00:00:00"})
	future := writeJPEG(t, e.cards, "DCIM/100MSDCF/DSC00002.JPG", mt.ExifSpec{DateTimeOriginal: "2099:01:01 12:00:00"})
	gps := writeJPEG(t, e.cards, "DCIM/100MSDCF/DSC00003.JPG", mt.ExifSpec{
		DateTimeOriginal: "2025:05:31 01:30:12", OffsetTimeOriginal: "+02:00",
		GPSDate: "2025:05:30", GPSTime: [3][2]uint32{{22, 1}, {30, 1}, {12, 1}}})
	out := e.run(t, e.cards)
	wantDone(t, out, catalog.Counts{Seen: 3, New: 3}, true)
	if out.ClockWarnings != 3 {
		t.Fatalf("clock warnings = %d, want 3", out.ClockWarnings)
	}
	ws, err := e.cat.ClockWarnings(t.Context(), out.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := map[catalog.ClockWarningKind]catalog.ClockWarning{}
	for _, w := range ws {
		got[w.Kind] = w
	}
	for kind, c := range map[catalog.ClockWarningKind]struct{ rel, file string }{
		catalog.ClockResetDefault: {"DCIM/100MSDCF/DSC00001.JPG", reset},
		catalog.ClockFuture:       {"DCIM/100MSDCF/DSC00002.JPG", future},
		catalog.ClockGPSMismatch:  {"DCIM/100MSDCF/DSC00003.JPG", gps},
	} {
		w, ok := got[kind]
		if !ok {
			t.Errorf("no %s warning in %+v", kind, ws)
			continue
		}
		if w.CardPath != c.rel || w.SHA256 != mustHash(t, c.file) {
			t.Errorf("%s warning = %+v", kind, w)
		}
	}
}

func TestRunMissingCard(t *testing.T) {
	e := newEnv(t)
	out := e.run(t, filepath.Join(e.cards, "nope"))
	if out.Status != catalog.StatusFailed || !strings.HasPrefix(out.Error, "scan card: ") || out.SafeToFormat {
		t.Errorf("ingest = %+v", out)
	}
}
