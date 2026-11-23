package media

import (
	"encoding/binary"
	"errors"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

const nameLayout = "2006-01-02 15.04.05"

var sonyGPS = time.Date(2025, 5, 30, 23, 30, 12, 0, time.UTC)

func checkSony(t *testing.T, d *ExifData, wantGPS time.Time) {
	t.Helper()
	if got := d.DateTime.Format(nameLayout); got != "2025-05-31 01.30.12" {
		t.Errorf("DateTime = %s", got)
	}
	if d.DateTime.Location() != time.Local {
		t.Errorf("DateTime location = %v, want time.Local", d.DateTime.Location())
	}
	if d.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", d.Camera)
	}
	if d.OffsetTime != "+02:00" {
		t.Errorf("OffsetTime = %q", d.OffsetTime)
	}
	if !d.GPSTime.Equal(wantGPS) {
		t.Errorf("GPSTime = %v, want %v", d.GPSTime, wantGPS)
	}
	if d.Warning != nil {
		t.Errorf("Warning = %v, want nil", d.Warning)
	}
}

func readSpecJPEG(t *testing.T, s mt.ExifSpec) *ExifData {
	t.Helper()
	b, err := mt.JPEG(s)
	if err != nil {
		t.Fatal(err)
	}
	d, err := ReadExif(writeBytes(t, t.TempDir(), "x.jpg", b))
	if err != nil {
		t.Fatalf("ReadExif: %v", err)
	}
	return d
}

func TestReadExifSonyJPEGFixture(t *testing.T) {
	d, err := ReadExif(fixture(t, mt.SonyJPEG))
	if err != nil {
		t.Fatal(err)
	}
	checkSony(t, d, sonyGPS)
}

func TestReadExifSonyARWFixture(t *testing.T) {
	d, err := ReadExif(fixture(t, mt.SonyARW))
	if err != nil {
		t.Fatal(err)
	}
	checkSony(t, d, sonyGPS)
}

func TestReadExifBigEndianTIFF(t *testing.T) {
	p := writeBytes(t, t.TempDir(), "x.TIF", mt.TIFF(binary.BigEndian, mt.SonyExif))
	d, err := ReadExif(p)
	if err != nil {
		t.Fatal(err)
	}
	checkSony(t, d, sonyGPS)
}

func TestReadExifWithoutOptionalTags(t *testing.T) {
	d := readSpecJPEG(t, mt.ExifSpec{DateTimeOriginal: "2025:05:31 01:30:12"})
	if got := d.DateTime.Format(nameLayout); got != "2025-05-31 01.30.12" {
		t.Errorf("DateTime = %s", got)
	}
	if d.OffsetTime != "" {
		t.Errorf("OffsetTime = %q", d.OffsetTime)
	}
	if !d.GPSTime.IsZero() {
		t.Errorf("GPSTime = %v", d.GPSTime)
	}
	if d.Camera != "" {
		t.Errorf("Camera = %q", d.Camera)
	}
}

func TestReadExifNoDateTime(t *testing.T) {
	d := readSpecJPEG(t, mt.ExifSpec{Make: "SONY", Model: "ILCE-7M4"})
	if !d.DateTime.IsZero() {
		t.Errorf("DateTime = %v", d.DateTime)
	}
	if d.Warning != nil {
		t.Errorf("Warning = %v, want nil", d.Warning)
	}
	if d.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", d.Camera)
	}
}

func TestReadExifFractionalGPSSeconds(t *testing.T) {
	s := mt.SonyExif
	s.GPSTime = [3][2]uint32{{23, 1}, {30, 1}, {1234, 100}}
	d := readSpecJPEG(t, s)
	want := time.Date(2025, 5, 30, 23, 30, 12, 340000000, time.UTC)
	if !d.GPSTime.Equal(want) {
		t.Errorf("GPSTime = %v, want %v", d.GPSTime, want)
	}
}

func TestReadExifBadGPSRational(t *testing.T) {
	cases := map[string][3][2]uint32{
		"zero denominator": {{23, 1}, {30, 0}, {12, 1}},
		"huge hour":        {{0xFFFFFFFF, 1}, {30, 1}, {12, 1}},
		"hour 50":          {{50, 1}, {30, 1}, {12, 1}},
	}
	for name, g := range cases {
		t.Run(name, func(t *testing.T) {
			s := mt.SonyExif
			s.GPSTime = g
			d := readSpecJPEG(t, s)
			if !d.GPSTime.IsZero() {
				t.Errorf("GPSTime = %v, want zero", d.GPSTime)
			}
			if got := d.DateTime.Format(nameLayout); got != "2025-05-31 01.30.12" {
				t.Errorf("DateTime = %s", got)
			}
		})
	}
}

func TestReadExifBadDateTimeWarning(t *testing.T) {
	d := readSpecJPEG(t, mt.ExifSpec{Make: "SONY", Model: "ILCE-7M4", DateTimeOriginal: "2025-05-31 01:30:12"})
	if !d.DateTime.IsZero() {
		t.Errorf("DateTime = %v", d.DateTime)
	}
	if d.Warning == nil {
		t.Error("Warning = nil, want a parse warning")
	}
	if d.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", d.Camera)
	}
}

func TestReadExifBadGPSDate(t *testing.T) {
	s := mt.SonyExif
	s.GPSDate = "2025-05-30"
	d := readSpecJPEG(t, s)
	if !d.GPSTime.IsZero() {
		t.Errorf("GPSTime = %v", d.GPSTime)
	}
}

func TestReadExifNotExif(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadExif(writeSourceFile(t, dir, "a.jpg", "first")); err == nil {
		t.Error("text file: want error")
	}
	if _, err := ReadExif(writeSourceFile(t, dir, "empty.jpg", "")); err == nil {
		t.Error("empty file: want error")
	}
	if _, err := ReadExif(filepath.Join(dir, "missing.jpg")); err == nil {
		t.Error("missing file: want error")
	}
}

func TestFileModDate(t *testing.T) {
	dir := t.TempDir()
	got, err := FileModDate(writeSourceFile(t, dir, "a.jpg", "x"))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(fixedMtime) {
		t.Errorf("FileModDate = %v, want %v", got, fixedMtime)
	}
	if _, err := FileModDate(filepath.Join(dir, "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing: err = %v, want fs.ErrNotExist", err)
	}
}
