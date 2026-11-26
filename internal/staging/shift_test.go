package staging

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// wall is a capture time (catalog.CaptureLayout) as the catalog reads it back.
func wall(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation(catalog.CaptureLayout, s, time.Local)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func secs(n int64) *int64 { return &n }

// shift runs a shift that must succeed.
func (e *opsEnv) shift(t *testing.T, n int64, shas ...string) ShiftResult {
	t.Helper()
	res, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: shas, Seconds: secs(n)})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// wantFiles fails unless the files below the photos root are exactly want.
func (e *opsEnv) wantFiles(t *testing.T, want ...string) {
	t.Helper()
	slices.Sort(want)
	if got := e.files(t); !slices.Equal(got, want) {
		t.Errorf("files = %q\nwant    %q", got, want)
	}
}

// wantAsset fails unless sha is staged at rel with capture (CaptureLayout, "" = none).
func (e *opsEnv) wantAsset(t *testing.T, sha, rel, capture string) {
	t.Helper()
	a := e.asset(t, sha)
	got := ""
	if !a.CaptureTime.IsZero() {
		got = a.CaptureTime.Format(catalog.CaptureLayout)
	}
	if a.RelPath != rel || got != capture || a.State != catalog.StateStaged {
		t.Errorf("asset %s = (%q, %q, %s), want (%q, %q, staged)", sha[:6], a.RelPath, got, a.State, rel, capture)
	}
}

// wantContent fails unless the file at rel has content's hash.
func (e *opsEnv) wantContent(t *testing.T, rel, content string) {
	t.Helper()
	got, err := media.HashFile(filepath.Join(e.photos, filepath.FromSlash(rel)))
	if err != nil {
		t.Error(err)
		return
	}
	if got != shaOf(content) {
		t.Errorf("%s changed: hash %s, want %s", rel, got, shaOf(content))
	}
}

func (e *opsEnv) wantDates(t *testing.T, id int64, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	got := e.folderRow(t, id).Dates
	if got == nil {
		got = []string{}
	}
	if !slices.Equal(got, want) {
		t.Errorf("folder %d dates = %v, want %v", id, got, want)
	}
}

func TestShiftWall(t *testing.T) {
	la, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}
	// 02:30 on that day doesn't exist in Los Angeles (the clocks jump from
	// 02:00 to 03:00), but a capture time is a wall clock: it stays 02:30.
	got := shiftWall(time.Date(2025, 3, 9, 1, 30, 0, 0, la), time.Hour)
	if s := got.Format(catalog.CaptureLayout); s != "2025-03-09T02:30:00" {
		t.Errorf("DST gap: %s, want 2025-03-09T02:30:00", s)
	}
	got = shiftWall(time.Date(2025, 6, 1, 10, 0, 0, 0, la), -24*time.Hour)
	if s := got.Format(catalog.CaptureLayout); s != "2025-05-31T10:00:00" {
		t.Errorf("month end: %s, want 2025-05-31T10:00:00", s)
	}
	got = shiftWall(time.Date(2025, 11, 2, 0, 30, 0, 0, la), 2*time.Hour)
	if s := got.Format(catalog.CaptureLayout); s != "2025-11-02T02:30:00" {
		t.Errorf("DST end: %s, want 2025-11-02T02:30:00 (two wall-clock hours)", s)
	}
}

