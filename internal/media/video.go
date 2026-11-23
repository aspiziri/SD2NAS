package media

import "time"

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
