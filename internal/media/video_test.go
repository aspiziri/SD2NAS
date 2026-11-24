package media

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/abema/go-mp4"

	mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"
)

// writeMP4 builds an MP4 from s and writes it to x.MP4 in a fresh temp dir.
func writeMP4(t *testing.T, s mt.VideoSpec) string {
	t.Helper()
	b, err := mt.MP4(s)
	if err != nil {
		t.Fatal(err)
	}
	return writeBytes(t, t.TempDir(), "x.MP4", b)
}

func checkGoPro(t *testing.T, m *VideoMeta) {
	t.Helper()
	want := time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC)
	if m.Width != 3840 || m.Height != 2160 || m.Resolution != "3840w" {
		t.Errorf("size = %dx%d %q", m.Width, m.Height, m.Resolution)
	}
	if m.FrameRate != "29.970" {
		t.Errorf("FrameRate = %q, want 29.970", m.FrameRate)
	}
	if m.Duration != 12.512 {
		t.Errorf("Duration = %v, want 12.512", m.Duration)
	}
	if !m.CreationTime.Equal(want) || m.CreationTime.Location() != time.UTC {
		t.Errorf("CreationTime = %v, want %v UTC", m.CreationTime, want)
	}
	if m.Firmware != "HD9.01.01.60.00" {
		t.Errorf("Firmware = %q", m.Firmware)
	}
	if m.Make != "" || m.Model != "" {
		t.Errorf("Make/Model = %q/%q, want empty", m.Make, m.Model)
	}
}

func TestReadVideoMetaGoProFixture(t *testing.T) {
	m, err := ReadVideoMeta(fixture(t, mt.GoProMP4))
	if err != nil {
		t.Fatal(err)
	}
	checkGoPro(t, m)
}

func TestReadVideoMetaVersion1Boxes(t *testing.T) {
	s := mt.GoProVideo
	s.Version = 1
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	checkGoPro(t, m)
}

func TestReadVideoMetaMakeModel(t *testing.T) {
	s := mt.GoProVideo
	s.Firmware, s.Make, s.Model = "", "Apple", "iPhone 15"
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if m.Make != "Apple" || m.Model != "iPhone 15" {
		t.Errorf("Make/Model = %q/%q", m.Make, m.Model)
	}
}

func TestReadVideoMetaNoCreationTime(t *testing.T) {
	s := mt.GoProVideo
	s.CreationTime = 0
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if !m.CreationTime.IsZero() {
		t.Errorf("CreationTime = %v, want zero", m.CreationTime)
	}
}

func TestReadVideoMetaPreUnixCreationTime(t *testing.T) {
	s := mt.GoProVideo
	s.CreationTime = 1
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if !m.CreationTime.IsZero() {
		t.Errorf("CreationTime = %v, want zero", m.CreationTime)
	}
	s.CreationTime = 2082844800
	m, err = ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if !m.CreationTime.Equal(time.Unix(0, 0)) {
		t.Errorf("CreationTime = %v, want Unix epoch", m.CreationTime)
	}
}

func TestReadVideoMetaAudioOnly(t *testing.T) {
	s := mt.GoProVideo
	s.Width, s.Height = 0, 0
	_, err := ReadVideoMeta(writeMP4(t, s))
	if err == nil || !strings.Contains(err.Error(), "could not extract video dimensions") {
		t.Errorf("err = %v", err)
	}
}

func TestReadVideoMetaNotMP4(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadVideoMeta(writeSourceFile(t, dir, "x.MP4", "first")); err == nil {
		t.Error("want error for non-MP4 content")
	}
	if _, err := ReadVideoMeta(dir + "/missing.MP4"); err == nil {
		t.Error("want error for missing file")
	}
}

// goProMP4 returns mt.MP4(mt.GoProVideo); udta is the last box in moov and
// moov the last in the file, which the byte surgery below relies on.
func goProMP4(t *testing.T) []byte {
	t.Helper()
	b, err := mt.MP4(mt.GoProVideo)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// boxStart returns the offset of the box header (size field) of the first
// box of type typ in b.
func boxStart(t *testing.T, b []byte, typ string) int {
	t.Helper()
	i := bytes.Index(b, []byte(typ))
	if i < 4 {
		t.Fatalf("no %s box", typ)
	}
	return i - 4
}

// growBox adds n to the 32-bit size of the box whose header starts at off.
func growBox(b []byte, off int, n uint32) {
	binary.BigEndian.PutUint32(b[off:], binary.BigEndian.Uint32(b[off:])+n)
}

// checkGoProCore checks what udta problems must never cost: size, frame
// rate, duration and creation time.
func checkGoProCore(t *testing.T, m *VideoMeta) {
	t.Helper()
	if m.Width != 3840 || m.Height != 2160 || m.FrameRate != "29.970" || m.Duration != 12.512 {
		t.Errorf("got %dx%d %q %v", m.Width, m.Height, m.FrameRate, m.Duration)
	}
	if !m.CreationTime.Equal(time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC)) {
		t.Errorf("CreationTime = %v", m.CreationTime)
	}
}

