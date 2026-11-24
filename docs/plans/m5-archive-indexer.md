# M5 Archive Indexer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give SD2NAS a hash index of the existing `Archive/` so cards stop importing files the owner already has. That means a resumable backfill of ~150k files and a nightly rescan at 03:00. The rescan follows moves, records in-place edits as aliases ("changed since indexed"), marks vanished files `missing` and indexes files copied in by hand. The catalog gets a nightly `VACUUM INTO` backup. An "Archive index" page shows progress, lets the owner pause and resume a pass, start a rescan or a full re-index, and works on a desktop and on a phone. Imports refuse "safe to format" for files whose archive copy is missing.

**Architecture:** A new package `internal/indexer` owns one walker for the backfill, every rescan and a full re-index. The first pass is a rescan over an empty index; a full re-index is a rescan that re-reads files already indexed. A pass lists `Archive/` (stat only), applying the built-in skip rules and the owner's `archive_skip` setting. It then reads, one at a time, only the files that are new or whose size or modified time changed (every file, on a full re-index). Each one is resolved by content hash against the catalog: new, moved, copy, changed in place, reappeared, recovered from culled, or still staged. Only a pass that ran to the end marks the archived files it didn't see as `missing`. Before marking each one, it re-checks that the file is gone and that `Archive/` itself is still there (a dropped share reads as "not found"). Folders a promote is writing into are skipped through the shared `*staging.Locks`, which the indexer sees through a one-method interface. Between files a pass waits while the owner has paused it, or while an import or promote is busy. A `time.Timer` loop backs up the catalog at 03:00 and then runs the nightly pass. `internal/catalog` gets schema version 3: the `missing` ingest outcome, `archive_copies`, `archive_changes`, `archive_scans`, and `assets.checked_at`/`promoted_at`. It also gets the archive queries. `internal/ingest` blocks "safe to format" on `missing`. `internal/api` serves `/api/archive/*`, `/api/settings` and the `indexer.progress` event. The web app gets the Archive index page and its nav pill.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `net/http`, `log/slog`, `modernc.org/sqlite`; Svelte 5.59.1 (runes), Vite 8.3.8, Node 24. No new dependencies.

