package media

import (
	"encoding/binary"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rwcarlsen/goexif/exif"

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

// corruptExifIFD returns a copy of a little-endian TIFF whose Exif sub-IFD
// claims 0x7FFF entries, so decoding it runs past the end of the data. The
// IFD is located through the ExifIFDPointer (0x8769) entry in IFD0.
func corruptExifIFD(t *testing.T, tiff []byte) []byte {
	t.Helper()
	le := binary.LittleEndian
	ifd0 := int(le.Uint32(tiff[4:]))
	n := int(le.Uint16(tiff[ifd0:]))
	for i := 0; i < n; i++ {
		e := ifd0 + 2 + 12*i
		if le.Uint16(tiff[e:]) == 0x8769 {
			out := append([]byte(nil), tiff...)
			le.PutUint16(out[le.Uint32(tiff[e+8:]):], 0x7FFF)
			return out
		}
	}
	t.Fatal("no ExifIFDPointer in IFD0")
	return nil
}

func TestReadExifCorruptExifIFDIsWarning(t *testing.T) {
	b := corruptExifIFD(t, mt.TIFF(binary.LittleEndian, mt.SonyExif))
	d, err := ReadExif(writeBytes(t, t.TempDir(), "x.ARW", b))
	if err != nil {
		t.Fatalf("ReadExif: %v, want nil error", err)
	}
	if d.Warning == nil {
		t.Error("Warning = nil, want the sub-IFD failure")
	}
	if d.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", d.Camera)
	}
	if d.OffsetTime != "" {
		t.Errorf("OffsetTime = %q, want empty", d.OffsetTime)
	}
}

// offsetTimeNotASCII returns a copy of a little-endian TIFF whose
// OffsetTimeOriginal (0x9011) entry in the Exif sub-IFD is retyped from
// ASCII to SHORT, so it decodes but can't be read as a string.
func offsetTimeNotASCII(t *testing.T, tiff []byte) []byte {
	t.Helper()
	le := binary.LittleEndian
	ifd0 := int(le.Uint32(tiff[4:]))
	for i := 0; i < int(le.Uint16(tiff[ifd0:])); i++ {
		e := ifd0 + 2 + 12*i
		if le.Uint16(tiff[e:]) != 0x8769 {
			continue
		}
		exifIFD := int(le.Uint32(tiff[e+8:]))
		for j := 0; j < int(le.Uint16(tiff[exifIFD:])); j++ {
			f := exifIFD + 2 + 12*j
			if le.Uint16(tiff[f:]) == 0x9011 {
				out := append([]byte(nil), tiff...)
				le.PutUint16(out[f+2:], 3) // SHORT
				le.PutUint32(out[f+4:], 3) // 3 shorts: still out of line
				return out
			}
		}
	}
	t.Fatal("no OffsetTimeOriginal in the Exif IFD")
	return nil
}

func TestReadExifOffsetTimeNotStringIsWarning(t *testing.T) {
	b := offsetTimeNotASCII(t, mt.TIFF(binary.LittleEndian, mt.SonyExif))
	d, err := ReadExif(writeBytes(t, t.TempDir(), "x.ARW", b))
	if err != nil {
		t.Fatalf("ReadExif: %v, want nil error", err)
	}
	if d.OffsetTime != "" {
		t.Errorf("OffsetTime = %q, want empty", d.OffsetTime)
	}
	if d.Warning == nil {
		t.Fatal("Warning = nil, want the OffsetTimeOriginal failure")
	}
	if msg := d.Warning.Error(); strings.Count(msg, "OffsetTimeOriginal") != 1 {
		t.Errorf("Warning = %q, want OffsetTimeOriginal named exactly once", msg)
	}
	if d.Camera != "SONY ILCE-7M4" || d.DateTime.IsZero() {
		t.Errorf("Camera = %q, DateTime = %v", d.Camera, d.DateTime)
	}
}

// panicDecoder replaces decodeExif with one that panics, restoring it on cleanup.
func panicDecoder(t *testing.T) {
	t.Helper()
	orig := decodeExif
	decodeExif = func(io.Reader) (*exif.Exif, error) { panic("bad ifd") }
	t.Cleanup(func() { decodeExif = orig })
}

func TestReadExifPanic(t *testing.T) {
	panicDecoder(t)
	b, err := os.ReadFile(fixture(t, mt.SonyJPEG))
	if err != nil {
		t.Fatal(err)
	}
	path := writeBytes(t, t.TempDir(), "x.jpg", b)
	d, err := ReadExif(path)
	if d != nil {
		t.Errorf("ExifData = %+v, want nil", d)
	}
	want := "exif " + path + ": the decoder panicked: bad ifd"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	// An open handle would make Remove fail on Windows.
	if err := os.Remove(path); err != nil {
		t.Errorf("file was left open: %v", err)
	}
}

func TestCaptureTimeSurvivesParserPanic(t *testing.T) {
	panicDecoder(t)
	c, err := CaptureTime(fixture(t, mt.SonyJPEG), KindImage)
	if err != nil {
		t.Fatal(err)
	}
	if c.Source != SourceMtime || c.Fallback == nil {
		t.Errorf("got %+v", c)
	}
}
