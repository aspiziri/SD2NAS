# M2 Catalog and Ingest (Headless) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Import a mounted card into `Staging/` with no clicks: a SQLite hash catalog (`internal/catalog`), the ingest pipeline and its one-at-a-time job queue (`internal/ingest`), and `POST /api/ingest`, `GET /api/ingest/current` and `GET /api/ingests[/{id}]` behind the request guard, so that re-inserting a card imports only what is new and the card script learns whether the card is safe to format.

**Architecture:** `internal/catalog` owns the database: one `*sql.DB` limited to one connection (WAL, the single writer), migrations keyed on `PRAGMA user_version`, and typed queries; hashes cross its API as lowercase hex and are stored as 32-byte BLOBs. `internal/ingest` has pure filesystem helpers (scan, partial cleanup, free space, durable mkdir), pure policy (clock checks, the two-line summary), a `Runner` that runs one ingest end to end (scan → classify → space check → copy → record → finish), and a `Manager` that queues jobs and runs them one at a time on a worker goroutine. `internal/api` adds the ingest endpoints and a guard on every mutating request; `cmd/sd2nas` wires it all up and shuts down so no half-copied file is left at a final name.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `database/sql` + `modernc.org/sqlite v1.46.1` (pure Go), `net/http`, `log/slog`; existing `internal/media` and `internal/media/mediatest`.

Spec: docs/PLAN.md

Relevant spec sections: §2 (mounts), §3 (storage, capacity guard, safe to format), §6 (catalog schema, duplicate detection, aliases, culled stays remembered), §7 (ingest pipeline and failure handling, the core of M2), §8 (folder naming only; locks are M4), §10 (`POST /api/ingest`, `GET /api/ingest/current`, `GET /api/ingests[/{id}]`, request guard), §11 (card script contract: `?wait=1&format=text` returns two lines, level then message; body `{path,label,uuid}`), §12 (clock checks), §13 (time zones, burst shots, day cutoff), §16 M2 checklist including the five items carried from M1.

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`. Task 1's `go get` rewrites the directive to `go 1.24.0` (required: the new dependencies declare `go 1.24.0`); keep that, and add no `toolchain` line. Use no API newer than Go 1.24: no `sync.WaitGroup.Go`, no `testing.T.Output`. Go 1.24 features that are fine: `testing.T.Context`, `encoding/json` `omitzero`, `slog.DiscardHandler`, `range` over an int. The local toolchain is go1.26 on Windows; CI runs `gofmt -l .`, `go vet ./...` and `go test ./...` on ubuntu with the Go version from `go.mod`.
- Dependencies: only `modernc.org/sqlite v1.46.1` (Task 1) and the indirect modules it brings (listed in Task 1). No migration library, router, ORM or anything else. Driver name `"sqlite"`, imported for side effects in `internal/catalog` only.
- M2 touches: `internal/catalog/` (new), `internal/ingest/` (new), `internal/media/copy.go`, `internal/media/syncdir_*.go`, `internal/media/copy_test.go`, `internal/media/import_test.go`, `internal/media/mediatest/card.go` (new), `internal/api/`, `cmd/sd2nas/main.go`, `deploy/Dockerfile`, `go.mod`, `go.sum`, and `docs/PLAN.md` (Task 10 only, the edits it lists). Do not modify `internal/config/` or other `internal/media` files.
- Tests pass on Windows and Linux. Use `path/filepath` for OS paths and `path` / slash-separated strings for anything stored in the catalog. OS-specific code goes in build-tagged files with a variant for every other OS. Never assert permission bits. Never depend on the value of `time.Local`: build expected values through `time.Local` or fixed zones and compare instants with `Equal`.
- Close every file, `*sql.Rows` and `*catalog.DB` before returning; Windows cannot delete open files and `t.TempDir` cleanup fails otherwise. In tests, call `t.TempDir()` before registering a `Close` cleanup so the close runs first.
- Never silently swallow an error: return it (wrapped with `%w`), record it in a documented field (`IngestFile.Error`, `Ingest.Error`), or log it where this plan says to. Use `errors.Join` when a cleanup step fails after an earlier error.
- Card files are only ever opened read-only, and nothing on a card is ever created, modified or deleted.
- The catalog has exactly one connection (`SetMaxOpenConns(1)`). Inside `internal/catalog`, never issue a query while iterating `*sql.Rows`: read all rows, close them, then query again. Inside a transaction use only the `*sql.Tx`.
- `rel_path` values are slash-separated and relative to the photos root, e.g. `Staging/2025-05-30/Capture/2025-05-31 01.30.12.ARW`. Staging lives at `<photos>/Staging`.
- Export only the identifiers listed in each task's **Produces** block. Unexported helpers are free unless a later task's **Consumes** block names them.
- After every task, `gofmt -l .` prints nothing and `go vet ./...` is clean.
- Tests that wait on another goroutine use a 10 s deadline and fail with a clear message; never rely on a sleep for correctness (short sleeps inside a polling loop are fine).
- Commit on branch `m2-catalog-ingest`. Every commit message ends with a blank line and then `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`.

## Review Focus

1. **A card that must not be reported safe to format**: only formats SD2NAS doesn't import (Sony `.HIF`), no media at all, or a single failed file. Expected: `safe_to_format` false and a warning-level message. Pinned in Task 7 (`TestRunUnsupportedFormatsNotSafe`, `TestRunEmptyCardNotSafe`) and Task 8 (`TestRunFailedFileNotSafe`, `TestRunZeroByteFileFails`).
2. **Re-inserting a card that was already imported**, including when the share is nearly full. Expected: 0 new, nothing copied, no space refusal, safe to format. Pinned in Task 7 (`TestRunRerunImportsNothing`, `TestRunNoSpaceCheckWithoutNewFiles`) and Task 10 (`TestIngestWaitText`).
3. **Card pulled or a read failing mid-copy.** Expected: never a file at a final name that isn't complete, finished files stay recorded, job `failed`, and a re-insert picks up where it left off. Pinned in Task 2 (`TestCopyHashedCloseErrorLeavesNoDst`) and Task 8 (`TestRunCardRemovedMidCopy`, `TestRunCopyFailureLeavesNothing`).
4. **SIGTERM during a long video copy.** Expected: the copy stops within one 32 KiB read, its partial is removed, the job is `cancelled` and recorded, queued jobs are `cancelled`. Pinned in Task 2 (`TestCopyHashedCanceledMidCopy`), Task 8 (`TestRunCancelledMidCopy`) and Task 9 (`TestManagerCloseCancelsRunning`).
5. **Card junk**: macOS `._*` files and `.Trashes`, `.Spotlight-V100`, `MISC/`, `.thumbnails`, Sony `THMBNL/` thumbnails and `M4ROOT/SUB/` proxies, GoPro `.LRV`/`.THM`. Expected: none of it is imported or counted. Pinned in Task 5 (`TestScanSkipsJunk`).
6. **Not enough space.** Expected: status `refused`, nothing copied, and a non-2xx response so the script leaves the card mounted. Pinned in Task 7 (`TestRunRefusedBelowReserve`) and Task 10 (`TestIngestRefusedIs507`).
7. **Burst shots in one second.** Expected: `-1`, `-2` follow card filename order. Pinned in Task 7 (`TestRunBurstOrder`).
8. **A request path that isn't a card**: `/cards` itself, a path outside it, a relative path, or a symlink from `/cards` into `/photos`. Expected: 400, nothing ingested. Pinned in Task 9 (`TestValidateCardPath`).
9. **A long ingest.** Expected: other requests are answered while it runs, and `?wait=1` gives up with 504 after its ceiling. Pinned in Task 10 (`TestIngestWaitTimeout`).
10. **Leftovers from a crash**: a stale `*.partial`, and a file copied into staging whose catalog write never happened. Expected: the partial is removed before copying, the unrecorded copy is adopted (not duplicated as `-1`). Pinned in Task 8 (`TestRunCleansStalePartials`, `TestRunAdoptsUnrecordedCopy`).

## Decisions

1. **`modernc.org/sqlite v1.46.1`.** It is the newest release whose `go.mod` declares `go 1.24.0`: every release from v1.46.2 through v1.59.0 declares `go 1.25.0` and v1.60.0 through v1.63.0 declare `go 1.26.0` (checked with `go mod download -json` on each). v1.46.1 (2026-02-18) is not retracted, bundles SQLite 3.51.2, and was verified under `GOTOOLCHAIN=go1.24.0`: it builds for windows and `GOOS=linux CGO_ENABLED=0`, opens a file database in WAL mode with `foreign_keys` on, and honours `_txlock=immediate`. `go get` also bumps the indirect `github.com/google/uuid` from v1.1.2 to v1.6.0 and `golang.org/x/sys` to v0.37.0; both are expected.
2. **Dockerfile copies `go.sum`.** It still copies only `go.mod` (its comment says to add `go.sum` once there are dependencies, which M1 added), so a container build cannot verify modules. Task 1 copies both and runs `go mod download` before copying sources.
3. **One connection.** `Open` sets `SetMaxOpenConns(1)`: the catalog is the single writer, no `SQLITE_BUSY` is possible, and reads are tiny. DSN pragmas, applied to every connection: `busy_timeout(5000)`, `foreign_keys(1)`, `journal_mode(WAL)`, `synchronous(FULL)` (the catalog is the record behind "safe to format", so a commit must survive power loss), and `_txlock=immediate`. `Open` checks that `PRAGMA journal_mode` really reports `wal` and fails otherwise. The path is passed without a `file:` prefix (modernc then uses it verbatim), and a path containing `?` is rejected.
4. **Migrations** are an ordered `[]string` in `schema.go`. Version N is `PRAGMA user_version`. Each pending migration runs in its own transaction that also sets `user_version`. A database whose version is newer than the binary's is refused ("created by a newer SD2NAS"), never touched.
5. **Schema changes from §6.** Ingest status gains `queued` (every job has a row and an id as soon as it's submitted). `ingests` gains `card_path`, `unsupported` and `clock_warnings` columns. File outcome gains `unsupported`. A `clock_warnings` table holds per-file clock warnings (§12 wants them on the summary and, later, on staging folders). Count columns are `NOT NULL DEFAULT 0`, and text columns where NULL would mean nothing are `NOT NULL DEFAULT ''`. NULL is kept where it means something: `assets.capture_time`, `rel_path` (culled), `mtime`, `first_ingest` (found by the indexer), `ingests.finished_at`, and `ingest_files.quick_fp`/`sha256`. Enumerations get `CHECK` constraints. `ingest_files` and `clock_warnings` are indexed by `ingest_id`. `card_size` is the total size of the media files found on the card. The exact SQL is in Task 1.
6. **Encodings.** Hashes are lowercase hex at the catalog API (what `internal/media` returns) and 32-byte BLOBs in the database; anything else is `ErrBadHash`. Timestamps (`started_at`, `updated_at`, …) are UTC text `2006-01-02T15:04:05.000Z`. `capture_time` is the naming wall clock with no zone, `2006-01-02T15:04:05` (`catalog.CaptureLayout`), read back in `time.Local`. `mtime` is the staged file's `ModTime().UnixNano()`. `staging_folders.dates` is a JSON array of `YYYY-MM-DD`, kept sorted.
7. **Settings** (§6 `settings` table) read in M2: `day_starts_at` (`HH:MM`, default `04:00`), `verify_known` (`quick` | `full`, default `quick`), and `free_space_reserve_gb` (a whole number of decimal GB, default `50`, i.e. 50,000,000,000 bytes). Values are validated strictly on write and on read: a bad stored value fails the ingest with an error naming the key, never a silent default. Import rules stay `media.DefaultRules()` in M2; settings-backed rules (M1 decision 1) arrive with the settings API.
8. **Known outcomes.** An asset's state maps to the outcome: `archived` → `in_archive`, `staged` → `in_staging`, `culled` and `missing` → `seen_before`. When a fingerprint matches several assets, the best state wins: archived, staged, culled, missing.
9. **Safe to format** = status `done` and `failed == 0` and `unsupported == 0` and `seen > 0`. "Verified" (§3) means: hashed while copying, file fsynced (`CopyHashed`), its folder fsynced (`media.SyncDir`, the M2 follow-up), and the asset committed with `synchronous=FULL`. There is no read-back of the staged file: right after the write it would come from the page cache and prove nothing about the disk. Clock warnings never block it.
10. **Unsupported formats**: a media-like extension that the rules don't import (lowercase): `heic heif hif raf orf rw2 pef srw x3f 3fr iiq gpr crw nrw srf sr2 mts m2ts mxf braw r3d insv insp`. They are recorded with outcome `unsupported` and block "safe to format", because formatting would lose them. Other unknown files (Sony `.XML` sidecars, `.BIN`, `.CTG`, GoPro `.LRV`/`.THM`) are ignored.
11. **Scan skip rules** (§7 step 2, extended): directories whose name starts with `.` (`.Trashes`, `.thumbnails`, `.Spotlight-V100`, `.fseventsd`), or is `MISC`, `System Volume Information`, `$RECYCLE.BIN` or `THMBNL` (Sony video thumbnails), or is `SUB` inside `M4ROOT` (Sony proxy clips), all case-insensitive. Files whose name starts with `.` (`._*` resource forks, `.DS_Store`). Non-regular entries, so symlinks are never followed. The whole card is walked, not just `DCIM/`, because Sony video lives in `PRIVATE/M4ROOT/CLIP`. An unreadable subdirectory becomes a `failed` row and the rest of the card is still imported; an unreadable card root fails the job.
12. **Target staging folder** for a folder date D: the folder whose `dates` contains D (the lowest id if several); else the folder *named* D, which gets D added to its dates; else a new row named D. Directories are created with `mkdirDurable`, which fsyncs each new directory's parent. A folder name that isn't a single path element is refused (defence against a hand-edited row).
13. **Copy errors** are classified by `media.ClassifyCopyError` (the M1 follow-up that tells the two `fs.ErrNotExist` and the two `fs.ErrExist` cases apart). Name taken → pick a new name with `FreeName`. Stale partial → remove it (ingest is the only writer of partials in Staging and runs alone, so it must be stale). Destination folder missing → recreate it. Each of these retries, up to 3 attempts per file. Source missing or anything else → the file fails. After any file failure the card root is stat'ed; if it is gone the job ends `failed` with `card removed during import`. A card whose mount point survives but whose reads return I/O errors ends `done` with every remaining file `failed`, which is still not safe to format.
14. **Duplicates found while copying.** If `FreeName` finds identical content already at the target, the card file is hashed and looked up: a catalog asset makes it a known outcome (and its fingerprint is added); no asset means a copy from an interrupted ingest, which is adopted as a `staged` asset with outcome `in_staging`. If the full hash of a fresh copy matches an existing asset (fingerprint collision or alias, §6), the fresh copy is deleted and the file gets that asset's known outcome.
15. **`CopyHashed` gains a `context.Context`** (`CopyHashed(ctx, src, dst)`, a breaking change; only M1's tests call it) so a SIGTERM stops a multi-GB copy at the next 32 KiB read instead of after it, and it **closes the source explicitly before the final rename** (M2 follow-up), so any error means no destination file. `media.SyncDir` fsyncs a directory on Unix and only checks that it is a directory elsewhere (NTFS journals metadata; Windows cannot fsync a directory handle without backup semantics). An `EINVAL` or other error from fsyncing on Unraid's shfs is surfaced, not ignored; FUSE turns an unimplemented `fsyncdir` into success itself.
16. **Partials** (`*.partial`) are deleted by `CleanPartials` on startup (`Manager.Recover`) and again before every copy phase, only under `Staging/`. M4 must keep ingest the only writer of partials in Staging, or clean them under the folder locks.
17. **Queue.** `Submit` writes a `queued` row at once. At most 8 jobs wait behind the running one; the 9th gets `ErrQueueFull` (503). The same resolved card path, while queued or running, returns the existing job. The queue is in memory: on restart, `queued` and `running` rows become `failed` (§7 "On startup").
18. **HTTP.** The request guard (§10) is implemented in M2 for every request whose method isn't GET or HEAD, because `POST /api/ingest` is the first mutating endpoint. M3's checklist item stays as is (it is about the UI's endpoints). The optional token stays M7. `?wait=1` maps the finished status to a code: done 200, refused 507, failed 500, cancelled 503, and 504 after `DefaultMaxWait = 2h` (the script's `curl --max-time 7200`). So every case where the card must stay mounted is non-2xx and fails `curl -f`. A client that hangs up just stops waiting; the job continues. M6 should switch the script to `curl --fail-with-body` so refusal reasons reach the notification.
19. **Not in M2:** SSE (`/api/events`, M3; headless clients poll `GET /api/ingest/current`), thumbnail queueing (M3), folder locks (M4: in M2, ingest is the only writer of `Staging/`), `reimport-culled`, settings endpoints.
20. **Clock checks** (§12, on new files only): `future` when the capture instant is more than 10 minutes after the NAS time; `reset_default` when the capture year is before 2005; `gps_mismatch` when EXIF GPS time and the capture instant differ by more than 5 minutes (GPS fixes lag a little). The capture instant is the EXIF wall clock in `OffsetTimeOriginal`'s zone when the offset is known, else `Capture.Time`. The time-zone hint (§12 row 4) is stored as `assets.tz_offset`, not raised as a warning.
21. **Catalog unavailable at startup** (for example `/config` not writable): SD2NAS keeps serving, as M0 does for bad mounts, `/healthz` reports the problem and returns 503, and the ingest routes are not mounted (the card script's request fails, so the card stays mounted).
22. **Measuring RAW ingest** (M2 follow-up) is a documented manual step on the NAS: the runner logs `capture` (time spent reading capture times), `copy` (time spent copying) and `copy_bytes` on its `ingest finished` line. Task 10 writes the procedure into PLAN.md and leaves that checklist item open for the user.
23. **Zero-byte media files** are `failed` (`empty file`): they mean an interrupted camera write, and the card should be looked at before it is formatted.
24. **Catalog writes in a running job use `context.WithoutCancel(ctx)`**, so a cancelled job still records the files it finished and its own `cancelled` status. The job's context only stops the pipeline between files and inside `CopyHashed`.

## File Structure

```
internal/catalog/
  catalog.go        DB, Open, Close, Version, ErrNotFound, ErrBadHash; tx, time and hex helpers   (Task 1)
  schema.go         SchemaVersion, migrations                                                   (Task 1)
  settings.go       VerifyMode, Settings, DefaultSettings, Key*, (*DB).Settings, SetSetting     (Task 1)
  assets.go         AssetState, Asset, CaptureLayout, InsertAsset, AssetsByFingerprint,
                    AssetBySHA, AddFingerprint, AddAlias                                        (Task 3)
  staging.go        StagingFolder, StagingFolderForDate, StagingFolders                         (Task 3)
  ingests.go        IngestStatus, Outcome, Counts, Ingest, IngestFile, ClockWarning(Kind),
                    ingest rows, ingest files, RecordNew, clock warnings                        (Task 4)
  helpers_test.go   openTest, hexN                                                              (Task 1)
