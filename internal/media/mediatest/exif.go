// Package mediatest synthesizes tiny camera files (EXIF JPEG/ARW, MP4) for
// tests, and lists the fixtures committed under testdata/. It must not
// import internal/media.
package mediatest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
	"sort"
)

// ExifSpec is the metadata a synthesized file carries.
type ExifSpec struct {
	Make, Model        string
	DateTimeOriginal   string       // "2006:01:02 15:04:05"
	OffsetTimeOriginal string       // "+02:00"
	GPSDate            string       // "2006:01:02"; "" omits the whole GPS IFD
	GPSTime            [3][2]uint32 // hour, minute, second as {numerator, denominator}
}

type entry struct {
	tag   uint16
	typ   uint16 // 1 BYTE, 2 ASCII, 4 LONG, 5 RATIONAL
	count uint32
	val   []byte // already in the TIFF byte order
}

func ascii(tag uint16, s string) entry {
	b := append([]byte(s), 0)
	return entry{tag, 2, uint32(len(b)), b}
}

// TIFF returns a TIFF structure (header, IFD0, Exif IFD, optional GPS IFD)
// in the given byte order: the bytes a JPEG's APP1 carries after "Exif\0\0".
// binary.Write into a bytes.Buffer cannot fail, so its error is not checked.
func TIFF(order binary.ByteOrder, s ExifSpec) []byte {
	var ifd0, exifIFD, gps []entry
	if s.Make != "" {
		ifd0 = append(ifd0, ascii(0x010F, s.Make))
	}
	if s.Model != "" {
		ifd0 = append(ifd0, ascii(0x0110, s.Model))
	}
	if s.DateTimeOriginal != "" {
		exifIFD = append(exifIFD, ascii(0x9003, s.DateTimeOriginal))
	}
	if s.OffsetTimeOriginal != "" {
		exifIFD = append(exifIFD, ascii(0x9011, s.OffsetTimeOriginal))
	}
	if s.GPSDate != "" {
		rats := make([]byte, 24)
		for i, r := range s.GPSTime {
			order.PutUint32(rats[i*8:], r[0])
			order.PutUint32(rats[i*8+4:], r[1])
		}
		gps = append(gps,
			entry{0x0000, 1, 4, []byte{2, 3, 0, 0}}, // GPSVersionID
			entry{0x0007, 5, 3, rats},               // GPSTimeStamp
			ascii(0x001D, s.GPSDate),                // GPSDateStamp
		)
	}

	// Layout: header | IFD0 | Exif IFD | GPS IFD, each IFD directly followed
	// by the values that don't fit in its 4-byte value fields. Pointer
	// values fit inline, so they are patched once the offsets are known
	// without changing any IFD's size.
	exifPtr, gpsPtr := make([]byte, 4), make([]byte, 4)
	ifd0 = append(ifd0, entry{0x8769, 4, 1, exifPtr})
	if len(gps) > 0 {
		ifd0 = append(ifd0, entry{0x8825, 4, 1, gpsPtr})
	}
	exifOff := 8 + ifdSize(ifd0)
	order.PutUint32(exifPtr, uint32(exifOff))
	order.PutUint32(gpsPtr, uint32(exifOff+ifdSize(exifIFD)))

	var buf bytes.Buffer
	if order == binary.LittleEndian {
		buf.WriteString("II")
	} else {
		buf.WriteString("MM")
	}
	binary.Write(&buf, order, uint16(42))
	binary.Write(&buf, order, uint32(8)) // IFD0 offset
	writeIFD(&buf, order, ifd0)
	writeIFD(&buf, order, exifIFD)
	if len(gps) > 0 {
		writeIFD(&buf, order, gps)
	}
	return buf.Bytes()
}

// ifdSize is the encoded size of an IFD: count, entries, next-IFD offset,
// then out-of-line values padded to even length.
func ifdSize(es []entry) int {
	n := 2 + 12*len(es) + 4
	for _, e := range es {
		if len(e.val) > 4 {
			n += len(e.val) + len(e.val)%2
		}
	}
	return n
}

// writeIFD appends one IFD at the buffer's current offset followed by its
// out-of-line values. Entries are sorted by tag, as TIFF requires.
func writeIFD(buf *bytes.Buffer, order binary.ByteOrder, es []entry) {
	sort.Slice(es, func(i, j int) bool { return es[i].tag < es[j].tag })
	dataOff := buf.Len() + 2 + 12*len(es) + 4
	var data []byte
	binary.Write(buf, order, uint16(len(es)))
	for _, e := range es {
		binary.Write(buf, order, e.tag)
		binary.Write(buf, order, e.typ)
		binary.Write(buf, order, e.count)
		if len(e.val) <= 4 {
			v := make([]byte, 4)
			copy(v, e.val)
			buf.Write(v)
			continue
		}
		binary.Write(buf, order, uint32(dataOff+len(data)))
		data = append(data, e.val...)
		if len(e.val)%2 == 1 {
			data = append(data, 0)
		}
	}
	binary.Write(buf, order, uint32(0)) // no next IFD
	buf.Write(data)
}

// JPEG returns a decodable 8x8 grey JPEG carrying s in an APP1 EXIF segment
// directly after SOI, where cameras put it.
func JPEG(s ExifSpec) ([]byte, error) {
	img := image.NewGray(image.Rect(0, 0, 8, 8))
	for i := range img.Pix {
		img.Pix[i] = 128
	}
	var enc bytes.Buffer
	if err := jpeg.Encode(&enc, img, &jpeg.Options{Quality: 75}); err != nil {
		return nil, err
	}
	payload := append([]byte("Exif\x00\x00"), TIFF(binary.LittleEndian, s)...)
	var out bytes.Buffer
	out.Write([]byte{0xFF, 0xD8, 0xFF, 0xE1})
	binary.Write(&out, binary.BigEndian, uint16(len(payload)+2))
	out.Write(payload)
	out.Write(enc.Bytes()[2:]) // the encoder's output minus its own SOI
	return out.Bytes(), nil
}

// ARW returns a little-endian TIFF carrying s: the container a Sony ARW
// uses, without image data (SD2NAS only reads the metadata).
func ARW(s ExifSpec) []byte { return TIFF(binary.LittleEndian, s) }
