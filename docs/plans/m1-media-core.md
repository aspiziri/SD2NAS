# M1 Media Core Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `internal/media`, the pure-function core that classifies, dates, names, hashes and safely copies camera files, ported from CameraImport, plus tiny synthesized JPEG/ARW/MP4 fixtures.

**Architecture:** One package, `internal/media`, made of stateless functions with no service structs and no progress state. Ingest (M2) will call them in this order: `KindOf` → `QuickFingerprint` → `CaptureTime` / `ReadVideoMeta` → `FolderDate` + `DestFileName` + `TypeSubdir` → `FreeName` → `CopyHashed`. A helper package, `internal/media/mediatest`, synthesizes EXIF and MP4 files for tests and writes the committed fixtures under `testdata/`.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24`), `github.com/rwcarlsen/goexif`, `github.com/abema/go-mp4`, standard library.

Spec: docs/PLAN.md

Relevant spec sections: §1 (scope), §4 (layout), §5 (CameraImport → SD2NAS mapping), §6 "Duplicate detection" (QuickFingerprint), §7 step 5 (how ingest will call this package), §12 (clock signals: OffsetTimeOriginal, GPS), §13 (time zones, GoPro, burst shots, day cutoff), §16 M1 checklist.

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`. `go.mod` keeps `go 1.24` and gets no `toolchain` line. Use no API newer than Go 1.24 (e.g. no `sync.WaitGroup.Go`, no `testing.T.Output`). The local toolchain is go1.26 on Windows; CI runs `gofmt -l .`, `go vet ./...` and `go test ./...` on ubuntu.
- M1 touches only `internal/media/`, `internal/media/mediatest/`, `testdata/`, `.gitattributes`, `go.mod`, `go.sum`, and one table row of `docs/PLAN.md` (Task 1). Do not modify `cmd/`, `internal/api/` or `internal/config/`.
- Dependencies: only `github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd` (Task 5) and `github.com/abema/go-mp4 v1.4.1` (Task 6), the versions CameraImport uses. `go-mp4` brings `github.com/google/uuid v1.1.2 // indirect`; that is expected. Nothing else outside the standard library.
- Tests pass on Windows and Linux. Use `path/filepath`. No Unix-only syscalls. Never assert permission bits. Never depend on the value of `time.Local`: build expected values with `time.Local` or a `time.FixedZone`, and compare instants with `Equal`.
- Close every file before returning. Windows cannot delete or rename open files, and `t.TempDir` cleanup fails if one is left open.
- Never silently swallow an error: return it (wrapped with `%w` where context helps), or surface it in a documented field (`Capture.Fallback`). Use `errors.Join` when a cleanup step fails after an earlier error.
- Source (card) files are only ever opened read-only.
- Time layouts, verbatim from CameraImport: filenames `"2006-01-02 15.04.05"`, folder dates `"2006-01-02"`.
- Export only the identifiers listed in each task's **Produces** block. Unexported helpers are free.
- After every task, `gofmt -l .` prints nothing and `go vet ./...` is clean.
- Commit on branch `m1-media-core`. Every commit message ends with a blank line and then `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **Zero or garbage EXIF rationals** (GPS `30/0`, absurd hour values). Manual lenses and cheap GPS loggers write these. Expected: no panic, `GPSTime` is zero, and the capture time is still read. Pinned in Task 5 (`TestReadExifBadGPSRational`).
2. **A stale `.partial` left by a crash or a pulled card.** Expected: `CopyHashed` refuses with `fs.ErrExist` and does not delete a partial it didn't create (M2's startup cleanup owns those). Pinned in Task 3 (`TestCopyHashedStalePartial`).
3. **Burst shots in the same second, and a re-inserted card.** Expected: different content gets `-1`, `-2`; identical content is reported as a duplicate, including when the identical copy sits at a suffixed name. Pinned in Task 3 (`TestFreeNameIdenticalAtSuffix`) and Task 7 (ported tests).
4. **Video with the audio track before the video track.** Expected: the frame rate comes from the video track's `mdhd`/`stts`, never the audio's. Pinned in Task 6 (the GoPro fixture has audio first).
5. **`mvhd` creation time of 0**, which many cameras write. Expected: falls back to the modified time, and `Capture.Fallback` says why. Pinned in Task 7 (`TestCaptureTimeVideoWithoutCreationTime`).

## Decisions

1. **Format lists and subfolders live in `internal/media/formats.go`**, as a `Rules` value with `DefaultRules()`. `internal/config` holds only process flags and env (its own doc comment says user settings will live in the catalog). M2's settings layer builds a `Rules` from stored settings. The §5 table row in `docs/PLAN.md` is updated to say so (Task 1).
2. **Format lists**: CameraImport's lists plus `cr3`, stored in lowercase and matched case-insensitively. Image: jpg, jpeg, png, gif, tif. RAW: arw, cr2, cr3, nef, dng. Video: mp4, avi, mov. Nothing else is added (no `tiff`, `heic`, `hif`). Subfolders: images at the folder root, RAW in `Capture`, video in `Videos`.
3. **Fixtures are synthesized, not real camera files.** Real JPEG/ARW/MP4 files are megabytes to gigabytes. `internal/media/mediatest` builds byte-exact TIFF/EXIF structures, a decodable 8×8 JPEG with an EXIF APP1 segment, an ARW-shaped TIFF, and an MP4 `moov` (mvhd/tkhd/mdhd/stts/udta) written with go-mp4's own `Writer`. The committed fixtures live under `testdata/cards/<LABEL>/DCIM/...`, a card-shaped layout that M2's ingest tests and `go run ./cmd/sd2nas --cards ./testdata/cards` can use directly. They are regenerated with `go test ./internal/media/mediatest -run TestFixtures -update`. There is no byte-for-byte drift check, because `image/jpeg` output may change between Go versions. Instead, `internal/media` tests assert the known metadata values of the committed files. `mediatest` is a normal package, so M2 tests can import it; it must never import `internal/media`, because that would create an import cycle in media's tests.
4. **`ReadExif` is kept but trimmed** to what SD2NAS uses: capture time, camera, `OffsetTimeOriginal` and GPS time. Lens, ISO, aperture, exposure, focal length and dimensions are not ported, because v1 has no consumer for them; they can be re-ported when one appears. This also drops CameraImport's `Rat(0)` calls, which panic on zero denominators.
5. **`OffsetTimeOriginal` (tag 0x9011) is read by tag ID** from the Exif sub-IFD with goexif's `tiff.DecodeDir`, because goexif's field table predates EXIF 2.31 and silently drops the tag. GPS uses goexif's own `GPSDateStamp`/`GPSTimeStamp` fields. GPS is advisory, so a malformed GPS block yields a zero `GPSTime` instead of failing the read.
6. **goexif's non-critical errors are tolerated.** When `exif.Decode` returns partial data with an error for which `exif.IsCriticalError` is false (a broken sub-IFD), the partial data is used. CameraImport discarded it. Critical errors are returned.
7. **`GetCaptureDate` is folded into `CaptureTime`.** `FileModDate` keeps its name but returns `(time.Time, error)` instead of falling back to `time.Now()`.
8. **`CaptureTime` returns a `Capture` struct** with the time, its `Source` (exif / mvhd / mtime), `Offset`, `GPS`, `Camera`, and `Fallback`: the reason a better source was tried and not used. It returns an error only when even the modified time can't be read. PNG and GIF go straight to the modified time with `Fallback == nil`, as CameraImport's `canCarryExif` did.
9. **Time zones** (§13): EXIF wall-clock times are interpreted in `time.Local`, the container's `TZ`. `mvhd` times are seconds since 1904-01-01 UTC, exposed as UTC in `VideoMeta.CreationTime`, and converted to `time.Local` by `CaptureTime`. A value of 0, or one at or above 2^40, means absent. The GoPro "video time is local" setting is later work; M1 only exposes the raw inputs: UTC `CreationTime`, udta `©mak`/`©mod` (QuickTime text atoms) as `Make`/`Model`, and udta `FIRM` as `Firmware`. GoPro writes `FIRM`, not `©mak`.
10. **Video naming is preserved verbatim**, including the odd case of an unknown frame rate, which gives a double space (`" - 3840w -  - 12sec"`). `ReadVideoMeta` still fails when no video track has dimensions, as in CameraImport. One bug is fixed: the frame rate comes from the first video track only, using that track's own `mdhd` timescale. The per-trak reset does this.
11. **`FolderDate`** uses `t`'s wall clock in its own location and returns a `"YYYY-MM-DD"` string. A time exactly at the cutoff belongs to the new day. `DefaultDayStartsAt = 4h`. Validating and parsing the `day_starts_at` setting is M2's job.
12. **Hashes are lowercase hex strings** (`HashFile`, `QuickFingerprint`, `CopyHashed`). M2 converts them to the catalog's BLOBs.
13. **QuickFingerprint encoding**: SHA-256 over the 8-byte big-endian size, then the first `min(64 KiB, size)` bytes, then the last `min(64 KiB, size)` bytes. For files under 128 KiB the two ranges overlap, and both are hashed anyway.
14. **CopyHashed's no-overwrite rule** is checked with `Lstat(dst)` before the partial is created and again right before the rename. `os.Rename` replaces existing files on both Linux and Windows. A hard-link-then-unlink trick was rejected because hard-link support on Unraid's shfs is a tunable. The remaining race window is covered by the §8 folder locks. There is no directory fsync, because it isn't portable. Files are created with mode `0o666`, so the process UMASK decides (000 on Unraid, which keeps files editable over SMB). `CopyHashed` does not create parent directories. Atime is set to now and mtime to the source's mtime. A pre-existing `dst.partial` is an error and is left alone.
15. **`FreeName`** ports CameraImport's collision loop, but returns content-comparison errors where CameraImport ignored them. It keeps CameraImport's `os.Stat`.

## File Structure

```
internal/media/
  formats.go        Kind, Rules, DefaultRules, KindOf, TypeSubdir          (Task 1)
  naming.go         DefaultDayStartsAt, FolderDate, DestFileName, SuffixName (Task 1)
  video.go          VideoMeta type (Task 1); ReadVideoMeta (Task 6)
  hash.go           HashFile, QuickFingerprint, filesAreIdentical          (Task 2)
  copy.go           PartialSuffix, CopyHashed                              (Task 3)
  collision.go      FreeName                                               (Task 3)
  exif.go           ExifData, ReadExif, FileModDate                        (Task 5)
  capture.go        TimeSource, Capture, CaptureTime, ErrNoExifTime/ErrNoMvhdTime (Task 7)
  helpers_test.go   writeSourceFile, writeBytes, pattern, fixedMtime       (Task 2)
  fixture_test.go   fixture(t, rel)                                        (Task 5)
  *_test.go         one per source file; import_test.go = ported CameraImport tests (Task 7)
