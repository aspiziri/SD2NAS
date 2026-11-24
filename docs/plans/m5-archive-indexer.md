# M5 Archive Indexer Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give SD2NAS a hash index of the existing `Archive/` so cards stop importing files the owner already has: a resumable backfill of ~150k files, a nightly rescan at 03:00 that follows moves, records in-place edits as aliases ("changed since indexed"), marks vanished files `missing` and indexes files copied in by hand, a nightly `VACUUM INTO` backup of the catalog, and an "Archive index" page that works on a desktop and on a phone. Imports refuse "safe to format" for files whose archive copy is missing.

**Architecture:** A new package `internal/indexer` owns one walker used for the backfill and every rescan (the first pass is simply a rescan over an empty index). A pass lists `Archive/` (stat only, Capture One skip rules), then reads only the files that are new or whose size or modified time changed, one at a time, and resolves each by content hash against the catalog (new, moved, copy, changed in place, reappeared, recovered from culled, still staged). Only a pass that ran to the end marks the archived files it didn't see as `missing`, and only after re-checking that each file and `Archive/` itself are still there (a dropped share reads as "not found"). Folders a promote is writing into are skipped through the shared `*staging.Locks`, which the indexer sees through a one-method interface. A `time.Timer` loop runs the nightly pass and then the catalog backup. `internal/catalog` gets schema version 3 (the `missing` ingest outcome, `archive_copies`, `archive_changes`, `archive_scans`) and the archive queries; `internal/ingest` blocks "safe to format" on `missing`; `internal/api` serves `/api/archive/*`, `/api/settings` and the `indexer.progress` event; the web app gets the Archive index page and its nav pill.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `net/http`, `log/slog`, `modernc.org/sqlite`; Svelte 5.59.1 (runes), Vite 8.3.8, Node 24. No new dependencies.

