package ingest

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

func TestDescribe(t *testing.T) {
	type counts = catalog.Counts
	const p = "SONY_A7IV: "
	tests := []struct {
		name    string
		mod     func(*catalog.Ingest)
		level   string
		message string
	}{
		{"done safe", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.SafeToFormat = catalog.StatusDone, counts{Seen: 3, New: 2, InArchive: 1}, true
		}, LevelNormal, p + "2 new, 1 already known. Card safe to format."},
		{"done staging only", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.SafeToFormat = catalog.StatusDone, counts{Seen: 2, InStaging: 2}, true
		}, LevelNormal, p + "0 new, 2 already known. Card safe to format."},
		{"forgotten is not safe to format", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.SafeToFormat = catalog.StatusDone, counts{Seen: 3, New: 2, InArchive: 1}, true
			in.ForgottenAt = time.Date(2026, 3, 9, 14, 0, 0, 0, time.UTC)
		}, LevelWarning, p + "forgotten on 2026-03-09: the files it brought in are no longer tracked, so inserting the card imports them again. Do not format the card."},
		{"done failed", func(in *catalog.Ingest) {
			in.Status, in.Counts = catalog.StatusDone, counts{Seen: 3, New: 1, InStaging: 1, Failed: 1}
		}, LevelWarning, p + "1 new, 1 already known, 1 failed. Do not format the card."},
		{"done unsupported", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.UnsupportedExts = catalog.StatusDone, counts{Seen: 1, New: 1, Unsupported: 4}, []string{".360", ".mkv"}
		}, LevelWarning, p + "1 new, 0 already known, 4 files not imported (.360, .mkv). Do not format the card."},
		{"one unsupported without extension", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.UnsupportedExts = catalog.StatusDone, counts{Seen: 1, New: 1, Unsupported: 1}, []string{""}
		}, LevelWarning, p + "1 new, 0 already known, 1 file not imported (no extension). Do not format the card."},
		{"many unsupported extensions", func(in *catalog.Ingest) {
			in.Status, in.Counts = catalog.StatusDone, counts{Seen: 1, New: 1, Unsupported: 7}
			in.UnsupportedExts = []string{".360", ".a", ".b", ".c", ".d", ".mkv", ""}
		}, LevelWarning, p + "1 new, 0 already known, 7 files not imported (.360, .a, .b, .c, .d and 2 more). Do not format the card."},
		{"unsupported extensions unknown", func(in *catalog.Ingest) {
			in.Status, in.Counts = catalog.StatusDone, counts{Seen: 1, New: 1, Unsupported: 2}
		}, LevelWarning, p + "1 new, 0 already known, 2 files not imported. Do not format the card."},
		{"unsupported extension with control characters", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.UnsupportedExts = catalog.StatusDone, counts{Seen: 1, New: 1, Unsupported: 1}, []string{".a\nb"}
		}, LevelWarning, p + "1 new, 0 already known, 1 file not imported (.a b). Do not format the card."},
		{"one clock warning", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.SafeToFormat, in.ClockWarnings = catalog.StatusDone, counts{Seen: 2, New: 2}, true, 1
		}, LevelWarning, p + "2 new, 0 already known. Card safe to format. Check the camera clock: 1 file has suspicious capture times."},
		{"three clock warnings", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.SafeToFormat, in.ClockWarnings = catalog.StatusDone, counts{Seen: 2, New: 2}, true, 3
		}, LevelWarning, p + "2 new, 0 already known. Card safe to format. Check the camera clock: 3 files have suspicious capture times."},
		{"empty card", func(in *catalog.Ingest) { in.Status = catalog.StatusDone },
			LevelWarning, p + "no photos or videos found. Do not format the card."},
		{"unsupported only", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.UnsupportedExts = catalog.StatusDone, counts{Unsupported: 2}, []string{".crm"}
		}, LevelWarning, p + "0 new, 0 already known, 2 files not imported (.crm). Do not format the card."},
		{"missing", func(in *catalog.Ingest) {
			in.Status, in.Counts = catalog.StatusDone, counts{Seen: 42, InArchive: 40, Missing: 2}
		}, LevelWarning, p + "0 new, 40 already known, 2 files' archive copies are missing. Do not format the card."},
		{"one missing", func(in *catalog.Ingest) {
			in.Status, in.Counts = catalog.StatusDone, counts{Seen: 2, New: 1, Missing: 1}
		}, LevelWarning, p + "1 new, 0 already known, 1 file's archive copy is missing. Do not format the card."},
		{"failed then missing then unsupported", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.UnsupportedExts = catalog.StatusDone, counts{Seen: 4, New: 1, InArchive: 1, Failed: 1, Missing: 2, Unsupported: 1}, []string{".mkv"}
		}, LevelWarning, p + "1 new, 1 already known, 1 failed, 2 files' archive copies are missing, 1 file not imported (.mkv). Do not format the card."},
		{"refused", func(in *catalog.Ingest) {
			in.Status = catalog.StatusRefused
			in.Error = "staging would drop below the 50.0 GB free-space reserve (2 new files need 0.1 GB, 40.0 GB free)"
		}, LevelAlert, p + "not imported, staging would drop below the 50.0 GB free-space reserve (2 new files need 0.1 GB, 40.0 GB free). Card left mounted."},
		{"failed with new", func(in *catalog.Ingest) {
			in.Status, in.Counts, in.Error = catalog.StatusFailed, counts{New: 5}, "card removed during import"
		}, LevelAlert, p + "import failed: card removed during import. 5 new copied before it stopped. Card left mounted."},
		{"failed no new", func(in *catalog.Ingest) {
			in.Status, in.Error = catalog.StatusFailed, "scan card: boom"
		}, LevelAlert, p + "import failed: scan card: boom. Card left mounted."},
		{"cancelled with new", func(in *catalog.Ingest) {
			in.Status, in.Counts = catalog.StatusCancelled, counts{New: 1}
		}, LevelAlert, p + "import cancelled because SD2NAS stopped. 1 new copied. Card left mounted."},
		{"cancelled no new", func(in *catalog.Ingest) { in.Status = catalog.StatusCancelled },
			LevelAlert, p + "import cancelled because SD2NAS stopped. Card left mounted."},
		{"queued", func(in *catalog.Ingest) { in.Status = catalog.StatusQueued },
			LevelNormal, p + "waiting for another import to finish."},
		{"running", func(in *catalog.Ingest) { in.Status = catalog.StatusRunning },
			LevelNormal, p + "importing."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := catalog.Ingest{CardLabel: "SONY_A7IV", CardPath: "/cards/SONY_A7IV"}
			tt.mod(&in)
			level, msg := Describe(in)
			if level != tt.level || msg != tt.message {
				t.Errorf("Describe = %q, %q\nwant       %q, %q", level, msg, tt.level, tt.message)
			}
			if strings.ContainsAny(msg, "\r\n") {
				t.Errorf("message contains a line break: %q", msg)
			}
		})
	}
}

