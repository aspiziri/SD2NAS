package ingest

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/aspiziri/SD2NAS/internal/media"
)

func rels(files []CardFile) []string {
	out := make([]string, 0, len(files))
	for _, f := range files {
		out = append(out, f.Rel)
	}
	return out
}

func TestScanSkipsJunk(t *testing.T) {
	card := t.TempDir()
	keep := []string{
		"DCIM/100GOPRO/GX010001.MP4",
		"DCIM/100MSDCF/DSC00001.ARW",
		"DCIM/100MSDCF/DSC00001.JPG",
		"PRIVATE/M4ROOT/CLIP/C0001.MP4",
		"SUB/keep.JPG", // SUB is only skipped inside M4ROOT
	}
	// Every regular file that is neither imported nor on the ignore list
	// (Decision 10), including extensions SD2NAS has never heard of and
	// files with no extension.
	unsupported := []string{
		"DCIM/100GOPRO/GS010003.360",
		"DCIM/100GOPRO/GX010002.3GP",
		"DCIM/100MSDCF/DSC00002.HIF",
		"DCIM/100MSDCF/DSC00003.TIFF",
		"DCIM/100MSDCF/DSC00004.mpo",
		"DCIM/100MSDCF/DSC00005.WebP",
		"DCIM/100MSDCF/README",
		"DCIM/101CANON/IMG_0001.CRM",
		"PRIVATE/M4ROOT/CLIP/C0002.M4V",
		"PRIVATE/M4ROOT/CLIP/C0003.MKV",
	}
	// Camera sidecars and metadata on the ignore list.
	ignored := []string{
		"DCIM/100GOPRO/GL010001.LRV",
		"DCIM/100GOPRO/GX010001.THM",
		"DCIM/100MEDIA/DJI_0001.LRF",
		"DCIM/CANONMSC/M0100.CTG",
		"PRIVATE/AVCHD/BDMV/INDEX.BDM",
		"PRIVATE/AVCHD/BDMV/CLIPINF/00000.CPI",
		"PRIVATE/AVCHD/BDMV/PLAYLIST/00000.MPL",
		"PRIVATE/M4ROOT/CLIP/C0001M01.XML",
		"PRIVATE/M4ROOT/MEDIAPRO.XML",
		"PRIVATE/M4ROOT/STATUS.BIN",
		"PRIVATE/SONY/SONYCARD.IND",
		"AVF_INFO/AVIN0001.BNP",
		"AVF_INFO/AVIN0001.INP",
		"AVF_INFO/AVIN0001.INT",
		"AVF_INFO/PRV00001.DAT",
		"DCIM/100MSDCF/C0001.SMI",
		"notes.txt",
	}
	junk := []string{
		"DCIM/100MSDCF/._DSC00001.JPG",
		"DCIM/100MSDCF/.DS_Store",
		"DCIM/.Trashes/501/DSC00009.JPG",
		".Trashes/501/DSC00010.JPG",
		".Spotlight-V100/Store-V2/x.JPG",
		"MISC/THM00001.JPG",
		"misc/lower.JPG",
		"misc/unknown.XYZ",
		"DCIM/100GOPRO/.thumbnails/x.JPG",
		"DCIM/100MSDCF/.hidden.XYZ",
		"PRIVATE/M4ROOT/THMBNL/C0001T01.JPG",
		"PRIVATE/M4ROOT/SUB/C0001S03.MP4",
		"System Volume Information/x.jpg",
		"System Volume Information/IndexerVolumeGuid",
		"$RECYCLE.BIN/x.jpg",
	}
	for _, list := range [][]string{keep, unsupported, ignored, junk} {
		for _, rel := range list {
			writeFile(t, card, rel, []byte("x"))
		}
	}

	res, err := Scan(card, media.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if got := rels(res.Media); !reflect.DeepEqual(got, keep) {
		t.Errorf("Media = %v, want %v", got, keep)
	}
	if got := rels(res.Unsupported); !reflect.DeepEqual(got, unsupported) {
		t.Errorf("Unsupported = %v, want %v", got, unsupported)
	}
	if len(res.Errors) != 0 {
		t.Errorf("Errors = %v, want none", res.Errors)
	}
	wantKind := map[string]media.Kind{".MP4": media.KindVideo, ".ARW": media.KindRaw, ".JPG": media.KindImage}
	for _, f := range append(append([]CardFile{}, res.Media...), res.Unsupported...) {
		if f.Size != 1 {
			t.Errorf("%s: Size = %d, want 1", f.Rel, f.Size)
		}
		if want := filepath.Join(card, filepath.FromSlash(f.Rel)); f.Path != want {
			t.Errorf("%s: Path = %q, want %q", f.Rel, f.Path, want)
		}
	}
	for _, f := range res.Media {
		if want := wantKind[filepath.Ext(f.Rel)]; f.Kind != want {
			t.Errorf("%s: Kind = %q, want %q", f.Rel, f.Kind, want)
		}
	}
	for _, f := range res.Unsupported {
		if f.Kind != "" {
			t.Errorf("%s: Kind = %q, want empty", f.Rel, f.Kind)
		}
	}
}

func TestUnsupportedExts(t *testing.T) {
	files := []CardFile{
		{Rel: "DCIM/A.MKV"}, {Rel: "DCIM/B.360"}, {Rel: "DCIM/c.mkv"}, {Rel: "README"}, {Rel: "DCIM/D.HIF"}, {Rel: "x/LICENSE"},
	}
	got := unsupportedExts(files)
	want := []string{".360", ".hif", ".mkv", ""}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unsupportedExts = %q, want %q", got, want)
	}
	if got := unsupportedExts(nil); got != nil {
		t.Errorf("unsupportedExts(nil) = %q, want nil", got)
	}
}

func TestScanSkipsSymlinks(t *testing.T) {
	card := t.TempDir()
	outside := writeFile(t, t.TempDir(), "o.JPG", []byte("x"))
	if err := os.MkdirAll(filepath.Join(card, "DCIM"), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(card, "DCIM", "link.JPG")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	res, err := Scan(card, media.DefaultRules())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Media) != 0 {
		t.Errorf("Media = %v, want none", rels(res.Media))
	}
}

func TestScanRootErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Scan(filepath.Join(dir, "missing"), media.DefaultRules()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing root: err = %v, want fs.ErrNotExist", err)
	}
	file := writeFile(t, dir, "f.txt", []byte("x"))
	if _, err := Scan(file, media.DefaultRules()); err == nil {
		t.Error("file root: err = nil, want an error")
	}
}
