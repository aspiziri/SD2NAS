package indexer

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

var t0 = time.Date(2025, 5, 30, 12, 0, 0, 0, time.UTC)

func rels(fs []file) []string {
	out := []string{}
	for _, f := range fs {
		out = append(out, f.rel)
	}
	return out
}

func TestSkipRules(t *testing.T) {
	s, _, photos := setup(t)
	const S = "Archive/2025-05-30 - Trip/"
	indexed := []string{S + "a.JPG", S + "Capture/b.ARW", S + "Selects/c.JPG", S + "Trash/d.ARW",
		S + "Videos/e.MP4", S + "Other/f.JPG", S + "Edits/g.JPG", "Archive/l.JPG", "Archive/Output/m.JPG"}
	skipped := []string{S + "Output/h.JPG", S + "Capture/CaptureOne/Cache/i.JPG", S + "Selects/CaptureOne/j.JPG",
		S + "2025-05-30 - Trip.cosessiondb", S + "notes.txt", S + ".DS_Store", S + "._a.JPG",
		S + "@eaDir/a.JPG/SYNOPHOTO_THUMB_XL.jpg", S + "#recycle/k.JPG"}
	for i, r := range append(append([]string{}, indexed...), skipped...) {
		put(t, photos, r, "content-"+string(rune('a'+i)), t0)
	}
	got := listing(t, s, catalog.DefaultSettings())
	want := append([]string{}, indexed...)
	sortStrings(want)
	if !reflect.DeepEqual(rels(got), want) {
		t.Fatalf("listing = %v\nwant %v", rels(got), want)
	}
	for _, f := range got {
		k, _ := media.DefaultRules().KindOf(f.rel)
		if f.kind != k || f.size == 0 || f.mtime != t0.Unix() {
			t.Errorf("%s: kind %q size %d mtime %d", f.rel, f.kind, f.size, f.mtime)
		}
	}
}

func TestSkipSetting(t *testing.T) {
	s, _, photos := setup(t)
	const S = "Archive/T/"
	for _, r := range []string{S + "Edits/g.JPG", S + "Selects/IMG_0001.jpg", S + "Capture/CaptureOne/x.JPG",
		S + "Output/h.JPG", S + "a.JPG", S + "x.cosessiondb", S + ".hid/y.JPG", S + "@eaDir/z.JPG"} {
		put(t, photos, r, r, t0)
	}
	set := catalog.DefaultSettings()
	set.ArchiveSkip = "Output/, Edits/, *.cosessiondb, IMG_*.JPG"
	want := []string{S + "Capture/CaptureOne/x.JPG", S + "a.JPG"}
	if got := rels(listing(t, s, set)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	set.ArchiveSkip = ""
	want = []string{S + "Capture/CaptureOne/x.JPG", S + "Edits/g.JPG", S + "Output/h.JPG", S + "Selects/IMG_0001.jpg", S + "a.JPG"}
	if got := rels(listing(t, s, set)); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSkipRulesReaches(t *testing.T) {
	entries, err := catalog.ParseArchiveSkip(catalog.DefaultArchiveSkip)
	if err != nil {
		t.Fatal(err)
	}
	k := newSkipRules(media.DefaultRules(), entries)
	for rel, want := range map[string]bool{
		"Archive/S/Output/x.JPG":             false,
		"Archive/S/Capture/CaptureOne/x.JPG": false,
		"Archive/S/.hidden/x.JPG":            false,
		"Archive/S/x.txt":                    false,
		"Archive/S/Capture/x.ARW":            true,
		"Archive/Output/x.JPG":               true,
	} {
		if got := k.reaches(rel); got != want {
			t.Errorf("reaches(%s) = %v, want %v", rel, got, want)
		}
	}
}

func TestListingSkipsHeldFolder(t *testing.T) {
	s, _, photos := setup(t)
	locks := newFakeLocks()
	s.locks = locks
	put(t, photos, "Archive/2025-05-30 - Trip/a.JPG", "a", t0)
	put(t, photos, "Archive/2025-05-30 - Trip/Sub/b.JPG", "b", t0)
	put(t, photos, "Archive/Other/c.JPG", "c", t0)
	locks.hold("archive/2025-05-30 - trip")
	got, r := listingResult(t, s, catalog.DefaultSettings())
	if !reflect.DeepEqual(rels(got), []string{"Archive/Other/c.JPG"}) || r.BusyFolders != 1 {
		t.Fatalf("got %v busy %d", rels(got), r.BusyFolders)
	}

	sc, err := s.newScan(t.Context(), catalog.DefaultSettings(), &ScanResult{})
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.walk(filepath.Join(photos, ArchiveDir), ArchiveDir, 0); err != nil {
		t.Fatal(err)
	}
	if !sc.skipped["Archive/2025-05-30 - Trip"] {
		t.Fatalf("held folder not in skipped: %v", sc.skipped)
	}

	locks.release("archive/2025-05-30 - trip")
	put(t, photos, "Archive/2025/05/T/d.JPG", "d", t0)
	put(t, photos, "Archive/2025/05/U/e.JPG", "e", t0)
	locks.hold("Archive/2025/05/T")
	got, r = listingResult(t, s, catalog.DefaultSettings())
	// directory order: "2025" sorts before "2025-05-30 - Trip"
	want := []string{"Archive/2025/05/U/e.JPG", "Archive/2025-05-30 - Trip/Sub/b.JPG", "Archive/2025-05-30 - Trip/a.JPG", "Archive/Other/c.JPG"}
	if r.BusyFolders != 1 || !reflect.DeepEqual(rels(got), want) {
		t.Fatalf("got %v busy %d, want %v", rels(got), r.BusyFolders, want)
	}
}

func TestListingUnreadableFolder(t *testing.T) {
	s, _, photos := setup(t)
	put(t, photos, "Archive/A/a.JPG", "a", t0)
	put(t, photos, "Archive/B/b.JPG", "b", t0)
	realReadDir := s.readDir
	s.readDir = func(p string) ([]os.DirEntry, error) {
		if filepath.Base(p) == "A" {
			return nil, errors.New("boom")
		}
		return realReadDir(p)
	}
	r := &ScanResult{}
	sc, err := s.newScan(t.Context(), catalog.DefaultSettings(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.walk(filepath.Join(photos, ArchiveDir), ArchiveDir, 0); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rels(sc.fresh), []string{"Archive/B/b.JPG"}) {
		t.Fatalf("got %v", rels(sc.fresh))
	}
	if len(r.Errors) != 1 || !sc.skipped["Archive/A"] {
		t.Fatalf("errors %v skipped %v", r.Errors, sc.skipped)
	}
}

func TestListingSymlinkNotFollowed(t *testing.T) {
	s, _, photos := setup(t)
	put(t, photos, "Archive/A/a.JPG", "a", t0)
	put(t, photos, "Elsewhere/e.JPG", "e", t0)
	if err := os.Symlink(filepath.Join(photos, "Elsewhere"), filepath.Join(photos, "Archive", "A", "dirlink")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(photos, "Elsewhere", "e.JPG"), filepath.Join(photos, "Archive", "A", "filelink.JPG")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if got := rels(listing(t, s, catalog.DefaultSettings())); !reflect.DeepEqual(got, []string{"Archive/A/a.JPG"}) {
		t.Fatalf("got %v", got)
	}
}