func TestDescribeOneLine(t *testing.T) {
	in := catalog.Ingest{CardLabel: "A\tB", Status: catalog.StatusFailed, Error: "a\nb"}
	_, msg := Describe(in)
	if !strings.HasPrefix(msg, "A B: import failed: a b.") {
		t.Errorf("message = %q", msg)
	}
	if strings.ContainsAny(msg, "\r\n\t") {
		t.Errorf("message contains control characters: %q", msg)
	}
}

func TestCardName(t *testing.T) {
	tests := []struct {
		name string
		in   catalog.Ingest
		want string
	}{
		{"path", catalog.Ingest{CardPath: "/cards/GOPRO"}, "GOPRO"},
		{"os path", catalog.Ingest{CardPath: filepath.Join(t.TempDir(), "cards", "GOPRO")}, "GOPRO"},
		{"neither", catalog.Ingest{}, "card"},
		{"control characters", catalog.Ingest{CardLabel: "  SONY \x00"}, "SONY"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CardName(tt.in); got != tt.want {
				t.Errorf("CardName = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestFormatGB(t *testing.T) {
	for _, tt := range []struct {
		in   uint64
		want string
	}{{0, "0.0 GB"}, {50e9, "50.0 GB"}, {812, "0.0 GB"}, {123_456_789_012, "123.5 GB"}} {
		if got := formatGB(tt.in); got != tt.want {
			t.Errorf("formatGB(%d) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