**Spec:** `docs/PLAN.md` §9 (indexer semantics: backfill, skip rules, moves, folder names as text, nightly rescan, "Rescan now"), §16 M5 checklist, §3 (catalog backup: `VACUUM INTO /photos/Management/SD2NAS/catalog-YYYY-MM-DD.db`, keep 14, folder is a setting; "safe to format"), §6 (assets, fingerprints, aliases, duplicate detection; `internal/catalog/schema.go` is authoritative), §10 (reserved `/api/archive/*`, `/api/settings`, `indexer.progress`), §13 (time zones). UI design: `docs/ui-mockup.html` `renderArchive()` (stats card, rescan bar, changed-since-indexed card with "Accept new hashes", recently promoted) and the nav badge (`pill warn` with the changed count). Context: `HANDOFF.md` ("M5 must build on / must not break", follow-ups F6–F14). Template: `docs/plans/m4-staging-ops.md`. Owner changes after the pre-flight scan: O1 (`archive_skip` setting), O2 (pause/resume), O3 (re-index everything).

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`, `go 1.24.0` in `go.mod` with no `toolchain` line. **Use no API newer than Go 1.24**: no `sync.WaitGroup.Go`, `testing/synctest`, `T.Output`, `os.Root` methods added in 1.25. Fine: `context.WithoutCancel`, `context.AfterFunc`, `min`/`max`, `slices`, `maps`, `t.Context()`, `slog.DiscardHandler`, `omitzero`, `range` over an int, `filepath.IsLocal`, `sync.Mutex.TryLock`, `sync/atomic` types, `time/tzdata` (tests only).
- **No new dependencies**, Go or npm. `go.mod`, `go.sum`, `web/package.json` and `web/package-lock.json` must not change. No cron library, no router, no test framework. Frontend tests use the built-in `node --test` on plain JS in `web/src/lib/*.js`. npm commands run **only inside `web/`**.
- M5 touches:
  - `internal/catalog/`: schema v3, archive queries, settings, and promote's `promoted_at` in `ArchiveFolder`/`ArchiveAsset`.
  - `internal/ingest/`: `run.go`, `describe.go` (the `missing` outcome).
  - `internal/staging/locks.go`: `NameHeld` only.
  - The new `internal/indexer/`.
  - `internal/api/`: new `archive.go` and `settings.go`, the `events.go` constant, test helpers.
  - `cmd/sd2nas/main.go`, `web/src/**`, and `docs/PLAN.md` (last task).

  Do not modify `internal/media/`, `internal/thumbs/`, `internal/config/`, `.github/`, `deploy/`, or any other file of `internal/staging/`. **`internal/indexer` imports only the standard library, `internal/catalog` and `internal/media`.** It never imports `internal/staging`, `internal/ingest` or `internal/api`, test files included.
- **Binding values** (reviewers check them verbatim):
  - Endpoints (request fields camelCase as in §10, response fields snake_case like M3/M4):
    - `GET /api/archive/stats` → 200 `Stats` (Task 9 lists the fields; it embeds the `indexer.progress` Status).
    - `POST /api/archive/rescan` with an optional body `{"full": bool}` → 202 `{"started": bool}`. No body, `{}` and `{"full": false}` start a normal pass. `{"full": true}` starts a full re-index (Decision 22). `started` is false when a pass is already running, or after shutdown.
    - `POST /api/archive/pause` and `POST /api/archive/resume` (no body) → 200 with the Status. When no pass is running → 409 `{"error": "No scan is running.", "code": "not_running"}`. Pausing a paused pass, or resuming one that isn't paused, is a 200 no-op.
    - `GET /api/archive/changed` → 200 `{"version": uint, "changes": [ArchiveChange…], "total": int, "latest_id": int}`. At most **500** changes, the oldest first (ordered by `id`). `latest_id` is the highest id **among the changes returned** (0 when none), so accepting up to it clears exactly what was shown.
    - `POST /api/archive/changed/accept` `{"upTo": int}` → 200 `{"accepted": int}`. It deletes every change with `id <= upTo`. A missing or null `upTo`, or one below 1 → 400 `upTo must be a positive whole number.`; a non-number `upTo` or an unknown field gets `decodeBody`'s sentence.
    - `GET /api/settings` → 200 `{"settings": {key: string…}}` with every key below and its effective value (`backup_dir` shows the effective folder). `PUT /api/settings` takes a JSON object of `key: string` (≥ 1 key, all applied in one transaction or none) → 200, the same body as GET. 400 sentences: `Name at least one setting.`, `Unknown setting <key>.`, `Setting <key> must be a string.`, `Setting <key>: <reason>.`, `Setting backup_dir: pick a folder outside Archive/ and Staging/.`
    - Bodies at most 64 KiB; every non-GET needs `X-SD2NAS: 1` (the existing guard). Errors are JSON `{"error": msg}` (plus `code` where named) with `Cache-Control: no-store`. After `Close`, accept, pause and resume answer 503 `SD2NAS is shutting down`.
  - Event `indexer.progress` (also sent as a snapshot frame on connect). Its data is the indexer `Status`:

    ```
    {"version": uint, "running": Progress|null, "last": ScanResult|null, "last_backup": BackupResult|null,
     "backing_up": bool, "pending_full": bool, "changed": int, "missing": int, "next_scan": RFC 3339}
    ```

    It is published at most every **250 ms** (≤ 4/s), plus at every forced change: pass start, phase change, pause or resume (by the owner or automatic), pass end, backup start and end, accept, settings change.
  - `Progress.paused_by` is `""` (running), `"user"` (paused with Pause) or `"busy"` (waiting for an import or promote). The owner's pause wins when both apply.
  - Settings (catalog `settings` table):
    - `archive_rescan_at` (default **`03:00`**, `HH:MM` 00:00–23:59, container local time `time.Local` from `TZ`).
    - `backup_dir` (default `""` = `<photos>/Management/SD2NAS`, i.e. `/photos/Management/SD2NAS` in the container; otherwise an absolute path; never inside `Archive/` or `Staging/`).
    - `archive_skip` (default **`Output/, CaptureOne/, *.cosessiondb`**). It is a comma-separated list; each entry is trimmed, and empty entries are dropped.
      - An entry ending in `/` is a **directory-name** pattern. It matches directory names at depth ≥ 2 below `Archive/` (inside a session folder, at any depth).
      - Any other entry is a **file-name** pattern, matched against file names at any depth.
      - Patterns use `path.Match` syntax (`*`, `?`, `[…]`) and match case-insensitively. An entry may contain no other `/`, no `\` and no control characters.
      - At most 50 entries and 2048 bytes. `""` means no extra rules.
      - The canonical text joins the entries with `, `. The rejection reason is `want a comma-separated list such as "Output/, CaptureOne/, *.cosessiondb"`.
      - A pass reads this setting once, when it starts.

    Existing keys keep their rules (`day_starts_at`, `verify_known`, `free_space_reserve_gb`, `staging_warn_days`), with plain reasons instead of `strconv` text (Task 3).
  - Built-in skip rules, never configurable:
    - directory and file names starting with `.`;
    - names equal (case-insensitively) to `@eaDir`, `@Recycle`, `@Recently-Snapshot`, `#recycle`, `#snapshot`, `$RECYCLE.BIN` or `System Volume Information`;
    - anything that isn't a regular file (symlinks are not followed);
    - files whose extension isn't an import format of `media.DefaultRules()`.

    Everything else under `Archive/` is indexed, minus `archive_skip`.
  - Backup:
    - It runs at `archive_rescan_at`, **before** that night's pass (Decision 16).
    - It writes `catalog-YYYY-MM-DD.db` (the local date) with `VACUUM INTO` to `catalog-YYYY-MM-DD.db.partial`, fsyncs it through a read-write handle, and renames it over any same-day file.
    - It then keeps the newest **14** files matching `^catalog-[0-9]{4}-[0-9]{2}-[0-9]{2}\.db$` and deletes older ones. Nothing else in the folder is ever touched, and pruning runs only after a successful backup.
  - Catalog: `SchemaVersion = 3`; new ingest outcome `missing`; `ingests.missing` column; `Counts.Missing` (`json:"missing"`, counted in `seen`).
  - Messages (exact):
    - Ingest: `1 file's archive copy is missing` / `<N> files' archive copies are missing` (`%d`, no thousands separator). A done import with any → `<new> new, <known> already known[, <F> failed], <missing text>[, <U> files not imported (…)]. Do not format the card.`
    - Scan `skipped`: `Archive/ is missing, so nothing was changed (is the photos share mounted?)` and `Archive/ went missing during the scan, so it was stopped (is the photos share still mounted?)`.
    - Scan error for a file that changed while read: `<rel_path> changed while it was read (still being copied?); the next scan picks it up`.
    - A pass found unfinished at startup is recorded with `stopped: true` and `errors: ["SD2NAS stopped before this scan finished"]`.
    - UI pause lines: `Paused. Press Resume to carry on.` (user) and `Paused while an import or promote runs. It carries on by itself.` (busy).
  - UI: phone breakpoint `max-width: 760px`; tap targets ≥ 44 px; stats grid two columns on a phone; long paths wrap (`overflow-wrap: anywhere`), never scroll the page sideways.
- **Responsive, not phone-only (owner requirement).** The Archive index page must work well at desktop width (multi-column stats, tables) and at 375×812 (two-column stats, stacked rows instead of wide tables, full-width buttons, the tab bar and safe areas respected). Reviewers check both.
- Svelte 5 runes only (`$state`, `$derived`, `$effect`, `$props`), no `{@html}`, no `export let`. Plain CSS (tokens in `web/src/app.css`, component styles scoped).
- Follow the M3/M4 live-page pattern:
  - one AbortController per load;
  - keep the last good data on screen and show a refetch failure above it;
  - refetch after a real reconnect, and whenever `indexer.progress.version` differs from the `version` in the data;
  - if a load returns a version other than the live one, load once more (the F10 lesson).
- **The indexer never writes the filesystem except the backup folder.** It never moves, renames or deletes anything under `Archive/`. Catalog writes for a file that was read use `context.WithoutCancel`, so a file read before shutdown is always recorded.
- SQLite has **one connection**: never call a `catalog.DB` method while a `*sql.Rows` from it is open, and inside `d.tx(…)` use only `tx`. Catalog methods return slices, not iterators.
- Tests pass on Windows and Linux:
  - Use `path/filepath` for OS paths and `path` for slash-separated `rel_path` values.
  - Close every file, `*sql.Rows`, `*catalog.DB` and `*indexer.Service`. Call `t.TempDir()` before registering a `Close` cleanup.
  - Tests that wait use a 10 s deadline and fail clearly. Sleeps only inside polling loops or for the negative checks this plan names (each ≤ 300 ms).
  - Set file times with `os.Chtimes` to whole seconds (some filesystems round).
  - Test hooks that block must also `select` on the context they are given, so `Close` can end them.
- **Commands.** Windows (Git Bash, repo root): `gofmt -l .` (prints nothing), `go vet ./...`, `go test ./...`. Linux (Docker Desktop running):
  `MSYS_NO_PATHCONV=1 docker run --rm -e TZ=Pacific/Chatham -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q exiftool ffmpeg && go vet ./... && go test ./...'`
  Race (Tasks 5, 7–12): `MSYS_NO_PATHCONV=1 docker run --rm -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q build-base exiftool ffmpeg && CGO_ENABLED=1 go test -race ./internal/... ./web/...'`
  Web (in `web/`): `npm test`, `npm run build` (then `git status --short web/dist` must show nothing).
- Never silently swallow an error. Return it wrapped with `%w`, answer the request with it, record it in the scan result (which logs it), or log it with `slog` (key `"err"`). Exceptions:
  - a failed write to an HTTP client that has gone;
  - a file that vanished between the listing and the read (it is simply not there any more);
  - `localStorage` being unavailable in the browser.
- Export only the identifiers listed in each task's **Produces** block. Unexported helpers are free unless a later task's **Consumes** names them. **Each task must compile and pass on its own**: a type is introduced in the first task that uses it.
- Commit on branch `m5-archive-indexer`, one commit per task (more is fine), with the message given in the task.
  - Every message ends with a blank line and then exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` — no other trailer, no other model name.
  - Never commit `HANDOFF.md`, `tmp/`, `web/node_modules/` or anything in `web/dist/` other than `.placeholder`.
  - Write files as UTF-8 with LF line endings, and check that `§`, `…`, `—` and `’` survive.
  - Never push.

## Review Focus

1. **Nothing is marked `missing` that isn't gone.**
   - Only a pass that ran to the end marks anything.
   - Each candidate is re-checked right before it is marked, and `Archive/` must still exist. A dropped share ends the pass with the "went missing" message and marks nothing more.
   - Rows below folders the pass skipped (busy, unreadable) are never candidates. Neither are rows below skip-rule folders, built-in or from `archive_skip` — including rules the owner adds after the rows were indexed.
   - A file whose old content still exists as a known copy moves to the copy instead.
   - Content overwritten in place with other known content is marked missing at once, and not re-read every night.

   Pinned in Task 8 (`TestGoneFileMarkedMissing`, `TestArchiveVanishesBeforeMissing`, `TestSkippedRowsNeverMissing`, `TestUnreadableFolderNotMissing`, `TestPrimaryGoneMovesToCopy`, `TestChangedToKnownContent`, `TestStoppedPassMarksNothingMissing`) and Task 6/8 (`TestNewSkipRuleKeepsIndexedRows`).
2. **Promote interplay.**
   - A folder whose `Archive/…` name (or a parent's) a promote holds is skipped whole. It is not indexed, nothing below it is marked missing, and the next pass picks it up.
   - Promoted files already `archived` with matching size and modified time are skipped without being read, including a promote that finishes during the pass.
   - Content still `staged` found in `Archive/` is left alone (`in_staging`).
   - `culled` content found in `Archive/` becomes `archived` at that path (M4 Decision 9).

   Pinned in Task 5 (`TestNameHeld`), Task 7 (`TestRescanSkipsPromotedFiles`, `TestPromoteFinishedMidPass`, `TestStagedContentLeftAlone`, `TestCulledContentRecovered`) and Task 8 (`TestPromoteHeldFolderSkipped`, `TestHalfPromotedFolder`).
3. **A change in place keeps the card's original a duplicate.**
   - The new content becomes the asset with the same kind, capture time, first ingest and `promoted_at`.
   - The old hash becomes an alias, and the old fingerprints (same values) point at the new asset. Aliases and ingest history follow.
   - A revert to an earlier hash works.
   - There is one "changed" entry per path, with a fresh id on re-change, and "Accept" clears exactly the entries the page showed.

   Pinned in Task 2 (`TestRecordArchiveChange*`, `TestAcceptArchiveChanges`, `TestArchiveChangesOrderAndLatest`) and Task 8 (`TestRescanChangedContent`, `TestChangeReverted`).
4. **Moves and duplicates.** A file that disappears from one path and appears at another in the same pass is one move: no `missing` and no new asset. It is found by quick fingerprint plus size without a full read. The same content at two archive paths is one asset plus one `archive_copies` row, never re-read while unchanged, and a copy's content is always fingerprinted before it becomes an asset. Pinned in Task 7 (`TestCopyOfForgottenAssetFingerprinted`) and Task 8 (`TestMoveWithinSession`, `TestSessionRenamedByHand`, `TestDuplicateRecordedAsCopy`).
5. **Long job hygiene.**
   - One file at a time, and each file is recorded as it is read, so an interrupted pass resumes without re-reading.
   - Between files the pass waits while the owner has paused it or an import or promote is busy. The current file finishes first.
   - Hashing stops mid-file on shutdown, also while paused.
   - Progress is throttled.

   Pinned in Task 7 (`TestBackfillResumes`), Task 9 (`TestCloseStopsPass`, `TestProgressThrottled`, `TestStartRescanOnce`) and Task 10 (`TestPausesWhileBusy`, `TestPauseResume`, `TestCloseWhilePaused`).
6. **Full re-index never empties the index.** The index stays in use while a full pass runs. It re-reads every indexed file at most once across stops and restarts (`checked_at` against the pass's `reread_before`), records content changes and gone files with the normal rules, and the next pass of any kind continues an unfinished one. Pinned in Task 10 (`TestFullReindexRereadsEverything`, `TestFullReindexResumes`, `TestFullReindexFindsChange`).
7. **Safe to format.** A card file whose matched asset is `missing` gets outcome `missing`. It is counted, blocks `safe_to_format` and is named in the message. A fingerprint that also matches an archived asset still wins as `in_archive`. Pinned in Task 4 (`TestRunMissingBlocksSafeToFormat`, `TestDescribeMissing`).
8. **Schedule and backup.**
   - The nightly run is at 03:00 container time, the next one strictly after now, and is re-armed after a settings change.
   - The backup runs first, even while a paused or manual pass is running, and the nightly pass is skipped when one already runs.
   - The backup is a consistent catalog the current build opens, and 14 are kept. A failed backup deletes nothing and is shown.
   - The folder can't be inside `Archive/` or `Staging/`.

   Pinned in Task 11 (`TestNextRun`, `TestNightlyBacksUpThenScans`, `TestNightlyBackupWhilePaused`, `TestBackupWritesAndPrunes`, `TestBackupFailureKeepsOldBackups`) and Task 12 (`TestSettingsAPI`).
9. **UI on both form factors.**
   - Progress comes from `indexer.progress` and shows phase, files, bytes, current path and percent.
   - Pause/Resume, and the re-index confirmation dialog.
   - Stats are refetched on version change and on reconnect.
   - Accept sends the shown `latest_id`, and the nav pill shows the changed count.
   - No horizontal scroll at 375 px.

   Pinned in Task 13 (`archive.test.js`) and the manual checklist in Task 14.

## Decisions

1. **One walker for backfill, rescan and re-index.** The first pass is a rescan over an empty index: every file is "new", so it is read and inserted as `archived` with `first_ingest = NULL`. Every later pass skips files whose size and modified time match the catalog. A full re-index (Decision 22) is a pass that doesn't skip.

   "Resumable" falls out of this: each file is committed as soon as it is read, so a pass stopped after 40,000 files leaves 40,000 rows that the next pass skips by size and modified time.

   Why: two code paths would have to agree on skip rules, move detection and the promote rules; one can't drift.
2. **A pass has three phases**:
   - *listing*: walk `Archive/`, stat only, compare each path with the catalog;
   - *hashing*: read the changed paths, then the new ones;
   - *checking*: archived rows not seen become a copy's path, or `missing` after re-checks.

   Reads wait until the listing is complete, so "disappeared" is known before a new path is resolved. That is what makes a move one move (§9), whatever order the walk meets the two paths in.
3. **Move detection** (§9 "same hash at a new path in the same scan"). A new path is first quick-fingerprinted (a 128 KiB read). The file is taken to be a catalog asset, moved, without a full read, when that asset has the same fingerprint and size and is either:
   - `archived` at a path this pass didn't see, or
   - `missing`.

   §6 treats fingerprint plus size as a safe match for camera files. Otherwise the file is fully hashed and resolved by hash:

   | Asset with that hash | Result |
   |---|---|
   | none | added |
   | `archived`, at this very path (a promote finished mid-pass) | unchanged |
   | `archived`, not seen at its path | moved |
   | `archived`, still present or below a folder this pass skipped | a copy |
   | `missing` | reappeared |
   | `culled` | recovered |
   | `staged` | left alone |

   Before reading a new path the pass also looks the path up in the catalog again: a promote that finished after the listing has already recorded it, with matching size and time, so it is skipped unread (pre-flight C5).

   Why the fingerprint shortcut: Capture One moves files between `Capture/`, `Selects/` and `Trash/`, and a session renamed by hand would otherwise mean re-reading the whole session.
4. **Same content at two archive paths** (duplicates in the archive). One asset row keeps one `rel_path`; every other path is a row in `archive_copies` (`rel_path`, `sha256`, `size`, `mtime`, `checked_at`). Later passes skip copy rows by size and modified time, like assets. When the primary path disappears and a copy was seen, the asset moves to the copy (no `missing`).

   Copy rows have no foreign key, because a copy may outlive its asset when Forget deletes a staged or culled one. A copy whose asset isn't `archived` any more is resolved again by its hash. It is fingerprinted first (128 KiB), so an asset it becomes always has a fingerprint (pre-flight C4).

   Why the table: without it every duplicate would be re-read every night; ~150k files hand-curated since 2007 will have some.
5. **What counts as a media file:** the import formats of `media.DefaultRules()` (the same lists ingest uses). A card can only bring those, so indexing anything else (PSD, HEIC exports) would cost reads and match nothing.
6. **Skip rules: a short built-in list plus the owner's `archive_skip` setting (O1), not §9's allow list.**
   - Built in, because they never hold the owner's photos and no setting should be able to break them: dot names, NAS metadata names, non-regular files, non-import formats.
   - `archive_skip` defaults to §9's skips: `Output/, CaptureOne/, *.cosessiondb`.

   The format is a comma-separated list (Global Constraints). Comma-separated is easy to type in a JSON string with curl, and a pattern can't express a name containing a comma, which is an accepted limit.

   Everything else below a session folder is indexed. §9's list (top level, `Capture/`, `Selects/`, `Trash/`, `Videos/`, `Other/`) describes the Capture One layout, but folders from before Capture One (2007…) have other subfolders whose originals must still count as "already have". `Trash/` counts as seen (§9).

   A pass reads the setting once when it starts, so a change made mid-pass takes effect at the next pass and a pass is consistent with itself. Rows already indexed below a path a new rule skips are left exactly as they are:
   - they stay `archived` and known, so cards still skip them;
   - they are never `missing`, never re-read, and their copies are kept;
   - removing the rule brings them back in scope unchanged.

   Why not delete them: a skip rule says "don't look there", not "forget what's there"; deleting would re-import those files from a card.
7. **Folder names are never parsed.** The indexer stores no capture time for indexed files (`capture_time` NULL): §9 asks only for the hash, nothing reads `capture_time` of archived assets, and reading EXIF/`mvhd` for 150k files would add I/O for nothing. Folder names are only ever handled as text (`2007-09-31 - Random Party` is indexed like any other; Task 7 pins it). "Recently promoted" groups by the folder name as text too.
8. **Changed in place.** The rescan re-hashes a path whose size or modified time changed.
   - Same hash: only the modified time (and `checked_at`) is updated.
   - A hash the catalog doesn't know: the change is applied at once, in one transaction:
     - a new asset row with the new hash, copying kind, capture time, time-zone offset, first ingest and `promoted_at` from the old row;
     - the old hash becomes an alias;
     - the old row's fingerprints are re-pointed to the new hash. The fingerprint values survive, which keeps §6's "fingerprints are never deleted" (pre-flight C14);
     - aliases, ingest-file rows and clock warnings follow;
     - the old row goes;
     - an `archive_changes` entry records path, old/new hash and size, when the old content was last indexed and when the change was seen.
   - A hash the catalog already has: the old content left this path. It goes through "checking", where it becomes a copy's path or `missing`, without the "is it back?" stat (the path was just read and holds other content: pre-flight B11). The path itself is resolved like a new one.

   §9 says "store the old hash as an alias, list it under changed since indexed", so the catalog is right before anyone looks. **"Accept new hashes" only clears the list**, and the mockup's toast becomes "Marked as reviewed. The old hashes stay as aliases, so the originals on a card still count as duplicates."

   Why not hold the change until accepted: there is no "reject" (SD2NAS never restores files), and a pending state would make every later pass re-read the file and complicate moves and missing. The list's purpose is to surface unexpected edits (a tool rewriting files, bit rot) so the owner can check them.
9. **`archive_changes` is a table**, with one row per path (`rel_path UNIQUE`).
   - A second change of the same path before it is accepted keeps the first old hash and indexed time, takes the new hash, and gets a **new id** (delete + insert).
   - `GET` lists the oldest 500 by id, and `latest_id` is the highest shown, so `accept {"upTo": latest_id}` accepts exactly what the page showed, however many there are (pre-flight D3).
   - Accepting deletes the rows; the aliases are the permanent record.
   - The mockup's "Likely cause" column becomes "Size change" (`+12.3 KB`, `same size`): the server can't know the cause and shouldn't guess.
10. **Missing files and their return.** An archived row becomes `missing` when all of these hold:
    - a complete pass didn't see it, and no copy of it was seen;
    - `os.Lstat` reports its file as not existing, or the path was read this pass with other content;
    - `Archive/` still exists.

    `rel_path` is kept, so the page can show where it was. The asset stays known: a card with that file gets the new `missing` outcome, which blocks "safe to format" (§16). When the content shows up again, at the same path or elsewhere, it becomes `archived` there ("reappeared"). The staging rescan already restages `missing` content found in `Staging/`. Re-importing a missing file from the card is not offered in M5 (follow-up in Task 15).
11. **Ingest outcome `missing`** instead of counting those files as `seen_before`: the import detail can then list them (with where they used to be), and the history keeps why the card wasn't safe.
    - Migration 3 rebuilds `ingest_files`, because SQLite can't change a `CHECK`. It keeps rowids and adds an index on `ingest_files(sha256)`, which the change transaction updates by.
    - `ingests.missing` stores the count.
    - The message is "N files' archive copies are missing", placed after the failed count; Global Constraints has the exact singular and plural.
    - The import detail shows a red stat and an open list.
12. **Promote interplay, without importing staging.** The indexer takes `Locks interface{ NameHeld(rel string) bool }`. The shared `*staging.Locks` satisfies it through a new method that reports whether a name key equal to `rel`, or to one of its parent directories, is held (case-insensitively, like the keys).

    Promote holds `Archive/<target>` from enqueue until the folder's catalog write is done. So the indexer checks every directory as it walks (a nested `{year}/{month}` target is caught at its own level) and skips it whole. It re-checks before each read and before each `missing`.

    The indexer never takes a lock itself. Holding a session folder for the minutes its files take to hash would make a promote into it fail with 409. Promote never removes anything from `Archive/`, so peeking is enough: a promote that starts after the check only adds files, whose content is either `staged` (left alone) or already `archived` at that path (skipped, Decision 3).

    `indexer.ArchiveDir`/`StagingDir` must equal `staging.ArchiveDir`/`staging.Dir`, and `catalog.ArchiveRoot` must equal `indexer.ArchiveDir + "/"`. An `internal/api` test checks this, since it is the one package that imports all three.
13. **Low-priority I/O without new dependencies.** On Unraid the archive is read through `shfs` (FUSE), so the disk reads are issued by the `shfs` daemon, not by SD2NAS. `ioprio_set`/`nice` on our threads wouldn't reach the disks, and must not break the Windows build. Instead:
    - one file at a time, sequential 1 MiB reads, no worker pool;
    - between files the pass **waits while an import is running or queued or a promote is running** (`Config.Busy`, wired in `main`, checked every 2 s, shown as `paused_by: "busy"`);
    - the nightly run is at 03:00.

    No bandwidth cap setting; it can come later if the nightly run gets in the way.
14. **When passes run.** Passes run nightly at `archive_rescan_at` (default 03:00) and from "Rescan now" or "Re-index everything". **Nothing starts at startup**, not even the first backfill: an update applied at noon shouldn't start hours of array reads, and the backfill is meant to run overnight (§9).
    - The first nightly run is the backfill; "Rescan now" starts it sooner.
    - An interrupted pass continues at the next nightly run or manual start (the page says so).
    - If a pass is already running at 03:00 (manual, or paused by the owner), the nightly run backs up the catalog and leaves that pass alone: it is the rescan.
    - A run missed while the container was down is not caught up.
15. **Schedule.** A `time.Timer` loop (no cron library). `nextRun(now, at)` is the first `HH:MM` strictly after `now` in `time.Local` — the container's `TZ`, which Unraid injects; `/healthz` warns when it is missing. `time.Date` normalises a time inside a DST gap. A settings change re-arms the timer at once.
16. **Catalog backup at the scheduled time, before the nightly pass**, every night, whatever any pass is doing. It is skipped only on shutdown.

    §3 says "after the rescan"; the order changes because a pass can now be paused for hours (O2), and the owner asked that a pause never blocks the backup. The backup is still a full daily snapshot. What matters in it, the culls and the ingest history, doesn't depend on that night's pass, and the index changes from the previous night's pass are in it. Task 15 updates §3.

    The backup runs `VACUUM INTO ?` (a bound parameter) into `<name>.partial`, fsyncs it through a read-write handle (Windows' `FlushFileBuffers` needs one: pre-flight C7), renames it over a same-day file, fsyncs the folder, then prunes to 14. Each backup is stored as an `archive_scans` row of kind `backup` and shown as `last_backup`.

    A failure is logged, stored and shown on the Archive page. Nothing is pruned after a failure, and there is no retry until the next night. Notifying about it waits for M6's notification channel (follow-up).
17. **Settings API in M5, no settings page.** §10 reserves `GET/PUT /api/settings`. The rescan time, backup folder and skip list must be configurable, and editing SQLite by hand (as `staging_warn_days` is today) is not reasonable for a path or a pattern list. The API is small (string values, the catalog validates, all-or-nothing) and is exactly what M7's page will call; until then it is used with `curl` (Task 15 records the command in PLAN.md). The Archive page shows the schedule, backup folder and skip list read-only.
18. **Scan history** lives in `archive_scans`: `kind`, `reread`, `reread_before`, `started_at`, `finished_at`, `complete`, and a `result` JSON written by the indexer. The newest 30 passes and the newest 30 backups are kept. A pass row with no `finished_at` at startup is closed as stopped.

    Why: "last scan", "last backup", "interrupted" and an unfinished re-index must survive a container restart, and the JSON keeps the catalog free of the indexer's result shape.
19. **Status, stats and events.**
    - `indexer.progress` carries the in-memory `Status`: live progress, last result, last backup, the changed and missing counts, an unfinished re-index, and the next run.
    - Its `version` bumps after each pass, backup, accept and settings change — not per file.
    - `GET /api/archive/stats` adds the catalog numbers: files and bytes indexed, copies, files brought in by imports (the mockup's "files in ingest history"), up to 50 missing files, the 5 most recently promoted folders, the skip list. It reads `version` before them, like M4's listings.
    - "Recently promoted" groups archived assets with a `promoted_at` by their session folder (the first path element under `Archive/`), newest first. `promoted_at` is a new column set by promote's catalog writes and backfilled from `updated_at` by the migration. The indexer bumps `updated_at` on every touch and move, so ordering by it would put a 2023 session first after a Capture One move (pre-flight D1).
    - With a nested promote pattern (`{year}/…`) the grouping is by year; the default pattern `{name}` is flat.
20. **M4 follow-ups.** None of F6–F14 is in a file M5 changes in that area: `locks.go` only gains `NameHeld`, and `Staging.svelte`, `ops.go`, `promote.go`, `reconcile.go`, `staging.go` and `ingest/run.go`'s folder waits are untouched. So all stay open, and are listed again in the final report. The new page applies the F10 lesson (reload when the loaded version differs from the live one), and the indexer's event is throttled from the start (the F7 lesson).
21. **Pause and resume (O2).**
    - `POST /api/archive/pause` sets an in-memory flag on the running pass; `resume` clears it and wakes the pass at once. The flag is not stored: a restart ends the pass as stopped, and the next pass starts unpaused.
    - The pass checks the flag between files: before each read, before each session folder while listing, and before each `missing` check. **The file being read when Pause arrives finishes and is recorded**, so a pause never wastes a half-read 4 GB video and never leaves a file half-recorded.
    - `paused_by` tells the owner's pause (`user`) from the automatic wait for an import or promote (`busy`); the owner's wins when both apply.
    - Shutdown while paused ends the pass cleanly (`stopped`), resumable as any pass.
    - A paused pass does not block the nightly backup (Decision 16). The nightly pass is skipped that night, because the paused pass is the rescan.
    - Pause and Resume publish `indexer.progress` at once.
22. **Re-index everything (O3)** is a pass with `reread` set. It reads and re-hashes every indexed file and copy, ignoring the size/mtime skip and the fingerprint move shortcut. It does this **without emptying the index**: cards keep matching archive files while it runs, and culled rows (the only record of culls) are untouched. Content changes are recorded as normal changes in place, and gone files go through the normal missing rules.

    **Resuming:**
    - Every read sets the row's new `checked_at` (assets and copies).
    - A full pass records `reread_before` (its start time) in its scan row, and a row needs re-reading only when its `checked_at` is older than that.
    - A full pass that didn't run to the end (`complete = 0`) leaves the re-index pending. The next pass of any kind — nightly, Rescan now — continues it with the same `reread_before`, so no file is re-read twice.
    - The page shows "A re-index was interrupted…" while one is pending.

    The UI asks for confirmation in a dialog, because it re-reads ~1–2 TB at array speed (hours). Pause works on it like any pass.

## File Structure

```
internal/catalog/
  schema.go        schemaV3: ingests.missing, ingest_files rebuilt with 'missing' + sha index,
                   assets.checked_at/promoted_at, archive_copies, archive_changes, archive_scans (Task 1)
  catalog.go       SchemaVersion = 3                                                 (Task 1)
  ingests.go       OutcomeMissing, Counts.Missing, ingests.missing read/write        (Task 1)
  stagingops.go    WrongStateError/ErrWrongState (Task 1); promoted_at in ArchiveFolder/ArchiveAsset (Task 2)
  archive.go       archive entries, copies, changes, counts, scans, backups, Backup  (Task 2)
  settings.go      archive_rescan_at, backup_dir, archive_skip, SettingError, SettingKeys,
                   Settings.Strings, SetSettings, ParseArchiveSkip, plain reasons    (Task 3)
internal/ingest/
  run.go           knownOutcome: missing → OutcomeMissing; safe needs Missing == 0   (Task 4)
  describe.go      the missing sentence                                              (Task 4)
internal/staging/locks.go   NameHeld                                                 (Task 5)
internal/indexer/
  indexer.go       Config, Service, New, Close (6); Status, Progress, OnChange, StartRescan, report (9)
  walk.go          built-in and setting skip rules, file, ScanResult, the listing    (Task 6)
  hash.go          hashFile (context-aware)                                          (Task 7)
  scan.go          the pass: hashing, resolve (7, 8), checking (8), wait/pause and re-read (10)
  stats.go         Stats, Changes, Accept                                            (Task 9)
  pause.go         Pause, Resume                                                     (Task 10)
  schedule.go      nextRun, the nightly loop, SettingsChanged                        (Task 11)
  backup.go        BackupResult, BackupDir, CheckBackupDir, backup, prune            (Task 11)
internal/api/
  archive.go       MountArchive: stats, rescan, pause, resume, changed, accept, indexer.progress (Task 12)
  settings.go      MountSettings: GET/PUT /api/settings                              (Task 12)
  events.go        EventIndexerProgress                                              (Task 12)
cmd/sd2nas/main.go indexer wiring (Busy, shared Locks), MountArchive, MountSettings  (Task 12)
web/src/lib/
  archive.js (+ archive.test.js)   scanLine, scanPercent, progressDetail, scanSummary, sizeChange,
                                   backupLine, lastScanLabel                         (Task 13)
  messages.js (+ test)             missingCopies                                     (Task 13)
  live.svelte.js                   indexer.progress                                  (Task 13)
web/src/pages/Archive.svelte       the Archive index page                            (Task 14)
web/src/components/Nav.svelte      Archive index tab with the changed pill           (Task 14)
web/src/App.svelte                 /archive route                                    (Task 14)
web/src/pages/IngestDetail.svelte  missing stat and list                             (Task 14)
docs/PLAN.md                                                                         (Task 15)
```

---

### Task 1: Catalog schema v3, the `missing` ingest outcome, `ErrWrongState`

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/catalog/schema.go`, `catalog.go`, `ingests.go`, `stagingops.go`, `catalog_test.go`, `ingests_test.go`, `stagingops_test.go`

**Interfaces:**
- Consumes: the existing migration runner (`migrations`, `d.tx`), `Counts`, `changeAsset`.
- Produces:

```go
const SchemaVersion = 3
const OutcomeMissing Outcome = "missing" // the card file's asset is missing from the archive
// Counts gains (counted in Seen):
Missing int `json:"missing"`

// ErrWrongState is what every *WrongStateError matches with errors.Is.
var ErrWrongState = errors.New("catalog: the asset is in another state")

// WrongStateError: an asset update found the asset in a state it doesn't apply to.
// Error() is exactly today's text, "<verb> <sha>: the asset is <state>" (pre-flight C2).
type WrongStateError struct {
	Verb, SHA256 string
	State        AssetState
}
func (e *WrongStateError) Error() string
func (e *WrongStateError) Is(target error) bool // target == ErrWrongState
```

- [ ] **Step 1: Write the failing tests**
  - `catalog_test.go` `TestOpenCreatesSchema`: the expected table list becomes `{"aliases", "archive_changes", "archive_copies", "archive_scans", "assets", "clock_warnings", "fingerprints", "ingest_files", "ingests", "settings", "staging_folders"}` (keep the query's order; add `sqlite_sequence` only if the query lists it — check what it returns for v2's AUTOINCREMENT table today and follow that).
  - `ingests_test.go` `TestMigrateV2ToV3`:
    - Build a v2 database by hand: `schemaV1`, `schemaV2` and `PRAGMA user_version = 2` on a raw `sql.Open("sqlite", path)`.
    - Insert one ingest, three `ingest_files` rows (outcomes `new`, `in_archive`, `seen_before`, distinct card paths), one archived asset with a `first_ingest`, and one with none.
    - `Open` → `Version` 3.
    - `IngestFiles(1)` returns the three rows in the same order with the same fields.
    - An `AddIngestFiles` row with `OutcomeMissing` is accepted, and `Ingest(1).Counts.Missing` is 0.
    - `pragma_index_list('ingest_files')` has `ingest_files_ingest` and `ingest_files_sha256`.
    - The imported archived asset has `promoted_at` = its `updated_at`; the other has NULL. Both have `checked_at` NULL.

    Also update `TestMigrateV1ToV2`'s version check to `SchemaVersion`.
  - `TestCountsAddMissing`: `Add(OutcomeMissing)` → `Seen 1, Missing 1`.
  - `TestFinishIngestStoresMissing`: `FinishIngest` with `Counts{Seen: 2, Missing: 2}` → read back `Missing 2`.
  - `stagingops_test.go` `TestChangeAssetWrongState`: `CullAsset` on an archived asset → `errors.Is(err, ErrWrongState)`, `errors.As` gives `State archived`, and `err.Error()` is exactly `cull <sha>: the asset is archived`. An unknown hash is still `ErrNotFound`.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/catalog/ -run 'Schema|Migrate|Missing|WrongState'` → failures (version 2, undefined names).
- [ ] **Step 3: Implement.** Append `schemaV3` to `migrations` exactly:

```go
// schemaV3 (M5): the archive indexer. Imports record files whose archive
// copy is missing; the indexer keeps duplicate paths, in-place changes,
// when it last read each file, and its scan history; promotes record when
// they happened.
const schemaV3 = `
ALTER TABLE ingests ADD COLUMN missing INTEGER NOT NULL DEFAULT 0;  -- files whose archive copy is missing (block safe_to_format)

-- SQLite can't change a CHECK constraint: rebuild ingest_files with the
-- 'missing' outcome, keeping rowids (IngestFiles orders by rowid).
CREATE TABLE ingest_files_new (
  ingest_id  INTEGER NOT NULL REFERENCES ingests(id),
  card_path  TEXT NOT NULL,
  size       INTEGER NOT NULL,
  quick_fp   BLOB,
  sha256     BLOB,
  outcome    TEXT NOT NULL CHECK (outcome IN ('new','in_archive','in_staging','seen_before','failed','unsupported','missing')),
  error      TEXT NOT NULL DEFAULT ''
);
INSERT INTO ingest_files_new (rowid, ingest_id, card_path, size, quick_fp, sha256, outcome, error)
  SELECT rowid, ingest_id, card_path, size, quick_fp, sha256, outcome, error FROM ingest_files;
DROP TABLE ingest_files;
ALTER TABLE ingest_files_new RENAME TO ingest_files;
CREATE INDEX ingest_files_ingest ON ingest_files(ingest_id);
CREATE INDEX ingest_files_sha256 ON ingest_files(sha256);

ALTER TABLE assets ADD COLUMN checked_at TEXT;   -- the indexer last read the file at rel_path then; NULL = never
ALTER TABLE assets ADD COLUMN promoted_at TEXT;  -- set by promote; "Recently promoted" orders by it
UPDATE assets SET promoted_at = updated_at WHERE state = 'archived' AND first_ingest IS NOT NULL;

-- Other archive paths holding the content of an archived asset (an asset
-- has one rel_path). No foreign key: a copy can outlive its asset (Forget
-- deletes staged and culled ones); the indexer resolves such a copy again.
CREATE TABLE archive_copies (
  rel_path   TEXT PRIMARY KEY,
  sha256     BLOB NOT NULL CHECK (length(sha256) = 32),
  size       INTEGER NOT NULL,
  mtime      INTEGER NOT NULL,
  checked_at TEXT NOT NULL
);
CREATE INDEX archive_copies_sha256 ON archive_copies(sha256);

-- "Changed since indexed": archive files whose content changed in place,
-- until accepted. One row per path; a re-change gets a new id.
CREATE TABLE archive_changes (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  rel_path   TEXT NOT NULL UNIQUE,
  old_sha256 BLOB NOT NULL CHECK (length(old_sha256) = 32),
  sha256     BLOB NOT NULL CHECK (length(sha256) = 32),
  old_size   INTEGER NOT NULL,
  size       INTEGER NOT NULL,
  indexed_at TEXT NOT NULL,                 -- when the old content was last indexed
  changed_at TEXT NOT NULL
);

-- Indexer passes and nightly backups; result is JSON owned by internal/indexer.
-- A pass with NULL finished_at is running (or the process died).
CREATE TABLE archive_scans (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  kind          TEXT NOT NULL CHECK (kind IN ('manual','nightly','backup')),
  reread        INTEGER NOT NULL DEFAULT 0,   -- a full re-index: files not checked since reread_before are re-read
  reread_before TEXT,
  started_at    TEXT NOT NULL,
  finished_at   TEXT,
  complete      INTEGER NOT NULL DEFAULT 0,   -- ran to the end; an incomplete re-index is continued by the next pass
  result        TEXT NOT NULL DEFAULT ''
);
`
```

  Then:
  - `SchemaVersion = 3`;
  - `OutcomeMissing`, and `Counts.Missing` handled by `Add`;
  - `validOutcome` accepts `missing`;
  - `ingestCols`, `scanIngest` and `FinishIngest` read and write `missing` (after `seen_before`, keeping the existing order otherwise);
  - `changeAsset`'s final error becomes `&WrongStateError{Verb: verb, SHA256: sha, State: AssetState(state)}`.
- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS. Ingest's `TestKnownOutcomeRefusesUnknownState` still passes: ingest changes in Task 4.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/
git commit -m "Add catalog schema 3 for the archive indexer and the missing outcome

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Catalog archive queries, promote's `promoted_at`, and the backup

Suggested implementer: sonnet (opus reviewer: the change transaction).

**Files:**
- Create: `internal/catalog/archive.go`, `internal/catalog/archive_test.go`
- Modify: `internal/catalog/stagingops.go`, `internal/catalog/stagingops_test.go`

**Interfaces:**
- Consumes:
  - schema v3 (Task 1);
  - `insertAsset`/`insertFingerprint`, `hexToBlob`/`blobToHex`, `formatTime`/`parseTime`;
  - `prefixRange`, `changeAsset`, `rewritePrefix`;
  - `ErrWrongState`, `ErrNotFound`.
- Produces:

```go
// ArchiveRoot is the rel_path prefix of the archive (§3).
const ArchiveRoot = "Archive/"

// ArchiveEntry is an archived or missing asset under Archive/: what a scan compares the disk with.
type ArchiveEntry struct {
	SHA256    string
	RelPath   string
	Size      int64
	Mtime     int64
	State     AssetState // archived | missing
	UpdatedAt time.Time
	CheckedAt time.Time  // zero: never read by the indexer
}
// ArchiveCopy is another archive path holding an asset's content.
type ArchiveCopy struct {
	RelPath    string
	SHA256     string
	Size       int64
	Mtime      int64
	CheckedAt  time.Time
	AssetState AssetState // the asset's state now; "" when no asset has this hash any more
}
type ArchiveChange struct {
	ID        int64     `json:"id"`
	RelPath   string    `json:"rel_path"`
	OldSHA256 string    `json:"old_sha256"`
	SHA256    string    `json:"sha256"`
	OldSize   int64     `json:"old_size"`
	Size      int64     `json:"size"`
	IndexedAt time.Time `json:"indexed_at"`
	ChangedAt time.Time `json:"changed_at"`
}
type MissingFile struct {
	RelPath string    `json:"rel_path"`
	Size    int64     `json:"size"`
	Since   time.Time `json:"since"` // updated_at: when it was marked missing
}
type RecentFolder struct {
	Folder string    `json:"folder"` // "Archive/<first path element>"
	Files  int       `json:"files"`
	At     time.Time `json:"at"`     // newest promoted_at of its files
}
type ArchiveCounts struct {
	Files    int   // archived assets under Archive/
	Bytes    int64 // their sizes
	Missing  int
	Changed  int   // archive_changes rows
	Copies   int
	Imported int   // assets with a first_ingest, any state ("files in ingest history")
}
type ArchiveScan struct {
	ID           int64
	Kind         string    // manual | nightly | backup
	Reread       bool
	RereadBefore time.Time // zero unless Reread
	StartedAt    time.Time
	FinishedAt   time.Time // zero: unfinished
	Complete     bool
	Result       string    // JSON written by internal/indexer
}

func (d *DB) ArchiveEntries(ctx context.Context) ([]ArchiveEntry, error)
func (d *DB) ArchiveEntryAt(ctx context.Context, relPath string) (ArchiveEntry, error) // the archived asset at relPath; ErrNotFound
func (d *DB) ArchiveCopies(ctx context.Context) ([]ArchiveCopy, error)
func (d *DB) IndexAsset(ctx context.Context, a Asset, quickFP string, checkedAt time.Time) error
func (d *DB) SetArchivePath(ctx context.Context, sha, relPath string, mtime int64, at time.Time) error
func (d *DB) MarkMissing(ctx context.Context, sha string, at time.Time) error
func (d *DB) RecordArchiveChange(ctx context.Context, old ArchiveEntry, sha, quickFP string, size, mtime int64, at time.Time) error
func (d *DB) PutArchiveCopy(ctx context.Context, c ArchiveCopy) error
func (d *DB) DeleteArchiveCopy(ctx context.Context, relPath string) error
func (d *DB) ArchiveChanges(ctx context.Context, limit int) (changes []ArchiveChange, total int, latestID int64, err error)
func (d *DB) AcceptArchiveChanges(ctx context.Context, upTo int64) (int, error)
func (d *DB) ArchiveCounts(ctx context.Context) (ArchiveCounts, error)
func (d *DB) MissingFiles(ctx context.Context, limit int) ([]MissingFile, error)
func (d *DB) RecentArchiveFolders(ctx context.Context, limit int) ([]RecentFolder, error)
func (d *DB) StartArchiveScan(ctx context.Context, kind string, rereadBefore time.Time, at time.Time) (int64, error) // rereadBefore zero = not a re-index
func (d *DB) FinishArchiveScan(ctx context.Context, id int64, at time.Time, complete bool, result string) error
func (d *DB) LastArchiveScan(ctx context.Context) (ArchiveScan, error)   // newest finished pass; ErrNotFound when none
func (d *DB) UnfinishedArchiveScans(ctx context.Context) ([]ArchiveScan, error)
func (d *DB) PendingReread(ctx context.Context) (time.Time, error)       // reread_before of an unfinished re-index; zero when none
func (d *DB) RecordArchiveBackup(ctx context.Context, at time.Time, ok bool, result string) error
func (d *DB) LastArchiveBackup(ctx context.Context) (ArchiveScan, error) // ErrNotFound when none
func (d *DB) Backup(ctx context.Context, path string) error
```

Semantics (each is one statement or one `d.tx`):
- `ArchiveEntries` returns every asset with `state IN ('archived','missing')` and `rel_path` in `prefixRange(ArchiveRoot)`, ordered by `rel_path`. `ArchiveEntryAt` returns the `archived` row at exactly `relPath`.
- `ArchiveCopies` returns every `archive_copies` row, `LEFT JOIN assets` for the state, ordered by `rel_path`.
- `IndexAsset` is `InsertAsset` plus `checked_at = checkedAt`, in the same transaction.
- `SetArchivePath` = `changeAsset(ctx, "set archive path", sha, []AssetState{StateArchived, StateMissing, StateCulled}, "state = 'archived', rel_path = ?, mtime = ?, updated_at = ?, checked_at = ?", …)`, with `checked_at = at`. A `staged` asset is `ErrWrongState`. Move, touch, reappear and recover are all this; it never touches `promoted_at`.
- `MarkMissing` = `changeAsset(…, []AssetState{StateArchived}, "state = 'missing', updated_at = ?")`, `rel_path` kept.
- `RecordArchiveChange`:
  - `ErrBadHash` for bad hashes; `old.SHA256 == sha` is an error.
  - In one transaction, exactly:

```sql
-- 1. The new content becomes the asset; kind, capture time, offset, first ingest and promoted_at stay.
INSERT INTO assets (sha256, size, kind, capture_time, tz_offset, state, rel_path, mtime, first_ingest, updated_at, checked_at, promoted_at)
  SELECT :new, :size, kind, capture_time, tz_offset, 'archived', :rel, :mtime, first_ingest, :at, :at, promoted_at
  FROM assets WHERE sha256 = :old AND state = 'archived';
-- (no row inserted → ErrWrongState, or ErrNotFound when :old is unknown; roll back)
-- 2. Fingerprints: the old content's values are kept and find the new asset (the card original stays a duplicate, §6).
INSERT OR IGNORE INTO fingerprints (quick_fp, sha256) SELECT quick_fp, :new FROM fingerprints WHERE sha256 = :old;
DELETE FROM fingerprints WHERE sha256 = :old;
INSERT OR IGNORE INTO fingerprints (quick_fp, sha256) VALUES (:fp, :new);   -- skipped when quickFP is ""
-- 3. Aliases: :new may be an earlier version of some asset (a revert): it is an asset again.
DELETE FROM aliases WHERE old_sha256 = :new;
UPDATE aliases SET sha256 = :new WHERE sha256 = :old;
INSERT INTO aliases (old_sha256, sha256, seen_at) VALUES (:old, :new, :at);
-- 4. History and warnings follow the content.
UPDATE ingest_files SET sha256 = :new WHERE sha256 = :old;
UPDATE clock_warnings SET sha256 = :new WHERE sha256 = :old;
DELETE FROM archive_copies WHERE sha256 = :old;    -- stale: the indexer moves an asset to a seen copy instead of calling this
-- 5. The old row.
DELETE FROM assets WHERE sha256 = :old;
-- 6. The change entry: a re-change of the same path keeps the first old hash, old size and indexed time, and gets a new id.
--    Read the existing row for :rel (if any) first, then:
DELETE FROM archive_changes WHERE rel_path = :rel;
INSERT INTO archive_changes (rel_path, old_sha256, sha256, old_size, size, indexed_at, changed_at)
  VALUES (:rel, :firstOld, :new, :firstOldSize, :size, :firstIndexedAt, :at);
```

  - `:rel` is `old.RelPath`. For a first change, `indexed_at` is `old.CheckedAt`, or `old.UpdatedAt` when that is zero.
  - If the new hash equals the first old hash of an existing entry (changed and then reverted), the entry is deleted instead of re-inserted.
- `PutArchiveCopy` upserts by `rel_path`. `DeleteArchiveCopy` of an absent row is not an error.
- `ArchiveChanges` returns up to `limit` rows **ordered by `id`**, `total` = `COUNT(*)`, and `latestID` = the highest id among the returned rows (0 when none).
- `AcceptArchiveChanges` runs `DELETE … WHERE id <= ?` and returns rows affected; `upTo < 1` → error.
- `ArchiveCounts`: one query per number is fine. `MissingFiles` returns `state = 'missing'` under `Archive/`, by `rel_path`, up to `limit`.
- `RecentArchiveFolders`:

```sql
SELECT substr(rel_path, 1, 8 + instr(substr(rel_path, 9), '/') - 1) AS folder, COUNT(*), MAX(promoted_at)
FROM assets
WHERE state = 'archived' AND promoted_at IS NOT NULL AND rel_path >= 'Archive/' AND rel_path < 'Archive0'
  AND instr(substr(rel_path, 9), '/') > 0
GROUP BY folder ORDER BY MAX(promoted_at) DESC, folder LIMIT ?
```

  (`'Archive/'` is 8 characters; `substr`/`instr` count characters, so non-ASCII names are fine.)
- `StartArchiveScan` inserts `(kind, reread, reread_before, started_at)`; `kind` must be `manual` or `nightly`.
- `FinishArchiveScan` sets `finished_at`, `complete` and `result` (exactly one row, else `ErrNotFound`). It then deletes all but the newest 30 rows with `kind != 'backup'`.
- `LastArchiveScan` and `UnfinishedArchiveScans` look only at `kind != 'backup'`.
- `PendingReread`: take the newest row with `reread = 1`. If it is not `complete`, return its `reread_before`; otherwise zero. (Every pass started while a re-index is pending is itself `reread = 1` with the same `reread_before`, so the newest such row decides.)
- `RecordArchiveBackup` inserts a finished `kind = 'backup'` row (`started_at = finished_at = at`, `complete = ok`) and keeps the newest 30 backup rows. `LastArchiveBackup` returns the newest backup row.
- `Backup` runs `d.db.ExecContext(ctx, "VACUUM INTO ?", path)`, wrapped `back up catalog to <path>: %w`.
- `stagingops.go`: `ArchiveFolder` and `ArchiveAsset` also set `promoted_at = at` on the rows they archive. Do it either by giving `rewritePrefix` an extra SET column used only by `ArchiveFolder`, or with a second `UPDATE` in the same transaction; the rename path's statement must not change.

- [ ] **Step 1: Write the failing tests** (`archive_test.go`, with a helper that inserts an archived asset with a fingerprint and one ingest-file row naming it):
  - `TestArchiveEntriesAndCopies`:
    - Insert an archived asset under `Archive/`, a missing one under `Archive/`, a staged one under `Staging/`, and an archived one under `Archive2/` (prefix trap) → the entries are the first two.
    - `ArchiveEntryAt` finds the archived one and returns `ErrNotFound` for the missing one's path.
    - A copy of the archived asset and a copy whose hash has no asset → `AssetState` `archived` and `""`.
  - `TestIndexAssetSetsCheckedAt`.
  - `TestSetArchivePath`: from archived (move), missing (reappear) and culled (recover, where `rel_path` was NULL) → archived at the new path with the new mtime and `checked_at`, `promoted_at` unchanged. From staged → `ErrWrongState`, row unchanged.
  - `TestMarkMissing`: archived → missing with `rel_path` kept; missing again → `ErrWrongState`.
  - `TestRecordArchiveChange`:
    - Setup: old asset A has kind raw, a capture time, first ingest 1, `promoted_at` set, fingerprint FA, one alias X→A, an ingest-file row naming A and a clock warning naming A. Change it to B with fingerprint FB.
    - `AssetsByHash(A)` is empty. B is archived at the same path, with the new size and mtime, and keeps kind, capture time, first ingest and `promoted_at`.
    - `AssetBySHA(A)` → B (alias), and `AssetBySHA(X)` → B.
    - `AssetsByFingerprint(FA)` and `(FB)` → B.
    - The `IngestFiles(1)` row has `SHA256 B` and `RelPath` the archive path.
    - `ArchiveChanges` → one entry: old A, new B, old size, size, and `indexed_at` = A's `checked_at`.
  - `TestRecordArchiveChangeTwiceAndRevert`:
    - A→B, then B→C → one entry (old A, new C), with an id greater than the first.
    - Then C→A → `AssetsByHash(A)` is the asset again, no alias has `old_sha256 = A`, `AssetBySHA(C)` → A, and the entry is gone.
  - `TestRecordArchiveChangeWrongState`: when the old row is missing or staged → `ErrWrongState`, and nothing changed (the transaction rolled back: old row, fingerprints and aliases intact).
  - `TestArchiveChangesOrderAndLatest`: entries created in the order `c.JPG`, `a.JPG`, `b.JPG` → listed in that id order. With `limit 2`, `latestID` is the second id and `total` is 3.
  - `TestAcceptArchiveChanges`: three entries → accept with `upTo` = the second id → 2 accepted, one left. `upTo 0` → error.
  - `TestArchiveCounts`, `TestMissingFiles`.
  - `TestRecentArchiveFolders`: two folders, one with a non-ASCII name. A file directly in `Archive/` is excluded, and so are assets without `promoted_at`. The order is newest `promoted_at` first, a later `SetArchivePath` (a move) does not change it, and `limit` is honoured.
  - `TestArchiveScans`:
    - Start two passes and finish the first → `LastArchiveScan` is the first, and `UnfinishedArchiveScans` is the second.
    - A backup row never shows in either.
    - Finishing 31 passes leaves 30 pass rows and does not delete backup rows.
  - `TestPendingReread`:
    - No re-index → zero.
    - Start a re-index with `rereadBefore` T and finish it incomplete → T.
    - A later re-index row with the same T, finished complete → zero.
  - `TestArchiveBackups`: `RecordArchiveBackup` twice → `LastArchiveBackup` is the second. 31 backups leave 30.
  - `TestBackupVacuumInto`: insert an asset, then `Backup(path)` in a temp dir → `Open(path)` works, the version is 3, and the asset is there. A second `Backup` onto that first backup (a non-empty file) → error.
  - `stagingops_test.go`: `TestArchiveFolderSetsPromotedAt` (and the same for `ArchiveAsset`): the archived rows have `promoted_at = at`; `RenameStagingFolder` doesn't set it.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/catalog/ -run 'Archive|Backup|Recent|Missing|Index|Pending|Promoted'` → build failure.
- [ ] **Step 3: Implement** `archive.go` and the `stagingops.go` change, as specified. Never keep a `*sql.Rows` open across another call: collect slices first.
- [ ] **Step 4: Run**: `go test ./internal/catalog/ ./internal/staging/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/
git commit -m "Add the catalog queries for the archive index and the VACUUM INTO backup

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Settings for the rescan time, backup folder and skip list; settings as strings

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/catalog/settings.go`, `internal/catalog/settings_test.go`

**Interfaces:**
- Produces:

```go
const (
	KeyArchiveRescanAt = "archive_rescan_at"
	KeyBackupDir       = "backup_dir"
	KeyArchiveSkip     = "archive_skip"
)
// DefaultArchiveSkip is §9's skips, as the setting's canonical text.
const DefaultArchiveSkip = "Output/, CaptureOne/, *.cosessiondb"

// Settings gains (all comparable, so Settings stays comparable with ==):
ArchiveRescanAt time.Duration // nightly run, since local midnight; default 3h
BackupDir       string        // "" = <photos>/Management/SD2NAS (the indexer resolves it)
ArchiveSkip     string        // canonical text of archive_skip; default DefaultArchiveSkip

// SettingError is a value a key doesn't accept; Error() keeps today's text
// ("setting <key> = "<value>": <reason>").
type SettingError struct{ Key, Value, Reason string }

func SettingKeys() []string                      // every known key, in a fixed order
func (s Settings) Strings() map[string]string    // canonical text of every key
func (d *DB) SetSettings(ctx context.Context, kv map[string]string) error // validate all, store all in one transaction
// ParseArchiveSkip splits and validates an archive_skip value: the entries
// in order (trimmed, empty ones dropped), as written; matching is the indexer's.
func ParseArchiveSkip(s string) ([]string, error)
```

Rules:
- `archive_rescan_at` uses the `day_starts_at` pattern (`HH:MM`); reason `want HH:MM, 00:00 to 23:59`.
- `backup_dir` is `""` or `filepath.IsAbs`, ≤ 1024 bytes, no control characters; reason `want an absolute path, or "" for the default`.
- `archive_skip` follows the Global Constraints:
  - each entry is a `path.Match` pattern, valid when neither `path.Match(e, "")` nor `path.Match(e, "x")` returns `path.ErrBadPattern`, after removing one trailing `/`;
  - no other `/`, no `\`, no control characters;
  - ≤ 50 entries, ≤ 2048 bytes;
  - reason `want a comma-separated list such as "Output/, CaptureOne/, *.cosessiondb"`.

  The stored and canonical value is the entries joined with `, `, so `" Output/ ,,*.cosessiondb"` stores as `Output/, *.cosessiondb`.
- Plain reasons for the existing keys (pre-flight C16): `free_space_reserve_gb` → `want a whole number of GB`; `staging_warn_days` → `want 0 (off) to 3650 days` for every failure, non-numbers included.
- `Strings()`:
  - `day_starts_at` and `archive_rescan_at` as `%02d:%02d`;
  - `verify_known` as stored;
  - `free_space_reserve_gb` as `ReserveBytes / 1e9`;
  - `staging_warn_days` as a decimal;
  - `backup_dir` and `archive_skip` as stored.
- Every `apply` failure for a known key becomes `*SettingError`; an unknown key stays `unknown setting "<key>"`.
- `Settings()` loops over `SettingKeys()` instead of its literal list.
- `SetSettings` with an empty map is an error, and any invalid key or value stores nothing.

- [ ] **Step 1: Write the failing tests**
  - Update `TestSettingsDefaults`'s struct literal: `ArchiveRescanAt: 3*time.Hour, ArchiveSkip: DefaultArchiveSkip`.
  - Round trips: `02:30`, an absolute temp path, and `archive_skip` `"Output/, Exports/, *.xmp"`.
  - Rejects `24:00`, `3:00`, `relative/dir`, a value with `\n`, and the skip values `"a/b"`, `"Output\\"`, `"[x"`, `"Out[/"` and 51 entries.
  - `ParseArchiveSkip(" Output/ ,,*.cosessiondb ")` → `["Output/", "*.cosessiondb"]`; `ParseArchiveSkip("")` → empty, no error.
  - `errors.As(err, &se)` gives `Key` and `Reason`; `free_space_reserve_gb "abc"` has the plain reason.
  - `Strings()` of the defaults has all seven keys, and feeding it to `SetSettings` succeeds and changes nothing.
  - `SetSettings` with one bad value stores none of the good ones.
  - Keep `TestSetSettingRejects` passing.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/catalog/ -run Setting` → build failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/settings.go internal/catalog/settings_test.go
git commit -m "Add the nightly rescan time, backup folder and archive skip settings

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Imports block "safe to format" on missing archive copies

Suggested implementer: haiku.

**Files:**
- Modify: `internal/ingest/run.go`, `internal/ingest/describe.go`, `internal/ingest/run_test.go`, `internal/ingest/describe_test.go`

**Interfaces:**
- Consumes: `catalog.OutcomeMissing`, `Counts.Missing` (Task 1).
- Produces: no new exports. `knownOutcome(StateMissing)` = `OutcomeMissing`; `stateRank` is unchanged (archived still wins over missing for a fingerprint matching both).

- [ ] **Step 1: Update and write tests**
  - `TestKnownOutcomeRefusesUnknownState`: `StateMissing` → `OutcomeMissing`.
  - `TestRunKnownOutcomes`: expect `Counts{Seen: 3, InArchive: 1, SeenBefore: 1, Missing: 1}` and **not** safe to format; the missing file's row has outcome `missing`.
  - `TestRunMissingBlocksSafeToFormat`:
    - A card whose only file matches a `missing` asset (archive path set) → status done, `SafeToFormat false`, `Counts.Missing 1`.
    - The `Describe` message ends with `1 file's archive copy is missing. Do not format the card.`
    - When the same fingerprint also matches an `archived` asset → `in_archive`, and the card is safe.
  - `describe_test.go` `TestDescribeMissing`:
    - `Counts{Seen: 42, New: 0, InArchive: 40, Missing: 2}` → `SONY_A7IV: 0 new, 40 already known, 2 files' archive copies are missing. Do not format the card.` (level warning).
    - With `Failed: 1`, the failed part comes before the missing part; with `Unsupported`, the unsupported part comes after it.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/ingest/ -run 'Known|Missing|Describe'`.
- [ ] **Step 3: Implement**:
  - `knownOutcome` maps `StateMissing` to `OutcomeMissing`.
  - In `finish`, `safe := … && c.Missing == 0 && …`.
  - In `describeDone`, `known` stays `InArchive + InStaging + SeenBefore`, and after the failed part add:

```go
	if c.Missing > 0 {
		body += ", " + missingCopies(c.Missing)
	}
```

  with `missingCopies(1) = "1 file's archive copy is missing"`, otherwise `fmt.Sprintf("%d files' archive copies are missing", n)`. The `ingest finished` log line gains `"missing", c.Missing`.
- [ ] **Step 4: Run**: `go test ./internal/ingest/ ./internal/api/` → PASS; `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/ingest/
git commit -m "Refuse safe to format when a card file's archive copy is missing

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: `Locks.NameHeld` for the indexer

Suggested implementer: haiku.

**Files:**
- Modify: `internal/staging/locks.go`, `internal/staging/locks_test.go`

**Interfaces:**
- Produces:

```go
// NameHeld reports whether directory name rel (a slash path below the
// photos root) or one of its parent directories is locked as a name, e.g.
// by a promote writing into Archive/<target>; folder-id locks don't count.
// Case is ignored, like TryLockSet's name keys. The archive indexer skips
// such directories (M5 Decision 12).
func (l *Locks) NameHeld(rel string) bool {
	k := strings.ToLower(strings.Trim(rel, "/"))
	l.mu.Lock()
	defer l.mu.Unlock()
	for k != "" {
		if _, ok := l.held[lockKey{name: k}]; ok {
			return true
		}
		i := strings.LastIndex(k, "/")
		if i < 0 {
			return false
		}
		k = k[:i]
	}
	return false
}
```

- [ ] **Step 1: Write the failing test** `TestNameHeld`:
  - Take `TryLockSet(HolderPromote, LockSet{IDs: {7}, Names: {"Archive/2025/05/2025-05-30 - Trip"}})`.
  - True for `archive/2025/05/2025-05-30 - TRIP` and `Archive/2025/05/2025-05-30 - Trip/Capture/x.ARW`.
  - False for `Archive/2025/05`, `Archive/2025/05/2025-05-30 - Trip2` and `Staging/2025-05-30 - Trip`.
  - After release → false.
  - A lock with only `IDs` → false for everything.
  - Run it concurrently with `TryLockSet`/release in a goroutine, for the race detector.
- [ ] **Step 2: Run it and confirm it fails**: `go test ./internal/staging/ -run NameHeld` → build failure.
- [ ] **Step 3: Implement** (code above).
- [ ] **Step 4: Run**: `go test ./internal/staging/` → PASS; the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/staging/locks.go internal/staging/locks_test.go
git commit -m "Let the archive indexer see which archive folders a promote holds

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `internal/indexer` skeleton, skip rules and the listing

Suggested implementer: sonnet.

**Files:**
- Create: `internal/indexer/indexer.go`, `internal/indexer/walk.go`, `internal/indexer/walk_test.go`, `internal/indexer/helpers_test.go`

**Interfaces:**
- Consumes: `catalog.DB`, `catalog.ParseArchiveSkip`/`Settings.ArchiveSkip` (Task 3), `media.DefaultRules`, `media.Rules.KindOf`.
- Produces:

```go
// Package indexer keeps the catalog in line with Archive/ (§9): the first
// pass is the backfill, later ones the nightly rescan; it also backs up the catalog.
package indexer

const (
	ArchiveDir = "Archive" // must equal staging.ArchiveDir and catalog.ArchiveRoot without its "/" (internal/api test)
	StagingDir = "Staging" // must equal staging.Dir
)

// Locks reports archive directories a promote is writing into; the shared
// *staging.Locks satisfies it (Decision 12).
type Locks interface{ NameHeld(rel string) bool }

type Config struct {
	DB         *catalog.DB
	PhotosRoot string
	Locks      Locks            // nil: nothing is ever held
	Busy       func() bool      // an import is running or queued, or a promote runs: wait between files; nil: never
	Now        func() time.Time // nil: time.Now
	Log        *slog.Logger     // nil: slog.Default()
}

const (
	TriggerManual  = "manual"
	TriggerNightly = "nightly"
	PhaseListing   = "listing"
	PhaseHashing   = "hashing"
	PhaseChecking  = "checking"
)

// ScanResult is what one pass did; it is stored as JSON in archive_scans
// and sent as Status.Last.
type ScanResult struct {
	Trigger       string    `json:"trigger"`
	Full          bool      `json:"full,omitempty"`  // a re-index (Decision 22)
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	Listed        int       `json:"listed"`         // media files found
	Unchanged     int       `json:"unchanged"`      // not read (size and modified time matched), or read with the same content
	Added         int       `json:"added"`          // new content indexed
	Moved         int       `json:"moved"`          // known content at a new path
	Changed       int       `json:"changed"`        // same path, new content ("changed since indexed")
	Reappeared    int       `json:"reappeared"`     // missing content found again
	Recovered     int       `json:"recovered"`      // culled content found in the archive, now archived
	Missing       int       `json:"missing"`        // newly marked missing
	Copies        int       `json:"copies"`         // new duplicate paths of archived content
	InStaging     int       `json:"in_staging"`     // content still staged (a promote in flight): left alone
	BusyFolders   int       `json:"busy_folders"`   // skipped: a promote held them
	Errors        []string  `json:"errors"`         // at most 20, never null
	ErrorsOmitted int       `json:"errors_omitted,omitempty"`
	Skipped       string    `json:"skipped,omitempty"` // why nothing (more) was done
	Stopped       bool      `json:"stopped,omitempty"` // interrupted: the next pass carries on
}

func New(cfg Config) (*Service, error)
func (s *Service) Close() // stops a pass between files (and mid-file) and the nightly loop; waits; idempotent
```

- `Service` (in this task) holds:
  - `db`, `photosRoot`, `locks`, `busy`, `now`, `log`, `rules` (`media.DefaultRules()`);
  - the seams `readDir func(string) ([]os.DirEntry, error)` (`os.ReadDir`), `hash func(context.Context, string) (string, error)`, `fingerprint func(string) (string, error)` (`media.QuickFingerprint`), `beforeRead func(ctx context.Context, rel string)`, `afterHash func(path string)` and `beforeMissing func(rel string)` (all nil by default; a blocking test hook selects on its `ctx`);
  - `bgCtx`, `bgCancel` and `wg`;
  - `pass sync.Mutex` (held by the running pass);
  - `mu sync.Mutex` guarding `closed`.

  Later tasks add their own fields (Task 9: `status`, `notify`, `lastReport`; Task 10: `userPaused`, `wake`, `idleEvery`; Task 11: `after`, `reschedule`).
- `ScanResult.errorf(format, args…)` works like `staging.RescanResult.errorf`: every problem is logged, the first 20 are listed and the rest counted. `finalError(msg)` lists the problem even past the cap. The result keeps an unexported `log`.

`walk.go`:

```go
// metadataNames are NAS and OS housekeeping names that never hold the
// owner's photos: the same list as staging's skipName (internal/staging/
// reconcile.go); keep them in sync. Dot-names are skipped as well.
var metadataNames = []string{"@eaDir", "@Recycle", "@Recently-Snapshot", "#recycle", "#snapshot",
	"$RECYCLE.BIN", "System Volume Information"}

// builtinSkip: names no setting can make the indexer look at.
func builtinSkip(name string) bool {
	if strings.HasPrefix(name, ".") {
		return true
	}
	for _, m := range metadataNames {
		if strings.EqualFold(name, m) {
			return true
		}
	}
	return false
}

// skipRules are the built-in rules plus archive_skip, fixed for one pass
// (Decision 6).
type skipRules struct {
	media media.Rules
	dirs  []string // lower-cased directory-name patterns ("output"), from entries ending in "/"
	files []string // lower-cased file-name patterns ("*.cosessiondb")
}

func newSkipRules(rules media.Rules, entries []string) skipRules // entries from catalog.ParseArchiveSkip

// dir reports a directory the listing never walks into; depth is its depth
// below Archive/ (1 = a session folder: only built-in rules apply).
func (k skipRules) dir(name string, depth int) bool {
	if builtinSkip(name) {
		return true
	}
	if depth >= 2 {
		n := strings.ToLower(name)
		for _, p := range k.dirs {
			if ok, _ := path.Match(p, n); ok { // patterns were validated by the catalog
				return true
			}
		}
	}
	return false
}

// file reports whether a regular file named name is indexed, and its kind.
func (k skipRules) file(name string) (media.Kind, bool) {
	if builtinSkip(name) {
		return "", false
	}
	n := strings.ToLower(name)
	for _, p := range k.files {
		if ok, _ := path.Match(p, n); ok {
			return "", false
		}
	}
	return k.media.KindOf(name)
}

// reaches reports whether the listing could reach rel_path rel ("Archive/S/Output/x.JPG")
// under these rules; a catalog row it can't reach is left alone (never missing).
func (k skipRules) reaches(rel string) bool {
	parts := strings.Split(strings.TrimPrefix(rel, ArchiveDir+"/"), "/")
	for i, p := range parts {
		if i == len(parts)-1 {
			_, ok := k.file(p)
			return ok
		}
		if k.dir(p, i+1) {
			return false
		}
	}
	return false
}
```

- `file` is the per-path value the pass carries:

```go
type file struct {
	rel         string
	size, mtime int64
	kind        media.Kind
	sha, fp     string               // known content (a copy whose size and time match) / after reading
	editedFrom  *catalog.ArchiveEntry // the archived content this path held (its size or time changed, or a re-index)
}
```

- This task's `scan` holds `s`, `ctx`, `r *ScanResult`, `skip skipRules`, `skipped map[string]bool` (rels the listing didn't read: held, unreadable) and `fresh []file`. Task 7 adds the rest.
- `newScan(ctx, set catalog.Settings, r *ScanResult) (*scan, error)` builds the skip rules from `set.ArchiveSkip`; a parse error fails the pass with that error. Settings are read once, at the start of a pass.
- The listing is a recursive `walk(dir, rel string, depth int) error` over `s.readDir`:
  - Names come back sorted. Symlinks are never followed: only `e.IsDir()` recurses, and only `e.Type().IsRegular()` files count.
  - Before reading a directory below `Archive/`, check `ctx` and `s.held(rel)`. A held directory goes into `skipped`, `BusyFolders` is incremented, and it is not read.
  - A `readDir` error on `Archive/` itself is returned (the caller tells "missing" from other errors). On any other directory it is recorded with `errorf`, and the directory goes into `skipped`.
  - A file whose `Info()` fails with `ErrNotExist` is ignored. Any other `Info()` error is a problem, and the file's path goes into `skipped`.
  - Each indexed file is handed to `sc.found(file{rel, size, mtime, kind})`. In this task `found` appends to `sc.fresh`; Task 7 gives it its real body.
  - `s.held(rel)` is `s.locks != nil && s.locks.NameHeld(rel)`.

`helpers_test.go`:
- `setup(t) (s *Service, db *catalog.DB, photos string)`: a temp catalog and photos root, with `t.Cleanup(s.Close)` registered after the `TempDir`s.
- `put(t, photos, rel, content string, mtime time.Time) string`: writes the file, `os.Chtimes` to whole seconds, returns the OS path.
- `sum(content) string`: SHA-256 hex.
- A fake `Locks`: a `map[string]bool` of lower-cased held names, behind a mutex, ancestor-aware like `NameHeld`.
- `listing(t, s, set) []file`: a pass's listing only, for these tests.

- [ ] **Step 1: Write the failing tests** (`walk_test.go`):
  - `TestSkipRules`, with the default settings:
    - Under `Archive/2025-05-30 - Trip/`, these are **indexed**: `a.JPG`, `Capture/b.ARW`, `Selects/c.JPG`, `Trash/d.ARW`, `Videos/e.MP4`, `Other/f.JPG`, `Edits/g.JPG` (a deny list).
    - These are **skipped**: `Output/h.JPG`, `Capture/CaptureOne/Cache/i.JPG`, `Selects/CaptureOne/j.JPG`, `2025-05-30 - Trip.cosessiondb`, `notes.txt`, `.DS_Store`, `._a.JPG`, `@eaDir/a.JPG/SYNOPHOTO_THUMB_XL.jpg`, `#recycle/k.JPG`.
    - Also indexed: `Archive/l.JPG` (the top level of the archive) and `Archive/Output/m.JPG` (a session folder that happens to be called Output: depth 1, so only built-in rules apply).
    - The listing yields exactly the indexed rel_paths, sorted, with the right kinds and sizes.
  - `TestSkipSetting`, with `archive_skip` set to `Output/, Edits/, *.cosessiondb, IMG_*.JPG`:
    - `Edits/g.JPG` and `Selects/IMG_0001.jpg` (case-insensitive) are skipped.
    - `Capture/CaptureOne/…` is now **indexed** (the entry was removed).
    - With `archive_skip` `""`: `Output/h.JPG` is indexed, and dot and `@eaDir` names are still skipped.
  - `TestSkipRulesReaches`: with the default rules, `Archive/S/Output/x.JPG`, `Archive/S/Capture/CaptureOne/x.JPG`, `Archive/S/.hidden/x.JPG` and `Archive/S/x.txt` → false; `Archive/S/Capture/x.ARW` and `Archive/Output/x.JPG` → true.
  - `TestListingSkipsHeldFolder`: fake locks hold `archive/2025-05-30 - trip` → none of its files are listed, and `BusyFolders` is 1. A nested hold on `Archive/2025/05/T` skips only `T`.
  - `TestListingUnreadableFolder`: `readDir` fails for one session folder → `errorf` records it, the folder is in `skipped`, and its siblings are listed.
  - `TestListingSymlinkNotFollowed` (skip on Windows when `os.Symlink` fails): neither a symlinked directory nor a symlinked file is listed.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/` → build failure.
- [ ] **Step 3: Implement** `indexer.go` (types, `New`, `Close`) and `walk.go`.
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS. `go list -f '{{join .Imports "\n"}}{{"\n"}}{{join .TestImports "\n"}}' ./internal/indexer/` names no `internal/staging`, `internal/ingest` or `internal/api`. `go vet ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Add the archive indexer package and its listing and skip rules

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: The pass: backfill, unchanged skip, resume, staged and culled content

Suggested implementer: sonnet (opus reviewer).

**Files:**
- Create: `internal/indexer/hash.go`, `internal/indexer/scan.go`, `internal/indexer/scan_test.go`
- Modify: `internal/indexer/indexer.go`, `internal/indexer/walk.go`

**Interfaces:**
- Consumes:
  - Task 2's queries;
  - Task 6's listing, `file`, `ScanResult`, `scan`;
  - `media.QuickFingerprint`;
  - `catalog.IndexAsset`, `AssetsByHash`, `AssetsByFingerprint`, `AddFingerprint`, `ArchiveEntryAt`.
- Produces (Task 9 adds status and events around it):

```go
// Rescan runs one pass now and waits for it (tests); it waits for a pass
// that is already running, stops when ctx or Close does, and is ErrClosed
// after Close. err is non-nil only for catalog failures; cancellation sets
// Stopped. full asks for a re-index; it has no effect until Task 10.
func (s *Service) Rescan(ctx context.Context, trigger string, full bool) (ScanResult, error)
var ErrClosed = errors.New("indexer: shutting down")
```

`Rescan` (pre-flight C17):
- under `s.mu`: return `ErrClosed` when `closed`; otherwise `s.wg.Add(1)`;
- `ctx, cancel := context.WithCancel(ctx)`; `stop := context.AfterFunc(s.bgCtx, cancel)`, deferring both;
- `s.pass.Lock()`, then `runPass`.

`hash.go`:

```go
// hashFile is media.HashFile that stops when ctx ends (a 4 GB video must
// not hold up shutdown), reading sequentially in 1 MiB blocks.
func hashFile(ctx context.Context, path string) (sum string, err error)
```

It is implemented with a `ctxReader{ctx, f}` that returns `ctx.Err()` before each `Read`, and `io.CopyBuffer` with a 1 MiB buffer. Errors are wrapped `hash <path>: %w`.

`scan.go` — the `scan` gains:

```go
	dbctx      context.Context                 // context.WithoutCancel(ctx): what was read is recorded
	byPath     map[string]catalog.ArchiveEntry // archived rows
	bySHA      map[string]catalog.ArchiveEntry // the same rows by hash
	copies     map[string]catalog.ArchiveCopy  // by rel_path
	keepCopy   map[string]bool                 // copy rows confirmed or written this pass
	seenCopies map[string]catalog.ArchiveCopy  // by sha: a copy of archived content seen this pass
	seen       map[string]bool                 // sha of archived rows accounted for this pass
	readOther  map[string]bool                 // rel_paths of archived rows read this pass with other content
	changed    []file
```

The pass (`runPass`, called with `s.pass` held). All problems go through `r.errorf`. In order:
1. Read the settings once and build the scan (Task 6).
2. `os.Stat(<photos>/Archive)`: if it doesn't exist → `Skipped` = the "is missing" message; return with nothing changed. Load `ArchiveEntries` (archived ones into `byPath`/`bySHA`; `missing` rows need no map, they are found by hash) and `ArchiveCopies`.
3. **Listing** (`PhaseListing`): `walk`. `found(f)`:

```go
func (sc *scan) found(f file) {
	sc.r.Listed++
	if a, ok := sc.byPath[f.rel]; ok {
		sc.seen[a.SHA256] = true
		if a.Size == f.size && a.Mtime == f.mtime && !sc.mustReread(a.CheckedAt) {
			sc.r.Unchanged++
			return
		}
		f.editedFrom = &a
		sc.changed = append(sc.changed, f)
		return
	}
	if c, ok := sc.copies[f.rel]; ok && c.Size == f.size && c.Mtime == f.mtime && !sc.mustReread(c.CheckedAt) {
		if c.AssetState == catalog.StateArchived {
			sc.keepCopy[f.rel] = true
			sc.seenCopies[c.SHA256] = c
			sc.r.Unchanged++
			return
		}
		f.sha = c.SHA256 // known content whose asset left the archive: resolved again without a full read (still fingerprinted)
	}
	sc.fresh = append(sc.fresh, f) // includes paths of missing rows and changed copies
}
```

   `mustReread` is `false` until Task 10.
4. **Hashing** (`PhaseHashing`): changed paths first, then fresh ones.
   - Each read is preceded by `sc.wait()`, which returns `sc.ctx.Err()` until Task 10, and by a re-check of `s.held(f.rel)`: if it is held now → `BusyFolders++`, skip.
   - `read(f, full bool) (ok bool, err error)`:
     - run the `beforeRead(sc.ctx, rel)` hook, then `os.Lstat` (not exist → skip silently);
     - fingerprint into `f.fp`; if `full`, `hash(ctx, path)` into `f.sha`;
     - run the `afterHash` hook, then `os.Lstat` again. If the size or modified time differs from the first `Lstat` → `r.errorf("%s changed while it was read (still being copied?); the next scan picks it up", rel)` and skip. Otherwise `f.size`/`f.mtime` take the stat's values.
     - Only cancellation and catalog failures are returned as errors.
   - **A changed path** gets a full read.
     - Same hash as `editedFrom` → `SetArchivePath(sha, rel, mtime)` (a touch; it also sets `checked_at`), `Unchanged++`.
     - Otherwise → `delete(sc.seen, editedFrom.SHA256)`, `sc.readOther[f.rel] = true`, then `resolve(f)`. Task 8 completes the edit branch; in this task an unknown hash with `editedFrom` set calls `RecordArchiveChange`, counts `Changed`, and sets `seen[editedFrom.SHA256] = true`, because the old row no longer exists (pre-flight C6).
   - **A fresh path:**
     1. Look it up again: `ArchiveEntryAt(f.rel)`. A promote that finished after the listing has already recorded it (pre-flight C5).
        - Found with the same size and mtime (and, from Task 10, not due for a re-read) → add it to `byPath`/`bySHA`, mark it `seen`, `Unchanged++`, no read.
        - Found with another size or time → add it the same way and handle it as a changed path.
     2. Otherwise **always fingerprint** (pre-flight C4: even a copy with a known hash).
     3. Then the move shortcut (Task 8).
     4. Then a full read unless `f.sha` is known.
     5. Then `resolve(f)`.
5. `resolve(f)` — this task's branches; Task 8 adds moved, copy and reappeared:

```go
	found, err := db.AssetsByHash(sc.dbctx, []string{f.sha})
	if err != nil {
		return err
	}
	a, known := found[f.sha]
	switch {
	case known && a.State == catalog.StateArchived && a.RelPath == f.rel:
		// already recorded here (a promote finished during the pass): never a copy of itself
		sc.seen[a.SHA256] = true
		if a.Mtime != f.mtime {
			err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now)
		}
		sc.r.Unchanged++
	case !known:
		err = db.IndexAsset(sc.dbctx, catalog.Asset{SHA256: f.sha, Size: f.size, Kind: f.kind,
			State: catalog.StateArchived, RelPath: f.rel, Mtime: f.mtime, UpdatedAt: now}, f.fp, now)
		if err == nil {
			sc.r.Added++
			sc.seen[f.sha] = true
		}
	case a.State == catalog.StateCulled:
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now) // M4 Decision 9: a promote stopped before its catalog write
		if err == nil {
			sc.r.Recovered++
		}
	case a.State == catalog.StateStaged:
		sc.r.InStaging++ // a promote is moving it, or a copy of a staging file: the staging side settles it
		return nil
	}
	if errors.Is(err, catalog.ErrWrongState) || errors.Is(err, catalog.ErrNotFound) {
		sc.r.errorf("%s: its catalog entry changed during the scan; the next scan looks again", f.rel)
		return nil
	}
	if err != nil {
		return err
	}
	if f.fp != "" && known {
		err = db.AddFingerprint(sc.dbctx, f.fp, a.SHA256) // insert-or-ignore: later moves of this file take the shortcut
	}
	return err
```

6. Cancellation anywhere → `Stopped = true`, with a `nil` error. Checking (`missing`) is Task 8, and runs only when steps 3–5 finished.

- [ ] **Step 1: Write the failing tests** (`scan_test.go`). Files are tiny text with `.JPG`/`.ARW` names (kinds come from the extension); count full reads by wrapping `s.hash`.
  - `TestBackfillIndexesArchive`:
    - Files: `Archive/2025-05-30 - Trip/a.JPG`, `Archive/2025-05-30 - Trip/Capture/b.ARW`, `Archive/2007-09-31 - Random Party/c.JPG` (folder names are text: Decision 7) → `Added 3, Listed 3`.
    - Each asset is `archived`, with `first_ingest` 0, a zero `capture_time`, the right kind, size, mtime (UnixNano of the file), rel_path and a non-zero `checked_at`.
    - `AssetsByFingerprint(QuickFingerprint(path))` finds each one.
  - `TestRescanSkipsUnchanged`: a second `Rescan` → zero hash calls, `Unchanged 3`, nothing else.
  - `TestRescanSkipsPromotedFiles`: an asset inserted as `archived` with `first_ingest` = an ingest id, at a path whose file has the same size and modified time → not read, unchanged, `first_ingest` kept.
  - `TestPromoteFinishedMidPass`:
    - Start with a staged asset at `Staging/T/x.JPG` and the same file already at `Archive/T/x.JPG` (renamed, same mtime), not held.
    - In the `beforeRead` hook for that path, the test runs `ArchiveFolder` (the promote's catalog write).
    - Result: zero full reads of it, no copy row, `Unchanged 1`, still archived at `Archive/T/x.JPG`.
  - `TestNewFileCopiedInByHand`: after a backfill, add `d.JPG` → `Added 1`, one hash call.
  - `TestBackfillResumes`: five files, and the `beforeRead` hook cancels the pass's context on the third read → `Stopped`, two assets. The next `Rescan` reads exactly three files → five assets, `Unchanged 2`.
  - `TestRescanAfterCloseRefused`: `Close`, then `Rescan` → `ErrClosed`.
  - `TestArchiveMissingChangesNothing`: no `Archive/`, with archived rows in the catalog → `Skipped` is the exact "is missing" message, and the rows are unchanged.
  - `TestStagedContentLeftAlone`: a `staged` asset under `Staging/` whose content is also at `Archive/X/a.JPG` → `InStaging 1`. The asset is still staged at its staging path, and there is no copy row.
  - `TestCulledContentRecovered`: a `culled` asset (no rel_path) whose content is at `Archive/X/a.JPG` → `Recovered 1`; archived at that path with the file's mtime.
  - `TestCopyOfForgottenAssetFingerprinted`:
    - Setup: a copy row whose asset no longer exists, at a path whose size and mtime match.
    - Pass: no full read, the content becomes an archived asset (`Added 1`), and `AssetsByFingerprint` finds it.
  - `TestFileChangingWhileReadSkipped`: the `afterHash` hook appends to the file once → the exact "changed while it was read" error, and no asset. The next pass adds it.
  - `TestTouchedFileOnlyUpdatesMtime`: same content, new modified time → one read, `Unchanged`, mtime updated, no change entry. The following pass reads nothing.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'Backfill|Rescan|Promote|NewFile|Staged|Culled|Forgotten|Changing|Touched|ArchiveMissing'`.
- [ ] **Step 3: Implement** `hash.go`, `scan.go` and `Rescan`. For now `runPass` builds the scan, runs it and returns the result: no status, no persistence.
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Index the archive: backfill, skip unchanged files, resume after a stop

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Moves, copies, changes in place, missing files, busy folders

Suggested implementer: sonnet (opus reviewer: Review Focus 1–4).

**Files:**
- Modify: `internal/indexer/scan.go`, `internal/indexer/scan_test.go`

**Interfaces:**
- Consumes: Task 7; `SetArchivePath`, `MarkMissing`, `RecordArchiveChange`, `PutArchiveCopy`, `DeleteArchiveCopy`.
- Produces: no new exports.

Add to the pass:

```go
// isGone: archived content not seen at its path this pass, and inside what the listing could read.
func (sc *scan) isGone(sha string) bool {
	e, ok := sc.bySHA[sha]
	return ok && !sc.seen[sha] && sc.inScope(e.RelPath)
}

// inScope: this pass's listing could have seen rel: the rules reach it, and
// neither it nor a parent directory was skipped (held, unreadable).
func (sc *scan) inScope(rel string) bool {
	if !sc.skip.reaches(rel) {
		return false
	}
	for p := rel; p != "." && p != "/" && p != ArchiveDir; p = path.Dir(p) {
		if sc.skipped[p] {
			return false
		}
	}
	return true
}
```

- **Move shortcut** (fresh path, `f.sha == ""`, not a re-index): after the fingerprint, call `AssetsByFingerprint(fp)`. The first candidate with `Size == f.size` that is either `missing`, or `archived` with `isGone(sha)`, gives `f.sha` without a full read. Otherwise read fully.
- **`resolve` branches added** (order matters; the "already here" case from Task 7 comes first):

```go
	case !known && f.editedFrom != nil && !sc.hasCopy(f.editedFrom.SHA256):
		err = db.RecordArchiveChange(sc.dbctx, *f.editedFrom, f.sha, f.fp, f.size, f.mtime, now)
		// Changed++, seen[f.sha] = true, seen[f.editedFrom.SHA256] = true (the old row is gone)
	case !known:
		// IndexAsset (Task 7): Added++. An edited path whose old content survives as a copy lands
		// here; the old content moves to that copy in the checking phase.
	case a.State == catalog.StateArchived && sc.isGone(a.SHA256):
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now) // Moved++, seen[sha] = true
	case a.State == catalog.StateArchived:
		// still at its own path, or below a folder this pass skipped: a duplicate in the archive
		err = db.PutArchiveCopy(sc.dbctx, catalog.ArchiveCopy{RelPath: f.rel, SHA256: a.SHA256, Size: f.size, Mtime: f.mtime, CheckedAt: now})
		// Copies++ unless sc.copies already had a row at f.rel; keepCopy[rel] = true; seenCopies[sha] = the copy
	case a.State == catalog.StateMissing:
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now) // Reappeared++
	case a.State == catalog.StateCulled: // Task 7
	case a.State == catalog.StateStaged: // Task 7
```

  `hasCopy(sha)` is `seenCopies[sha]` present.
- **Checking** (`PhaseChecking`) runs only after the hashing phase finished. For each archived row in `byPath`, sorted by `rel_path`, that is not `seen` and is `inScope`:
  1. `sc.wait()`. If `ctx` ended → stop (`Stopped`).
  2. A seen copy of its content → `SetArchivePath` to the copy's path and mtime, `DeleteArchiveCopy(copy)`, `Moved++`, and drop it from `seenCopies`.
  3. `s.held(rel)` → leave it.
  4. Run the `beforeMissing(rel)` hook.
     - If `rel` is in `readOther` (read this pass with other content: pre-flight B11), skip the stat.
     - Otherwise `os.Lstat(abs(rel))`. No error → it is back; leave it (the next pass reads it). An error other than not-exist → `errorf` and leave it.
  5. `os.Stat(<photos>/Archive)` must succeed.
     - Not exist → `Skipped` = the "went missing during the scan" message; stop the checking phase, so nothing more is marked.
     - Another error → `errorf` and stop.
  6. `MarkMissing` → `Missing++`. `ErrWrongState` or `ErrNotFound` → `errorf`, continue.

  Then delete every copy row that is `inScope`, not in `keepCopy`, and not just promoted to primary (its path is gone, or holds changed content). Copy rows out of scope — below a skipped or newly skip-ruled folder — are kept.

- [ ] **Step 1: Write the failing tests** (`scan_test.go`):
  - `TestMoveWithinSession`: backfill, then `os.Rename` `Capture/b.ARW` → `Selects/b.ARW` (keeps mtime) → `Moved 1`, `Missing 0`, `Added 0`, **zero full hash calls** (fingerprint only), rel_path updated.
  - `TestSessionRenamedByHand`: rename the session folder → every file `Moved`, nothing missing or added, zero full hash calls.
  - `TestMovedAndChangedIsNewPlusMissing`: move a file and change its content → the new path is `Added`, and the old asset is `Missing` (it really is gone).
  - `TestRescanChangedContent`: overwrite an archived file with other content (new mtime) → `Changed 1`, `Missing 0`. The old hash is only an alias of the new asset. The change entry lists the old/new hash and sizes. The old fingerprint finds the new asset.
  - `TestChangeReverted`: change A→B (pass), then write the original bytes back (pass) → asset A is archived at the path, `AssetBySHA(B)` → A, and no change entry is left.
  - `TestChangedToKnownContent`: overwrite file P (asset A) with the bytes of archived file Q (asset B) → P becomes a copy of B (`Copies 1`), and A is `Missing 1` in the same pass. The **next pass reads nothing** (P is an unchanged copy).
  - `TestDuplicateRecordedAsCopy`:
    1. The same bytes at two paths in a backfill → one asset (at the first path by sort order) and one copy row.
    2. The next pass reads nothing (`Unchanged 2`).
    3. Delete the primary → `Moved 1` to the copy's path, the copy row is gone, `Missing 0`.
    4. Delete that file too → `Missing 1`.
    5. Separately: deleting a copy while the primary is still there removes the copy row, and nothing is missing.
  - `TestPrimaryGoneMovesToCopy`: a copy is seen, and its primary is edited in place (new content) → the path gets a new asset (`Added`), the old asset moves to the copy (`Moved`), and there is no change entry.
  - `TestGoneFileMarkedMissing`: delete a file → `Missing 1`; the asset is `missing` with rel_path kept, and `MissingFiles` lists it.
  - `TestMissingReappears`: at the same path → `Reappeared 1`, archived. At another path → `Reappeared 1`, via the fingerprint shortcut (no full hash).
  - `TestArchiveVanishesBeforeMissing`: the `beforeMissing` hook renames `Archive/` away on its first call → `Skipped` is the exact "went missing during the scan" message, and zero assets are `missing`.
  - `TestStoppedPassMarksNothingMissing`: an archived row whose file is absent, and a pass cancelled during hashing (in the `beforeRead` hook) → the row is still `archived`.
  - `TestSkippedRowsNeverMissing`: archived rows at `Archive/S/Output/x.JPG`, `Archive/S/Capture/CaptureOne/y.JPG` and `Archive/S/@eaDir/z.JPG`, with no files → they stay `archived`.
  - `TestNewSkipRuleKeepsIndexedRows`:
    - Pass 1 indexes `Archive/S/Edits/a.JPG`. During pass 2, the `beforeRead` hook sets `archive_skip` to the default plus `Edits/`, and pass 2 still indexes a new `Edits/b.JPG`: the rules are fixed at the start.
    - Pass 3: a new `Edits/c.JPG` is not indexed. After deleting `a.JPG`, `a` stays `archived` (not missing) and `b` stays `archived`, with no reads under `Edits/`.
    - Pass 4, after removing the rule: `c` is added, `a` is marked missing, and `b` is unchanged.
  - `TestUnreadableFolderNotMissing`: `s.readDir` fails with a permission error for one session folder that holds an indexed file → a listed problem, and that asset stays `archived`. A sibling folder's deleted file is still marked missing.
  - `TestPromoteHeldFolderSkipped`: fake locks hold `Archive/2025-05-30 - Trip`, which contains a new file and an archived row whose file is absent → neither is indexed nor marked missing, `BusyFolders 1`. After release, the next pass adds the file and marks the row missing.
  - `TestHalfPromotedFolder`:
    - Setup: `CreateStagingFolder("T")`, and staged assets at `Staging/T/…` inserted with the **files' UnixNano mtimes**, while the files themselves are already at `Archive/T/…` (the rename is done, the catalog write is not). The name is held.
    - Pass → skipped.
    - Then the test applies `ArchiveFolder` (the promote's catalog write) and releases → the next pass reads nothing and counts them `Unchanged`.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'Move|Renamed|Changed|Revert|Duplicate|Copy|Missing|Reappear|Vanish|Stopped|Skip|Unreadable|Promote'`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the Linux command and the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Follow moves, duplicates and edits in the archive and mark vanished files missing

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Status, progress, events, "Rescan now", stats and accept

Suggested implementer: sonnet (opus reviewer: locking, shutdown).

**Files:**
- Create: `internal/indexer/stats.go`, `internal/indexer/service_test.go`
- Modify: `internal/indexer/indexer.go`, `internal/indexer/scan.go`

**Interfaces:**
- Consumes: Tasks 2, 3, 7, 8.
- Produces:

```go
// Status is the indexer.progress event data (Decision 19). Task 11 adds
// LastBackup, BackingUp and NextScan; Task 10 adds PendingFull.
type Status struct {
	Version uint64      `json:"version"`   // bumps after each pass, backup, accept and settings change
	Running *Progress   `json:"running"`   // nil when no pass runs
	Last    *ScanResult `json:"last"`      // the last finished pass; nil before the first
	Changed int         `json:"changed"`   // "changed since indexed" entries
	Missing int         `json:"missing"`   // missing assets
}
type Progress struct {
	Trigger     string    `json:"trigger"`
	Full        bool      `json:"full"`          // a re-index
	Phase       string    `json:"phase"`         // "" until the pass starts listing, then listing | hashing | checking
	StartedAt   time.Time `json:"started_at"`
	Listed      int       `json:"listed"`
	ToHash      int       `json:"to_hash"`       // files to read (changed + new); 0 while listing
	Hashed      int       `json:"hashed"`
	BytesToHash int64     `json:"bytes_to_hash"`
	BytesHashed int64     `json:"bytes_hashed"`
	Folder      string    `json:"folder"`        // the session folder being listed or read
	Current     string    `json:"current"`       // the file being read; "" while listing or checking
	PausedBy    string    `json:"paused_by"`     // "" | "user" | "busy" (Task 10)
}
type Stats struct {
	Status
	Files          int                    `json:"files"`
	Bytes          int64                  `json:"bytes"`
	Copies         int                    `json:"copies"`
	Imported       int                    `json:"imported"`
	MissingFiles   []catalog.MissingFile  `json:"missing_files"`  // ≤ 50, never null
	Recent         []catalog.RecentFolder `json:"recent"`         // ≤ 5, never null
	RescanAt       string                 `json:"rescan_at"`      // "03:00"
	BackupDir      string                 `json:"backup_dir"`     // effective
	SkipRules      []string               `json:"skip_rules"`     // effective archive_skip entries, never null
	ArchivePath    string                 `json:"archive_path"`   // OS path of Archive/
	ArchivePresent bool                   `json:"archive_present"`
}
type ChangeList struct {
	Version  uint64                  `json:"version"`
	Changes  []catalog.ArchiveChange `json:"changes"` // ≤ 500, never null
	Total    int                     `json:"total"`
	LatestID int64                   `json:"latest_id"`
}

func (s *Service) Status() Status
func (s *Service) OnChange(fn func(Status)) // before serving; fn runs under s.mu and must only publish
func (s *Service) StartRescan(full bool) bool // a manual pass in the background; false if one runs or after Close (full takes effect in Task 10)
func (s *Service) Stats(ctx context.Context) (Stats, error)
func (s *Service) Changes(ctx context.Context) (ChangeList, error)
func (s *Service) Accept(ctx context.Context, upTo int64) (int, error) // ErrClosed after Close
```

Behaviour:
- **`New`:**
  - Close unfinished `archive_scans` rows with `FinishArchiveScan(…, complete false, …)`, where the result is `{"trigger": kind, "full": reread, "started_at": …, "finished_at": now, "stopped": true, "errors": ["SD2NAS stopped before this scan finished"]}`.
  - Load `LastArchiveScan` into `Last`. `ErrNotFound` → `Last` nil (a fresh catalog); bad JSON → log, `Last` nil.
  - Load `ArchiveCounts` into `Changed`/`Missing`.
  - Other errors are returned (main reports them).
- **`runPass(ctx, trigger, full)`** is called with `s.pass` held: by `Rescan`, and by `StartRescan`'s goroutine.
  - `StartRescan` works under `s.mu`: check `closed`; `s.pass.TryLock()`; `s.wg.Add(1)`; set a `Running` placeholder `{Trigger, StartedAt, Phase: ""}` (pre-flight B15); force-publish; return true.
  - The pass then runs in order:
    1. `StartArchiveScan`.
    2. `Running` gets `Phase: listing` (forced publish).
    3. The pass itself.
    4. Set `FinishedAt`, then `FinishArchiveScan` on `dbctx`, with `complete` = the pass ran to the end (not stopped, not skipped, no catalog error) and the JSON.
    5. Refresh the counts.
    6. Set `Last`, `Running = nil`, `Version++`, and force-publish.
    7. Log `archive scan` with every count, `full`, `stopped`, `skipped` and `problems`.
  - A catalog error becomes the last entry of `Errors` (via `finalError`) and is logged at Error.
- **`progress(force, fn)`** applies `fn` to `status.Running` under `s.mu` (a no-op when nil) and reports.
- **`reportLocked(force)`**: unless `force`, skip when the last publish was less than 250 ms ago (real clock). Otherwise call `notify(statusLocked())`, with `Running`/`Last` copied.
  - Forced: start, each phase change, pause and resume (Task 10), end, backup (Task 11), accept, settings change.
  - The pass updates `Listed` per file found and `Folder` per session folder. When the hashing phase starts it sets `ToHash`/`BytesToHash` (files that need a full read or a fingerprint). It sets `Current` before each read, and `Hashed`/`BytesHashed` after every read.
- **`Stats`:**
  - `Status()` first, for its version;
  - then `ArchiveCounts`, `MissingFiles(50)`, `RecentArchiveFolders(5)`;
  - then `Settings`: `RescanAt` from `Strings()`, `SkipRules` = `ParseArchiveSkip(set.ArchiveSkip)`, and `BackupDir` = `<photos>/Management/SD2NAS` or `set.BackupDir` (Task 11 moves this into `BackupDir`);
  - then `ArchivePath` and `ArchivePresent` (`os.Stat`).
- **`Changes`** reads the version first, then `ArchiveChanges(500)`. **`Accept`**: `ErrClosed` after `Close`; `upTo < 1` → error; `AcceptArchiveChanges`; refresh the counts; `Version++`; force-publish.
- **`Close`**: set `closed = true` under `s.mu`, cancel `bgCtx`, `wg.Wait()`. After `Close`, `StartRescan` is false, `Accept` is `ErrClosed`, and `Status`/`Stats`/`Changes` still work (main closes the database later).

- [ ] **Step 1: Write the failing tests** (`service_test.go`):
  - `TestStartRescanOnce`:
    - The `beforeRead` hook blocks on a channel (and on `ctx.Done()`).
    - `StartRescan` → true, and again → false. `Status().Running` is non-nil with trigger `manual`.
    - Unblock → wait for `Running == nil`. `Last.Added` is set, `Version` has bumped, and `LastArchiveScan` holds the same JSON with `Complete` true.
  - `TestCloseStopsPass`:
    - The `beforeRead` hook blocks on `ctx.Done()` inside the first read; `Close`, run in a goroutine, returns within 10 s.
    - `Last.Stopped` is set, and the scan row is incomplete.
    - A new `Service` over the same catalog and photos finishes the remaining files with one read each.
  - `TestInterruptedScanRecorded`: `StartArchiveScan` by hand, then `New` → `Status().Last.Stopped`, and `Errors` is exactly `["SD2NAS stopped before this scan finished"]`.
  - `TestNewOnFreshCatalog`: `Last` is nil, and there is no error.
  - `TestProgressThrottled`:
    - 300 files; record each `OnChange` frame with `time.Now()`.
    - For consecutive frames where both have `Running != nil` and the same `Phase` and `PausedBy`, the gap is ≥ 240 ms. Only forced frames may come sooner, and every forced frame inside a pass changes `Phase` or `PausedBy`.
    - The last frame has `Running == nil` and `Last.Added == 300`.
  - `TestStatsAndAccept`:
    - After a pass with one change and one missing file, `Stats` has files, bytes, `Changed 1`, `Missing 1`, one `MissingFiles` entry, `RescanAt "03:00"`, `SkipRules ["Output/", "CaptureOne/", "*.cosessiondb"]` and `ArchivePresent`. `Changes` has 1 entry and `LatestID`.
    - `Accept(LatestID)` → 1, `Status().Changed 0`, the version has bumped, and a forced frame was published.
    - `Accept(0)` → error, and `Accept` after `Close` → `ErrClosed`.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'StartRescan|Close|Interrupted|Fresh|Throttled|Stats'`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Report archive scan progress, keep the scan history and accept changed files

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Pausing (automatic and by the owner) and "Re-index everything"

Suggested implementer: sonnet (opus reviewer: Review Focus 5–6).

**Files:**
- Create: `internal/indexer/pause.go`, `internal/indexer/pause_test.go`, `internal/indexer/reindex_test.go`
- Modify: `internal/indexer/indexer.go`, `internal/indexer/scan.go`, `internal/indexer/stats.go`

**Interfaces:**
- Consumes: Task 9's status and `runPass`; `catalog.PendingReread`, `StartArchiveScan(… rereadBefore …)`, `ArchiveEntry.CheckedAt`, `ArchiveCopy.CheckedAt`.
- Produces:

```go
var ErrNotRunning = errors.New("indexer: no scan is running")

func (s *Service) Pause() (Status, error)  // ErrNotRunning, ErrClosed
func (s *Service) Resume() (Status, error) // ErrNotRunning, ErrClosed
// StartRescan(full) and Rescan(…, full) (Tasks 7, 9) now honour full (Decision 22).
// Status gains:
PendingFull bool `json:"pending_full"` // a re-index didn't finish; the next pass continues it
```

The `Service` gains `userPaused bool` (under `s.mu`, reset when a pass ends), `wake chan struct{}` (buffer 1) and `idleEvery` (2 s).

**`wait()`** replaces the stub and runs before each read, before each session folder while listing, and before each missing check (pre-flight A11: `Progress` exists since Task 9):

```go
// wait returns when the pass may go on: not paused by the owner, and no
// import or promote busy (Decisions 13 and 21). busy is called without s.mu:
// main's Busy takes the ingest manager's and the staging service's locks.
func (sc *scan) wait() error {
	for {
		by := ""
		if sc.s.isUserPaused() {
			by = "user"
		} else if sc.s.busy != nil && sc.s.busy() {
			by = "busy"
		}
		sc.s.setPausedBy(by) // forced publish when it changes
		if by == "" || sc.ctx.Err() != nil {
			return sc.ctx.Err()
		}
		select {
		case <-sc.ctx.Done():
			return sc.ctx.Err()
		case <-sc.s.wake: // Resume
		case <-time.After(sc.s.idleEvery):
		}
	}
}
```

- `Pause` sets `userPaused` when a pass is running; `Resume` clears it and does a non-blocking send on `wake`. Both force-publish and return `Status()`. With no pass running → `ErrNotRunning`; after `Close` → `ErrClosed`. Pausing a paused pass, or resuming one that isn't paused, changes nothing and is not an error.
- A file being read when Pause arrives is not interrupted: the read, the catalog write and the progress update finish, and the pass waits before the next read.
- Shutdown while paused: `ctx` ends `wait`, and the pass ends `Stopped` as usual.

**Re-index:**
- At pass start, `runPass` calls `PendingReread`. If it returns `T` ≠ zero, this pass continues that re-index: `rereadBefore = T`, `Full = true`, whatever the trigger. Otherwise, if `full` was asked for, `rereadBefore` = the pass's start time.
- `StartArchiveScan(kind, rereadBefore, now)`.
- `mustReread(t)` is `!rereadBefore.IsZero() && t.Before(rereadBefore)` (a zero `checked_at` is before anything).
- `found` already uses it for rows and copies (Task 7).
- A fresh path's catalog re-look (Task 7) also requires `!mustReread(e.CheckedAt)` to skip.
- A re-index never takes the move shortcut: every fresh path gets a full read.
- `Status.PendingFull` is loaded in `New` (`PendingReread` ≠ zero) and refreshed after each pass. `ScanResult.Full` and `Progress.Full` are set.

- [ ] **Step 1: Write the failing tests**
  - `pause_test.go`:
    - `TestPausesWhileBusy`: `Busy` returns true until the test flips it, with `idleEvery` 10 ms → no hash call while busy (check after 200 ms), and a frame with `paused_by: "busy"`. Then the pass completes.
    - `TestPauseResume`:
      - Five files; the `beforeRead` hook for the second file calls `Pause()` and returns.
      - The second file is still read and recorded, and no third read happens within 200 ms. `Status().Running.PausedBy` is `"user"`, and a forced frame was published.
      - `Resume()` → the pass finishes promptly (well under `idleEvery`, with `idleEvery` set to 1 h for this test) and `Added 5`.
    - `TestPauseNotRunning`: `Pause()` and `Resume()` with no pass → `ErrNotRunning`; after `Close` → `ErrClosed`.
    - `TestUserPauseWinsOverBusy`: both apply → `paused_by` is `"user"`. After `Resume`, while still busy → `"busy"`.
    - `TestCloseWhilePaused`: pause, then `Close` returns within 10 s; `Last.Stopped`. A new pass continues, and the next pass starts unpaused.
  - `reindex_test.go`:
    - `TestFullReindexRereadsEverything`: after a backfill of four files plus one copy, `Rescan(…, full true)` → five full reads, `Full` true, `Unchanged 5`, nothing changed, and every `checked_at` is at or after the pass start. Asset and copy counts are unchanged throughout: the index is never emptied.
    - `TestFullReindexFindsChange`: content changed **without** a size or mtime change (write other bytes of the same length, restore the mtime) → a normal pass reads nothing; a re-index records `Changed 1` and the alias.
    - `TestFullReindexResumes`:
      - Ten files; the re-index is stopped after its fourth read.
      - `PendingFull` is true; then a normal `Rescan` (manual trigger) continues the re-index: `Full` true, exactly six full reads, and the scan row has the same `reread_before`.
      - `PendingFull` is then false, and the following normal pass reads nothing.
    - `TestFullReindexGoneFile`: a file deleted before the re-index → `Missing 1` by the normal rules.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'Pause|Busy|Full|Reindex|ClosedWhile'`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Pause and resume archive scans and re-index the whole archive

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Nightly schedule and the catalog backup

Suggested implementer: sonnet.

**Files:**
- Create: `internal/indexer/schedule.go`, `internal/indexer/backup.go`, `internal/indexer/schedule_test.go`, `internal/indexer/backup_test.go`
- Modify: `internal/indexer/indexer.go`, `internal/indexer/stats.go`

**Interfaces:**
- Consumes: `catalog.Settings.ArchiveRescanAt`/`BackupDir` (Task 3), `catalog.DB.Backup`/`RecordArchiveBackup`/`LastArchiveBackup` (Task 2), `media.SyncDir`, Tasks 9–10.
- Produces:

```go
const BackupKeep = 14

// BackupResult is what one nightly backup did (Status.LastBackup).
type BackupResult struct {
	Path    string    `json:"path"`             // the file written or attempted
	At      time.Time `json:"at"`
	Bytes   int64     `json:"bytes"`
	Removed []string  `json:"removed"`          // old backups deleted (file names), never null
	Error   string    `json:"error,omitempty"`
}
// Status gains:
LastBackup *BackupResult `json:"last_backup"`
BackingUp  bool          `json:"backing_up"`
NextScan   time.Time     `json:"next_scan,omitzero"`

var ErrBackupInside = errors.New("pick a folder outside Archive/ and Staging/")

func (s *Service) Start()                                   // starts the nightly loop once
func (s *Service) SettingsChanged()                         // re-read the schedule now (non-blocking); forced publish
func (s *Service) BackupDir(set catalog.Settings) string    // set.BackupDir, or <photos>/Management/SD2NAS
func CheckBackupDir(photosRoot, dir string) error           // ErrBackupInside when an absolute dir is (in) Archive/ or Staging/; nil for a relative dir (the catalog rejects it)
```

The `Service` gains `after func(time.Duration) <-chan time.Time` (default `time.After`; unreferenced timers are collected since Go 1.23) and `reschedule chan struct{}` (buffer 1). `New` also loads `LastArchiveBackup` into `LastBackup` (`ErrNotFound` → nil).

`schedule.go`:

```go
// nextRun is the first time of day at (since midnight) strictly after now,
// in now's location (time.Local: the container's TZ). time.Date normalises
// a time that falls in a DST gap.
func nextRun(now time.Time, at time.Duration) time.Time {
	h, m := int(at/time.Hour), int(at%time.Hour/time.Minute)
	y, mo, d := now.Date()
	t := time.Date(y, mo, d, h, m, 0, 0, now.Location())
	if !t.After(now) {
		t = time.Date(y, mo, d+1, h, m, 0, 0, now.Location())
	}
	return t
}

func (s *Service) loop() {
	defer s.wg.Done()
	for {
		at := catalog.DefaultSettings().ArchiveRescanAt
		if set, err := s.db.Settings(s.bgCtx); err != nil {
			s.log.Error("read the rescan time; using the default", "err", err)
		} else {
			at = set.ArchiveRescanAt
		}
		next := nextRun(s.now(), at)
		s.setNextScan(next) // Status.NextScan, Version++, forced publish
		select {
		case <-s.bgCtx.Done():
			return
		case <-s.reschedule:
			continue
		case <-s.after(next.Sub(s.now())):
		}
		s.nightly()
	}
}

// nightly backs up the catalog first, whatever a pass is doing (Decision 16),
// then runs the nightly pass unless one is already running (paused or not).
func (s *Service) nightly() {
	s.backupNow(s.bgCtx)
	if s.bgCtx.Err() != nil {
		return
	}
	if !s.pass.TryLock() {
		s.log.Info("nightly rescan skipped: a scan is already running")
		return
	}
	defer s.pass.Unlock()
	if s.bgCtx.Err() != nil { // pre-flight C18
		return
	}
	s.runPass(s.bgCtx, TriggerNightly, false) // continues a pending re-index (Task 10)
}
```

- `Start` starts `loop` once (`wg.Add` under `s.mu`, not after `Close`).
- `SettingsChanged` does a non-blocking send on `reschedule`, `Version++`, and a forced publish.
- `backupNow(ctx)`:
  1. `BackingUp = true`, forced publish.
  2. `res := s.backup(ctx)`.
  3. `RecordArchiveBackup(dbctx, res.At, res.Error == "", json)` (an error there is logged).
  4. `LastBackup = res`, `BackingUp = false`, `Version++`, forced publish.

`backup.go`:

```go
var backupName = regexp.MustCompile(`^catalog-[0-9]{4}-[0-9]{2}-[0-9]{2}\.db$`)

func (s *Service) backup(ctx context.Context) *BackupResult {
	at := s.now()
	res := &BackupResult{At: at, Removed: []string{}}
	set, err := s.db.Settings(ctx)
	if err != nil {
		res.Error = err.Error()
		s.log.Error("catalog backup failed", "err", err)
		return res
	}
	dir := s.BackupDir(set)
	res.Path = filepath.Join(dir, "catalog-"+at.Format("2006-01-02")+".db")
	if err := s.writeBackup(ctx, dir, res.Path); err != nil {
		res.Error = err.Error()
		s.log.Error("catalog backup failed", "path", res.Path, "err", err)
		return res // nothing is pruned after a failure
	}
	if info, err := os.Stat(res.Path); err != nil {
		s.log.Warn("catalog backup written but its size is unknown", "path", res.Path, "err", err)
	} else {
		res.Bytes = info.Size()
	}
	removed, err := pruneBackups(dir, BackupKeep)
	res.Removed = append(res.Removed, removed...)
	if err != nil {
		res.Error = "backup written, but old backups could not all be removed: " + err.Error()
		s.log.Error("prune catalog backups", "dir", dir, "err", err)
	}
	s.log.Info("catalog backup", "path", res.Path, "bytes", res.Bytes, "removed", len(removed))
	return res
}

func (s *Service) writeBackup(ctx context.Context, dir, path string) error {
	if err := CheckBackupDir(s.photosRoot, dir); err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp := path + ".partial"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := s.db.Backup(ctx, tmp); err != nil {
		return errors.Join(err, removeIfPresent(tmp))
	}
	if err := syncFile(tmp); err != nil {
		return errors.Join(err, removeIfPresent(tmp))
	}
	if err := os.Rename(tmp, path); err != nil { // replaces a same-day backup
		return errors.Join(err, removeIfPresent(tmp))
	}
	return media.SyncDir(dir)
}

// syncFile fsyncs path through a read-write handle: Windows' FlushFileBuffers
// refuses a read-only one (pre-flight C7).
func syncFile(path string) (err error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	return f.Sync()
}

// pruneBackups keeps the newest keep catalog-YYYY-MM-DD.db files in dir
// (the date sorts as text) and deletes the rest; nothing else is touched.
func pruneBackups(dir string, keep int) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.Type().IsRegular() && backupName.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	slices.Sort(names)
	var removed []string
	var errs []error
	for _, n := range names[:max(0, len(names)-keep)] {
		if err := os.Remove(filepath.Join(dir, n)); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, n)
	}
	return removed, errors.Join(errs...)
}
```

`CheckBackupDir` returns nil for a relative `dir`. Otherwise it cleans `dir` and, for `Archive` and `Staging` under the photos root, refuses it when `filepath.Rel(root, dir)` is `.` or `filepath.IsLocal`. `removeIfPresent` ignores not-exist. `Stats.BackupDir` now uses `BackupDir(set)`.

- [ ] **Step 1: Write the failing tests**
  - `schedule_test.go` (imports `_ "time/tzdata"`) `TestNextRun`, in `America/Los_Angeles`:
    - 02:00 → the same day at 03:00; exactly 03:00 → the next day at 03:00; 23:30 → the next day at 03:00.
    - `at` 00:00, from 23:59 → the next midnight.
    - The spring-forward day 2026-03-08, `at` 02:30, from 01:00 → a time on 2026-03-08, after `now`.
    - The fall-back day 2026-11-01, `at` 01:30, from 00:00 → 2026-11-01, after `now`.
  - `TestNightlyBacksUpThenScans`:
    - `s.after` records the duration and returns a test channel; `Start`, then fire it.
    - Wait for `LastBackup` (no error, the file `catalog-<today>.db` is in the backup folder) and then `Last.Trigger == "nightly"`. The backup's `at` is before the pass's `started_at`.
    - The loop asks `after` again, and `Close` returns.
  - `TestNightlyBackupWhilePaused`:
    - A manual pass is paused (`Pause()`); fire the nightly timer.
    - `LastBackup` appears while `Running.PausedBy` is still `"user"`, and no nightly pass starts (the scan rows still show only the manual one).
    - `Resume()` → the manual pass finishes.
  - `TestSettingsChangedReschedules`: `SetSetting(archive_rescan_at, "05:15")`, then `SettingsChanged()` → the next `after` duration targets 05:15, and `Status().NextScan` shows it.
  - `backup_test.go` `TestBackupWritesAndPrunes`:
    - The backup folder holds 16 files `catalog-<s.now() minus 1…16 days>.db` (pre-flight B17), plus `notes.txt`, `catalog-old.db` and `catalog-<date>.db.bak`.
    - After a backup: today's file opens with `catalog.Open` (version 3, with the test's asset), and exactly 14 `catalog-YYYY-MM-DD.db` files remain (today plus the 13 newest old ones). `Removed` names the 3 oldest; the other files are untouched; no `.partial` is left.
    - A second backup the same day replaces the file, with no error.
  - `TestBackupFailureKeepsOldBackups`:
    - (a) `backup_dir` names an existing regular file → `Error` set, nothing written, and `LastBackup.Error` set.
    - (b) A folder holding 16 old backups plus a non-empty **directory** named `catalog-<today>.db.partial`, so removing the stale partial fails on Windows and Linux alike → `Error` set, and all 16 old files still there.
  - `TestCheckBackupDir`: `<photos>/Archive`, `<photos>/Archive/x` and `<photos>/Staging/y` → `ErrBackupInside`; `<photos>/Management/SD2NAS`, `<photos>/ArchiveBackups` and `relative` → nil.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'NextRun|Nightly|Reschedule|Backup'`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the Linux command (TZ `Pacific/Chatham`) and the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Back up the catalog and rescan the archive every night

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: API for the archive index and settings; `indexer.progress`; wiring in main

Suggested implementer: sonnet.

**Files:**
- Create: `internal/api/archive.go`, `internal/api/settings.go`, `internal/api/archive_test.go`, `internal/api/settings_test.go`
- Modify: `internal/api/events.go`, `internal/api/full_test.go`, `cmd/sd2nas/main.go`

**Interfaces:**
- Consumes:
  - `indexer.Service` (Tasks 9–11), `indexer.CheckBackupDir`, `indexer.ErrNotRunning`, `indexer.ErrClosed`;
  - `catalog.SettingKeys`/`SetSettings`/`SettingError`/`Settings.Strings`;
  - the existing `decodeBody`, `writeJSON`, `writeError`, `opContext`.
- Produces:

```go
const EventIndexerProgress = "indexer.progress"

// MountArchive registers /api/archive/* and the indexer.progress event, and
// makes Close close idx. Call it before serving.
func (s *Server) MountArchive(idx *indexer.Service)
// MountSettings registers GET/PUT /api/settings.
func (s *Server) MountSettings(db *catalog.DB, idx *indexer.Service, photosRoot string)
```

Handlers:
- `GET /api/archive/stats` → `idx.Stats(r.Context())` (a failure → 500, and the error is logged).
- `POST /api/archive/rescan`:
  - An empty body (`r.ContentLength == 0` or `EOF` on the first read) → normal. Otherwise `decodeBody` into `struct{ Full bool \`json:"full"\` }` (64 KiB).
  - → 202 `{"started": idx.StartRescan(full)}`.
- `POST /api/archive/pause` / `resume` → 200 with the returned Status.
  - `ErrNotRunning` → 409 `{"error": "No scan is running.", "code": "not_running"}`.
  - `ErrClosed` → 503 `SD2NAS is shutting down`.
- `GET /api/archive/changed` → `idx.Changes`.
- `POST /api/archive/changed/accept`:
  - `decodeBody` into `struct{ UpTo *int64 \`json:"upTo"\` }` (64 KiB). Nil or < 1 → 400 `upTo must be a positive whole number.`
  - `idx.Accept(opContext(r), *UpTo)` → 200 `{"accepted": n}`; `ErrClosed` → 503.
- Events:
  - `s.hub.AddSnapshot(func() (string, any, error) { return EventIndexerProgress, idx.Status(), nil })`;
  - `idx.OnChange(func(st indexer.Status) { publish, log on error })`. The callback only publishes (the lock-order rule);
  - `s.closers = append(s.closers, idx.Close)`.
- `GET /api/settings`: `db.Settings` → `Strings()`, with `backup_dir` replaced by `idx.BackupDir(set)` → `{"settings": …}`.
- `PUT /api/settings`:
  1. `decodeBody` into `map[string]any` (64 KiB). Empty → `Name at least one setting.`
  2. For each key in sorted order:
     - not in `SettingKeys()` → `Unknown setting <key>.`;
     - not a string → `Setting <key> must be a string.`;
     - `backup_dir` that is non-empty and fails `indexer.CheckBackupDir(photosRoot, v)` → `Setting backup_dir: pick a folder outside Archive/ and Staging/.`
  3. `db.SetSettings(opContext(r), kv)`. A `*catalog.SettingError` → `Setting <key>: <reason>.`; other errors → 500.
  4. On success, `idx.SettingsChanged()` and the GET body. A changed `archive_skip` applies from the next pass.

`full_test.go`:
- `fullServer` gains `idx *indexer.Service` and a `busy atomic.Bool` passed as `Config.Busy: f.busy.Load` (pre-flight B18).
- It mounts `MountArchive` and `MountSettings`.
- It uses the real `*staging.Locks` as `Config.Locks`.

`cmd/sd2nas/main.go`, after `srv.MountForget(…)`:

```go
		idx, idxErr := indexer.New(indexer.Config{DB: db, PhotosRoot: cfg.Photos, Locks: locks, Log: log,
			Busy: func() bool { // Decision 13: imports and promotes go first
				running, queued := mgr.Current()
				return running != nil || len(queued) > 0 || svc.Promotes().Running != nil
			}})
		if idxErr != nil {
			log.Error("archive indexer unavailable", "err", idxErr)
			srv.AddProblem("archive indexer: " + idxErr.Error())
		} else {
			srv.MountArchive(idx) // srv.Close closes idx (after the staging service)
			srv.MountSettings(db, idx, cfg.Photos)
			idx.Start() // nightly backup and rescan; nothing runs at startup (Decision 14)
		}
```

- [ ] **Step 1: Write the failing tests**
  - `archive_test.go`:
    - `TestArchiveDirsMatch`: `indexer.ArchiveDir == staging.ArchiveDir`, `indexer.StagingDir == staging.Dir`, and `catalog.ArchiveRoot == indexer.ArchiveDir+"/"`.
    - `TestArchiveStatsAPI`:
      - Write two files under `<photos>/Archive/S/`, then `POST /api/archive/rescan` with no body → 202 `{"started":true}`.
      - Poll the stats until `running` is null → `files` 2 and `last.added` 2.
      - `rescan_at` is `03:00`, `backup_dir` ends in `Management/SD2NAS` (OS separators), and `skip_rules` is the three defaults.
      - `missing_files` and `recent` are `[]`, not null.
    - `TestArchiveRescanBusyAndPause`:
      - Set `f.busy` true, then `POST rescan` → 202 started; a second `POST` → `{"started":false}`. The stats show `running.paused_by` `"busy"`.
      - `POST /api/archive/pause` → 200 with `running.paused_by` `"user"`; `POST /api/archive/resume` → 200.
      - Set `f.busy` false → the pass finishes.
      - Then `POST /api/archive/pause` → 409 `not_running` with the exact sentence.
    - `TestArchiveRescanFull`: after a pass, `POST rescan {"full":true}` → `last.full` true, and `last.unchanged` equals the file count. `{"full":"yes"}` and `{"x":1}` → 400 with `decodeBody`'s sentences.
    - `TestArchiveChangedAndAccept`:
      - A change made through a rescan → `GET …/changed` has 1 entry and `latest_id`.
      - Accept `{"upTo": latest}` → `{"accepted":1}`.
      - `{}` and `{"upTo":0}` → `upTo must be a positive whole number.`; `{"upTo":"x"}` and `{"upTo":1,"x":1}` → `decodeBody`'s sentences.
    - `TestArchiveAfterClose`: `s.Close()` → accept, pause and resume answer 503 `SD2NAS is shutting down`, and rescan answers 202 `{"started":false}`.
    - `TestIndexerProgressEvent`: the SSE stream's first frames include `event: indexer.progress` with `"version"`. A rescan publishes a frame with `"running":{` and a later one with `"running":null`, and pause and resume publish frames.
  - `settings_test.go` `TestSettingsAPI`:
    - GET → all seven keys: `archive_rescan_at` `03:00`, `archive_skip` `Output/, CaptureOne/, *.cosessiondb`, and `backup_dir` effective.
    - PUT `{"archive_rescan_at":"04:45"}` → 200 with it, and the stats `rescan_at` is `04:45`.
    - PUT `{"archive_skip":"Output/, Exports/"}` → 200, and the stats `skip_rules` follows.
    - PUT without `X-SD2NAS` → 403.
    - These PUTs → 400 with the exact sentences: `{}`, `{"nope":"1"}`, `{"archive_rescan_at":5}`, `{"archive_rescan_at":"25:00"}`, `{"archive_skip":"a/b"}`, `{"backup_dir":"<photos>/Archive/b"}`, `{"backup_dir":"relative"}` (the catalog's `want an absolute path…` reason).
    - A PUT with one good and one bad key stores neither.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/api/ -run 'Archive|Indexer|Settings'`.
- [ ] **Step 3: Implement** and wire `main.go`.
- [ ] **Step 4: Run**:
  - `go test ./internal/api/ ./cmd/...` → PASS.
  - `gofmt -l .`, `go vet ./...`, `go test ./...`, and the Linux and race commands → PASS.
  - Run the binary on temp dirs: `go run ./cmd/sd2nas --cards ./testdata/cards --photos <tmp>/photos --config <tmp>/config`. `curl -s localhost:8080/api/archive/stats` answers, and `Ctrl+C` exits cleanly.
- [ ] **Step 5: Commit**

```bash
git add internal/api/ cmd/sd2nas/main.go
git commit -m "Serve the archive index, settings and indexer progress over the API

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 13: Web logic for the archive page (node tests)

Suggested implementer: haiku (all strings below are exact).

**Files:**
- Create: `web/src/lib/archive.js`, `web/src/lib/archive.test.js`
- Modify: `web/src/lib/messages.js`, `web/src/lib/messages.test.js`, `web/src/lib/live.svelte.js`

**Interfaces:**
- Consumes: `formatBytes`, `formatCount`, `formatWhen` (`format.js`), `plural`, `joinList` (`messages.js`).
- Produces:

```js
// archive.js
export function scanPercent(p)       // null while listing or with no progress; 0–99 while hashing; 100 for checking
export function scanLine(p)          // the sentence under the progress bar
export function progressDetail(p)    // files and bytes done of total
export function scanSummary(r)       // the toast after a manual rescan or re-index
export function sizeChange(oldSize, size)
export function lastScanLabel(last)  // 'Never' or formatWhen(last.finished_at)
export function backupLine(b)
// messages.js
export function missingCopies(n)
// live.svelte.js: live.indexer = { version: 0, running: null, last: null, last_backup: null, backing_up: false,
//   pending_full: false, changed: 0, missing: 0, next_scan: null }, updated on 'indexer.progress'
```

Exact outputs (`verb` is `Re-indexing` when `p.full`, otherwise `Rescanning`):
- `scanPercent`:
  - `null` for `null`, or for phase `''` or `listing`;
  - phase `hashing`: `Math.min(99, Math.floor(100 * bytes_hashed / bytes_to_hash))` when `bytes_to_hash > 0`, else by `hashed / to_hash`, else 0;
  - phase `checking`: 100.
- `scanLine`:
  - null → `''`;
  - `paused_by === 'user'` → `Paused. Press Resume to carry on.`;
  - `paused_by === 'busy'` → `Paused while an import or promote runs. It carries on by itself.`;
  - phase `''` → `${verb}… starting`;
  - `listing` → `${verb}… looking for files (${formatCount(listed)} found so far)`;
  - `hashing` → `Rescanning… ${pct}% (only files whose size or modified time changed get re-hashed)` (the mockup's sentence) for a rescan, and `Re-indexing… ${pct}% (every file is read again)` for a re-index;
  - `checking` → `${verb}… checking for files that are gone`.
- `progressDetail`:
  - `hashing` → `${formatCount(hashed)} of ${formatCount(to_hash)} files · ${formatBytes(bytes_hashed)} of ${formatBytes(bytes_to_hash)}`;
  - `listing` → `${formatCount(listed)} files found`;
  - otherwise `''`.
- `scanSummary` (`noun` = `Re-index` when `r.full`, else `Rescan`):
  - `skipped` → `${noun} skipped: ${skipped}`;
  - `stopped` → `${noun} stopped before it finished. It carries on at the next scan.`;
  - otherwise collect the parts, in this order, when non-zero: `N new` (added), `N moved`, `N changed`, `N back` (reappeared), `N recovered`, `N missing`, `N duplicates` (copies; `1 duplicate`). The result is `${noun} complete: ${joinList(parts)}.`, or `${noun} complete: no changes.`;
  - then append ` ${plural(busy_folders, 'folder was', 'folders were')} being promoted and will be scanned next time.` when there are busy folders, and ` ${plural(errors.length, 'problem', 'problems')}: ${errors[0]}` when there are errors.
- `sizeChange(1000, 1000)` → `same size`; `(1000, 13300)` → `+12.3 KB`; `(13300, 1000)` → `-12.3 KB`.
- `backupLine`:
  - null → `No catalog backup yet.`;
  - with an error → `The last catalog backup failed: ${error}`;
  - otherwise `Last catalog backup ${formatWhen(at)}, ${formatBytes(bytes)}.`
- `missingCopies(1)` → `1 file's archive copy is missing`; `missingCopies(2)` → `2 files' archive copies are missing`; `missingCopies(1234)` → `1,234 files' archive copies are missing`. The UI groups thousands; the server's message does not (`%d`).

- [ ] **Step 1: Write the failing tests** for every output above, including:
  - `scanPercent` clamping at 99 when `bytes_hashed == bytes_to_hash`;
  - progress counted by `to_hash` only;
  - every `scanLine` case, for both a rescan and a re-index;
  - `scanSummary` for a full pass.
- [ ] **Step 2: Run them and confirm they fail**: `cd web && npm test`.
- [ ] **Step 3: Implement.** In `live.svelte.js`, add the `indexer` field and `source.addEventListener('indexer.progress', (e) => { live.indexer = JSON.parse(e.data); })`.
- [ ] **Step 4: Run**: `cd web && npm test && npm run build` → PASS; `git status --short web/dist` empty.
- [ ] **Step 5: Commit**

```bash
git add web/src/lib/
git commit -m "Add the web logic for the archive index page

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: Archive index page, nav pill, missing copies on the import page

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/pages/Archive.svelte`
- Modify: `web/src/App.svelte`, `web/src/components/Nav.svelte`, `web/src/pages/IngestDetail.svelte`

**Interfaces:**
- Consumes: Task 12's endpoints, Task 13's helpers, `api`, `live`, `toast`, `describeError`, `Dialog.svelte`.
- Produces: route `/archive` (`{ tab: 'archive', view: 'archive' }`).

Page (`Archive.svelte`), following `docs/ui-mockup.html` `renderArchive()`:
- **Data.** `load()` fetches `/api/archive/stats` and, when `stats.changed > 0`, `/api/archive/changed`.
  - One AbortController: a newer load aborts the older one.
  - Keep the last good `stats`/`changes`, and show `Couldn't refresh the archive index: <error>` above them on a refetch failure.
  - Load on mount, after a real reconnect, and when `live.indexer.version !== stats.version`.
  - After a load whose `version` differs from `live.indexer.version` (and the live one is non-zero), load once more.
  - Progress comes from `live.indexer.running`, not from the stats.
- **Header.** `<h1>Archive index</h1>` and `<p class="sub">Hash catalog of <span class="mono">{archive_path}</span>, used to skip files you already have.</p>`. When `archive_present` is false, show `note warn` `Archive/ isn't there. Is the photos share mounted?`.
- **Stats card.**
  - Four `.stat`s: files indexed, archive size (`formatBytes(bytes)`), files in ingest history (`imported`), last scan (`lastScanLabel(last)`).
  - **While a pass runs**, a progress block:
    - a `.bar` with `scanPercent` width (an indeterminate style while it is `null`);
    - `scanLine(running)`;
    - `progressDetail(running)`;
    - the current path (`running.current`, or `running.folder` while listing), mono, wrapping;
    - a **Pause** button (when `paused_by !== 'user'`) or a **Resume** button (when it is `'user'`), each ≥ 44 px. These call `POST /api/archive/pause|resume`; a 409 → a toast with the server's sentence, then `load()`.
  - **Otherwise** two buttons: **Rescan now** (`POST /api/archive/rescan`; `started: false` → toast `A scan is already running.`) and **Re-index everything…**, which opens a `Dialog`:
    - title `Re-index everything?`;
    - body `SD2NAS reads and re-hashes every file in the archive again: ${formatCount(files)} files, ${formatBytes(bytes)}. At array speed that takes hours. The index stays in use the whole time, so cards keep skipping files you already have; imports and promotes still go first, and you can pause it, or stop SD2NAS and it carries on later.`;
    - buttons **Cancel** and **Re-index everything** (primary), which post `{"full": true}`.
  - When `live.indexer.backing_up`, show the muted line `Backing up the catalog…`.
  - A muted line: `Next scan {formatWhen(next_scan)} · catalog backups in <span class="mono">{backup_dir}</span>`, then `backupLine(last_backup)` (red when it has an error).
  - A muted line: `Not indexed inside session folders: {skip_rules.join(', ') || 'nothing extra'}, plus hidden and NAS metadata folders.`
  - When `pending_full` and no pass runs: `note warn` `A re-index was interrupted. It carries on at the next scan (tonight, or press Rescan now).` Otherwise, when `last.stopped`: `note warn` `The last scan didn't finish. It carries on at the next nightly scan, or now with Rescan now.`
  - The problems of the last scan go in a `<details>` (`N problems in the last scan`), with up to 20 lines plus `…and N more` from `errors_omitted`.
- **Toast.** When a pass this page started finishes (`live.indexer.last.finished_at` changes while `scanAsked`), toast `scanSummary(last)`, with `warn` when it was skipped, stopped or had problems.
- **Missing card** (when `missing > 0`):
  - `<h2>⚠ {plural(missing, 'file is', 'files are')} missing from the archive</h2>`;
  - the text `SD2NAS indexed these files, but they're no longer in the archive. Cards that still hold them won't be reported as safe to format. If you moved them, the next scan finds them.`;
  - the list of `missing_files` (mono path, size), with `…and N more` beyond 50.
- **Changed card** (when `changed > 0`):
  - `<h2>⚠ {plural(changed, 'archive file', 'archive files')} changed since they were indexed</h2>` and `Same path, different content. Usually an editor writing metadata into the file.`;
  - a table **File | Last indexed | Size change** on desktop (the date part of `formatWhen(indexed_at)`, and `sizeChange(old_size, size)`), stacked rows on a phone; `…and N more` when `total > changes.length`;
  - the button **Accept new hashes** (primary, ≥ 44 px) → `POST /api/archive/changed/accept {"upTo": latest_id}` → toast `Marked as reviewed. The old hashes stay as aliases, so the originals on a card still count as duplicates.` → `load()`;
  - beside it, the muted line `The old hash is kept as an alias, so the original file from a card still counts as a duplicate.`;
  - errors → `describeError` in a `note bad`.
- **Recently promoted card** (when `recent` is non-empty): a table **Folder | Files | Promoted**. The name is `folder` without the `Archive/` prefix, with the full path muted mono on desktop; Promoted is the date part of `formatWhen(at)`.
- **Layout.**
  - Cards stack; `.stats` has 4 columns on desktop and 2 on a phone.
  - Tables become stacked rows at `max-width: 760px`. There is no horizontal scroll at 375 px: paths get `overflow-wrap: anywhere`.
  - Buttons are full width on a phone (Pause/Resume and the two scan buttons stack), and all tap targets are ≥ 44 px.

Nav: add an **Archive index** tab (`href="/archive"`, an archive-box icon in the same SVG style) after Staging, with `{#if live.indexer.changed}<span class="pill warn">{live.indexer.changed}</span>{/if}` (the mockup's badge). On a phone the pill sits like the ingest one.

`IngestDetail.svelte`:
- In the stats: `{#if c.missing}<div class="stat bad"><b>{formatCount(c.missing)}</b>archive copies missing</div>{/if}`.
- Before the failed list, an open `<details>`:
  - summary `{missingCopies(missing.length)}`;
  - the text `SD2NAS knows these files, but their copies in the archive are gone, so the card isn't safe to format. Keep it until you've found or restored them; the Archive index page lists the missing files.`;
  - rows `card_path → rel_path` (the `rel_path` is where the copy used to be).

- [ ] **Step 1: Implement** the page, the route, the nav tab and the import-page additions.
- [ ] **Step 2: Build**: `cd web && npm test && npm run build` → PASS, with zero Svelte warnings; `git status --short web/dist` empty; `SD2NAS_REQUIRE_UI=1 go test ./web/` → PASS.
- [ ] **Step 3: Manual check** (record what was checked in the report).
  - **Setup:** run the binary against a temp `photos` with:
    - `Archive/2025-05-30 - Trip/{a.JPG, Capture/b.ARW, Selects/c.JPG, Output/d.JPG, 2025-05-30 - Trip.cosessiondb}`;
    - `Archive/2007-09-31 - Random Party/e.JPG`;
    - four distinct files for a, b, c and e (fixture bytes from `testdata/cards`, or any distinct content with those extensions), plus about 200 more small files for a visible pass.
  - **Check** in the browser pane, at desktop width and at 375×812:
    - Rescan now shows the phase, `progressDetail`, the current path and the bar, then the toast (`Rescan complete: N new.`), and the stats fill in.
    - During a pass, press Pause → `Paused. Press Resume to carry on.` and the Resume button appear, and the counts stop moving; Resume carries on.
    - Re-index everything opens the dialog; confirming shows `Re-indexing…`.
    - Edit `a.JPG`'s bytes and rescan → the nav pill shows 1, and the changed card lists it; Accept clears it.
    - Delete `c.JPG` and rescan → the missing card shows it.
    - The skip list line shows the defaults; after `PUT /api/settings {"archive_skip":"Output/, Selects/"}` (curl), it updates on reload.
    - No horizontal scroll, the tab bar is not covered, and buttons are ≥ 44 px.
  - Reset the viewport afterwards.
- [ ] **Step 4: Commit**

```bash
git add web/src/
git commit -m "Add the Archive index page with progress, pause, rescan, re-index and changed files

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 15: PLAN.md: tick M5, record the decisions; final verification

Suggested implementer: haiku.

**Files:**
- Modify: `docs/PLAN.md`

**Interfaces:**
- Consumes: this plan's Decisions.
- Produces: documentation only.

- [ ] **Step 1: Edit `docs/PLAN.md`**
1. In §16 **M5**, tick every item except `Backfill run against the real archive` (the owner's, on the NAS), and add under the list:

```markdown
Implementation plan: `docs/plans/m5-archive-indexer.md`. One walker serves
the backfill, the nightly rescan and "Re-index everything": the first pass
is a rescan over an empty index, every file is committed as it is read, so
an interrupted pass carries on where it stopped. A pass lists `Archive/`
(stat only), then reads only new files and files whose size or modified
time changed, one at a time, waiting while an import or promote runs or
while the owner has paused it from the Archive index page (the file being
read finishes first). Moves are found by quick fingerprint and size (no
full read); the same content at a second archive path is recorded in
`archive_copies`; a file changed in place gets its new hash at once, the
old one becomes an alias and the path is listed under "changed since
indexed" until accepted (accepting only clears the list). Only a pass that
ran to the end marks files `missing`, each after re-checking that it and
`Archive/` are still there; folders a promote holds are skipped and picked
up next time; staged content found in the archive is left alone and culled
content becomes archived. "Re-index everything" re-reads every indexed file
without emptying the index (cards keep matching while it runs); a stopped
re-index is continued by the next pass. Imports give files whose archive
copy is missing the outcome `missing`, which blocks safe to format ("N
files' archive copies are missing"). Only import formats are indexed;
hidden and NAS metadata names are always skipped, and `archive_skip`
(default `Output/, CaptureOne/, *.cosessiondb`) lists what else to skip;
rows already indexed under a newly skipped path stay known and are never
marked missing. Nothing runs at startup: at `archive_rescan_at` (default
03:00 container time) the catalog is backed up with `VACUUM INTO` to
`backup_dir` (default `/photos/Management/SD2NAS`, keeping 14), then the
nightly pass runs — the first one is the backfill — unless a pass is
already running; or press "Rescan now". Schema version 3. Settings are read
and changed with `GET/PUT /api/settings` until M7's page, e.g. from the
Unraid terminal
`curl -s -X PUT -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' http://localhost:4040/api/settings -d '{"archive_skip":"Output/, CaptureOne/, *.cosessiondb, Exports/"}'`.
**To run the backfill:** after updating, either wait for 03:00 or open
Archive index → Rescan now; the page shows progress, and the container log
has an `archive scan` line when it finishes. Tick the item with the file
count and how long it took.
```

2. In §6, after the **Aliases** paragraph, add: ``Schema version 3 adds the `missing` ingest outcome (`ingests.missing`), `archive_copies` (other archive paths holding an asset's content), `archive_changes` ("changed since indexed"), `archive_scans` (the indexer's passes and backups), and `assets.checked_at` / `promoted_at`. A change in place moves the old fingerprints (their values are kept), aliases and ingest history to the new hash.``
3. In §9:
   - Replace the **What gets indexed inside a session folder** bullet with: ``**What gets indexed:** every file under `Archive/` with an import format, except hidden and NAS metadata names (always skipped) and what the `archive_skip` setting lists — by default `Output/` and `CaptureOne/` folders inside a session and `*.cosessiondb`. A rule added later leaves what is already indexed there known; it just isn't looked at any more. Files in `Trash/` count as seen, so rejected shots don't come back from the card.``
   - After "Moves are recognized" add: ``A copy of archived content at a second path is remembered as a copy and not re-read; when the primary path goes, the asset moves to the copy.``
   - After the **Nightly rescan** list add: ``"Changed since indexed" entries are already applied when listed; accepting them clears the list. A running pass can be paused and resumed from the Archive index page. "Re-index everything" re-reads every file without emptying the index; an interrupted one continues at the next pass.``
   - In the **Nightly rescan** list, change ``then the catalog backup (§3)`` to ``the catalog backup (§3) runs first, at the same time``.
4. In §10, change the event sentence to end with ``…; `indexer.progress` (M5) carries the indexer status.`` Add to the endpoint block, next to their neighbours, `POST   /api/archive/rescan               {full}  a rescan, or a re-index when full`, `POST   /api/archive/pause` and `POST   /api/archive/resume`, and note: ```GET/PUT /api/settings` (M5) read and change the catalog settings as strings; no settings page until M7.``
5. In §3 **Catalog backup**:
   - Replace ``Nightly, after the rescan,`` with ``Nightly, just before the rescan (so a paused scan can't hold it up),``.
   - After "The folder is a setting.", add: ``It is `backup_dir` (empty = `/photos/Management/SD2NAS`; never inside `Archive/` or `Staging/`); a failed backup is shown on the Archive index page and deletes nothing.``
6. Add these follow-ups as unchecked items:
   - Under **M6**: ``- [ ] Notify when the nightly catalog backup fails or the nightly rescan finds missing files``
   - Under **M7**:
     - extend the settings item to ``- [ ] Settings page: `staging_warn_days`, the default promote pattern (now remembered per device), `day_starts_at`, `archive_rescan_at`, `backup_dir`, `archive_skip` ``;
     - add ``- [ ] Offer to re-import files whose archive copy is missing (imports now list them and refuse safe to format)``.
7. F6–F14 from the M4 review remain open (not in code M5 changed): say so only in the final report, not in PLAN.md.

- [ ] **Step 2: Check CI and Docker need nothing.** `ci.yml` already runs gofmt, vet, `go test` with the tools, `npm ci/test/build`, and `go test ./web/` with `SD2NAS_REQUIRE_UI=1`. The Dockerfile copies `internal/` and `web/` as a whole. If anything needs changing, stop and report instead of editing `.github/` or `deploy/`.

- [ ] **Step 3: Final verification of the whole branch.** Run each of these and paste the tail of its output in the report:
- `gofmt -l .` prints nothing; `go vet ./...` and `go test ./...` pass on Windows; the Linux and race commands pass.
- `go list -f '{{join .Imports "\n"}}{{"\n"}}{{join .TestImports "\n"}}' ./internal/indexer/ | grep -E 'internal/(staging|ingest|api)'` prints nothing.
- In `web/`: `npm ci && npm test && npm run build`, then `SD2NAS_REQUIRE_UI=1 go test ./web/ -v` passes.
- The image smoke run:
  1. `docker build -f deploy/Dockerfile -t sd2nas:m5 .`
  2. Run it with:
     - `testdata/cards` read-only at `/cards`;
     - a temp `/photos` holding `Archive/2025-05-30 - Trip/` (fixture JPG/ARW at the top level and in `Capture/` and `Selects/`, an `Output/` copy and a `.cosessiondb`);
     - a temp `/config`, `-e TZ=America/Los_Angeles` and `-p 18080:8080`.
  3. `POST /api/archive/rescan` with `X-SD2NAS: 1` → 202. `GET /api/archive/stats` soon shows `running: null`, and `last.added` = the media count outside `Output/`.
  4. `POST /api/archive/rescan {"full":true}` → `last.full` true, nothing changed.
  5. Put a byte-copy of one `SONY_A7IV` card file into the archive folder and rescan; then delete it and rescan again (`missing` 1).
  6. `POST /api/ingest?wait=1&format=text` with `{"path":"/cards/SONY_A7IV"}` → the message contains `1 file's archive copy is missing. Do not format the card.`
  7. `PUT /api/settings {"archive_rescan_at":"04:00"}` → 200, and the stats `next_scan` is at 04:00.
  8. `docker stop` exits within 2 s.
  9. Remove the container and the image afterwards.
- `git status --short` shows only `HANDOFF.md` untracked.
- The trailer counts: `git log --format=%B main..HEAD | grep -c 'Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>'` equals `git rev-list --count main..HEAD`, and `git log --format=%B main..HEAD | grep -ci 'co-authored-by'` equals the same number.

- [ ] **Step 4: Commit**

```bash
git add docs/PLAN.md
git commit -m "Tick M5 in the plan and record its decisions and follow-ups

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
