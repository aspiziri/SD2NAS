package media

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

// TimeSource says where a Capture's Time came from.
type TimeSource string

const (
	SourceExif  TimeSource = "exif"
	SourceMvhd  TimeSource = "mvhd"
	SourceMtime TimeSource = "mtime"
)

var (
	ErrNoExifTime = errors.New("media: no DateTimeOriginal or DateTime in EXIF")
	ErrNoMvhdTime = errors.New("media: mvhd has no creation time")
)

// Capture is what CaptureTime learned about when a file was shot.
type Capture struct {
	Time     time.Time  // wall-clock capture time in time.Local; names the file
	Source   TimeSource // where Time came from
	Offset   string     // EXIF OffsetTimeOriginal (e.g. "+02:00"); "" when unknown
	GPS      time.Time  // EXIF GPS timestamp in UTC; zero when absent
	Camera   string     // EXIF "Make Model", or video udta make/model; "" when unknown
	Fallback error      // why a better source than mtime was tried and not used (includes ExifData.Warning when EXIF was read); nil otherwise
}

// canCarryExif reports whether the format can hold EXIF. For formats that
// can't, goexif scans the whole file before failing, so skip straight to
// the modified time.
func canCarryExif(path string, kind Kind) bool {
	if kind == KindVideo {
		return false
	}
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "png", "gif":
		return false
	}
	return true
}

// CaptureTime resolves a file's capture time: EXIF for images and RAW
// (except png/gif, which can't carry it), mvhd for video, else the
// modified time. It errors only when the modified time can't be read.
func CaptureTime(path string, kind Kind) (Capture, error) {
	var c Capture
	if kind == KindVideo {
		meta, err := ReadVideoMeta(path)
		switch {
		case err != nil:
			c.Fallback = fmt.Errorf("video metadata: %w", err)
		case meta.CreationTime.IsZero():
			c.Camera = strings.TrimSpace(meta.Make + " " + meta.Model)
			c.Fallback = ErrNoMvhdTime
		default:
			c.Camera = strings.TrimSpace(meta.Make + " " + meta.Model)
			c.Time = meta.CreationTime.In(time.Local)
			c.Source = SourceMvhd
		}
	}
	if c.Source == "" && canCarryExif(path, kind) {
		x, err := ReadExif(path)
		switch {
		case err != nil:
			c.Fallback = fmt.Errorf("exif: %w", err)
		default:
			c.Offset, c.GPS, c.Camera = x.OffsetTime, x.GPSTime, x.Camera
			if x.DateTime.IsZero() {
				c.Fallback = errors.Join(ErrNoExifTime, x.Warning)
			} else {
				c.Time = x.DateTime
				c.Source = SourceExif
			}
		}
	}
	if c.Source == "" {
		mtime, err := FileModDate(path)
		if err != nil {
			return Capture{}, errors.Join(c.Fallback, err)
		}
		c.Time = mtime
		c.Source = SourceMtime
	}
	return c, nil
}
