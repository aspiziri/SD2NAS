package mediatest

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"image/jpeg"
	"testing"
)

func TestTIFFBytes(t *testing.T) {
	got := hex.EncodeToString(TIFF(binary.LittleEndian, ExifSpec{DateTimeOriginal: "2025:05:31 01:30:12"}))
	want := "49492a0008000000010069870400010000001a00000000000000010003900200140000002c00000000000000323032353a30353a33312030313a33303a313200"
	if got != want {
		t.Errorf("minimal TIFF:\n got %s\nwant %s", got, want)
	}
	got = hex.EncodeToString(TIFF(binary.LittleEndian, SonyExif))
	want = "49492a000800000004000f010200050000003e00000010010200090000004400000069870400010000004e00000025880400010000008800000000000000534f4e590000494c43452d374d340000020003900200140000006c00000011900200070000008000000000000000323032353a30353a33312030313a33303a3132002b30323a3030000003000000010004000000020300000700050003000000b20000001d0002000b000000ca0000000000000017000000010000001e000000010000000c00000001000000323032353a30353a33300000"
	if got != want {
		t.Errorf("SonyExif TIFF:\n got %s\nwant %s", got, want)
	}
}

func TestTIFFBigEndianHeader(t *testing.T) {
	b := TIFF(binary.BigEndian, SonyExif)
	if !bytes.HasPrefix(b, []byte("MM\x00\x2a\x00\x00\x00\x08")) {
		t.Errorf("big-endian header = % x", b[:8])
	}
}

func TestJPEGStructure(t *testing.T) {
	b, err := JPEG(SonyExif)
	if err != nil {
		t.Fatal(err)
	}
	tiff := TIFF(binary.LittleEndian, SonyExif)
	if !bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF, 0xE1}) {
		t.Fatalf("does not start with SOI + APP1: % x", b[:4])
	}
	if n := int(binary.BigEndian.Uint16(b[4:6])); n != len(tiff)+8 {
		t.Errorf("APP1 length = %d, want %d", n, len(tiff)+8)
	}
	if string(b[6:12]) != "Exif\x00\x00" || !bytes.Equal(b[12:12+len(tiff)], tiff) {
		t.Error("APP1 payload is not Exif\\0\\0 + TIFF(LittleEndian, spec)")
	}
	img, err := jpeg.Decode(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("not a decodable JPEG: %v", err)
	}
	if img.Bounds().Dx() != 8 || img.Bounds().Dy() != 8 {
		t.Errorf("bounds = %v", img.Bounds())
	}
}

func TestARWIsLittleEndianTIFF(t *testing.T) {
	if !bytes.Equal(ARW(SonyExif), TIFF(binary.LittleEndian, SonyExif)) {
		t.Error("ARW differs from TIFF(LittleEndian, spec)")
	}
}
