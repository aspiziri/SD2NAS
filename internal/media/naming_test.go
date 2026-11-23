package media

import (
	"testing"
	"time"
)

func TestFolderDate(t *testing.T) {
	z := time.FixedZone("CEST", 2*3600)
	cases := []struct {
		t    time.Time
		cut  time.Duration
		want string
	}{
		{time.Date(2025, 5, 31, 1, 30, 12, 0, z), 4 * time.Hour, "2025-05-30"}, // §7: a night out stays together
		{time.Date(2025, 5, 31, 4, 0, 0, 0, z), 4 * time.Hour, "2025-05-31"},   // exactly at the cutoff: new day
		{time.Date(2025, 5, 31, 3, 59, 59, 999999999, z), 4 * time.Hour, "2025-05-30"},
		{time.Date(2025, 5, 31, 15, 4, 5, 0, z), 4 * time.Hour, "2025-05-31"},
		{time.Date(2025, 3, 1, 2, 0, 0, 0, z), 4 * time.Hour, "2025-02-28"},
		{time.Date(2024, 3, 1, 2, 0, 0, 0, z), 4 * time.Hour, "2024-02-29"},
		{time.Date(2025, 1, 1, 0, 30, 0, 0, z), 4 * time.Hour, "2024-12-31"},
		{time.Date(2025, 5, 31, 0, 0, 0, 0, z), 0, "2025-05-31"},
	}
	for _, c := range cases {
		if got := FolderDate(c.t, c.cut); got != c.want {
			t.Errorf("FolderDate(%v, %v) = %s, want %s", c.t, c.cut, got, c.want)
		}
	}
	if DefaultDayStartsAt != 4*time.Hour {
		t.Errorf("DefaultDayStartsAt = %v", DefaultDayStartsAt)
	}
}

func TestDestFileName(t *testing.T) {
	ts := time.Date(2025, 5, 31, 1, 30, 12, 0, time.FixedZone("CEST", 2*3600))
	full := &VideoMeta{Resolution: "3840w", FrameRate: "29.970", Duration: 12.512}
	cases := []struct {
		kind Kind
		meta *VideoMeta
		ext  string
		want string
	}{
		{KindImage, nil, ".JPG", "2025-05-31 01.30.12.JPG"},
		{KindRaw, nil, ".ARW", "2025-05-31 01.30.12.ARW"},
		{KindVideo, full, ".MP4", "2025-05-31 01.30.12 - 3840w - 29.970 - 12sec.MP4"},
		{KindVideo, nil, ".MP4", "2025-05-31 01.30.12.MP4"},
		{KindVideo, &VideoMeta{Resolution: "1920w", Duration: 59.999}, ".mov", "2025-05-31 01.30.12 - 1920w -  - 59sec.mov"},
		{KindImage, full, ".JPG", "2025-05-31 01.30.12.JPG"}, // meta only matters for video
		{KindImage, nil, "", "2025-05-31 01.30.12"},
	}
	for _, c := range cases {
		if got := DestFileName(ts, c.kind, c.meta, c.ext); got != c.want {
			t.Errorf("DestFileName(%s, %+v, %q) = %q, want %q", c.kind, c.meta, c.ext, got, c.want)
		}
	}
}

func TestSuffixName(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want string
	}{
		{"2025-05-31 01.30.12.JPG", 0, "2025-05-31 01.30.12.JPG"},
		{"2025-05-31 01.30.12.JPG", 1, "2025-05-31 01.30.12-1.JPG"},
		{"2025-05-31 01.30.12 - 3840w - 29.970 - 12sec.MP4", 2, "2025-05-31 01.30.12 - 3840w - 29.970 - 12sec-2.MP4"},
		{"abc", 1, "abc-1"},
	}
	for _, c := range cases {
		if got := SuffixName(c.name, c.n); got != c.want {
			t.Errorf("SuffixName(%q, %d) = %q, want %q", c.name, c.n, got, c.want)
		}
	}
}
