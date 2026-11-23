package media

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
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
// Make, Model and Firmware are advisory metadata, read by readUdta rather
// than go-mp4's walker so that a loosely written udta never costs the rest:
// a malformed udta text atom (shorter than its declared length) yields an
// empty string, and a malformed udta child list (a QuickTime 32-bit zero
// terminator, a child smaller than its header or larger than what remains)
// ends the udta read, keeping what was read before it; neither is an error.
// A udta that runs past the end of the file is an error, like any other
// truncation.
func ReadVideoMeta(path string) (_ *VideoMeta, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open video file: %w", err)
	}
	defer func() {
		if cerr := file.Close(); cerr != nil {
			err = errors.Join(err, cerr)
		}
	}()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("failed to stat video file: %w", err)
	}

	meta := &VideoMeta{}
	var videoDuration, creation uint64
	var videoTimeScale, currentMediaTimeScale uint32
	var currentTrackIsVideo bool

	_, err = mp4.ReadBoxStructure(file, func(h *mp4.ReadHandle) (interface{}, error) {
		switch h.BoxInfo.Type.String() {
		case "udta": // not expanded: see readUdta
			return nil, readUdta(file, h.BoxInfo, info.Size(), meta)
		case "moov", "mdia", "minf", "stbl":
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

// maxUdtaAtom bounds the udta child payloads read into memory. Make, model
// and firmware strings are a few bytes; a larger atom is skipped.
const maxUdtaAtom = 64 << 10

// readUdta reads ©mak, ©mod and FIRM from the children of the udta box bi,
// staying within bi's payload. Cameras write udta loosely (QuickTime ends the
// child list with a 32-bit zero, which go-mp4 accepts only in files with the
// "qt  " brand), and it is advisory, so a child header that is smaller than
// 8 bytes or larger than what remains of udta ends the read without an
// error (64-bit child sizes are not supported and end it too). A udta that
// extends past fileSize (a truncated file) and read errors are returned.
func readUdta(r io.ReaderAt, bi mp4.BoxInfo, fileSize int64, meta *VideoMeta) error {
	if fileSize < 0 || bi.Size > uint64(fileSize) || bi.Offset > uint64(fileSize)-bi.Size {
		return fmt.Errorf("udta at offset %d: size %d runs past the end of the file (%d bytes)", bi.Offset, bi.Size, fileSize)
	}
	off, end := int64(bi.Offset+bi.HeaderSize), int64(bi.Offset+bi.Size)
	var hdr [8]byte
	for end-off >= int64(len(hdr)) {
		if _, err := r.ReadAt(hdr[:], off); err != nil {
			return fmt.Errorf("udta child at offset %d: %w", off, err)
		}
		size := int64(binary.BigEndian.Uint32(hdr[:4]))
		if size < int64(len(hdr)) || size > end-off {
			return nil // terminator or malformed child: the rest is unreadable
		}
		typ := mp4.BoxType{hdr[4], hdr[5], hdr[6], hdr[7]}
		if n := size - int64(len(hdr)); (typ == boxMak || typ == boxMod || typ == boxFirm) && n <= maxUdtaAtom {
			b := make([]byte, n)
			if _, err := r.ReadAt(b, off+int64(len(hdr))); err != nil {
				return fmt.Errorf("udta %s at offset %d: %w", typ, off, err)
			}
			switch typ {
			case boxMak:
				meta.Make = quickTimeText(b)
			case boxMod:
				meta.Model = quickTimeText(b)
			case boxFirm:
				meta.Firmware = strings.TrimRight(string(b), "\x00 ")
			}
		}
		off += size
	}
	return nil
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