internal/media/
  copy.go           CopyHashed(ctx, …) closing the source before rename; CopyFailure,
                    ClassifyCopyError                                                           (Task 2)
  syncdir_unix.go   SyncDir (//go:build unix)                                                   (Task 2)
  syncdir_other.go  SyncDir (//go:build !unix)                                                  (Task 2)
internal/media/mediatest/
  card.go           CopyCard                                                                    (Task 5)
internal/ingest/
  scan.go           StagingDir, CardFile, ScanError, ScanResult, Scan                           (Task 5)
  partials.go       CleanPartials                                                               (Task 5)
  fsutil.go         mkdirDurable                                                                (Task 5)
  freespace_statfs.go   FreeSpace (//go:build linux || darwin)                                  (Task 5)
  freespace_windows.go  FreeSpace                                                               (Task 5)
  freespace_other.go    FreeSpace (//go:build !linux && !darwin && !windows)                    (Task 5)
  clock.go          FutureTolerance, GPSTolerance, ResetBeforeYear, CheckClock                  (Task 6)
  describe.go       Level*, Describe, CardName                                                  (Task 6)
  progress.go       Phase*, Progress, NewProgress, ProgressSnapshot                             (Task 7)
  run.go            Request, Config, Runner, NewRunner, Run (core Task 7, failures Task 8)
  manager.go        ErrBadCardPath, ErrQueueFull, ErrClosed, MaxQueued, Manager, Job,
                    ValidateCardPath                                                            (Task 9)
internal/api/
  guard.go          guard                                                                       (Task 10)
  ingest.go         DefaultMaxWait, MountIngest, ingest handlers                                (Task 10)
  server.go         ServeHTTP through guard, AddProblem                                         (Task 10)
cmd/sd2nas/main.go  open catalog, Recover, Start, MountIngest, shutdown order                   (Task 10)
deploy/Dockerfile   copy go.sum, go mod download                                                (Task 1)
docs/PLAN.md        M2 checklist and notes                                                      (Task 10)
```

---

### Task 1: Catalog database, schema, migrations and settings

**Files:**
- Create: `internal/catalog/catalog.go`, `internal/catalog/schema.go`, `internal/catalog/settings.go`
- Test: `internal/catalog/catalog_test.go`, `internal/catalog/settings_test.go`, `internal/catalog/helpers_test.go`
- Modify: `go.mod`, `go.sum`, `deploy/Dockerfile`

**Consumes:** nothing from earlier tasks. `internal/catalog` does not exist yet.

**Produces:**

```go
// Package catalog is SD2NAS's SQLite database: every file ever imported or
// indexed, the ingest history, staging folders and settings.
package catalog

type DB struct{ db *sql.DB } // the only field; later tasks use d.db

var ErrNotFound = errors.New("catalog: not found")
var ErrBadHash = errors.New("catalog: hash must be 64 lowercase hex digits")

const SchemaVersion = 1 // == len(migrations)

func Open(path string) (*DB, error)
func (d *DB) Close() error
func (d *DB) Version(ctx context.Context) (int, error) // PRAGMA user_version

type VerifyMode string

const (
	VerifyQuick VerifyMode = "quick"
	VerifyFull  VerifyMode = "full"
)

const (
	KeyDayStartsAt        = "day_starts_at"
	KeyVerifyKnown        = "verify_known"
	KeyFreeSpaceReserveGB = "free_space_reserve_gb"
)

type Settings struct {
	DayStartsAt  time.Duration // folder-day cutoff, from "HH:MM"
	VerifyKnown  VerifyMode
	ReserveBytes uint64 // free-space reserve on the photos share
}

func DefaultSettings() Settings // {4h, VerifyQuick, 50_000_000_000}
func (d *DB) Settings(ctx context.Context) (Settings, error)
func (d *DB) SetSetting(ctx context.Context, key, value string) error

// Unexported helpers later tasks in this package use (exact names):
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error // BeginTx; fn; Commit, or Rollback joined onto fn's error
func formatTime(t time.Time) string                               // t.UTC().Format("2006-01-02T15:04:05.000Z07:00"), so UTC prints "Z"
func parseTime(s string) (time.Time, error)
func hexToBlob(s string) ([]byte, error) // 64 lowercase hex digits → 32 bytes, else ErrBadHash (wrapped, naming s)
func blobToHex(b []byte) string          // lowercase
```

- [ ] **Step 1: Add the dependency and fix the Dockerfile**

Run: `go get modernc.org/sqlite@v1.46.1`
Expected: `go: upgraded go 1.24 => 1.24.0`, `go: upgraded github.com/google/uuid v1.1.2 => v1.6.0`, and `added` lines for `github.com/dustin/go-humanize v1.0.1`, `github.com/mattn/go-isatty v0.0.20`, `github.com/ncruces/go-strftime v1.0.0`, `github.com/remyoudompheng/bigfft v0.0.0-20230129092748-24d4a6f8daec`, `golang.org/x/exp v0.0.0-20251023183803-a4bb9ffd2546`, `golang.org/x/sys v0.37.0`, `modernc.org/libc v1.67.6`, `modernc.org/mathutil v1.7.1`, `modernc.org/memory v1.11.0`, `modernc.org/sqlite v1.46.1`. After Step 3 adds the import, `go mod tidy` moves `modernc.org/sqlite` into the direct `require` block. There must be no `toolchain` line.

In `deploy/Dockerfile`, replace `COPY go.mod ./` and the comment line under it with:

```dockerfile
COPY go.mod go.sum ./
RUN go mod download
```

- [ ] **Step 2: Write the failing tests**

`helpers_test.go`:

```go
// openTest opens a fresh catalog in a temp dir and closes it at cleanup.
func openTest(t *testing.T) *DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sd2nas.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := d.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	return d
}

// hexN returns a valid hash: the byte n repeated 32 times, as hex.
func hexN(n byte) string { return strings.Repeat(fmt.Sprintf("%02x", n), 32) }
```

`catalog_test.go`:
- `TestOpenCreatesSchema`: `openTest`, then `Version` is 1 (`SchemaVersion`), and the table names in `sqlite_master` (type `table`, sorted) are exactly `aliases, assets, clock_warnings, fingerprints, ingest_files, ingests, settings, staging_folders`.
- `TestOpenPragmas`: on the open DB, `PRAGMA journal_mode` is `"wal"`, `PRAGMA foreign_keys` is 1, `PRAGMA synchronous` is 2 (FULL), and `PRAGMA busy_timeout` is 5000.
- `TestOpenTwice`: `Open`, `Close`, `Open` the same path again: no error, `Version` still 1, and the `settings` row inserted before the first close is still there.
- `TestOpenRefusesNewerSchema`: open, `d.db.Exec("PRAGMA user_version = 2")`, close, reopen. The error contains `"newer"`, and the reopened handle is nil.
- `TestOpenRejectsQuestionMark`: `Open(filepath.Join(t.TempDir(), "a?b.db"))` errors.
- `TestForeignKeysEnforced`: `INSERT INTO fingerprints(quick_fp, sha256) VALUES (?, ?)` with two `hexToBlob(hexN(1))` values and no asset errors.
- `TestTimeRoundTrip`: `formatTime(time.Date(2025, 5, 31, 1, 30, 12, 123456789, time.FixedZone("CEST", 2*3600)))` is `"2025-05-30T23:30:12.123Z"`, and `parseTime` of it is `Equal` to `time.Date(2025, 5, 30, 23, 30, 12, 123000000, time.UTC)`.
- `TestHexBlob`: `hexToBlob(hexN(0xab))` gives 32 bytes of `0xab`, and `blobToHex` of that is `hexN(0xab)`. `"abc"`, a 64-character string containing `g`, and `strings.ToUpper(hexN(0xab))` each give an error for which `errors.Is(err, ErrBadHash)` holds.

`settings_test.go`:
- `TestSettingsDefaults`: on an empty catalog, `Settings` equals `DefaultSettings()`, which is `{4 * time.Hour, VerifyQuick, 50_000_000_000}`.
- `TestSetSettingRoundTrip`: `SetSetting` with `day_starts_at` `"05:30"` gives `DayStartsAt == 5*time.Hour + 30*time.Minute`; `"00:00"` gives 0; `verify_known` `"full"` gives `VerifyFull`; `free_space_reserve_gb` `"0"` gives 0 and `"120"` gives `120_000_000_000`. Setting a key twice keeps the last value (upsert).
- `TestSetSettingRejects`: each of these errors and leaves the stored value unchanged: key `"nope"`; `day_starts_at` `"24:00"`, `"4:00"`, `"04:60"`, `""`; `verify_known` `"fast"`; `free_space_reserve_gb` `"-1"`, `"1.5"`, `"abc"`, `"99999999999"` (overflows uint64 bytes).
- `TestSettingsReportsBadStoredValue`: `d.db.Exec("INSERT INTO settings(key, value) VALUES ('day_starts_at', 'noon')")`, then `Settings` returns an error containing `"day_starts_at"`.

- [ ] **Step 3: Run the tests and confirm they fail**

Run: `go test ./internal/catalog/ -v`
Expected: build failure (`undefined: Open`).

- [ ] **Step 4: Implement `catalog.go`, `schema.go` and `settings.go`**

`Open`: reject a path containing `?`; `sql.Open("sqlite", path+dsnParams)`; `SetMaxOpenConns(1)`; check `PRAGMA journal_mode` returns `wal` (else an error naming the mode it got); run the migrations. On any failure, close the `*sql.DB` and return the error joined with the close error, wrapped as `open catalog <path>: …`. Import `_ "modernc.org/sqlite"`.

```go
const dsnParams = "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)" +
	"&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
```

Migrations: read `PRAGMA user_version` as v. If `v > len(migrations)`, return `fmt.Errorf("catalog schema version %d was created by a newer SD2NAS (this one knows %d)", v, len(migrations))`. For each `i` from v to `len(migrations)-1`, use `d.tx` to `Exec(migrations[i])` and then `Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1))`. modernc runs multi-statement `Exec`.

`schema.go`, migration 1 (`var migrations = []string{schemaV1}`):

```sql
CREATE TABLE ingests (
  id             INTEGER PRIMARY KEY,
  card_path      TEXT NOT NULL,
  card_label     TEXT NOT NULL DEFAULT '',
  card_uuid      TEXT NOT NULL DEFAULT '',     -- filesystem UUID (the reader's SERIAL is shared by every card)
  card_size      INTEGER NOT NULL DEFAULT 0,   -- bytes of media files found
  started_at     TEXT NOT NULL,
  finished_at    TEXT,
  status         TEXT NOT NULL CHECK (status IN ('queued','running','done','failed','refused','cancelled')),
  seen           INTEGER NOT NULL DEFAULT 0,
  new            INTEGER NOT NULL DEFAULT 0,
  in_archive     INTEGER NOT NULL DEFAULT 0,
  in_staging     INTEGER NOT NULL DEFAULT 0,
  seen_before    INTEGER NOT NULL DEFAULT 0,
  failed         INTEGER NOT NULL DEFAULT 0,
  unsupported    INTEGER NOT NULL DEFAULT 0,
  clock_warnings INTEGER NOT NULL DEFAULT 0,
  safe_to_format INTEGER NOT NULL DEFAULT 0,
  error          TEXT NOT NULL DEFAULT ''
);

-- One row per unique file content ever seen, whether imported or indexed.
CREATE TABLE assets (
  sha256        BLOB PRIMARY KEY CHECK (length(sha256) = 32),
  size          INTEGER NOT NULL,
  kind          TEXT NOT NULL CHECK (kind IN ('image','raw','video')),
  capture_time  TEXT,                         -- local wall clock as named, 2006-01-02T15:04:05
  tz_offset     TEXT NOT NULL DEFAULT '',     -- EXIF OffsetTimeOriginal, if any
  state         TEXT NOT NULL CHECK (state IN ('staged','archived','culled','missing')),
  rel_path      TEXT,                         -- slash-separated, relative to /photos; NULL when culled
  mtime         INTEGER,                      -- UnixNano of the stored file, for cheap rescans
  first_ingest  INTEGER REFERENCES ingests(id),  -- NULL if found by the indexer
  updated_at    TEXT NOT NULL
);
CREATE INDEX assets_path ON assets(rel_path);

-- Quick fingerprints, many-to-one, never deleted (§6 "aliases").
CREATE TABLE fingerprints (
  quick_fp  BLOB NOT NULL CHECK (length(quick_fp) = 32),
  sha256    BLOB NOT NULL REFERENCES assets(sha256),
  PRIMARY KEY (quick_fp, sha256)
);

-- Content that used to be an asset but changed in place.
CREATE TABLE aliases (
  old_sha256 BLOB PRIMARY KEY CHECK (length(old_sha256) = 32),
  sha256     BLOB NOT NULL REFERENCES assets(sha256),
  seen_at    TEXT NOT NULL
);

-- What happened to each file on the card.
CREATE TABLE ingest_files (
  ingest_id  INTEGER NOT NULL REFERENCES ingests(id),
  card_path  TEXT NOT NULL,                   -- slash-separated, relative to the card root
  size       INTEGER NOT NULL,
  quick_fp   BLOB,
  sha256     BLOB,
  outcome    TEXT NOT NULL CHECK (outcome IN ('new','in_archive','in_staging','seen_before','failed','unsupported')),
  error      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX ingest_files_ingest ON ingest_files(ingest_id);

CREATE TABLE clock_warnings (
  ingest_id  INTEGER NOT NULL REFERENCES ingests(id),
  card_path  TEXT NOT NULL,
  sha256     BLOB,
  kind       TEXT NOT NULL CHECK (kind IN ('future','reset_default','gps_mismatch')),
  detail     TEXT NOT NULL
);
CREATE INDEX clock_warnings_ingest ON clock_warnings(ingest_id);

CREATE TABLE staging_folders (
  id         INTEGER PRIMARY KEY,
  name       TEXT NOT NULL UNIQUE,            -- directory name under Staging/
  dates      TEXT NOT NULL,                   -- JSON array of YYYY-MM-DD it covers, sorted
  created_at TEXT NOT NULL
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

`d.tx`: `BeginTx(ctx, nil)`; on fn error, `errors.Join(err, rollbackErr)` where a rollback error of `sql.ErrTxDone` is not joined; else `Commit`.

`settings.go`: one parser per key, shared by `SetSetting` (validate, then `INSERT … ON CONFLICT(key) DO UPDATE SET value = excluded.value`) and `Settings` (start from `DefaultSettings()`, read all rows, parse each known key, ignore unknown keys so a newer binary's keys don't break an older one). A parse error is `fmt.Errorf("setting %s = %q: %w", key, value, err)`. `day_starts_at` must match `^([01][0-9]|2[0-3]):[0-5][0-9]$`. `free_space_reserve_gb` is `strconv.ParseUint(v, 10, 64)` and must not overflow when multiplied by 1e9 (`n > math.MaxUint64/1_000_000_000` is an error).

- [ ] **Step 5: Run the tests and confirm they pass**

Run: `go mod tidy`, then `go test ./internal/catalog/ -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS, `go.mod` lists `modernc.org/sqlite v1.46.1` in the direct block, and `go vet`/`gofmt` print nothing. If Docker is available, `docker build -f deploy/Dockerfile -t sd2nas:m2 .` succeeds; otherwise say so in the commit body.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum deploy/Dockerfile internal/catalog/
git commit -m "Add catalog schema, migrations and settings" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: CopyHashed closes its source first, honours a context, and classifies its errors; SyncDir

**Files:**
- Modify: `internal/media/copy.go`, `internal/media/copy_test.go`, `internal/media/import_test.go`
- Create: `internal/media/syncdir_unix.go`, `internal/media/syncdir_other.go`
- Test: `internal/media/copy_test.go`, `internal/media/syncdir_test.go`

**Consumes** (existing code in `internal/media/copy.go`; read it first):

```go
const PartialSuffix = ".partial"
func CopyHashed(src, dst string) (string, error)          // the signature this task changes
func copyToPartial(r io.Reader, partial string) (string, error) // O_EXCL create, hash while copying, fsync, close; removes its partial on failure
func finishPartial(partial, dst string, mtime time.Time) error  // Chtimes, mustNotExist(dst), Rename
func removePartial(partial string, err error) error
func mustNotExist(path string) error // fmt.Errorf("%s: %w", path, fs.ErrExist) when path exists; not a *fs.PathError
```

Test helpers in `internal/media/helpers_test.go`: `writeSourceFile(t, dir, name, content string) string`, `writeBytes(t, dir, name string, data []byte) string`, `pattern(n int) []byte`, and `fixedMtime`.

**Produces:**

```go
// CopyHashed (doc keeps M1's text, plus): ctx is checked before starting and
// before every read, so a cancelled copy stops within one read; the error
// then wraps ctx.Err(). The source is closed before the partial is renamed
// into place, so whenever CopyHashed returns an error, dst was not created.
func CopyHashed(ctx context.Context, src, dst string) (string, error)

// CopyFailure says why CopyHashed failed, so a caller can choose to retry.
type CopyFailure int

const (
	CopyFailOther          CopyFailure = iota // anything else (I/O error, source not a regular file, …)
	CopyFailCanceled                          // ctx was cancelled or timed out
	CopyFailSourceMissing                     // src does not exist (card pulled, file gone)
	CopyFailDestDirMissing                    // dst's directory does not exist
	CopyFailDestTaken                         // dst already exists
	CopyFailStalePartial                      // dst+PartialSuffix already exists, left by an earlier copy
)

// ClassifyCopyError classifies a non-nil error returned by CopyHashed(ctx, src, dst).
func ClassifyCopyError(err error, src, dst string) CopyFailure

// SyncDir makes a create or rename inside dir durable: on Unix it fsyncs
// the directory; elsewhere it only checks that dir is a directory. Both
// return an error wrapping fs.ErrNotExist for a missing dir and an error
// for a path that isn't a directory.
func SyncDir(dir string) error
```

- [ ] **Step 1: Update the existing callers and write the failing tests**

Replace every `CopyHashed(` call in `copy_test.go` and `import_test.go` with `CopyHashed(t.Context(), ` (`importOne` and `TestImportFixtureCard` both have `t`). Then add these to `copy_test.go`:

```go
// fakeSource stands in for the opened card file.
type fakeSource struct {
	r        io.Reader
	info     fs.FileInfo
	closeErr error
	onClose  func()
	closed   bool
}

func (f *fakeSource) Read(p []byte) (int, error)  { return f.r.Read(p) }
func (f *fakeSource) Stat() (fs.FileInfo, error) { return f.info, nil }
func (f *fakeSource) Close() error {
	f.closed = true
	if f.onClose != nil {
		f.onClose()
	}
	return f.closeErr
}

type readFunc func([]byte) (int, error)

func (f readFunc) Read(p []byte) (int, error) { return f(p) }

// fakeOpen serves f, with src's real FileInfo (so ModTime and IsRegular work).
func fakeOpen(t *testing.T, src string, f *fakeSource) func(string) (sourceFile, error) {
	t.Helper()
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	f.info = info
	return func(string) (sourceFile, error) { return f, nil }
}

func assertAbsent(t *testing.T, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s exists: %v", p, err)
		}
	}
}

func TestCopyHashedCloseErrorLeavesNoDst(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "content")
	dst := filepath.Join(dir, "out.jpg")
	boom := errors.New("close failed")
	f := &fakeSource{r: strings.NewReader("content"), closeErr: boom}

	_, err := copyHashed(t.Context(), src, dst, fakeOpen(t, src, f))
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want %v", err, boom)
	}
	assertAbsent(t, dst, dst+PartialSuffix)
}

func TestCopyHashedClosesSourceBeforeRename(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "content")
	dst := filepath.Join(dir, "out.jpg")
	f := &fakeSource{r: strings.NewReader("content")}
	f.onClose = func() {
		if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("dst existed when the source was closed: %v", err)
		}
		if _, err := os.Stat(dst + PartialSuffix); err != nil {
			t.Errorf("partial should exist when the source is closed: %v", err)
		}
	}

	sum, err := copyHashed(t.Context(), src, dst, fakeOpen(t, src, f))
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256([]byte("content"))
	if sum != hex.EncodeToString(want[:]) || !f.closed {
		t.Errorf("sum = %s, closed = %v", sum, f.closed)
	}
	if data, err := os.ReadFile(dst); err != nil || string(data) != "content" {
		t.Errorf("dst = %q, %v", data, err)
	}
}

