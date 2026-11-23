package media

import (
	"strings"
	"testing"
	"time"

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
