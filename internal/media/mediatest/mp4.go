package mediatest

import (
	"errors"
	"io"

	"github.com/abema/go-mp4"
)

// VideoSpec describes a synthesized MP4: ftyp plus a moov, no media data.
type VideoSpec struct {
	Version        uint8  // mvhd/tkhd/mdhd box version, 0 or 1
	CreationTime   uint64 // mvhd creation time, seconds since 1904-01-01 UTC
	Timescale      uint32 // mvhd timescale (ticks per second)
	Duration       uint64 // mvhd duration in Timescale ticks
	Width, Height  uint32 // video tkhd size in pixels; 0 omits the video trak
	MediaTimescale uint32 // video mdhd timescale
	SampleCount    uint32 // video stts: one entry of SampleCount samples…
	SampleDelta    uint32 // …each SampleDelta MediaTimescale ticks long
	Audio          bool   // add an audio trak (tkhd 0x0, mdhd 48000, stts 1×1024) BEFORE the video trak
	Make, Model    string // udta ©mak / ©mod QuickTime text atoms; "" omits
	Firmware       string // udta FIRM raw bytes (GoPro); "" omits
}

// seekBuf is an in-memory io.WriteSeeker: go-mp4's writer seeks back to patch
// box sizes, and the standard library has no such buffer.
type seekBuf struct {
	b   []byte
	pos int
}

func (s *seekBuf) Write(p []byte) (int, error) {
	if need := s.pos + len(p); need > len(s.b) {
		s.b = append(s.b, make([]byte, need-len(s.b))...)
	}
	copy(s.b[s.pos:], p)
	s.pos += len(p)
	return len(p), nil
}

func (s *seekBuf) Seek(off int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = off
	case io.SeekCurrent:
		abs = int64(s.pos) + off
	case io.SeekEnd:
		abs = int64(len(s.b)) + off
	default:
		return 0, errors.New("seekBuf: bad whence")
	}
	if abs < 0 {
		return 0, errors.New("seekBuf: negative position")
	}
	s.pos = int(abs)
	return abs, nil
}

// MP4 returns an MP4 with ftyp and a moov built from s, and no media data.
func MP4(s VideoSpec) ([]byte, error) {
	buf := &seekBuf{}
	w := mp4.NewWriter(buf)
	var err error // first error wins; later calls become no-ops
	box := func(t mp4.BoxType, payload mp4.IImmutableBox, children func()) {
		if err != nil {
			return
		}
		if _, err = w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			return
		}
		if payload != nil {
			if _, err = mp4.Marshal(w, payload, mp4.Context{}); err != nil {
				return
			}
		}
		if children != nil {
			children()
		}
		if err == nil {
			_, err = w.EndBox()
		}
	}
	raw := func(t mp4.BoxType, data []byte) {
		if err != nil {
			return
		}
		if _, err = w.StartBox(&mp4.BoxInfo{Type: t}); err != nil {
			return
		}
		if _, err = w.Write(data); err != nil {
			return
		}
		_, err = w.EndBox()
	}
	bt := mp4.StrToBoxType
	track := func(width, height, timescale, count, delta uint32) {
		box(bt("trak"), nil, func() {
			box(bt("tkhd"), &mp4.Tkhd{FullBox: mp4.FullBox{Version: s.Version}, Width: width << 16, Height: height << 16}, nil)
			box(bt("mdia"), nil, func() {
				box(bt("mdhd"), &mp4.Mdhd{FullBox: mp4.FullBox{Version: s.Version}, Timescale: timescale}, nil)
				box(bt("minf"), nil, func() {
					box(bt("stbl"), nil, func() {
						box(bt("stts"), &mp4.Stts{EntryCount: 1, Entries: []mp4.SttsEntry{{SampleCount: count, SampleDelta: delta}}}, nil)
					})
				})
			})
		})
	}

	box(bt("ftyp"), &mp4.Ftyp{MajorBrand: [4]byte{'m', 'p', '4', '1'}, CompatibleBrands: []mp4.CompatibleBrandElem{{CompatibleBrand: [4]byte{'m', 'p', '4', '1'}}}}, nil)
	box(bt("moov"), nil, func() {
		mvhd := &mp4.Mvhd{FullBox: mp4.FullBox{Version: s.Version}, Timescale: s.Timescale, Rate: 0x00010000, Volume: 0x0100, NextTrackID: 3}
		if s.Version == 1 {
			mvhd.CreationTimeV1, mvhd.DurationV1 = s.CreationTime, s.Duration
		} else {
			mvhd.CreationTimeV0, mvhd.DurationV0 = uint32(s.CreationTime), uint32(s.Duration)
		}
		box(bt("mvhd"), mvhd, nil)
		if s.Audio {
			track(0, 0, 48000, 1, 1024)
		}
		if s.Width > 0 && s.Height > 0 {
			track(s.Width, s.Height, s.MediaTimescale, s.SampleCount, s.SampleDelta)
		}
		if s.Make != "" || s.Model != "" || s.Firmware != "" {
			box(bt("udta"), nil, func() {
				if s.Make != "" {
					raw(mp4.BoxType{0xA9, 'm', 'a', 'k'}, qtText(s.Make))
				}
				if s.Model != "" {
					raw(mp4.BoxType{0xA9, 'm', 'o', 'd'}, qtText(s.Model))
				}
				if s.Firmware != "" {
					raw(bt("FIRM"), []byte(s.Firmware))
				}
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return buf.b, nil
}

// qtText encodes a QuickTime user-data text atom: 16-bit length, 16-bit
// language code (0x55C4 = "und"), then the text.
func qtText(s string) []byte {
	b := []byte{byte(len(s) >> 8), byte(len(s)), 0x55, 0xC4}
	return append(b, s...)
}