func TestCopyHashedCanceledBeforeStart(t *testing.T) {
	dir := t.TempDir()
	src := writeSourceFile(t, dir, "src.jpg", "content")
	dst := filepath.Join(dir, "out.jpg")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	opened := 0
	open := func(string) (sourceFile, error) { opened++; return nil, errors.New("must not open") }

	_, err := copyHashed(ctx, src, dst, open)
	if !errors.Is(err, context.Canceled) || opened != 0 {
		t.Fatalf("err = %v, opened = %d; want context.Canceled and no open", err, opened)
	}
	assertAbsent(t, dst, dst+PartialSuffix)
}

// Review Focus 4: SIGTERM during a long copy.
func TestCopyHashedCanceledMidCopy(t *testing.T) {
	dir := t.TempDir()
	data := pattern(1 << 20)
	src := writeBytes(t, dir, "src.MP4", data)
	dst := filepath.Join(dir, "out.MP4")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	off, reads := 0, 0
	f := &fakeSource{r: readFunc(func(p []byte) (int, error) {
		reads++
		if reads == 2 {
			cancel() // SIGTERM arrives while the second chunk is being read
		}
		n := copy(p, data[off:])
		off += n
		if n == 0 {
			return 0, io.EOF
		}
		return n, nil
	})}

	_, err := copyHashed(ctx, src, dst, fakeOpen(t, src, f))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if reads != 2 || !f.closed {
		t.Errorf("reads = %d (want 2: no read after the cancel), closed = %v", reads, f.closed)
	}
	assertAbsent(t, dst, dst+PartialSuffix)
}
```

`TestClassifyCopyError`: table-driven, each error produced by a real `CopyHashed` call in a fresh temp dir:
- missing source (`src` = `dir/missing.jpg`, `dst` = `dir/out.jpg`) → `CopyFailSourceMissing`
- missing destination folder (`dst` = `dir/nope/out.jpg`) → `CopyFailDestDirMissing`
- existing `dst` → `CopyFailDestTaken`
- existing `dst+PartialSuffix` → `CopyFailStalePartial`
- a cancelled context → `CopyFailCanceled`
- a source that is a directory → `CopyFailOther`
- also `ClassifyCopyError(errors.New("x"), src, dst)` → `CopyFailOther`, and `errors.Join(fmt.Errorf("wrapped: %w", context.DeadlineExceeded))` → `CopyFailCanceled`

`syncdir_test.go`, `TestSyncDir`: `SyncDir(t.TempDir())` is nil; `SyncDir(filepath.Join(dir, "missing"))` satisfies `errors.Is(err, fs.ErrNotExist)`; `SyncDir` of a regular file returns a non-nil error.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/media/ -run 'CopyHashed|Classify|SyncDir|Import' -v`
Expected: build failure (`undefined: copyHashed`, `undefined: sourceFile`, `undefined: ClassifyCopyError`, `undefined: SyncDir`).

- [ ] **Step 3: Implement**

`copy.go`. `CopyHashed(ctx, src, dst)` is `copyHashed(ctx, src, dst, openSource)`. Keep M1's doc comment and add the ctx and close-before-rename sentences from **Produces**.

```go
// sourceFile is the part of *os.File CopyHashed uses; tests substitute it.
type sourceFile interface {
	io.Reader
	Stat() (fs.FileInfo, error)
	Close() error
}

// openSource must not return a typed-nil *os.File inside the interface.
func openSource(name string) (sourceFile, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func copyHashed(ctx context.Context, src, dst string, open func(string) (sourceFile, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := mustNotExist(dst); err != nil {
		return "", err
	}
	in, err := open(src)
	if err != nil {
		return "", err
	}
	info, err := in.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("copy %s: not a regular file", src)
	}
	if err != nil {
		return "", errors.Join(err, in.Close())
	}
	partial := dst + PartialSuffix
	sum, err := copyToPartial(ctxReader{ctx, in}, partial)
	// Close the card file before the partial becomes dst: nothing may fail
	// after the rename, so that any error means no dst.
	if cerr := in.Close(); cerr != nil {
		if err == nil { // the partial is complete and ours: remove it
			return "", removePartial(partial, cerr)
		}
		err = errors.Join(err, cerr)
	}
	if err != nil {
		return "", err // copyToPartial already removed its own partial
	}
	if err := finishPartial(partial, dst, info.ModTime()); err != nil {
		return "", removePartial(partial, err)
	}
	return sum, nil
}

// ctxReader stops a copy at the next read once ctx is done. It has no
// WriteTo, so io.Copy reads through it chunk by chunk.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
```

`ClassifyCopyError`, checked in this order: `errors.Is` `context.Canceled` or `context.DeadlineExceeded` → `CopyFailCanceled`. `errors.Is(err, fs.ErrExist)`: if `errors.As` finds a `*fs.PathError` whose `Path == dst+PartialSuffix`, then `CopyFailStalePartial`, else `CopyFailDestTaken`. `errors.Is(err, fs.ErrNotExist)` with a `*fs.PathError`: `Path == src` → `CopyFailSourceMissing`, `Path == dst+PartialSuffix` → `CopyFailDestDirMissing`. Anything else → `CopyFailOther`. Extend `CopyHashed`'s doc paragraph about the two `fs.ErrExist` cases to say "`ClassifyCopyError` tells these apart, and also a missing source from a missing destination folder (both wrap `fs.ErrNotExist`)".

`syncdir_unix.go` (`//go:build unix`): `os.Open(dir)`; `Stat`, and if it isn't a directory, return `fmt.Errorf("sync %s: not a directory", dir)`; `f.Sync()`; `Close` with its error joined. `syncdir_other.go` (`//go:build !unix`): `os.Stat(dir)`, return its error, and the same not-a-directory error. Put the doc comment from **Produces** on both, with the Windows reason (Decision 15) on the `!unix` one.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/media/... -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS (including every M1 test), with nothing printed by vet or gofmt.

- [ ] **Step 5: Commit**

```bash
git add internal/media/copy.go internal/media/copy_test.go internal/media/import_test.go internal/media/syncdir_unix.go internal/media/syncdir_other.go internal/media/syncdir_test.go
git commit -m "Close the card file before CopyHashed renames, honour a context, classify copy errors" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Catalog assets, fingerprints, aliases and staging folders

**Files:**
- Create: `internal/catalog/assets.go`, `internal/catalog/staging.go`
- Test: `internal/catalog/assets_test.go`, `internal/catalog/staging_test.go`

**Consumes** (Task 1, package `catalog`; see `schema.go` for the columns):

```go
type DB struct{ db *sql.DB }
var ErrNotFound, ErrBadHash error
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error
func formatTime(t time.Time) string  // UTC "2006-01-02T15:04:05.000Z"
func parseTime(s string) (time.Time, error)
func hexToBlob(s string) ([]byte, error) // wraps ErrBadHash
func blobToHex(b []byte) string
// tests: openTest(t) *DB, hexN(n byte) string
```

From `internal/media`: `type Kind string` with `KindImage = "image"`, `KindRaw = "raw"`, `KindVideo = "video"`.

**Produces:**

```go
type AssetState string

const (
	StateStaged   AssetState = "staged"
	StateArchived AssetState = "archived"
	StateCulled   AssetState = "culled"
	StateMissing  AssetState = "missing"
)

// CaptureLayout is how assets.capture_time stores the naming wall clock.
const CaptureLayout = "2006-01-02T15:04:05"

type Asset struct {
	SHA256      string     `json:"sha256"`       // lowercase hex
	Size        int64      `json:"size"`
	Kind        media.Kind `json:"kind"`
	CaptureTime time.Time  `json:"capture_time,omitzero"` // wall clock; stored without zone, read back in time.Local; zero = NULL
	TZOffset    string     `json:"tz_offset,omitempty"`
	State       AssetState `json:"state"`
	RelPath     string     `json:"rel_path,omitempty"` // "" = NULL
	Mtime       int64      `json:"mtime,omitempty"`    // UnixNano; 0 = NULL
	FirstIngest int64      `json:"first_ingest,omitempty"` // 0 = NULL
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (d *DB) InsertAsset(ctx context.Context, a Asset, quickFP string) error // asset + its fingerprint, one tx; quickFP "" = none
func (d *DB) AssetsByFingerprint(ctx context.Context, quickFP string) ([]Asset, error) // ordered by sha256; nil when none
func (d *DB) AssetBySHA(ctx context.Context, sha string) (Asset, error) // follows aliases one level; ErrNotFound
func (d *DB) AddFingerprint(ctx context.Context, quickFP, sha string) error // idempotent
func (d *DB) AddAlias(ctx context.Context, oldSHA, sha string, at time.Time) error

type StagingFolder struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Dates     []string  `json:"dates"` // sorted YYYY-MM-DD
	CreatedAt time.Time `json:"created_at"`
}

// StagingFolderForDate returns the folder ingest files a capture on date
// (YYYY-MM-DD) into, creating or extending a row as Decision 12 says.
func (d *DB) StagingFolderForDate(ctx context.Context, date string, at time.Time) (StagingFolder, error)
func (d *DB) StagingFolders(ctx context.Context) ([]StagingFolder, error) // ordered by name

// Unexported, used by Task 4's RecordNew inside its transaction:
func insertAsset(ctx context.Context, tx *sql.Tx, a Asset) error
func insertFingerprint(ctx context.Context, tx *sql.Tx, quickFP, sha string) error // INSERT OR IGNORE
```

- [ ] **Step 1: Write the failing tests**

`assets_test.go` (`t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)`; `capture := time.Date(2025, 5, 31, 1, 30, 12, 0, time.Local)`):
- `TestInsertAndLookupAsset`: insert `Asset{SHA256: hexN(1), Size: 598, Kind: media.KindImage, CaptureTime: capture, TZOffset: "+02:00", State: StateStaged, RelPath: "Staging/2025-05-30/2025-05-31 01.30.12.JPG", Mtime: 1748647812000000000, UpdatedAt: t0}` with fingerprint `hexN(2)`. `AssetsByFingerprint(hexN(2))` returns one asset whose fields all match (`CaptureTime.Format(CaptureLayout) == "2025-05-31T01:30:12"`, `UpdatedAt.Equal(t0)`), and `AssetBySHA(hexN(1))` returns the same. `AssetsByFingerprint(hexN(9))` gives `nil, nil`, and `AssetBySHA(hexN(9))` gives `errors.Is(err, ErrNotFound)`.
- `TestInsertAssetNullables`: a culled asset with `RelPath ""`, `Mtime 0`, `FirstIngest 0` and a zero `CaptureTime` reads back with the same zero values, and a raw query confirms `rel_path IS NULL AND mtime IS NULL AND first_ingest IS NULL AND capture_time IS NULL`.
- `TestInsertAssetRejects`: a duplicate `SHA256`, `SHA256 "abc"` (`ErrBadHash`), `Kind "photo"`, `State "deleted"` and a zero `UpdatedAt` each error, and no extra row exists afterwards (`SELECT count(*) FROM assets`).
- `TestFingerprintManyToOne`: asset A (`hexN(1)`) with fingerprint F1, plus `AddFingerprint(F2, A)`, so both fingerprints find A. Asset B (`hexN(3)`) also gets F1 (a collision), and `AssetsByFingerprint(F1)` returns `[A, B]` in sha order.
- `TestAddFingerprintIdempotent`: calling it twice is fine. With an unknown sha it errors (foreign key).
- `TestAssetBySHAFollowsAlias`: `AddAlias(hexN(7), hexN(1), t0)` makes `AssetBySHA(hexN(7))` return asset `hexN(1)`. `AddAlias` to an unknown sha errors.

`staging_test.go` (raw `d.db.ExecContext` inserts set up custom rows, `dates` as JSON text):
- `TestStagingFolderForDateCreates`: on an empty catalog, `StagingFolderForDate("2025-05-30", t0)` gives `{Name: "2025-05-30", Dates: ["2025-05-30"]}` with `ID > 0` and `CreatedAt` equal to `t0`. A second call returns the same `ID`, and `StagingFolders` has exactly one row.
- `TestStagingFolderForDateUsesCoveringFolder`: row `("2025-05-29 - Trip", '["2025-05-29","2025-05-30"]')`. `ForDate("2025-05-30")` returns that row, and no new row is made.
- `TestStagingFolderForDateLowestID`: two rows both covering `2025-05-30` → the lower id.
- `TestStagingFolderForDateAdoptsSameName`: row `("2025-05-31", '["2025-05-30"]')`. `ForDate("2025-05-31")` returns that row with `Dates == ["2025-05-30", "2025-05-31"]`, also persisted.
- `TestStagingFolderForDateRejectsBadDate`: `"2025-5-30"`, `"../x"` and `""` each error.
- `TestStagingFolders`: rows `"b"`, `"a"` come back ordered `a`, `b`, with dates decoded.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/catalog/ -run 'Asset|Fingerprint|Alias|Staging' -v`
Expected: build failure (`undefined: Asset`).

- [ ] **Step 3: Implement `assets.go` and `staging.go`**

One select list, `const assetCols = "sha256, size, kind, capture_time, tz_offset, state, rel_path, mtime, first_ingest, updated_at"`, and one `scanAsset(interface{ Scan(...any) error }) (Asset, error)` that uses `sql.NullString` / `sql.NullInt64` for the nullable columns. Write NULL for the documented zero values. `insertAsset` validates the hash with `hexToBlob` and requires a non-zero `UpdatedAt`; the CHECK constraints catch bad kinds and states. `AssetsByFingerprint`: `SELECT <a.cols> FROM fingerprints f JOIN assets a ON a.sha256 = f.sha256 WHERE f.quick_fp = ? ORDER BY a.sha256`. `AssetBySHA`: assets by sha; on `sql.ErrNoRows`, `SELECT sha256 FROM aliases WHERE old_sha256 = ?` and look that up; else `ErrNotFound`.

`StagingFolderForDate`: validate `date` against `^[0-9]{4}-[0-9]{2}-[0-9]{2}$`, then in one `d.tx`: (1) `SELECT id, name, dates, created_at FROM staging_folders f WHERE EXISTS (SELECT 1 FROM json_each(f.dates) WHERE json_each.value = ?) ORDER BY id LIMIT 1`; (2) else the row `WHERE name = ?`: append the date, sort, `json.Marshal`, and `UPDATE`; (3) else insert `(date, '["<date>"]', formatTime(at))`.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/catalog/ -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS, with nothing printed.

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/assets.go internal/catalog/staging.go internal/catalog/assets_test.go internal/catalog/staging_test.go
git commit -m "Add catalog assets, fingerprints, aliases and staging folders" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Catalog ingests, ingest files, clock warnings and RecordNew

**Files:**
- Create: `internal/catalog/ingests.go`
- Test: `internal/catalog/ingests_test.go`

**Consumes** (Tasks 1 and 3, package `catalog`):

```go
type DB struct{ db *sql.DB }
var ErrNotFound, ErrBadHash error
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) error
func formatTime(t time.Time) string; func parseTime(s string) (time.Time, error)
func hexToBlob(s string) ([]byte, error); func blobToHex(b []byte) string
type Asset struct{ SHA256 string; Size int64; Kind media.Kind; CaptureTime time.Time; TZOffset string; State AssetState; RelPath string; Mtime int64; FirstIngest int64; UpdatedAt time.Time }
const StateStaged, StateArchived, StateCulled, StateMissing AssetState
func (d *DB) InsertAsset(ctx context.Context, a Asset, quickFP string) error
func (d *DB) AssetsByFingerprint(ctx context.Context, quickFP string) ([]Asset, error)
func (d *DB) AssetBySHA(ctx context.Context, sha string) (Asset, error)
func insertAsset(ctx context.Context, tx *sql.Tx, a Asset) error
func insertFingerprint(ctx context.Context, tx *sql.Tx, quickFP, sha string) error
// tests: openTest(t) *DB, hexN(n byte) string
```

