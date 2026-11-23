package media

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDayStartsAt is the time of day at which a new staging folder date
// begins.
const DefaultDayStartsAt = 4 * time.Hour

// FolderDate returns the staging folder date (YYYY-MM-DD) for a capture at
// t: t's calendar date in t's own location, or the previous date when t's
// time of day is before dayStartsAt. Only folders use the cutoff; filenames
// keep the real time (§13).
func FolderDate(t time.Time, dayStartsAt time.Duration) string {
	y, m, d := t.Date()
	sinceMidnight := time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
	if sinceMidnight < dayStartsAt {
		d-- // time.Date normalizes day 0 to the last day of the previous month
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}

// DestFileName builds the destination file name for a capture at t. Videos
// with meta also carry resolution, frame rate and whole seconds of duration.
func DestFileName(t time.Time, kind Kind, meta *VideoMeta, ext string) string {
	stamp := t.Format("2006-01-02 15.04.05")
	if kind == KindVideo && meta != nil {
		return fmt.Sprintf("%s - %s - %s - %dsec%s", stamp, meta.Resolution, meta.FrameRate, int(meta.Duration), ext)
	}
	return stamp + ext
}

// SuffixName inserts "-n" before the extension of name to resolve a name
// collision; n == 0 returns name unchanged.
func SuffixName(name string, n int) string {
	if n == 0 {
		return name
	}
	ext := filepath.Ext(name)
	return fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n, ext)
}
