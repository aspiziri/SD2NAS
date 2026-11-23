package mediatest

import (
	"bytes"
	"testing"

	"github.com/abema/go-mp4"
)

func TestMP4Structure(t *testing.T) {
	b, err := MP4(GoProVideo)
	if err != nil {
		t.Fatal(err)
	}
	if string(b[4:8]) != "ftyp" {
		t.Fatalf("first box = %q, want ftyp", b[4:8])
	}
	traks, err := mp4.ExtractBox(bytes.NewReader(b), nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeTrak()})
	if err != nil || len(traks) != 2 {
		t.Fatalf("traks = %d, %v; want 2 (audio + video)", len(traks), err)
	}
	mv, err := mp4.ExtractBoxWithPayload(bytes.NewReader(b), nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvhd()})
	if err != nil || len(mv) != 1 {
		t.Fatalf("mvhd: %d, %v", len(mv), err)
	}
	if got := mv[0].Payload.(*mp4.Mvhd).GetCreationTime(); got != 3831525012 {
		t.Errorf("mvhd creation = %d", got)
	}
	firm, err := mp4.ExtractBox(bytes.NewReader(b), nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeUdta(), mp4.StrToBoxType("FIRM")})
	if err != nil || len(firm) != 1 {
		t.Errorf("udta/FIRM: %d, %v", len(firm), err)
	}
}