**Produces:**

```go
type IngestStatus string

const (
	StatusQueued    IngestStatus = "queued"
	StatusRunning   IngestStatus = "running"
	StatusDone      IngestStatus = "done"
	StatusFailed    IngestStatus = "failed"
	StatusRefused   IngestStatus = "refused"
	StatusCancelled IngestStatus = "cancelled"
)

func (s IngestStatus) Terminal() bool // done, failed, refused, cancelled

type Outcome string

const (
	OutcomeNew         Outcome = "new"
	OutcomeInArchive   Outcome = "in_archive"
	OutcomeInStaging   Outcome = "in_staging"
	OutcomeSeenBefore  Outcome = "seen_before"
	OutcomeFailed      Outcome = "failed"
	OutcomeUnsupported Outcome = "unsupported"
)

// Counts are the outcomes of the ingest_files rows written for one ingest.
type Counts struct {
	Seen        int `json:"seen"` // every outcome except unsupported
	New         int `json:"new"`
	InArchive   int `json:"in_archive"`
	InStaging   int `json:"in_staging"`
	SeenBefore  int `json:"seen_before"`
	Failed      int `json:"failed"`
	Unsupported int `json:"unsupported"`
}

func (c *Counts) Add(o Outcome)

type Ingest struct {
	ID            int64        `json:"id"`
	CardPath      string       `json:"card_path"`
	CardLabel     string       `json:"card_label"`
	CardUUID      string       `json:"card_uuid"`
	CardSize      int64        `json:"card_size"`
	StartedAt     time.Time    `json:"started_at"`
	FinishedAt    time.Time    `json:"finished_at,omitzero"`
	Status        IngestStatus `json:"status"`
	Counts        Counts       `json:"counts"`
	ClockWarnings int          `json:"clock_warnings"`
	SafeToFormat  bool         `json:"safe_to_format"`
	Error         string       `json:"error,omitempty"`
}

type IngestFile struct {
	CardPath string  `json:"card_path"` // slash-separated, relative to the card root
	Size     int64   `json:"size"`
	QuickFP  string  `json:"quick_fp,omitempty"` // "" = NULL
	SHA256   string  `json:"sha256,omitempty"`   // "" = NULL
	Outcome  Outcome `json:"outcome"`
	Error    string  `json:"error,omitempty"`
	RelPath  string  `json:"rel_path,omitempty"` // read only: rel_path of the asset with SHA256 (LEFT JOIN); ignored on write
}

type ClockWarningKind string

const (
	ClockFuture       ClockWarningKind = "future"
	ClockResetDefault ClockWarningKind = "reset_default"
	ClockGPSMismatch  ClockWarningKind = "gps_mismatch"
)

type ClockWarning struct {
	CardPath string           `json:"card_path"`
	SHA256   string           `json:"sha256,omitempty"`
	Kind     ClockWarningKind `json:"kind"`
	Detail   string           `json:"detail"`
}

func (d *DB) CreateIngest(ctx context.Context, cardPath, label, uuid string, at time.Time) (Ingest, error) // status queued, StartedAt = at
func (d *DB) StartIngest(ctx context.Context, id int64, at time.Time) error // queued → running, StartedAt = at; error unless queued
// FinishIngest writes in.Status (must be Terminal), FinishedAt (must be
// non-zero), Counts, ClockWarnings, CardSize, SafeToFormat and Error to row
// in.ID, which must be queued or running (ErrNotFound when there is no row).
func (d *DB) FinishIngest(ctx context.Context, in Ingest) error
func (d *DB) Ingest(ctx context.Context, id int64) (Ingest, error) // ErrNotFound
func (d *DB) Ingests(ctx context.Context, limit int, beforeID int64) ([]Ingest, error) // newest first; limit <= 0 → 50, capped at 500; beforeID 0 = no bound
func (d *DB) FailInterrupted(ctx context.Context, at time.Time) (int64, error) // queued/running → failed
func (d *DB) AddIngestFiles(ctx context.Context, ingestID int64, files []IngestFile) error // one tx
func (d *DB) IngestFiles(ctx context.Context, ingestID int64) ([]IngestFile, error) // insertion order
// RecordNew, in one transaction: inserts a with FirstIngest = ingestID,
// adds quickFP → a.SHA256, and writes f (QuickFP and SHA256 taken from
// quickFP and a; f.Outcome must be OutcomeNew or OutcomeInStaging).
func (d *DB) RecordNew(ctx context.Context, ingestID int64, a Asset, quickFP string, f IngestFile) error
func (d *DB) AddClockWarnings(ctx context.Context, ingestID int64, ws []ClockWarning) error
func (d *DB) ClockWarnings(ctx context.Context, ingestID int64) ([]ClockWarning, error)
```

- [ ] **Step 1: Write the failing tests**

`ingests_test.go` (`t0`, `t1`, `t2` are distinct UTC instants with millisecond precision):
- `TestIngestLifecycle`: `CreateIngest("/cards/SONY_A7IV", "SONY_A7IV", "1234-ABCD", t0)` gives `ID > 0`, `StatusQueued`, `StartedAt.Equal(t0)`, and a zero `FinishedAt`. `StartIngest(id, t1)` makes `Ingest(id)` running with `StartedAt == t1`. `FinishIngest(Ingest{ID: id, Status: StatusDone, FinishedAt: t2, Counts: Counts{Seen: 3, New: 1, InArchive: 1, SeenBefore: 1}, ClockWarnings: 2, CardSize: 1234, SafeToFormat: true})` makes `Ingest(id)` equal, field by field, to the created row with those values (card path, label and uuid unchanged, `Error ""`).
- `TestStartIngestTwice`: a second `StartIngest` errors, and so does one on an unknown id.
- `TestFinishIngestRejects`: `Status: StatusRunning` errors; a zero `FinishedAt` errors; finishing an already-finished row errors and doesn't change it; an unknown id gives `ErrNotFound`.
- `TestIngestsNewestFirst`: create 3 rows. `Ingests(2, 0)` gives ids `[3, 2]`, `Ingests(50, 2)` gives `[1]`, and `Ingests(0, 0)` gives all 3.
- `TestFailInterrupted`: rows queued, running (started) and done (finished). `FailInterrupted(t2)` returns 2. The first two are now `failed` with `Error == "interrupted: SD2NAS stopped before this import finished"` and `FinishedAt == t2`; the done row is unchanged.
- `TestCountsAdd`: adding one of each outcome gives `Counts{Seen: 5, New: 1, InArchive: 1, InStaging: 1, SeenBefore: 1, Failed: 1, Unsupported: 1}`.
- `TestIngestFilesRoundTrip`: archived asset `hexN(1)` at `"Archive/2025-05-30 - Trip/x.JPG"`. `AddIngestFiles` with `{CardPath: "DCIM/100MSDCF/A.JPG", Size: 10, Outcome: OutcomeFailed, Error: "boom"}` and `{CardPath: "DCIM/100MSDCF/B.JPG", Size: 20, QuickFP: hexN(2), SHA256: hexN(1), Outcome: OutcomeInArchive}`. `IngestFiles` returns both in that order: the first with empty `QuickFP`, `SHA256` and `RelPath`, the second with `RelPath "Archive/2025-05-30 - Trip/x.JPG"`. A bad outcome (`"meh"`) or bad hash makes `AddIngestFiles` error and write nothing.
- `TestRecordNew`: `RecordNew(id, Asset{SHA256: hexN(1), …, State: StateStaged, RelPath: "Staging/2025-05-30/2025-05-31 01.30.12.JPG"}, hexN(2), IngestFile{CardPath: "DCIM/100MSDCF/DSC00001.JPG", Size: 598, Outcome: OutcomeNew})`, then `AssetBySHA(hexN(1)).FirstIngest == id`, `AssetsByFingerprint(hexN(2))` holds it, and `IngestFiles(id)` has one row with `QuickFP hexN(2)`, `SHA256 hexN(1)`, and that `RelPath`.
- `TestRecordNewIsAtomic`: insert asset `hexN(1)` first, then `RecordNew` with the same sha and fingerprint `hexN(5)` errors. `IngestFiles(id)` is empty and `AssetsByFingerprint(hexN(5))` is nil. `RecordNew` with `Outcome: OutcomeFailed` errors too.
- `TestClockWarningsRoundTrip`: two warnings come back in order with all fields. An empty `SHA256` is stored as NULL and comes back `""`. Kind `"late"` errors.
- `TestIngestJSON`: `json.Marshal(Ingest{ID: 1, Status: StatusQueued, StartedAt: t0})` contains `"counts":{"seen":0,` and `"safe_to_format":false`, and has no `"finished_at"` key.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/catalog/ -run 'Ingest|Counts|RecordNew|ClockWarnings|FailInterrupted' -v`
Expected: build failure (`undefined: IngestStatus`).

- [ ] **Step 3: Implement `ingests.go`**

Use one ingest select list and one `scanIngest`. `StartIngest`: `UPDATE … SET status='running', started_at=? WHERE id=? AND status='queued'`, and zero rows affected is an error (`ErrNotFound` when no row has that id). `FinishIngest`: validate, then `UPDATE … WHERE id=? AND status IN ('queued','running')`; on zero rows, `ErrNotFound` if the id doesn't exist, else `fmt.Errorf("ingest %d is already finished", id)`. `IngestFiles`: `SELECT f.card_path, f.size, f.quick_fp, f.sha256, f.outcome, f.error, COALESCE(a.rel_path, '') FROM ingest_files f LEFT JOIN assets a ON a.sha256 = f.sha256 WHERE f.ingest_id = ? ORDER BY f.rowid`. An empty hash string is written as NULL. `RecordNew` uses `d.tx` with `insertAsset`, `insertFingerprint` and an ingest_files insert.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/catalog/ -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS, with nothing printed.

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/ingests.go internal/catalog/ingests_test.go
git commit -m "Add catalog ingest history, per-file outcomes and clock warnings" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Card scan, partial cleanup, free space and durable mkdir

**Files:**
- Create: `internal/ingest/scan.go`, `internal/ingest/partials.go`, `internal/ingest/fsutil.go`, `internal/ingest/freespace_statfs.go`, `internal/ingest/freespace_windows.go`, `internal/ingest/freespace_other.go`, `internal/media/mediatest/card.go`
- Test: `internal/ingest/scan_test.go`, `internal/ingest/partials_test.go`, `internal/ingest/fsutil_test.go`, `internal/ingest/freespace_test.go`, `internal/ingest/helpers_test.go`, `internal/media/mediatest/card_test.go`

**Consumes** (`internal/media`):

```go
type Kind string // KindImage "image", KindRaw "raw", KindVideo "video"
type Rules struct{ ImageFormats, RawFormats, VideoFormats []string; ImageSubdir, RawSubdir, VideoSubdir string }
func DefaultRules() Rules
func (r Rules) KindOf(name string) (Kind, bool) // by extension, case-insensitive
const PartialSuffix = ".partial"
func SyncDir(dir string) error // Task 2: fsync on Unix; checks it's a directory elsewhere
```

`internal/media/mediatest` constants: `SonyJPEG = "cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.JPG"`, `SonyARW = "cards/SONY_A7IV/DCIM/100MSDCF/DSC00001.ARW"`, `GoProMP4 = "cards/GOPRO/DCIM/100GOPRO/GX010001.MP4"` (relative to the repo's `testdata/`). `mediatest` must never import `internal/media`.

**Produces:**

```go
package ingest // doc: "Package ingest imports a mounted camera card into Staging: scan, classify against the catalog, copy what is new, and record it."

const StagingDir = "Staging" // under the photos root

type CardFile struct {
	Path string     // OS path
	Rel  string     // slash-separated, relative to the card root
	Size int64
	Kind media.Kind // "" for unsupported files
}

type ScanError struct {
	Rel string // the entry that couldn't be read
	Err error
}

type ScanResult struct {
	Media       []CardFile  // files the rules import, sorted by Rel
	Unsupported []CardFile  // media formats the rules don't import (Decision 10), sorted by Rel
	Errors      []ScanError // unreadable entries below the root
}

func Scan(root string, rules media.Rules) (ScanResult, error) // error only when root itself is unusable
func CleanPartials(stagingRoot string) ([]string, error)       // removed files, slash rel to stagingRoot, sorted; missing root = nil, nil
func FreeSpace(path string) (uint64, error)                    // bytes available to this user on path's filesystem
func mkdirDurable(dir string) error                            // unexported; Tasks 7 and 8 call it

// in internal/media/mediatest/card.go:
// CopyCard copies the committed card testdata/cards/<label> into
// dst/<label>, keeping modified times, and returns the new card root.
// testdata is the repo's testdata directory (tests at internal/X pass
// filepath.Join("..", "..", "testdata")).
func CopyCard(testdata, label, dst string) (string, error)
```

- [ ] **Step 1: Write the failing tests**

`helpers_test.go` (package `ingest`):

```go
// writeFile writes root/rel (slash-separated), creating parent folders.
func writeFile(t *testing.T, root, rel string, data []byte) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}
```

`scan_test.go`, `TestScanSkipsJunk` (Review Focus 5). Write `[]byte("x")` at each of these paths under `card := t.TempDir()`:

```go
keep := []string{ // want in Media, in this order
	"DCIM/100GOPRO/GX010001.MP4",
	"DCIM/100MSDCF/DSC00001.ARW",
	"DCIM/100MSDCF/DSC00001.JPG",
	"PRIVATE/M4ROOT/CLIP/C0001.MP4",
	"SUB/keep.JPG", // SUB is only skipped inside M4ROOT
}
unsupported := []string{"DCIM/100MSDCF/DSC00002.HIF"}
junk := []string{
	"DCIM/100MSDCF/._DSC00001.JPG",
	"DCIM/100MSDCF/.DS_Store",
	"DCIM/.Trashes/501/DSC00009.JPG",
	".Trashes/501/DSC00010.JPG",
	".Spotlight-V100/Store-V2/x.JPG",
	"MISC/THM00001.JPG",
	"misc/lower.JPG",
	"DCIM/100GOPRO/GL010001.LRV",
	"DCIM/100GOPRO/GX010001.THM",
	"DCIM/100GOPRO/.thumbnails/x.JPG",
	"PRIVATE/M4ROOT/THMBNL/C0001T01.JPG",
	"PRIVATE/M4ROOT/SUB/C0001S03.MP4",
	"PRIVATE/M4ROOT/CLIP/C0001M01.XML",
	"System Volume Information/x.jpg",
	"$RECYCLE.BIN/x.jpg",
}
```

Then `Scan(card, media.DefaultRules())` has `Media` Rels equal to `keep`, the `Unsupported` Rels equal to `unsupported`, no `Errors`, and every `Size == 1`. Kinds: MP4 → video, ARW → raw, JPG → image, and the unsupported file's kind is `""`. Each `Path` equals `filepath.Join(card, filepath.FromSlash(Rel))`.
- `TestScanSkipsSymlinks`: `os.Symlink(outsideJPG, filepath.Join(card, "DCIM", "link.JPG"))` (call `t.Skip` if creating the symlink fails, as it does on Windows without the privilege) leaves `Media` empty.
- `TestScanRootErrors`: a missing root gives an error with `errors.Is(err, fs.ErrNotExist)`; a root that is a regular file gives an error.

`partials_test.go`:
- `TestCleanPartials`: under `staging`, write `2025-05-30/a.JPG.partial`, `2025-05-30/Capture/b.ARW.partial`, `2025-05-30/c.JPG`, and a directory `x.partial/`. `CleanPartials` returns `["2025-05-30/Capture/b.ARW.partial", "2025-05-30/a.JPG.partial"]`, and both are gone, while `c.JPG` and the directory `x.partial` still exist.
- `TestCleanPartialsMissingRoot`: `CleanPartials(filepath.Join(t.TempDir(), "Staging"))` gives `nil, nil`.

`fsutil_test.go`, `TestMkdirDurable`: it creates `a/b/c` under a temp dir (a directory afterwards), succeeds again on an existing directory, and fails when `a/file` is a regular file and the target is `a/file/d`.

`freespace_test.go`, `TestFreeSpace`: `FreeSpace(t.TempDir())` is greater than 0 with a nil error, and a missing path errors.

`mediatest/card_test.go`, `TestCopyCard`: `CopyCard(filepath.Join("..", "..", "..", "testdata"), "SONY_A7IV", t.TempDir())` returns `<dst>/SONY_A7IV`, which contains `DCIM/100MSDCF/DSC00001.JPG` and `.ARW` with the same bytes and `ModTime` as the originals.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/ingest/ ./internal/media/mediatest/ -run 'Scan|Partials|Mkdir|FreeSpace|CopyCard' -v`
Expected: build failure (`undefined: Scan`, `undefined: CopyCard`).

- [ ] **Step 3: Implement**

`Scan`: `os.Stat(root)` must succeed and be a directory. Then `filepath.WalkDir(root, …)`. An error on the root entry is returned. Below the root, an error becomes a `ScanError` with the entry's slash Rel, plus `fs.SkipDir` for a directory. Skip directories by Decision 11: a name starting with `.`, or `strings.EqualFold` to `MISC`, `System Volume Information`, `$RECYCLE.BIN` or `THMBNL`, or `SUB` whose parent directory's name is `M4ROOT` (EqualFold). Skip files whose name starts with `.` and any entry where `!d.Type().IsRegular()`. Classify with `rules.KindOf` first, then the lowercase extension against the Decision 10 set (a package-level `map[string]bool`). Get `Size` from `d.Info()`; an error there becomes a `ScanError`. Sort both slices by `Rel`.

`CleanPartials`: return `nil, nil` when `stagingRoot` doesn't exist. Otherwise walk it, `os.Remove` every regular file whose name ends in `media.PartialSuffix`, and return walk and remove errors wrapped as `clean partials: …`.

`mkdirDurable(dir)`: walk up from `dir` collecting the components that don't exist (any `Stat` error other than `fs.ErrNotExist` is returned). Then, from the top down, `os.Mkdir(p, 0o777)` (UMASK decides; an `fs.ErrExist` for an existing directory is fine), followed by `media.SyncDir(filepath.Dir(p))`. A path component that exists but isn't a directory is an error.