internal/media/mediatest/
  exif.go           ExifSpec, TIFF, JPEG, ARW                              (Task 4)
  mp4.go            VideoSpec, MP4                                         (Task 6)
  fixtures.go       SonyExif, GoProVideo, fixture paths, Fixtures()        (Tasks 4, 6)
  fixtures_test.go  TestFixtures (-update writes testdata/)                (Task 4)
testdata/cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.JPG, DSC00001.ARW          (Task 4)
testdata/cards/GOPRO/DCIM/100GOPRO/GX010001.MP4                            (Task 6)
```

---

### Task 1: Formats, kinds and naming rules

**Files:**
- Create: `internal/media/formats.go`, `internal/media/naming.go`, `internal/media/video.go`
- Test: `internal/media/formats_test.go`, `internal/media/naming_test.go`
- Modify: `docs/PLAN.md` (one row of the §5 table)

**Interfaces:**
- Consumes: nothing (first task; `internal/media` does not exist yet).
- Produces (later tasks rely on these exact names):

```go
package media // doc comment: "Package media classifies, dates, names, hashes and copies camera files. It is pure functions: no service state."

type Kind string

const (
	KindImage Kind = "image"
	KindRaw   Kind = "raw"
	KindVideo Kind = "video"
)

// Rules is the import rule set: which extensions are imported as which
// kind, and which subfolder of a session folder each kind goes in
// ("" = the folder itself).
type Rules struct {
	ImageFormats []string // lowercase, no dot
	RawFormats   []string
	VideoFormats []string
	ImageSubdir  string
	RawSubdir    string
	VideoSubdir  string
}

func DefaultRules() Rules
func (r Rules) KindOf(name string) (Kind, bool)
func TypeSubdir(kind Kind, r Rules) string

const DefaultDayStartsAt = 4 * time.Hour
func FolderDate(t time.Time, dayStartsAt time.Duration) string
func DestFileName(t time.Time, kind Kind, meta *VideoMeta, ext string) string
func SuffixName(name string, n int) string

// in video.go (Task 6 adds ReadVideoMeta to this file)
type VideoMeta struct {
	Duration     float64   // seconds: mvhd duration / mvhd timescale
	Width        int       // first video track, pixels
	Height       int
	FrameRate    string    // fmt "%.3f", e.g. "29.970"; "" when unknown
	Resolution   string    // fmt "%dw" of Width, e.g. "3840w"
	CreationTime time.Time // mvhd creation time in UTC; zero when absent
	Make         string    // udta ©mak; "" when absent
	Model        string    // udta ©mod; "" when absent
	Firmware     string    // udta FIRM (GoPro); "" when absent
}
```

- [ ] **Step 1: Write the failing tests**

`internal/media/formats_test.go`:

```go
func TestKindOf(t *testing.T) {
	r := DefaultRules()
	cases := []struct {
		name string
		kind Kind
		ok   bool
	}{
		{"DSC00001.JPG", KindImage, true},
		{"photo.jpeg", KindImage, true},
		{"scan.TIF", KindImage, true},
		{"screen.png", KindImage, true},
		{"anim.GIF", KindImage, true},
		{"DSC00001.ARW", KindRaw, true},
		{"IMG_0001.CR3", KindRaw, true},
		{"IMG_0001.cr2", KindRaw, true},
		{"DSC_0001.NEF", KindRaw, true},
		{"PXL_0001.dng", KindRaw, true},
		{"GX010001.MP4", KindVideo, true},
		{"clip.MOV", KindVideo, true},
		{"old.avi", KindVideo, true},
		{"GL010001.LRV", "", false},
		{"GX010001.THM", "", false},
		{"README", "", false},
		{"photo.jpg.zip", "", false},
		{filepath.Join("DCIM", "100MSDCF", "DSC00002.JPG"), KindImage, true},
	}
	for _, c := range cases {
		kind, ok := r.KindOf(c.name)
		if kind != c.kind || ok != c.ok {
			t.Errorf("KindOf(%q) = %q, %v; want %q, %v", c.name, kind, ok, c.kind, c.ok)
		}
	}
}

func TestKindOfCustomRules(t *testing.T) {
	r := Rules{ImageFormats: []string{"heic"}}
	if k, ok := r.KindOf("IMG_1.HEIC"); k != KindImage || !ok {
		t.Errorf("custom rules: got %q, %v", k, ok)
	}
	if _, ok := r.KindOf("IMG_1.jpg"); ok {
		t.Error("jpg matched rules that don't list it")
	}
}

func TestTypeSubdir(t *testing.T) {
	r := DefaultRules()
	for kind, want := range map[Kind]string{KindImage: "", KindRaw: "Capture", KindVideo: "Videos", Kind("other"): ""} {
		if got := TypeSubdir(kind, r); got != want {
			t.Errorf("TypeSubdir(%q) = %q, want %q", kind, got, want)
		}
	}
}
```

`internal/media/naming_test.go`:

```go
func TestFolderDate(t *testing.T) {
	z := time.FixedZone("CEST", 2*3600)
	cases := []struct {
		t    time.Time
		cut  time.Duration
		want string
	}{
		{time.Date(2025, 5, 31, 1, 30, 12, 0, z), 4 * time.Hour, "2025-05-30"}, // §7: a night out stays together
		{time.Date(2025, 5, 31, 4, 0, 0, 0, z), 4 * time.Hour, "2025-05-31"},   // exactly at the cutoff: new day
		{time.Date(2025, 5, 31, 3, 59, 59, 999999999, z), 4 * time.Hour, "2025-05-30"},
		{time.Date(2025, 5, 31, 15, 4, 5, 0, z), 4 * time.Hour, "2025-05-31"},
		{time.Date(2025, 3, 1, 2, 0, 0, 0, z), 4 * time.Hour, "2025-02-28"},
		{time.Date(2024, 3, 1, 2, 0, 0, 0, z), 4 * time.Hour, "2024-02-29"},
		{time.Date(2025, 1, 1, 0, 30, 0, 0, z), 4 * time.Hour, "2024-12-31"},
		{time.Date(2025, 5, 31, 0, 0, 0, 0, z), 0, "2025-05-31"},
	}
	for _, c := range cases {
		if got := FolderDate(c.t, c.cut); got != c.want {
			t.Errorf("FolderDate(%v, %v) = %s, want %s", c.t, c.cut, got, c.want)
		}
	}
	if DefaultDayStartsAt != 4*time.Hour {
		t.Errorf("DefaultDayStartsAt = %v", DefaultDayStartsAt)
	}
}

