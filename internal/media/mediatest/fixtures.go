package mediatest

// SonyExif: a RAW+JPEG pair shot at 01:30:12 local time on a camera set to
// UTC+2 with GPS: its folder date under the default 04:00 cutoff is 2025-05-30.
var SonyExif = ExifSpec{
	Make: "SONY", Model: "ILCE-7M4",
	DateTimeOriginal: "2025:05:31 01:30:12", OffsetTimeOriginal: "+02:00",
	GPSDate: "2025:05:30", GPSTime: [3][2]uint32{{23, 1}, {30, 1}, {12, 1}},
}

// Fixture paths are slash-separated and relative to the repo's testdata/.
const (
	SonyJPEG = "cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.JPG"
	SonyARW  = "cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.ARW"
)

// Fixture is one committed test file and the generator that produces it.
type Fixture struct {
	Path  string                 // relative to testdata/, slash-separated
	Build func() ([]byte, error) // produces the file's bytes
}

// Fixtures lists every committed fixture.
func Fixtures() []Fixture {
	return []Fixture{
		{SonyJPEG, func() ([]byte, error) { return JPEG(SonyExif) }},
		{SonyARW, func() ([]byte, error) { return ARW(SonyExif), nil }},
	}
}