`freespace_statfs.go` (`//go:build linux || darwin`):

```go
func FreeSpace(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, &fs.PathError{Op: "statfs", Path: path, Err: err}
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil // Bsize is int64 on linux, uint32 on darwin
}
```

`freespace_windows.go` (standard library only):

```go
var procGetDiskFreeSpaceExW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

func FreeSpace(path string) (uint64, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var avail uint64
	r, _, callErr := procGetDiskFreeSpaceExW.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)), 0, 0)
	if r == 0 {
		return 0, &fs.PathError{Op: "GetDiskFreeSpaceEx", Path: path, Err: callErr}
	}
	return avail, nil
}
```

`freespace_other.go` (`//go:build !linux && !darwin && !windows`): return `0, fmt.Errorf("free space of %s: %w", path, errors.ErrUnsupported)`.

`CopyCard`: walk `filepath.Join(testdata, "cards", label)`, recreate directories, copy each regular file, close both files (joining close errors), then `os.Chtimes` with the source's mtime.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/ingest/ ./internal/media/mediatest/ -v`, then `GOOS=linux go vet ./internal/ingest/` and `GOOS=darwin go vet ./internal/ingest/` (so every build-tagged file at least type-checks), then `go vet ./...` and `gofmt -l .`
Expected: all PASS, with nothing printed.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/ internal/media/mediatest/card.go internal/media/mediatest/card_test.go
git commit -m "Add card scan with junk skipping, partial cleanup, free space and durable mkdir" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Clock checks and the two-line summary

**Files:**
- Create: `internal/ingest/clock.go`, `internal/ingest/describe.go`
- Test: `internal/ingest/clock_test.go`, `internal/ingest/describe_test.go`

**Consumes:**

```go
// internal/media
type TimeSource string // SourceExif "exif", SourceMvhd "mvhd", SourceMtime "mtime"
type Capture struct {
	Time     time.Time  // wall-clock capture time in time.Local (EXIF), or an instant converted to time.Local (mvhd, mtime)
	Source   TimeSource
	Offset   string     // EXIF OffsetTimeOriginal, e.g. "+02:00"; "" when unknown
	GPS      time.Time  // EXIF GPS timestamp in UTC; zero when absent
	Camera   string
	Fallback error
}

// internal/catalog (Task 4)
type ClockWarningKind string // ClockFuture "future", ClockResetDefault "reset_default", ClockGPSMismatch "gps_mismatch"
type ClockWarning struct{ CardPath, SHA256 string; Kind ClockWarningKind; Detail string }
type IngestStatus string // StatusQueued, StatusRunning, StatusDone, StatusFailed, StatusRefused, StatusCancelled
type Counts struct{ Seen, New, InArchive, InStaging, SeenBefore, Failed, Unsupported int }
type Ingest struct {
	ID int64; CardPath, CardLabel, CardUUID string; CardSize int64
	StartedAt, FinishedAt time.Time; Status IngestStatus; Counts Counts
	ClockWarnings int; SafeToFormat bool; Error string
}
```

**Produces:**

```go
const (
	FutureTolerance = 10 * time.Minute
	GPSTolerance    = 5 * time.Minute
	ResetBeforeYear = 2005
)

// CheckClock returns the §12 warnings for one new file, in the order
// future, reset_default, gps_mismatch. CardPath and SHA256 are left for
// the caller to fill.
func CheckClock(c media.Capture, now time.Time) []catalog.ClockWarning

const (
	LevelNormal  = "normal"
	LevelWarning = "warning"
	LevelAlert   = "alert"
)

// Describe returns the two lines the card script turns into an Unraid
// notification: a level and a one-line message (no control characters).
func Describe(in catalog.Ingest) (level, message string)

// CardName is the card's display name: the label (with control characters
// replaced by spaces and trimmed), else the last element of CardPath, else "card".
func CardName(in catalog.Ingest) string

// unexported, used by Task 7:
func formatGB(b uint64) string            // fmt.Sprintf("%.1f GB", float64(b)/1e9)
func plural(n int, one, many string) string // one when n == 1, else many
```

- [ ] **Step 1: Write the failing tests**

`clock_test.go`, `TestCheckClock`, table-driven. Every case uses `now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)`, and capture times are built in UTC so `time.Local` never matters. `wall := time.Date(2025, 5, 31, 1, 30, 12, 0, time.UTC)`, `gps := time.Date(2025, 5, 30, 23, 30, 12, 0, time.UTC)`.

| Capture | Want (kind: detail) |
|---|---|
| `{Time: wall, Source: SourceExif, Offset: "+02:00", GPS: gps}` | none (01:30:12+02:00 is 23:30:12Z) |
| same, GPS `gps.Add(-4*time.Minute - 59*time.Second)` | none |
| same, GPS `gps.Add(-5*time.Minute - time.Second)` | `gps_mismatch: camera clock is 5m0s ahead of GPS time` |
| same, GPS `gps.Add(time.Hour)` | `gps_mismatch: camera clock is 1h0m0s behind GPS time` |
| `{Time: wall, Source: SourceExif, GPS: gps}` (no offset, so the instant is `wall` itself) | `gps_mismatch: camera clock is 2h0m0s ahead of GPS time` |
| `{Time: time.Date(2026, 1, 1, 12, 10, 0, 0, time.UTC), Source: SourceExif}` | none (exactly at the tolerance) |
| `{Time: time.Date(2026, 1, 1, 12, 10, 1, 0, time.UTC), Source: SourceMtime}` | `future: capture time 2026-01-01 12:10:01 is after the NAS time 2026-01-01 12:00:00` |
| `{Time: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Source: SourceExif}` | `reset_default: capture time 2000-01-01 00:00:00 is before 2005: the camera clock was probably reset` |
| `{Time: time.Date(2005, 1, 1, 0, 0, 0, 0, time.UTC), Source: SourceExif}` | none |
| `{Time: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), Source: SourceExif, GPS: time.Date(2025, 5, 30, 0, 0, 0, 0, time.UTC)}` | `reset_default` then `gps_mismatch` (in that order) |
| `{Time: wall, Source: SourceExif, Offset: "garbage", GPS: gps}` | same as the no-offset row (an unparsable offset counts as unknown) |

Each returned warning has an empty `CardPath` and `SHA256`.

`describe_test.go`, `TestDescribe`, table-driven, with `base := catalog.Ingest{CardLabel: "SONY_A7IV", CardPath: "/cards/SONY_A7IV"}`:

| Ingest (fields set on base) | Level | Message |
|---|---|---|
| done, Seen 3, New 2, InArchive 1, safe | normal | `SONY_A7IV: 2 new, 1 already known. Card safe to format.` |
| done, Seen 2, InStaging 2, safe | normal | `SONY_A7IV: 0 new, 2 already known. Card safe to format.` |
| done, Seen 3, New 1, InStaging 1, Failed 1 | warning | `SONY_A7IV: 1 new, 1 already known, 1 failed. Do not format the card.` |
| done, Seen 1, New 1, Unsupported 4 | warning | `SONY_A7IV: 1 new, 0 already known, 4 in formats SD2NAS does not import. Do not format the card.` |
| done, Seen 2, New 2, safe, ClockWarnings 1 | warning | `SONY_A7IV: 2 new, 0 already known. Card safe to format. Check the camera clock: 1 file has suspicious capture times.` |
| done, Seen 2, New 2, safe, ClockWarnings 3 | warning | `… Check the camera clock: 3 files have suspicious capture times.` |
| done, Seen 0, Unsupported 0 | warning | `SONY_A7IV: no photos or videos found. Do not format the card.` |
| refused, Error `staging would drop below the 50.0 GB free-space reserve (2 new files need 0.1 GB, 40.0 GB free)` | alert | `SONY_A7IV: not imported, staging would drop below the 50.0 GB free-space reserve (2 new files need 0.1 GB, 40.0 GB free). Card left mounted.` |
| failed, New 5, Error `card removed during import` | alert | `SONY_A7IV: import failed: card removed during import. 5 new copied before it stopped. Card left mounted.` |
| cancelled, New 1 | alert | `SONY_A7IV: import cancelled because SD2NAS stopped. 1 new copied. Card left mounted.` |
| queued | normal | `SONY_A7IV: waiting for another import to finish.` |
| running | normal | `SONY_A7IV: importing.` |

Plus:
- `TestDescribeOneLine`: a failed ingest with `Error "a\nb"` and `CardLabel "A\tB"` gives a message starting `A B: import failed: a b.`, and no message in the table contains `\n` or `\r`.
- `TestCardName`: `CardLabel ""` with `CardPath "/cards/GOPRO"` gives `GOPRO` (also with a Windows path `C:\cards\GOPRO`, through `filepath.Base`); both empty gives `card`; `"  SONY \x00"` gives `SONY`.
- `TestFormatGB`: 0 gives `"0.0 GB"`, 50e9 gives `"50.0 GB"`, 812 gives `"0.0 GB"`, 123_456_789_012 gives `"123.5 GB"`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/ingest/ -run 'Clock|Describe|CardName|FormatGB' -v`
Expected: build failure (`undefined: CheckClock`).

- [ ] **Step 3: Implement**

`clock.go`: the capture instant is, when `c.Source == media.SourceExif` and `c.Offset` matches `^[+-]([01][0-9]|2[0-3]):[0-5][0-9]$`, `c.Time`'s wall-clock fields in `time.FixedZone("", ±(h*3600+m*60))`; otherwise `c.Time`. Future: `instant.After(now.Add(FutureTolerance))`, with the detail formatting `instant.In(now.Location())` and `now` using `"2006-01-02 15:04:05"`. Reset: `c.Time.Year() < ResetBeforeYear` (wall clock). GPS: when `!c.GPS.IsZero()`, `d := instant.Sub(c.GPS)`; warn if `|d| > GPSTolerance`, with detail `fmt.Sprintf("camera clock is %s %s GPS time", abs(d).Round(time.Minute), "ahead of"|"behind")` (`d > 0` means ahead of).

`describe.go`: `known := InArchive + InStaging + SeenBefore`. Build the messages exactly as the table shows. "Card safe to format." comes from `SafeToFormat`, not recomputed. The level for done is normal only when `SafeToFormat && ClockWarnings == 0`. Pass the final message through the same control-character → space replacement as `CardName`, and trim it.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/ingest/ -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS, with nothing printed.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/clock.go internal/ingest/describe.go internal/ingest/clock_test.go internal/ingest/describe_test.go
git commit -m "Add camera clock checks and the ingest summary text" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: The ingest run: classify, space check, copy and record

**Files:**
- Create: `internal/ingest/run.go`, `internal/ingest/progress.go`
- Test: `internal/ingest/run_test.go`, `internal/ingest/progress_test.go`; extend `internal/ingest/helpers_test.go`

**Consumes:**

```go
// internal/media
func DefaultRules() Rules; func TypeSubdir(kind Kind, r Rules) string
const DefaultDayStartsAt = 4 * time.Hour
func QuickFingerprint(path string) (string, error); func HashFile(path string) (string, error) // lowercase hex
func CaptureTime(path string, kind Kind) (Capture, error) // errors only when even the mtime can't be read
func ReadVideoMeta(path string) (*VideoMeta, error)
func FolderDate(t time.Time, dayStartsAt time.Duration) string // "YYYY-MM-DD"
func DestFileName(t time.Time, kind Kind, meta *VideoMeta, ext string) string
func FreeName(src, dir, name string) (dst string, duplicate bool, err error) // collision loop; duplicate = identical content already at dst
func CopyHashed(ctx context.Context, src, dst string) (string, error) // Task 2
func SyncDir(dir string) error // Task 2
const PartialSuffix = ".partial"

// internal/catalog (Tasks 1, 3, 4)
func (d *DB) Settings(ctx) (Settings, error) // Settings{DayStartsAt time.Duration; VerifyKnown VerifyMode; ReserveBytes uint64}; VerifyQuick / VerifyFull
func (d *DB) SetSetting(ctx, key, value string) error // keys KeyDayStartsAt, KeyVerifyKnown, KeyFreeSpaceReserveGB
func (d *DB) AssetsByFingerprint(ctx, quickFP string) ([]Asset, error)
func (d *DB) AssetBySHA(ctx, sha string) (Asset, error) // ErrNotFound
func (d *DB) AddFingerprint(ctx, quickFP, sha string) error
func (d *DB) InsertAsset(ctx, a Asset, quickFP string) error
func (d *DB) StagingFolderForDate(ctx, date string, at time.Time) (StagingFolder, error) // StagingFolder{ID, Name, Dates, CreatedAt}
func (d *DB) StagingFolders(ctx) ([]StagingFolder, error)
func (d *DB) CreateIngest(ctx, cardPath, label, uuid string, at time.Time) (Ingest, error)
func (d *DB) StartIngest(ctx, id int64, at time.Time) error
func (d *DB) FinishIngest(ctx, in Ingest) error
func (d *DB) Ingest(ctx, id int64) (Ingest, error)
func (d *DB) AddIngestFiles(ctx, ingestID int64, files []IngestFile) error
func (d *DB) IngestFiles(ctx, ingestID int64) ([]IngestFile, error)
func (d *DB) RecordNew(ctx, ingestID int64, a Asset, quickFP string, f IngestFile) error
func (d *DB) AddClockWarnings(ctx, ingestID int64, ws []ClockWarning) error
func (d *DB) ClockWarnings(ctx, ingestID int64) ([]ClockWarning, error)
// Asset{SHA256, Size, Kind, CaptureTime, TZOffset, State, RelPath, Mtime, FirstIngest, UpdatedAt}; CaptureLayout = "2006-01-02T15:04:05"
// AssetState: StateStaged, StateArchived, StateCulled, StateMissing
// Outcome: OutcomeNew, OutcomeInArchive, OutcomeInStaging, OutcomeSeenBefore, OutcomeFailed, OutcomeUnsupported
// Counts{Seen, New, InArchive, InStaging, SeenBefore, Failed, Unsupported}, (*Counts).Add(Outcome)
// IngestFile{CardPath, Size, QuickFP, SHA256, Outcome, Error, RelPath}
// IngestStatus: StatusDone, StatusFailed, StatusRefused, StatusCancelled, …

// internal/ingest (Tasks 5, 6)
const StagingDir = "Staging"
type CardFile struct{ Path, Rel string; Size int64; Kind media.Kind }
type ScanResult struct{ Media, Unsupported []CardFile; Errors []ScanError } // ScanError{Rel string; Err error}
func Scan(root string, rules media.Rules) (ScanResult, error)
func CleanPartials(stagingRoot string) ([]string, error)
func FreeSpace(path string) (uint64, error)
func mkdirDurable(dir string) error
func CheckClock(c media.Capture, now time.Time) []catalog.ClockWarning
func CardName(in catalog.Ingest) string
func formatGB(b uint64) string
func writeFile(t, root, rel string, data []byte) string // helpers_test.go

// internal/media/mediatest
func CopyCard(testdata, label, dst string) (string, error)
type ExifSpec struct{ Make, Model, DateTimeOriginal, OffsetTimeOriginal, GPSDate string; GPSTime [3][2]uint32 }
func JPEG(s ExifSpec) ([]byte, error)
var SonyExif ExifSpec // 2025:05:31 01:30:12, +02:00, GPS 2025:05:30 23:30:12
```

**Produces:**

```go
// Request is what the card script posts.
type Request struct {
	Path  string // card root inside the container, e.g. /cards/SONY_A7IV
	Label string
	UUID  string
}

type Config struct {
	Catalog    *catalog.DB
	PhotosRoot string                            // the photos share; Staging is PhotosRoot/Staging
	Now        func() time.Time                  // nil: time.Now
	FreeSpace  func(path string) (uint64, error) // nil: FreeSpace
	Log        *slog.Logger                      // nil: slog.Default()
}

type Runner struct {
	cfg Config
	// Seams, set by NewRunner to the media functions; package tests replace
	// them to inject failures (Task 8) or block (Task 9).
	fingerprint func(path string) (string, error)                         // media.QuickFingerprint
	hashFile    func(path string) (string, error)                         // media.HashFile
	copyFile    func(ctx context.Context, src, dst string) (string, error) // media.CopyHashed
}

func NewRunner(cfg Config) *Runner
func (r *Runner) now() time.Time                         // unexported: cfg.Now or time.Now; Task 9 uses it
func (r *Runner) log() *slog.Logger                      // unexported: cfg.Log or slog.Default(); Task 9 uses it
func (r *Runner) freeSpace(path string) (uint64, error)  // unexported: cfg.FreeSpace or FreeSpace
func (j *ingestRun) cat() *catalog.DB                    // unexported: j.r.cfg.Catalog

// Run imports the card of in (a queued row from CreateIngest) and returns
// the finished row. The outcome, including failure, is in the row; the
// error is non-nil only when the catalog couldn't record the start or the
// finish.
func (r *Runner) Run(ctx context.Context, in catalog.Ingest, p *Progress) (catalog.Ingest, error)

const (
	PhaseQueued      = "queued"
	PhaseScanning    = "scanning"
	PhaseClassifying = "classifying"
	PhaseCopying     = "copying"
	PhaseFinishing   = "finishing"
	PhaseDone        = "done"
)

type ProgressSnapshot struct {
	Phase      string         `json:"phase"`
	FilesTotal int            `json:"files_total"`
	FilesDone  int            `json:"files_done"`
	BytesTotal int64          `json:"bytes_total"`
	BytesDone  int64          `json:"bytes_done"`
	Counts     catalog.Counts `json:"counts"`
}

// Progress is a running ingest's live state, safe for concurrent use.
type Progress struct {
	mu sync.Mutex
	s  ProgressSnapshot
}

func NewProgress() *Progress                      // Phase = PhaseQueued
func (p *Progress) Snapshot() ProgressSnapshot
func (p *Progress) update(fn func(*ProgressSnapshot)) // unexported
```

- [ ] **Step 1: Write the failing tests**

Add to `helpers_test.go`:

```go
var testNow = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

type env struct {
	r      *Runner
	cat    *catalog.DB
	photos string
	cards  string
	free   uint64 // what FreeSpace reports
}