**Spec:** `docs/PLAN.md` §9 (indexer semantics: backfill, skip rules, moves, folder names as text, nightly rescan, "Rescan now"), §16 M5 checklist, §3 (catalog backup: `VACUUM INTO /photos/Management/SD2NAS/catalog-YYYY-MM-DD.db`, keep 14, folder is a setting; "safe to format"), §6 (assets, fingerprints, aliases, duplicate detection; `internal/catalog/schema.go` is authoritative), §10 (reserved `/api/archive/*`, `/api/settings`, `indexer.progress`), §13 (time zones). UI design: `docs/ui-mockup.html` `renderArchive()` (stats card, rescan bar, changed-since-indexed card with "Accept new hashes", recently promoted) and the nav badge (`pill warn` with the changed count). Context: `HANDOFF.md` ("M5 must build on / must not break", follow-ups F6–F14). Template: `docs/plans/m4-staging-ops.md`.

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`, `go 1.24.0` in `go.mod` with no `toolchain` line. **Use no API newer than Go 1.24**: no `sync.WaitGroup.Go`, `testing/synctest`, `T.Output`, `os.Root` methods added in 1.25. Fine: `context.WithoutCancel`, `min`/`max`, `slices`, `maps`, `t.Context()`, `slog.DiscardHandler`, `omitzero`, `range` over an int, `filepath.IsLocal`, `sync.Mutex.TryLock`, `time/tzdata` (tests only).
- **No new dependencies**, Go or npm. `go.mod`, `go.sum`, `web/package.json` and `web/package-lock.json` must not change. No cron library, no router, no test framework. Frontend tests use the built-in `node --test` on plain JS in `web/src/lib/*.js`. npm commands run **only inside `web/`**.
- M5 touches: `internal/catalog/` (schema v3, archive queries, settings), `internal/ingest/` (`run.go`, `describe.go`: the `missing` outcome), `internal/staging/locks.go` (`NameHeld` only), the new `internal/indexer/`, `internal/api/` (new `archive.go`, `settings.go`, `events.go` constant), `cmd/sd2nas/main.go`, `web/src/**`, and `docs/PLAN.md` (last task). Do not modify `internal/media/`, `internal/thumbs/`, `internal/config/`, `.github/`, `deploy/`, or any other file of `internal/staging/`. **`internal/indexer` imports only the standard library, `internal/catalog` and `internal/media`** (never `internal/staging`, `internal/ingest` or `internal/api`); check with `go list -f '{{join .Imports "\n"}}' ./internal/indexer/` and its test files' imports.
- **Binding values** (reviewers check them verbatim):
  - Endpoints (request fields camelCase as in §10, response fields snake_case like M3/M4):
    - `GET /api/archive/stats` → 200 `Stats` (Task 11 lists the fields; it embeds the `indexer.progress` Status).
    - `POST /api/archive/rescan` → 202 `{"started": bool}` (false: a pass is already running, or shutting down).
    - `GET /api/archive/changed` → 200 `{"version": uint, "changes": [ArchiveChange…], "total": int, "latest_id": int}`; at most **500** changes, ordered by `rel_path`; `latest_id` is the highest id of all unaccepted changes (0 when none).
    - `POST /api/archive/changed/accept` `{"upTo": int}` (required, ≥ 1) → 200 `{"accepted": int}`: deletes every change with `id <= upTo`. Bad or missing `upTo` → 400 `upTo must be a positive whole number.`
    - `GET /api/settings` → 200 `{"settings": {key: string…}}` with every key below and its effective value (`backup_dir` shows the effective folder). `PUT /api/settings` with a JSON object of `key: string` (≥ 1 key, all applied in one transaction or none) → 200, the same body as GET. 400 sentences: `Name at least one setting.`, `Unknown setting <key>.`, `Setting <key> must be a string.`, `Setting <key>: <reason>.`, `Setting backup_dir: pick a folder outside Archive/ and Staging/.`
    - Bodies at most 64 KiB; unknown JSON fields → 400 (the existing `decodeBody`); every non-GET needs `X-SD2NAS: 1` (the existing guard); errors are JSON `{"error": msg}` with `Cache-Control: no-store`; 503 `SD2NAS is shutting down` after `Close`.
  - Event `indexer.progress` (also a snapshot frame on connect): data = indexer `Status` `{"version": uint, "running": Progress|null, "last": ScanResult|null, "changed": int, "missing": int, "next_scan": RFC 3339}`. Published at most every **250 ms** (≤ 4/s) plus at every forced change: pass start, phase change, pause/resume, pass end, accept, settings change.
  - Settings (catalog `settings` table): `archive_rescan_at` (default **`03:00`**, `HH:MM` 00:00–23:59, container local time `time.Local` from `TZ`); `backup_dir` (default `""` = `<photos>/Management/SD2NAS`, i.e. `/photos/Management/SD2NAS` in the container; otherwise an absolute path; never inside `Archive/` or `Staging/`). Existing keys keep their rules: `day_starts_at`, `verify_known`, `free_space_reserve_gb`, `staging_warn_days`.
  - Backup: file name `catalog-YYYY-MM-DD.db` (the local date of the backup), written with `VACUUM INTO` to `catalog-YYYY-MM-DD.db.partial`, fsynced, renamed over any same-day file; then the newest **14** files matching `^catalog-[0-9]{4}-[0-9]{2}-[0-9]{2}\.db$` are kept and older ones deleted. Nothing else in the folder is ever touched. Pruning runs only after a successful backup.
  - Skip rules (§9): every directory and file whose name starts with `.`, or equals (case-insensitively) `@eaDir`, `@Recycle`, `@Recently-Snapshot`, `#recycle`, `#snapshot`, `$RECYCLE.BIN`, `System Volume Information`; directories named `Output` or `CaptureOne` (case-insensitively) anywhere **below** a session folder (depth ≥ 2 under `Archive/`); files ending in `.cosessiondb`; anything that isn't a regular file (symlinks are not followed); files whose extension isn't an import format of `media.DefaultRules()`. Everything else under `Archive/` is indexed, including top-level media, `Capture/`, `Selects/`, `Trash/`, `Videos/`, `Other/` and any other subfolder.
  - Catalog: `SchemaVersion = 3`; new ingest outcome `missing`; `ingests.missing` column; `Counts.Missing` (`json:"missing"`, counted in `seen`).
  - Messages (exact):
    - Ingest: `1 file's archive copy is missing` / `<N> files' archive copies are missing`; a done import with any → `<new> new, <known> already known[, <F> failed], <missing text>[, <U> files not imported (…)]. Do not format the card.`
    - Scan `skipped`: `Archive/ is missing, so nothing was changed (is the photos share mounted?)`; `Archive/ went missing during the scan, so it was stopped (is the photos share still mounted?)`.
    - Scan error for a file that changed while read: `<rel_path> changed while it was read (still being copied?); the next scan picks it up`.
    - A pass found unfinished at startup is recorded with `stopped: true` and `errors: ["SD2NAS stopped before this scan finished"]`.
  - UI: phone breakpoint `max-width: 760px`; tap targets ≥ 44 px; stats grid two columns on a phone; long paths wrap (`overflow-wrap: anywhere`), never scroll the page sideways.
- **Responsive, not phone-only (owner requirement).** The Archive index page must work well at desktop width (multi-column stats, tables) and at 375×812 (two-column stats, stacked rows instead of wide tables, full-width buttons, the tab bar and safe areas respected). Reviewers check both.
- Svelte 5 runes only (`$state`, `$derived`, `$effect`, `$props`), no `{@html}`, no `export let`. Plain CSS (tokens in `web/src/app.css`, component styles scoped). Follow the M3/M4 live-page pattern: one AbortController per load, keep the last good data on screen and show a refetch failure above it, refetch after a real reconnect and whenever `indexer.progress.version` differs from the `version` in the data (and load once more if a load returns a version other than the live one: the F10 lesson).
- **The filesystem is never written by the indexer except the backup folder.** It never moves, renames or deletes anything under `Archive/`. Catalog writes for a file that was read use `context.WithoutCancel`, so a file read before shutdown is always recorded.
- SQLite has **one connection**: never call a `catalog.DB` method while a `*sql.Rows` from it is open, and inside `d.tx(…)` use only `tx`. Catalog methods return slices, not iterators.
- Tests pass on Windows and Linux. Use `path/filepath` for OS paths and `path` for slash-separated `rel_path` values. Close every file, `*sql.Rows`, `*catalog.DB` and `*indexer.Service`. In tests, call `t.TempDir()` before registering a `Close` cleanup. Tests that wait use a 10 s deadline and fail clearly; sleeps only inside polling loops or for the negative checks this plan names (each ≤ 300 ms). Set file times with `os.Chtimes` to whole seconds (some filesystems round).
- **Commands.** Windows (Git Bash, repo root): `gofmt -l .` (prints nothing), `go vet ./...`, `go test ./...`. Linux (Docker Desktop running):
  `MSYS_NO_PATHCONV=1 docker run --rm -e TZ=Pacific/Chatham -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q exiftool ffmpeg && go vet ./... && go test ./...'`
  Race (Tasks 5, 7, 8, 9, 10, 11): `MSYS_NO_PATHCONV=1 docker run --rm -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q build-base exiftool ffmpeg && CGO_ENABLED=1 go test -race ./internal/... ./web/...'`
  Web (in `web/`): `npm test`, `npm run build` (then `git status --short web/dist` must show nothing).
- Never silently swallow an error: return it wrapped with `%w`, answer the request with it, record it in the scan result (which logs it), or log it with `slog` (key `"err"`). Exceptions: a failed write to an HTTP client that has gone, a file that vanished between the listing and the read (it is simply not there any more), and `localStorage` being unavailable in the browser.
- Export only the identifiers listed in each task's **Produces** block. Unexported helpers are free unless a later task's **Consumes** names them.
- Commit on branch `m5-archive-indexer`, one commit per task (more is fine), with the message given in the task. Every message ends with a blank line and then exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` — no other trailer, no other model name. Never commit `HANDOFF.md`, `tmp/`, `web/node_modules/` or anything in `web/dist/` other than `.placeholder`. Write files as UTF-8 with LF line endings (check `§`, `…`, `—`, `’` survive). Never push.

## Review Focus

1. **Nothing is marked `missing` that isn't gone.** Only a pass that ran to the end marks anything; each candidate is re-checked with `os.Lstat` right before it is marked and `Archive/` must still exist (a dropped share ends the pass with the "went missing" message and marks nothing more); rows below folders the pass skipped (busy, unreadable) or below skip-rule folders (`Output/`, `CaptureOne/`, dot and NAS metadata names) are never candidates; a file whose old content still exists as a known copy moves to the copy instead. Pinned in Task 8 (`TestGoneFileMarkedMissing`, `TestArchiveVanishesBeforeMissing`, `TestSkippedRowsNeverMissing`, `TestUnreadableFolderNotMissing`, `TestPrimaryGoneMovesToCopy`) and Task 9 (`TestStoppedPassMarksNothingMissing`).
2. **Promote interplay.** A folder whose `Archive/…` name (or a parent's) a promote holds is skipped whole — not indexed, nothing below it missing — and picked up by the next pass; promoted files already `archived` with matching size and modified time are skipped without being read; content still `staged` found in `Archive/` is left alone (`in_staging`); `culled` content found in `Archive/` becomes `archived` at that path (M4 Decision 9). Pinned in Task 5 (`TestNameHeld`), Task 7 (`TestRescanSkipsPromotedFiles`, `TestStagedContentLeftAlone`, `TestCulledContentRecovered`), Task 8 (`TestPromoteHeldFolderSkipped`, `TestHalfPromotedFolder`).
3. **A change in place keeps the card's original a duplicate.** The new content becomes the asset (same kind, capture time, first ingest), the old hash an alias, the old fingerprints point at the new asset, aliases and ingest history follow, a revert to an earlier hash works, one "changed" entry per path with a fresh id on re-change, and "Accept" only clears entries up to the id the page showed. Pinned in Task 2 (`TestRecordArchiveChange*`, `TestAcceptArchiveChanges`) and Task 8 (`TestRescanChangedContent`, `TestChangeReverted`).
4. **Moves and duplicates.** A file that disappears from one path and appears at another in the same pass is one move (no `missing`, no new asset), found by quick fingerprint plus size without a full read; the same content at two archive paths is one asset plus one `archive_copies` row, never re-read while unchanged. Pinned in Task 8 (`TestMoveWithinSession`, `TestSessionRenamedByHand`, `TestDuplicateRecordedAsCopy`).
5. **Long job hygiene.** One file at a time; pauses between files while an import or a promote is busy; hashing stops mid-file on shutdown; each file is recorded as it is read, so an interrupted backfill resumes without re-reading; progress is throttled. Pinned in Task 7 (`TestBackfillResumes`), Task 8 (`TestPausesWhileBusy`), Task 9 (`TestCloseStopsPass`, `TestProgressThrottled`, `TestStartRescanOnce`).
6. **Safe to format.** A card file whose matched asset is `missing` gets outcome `missing`, is counted, blocks `safe_to_format` and is named in the message; a fingerprint also matching an archived asset still wins as `in_archive`. Pinned in Task 4 (`TestRunMissingBlocksSafeToFormat`, `TestDescribeMissing`).
7. **Schedule and backup.** 03:00 container time, the next one strictly after now, re-armed after a settings change; the backup is a consistent catalog the current build opens; keep 14; a failed backup deletes nothing and is shown; the folder can't be inside `Archive/` or `Staging/`. Pinned in Task 10 (`TestNextRun`, `TestNightlyRunsScanThenBackup`, `TestBackupWritesAndPrunes`, `TestBackupFailureKeepsOldBackups`) and Task 11 (`TestSettingsAPI`).
8. **UI on both form factors.** Progress from `indexer.progress`, stats refetched on version change and reconnect, accept with the shown `latest_id`, nav pill with the changed count, no horizontal scroll at 375 px. Pinned in Task 12 (`archive.test.js`) and the manual checklist in Task 13.

## Decisions

1. **One walker for backfill and rescan.** The first pass is a rescan over an empty index: every file is "new", so it is read and inserted as `archived` with `first_ingest = NULL`. Every later pass skips files whose size and modified time match the catalog. "Resumable" then falls out: each file is committed as soon as it is read, so a pass stopped after 40,000 files leaves 40,000 rows that the next pass skips by size and modified time. Why: two code paths would have to agree on skip rules, move detection and the promote rules; one can't drift.
2. **A pass has four phases** — *listing* (walk `Archive/`, stat only, compare each path with the catalog), *hashing* (read the changed paths, then the new ones), *checking* (archived rows not seen → copy, or `missing` after re-checks), then on the nightly pass *backup*. Reads are deferred until the listing is complete so that "disappeared" is known before a new path is resolved; that is what makes a move one move (§9) whatever order the walk meets the two paths in.
3. **Move detection** (§9 "same hash at a new path in the same scan"). A new path is first quick-fingerprinted (128 KiB read); if a catalog asset with that fingerprint and the same size is `archived` at a path this pass didn't see, or is `missing`, it is that asset, moved (no full read — §6 treats fingerprint plus size as a safe match for camera files). Otherwise the file is fully hashed and resolved by hash: unknown → added; archived and not seen → moved; archived and still present (or below a folder this pass skipped) → a copy; `missing` → reappeared; `culled` → recovered; `staged` → left alone. Why the fingerprint shortcut: Capture One moves files between `Capture/`, `Selects/` and `Trash/`, and a session renamed by hand would otherwise mean re-reading the whole session.
4. **Same content at two archive paths** (duplicates in the archive): one asset row keeps one `rel_path`; every other path is a row in a new `archive_copies` table (`rel_path`, `sha256`, `size`, `mtime`) that later passes skip by size and modified time like assets. When the primary path disappears and a copy was seen, the asset moves to the copy (no `missing`). Copy rows have no foreign key (a copy may outlive its asset when Forget deletes a staged/culled one); a copy whose asset isn't `archived` any more is resolved again by its hash. Why: without the table every duplicate would be re-read every night; ~150k files hand-curated since 2007 will have some.
5. **What counts as a media file:** the import formats of `media.DefaultRules()` (the same lists ingest uses). A card can only bring those, so indexing anything else (PSD, HEIC exports) would cost reads and match nothing.
6. **Skip rules are a deny list, not §9's allow list.** Indexed: everything below a session folder except `Output/`, `CaptureOne/` (at any depth below the session), `*.cosessiondb`, dot and NAS metadata names. §9's list (top level, `Capture/`, `Selects/`, `Trash/`, `Videos/`, `Other/`) describes the Capture One layout; folders from before Capture One (2007…) have other subfolders whose originals must still count as "already have". `Trash/` counts as seen (§9).
7. **Folder names are never parsed.** The indexer stores no capture time for indexed files (`capture_time` NULL): §9 asks only for the hash, nothing reads `capture_time` of archived assets, and reading EXIF/`mvhd` for 150k files would add I/O for nothing. Folder names are only ever handled as text (`2007-09-31 - Random Party` is indexed like any other; Task 7 pins it). "Recently promoted" groups by the folder name as text too.
8. **Changed in place.** The rescan re-hashes a path whose size or modified time changed. Same hash → only the modified time is updated. A hash the catalog doesn't know → the change is applied at once, in one transaction: a new asset row with the new hash (copying kind, capture time, time-zone offset and first ingest from the old row), the old hash becomes an alias, the old row's fingerprints, aliases, ingest-file rows and clock warnings point at the new hash, the old row goes, and an `archive_changes` entry records path, old/new hash and size, when the old content was last indexed and when the change was seen. A hash the catalog already has → the old content left this path (it goes through "checking": copy or `missing`) and the path is resolved like a new one. §9 says "store the old hash as an alias, list it under changed since indexed", so the catalog is right before anyone looks; **"Accept new hashes" only clears the list** (the mockup's toast becomes "Marked as reviewed. The old hashes stay as aliases, so the originals on a card still count as duplicates."). Why not hold the change until accepted: there is no "reject" (SD2NAS never restores files), and a pending state would make every later pass re-read the file and complicate moves and missing. The list's purpose is to surface unexpected edits (a tool rewriting files, bit rot) so the owner can check them.
9. **`archive_changes` is a table**, one row per path (`rel_path UNIQUE`). A second change of the same path before it is accepted keeps the first old hash and indexed time, takes the new hash, and gets a **new id** (delete + insert), so `POST …/accept {"upTo"}` accepts exactly what the page showed. Accepting deletes the rows (the aliases are the permanent record). The mockup's "Likely cause" column becomes "Size change" (`+12.3 KB`, `same size`): the server can't know the cause and shouldn't guess.
10. **Missing files and their return.** An archived row not seen by a complete pass, with no seen copy, whose file `os.Lstat` reports as not existing while `Archive/` exists, becomes `missing` (`rel_path` kept, so the page can show where it was). It stays known: a card with that file gets the new `missing` outcome, which blocks "safe to format" (§16). When the content shows up again — same path or elsewhere — it becomes `archived` there ("reappeared"); the staging rescan already restages `missing` content found in `Staging/`. Re-importing a missing file from the card is not offered in M5 (follow-up in Task 14).
11. **Ingest outcome `missing`** instead of counting those files as `seen_before`: the import detail can then list them (with where they used to be) and the history keeps why the card wasn't safe. Migration 3 rebuilds `ingest_files` (SQLite can't change a `CHECK`), keeping rowids, and adds an index on `ingest_files(sha256)` (the change transaction updates by hash). `ingests.missing` stores the count. The message is "N files' archive copies are missing" (exact singular/plural in Global Constraints), placed after the failed count; the import detail shows a red stat and an open list.
12. **Promote interplay, without importing staging.** The indexer takes `Locks interface{ NameHeld(rel string) bool }`, satisfied by the shared `*staging.Locks` through a new method that reports whether a name key equal to `rel` or to one of its parent directories is held (case-insensitively, like the keys). Promote holds `Archive/<target>` from enqueue until the folder's catalog write is done, so the indexer checks every directory as it walks (a nested `{year}/{month}` target is caught at its own level) and skips it whole; it re-checks before each read and before each `missing`. It never takes a lock itself: holding a session folder for the minutes its files take to hash would make a promote into it fail with 409, while promote never removes anything from `Archive/`, so peeking is enough; a promote that starts after the check only adds files, whose content is `staged` (left alone) or already `archived` with matching size and time (skipped). `indexer.ArchiveDir`/`StagingDir` must equal `staging.ArchiveDir`/`staging.Dir` (checked in an `internal/api` test, the one package that imports both).
13. **Low-priority I/O without new dependencies.** On Unraid the archive is read through `shfs` (FUSE), so the disk reads are issued by the `shfs` daemon, not by SD2NAS: `ioprio_set`/`nice` on our threads wouldn't reach the disks (and must not break the Windows build). Instead: one file at a time, sequential 1 MiB reads, no worker pool; between files the pass **pauses while an import is running or queued or a promote is running** (`Config.Busy`, wired in `main`; checked every 2 s, shown as "Paused"); the nightly run is at 03:00. No bandwidth cap setting (can come later if the nightly run gets in the way).
14. **When passes run.** Nightly at `archive_rescan_at` (default 03:00), and from "Rescan now". **Nothing starts at startup**, not even the first backfill: an update applied at noon shouldn't start hours of array reads, and the backfill is meant to run overnight (§9). The first nightly run is the backfill; "Rescan now" starts it sooner. An interrupted pass continues at the next nightly run or "Rescan now" (the page says so). If a manual pass is running at 03:00, the nightly run waits for it, then runs its own pass (cheap: size/mtime) and the backup. A run missed while the container was down is not caught up.
15. **Schedule.** A `time.Timer` loop (no cron library): `nextRun(now, at)` is the first `HH:MM` strictly after `now` in `time.Local` (the container's `TZ`, which Unraid injects; `/healthz` warns when it is missing); `time.Date` normalises a time inside a DST gap. A settings change re-arms the timer at once.
16. **Catalog backup** after the nightly pass, whether the pass found problems or not (the backup matters more), skipped only on shutdown. `VACUUM INTO ?` (bound parameter) into `<name>.partial`, fsync, rename over a same-day file, fsync the folder, then prune to 14. A failure is logged, stored in the scan result (`backup.error`) and shown on the Archive page; nothing is pruned after a failure and there is no retry until the next night. Notifying about it waits for M6's notification channel (follow-up).
17. **Settings API in M5, no settings page.** §10 reserves `GET/PUT /api/settings`; the rescan time and backup folder must be configurable, and editing SQLite by hand (as `staging_warn_days` is today) is not reasonable for a folder path. The API is small (string values, the catalog validates, all-or-nothing) and is exactly what M7's page will call; until then it is used with `curl` (Task 14 records the command in PLAN.md). The Archive page shows the schedule and backup folder read-only.
18. **Scan history** lives in `archive_scans` (`kind`, `started_at`, `finished_at`, `result` JSON written by the indexer; the newest 30 kept). A row with no `finished_at` at startup is closed as stopped. Why: "last scan" and "interrupted" must survive a container restart; the JSON keeps the catalog free of the indexer's result shape.
19. **Status, stats and events.** `indexer.progress` carries the in-memory `Status` (live progress, last result, changed and missing counts, next run) with a `version` that bumps after each pass, accept and settings change — not per file. `GET /api/archive/stats` adds the catalog numbers (files and bytes indexed, copies, files brought in by imports — the mockup's "files in ingest history" — up to 50 missing files, the 5 most recently promoted folders) and reads `version` before them, like M4's listings. "Recently promoted" is the archived assets with a `first_ingest`, grouped by their session folder (the first path element under `Archive/`), newest `updated_at` first: promote sets `updated_at`, and nothing else is stored about promotes.
20. **M4 follow-ups.** None of F6–F14 is in a file M5 changes in that area (`locks.go` only gains `NameHeld`; `Staging.svelte`, `ops.go`, `promote.go`, `reconcile.go`, `staging.go`, `ingest/run.go`'s folder waits are untouched), so all stay open and are listed again in Task 14. The new page applies the F10 lesson (reload when the loaded version differs from the live one) and the indexer's event is throttled from the start (the F7 lesson).

## File Structure

```
internal/catalog/
  schema.go        schemaV3: ingests.missing, ingest_files rebuilt with 'missing' + sha index,
                   archive_copies, archive_changes, archive_scans                    (Task 1)
  catalog.go       SchemaVersion = 3                                                 (Task 1)
  ingests.go       OutcomeMissing, Counts.Missing, ingests.missing read/write        (Task 1)
  stagingops.go    ErrWrongState wrapped by changeAsset                               (Task 1)
  archive.go       ArchiveEntry/Copy/Change, the archive queries, scans, Backup      (Task 2)
  settings.go      archive_rescan_at, backup_dir, SettingError, SettingKeys,
                   Settings.Strings, SetSettings                                     (Task 3)
internal/ingest/
  run.go           knownOutcome: missing → OutcomeMissing; safe needs Missing == 0   (Task 4)
  describe.go      the missing sentence                                              (Task 4)
internal/staging/locks.go   NameHeld                                                 (Task 5)
internal/indexer/
  indexer.go       Config, Service, New, Status, Progress, ScanResult, OnChange,
                   Rescan, StartRescan, Close, report throttle                       (Tasks 6, 9)
  walk.go          skip rules, the listing phase                                     (Task 6)
  hash.go          hashFile (context-aware), stable reads                            (Task 7)
  scan.go          the pass: hashing, resolve, checking                              (Tasks 7, 8)
  schedule.go      nextRun, the nightly loop, SettingsChanged                        (Task 10)
  backup.go        BackupDir, CheckBackupDir, backup, prune                          (Task 10)
  stats.go         Stats, Changes, Accept                                            (Task 9)
internal/api/
  archive.go       MountArchive: stats, rescan, changed, accept, indexer.progress    (Task 11)
  settings.go      MountSettings: GET/PUT /api/settings                              (Task 11)
  events.go        EventIndexerProgress                                              (Task 11)
cmd/sd2nas/main.go indexer wiring (Busy, shared Locks), MountArchive, MountSettings  (Task 11)
web/src/lib/
  archive.js (+ archive.test.js)   scanLine, scanPercent, scanSummary, sizeChange,
                                   backupLine, lastScanLabel                         (Task 12)
  messages.js (+ test)             missingCopies                                     (Task 12)
  live.svelte.js                   indexer.progress                                  (Task 12)
web/src/pages/Archive.svelte       the Archive index page                            (Task 13)
web/src/components/Nav.svelte      Archive index tab with the changed pill           (Task 13)
web/src/App.svelte                 /archive route                                    (Task 13)
web/src/pages/IngestDetail.svelte  missing stat and list                             (Task 13)
docs/PLAN.md                                                                         (Task 14)
```

---

### Task 1: Catalog schema v3, the `missing` ingest outcome, `ErrWrongState`

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/catalog/schema.go`, `catalog.go`, `ingests.go`, `stagingops.go`, `ingests_test.go`, `stagingops_test.go`

**Interfaces:**
- Consumes: the existing migration runner (`migrations`, `d.tx`), `Counts`, `changeAsset`.
- Produces:

```go
const SchemaVersion = 3
const OutcomeMissing Outcome = "missing" // the card file's asset is missing from the archive
// Counts gains (counted in Seen):
Missing int `json:"missing"`
// ErrWrongState: an asset update found the asset in a state the update doesn't apply to.
var ErrWrongState = errors.New("catalog: the asset is in another state")
```

- [ ] **Step 1: Write the failing tests**
  - `ingests_test.go` `TestMigrateV2ToV3`: build a v2 database by hand (`schemaV1`, `schemaV2`, `PRAGMA user_version = 2` on a raw `sql.Open("sqlite", path)`), insert one ingest and three `ingest_files` rows (outcomes `new`, `in_archive`, `seen_before`, distinct card paths); `Open` → `Version` 3; `IngestFiles(1)` returns the three in the same order with the same fields; an `AddIngestFiles` row with `OutcomeMissing` is accepted; `Ingest(1).Counts.Missing` is 0; `pragma_index_list('ingest_files')` has `ingest_files_ingest` and `ingest_files_sha256`; tables `archive_copies`, `archive_changes`, `archive_scans` exist (`sqlite_master`). Update `TestMigrateV1ToV2`'s version check to `SchemaVersion` (it now ends at 3).
  - `TestCountsAddMissing`: `Add(OutcomeMissing)` → `Seen 1, Missing 1`.
  - `TestFinishIngestStoresMissing`: `FinishIngest` with `Counts{Seen: 2, Missing: 2}` → read back `Missing 2`.
  - `stagingops_test.go` `TestChangeAssetWrongState`: `CullAsset` on an archived asset → `errors.Is(err, ErrWrongState)` and the text still contains `the asset is archived`; unknown hash still `ErrNotFound`.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/catalog/ -run 'Migrate|Missing|WrongState'` → failures (version 2, undefined names).
- [ ] **Step 3: Implement.** Append `schemaV3` to `migrations` exactly:

```go
// schemaV3 (M5): the archive indexer. Imports record files whose archive
// copy is missing; the indexer keeps duplicate paths, in-place changes and
// its scan history.
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

-- Other archive paths holding the content of an archived asset (an asset
-- has one rel_path). No foreign key: a copy can outlive its asset (Forget
-- deletes staged and culled ones); the indexer resolves such a copy again.
CREATE TABLE archive_copies (
  rel_path TEXT PRIMARY KEY,
  sha256   BLOB NOT NULL CHECK (length(sha256) = 32),
  size     INTEGER NOT NULL,
  mtime    INTEGER NOT NULL
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

-- Indexer passes; result is JSON owned by internal/indexer. NULL finished_at = still running (or the process died).
CREATE TABLE archive_scans (
  id          INTEGER PRIMARY KEY AUTOINCREMENT,
  kind        TEXT NOT NULL CHECK (kind IN ('manual','nightly')),
  started_at  TEXT NOT NULL,
  finished_at TEXT,
  result      TEXT NOT NULL DEFAULT ''
);
`
```

Then: `SchemaVersion = 3`; `OutcomeMissing`; `Counts.Missing` with `Add` handling it; `validOutcome` accepts it; `ingestCols`, `scanIngest` and `FinishIngest` read and write `missing` (after `seen_before`, keeping the existing order otherwise). In `changeAsset`, the final error becomes `fmt.Errorf("%s %s: the asset is %s: %w", verb, sha, state, ErrWrongState)`.
- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS (ingest's `TestKnownOutcomeRefusesUnknownState` still passes: ingest changes in Task 4).
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/
git commit -m "Add catalog schema 3 for the archive indexer and the missing outcome

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Catalog archive queries and the backup

Suggested implementer: sonnet (opus reviewer: the change transaction).

**Files:**
- Create: `internal/catalog/archive.go`, `internal/catalog/archive_test.go`

**Interfaces:**
- Consumes: schema v3 (Task 1), `insertAsset`/`insertFingerprint`, `hexToBlob`/`blobToHex`, `formatTime`/`parseTime`, `prefixRange`, `changeAsset`, `ErrWrongState`, `ErrNotFound`.
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
}
// ArchiveCopy is another archive path holding an asset's content.
type ArchiveCopy struct {
	RelPath    string
	SHA256     string
	Size       int64
	Mtime      int64
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
	At     time.Time `json:"at"`     // newest updated_at of its imported files
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
	ID         int64
	Kind       string // manual | nightly
	StartedAt  time.Time
	FinishedAt time.Time // zero: unfinished
	Result     string    // JSON written by internal/indexer
}

func (d *DB) ArchiveEntries(ctx context.Context) ([]ArchiveEntry, error)
func (d *DB) ArchiveCopies(ctx context.Context) ([]ArchiveCopy, error)
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
func (d *DB) StartArchiveScan(ctx context.Context, kind string, at time.Time) (int64, error)
func (d *DB) FinishArchiveScan(ctx context.Context, id int64, at time.Time, result string) error
func (d *DB) LastArchiveScan(ctx context.Context) (ArchiveScan, error) // newest finished; ErrNotFound when none
func (d *DB) UnfinishedArchiveScans(ctx context.Context) ([]ArchiveScan, error)
func (d *DB) Backup(ctx context.Context, path string) error
```

Semantics (each is one statement or one `d.tx`):
- `ArchiveEntries`: every asset with `state IN ('archived','missing')` and `rel_path` in `prefixRange(ArchiveRoot)`, ordered by `rel_path`.
- `ArchiveCopies`: every `archive_copies` row `LEFT JOIN assets` for the state, ordered by `rel_path`.
- `SetArchivePath`: `changeAsset(ctx, "set archive path", sha, []AssetState{StateArchived, StateMissing, StateCulled}, "state = 'archived', rel_path = ?, mtime = ?, updated_at = ?", …)` — a `staged` asset is `ErrWrongState`. (Move, touch, reappear and recover are all this.)
- `MarkMissing`: `changeAsset(…, []AssetState{StateArchived}, "state = 'missing', updated_at = ?")`, `rel_path` kept.
- `RecordArchiveChange`: `ErrBadHash` for bad hashes; `old.SHA256 == sha` is an error. In one transaction, exactly:

```sql
-- 1. The new content becomes the asset; kind, capture time, offset and first ingest stay.
INSERT INTO assets (sha256, size, kind, capture_time, tz_offset, state, rel_path, mtime, first_ingest, updated_at)
  SELECT :new, :size, kind, capture_time, tz_offset, 'archived', :rel, :mtime, first_ingest, :at
  FROM assets WHERE sha256 = :old AND state = 'archived';
-- (no row inserted → ErrWrongState, or ErrNotFound when :old is unknown; roll back)
-- 2. Fingerprints: the old content's still find the asset (the card original stays a duplicate, §6).
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

  `:rel` is `old.RelPath`; `indexed_at` for a first change is `old.UpdatedAt`. If the new hash equals the first old hash of an existing entry (changed and then reverted), the entry is deleted instead of re-inserted.
- `PutArchiveCopy`: upsert by `rel_path`. `DeleteArchiveCopy`: deleting an absent row is not an error.
- `ArchiveChanges`: up to `limit` rows by `rel_path`, plus `COUNT(*)` and `MAX(id)` (0 when empty). `AcceptArchiveChanges`: `DELETE … WHERE id <= ?`, returns rows affected; `upTo < 1` → error.
- `ArchiveCounts`: one query per number is fine. `MissingFiles`: `state = 'missing'` under `Archive/`, by `rel_path`, `limit`.
- `RecentArchiveFolders`:

```sql
SELECT substr(rel_path, 1, 8 + instr(substr(rel_path, 9), '/') - 1) AS folder, COUNT(*), MAX(updated_at)
FROM assets
WHERE state = 'archived' AND first_ingest IS NOT NULL AND rel_path >= 'Archive/' AND rel_path < 'Archive0'
  AND instr(substr(rel_path, 9), '/') > 0
GROUP BY folder ORDER BY MAX(updated_at) DESC, folder LIMIT ?
```

  (`'Archive/'` is 8 characters; `substr`/`instr` count characters, so non-ASCII names are fine.)
- `StartArchiveScan` inserts `(kind, started_at)`; `FinishArchiveScan` sets `finished_at`, `result` (exactly one row, else `ErrNotFound`) and then deletes all but the newest 30 rows by id; `UnfinishedArchiveScans` returns rows with `finished_at IS NULL`.
- `Backup`: `d.db.ExecContext(ctx, "VACUUM INTO ?", path)`, wrapped `back up catalog to <path>: %w`.

- [ ] **Step 1: Write the failing tests** (`archive_test.go`, with a helper that inserts an archived asset with a fingerprint and one ingest-file row naming it):
  - `TestArchiveEntriesAndCopies`: archived under `Archive/`, missing under `Archive/`, staged under `Staging/`, archived under `Archive2/` (prefix trap) → entries are the first two; a copy of the archived asset and a copy whose hash has no asset → `AssetState` `archived` and `""`.
  - `TestSetArchivePath`: from archived (move), missing (reappear) and culled (recover, `rel_path` was NULL) → archived at the new path with the new mtime; from staged → `ErrWrongState`, row unchanged.
  - `TestMarkMissing`: archived → missing with `rel_path` kept; missing again → `ErrWrongState`.
  - `TestRecordArchiveChange`: old asset A (kind raw, capture time set, first ingest 1, fingerprint FA, one alias X→A, an ingest-file row naming A, a clock warning naming A) changed to B with fingerprint FB → `AssetsByHash(A)` empty; B archived at the same path, size/mtime new, kind/capture time/first ingest kept; `AssetBySHA(A)` → B (alias); `AssetBySHA(X)` → B; `AssetsByFingerprint(FA)` and `(FB)` → B; `IngestFiles(1)` row has `SHA256 B` and `RelPath` the archive path; `ArchiveChanges` → one entry (old A, new B, old size, size, `indexed_at` = A's `updated_at`).
  - `TestRecordArchiveChangeTwiceAndRevert`: A→B then B→C → one entry, old A, new C, id greater than the first; then C→A → `AssetsByHash(A)` is the asset again, no alias with `old_sha256 = A`, `AssetBySHA(C)` → A, and the entry is gone.
  - `TestRecordArchiveChangeWrongState`: old row missing or staged → `ErrWrongState`, nothing changed (transaction rolled back: old row, fingerprints, aliases intact).
  - `TestAcceptArchiveChanges`: three entries → accept `upTo` = second id → 2 accepted, one left; `upTo 0` → error.
  - `TestArchiveCounts`, `TestMissingFiles`, `TestRecentArchiveFolders` (two folders, one with a non-ASCII name, a file directly in `Archive/` excluded, staged and indexer-found assets excluded; order by newest `updated_at`; `limit` honoured).
  - `TestArchiveScans`: start two, finish the first → `LastArchiveScan` is it, `UnfinishedArchiveScans` is the second; finishing 31 rows leaves 30.
  - `TestBackupVacuumInto`: insert an asset, `Backup(path)` in a temp dir → `Open(path)` works, version 3, the asset is there; `Backup` onto an existing file → error.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/catalog/ -run 'Archive|Backup|Recent|Missing'` → build failure.
- [ ] **Step 3: Implement** `archive.go` as specified (no `*sql.Rows` open across another call; collect slices first).
- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/archive.go internal/catalog/archive_test.go
git commit -m "Add the catalog queries for the archive index and the VACUUM INTO backup

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Settings for the rescan time and backup folder; settings as strings

Suggested implementer: haiku (the code is small; the rules are below).

**Files:**
- Modify: `internal/catalog/settings.go`, `internal/catalog/settings_test.go`

**Interfaces:**
- Produces:

```go
const (
	KeyArchiveRescanAt = "archive_rescan_at"
	KeyBackupDir       = "backup_dir"
)
// Settings gains:
ArchiveRescanAt time.Duration // nightly rescan, since local midnight; default 3h
BackupDir       string        // "" = <photos>/Management/SD2NAS (the indexer resolves it)

// SettingError is a value a key doesn't accept; Error() keeps today's text
// ("setting <key> = "<value>": <reason>").
type SettingError struct{ Key, Value, Reason string }

func SettingKeys() []string                      // every known key, in a fixed order
func (s Settings) Strings() map[string]string    // canonical text of every key
func (d *DB) SetSettings(ctx context.Context, kv map[string]string) error // validate all, store all in one transaction
```

Rules: `archive_rescan_at` uses the `day_starts_at` pattern (`HH:MM`, reason `want HH:MM, 00:00 to 23:59`); `backup_dir` is `""` or `filepath.IsAbs`, ≤ 1024 bytes, no control characters (reason `want an absolute path, or "" for the default`). `Strings`: `day_starts_at` and `archive_rescan_at` as `%02d:%02d`, `verify_known` as stored, `free_space_reserve_gb` as `ReserveBytes / 1e9`, `staging_warn_days` as a decimal, `backup_dir` as stored. Every `apply` failure for a known key becomes `*SettingError`; an unknown key stays `unknown setting "<key>"`. `Settings()` loops `SettingKeys()` instead of its literal list. `SetSettings` with an empty map is an error; any invalid key or value stores nothing.

- [ ] **Step 1: Write the failing tests**: defaults (`ArchiveRescanAt 3h`, `BackupDir ""`); round trip `02:30` and an absolute temp path; rejects `24:00`, `3:00`, `relative/dir`, a value with `\n`; `errors.As(err, &se)` with `Key`/`Reason`; `Strings()` of the defaults has all six keys and feeding it to `SetSettings` succeeds and changes nothing; `SetSettings` with one bad value stores none of the good ones.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/catalog/ -run Setting` → build failure.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/settings.go internal/catalog/settings_test.go
git commit -m "Add the nightly rescan time and backup folder settings

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Imports block "safe to format" on missing archive copies

Suggested implementer: haiku.

**Files:**
- Modify: `internal/ingest/run.go`, `internal/ingest/describe.go`, `internal/ingest/run_test.go`, `internal/ingest/describe_test.go`

**Interfaces:**
- Consumes: `catalog.OutcomeMissing`, `Counts.Missing` (Task 1).
- Produces: no new exports. `knownOutcome(StateMissing)` = `OutcomeMissing`; `stateRank` unchanged (archived still wins over missing for a fingerprint matching both).

- [ ] **Step 1: Update and write tests**
  - `TestKnownOutcomeRefusesUnknownState`: `StateMissing` → `OutcomeMissing`.
  - `TestRunKnownOutcomes`: expect `Counts{Seen: 3, InArchive: 1, SeenBefore: 1, Missing: 1}` and **not** safe to format; the missing file's row has outcome `missing`.
  - `TestRunMissingBlocksSafeToFormat`: a card whose only file matches a `missing` asset (archive path set) → status done, `SafeToFormat false`, `Counts.Missing 1`, `Describe` message ends with `1 file's archive copy is missing. Do not format the card.`; the same fingerprint also matching an `archived` asset → `in_archive`, safe.
  - `describe_test.go` `TestDescribeMissing`: `Counts{Seen: 42, New: 0, InArchive: 40, Missing: 2}` → `SONY_A7IV: 0 new, 40 already known, 2 files' archive copies are missing. Do not format the card.` (level warning); with `Failed: 1` the failed part comes before the missing part; with `Unsupported` it comes after.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/ingest/ -run 'Known|Missing|Describe'`.
- [ ] **Step 3: Implement**: `knownOutcome`; in `finish`, `safe := … && c.Missing == 0 && …`; in `describeDone`, `known` stays `InArchive + InStaging + SeenBefore`, and after the failed part:

```go
	if c.Missing > 0 {
		body += ", " + missingCopies(c.Missing)
	}
```

with `missingCopies(1) = "1 file's archive copy is missing"`, otherwise `fmt.Sprintf("%d files' archive copies are missing", n)`. The log line `ingest finished` gains `"missing", c.Missing`.
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

- [ ] **Step 1: Write the failing test** `TestNameHeld`: `TryLockSet(HolderPromote, LockSet{IDs: {7}, Names: {"Archive/2025/05/2025-05-30 - Trip"}})` → true for `archive/2025/05/2025-05-30 - TRIP`, `Archive/2025/05/2025-05-30 - Trip/Capture/x.ARW`; false for `Archive/2025/05`, `Archive/2025/05/2025-05-30 - Trip2`, `Staging/2025-05-30 - Trip`; after release → false; a lock with only `IDs` → false for everything. Run concurrently with `TryLockSet`/release in a goroutine for the race detector.
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

### Task 6: `internal/indexer` skeleton and the listing rules

Suggested implementer: sonnet.

**Files:**
- Create: `internal/indexer/indexer.go`, `internal/indexer/walk.go`, `internal/indexer/walk_test.go`, `internal/indexer/helpers_test.go`

**Interfaces:**
- Consumes: `catalog.DB`, `media.DefaultRules`, `media.Rules.KindOf`.
- Produces (Task 9 fills in the methods; this task needs the types, `New`, `Close` and the listing):

```go
// Package indexer keeps the catalog in line with Archive/ (§9): the first
// pass is the backfill, later ones the nightly rescan, then the catalog backup.
package indexer

const (
	ArchiveDir = "Archive" // must equal staging.ArchiveDir (internal/api test)
	StagingDir = "Staging" // must equal staging.Dir
)

// Locks reports archive directories a promote is writing into; the shared
// *staging.Locks satisfies it (Decision 12).
type Locks interface{ NameHeld(rel string) bool }

type Config struct {
	DB         *catalog.DB
	PhotosRoot string
	Locks      Locks            // nil: nothing is ever held
	Busy       func() bool      // an import is running or queued, or a promote runs: pause between files; nil: never
	Now        func() time.Time // nil: time.Now
	Log        *slog.Logger     // nil: slog.Default()
}

const (
	TriggerManual  = "manual"
	TriggerNightly = "nightly"
	PhaseListing   = "listing"
	PhaseHashing   = "hashing"
	PhaseChecking  = "checking"
	PhaseBackup    = "backup"
)

func New(cfg Config) (*Service, error)
func (s *Service) Close() // stops a pass between files (and mid-file) and the nightly loop; waits; idempotent
```

`Service` holds `db, photosRoot, locks, busy, now, log, rules`, the seams `readDir` (`os.ReadDir`), `hash`, `fingerprint`, `after`, `beforeRead`, `afterHash`, `beforeMissing`, `idleEvery` (2 s), `bgCtx/bgCancel/wg`, `pass sync.Mutex` (held by the running pass), `reschedule chan struct{}` (buffer 1), and `mu` guarding `status`, `notify`, `closed`, `lastReport`. `New` only sets these up in this task (Task 9 adds the startup reads).

`walk.go`:

```go
// metadataNames are NAS and OS housekeeping names that never hold the
// owner's photos: the same list as staging's skipName (internal/staging/
// reconcile.go); keep them in sync. Dot-names are skipped as well.
var metadataNames = []string{"@eaDir", "@Recycle", "@Recently-Snapshot", "#recycle", "#snapshot",
	"$RECYCLE.BIN", "System Volume Information"}

// sessionSkipDirs never hold originals: Capture One's exports and its
// cache and settings (§9). Skipped anywhere below a session folder.
var sessionSkipDirs = []string{"Output", "CaptureOne"}

func skipName(name string) bool {
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

// skipDir reports a directory the indexer never walks into; depth is its
// depth below Archive/ (1 = a session folder).
func skipDir(name string, depth int) bool {
	if skipName(name) {
		return true
	}
	if depth >= 2 {
		for _, n := range sessionSkipDirs {
			if strings.EqualFold(name, n) {
				return true
			}
		}
	}
	return false
}

// indexable reports whether a regular file named name is indexed, and its kind.
func indexable(rules media.Rules, name string) (media.Kind, bool) {
	if skipName(name) || strings.HasSuffix(strings.ToLower(name), ".cosessiondb") {
		return "", false
	}
	return rules.KindOf(name)
}

// skipRel reports a rel_path ("Archive/S/Output/x.JPG") the listing never
// reaches, so a catalog row there is left alone (never missing).
func skipRel(rel string) bool {
	parts := strings.Split(strings.TrimPrefix(rel, ArchiveDir+"/"), "/")
	for i, p := range parts {
		if i == len(parts)-1 {
			_, ok := indexable(media.DefaultRules(), p)
			return !ok
		}
		if skipDir(p, i+1) {
			return true
		}
	}
	return false
}
```

The listing is a recursive `walk(dir, rel string, depth int) error` over `s.readDir` (sorted names, symlinks never followed: only `e.IsDir()` recurses and only `e.Type().IsRegular()` files count). Before reading a directory below `Archive/` it checks `ctx` and `s.held(rel)`; a held directory is recorded in the pass's skipped set and `busy_folders` is incremented, and it is not read. A `ReadDir` error on `Archive/` itself is returned (the caller tells "missing" from other errors); on any other directory it is recorded as a problem and the directory goes into the skipped set. A file whose `Info()` fails with `ErrNotExist` is ignored; any other `Info()` error is a problem and the file's path goes into the skipped set. Each indexable file is handed to `found(file{rel, size, mtime, kind})` (Task 7). For this task `found` may simply append to a slice; `held(rel)` is `s.locks != nil && s.locks.NameHeld(rel)`.

`helpers_test.go`: `setup(t) (s *Service, db *catalog.DB, photos string)` with a temp catalog and photos root (`t.Cleanup(s.Close)` after the `TempDir`s), `put(t, photos, rel, content string, mtime time.Time) string` (writes, `os.Chtimes` to whole seconds, returns the OS path), `sum(content) string` (SHA-256 hex), and a fake `Locks` (`map[string]bool` of lower-cased held names, ancestor-aware like `NameHeld`).

- [ ] **Step 1: Write the failing tests** (`walk_test.go`):
  - `TestSkipRules`: under `Archive/2025-05-30 - Trip/`: `a.JPG`, `Capture/b.ARW`, `Selects/c.JPG`, `Trash/d.ARW`, `Videos/e.MP4`, `Other/f.JPG`, `Edits/g.JPG` (deny list: indexed), `Output/h.JPG`, `Capture/CaptureOne/Cache/i.JPG`, `Selects/CaptureOne/j.JPG`, `2025-05-30 - Trip.cosessiondb`, `notes.txt`, `.DS_Store`, `._a.JPG`, `@eaDir/a.JPG/SYNOPHOTO_THUMB_XL.jpg`, `#recycle/k.JPG`; plus `Archive/l.JPG` (top level of the archive, indexed) and `Archive/Output/m.JPG` (a session folder that happens to be called Output: depth 1, indexed). The listing yields exactly the indexed rel_paths, sorted, with the right kinds and sizes.
  - `TestSkipRel`: `Archive/S/Output/x.JPG`, `Archive/S/Capture/CaptureOne/x.JPG`, `Archive/S/.hidden/x.JPG`, `Archive/S/x.txt` → true; `Archive/S/Capture/x.ARW`, `Archive/Output/x.JPG` → false.
  - `TestListingSkipsHeldFolder`: fake locks hold `archive/2025-05-30 - trip` → none of its files listed, `busy_folders` 1; a nested hold `Archive/2025/05/T` skips only `T`.
  - `TestListingSymlinkNotFollowed` (skip on Windows when `os.Symlink` fails): a symlinked directory and a symlinked file are not listed.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/` → build failure.
- [ ] **Step 3: Implement** `indexer.go` (types, `New`, `Close`) and `walk.go`.
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; `go list -f '{{join .Imports "\n"}}' ./internal/indexer/` names no `internal/staging`, `internal/ingest` or `internal/api`; `go vet ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Add the archive indexer package and its Capture One listing rules

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: The pass: backfill, unchanged skip, resume, staged and culled content

Suggested implementer: sonnet (opus reviewer).

**Files:**
- Create: `internal/indexer/hash.go`, `internal/indexer/scan.go`, `internal/indexer/scan_test.go`
- Modify: `internal/indexer/indexer.go`, `internal/indexer/walk.go`

**Interfaces:**
- Consumes: Task 2's queries, Task 6's listing, `media.QuickFingerprint`, `catalog.InsertAsset`, `AssetsByHash`, `AssetsByFingerprint`, `AddFingerprint`.
- Produces (Task 9 wraps it with status and events):

```go
// ScanResult is what one pass did; it is stored as JSON in archive_scans
// and sent as Status.Last.
type ScanResult struct {
	Trigger       string        `json:"trigger"`
	StartedAt     time.Time     `json:"started_at"`
	FinishedAt    time.Time     `json:"finished_at"`
	Listed        int           `json:"listed"`         // media files found
	Unchanged     int           `json:"unchanged"`      // size and modified time matched: not read
	Added         int           `json:"added"`          // new content indexed
	Moved         int           `json:"moved"`          // known content at a new path
	Changed       int           `json:"changed"`        // same path, new content ("changed since indexed")
	Reappeared    int           `json:"reappeared"`     // missing content found again
	Recovered     int           `json:"recovered"`      // culled content found in the archive, now archived
	Missing       int           `json:"missing"`        // newly marked missing
	Copies        int           `json:"copies"`         // new duplicate paths of archived content
	InStaging     int           `json:"in_staging"`     // content still staged (a promote in flight): left alone
	BusyFolders   int           `json:"busy_folders"`   // skipped: a promote held them
	Errors        []string      `json:"errors"`         // at most 20, never null
	ErrorsOmitted int           `json:"errors_omitted,omitempty"`
	Skipped       string        `json:"skipped,omitempty"` // why nothing (more) was done
	Stopped       bool          `json:"stopped,omitempty"` // interrupted: the next pass carries on
	Backup        *BackupResult `json:"backup,omitempty"`  // nightly only (Task 10)
}

// Rescan runs one pass now and waits for it (tests, the nightly run); it
// waits for a pass that is already running. err is non-nil only for
// catalog failures; cancellation sets Stopped.
func (s *Service) Rescan(ctx context.Context, trigger string) (ScanResult, error)
```

`hash.go`:

```go
// hashFile is media.HashFile that stops when ctx ends (a 4 GB video must
// not hold up shutdown), reading sequentially in 1 MiB blocks.
func hashFile(ctx context.Context, path string) (sum string, err error)
```

(implemented with a `ctxReader{ctx, f}` that returns `ctx.Err()` before each `Read`, and `io.CopyBuffer` with a 1 MiB buffer; errors wrapped `hash <path>: %w`).

`scan.go` — a pass is a `*scan` value:

```go
type file struct {
	rel         string
	size, mtime int64
	kind        media.Kind
	sha, fp     string               // known content (a copy whose size and time match) / after reading
	editedFrom  *catalog.ArchiveEntry // the archived content this path held (its size or time changed)
}

type scan struct {
	s          *Service
	ctx        context.Context // stops the pass between files and mid-hash
	dbctx      context.Context // context.WithoutCancel(ctx): what was read is recorded
	r          *ScanResult
	byPath     map[string]catalog.ArchiveEntry // archived rows
	bySHA      map[string]catalog.ArchiveEntry // the same rows by hash
	copies     map[string]catalog.ArchiveCopy  // by rel_path
	keepCopy   map[string]bool                 // copy rows confirmed or written this pass
	seenCopies map[string]catalog.ArchiveCopy  // by sha: a copy of archived content seen this pass
	skipped    map[string]bool                 // rels the listing didn't read (held, unreadable)
	seen       map[string]bool                 // sha of archived rows accounted for this pass
	changed    []file
	fresh      []file
}
```

The pass (`run`), in order — all problems go through `r.errorf` (logged, first 20 listed, rest counted), exactly like `staging.RescanResult.errorf`:
1. `os.Stat(<photos>/Archive)`: not exist → `Skipped` = the "is missing" message, return (nothing changed). Load `ArchiveEntries` (archived ones into `byPath`/`bySHA`; `missing` rows need no map, they are found by hash) and `ArchiveCopies`.
2. **Listing** (`PhaseListing`): `walk`. `found(f)`:

```go
func (sc *scan) found(f file) {
	sc.r.Listed++
	if a, ok := sc.byPath[f.rel]; ok {
		sc.seen[a.SHA256] = true
		if a.Size == f.size && a.Mtime == f.mtime {
			sc.r.Unchanged++
			return
		}
		f.editedFrom = &a
		sc.changed = append(sc.changed, f)
		return
	}
	if c, ok := sc.copies[f.rel]; ok && c.Size == f.size && c.Mtime == f.mtime {
		if c.AssetState == catalog.StateArchived {
			sc.keepCopy[f.rel] = true
			sc.seenCopies[c.SHA256] = c
			sc.r.Unchanged++
			return
		}
		f.sha = c.SHA256 // known content whose asset left the archive: resolve it again, unread
	}
	sc.fresh = append(sc.fresh, f) // includes paths of missing rows and changed copies
}
```

3. **Hashing** (`PhaseHashing`), changed paths first, then fresh ones, each preceded by `idle()` (Task 8; a no-op here) and a re-check `s.held(f.rel)` (held now → `BusyFolders++`, skip). `read(f, full bool)` does: `beforeRead` hook; `os.Lstat` (not exist → skip silently); fingerprint; if `full`, `hash(ctx, path)`; `afterHash` hook; `os.Lstat` again — a size or modified time different from the first `Lstat` → `r.errorf("%s changed while it was read (still being copied?); the next scan picks it up", rel)` and skip; otherwise `f.size`/`f.mtime` take the stat's values. Progress counts every read (Task 9).
   - A changed path: full read. Same hash as `editedFrom` → `SetArchivePath(sha, rel, mtime)` (a touch), `Unchanged++`. Otherwise `delete(sc.seen, editedFrom.SHA256)` and `resolve(f)` (Task 8 completes the edit branch; in this task an unknown hash with `editedFrom` set calls `RecordArchiveChange` and counts `Changed`).
   - A fresh path with `f.sha == ""`: fingerprint first; the move shortcut is Task 8; then a full read. Then `resolve(f)`.
4. `resolve(f)` (this task: the branches below; Task 8 adds moved, copy, reappeared):

```go
	found, err := db.AssetsByHash(sc.dbctx, []string{f.sha})
	if err != nil {
		return err
	}
	a, known := found[f.sha]
	switch {
	case !known:
		err = db.InsertAsset(sc.dbctx, catalog.Asset{SHA256: f.sha, Size: f.size, Kind: f.kind,
			State: catalog.StateArchived, RelPath: f.rel, Mtime: f.mtime, UpdatedAt: now}, f.fp)
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
	if errors.Is(err, catalog.ErrWrongState) {
		sc.r.errorf("%s: its catalog entry changed during the scan; the next scan looks again", f.rel)
		return nil
	}
	return err
```

   After a successful branch that read a fingerprint for content first indexed elsewhere, call `AddFingerprint(fp, sha)` (insert-or-ignore) so later moves of this file take the shortcut.
5. Cancellation anywhere → `Stopped = true`, return `nil` error (Task 9 records it). Checking (`missing`) is Task 8 and runs only when steps 2–4 finished.

- [ ] **Step 1: Write the failing tests** (`scan_test.go`; files are tiny text with `.JPG`/`.ARW` names — kinds come from the extension; count reads by wrapping `s.hash`):
  - `TestBackfillIndexesArchive`: `Archive/2025-05-30 - Trip/a.JPG`, `Archive/2025-05-30 - Trip/Capture/b.ARW`, `Archive/2007-09-31 - Random Party/c.JPG` (folder names are text: Decision 7) → `Added 3, Listed 3`; each asset `archived`, `first_ingest` 0, `capture_time` zero, right kind, size, mtime (UnixNano of the file), rel_path; `AssetsByFingerprint(QuickFingerprint(path))` finds each.
  - `TestRescanSkipsUnchanged`: second `Rescan` → zero hash calls, `Unchanged 3`, nothing else.
  - `TestRescanSkipsPromotedFiles`: an asset inserted as `archived` with `first_ingest` = an ingest id, at a path whose file has the same size and modified time → not read, unchanged, `first_ingest` kept.
  - `TestNewFileCopiedInByHand`: after a backfill, add `d.JPG` → `Added 1`, one hash call.
  - `TestBackfillResumes`: five files; `beforeRead` cancels the pass's context on the third read → `Stopped`, two assets; the next `Rescan` reads exactly three files → five assets, `Unchanged 2`.
  - `TestArchiveMissingChangesNothing`: no `Archive/` (with archived rows in the catalog) → `Skipped` is the exact "is missing" message; rows unchanged.
  - `TestStagedContentLeftAlone`: a `staged` asset under `Staging/` whose content is also at `Archive/X/a.JPG` → `InStaging 1`; asset still staged at its staging path; no copy row.
  - `TestCulledContentRecovered`: a `culled` asset (no rel_path) whose content is at `Archive/X/a.JPG` → `Recovered 1`; archived at that path with the file's mtime.
  - `TestFileChangingWhileReadSkipped`: `afterHash` appends to the file once → the exact "changed while it was read" error; no asset; the next pass adds it.
  - `TestTouchedFileOnlyUpdatesMtime`: same content, new modified time → one read, `Unchanged`, mtime updated, no change entry; the following pass reads nothing.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'Backfill|Rescan|NewFile|Staged|Culled|Changing|Touched|ArchiveMissing'`.
- [ ] **Step 3: Implement** `hash.go`, `scan.go`, `Rescan` (for now: take `s.pass`, build the scan, run it, return the result; no status, no persistence).
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Index the archive: backfill, skip unchanged files, resume after a stop

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Moves, copies, changes in place, missing files, busy folders, pausing

Suggested implementer: sonnet (opus reviewer: Review Focus 1–4).

**Files:**
- Modify: `internal/indexer/scan.go`, `internal/indexer/scan_test.go`

**Interfaces:**
- Consumes: Task 7; `SetArchivePath`, `MarkMissing`, `RecordArchiveChange`, `PutArchiveCopy`, `DeleteArchiveCopy`.
- Produces: no new exports.

Add to the pass:

```go
// isGone: archived content not seen at its path this pass, and inside what the listing read.
func (sc *scan) isGone(sha string) bool {
	e, ok := sc.bySHA[sha]
	return ok && !sc.seen[sha] && sc.inScope(e.RelPath)
}

// inScope: the listing could have seen rel (no skip-rule element, no
// skipped ancestor directory, not itself skipped).
func (sc *scan) inScope(rel string) bool {
	if skipRel(rel) {
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

- **Move shortcut** (fresh path, `f.sha == ""`): after the fingerprint, `AssetsByFingerprint(fp)`; the first candidate with `Size == f.size` that is `missing`, or `archived` with `isGone(sha)`, gives `f.sha` without a full read. Otherwise read fully.
- **`resolve` branches added** (order matters):

```go
	case !known && f.editedFrom != nil && !sc.hasCopy(f.editedFrom.SHA256):
		err = db.RecordArchiveChange(sc.dbctx, *f.editedFrom, f.sha, f.fp, f.size, f.mtime, now)
		// Changed++, seen[f.sha] = true
	case !known:
		// InsertAsset (Task 7): Added++ (an edited path whose old content survives as a copy lands here;
		// the old content moves to that copy in the checking phase)
	case a.State == catalog.StateArchived && sc.isGone(a.SHA256):
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now) // Moved++, seen[sha] = true
	case a.State == catalog.StateArchived:
		// still at its path, or below a folder this pass skipped: a duplicate in the archive
		err = db.PutArchiveCopy(sc.dbctx, catalog.ArchiveCopy{RelPath: f.rel, SHA256: a.SHA256, Size: f.size, Mtime: f.mtime})
		// Copies++ unless the row already existed (a copy that changed and was re-read), keepCopy[rel] = true, seenCopies[sha] = the copy
	case a.State == catalog.StateMissing:
		err = db.SetArchivePath(sc.dbctx, a.SHA256, f.rel, f.mtime, now) // Reappeared++
	case a.State == catalog.StateCulled: // Task 7
	case a.State == catalog.StateStaged: // Task 7
```

  `hasCopy(sha)` is `seenCopies[sha]` present.
- **Checking** (`PhaseChecking`), only after the hashing phase finished: for each archived row in `byPath`, sorted by `rel_path`, that is not `seen` and is `inScope`:
  1. `ctx` ended → stop (`Stopped`).
  2. A seen copy of its content → `SetArchivePath` to the copy's path and mtime, `DeleteArchiveCopy(copy)`, `Moved++`, drop it from `seenCopies`.
  3. `s.held(rel)` → leave it.
  4. `beforeMissing(rel)` hook; `os.Lstat(abs(rel))`: no error → it is back (leave it; the next pass reads it); an error other than not-exist → `errorf` and leave it.
  5. `os.Stat(<photos>/Archive)` must succeed; not-exist → `Skipped` = the "went missing during the scan" message and stop the checking phase (nothing more is marked); another error → `errorf` and stop.
  6. `MarkMissing` → `Missing++` (`ErrWrongState` → `errorf`, continue).
  Then delete every copy row that is `inScope`, not in `keepCopy` and not just promoted to primary (its path is gone or changed content).
- **Pausing**: `idle()` before every read:

```go
// idle waits while an import or promote is busy (Decision 13).
func (sc *scan) idle() error {
	busy := sc.s.busy
	if busy == nil || !busy() {
		return sc.ctx.Err()
	}
	sc.s.progress(true, func(p *Progress) { p.Paused = true })
	defer sc.s.progress(true, func(p *Progress) { p.Paused = false })
	for busy() {
		select {
		case <-sc.ctx.Done():
			return sc.ctx.Err()
		case <-time.After(sc.s.idleEvery):
		}
	}
	return sc.ctx.Err()
}
```

  (`progress` is a no-op stub until Task 9.) `busy` is never called with `s.mu` held: `main`'s `Busy` takes the ingest manager's and the staging service's locks.

- [ ] **Step 1: Write the failing tests** (`scan_test.go`):
  - `TestMoveWithinSession`: backfill, then `os.Rename` `Capture/b.ARW` → `Selects/b.ARW` (keeps mtime) → `Moved 1`, `Missing 0`, `Added 0`, **zero full hash calls** (fingerprint only); rel_path updated.
  - `TestSessionRenamedByHand`: rename the session folder → every file `Moved`, nothing missing or added, zero full hash calls.
  - `TestMovedAndChangedIsNewPlusMissing`: move a file and change its content → the new path is `Added`, the old asset `Missing` (it really is gone).
  - `TestRescanChangedContent`: overwrite an archived file with other content (new mtime) → `Changed 1`; old hash only an alias of the new asset; the change entry lists old/new hash and sizes; the old fingerprint finds the new asset; `Status`-free check via `ArchiveChanges`.
  - `TestChangeReverted`: change A→B (pass), then write the original bytes back (pass) → asset A archived at the path, `AssetBySHA(B)` → A, no change entry left.
  - `TestChangedToKnownContent`: overwrite file P (asset A) with the bytes of archived file Q (asset B) → P becomes a copy of B (`Copies 1`), A `Missing 1`.
  - `TestDuplicateRecordedAsCopy`: the same bytes at two paths in a backfill → one asset (the first path by sort order), one copy row; the next pass reads nothing (`Unchanged 2`); delete the primary → `Moved 1` to the copy's path, copy row gone, `Missing 0`; delete that file too → `Missing 1`; a deleted copy (primary still there) → copy row removed, nothing missing.
  - `TestPrimaryGoneMovesToCopy`: a copy seen, its primary edited in place (new content) → the path gets a new asset (`Added`), the old asset moves to the copy (`Moved`), no change entry.
  - `TestGoneFileMarkedMissing`: delete a file → `Missing 1`; asset `missing` with rel_path kept; `MissingFiles` lists it.
  - `TestMissingReappears`: same path → `Reappeared 1`, archived; at another path → `Reappeared 1` via the fingerprint shortcut (no full hash).
  - `TestArchiveVanishesBeforeMissing`: `beforeMissing` renames `Archive/` away on the first call → `Skipped` = the exact "went missing during the scan" message; zero assets `missing`.
  - `TestSkippedRowsNeverMissing`: archived rows at `Archive/S/Output/x.JPG`, `Archive/S/Capture/CaptureOne/y.JPG`, `Archive/S/@eaDir/z.JPG` with no files → stay `archived`.
  - `TestUnreadableFolderNotMissing`: `s.readDir` fails with a permission error for one session folder holding an indexed file → a listed problem, the file's asset stays `archived`, and a sibling folder's deleted file is still marked missing.
  - `TestPromoteHeldFolderSkipped`: fake locks hold `Archive/2025-05-30 - Trip` holding a new file and an archived row whose file is absent → neither indexed nor missing, `BusyFolders 1`; release → next pass adds the file and marks the row missing.
  - `TestHalfPromotedFolder`: files at `Archive/T/…` whose assets are `staged` at `Staging/T/…` (the rename done, the catalog write not yet), name held → skipped; then the test applies `ArchiveFolder` (the promote's catalog write) and releases → the next pass reads nothing and counts them `Unchanged`.
  - `TestPausesWhileBusy`: `Busy` true until the test flips it, `idleEvery` 10 ms → no hash call while busy (check after 200 ms), then the pass completes.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'Move|Renamed|Changed|Revert|Duplicate|Copy|Missing|Reappear|Vanish|Skipped|Unreadable|Promote|Busy'`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; Linux command and race command → PASS.
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
// Status is the indexer.progress event data (Decision 19).
type Status struct {
	Version  uint64      `json:"version"`   // bumps after each pass, accept and settings change
	Running  *Progress   `json:"running"`   // nil when no pass runs
	Last     *ScanResult `json:"last"`      // the last finished pass; nil before the first
	Changed  int         `json:"changed"`   // "changed since indexed" entries
	Missing  int         `json:"missing"`   // missing assets
	NextScan time.Time   `json:"next_scan,omitzero"`
}
type Progress struct {
	Trigger     string    `json:"trigger"`
	Phase       string    `json:"phase"`
	StartedAt   time.Time `json:"started_at"`
	Listed      int       `json:"listed"`
	ToHash      int       `json:"to_hash"`       // files to read (changed + new); 0 while listing
	Hashed      int       `json:"hashed"`
	BytesToHash int64     `json:"bytes_to_hash"`
	BytesHashed int64     `json:"bytes_hashed"`
	Folder      string    `json:"folder"`        // the session folder being listed or read
	Paused      bool      `json:"paused"`        // waiting for an import or promote
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
	ArchivePath    string                 `json:"archive_path"`   // OS path of Archive/
	ArchivePresent bool                   `json:"archive_present"`
}
type ChangeList struct {
	Version  uint64                  `json:"version"`
	Changes  []catalog.ArchiveChange `json:"changes"` // ≤ 500, never null
	Total    int                     `json:"total"`
	LatestID int64                   `json:"latest_id"`
}

var ErrClosed = errors.New("indexer: shutting down")

func (s *Service) Status() Status
func (s *Service) OnChange(fn func(Status)) // before serving; fn runs under s.mu, must only publish
func (s *Service) StartRescan() bool         // manual pass in the background; false if one runs or closing
func (s *Service) Stats(ctx context.Context) (Stats, error)
func (s *Service) Changes(ctx context.Context) (ChangeList, error)
func (s *Service) Accept(ctx context.Context, upTo int64) (int, error)
```

Behaviour:
- `New`: closes unfinished `archive_scans` rows (`FinishArchiveScan` with `{"trigger": kind, "started_at": …, "finished_at": now, "stopped": true, "errors": ["SD2NAS stopped before this scan finished"]}`), loads `LastArchiveScan` into `Last` (bad JSON → log, `Last` nil), `ArchiveCounts` into `Changed`/`Missing`. Errors are returned (main reports them).
- `runPass(ctx, trigger)` (called with `s.pass` held; `Rescan` takes it with `Lock`, `StartRescan` with `TryLock` under `s.mu` after checking `closed`, then `s.wg.Add(1)` and sets a `Running` placeholder before returning true): `StartArchiveScan`; `Running` = fresh `Progress` (force publish); the pass; `FinishedAt`; `FinishArchiveScan` with the JSON (on `dbctx`); refresh counts; `Last`, `Running = nil`, `Version++`, force publish; log `archive scan` with every count, `stopped`, `skipped`, `problems`. A catalog error becomes the last entry of `Errors` (listed even past the cap) and is logged at Error.
- `progress(force, fn)`: under `s.mu`, apply `fn` to `status.Running` (no-op when nil) and report. `reportLocked(force)`: unless `force`, skip when the last publish was < 250 ms ago (real clock); `notify(statusLocked())` with `Running`/`Last` copied. Forced: start, each phase change, pause/resume, end, accept, settings change. The pass updates `Listed` per file found, `Folder` per session folder, `ToHash`/`BytesToHash` when the hashing phase starts, `Hashed`/`BytesHashed` after every read.
- `Stats`: `Status()` first (its version), then `ArchiveCounts`, `MissingFiles(50)`, `RecentArchiveFolders(5)`, `Settings` (`RescanAt` from `Strings()`, `BackupDir` via Task 10's `BackupDir`; until Task 10 return the default folder), `ArchivePath`, `ArchivePresent` (`os.Stat`).
- `Changes`: version first, then `ArchiveChanges(500)`. `Accept`: `upTo < 1` → error; `AcceptArchiveChanges`; refresh counts; `Version++`; force publish.
- `Close`: `closed = true` under `s.mu`; cancel `bgCtx`; `wg.Wait()`. After `Close`, `StartRescan` is false and `Accept`/`Stats` still work (the database is closed later by main).

- [ ] **Step 1: Write the failing tests** (`service_test.go`):
  - `TestStartRescanOnce`: `beforeRead` blocks on a channel; `StartRescan` true, again false, `Status().Running` non-nil with trigger `manual`; unblock → wait for `Running == nil`; `Last.Added` set; `Version` bumped; `LastArchiveScan` holds the same JSON.
  - `TestCloseStopsPass`: block inside the first read (`beforeRead`), `Close` in a goroutine returns within 10 s; `Last.Stopped`; a new `Service` over the same catalog and photos finishes the remaining files with one read each.
  - `TestStoppedPassMarksNothingMissing`: an archived row whose file is absent plus a pass cancelled during hashing → still `archived`.
  - `TestInterruptedScanRecorded`: `StartArchiveScan` by hand, then `New` → `Status().Last.Stopped`, `Errors` is exactly `["SD2NAS stopped before this scan finished"]`.
  - `TestProgressThrottled`: 300 files; record each `OnChange` frame with `time.Now()`; for consecutive frames where both have `Running != nil` and the same `Phase` and `Paused`, the gap is ≥ 240 ms (only forced frames may come sooner, and every forced frame inside a pass changes `Phase` or `Paused`); the last frame has `Running == nil` and `Last.Added == 300`.
  - `TestStatsAndAccept`: after a pass with one change and one missing → `Stats` (files, bytes, `Changed 1`, `Missing 1`, `MissingFiles` 1, `RescanAt "03:00"`, `ArchivePresent`), `Changes` (1, `LatestID`); `Accept(LatestID)` → 1, `Status().Changed 0`, version bumped, a forced frame published; `Accept(0)` → error.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'StartRescan|Close|Stopped|Interrupted|Throttled|Stats'`.
- [ ] **Step 3: Implement.**
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; the race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Report archive scan progress, keep the scan history and accept changed files

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Nightly schedule and the catalog backup

Suggested implementer: sonnet.

**Files:**
- Create: `internal/indexer/schedule.go`, `internal/indexer/backup.go`, `internal/indexer/schedule_test.go`, `internal/indexer/backup_test.go`
- Modify: `internal/indexer/indexer.go`, `internal/indexer/stats.go`

**Interfaces:**
- Consumes: `catalog.Settings.ArchiveRescanAt`/`BackupDir` (Task 3), `catalog.DB.Backup` (Task 2), `media.SyncDir`, Task 9.
- Produces:

```go
const BackupKeep = 14

// BackupResult is what the nightly backup did (ScanResult.Backup).
type BackupResult struct {
	Path    string    `json:"path"`             // the file written or attempted
	At      time.Time `json:"at"`
	Bytes   int64     `json:"bytes"`
	Removed []string  `json:"removed"`          // old backups deleted (file names), never null
	Error   string    `json:"error,omitempty"`
}

var ErrBackupInside = errors.New("pick a folder outside Archive/ and Staging/")

func (s *Service) Start()                                   // starts the nightly loop once
func (s *Service) SettingsChanged()                         // re-read the schedule now (non-blocking)
func (s *Service) BackupDir(set catalog.Settings) string    // set.BackupDir, or <photos>/Management/SD2NAS
func CheckBackupDir(photosRoot, dir string) error           // ErrBackupInside when dir is in Archive/ or Staging/ (or is one)
```

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
```

`nightly()`: `s.pass.Lock()` (waits for a manual pass), `runPass(s.bgCtx, TriggerNightly)`; inside `runPass`, when the trigger is nightly and the context is still live, the phase becomes `backup` (forced publish) and `r.Backup = s.backup(...)` runs **before** the scan row is finished, so the stored result includes it. `s.after` defaults to `time.After` (unreferenced timers are collected since Go 1.23). `SettingsChanged` does a non-blocking send on `reschedule` and a forced publish.

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
	if info, err := os.Stat(res.Path); err == nil {
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

`CheckBackupDir` cleans `dir` and, for `Archive` and `Staging` under the photos root, refuses it when `filepath.Rel(root, dir)` is `.` or `filepath.IsLocal`. `syncFile` opens, `Sync`s and closes. `removeIfPresent` ignores not-exist.

- [ ] **Step 1: Write the failing tests**
  - `schedule_test.go` (imports `_ "time/tzdata"`): `TestNextRun` in `America/Los_Angeles`: 02:00 → same day 03:00; exactly 03:00 → next day 03:00; 23:30 → next day 03:00; `at` 00:00 at 23:59 → next midnight; spring-forward day 2026-03-08 with `at` 02:30 from 01:00 → a time on 2026-03-08 after `now`; fall-back day 2026-11-01 with `at` 01:30 from 00:00 → 2026-11-01, after `now`.
  - `TestNightlyRunsScanThenBackup`: `s.after` records the duration and returns a test channel; `Start`; fire it → wait for `Last.Trigger == "nightly"` with `Last.Backup.Error == ""` and the file `catalog-<today>.db` in the backup folder; the loop asks `after` again; `Close` returns.
  - `TestSettingsChangedReschedules`: `SetSetting(archive_rescan_at, "05:15")`, `SettingsChanged()` → the next `after` duration targets 05:15 and `Status().NextScan` shows it.
  - `TestNightlyWaitsForManualPass`: a manual pass blocked in `beforeRead`; fire the nightly timer; unblock → two passes recorded in order (manual, then nightly with a backup).
  - `backup_test.go` `TestBackupWritesAndPrunes`: backup folder holds `catalog-2026-10-01.db` … 16 such files (dates before today), `notes.txt`, `catalog-old.db`, `catalog-2026-10-01.db.bak` → after a backup: today's file opens with `catalog.Open` (version 3, the test's asset is there); exactly 14 `catalog-YYYY-MM-DD.db` files remain (today plus the 13 newest old ones); `Removed` names the 3 oldest; the other files untouched; no `.partial` left. A second backup the same day replaces the file (no error).
  - `TestBackupFailureKeepsOldBackups`: (a) `backup_dir` names an existing regular file → `Error` set, nothing written; (b) a folder holding 16 old backups plus a non-empty **directory** named `catalog-<today>.db.partial` (so removing the stale partial fails on Windows and Linux alike) → `Error` set and all 16 old files still there.
  - `TestCheckBackupDir`: `<photos>/Archive`, `<photos>/Archive/x`, `<photos>/Staging/y` → `ErrBackupInside`; `<photos>/Management/SD2NAS`, `<photos>/ArchiveBackups` → nil.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/indexer/ -run 'NextRun|Nightly|Reschedule|Backup'`.
- [ ] **Step 3: Implement**; `Stats.BackupDir` now uses `BackupDir(set)`.
- [ ] **Step 4: Run**: `go test ./internal/indexer/` → PASS; Linux command (TZ `Pacific/Chatham`) and race command → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/indexer/
git commit -m "Rescan the archive nightly and back up the catalog afterwards

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: API for the archive index and settings; `indexer.progress`; wiring in main

Suggested implementer: sonnet.

**Files:**
- Create: `internal/api/archive.go`, `internal/api/settings.go`, `internal/api/archive_test.go`, `internal/api/settings_test.go`
- Modify: `internal/api/events.go`, `internal/api/full_test.go` (add the indexer to `fullServer`), `cmd/sd2nas/main.go`

**Interfaces:**
- Consumes: `indexer.Service` (Tasks 9–10), `indexer.CheckBackupDir`, `catalog.SettingKeys`/`SetSettings`/`SettingError`/`Settings.Strings`, the existing `decodeBody`, `writeJSON`, `writeError`, `opContext`.
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
- `GET /api/archive/stats` → `idx.Stats(r.Context())` (500 with the error logged on failure).
- `POST /api/archive/rescan` → 202 `{"started": idx.StartRescan()}`.
- `GET /api/archive/changed` → `idx.Changes`.
- `POST /api/archive/changed/accept`: `decodeBody` into `struct{ UpTo *int64 \`json:"upTo"\` }` (64 KiB); nil or < 1 → 400 `upTo must be a positive whole number.`; `idx.Accept(opContext(r), *UpTo)` → 200 `{"accepted": n}`.
- Events: `s.hub.AddSnapshot(func() (string, any, error) { return EventIndexerProgress, idx.Status(), nil })` and `idx.OnChange(func(st indexer.Status) { publish, log on error })` — the callback only publishes (lock-order rule). `s.closers = append(s.closers, idx.Close)`.
- `GET /api/settings`: `db.Settings` → `Strings()`, with `backup_dir` replaced by `idx.BackupDir(set)` → `{"settings": …}`.
- `PUT /api/settings`: `decodeBody` into `map[string]any` (64 KiB); empty → `Name at least one setting.`; for each key in sorted order: not in `SettingKeys()` → `Unknown setting <key>.`; not a string → `Setting <key> must be a string.`; `backup_dir` that is non-empty and fails `indexer.CheckBackupDir(photosRoot, v)` → `Setting backup_dir: pick a folder outside Archive/ and Staging/.`. Then `db.SetSettings(opContext(r), kv)`: a `*catalog.SettingError` → `Setting <key>: <reason>.`; other errors → 500. Success → `idx.SettingsChanged()` and the GET body.
- The 503 after shutdown follows the existing pattern (`indexer.ErrClosed` → `SD2NAS is shutting down`).

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
			idx.Start() // nightly rescan and backup; nothing runs at startup (Decision 14)
		}
```

- [ ] **Step 1: Write the failing tests**
  - `archive_test.go`: `TestArchiveDirsMatchStaging` (`indexer.ArchiveDir == staging.ArchiveDir`, `indexer.StagingDir == staging.Dir`); `TestArchiveStatsAPI` (write two files under `<photos>/Archive/S/`, `POST /api/archive/rescan` → 202 `{"started":true}`, poll stats until `running` is null → `files` 2, `last.added` 2, `rescan_at` `03:00`, `backup_dir` ends in `Management/SD2NAS` (OS separators), `missing_files` and `recent` are `[]` not null); `TestArchiveRescanBusy` (the test's `Config.Busy` returns true, so the first pass pauses before its first read: a second `POST` → `{"started":false}` and stats show `running.paused` true; flip `Busy` to false → the pass finishes); `TestArchiveChangedAndAccept` (a change made through a rescan → `GET …/changed` has 1 and `latest_id`; accept `{"upTo": latest}` → `{"accepted":1}`; `{}`, `{"upTo":0}`, `{"upTo":"x"}`, `{"upTo":1,"x":1}` → 400 with the exact sentences (`decodeBody`'s for the last two)); `TestIndexerProgressEvent` (the SSE stream's first frames include `event: indexer.progress` with `"version"`; a rescan publishes a frame with `"running":{` and a later one with `"running":null`).
  - `settings_test.go` `TestSettingsAPI`: GET → all six keys, `archive_rescan_at` `03:00`, `backup_dir` effective; PUT `{"archive_rescan_at":"04:45"}` → 200 with it, and stats `rescan_at` `04:45`; PUT without `X-SD2NAS` → 403; PUT `{}` / `{"nope":"1"}` / `{"archive_rescan_at":5}` / `{"archive_rescan_at":"25:00"}` / `{"backup_dir":"<photos>/Archive/b"}` / `{"backup_dir":"relative"}` → 400 with the exact sentences; a PUT with one good and one bad key stores neither.
- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/api/ -run 'Archive|Indexer|Settings'`.
- [ ] **Step 3: Implement** and wire `main.go`.
- [ ] **Step 4: Run**: `go test ./internal/api/ ./cmd/...` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, Linux and race commands → PASS. Run the binary on temp dirs (`go run ./cmd/sd2nas --cards ./testdata/cards --photos <tmp>/photos --config <tmp>/config`), `curl -s localhost:8080/api/archive/stats` answers, `docker`-free `Ctrl+C` exits cleanly.
- [ ] **Step 5: Commit**

```bash
git add internal/api/ cmd/sd2nas/main.go
git commit -m "Serve the archive index, settings and indexer progress over the API

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: Web logic for the archive page (node tests)

Suggested implementer: haiku (all strings below are exact).

**Files:**
- Create: `web/src/lib/archive.js`, `web/src/lib/archive.test.js`
- Modify: `web/src/lib/messages.js`, `web/src/lib/messages.test.js`, `web/src/lib/live.svelte.js`

**Interfaces:**
- Consumes: `formatBytes`, `formatCount`, `formatWhen` (`format.js`), `plural`, `joinList` (`messages.js`).
- Produces:

```js
// archive.js
export function scanPercent(p)       // null while listing or with no progress; 0–99 while hashing; 100 for checking/backup
export function scanLine(p)          // the text under the progress bar
export function scanSummary(r)       // the toast after a manual rescan
export function sizeChange(oldSize, size)
export function lastScanLabel(last)  // 'Never' or formatWhen(last.finished_at)
export function backupLine(b)
// messages.js
export function missingCopies(n)
// live.svelte.js: live.indexer = { version: 0, running: null, last: null, changed: 0, missing: 0, next_scan: null }, updated on 'indexer.progress'
```

Exact outputs:
- `scanPercent`: `null` for `null` or phase `listing`; phase `hashing`: `Math.min(99, Math.floor(100 * bytes_hashed / bytes_to_hash))` when `bytes_to_hash > 0`, else by `hashed / to_hash`, else 0; phases `checking` and `backup`: 100.
- `scanLine`: `''` for null; paused → `Paused while an import or promote runs. It carries on by itself.`; `listing` → `Rescanning… looking for files (${formatCount(listed)} found so far)`; `hashing` → `Rescanning… ${pct}% (only files whose size or modified time changed get re-hashed)` (the mockup's sentence); `checking` → `Rescanning… checking for files that are gone`; `backup` → `Backing up the catalog…`.
- `scanSummary`: `skipped` → `Rescan skipped: ${skipped}`; `stopped` → `Rescan stopped before it finished. It carries on at the next scan.`; else the parts in this order when non-zero: `N new` (added), `N moved`, `N changed`, `N back` (reappeared), `N recovered`, `N missing`, `N duplicates` (copies, `1 duplicate`) → `Rescan complete: ${joinList(parts)}.` or `Rescan complete: no changes.`; then ` ${plural(busy_folders, 'folder was', 'folders were')} being promoted and will be scanned next time.` and ` ${plural(errors.length, 'problem', 'problems')}: ${errors[0]}` when present.
- `sizeChange(1000, 1000)` → `same size`; `(1000, 13300)` → `+12.3 KB`; `(13300, 1000)` → `-12.3 KB`.
- `backupLine`: null → `No catalog backup yet.`; error → `The last catalog backup failed: ${error}`; else `Last catalog backup ${formatWhen(at)}, ${formatBytes(bytes)}.`
- `missingCopies(1)` → `1 file's archive copy is missing`; `missingCopies(2)` → `2 files' archive copies are missing`; `missingCopies(1234)` → `1,234 files' archive copies are missing` (same text as the server's).

- [ ] **Step 1: Write the failing tests** for every output above (including `scanPercent` clamping at 99 with `bytes_hashed == bytes_to_hash`, and `to_hash`-only progress).
- [ ] **Step 2: Run them and confirm they fail**: `cd web && npm test`.
- [ ] **Step 3: Implement**, and in `live.svelte.js` add the `indexer` field and `source.addEventListener('indexer.progress', (e) => { live.indexer = JSON.parse(e.data); })`.
- [ ] **Step 4: Run**: `cd web && npm test && npm run build` → PASS; `git status --short web/dist` empty.
- [ ] **Step 5: Commit**

```bash
git add web/src/lib/
git commit -m "Add the web logic for the archive index page

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 13: Archive index page, nav pill, missing copies on the import page

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/pages/Archive.svelte`
- Modify: `web/src/App.svelte`, `web/src/components/Nav.svelte`, `web/src/pages/IngestDetail.svelte`

**Interfaces:**
- Consumes: Task 11's endpoints, Task 12's helpers, `api`, `live`, `toast`, `describeError`.
- Produces: route `/archive` (`{ tab: 'archive', view: 'archive' }`).

Page (`Archive.svelte`), following `docs/ui-mockup.html` `renderArchive()`:
- **Data**: `load()` fetches `/api/archive/stats` and, when `stats.changed > 0`, `/api/archive/changed`, with one AbortController (a newer load aborts the older); keeps the last good `stats`/`changes` and shows `Couldn't refresh the archive index: <error>` above them on a refetch failure. Loads on mount, after a real reconnect, and when `live.indexer.version !== stats.version`; after a load whose `version` differs from `live.indexer.version` (and the live one is non-zero), load once more. Progress comes from `live.indexer.running` (not from the stats).
- **Header**: `<h1>Archive index</h1>` and `<p class="sub">Hash catalog of <span class="mono">{archive_path}</span>, used to skip files you already have.</p>`. When `archive_present` is false: `note warn` `Archive/ isn't there. Is the photos share mounted?`.
- **Stats card**: four `.stat`s — files indexed, archive size (`formatBytes(bytes)`), files in ingest history (`imported`), last scan (`lastScanLabel(last)`). Under them: while running, `.bar` with `scanPercent` width (an indeterminate style while `null`) and `scanLine`; otherwise a **Rescan now** button (`POST /api/archive/rescan`; `started: false` → toast `A scan is already running.`). A muted line: `Next scan {formatWhen(next_scan)} · catalog backups in <span class="mono">{backup_dir}</span>` and `backupLine(last?.backup)` (red when it has an error). When `last.stopped`: `note warn` `The last scan didn't finish. It carries on at the next nightly scan, or now with Rescan now.` Problems of the last scan in a `<details>` (`N problems in the last scan`, up to 20 lines plus `…and N more` from `errors_omitted`).
- **Toast**: after a rescan this page started finishes (the `live.indexer.last.finished_at` changes while `rescanAsked`), toast `scanSummary(last)` (`warn` when skipped, stopped or with problems).
- **Missing card** (when `missing > 0`): `<h2>⚠ {plural(missing, 'file is', 'files are')} missing from the archive</h2>`, the text `SD2NAS indexed these files, but they're no longer in the archive. Cards that still hold them won't be reported as safe to format. If you moved them, the next scan finds them.` and the list of `missing_files` (`mono` path, size) with `…and N more` beyond 50.
- **Changed card** (when `changed > 0`): `<h2>⚠ {plural(changed, 'archive file', 'archive files')} changed since they were indexed</h2>`, `Same path, different content. Usually an editor writing metadata into the file.`; a table **File | Last indexed | Size change** on desktop (`formatWhen(indexed_at)` date part, `sizeChange(old_size, size)`), stacked rows on a phone; `…and N more` when `total > changes.length`; button **Accept new hashes** (primary, ≥ 44 px) → `POST /api/archive/changed/accept {"upTo": latest_id}` → toast `Marked as reviewed. The old hashes stay as aliases, so the originals on a card still count as duplicates.` → `load()`; beside it the muted line `The old hash is kept as an alias, so the original file from a card still counts as a duplicate.` Errors → `describeError` in a `note bad`.
- **Recently promoted card** (when `recent` is non-empty): table **Folder | Files | Promoted** (`folder` without the `Archive/` prefix as the name, the full path muted mono on desktop; `formatWhen(at)` date part).
- **Layout**: cards stack; `.stats` 4 columns on desktop, 2 on a phone; tables become stacked rows at `max-width: 760px` (no horizontal scroll at 375 px: `overflow-wrap: anywhere` on paths); buttons full width on a phone; all tap targets ≥ 44 px.

Nav: an **Archive index** tab (`href="/archive"`, an archive-box icon in the same SVG style) after Staging, with `{#if live.indexer.changed}<span class="pill warn">{live.indexer.changed}</span>{/if}` (the mockup's badge); on a phone the pill sits like the ingest one.

`IngestDetail.svelte`: `{#if c.missing}<div class="stat bad"><b>{formatCount(c.missing)}</b>archive copies missing</div>{/if}` in the stats, and before the failed list an open `<details>` with summary `{missingCopies(missing.length)}`, the text `SD2NAS knows these files, but their copies in the archive are gone, so the card isn't safe to format. Keep it until you've found or restored them; the Archive index page lists the missing files.` and rows `card_path → rel_path` (the `rel_path` is where the copy used to be).

- [ ] **Step 1: Implement** the page, the route, the nav tab and the import-page additions.
- [ ] **Step 2: Build**: `cd web && npm test && npm run build` → PASS, zero Svelte warnings; `git status --short web/dist` empty; `SD2NAS_REQUIRE_UI=1 go test ./web/` → PASS.
- [ ] **Step 3: Manual check** (record what was checked in the report): run the binary against a temp `photos` with `Archive/2025-05-30 - Trip/{a.JPG, Capture/b.ARW, Selects/c.JPG, Output/d.JPG, 2025-05-30 - Trip.cosessiondb}` and `Archive/2007-09-31 - Random Party/e.JPG` (four distinct files for a, b, c, e — fixture bytes from `testdata/cards` or any distinct content with those extensions), open `/archive` in the browser pane at desktop width and at 375×812: Rescan now shows progress then the toast (`Rescan complete: 4 new.`), the stats fill in; edit `a.JPG`'s bytes and rescan → the nav pill shows 1 and the changed card lists it; Accept clears it; delete `c.JPG` and rescan → the missing card shows it; no horizontal scroll, tab bar not covered, buttons ≥ 44 px; reset the viewport afterwards.
- [ ] **Step 4: Commit**

```bash
git add web/src/
git commit -m "Add the Archive index page with rescan, changed files and missing files

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: PLAN.md: tick M5, record the decisions; final verification

Suggested implementer: haiku.

**Files:**
- Modify: `docs/PLAN.md`

**Interfaces:**
- Consumes: this plan's Decisions.
- Produces: documentation only.

- [ ] **Step 1: Edit `docs/PLAN.md`**
1. In §16 **M5**, tick every item except `Backfill run against the real archive` (the owner's, on the NAS) and add under the list:

```markdown
Implementation plan: `docs/plans/m5-archive-indexer.md`. One walker serves
the backfill and the nightly rescan: the first pass is a rescan over an
empty index, every file is committed as it is read, so an interrupted pass
carries on where it stopped. A pass lists `Archive/` (stat only), then reads
only new files and files whose size or modified time changed, one at a
time, pausing while an import or promote runs. Moves are found by quick
fingerprint and size (no full read); the same content at a second archive
path is recorded in `archive_copies`; a file changed in place gets its new
hash at once, the old one becomes an alias and the path is listed under
"changed since indexed" until accepted (accepting only clears the list).
Only a pass that ran to the end marks files `missing`, each after
re-checking that it and `Archive/` are still there; folders a promote holds
are skipped and picked up next time; staged content found in the archive
is left alone and culled content becomes archived. Imports give files whose
archive copy is missing the outcome `missing`, which blocks safe to format
("N files' archive copies are missing"). Everything below a session folder
is indexed except `Output/`, `CaptureOne/`, `*.cosessiondb` and NAS
metadata; only import formats count. Nothing runs at startup: the first
nightly run (`archive_rescan_at`, default 03:00 container time) is the
backfill, or press "Rescan now". After the nightly pass the catalog is
backed up with `VACUUM INTO` to `backup_dir` (default
`/photos/Management/SD2NAS`), keeping 14. Schema version 3. Settings are
read and changed with `GET/PUT /api/settings` until M7's page, e.g.
`curl -s -X PUT -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' http://localhost:4040/api/settings -d '{"archive_rescan_at":"02:30"}'`.
**To run the backfill:** after updating, either wait for 03:00 or open
Archive index → Rescan now; the page shows progress, and the container log
has an `archive scan` line when it finishes. Tick the item with the file
count and how long it took.
```

2. In §6 after the **Aliases** paragraph, add: ``Schema version 3 adds the `missing` ingest outcome (`ingests.missing`), `archive_copies` (other archive paths holding an asset's content), `archive_changes` ("changed since indexed") and `archive_scans` (the indexer's history). A change in place moves the old fingerprints, aliases and ingest history to the new hash.``
3. In §9, after "Moves are recognized", add: ``A copy of archived content at a second path is remembered as a copy and not re-read; when the primary path goes, the asset moves to the copy.``; after the **Nightly rescan** list add: ``"Changed since indexed" entries are already applied when listed; accepting them clears the list.``
4. In §10, change the event sentence to end ``…; `indexer.progress` (M5) carries the indexer status `{version, running, last, changed, missing, next_scan}`.`` and note under the endpoint block: ```GET/PUT /api/settings` (M5) read and change the catalog settings as strings; no settings page until M7.``
5. In §3 **Catalog backup**, after "The folder is a setting.", add: ``It is `backup_dir` (empty = `/photos/Management/SD2NAS`; never inside `Archive/` or `Staging/`); a failed backup is shown on the Archive index page and deletes nothing.``
6. Add these follow-ups as unchecked items:
   - Under **M6**: ``- [ ] Notify when the nightly catalog backup fails or the nightly rescan finds missing files``
   - Under **M7**: extend the settings item to ``- [ ] Settings page: `staging_warn_days`, the default promote pattern (now remembered per device), `day_starts_at`, `archive_rescan_at`, `backup_dir` `` and add ``- [ ] Offer to re-import files whose archive copy is missing (imports now list them and refuse safe to format)``
7. Record that F6–F14 from the M4 review remain open (not in code M5 changed) only in the final report, not in PLAN.md.

- [ ] **Step 2: Check CI and Docker need nothing**: `ci.yml` already runs gofmt, vet, `go test` with the tools, `npm ci/test/build` and `go test ./web/` with `SD2NAS_REQUIRE_UI=1`; the Dockerfile copies `internal/` and `web/` as a whole. If anything needs changing, stop and report instead of editing `.github/` or `deploy/`.

- [ ] **Step 3: Final verification of the whole branch** — run each and paste the tail of its output in the report:
- `gofmt -l .` prints nothing; `go vet ./...` and `go test ./...` pass on Windows; the Linux and race commands pass.
- `go list -f '{{join .Imports "\n"}}{{"\n"}}{{join .TestImports "\n"}}' ./internal/indexer/ | grep -E 'internal/(staging|ingest|api)'` prints nothing.
- In `web/`: `npm ci && npm test && npm run build`, then `SD2NAS_REQUIRE_UI=1 go test ./web/ -v` passes.
- Image smoke run: `docker build -f deploy/Dockerfile -t sd2nas:m5 .`; run with `testdata/cards` read-only at `/cards`, a temp `/photos` holding `Archive/2025-05-30 - Trip/` (fixture JPG/ARW at the top level and in `Capture/`, `Selects/`, an `Output/` copy and a `.cosessiondb`), a temp `/config`, `-e TZ=America/Los_Angeles`, `-p 18080:8080`; `POST /api/archive/rescan` with `X-SD2NAS: 1` → 202; `GET /api/archive/stats` soon shows `running: null`, `last.added` = the media count outside `Output/`; put a byte-copy of one `SONY_A7IV` card file into the archive folder, rescan, then delete it and rescan again (`missing` 1); `POST /api/ingest?wait=1&format=text` with `{"path":"/cards/SONY_A7IV"}` → the message contains `1 file's archive copy is missing. Do not format the card.`; `PUT /api/settings {"archive_rescan_at":"04:00"}` → 200 and stats `next_scan` at 04:00; `docker stop` exits within 2 s. Remove the container and image afterwards.
- `git status --short` shows only `HANDOFF.md` untracked.
- `git log --format=%B main..HEAD | grep -c 'Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>'` equals `git rev-list --count main..HEAD`, and `git log --format=%B main..HEAD | grep -ci 'co-authored-by'` equals the same number.

- [ ] **Step 4: Commit**

```bash
git add docs/PLAN.md
git commit -m "Tick M5 in the plan and record its decisions and follow-ups

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
