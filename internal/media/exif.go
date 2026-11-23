package media

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"time"

	"github.com/rwcarlsen/goexif/exif"
	"github.com/rwcarlsen/goexif/tiff"
)

// ExifData is the part of a file's EXIF block the importer uses.
type ExifData struct {
	DateTime   time.Time // DateTimeOriginal, else DateTime, as a wall clock in time.Local; zero when absent or unparsable
	Camera     string    // "Make Model" when both are present
	OffsetTime string    // OffsetTimeOriginal as written (e.g. "+02:00"); "" when absent
	GPSTime    time.Time // GPSDateStamp + GPSTimeStamp in UTC; zero when absent or malformed
	// Warning holds problems that did not stop the read but that a caller
	// may want to report: goexif's non-critical decode error (a broken
	// sub-IFD, partial data was used), a DateTime[Original] string that is
	// present but doesn't parse, and an OffsetTimeOriginal that can't be read. nil when there were none. A merely absent
	// DateTime is not a warning. Errors are joined with errors.Join.
	Warning error
}

// ReadExif decodes the EXIF block of the file at path.
func ReadExif(path string) (_ *ExifData, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	x, err := exif.Decode(f)
	if err != nil && (x == nil || exif.IsCriticalError(err)) {
		return nil, fmt.Errorf("exif %s: %w", path, err)
	}
	// A non-critical error means a sub-IFD failed; use what was decoded and
	// keep the error in Warning.
	d := &ExifData{GPSTime: gpsTime(x), Warning: err}
	if dt, err := x.DateTime(); err == nil {
		d.DateTime = dt
	} else if !isTagNotPresent(err) {
		// The tag is there but unusable (a malformed string): say so.
		d.Warning = errors.Join(d.Warning, fmt.Errorf("DateTime: %w", err))
	}
	mk, okMake := strTag(x, exif.Make)
	md, okModel := strTag(x, exif.Model)
	if okMake && okModel {
		d.Camera = mk + " " + md
	}
	// OffsetTimeOriginal is an advisory time-zone hint: a failure to read it
	// is a warning, not a failed read.
	if d.OffsetTime, err = offsetTimeOriginal(x); err != nil {
		d.OffsetTime = ""
		d.Warning = errors.Join(d.Warning, fmt.Errorf("OffsetTimeOriginal: %w", err))
	}
	return d, nil
}

// FileModDate returns the file's modification time.
func FileModDate(path string) (time.Time, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return fi.ModTime(), nil
}

// isTagNotPresent reports whether err is goexif's "tag absent" error, which
// is not a problem: many files carry no capture time.
func isTagNotPresent(err error) bool {
	var np exif.TagNotPresentError
	return errors.As(err, &np)
}

func strTag(x *exif.Exif, name exif.FieldName) (string, bool) {
	t, err := x.Get(name)
	if err != nil {
		return "", false
	}
	s, err := t.StringVal()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

const tagOffsetTimeOriginal = 0x9011

// offsetTimeOriginal reads tag 0x9011 from the Exif sub-IFD by ID: goexif's
// field table predates EXIF 2.31. "" when there is no Exif IFD or no tag.
func offsetTimeOriginal(x *exif.Exif) (string, error) {
	ptr, err := x.Get(exif.ExifIFDPointer)
	if err != nil {
		return "", nil // no Exif sub-IFD
	}
	off, err := ptr.Int64(0)
	if err != nil {
		return "", fmt.Errorf("ExifIFDPointer: %w", err)
	}
	r := bytes.NewReader(x.Raw)
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek to Exif IFD: %w", err)
	}
	dir, _, err := tiff.DecodeDir(r, x.Tiff.Order)
	if err != nil {
		return "", fmt.Errorf("decode Exif IFD: %w", err)
	}
	for _, t := range dir.Tags {
		if t.Id != tagOffsetTimeOriginal {
			continue
		}
		s, err := t.StringVal()
		if err != nil {
			return "", err // ReadExif adds the "OffsetTimeOriginal: " prefix
		}
		return strings.TrimSpace(s), nil
	}
	return "", nil
}

// gpsTime combines GPSDateStamp and GPSTimeStamp (UTC). GPS is advisory
// (§12), so anything missing or malformed gives the zero time.
func gpsTime(x *exif.Exif) time.Time {
	ds, ok := strTag(x, exif.GPSDateStamp)
	if !ok {
		return time.Time{}
	}
	day, err := time.Parse("2006:01:02", ds)
	if err != nil {
		return time.Time{}
	}
	ts, err := x.Get(exif.GPSTimeStamp)
	if err != nil || ts.Count < 3 {
		return time.Time{}
	}
	var d time.Duration
	// Each unit has its own bound on the whole part: hour < 24, minute < 60,
	// second < 61 (a leap second).
	units := []struct {
		unit  time.Duration
		limit int64
	}{{time.Hour, 24}, {time.Minute, 60}, {time.Second, 61}}
	for i, u := range units {
		unit := u.unit
		num, den, err := ts.Rat2(i)
		if err != nil || den <= 0 || num < 0 || num/den >= u.limit {
			return time.Time{}
		}
		d += time.Duration(num/den)*unit + time.Duration(math.Round(float64(num%den)*float64(unit)/float64(den)))
	}
	return day.Add(d)
}