func TestDestFileName(t *testing.T) {
	ts := time.Date(2025, 5, 31, 1, 30, 12, 0, time.FixedZone("CEST", 2*3600))
	full := &VideoMeta{Resolution: "3840w", FrameRate: "29.970", Duration: 12.512}
	cases := []struct {
		kind Kind
		meta *VideoMeta
		ext  string
		want string
	}{
		{KindImage, nil, ".JPG", "2025-05-31 01.30.12.JPG"},
		{KindRaw, nil, ".ARW", "2025-05-31 01.30.12.ARW"},
		{KindVideo, full, ".MP4", "2025-05-31 01.30.12 - 3840w - 29.970 - 12sec.MP4"},
		{KindVideo, nil, ".MP4", "2025-05-31 01.30.12.MP4"},
		{KindVideo, &VideoMeta{Resolution: "1920w", Duration: 59.999}, ".mov", "2025-05-31 01.30.12 - 1920w -  - 59sec.mov"},
		{KindImage, full, ".JPG", "2025-05-31 01.30.12.JPG"}, // meta only matters for video
		{KindImage, nil, "", "2025-05-31 01.30.12"},
	}
	for _, c := range cases {
		if got := DestFileName(ts, c.kind, c.meta, c.ext); got != c.want {
			t.Errorf("DestFileName(%s, %+v, %q) = %q, want %q", c.kind, c.meta, c.ext, got, c.want)
		}
	}
}