// A QuickTime udta child list may end with a 32-bit zero. go-mp4's walker
// accepts that only in files whose ftyp lists the "qt  " brand; the
// synthesized file's brand is mp41.
func TestReadVideoMetaUdtaZeroTerminator(t *testing.T) {
	b := goProMP4(t)
	if bytes.Contains(b[:boxStart(t, b, "moov")], []byte("qt  ")) {
		t.Fatal("ftyp lists the qt brand; the test needs a non-QuickTime file")
	}
	udta, moov := boxStart(t, b, "udta"), boxStart(t, b, "moov")
	b = append(b, 0, 0, 0, 0)
	growBox(b, udta, 4)
	growBox(b, moov, 4)
	m, err := ReadVideoMeta(writeBytes(t, t.TempDir(), "x.MP4", b))
	if err != nil {
		t.Fatal(err)
	}
	checkGoPro(t, m)
}

// A udta child whose size overruns udta is malformed advisory data: it is
// skipped, and everything outside udta is still returned.
func TestReadVideoMetaUdtaChildOverruns(t *testing.T) {
	b := goProMP4(t)
	binary.BigEndian.PutUint32(b[boxStart(t, b, "FIRM"):], 0xFFFF)
	m, err := ReadVideoMeta(writeBytes(t, t.TempDir(), "x.MP4", b))
	if err != nil {
		t.Fatal(err)
	}
	checkGoProCore(t, m)
	if m.Firmware != "" {
		t.Errorf("Firmware = %q, want empty", m.Firmware)
	}
}

// A udta child with size < 8 (here 4, followed by more data) stops the udta
// walk; it is not an error.
func TestReadVideoMetaUdtaBadChildSize(t *testing.T) {
	b := goProMP4(t)
	udta, moov := boxStart(t, b, "udta"), boxStart(t, b, "moov")
	b = append(b, 0, 0, 0, 4, 0xDE, 0xAD, 0xBE, 0xEF, 0, 0, 0, 0) // size 4, then junk
	growBox(b, udta, 12)
	growBox(b, moov, 12)
	m, err := ReadVideoMeta(writeBytes(t, t.TempDir(), "x.MP4", b))
	if err != nil {
		t.Fatal(err)
	}
	checkGoPro(t, m)
}

// Cutting the file anywhere inside moov is a structural error, udta included.
func TestReadVideoMetaTruncated(t *testing.T) {
	b := goProMP4(t)
	moovLen := len(b) - boxStart(t, b, "moov")
	dir := t.TempDir()
	for n := 1; n < moovLen; n += 3 {
		if _, err := ReadVideoMeta(writeBytes(t, dir, "x.MP4", b[:len(b)-n])); err == nil {
			t.Errorf("cut %d of %d moov bytes: want error", n, moovLen)
		}
	}
}

func TestReadVideoMetaZeroMediaTimescale(t *testing.T) {
	s := mt.GoProVideo
	s.MediaTimescale = 0
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameRate != "" {
		t.Errorf("FrameRate = %q, want empty", m.FrameRate)
	}
}

func TestReadVideoMetaZeroSampleDelta(t *testing.T) {
	s := mt.GoProVideo
	s.SampleDelta = 0
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if m.FrameRate != "" {
		t.Errorf("FrameRate = %q, want empty", m.FrameRate)
	}
}

func TestReadVideoMetaZeroTimescale(t *testing.T) {
	s := mt.GoProVideo
	s.Timescale = 0
	m, err := ReadVideoMeta(writeMP4(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if m.Duration != 0 {
		t.Errorf("Duration = %v, want 0", m.Duration)
	}
}

func TestReadVideoMetaPanic(t *testing.T) {
	orig := readBoxStructure
	readBoxStructure = func(io.ReadSeeker, mp4.ReadHandler, ...interface{}) ([]interface{}, error) {
		panic("bad box")
	}
	t.Cleanup(func() { readBoxStructure = orig })
	b, err := os.ReadFile(fixture(t, mt.GoProMP4))
	if err != nil {
		t.Fatal(err)
	}
	path := writeBytes(t, t.TempDir(), "x.MP4", b)
	m, err := ReadVideoMeta(path)
	if m != nil {
		t.Errorf("VideoMeta = %+v, want nil", m)
	}
	want := "video " + path + ": the parser panicked: bad box"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
	if err := os.Remove(path); err != nil {
		t.Errorf("file was left open: %v", err)
	}
}
