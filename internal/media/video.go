package media

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/abema/go-mp4"
)

// VideoMeta is the container metadata of a video file. ReadVideoMeta fills it.
type VideoMeta struct {
	Duration     float64 // seconds: mvhd duration / mvhd timescale
	Width        int     // first video track, pixels
	Height       int
	FrameRate    string    // fmt "%.3f", e.g. "29.970"; "" when unknown
	Resolution   string    // fmt "%dw" of Width, e.g. "3840w"
	CreationTime time.Time // mvhd creation time in UTC; zero when absent (values before the Unix epoch count as absent)
	Make         string    // udta ©mak; "" when absent
	Model        string    // udta ©mod; "" when absent
	Firmware     string    // udta FIRM (GoPro); "" when absent
}

// mp4EpochOffset is the number of seconds from 1904-01-01 (the MP4 epoch)
// to 1970-01-01 (the Unix epoch).
const mp4EpochOffset = 2082844800

var (
	boxMak  = mp4.BoxType{0xA9, 'm', 'a', 'k'}
	boxMod  = mp4.BoxType{0xA9, 'm', 'o', 'd'}
	boxFirm = mp4.StrToBoxType("FIRM")
)

// ReadVideoMeta reads duration, first-video-track size and frame rate, the
// mvhd creation time, and udta make/model/firmware from an MP4/MOV.
//
// CreationTime is zero (absent) when mvhd's value is below 2082844800 (the
// seconds from 1904-01-01 to the Unix epoch: it includes 0, which many
// cameras write, and would otherwise give a pre-1970 time) or at or above 2^40.
//
// Make, Model and Firmware are advisory metadata: a malformed udta text atom
// (shorter than its declared length) yields an empty string rather than an
// error.
func ReadVideoMeta(path string) (*VideoMeta, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open video file: %w", err)
	}
	defer file.Close()

	meta := &VideoMeta{}
	var videoDuration, creation uint64
	var videoTimeScale, currentMediaTimeScale uint32
	var currentTrackIsVideo bool

	_, err = mp4.ReadBoxStructure(file, func(h *mp4.ReadHandle) (interface{}, error) {
		// Children of udta: QuickTime text atoms and GoPro's FIRM.
		if len(h.Path) >= 2 && h.Path[len(h.Path)-2] == mp4.BoxTypeUdta() {
			switch h.BoxInfo.Type {
			case boxMak, boxMod, boxFirm:
				var b bytes.Buffer
				if _, err := h.ReadData(&b); err != nil {
					return nil, err
				}
				switch h.BoxInfo.Type {
				case boxMak:
					meta.Make = quickTimeText(b.Bytes())
				case boxMod:
					meta.Model = quickTimeText(b.Bytes())
				case boxFirm:
					meta.Firmware = strings.TrimRight(b.String(), "\x00 ")
				}
			}
			return nil, nil
		}
		switch h.BoxInfo.Type.String() {
		case "moov", "mdia", "minf", "stbl", "udta":
			_, err := h.Expand()
			return nil, err
		case "trak": // reset per-track state before expanding
			currentTrackIsVideo = false
			currentMediaTimeScale = 0
			_, err := h.Expand()
			return nil, err
		case "mvhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			mvhd := box.(*mp4.Mvhd)
			videoDuration = mvhd.GetDuration()
			videoTimeScale = mvhd.Timescale
			creation = mvhd.GetCreationTime()
		case "tkhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			tkhd := box.(*mp4.Tkhd)
			width, height := int(tkhd.Width>>16), int(tkhd.Height>>16)
			if width > 0 && height > 0 {
				currentTrackIsVideo = true
				if meta.Width == 0 {
					meta.Width, meta.Height = width, height
					meta.Resolution = fmt.Sprintf("%dw", width)
				}
			}
		case "mdhd":
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			currentMediaTimeScale = box.(*mp4.Mdhd).Timescale
		case "stts":
			if !currentTrackIsVideo || meta.FrameRate != "" {
				return nil, nil
			}
			box, _, err := h.ReadPayload()
			if err != nil {
				return nil, err
			}
			stts := box.(*mp4.Stts)
			if len(stts.Entries) > 0 && currentMediaTimeScale > 0 {
				var samples, delta uint64
				for _, e := range stts.Entries {
					samples += uint64(e.SampleCount)
					delta += uint64(e.SampleCount) * uint64(e.SampleDelta)
				}
				if samples > 0 && delta > 0 {
					fps := float64(currentMediaTimeScale) / (float64(delta) / float64(samples))
					meta.FrameRate = fmt.Sprintf("%.3f", fps)
				}
			}
		}
		return nil, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to parse video metadata: %w", err)
	}
	if videoTimeScale > 0 {
		meta.Duration = float64(videoDuration) / float64(videoTimeScale)
	}
	// Below the Unix epoch (which includes 0, "not set") would name files
	// from garbage; at or above 2^40 s (about year 36,800) is garbage too.
	if creation >= mp4EpochOffset && creation < 1<<40 {
		meta.CreationTime = time.Unix(int64(creation)-mp4EpochOffset, 0).UTC()
	}
	if meta.Width == 0 || meta.Height == 0 {
		return nil, fmt.Errorf("could not extract video dimensions")
	}
	return meta, nil
}

// quickTimeText decodes a QuickTime user-data text atom (16-bit length,
// 16-bit language, text). Optional metadata: a malformed atom gives "".
func quickTimeText(b []byte) string {
	if len(b) < 4 {
		return ""
	}
	n := int(b[0])<<8 | int(b[1])
	if n > len(b)-4 {
		return ""
	}
	return strings.TrimRight(string(b[4:4+n]), "\x00 ")
}