func TestSuffixName(t *testing.T) {
	cases := []struct {
		name string
		n    int
		want string
	}{
		{"2025-05-31 01.30.12.JPG", 0, "2025-05-31 01.30.12.JPG"},
		{"2025-05-31 01.30.12.JPG", 1, "2025-05-31 01.30.12-1.JPG"},
		{"2025-05-31 01.30.12 - 3840w - 29.970 - 12sec.MP4", 2, "2025-05-31 01.30.12 - 3840w - 29.970 - 12sec-2.MP4"},
		{"abc", 1, "abc-1"},
	}
	for _, c := range cases {
		if got := SuffixName(c.name, c.n); got != c.want {
			t.Errorf("SuffixName(%q, %d) = %q, want %q", c.name, c.n, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/media/ -run 'TestKindOf|TestTypeSubdir|TestFolderDate|TestDestFileName|TestSuffixName' -v`
Expected: build failure (`undefined: DefaultRules`, …).

- [ ] **Step 3: Implement `formats.go`, `naming.go`, and the `VideoMeta` type in `video.go`**

- `DefaultRules()` returns exactly: `ImageFormats: {"jpg", "jpeg", "png", "gif", "tif"}`, `RawFormats: {"arw", "cr2", "cr3", "nef", "dng"}`, `VideoFormats: {"mp4", "avi", "mov"}`, `ImageSubdir: ""`, `RawSubdir: "Capture"`, `VideoSubdir: "Videos"`.
- `KindOf`: `ext := strings.TrimPrefix(filepath.Ext(name), ".")`. An empty ext gives `("", false)`. Otherwise compare it with `strings.EqualFold` against the image, video and RAW lists, in that order (CameraImport's order).
- `TypeSubdir`: switch on kind, returning the matching `Rules` field; `""` for an unknown kind.
- `DestFileName` is a port of CameraImport's `importFile`. For `kind == KindVideo && meta != nil`: `fmt.Sprintf("%s - %s - %s - %dsec%s", t.Format("2006-01-02 15.04.05"), meta.Resolution, meta.FrameRate, int(meta.Duration), ext)`. Otherwise: `t.Format("2006-01-02 15.04.05") + ext`.
- `SuffixName`: for `n == 0` return `name` unchanged. Otherwise `ext := filepath.Ext(name)` and return `fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), n, ext)`.
- `FolderDate` must use the wall clock, not `t.Add(-dayStartsAt)`, which goes wrong on DST-change days:

```go
// FolderDate returns the staging folder date (YYYY-MM-DD) for a capture at
// t: t's calendar date in t's own location, or the previous date when t's
// time of day is before dayStartsAt. Only folders use the cutoff; filenames
// keep the real time (§13).
func FolderDate(t time.Time, dayStartsAt time.Duration) string {
	y, m, d := t.Date()
	sinceMidnight := time.Duration(t.Hour())*time.Hour +
		time.Duration(t.Minute())*time.Minute +
		time.Duration(t.Second())*time.Second +
		time.Duration(t.Nanosecond())
	if sinceMidnight < dayStartsAt {
		d-- // time.Date normalizes day 0 to the last day of the previous month
	}
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Format("2006-01-02")
}
```

- Put the `VideoMeta` struct exactly as in the Produces block in `video.go`, with a doc comment saying `ReadVideoMeta` fills it.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/media/ -v`
Expected: PASS. Also `gofmt -l internal` prints nothing and `go vet ./internal/media/` is clean.

- [ ] **Step 5: Record the format-list decision in the spec**

In `docs/PLAN.md` §5, replace the table row whose first cell is `internal/config/config.go` (destination `internal/config/`, "Same format lists and subfolder paths…") with:

```
| `internal/config/config.go` | `internal/media/formats.go` | Same format lists and subfolder paths, as `media.Rules` / `DefaultRules()` (`internal/config` holds only process flags and env). Source and destination come from mounts, not settings. Add `cr3`. |
```

Change nothing else in `docs/PLAN.md`.

- [ ] **Step 6: Commit**

```bash
git add internal/media/formats.go internal/media/naming.go internal/media/video.go internal/media/formats_test.go internal/media/naming_test.go docs/PLAN.md
git commit -m "Add media formats and naming rules" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Full-file hash, quick fingerprint, identical-content check

**Files:**
- Create: `internal/media/hash.go`, `internal/media/helpers_test.go`
- Test: `internal/media/hash_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks (package `media` already exists in `internal/media/`).
- Produces:

```go
func HashFile(path string) (string, error)         // lowercase hex SHA-256 of the whole file
func QuickFingerprint(path string) (string, error) // lowercase hex, definition below
func filesAreIdentical(path1, path2 string) (bool, error) // unexported; used by FreeName (Task 3)
```

Test helpers in `helpers_test.go`, which later tasks' tests use:

```go
// fixedMtime is the mtime every helper-written file gets, so each one
// resolves to the same capture date (and thus the same destination name)
// when it has no embedded time. Ported from CameraImport's importer_test.go.
var fixedMtime = time.Date(2026, 1, 2, 15, 4, 5, 0, time.Local)

func writeBytes(t *testing.T, dir, name string, data []byte) string    // writes dir/name (0o644), sets atime+mtime to fixedMtime, returns the path; t.Fatal on error
func writeSourceFile(t *testing.T, dir, name, content string) string   // writeBytes with []byte(content); CameraImport's helper
func pattern(n int) []byte                                             // n bytes, b[i] = byte(i % 251)
```

- [ ] **Step 1: Write the helpers and the failing tests**

Write `helpers_test.go` as specified above. Then `hash_test.go`:

```go
const (
	sumHello   = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	sumEmpty   = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	sumPat200k = "e24bc62381f1224fbbb74688663f8f9743b9680b193edd666835e97b06e730eb"

	fpEmpty      = "af5570f5a1810b7af78caf4bc70a660f0df51e42baf91d4de5b2328de0e83dfc"
	fpHello      = "6bbb1026316442d9ade2fc3c01e4907ed7b2469a1c3670b3f473b65a89a7e49a"
	fpPat64k     = "66f10c0273c620c85b7ebfcdb03a6b3669e9d8d40ee1863400ab36cb06091fa1" // pattern(65536)
	fpPat200k    = "84e01709c427c78312b2469f33ae47bc3173a5fe9846d068a46e784c66bc9cd0" // pattern(200000)
	fpPat200kEnd = "dcdda296b8da6a058a69f6134681ac5dc358d577c86a263241e30fa0341aff7e" // pattern(200000) with byte 199999 ^= 0xFF
)
```

Tests (assert with `got != want` → `t.Errorf`):
- `TestHashFile`: `"hello"` → `sumHello`; an empty file → `sumEmpty`; `pattern(200000)` → `sumPat200k`; a missing path → non-nil error.
- `TestQuickFingerprintVectors`: empty → `fpEmpty`; `"hello"` → `fpHello`; `pattern(65536)` → `fpPat64k`; `pattern(200000)` → `fpPat200k`.
- `TestQuickFingerprintSamplesHeadAndTailOnly`: `pattern(200000)` with byte 100000 `^= 0xFF` still gives `fpPat200k` (the middle isn't read). With byte 199999 `^= 0xFF` it gives `fpPat200kEnd`. `pattern(200001)` gives something other than `fpPat200k` (the size counts).
- `TestQuickFingerprintMissingFile`: non-nil error.
- `TestFilesAreIdenticalComparesContent`, ported verbatim from CameraImport:

```go
// Same size but different bytes must NOT be treated as identical.
func TestFilesAreIdenticalComparesContent(t *testing.T) {
	dir := t.TempDir()
	p1 := writeSourceFile(t, dir, "a.jpg", "AAAA")
	p2 := writeSourceFile(t, dir, "b.jpg", "BBBB")

	identical, err := filesAreIdentical(p1, p2)
	if err != nil {
		t.Fatal(err)
	}
	if identical {
		t.Error("same-size files with different content reported as identical")
	}
}
```

- `TestFilesAreIdenticalSameContent`: two files both `"same"` → `true, nil`. `"a"` vs `"bb"` → `false, nil`. A missing second path → non-nil error.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/media/ -run 'Hash|Fingerprint|FilesAreIdentical' -v`
Expected: build failure (`undefined: HashFile`, …).

- [ ] **Step 3: Implement `hash.go`**

- `HashFile`: open the file, `io.Copy` into `sha256.New()`, return `hex.EncodeToString`.
- `filesAreIdentical` (port of CameraImport): `os.Stat` both paths and return any error. If the sizes differ, return `false, nil`. Otherwise compare `HashFile` of both.
- `QuickFingerprint` implements §6 exactly ("SHA-256 of `size || first 64 KB || last 64 KB`"). Use `io.CopyN` so a file that shrinks mid-read is an error, not a silent short hash:

```go
const fingerprintChunk = 64 << 10

// QuickFingerprint returns the hex SHA-256 of the file's size as 8 bytes
// big-endian, then its first min(64 KiB, size) bytes, then its last
// min(64 KiB, size) bytes (§6). For files under 128 KiB the two ranges
// overlap and both are hashed. It reads at most 128 KiB.
func QuickFingerprint(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	size := info.Size()
	h := sha256.New()
	var sz [8]byte
	binary.BigEndian.PutUint64(sz[:], uint64(size))
	h.Write(sz[:])
	head := min(size, fingerprintChunk)
	if _, err := io.CopyN(h, io.NewSectionReader(f, 0, head), head); err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	tailOff := max(0, size-fingerprintChunk)
	if _, err := io.CopyN(h, io.NewSectionReader(f, tailOff, size-tailOff), size-tailOff); err != nil {
		return "", fmt.Errorf("fingerprint %s: %w", path, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
```

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/media/ -v`
Expected: PASS. `gofmt -l internal` prints nothing; `go vet ./internal/media/` is clean.

- [ ] **Step 5: Commit**

```bash
git add internal/media/hash.go internal/media/hash_test.go internal/media/helpers_test.go
git commit -m "Add file hashing and quick fingerprint" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: CopyHashed and the collision loop

**Files:**
- Create: `internal/media/copy.go`, `internal/media/collision.go`
- Test: `internal/media/copy_test.go`, `internal/media/collision_test.go`

**Interfaces:**
- Consumes (already in package `media`):
  - `func HashFile(path string) (string, error)`: lowercase hex SHA-256.
  - `func filesAreIdentical(path1, path2 string) (bool, error)`: `false, nil` on size mismatch, else compares full hashes.
  - `func SuffixName(name string, n int) string`: `n == 0` returns `name`; otherwise inserts `-n` before the extension (`"a.JPG", 1` → `"a-1.JPG"`).
  - Test helpers in `helpers_test.go`: `var fixedMtime = time.Date(2026, 1, 2, 15, 4, 5, 0, time.Local)`, `writeBytes(t, dir, name string, data []byte) string` and `writeSourceFile(t, dir, name, content string) string` (both set mtime to `fixedMtime`), `pattern(n int) []byte`.
- Produces:

```go
const PartialSuffix = ".partial" // M2's startup cleanup deletes stray *PartialSuffix files

// CopyHashed copies src to dst and returns the hex SHA-256 of the bytes
// copied. It writes dst+PartialSuffix (created with O_EXCL), hashes while
// copying, fsyncs, sets the source's mtime, then renames into place. It
// never overwrites an existing dst (error wraps fs.ErrExist) and removes
// its partial on any failure. The parent directory must exist.
func CopyHashed(src, dst string) (string, error)

// FreeName is CameraImport's collision loop. It tries name, then
// SuffixName(name, 1), SuffixName(name, 2), … in dir and returns the first
// path that doesn't exist (duplicate == false), or the first existing path
// whose content is identical to src (duplicate == true: skip the copy).
func FreeName(src, dir, name string) (dst string, duplicate bool, err error)
```

- [ ] **Step 1: Write the failing tests**

`copy_test.go`:
- `TestCopyHashedCopiesAndHashes`: `src := writeSourceFile(t, dir, "src.jpg", "hello")`, `dst := filepath.Join(dir, "out.jpg")`. Check that `CopyHashed(src, dst)` returns `"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"` with nil error, that dst's content is `"hello"`, that `os.Stat(dst).ModTime().Equal(fixedMtime)`, and that `os.Stat(dst + PartialSuffix)` gives `fs.ErrNotExist`.
- `TestCopyHashedLargeFile`: `pattern(200000)` copies with hash `"e24bc62381f1224fbbb74688663f8f9743b9680b193edd666835e97b06e730eb"`, and `HashFile(dst)` returns the same value.
- `TestCopyFileRefusesToOverwrite`, ported from CameraImport with `copyFile` replaced by `CopyHashed` and two extra assertions at the end:

```go
func TestCopyFileRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "new content")
	dst := writeSourceFile(t, dir, "dst.jpg", "existing content")

	_, err := CopyHashed(src, dst)
	if err == nil {
		t.Fatal("CopyHashed overwrote an existing file")
	}
	data, err2 := os.ReadFile(dst)
	if err2 != nil {
		t.Fatal(err2)
	}
	if string(data) != "existing content" {
		t.Errorf("existing file was modified: %q", data)
	}
	if !errors.Is(err, fs.ErrExist) {
		t.Errorf("error %v does not wrap fs.ErrExist", err)
	}
	if _, err := os.Stat(dst + PartialSuffix); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial left behind: %v", err)
	}
}
```

- `TestCopyHashedStalePartial` (Review Focus 2): `writeSourceFile(t, dir, "out.jpg"+PartialSuffix, "stale")`. `CopyHashed(src, out.jpg)` returns an error that wraps `fs.ErrExist`. The partial still reads `"stale"`, and `out.jpg` does not exist.
- `TestCopyHashedMissingSource`: the error wraps `fs.ErrNotExist`, and neither dst nor dst+PartialSuffix exists.
- `TestCopyHashedMissingDestDir`: dst = `filepath.Join(dir, "nope", "out.jpg")` gives an error, and `filepath.Join(dir, "nope")` was not created.
- `TestCopyHashedSourceIsDirectory`: src is a subdirectory (`os.Mkdir`), so the call errors and no partial exists.
- `TestCopyToPartialRemovesPartialOnReadError`, against the unexported seam (see Step 3):

```go
func TestCopyToPartialRemovesPartialOnReadError(t *testing.T) {
	boom := errors.New("card pulled")
	p := filepath.Join(t.TempDir(), "x.jpg"+PartialSuffix)
	r := io.MultiReader(strings.NewReader("abc"), iotest.ErrReader(boom))
	if _, err := copyToPartial(r, p); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("partial left behind after a failed copy: %v", err)
	}
}
```

`collision_test.go` (each test uses `dir := t.TempDir()`, a separate `srcDir := t.TempDir()`, and `name := "2026-01-02 15.04.05.jpg"`):
- `TestFreeNameFreeTarget`: an empty dir gives `filepath.Join(dir, name), false, nil`.
- `TestFreeNameSkipsIdentical`: dir/name has the same content as src, giving `filepath.Join(dir, name), true, nil`.
- `TestFreeNameAddsSuffix`: dir/name `"first"` and dir/`"2026-01-02 15.04.05-1.jpg"` `"second"`, src `"third"`. Result: `filepath.Join(dir, "2026-01-02 15.04.05-2.jpg"), false, nil`.
- `TestFreeNameIdenticalAtSuffix` (Review Focus 3): dir/name `"other"` and dir/`…-1.jpg` with src's content. Result: the `-1` path, `true`, nil.
- `TestFreeNameSameSizeDifferentContent`: dir/name `"AAAA"`, src `"BBBB"`, giving the `-1` path and `false`.
- `TestFreeNameSourceMissing`: dir/name exists and src does not, so `err != nil` (CameraImport ignored this error; SD2NAS returns it).

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/media/ -run 'Copy|FreeName' -v`
Expected: build failure (`undefined: CopyHashed`, …).

- [ ] **Step 3: Implement `copy.go`**

This is the port of CameraImport's `copyFile`, extended as §5 describes. The ordering below is the design: the partial is removed only when this call created it, and dst is checked again right before the rename, because `os.Rename` silently replaces on both OSes.

```go
const PartialSuffix = ".partial"

func CopyHashed(src, dst string) (string, error) {
	if err := mustNotExist(dst); err != nil {
		return "", err
	}
	in, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("copy %s: not a regular file", src)
	}
	partial := dst + PartialSuffix
	sum, err := copyToPartial(in, partial)
	if err != nil {
		return "", err
	}
	// The partial exists and is ours: remove it on any later failure.
	if err := finishPartial(partial, dst, info.ModTime()); err != nil {
		if rmErr := os.Remove(partial); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
		return "", err
	}
	return sum, nil
}

// copyToPartial creates partial with O_EXCL (a stale partial is an
// fs.ErrExist error and is left alone, since it isn't ours), copies r into
// it while hashing, fsyncs and closes it. On failure it removes the partial.
func copyToPartial(r io.Reader, partial string) (string, error) {
	f, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666) // UMASK decides
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		if rmErr := os.Remove(partial); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// finishPartial keeps the source's modified time, then renames into place
// unless something appeared at dst meanwhile (folder locks, §8, make that
// rare; this check keeps it from ever overwriting).
func finishPartial(partial, dst string, mtime time.Time) error {
	if err := os.Chtimes(partial, time.Now(), mtime); err != nil {
		return err
	}
	if err := mustNotExist(dst); err != nil {
		return err
	}
	return os.Rename(partial, dst)
}

func mustNotExist(path string) error {
	_, err := os.Lstat(path)
	if err == nil {
		return fmt.Errorf("%s: %w", path, fs.ErrExist)
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}
```

- [ ] **Step 4: Implement `collision.go`**

This ports CameraImport's loop, keeping `os.Stat`. `os.Stat` → `fs.ErrNotExist` returns `(path, false, nil)`. Any other Stat error is returned. For an existing path, `filesAreIdentical(src, path)`: an error is returned (not ignored), and `true` returns `(path, true, nil)`. Otherwise try the next `n`. Candidates are `filepath.Join(dir, SuffixName(name, n))` for n = 0, 1, 2, …

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go test ./internal/media/ -v`
Expected: PASS on Windows. Also run `gofmt -l internal` (no output) and `go vet ./internal/media/`.

- [ ] **Step 6: Commit**

```bash
git add internal/media/copy.go internal/media/collision.go internal/media/copy_test.go internal/media/collision_test.go
git commit -m "Add CopyHashed and collision naming" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Synthesized EXIF fixtures (mediatest)

**Files:**
- Create: `internal/media/mediatest/exif.go`, `internal/media/mediatest/fixtures.go`
- Test: `internal/media/mediatest/exif_test.go`, `internal/media/mediatest/fixtures_test.go`
- Generate and commit: `testdata/cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.JPG`, `testdata/cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.ARW`
- Modify: `.gitattributes` (append one line)

Why synthesized: real camera files are megabytes. These builders produce byte-exact EXIF structures of a few hundred bytes with known values. The committed copies give M2's ingest tests (and `--cards ./testdata/cards` in local dev) a card-shaped folder.

**Interfaces:**
- Consumes: nothing. `mediatest` imports only the standard library in this task, and must **never** import `github.com/aspiziri/SD2NAS/internal/media` (media's tests import mediatest).
- Produces (package `mediatest`, import path `github.com/aspiziri/SD2NAS/internal/media/mediatest`):

```go
type ExifSpec struct {
	Make, Model        string
	DateTimeOriginal   string       // "2006:01:02 15:04:05"
	OffsetTimeOriginal string       // "+02:00"
	GPSDate            string       // "2006:01:02"; "" omits the whole GPS IFD
	GPSTime            [3][2]uint32 // hour, minute, second as {numerator, denominator}
}
func TIFF(order binary.ByteOrder, s ExifSpec) []byte
func JPEG(s ExifSpec) ([]byte, error)
func ARW(s ExifSpec) []byte

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

type Fixture struct {
	Path  string                 // relative to testdata/, slash-separated
	Build func() ([]byte, error) // produces the file's bytes
}
func Fixtures() []Fixture // SonyJPEG (JPEG(SonyExif)) and SonyARW (ARW(SonyExif)); Task 6 appends the MP4
```

- [ ] **Step 1: Write the failing tests**

`exif_test.go`:

```go
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
```

`fixtures_test.go`:

```go
var update = flag.Bool("update", false, "rewrite the committed fixtures under testdata/")

// TestFixtures builds every fixture. With -update it writes them to the
// repo's testdata/; without it, it checks that they are committed.
// Regenerate: go test ./internal/media/mediatest -run TestFixtures -update
func TestFixtures(t *testing.T) {
	for _, f := range Fixtures() {
		data, err := f.Build()
		if err != nil {
			t.Fatalf("%s: %v", f.Path, err)
		}
		path := filepath.Join("..", "..", "..", "testdata", filepath.FromSlash(f.Path))
		if *update {
			if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("fixture %s missing (go test ./internal/media/mediatest -run TestFixtures -update): %v", f.Path, err)
		}
	}
}
```

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/media/mediatest/ -v`
Expected: build failure (`undefined: TIFF`, …).

- [ ] **Step 3: Implement `mediatest/exif.go`**

Use this as written: the expected hex in `TestTIFFBytes` was produced by this exact code, and goexif's decoding of it was verified.

```go
// Package mediatest synthesizes tiny camera files (EXIF JPEG/ARW, MP4) for
// tests, and lists the fixtures committed under testdata/. It must not
// import internal/media.
package mediatest

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/jpeg"
)

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
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && es[j].tag < es[j-1].tag; j-- {
			es[j], es[j-1] = es[j-1], es[j]
		}
	}
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
```

(`binary.Write` into a `bytes.Buffer` cannot fail, so its error is not checked. Say so in a one-line comment above `TIFF`.)

Declare `ExifSpec` in `exif.go`, after `package`/`import`, exactly as in the Produces block. Put `SonyExif`, the path constants, `Fixture` and `Fixtures()` in `fixtures.go`, also exactly as in the Produces block. `Fixtures()` returns `{SonyJPEG, func() ([]byte, error) { return JPEG(SonyExif) }}` and `{SonyARW, func() ([]byte, error) { return ARW(SonyExif), nil }}`.

- [ ] **Step 4: Run the tests and confirm they pass, then generate the fixtures**

Run: `go test ./internal/media/mediatest/ -run 'TIFF|JPEG|ARW' -v`
Expected: PASS.
Run: `go test ./internal/media/mediatest/ -run TestFixtures -update`, then `go test ./internal/media/mediatest/ -v`.
Expected: PASS. `testdata/cards/SONY_A7IV/DCIM/100MSDCF/` contains `DSC00001.JPG` (about 600 bytes) and `DSC00001.ARW` (214 bytes).

- [ ] **Step 5: Mark the fixtures binary**

The repo has `core.autocrlf=true`, so append this line to `.gitattributes`:

```
testdata/cards/** binary
```

- [ ] **Step 6: Commit**

```bash
git add .gitattributes internal/media/mediatest testdata/cards/SONY_A7IV
git commit -m "Add synthesized EXIF fixtures" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: EXIF capture time, OffsetTimeOriginal and GPS time

**Files:**
- Create: `internal/media/exif.go`, `internal/media/fixture_test.go`
- Test: `internal/media/exif_test.go`
- Modify: `go.mod`, `go.sum` (add goexif)

**Interfaces:**
- Consumes:
  - Test helpers in `internal/media/helpers_test.go`: `var fixedMtime = time.Date(2026, 1, 2, 15, 4, 5, 0, time.Local)`, `writeBytes(t *testing.T, dir, name string, data []byte) string` and `writeSourceFile(t *testing.T, dir, name, content string) string`, which write the file and set its mtime to `fixedMtime`.
  - Package `github.com/aspiziri/SD2NAS/internal/media/mediatest`: `type ExifSpec struct { Make, Model, DateTimeOriginal ("2006:01:02 15:04:05"), OffsetTimeOriginal, GPSDate ("2006:01:02", "" omits GPS) string; GPSTime [3][2]uint32 }`, `func TIFF(order binary.ByteOrder, s ExifSpec) []byte`, `func JPEG(s ExifSpec) ([]byte, error)`, `func ARW(s ExifSpec) []byte`, `var SonyExif` (SONY / ILCE-7M4, `"2025:05:31 01:30:12"`, `"+02:00"`, GPS `"2025:05:30"` 23:30:12), and the consts `SonyJPEG`, `SonyARW` (paths relative to the repo's `testdata/`, slash-separated).
- Produces:

```go
type ExifData struct {
	DateTime   time.Time // DateTimeOriginal, else DateTime, as a wall clock in time.Local; zero when absent or unparsable
	Camera     string    // "Make Model" when both are present
	OffsetTime string    // OffsetTimeOriginal as written (e.g. "+02:00"); "" when absent
	GPSTime    time.Time // GPSDateStamp + GPSTimeStamp in UTC; zero when absent or malformed
}
func ReadExif(path string) (*ExifData, error)
func FileModDate(path string) (time.Time, error) // os.Stat(path).ModTime()
```

In `fixture_test.go`: `func fixture(t *testing.T, rel string) string`. It returns `filepath.Join("..", "..", "testdata", filepath.FromSlash(rel))` and calls `t.Fatalf` if the file is missing.

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/rwcarlsen/goexif@v0.0.0-20190401172101-9e8deecbddbd`
Expected: `go.mod` gains `require github.com/rwcarlsen/goexif v0.0.0-20190401172101-9e8deecbddbd`, still says `go 1.24`, and has no `toolchain` line. (Run `go mod tidy` after Step 4 so it isn't marked `// indirect`.)

- [ ] **Step 2: Write the failing tests**

`exif_test.go` (`import mt "github.com/aspiziri/SD2NAS/internal/media/mediatest"`):

```go
const nameLayout = "2006-01-02 15.04.05"

var sonyGPS = time.Date(2025, 5, 30, 23, 30, 12, 0, time.UTC)

func checkSony(t *testing.T, d *ExifData, wantGPS time.Time) {
	t.Helper()
	if got := d.DateTime.Format(nameLayout); got != "2025-05-31 01.30.12" {
		t.Errorf("DateTime = %s", got)
	}
	if d.DateTime.Location() != time.Local {
		t.Errorf("DateTime location = %v, want time.Local", d.DateTime.Location())
	}
	if d.Camera != "SONY ILCE-7M4" {
		t.Errorf("Camera = %q", d.Camera)
	}
	if d.OffsetTime != "+02:00" {
		t.Errorf("OffsetTime = %q", d.OffsetTime)
	}
	if !d.GPSTime.Equal(wantGPS) {
		t.Errorf("GPSTime = %v, want %v", d.GPSTime, wantGPS)
	}
}
```

- `TestReadExifSonyJPEGFixture`: `ReadExif(fixture(t, mt.SonyJPEG))` → nil error, `checkSony(t, d, sonyGPS)`.
- `TestReadExifSonyARWFixture`: the same for `mt.SonyARW`.
- `TestReadExifBigEndianTIFF`: `writeBytes(t, dir, "x.TIF", mt.TIFF(binary.BigEndian, mt.SonyExif))` passes `checkSony(…, sonyGPS)`.
- `TestReadExifWithoutOptionalTags`: `mt.JPEG(mt.ExifSpec{DateTimeOriginal: "2025:05:31 01:30:12"})` gives the right DateTime, `OffsetTime == ""`, `GPSTime.IsZero()` and `Camera == ""`.
- `TestReadExifNoDateTime`: `mt.JPEG(mt.ExifSpec{Make: "SONY", Model: "ILCE-7M4"})` gives a nil error, `DateTime.IsZero()` and `Camera == "SONY ILCE-7M4"`.
- `TestReadExifFractionalGPSSeconds`: a SonyExif copy with `GPSTime = {{23, 1}, {30, 1}, {1234, 100}}` gives GPSTime `Equal(time.Date(2025, 5, 30, 23, 30, 12, 340000000, time.UTC))`.
- `TestReadExifBadGPSRational` (Review Focus 1): GPSTime `{{23, 1}, {30, 0}, {12, 1}}` and, separately, `{{0xFFFFFFFF, 1}, {30, 1}, {12, 1}}`. Each gives no panic, a nil error, `GPSTime.IsZero()`, and DateTime still `"2025-05-31 01.30.12"`.
- `TestReadExifBadGPSDate`: `GPSDate: "2025-05-30"` (wrong separators) gives `GPSTime.IsZero()` and a nil error.
- `TestReadExifNotExif`: `writeSourceFile(t, dir, "a.jpg", "first")` gives a non-nil error. An empty file gives a non-nil error. A missing path gives a non-nil error.
- `TestFileModDate`: `writeSourceFile` gives `Equal(fixedMtime)` and a nil error. A missing path gives an error that wraps `fs.ErrNotExist`.

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/media/ -run 'Exif|FileModDate' -v`
Expected: build failure (`undefined: ReadExif`).

- [ ] **Step 4: Implement `exif.go`**

`ReadExif` is a port of CameraImport's `internal/exif/exif.go`, trimmed and extended. goexif's verified API: `exif.Decode(io.Reader) (*exif.Exif, error)` returns a partial `*Exif` plus an error for which `exif.IsCriticalError(err)` is false when only a sub-IFD failed. Also `(*Exif).DateTime() (time.Time, error)` (DateTimeOriginal, else DateTime, parsed in `time.Local`), `(*Exif).Get(exif.FieldName) (*tiff.Tag, error)`, `x.Raw []byte`, `x.Tiff.Order`, `tiff.DecodeDir(r tiff.ReadAtReader, order) (*tiff.Dir, int32, error)`, `(*tiff.Tag).StringVal()`, `.Int64(i)`, `.Rat2(i) (num, den int64, err)` (which panics only when i is out of range, so check `Count`), and `Tag.Id`, `Tag.Count`. goexif has no field for 0x9011, and its `LoadTags` drops unknown IDs, so read it by ID:

```go
func ReadExif(path string) (*ExifData, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	x, err := exif.Decode(f)
	if err != nil && (x == nil || exif.IsCriticalError(err)) {
		return nil, fmt.Errorf("exif %s: %w", path, err)
	}
	// A non-critical error means a sub-IFD failed; use what was decoded.
	d := &ExifData{GPSTime: gpsTime(x)}
	if dt, err := x.DateTime(); err == nil {
		d.DateTime = dt
	}
	mk, okMake := strTag(x, exif.Make)
	md, okModel := strTag(x, exif.Model)
	if okMake && okModel {
		d.Camera = mk + " " + md
	}
	if d.OffsetTime, err = offsetTimeOriginal(x); err != nil {
		return nil, fmt.Errorf("exif %s: %w", path, err)
	}
	return d, nil
}

func strTag(x *exif.Exif, name exif.FieldName) (string, bool) {
	t, err := x.Get(name)
	if err != nil {
		return "", false
	}
	s, err := t.StringVal()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(s), true
}

const tagOffsetTimeOriginal = 0x9011

// offsetTimeOriginal reads tag 0x9011 from the Exif sub-IFD by ID: goexif's
// field table predates EXIF 2.31. "" when there is no Exif IFD or no tag.
func offsetTimeOriginal(x *exif.Exif) (string, error) {
	ptr, err := x.Get(exif.ExifIFDPointer)
	if err != nil {
		return "", nil // no Exif sub-IFD
	}
	off, err := ptr.Int64(0)
	if err != nil {
		return "", fmt.Errorf("ExifIFDPointer: %w", err)
	}
	r := bytes.NewReader(x.Raw)
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return "", fmt.Errorf("seek to Exif IFD: %w", err)
	}
	dir, _, err := tiff.DecodeDir(r, x.Tiff.Order)
	if err != nil {
		return "", fmt.Errorf("decode Exif IFD: %w", err)
	}
	for _, t := range dir.Tags {
		if t.Id != tagOffsetTimeOriginal {
			continue
		}
		s, err := t.StringVal()
		if err != nil {
			return "", fmt.Errorf("OffsetTimeOriginal: %w", err)
		}
		return strings.TrimSpace(s), nil
	}
	return "", nil
}

// gpsTime combines GPSDateStamp and GPSTimeStamp (UTC). GPS is advisory
// (§12), so anything missing or malformed gives the zero time.
func gpsTime(x *exif.Exif) time.Time {
	ds, ok := strTag(x, exif.GPSDateStamp)
	if !ok {
		return time.Time{}
	}
	day, err := time.Parse("2006:01:02", ds)
	if err != nil {
		return time.Time{}
	}
	ts, err := x.Get(exif.GPSTimeStamp)
	if err != nil || ts.Count < 3 {
		return time.Time{}
	}
	var d time.Duration
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		num, den, err := ts.Rat2(i)
		if err != nil || den <= 0 || num < 0 || num/den > 60 {
			return time.Time{}
		}
		d += time.Duration(num/den)*unit + time.Duration(math.Round(float64(num%den)*float64(unit)/float64(den)))
	}
	return day.Add(d)
}
```

`FileModDate` is `os.Stat` → `ModTime()`, returning the Stat error.

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go mod tidy`, then `go test ./internal/media/... -v`
Expected: PASS. `go.mod` lists goexif as a direct requirement under `go 1.24`. `gofmt -l internal` prints nothing; `go vet ./...` is clean.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/media/exif.go internal/media/exif_test.go internal/media/fixture_test.go
git commit -m "Read EXIF capture time, offset and GPS time" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Video metadata and mvhd creation time

**Files:**
- Create: `internal/media/mediatest/mp4.go`, `internal/media/mediatest/mp4_test.go`, `internal/media/video_test.go`
- Modify: `internal/media/mediatest/fixtures.go` (add `GoProVideo`, `GoProMP4`, and a `Fixtures()` entry), `internal/media/video.go` (add `ReadVideoMeta`), `go.mod`, `go.sum`
- Generate and commit: `testdata/cards/GOPRO/DCIM/100GOPRO/GX010001.MP4`

**Interfaces:**
- Consumes:
  - `internal/media/video.go` already declares:

    ```go
    type VideoMeta struct {
    	Duration     float64   // seconds: mvhd duration / mvhd timescale
    	Width        int       // first video track, pixels
    	Height       int
    	FrameRate    string    // fmt "%.3f", e.g. "29.970"; "" when unknown
    	Resolution   string    // fmt "%dw" of Width, e.g. "3840w"
    	CreationTime time.Time // mvhd creation time in UTC; zero when absent
    	Make         string    // udta ©mak; "" when absent
    	Model        string    // udta ©mod; "" when absent
    	Firmware     string    // udta FIRM (GoPro); "" when absent
    }
    ```

  - Test helpers in `internal/media`: `writeBytes(t, dir, name string, data []byte) string`, `writeSourceFile(t, dir, name, content string) string`, and `fixture(t, rel string) string` (the path under the repo's `testdata/`).
  - `internal/media/mediatest/fixtures.go` already has `type Fixture struct { Path string; Build func() ([]byte, error) }` and `func Fixtures() []Fixture` (returning the Sony JPEG and ARW). `TestFixtures` with `-update` writes every entry to `testdata/<Path>`.
- Produces:

```go
// package media
func ReadVideoMeta(path string) (*VideoMeta, error)

// package mediatest
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
func MP4(s VideoSpec) ([]byte, error)

// GoProVideo: 4K at 29.97 fps, 12.512 s, created 2025-05-31 08:30:12 UTC
// (3831525012 s after 1904-01-01), GoPro FIRM atom, audio trak first.
var GoProVideo = VideoSpec{
	CreationTime: 3831525012, Timescale: 1000, Duration: 12512,
	Width: 3840, Height: 2160, MediaTimescale: 30000, SampleCount: 375, SampleDelta: 1001,
	Audio: true, Firmware: "HD9.01.01.60.00",
}
const GoProMP4 = "cards/GOPRO/DCIM/100GOPRO/GX010001.MP4"
```

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/abema/go-mp4@v1.4.1`
Expected: `go.mod` gains `github.com/abema/go-mp4 v1.4.1` and `github.com/google/uuid v1.1.2 // indirect`, keeps `go 1.24`, and gets no `toolchain` line.

- [ ] **Step 2: Write the failing tests**

`mediatest/mp4_test.go`, which checks the builder against go-mp4's own reader (verified API: `mp4.ExtractBox(r io.ReadSeeker, parent *mp4.BoxInfo, path mp4.BoxPath) ([]*mp4.BoxInfo, error)` and `mp4.ExtractBoxWithPayload(...) ([]*mp4.BoxInfoWithPayload, error)`, where the payload is `.Payload`):

```go
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
```

`internal/media/video_test.go`, with `want := time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC)` and a helper `writeMP4(t, spec) string` that calls `mt.MP4` and `writeBytes`:
- `TestReadVideoMetaGoProFixture`: `ReadVideoMeta(fixture(t, mt.GoProMP4))` gives Width 3840, Height 2160, Resolution `"3840w"`, FrameRate `"29.970"` (Review Focus 4: the audio trak comes first and must not set it), Duration `12.512`, `CreationTime.Equal(want)` with `CreationTime.Location() == time.UTC`, Firmware `"HD9.01.01.60.00"`, and Make and Model `""`.
- `TestReadVideoMetaVersion1Boxes`: `GoProVideo` with `Version = 1` gives the same values.
- `TestReadVideoMetaMakeModel`: `Make "Apple"`, `Model "iPhone 15"` read back as `Make == "Apple"`, `Model == "iPhone 15"`.
- `TestReadVideoMetaNoCreationTime`: `CreationTime = 0` gives `CreationTime.IsZero()` and a nil error.
- `TestReadVideoMetaAudioOnly`: `Width = 0, Height = 0` gives an error whose text contains `"could not extract video dimensions"`.
- `TestReadVideoMetaNotMP4`: `writeSourceFile(t, dir, "x.MP4", "first")` gives a non-nil error. A missing path gives a non-nil error.

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/media/... -run 'MP4|VideoMeta' -v`
Expected: build failure (`undefined: MP4`, `undefined: ReadVideoMeta`).

- [ ] **Step 4: Implement `mediatest/mp4.go`, and add the fixture**

The builder uses go-mp4's writer (verified API: `mp4.NewWriter(io.WriteSeeker)`, `(*Writer).StartBox(&mp4.BoxInfo{Type: t})`, `EndBox()`, `Write`, and `mp4.Marshal(w, box, mp4.Context{})`). The writer seeks back to patch sizes, so it needs an in-memory `io.WriteSeeker`; the standard library has none, hence `seekBuf`:

```go
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
```

Declare `VideoSpec` at the top of `mp4.go`, exactly as in the Produces block. In `fixtures.go`, add `GoProVideo`, `GoProMP4`, and a third `Fixtures()` entry `{GoProMP4, func() ([]byte, error) { return MP4(GoProVideo) }}`.

- [ ] **Step 5: Implement `ReadVideoMeta` in `internal/media/video.go`**

This ports CameraImport's `getVideoMetadata`, keeping its error strings, the `%dw` and `%.3f` formats, and the first-video-track rule. It adds udta expansion, the mvhd creation time, and make/model/firmware. go-mp4 API used: `mp4.ReadBoxStructure(r io.ReadSeeker, func(h *mp4.ReadHandle) (interface{}, error))`, `h.BoxInfo.Type`, `h.Path` (a `mp4.BoxPath`, i.e. `[]mp4.BoxType`, including the current box), `h.Expand()`, `h.ReadPayload()`, `h.ReadData(io.Writer)`, `(*mp4.Mvhd).GetCreationTime()`, `GetDuration()`, `.Timescale`, `(*mp4.Tkhd).Width/Height` (16.16 fixed point), `(*mp4.Mdhd).Timescale`, and `(*mp4.Stts).Entries[i].SampleCount/SampleDelta`.

```go
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
	// 0 means "not set"; values at or above 2^40 s (about year 36,800) are garbage.
	if creation != 0 && creation < 1<<40 {
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
```

- [ ] **Step 6: Generate the fixture and run the tests**

Run: `go test ./internal/media/mediatest/ -run TestFixtures -update`, then `go mod tidy`, then `go test ./internal/media/... -v`
Expected: PASS. `testdata/cards/GOPRO/DCIM/100GOPRO/GX010001.MP4` exists (about 565 bytes). The Sony fixtures are rewritten with identical metadata, so `git status` may show them as modified only if the local `image/jpeg` encoder changed. Commit whatever was regenerated. `gofmt -l internal` prints nothing; `go vet ./...` is clean.

- [ ] **Step 7: Commit**

```bash
git add go.mod go.sum internal/media/video.go internal/media/video_test.go internal/media/mediatest testdata/cards
git commit -m "Read video metadata and mvhd creation time" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: CaptureTime and the ported CameraImport import tests

**Files:**
- Create: `internal/media/capture.go`
- Test: `internal/media/capture_test.go`, `internal/media/import_test.go`

**Interfaces:**
- Consumes (all in package `media` unless noted):
  - `type Kind string` with `KindImage = "image"`, `KindRaw = "raw"`, `KindVideo = "video"`; `func DefaultRules() Rules`; `func (r Rules) KindOf(name string) (Kind, bool)`; `func TypeSubdir(kind Kind, r Rules) string` (defaults: image `""`, raw `"Capture"`, video `"Videos"`).
  - `const DefaultDayStartsAt = 4 * time.Hour`; `func FolderDate(t time.Time, dayStartsAt time.Duration) string` (`"YYYY-MM-DD"`, with the cutoff applied).
  - `func DestFileName(t time.Time, kind Kind, meta *VideoMeta, ext string) string`: `"2006-01-02 15.04.05"+ext`, or for video with meta `"<time> - <Resolution> - <FrameRate> - <int Duration>sec<ext>"`.
  - `func FreeName(src, dir, name string) (dst string, duplicate bool, err error)`; `func CopyHashed(src, dst string) (string, error)` (lowercase hex SHA-256; never overwrites; dir must exist); `func HashFile(path string) (string, error)`; `func QuickFingerprint(path string) (string, error)`.
  - `func ReadExif(path string) (*ExifData, error)` with `ExifData{DateTime time.Time (Local; zero if absent), Camera, OffsetTime string, GPSTime time.Time (UTC; zero if absent)}`; `func FileModDate(path string) (time.Time, error)`.
  - `func ReadVideoMeta(path string) (*VideoMeta, error)` with `VideoMeta{Duration float64; Width, Height int; FrameRate, Resolution string; CreationTime time.Time (UTC; zero if absent); Make, Model, Firmware string}`.
  - Test helpers: `fixedMtime` (`2026-01-02 15:04:05`, `time.Local`), `writeBytes`, `writeSourceFile` (both set mtime to `fixedMtime`), `fixture(t, rel) string`.
  - `mediatest` (`mt`): `ExifSpec`, `JPEG(ExifSpec) ([]byte, error)`, `VideoSpec`, `MP4(VideoSpec) ([]byte, error)`, `SonyExif`, `GoProVideo`, and the paths `SonyJPEG`, `SonyARW`, `GoProMP4`. The Sony pair was shot at `2025:05:31 01:30:12` with offset `+02:00`, GPS `2025-05-30 23:30:12Z`, camera `SONY ILCE-7M4`. The GoPro video was created at `2025-05-31 08:30:12Z` (3840w, 29.970 fps, 12.512 s, no make/model).
- Produces:

```go
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
	Fallback error      // why a better source than mtime was tried and not used; nil otherwise
}

// CaptureTime resolves a file's capture time: EXIF for images and RAW
// (except png/gif, which can't carry it), mvhd for video, else the
// modified time. It errors only when the modified time can't be read.
func CaptureTime(path string, kind Kind) (Capture, error)
```

- [ ] **Step 1: Write the failing tests**

`capture_test.go` (`const nameLayout = "2006-01-02 15.04.05"` already exists in `exif_test.go`, so reuse it; declare the package-level `var goproUTC = time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC)`, which `import_test.go` also uses):
- `TestCaptureTimeSonyJPEG`: `CaptureTime(fixture(t, mt.SonyJPEG), KindImage)` gives Source `SourceExif`, `Time.Format(nameLayout) == "2025-05-31 01.30.12"`, `Time.Location() == time.Local`, Offset `"+02:00"`, `GPS.Equal(time.Date(2025, 5, 30, 23, 30, 12, 0, time.UTC))`, Camera `"SONY ILCE-7M4"`, and Fallback nil.
- `TestCaptureTimeSonyARW`: the same for `mt.SonyARW` with `KindRaw`.
- `TestCaptureTimeGoProMP4`: `mt.GoProMP4` with `KindVideo` gives Source `SourceMvhd`, `Time.Equal(goproUTC)`, `Time.Location() == time.Local`, Camera `""`, and Fallback nil.
- `TestCaptureTimeVideoMakeModel`: GoProVideo with `Make "Apple"`, `Model "iPhone 15"` gives Camera `"Apple iPhone 15"`.
- `TestCaptureTimePNGUsesMtime`: `writeSourceFile(t, dir, "x.png", "not a png")` with KindImage gives Source `SourceMtime`, `Time.Equal(fixedMtime)`, and Fallback nil (png can't carry EXIF, so nothing was tried).
- `TestCaptureTimeJPEGWithoutDate`: `writeBytes` of `mt.JPEG(mt.ExifSpec{Make: "SONY", Model: "ILCE-7M4"})` gives Source mtime, `Time.Equal(fixedMtime)`, `errors.Is(Fallback, ErrNoExifTime)`, and Camera `"SONY ILCE-7M4"`.
- `TestCaptureTimeNotReallyJPEG`: `writeSourceFile(t, dir, "a.jpg", "first")` gives Source mtime and `Fallback != nil`.
- `TestCaptureTimeVideoWithoutCreationTime` (Review Focus 5): GoProVideo with `CreationTime = 0` gives Source mtime, `Time.Equal(fixedMtime)`, and `errors.Is(Fallback, ErrNoMvhdTime)`.
- `TestCaptureTimeUnparsableVideo`: `writeSourceFile(t, dir, "x.MP4", "first")` with KindVideo gives Source mtime and `Fallback != nil`.
- `TestCaptureTimeMissingFile`: a non-nil error.

`import_test.go` ports CameraImport's `importer_test.go`. The bodies and expected values are kept; `importOne` replaces `ImportService.importFile` with the M1 pipeline:

```go
// importOne is the M1 equivalent of CameraImport's importFile for one
// image going into destBase/shoot: capture time → name → collision loop →
// CopyHashed.
func importOne(t *testing.T, path, destBase string) {
	t.Helper()
	c, err := CaptureTime(path, KindImage)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(destBase, "shoot", TypeSubdir(KindImage, DefaultRules()))
	if err := os.MkdirAll(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	dst, duplicate, err := FreeName(path, dir, DestFileName(c.Time, KindImage, nil, filepath.Ext(path)))
	if err != nil {
		t.Fatalf("FreeName(%s): %v", path, err)
	}
	if duplicate {
		return // identical file already imported
	}
	if _, err := CopyHashed(path, dst); err != nil {
		t.Fatalf("CopyHashed(%s): %v", path, err)
	}
}
```

- `readDestDir(t, destBase) map[string]string`: verbatim from CameraImport (reads `destBase/shoot`, mapping name → content).
- `TestImportFileCollisionDoesNotOverwrite`: verbatim. Write `a.jpg` `"first"`, `b.jpg` `"second, different content"` and `c.jpg` `"third, also different content here"`, then call `importOne` on each. Expect 3 files and no duplicate content. Add one assertion: the names are exactly `"2026-01-02 15.04.05.jpg"`, `"2026-01-02 15.04.05-1.jpg"` and `"2026-01-02 15.04.05-2.jpg"`.
- `TestImportFileSkipsIdenticalDuplicate`: verbatim. Calling `importOne` twice on the same `"same content"` file leaves 1 file.
- `TestImportFileFolderMatchesCaptureDate`: port the intent (the folder comes from the same capture time as the filename). `writeSourceFile(t, src, "a.jpg", "content")` and then `CaptureTime` give `FolderDate(c.Time, 0) == "2026-01-02"` and `FolderDate(c.Time, DefaultDayStartsAt) == "2026-01-02"`, while `DestFileName(c.Time, KindImage, nil, ".jpg") == "2026-01-02 15.04.05.jpg"`.
- `TestImportFixtureCard`: the whole M1 pipeline over the three committed fixtures. For each fixture: kind from `DefaultRules().KindOf`, then `CaptureTime`, `ReadVideoMeta` for video (nil meta otherwise), and `rel := filepath.Join(FolderDate(c.Time, DefaultDayStartsAt), TypeSubdir(kind, DefaultRules()), DestFileName(c.Time, kind, meta, filepath.Ext(path)))`. Expected:
  - `mt.SonyJPEG`: `filepath.Join("2025-05-30", "2025-05-31 01.30.12.JPG")`
  - `mt.SonyARW`: `filepath.Join("2025-05-30", "Capture", "2025-05-31 01.30.12.ARW")`
  - `mt.GoProMP4`: with `local := goproUTC.In(time.Local)`, `filepath.Join(FolderDate(local, DefaultDayStartsAt), "Videos", local.Format(nameLayout)+" - 3840w - 29.970 - 12sec.MP4")`

  Then `MkdirAll` each target directory under a `t.TempDir()` staging root, `FreeName` and `CopyHashed` each file in, and assert that the returned hash equals `HashFile(source)`, and that each source's `QuickFingerprint` is the same before and after (cards are never modified).

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/media/ -run 'CaptureTime|Import' -v`
Expected: build failure (`undefined: CaptureTime`).

- [ ] **Step 3: Implement `capture.go`**

`canCarryExif` ports CameraImport's function onto a path and kind:

```go
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
```

`CaptureTime`:
- For `KindVideo`, call `ReadVideoMeta`. An error gives `Fallback = fmt.Errorf("video metadata: %w", err)`. A zero `CreationTime` gives `Fallback = ErrNoMvhdTime`. Otherwise `Time = meta.CreationTime.In(time.Local)` and `Source = SourceMvhd`. Whenever the meta was read, set `Camera = strings.TrimSpace(meta.Make + " " + meta.Model)`.
- If `canCarryExif`, call `ReadExif`. An error gives `Fallback = fmt.Errorf("exif: %w", err)`. A zero `DateTime` gives `Fallback = ErrNoExifTime`. Otherwise `Time = x.DateTime` and `Source = SourceExif`. Whenever EXIF was read, copy `Offset`, `GPS` and `Camera` from it, even if the time is missing.
- If `Source` is still empty, call `FileModDate`. If that fails, return `Capture{}` and the error (joined with `Fallback` when non-nil). Otherwise `Source = SourceMtime`.

- [ ] **Step 4: Run the whole package**

Run: `go test ./internal/media/... -v`, then `gofmt -l .`, `go vet ./...` and `go test ./...`
Expected: all PASS, and gofmt prints nothing. This is the full CI set, run locally on Windows; CI repeats it on Linux.

- [ ] **Step 5: Commit**

```bash
git add internal/media/capture.go internal/media/capture_test.go internal/media/import_test.go
git commit -m "Add CaptureTime and port CameraImport import tests" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