func TestShiftedName(t *testing.T) {
	old := time.Date(2025, 5, 30, 14, 2, 11, 0, time.UTC)
	later := old.Add(time.Hour)
	for _, c := range []struct {
		name, want string
		ok         bool
	}{
		{"2025-05-30 14.02.11.JPG", "2025-05-30 15.02.11.JPG", true},
		{"2025-05-30 14.02.11-1.JPG", "2025-05-30 15.02.11-1.JPG", true},
		{"2025-05-30 14.02.11 - 3840x2160 - 25p - 12sec.MP4", "2025-05-30 15.02.11 - 3840x2160 - 25p - 12sec.MP4", true},
		{"IMG_0001.JPG", "IMG_0001.JPG", false},
		{"2025-05-30 09.00.00.JPG", "2025-05-30 09.00.00.JPG", false}, // a stamp, but not the capture time
	} {
		got, ok := shiftedName(c.name, old, later)
		if got != c.want || ok != c.ok {
			t.Errorf("shiftedName(%q) = %q, %v; want %q, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

// TestShiftRuleCases checks the rule table the web preview also reads
// (Decision 27): same inputs, same outputs.
func TestShiftRuleCases(t *testing.T) {
	raw, err := os.ReadFile("../../web/src/lib/testdata/rules-cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases struct {
		ShiftWall []struct {
			Capture string `json:"capture"`
			Seconds int64  `json:"seconds"`
			Want    string `json:"want"`
		} `json:"shiftWall"`
		ShiftedName []struct {
			Name    string `json:"name"`
			Capture string `json:"capture"`
			Seconds int64  `json:"seconds"`
			Want    string `json:"want"`
		} `json:"shiftedName"`
		FolderDate []struct {
			Capture     string `json:"capture"`
			DayStartsAt string `json:"dayStartsAt"`
			Want        string `json:"want"`
		} `json:"folderDate"`
		WholeName []struct {
			Name  string   `json:"name"`
			Dates []string `json:"dates"`
			Want  string   `json:"want"`
		} `json:"wholeName"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases.ShiftWall) < 2 || len(cases.ShiftedName) < 5 || len(cases.FolderDate) < 3 || len(cases.WholeName) < 2 {
		t.Fatalf("too few cases: %d shiftWall, %d shiftedName, %d folderDate, %d wholeName",
			len(cases.ShiftWall), len(cases.ShiftedName), len(cases.FolderDate), len(cases.WholeName))
	}
	parse := func(s string) time.Time {
		v, err := time.Parse(catalog.CaptureLayout, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range cases.ShiftWall {
		if got := shiftWall(parse(c.Capture), time.Duration(c.Seconds)*time.Second).Format(catalog.CaptureLayout); got != c.Want {
			t.Errorf("shiftWall(%s, %d) = %s, want %s", c.Capture, c.Seconds, got, c.Want)
		}
	}
	for _, c := range cases.ShiftedName {
		old := parse(c.Capture)
		if got, _ := shiftedName(c.Name, old, shiftWall(old, time.Duration(c.Seconds)*time.Second)); got != c.Want {
			t.Errorf("shiftedName(%q, %s, %d) = %q, want %q", c.Name, c.Capture, c.Seconds, got, c.Want)
		}
	}
	for _, c := range cases.FolderDate {
		hm, err := time.Parse("15:04", c.DayStartsAt)
		if err != nil {
			t.Fatal(err)
		}
		cutoff := time.Duration(hm.Hour())*time.Hour + time.Duration(hm.Minute())*time.Minute
		if got := media.FolderDate(parse(c.Capture), cutoff); got != c.Want {
			t.Errorf("FolderDate(%s, %s) = %s, want %s", c.Capture, c.DayStartsAt, got, c.Want)
		}
	}
	for _, c := range cases.WholeName {
		if got := wholeFolderName(c.Name, c.Dates); got != c.Want {
			t.Errorf("wholeFolderName(%q, %v) = %q, want %q", c.Name, c.Dates, got, c.Want)
		}
	}
}

func TestShiftRenamesKeepsBytes(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30", "2025-05-30")
	a := e.stage(t, f, "2025-05-30 14.02.11.JPG", "a", wall(t, "2025-05-30T14:02:11"))
	b := e.stage(t, f, "2025-05-30 15.00.00.JPG", "b", wall(t, "2025-05-30T15:00:00"))
	in, err := e.db.CreateIngest(t.Context(), "/cards/X", "X", "", opsNow)
	if err != nil {
		t.Fatal(err)
	}
	err = e.db.AddClockWarnings(t.Context(), in.ID, []catalog.ClockWarning{
		{CardPath: "DCIM/a.JPG", SHA256: a, Kind: catalog.ClockFuture, Detail: "x"},
		{CardPath: "DCIM/b.JPG", SHA256: b, Kind: catalog.ClockResetDefault, Detail: "y"},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := e.svc.Status().Version

	res := e.shift(t, 3600, a, b)
	if res.Shifted != 2 || res.Renamed != 2 || res.Moved != 0 || res.Skipped != 0 || res.Removed || res.Folder.ID != f.ID || res.WantedName != "" {
		t.Errorf("result = %+v", res)
	}
	e.wantFiles(t, "Staging/2025-05-30/2025-05-30 15.02.11.JPG", "Staging/2025-05-30/2025-05-30 16.00.00.JPG")
	e.wantContent(t, "Staging/2025-05-30/2025-05-30 15.02.11.JPG", "a")
	e.wantContent(t, "Staging/2025-05-30/2025-05-30 16.00.00.JPG", "b")
	e.wantAsset(t, a, "Staging/2025-05-30/2025-05-30 15.02.11.JPG", "2025-05-30T15:02:11")
	e.wantAsset(t, b, "Staging/2025-05-30/2025-05-30 16.00.00.JPG", "2025-05-30T16:00:00")
	if !e.asset(t, a).UpdatedAt.Equal(opsNow) {
		t.Errorf("updated_at = %v, want %v", e.asset(t, a).UpdatedAt, opsNow)
	}
	if got := e.svc.Status().Version; got <= v {
		t.Errorf("version = %d, did not go up from %d", got, v)
	}
	ws, err := e.db.ClockWarningsUnder(t.Context(), Dir+"/", catalog.StateStaged)
	if err != nil || len(ws) != 0 {
		t.Errorf("clock warnings left: %+v, %v", ws, err)
	}
	e.wantDates(t, f.ID, "2025-05-30")
	e.lockFree(t, f.ID)
}

func TestShiftWholeFolderFollowsDate(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	a := e.stage(t, f, "2025-05-30 10.00.00.JPG", "a", wall(t, "2025-05-30T10:00:00"))
	raw := e.stage(t, f, "Capture/2025-05-30 10.00.00.ARW", "a-raw", wall(t, "2025-05-30T10:00:00"))
	res := e.shift(t, 86400, a, raw)
	if res.Folder.ID != f.ID || res.Folder.Name != "2025-05-31 - Trip" || res.Shifted != 2 || res.Moved != 0 || res.WantedName != "" {
		t.Errorf("result = %+v", res)
	}
	if row := e.folderRow(t, f.ID); row.Name != "2025-05-31 - Trip" {
		t.Errorf("folder name = %q", row.Name)
	}
	e.wantDates(t, f.ID, "2025-05-31")
	e.wantFiles(t, "Staging/2025-05-31 - Trip/2025-05-31 10.00.00.JPG", "Staging/2025-05-31 - Trip/Capture/2025-05-31 10.00.00.ARW")
	e.wantAsset(t, raw, "Staging/2025-05-31 - Trip/Capture/2025-05-31 10.00.00.ARW", "2025-05-31T10:00:00")
	e.lockFree(t, f.ID)
	e.nameKeyFree(t, "2025-05-31 - Trip")
}

func TestShiftWholeFolderNameTaken(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30")
	taken := e.folder(t, "2025-05-31 - Trip", "2025-05-31")
	a := e.stage(t, f, "2025-05-30 10.00.00.JPG", "a", wall(t, "2025-05-30T10:00:00"))
	res := e.shift(t, 86400, a)
	if res.Folder.Name != "2025-05-30 - Trip" || res.WantedName != "2025-05-31 - Trip" || res.Shifted != 1 || res.Moved != 0 {
		t.Errorf("result = %+v", res)
	}
	e.wantDates(t, f.ID, "2025-05-31")
	e.wantDates(t, taken.ID, "2025-05-31")
	e.wantFiles(t, "Staging/2025-05-30 - Trip/2025-05-31 10.00.00.JPG")
	e.wantAsset(t, a, "Staging/2025-05-30 - Trip/2025-05-31 10.00.00.JPG", "2025-05-31T10:00:00")
}

func TestShiftWholeUntitled(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "Untitled", "2025-05-30")
	a := e.stage(t, f, "2025-05-30 10.00.00.JPG", "a", wall(t, "2025-05-30T10:00:00"))
	res := e.shift(t, 86400, a)
	if res.Folder.Name != "Untitled" || res.WantedName != "" || res.Shifted != 1 {
		t.Errorf("result = %+v", res)
	}
	e.wantDates(t, f.ID, "2025-05-31")
	e.wantFiles(t, "Staging/Untitled/2025-05-31 10.00.00.JPG")
}

func TestShiftPartialStays(t *testing.T) {
	e := newOpsEnv(t)
	f := e.folder(t, "2025-05-30 - Trip", "2025-05-30", "2025-05-31")
	x := e.stage(t, f, "2025-05-30 09.00.00.JPG", "x", wall(t, "2025-05-30T09:00:00"))
	e.stage(t, f, "2025-05-31 10.00.00.JPG", "y", wall(t, "2025-05-31T10:00:00"))
	res := e.shift(t, 86400, x)
	if res.Shifted != 1 || res.Renamed != 1 || res.Moved != 0 || res.Folder.Name != "2025-05-30 - Trip" || res.WantedName != "" {
		t.Errorf("result = %+v", res)
	}
	e.wantFiles(t, "Staging/2025-05-30 - Trip/2025-05-31 09.00.00.JPG", "Staging/2025-05-30 - Trip/2025-05-31 10.00.00.JPG")
	e.wantDates(t, f.ID, "2025-05-30", "2025-05-31")
}

func TestShiftPartialRefilesExisting(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31 - Beach", "2025-05-31")
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	xr := e.stage(t, a, "Capture/2025-05-30 10.00.00.ARW", "x-raw", wall(t, "2025-05-30T10:00:00"))
	y := e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	res := e.shift(t, 86400, x, xr)
	if res.Shifted != 2 || res.Renamed != 2 || res.Moved != 2 || res.Removed || res.Folder.ID != a.ID {
		t.Errorf("result = %+v", res)
	}
	e.wantFiles(t, "Staging/2025-05-30/2025-05-30 11.00.00.JPG",
		"Staging/2025-05-31 - Beach/2025-05-31 10.00.00.JPG", "Staging/2025-05-31 - Beach/Capture/2025-05-31 10.00.00.ARW")
	e.wantAsset(t, xr, "Staging/2025-05-31 - Beach/Capture/2025-05-31 10.00.00.ARW", "2025-05-31T10:00:00")
	e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 11.00.00.JPG", "2025-05-30T11:00:00")
	e.wantDates(t, a.ID, "2025-05-30")
	e.wantDates(t, b.ID, "2025-05-31")
	e.lockFree(t, a.ID, b.ID)
}

func TestShiftPartialNewFolder(t *testing.T) {
	e := newOpsEnv(t)
	created := time.Date(2025, 6, 3, 8, 0, 0, 0, time.UTC)
	a, err := e.db.CreateStagingFolder(t.Context(), "2025-05-30 - Trip", []string{"2025-05-30"}, created)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.photos, Dir, a.Name), 0o777); err != nil {
		t.Fatal(err)
	}
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	res := e.shift(t, 3*86400, x)
	if res.Moved != 1 || res.Shifted != 1 || res.Folder.ID != a.ID {
		t.Errorf("result = %+v", res)
	}
	e.wantFiles(t, "Staging/2025-05-30 - Trip/2025-05-30 11.00.00.JPG", "Staging/2025-06-02/2025-06-02 10.00.00.JPG")
	e.wantAsset(t, x, "Staging/2025-06-02/2025-06-02 10.00.00.JPG", "2025-06-02T10:00:00")
	all, err := e.db.StagingFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var nf catalog.StagingFolder
	for _, r := range all {
		if r.Name == "2025-06-02" {
			nf = r
		}
	}
	if nf.ID == 0 || !slices.Equal(nf.Dates, []string{"2025-06-02"}) || !nf.CreatedAt.Equal(created) {
		t.Errorf("new folder = %+v, want 2025-06-02 created %v", nf, created)
	}
	e.wantDates(t, a.ID, "2025-05-30")
	e.lockFree(t, a.ID, nf.ID)
	e.nameKeyFree(t, "2025-06-02")
}

func TestShiftDayCutoff(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "2025-05-31 03.30.00.JPG", "x", wall(t, "2025-05-31T03:30:00"))
	e.stage(t, a, "2025-05-30 12.00.00.JPG", "y", wall(t, "2025-05-30T12:00:00"))
	if res := e.shift(t, 20*60, x); res.Moved != 0 {
		t.Errorf("03:50 is before the 04:00 cutoff, so it stays: %+v", res)
	}
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-31 03.50.00.JPG", "2025-05-31T03:50:00")
	if res := e.shift(t, 20*60, x); res.Moved != 1 {
		t.Errorf("04:10 is after the cutoff, so it moves: %+v", res)
	}
	e.wantAsset(t, x, "Staging/2025-05-31/2025-05-31 04.10.00.JPG", "2025-05-31T04:10:00")
}

func TestShiftBurst(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "2025-05-30 12.00.05.JPG", "x", wall(t, "2025-05-30T12:00:05"))
	x1 := e.stage(t, a, "2025-05-30 12.00.05-1.JPG", "x1", wall(t, "2025-05-30T12:00:05"))
	e.shift(t, 60, x, x1)
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 12.01.05.JPG", "2025-05-30T12:01:05")
	e.wantAsset(t, x1, "Staging/2025-05-30/2025-05-30 12.01.05-1.JPG", "2025-05-30T12:01:05")
	e.wantContent(t, "Staging/2025-05-30/2025-05-30 12.01.05-1.JPG", "x1")
}

func TestShiftOrder(t *testing.T) {
	t.Run("forward", func(t *testing.T) {
		e := newOpsEnv(t)
		a := e.folder(t, "2025-05-30", "2025-05-30")
		x := e.stage(t, a, "2025-05-30 12.00.00.JPG", "x", wall(t, "2025-05-30T12:00:00"))
		y := e.stage(t, a, "2025-05-30 12.00.05.JPG", "y", wall(t, "2025-05-30T12:00:05"))
		e.shift(t, 5, x, y)
		e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 12.00.05.JPG", "2025-05-30T12:00:05")
		e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 12.00.10.JPG", "2025-05-30T12:00:10")
	})
	t.Run("back", func(t *testing.T) {
		e := newOpsEnv(t)
		a := e.folder(t, "2025-05-30", "2025-05-30")
		x := e.stage(t, a, "2025-05-30 12.00.05.JPG", "x", wall(t, "2025-05-30T12:00:05"))
		y := e.stage(t, a, "2025-05-30 12.00.10.JPG", "y", wall(t, "2025-05-30T12:00:10"))
		e.shift(t, -5, x, y)
		e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 12.00.00.JPG", "2025-05-30T12:00:00")
		e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 12.00.05.JPG", "2025-05-30T12:00:05")
	})
}

func TestShiftCollision(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "2025-05-30 12.00.00.JPG", "x", wall(t, "2025-05-30T12:00:00"))
	e.write(t, "Staging/2025-05-30/2025-05-30 13.00.00.JPG", "someone else's")
	res := e.shift(t, 3600, x)
	if res.Renamed != 1 {
		t.Errorf("result = %+v", res)
	}
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 13.00.00-1.JPG", "2025-05-30T13:00:00")
	e.wantContent(t, "Staging/2025-05-30/2025-05-30 13.00.00.JPG", "someone else's")
	e.wantContent(t, "Staging/2025-05-30/2025-05-30 13.00.00-1.JPG", "x")
}

func TestShiftNoStamp(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "IMG_0001.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	res := e.shift(t, 3600, x)
	if res.Shifted != 1 || res.Renamed != 0 {
		t.Errorf("result = %+v", res)
	}
	e.wantAsset(t, x, "Staging/2025-05-30/IMG_0001.JPG", "2025-05-30T11:00:00")
}

func TestShiftNoCapture(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "IMG_0001.JPG", "x", time.Time{})
	y := e.stage(t, a, "2025-05-30 10.00.00.JPG", "y", wall(t, "2025-05-30T10:00:00"))
	res := e.shift(t, 3600, x, y)
	if res.Skipped != 1 || res.Shifted != 1 {
		t.Errorf("result = %+v", res)
	}
	e.wantAsset(t, x, "Staging/2025-05-30/IMG_0001.JPG", "")
	e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 11.00.00.JPG", "2025-05-30T11:00:00")
}

func TestShiftRefusals(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "1970-01-01", "1970-01-01")
	c := e.folder(t, "2099-12-31", "2099-12-31")
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	y := e.stage(t, b, "1970-01-01 00.30.00.JPG", "y", wall(t, "1970-01-01T00:30:00"))
	z := e.stage(t, c, "2099-12-31 23.30.00.JPG", "z", wall(t, "2099-12-31T23:30:00"))
	arch := e.stage(t, a, "2025-05-30 11.00.00.JPG", "arch", wall(t, "2025-05-30T11:00:00"))
	if err := e.db.ArchiveAsset(t.Context(), arch, "Archive/2025-05-30/2025-05-30 11.00.00.JPG", opsNow); err != nil {
		t.Fatal(err)
	}
	before := e.files(t)
	for _, c := range []struct {
		name    string
		shas    []string
		seconds *int64
		want    string
	}{
		{"no seconds", []string{x}, nil, "Say how far to shift: seconds is required"},
		{"zero", []string{x}, secs(0), "Shift by at least one second"},
		{"too far", []string{x}, secs(MaxShift + 1), "Shift by at most 100 years"},
		{"too far back", []string{x}, secs(-MaxShift - 1), "Shift by at most 100 years"},
		{"two folders", []string{x, y}, secs(60), "Shift files from one folder at a time"},
		{"archived", []string{arch}, secs(60), "is not in staging (it is archived)"},
		{"no files", nil, secs(60), "no files given"},
		{"1969", []string{y}, secs(-3600), "Shifting by that much would put 1970-01-01 00.30.00.JPG outside 1970–2099"},
		{"2100", []string{z}, secs(3600), "Shifting by that much would put 2099-12-31 23.30.00.JPG outside 1970–2099"},
	} {
		_, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: c.shas, Seconds: c.seconds})
		if !errors.Is(err, ErrBadRequest) || !strings.HasSuffix(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want ErrBadRequest ending %q", c.name, err, c.want)
		}
	}
	if after := e.files(t); !slices.Equal(after, before) {
		t.Errorf("files changed: %v, want %v", after, before)
	}
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "2025-05-30T10:00:00")
	e.wantAsset(t, y, "Staging/1970-01-01/1970-01-01 00.30.00.JPG", "1970-01-01T00:30:00")
	e.lockFree(t, a.ID, b.ID, c.ID)
}

func TestShiftBusy(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	b := e.folder(t, "2025-05-31 - Beach", "2025-05-31")
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	before := e.files(t)
	try := func(what string, n int64, want int64) {
		t.Helper()
		_, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: []string{x}, Seconds: secs(n)})
		var busy *BusyError
		if !errors.As(err, &busy) || busy.ID != want {
			t.Errorf("%s: err = %v, want a *BusyError for folder %d", what, err, want)
		}
		if after := e.files(t); !slices.Equal(after, before) {
			t.Errorf("%s: files changed: %v", what, after)
		}
		e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "2025-05-30T10:00:00")
	}

	release, err := e.locks.TryLock(HolderIngest, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	try("source held", 3600, a.ID)
	release()

	release, err = e.locks.TryLock(HolderIngest, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	try("re-file target held", 86400, b.ID)
	release()

	release, err = e.locks.TryLockSet(HolderIngest, LockSet{Names: []string{Dir + "/2025-06-02"}})
	if err != nil {
		t.Fatal(err)
	}
	try("new folder's name held", 3*86400, 0)
	release()
	e.lockFree(t, a.ID, b.ID)
	if all, _ := e.db.StagingFolders(t.Context()); len(all) != 2 {
		t.Errorf("folders = %+v, want the two", all)
	}
}

func TestShiftPartialFailure(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	y := e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	calls := 0
	e.svc.rename = func(o, n string) error {
		calls++
		if calls == 2 {
			return errors.New("disk on fire")
		}
		return os.Rename(o, n)
	}
	_, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: []string{x, y}, Seconds: secs(3600)})
	if err == nil || !strings.HasPrefix(err.Error(), "shifted 1 of 2 files: ") || !strings.Contains(err.Error(), "disk on fire") {
		t.Fatalf("err = %v", err)
	}
	// Latest first: y went, x did not.
	e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 12.00.00.JPG", "2025-05-30T12:00:00")
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "2025-05-30T10:00:00")
	e.wantFiles(t, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "Staging/2025-05-30/2025-05-30 12.00.00.JPG")
	e.lockFree(t, a.ID)
}

func TestShiftCatalogFailureMovesBack(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	y := e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	calls := 0
	e.svc.shiftAsset = func(ctx context.Context, sha, rel string, capture, at time.Time) error {
		calls++
		if calls == 2 {
			return errors.New("catalog on fire")
		}
		return e.db.ShiftAsset(ctx, sha, rel, capture, at)
	}
	_, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: []string{x, y}, Seconds: secs(3600)})
	if err == nil || !strings.HasPrefix(err.Error(), "shifted 1 of 2 files: ") || !strings.Contains(err.Error(), "catalog on fire") {
		t.Fatalf("err = %v", err)
	}
	e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 12.00.00.JPG", "2025-05-30T12:00:00")
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "2025-05-30T10:00:00")
	if !e.asset(t, x).UpdatedAt.Equal(opsNow) {
		t.Errorf("x's row changed: %+v", e.asset(t, x))
	}
	e.wantFiles(t, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "Staging/2025-05-30/2025-05-30 12.00.00.JPG")
	e.wantContent(t, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "x")
	e.lockFree(t, a.ID)
}

func TestShiftRefilesIntoFolderNamedDate(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	named := e.folder(t, "2025-06-02") // no dates
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	res := e.shift(t, 3*86400, x)
	if res.Moved != 1 {
		t.Errorf("result = %+v", res)
	}
	e.wantAsset(t, x, "Staging/2025-06-02/2025-06-02 10.00.00.JPG", "2025-06-02T10:00:00")
	e.wantDates(t, named.ID, "2025-06-02")
	if all, _ := e.db.StagingFolders(t.Context()); len(all) != 2 {
		t.Errorf("folders = %+v, want the two", all)
	}
}

func TestShiftStopsOnClose(t *testing.T) {
	e := newOpsEnv(t)
	a := e.folder(t, "2025-05-30", "2025-05-30")
	x := e.stage(t, a, "2025-05-30 10.00.00.JPG", "x", wall(t, "2025-05-30T10:00:00"))
	y := e.stage(t, a, "2025-05-30 11.00.00.JPG", "y", wall(t, "2025-05-30T11:00:00"))
	z := e.stage(t, a, "2025-05-30 12.00.00.JPG", "z", wall(t, "2025-05-30T12:00:00"))
	entered := make(chan struct{})
	closed := make(chan struct{})
	calls := 0
	e.svc.shiftAsset = func(ctx context.Context, sha, rel string, capture, at time.Time) error {
		calls++
		if calls == 1 {
			close(entered)
			<-e.svc.bgCtx.Done() // Close has begun
			// Close must still be waiting for this shift.
			for deadline := time.Now().Add(50 * time.Millisecond); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
				select {
				case <-closed:
					t.Error("Close returned while a shift was running")
				default:
				}
			}
		}
		return e.db.ShiftAsset(ctx, sha, rel, capture, at)
	}
	go func() {
		<-entered
		e.svc.Close()
		close(closed)
	}()
	_, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: []string{x, y, z}, Seconds: secs(3600)})
	if err == nil || err.Error() != "shifted 1 of 3 files: SD2NAS is shutting down" {
		t.Errorf("err = %v", err)
	}
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}
	e.wantAsset(t, z, "Staging/2025-05-30/2025-05-30 13.00.00.JPG", "2025-05-30T13:00:00")
	e.wantAsset(t, y, "Staging/2025-05-30/2025-05-30 11.00.00.JPG", "2025-05-30T11:00:00")
	e.wantAsset(t, x, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "2025-05-30T10:00:00")
	e.wantFiles(t, "Staging/2025-05-30/2025-05-30 10.00.00.JPG", "Staging/2025-05-30/2025-05-30 11.00.00.JPG",
		"Staging/2025-05-30/2025-05-30 13.00.00.JPG")
	if _, err := e.svc.Shift(t.Context(), ShiftRequest{SHAs: []string{x}, Seconds: secs(60)}); !errors.Is(err, ErrClosed) {
		t.Errorf("after Close: err = %v, want ErrClosed", err)
	}
}
