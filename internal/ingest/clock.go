package ingest

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

const (
	// FutureTolerance is how far past the NAS clock a capture time may be
	// before it is flagged.
	FutureTolerance = 10 * time.Minute
	// GPSTolerance is how far the camera clock may differ from the GPS time.
	GPSTolerance = 5 * time.Minute
	// ResetBeforeYear: capture times before this year look like a camera
	// whose clock was reset to its factory default.
	ResetBeforeYear = 2005
)

const clockTimeLayout = "2006-01-02 15:04:05"

var offsetPattern = regexp.MustCompile(`^[+-]([01][0-9]|2[0-3]):[0-5][0-9]$`)

// captureInstant returns the moment c was shot. EXIF wall-clock times are
// reinterpreted in the file's own UTC offset when it has a valid one.
func captureInstant(c media.Capture) time.Time {
	if c.Source != media.SourceExif || !offsetPattern.MatchString(c.Offset) {
		return c.Time
	}
	h, _ := strconv.Atoi(c.Offset[1:3])
	m, _ := strconv.Atoi(c.Offset[4:6])
	secs := h*3600 + m*60
	if c.Offset[0] == '-' {
		secs = -secs
	}
	t := c.Time
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(),
		time.FixedZone("", secs))
}

// CheckClock returns the §12 warnings for one new file, in the order
// future, reset_default, gps_mismatch. CardPath and SHA256 are left for
// the caller to fill.
func CheckClock(c media.Capture, now time.Time) []catalog.ClockWarning {
	var out []catalog.ClockWarning
	instant := captureInstant(c)
	if instant.After(now.Add(FutureTolerance)) {
		out = append(out, catalog.ClockWarning{
			Kind: catalog.ClockFuture,
			Detail: fmt.Sprintf("capture time %s is after the NAS time %s",
				instant.In(now.Location()).Format(clockTimeLayout), now.Format(clockTimeLayout)),
		})
	}
	if c.Time.Year() < ResetBeforeYear {
		out = append(out, catalog.ClockWarning{
			Kind: catalog.ClockResetDefault,
			Detail: fmt.Sprintf("capture time %s is before %d: the camera clock was probably reset",
				c.Time.Format(clockTimeLayout), ResetBeforeYear),
		})
	}
	if !c.GPS.IsZero() {
		d := instant.Sub(c.GPS)
		dir := "ahead of"
		if d < 0 {
			d, dir = -d, "behind"
		}
		if d > GPSTolerance {
			out = append(out, catalog.ClockWarning{
				Kind:   catalog.ClockGPSMismatch,
				Detail: fmt.Sprintf("camera clock is %s %s GPS time", d.Round(time.Minute), dir),
			})
		}
	}
	return out
}
