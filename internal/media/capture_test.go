package media

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

var goproUTC = time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC)

func checkSonyCapture(t *testing.T, c Capture) {
	t.Helper()
	if c.Source != SourceExif {
		t.Errorf("Source = %q, want %q", c.Source, SourceExif)
	}
	if got := c.Time.Format(nameLayout); got != "2025-05-31 01.30.12" {
		t.Errorf("Time = %q", got)
	}
	if c.Time.Location() != time.Local {
		t.Errorf("Location = %v, want Local", c.Time.Location())
	}
	if c.Offset != "+02:00" {
		t.Errorf("Offset = %q", c.Offset)
	}
	if !c.GPS.Equal(time.Date(2025, 5, 30, 23, 30, 12, 0, time.UTC)) {
		t.Errorf("GPS = %v", c.GPS)
	}
	if c.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", c.Camera)
	}
	if c.Fallback != nil {
		t.Errorf("Fallback = %v, want nil", c.Fallback)
	}
}

func TestCaptureTimeSonyJPEG(t *testing.T) {
	c, err := CaptureTime(fixture(t, mt.SonyJPEG), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	checkSonyCapture(t, c)
}

func TestCaptureTimeSonyARW(t *testing.T) {
	c, err := CaptureTime(fixture(t, mt.SonyARW), KindRaw)
	if err != nil {
		t.Fatal(err)
	}
	checkSonyCapture(t, c)
}

func TestCaptureTimeGoProMP4(t *testing.T) {
	c, err := CaptureTime(fixture(t, mt.GoProMP4), KindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMvhd {
		t.Errorf("Source = %q", c.Source)
	}
	if !c.Time.Equal(goproUTC) {
		t.Errorf("Time = %v", c.Time)
	}
	if c.Time.Location() != time.Local {
		t.Errorf("Location = %v, want Local", c.Time.Location())
	}
	if c.Camera != "" {
		t.Errorf("Camera = %q", c.Camera)
	}
	if c.Fallback != nil {
		t.Errorf("Fallback = %v", c.Fallback)
	}
}

func TestCaptureTimeVideoMakeModel(t *testing.T) {
	s := mt.GoProVideo
	s.Firmware, s.Make, s.Model = "", "Apple", "iPhone 15"
	c, err := CaptureTime(writeMP4(t, s), KindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if c.Camera != "Apple iPhone 15" {
		t.Errorf("Camera = %q", c.Camera)
	}
}

func TestCaptureTimePNGUsesMtime(t *testing.T) {
	p := writeSourceFile(t, t.TempDir(), "x.png", "not a png")
	c, err := CaptureTime(p, KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMtime || !c.Time.Equal(fixedMtime) || c.Fallback != nil {
		t.Errorf("got %+v", c)
	}
}

// A .cr3 is ISO-BMFF, which goexif can't parse: it would scan the whole file
// for an APP1 marker. The fake file carries a valid JPEG EXIF block, so a
// CaptureTime that still called ReadExif would return SourceExif.
func TestCaptureTimeCR3SkipsExif(t *testing.T) {
	b, err := mt.JPEG(mt.SonyExif)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"IMG_0001.CR3", "IMG_0002.cr3", "IMG_0003.Cr3"} {
		c, err := CaptureTime(writeBytes(t, t.TempDir(), name, b), KindRaw)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if c.Source != SourceMtime || !c.Time.Equal(fixedMtime) {
			t.Errorf("%s: Source = %q, Time = %v; want mtime %v", name, c.Source, c.Time, fixedMtime)
		}
		if !errors.Is(c.Fallback, ErrExifUnsupported) || !strings.Contains(c.Fallback.Error(), filepath.Ext(name)) {
			t.Errorf("%s: Fallback = %v, want ErrExifUnsupported naming %s", name, c.Fallback, filepath.Ext(name))
		}
		if c.Camera != "" || c.Offset != "" || !c.GPS.IsZero() {
			t.Errorf("%s: EXIF fields set: %+v", name, c)
		}
	}
}

func TestCaptureTimeJPEGWithoutDate(t *testing.T) {
	b, err := mt.JPEG(mt.ExifSpec{Make: "SONY", Model: "ILCE-7M4"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := CaptureTime(writeBytes(t, t.TempDir(), "a.jpg", b), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMtime || !c.Time.Equal(fixedMtime) {
		t.Errorf("got %+v", c)
	}
	if !errors.Is(c.Fallback, ErrNoExifTime) {
		t.Errorf("Fallback = %v", c.Fallback)
	}
	if c.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", c.Camera)
	}
}

func TestCaptureTimeMalformedExifDateTimeWarning(t *testing.T) {
	b, err := mt.JPEG(mt.ExifSpec{Make: "SONY", Model: "ILCE-7M4", DateTimeOriginal: "2025-05-31 01:30:12"})
	if err != nil {
		t.Fatal(err)
	}
	c, err := CaptureTime(writeBytes(t, t.TempDir(), "a.jpg", b), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMtime || !c.Time.Equal(fixedMtime) {
		t.Errorf("got %+v", c)
	}
	if !errors.Is(c.Fallback, ErrNoExifTime) {
		t.Fatalf("Fallback = %v", c.Fallback)
	}
	if !strings.Contains(c.Fallback.Error(), "DateTime") {
		t.Errorf("Fallback %q does not name DateTime", c.Fallback)
	}
}

func TestCaptureTimeNotReallyJPEG(t *testing.T) {
	c, err := CaptureTime(writeSourceFile(t, t.TempDir(), "a.jpg", "first"), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMtime || c.Fallback == nil {
		t.Errorf("got %+v", c)
	}
}

func TestCaptureTimeVideoWithoutCreationTime(t *testing.T) {
	for _, ct := range []uint64{0, 1} {
		s := mt.GoProVideo
		s.CreationTime = ct
		c, err := CaptureTime(writeMP4(t, s), KindVideo)
		if err != nil {
			t.Fatal(err)
		}
		if c.Source != SourceMtime || !c.Time.Equal(fixedMtime) {
			t.Errorf("CreationTime=%d: got %+v", ct, c)
		}
		if !errors.Is(c.Fallback, ErrNoMvhdTime) {
			t.Errorf("CreationTime=%d: Fallback = %v", ct, c.Fallback)
		}
	}
}

func TestCaptureTimeUnparsableVideo(t *testing.T) {
	c, err := CaptureTime(writeSourceFile(t, t.TempDir(), "x.MP4", "first"), KindVideo)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMtime || c.Fallback == nil {
		t.Errorf("got %+v", c)
	}
}

func TestCaptureTimeMissingFile(t *testing.T) {
	if _, err := CaptureTime(filepath.Join(t.TempDir(), "nope.jpg"), KindImage); err == nil {
		t.Error("want error")
	}
}