func newEnv(t *testing.T) *env {
	t.Helper()
	cfgDir := t.TempDir()
	cat, err := catalog.Open(filepath.Join(cfgDir, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cat.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	e := &env{cat: cat, photos: t.TempDir(), cards: t.TempDir(), free: 1e12}
	e.r = NewRunner(Config{
		Catalog:    cat,
		PhotosRoot: e.photos,
		Now:        func() time.Time { return testNow },
		FreeSpace:  func(string) (uint64, error) { return e.free, nil },
		Log:        slog.New(slog.DiscardHandler),
	})
	return e
}

// fixtureCard copies a committed card (SONY_A7IV or GOPRO) into e.cards.
func (e *env) fixtureCard(t *testing.T, label string) string {
	t.Helper()
	card, err := mt.CopyCard(filepath.Join("..", "..", "testdata"), label, e.cards)
	if err != nil {
		t.Fatal(err)
	}
	return card
}

// run creates the queued row for card and runs it to the end.
func (e *env) run(t *testing.T, card string) catalog.Ingest {
	t.Helper()
	in, err := e.cat.CreateIngest(t.Context(), card, filepath.Base(card), "1234-ABCD", testNow)
	if err != nil {
		t.Fatal(err)
	}
	out, err := e.r.Run(t.Context(), in, NewProgress())
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// staged lists regular files under photos/Staging as slash paths relative
// to photos, sorted; nil when Staging doesn't exist.
func (e *env) staged(t *testing.T) []string

// files maps CardPath → IngestFile for an ingest.
func (e *env) files(t *testing.T, id int64) map[string]catalog.IngestFile

// writeJPEG writes a synthesized JPEG carrying s at card/rel.
func writeJPEG(t *testing.T, card, rel string, s mt.ExifSpec) string
```

`run_test.go`. Sony fixture names: `sonyJPEG = "Staging/2025-05-30/2025-05-31 01.30.12.JPG"`, `sonyARW = "Staging/2025-05-30/Capture/2025-05-31 01.30.12.ARW"`.
- `TestRunFixtureCard`: `card := e.fixtureCard(t, "SONY_A7IV")`; record the `QuickFingerprint` of both card files. `out := e.run(t, card)` gives `StatusDone`, `Counts{Seen: 2, New: 2}`, `SafeToFormat` true, `ClockWarnings` 0, a non-zero `FinishedAt`, `CardSize` equal to the sum of both card files' sizes, and `Error ""`. `e.staged(t)` is `[sonyJPEG, sonyARW]`, and each staged file's bytes equal its source. `StagingFolders` is `[{Name: "2025-05-30", Dates: ["2025-05-30"]}]`. `AssetBySHA(HashFile(card JPG))` has `State` staged, `RelPath == sonyJPEG`, `TZOffset "+02:00"`, `Kind` image, `FirstIngest == out.ID`, `CaptureTime.Format(catalog.CaptureLayout) == "2025-05-31T01:30:12"`, and `Mtime` equal to the staged file's `ModTime().UnixNano()`. `IngestFiles` has two `new` rows with `RelPath` set. The card files' fingerprints are unchanged.
- `TestRunGoProCard`: with `local := time.Date(2025, 5, 31, 8, 30, 12, 0, time.UTC).In(time.Local)`, `staged` is `[path.Join("Staging", media.FolderDate(local, media.DefaultDayStartsAt), "Videos", local.Format("2006-01-02 15.04.05")+" - 3840w - 29.970 - 12sec.MP4")]`.
- `TestRunRerunImportsNothing` (Review Focus 2): run the Sony card twice. The second run gives `Counts{Seen: 2, InStaging: 2}`, `New 0`, safe true. `staged` and `StagingFolders` are unchanged, and the second ingest's rows are `in_staging` with `RelPath` `sonyJPEG` / `sonyARW`.
- `TestRunKnownOutcomes`: a card with three JPEGs (`writeJPEG` with DTO `"2025:06:01 10:00:00"`, `…:01`, `…:02`, distinct bytes) under `DCIM/100MSDCF/`. Seed each with `InsertAsset` (sha `HashFile`, fp `QuickFingerprint`, `UpdatedAt: testNow`) as `archived` (`RelPath "Archive/2025-06-01 - Hike/2025-06-01 10.00.00.JPG"`), `culled` (no path) and `missing`. Expected: `Counts{Seen: 3, InArchive: 1, SeenBefore: 2}`, safe true, `staged` nil, and the archived row's `RelPath` is the archive path.
- `TestRunVerifyFull`: seed `InsertAsset(Asset{SHA256: strings.Repeat("bb", 32), State: archived, …}, QuickFingerprint(card JPG))`, a fingerprint collision with different content. With the default `quick` setting the JPG is `in_archive` and not copied (subtest 1, fresh env). With `SetSetting(KeyVerifyKnown, "full")` the JPG is `new` and staged (subtest 2, fresh env).
- `TestRunDuplicateOnCard`: the same `mt.JPEG(mt.SonyExif)` bytes at `DCIM/100MSDCF/DSC00001.JPG` and `DCIM/101MSDCF/DSC00001.JPG` give `Counts{Seen: 2, New: 1, InStaging: 1}`, one staged file, safe true.
- `TestRunCopyMatchesExistingAsset` (§6 row 3): `InsertAsset` archived with sha = `HashFile(card JPG)` but fingerprint `strings.Repeat("aa", 32)`, so the quick lookup misses. Run the Sony card: the JPG is `in_archive`, `staged` is `[sonyARW]` (the JPG copy was deleted), `AssetsByFingerprint(QuickFingerprint(card JPG))` now returns the archived asset, and safe is true.
- `TestRunBurstOrder` (Review Focus 7): three JPEGs with DTO `"2025:06:01 10:00:00"` and Models `"A"`, `"B"`, `"C"`, written in this order as `DSC00003.JPG` (A), `DSC00001.JPG` (B), `DSC00002.JPG` (C) under `DCIM/100MSDCF`. The `files` map gives `DSC00001.JPG → "Staging/2025-06-01/2025-06-01 10.00.00.JPG"`, `DSC00002.JPG → "…10.00.00-1.JPG"`, `DSC00003.JPG → "…10.00.00-2.JPG"`.
- `TestRunDayCutoffSetting`: `SetSetting(KeyDayStartsAt, "00:00")` puts the Sony JPG at `"Staging/2025-05-31/2025-05-31 01.30.12.JPG"`.
- `TestRunRefusedBelowReserve` (Review Focus 6): `e.free = 50e9 + 100`. The Sony run gives `StatusRefused`, `Counts{}` (zero: no known or failed files), safe false, `staged` nil, and `Error == "staging would drop below the 50.0 GB free-space reserve (2 new files need 0.0 GB, 50.0 GB free)"`. Then `SetSetting(KeyFreeSpaceReserveGB, "0")` and running again gives done, New 2.
- `TestRunNoSpaceCheckWithoutNewFiles`: run the Sony card, set `e.free = 0`, and run again: done, InStaging 2, safe true.
- `TestRunUnsupportedFormatsNotSafe` (Review Focus 1): the Sony card plus `DCIM/100MSDCF/DSC00002.HIF` gives `Counts{Seen: 2, New: 2, Unsupported: 1}`, safe false, and an `unsupported` row for `DCIM/100MSDCF/DSC00002.HIF` with its size.
- `TestRunEmptyCardNotSafe` (Review Focus 1): a card holding only `DCIM/100MSDCF/._DSC00001.JPG` gives done, `Counts{}`, safe false.
- `TestRunClockWarnings`: JPEGs with DTO `"2000:01:01 00:00:00"` (reset), `"2099:01:01 12:00:00"` (future), and `{DTO "2025:05:31 01:30:12", Offset "+02:00", GPSDate "2025:05:30", GPSTime {{22,1},{30,1},{12,1}}}` (an hour off) give `ClockWarnings 3` and safe true. `catalog.ClockWarnings(out.ID)` holds the kinds `reset_default`, `future` and `gps_mismatch`, each with the right `CardPath` and `SHA256` set to the staged asset's sha.
- `TestRunMissingCard`: a `CardPath` that doesn't exist gives `StatusFailed` and an `Error` with the prefix `"scan card: "`.

`progress_test.go`, `TestProgress`: `NewProgress().Snapshot().Phase == PhaseQueued`. After `e.r.Run` on the Sony card with a `p` the test keeps, `p.Snapshot()` has `Phase PhaseDone`, `FilesDone == FilesTotal == 2`, `BytesDone == BytesTotal`, and `Counts == out.Counts`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/ingest/ -run 'Run|Progress' -v`
Expected: build failure (`undefined: NewRunner`).

- [ ] **Step 3: Implement `progress.go` and `run.go`**

`Run` creates a per-run state value, `type ingestRun struct` (r, ctx, `dbctx := context.WithoutCancel(ctx)` for every catalog call (Decision 24), id, card root, `now := r.now()` taken once, `rules := media.DefaultRules()`, settings, `stagingRoot`, a logger `r.log().With("ingest", id, "card", CardName(in))`, `p`, counts, warnings, cardSize, and the `scan`/`classify`/`capture`/`copy` durations and copied bytes). It calls `StartIngest(dbctx, in.ID, now)` (an error is returned as `Run`'s error), then `status, errText := j.pipeline()`, then `return j.finish(status, errText)`.

`pipeline()`, in order:
1. Phase scanning. `Settings` (on error, return failed, `"settings: " + err.Error()`). `Scan(card, rules)` (on error, failed, `"scan card: " + err.Error()`). Each `ScanError` becomes a failed row `{CardPath: Rel, Error: Err.Error()}`; each unsupported file becomes `{CardPath, Size, Outcome: unsupported}`. `cardSize` is the sum of the `Media` sizes.
2. Phase classifying, `FilesTotal = len(Media)`. Classify with 3 workers (an index channel, `results[i]` written by index, `sync.WaitGroup` with `Add`/`Done`). `classifyOne(f CardFile) classified`, with `type classified struct { CardFile; fp string; outcome catalog.Outcome; sha string; err error }` (`sha` is the matched asset's for a known outcome, `err` the reason for a failed one): `fp := r.fingerprint(f.Path)` (on error, failed); `matches := AssetsByFingerprint(fp)` (on error, failed); no match → new. In quick mode the best match by state precedence (Decision 8) is known, with that asset's sha. In full mode, `h := r.hashFile(f.Path)`: a match whose sha equals `h` → known; else `AssetBySHA(h)` found → `AddFingerprint(fp, a.SHA256)` and known; `ErrNotFound` → new; any other error → failed. Each classified file increments `FilesDone`.
3. Record the failed, unsupported and known rows (known rows carry `QuickFP` and the matched `SHA256`) with one `AddIngestFiles`, adding each to `counts` and to the progress counts (on error, failed, `"record files: " + err.Error()`).
4. With no new files, return done.
5. `CleanPartials(stagingRoot)` (on error, failed, `"clean partial files: " + …`); log each removed path at Warn.
6. `free := r.freeSpace(PhotosRoot)` (on error, failed, `"free space: " + …`). With `need` the sum of new sizes, if `free < need + settings.ReserveBytes`, return refused with `fmt.Sprintf("staging would drop below the %s free-space reserve (%d new %s need %s, %s free)", formatGB(reserve), n, plural(n, "file", "files"), formatGB(need), formatGB(free))`.
7. Phase copying (`FilesTotal`, `BytesTotal`). For each new file in Rel order (`Scan` sorted them, which is what keeps bursts in card order), call `res := j.importOne(f)`. If `!res.recorded`, `AddIngestFiles(dbctx, id, []IngestFile{res.row})` (on error, failed, `"record files: …"`). Add the outcome to `counts`, append `res.warnings`, and increment `FilesDone`/`BytesDone`.
8. Return done.

`importOne` (Task 8 extends it; keep these names: `newFile`, `fileResult`, `copyNew`, `recordExisting`, `dropCopy`, `failedRow`):

```go
// newFile is a card file classified as new, with its quick fingerprint.
type newFile struct {
	CardFile
	fp string
}

// fileResult is what importing one new file produced.
type fileResult struct {
	row      catalog.IngestFile
	recorded bool // RecordNew already wrote row (and the asset)
	warnings []catalog.ClockWarning
}

func (j *ingestRun) importOne(f newFile) fileResult {
	fail := func(err error) fileResult { return fileResult{row: failedRow(f, err)} }
	start := time.Now()
	c, err := media.CaptureTime(f.Path, f.Kind)
	var meta *media.VideoMeta
	if err == nil && f.Kind == media.KindVideo {
		if m, verr := media.ReadVideoMeta(f.Path); verr == nil {
			meta = m // on error the name has no video suffix; the reason is already in c.Fallback
		}
	}
	j.captureDur += time.Since(start)
	if err != nil {
		return fail(fmt.Errorf("capture time: %w", err))
	}
	if c.Fallback != nil {
		j.log.Warn("capture time fell back", "file", f.Rel, "source", c.Source, "reason", c.Fallback)
	}
	folder, err := j.cat().StagingFolderForDate(j.dbctx, media.FolderDate(c.Time, j.settings.DayStartsAt), j.now)
	if err != nil {
		return fail(fmt.Errorf("staging folder: %w", err))
	}
	if folder.Name == "" || folder.Name == "." || folder.Name == ".." || strings.ContainsAny(folder.Name, `/\`) {
		return fail(fmt.Errorf("staging folder name %q is not a single folder name", folder.Name))
	}
	dir := filepath.Join(j.stagingRoot, folder.Name, media.TypeSubdir(f.Kind, j.rules))
	if err := mkdirDurable(dir); err != nil {
		return fail(err)
	}
	name := media.DestFileName(c.Time, f.Kind, meta, filepath.Ext(f.Path))
	start = time.Now()
	dst, sum, dup, err := j.copyNew(f, dir, name)
	j.copyDur += time.Since(start)
	if err != nil {
		return fail(err)
	}
	if dup {
		return j.recordExisting(f, dst)
	}
	j.copyBytes += f.Size
	if err := media.SyncDir(filepath.Dir(dst)); err != nil {
		return fail(fmt.Errorf("copied to %s but could not sync its folder: %w", j.rel(dst), err))
	}
	existing, err := j.cat().AssetBySHA(j.dbctx, sum)
	switch {
	case err == nil:
		return j.dropCopy(f, dst, existing) // fingerprint collision or alias (§6)
	case !errors.Is(err, catalog.ErrNotFound):
		return fail(fmt.Errorf("copied to %s but the catalog lookup failed: %w", j.rel(dst), err))
	}
	info, err := os.Stat(dst)
	if err != nil {
		return fail(fmt.Errorf("copied to %s but could not stat it: %w", j.rel(dst), err))
	}
	a := catalog.Asset{SHA256: sum, Size: f.Size, Kind: f.Kind, CaptureTime: c.Time, TZOffset: c.Offset,
		State: catalog.StateStaged, RelPath: j.rel(dst), Mtime: info.ModTime().UnixNano(), UpdatedAt: j.now}
	row := catalog.IngestFile{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, SHA256: sum, Outcome: catalog.OutcomeNew, RelPath: a.RelPath}
	if err := j.cat().RecordNew(j.dbctx, j.id, a, f.fp, row); err != nil {
		return fail(fmt.Errorf("copied to %s but not recorded: %w", a.RelPath, err))
	}
	ws := CheckClock(c, j.now)
	for i := range ws {
		ws[i].CardPath, ws[i].SHA256 = f.Rel, sum
	}
	return fileResult{row: row, recorded: true, warnings: ws}
}
```

- `failedRow(f newFile, err error) catalog.IngestFile`: `{CardPath: f.Rel, Size: f.Size, QuickFP: f.fp, Outcome: failed, Error: err.Error()}`.
- `j.rel(dst)`: `filepath.Rel(PhotosRoot, dst)` passed through `filepath.ToSlash`.
- `copyNew(f, dir, name) (dst, sum string, dup bool, err error)` for now: `FreeName`; if it errors or reports a duplicate, return that; else `sum, err = j.r.copyFile(j.ctx, f.Path, dst)`. Task 8 replaces it with the retrying version.
- `recordExisting(f, dst)`: `sum := r.hashFile(f.Path)` (on error, fail). `AssetBySHA(sum)`: when found, `AddFingerprint(f.fp, a.SHA256)` (on error, fail) and return the known row for the asset. `ErrNotFound` for now gives fail with `fmt.Errorf("identical file already at %s but not in the catalog", j.rel(dst))` (Task 8 adopts it instead); any other error, fail.
- `dropCopy(f, dst, a)`: `os.Remove(dst)` (on error, fail with `"duplicate copy %s could not be removed: %w"`), `media.SyncDir(dir)`, `AddFingerprint(f.fp, a.SHA256)`, each error a fail, then return the known row (`{CardPath, Size, QuickFP: f.fp, SHA256: a.SHA256, Outcome: knownOutcome(a.State)}`).

`finish(status, errText)`: phase finishing. If there are warnings, `AddClockWarnings` (on error, status failed with `"record clock warnings: …"`). `safe := status == StatusDone && counts.Failed == 0 && counts.Unsupported == 0 && counts.Seen > 0` (Decision 9). `FinishIngest(dbctx, Ingest{ID, Status, FinishedAt: r.now(), Counts, ClockWarnings: len(warnings), CardSize, SafeToFormat: safe, Error: errText})`, whose error is returned. Then `j.log.Info("ingest finished", "status", status, "seen", counts.Seen, "new", counts.New, "failed", counts.Failed, "scan", scanDur, "classify", classifyDur, "capture", captureDur, "copy", copyDur, "copy_bytes", copyBytes)`, phase done, and `return cat.Ingest(dbctx, id)`. These log keys are the RAW measurement (Decision 22), so keep their names.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/ingest/ -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS, with nothing printed.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/run.go internal/ingest/progress.go internal/ingest/run_test.go internal/ingest/progress_test.go internal/ingest/helpers_test.go
git commit -m "Run an ingest: classify, check space, copy new files and record them" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Ingest failure handling: retries, pulled cards, cancellation, crash leftovers

**Files:**
- Modify: `internal/ingest/run.go`
- Test: `internal/ingest/run_fail_test.go`

**Consumes** (Task 7's `run.go`; read it first):

```go
type Runner struct {
	cfg Config
	fingerprint func(path string) (string, error)
	hashFile    func(path string) (string, error)
	copyFile    func(ctx context.Context, src, dst string) (string, error)
}
type ingestRun struct{ r *Runner; ctx, dbctx context.Context; id int64; card, stagingRoot string; now time.Time; log *slog.Logger; … }
type newFile struct{ CardFile; fp string }
type fileResult struct{ row catalog.IngestFile; recorded bool; warnings []catalog.ClockWarning }
func (j *ingestRun) pipeline() (catalog.IngestStatus, string) // numbered steps 1–8 in Task 7
func (j *ingestRun) classifyOne(f CardFile) classified        // fp → lookup → new | known | failed
func (j *ingestRun) importOne(f newFile) fileResult
func (j *ingestRun) copyNew(f newFile, dir, name string) (dst, sum string, dup bool, err error)
func (j *ingestRun) recordExisting(f newFile, dst string) fileResult
func (j *ingestRun) rel(dst string) string
func failedRow(f newFile, err error) catalog.IngestFile
func mkdirDurable(dir string) error
// media: ClassifyCopyError(err, src, dst) CopyFailure with CopyFailOther, CopyFailCanceled,
// CopyFailSourceMissing, CopyFailDestDirMissing, CopyFailDestTaken, CopyFailStalePartial;
// PartialSuffix; CopyHashed(ctx, src, dst); SyncDir(dir)
// catalog: RecordNew(ctx, ingestID, a Asset, quickFP, f IngestFile) (f.Outcome new or in_staging)
// tests (helpers_test.go): newEnv(t) *env with e.r, e.cat, e.photos, e.cards, e.free; e.fixtureCard(t, label);
// e.run(t, card) catalog.Ingest; e.staged(t) []string; e.files(t, id) map[string]catalog.IngestFile;
// writeFile, writeJPEG; testNow; sonyJPEG/sonyARW are "Staging/2025-05-30/2025-05-31 01.30.12.JPG" and "Staging/2025-05-30/Capture/2025-05-31 01.30.12.ARW"
```

Sony card file order is `DCIM/100MSDCF/DSC00001.ARW` then `DCIM/100MSDCF/DSC00001.JPG`.

**Produces:** no new exported identifiers. `fileResult` gains `canceled bool`. `Run` now returns `StatusCancelled` and `card removed during import` failures, and adopts unrecorded copies.

- [ ] **Step 1: Write the failing tests**

`run_fail_test.go` (each test uses `newEnv` and the Sony card unless stated):
- `TestRunFailedFileNotSafe` (Review Focus 1): `e.r.fingerprint` fails with `errors.New("read error")` for paths ending `.ARW` and calls `media.QuickFingerprint` otherwise. Result: done, `Counts{Seen: 2, New: 1, Failed: 1}`, safe false. The ARW row is `failed` with an `Error` containing `"read error"`. `staged` is `[sonyJPEG]`.
- `TestRunZeroByteFileFails`: the Sony card plus an empty `DCIM/100MSDCF/DSC00002.JPG` gives that row `failed` with `Error "empty file"`, New 2, safe false. `e.r.fingerprint` is never called for it (wrap it to record paths).
- `TestRunCopyFailureLeavesNothing` (Review Focus 3): `e.r.copyFile` returns `"", errors.New("I/O error")` for the ARW without touching the disk. Result: Failed 1, New 1, safe false, `staged == [sonyJPEG]`, and no `.partial` anywhere under photos. Then restore `e.r.copyFile = media.CopyHashed` and run again: `Counts{Seen: 2, New: 1, InStaging: 1}`, safe true, and both files staged.
- `TestRunCardRemovedMidCopy` (Review Focus 3): `e.r.copyFile` counts calls; on the second call it does `os.RemoveAll(card)` and then calls `media.CopyHashed`. Result: `StatusFailed`, `Error` with the prefix `"card removed during import"`, `Counts{Seen: 2, New: 1, Failed: 1}`, safe false, a non-zero `FinishedAt`, `staged == [sonyARW]`, and no partials.
- `TestRunCancelledMidCopy` (Review Focus 4): `ctx, cancel := context.WithCancel(t.Context())`, and `e.r.copyFile` calls `cancel()` and then `media.CopyHashed(ctx, src, dst)`. Run through a helper that passes `ctx` to `Run`. Result: `StatusCancelled`, `Counts.New == 0`, safe false, `staged` nil, no partials, and the catalog row (read back with `Ingest`) is `cancelled` with a non-zero `FinishedAt`.
- `TestRunCancelledBeforeCopy`: `ctx` cancelled before `Run` gives `StatusCancelled`, nothing staged, and `e.r.copyFile` never called.
- `TestRunRetriesTakenName`: on its first call `e.r.copyFile` writes `"someone else"` at `dst` and then calls `media.CopyHashed`, which fails with `CopyFailDestTaken`. The ARW ends up at `"Staging/2025-05-30/Capture/2025-05-31 01.30.12-1.ARW"` with the card's bytes, the intruder file is unchanged, New 2, and safe is true.
- `TestRunRetriesStalePartial`: on its first call `e.r.copyFile` writes `dst+media.PartialSuffix` and then calls `media.CopyHashed`. The ARW is staged at its base name, no partials are left, and New is 2.
- `TestRunRetriesMissingDestDir`: on its first call `e.r.copyFile` does `os.RemoveAll(filepath.Dir(dst))` and then calls `media.CopyHashed`. The ARW is staged at `sonyARW` and New is 2.
- `TestRunGivesUpAfterThreeAttempts`: `e.r.copyFile` always writes `dst` first. The ARW is `failed` with an `Error` containing `"gave up after 3 attempts"`, and `copyFile` was called exactly 3 times for it.
- `TestRunCleansStalePartials` (Review Focus 10): before the run, write `"Staging/2025-05-30/2025-05-31 01.30.12.JPG.partial"` under photos. After it: New 2, `staged == [sonyJPEG, sonyARW]`, and the staged JPG has the card's bytes.
- `TestRunAdoptsUnrecordedCopy` (Review Focus 10): before the run, copy the card JPG's bytes to `photos/` + `sonyJPEG` (a copy whose catalog write never happened). After it: the JPG is `in_staging` with `RelPath == sonyJPEG`, the ARW is `new`, `staged == [sonyJPEG, sonyARW]` (no `-1`), `AssetBySHA(HashFile(card JPG))` is `staged` at `sonyJPEG` with `FirstIngest == out.ID`, and safe is true.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/ingest/ -run 'TestRun(Failed|ZeroByte|CopyFailure|CardRemoved|Cancelled|Retries|GivesUp|CleansStale|Adopts)' -v`
Expected: several FAILs (`TestRunFailedFileNotSafe` and `TestRunCopyFailureLeavesNothing` may already pass; that is fine).

- [ ] **Step 3: Implement**

- `classifyOne`: `f.Size == 0` → failed, `"empty file"`, before fingerprinting.
- Cancellation. Classification workers skip files once `j.ctx.Err() != nil` (their results are marked not processed and are not recorded). After classification and after recording its rows, `if j.ctx.Err() != nil` return cancelled, `""`. The copy loop checks `j.ctx.Err()` before each file, and a `res.canceled` result (not recorded) returns cancelled. In `importOne`, when `copyNew` errors and `media.ClassifyCopyError(err, f.Path, dst) == media.CopyFailCanceled`, return `fileResult{canceled: true}`.
- Pulled card. Add `func (j *ingestRun) cardErr() error { _, err := os.Stat(j.card); return err }`. After classification has recorded its rows, if any file failed and `cardErr() != nil`, return failed with `"card removed during import: " + err.Error()`. In the copy loop, after recording a failed row, do the same check and return it.
- Replace `copyNew` with:

```go
// copyNew picks a free name in dir and copies f there, retrying the
// failures a retry can fix (Decision 13). dup reports identical content
// already at dst, so nothing was copied.
func (j *ingestRun) copyNew(f newFile, dir, name string) (dst, sum string, dup bool, err error) {
	const attempts = 3
	for range attempts {
		dst, dup, err = media.FreeName(f.Path, dir, name)
		if err != nil || dup {
			return dst, "", dup, err
		}
		sum, err = j.r.copyFile(j.ctx, f.Path, dst)
		if err == nil {
			return dst, sum, false, nil
		}
		switch media.ClassifyCopyError(err, f.Path, dst) {
		case media.CopyFailDestTaken:
			j.log.Warn("destination appeared during the copy; choosing another name", "dst", j.rel(dst))
		case media.CopyFailStalePartial:
			// Ingest is the only writer of partials in Staging and runs alone,
			// so this one was left by an earlier, interrupted copy.
			if rmErr := os.Remove(dst + media.PartialSuffix); rmErr != nil {
				return dst, "", false, errors.Join(err, rmErr)
			}
			j.log.Warn("removed a stale partial", "path", j.rel(dst)+media.PartialSuffix)
		case media.CopyFailDestDirMissing:
			if mkErr := mkdirDurable(dir); mkErr != nil {
				return dst, "", false, errors.Join(err, mkErr)
			}
		default: // cancelled, source missing, or anything else: a retry can't help
			return dst, "", false, err
		}
	}
	return dst, "", false, fmt.Errorf("gave up after %d attempts: %w", attempts, err)
}
```

- `recordExisting`, the `ErrNotFound` branch: adopt the file (Decision 14). `media.SyncDir(filepath.Dir(dst))`, `os.Stat(dst)` for `Mtime`, `media.CaptureTime` for `CaptureTime`/`TZOffset` (a failure only leaves them empty: log it at Warn), then `RecordNew` with `State: staged`, `RelPath: j.rel(dst)` and `Outcome: in_staging`, returned with `recorded: true`. Any error makes the file fail.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/ingest/ -v`, then `go vet ./...` and `gofmt -l .`
Expected: all PASS (Task 7's tests too), with nothing printed.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/run.go internal/ingest/run_fail_test.go
git commit -m "Handle pulled cards, cancellation, copy retries and crash leftovers in ingest" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: The ingest manager: one job at a time, a queue, recovery and shutdown

**Files:**
- Create: `internal/ingest/manager.go`
- Test: `internal/ingest/manager_test.go`

**Consumes:**

```go
// internal/ingest (Tasks 5–8)
type Request struct{ Path, Label, UUID string }
type Config struct{ Catalog *catalog.DB; PhotosRoot string; Now func() time.Time; FreeSpace func(string) (uint64, error); Log *slog.Logger }
type Runner struct{ cfg Config; fingerprint, hashFile func(string) (string, error); copyFile func(ctx context.Context, src, dst string) (string, error) }
func NewRunner(cfg Config) *Runner
func (r *Runner) now() time.Time
func (r *Runner) log() *slog.Logger
func (r *Runner) Run(ctx context.Context, in catalog.Ingest, p *Progress) (catalog.Ingest, error) // returns StatusCancelled when ctx is cancelled mid-run
type Progress struct{ … }; func NewProgress() *Progress; func (p *Progress) Snapshot() ProgressSnapshot
const StagingDir = "Staging"
func CleanPartials(stagingRoot string) ([]string, error)
// tests: newEnv(t) *env (e.r, e.cat, e.photos, e.cards), e.fixtureCard(t, label) string,
// e.staged(t) []string (regular files under photos/Staging, nil if none), writeFile

// internal/catalog
func (d *DB) CreateIngest(ctx, cardPath, label, uuid string, at time.Time) (Ingest, error) // status queued
func (d *DB) StartIngest(ctx, id int64, at time.Time) error
func (d *DB) FinishIngest(ctx, in Ingest) error
func (d *DB) Ingest(ctx, id int64) (Ingest, error)
func (d *DB) Ingests(ctx, limit int, beforeID int64) ([]Ingest, error)
func (d *DB) FailInterrupted(ctx, at time.Time) (int64, error)
// StatusQueued, StatusRunning, StatusDone, StatusFailed, StatusCancelled
```

**Produces:**

```go
var (
	ErrBadCardPath = errors.New("ingest: not a card folder")
	ErrQueueFull   = errors.New("ingest: too many imports waiting")
	ErrClosed      = errors.New("ingest: shutting down")
)

const MaxQueued = 8 // jobs waiting behind the running one

// ValidateCardPath resolves symlinks in cardsRoot and path, and returns the
// resolved path if it is an absolute path to a directory strictly inside
// cardsRoot.
func ValidateCardPath(cardsRoot, path string) (string, error)

type Manager struct{ /* unexported */ }

func NewManager(r *Runner, cardsRoot string) *Manager
func (m *Manager) Recover(ctx context.Context) error // FailInterrupted + CleanPartials(PhotosRoot/Staging); call before Start
func (m *Manager) Start()                            // starts the worker goroutine; idempotent
func (m *Manager) Submit(ctx context.Context, req Request) (*Job, error) // errors wrap ErrBadCardPath, ErrQueueFull or ErrClosed, or are catalog errors
func (m *Manager) Current() (running *Job, queued []*Job)
func (m *Manager) Close(ctx context.Context) error // cancels the running job, cancels queued ones, waits for the worker or ctx

type Job struct{ /* unexported */ }

func (j *Job) ID() int64
func (j *Job) Done() <-chan struct{}         // closed when the job has finished
func (j *Job) Result() catalog.Ingest        // the final row once Done is closed; before that, the row as last known
func (j *Job) Progress() ProgressSnapshot
```

- [ ] **Step 1: Write the failing tests**

`manager_test.go` helpers:
- `waitDone(t, j *Job)`: select on `j.Done()` with a 10 s timer, failing with `"job %d did not finish"`.
- `newManager(t, e *env) *Manager`: `NewManager(e.r, e.cards)`, `Start()`, and a `t.Cleanup` that calls `Close` with a 10 s context and reports its error. `newEnv` registered the catalog close first, so this close runs before it.
- `gate(e *env) (started <-chan string, release func())`: replaces `e.r.copyFile` with one that sends `src` on a buffered `started` channel (capacity 16), then waits for the release channel or `ctx.Done()` (returning `"", ctx.Err()`), then calls `media.CopyHashed(ctx, src, dst)`.

Tests:
- `TestValidateCardPath` (Review Focus 8): `cards := t.TempDir()` holds `SONY/`, the file `f.txt`, and a sibling `outside := t.TempDir()`. `ValidateCardPath(cards, filepath.Join(cards, "SONY"))` returns `filepath.EvalSymlinks` of that path. These error: `cards` itself; `filepath.Join(cards, "..", filepath.Base(outside))`; `"SONY"` (relative); `filepath.Join(cards, "missing")` (also `errors.Is(err, fs.ErrNotExist)`); `filepath.Join(cards, "f.txt")`; and a symlink `cards/link → outside` (skip that case if `os.Symlink` fails).
- `TestManagerRunsOneAtATime`: cards A (`e.fixtureCard(t, "SONY_A7IV")`) and B (`e.fixtureCard(t, "GOPRO")`), with the copy gated. Submit A and wait for its first `started`. Submit B: `B.Result().Status == StatusQueued`, and `Current()` gives running A and queued `[B]`. Submitting A's path again returns the same `*Job` (pointer equal) and `Ingests` still has 2 rows. Call `release()`, then `waitDone` A and B: both done. The `started` channel yielded A's two card files first and B's file last (B never started while A ran; the env's fixed clock makes timestamps useless for this).
- `TestManagerQueueFull`: the copy is gated and never released. Make 9 more card dirs `cards/C1` … `cards/C9`, each with one JPEG (`writeFile`) under `DCIM/`. Submit the Sony card (wait for `started`), then C1–C8, which all queue; submitting C9 gives `errors.Is(err, ErrQueueFull)`. The cleanup `Close` must still finish within its deadline.
- `TestManagerCloseCancelsRunning` (Review Focus 4): with the copy gated, A is running and B queued. `Close(ctx 10s)` returns nil, and both `Done` channels are closed. A's result is `StatusCancelled` with safe false; B's is `StatusCancelled` with `Error == "SD2NAS stopped before this import started"` and a non-zero `FinishedAt`. The catalog rows agree. `e.staged(t)` is nil (no partials, no files). `Submit` after `Close` gives `ErrClosed`, and a second `Close` returns nil.
- `TestManagerRecover`: `CreateIngest` twice and `StartIngest` the second (one queued, one running), then write `photos/Staging/2025-05-30/x.JPG.partial` and `photos/Archive/y.JPG.partial`. `Recover(ctx)` returns nil. Both rows are `failed`, the Staging partial is gone, and the Archive partial is still there.
- `TestManagerSubmitBadPath`: `Submit` of `filepath.Dir(e.cards)` gives `errors.Is(err, ErrBadCardPath)` and creates no ingest row.
- `TestManagerDefaultLabel`: `Request{Path: card}` with an empty label gives `CardLabel == filepath.Base(card)`. `Request{Path: card, Label: "  CARD  "}` gives `"CARD"` (trimmed).
- `TestJobProgress`: after a finished run, `j.Progress().Phase == PhaseDone`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/ingest/ -run 'Manager|ValidateCardPath|JobProgress' -v`
Expected: build failure (`undefined: NewManager`).

- [ ] **Step 3: Implement `manager.go`**

`ValidateCardPath`: `filepath.IsAbs(path)`, else an error. `EvalSymlinks` both (errors wrapped with `%w`). `rel, err := filepath.Rel(root, real)`; reject `rel == "."`, `rel == ".."`, a prefix of `".."+string(filepath.Separator)`, or an absolute rel, with `fmt.Errorf("%s is not inside the cards folder %s", path, cardsRoot)`. `Stat` must show a directory.

```go
type Manager struct {
	r         *Runner
	cardsRoot string
	ctx       context.Context    // the running job's context; cancelled by Close
	cancel    context.CancelFunc
	wake      chan struct{}      // capacity 1; Submit does a non-blocking send
	exited    chan struct{}      // closed when the worker returns
	mu        sync.Mutex
	running   *Job
	queue     []*Job
	closed    bool
	started   bool
}

type Job struct {
	path     string // resolved card path, for de-duplication
	done     chan struct{}
	progress *Progress
	mu       sync.Mutex
	row      catalog.Ingest
}
```

- `Submit`: return `ErrClosed` if closed. `ValidateCardPath`, wrapping its error as `fmt.Errorf("%w: %v", ErrBadCardPath, err)`. Under `m.mu`, return the running or queued job with the same path; else `ErrQueueFull` if `len(queue) >= MaxQueued`; else `CreateIngest(ctx, path, label, req.UUID, r.now())`, where label is `strings.TrimSpace(req.Label)` or `filepath.Base(path)`. Append the job with `NewProgress()` and wake the worker.
- Worker loop: `next()` pops the head under the lock (setting `m.running`). It returns nil once closed, and otherwise waits on `m.wake` or `m.ctx.Done()` when the queue is empty. For each job, set the in-memory row to running (`StartedAt = r.now()`) and call `out, err := m.r.Run(m.ctx, row, j.progress)`. If `err != nil`, log it at Error and use the last row with `Status: failed`, `Error: err.Error()`, `FinishedAt: r.now()`. Then, under the lock, clear `m.running`, store the row and `close(j.done)`.
- `Close`: mark closed (a second call only waits); take the queue; `m.cancel()`. For each queued job, `FinishIngest(ctx, row with Status cancelled, Error "SD2NAS stopped before this import started", FinishedAt now)`, collecting the errors, then store the row and close `done`. If started, wait for `m.exited` or `ctx.Done()` (adding `ctx.Err()`). Return `errors.Join(errs...)`.
- `Recover`: `FailInterrupted(ctx, r.now())`, logging the count when it isn't 0. `CleanPartials(filepath.Join(PhotosRoot, StagingDir))`, logging each removed path. Join both errors.

- [ ] **Step 4: Run the tests and confirm they pass**

Run: `go test ./internal/ingest/ -v -count=3` (repeated to shake out ordering bugs), then `go vet ./...` and `gofmt -l .`
Expected: all PASS three times, with nothing printed. If a Linux machine or WSL is at hand, also `go test -race ./internal/ingest/`.

- [ ] **Step 5: Commit**

```bash
git add internal/ingest/manager.go internal/ingest/manager_test.go
git commit -m "Queue ingests and run them one at a time with recovery and shutdown" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Ingest HTTP API, request guard, wiring in main, and the M2 checklist

**Files:**
- Create: `internal/api/guard.go`, `internal/api/ingest.go`
- Modify: `internal/api/server.go`, `cmd/sd2nas/main.go`, `docs/PLAN.md`
- Test: `internal/api/guard_test.go`, `internal/api/ingest_test.go`, `internal/api/server_test.go` (one test added)

**Consumes:**

```go
// internal/api (existing server.go; read it)
type Server struct{ version string; started time.Time; mounts []Mount; writable map[string]PathInfo; mux *http.ServeMux }
func New(version string, mounts []Mount) *Server // registers GET /healthz and GET /{$}
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request)
// handleHealthz builds h.Problems; any problem → "degraded" and 503

// internal/ingest (Tasks 5–9)
type Request struct{ Path, Label, UUID string }
type Config struct{ Catalog *catalog.DB; PhotosRoot string; Now func() time.Time; FreeSpace func(string) (uint64, error); Log *slog.Logger }
func NewRunner(cfg Config) *Runner
func NewManager(r *Runner, cardsRoot string) *Manager
func (m *Manager) Recover(ctx) error; func (m *Manager) Start(); func (m *Manager) Close(ctx) error
func (m *Manager) Submit(ctx, req Request) (*Job, error) // ErrBadCardPath, ErrQueueFull, ErrClosed
func (m *Manager) Current() (running *Job, queued []*Job)
func (j *Job) ID() int64; func (j *Job) Done() <-chan struct{}; func (j *Job) Result() catalog.Ingest; func (j *Job) Progress() ProgressSnapshot
type ProgressSnapshot struct{ Phase string; FilesTotal, FilesDone int; BytesTotal, BytesDone int64; Counts catalog.Counts } // json tags phase, files_total, …
func Describe(in catalog.Ingest) (level, message string)
func CardName(in catalog.Ingest) string

// internal/catalog
func Open(path string) (*DB, error); func (d *DB) Close() error
func (d *DB) Ingest(ctx, id int64) (Ingest, error)                   // ErrNotFound
func (d *DB) Ingests(ctx, limit int, beforeID int64) ([]Ingest, error)
func (d *DB) IngestFiles(ctx, ingestID int64) ([]IngestFile, error)
func (d *DB) ClockWarnings(ctx, ingestID int64) ([]ClockWarning, error)
// Ingest (json: id, card_path, card_label, card_uuid, card_size, started_at, finished_at, status, counts, clock_warnings, safe_to_format, error)
// IngestFile{CardPath, Size, QuickFP, SHA256, Outcome, Error, RelPath}; OutcomeNew
// IngestStatus: StatusDone, StatusRefused, StatusFailed, StatusCancelled

// internal/media/mediatest
func CopyCard(testdata, label, dst string) (string, error)
```

**Produces:**

```go
const DefaultMaxWait = 2 * time.Hour // ?wait=1 ceiling; matches the card script's curl --max-time 7200

// MountIngest registers the ingest endpoints. Call it before serving.
func (s *Server) MountIngest(m *ingest.Manager, db *catalog.DB, maxWait time.Duration)

// AddProblem adds a permanent /healthz problem (e.g. the catalog failed to
// open). Call it before serving.
func (s *Server) AddProblem(p string)

func guard(next http.Handler) http.Handler // unexported; ServeHTTP goes through it
```

Routes (Go 1.22 patterns):

```
POST /api/ingest           body {"path","label","uuid"}; ?wait=1 blocks; ?format=text gives two lines
GET  /api/ingest/current   {"running": view|null, "queued": [view…]}
GET  /api/ingests          ?limit=&before=  → {"ingests": [view…]}
GET  /api/ingests/{id}     view + files (outcome != new) + clock_details
```

The JSON view of an ingest:

```go
type ingestView struct {
	catalog.Ingest                                 // fields flattened
	Level        string                   `json:"level"`
	Message      string                   `json:"message"`
	Progress     *ingest.ProgressSnapshot `json:"progress,omitempty"`      // while queued or running
	Files        []catalog.IngestFile     `json:"files,omitempty"`         // detail only: every outcome except new
	ClockDetails []catalog.ClockWarning   `json:"clock_details,omitempty"` // detail only
}
```

- [ ] **Step 1: Write the failing tests**

`guard_test.go`, `TestGuard`: wrap a handler that writes 200 and send requests with `httptest.NewRequest` (whose `Host` is `example.com`):

| Method | `X-SD2NAS` | `Origin` | Want |
|---|---|---|---|
| GET | none | none | 200 |
| HEAD | none | `http://evil.example` | 200 |
| POST | none | none | 403 |
| POST | `1` | none | 200 |
| POST | `1` | `http://example.com` | 200 |
| POST | `1` | `https://EXAMPLE.com` | 200 |
| POST | `1` | `http://evil.example` | 403 |
| POST | `1` | `null` | 403 |
| POST | `true` | none | 403 |
| DELETE | none | none | 403 |
| OPTIONS | none | `http://evil.example` | 403 (no CORS is ever answered) |

A 403 has a JSON body `{"error": "…"}`, and the inner handler was not called.

`ingest_test.go` helper:

```go
// newIngestServer serves the ingest API over a fresh catalog, with the
// committed SONY_A7IV card copied under a temp cards root.
func newIngestServer(t *testing.T, free func(string) (uint64, error), maxWait time.Duration) (s *Server, card string)
```

It creates the cards, photos and config temp dirs first; `CopyCard(filepath.Join("..", "..", "testdata"), "SONY_A7IV", cards)`; `catalog.Open` plus a cleanup that closes it; `ingest.NewRunner(ingest.Config{Catalog, PhotosRoot, FreeSpace: free, Log: slog.New(slog.DiscardHandler)})`; `NewManager(runner, cards)`, `Start()`, and a cleanup that closes the manager with a 10 s context (registered after the catalog's, so it runs first); `New("test", nil)` and `MountIngest(m, db, maxWait)`. `post(t, s, query, body string) *httptest.ResponseRecorder` sets `X-SD2NAS: 1` and `Content-Type: application/json`. The body is `fmt.Sprintf(`{"path":%q,"label":"SONY_A7IV","uuid":"1234-ABCD"}`, card)`. `plenty` is a FreeSpace that returns 1e12.

- `TestIngestWaitText` (Review Focus 2): `post(t, s, "?wait=1&format=text", body)` gives 200, `Content-Type` `text/plain; charset=utf-8`, and body exactly `"normal\nSONY_A7IV: 2 new, 0 already known. Card safe to format.\n"`. The same request again gives 200 and `"normal\nSONY_A7IV: 0 new, 2 already known. Card safe to format.\n"`.
- `TestIngestWaitJSON`: `?wait=1` gives 200 with JSON `status "done"`, `safe_to_format true`, `counts.new 2`, `level "normal"`, and an `id` greater than 0.
- `TestIngestNoWait`: no query gives 202, JSON with `id > 0` and `status` one of `queued`/`running`/`done`. Poll `GET /api/ingests/{id}` (10 s deadline) until `status == "done"`; its JSON has no `files` key (every file was new) and `counts.new` is 2.
- `TestIngestRefusedIs507` (Review Focus 6): FreeSpace returns 0. `?wait=1&format=text` gives 507; line 1 is `alert` and line 2 starts with `SONY_A7IV: not imported, staging would drop below the 50.0 GB free-space reserve`.
- `TestIngestBadRequests`: a POST without `X-SD2NAS` gives 403. Each of these gives 400 with a JSON `error`: body `{`, body `{"path":"x","extra":1}`, body `{"path":""}`, and a path outside the cards root (`t.TempDir()`). None of them creates an ingest row (`GET /api/ingests` is empty).
- `TestIngestWaitTimeout` (Review Focus 9): FreeSpace blocks until a `release` channel is closed (`defer close(release)` in the test body), with `maxWait` 50 ms. `?wait=1&format=text` gives 504 and body `"alert\nSONY_A7IV: still importing after 50ms. Card left mounted.\n"`. While the job is still blocked, `GET /healthz` answers (any status), and `GET /api/ingest/current` gives 200 with a non-null `running` whose `id` matches and whose `progress.phase` is `"classifying"`.
- `TestIngestHistory`: two `?wait=1` runs. `GET /api/ingests` lists the ids `[2, 1]`; `?limit=1` gives `[2]`; `?before=2` gives `[1]`; `?limit=x` gives 400. `GET /api/ingests/2` has `files` with 2 entries, both `outcome "in_staging"` with `rel_path` set. `/api/ingests/999` gives 404, and `/api/ingests/abc` gives 400.
- `TestIngestCurrentIdle`: the body decodes to `running == nil` and an empty, non-nil `queued` slice (JSON `[]`, not `null`).

`server_test.go`, `TestHealthzReportsAddedProblem`: `s.AddProblem("catalog: boom")` makes `/healthz` return 503, `status "degraded"`, and `problems` containing `"catalog: boom"`.

- [ ] **Step 2: Run the tests and confirm they fail**

Run: `go test ./internal/api/ -v`
Expected: build failure (`undefined: guard`, `undefined: (*Server).MountIngest`).

- [ ] **Step 3: Implement the guard, handlers and AddProblem**

`guard.go`: GET and HEAD pass through. Otherwise the request needs `r.Header.Get("X-SD2NAS") == "1"` (else 403, `"missing X-SD2NAS: 1 header"`). If there is an `Origin`, it must parse as an `http` or `https` URL whose `Host` matches `r.Host` under `strings.EqualFold` (else 403, `"cross-origin request refused"`). `server.go`: `New` stores `s.handler = guard(s.mux)` and `ServeHTTP` calls it. Add `problems []string`, which `AddProblem` appends to and `handleHealthz` copies into `h.Problems` first.

`ingest.go`. Shared helpers: `writeJSON(w, code, v)` (`Content-Type: application/json`, `Cache-Control: no-store`; an encode error means the client hung up and is ignored, as in `handleHealthz`), `writeError(w, code, msg)` (writes `{"error": msg}`), and `writeText(w, code, level, msg)` (writes `level + "\n" + msg + "\n"`). The POST handler, whose status mapping is the card script's contract:

```go
func (h *ingestHandlers) post(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		Label string `json:"label"`
		UUID  string `json:"uuid"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "bad request body: "+err.Error())
		return
	}
	if body.Path == "" {
		writeError(w, http.StatusBadRequest, "path is required")
		return
	}
	job, err := h.m.Submit(r.Context(), ingest.Request{Path: body.Path, Label: body.Label, UUID: body.UUID})
	switch {
	case errors.Is(err, ingest.ErrBadCardPath):
		writeError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ingest.ErrQueueFull), errors.Is(err, ingest.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	q := r.URL.Query()
	text := q.Get("format") == "text"
	if v := q.Get("wait"); v != "1" && v != "true" {
		h.respond(w, http.StatusAccepted, job, text) // the view of job.Result() with its progress
		return
	}
	timer := time.NewTimer(h.maxWait)
	defer timer.Stop()
	select {
	case <-job.Done():
		h.respond(w, finishedCode(job.Result().Status), job, text)
	case <-timer.C:
		msg := fmt.Sprintf("%s: still importing after %s. Card left mounted.", ingest.CardName(job.Result()), h.maxWait)
		if text {
			writeText(w, http.StatusGatewayTimeout, ingest.LevelAlert, msg)
		} else {
			writeJSON(w, http.StatusGatewayTimeout, map[string]any{"error": msg, "id": job.ID()})
		}
	case <-r.Context().Done():
		// The client hung up; the job carries on.
	}
}

// finishedCode: anything that must leave the card mounted is non-2xx (curl -f fails).
func finishedCode(s catalog.IngestStatus) int {
	switch s {
	case catalog.StatusDone:
		return http.StatusOK
	case catalog.StatusRefused:
		return http.StatusInsufficientStorage
	case catalog.StatusCancelled:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}
```

`respond` builds `ingestView{Ingest: row, Level, Message (from ingest.Describe), Progress (while not finished)}` and writes JSON, or in text mode just level and message. `GET /api/ingests/{id}` reads `db.Ingest` (`ErrNotFound` → 404), `IngestFiles` filtered to `Outcome != OutcomeNew`, and `ClockWarnings`, and adds progress when the id is the manager's running or queued job. Bad `limit`/`before`/`id` values (not a positive integer) give 400. `queued` is always a non-nil slice.

- [ ] **Step 4: Run the API tests and confirm they pass**

Run: `go test ./internal/api/ -v`
Expected: all PASS, including the M0 `/healthz` tests.

- [ ] **Step 5: Wire it up in `cmd/sd2nas/main.go`**

After `config.Load` and the umask, make `cfg.Cards`, `cfg.Photos` and `cfg.Data` absolute with `filepath.Abs` (so the request path the script sends and the logged paths agree). After `api.New` and the write-probe logging, set up `ctx, stop := signal.NotifyContext(...)` (moved up from below), then:

```go
	var mgr *ingest.Manager
	db, err := catalog.Open(filepath.Join(cfg.Data, "sd2nas.db"))
	if err != nil {
		// Keep serving so /healthz can explain; without the routes the card
		// script's request fails and the card stays mounted.
		log.Error("catalog unavailable; ingest is disabled", "err", err)
		srv.AddProblem("catalog: " + err.Error())
	} else {
		defer func() {
			if err := db.Close(); err != nil {
				log.Error("closing catalog", "err", err)
			}
		}()
		mgr = ingest.NewManager(ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: cfg.Photos, Log: log}), cfg.Cards)
		if err := mgr.Recover(ctx); err != nil {
			log.Error("startup recovery failed", "err", err)
			srv.AddProblem("startup recovery: " + err.Error())
		}
		mgr.Start()
		srv.MountIngest(mgr, db, api.DefaultMaxWait)
	}
```

Shutdown, after `<-ctx.Done()`, uses one 8 s budget (inside Docker's default 10 s stop timeout): first `mgr.Close(shutdownCtx)`, which cancels the running copy so `?wait=1` handlers return, then `httpSrv.Shutdown(shutdownCtx)`. Return `errors.Join` of both, each wrapped with what it was stopping. The deferred `db.Close` runs last.

- [ ] **Step 6: Smoke-test the binary**

Run (Git Bash, repo root) in the background: `go run ./cmd/sd2nas --addr 127.0.0.1:8099 --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config`. Then:

```bash
CARD="$(pwd -W 2>/dev/null || pwd)/testdata/cards/SONY_A7IV"
curl -sS -X POST "http://127.0.0.1:8099/api/ingest?wait=1&format=text" -H "X-SD2NAS: 1" \
  -H "Content-Type: application/json" -d "{\"path\":\"$CARD\",\"label\":\"SONY_A7IV\",\"uuid\":\"TEST\"}"
curl -sS -o /dev/null -w "%{http_code}\n" -X POST "http://127.0.0.1:8099/api/ingest" -d '{}'
```

Expected: the first prints `normal` and then `SONY_A7IV: 2 new, 0 already known. Card safe to format.` (or `0 new, 2 already known` if `./tmp` held an earlier run). The second prints `403`. `./tmp/photos/Staging/2025-05-30/` holds the JPG and `Capture/` the ARW. Stop the server with Ctrl-C (or stop the background task) and check that the log ends with `shutting down` and no error. `./tmp` is git-ignored.

- [ ] **Step 7: Update `docs/PLAN.md`**

In §16 M2, tick (`- [x]`) every item except `Measure RAW ingest on the NAS…`, which stays `- [ ]`. After the M2 list, add this paragraph, in the style of the M1 note:

```markdown
Implementation plan: `docs/plans/m2-catalog-ingest.md`. The schema in
`internal/catalog/schema.go` is authoritative; it extends §6 with a `queued`
ingest status, `ingests.card_path`/`unsupported`/`clock_warnings` columns, an
`unsupported` file outcome and a `clock_warnings` table. A card is safe to
format only when the import finished, nothing failed, no file was in a format
SD2NAS doesn't import (e.g. `.HIF`), and at least one photo or video was found.
Refused, failed and cancelled imports answer `?wait=1` with a non-2xx status, so
the card stays mounted; M6's script should use `curl --fail-with-body` so the
reason reaches the notification. **To measure RAW ingest:** insert a card holding
at least 50 RAW files not yet imported, then read the `ingest finished` line in
the container log: `capture` is the time spent reading capture times, `copy` the
time spent copying, `copy_bytes` the bytes copied. Record the numbers here and
tick the item. If `capture` is more than a quarter of `copy`, reading EXIF from
the staged copy instead of the card is the follow-up.
```

- [ ] **Step 8: Run the full CI set**

Run: `gofmt -l .`, `go vet ./...`, `go test ./...`
Expected: gofmt and vet print nothing, and every package passes.

- [ ] **Step 9: Commit**

```bash
git add internal/api/ cmd/sd2nas/main.go docs/PLAN.md
git commit -m "Serve the ingest API behind the request guard and wire ingest into main" -m "Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
