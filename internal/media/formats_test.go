package media

import (
	"path/filepath"
	"testing"
)

func TestKindOf(t *testing.T) {
	r := DefaultRules()
	cases := []struct {
		name string
		kind Kind
		ok   bool
	}{
		{"DSC00001.JPG", KindImage, true},
		{"photo.jpeg", KindImage, true},
		{"scan.TIF", KindImage, true},
		{"screen.png", KindImage, true},
		{"anim.GIF", KindImage, true},
		{"DSC00001.ARW", KindRaw, true},
		{"IMG_0001.CR3", KindRaw, true},
		{"IMG_0001.cr2", KindRaw, true},
		{"DSC_0001.NEF", KindRaw, true},
		{"PXL_0001.dng", KindRaw, true},
		{"GX010001.MP4", KindVideo, true},
		{"clip.MOV", KindVideo, true},
		{"old.avi", KindVideo, true},
		{"GL010001.LRV", "", false},
		{"GX010001.THM", "", false},
		{"README", "", false},
		{"photo.jpg.zip", "", false},
		{filepath.Join("DCIM", "100MSDCF", "DSC00002.JPG"), KindImage, true},
	}
	for _, c := range cases {
		kind, ok := r.KindOf(c.name)
		if kind != c.kind || ok != c.ok {
			t.Errorf("KindOf(%q) = %q, %v; want %q, %v", c.name, kind, ok, c.kind, c.ok)
		}
	}
}

func TestKindOfCustomRules(t *testing.T) {
	r := Rules{ImageFormats: []string{"heic"}}
	if k, ok := r.KindOf("IMG_1.HEIC"); k != KindImage || !ok {
		t.Errorf("custom rules: got %q, %v", k, ok)
	}
	if _, ok := r.KindOf("IMG_1.jpg"); ok {
		t.Error("jpg matched rules that don't list it")
	}
}

func TestTypeSubdir(t *testing.T) {
	r := DefaultRules()
	for kind, want := range map[Kind]string{KindImage: "", KindRaw: "Capture", KindVideo: "Videos", Kind("other"): ""} {
		if got := TypeSubdir(kind, r); got != want {
			t.Errorf("TypeSubdir(%q) = %q, want %q", kind, got, want)
		}
	}
}
