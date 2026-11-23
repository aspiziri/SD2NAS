package ingest

import (
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

func TestCheckClock(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	wall := time.Date(2025, 5, 31, 1, 30, 12, 0, time.UTC)
	gps := time.Date(2025, 5, 30, 23, 30, 12, 0, time.UTC)
	type want struct {
		kind   catalog.ClockWarningKind
		detail string
	}
	const resetDetail = "capture time 2000-01-01 00:00:00 is before 2005: the camera clock was probably reset"
	tests := []struct {
		name string
		c    media.Capture
		want []want
	}{
		{"gps matches offset", media.Capture{Time: wall, Source: media.SourceExif, Offset: "+02:00", GPS: gps}, nil},
		{"gps just inside tolerance", media.Capture{Time: wall, Source: media.SourceExif, Offset: "+02:00", GPS: gps.Add(-4*time.Minute - 59*time.Second)}, nil},
		{"gps ahead", media.Capture{Time: wall, Source: media.SourceExif, Offset: "+02:00", GPS: gps.Add(-5*time.Minute - time.Second)},
			[]want{{catalog.ClockGPSMismatch, "camera clock is 5m0s ahead of GPS time"}}},
		{"gps behind", media.Capture{Time: wall, Source: media.SourceExif, Offset: "+02:00", GPS: gps.Add(time.Hour)},
			[]want{{catalog.ClockGPSMismatch, "camera clock is 1h0m0s behind GPS time"}}},
		{"no offset", media.Capture{Time: wall, Source: media.SourceExif, GPS: gps},
			[]want{{catalog.ClockGPSMismatch, "camera clock is 2h0m0s ahead of GPS time"}}},
		{"garbage offset", media.Capture{Time: wall, Source: media.SourceExif, Offset: "garbage", GPS: gps},
			[]want{{catalog.ClockGPSMismatch, "camera clock is 2h0m0s ahead of GPS time"}}},
		{"future at tolerance", media.Capture{Time: time.Date(2026, 1, 1, 12, 10, 0, 0, time.UTC), Source: media.SourceExif}, nil},
		{"future", media.Capture{Time: time.Date(2026, 1, 1, 12, 10, 1, 0, time.UTC), Source: media.SourceMtime},
			[]want{{catalog.ClockFuture, "capture time 2026-01-01 12:10:01 is after the NAS time 2026-01-01 12:00:00"}}},
		{"reset", media.Capture{Time: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Source: media.SourceExif},
			[]want{{catalog.ClockResetDefault, resetDetail}}},
		{"2005 is fine", media.Capture{Time: time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC), Source: media.SourceExif}, nil},
		{"reset then gps", media.Capture{Time: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Source: media.SourceExif, GPS: time.Date(2025, 5, 30, 0, 0, 0, 0, time.UTC)},
			[]want{{catalog.ClockResetDefault, resetDetail}, {catalog.ClockGPSMismatch, ""}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckClock(tt.c, now)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d warnings %+v, want %d", len(got), got, len(tt.want))
			}
			for i, w := range got {
				if w.Kind != tt.want[i].kind {
					t.Errorf("[%d] kind = %q, want %q", i, w.Kind, tt.want[i].kind)
				}
				if tt.want[i].detail != "" && w.Detail != tt.want[i].detail {
					t.Errorf("[%d] detail = %q, want %q", i, w.Detail, tt.want[i].detail)
				}
				if w.CardPath != "" || w.SHA256 != "" {
					t.Errorf("[%d] CardPath/SHA256 not empty: %+v", i, w)
				}
			}
		})
	}
}
