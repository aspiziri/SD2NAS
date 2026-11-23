# M4 Staging Operations Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Turn the read-only staging UI into the place where imports get sorted: rename, merge, move files, cull, discard and promote staging folders into `Archive/`, forget an import (to retest a card) or re-import culled files, keep the catalog in step with changes made over SMB, warn about folders that sit in staging too long, and do all of it comfortably by mouse on a desktop and by touch on a phone.

**Architecture:** `internal/staging` gains the operations. `Locks` (per staging folder) is shared with `internal/ingest`: an import holds the lock of every folder it writes into until it ends, staging operations take locks without waiting (`409 Conflict` when busy), and an import waits for a folder an operation holds. Every operation changes the filesystem first and records each step in the catalog right after it, and a reconcile pass (at startup and on demand) repairs anything a crash or an SMB edit left behind. Promote runs as a background job that renames folders into `Archive/` (falling back to copy, verify, delete across filesystems) and reports progress. `internal/catalog` gets schema version 2 (`ingests.forgotten_at`) and the write queries. `internal/api` gets the write endpoints, the `staging.changed` and `promote.progress` events and maps busy folders to 409. The web app gets selection (click, shift-click, checkboxes on desktop; long-press and swipe on touch), a selection bar, dialogs with confirmations, promote progress, a rescan button, age warnings and the forget / re-import actions.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `net/http`, `log/slog`, `modernc.org/sqlite`; Svelte 5.59.1 (runes), Vite 8.3.8, Node 24. No new dependencies.

**Spec:** `docs/PLAN.md` §3 (age warnings, capacity), §6 (catalog; `internal/catalog/schema.go` is authoritative; "Culled stays remembered", "Re-import anyway"), §7 (ingest takes the folder lock while writing), §8 (operations table, folder locks, naming `YYYY-MM-DD - Title`, promote pattern, Capture One layout, EXDEV, confirmations), §10 (endpoints, events, request guard), §13 (same share for promote), §16 M4 checklist. UI design: `docs/ui-mockup.html` (staging list, folder grid, selection toolbar, rename/merge/discard/promote/cull dialogs). Template and conventions: `docs/plans/m3-web-ui.md`.

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`, `go 1.24.0` in `go.mod` with no `toolchain` line. **Use no API newer than Go 1.24**: no `sync.WaitGroup.Go`, `testing/synctest`, `T.Output`, `os.Root` methods added in 1.25. Fine: `context.WithoutCancel`, `min`/`max`, `slices`, `maps`, `t.Context()`, `slog.DiscardHandler`, `omitzero`, `range` over an int, `filepath.IsLocal`.
- **No new dependencies**, Go or npm. `go.mod`, `go.sum`, `web/package.json` and `web/package-lock.json` must not change. No router, gesture/selection library, test framework or CSS framework. Frontend tests use the built-in `node --test` on plain JS in `web/src/lib/*.js`. npm commands run **only inside `web/`**.
- M4 touches: `internal/catalog/` (schema v2, new queries), `internal/staging/` (operations, locks, promote, reconcile, forget), `internal/ingest/` (`run.go` takes folder locks, `manager.go` gains `Exclusive`, `partials.go` tolerates folders vanishing mid-walk), `internal/thumbs/thumbs.go` (`Remove`), `internal/api/`, `cmd/sd2nas/main.go`, `web/src/**`, and `docs/PLAN.md` (Task 18 only). Do not modify `internal/media/`, `internal/config/`, `.github/`, `deploy/` (Task 18 checks they need nothing). **`internal/staging` must never import `internal/ingest`** (ingest imports staging for the locks); staging's internal (`package staging`) tests can't either.
- **Binding values** (reviewers check them verbatim):
  - Endpoints (request field names camelCase as in §10, response fields snake_case like M3):
    - `PATCH /api/staging/{id}` `{"title": string}` (required; `""` drops the title) → 200 `{"folder": Folder}`.
    - `POST /api/staging/merge` `{"ids": [int…], "title": string}` (≥ 2 distinct ids) → 200 `{"folder": Folder}`.
    - `POST /api/staging/move` `{"fileShas": [hex…], "toId": int}` **or** `{"fileShas": […], "newTitle": string}` (exactly one of the two) → 200 `{"folder": Folder, "moved": int, "removed": [id…]}`.
    - `POST /api/staging/cull` `{"fileShas": [hex…]}` → 200 `{"culled": int, "removed": [id…]}`.
    - `DELETE /api/staging/{id}` → 200 `{"culled": int, "removed": bool, "kept": [path…]}`.
    - `POST /api/staging/promote` `{"ids": [int…], "pattern": string}` (`""` = `{name}`) → 202 `{"job": PromoteJob}`. `GET /api/staging/promote` → 200 `{"job": PromoteJob|null}` (the running or last job).
    - `POST /api/staging/rescan` → 202 `{"started": bool}` (false: one was already running).
    - `GET /api/ingest/{id}/forget` → 200 ForgetSummary. `POST /api/ingest/{id}/forget` `{"deleteFiles": bool}` (required) → 200 `{"result": ForgetResult, "ingest": ingestView}`. `POST /api/ingest/{id}/reimport-culled` → 200 `{"forgotten": int, "started": ingestView|null, "message": string}`.
    - `fileShas` holds at most **20000** hashes (`staging.MaxFiles`); request bodies at most 4 MiB for move/cull, 64 KiB otherwise; unknown JSON fields → 400.
    - Errors are JSON `{"error": msg, "code"?: string, "folder_id"?: int, "holder"?: string}` with `Cache-Control: no-store`. 409 codes: `folder_busy` (with `holder` = `ingest` | `operation` | `rescan` and `folder_id`), `name_taken`, `changed`, `promote_running`, `ingest_running`, `already_forgotten`. 400 for bad input (bad title, bad pattern, bad hashes, files not in staging), 404 for an unknown folder or ingest, 503 `shutting down`. The busy messages are exactly: `<name> is being imported into. Try again when the import finishes.` / `<name> is busy with another change. Try again in a moment.` / `<name> is being rescanned. Try again in a moment.`
    - `GET /api/staging` adds `version`, `stale_after_days`, `stale` (count) to the listing; Folder adds `age_days` and `stale`; `GET /api/staging/{id}/files` adds `version`. The ingest view adds `forgotten_at` (omitted when never forgotten).
  - Events on `/api/events` (both also sent as a snapshot on connect): `staging.changed` data `{"version": uint, "rescanning": bool, "last_rescan": RescanResult|null}`, published after every change (operation, rescan, finished ingest); `promote.progress` data = PromoteJob or `null`, published at most every **250 ms** plus at every folder and job state change.
  - Locks: an import holds the lock of each staging folder it writes into **until the import ends**; staging operations take all their locks at once or fail immediately (`*staging.BusyError`, never wait); the import waits for a folder an operation holds and looks the folder up again once it has the lock; the rescan skips busy folders.
  - Folder names: `YYYY-MM-DD - Title`; a title is trimmed and must not contain `/ \ < > : " | ? *` or control characters, start with `.`, end with `.` or a space, or exceed **200** bytes (`staging.MaxNameBytes`). Promote pattern tokens `{name}`, `{year}`, `{month}`; default `{name}`; the result lives under `Archive/`, and every path element obeys the same rules.
  - Junk files that never keep a folder alive (deleted when the folder is otherwise empty): `.DS_Store`, `Thumbs.db`, `desktop.ini` (case-insensitive), `._*`, `*.partial`.
  - Catalog: `SchemaVersion = 2`; migration 2 adds `ingests.forgotten_at TEXT` and `CREATE INDEX assets_first_ingest ON assets(first_ingest)`. Setting `staging_warn_days`, default **30**, `0` = off, at most 3650.
  - UI: long press **450 ms** with **10 px** slop; phone breakpoint `max-width: 760px`; tap targets ≥ 44 px; sticky elements use `top: env(safe-area-inset-top)`; the phone selection bar sits above the tab bar at `bottom: calc(var(--tabbar) + env(safe-area-inset-bottom))`.
- **Responsive, not phone-only (owner requirement).** Every page and dialog must work well on a desktop (mouse, keyboard: click opens, Ctrl/⌘-click and the checkbox toggle, Shift-click selects a range, Esc clears, Ctrl/⌘-A selects all, multi-column grids) and on a phone (touch: tap opens, long-press starts selecting, then taps toggle; in the file grid a long-press followed by a drag selects every file the finger crosses; bottom selection bar; full-width dialogs). Reviewers check both at 375×812 and at desktop width.
- Svelte 5 runes only (`$state`, `$derived`, `$effect`, `$props`), no `{@html}`, no `export let`. Plain CSS (tokens in `web/src/app.css`, component styles scoped). Logic worth testing (selection, naming, messages) lives in `web/src/lib/*.js` with `*.test.js` beside it. Follow M3's live-page pattern: one AbortController per load, keep the last good data on screen and show a refetch failure above it, refetch after a real reconnect.
- **Filesystem first, then the catalog, after every step.** Never delete a file except to cull, discard or forget it (with `deleteFiles`), or after a verified copy. Never overwrite: every move goes through `media.FreeName` (CameraImport's collision loop). Mutating handlers run with `context.WithoutCancel(r.Context())`, so a phone going to sleep can't stop an operation half way.
- SQLite has **one connection**: never call a `catalog.DB` method while a `*sql.Rows` from it is open, and inside `d.tx(…)` use only `tx`. Catalog methods return slices, not iterators.
- Tests pass on Windows and Linux. Use `path/filepath` for OS paths and `path` for slash-separated `rel_path` values. Close every file, `*sql.Rows`, `*catalog.DB` and `*staging.Service`. In tests, call `t.TempDir()` before registering a `Close` cleanup. Tests that wait use a 10 s deadline and fail clearly; sleeps only inside polling loops, or for the few negative checks this plan names (each ≤ 300 ms).
- **Commands.** Windows (Git Bash, repo root): `gofmt -l .` (prints nothing), `go vet ./...`, `go test ./...`. Linux (Docker Desktop running):
  `MSYS_NO_PATHCONV=1 docker run --rm -e TZ=Pacific/Chatham -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q exiftool ffmpeg && go vet ./... && go test ./...'`
  Race (Tasks 3, 7, 8, 10, 11, 12): `MSYS_NO_PATHCONV=1 docker run --rm -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q build-base exiftool ffmpeg && CGO_ENABLED=1 go test -race ./internal/... ./web/...'`
  Web (in `web/`): `npm test`, `npm run build` (then `git status --short web/dist` must show nothing: only `.placeholder` is tracked and it survives).
- Never silently swallow an error: return it wrapped with `%w`, answer the request with it, or log it with `slog` (key `"err"`). Exceptions: a failed write to an HTTP client that has gone, and `localStorage` being unavailable in the browser (private mode; commented).
- Export only the identifiers listed in each task's **Produces** block. Unexported helpers are free unless a later task's **Consumes** names them.
- Commit on branch `m4-staging-ops`, one commit per task (more is fine), with the message given in the task (an imperative sentence in the repo's style). Every message ends with a blank line and then exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>` — no other trailer, no other model name. Never commit `HANDOFF.md`, `tmp/`, `web/node_modules/` or anything in `web/dist/` other than `.placeholder`. Never push.

## Review Focus

1. **Locks and the running import.** An operation on a folder an import is filling must answer 409 with holder `ingest` for the whole import, an import must wait (not fail) for a folder an operation holds, and must write into the folder's *current* name after the wait (it may have been renamed, merged away or promoted). Every lock must be released on every path (errors, cancellation, panic-free early returns), and nothing may deadlock: only the import waits while holding locks; operations never wait; the rescan skips. Pinned in Task 3 (`TestTryLockAllOrNothing`, `TestLockWaitsForRelease`, `TestLocksConcurrent`, `TestRunHoldsFolderLocks`, `TestRunWaitsForBusyFolder`, `TestRunFollowsFolderRenamedWhileWaiting`, `TestRunCancelledWhileWaiting`), Task 5 (`TestRenameBusy`), Task 7 (`TestPromoteReleasesEachFolder`), Task 11 (`TestStagingRenameBusy409`).
2. **Never lose or overwrite a file.** Moves use the collision loop; identical content already at the destination removes only the source; a cross-device move deletes the original only after re-hashing the copy and matching the catalog hash; files SD2NAS doesn't track (sidecars, notes, media not yet rescanned) are moved along by merge and promote and never deleted by discard or cull. Pinned in Task 4 (`TestMoveFileCollision`, `TestMoveFileIdentical`, `TestMoveFileCrossDevice`, `TestMoveFileCrossDeviceMismatch`, `TestRemoveEmptyTree`), Task 5 (`TestMergeMovesFilesAndRemovesSources`), Task 6 (`TestDiscardKeepsUntrackedFiles`), Task 7 (`TestPromoteMergesIntoExistingTarget`, `TestPromoteCrossDeviceVerifyFails`).
3. **Catalog and disk agree after partial failures, crashes and SMB edits.** Each filesystem step is recorded right after it; a rescan adopts untracked folders and media, follows files moved by hand, culls files deleted by hand, restores culled content put back, drops rows whose folder is gone, and does nothing at all when `Staging/` is missing (an unmounted share must not cull everything). Pinned in Task 8 (`TestReconcile*`, especially `TestReconcileStagingMissing`, `TestReconcileFollowsMoveBetweenFolders`, `TestReconcileSkipsFileBeingWritten`).
4. **Odd names.** Non-ASCII folder names (SQLite `substr`/`length` count characters, Go counts bytes), the prefix trap (`2025-05-30` vs `2025-05-30 - Trip`), SMB-illegal characters, a case-only rename on a case-insensitive disk, a title that would collide with another folder or an untracked directory. Pinned in Task 2 (`TestRenameStagingFolderRewritesPaths`), Task 4 (`TestCleanTitle`, `TestExpandPattern`), Task 5 (`TestRenameNameTaken`, `TestRenameNonASCII`), Task 13 (`naming.test.js`, which mirrors the Go rules).
5. **Promote.** Instant rename into `Archive/<pattern>` when the target doesn't exist; file-by-file merge with the collision loop when it does (leaving Capture One's own files alone); EXDEV → copy, verify, delete; the staging row dropped and assets `archived` with new paths; tracked files missing on disk culled first; one job at a time; folders released as each finishes; shutdown stops the job between files and leaves sources intact. Pinned in Task 7 (`TestPromote*`).
6. **Forget and re-import.** `archived` rows are never forgotten; refused while any import runs or waits (and imports wait while a forget runs); fingerprints, aliases and clock warnings of forgotten assets go too, so the same card imports them again; the ingest stays in the history marked forgotten. Pinned in Task 10 (`TestForgetAssets`, `TestManagerExclusive`, `TestForget*`) and Task 12 (`TestForgetAPIRetestsCard`, `TestReimportCulledAPI`).
7. **Events.** `staging.changed` carries a version the pages compare with the `version` in their data, so a missed event plus a reconnect never leaves stale data, and the number of events per operation is small (no subscriber is dropped by a 2,000-file promote). Pinned in Task 7 (`TestPromoteProgressThrottled`), Task 11 (`TestStagingChangedEvent`), Task 12 (`TestPromoteAPI`).
8. **Selection on both input types.** Desktop: click opens, Ctrl/⌘-click and the checkbox toggle, Shift-click ranges, Esc clears. Phone: long-press starts selecting (no iOS callout or text selection), taps then toggle, a long-press-and-drag in the file grid selects a range without scrolling the page (auto-scroll near the edges), and the click a long press produces is swallowed. The selection bar never covers the last row or the tab bar. Pinned in Task 13 (`selection.test.js`) and the manual checklist in Task 16, Step 5.

## Decisions

1. **An import holds its folder locks until it ends.** §8 says an operation on "a folder that a card ingest is currently filling" gets 409. Locking per file would make that a coin toss (the gap between two copies is short), so the import keeps each folder it has written into locked until it finishes. Operations never wait (409 at once, so a tap never hangs); the import waits for an operation's folder and then looks its row up again, because the operation may have renamed, merged or promoted it. The rescan skips busy folders and reports how many. Only the import waits while holding locks, so there is no deadlock cycle.
2. **Locks live in `internal/staging`** (PLAN §4), and `internal/ingest` imports staging for them (`ingest.Config.Locks`, shared with `staging.Config.Locks` in `main`). Staging imports neither ingest nor the reverse cycle.
3. **Merge and promote move every file, catalogued or not, keeping its path inside the folder** (`Capture/x.ARW` stays in `Capture/`), so CameraImport's layout (top-level JPGs, `Capture/`, `Videos/`) and any sidecar or note the owner added survive. Move files moves only the selected (catalogued) files, also keeping their sub-path. Promoting into an existing archive folder never touches files already there (Capture One's `Selects/`, `Output/`, `.cosessiondb`); a clash gets `-1`, `-2`, and identical content just drops the staging copy.
4. **Discard and cull delete only catalogued files.** A folder still holding untracked files after a discard keeps its row and directory, and the answer lists what kept it ("rescan, then discard again"). OS junk (`.DS_Store`, `Thumbs.db`, `desktop.ini`, `._*`) and stale `*.partial` never keep a folder alive.
5. **Folder naming.** Rename asks only for the title and keeps the date prefix; a folder whose name has no date (adopted from SMB) takes its earliest covered date as the prefix, or no prefix if it has none. Merge names the result `<earliest date> - <title>`; if one source already has that name it is the target (so its id survives), else the source with the earliest date. Move to "New folder…" requires a title and uses the earliest source folder's date (what the mockup previews). A name already used by another folder row or by a directory in `Staging/` is a 409 `name_taken`, never an automatic merge. Titles may not contain characters Windows can't use over SMB.
6. **Dates and age.** Merge unions the sources' dates; move adds to the destination the folder dates of the moved files (capture time with today's day cutoff, else the source folder's date). Merged and new folders take the oldest `created_at` of their sources, so "in staging for N days" stays honest. Age is counted from `created_at`; a folder adopted by the rescan starts counting at adoption.
7. **Files deleted over SMB become `culled`** (the owner deleted them on purpose, so they shouldn't come back from the card; "Re-import anyway" can still bring them back). M5 owns `missing` for vanished archive files.
8. **Reconcile runs at startup and from a "Rescan staging" button** (`POST /api/staging/rescan`), not on a timer: SMB edits are occasional and the owner knows when he made one. It hashes new files while checking their size and modified time don't change during the read (a file still being copied over SMB is skipped and picked up next time), ignores non-media files and `*.partial`, never follows content into another folder that is busy, and leaves copies of content that is already tracked elsewhere alone (counted as duplicates). Content edited in place in Staging is not detected (M5 handles that for the archive).
9. **Promote is one job at a time** (409 `promote_running` for a second); the UI disables Promote while one runs. All its folders are locked when it starts (so busy folders fail the request at once) and each is released as soon as it is done. Tracked files missing on disk are culled first, so the archive index never claims a file it doesn't have. If the catalog write after a successful folder rename fails, the files are in the archive but the rows aren't; a rescan would then cull them, which still keeps them from being re-imported, and M5's indexer must turn a culled hash it finds in `Archive/` into `archived` (follow-up recorded in Task 18). The pattern is sent with each request; the UI remembers the last one per device in `localStorage` (no server setting until a settings page exists).
10. **Forget an import** deletes the `staged` and `culled` assets linked to that import — those it brought in (`first_ingest`) and those any of its file rows names (so forgetting the second import of a card works too) — with their fingerprints, aliases and clock warnings, in one transaction, after deleting their files when `deleteFiles` is set. `archived` (and `missing`) rows are never deleted. The ingest row and its `ingest_files` stay, with `forgotten_at` set. It is refused while any import runs or waits, and `ingest.Manager.Exclusive` keeps a newly submitted import queued until the forget is done. Without `deleteFiles` the files stay in `Staging/` untracked: the next import of that card or the next rescan picks them up again. The UI's "also delete" checkbox starts unticked.
11. **"Re-import anyway"** (`POST /api/ingest/{id}/reimport-culled`): the card may be long gone, so it forgets that the import's "seen before" files whose asset is still `culled` were culled (the same deletion as forget, for those rows only), then, if the card's folder is still mounted, starts an import of it at once; otherwise the next time the card goes in brings them back. It never touches files.
12. **Thumbnails** are keyed by hash, so moves, merges and promotes keep them. Cull, discard, forget and rescan-culled files have their cached thumbnail and preview deleted (`thumbs.Cache.Remove`, called through `staging.Config.Removed`), so `/config` doesn't grow with culled shots. A render racing the delete can leave one file behind; that is accepted.
13. **Staging age warnings**: setting `staging_warn_days` (default 30, 0 = off; no settings UI yet). A stale folder shows "In staging for N days" and a pill; the page shows a banner counting them. The daily notification §3 mentions needs the M6 card-script channel and is left for M6.
14. **Events.** `staging.changed` carries a monotonically increasing `version`; `GET /api/staging` and `…/files` return the version their data reflects, so a page refetches exactly when the event's version differs (and a snapshot on reconnect that matches costs nothing). It is published after each operation, after a rescan that changed something, and when an import finishes (finished hook). `promote.progress` is throttled to 4 per second plus state changes, well under the hub's 16-frame buffer.
15. **Cross-device detection**: `errors.Is(err, syscall.EXDEV)`, plus `ERROR_NOT_SAME_DEVICE` (17) on Windows. Tests inject EXDEV through `Service.rename`. Within one Unraid share a rename never crosses disks, so the fallback is for setups that split Staging and Archive (§13).
16. **Durability**: folders touched by an operation are fsynced once at its end (`media.SyncDir`), not after every file; a crash before that can only undo renames, which the rescan repairs.
17. **The `api()` fix** (Task 1): an empty 200/204 body is `null`, and `ApiError` keeps the parsed error body in `data` so the UI can read `code` and `holder`.
18. **Interaction model** (Task 13): one rule set for both input types — when nothing is selected a click/tap opens, when something is selected it toggles; Ctrl/⌘-click toggles and Shift-click ranges on desktop; on touch a long-press selects and a drag after it selects a range. Desktop gets a hover checkbox on tiles and a checkbox on folder cards; touch gets long-press. Destructive actions (cull, discard, forget) confirm in a dialog in red, as in the mockup; rename, merge, move and promote show a preview of the resulting names.

## File Structure

```
internal/catalog/
  schema.go           schemaV2 (forgotten_at, assets_first_ingest)                    (Task 2)
  catalog.go          SchemaVersion = 2                                               (Task 2)
  ingests.go          Ingest.ForgottenAt, MarkIngestForgotten                         (Task 2)
  settings.go         KeyStagingWarnDays, Settings.StagingWarnDays                    (Task 2)
  stagingops.go       ErrNameTaken, Create/Rename/Delete folder, SetStagingFolderInfo,
                      AssetsByHash, MoveAsset, CullAsset, ArchiveAsset, ArchiveFolder,
                      RestageAsset                                                    (Task 2)
  forget.go           IngestAssets, ForgetAssets, CulledSeenBefore                    (Task 10)
internal/staging/
  locks.go            Locks, Holder*, BusyError, ErrBusy                              (Task 3)
  staging.go          Config, NewService, Status, RescanResult, OnChange, Touch, Close,
                      List/Files gain version and age                            (Tasks 4, 9)
  names.go            CleanTitle, FolderName, ExpandPattern, MaxNameBytes, errors     (Task 4)
  fsops.go            moveFile, copyVerify, removeEmptyTree, folderFiles, hashFile    (Task 4)
  xdev_windows.go, xdev_other.go   crossDevice                                        (Task 4)
  ops.go              Rename, Merge (Task 5); Move, Cull, Discard (Task 6)
  promote.go          Promote, PromoteJob, OnPromote                                  (Task 7)
  reconcile.go        Reconcile, StartRescan                                          (Task 8)
  forget.go           ForgetSummary, Forget, ForgetCulled                             (Task 10)
internal/ingest/
  run.go              Config.Locks; the import holds its folders' locks               (Task 3)
  partials.go         tolerate folders renamed mid-walk                               (Task 3)
  manager.go          ErrBusy, Exclusive                                              (Task 10)
internal/thumbs/thumbs.go   Remove                                                    (Task 11)
internal/api/
  staging.go          write endpoints, error mapping, staging.changed (Task 11);
                      promote, rescan, promote.progress (Task 12)
  forget.go           MountForget: forget summary, forget, reimport-culled            (Task 12)
  events.go           EventStagingChanged, EventPromoteProgress                  (Tasks 11, 12)
cmd/sd2nas/main.go    shared Locks, staging Service, thumbnail cleanup (11); MountForget, startup rescan (12)
web/src/lib/
  api.js              empty bodies, ApiError.data                                     (Task 1)
  naming.js, selection.js, messages.js (+ *.test.js)                                  (Task 13)
  press.js            longPressSelect action (touch)                                  (Task 14)
  toast.svelte.js     toast store                                                     (Task 14)
  live.svelte.js      staging.changed, promote.progress                               (Task 14)
web/src/components/
  Dialog.svelte, Toast.svelte, SelectionBar.svelte                                    (Task 14)
  PromoteProgress.svelte                                                              (Task 15)
  Thumb.svelte        draggable="false"                                               (Task 14)
web/src/app.css       button variants, form fields, touch selection rules             (Task 14)
web/src/App.svelte    <Toast />                                                       (Task 14)
web/src/pages/Staging.svelte      selection, dialogs, promote progress, rescan        (Task 15)
web/src/pages/Folder.svelte       file selection, move, cull                          (Task 16)
web/src/pages/IngestDetail.svelte, Ingest.svelte   forget, re-import, forgotten pill  (Task 17)
docs/PLAN.md                                                                          (Task 18)
```

---

### Task 1: `api()` accepts empty bodies and keeps the error body

Suggested implementer: haiku (transcription).

**Files:**
- Modify: `web/src/lib/api.js`, `web/src/lib/api.test.js`

**Interfaces:**
- Consumes: the existing `api(path, {method, body, signal})` and `ApiError(status, message)` in `web/src/lib/api.js`.
- Produces:

```js
// ApiError gains `data`: the parsed JSON error body (an object) or null.
export class ApiError extends Error { constructor(status, message, data = null) }
// api() resolves to null for 204 and for any 2xx with an empty (or whitespace-only) body.
export async function api(path, { method = 'GET', body, signal } = {})
```

- [ ] **Step 1: Write the failing tests** — append to `web/src/lib/api.test.js`:

```js
test('204 No Content resolves to null', async () => {
  respond(null, { status: 204 });
  assert.equal(await api('/api/x', { method: 'DELETE' }), null);
});

test('a 200 with an empty or blank body resolves to null', async () => {
  respond('', { status: 200 });
  assert.equal(await api('/api/x', { method: 'POST' }), null);
  respond('  \n', { status: 200 });
  assert.equal(await api('/api/x', { method: 'POST' }), null);
});

test('an error keeps its JSON body in data', async () => {
  respond('{"error":"2025-05-30 is being imported into. Try again when the import finishes.","code":"folder_busy","folder_id":3,"holder":"ingest"}', { status: 409 });
  await assert.rejects(api('/api/staging/3', { method: 'PATCH', body: { title: 'x' } }), (err) =>
    err instanceof ApiError && err.status === 409 && err.data.code === 'folder_busy' && err.data.holder === 'ingest' &&
    err.message.startsWith('2025-05-30 is being imported into'));
});

test('an error without a JSON body has null data', async () => {
  respond('oops', { status: 502, statusText: 'Bad Gateway' });
  await assert.rejects(api('/api/x'), (err) => err.status === 502 && err.data === null);
});
```

- [ ] **Step 2: Run them and confirm they fail**

Run (in `web/`): `npm test`
Expected: the 204 and empty-body tests fail (`did not answer with JSON`), and the `data` tests fail (`data` is undefined).

- [ ] **Step 3: Implement** — replace `ApiError` and `api` in `web/src/lib/api.js` (keep `parseJSON`, `send` and `getHealth`):

```js
export class ApiError extends Error {
  constructor(status, message, data = null) {
    super(message);
    this.name = 'ApiError';
    this.status = status; // 0 = no response (network failure)
    this.data = data; // the parsed JSON error body ({error, code, …}) or null
  }
}
```

```js
export async function api(path, { method = 'GET', body, signal } = {}) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET' && method !== 'HEAD') headers['X-SD2NAS'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await send(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal });
  const text = await res.text();
  if (!res.ok) {
    const parsed = parseJSON(text);
    const data = parsed.ok && parsed.value && typeof parsed.value === 'object' ? parsed.value : null;
    const msg = data && typeof data.error === 'string' ? data.error : `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg, data);
  }
  // 204, or an endpoint that answers success with no body.
  if (res.status === 204 || text.trim() === '') return null;
  const parsed = parseJSON(text);
  if (!parsed.ok) throw new ApiError(res.status, `${path} did not answer with JSON: ${parsed.error.message}`);
  return parsed.value;
}
```

- [ ] **Step 4: Run the tests and the build**

Run (in `web/`): `npm test` → all pass (the existing "a 200 that is not JSON is an error" still passes). `npm run build` succeeds; `git status --short web/dist` prints nothing.

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/api.js web/src/lib/api.test.js
git commit -m "Accept empty API answers and keep error bodies in ApiError

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Catalog schema v2 and the staging write queries

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/catalog/schema.go`, `internal/catalog/catalog.go`, `internal/catalog/ingests.go`, `internal/catalog/settings.go`, `internal/catalog/settings_test.go`
- Create: `internal/catalog/stagingops.go`, `internal/catalog/stagingops_test.go`

**Interfaces:**
- Consumes (existing): `DB.tx`, `scanStagingFolder`, `stagingCols`, `dateRE`, `prefixRange`, `scanAsset`, `assetCols`, `hexToBlob`, `formatTime`, `parseTime`, `requireChanged`, `ErrNotFound`, `StagingFolder`, `Asset`, `AssetState`, `Ingest`, `scanIngest`, `ingestCols`, `Settings.apply`; test helpers `openTest`, `hexN`, `t0`, `insertFolder`.
- Produces:

```go
const SchemaVersion = 2
var ErrNameTaken = errors.New("catalog: a staging folder with that name already exists")

// Ingest gains:  ForgottenAt time.Time `json:"forgotten_at,omitzero"`
func (d *DB) MarkIngestForgotten(ctx context.Context, id int64, at time.Time) error

const KeyStagingWarnDays = "staging_warn_days"
// Settings gains StagingWarnDays int (default 30; 0 = off; 0..3650)

func (d *DB) CreateStagingFolder(ctx context.Context, name string, dates []string, createdAt time.Time) (StagingFolder, error)
func (d *DB) RenameStagingFolder(ctx context.Context, id int64, name, oldPrefix, newPrefix string, at time.Time) error
func (d *DB) SetStagingFolderInfo(ctx context.Context, id int64, dates []string, createdAt time.Time) error // zero createdAt: unchanged
func (d *DB) DeleteStagingFolder(ctx context.Context, id int64) error
func (d *DB) AssetsByHash(ctx context.Context, shas []string) (map[string]Asset, error) // exact hashes, no alias lookup
func (d *DB) MoveAsset(ctx context.Context, sha, relPath string, at time.Time) error    // staged → staged at relPath
func (d *DB) CullAsset(ctx context.Context, sha string, at time.Time) error             // staged → culled, rel_path NULL
func (d *DB) ArchiveAsset(ctx context.Context, sha, relPath string, at time.Time) error // staged → archived at relPath
func (d *DB) RestageAsset(ctx context.Context, sha, relPath string, mtime int64, at time.Time) error // culled|missing → staged
func (d *DB) ArchiveFolder(ctx context.Context, id int64, fromPrefix, toPrefix string, at time.Time) (int, error)
```

- [ ] **Step 1: Write the failing tests** in `internal/catalog/stagingops_test.go` (package `catalog`). Helpers:

```go
// stagedAt inserts staged asset hexN(n) at rel (fingerprint hexN(100+n)).
func stagedAt(t *testing.T, d *DB, n byte, rel string) string {
	t.Helper()
	sha := hexN(n)
	a := Asset{SHA256: sha, Size: int64(n), Kind: media.KindImage, State: StateStaged, RelPath: rel, UpdatedAt: t0}
	if err := d.InsertAsset(t.Context(), a, hexN(100+n)); err != nil {
		t.Fatal(err)
	}
	return sha
}

func pathState(t *testing.T, d *DB, sha string) (string, AssetState) {
	t.Helper()
	a, err := d.AssetBySHA(t.Context(), sha)
	if err != nil {
		t.Fatal(err)
	}
	return a.RelPath, a.State
}
```

Tests:
- `TestMigrateV1ToV2`: write a version-1 database by hand — `sql.Open("sqlite", path)`, `Exec(schemaV1)`, `Exec("PRAGMA user_version = 1")`, `INSERT INTO ingests (card_path, started_at, status) VALUES ('/cards/X', '2026-01-01T12:00:00.000Z', 'done')`, close. `Open(path)` → `Version` is 2; `Ingest(ctx, 1)` has a zero `ForgottenAt`; `SELECT name FROM pragma_index_list('assets')` includes `assets_first_ingest`.
- `TestMarkIngestForgotten`: `CreateIngest` + `StartIngest` + `FinishIngest` (status done) → `MarkIngestForgotten(id, t0)` → `Ingest(id).ForgottenAt.Equal(t0)` and its JSON contains `"forgotten_at"`; a second call errors (not `ErrNotFound`); a queued ingest errors; id 999 → `ErrNotFound`. An ingest never forgotten marshals without `forgotten_at`.
- `TestStagingWarnDaysSetting` (in `settings_test.go`): default 30; `SetSetting(KeyStagingWarnDays, "0")` → 0; `"45"` → 45; `"-1"`, `"x"`, `"3651"` → error.
- `TestCreateStagingFolder`: `CreateStagingFolder(ctx, "2025-05-30 - Trip", []string{"2025-05-31", "2025-05-30", "2025-05-31"}, t0)` → `Dates` `["2025-05-30","2025-05-31"]`, `CreatedAt` equal to t0; the same name again → `errors.Is(err, ErrNameTaken)`; a bad date `"2025-5-30"` → error; nil dates → `[]string{}`.
- `TestRenameStagingFolderRewritesPaths` (Review Focus 4): rows `2025-05-30` and `2025-05-30 - Trip` (`insertFolder`); staged `Staging/2025-05-30/a.JPG` and `Staging/2025-05-30/Capture/a.ARW`, the trap `Staging/2025-05-30 - Trip/b.JPG`, and a culled asset without a path. `RenameStagingFolder(ctx, id, "2025-05-30 - Zürich", "Staging/2025-05-30/", "Staging/2025-05-30 - Zürich/", t0)` → `Staging/2025-05-30 - Zürich/a.JPG` and `…/Capture/a.ARW`; the trap is unchanged; the row is renamed. Rename again to `2025-05-30 - Zug` with old prefix `Staging/2025-05-30 - Zürich/` (a two-byte character in the old prefix: `length()` must count characters) → `Staging/2025-05-30 - Zug/Capture/a.ARW`. Renaming to `2025-05-30 - Trip` → `ErrNameTaken` and nothing changed. Renaming to its own name is a no-op. Unknown id → `ErrNotFound`. A prefix without its trailing `/` → error.
- `TestSetStagingFolderInfo`: dates replaced (sorted, de-duplicated); zero `createdAt` keeps it, a non-zero one sets it; unknown id → `ErrNotFound`.
- `TestDeleteStagingFolder`: deletes; again → `ErrNotFound`.
- `TestAssetsByHash`: 600 staged assets (`stagedAt` with `n` up to 255 won't do; build hashes with `fmt.Sprintf("%064x", i)` and insert directly) plus `AddAlias(old, <first hash>, t0)`: asking for all 600, one unknown hash and `old` returns exactly the 600 (the alias is **not** followed); a bad hash → `ErrBadHash`; an empty list → empty map.
- `TestAssetStateChanges`: `MoveAsset` changes a staged path; `CullAsset` → culled, `RelPath` "", and `AssetsByFingerprint(hexN(100+n))` still finds it; `RestageAsset` on it → staged at a new path with the mtime; `ArchiveAsset` → archived at an `Archive/…` path. Each on an asset in the wrong state (`MoveAsset` of a culled one, `RestageAsset` of a staged one, `CullAsset` of an archived one) → an error mentioning the state that is not `ErrNotFound`; an unknown hash → `ErrNotFound`.
- `TestArchiveFolder`: row `2025-05-30` with two staged assets plus the `2025-05-30 - Trip` trap → `ArchiveFolder(ctx, id, "Staging/2025-05-30/", "Archive/2025-05-30/", t0)` returns 2; both are archived under `Archive/2025-05-30/` with their sub-paths; the row is gone; the trap is staged and unchanged. An unknown id → `ErrNotFound` and nothing changed.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/catalog/`
Expected: build failure (`CreateStagingFolder undefined`, …).

- [ ] **Step 3: Schema version 2, `ForgottenAt`, the setting**

In `schema.go`:

```go
var migrations = []string{schemaV1, schemaV2}

// schemaV2 (M4): "Forget this import" marks the ingest instead of deleting
// it, and forgetting finds an import's assets by first_ingest.
const schemaV2 = `
ALTER TABLE ingests ADD COLUMN forgotten_at TEXT;   -- set by "Forget this import"; NULL = never
CREATE INDEX assets_first_ingest ON assets(first_ingest);
`
```

In `catalog.go`: `const SchemaVersion = 2`.

In `ingests.go`, add the field after `Error`:

```go
	ForgottenAt time.Time `json:"forgotten_at,omitzero"` // "Forget this import" ran then; zero = never
```

Append `, forgotten_at` to `ingestCols`; in `scanIngest` add `forgotten sql.NullString` as the last scan target and, after the `finished` block:

```go
	if forgotten.Valid {
		if in.ForgottenAt, err = parseTime(forgotten.String); err != nil {
			return Ingest{}, fmt.Errorf("ingest %d: parse forgotten_at: %w", in.ID, err)
		}
	}
```

and add:

```go
// MarkIngestForgotten records that ingest id was forgotten at at. The
// ingest must have finished and not been forgotten already.
func (d *DB) MarkIngestForgotten(ctx context.Context, id int64, at time.Time) error {
	res, err := d.db.ExecContext(ctx, `UPDATE ingests SET forgotten_at = ?
		WHERE id = ? AND forgotten_at IS NULL AND status NOT IN ('queued','running')`, formatTime(at), id)
	if err != nil {
		return fmt.Errorf("forget ingest %d: %w", id, err)
	}
	return d.requireChanged(ctx, res, id, "forget", "has not finished or was already forgotten")
}
```

In `settings.go`: the key `KeyStagingWarnDays = "staging_warn_days"`; the field `StagingWarnDays int // days in staging before a folder is flagged; 0 = never`; `StagingWarnDays: 30` in `DefaultSettings`; in `apply`:

```go
	case KeyStagingWarnDays:
		var n int
		n, err = strconv.Atoi(value)
		if err == nil && (n < 0 || n > 3650) {
			err = fmt.Errorf("want 0 (off) to 3650 days")
		}
		if err == nil {
			s.StagingWarnDays = n
		}
```

and add `KeyStagingWarnDays` to the key list `Settings` reads.

- [ ] **Step 4: `internal/catalog/stagingops.go`**

```go
package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// ErrNameTaken reports a staging folder name another folder already has.
var ErrNameTaken = errors.New("catalog: a staging folder with that name already exists")

// hashChunk bounds the number of ? in one IN (…) list.
const hashChunk = 500

// normalizeDates sorts and de-duplicates dates (each YYYY-MM-DD) and
// returns them as the JSON the dates column holds.
func normalizeDates(dates []string) (string, error) {
	out := []string{}
	for _, d := range dates {
		if !dateRE.MatchString(d) {
			return "", fmt.Errorf("bad date %q", d)
		}
		out = append(out, d)
	}
	slices.Sort(out)
	b, err := json.Marshal(slices.Compact(out))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// nameTaken returns ErrNameTaken when a row other than except is named name.
func nameTaken(ctx context.Context, tx *sql.Tx, name string, except int64) error {
	var id int64
	err := tx.QueryRowContext(ctx, "SELECT id FROM staging_folders WHERE name = ? AND id != ?", name, except).Scan(&id)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	}
	return fmt.Errorf("%w: %q", ErrNameTaken, name)
}

// CreateStagingFolder adds a folder row. ErrNameTaken when the name exists.
func (d *DB) CreateStagingFolder(ctx context.Context, name string, dates []string, createdAt time.Time) (StagingFolder, error) {
	ds, err := normalizeDates(dates)
	if err != nil {
		return StagingFolder{}, fmt.Errorf("create staging folder %q: %w", name, err)
	}
	var id int64
	err = d.tx(ctx, func(tx *sql.Tx) error {
		if err := nameTaken(ctx, tx, name, 0); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, "INSERT INTO staging_folders (name, dates, created_at) VALUES (?,?,?)",
			name, ds, formatTime(createdAt))
		if err != nil {
			return err
		}
		id, err = res.LastInsertId()
		return err
	})
	if err != nil {
		return StagingFolder{}, fmt.Errorf("create staging folder %q: %w", name, err)
	}
	return d.StagingFolderByID(ctx, id)
}

// rewritePrefix moves every asset whose rel_path lies below from to the
// same place below to, setting state unless it is "", and only for assets
// in onlyState unless that is "". SQLite's substr and length count
// characters, not bytes, so the old prefix is measured in SQL: folder names
// may hold any UTF-8.
func rewritePrefix(ctx context.Context, tx *sql.Tx, from, to string, state, onlyState AssetState, at time.Time) (int64, error) {
	lo, hi, err := prefixRange(from)
	if err != nil {
		return 0, err
	}
	if _, _, err := prefixRange(to); err != nil {
		return 0, err
	}
	q := "UPDATE assets SET rel_path = ? || substr(rel_path, length(?) + 1), updated_at = ?"
	args := []any{to, from, formatTime(at)}
	if state != "" {
		q += ", state = ?"
		args = append(args, string(state))
	}
	q += " WHERE rel_path >= ? AND rel_path < ?"
	args = append(args, lo, hi)
	if onlyState != "" {
		q += " AND state = ?"
		args = append(args, string(onlyState))
	}
	res, err := tx.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func folderInTx(ctx context.Context, tx *sql.Tx, id int64) (StagingFolder, error) {
	f, err := scanStagingFolder(tx.QueryRowContext(ctx, "SELECT "+stagingCols+" FROM staging_folders WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return StagingFolder{}, ErrNotFound
	}
	return f, err
}

// RenameStagingFolder renames folder id to name and, in the same
// transaction, moves every asset below oldPrefix (Staging/<old>/) to the
// same place below newPrefix (Staging/<name>/). ErrNameTaken when another
// folder already has name.
func (d *DB) RenameStagingFolder(ctx context.Context, id int64, name, oldPrefix, newPrefix string, at time.Time) error {
	err := d.tx(ctx, func(tx *sql.Tx) error {
		f, err := folderInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if f.Name == name {
			return nil
		}
		if err := nameTaken(ctx, tx, name, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE staging_folders SET name = ? WHERE id = ?", name, id); err != nil {
			return err
		}
		_, err = rewritePrefix(ctx, tx, oldPrefix, newPrefix, "", "", at)
		return err
	})
	if err != nil {
		return fmt.Errorf("rename staging folder %d to %q: %w", id, name, err)
	}
	return nil
}

// SetStagingFolderInfo replaces folder id's dates and, unless createdAt is
// zero, its creation time.
func (d *DB) SetStagingFolderInfo(ctx context.Context, id int64, dates []string, createdAt time.Time) error {
	ds, err := normalizeDates(dates)
	if err != nil {
		return fmt.Errorf("staging folder %d: %w", id, err)
	}
	q, args := "UPDATE staging_folders SET dates = ? WHERE id = ?", []any{ds, id}
	if !createdAt.IsZero() {
		q, args = "UPDATE staging_folders SET dates = ?, created_at = ? WHERE id = ?", []any{ds, formatTime(createdAt), id}
	}
	return d.exactlyOne(ctx, fmt.Sprintf("staging folder %d", id), q, args...)
}

// DeleteStagingFolder removes folder id's row (not its assets).
func (d *DB) DeleteStagingFolder(ctx context.Context, id int64) error {
	return d.exactlyOne(ctx, fmt.Sprintf("delete staging folder %d", id), "DELETE FROM staging_folders WHERE id = ?", id)
}

// exactlyOne runs q, which must change one row; none is ErrNotFound.
func (d *DB) exactlyOne(ctx context.Context, what, q string, args ...any) error {
	res, err := d.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", what, ErrNotFound)
	}
	return nil
}

// AssetsByHash returns the assets with exactly these hashes (aliases are
// not followed), keyed by hash. Unknown hashes are absent.
func (d *DB) AssetsByHash(ctx context.Context, shas []string) (map[string]Asset, error) {
	out := map[string]Asset{}
	for start := 0; start < len(shas); start += hashChunk {
		chunk := shas[start:min(start+hashChunk, len(shas))]
		args := make([]any, len(chunk))
		for i, s := range chunk {
			b, err := hexToBlob(s)
			if err != nil {
				return nil, err
			}
			args[i] = b
		}
		q := "SELECT " + assetCols + " FROM assets WHERE sha256 IN (?" + strings.Repeat(",?", len(chunk)-1) + ")"
		rows, err := d.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, fmt.Errorf("assets by hash: %w", err)
		}
		for rows.Next() {
			a, err := scanAsset(rows)
			if err != nil {
				return nil, errors.Join(fmt.Errorf("assets by hash: %w", err), rows.Close())
			}
			out[a.SHA256] = a
		}
		if err := rows.Err(); err != nil {
			return nil, errors.Join(fmt.Errorf("assets by hash: %w", err), rows.Close())
		}
		if err := rows.Close(); err != nil {
			return nil, fmt.Errorf("assets by hash: %w", err)
		}
	}
	return out, nil
}

// changeAsset updates one asset that must be in one of states (the IN list
// is built from AssetState constants only, never from input). It names the
// state it found when the asset is in another, and returns ErrNotFound for
// an unknown hash.
func (d *DB) changeAsset(ctx context.Context, verb, sha string, states []AssetState, set string, args ...any) error {
	b, err := hexToBlob(sha)
	if err != nil {
		return err
	}
	in := make([]string, len(states))
	for i, s := range states {
		in[i] = "'" + string(s) + "'"
	}
	q := "UPDATE assets SET " + set + " WHERE sha256 = ? AND state IN (" + strings.Join(in, ",") + ")"
	res, err := d.db.ExecContext(ctx, q, append(args, b)...)
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	if n == 1 {
		return nil
	}
	var state string
	err = d.db.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", b).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s %s: %w", verb, sha, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", verb, sha, err)
	}
	return fmt.Errorf("%s %s: the asset is %s", verb, sha, state)
}

var onlyStaged = []AssetState{StateStaged}

// MoveAsset records that staged asset sha now lives at relPath.
func (d *DB) MoveAsset(ctx context.Context, sha, relPath string, at time.Time) error {
	return d.changeAsset(ctx, "move", sha, onlyStaged, "rel_path = ?, updated_at = ?", relPath, formatTime(at))
}

// CullAsset records that staged asset sha was deleted on purpose (§6
// "Culled stays remembered"): state culled, no path; fingerprints stay.
func (d *DB) CullAsset(ctx context.Context, sha string, at time.Time) error {
	return d.changeAsset(ctx, "cull", sha, onlyStaged, "state = 'culled', rel_path = NULL, updated_at = ?", formatTime(at))
}

// ArchiveAsset records that staged asset sha was promoted to relPath.
func (d *DB) ArchiveAsset(ctx context.Context, sha, relPath string, at time.Time) error {
	return d.changeAsset(ctx, "archive", sha, onlyStaged, "state = 'archived', rel_path = ?, updated_at = ?", relPath, formatTime(at))
}

// RestageAsset records that the content of a culled or missing asset is in
// staging again, at relPath.
func (d *DB) RestageAsset(ctx context.Context, sha, relPath string, mtime int64, at time.Time) error {
	return d.changeAsset(ctx, "restage", sha, []AssetState{StateCulled, StateMissing},
		"state = 'staged', rel_path = ?, mtime = ?, updated_at = ?", relPath, mtime, formatTime(at))
}

// ArchiveFolder records a promoted folder in one transaction: every staged
// asset below fromPrefix becomes archived at the same place below toPrefix,
// and row id is deleted. It returns how many assets it archived.
func (d *DB) ArchiveFolder(ctx context.Context, id int64, fromPrefix, toPrefix string, at time.Time) (int, error) {
	var n int64
	err := d.tx(ctx, func(tx *sql.Tx) error {
		if _, err := folderInTx(ctx, tx, id); err != nil {
			return err
		}
		var err error
		if n, err = rewritePrefix(ctx, tx, fromPrefix, toPrefix, StateArchived, StateStaged, at); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "DELETE FROM staging_folders WHERE id = ?", id)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("archive staging folder %d: %w", id, err)
	}
	return int(n), nil
}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/catalog/` → PASS. Then `gofmt -l .`, `go vet ./...`, `go test ./...` and the Linux command → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/catalog/
git commit -m "Add catalog schema v2 and the staging write queries

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Folder locks, taken by every import

Suggested implementer: sonnet (reviewer: opus — concurrency).

**Files:**
- Create: `internal/staging/locks.go`, `internal/staging/locks_test.go`, `internal/ingest/locks_test.go`
- Modify: `internal/ingest/run.go`, `internal/ingest/partials.go`

**Interfaces:**
- Consumes: `catalog.DB.StagingFolderForDate`; in tests `catalog.DB.RenameStagingFolder`, `DeleteStagingFolder` (Task 2); ingest test helpers `newEnv`, `env.fixtureCard`, `env.staged`, `gate`, `waitStarted`, `testNow`, `NewProgress`, `Progress.Snapshot`, `PhaseCopying`.
- Produces:

```go
// package staging
type Holder string
const (
	HolderIngest    Holder = "ingest"    // a card import writing into the folder
	HolderOperation Holder = "operation" // rename, merge, move, cull, discard, promote, forget
	HolderRescan    Holder = "rescan"    // the reconcile pass
)
var ErrBusy = errors.New("staging: folder is busy")
type BusyError struct{ ID int64; Name string; Holder Holder } // errors.Is(err, ErrBusy); Name may be ""
type Locks struct{ /* unexported */ }
func NewLocks() *Locks
func (l *Locks) TryLock(h Holder, ids ...int64) (release func(), err error)        // all or none, never waits
func (l *Locks) TryLockEach(h Holder, ids ...int64) (releases []func(), err error) // same; one release per (distinct) id, in order
func (l *Locks) Lock(ctx context.Context, h Holder, id int64) (release func(), err error) // waits until free or ctx ends

// package ingest — Config gains:
Locks *staging.Locks // folder locks shared with staging operations; nil: a private set
```

- [ ] **Step 1: Write the failing lock tests** in `internal/staging/locks_test.go` (package `staging`):
- `TestTryLockAllOrNothing`: `TryLock(HolderOperation, 1, 2)` succeeds; `TryLock(HolderIngest, 2, 3)` fails with `*BusyError{ID: 2, Holder: HolderOperation}` and `errors.Is(err, ErrBusy)`; 3 is still free afterwards. `TryLock(h, 4, 4)` locks 4 once and one release frees it.
- `TestReleaseIdempotent`: a release called twice is harmless; after the first call another holder takes the id, and the first holder's second call does not free it.
- `TestLockWaitsForRelease`: A holds 7; a goroutine calls `Lock(ctx, HolderIngest, 7)` and signals on return; it must not return within 100 ms (named negative check) and must return within 10 s of A's release.
- `TestLockContextCancel`: `Lock` on a held id with a context cancelled while waiting returns `context.Canceled` and holds nothing.
- `TestTryLockEach`: `TryLockEach(h, 1, 2)` returns two releases; calling the first frees 1 only.
- `TestLocksConcurrent`: 40 goroutines × 200 iterations pick an id in 1..5 and either `TryLock` or `Lock` (with a 5 ms timeout); while holding, an atomic per-id counter must never exceed 1. Run under `-race` (Step 6).

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/staging/ -run Lock` → build failure.

- [ ] **Step 3: Implement `internal/staging/locks.go`**

```go
package staging

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
)

// Holder says who holds a folder lock; the API turns it into the 409 message.
type Holder string

const (
	HolderIngest    Holder = "ingest"    // a card import writing into the folder
	HolderOperation Holder = "operation" // rename, merge, move, cull, discard, promote, forget
	HolderRescan    Holder = "rescan"    // the reconcile pass
)

// ErrBusy is what every *BusyError matches with errors.Is.
var ErrBusy = errors.New("staging: folder is busy")

// BusyError reports a staging folder whose lock someone else holds.
type BusyError struct {
	ID     int64
	Name   string // filled in by the operation when it knows the folder; may be ""
	Holder Holder
}

func (e *BusyError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("staging folder %q is busy (%s)", e.Name, e.Holder)
	}
	return fmt.Sprintf("staging folder %d is busy (%s)", e.ID, e.Holder)
}

func (e *BusyError) Is(target error) bool { return target == ErrBusy }

// Locks are §8's folder locks: every operation, and an import while it
// writes into a folder, holds the lock of each staging folder it touches.
// Keys are staging_folders ids.
type Locks struct {
	mu   sync.Mutex
	held map[int64]*lockState
}

type lockState struct {
	holder   Holder
	released chan struct{} // closed on release; waiters then try again
}

func NewLocks() *Locks { return &Locks{held: map[int64]*lockState{}} }

func uniqueIDs(ids []int64) []int64 {
	out := slices.Clone(ids)
	slices.Sort(out)
	return slices.Compact(out)
}

// TryLock takes the locks of every id at once, or none. It never waits: a
// held lock fails it with a *BusyError naming that folder.
func (l *Locks) TryLock(h Holder, ids ...int64) (release func(), err error) {
	rs, err := l.TryLockEach(h, uniqueIDs(ids)...)
	if err != nil {
		return nil, err
	}
	return func() {
		for _, r := range rs {
			r()
		}
	}, nil
}

// TryLockEach is TryLock with one release per id (ids must be distinct), so
// a long job can give each folder back as soon as it is done with it.
func (l *Locks) TryLockEach(h Holder, ids ...int64) ([]func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, id := range ids {
		if st, ok := l.held[id]; ok {
			return nil, &BusyError{ID: id, Holder: st.holder}
		}
	}
	out := make([]func(), len(ids))
	for i, id := range ids {
		out[i] = l.take(id, h)
	}
	return out, nil
}

// take records h as id's holder (l.mu held) and returns its release.
func (l *Locks) take(id int64, h Holder) func() {
	st := &lockState{holder: h, released: make(chan struct{})}
	l.held[id] = st
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.held[id] == st {
				delete(l.held, id)
			}
			l.mu.Unlock()
			close(st.released)
		})
	}
}

// Lock takes id's lock, waiting while someone else holds it, until ctx
// ends. Only an import waits (Decision 1); operations use TryLock.
func (l *Locks) Lock(ctx context.Context, h Holder, id int64) (func(), error) {
	for {
		l.mu.Lock()
		st, ok := l.held[id]
		if !ok {
			release := l.take(id, h)
			l.mu.Unlock()
			return release, nil
		}
		l.mu.Unlock()
		select {
		case <-st.released:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}
```

- [ ] **Step 4: Write the failing ingest tests** in `internal/ingest/locks_test.go` (package `ingest`). The committed `SONY_A7IV` card lands in folder `2025-05-30`. Helpers:

```go
type runResult struct {
	out catalog.Ingest
	err error
}

// runAsync runs the queued ingest in on e's runner in the background.
func runAsync(ctx context.Context, e *env, in catalog.Ingest, p *Progress) <-chan runResult {
	done := make(chan runResult, 1)
	go func() {
		out, err := e.r.Run(ctx, in, p)
		done <- runResult{out, err}
	}()
	return done
}

func waitRun(t *testing.T, done <-chan runResult) catalog.Ingest {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return r.out
	case <-time.After(10 * time.Second):
		t.Fatal("import did not finish")
		return catalog.Ingest{}
	}
}

func waitPhase(t *testing.T, p *Progress, phase string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for p.Snapshot().Phase != phase {
		if time.Now().After(deadline) {
			t.Fatalf("phase is %q, want %q", p.Snapshot().Phase, phase)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// queued creates the ingest row for card.
func queued(t *testing.T, e *env, card string) catalog.Ingest {
	t.Helper()
	in, err := e.cat.CreateIngest(t.Context(), card, filepath.Base(card), "", testNow)
	if err != nil {
		t.Fatal(err)
	}
	return in
}
```

Tests (Review Focus 1):
- `TestRunHoldsFolderLocks`: `started, release := gate(e)`; `runAsync`; `waitStarted`; the one row from `StagingFolders` → `e.r.locks.TryLock(staging.HolderOperation, id)` fails with a `*staging.BusyError` whose `Holder` is `staging.HolderIngest`; `release()`; `waitRun` → status done; now `TryLock` succeeds (release it).
- `TestRunWaitsForBusyFolder`: `f := e.cat.StagingFolderForDate(ctx, "2025-05-30", testNow)`; hold it with `e.r.locks.TryLock(staging.HolderOperation, f.ID)`; wrap `e.r.copyFile` to close a `copied` channel (once) and then call `media.CopyHashed`; `runAsync` with a `NewProgress()`; `waitPhase(PhaseCopying)`; a 300 ms select proves no copy started (negative check); release the lock; `waitRun` → done with `Counts.New == 2` and both files under `Staging/2025-05-30/`.
- `TestRunFollowsFolderRenamedWhileWaiting`: as above, but while the import waits, `e.cat.RenameStagingFolder(ctx, f.ID, "2025-05-30 - Trip", "Staging/2025-05-30/", "Staging/2025-05-30 - Trip/", testNow)`, then release → `e.staged(t)` lists both files under `Staging/2025-05-30 - Trip/`, and each new asset's `RelPath` is there too.
- `TestRunFolderDeletedWhileWaiting`: while waiting, `e.cat.DeleteStagingFolder(ctx, f.ID)`, then release → a new `2025-05-30` row with a different id exists and the import finished with 2 new.
- `TestRunCancelledWhileWaiting`: hold the folder; `runAsync` with a cancellable context; `waitPhase(PhaseCopying)`; cancel → status `cancelled`, `e.staged(t)` is empty; the test's lock is still held (then release it).

- [ ] **Step 5: Implement the import side** in `internal/ingest/run.go`

Import `github.com/aspiziri/SD2NAS/internal/staging`. In `Config` add:

```go
	Locks      *staging.Locks                    // folder locks shared with staging operations; nil: a private set
```

In `Runner` add `locks *staging.Locks`; `NewRunner` becomes:

```go
func NewRunner(cfg Config) *Runner {
	locks := cfg.Locks
	if locks == nil {
		locks = staging.NewLocks()
	}
	return &Runner{cfg: cfg, locks: locks, fingerprint: media.QuickFingerprint, hashFile: media.HashFile, copyFile: media.CopyHashed}
}
```

In `ingestRun` add `held map[int64]func() // staging folder id → release; kept until the run ends (Decision 1)`, set `held: map[int64]func(){}` in `Run`'s literal, and end `Run` with:

```go
	status, errText := j.pipeline()
	j.releaseFolders()
	return j.finish(status, errText)
```

Add:

```go
// maxFolderAttempts bounds how often folderFor looks a folder up again
// after waiting for it.
const maxFolderAttempts = 5

// folderFor returns the staging folder for date and holds its lock until
// the run ends. A staging operation may hold it: the import waits, and
// since that operation may rename, merge away or promote the folder, it
// looks the row up again once it has the lock.
func (j *ingestRun) folderFor(date string) (catalog.StagingFolder, error) {
	for range maxFolderAttempts {
		f, err := j.cat().StagingFolderForDate(j.dbctx, date, j.now)
		if err != nil {
			return catalog.StagingFolder{}, err
		}
		if _, ok := j.held[f.ID]; ok {
			return f, nil
		}
		release, err := j.r.locks.Lock(j.ctx, staging.HolderIngest, f.ID)
		if err != nil {
			return catalog.StagingFolder{}, err
		}
		j.held[f.ID] = release
	}
	return catalog.StagingFolder{}, fmt.Errorf("the staging folder for %s kept changing while the import waited for it", date)
}

// releaseFolders gives back every folder lock the run holds.
func (j *ingestRun) releaseFolders() {
	for id, release := range j.held {
		release()
		delete(j.held, id)
	}
}
```

In `importOne`, replace the `StagingFolderForDate` call and its error check with:

```go
	folder, err := j.folderFor(media.FolderDate(c.Time, j.settings.DayStartsAt))
	if err != nil {
		if j.ctx.Err() != nil {
			return fileResult{canceled: true}
		}
		return fail(fmt.Errorf("staging folder: %w", err))
	}
```

In `internal/ingest/partials.go`, let the walk tolerate folders a staging operation renames or removes while it runs:

```go
	err := filepath.WalkDir(stagingRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A staging operation may rename or remove a folder mid-walk.
			if path != stagingRoot && errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !d.Type().IsRegular() || !strings.HasSuffix(d.Name(), media.PartialSuffix) {
			return nil
		}
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		// …the rest is unchanged
```

- [ ] **Step 6: Run the tests, the full set and the race run**

`go test ./internal/staging/ ./internal/ingest/` → PASS. Then `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command and the **race** command → PASS. `go list -deps ./internal/staging | grep internal/ingest` prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/staging/locks.go internal/staging/locks_test.go internal/ingest/run.go internal/ingest/partials.go internal/ingest/locks_test.go
git commit -m "Lock staging folders while an import writes into them

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Staging service config, change notifications, names and safe file moves

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/staging/staging.go`
- Create: `internal/staging/names.go`, `internal/staging/names_test.go`, `internal/staging/fsops.go`, `internal/staging/fsops_test.go`, `internal/staging/xdev_windows.go`, `internal/staging/xdev_other.go`, `internal/staging/helpers_test.go`, `internal/staging/service_test.go`

**Interfaces:**
- Consumes: `Locks`, `NewLocks` (Task 3); `catalog.StagingFolder`, `catalog.DB.CreateStagingFolder`, `InsertAsset`, `AssetBySHA`; `media.FreeName`, `media.CopyHashed`, `media.ClassifyCopyError`, `media.PartialSuffix`, `media.SyncDir`, `media.DefaultRules`.
- Produces:

```go
type Config struct {
	DB         *catalog.DB
	PhotosRoot string
	Locks      *Locks              // shared with ingest.Config.Locks; nil: a private set
	Now        func() time.Time    // nil: time.Now
	Log        *slog.Logger        // nil: slog.Default()
	Removed    func(shas []string) // called after files are culled, discarded or forgotten (thumbnail cleanup); nil: nothing
}
func NewService(cfg Config) *Service // New(db, photosRoot) stays, = NewService(Config{DB: db, PhotosRoot: photosRoot})
type Status struct {
	Version    uint64        `json:"version"`
	Rescanning bool          `json:"rescanning"`
	LastRescan *RescanResult `json:"last_rescan"`
}
type RescanResult struct { /* fields below; filled by Task 8 */ }
func (s *Service) Status() Status
func (s *Service) OnChange(fn func(Status)) // call before serving; fn runs under the service's lock and must not block
func (s *Service) Touch()                   // records a change made elsewhere (an import finished)
func (s *Service) Close()                   // stops background work (promote, rescan) and waits for it
var ErrBadTitle, ErrBadPattern, ErrBadRequest, ErrClosed error
const MaxNameBytes = 200
const DefaultPattern = "{name}"
func CleanTitle(title string) (string, error)
func FolderName(date, title string) string
func ExpandPattern(pattern string, f catalog.StagingFolder) (string, error)
```

Unexported, used by Tasks 5–10: `s.locks`, `s.db`, `s.now()`, `s.log`, `s.rename` (seam, `os.Rename`), `s.rules`, `s.bgCtx`, `s.wg`, `s.mu`, `s.closed`, `s.changed()`, `s.removed(shas)`, `s.folderDir(name)`, `s.abs(rel)`, `s.relOf(abs)`, `checkName`, `safeName`, `folderDate`, `moveFile`, `copyVerify`, `removeEmptyTree`, `folderFiles`, `isJunk`, `hashFile`, `syncDirs`, `crossDevice`, `uniqueIDs` (Task 3).

- [ ] **Step 1: Write the failing tests**

`internal/staging/helpers_test.go` (package `staging`) — the environment Tasks 4–10 share:

```go
// opsNow is the service's clock in operation tests.
var opsNow = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// opsEnv is a Service over a temp photos share and catalog.
type opsEnv struct {
	svc     *Service
	db      *catalog.DB
	photos  string
	locks   *Locks
	mu      sync.Mutex
	removed []string // hashes passed to Config.Removed
	changes []Status // statuses passed to OnChange
}

func newOpsEnv(t *testing.T) *opsEnv {
	t.Helper()
	photos, config := t.TempDir(), t.TempDir()
	db, err := catalog.Open(filepath.Join(config, "sd2nas.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close catalog: %v", err)
		}
	})
	e := &opsEnv{db: db, photos: photos, locks: NewLocks()}
	e.svc = NewService(Config{DB: db, PhotosRoot: photos, Locks: e.locks, Now: func() time.Time { return opsNow },
		Log: slog.New(slog.DiscardHandler), Removed: func(shas []string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.removed = append(e.removed, shas...)
		}})
	e.svc.OnChange(func(st Status) {
		e.mu.Lock()
		defer e.mu.Unlock()
		e.changes = append(e.changes, st)
	})
	t.Cleanup(e.svc.Close) // runs before the catalog closes
	return e
}

func shaOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// write writes content at rel (slash path below the photos root) and returns the OS path.
func (e *opsEnv) write(t *testing.T, rel, content string) string {
	t.Helper()
	p := filepath.Join(e.photos, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o666); err != nil {
		t.Fatal(err)
	}
	return p
}

// folder creates a staging folder row and its directory.
func (e *opsEnv) folder(t *testing.T, name string, dates ...string) catalog.StagingFolder {
	t.Helper()
	f, err := e.db.CreateStagingFolder(t.Context(), name, dates, opsNow)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.photos, Dir, name), 0o777); err != nil {
		t.Fatal(err)
	}
	return f
}

// stage writes content at rel inside folder f (e.g. "Capture/a.ARW") and
// records it as a staged asset; it returns the content hash.
func (e *opsEnv) stage(t *testing.T, f catalog.StagingFolder, rel, content string, capture time.Time) string {
	t.Helper()
	relPath := Dir + "/" + f.Name + "/" + rel
	e.write(t, relPath, content)
	kind, ok := media.DefaultRules().KindOf(rel)
	if !ok {
		t.Fatalf("%s is not a media file", rel)
	}
	sha := shaOf(content)
	a := catalog.Asset{SHA256: sha, Size: int64(len(content)), Kind: kind, CaptureTime: capture,
		State: catalog.StateStaged, RelPath: relPath, UpdatedAt: opsNow}
	if err := e.db.InsertAsset(t.Context(), a, shaOf("fp:"+content)); err != nil {
		t.Fatal(err)
	}
	return sha
}

// files lists every regular file below the photos root as sorted slash paths.
func (e *opsEnv) files(t *testing.T) []string { /* filepath.WalkDir + Rel + ToSlash + sort; nil when none */ }

func (e *opsEnv) asset(t *testing.T, sha string) catalog.Asset { /* AssetBySHA or t.Fatal */ }

// version is the last status OnChange saw (0 if none).
func (e *opsEnv) version() uint64 { /* under e.mu */ }

func (e *opsEnv) removedHashes() []string { /* sorted copy under e.mu */ }
```

`names_test.go`:
- `TestCleanTitle`: `"  Trip "` → `"Trip"`; `""` and `"   "` → `""` without error; `"Zürich & Co."` → error (ends with a dot); `"Zürich & Co"` ok; each of `a/b`, `a\b`, `a:b`, `a*b`, `a?b`, `a"b`, `a<b`, `a>b`, `a|b`, `"a\tb"`, `.hidden`, `trailing.`, `strings.Repeat("é", 101)` (202 bytes) → `errors.Is(err, ErrBadTitle)`; `strings.Repeat("é", 100)` (200 bytes) ok.
- `TestFolderName`: `("2025-05-30","Trip")` → `2025-05-30 - Trip`; `("2025-05-30","")` → `2025-05-30`; `("","Trip")` → `Trip`.
- `TestExpandPattern`: folder `{Name: "2025-05-30 - Trip", Dates: ["2025-05-30","2025-05-31"]}`: `""` and `"{name}"` → `2025-05-30 - Trip`; `"{year}/{month}/{name}"` → `2025/05/2025-05-30 - Trip`; `"{year}/{name"` → `ErrBadPattern` (unbalanced); `"{day}"` → `ErrBadPattern` naming `{day}`; `"../{name}"` → `ErrBadPattern`; `"{year}//{name}"` → `ErrBadPattern` (empty element). A folder named `Holiday` with dates `["2025-06-01"]` → `{year}` gives `2025`; with no dates → `ErrBadPattern` ("needs a date"). A folder named `What?` with `{name}` → `ErrBadPattern`.

`fsops_test.go` (use `newOpsEnv`; the moves are unexported methods):
- `TestMoveFileRename`: `moveFile(ctx, src, dir, "a.JPG", "")` into a new subfolder → the file is at `dir/a.JPG`, `src` is gone, the content is the same.
- `TestMoveFileCollision`: `dir/a.JPG` exists with different content (and `dir/a-1.JPG` too) → lands at `dir/a-2.JPG`; nothing overwritten.
- `TestMoveFileIdentical`: `dir/a.JPG` has the same content → returns `dir/a.JPG`, `src` is removed, one copy remains.
- `TestMoveFileCrossDevice`: `e.svc.rename = func(o, n string) error { return &os.LinkError{Op: "rename", Old: o, New: n, Err: syscall.EXDEV} }` → the copy lands, `src` is gone, the copy's mtime equals the original's (CopyHashed keeps it), `want` = the right hash passes.
- `TestMoveFileCrossDeviceMismatch`: EXDEV and a `want` that isn't the content's hash → error, `src` still there, no file (and no `.partial`) in `dir`.
- `TestMoveFileRenameError`: a rename seam returning another error (e.g. `fs.ErrPermission`) → that error, `src` untouched, no copy made.
- `TestRemoveEmptyTree` (Review Focus 2): `x/Capture/` (empty), `x/Videos/.DS_Store`, `x/Thumbs.db` → gone, true. `y/Capture/a.ARW` plus `y/.DS_Store` → not gone, kept `["Capture/a.ARW"]`, **`.DS_Store` still there** (junk goes only with an otherwise empty folder), the empty `y/Videos/` removed. A missing directory → gone, no error.
- `TestFolderFiles`: lists regular files below a folder as sorted slash paths, skipping junk (`.DS_Store`, `._a.JPG`, `x.JPG.partial`, `THUMBS.DB`); a missing folder → nil, no error.
- `TestCrossDevice`: `crossDevice(&os.LinkError{Err: syscall.EXDEV})` is true; `crossDevice(fs.ErrPermission)` false.

`service_test.go`:
- `TestStatusAndTouch`: a new service has `Status{Version: 0}`; `Touch()` twice → version 2, and `OnChange` saw versions 1 and 2 in order.
- `TestCloseIdempotent`: `Close()` twice returns.

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/staging/` → build failure.

- [ ] **Step 3: Service config and status** — in `staging.go` replace `Service`, `New` with:

```go
// Config wires a Service.
type Config struct {
	DB         *catalog.DB
	PhotosRoot string
	Locks      *Locks              // shared with ingest.Config.Locks; nil: a private set
	Now        func() time.Time    // nil: time.Now
	Log        *slog.Logger        // nil: slog.Default()
	Removed    func(shas []string) // after files are culled, discarded or forgotten (thumbnail cleanup); nil: nothing
}

// Service reads and changes the folders under Staging/.
type Service struct {
	db         *catalog.DB
	photosRoot string
	locks      *Locks
	now        func() time.Time
	log        *slog.Logger
	removedFn  func([]string)
	rules      media.Rules
	rename     func(oldpath, newpath string) error // os.Rename; tests inject EXDEV

	bgCtx    context.Context // cancelled by Close; promote and rescan run on it
	bgCancel context.CancelFunc
	wg       sync.WaitGroup

	mu     sync.Mutex // guards status, notify, closed
	status Status
	notify func(Status)
	closed bool
}

// New is NewService with only the catalog and the photos root.
func New(db *catalog.DB, photosRoot string) *Service {
	return NewService(Config{DB: db, PhotosRoot: photosRoot})
}

func NewService(cfg Config) *Service {
	s := &Service{db: cfg.DB, photosRoot: cfg.PhotosRoot, locks: cfg.Locks, now: cfg.Now, log: cfg.Log,
		removedFn: cfg.Removed, rules: media.DefaultRules(), rename: os.Rename}
	if s.locks == nil {
		s.locks = NewLocks()
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	return s
}

// Status is what the staging.changed event carries (Decision 14).
type Status struct {
	Version    uint64        `json:"version"`     // bumps on every change to staging
	Rescanning bool          `json:"rescanning"`  // a reconcile pass is running
	LastRescan *RescanResult `json:"last_rescan"` // the last finished pass; nil before the first
}

// RescanResult is what one reconcile pass did (Task 8 fills it).
type RescanResult struct {
	StartedAt      time.Time `json:"started_at"`
	FinishedAt     time.Time `json:"finished_at"`
	AdoptedFolders int       `json:"adopted_folders"` // directories that had no row
	AddedFiles     int       `json:"added_files"`     // media files the catalog didn't know
	MovedFiles     int       `json:"moved_files"`     // tracked files found at a new path
	RestoredFiles  int       `json:"restored_files"`  // culled content put back
	CulledFiles    int       `json:"culled_files"`    // tracked files gone from disk
	RemovedFolders int       `json:"removed_folders"` // rows whose directory is gone
	Duplicates     int       `json:"duplicates"`      // copies of content tracked elsewhere, left alone
	BusyFolders    int       `json:"busy_folders"`    // skipped: an import or operation held them
	Errors         []string  `json:"errors"`          // at most maxRescanErrors
	Skipped        string    `json:"skipped,omitempty"` // why nothing was done at all
}

var ErrClosed = errors.New("staging: shutting down")

func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// OnChange registers fn to receive the status after every change. Call it
// before serving. fn runs with the service's lock held, so the events it
// publishes stay in order; it must not block or call back into s.
func (s *Service) OnChange(fn func(Status)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notify = fn
}

// Touch records a change to staging made elsewhere (an import finished).
func (s *Service) Touch() { s.changed() }

func (s *Service) changed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status.Version++
	s.notifyLocked()
}

func (s *Service) notifyLocked() {
	if s.notify != nil {
		s.notify(s.status)
	}
}

// removed reports hashes whose files are gone for good.
func (s *Service) removed(shas []string) {
	if len(shas) > 0 && s.removedFn != nil {
		s.removedFn(shas)
	}
}

// Close stops a running promote or rescan between files and waits for it.
// Calling it again does nothing.
func (s *Service) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	s.bgCancel()
	s.wg.Wait()
}

func (s *Service) folderDir(name string) string { return filepath.Join(s.photosRoot, Dir, name) }

// abs is the OS path of rel_path rel.
func (s *Service) abs(rel string) string { return filepath.Join(s.photosRoot, filepath.FromSlash(rel)) }

// relOf is OS path p as a rel_path; an error when p is outside the photos root.
func (s *Service) relOf(p string) (string, error) {
	rel, err := filepath.Rel(s.photosRoot, p)
	if err != nil || !filepath.IsLocal(rel) {
		return "", fmt.Errorf("%s is not inside the photos share %s", p, s.photosRoot)
	}
	return filepath.ToSlash(rel), nil
}

// safeName rejects a catalog folder name that isn't a single path element.
func safeName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("staging folder name %q is not a single folder name", name)
	}
	return nil
}
```

In `Files`, replace the inline name check with `if err := safeName(row.Name); err != nil { return FolderFiles{}, fmt.Errorf("staging folder %d: %w", id, err) }`. (`Status.LastRescan` and `RescanResult` are declared here so Tasks 8 and 11 compile against one shape.)

- [ ] **Step 4: `internal/staging/names.go`**

```go
package staging

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

var (
	ErrBadTitle   = errors.New("staging: bad folder title")
	ErrBadPattern = errors.New("staging: bad promote pattern")
	ErrBadRequest = errors.New("staging: bad request")
)

// MaxNameBytes caps a folder name, and each element of a promote target,
// well below every filesystem's 255.
const MaxNameBytes = 200

// DefaultPattern promotes into the archive root, matching its flat layout.
const DefaultPattern = "{name}"

// badNameChars can't appear in a folder name: "/" separates paths, and the
// rest are what Windows refuses, so the owner's SMB clients can open it.
const badNameChars = `/\<>:"|?*`

// checkName says why s can't be a folder name, or returns nil.
func checkName(s string) error {
	switch {
	case s == "":
		return errors.New("is empty")
	case s == "." || s == "..":
		return errors.New("is not a folder name")
	case strings.HasPrefix(s, "."):
		return errors.New("starts with a dot (it would be hidden)")
	case strings.HasSuffix(s, ".") || strings.HasSuffix(s, " "):
		return errors.New("ends with a dot or a space, which Windows can't use")
	case len(s) > MaxNameBytes:
		return fmt.Errorf("is longer than %d bytes", MaxNameBytes)
	case !utf8.ValidString(s):
		return errors.New("is not valid UTF-8")
	}
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(badNameChars, r) {
			return fmt.Errorf("contains %q, which can't be used in a folder name", r)
		}
	}
	return nil
}

// CleanTitle trims a folder title and checks it can be part of a folder
// name. An empty title is fine: the folder is then named by its date.
func CleanTitle(title string) (string, error) {
	t := strings.TrimSpace(title)
	if t == "" {
		return "", nil
	}
	if err := checkName(t); err != nil {
		return "", fmt.Errorf("%w: the title %v", ErrBadTitle, err)
	}
	return t, nil
}

// FolderName joins a date and a title the archive's way (§8):
// "2007-11-10 - SMU Homecoming", or just the date, or just the title.
func FolderName(date, title string) string {
	switch {
	case date == "":
		return title
	case title == "":
		return date
	}
	return date + " - " + title
}

// folderDate is the date a folder is filed under: its name's leading date,
// else the earliest date it covers (Dates is sorted), else "".
func folderDate(f catalog.StagingFolder) string {
	if d, _ := SplitName(f.Name); d != "" {
		return d
	}
	if len(f.Dates) > 0 {
		return f.Dates[0]
	}
	return ""
}

var tokenRE = regexp.MustCompile(`\{[^{}]*\}`)

// ExpandPattern returns the folder below Archive/ that f is promoted to, as
// a slash path. Tokens: {name}, {year}, {month} (from folderDate). Every
// element of the result must be a usable folder name.
func ExpandPattern(pattern string, f catalog.StagingFolder) (string, error) {
	p := strings.TrimSpace(pattern)
	if p == "" {
		p = DefaultPattern
	}
	date := folderDate(f)
	var bad error
	out := tokenRE.ReplaceAllStringFunc(p, func(tok string) string {
		switch tok {
		case "{name}":
			return f.Name
		case "{year}", "{month}":
			if date == "" {
				bad = fmt.Errorf("%s needs a date, and %q has none", tok, f.Name)
				return ""
			}
			if tok == "{year}" {
				return date[:4]
			}
			return date[5:7]
		}
		bad = fmt.Errorf("unknown token %s (use {name}, {year} or {month})", tok)
		return ""
	})
	if bad != nil {
		return "", fmt.Errorf("%w: %v", ErrBadPattern, bad)
	}
	if strings.ContainsAny(out, "{}") {
		return "", fmt.Errorf("%w: unbalanced braces in %q", ErrBadPattern, pattern)
	}
	for _, el := range strings.Split(out, "/") {
		if err := checkName(el); err != nil {
			return "", fmt.Errorf("%w: in %q a folder name %v", ErrBadPattern, out, err)
		}
	}
	return out, nil
}
```

- [ ] **Step 5: `internal/staging/fsops.go` and the EXDEV check**

`xdev_other.go`:

```go
//go:build !windows

package staging

import (
	"errors"
	"syscall"
)

// crossDevice reports a rename that would cross filesystems (§13).
func crossDevice(err error) bool { return errors.Is(err, syscall.EXDEV) }
```

`xdev_windows.go`:

```go
//go:build windows

package staging

import (
	"errors"
	"syscall"
)

// errNotSameDevice is ERROR_NOT_SAME_DEVICE: MoveFileEx can't move a file
// to another volume.
const errNotSameDevice = syscall.Errno(17)

// crossDevice reports a rename that would cross filesystems (§13).
func crossDevice(err error) bool {
	return errors.Is(err, syscall.EXDEV) || errors.Is(err, errNotSameDevice)
}
```

`fsops.go`:

```go
package staging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// maxKept is how many leftover paths an answer names.
const maxKept = 20

// isJunk reports files that never keep a folder alive: what Windows and
// macOS leave behind over SMB, and stale partial copies (an import only
// writes partials into folders whose lock it holds).
func isJunk(name string) bool {
	switch strings.ToLower(name) {
	case ".ds_store", "thumbs.db", "desktop.ini":
		return true
	}
	return strings.HasPrefix(name, "._") || strings.HasSuffix(name, media.PartialSuffix)
}

// folderFiles lists the regular files below dir as sorted slash paths
// relative to dir, without junk. Symlinks and other special files are not
// listed and stay where they are. A missing dir has no files.
func folderFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if p == dir && errors.Is(err, fs.ErrNotExist) {
				return fs.SkipAll
			}
			return err
		}
		if !d.Type().IsRegular() || isJunk(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", dir, err)
	}
	slices.Sort(out)
	return out, nil
}

// removeEmptyTree deletes dir when it holds nothing but folders and junk,
// deepest first; empty subfolders go even when dir stays. It reports
// whether dir is gone (a missing dir counts) and, when it isn't, up to
// maxKept of the paths (relative to dir) that kept it.
func removeEmptyTree(dir string) (gone bool, kept []string, err error) {
	var walk func(abs, rel string) (bool, error)
	walk = func(abs, rel string) (bool, error) {
		entries, err := os.ReadDir(abs)
		if errors.Is(err, fs.ErrNotExist) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		empty := true
		var junk []string
		for _, e := range entries {
			child, childRel := filepath.Join(abs, e.Name()), path.Join(rel, e.Name())
			switch {
			case e.IsDir():
				childGone, err := walk(child, childRel)
				if err != nil {
					return false, err
				}
				empty = empty && childGone
			case e.Type().IsRegular() && isJunk(e.Name()):
				junk = append(junk, child)
			default:
				empty = false
				if len(kept) < maxKept {
					kept = append(kept, childRel)
				}
			}
		}
		if !empty {
			return false, nil
		}
		for _, j := range junk {
			if err := os.Remove(j); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return false, err
			}
		}
		if err := os.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
		return true, nil
	}
	gone, err = walk(dir, "")
	if err != nil {
		return false, kept, fmt.Errorf("clean up %s: %w", dir, err)
	}
	return gone, kept, nil
}

// errRetryName: the name chosen for a copy was taken meanwhile; pick again.
var errRetryName = errors.New("destination taken during the copy")

// moveFile moves the regular file src into dir as name, or name-1, name-2…
// when that is taken by other content (CameraImport's collision loop,
// media.FreeName). When identical content is already there, src is removed
// and that path returned. A rename across filesystems falls back to copy,
// verify, delete (copyVerify). want is the hash src must have, "" when it
// isn't catalogued. It returns the new path.
func (s *Service) moveFile(ctx context.Context, src, dir, name, want string) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	for range 3 {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		dst, dup, err := media.FreeName(src, dir, name)
		if err != nil {
			return "", err
		}
		if dup {
			if err := os.Remove(src); err != nil {
				return "", fmt.Errorf("identical copy already at %s, but removing %s failed: %w", dst, src, err)
			}
			return dst, nil
		}
		err = s.rename(src, dst)
		if err == nil {
			return dst, nil
		}
		if !crossDevice(err) {
			return "", err
		}
		dst, err = s.copyVerify(ctx, src, dst, want)
		if errors.Is(err, errRetryName) {
			continue
		}
		return dst, err
	}
	return "", fmt.Errorf("move %s: destination names in %s kept being taken", src, dir)
}

// copyVerify copies src to dst (media.CopyHashed: a partial, fsync, rename,
// never overwriting), hashes dst again, and removes src only when the copy
// matches what was read and, if want isn't "", the catalog's hash.
func (s *Service) copyVerify(ctx context.Context, src, dst, want string) (string, error) {
	sum, err := media.CopyHashed(ctx, src, dst)
	if err != nil {
		switch media.ClassifyCopyError(err, src, dst) {
		case media.CopyFailDestTaken:
			return "", errRetryName
		case media.CopyFailStalePartial:
			// We hold the folder's lock (in Archive/: the only promote job),
			// so this partial is left over from an interrupted copy.
			if rmErr := os.Remove(dst + media.PartialSuffix); rmErr != nil {
				return "", errors.Join(err, rmErr)
			}
			s.log.Warn("removed a stale partial", "path", dst+media.PartialSuffix)
			return "", errRetryName
		}
		return "", err
	}
	got, err := hashFile(ctx, dst)
	if err == nil && (got != sum || (want != "" && got != want)) {
		err = fmt.Errorf("the copy of %s does not match (read %s, copy %s, catalog %s)", src, sum, got, want)
	}
	if err != nil {
		if rmErr := os.Remove(dst); rmErr != nil {
			err = errors.Join(err, rmErr)
		}
		return "", err
	}
	if err := os.Remove(src); err != nil {
		return "", fmt.Errorf("copied and verified %s, but removing the original failed: %w", dst, err)
	}
	return dst, nil
}

// hashFile is media.HashFile that stops when ctx ends, so shutdown never
// waits for a multi-gigabyte video to be read.
func hashFile(ctx context.Context, p string) (sum string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, ctxReader{ctx, f}); err != nil {
		return "", fmt.Errorf("hash %s: %w", p, err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

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

// syncDirs makes the renames and removals in dirs durable (Decision 16).
// Folders that no longer exist are skipped.
func syncDirs(dirs map[string]bool) error {
	var errs []error
	for d := range dirs {
		if _, err := os.Stat(d); errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err := media.SyncDir(d); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

- [ ] **Step 6: Run the tests**

`go test ./internal/staging/` → PASS (the M3 tests too). Then `gofmt -l .`, `go vet ./...`, `go test ./...` and the Linux command → PASS. Cross-compile check: `GOOS=linux go vet ./internal/staging/` and `GOOS=windows go vet ./internal/staging/`.

- [ ] **Step 7: Commit**

```bash
git add internal/staging/
git commit -m "Add the staging service config, change notifications, names and safe moves

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Rename and merge

Suggested implementer: sonnet.

**Files:**
- Create: `internal/staging/ops.go`, `internal/staging/ops_test.go`

**Interfaces:**
- Consumes: Task 4's service internals (`opsEnv`, `moveFile`, `removeEmptyTree`, `folderFiles`, `syncDirs`, `checkName`, `safeName`, `folderDate`, `CleanTitle`, `FolderName`, `s.changed`, `s.removed`, `s.relOf`); `Locks.TryLock`, `BusyError` (Task 3); `catalog.DB.StagingFolderByID`, `StagingFolders`, `AssetsUnder`, `RenameStagingFolder`, `SetStagingFolderInfo`, `DeleteStagingFolder`, `MoveAsset`, `CullAsset`, `ErrNameTaken`, `ErrNotFound` (Task 2); `Service.Files` (M3).
- Produces:

```go
func (s *Service) Rename(ctx context.Context, id int64, title string) (Folder, error)
func (s *Service) Merge(ctx context.Context, ids []int64, title string) (Folder, error)
```

Unexported, used by Tasks 6, 7, 10: `s.lockFolders(ctx, ids) ([]catalog.StagingFolder, func(), error)`, `s.lockRows(h, rows...)`, `s.nameFree(ctx, name, self...)`, `s.renameFolder(ctx, row, name, touched)`, `s.summary(ctx, id)`, `prefixOf(name)`, `earliestDate(rows)`, `unionDates(rows...)`, `oldestCreated(rows)`.

- [ ] **Step 1: Write the failing tests** in `internal/staging/ops_test.go` (package `staging`, `newOpsEnv`). Each test also checks that the folders' locks are free afterwards (`e.locks.TryLock(HolderRescan, id)` succeeds) and, for a change, that `e.version()` went up.

Rename:
- `TestRenameKeepsDatePrefix`: `f := e.folder(t, "2025-05-30", "2025-05-30")` with `a.JPG` and `Capture/a.ARW` staged → `Rename(ctx, f.ID, "  Trip ")` → returned `Folder.Name` is `2025-05-30 - Trip`, `Files` 2; on disk `Staging/2025-05-30 - Trip/a.JPG` and `…/Capture/a.ARW`, no `Staging/2025-05-30`; both assets' `RelPath` moved.
- `TestRenameEmptyTitleKeepsDate`: `2025-05-30 - Trip` + `""` → `2025-05-30`.
- `TestRenameNoDatePrefix`: an adopted `Holiday` with dates `2025-06-01` + `Beach` → `2025-06-01 - Beach`; `Holiday` with no dates + `Beach` → `Beach`; with no dates and `""` → `ErrBadTitle`.
- `TestRenameNameTaken` (Review Focus 4): another row `2025-05-30 - Trip` → `Rename(f, "Trip")` is `catalog.ErrNameTaken`; with only an untracked directory `Staging/2025-05-30 - Trip/` → also `ErrNameTaken`; nothing moved on disk or in the catalog.
- `TestRenameBadTitle`: `a/b`, `x:y`, `.hidden` → `ErrBadTitle`, nothing changed.
- `TestRenameBusy` (Review Focus 1): `e.locks.TryLock(HolderIngest, f.ID)` held → `Rename` returns a `*BusyError` with `Holder == HolderIngest` and `Name == "2025-05-30"`; nothing changed.
- `TestRenameMissingDir`: a row without a directory → renamed in the catalog only, no directory created.
- `TestRenameNonASCII` (Review Focus 4): `2025-05-30` → `Zürich` → `Zug`: paths are right after both.
- `TestRenameUnknown`: id 999 → `catalog.ErrNotFound`.
- `TestRenameCaseOnly`: `2025-05-30 - trip` → `Trip`: succeeds on every OS (on a case-insensitive disk the existing directory is the folder itself).

Merge:
- `TestMergeMovesFilesAndRemovesSources` (Review Focus 2): `A = 2025-05-30` (staged `a.JPG`, `Capture/a.ARW`; untracked `notes.txt`; junk `.DS_Store`), `B = 2025-05-31` (staged `Videos/v.MP4`; staged `Capture/a.ARW` with *different* content) → `Merge(ctx, [B, A], "Trip")` → result `2025-05-30 - Trip` with id A.ID; on disk: `a.JPG`, `Capture/a.ARW`, `Capture/a-1.ARW` (B's), `Videos/v.MP4`, `notes.txt` under `Staging/2025-05-30 - Trip/`; `Staging/2025-05-31` is gone; B's row is gone (`StagingFolderByID` → `ErrNotFound`); every asset's `RelPath` matches the disk; dates `["2025-05-30","2025-05-31"]`.
- `TestMergePicksFolderWithFinalName`: `2025-05-30` (id 1) + `2025-05-30 - Dinner` (id 2), title `Dinner` → the result has id 2 and name `2025-05-30 - Dinner`; folder 1's files are inside it and `Staging/2025-05-30` is gone.
- `TestMergeIdenticalFile`: both folders hold an untracked `notes.txt` with the same content → one copy left, the source removed.
- `TestMergeCullsMissingTracked`: a staged asset of B whose file was deleted by hand → culled (`e.asset(t, sha).State == StateCulled`), its hash reported through `Removed`.
- `TestMergeOldestCreated`: B's `created_at` is older (set with `SetStagingFolderInfo`) → the merged folder has B's `created_at`.
- `TestMergeNeedsTwo`: one id (or the same id twice) → `ErrBadRequest`.
- `TestMergeNameTaken`: a third folder (not merged) already named `2025-05-30 - Trip` → `ErrNameTaken`, nothing moved.
- `TestMergeBusy`: B locked by an import → `*BusyError` naming B, nothing moved, A's lock free afterwards.
- `TestMergeKeepsSourceWithSpecialFile` (skip on Windows: symlinks need privileges): a symlink in B → files moved, B's row and directory stay (the symlink is never moved or deleted), and `Merge` returns no error.

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/staging/ -run 'Rename|Merge'` → build failure.

- [ ] **Step 3: Implement `internal/staging/ops.go`**

```go
package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// prefixOf is the rel_path prefix of the files in folder name.
func prefixOf(name string) string { return Dir + "/" + name + "/" }

// rows reads folders ids, refusing names that aren't one path element.
func (s *Service) rows(ctx context.Context, ids []int64) ([]catalog.StagingFolder, error) {
	out := make([]catalog.StagingFolder, 0, len(ids))
	for _, id := range ids {
		row, err := s.db.StagingFolderByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if err := safeName(row.Name); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}

// lockRows takes the locks of rows for one operation, or returns the
// *BusyError of the first busy one with its name filled in.
func (s *Service) lockRows(h Holder, rows ...catalog.StagingFolder) (func(), error) {
	ids := make([]int64, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	release, err := s.locks.TryLock(h, ids...)
	var busy *BusyError
	if errors.As(err, &busy) {
		for _, r := range rows {
			if r.ID == busy.ID {
				busy.Name = r.Name
			}
		}
	}
	return release, err
}

// lockFolders reads, locks and reads again folders ids (an operation that
// finished just before the lock may have renamed one). The caller releases.
func (s *Service) lockFolders(ctx context.Context, ids []int64) ([]catalog.StagingFolder, func(), error) {
	rows, err := s.rows(ctx, ids)
	if err != nil {
		return nil, nil, err
	}
	release, err := s.lockRows(HolderOperation, rows...)
	if err != nil {
		return nil, nil, err
	}
	rows, err = s.rows(ctx, ids)
	if err != nil {
		release()
		return nil, nil, err
	}
	return rows, release, nil
}

// summary is folder id as the UI shows it.
func (s *Service) summary(ctx context.Context, id int64) (Folder, error) {
	ff, err := s.Files(ctx, id, "")
	if err != nil {
		return Folder{}, err
	}
	return ff.Folder, nil
}

// nameFree returns catalog.ErrNameTaken when a folder row other than self,
// or a directory in Staging/ that isn't one of self's, already has name. On
// a case-insensitive disk a case-only rename finds the folder itself, which
// doesn't count.
func (s *Service) nameFree(ctx context.Context, name string, self ...catalog.StagingFolder) error {
	mine := map[int64]bool{}
	for _, r := range self {
		mine[r.ID] = true
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return err
	}
	for _, r := range all {
		if r.Name == name && !mine[r.ID] {
			return fmt.Errorf("%w: %q", catalog.ErrNameTaken, name)
		}
	}
	info, err := os.Lstat(s.folderDir(name))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, r := range self {
		if own, err := os.Lstat(s.folderDir(r.Name)); err == nil && os.SameFile(info, own) {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (a folder by that name is already in %s/)", catalog.ErrNameTaken, name, Dir)
}

// renameFolder renames row's directory, when it has one, then its row and
// its assets' paths. The caller holds row's lock and checked nameFree.
func (s *Service) renameFolder(ctx context.Context, row catalog.StagingFolder, name string, touched map[string]bool) error {
	from, to := s.folderDir(row.Name), s.folderDir(name)
	switch _, err := os.Lstat(from); {
	case err == nil:
		if err := s.rename(from, to); err != nil {
			return fmt.Errorf("rename %s/%s: %w", Dir, row.Name, err)
		}
		touched[filepath.Join(s.photosRoot, Dir)] = true
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := s.db.RenameStagingFolder(ctx, row.ID, name, prefixOf(row.Name), prefixOf(name), s.now()); err != nil {
		return fmt.Errorf("renamed %s/%s to %q but could not record it (a rescan repairs this): %w", Dir, row.Name, name, err)
	}
	return nil
}

// Rename gives folder id the title, keeping its date prefix (§8):
// "2025-05-30" with "Trip" becomes "2025-05-30 - Trip"; an empty title
// leaves the date. A name without a date takes the folder's earliest date.
func (s *Service) Rename(ctx context.Context, id int64, title string) (Folder, error) {
	t, err := CleanTitle(title)
	if err != nil {
		return Folder{}, err
	}
	rows, release, err := s.lockFolders(ctx, []int64{id})
	if err != nil {
		return Folder{}, err
	}
	defer release()
	row := rows[0]
	name := FolderName(folderDate(row), t)
	if name == "" {
		return Folder{}, fmt.Errorf("%w: the folder has no date, so it needs a title", ErrBadTitle)
	}
	if err := checkName(name); err != nil {
		return Folder{}, fmt.Errorf("%w: %q %v", ErrBadTitle, name, err)
	}
	if name != row.Name {
		if err := s.nameFree(ctx, name, row); err != nil {
			return Folder{}, err
		}
		defer s.changed()
		touched := map[string]bool{}
		if err := s.renameFolder(ctx, row, name, touched); err != nil {
			return Folder{}, err
		}
		if err := syncDirs(touched); err != nil {
			return Folder{}, err
		}
	}
	return s.summary(ctx, id)
}

// earliestDate is the earliest folderDate among rows, "" when none has one.
func earliestDate(rows []catalog.StagingFolder) string {
	best := ""
	for _, r := range rows {
		if d := folderDate(r); d != "" && (best == "" || d < best) {
			best = d
		}
	}
	return best
}

// unionDates is every date the rows cover, plus extra.
func unionDates(rows []catalog.StagingFolder, extra ...string) []string {
	out := slices.Clone(extra)
	for _, r := range rows {
		out = append(out, r.Dates...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// oldestCreated is the earliest created_at among rows.
func oldestCreated(rows []catalog.StagingFolder) time.Time {
	var t time.Time
	for _, r := range rows {
		if t.IsZero() || r.CreatedAt.Before(t) {
			t = r.CreatedAt
		}
	}
	return t
}

// mergeTarget is the folder a merge into name keeps: the one already named
// name, else the one with the earliest date (lowest id on a tie; folders
// with no date last).
func mergeTarget(rows []catalog.StagingFolder, name string) catalog.StagingFolder {
	for _, r := range rows {
		if r.Name == name {
			return r
		}
	}
	best := rows[0]
	for _, r := range rows[1:] {
		bd, rd := folderDate(best), folderDate(r)
		switch {
		case bd == "" && rd != "", rd != "" && rd < bd, rd == bd && r.ID < best.ID:
			best = r
		}
	}
	return best
}

// Merge moves every file of folders ids into one folder named for the
// earliest date among them and title, then removes the emptied sources
// (§8). Files keep their path inside the folder; clashes get -1, -2.
func (s *Service) Merge(ctx context.Context, ids []int64, title string) (Folder, error) {
	ids = uniqueIDs(ids)
	if len(ids) < 2 {
		return Folder{}, fmt.Errorf("%w: merging needs at least two folders", ErrBadRequest)
	}
	t, err := CleanTitle(title)
	if err != nil {
		return Folder{}, err
	}
	rows, release, err := s.lockFolders(ctx, ids)
	if err != nil {
		return Folder{}, err
	}
	defer release()
	name := FolderName(earliestDate(rows), t)
	if name == "" {
		return Folder{}, fmt.Errorf("%w: these folders have no date, so the merged folder needs a title", ErrBadTitle)
	}
	if err := checkName(name); err != nil {
		return Folder{}, fmt.Errorf("%w: %q %v", ErrBadTitle, name, err)
	}
	if err := s.nameFree(ctx, name, rows...); err != nil {
		return Folder{}, err
	}
	target := mergeTarget(rows, name)
	defer s.changed()
	touched := map[string]bool{}
	for _, src := range rows {
		if src.ID == target.ID {
			continue
		}
		if err := s.mergeInto(ctx, src, target, touched); err != nil {
			return Folder{}, fmt.Errorf("merge %q into %q: %w", src.Name, target.Name, err)
		}
	}
	if err := s.db.SetStagingFolderInfo(ctx, target.ID, unionDates(rows), oldestCreated(rows)); err != nil {
		return Folder{}, err
	}
	if target.Name != name {
		if err := s.renameFolder(ctx, target, name, touched); err != nil {
			return Folder{}, err
		}
	}
	if err := syncDirs(touched); err != nil {
		return Folder{}, err
	}
	return s.summary(ctx, target.ID)
}

// mergeInto moves every file of src, catalogued or not, to the same path
// inside dst, then removes src when nothing is left. Catalogued files whose
// file is already gone are culled, as a rescan would.
func (s *Service) mergeInto(ctx context.Context, src, dst catalog.StagingFolder, touched map[string]bool) error {
	srcPrefix := prefixOf(src.Name)
	assets, err := s.db.AssetsUnder(ctx, srcPrefix, catalog.StateStaged)
	if err != nil {
		return err
	}
	tracked := map[string]catalog.Asset{} // path inside the folder → asset
	for _, a := range assets {
		tracked[strings.TrimPrefix(a.RelPath, srcPrefix)] = a
	}
	files, err := folderFiles(s.folderDir(src.Name))
	if err != nil {
		return err
	}
	for _, rel := range files {
		a, isTracked := tracked[rel]
		from := filepath.Join(s.folderDir(src.Name), filepath.FromSlash(rel))
		dir := filepath.Join(s.folderDir(dst.Name), filepath.FromSlash(path.Dir(rel)))
		to, err := s.moveFile(ctx, from, dir, path.Base(rel), a.SHA256)
		if err != nil {
			return err
		}
		touched[filepath.Dir(from)], touched[filepath.Dir(to)] = true, true
		if !isTracked {
			continue
		}
		delete(tracked, rel)
		newRel, err := s.relOf(to)
		if err == nil {
			err = s.db.MoveAsset(ctx, a.SHA256, newRel, s.now())
		}
		if err != nil {
			return fmt.Errorf("moved %s but could not record it (a rescan repairs this): %w", rel, err)
		}
	}
	if err := s.cullGone(ctx, tracked); err != nil {
		return err
	}
	gone, kept, err := removeEmptyTree(s.folderDir(src.Name))
	if err != nil {
		return err
	}
	if !gone {
		// Only special files (symlinks, devices) are never moved.
		s.log.Warn("merged folder kept: it holds files that were not moved", "folder", src.Name, "kept", kept)
		return nil
	}
	touched[filepath.Join(s.photosRoot, Dir)] = true
	return s.db.DeleteStagingFolder(ctx, src.ID)
}

// cullGone culls catalogued files whose file is no longer on disk.
func (s *Service) cullGone(ctx context.Context, gone map[string]catalog.Asset) error {
	var shas []string
	for _, a := range gone {
		s.log.Warn("culling a staged file that is no longer on disk", "path", a.RelPath)
		if err := s.db.CullAsset(ctx, a.SHA256, s.now()); err != nil {
			s.removed(shas)
			return err
		}
		shas = append(shas, a.SHA256)
	}
	s.removed(shas)
	return nil
}
```

Note `mergeTarget`'s tie rule: `rd == bd && r.ID < best.ID` also covers two dateless folders.

- [ ] **Step 4: Run the tests**: `go test ./internal/staging/` → PASS; then `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/staging/ops.go internal/staging/ops_test.go
git commit -m "Rename and merge staging folders

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: Move files, cull files and discard folders

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/staging/ops.go`, `internal/staging/ops_test.go`

**Interfaces:**
- Consumes: Task 5's helpers; `catalog.DB.AssetsByHash`, `CreateStagingFolder`, `Settings`; `media.FolderDate`.
- Produces:

```go
const MaxFiles = 20000 // hashes one move or cull may name
var ErrChanged = errors.New("staging: files moved while the request was being made; reload and try again")

type MoveRequest struct {
	SHAs     []string
	ToID     int64  // an existing folder…
	NewTitle string // …or a new folder "<earliest source date> - NewTitle" (exactly one of the two)
}
type MoveResult struct {
	Folder  Folder  `json:"folder"`  // the destination
	Moved   int     `json:"moved"`
	Removed []int64 `json:"removed"` // source folders left empty and removed
}
type CullResult struct {
	Culled  int     `json:"culled"`
	Removed []int64 `json:"removed"`
}
type DiscardResult struct {
	Culled  int      `json:"culled"`
	Removed bool     `json:"removed"` // false: untracked files kept the folder (listed in Kept)
	Kept    []string `json:"kept"`
}
func (s *Service) Move(ctx context.Context, req MoveRequest) (MoveResult, error)
func (s *Service) Cull(ctx context.Context, shas []string) (CullResult, error)
func (s *Service) Discard(ctx context.Context, id int64) (DiscardResult, error)
```

Unexported, used by Tasks 7, 10: `checkSHAs`, `s.locate(ctx, shas)`, `s.dropIfEmpty(ctx, row, touched) (bool, error)`.

- [ ] **Step 1: Write the failing tests** (append to `ops_test.go`; captures at 2025-05-30 10:00 local, `at(10)` from `staging_test.go`):
- `TestMoveToExistingFolder`: A `2025-05-30` holds `a.JPG`, `Capture/a.ARW`, `b.JPG`; B `2025-06-01 - Beach` → `Move({SHAs: [a.JPG, a.ARW], ToID: B})` → `Staging/2025-06-01 - Beach/a.JPG` and `…/Capture/a.ARW`; `Moved` 2; `Removed` empty (A still has b.JPG); B's dates now include `2025-05-30`; A's dates unchanged; result `Folder.ID == B`.
- `TestMoveDatesUseDayCutoff`: a file captured 2025-05-31 01:30 local moved into B adds `2025-05-30` (default cutoff 04:00); a file with no capture time adds the source folder's date.
- `TestMoveLastFilesRemovesSource`: moving A's every staged file (A also has `.DS_Store` and an empty `Videos/`) → A's directory and row gone, `Removed == [A.ID]`.
- `TestMoveToNewFolder`: `Move({SHAs: [b.JPG], NewTitle: " Picks "})` → a new folder `2025-05-30 - Picks` holding `b.JPG`, dates `["2025-05-30"]`, `CreatedAt` = A's.
- `TestMoveNewFolderRules`: `NewTitle` blank → `ErrBadTitle`; both `ToID` and `NewTitle` → `ErrBadRequest`; neither → `ErrBadRequest`; a `NewTitle` whose name exists → `ErrNameTaken`; `ToID` 999 → `ErrNotFound`.
- `TestMoveCollision` (Review Focus 2): B already has a different `a.JPG` → lands as `a-1.JPG`, the asset's `RelPath` says so.
- `TestMoveToSameFolderIsNoop`: moving A's file to A → `Moved` 0, nothing changed.
- `TestMoveRejectsBadHashes`: an unknown hash, an archived asset's hash, a culled asset's hash, `"xyz"`, an empty list, `MaxFiles+1` hashes → `ErrBadRequest`; a duplicate hash in the list is moved once.
- `TestMoveBusyDestination`: B held by an import → `*BusyError{Holder: HolderIngest, Name: B.Name}`; nothing moved; A free afterwards.
- `TestMoveCrossDevice`: rename seam returns EXDEV → files copied, verified, originals gone, assets updated.
- `TestCullDeletesAndRemembers`: cull `a.JPG` → file gone; state culled, `RelPath` ""; `AssetsByFingerprint(shaOf("fp:"+content))` still finds it; `removedHashes()` holds its hash; version up; `Culled` 1, `Removed` empty.
- `TestCullLastFileRemovesFolder`: culling every staged file of A (plus junk present) → A gone, `Removed == [A.ID]`.
- `TestCullMissingFile`: a staged file already deleted by hand → still culled, no error.
- `TestCullAcrossFolders`: files from A and B in one call → both culled; both folders' locks taken (with B held by an import: `*BusyError`, nothing culled in A either).
- `TestDiscardRemovesFolder`: A with staged files, junk and empty subfolders → `Discard(A)` → `{Culled: 3, Removed: true, Kept: []}`, directory and row gone, hashes reported.
- `TestDiscardKeepsUntrackedFiles` (Review Focus 2, Decision 4): A also holds untracked `notes.txt` and an untracked `c.JPG` → `{Culled: n, Removed: false, Kept: ["c.JPG", "notes.txt"]}`; both files still on disk; the row stays (0 files).
- `TestDiscardBusy`: A held by a rescan → `*BusyError{Holder: HolderRescan}`; nothing deleted.

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/staging/ -run 'Move|Cull|Discard'` → build failure.

- [ ] **Step 3: Implement** (append to `ops.go`; add `"github.com/aspiziri/SD2NAS/internal/media"` to its imports):

```go
// MaxFiles is how many files one move or cull may name.
const MaxFiles = 20000

// ErrChanged: a file moved to a folder the request did not lock between
// the lookup and the lock.
var ErrChanged = errors.New("staging: files moved while the request was being made; reload and try again")

// checkSHAs de-duplicates shas and checks each is 64 lowercase hex digits.
func checkSHAs(shas []string) ([]string, error) {
	if len(shas) == 0 {
		return nil, fmt.Errorf("%w: no files given", ErrBadRequest)
	}
	if len(shas) > MaxFiles {
		return nil, fmt.Errorf("%w: at most %d files at once", ErrBadRequest, MaxFiles)
	}
	out := slices.Clone(shas)
	slices.Sort(out)
	out = slices.Compact(out)
	for _, h := range out {
		if len(h) != 64 || strings.Trim(h, "0123456789abcdef") != "" {
			return nil, fmt.Errorf("%w: %q is not a file hash", ErrBadRequest, h)
		}
	}
	return out, nil
}

// located is a staged file and the folder holding it.
type located struct {
	asset  catalog.Asset
	folder catalog.StagingFolder
	rel    string // path inside the folder
}

// locate finds staged files shas and their folders. A hash that isn't a
// staged file inside a staging folder is ErrBadRequest.
func (s *Service) locate(ctx context.Context, shas []string) ([]located, error) {
	byHash, err := s.db.AssetsByHash(ctx, shas)
	if err != nil {
		return nil, err
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return nil, err
	}
	byName := map[string]catalog.StagingFolder{}
	for _, r := range all {
		byName[r.Name] = r
	}
	out := make([]located, 0, len(shas))
	for _, h := range shas {
		a, ok := byHash[h]
		if !ok {
			return nil, fmt.Errorf("%w: no file %s", ErrBadRequest, h)
		}
		if a.State != catalog.StateStaged {
			return nil, fmt.Errorf("%w: %s is not in staging (it is %s)", ErrBadRequest, h, a.State)
		}
		name, rel, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/")
		row, known := byName[name]
		if !strings.HasPrefix(a.RelPath, Dir+"/") || !ok || !known {
			return nil, fmt.Errorf("%w: %s is not inside a staging folder", ErrBadRequest, a.RelPath)
		}
		out = append(out, located{asset: a, folder: row, rel: rel})
	}
	slices.SortFunc(out, func(a, b located) int { return strings.Compare(a.asset.RelPath, b.asset.RelPath) })
	return out, nil
}

// lockLocated locates shas, locks their folders plus extra, and locates
// again under the locks: a file that moved meanwhile is ErrChanged.
func (s *Service) lockLocated(ctx context.Context, shas []string, extra ...int64) ([]located, map[int64]catalog.StagingFolder, func(), error) {
	files, err := s.locate(ctx, shas)
	if err != nil {
		return nil, nil, nil, err
	}
	ids := slices.Clone(extra)
	for _, f := range files {
		ids = append(ids, f.folder.ID)
	}
	rows, release, err := s.lockFolders(ctx, uniqueIDs(ids))
	if err != nil {
		return nil, nil, nil, err
	}
	locked := map[int64]catalog.StagingFolder{}
	for _, r := range rows {
		locked[r.ID] = r
	}
	files, err = s.locate(ctx, shas)
	if err == nil {
		for _, f := range files {
			if _, ok := locked[f.folder.ID]; !ok {
				err = ErrChanged
				break
			}
		}
	}
	if err != nil {
		release()
		return nil, nil, nil, err
	}
	return files, locked, release, nil
}

// dropIfEmpty removes row's directory and row when no staged file is left
// in it and only junk remains on disk. It reports whether it removed them.
func (s *Service) dropIfEmpty(ctx context.Context, row catalog.StagingFolder, touched map[string]bool) (bool, error) {
	left, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
	if err != nil || len(left) > 0 {
		return false, err
	}
	gone, _, err := removeEmptyTree(s.folderDir(row.Name))
	if err != nil || !gone {
		return false, err
	}
	touched[filepath.Join(s.photosRoot, Dir)] = true
	return true, s.db.DeleteStagingFolder(ctx, row.ID)
}

// moveDate is the folder date a moved file adds to its destination: its
// capture date with today's day cutoff, else its source folder's date.
func moveDate(a catalog.Asset, src catalog.StagingFolder, cutoff time.Duration) string {
	if !a.CaptureTime.IsZero() {
		return media.FolderDate(a.CaptureTime, cutoff)
	}
	return folderDate(src)
}

// Move moves staged files into an existing folder or a new one (§8 "Move
// files"), keeping each file's path inside its folder. Source folders left
// with no files are removed.
func (s *Service) Move(ctx context.Context, req MoveRequest) (MoveResult, error) {
	shas, err := checkSHAs(req.SHAs)
	if err != nil {
		return MoveResult{}, err
	}
	title := strings.TrimSpace(req.NewTitle)
	switch {
	case req.ToID > 0 && title != "", req.ToID <= 0 && req.NewTitle == "":
		return MoveResult{}, fmt.Errorf("%w: give either a destination folder or a new folder's title", ErrBadRequest)
	case req.ToID <= 0:
		if title, err = CleanTitle(title); err != nil {
			return MoveResult{}, err
		}
		if title == "" {
			return MoveResult{}, fmt.Errorf("%w: a new folder needs a title", ErrBadTitle)
		}
	}
	var extra []int64
	if req.ToID > 0 {
		extra = append(extra, req.ToID)
	}
	files, locked, release, err := s.lockLocated(ctx, shas, extra...)
	if err != nil {
		return MoveResult{}, err
	}
	defer release()
	settings, err := s.db.Settings(ctx)
	if err != nil {
		return MoveResult{}, err
	}
	var sources []catalog.StagingFolder
	for id, r := range locked {
		if id != req.ToID {
			sources = append(sources, r)
		}
	}
	slices.SortFunc(sources, func(a, b catalog.StagingFolder) int { return strings.Compare(a.Name, b.Name) })

	defer s.changed()
	dest, ok := locked[req.ToID]
	if !ok { // a new folder
		name := FolderName(earliestDate(sources), title)
		if err := checkName(name); err != nil {
			return MoveResult{}, fmt.Errorf("%w: %q %v", ErrBadTitle, name, err)
		}
		if err := s.nameFree(ctx, name); err != nil {
			return MoveResult{}, err
		}
		if dest, err = s.db.CreateStagingFolder(ctx, name, nil, oldestCreated(sources)); err != nil {
			return MoveResult{}, err
		}
		releaseNew, err := s.lockRows(HolderOperation, dest) // nobody else knows it yet
		if err != nil {
			return MoveResult{}, err
		}
		defer releaseNew()
		if err := os.MkdirAll(s.folderDir(name), 0o777); err != nil {
			return MoveResult{}, err
		}
	}
	res := MoveResult{Removed: []int64{}}
	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true}
	dates := slices.Clone(dest.Dates)
	for _, f := range files {
		if f.folder.ID == dest.ID {
			continue
		}
		from := s.abs(f.asset.RelPath)
		dir := filepath.Join(s.folderDir(dest.Name), filepath.FromSlash(path.Dir(f.rel)))
		to, err := s.moveFile(ctx, from, dir, path.Base(f.rel), f.asset.SHA256)
		if err != nil {
			return res, fmt.Errorf("moved %d of %d files: %w", res.Moved, len(files), err)
		}
		touched[filepath.Dir(from)], touched[filepath.Dir(to)] = true, true
		newRel, err := s.relOf(to)
		if err == nil {
			err = s.db.MoveAsset(ctx, f.asset.SHA256, newRel, s.now())
		}
		if err != nil {
			return res, fmt.Errorf("moved %s but could not record it (a rescan repairs this): %w", f.asset.RelPath, err)
		}
		res.Moved++
		if d := moveDate(f.asset, f.folder, settings.DayStartsAt); d != "" {
			dates = append(dates, d)
		}
	}
	if err := s.db.SetStagingFolderInfo(ctx, dest.ID, dates, time.Time{}); err != nil {
		return res, err
	}
	for _, src := range sources {
		removed, err := s.dropIfEmpty(ctx, src, touched)
		if err != nil {
			return res, err
		}
		if removed {
			res.Removed = append(res.Removed, src.ID)
		}
	}
	if err := syncDirs(touched); err != nil {
		return res, err
	}
	res.Folder, err = s.summary(ctx, dest.ID)
	return res, err
}

// Cull deletes staged files and remembers them as culled (§6), so the card
// never brings them back. Folders left with no files are removed.
func (s *Service) Cull(ctx context.Context, shas []string) (CullResult, error) {
	shas, err := checkSHAs(shas)
	if err != nil {
		return CullResult{}, err
	}
	files, locked, release, err := s.lockLocated(ctx, shas)
	if err != nil {
		return CullResult{}, err
	}
	defer release()
	defer s.changed()
	res := CullResult{Removed: []int64{}}
	var culled []string
	defer func() { s.removed(culled) }()
	touched := map[string]bool{}
	for _, f := range files {
		p := s.abs(f.asset.RelPath)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("culled %d of %d files: %w", res.Culled, len(files), err)
		}
		touched[filepath.Dir(p)] = true
		if err := s.db.CullAsset(ctx, f.asset.SHA256, s.now()); err != nil {
			return res, fmt.Errorf("deleted %s but could not record it (a rescan repairs this): %w", f.asset.RelPath, err)
		}
		culled = append(culled, f.asset.SHA256)
		res.Culled++
	}
	for _, row := range sortedRows(locked) {
		removed, err := s.dropIfEmpty(ctx, row, touched)
		if err != nil {
			return res, err
		}
		if removed {
			res.Removed = append(res.Removed, row.ID)
		}
	}
	return res, syncDirs(touched)
}

func sortedRows(m map[int64]catalog.StagingFolder) []catalog.StagingFolder {
	out := make([]catalog.StagingFolder, 0, len(m))
	for _, r := range m {
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b catalog.StagingFolder) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// Discard culls every staged file of folder id and removes the folder. Files
// SD2NAS doesn't track are never deleted: they keep the folder, and Kept
// lists them (Decision 4).
func (s *Service) Discard(ctx context.Context, id int64) (DiscardResult, error) {
	rows, release, err := s.lockFolders(ctx, []int64{id})
	if err != nil {
		return DiscardResult{}, err
	}
	defer release()
	row := rows[0]
	assets, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
	if err != nil {
		return DiscardResult{}, err
	}
	defer s.changed()
	res := DiscardResult{Kept: []string{}}
	var culled []string
	defer func() { s.removed(culled) }()
	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true}
	for _, a := range assets {
		p := s.abs(a.RelPath)
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, fmt.Errorf("discarded %d of %d files: %w", res.Culled, len(assets), err)
		}
		touched[filepath.Dir(p)] = true
		if err := s.db.CullAsset(ctx, a.SHA256, s.now()); err != nil {
			return res, fmt.Errorf("deleted %s but could not record it (a rescan repairs this): %w", a.RelPath, err)
		}
		culled = append(culled, a.SHA256)
		res.Culled++
	}
	gone, kept, err := removeEmptyTree(s.folderDir(row.Name))
	if err != nil {
		return res, err
	}
	if gone {
		if err := s.db.DeleteStagingFolder(ctx, row.ID); err != nil {
			return res, err
		}
		res.Removed = true
	} else {
		res.Kept = kept
	}
	return res, syncDirs(touched)
}
```

- [ ] **Step 4: Run the tests**: `go test ./internal/staging/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/staging/ops.go internal/staging/ops_test.go
git commit -m "Move, cull and discard staged files

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Promote as a background job

Suggested implementer: sonnet (reviewer: opus — concurrency and data safety).

**Files:**
- Create: `internal/staging/promote.go`, `internal/staging/promote_test.go`
- Modify: `internal/staging/staging.go` (the job fields on `Service`)

**Interfaces:**
- Consumes: Tasks 3–6 (`TryLockEach`, `BusyError`, `rows`, `prefixOf`, `moveFile`, `folderFiles`, `removeEmptyTree`, `syncDirs`, `cullGone`, `ExpandPattern`, `DefaultPattern`, `crossDevice`, `s.wg`, `s.bgCtx`, `s.mu`, `s.closed`, `s.changed`); `catalog.DB.AssetsUnder`, `ArchiveFolder`, `ArchiveAsset`, `DeleteStagingFolder`.
- Produces:

```go
const ArchiveDir = "Archive"
var ErrPromoteRunning = errors.New("staging: a promote is already running")
const (
	PromoteRunning = "running" // job
	PromoteWaiting = "waiting" // folder
	PromoteMoving  = "moving"  // folder
	PromoteDone    = "done"    // folder or job
	PromoteFailed  = "failed"  // folder, or a job where some folder failed
)
type PromoteFolder struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Target string `json:"target"`           // "Archive/…"
	Files  int    `json:"files"`            // catalogued files when the job started
	Done   int    `json:"done"`             // of those, moved so far
	State  string `json:"state"`
	Merged bool   `json:"merged,omitempty"` // the target existed: merged file by file
	Copied bool   `json:"copied,omitempty"` // across filesystems: copied, verified, deleted
	Error  string `json:"error,omitempty"`
}
type PromoteJob struct {
	ID         int64           `json:"id"`
	State      string          `json:"state"`
	Pattern    string          `json:"pattern"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt time.Time       `json:"finished_at,omitzero"`
	FilesTotal int             `json:"files_total"`
	FilesDone  int             `json:"files_done"`
	BytesTotal int64           `json:"bytes_total"`
	BytesDone  int64           `json:"bytes_done"`
	Folders    []PromoteFolder `json:"folders"`
}
func (s *Service) Promote(ctx context.Context, ids []int64, pattern string) (PromoteJob, error) // validates, locks, starts; returns the first snapshot
func (s *Service) LastPromote() (PromoteJob, bool)  // the running or last job; false before the first
func (s *Service) OnPromote(fn func(PromoteJob))    // call before serving; fn runs under a lock and must not block
```

- [ ] **Step 1: Write the failing tests** in `internal/staging/promote_test.go` (`newOpsEnv`). Helpers:

```go
// waitPromote polls LastPromote until the job is no longer running.
func waitPromote(t *testing.T, s *Service) PromoteJob {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		j, ok := s.LastPromote()
		if ok && j.State != PromoteRunning {
			return j
		}
		if time.Now().After(deadline) {
			t.Fatalf("promote still running: %+v", j)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// blockRename makes s.rename wait for release when old ends with suffix.
func blockRename(s *Service, suffix string) (entered <-chan struct{}, release func()) { /* channels + sync.Once; then calls os.Rename */ }
```

Tests (Review Focus 5):
- `TestPromoteRenamesIntoArchive`: folder `2025-05-30 - Trip` with staged `a.JPG`, `Capture/a.ARW`, `Videos/v.MP4`, untracked `notes.txt` → `Promote(ctx, [id], "")` returns a job with `State: running`, `FilesTotal: 3`, the folder `waiting` and `Target: "Archive/2025-05-30 - Trip"`; `waitPromote` → `done`, `FilesDone: 3`, `BytesDone` = the sizes; on disk `Archive/2025-05-30 - Trip/a.JPG`, `…/Capture/a.ARW`, `…/Videos/v.MP4`, `…/notes.txt`, nothing under `Staging/`; the three assets are `archived` at those paths; the row is gone; the lock is free; `OnPromote` saw a first report `running` and a last `done`; the staging version went up.
- `TestPromotePattern`: `"{year}/{month}/{name}"` → `Archive/2025/05/2025-05-30 - Trip/…`.
- `TestPromoteMergesIntoExistingTarget` (Review Focus 2): `Archive/2025-05-30 - Trip/` already holds `Capture/a.ARW` (different content), `Selects/x.ARW` and `2025-05-30 - Trip.cosessiondb` → the folder ends `merged`, `Merged: true`; staged `Capture/a.ARW` lands as `Capture/a-1.ARW`; the existing three files are byte-for-byte unchanged; `Staging/2025-05-30 - Trip` and its row are gone.
- `TestPromoteIdenticalInTarget`: the archive already has the same content at `a.JPG` → the staging copy is removed and the asset is `archived` at `Archive/…/a.JPG`.
- `TestPromoteCrossDevice`: `e.svc.rename` always returns EXDEV → done, `Copied: true`, every file copied with its content and mtime, originals gone, assets archived.
- `TestPromoteCrossDeviceVerifyFails`: EXDEV, and a staged file whose content was changed on disk after it was recorded (the catalog hash no longer matches) → that folder `failed` with an error mentioning "does not match"; the job `failed`; the changed source file is still in Staging; no copy of it in Archive; the row stays; the lock is free.
- `TestPromoteCullsMissingTracked`: a staged file deleted by hand → culled before the move (`State == culled`, reported through `Removed`), the rest promoted.
- `TestPromoteBadPattern`: `"{day}"` → `ErrBadPattern`, no job started (`LastPromote` false), the folder's lock free.
- `TestPromoteBusy`: the folder held by an import → `*BusyError{Holder: HolderIngest, Name: …}`; no job.
- `TestPromoteOneAtATime`: `blockRename` on the first folder; `Promote([A])`; wait for `entered`; `Promote([B])` → `ErrPromoteRunning` and B's lock free; release; `waitPromote` done; `Promote([B])` now works.
- `TestPromoteReleasesEachFolder` (Review Focus 1): A and B in one job, `blockRename` on B; once B's rename is entered, A is done and `e.locks.TryLock(HolderRescan, A.ID)` succeeds while `TryLock(…, B.ID)` is busy; release; done.
- `TestPromoteStopsOnClose`: `blockRename` on A returning EXDEV after release (wrap: wait, then return EXDEV); `Promote([A])`; wait for `entered`; call `e.svc.Close()` in a goroutine; release → `Close` returns within 10 s; the job is `failed`; every file of A is still in `Staging/`; nothing under `Archive/` (no empty folder either).
- `TestPromoteProgressThrottled` (Review Focus 7): an existing target (merge mode) and 60 staged files → at most 8 `OnPromote` calls in all, and the last has `FilesDone: 60`, `State: done`.

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/staging/ -run Promote` → build failure.

- [ ] **Step 3: Add the job fields to `Service`** (in `staging.go`):

```go
	pmu        sync.Mutex // guards the promote fields below
	job        *PromoteJob
	jobSeq     int64
	promoting  bool
	promoteFn  func(PromoteJob)
	lastReport time.Time
```

- [ ] **Step 4: Implement `internal/staging/promote.go`**

```go
package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

// ArchiveDir is the archive under the photos root (§3).
const ArchiveDir = "Archive"

// promoteReportEvery throttles promote.progress (Decision 14).
const promoteReportEvery = 250 * time.Millisecond

var ErrPromoteRunning = errors.New("staging: a promote is already running")

const (
	PromoteRunning = "running"
	PromoteWaiting = "waiting"
	PromoteMoving  = "moving"
	PromoteDone    = "done"
	PromoteFailed  = "failed"
)

// …PromoteFolder and PromoteJob exactly as in Produces…

func (j *PromoteJob) clone() PromoteJob {
	c := *j
	c.Folders = slices.Clone(j.Folders)
	return c
}

// OnPromote registers fn to receive the job's progress. Call it before
// serving. fn runs with the job's lock held and must not block.
func (s *Service) OnPromote(fn func(PromoteJob)) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.promoteFn = fn
}

// LastPromote returns the running or last promote job.
func (s *Service) LastPromote() (PromoteJob, bool) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if s.job == nil {
		return PromoteJob{}, false
	}
	return s.job.clone(), true
}

// busyNamed fills a *BusyError's folder name from rows.
func busyNamed(err error, rows []catalog.StagingFolder) error {
	var busy *BusyError
	if errors.As(err, &busy) {
		for _, r := range rows {
			if r.ID == busy.ID {
				busy.Name = r.Name
			}
		}
	}
	return err
}

// Promote checks the request, locks every folder and starts moving them
// into Archive/<pattern> in the background (§8). Busy folders, a bad
// pattern or another running promote fail the request before anything
// moves. Each folder's lock is released as soon as it is done.
func (s *Service) Promote(ctx context.Context, ids []int64, pattern string) (PromoteJob, error) {
	ids = uniqueIDs(ids)
	if len(ids) == 0 {
		return PromoteJob{}, fmt.Errorf("%w: no folders given", ErrBadRequest)
	}
	pattern = strings.TrimSpace(pattern)
	if pattern == "" {
		pattern = DefaultPattern
	}
	s.pmu.Lock()
	if s.promoting {
		s.pmu.Unlock()
		return PromoteJob{}, ErrPromoteRunning
	}
	s.promoting = true
	s.pmu.Unlock()
	started := false
	defer func() {
		if !started {
			s.pmu.Lock()
			s.promoting = false
			s.pmu.Unlock()
		}
	}()

	rows, err := s.rows(ctx, ids)
	if err != nil {
		return PromoteJob{}, err
	}
	releases, err := s.locks.TryLockEach(HolderOperation, ids...)
	if err != nil {
		return PromoteJob{}, busyNamed(err, rows)
	}
	defer func() {
		if !started {
			for _, r := range releases {
				r()
			}
		}
	}()
	if rows, err = s.rows(ctx, ids); err != nil { // again, under the locks
		return PromoteJob{}, err
	}
	job := &PromoteJob{State: PromoteRunning, Pattern: pattern, StartedAt: s.now(), Folders: make([]PromoteFolder, len(rows))}
	for i, row := range rows {
		target, err := ExpandPattern(pattern, row)
		if err != nil {
			return PromoteJob{}, err
		}
		assets, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
		if err != nil {
			return PromoteJob{}, err
		}
		var bytes int64
		for _, a := range assets {
			bytes += a.Size
		}
		job.Folders[i] = PromoteFolder{ID: row.ID, Name: row.Name, Target: ArchiveDir + "/" + target, Files: len(assets), State: PromoteWaiting}
		job.FilesTotal += len(assets)
		job.BytesTotal += bytes
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return PromoteJob{}, ErrClosed
	}
	s.wg.Add(1)
	s.mu.Unlock()
	started = true
	s.pmu.Lock()
	s.jobSeq++
	job.ID = s.jobSeq
	s.job = job
	snap := job.clone()
	s.reportLocked(true)
	s.pmu.Unlock()
	go func() {
		defer s.wg.Done()
		s.runPromote(rows, releases)
	}()
	return snap, nil
}

// updateJob changes the job under its lock and reports it (throttled
// unless force).
func (s *Service) updateJob(force bool, fn func(*PromoteJob)) {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	fn(s.job)
	s.reportLocked(force)
}

func (s *Service) reportLocked(force bool) {
	now := time.Now() // the real clock: tests fix s.now
	if !force && now.Sub(s.lastReport) < promoteReportEvery {
		return
	}
	s.lastReport = now
	if s.promoteFn != nil {
		s.promoteFn(s.job.clone())
	}
}

func (s *Service) runPromote(rows []catalog.StagingFolder, releases []func()) {
	ctx := s.bgCtx
	failed := false
	for i, row := range rows {
		err := ctx.Err()
		if err == nil {
			s.updateJob(true, func(j *PromoteJob) { j.Folders[i].State = PromoteMoving })
			err = s.promoteFolder(ctx, i, row, s.targetOf(i))
		}
		releases[i]()
		s.changed()
		if err != nil {
			failed = true
			if ctx.Err() != nil {
				err = fmt.Errorf("not finished: SD2NAS is shutting down (%w)", err)
			}
			s.log.Error("promote failed", "folder", row.Name, "err", err)
		}
		s.updateJob(true, func(j *PromoteJob) {
			if err != nil {
				j.Folders[i].State, j.Folders[i].Error = PromoteFailed, err.Error()
			} else {
				j.Folders[i].State = PromoteDone
			}
		})
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	s.job.State = PromoteDone
	if failed {
		s.job.State = PromoteFailed
	}
	s.job.FinishedAt = s.now()
	s.promoting = false
	s.reportLocked(true)
}

func (s *Service) targetOf(i int) string {
	s.pmu.Lock()
	defer s.pmu.Unlock()
	return s.job.Folders[i].Target
}

// promoteFolder moves folder row to target (a rel_path under Archive/): one
// rename when target doesn't exist, else file by file (Decision 3). Files
// move on ctx, so shutdown stops between files; the catalog is written on
// a context that isn't cancelled, so a moved file is always recorded.
func (s *Service) promoteFolder(ctx context.Context, i int, row catalog.StagingFolder, target string) error {
	dbctx := context.WithoutCancel(ctx)
	src := s.folderDir(row.Name)
	if _, err := os.Lstat(src); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s/%s is not on disk", Dir, row.Name)
		}
		return err
	}
	prefix := prefixOf(row.Name)
	assets, err := s.db.AssetsUnder(dbctx, prefix, catalog.StateStaged)
	if err != nil {
		return err
	}
	present, gone := map[string]catalog.Asset{}, map[string]catalog.Asset{}
	for _, a := range assets {
		switch _, err := os.Lstat(s.abs(a.RelPath)); {
		case err == nil:
			present[strings.TrimPrefix(a.RelPath, prefix)] = a
		case errors.Is(err, fs.ErrNotExist):
			gone[a.RelPath] = a
		default:
			return err
		}
	}
	// The archive index must only claim files it has (Decision 9).
	if err := s.cullGone(dbctx, gone); err != nil {
		return err
	}
	dst := s.abs(target)
	touched := map[string]bool{filepath.Join(s.photosRoot, Dir): true, filepath.Dir(dst): true}
	switch _, err := os.Lstat(dst); {
	case errors.Is(err, fs.ErrNotExist):
		if err := os.MkdirAll(filepath.Dir(dst), 0o777); err != nil {
			return err
		}
		err := s.rename(src, dst)
		if err == nil {
			n, err := s.db.ArchiveFolder(dbctx, row.ID, prefix, target+"/", s.now())
			if err != nil {
				return fmt.Errorf("moved to %s but could not record it: %w", target, err)
			}
			var bytes int64
			for _, a := range present {
				bytes += a.Size
			}
			s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Done += n; j.FilesDone += n; j.BytesDone += bytes })
			return syncDirs(touched)
		}
		if !crossDevice(err) {
			return err
		}
		// moveFile creates the target folders as files arrive, so a stop
		// before the first file leaves nothing behind in Archive/.
		s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Copied = true })
	case err != nil:
		return err
	default:
		s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Merged = true })
	}
	return s.promoteFiles(ctx, i, row, present, dst, touched)
}

// promoteFiles moves every file of row, catalogued or not, to the same path
// inside dst (collision loop; EXDEV → copy, verify, delete), then removes
// the emptied staging folder and its row.
func (s *Service) promoteFiles(ctx context.Context, i int, row catalog.StagingFolder, present map[string]catalog.Asset, dst string, touched map[string]bool) error {
	dbctx := context.WithoutCancel(ctx)
	src := s.folderDir(row.Name)
	files, err := folderFiles(src)
	if err != nil {
		return err
	}
	for _, rel := range files {
		a, tracked := present[rel]
		from := filepath.Join(src, filepath.FromSlash(rel))
		to, err := s.moveFile(ctx, from, filepath.Join(dst, filepath.FromSlash(path.Dir(rel))), path.Base(rel), a.SHA256)
		if err != nil {
			return err
		}
		touched[filepath.Dir(from)], touched[filepath.Dir(to)] = true, true
		if !tracked {
			continue
		}
		newRel, err := s.relOf(to)
		if err == nil {
			err = s.db.ArchiveAsset(dbctx, a.SHA256, newRel, s.now())
		}
		if err != nil {
			return fmt.Errorf("moved %s but could not record it: %w", rel, err)
		}
		size := a.Size
		s.updateJob(false, func(j *PromoteJob) { j.Folders[i].Done++; j.FilesDone++; j.BytesDone += size })
	}
	gone, kept, err := removeEmptyTree(src)
	if err != nil {
		return err
	}
	if !gone {
		return fmt.Errorf("%s/%s still holds %s", Dir, row.Name, strings.Join(kept, ", "))
	}
	if err := s.db.DeleteStagingFolder(dbctx, row.ID); err != nil {
		return err
	}
	return syncDirs(touched)
}
```

Make `lockRows` (Task 5) use `busyNamed` too, so the name-filling lives in one place.

- [ ] **Step 5: Run the tests, the full set and the race run**: `go test ./internal/staging/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command and the **race** command → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/staging/
git commit -m "Promote staging folders into the archive as a background job

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Startup reconcile and on-demand rescan

Suggested implementer: sonnet (reviewer: opus — data safety).

**Files:**
- Create: `internal/staging/reconcile.go`, `internal/staging/reconcile_test.go`

**Interfaces:**
- Consumes: `RescanResult`, `Status` (Task 4); `cullGone`, `prefixOf`, `unionDates` (Task 5); `folderFiles`, `hashFile`, `safeName` (Task 4); `diskFolders` (M3); `catalog.DB.CreateStagingFolder`, `InsertAsset`, `AssetBySHA`, `MoveAsset`, `RestageAsset`, `AddFingerprint`, `SetStagingFolderInfo`, `DeleteStagingFolder`, `AssetsUnder`, `Settings`; `media.QuickFingerprint`, `media.CaptureTime`, `media.FolderDate`; test fixtures from `internal/media/mediatest` (`JPEG`, `ExifSpec`).
- Produces:

```go
func (s *Service) Reconcile(ctx context.Context) (RescanResult, error) // one pass; bumps the version when it changed something
func (s *Service) StartRescan() bool                                    // runs Reconcile in the background unless one runs; false if it didn't start one
```

- [ ] **Step 1: Write the failing tests** in `internal/staging/reconcile_test.go` (`newOpsEnv`; `jpeg(t, "2025:06:02 10:00:00")` returns `mediatest.JPEG(mediatest.ExifSpec{DateTimeOriginal: …})` bytes as a string). Review Focus 3:
- `TestReconcileAdoptsUntrackedFolder`: `Staging/2025-06-02 - Beach/` with a JPEG (EXIF 2025-06-02 10:00), `notes.txt`, `.DS_Store` → `Reconcile` → `AdoptedFolders 1, AddedFiles 1`; a row `2025-06-02 - Beach` with dates `["2025-06-02"]` and `CreatedAt` = opsNow; the asset is staged at that path with `CaptureTime` 2025-06-02 10:00, `FirstIngest` 0; `AssetsByFingerprint(media.QuickFingerprint(file))` finds it; `notes.txt` is untouched and not in the catalog; the version went up.
- `TestReconcileAddsFileToTrackedFolder`: a JPEG dropped into an existing folder → added; the folder's dates gain its date.
- `TestReconcileCullsRemovedFiles`: a tracked file deleted by hand → `CulledFiles 1`, state culled, fingerprint kept, hash reported through `Removed`.
- `TestReconcileDropsFolderWithoutDir`: a row whose directory was deleted (with two tracked files) → both culled, `RemovedFolders 1`, the row gone.
- `TestReconcileFollowsMoveBetweenFolders`: a tracked file moved by hand from A to B → `MovedFiles 1`, `CulledFiles 0`, the asset staged at B's path, nothing duplicated.
- `TestReconcileRestoresCulledFile`: content of a culled asset put back into a folder → `RestoredFiles 1`, staged at the new path.
- `TestReconcileLeavesDuplicates`: a second copy of a tracked file (original still there), and a copy of an `archived` asset's content → `Duplicates 2`, neither recorded, both left on disk.
- `TestReconcileSkipsBusyFolder`: a folder held by an import with a new file in it and a tracked file deleted → `BusyFolders 1`, neither the addition nor the removal recorded.
- `TestReconcileSkipsMoveFromBusyFolder`: a file moved by hand out of a folder an import holds → left untracked this time, `BusyFolders 1`.
- `TestReconcileStagingMissing` (Review Focus 3): rows exist, `Staging/` deleted → `Skipped` non-empty, nothing culled or removed, version unchanged.
- `TestReconcileIgnoresPartialsAndOtherFiles`: `x.JPG.partial`, `readme.txt` and a file directly in `Staging/` → nothing added.
- `TestReconcileSkipsFileBeingWritten`: set the unexported hook `e.svc.afterHash` (nil in production; see Step 3) to append a byte to the file it is given, as an SMB copy still running would → the file is not added, and `Errors` mentions "changed while it was read"; with the hook removed, the next pass adds it.
- `TestReconcileNoChangesNoBump`: a second pass on a reconciled tree → nothing changed, `changed()` not called (version unchanged).
- `TestStartRescan`: `StartRescan()` → true; `Status().Rescanning` true until it finishes (poll until false, 10 s); then `LastRescan` is set, and `OnChange` saw a status with `Rescanning: true` and a later one with `false` and `LastRescan`. After `Close`, `StartRescan()` → false.

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/staging/ -run 'Reconcile|Rescan'` → build failure.

- [ ] **Step 3: Implement `internal/staging/reconcile.go`**

Add the test seam to `Service`: `afterHash func(path string) // tests: runs between hashing a rescanned file and checking it didn't change`.

```go
package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/media"
)

// maxRescanErrors bounds RescanResult.Errors.
const maxRescanErrors = 20

func (r *RescanResult) errorf(format string, args ...any) {
	if len(r.Errors) < maxRescanErrors {
		r.Errors = append(r.Errors, fmt.Sprintf(format, args...))
	}
}

func (r *RescanResult) changedAnything() bool {
	return r.AdoptedFolders+r.AddedFiles+r.MovedFiles+r.RestoredFiles+r.CulledFiles+r.RemovedFolders > 0
}

// Reconcile brings the catalog in line with Staging/ on disk (§8, Decision
// 8): it adopts directories without a row, records media files the catalog
// doesn't know (or follows them when they were moved by hand), culls
// tracked files that are gone, and drops rows whose directory is gone.
// Busy folders are skipped. When Staging/ itself is missing it changes
// nothing: an unmounted share must not cull everything.
func (s *Service) Reconcile(ctx context.Context) (RescanResult, error) {
	r := RescanResult{StartedAt: s.now(), Errors: []string{}}
	err := s.reconcile(ctx, &r)
	r.FinishedAt = s.now()
	if r.changedAnything() {
		s.changed()
	}
	return r, err
}

func (s *Service) reconcile(ctx context.Context, r *RescanResult) error {
	rows, err := s.db.StagingFolders(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.abs(Dir)); errors.Is(err, fs.ErrNotExist) {
		if len(rows) > 0 {
			r.Skipped = Dir + "/ is missing, so nothing was changed (is the photos share mounted?)"
		}
		return nil
	} else if err != nil {
		return err
	}
	onDisk, err := s.diskFolders()
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, row := range rows {
		known[row.Name] = true
	}
	for _, name := range slices.Sorted(maps.Keys(onDisk)) {
		if known[name] {
			continue
		}
		var dates []string
		if d, _ := SplitName(name); d != "" {
			dates = []string{d}
		}
		row, err := s.db.CreateStagingFolder(ctx, name, dates, s.now())
		if errors.Is(err, catalog.ErrNameTaken) {
			continue // an import created it meanwhile
		}
		if err != nil {
			return err
		}
		r.AdoptedFolders++
		rows = append(rows, row)
	}
	settings, err := s.db.Settings(ctx)
	if err != nil {
		return err
	}
	busy := map[int64]bool{}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := s.reconcileAdditions(ctx, row, settings.DayStartsAt, r, busy); err != nil {
			return err
		}
	}
	for _, row := range rows {
		if err := s.reconcileRemovals(ctx, row, r, busy); err != nil {
			return err
		}
	}
	if err := s.reconcileOrphans(ctx, r); err != nil {
		return err
	}
	r.BusyFolders = len(busy)
	return nil
}

// lockForRescan takes id's lock, or records it as busy and returns nil.
func (s *Service) lockForRescan(id int64, busy map[int64]bool) (func(), error) {
	release, err := s.locks.TryLock(HolderRescan, id)
	if errors.Is(err, ErrBusy) {
		busy[id] = true
		return nil, nil
	}
	return release, err
}

func (s *Service) reconcileAdditions(ctx context.Context, row catalog.StagingFolder, cutoff time.Duration, r *RescanResult, busy map[int64]bool) error {
	release, err := s.lockForRescan(row.ID, busy)
	if err != nil || release == nil {
		return err
	}
	defer release()
	row, err = s.db.StagingFolderByID(ctx, row.ID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := safeName(row.Name); err != nil {
		r.errorf("%v", err)
		return nil
	}
	prefix := prefixOf(row.Name)
	files, err := folderFiles(s.folderDir(row.Name))
	if err != nil {
		r.errorf("%v", err)
		return nil
	}
	assets, err := s.db.AssetsUnder(ctx, prefix, catalog.StateStaged)
	if err != nil {
		return err
	}
	tracked := map[string]bool{}
	for _, a := range assets {
		tracked[strings.TrimPrefix(a.RelPath, prefix)] = true
	}
	var dates []string
	for _, rel := range files {
		if tracked[rel] {
			continue
		}
		kind, ok := s.rules.KindOf(rel)
		if !ok {
			continue // not a photo or video: left alone
		}
		date, err := s.adoptFile(ctx, row, prefix+rel, kind, cutoff, r, busy)
		if err != nil {
			return err
		}
		if date != "" {
			dates = append(dates, date)
		}
	}
	if len(dates) == 0 {
		return nil
	}
	return s.db.SetStagingFolderInfo(ctx, row.ID, unionDates([]catalog.StagingFolder{row}, dates...), time.Time{})
}

// adoptFile records the untracked media file at relPath and returns the
// folder date it adds ("" for none). Problems with the file itself go into
// r; only catalog failures and cancellation are returned.
func (s *Service) adoptFile(ctx context.Context, row catalog.StagingFolder, relPath string, kind media.Kind,
	cutoff time.Duration, r *RescanResult, busy map[int64]bool) (string, error) {
	p := s.abs(relPath)
	before, err := os.Stat(p)
	if err != nil {
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	sum, err := hashFile(ctx, p)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	if s.afterHash != nil {
		s.afterHash(p)
	}
	fp, err := media.QuickFingerprint(p)
	if err != nil {
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	after, err := os.Stat(p)
	if err != nil {
		r.errorf("%s: %v", relPath, err)
		return "", nil
	}
	if before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		r.errorf("%s changed while it was read (still being copied?); the next rescan adds it", relPath)
		return "", nil
	}
	existing, err := s.db.AssetBySHA(ctx, sum)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		c, cerr := media.CaptureTime(p, kind)
		if cerr != nil {
			s.log.Warn("rescanned file has no capture time", "path", relPath, "err", cerr)
		}
		a := catalog.Asset{SHA256: sum, Size: after.Size(), Kind: kind, CaptureTime: c.Time, TZOffset: c.Offset,
			State: catalog.StateStaged, RelPath: relPath, Mtime: after.ModTime().UnixNano(), UpdatedAt: s.now()}
		if err := s.db.InsertAsset(ctx, a, fp); err != nil {
			return "", err
		}
		r.AddedFiles++
		if c.Time.IsZero() {
			return "", nil
		}
		return media.FolderDate(c.Time, cutoff), nil
	case err != nil:
		return "", err
	case existing.SHA256 != sum: // an alias: an archived file's content before an edit
		r.Duplicates++
		return "", nil
	}
	date := ""
	if !existing.CaptureTime.IsZero() {
		date = media.FolderDate(existing.CaptureTime, cutoff)
	}
	switch existing.State {
	case catalog.StateStaged:
		if _, err := os.Lstat(s.abs(existing.RelPath)); !errors.Is(err, fs.ErrNotExist) {
			r.Duplicates++ // the tracked copy is still there (or can't be checked)
			return "", nil
		}
		release, err := s.lockOwner(ctx, existing.RelPath, row.ID, busy)
		if err != nil || release == nil {
			return "", err // nil, nil: its folder is busy; the next rescan follows it
		}
		defer release()
		if err := s.db.MoveAsset(ctx, sum, relPath, s.now()); err != nil {
			return "", err
		}
		r.MovedFiles++
	case catalog.StateCulled, catalog.StateMissing:
		if err := s.db.RestageAsset(ctx, sum, relPath, after.ModTime().UnixNano(), s.now()); err != nil {
			return "", err
		}
		if err := s.db.AddFingerprint(ctx, fp, sum); err != nil {
			return "", err
		}
		r.RestoredFiles++
	default: // archived: a copy of an archive file, left alone
		r.Duplicates++
		return "", nil
	}
	return date, nil
}

// lockOwner takes the lock of the staging folder holding relPath unless
// that is self or has no row (then it returns a no-op release). It returns
// nil, nil when that folder is busy.
func (s *Service) lockOwner(ctx context.Context, relPath string, self int64, busy map[int64]bool) (func(), error) {
	name, _, ok := strings.Cut(strings.TrimPrefix(relPath, Dir+"/"), "/")
	if !strings.HasPrefix(relPath, Dir+"/") || !ok {
		return func() {}, nil
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return nil, err
	}
	for _, r := range all {
		if r.Name == name && r.ID != self {
			return s.lockForRescan(r.ID, busy)
		}
	}
	return func() {}, nil
}

func (s *Service) reconcileRemovals(ctx context.Context, row catalog.StagingFolder, r *RescanResult, busy map[int64]bool) error {
	release, err := s.lockForRescan(row.ID, busy)
	if err != nil || release == nil {
		return err
	}
	defer release()
	row, err = s.db.StagingFolderByID(ctx, row.ID)
	if errors.Is(err, catalog.ErrNotFound) {
		return nil
	}
	if err != nil || safeName(row.Name) != nil {
		return err
	}
	assets, err := s.db.AssetsUnder(ctx, prefixOf(row.Name), catalog.StateStaged)
	if err != nil {
		return err
	}
	gone := map[string]catalog.Asset{}
	for _, a := range assets {
		switch _, err := os.Lstat(s.abs(a.RelPath)); {
		case errors.Is(err, fs.ErrNotExist):
			gone[a.RelPath] = a
		case err != nil:
			r.errorf("%s: %v", a.RelPath, err)
		}
	}
	if err := s.cullGone(ctx, gone); err != nil {
		return err
	}
	r.CulledFiles += len(gone)
	if _, err := os.Lstat(s.folderDir(row.Name)); errors.Is(err, fs.ErrNotExist) && len(gone) == len(assets) {
		if err := s.db.DeleteStagingFolder(ctx, row.ID); err != nil {
			return err
		}
		r.RemovedFolders++
	}
	return nil
}

// reconcileOrphans culls staged files below Staging/ that no folder row
// owns and that are gone from disk.
func (s *Service) reconcileOrphans(ctx context.Context, r *RescanResult) error {
	rows, err := s.db.StagingFolders(ctx)
	if err != nil {
		return err
	}
	names := map[string]bool{}
	for _, row := range rows {
		names[row.Name] = true
	}
	assets, err := s.db.AssetsUnder(ctx, Dir+"/", catalog.StateStaged)
	if err != nil {
		return err
	}
	gone := map[string]catalog.Asset{}
	for _, a := range assets {
		if name, _, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/"); ok && names[name] {
			continue
		}
		if _, err := os.Lstat(s.abs(a.RelPath)); errors.Is(err, fs.ErrNotExist) {
			gone[a.RelPath] = a
		}
	}
	if err := s.cullGone(ctx, gone); err != nil {
		return err
	}
	r.CulledFiles += len(gone)
	return nil
}

// StartRescan runs Reconcile in the background unless one is running or
// the service is closing, and reports whether it started one. Status
// tracks it (Rescanning, then LastRescan).
func (s *Service) StartRescan() bool {
	s.mu.Lock()
	if s.closed || s.status.Rescanning {
		s.mu.Unlock()
		return false
	}
	s.status.Rescanning = true
	s.notifyLocked()
	s.wg.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.wg.Done()
		r, err := s.Reconcile(s.bgCtx)
		switch {
		case errors.Is(err, context.Canceled):
			s.log.Info("staging rescan stopped: shutting down")
			r.errorf("stopped: SD2NAS is shutting down")
		case err != nil:
			s.log.Error("staging rescan failed", "err", err)
			r.errorf("%v", err)
		default:
			s.log.Info("staging rescan", "adopted_folders", r.AdoptedFolders, "added", r.AddedFiles, "moved", r.MovedFiles,
				"restored", r.RestoredFiles, "culled", r.CulledFiles, "removed_folders", r.RemovedFolders,
				"duplicates", r.Duplicates, "busy", r.BusyFolders, "problems", len(r.Errors), "skipped", r.Skipped)
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		s.status.Rescanning = false
		s.status.LastRescan = &r
		s.notifyLocked()
	}()
	return true
}
```

- [ ] **Step 4: Run the tests, the full set and the race run**: `go test ./internal/staging/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command, the **race** command → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/staging/
git commit -m "Reconcile staging with the disk at startup and on demand

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: Staging age warnings and the listing version

Suggested implementer: haiku.

**Files:**
- Modify: `internal/staging/staging.go`, `internal/staging/staging_test.go`

**Interfaces:**
- Consumes: `catalog.Settings.StagingWarnDays` (Task 2); `Service.Status`, `s.now` (Task 4).
- Produces (JSON shapes are binding, see Global Constraints):

```go
// Folder gains:
AgeDays int  `json:"age_days"` // whole days since created_at
Stale   bool `json:"stale"`    // AgeDays >= staging_warn_days (and that setting isn't 0)
// Listing gains:
Version        uint64 `json:"version"`          // Status().Version read before the listing was built
StaleAfterDays int    `json:"stale_after_days"` // the setting; 0 = off
Stale          int    `json:"stale"`            // how many folders are stale
// FolderFiles gains:
Version uint64 `json:"version"`
```

- [ ] **Step 1: Make the M3 fixture's clock fixed** — in `staging_test.go`'s `setup`, build the service with `NewService(Config{DB: db, PhotosRoot: photos, Now: func() time.Time { return now }})` (its folders are created at `now`, so their age is 0 and the existing expectations hold). Add `t.Cleanup(svc.Close)`.

- [ ] **Step 2: Write the failing tests** (append to `staging_test.go`):
- `TestListAgeWarnings`: a service whose clock is `now.Add(40 * 24 * time.Hour)` over the `setup` catalog → `f1` has `AgeDays 40`, `Stale true` and its last warning is exactly `In staging for 40 days`; the listing has `StaleAfterDays 30` and `Stale 2`. With the clock at `now.Add(29*24*time.Hour + 23*time.Hour)` → `AgeDays 29`, not stale. With `SetSetting(KeyStagingWarnDays, "0")` and the 40-day clock → not stale, `StaleAfterDays 0`, no age warning. A clock before `created_at` → `AgeDays 0`.
- `TestListAndFilesCarryVersion`: `Touch()` twice → `List` has `Version 2` and `Files(f1)` has `Version 2`.

- [ ] **Step 3: Run them and confirm they fail**: `go test ./internal/staging/ -run 'Age|Version'` → build failure.

- [ ] **Step 4: Implement** in `staging.go`:

```go
// ageRule flags folders that have sat in staging too long (§3, Decision 13).
type ageRule struct {
	now      time.Time
	warnDays int // 0 = never
}

func (s *Service) ageRule(ctx context.Context) (ageRule, error) {
	set, err := s.db.Settings(ctx)
	if err != nil {
		return ageRule{}, err
	}
	return ageRule{now: s.now(), warnDays: set.StagingWarnDays}, nil
}
```

`List`: read `version := s.Status().Version` first, then `age, err := s.ageRule(ctx)`; build `Listing{…, Version: version, StaleAfterDays: age.warnDays}`; pass `age` to `summarize`; count `Stale`. `Files`: the same version and age, `FolderFiles.Version`. In `summarize(row, sorted, warned, onDisk, age)`, after the clock warnings:

```go
	f.AgeDays = max(0, int(age.now.Sub(row.CreatedAt)/(24*time.Hour)))
	if age.warnDays > 0 && f.AgeDays >= age.warnDays {
		f.Stale = true
		f.Warnings = append(f.Warnings, fmt.Sprintf("In staging for %d days", f.AgeDays))
	}
```

- [ ] **Step 5: Run the tests**: `go test ./internal/staging/ ./internal/api/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command → PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/staging/staging.go internal/staging/staging_test.go
git commit -m "Flag folders that have been in staging too long

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Forget an import, and forget culls for "Re-import anyway"

Suggested implementer: sonnet (reviewer: opus — destructive).

**Files:**
- Create: `internal/catalog/forget.go`, `internal/catalog/forget_test.go`, `internal/staging/forget.go`, `internal/staging/forget_test.go`
- Modify: `internal/ingest/manager.go`, `internal/ingest/manager_test.go`

**Interfaces:**
- Consumes: `catalog.DB.Ingest`, `MarkIngestForgotten`, `RecordNew`, `AddIngestFiles`, `AddClockWarnings`, `AddAlias`, `CreateIngest`, `StartIngest`, `FinishIngest` (tests); staging's `lockFolders`, `prefixOf`, `dropIfEmpty`, `syncDirs`, `s.removed`, `s.changed`; ingest test helpers `newEnv`, `newManager`, `gate`, `waitStarted`, `waitDone`, `env.fixtureCard`.
- Produces:

```go
// package catalog
func (d *DB) IngestAssets(ctx context.Context, ingestID int64) ([]Asset, error)   // first_ingest = id, or named by one of its ingest_files rows; by sha
func (d *DB) ForgetAssets(ctx context.Context, shas []string) ([]string, error)    // deletes staged|culled ones (+ fingerprints, aliases, clock warnings); returns those deleted
func (d *DB) CulledSeenBefore(ctx context.Context, ingestID int64) ([]string, error) // its seen_before files whose asset is still culled, sorted

// package ingest
var ErrBusy = errors.New("ingest: an import is running or waiting")
func (m *Manager) Exclusive() (release func(), err error)

// package staging
var ErrIngestRunning = errors.New("staging: that import has not finished")
var ErrAlreadyForgotten = errors.New("staging: that import was already forgotten")
type ForgetSummary struct {
	Staged       int   `json:"staged"`       // files of this import still in staging
	StagedBytes  int64 `json:"staged_bytes"`
	Culled       int   `json:"culled"`       // culled files of this import (the card brings them back after a forget)
	Archived     int   `json:"archived"`     // already in the archive: stay known
	Reimportable int   `json:"reimportable"` // its "seen before" files whose asset is culled ("Re-import anyway")
	Forgotten    bool  `json:"forgotten"`
}
type ForgetResult struct {
	Forgotten    int     `json:"forgotten"`     // asset rows deleted
	DeletedFiles int     `json:"deleted_files"`
	KeptArchived int     `json:"kept_archived"`
	Removed      []int64 `json:"removed"`       // staging folders left empty and removed
}
func (s *Service) ForgetSummary(ctx context.Context, ingestID int64) (ForgetSummary, error)
func (s *Service) Forget(ctx context.Context, ingestID int64, deleteFiles bool) (ForgetResult, error)
func (s *Service) ForgetCulled(ctx context.Context, ingestID int64) (int, error)
```

- [ ] **Step 1: Write the failing catalog tests** in `internal/catalog/forget_test.go`. Fixture: ingest 1 (done) with `RecordNew` of staged A and B (outcome new), an `ingest_files` row `seen_before` naming culled asset C, one `in_archive` naming archived asset D, one `in_staging` naming staged E whose `first_ingest` is another ingest 2, and one `failed` row without a hash; a clock warning for A; an alias row whose target is B.
- `TestIngestAssets`: ingest 1 → A, B, C, D, E (sorted by hash, each once); ingest 2 → E; ingest 99 → empty.
- `TestForgetAssets` (Review Focus 6): `ForgetAssets([A, B, C, D, unknown])` returns `[A, B, C]` (in the given order); A, B, C are `ErrNotFound`; `AssetsByFingerprint` of their fingerprints is empty; the alias to B and A's clock warning are gone; D is unchanged (archived is never forgotten); the `ingest_files` rows are all still there. A bad hash → `ErrBadHash`, nothing deleted (transaction).
- `TestCulledSeenBefore`: ingest 1 → `[C]`; after `RestageAsset(C)` → empty.

- [ ] **Step 2: Write the failing manager test** in `internal/ingest/manager_test.go`:
- `TestManagerExclusive`: idle → `Exclusive()` succeeds; a second `Exclusive()` also succeeds (holds nest); `Submit` the SONY card → it stays queued (`Current()` running nil, queued 1) and isn't done within 200 ms (named negative check); `Exclusive()` now fails with `ErrBusy` (queued); release both holds (releasing twice is harmless) → `waitDone`. Then `started, release := gate(e)`, submit the GOPRO card, `waitStarted` → `Exclusive()` fails with `ErrBusy` (running); `release()`; `waitDone`; idle again → `Exclusive()` succeeds.

- [ ] **Step 3: Write the failing staging tests** in `internal/staging/forget_test.go` (`newOpsEnv`). Helper `importOf(t, e, files ...)` creates a finished ingest whose `RecordNew` rows stage the given files into folder `2025-05-30` (written with `e.write`), plus optional culled/archived rows linked by `AddIngestFiles`.
- `TestForgetSummary`: 2 staged (sizes 3 and 5), 1 culled seen-before, 1 archived → `{Staged: 2, StagedBytes: 8, Culled: 1, Archived: 1, Reimportable: 1}`.
- `TestForgetDeletesFiles`: `Forget(id, true)` → both staged files gone from disk; A, B, C rows gone; D kept; `DeletedFiles 2`, `Forgotten 3`, `KeptArchived 1`; the folder's directory and row removed (`Removed == [id]`); the ingest's `ForgottenAt` set; hashes reported through `Removed`; version up.
- `TestForgetKeepsFiles`: `Forget(id, false)` → rows gone, files still on disk, the folder row stays, `DeletedFiles 0`.
- `TestForgetRefuses`: a running ingest → `ErrIngestRunning`; a forgotten one → `ErrAlreadyForgotten`; id 99 → `catalog.ErrNotFound`.
- `TestForgetBusyFolder`: the folder held by an import → `*BusyError`, nothing deleted, ingest not marked.
- `TestForgetCulled`: → 1; C is gone; nothing on disk changed; a second call → 0.

- [ ] **Step 4: Run them and confirm they fail**: `go test ./internal/catalog/ ./internal/ingest/ ./internal/staging/` → build failures.

- [ ] **Step 5: Implement `internal/catalog/forget.go`**

```go
package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// IngestAssets returns every asset import ingestID is linked to: those it
// brought in (first_ingest) and those any of its ingest_files rows names
// (already in staging or the archive, seen before), ordered by hash.
func (d *DB) IngestAssets(ctx context.Context, ingestID int64) ([]Asset, error) {
	rows, err := d.db.QueryContext(ctx, "SELECT "+assetCols+" FROM assets WHERE first_ingest = ?"+
		" UNION SELECT "+assetCols+" FROM assets WHERE sha256 IN"+
		" (SELECT sha256 FROM ingest_files WHERE ingest_id = ? AND sha256 IS NOT NULL) ORDER BY sha256", ingestID, ingestID)
	if err != nil {
		return nil, fmt.Errorf("assets of ingest %d: %w", ingestID, err)
	}
	var out []Asset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("assets of ingest %d: %w", ingestID, err), rows.Close())
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("assets of ingest %d: %w", ingestID, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("assets of ingest %d: %w", ingestID, err)
	}
	return out, nil
}

// ForgetAssets deletes the staged or culled assets among shas, with their
// fingerprints, aliases and clock warnings, in one transaction, so a card
// holding that content imports it as new again (§8 "Forget an import").
// Archived and missing assets are never deleted; unknown hashes are
// skipped. It returns the hashes it deleted, in the order given.
func (d *DB) ForgetAssets(ctx context.Context, shas []string) ([]string, error) {
	var out []string
	err := d.tx(ctx, func(tx *sql.Tx) error {
		out = nil
		for _, sha := range shas {
			b, err := hexToBlob(sha)
			if err != nil {
				return err
			}
			var state string
			err = tx.QueryRowContext(ctx, "SELECT state FROM assets WHERE sha256 = ?", b).Scan(&state)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if state != string(StateStaged) && state != string(StateCulled) {
				continue
			}
			for _, q := range []string{
				"DELETE FROM fingerprints WHERE sha256 = ?",
				"DELETE FROM aliases WHERE sha256 = ?",
				"DELETE FROM clock_warnings WHERE sha256 = ?",
				"DELETE FROM assets WHERE sha256 = ?",
			} {
				if _, err := tx.ExecContext(ctx, q, b); err != nil {
					return fmt.Errorf("forget %s: %w", sha, err)
				}
			}
			out = append(out, sha)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("forget assets: %w", err)
	}
	return out, nil
}

// CulledSeenBefore returns the hashes of import ingestID's "seen before"
// files whose asset is still culled, sorted.
func (d *DB) CulledSeenBefore(ctx context.Context, ingestID int64) ([]string, error) {
	rows, err := d.db.QueryContext(ctx, `SELECT DISTINCT a.sha256 FROM ingest_files f JOIN assets a ON a.sha256 = f.sha256
		WHERE f.ingest_id = ? AND f.outcome = 'seen_before' AND a.state = 'culled' ORDER BY a.sha256`, ingestID)
	if err != nil {
		return nil, fmt.Errorf("culled files of ingest %d: %w", ingestID, err)
	}
	var out []string
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, errors.Join(fmt.Errorf("culled files of ingest %d: %w", ingestID, err), rows.Close())
		}
		out = append(out, blobToHex(b))
	}
	if err := rows.Err(); err != nil {
		return nil, errors.Join(fmt.Errorf("culled files of ingest %d: %w", ingestID, err), rows.Close())
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("culled files of ingest %d: %w", ingestID, err)
	}
	return out, nil
}
```

- [ ] **Step 6: `Manager.Exclusive`** in `internal/ingest/manager.go`: add `holds int // Exclusive holds; while > 0 no queued job starts` to `Manager`, and

```go
// ErrBusy reports that an import is running or waiting.
var ErrBusy = errors.New("ingest: an import is running or waiting")

// Exclusive holds new imports back until release is called, for work that
// must not overlap one (forgetting an import, Decision 10). Submit still
// queues jobs, but none starts while any hold is held. It fails with
// ErrBusy while an import runs or waits.
func (m *Manager) Exclusive() (release func(), err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running != nil || len(m.queue) > 0 {
		return nil, ErrBusy
	}
	m.holds++
	var once sync.Once
	return func() {
		once.Do(func() {
			m.mu.Lock()
			m.holds--
			m.mu.Unlock()
			select {
			case m.wake <- struct{}{}:
			default:
			}
		})
	}, nil
}
```

In `next`, start a job only when `len(m.queue) > 0 && m.holds == 0`.

- [ ] **Step 7: Implement `internal/staging/forget.go`**

```go
package staging

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/aspiziri/SD2NAS/internal/catalog"
)

var (
	ErrIngestRunning    = errors.New("staging: that import has not finished")
	ErrAlreadyForgotten = errors.New("staging: that import was already forgotten")
)

// …ForgetSummary and ForgetResult exactly as in Produces…

// ForgetSummary says what forgetting import ingestID would do.
func (s *Service) ForgetSummary(ctx context.Context, ingestID int64) (ForgetSummary, error) {
	in, err := s.db.Ingest(ctx, ingestID)
	if err != nil {
		return ForgetSummary{}, err
	}
	assets, err := s.db.IngestAssets(ctx, ingestID)
	if err != nil {
		return ForgetSummary{}, err
	}
	culled, err := s.db.CulledSeenBefore(ctx, ingestID)
	if err != nil {
		return ForgetSummary{}, err
	}
	out := ForgetSummary{Reimportable: len(culled), Forgotten: !in.ForgottenAt.IsZero()}
	for _, a := range assets {
		switch a.State {
		case catalog.StateStaged:
			out.Staged++
			out.StagedBytes += a.Size
		case catalog.StateCulled:
			out.Culled++
		case catalog.StateArchived:
			out.Archived++
		}
	}
	return out, nil
}

// Forget forgets import ingestID (§8, Decision 10): its staged and culled
// assets are deleted from the catalog with their fingerprints, after their
// files in Staging/ are deleted when deleteFiles is set, so the same card
// imports them again. Archived assets stay known. The ingest stays in the
// history, marked forgotten. The caller keeps imports from running
// meanwhile (ingest.Manager.Exclusive).
func (s *Service) Forget(ctx context.Context, ingestID int64, deleteFiles bool) (ForgetResult, error) {
	in, err := s.db.Ingest(ctx, ingestID)
	if err != nil {
		return ForgetResult{}, err
	}
	if !in.Status.Terminal() {
		return ForgetResult{}, ErrIngestRunning
	}
	if !in.ForgottenAt.IsZero() {
		return ForgetResult{}, ErrAlreadyForgotten
	}
	assets, err := s.db.IngestAssets(ctx, ingestID)
	if err != nil {
		return ForgetResult{}, err
	}
	all, err := s.db.StagingFolders(ctx)
	if err != nil {
		return ForgetResult{}, err
	}
	byName := map[string]catalog.StagingFolder{}
	for _, r := range all {
		byName[r.Name] = r
	}
	res := ForgetResult{Removed: []int64{}}
	var forget, staged []catalog.Asset
	folderIDs := map[int64]bool{}
	for _, a := range assets {
		switch a.State {
		case catalog.StateStaged:
			staged = append(staged, a)
			if name, _, ok := strings.Cut(strings.TrimPrefix(a.RelPath, Dir+"/"), "/"); ok {
				if r, known := byName[name]; known {
					folderIDs[r.ID] = true
				}
			}
			forget = append(forget, a)
		case catalog.StateCulled:
			forget = append(forget, a)
		case catalog.StateArchived:
			res.KeptArchived++
		}
	}
	ids := make([]int64, 0, len(folderIDs))
	for id := range folderIDs {
		ids = append(ids, id)
	}
	rows, release, err := s.lockFolders(ctx, uniqueIDs(ids))
	if err != nil {
		return ForgetResult{}, err
	}
	defer release()
	defer s.changed()
	touched := map[string]bool{}
	if deleteFiles {
		for _, a := range staged {
			p := s.abs(a.RelPath)
			if !strings.HasPrefix(a.RelPath, Dir+"/") {
				continue // never delete outside Staging/
			}
			if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return res, fmt.Errorf("deleted %d files of the import: %w", res.DeletedFiles, err)
			}
			touched[filepath.Dir(p)] = true
			res.DeletedFiles++
		}
	}
	shas := make([]string, len(forget))
	for i, a := range forget {
		shas[i] = a.SHA256
	}
	gone, err := s.db.ForgetAssets(ctx, shas)
	if err != nil {
		return res, err
	}
	res.Forgotten = len(gone)
	s.removed(gone)
	if err := s.db.MarkIngestForgotten(ctx, ingestID, s.now()); err != nil {
		return res, err
	}
	if deleteFiles {
		for _, row := range rows {
			removed, err := s.dropIfEmpty(ctx, row, touched)
			if err != nil {
				return res, err
			}
			if removed {
				res.Removed = append(res.Removed, row.ID)
			}
		}
	}
	return res, syncDirs(touched)
}

// ForgetCulled forgets that import ingestID's culled "seen before" files
// were culled, so the next import of that card copies them again
// ("Re-import anyway", Decision 11). It touches no files and returns how
// many it forgot.
func (s *Service) ForgetCulled(ctx context.Context, ingestID int64) (int, error) {
	in, err := s.db.Ingest(ctx, ingestID)
	if err != nil {
		return 0, err
	}
	if !in.Status.Terminal() {
		return 0, ErrIngestRunning
	}
	shas, err := s.db.CulledSeenBefore(ctx, ingestID)
	if err != nil {
		return 0, err
	}
	gone, err := s.db.ForgetAssets(ctx, shas)
	if err != nil {
		return 0, err
	}
	s.removed(gone)
	return len(gone), nil
}
```

- [ ] **Step 8: Run the tests, the full set and the race run**: `go test ./internal/catalog/ ./internal/ingest/ ./internal/staging/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command, the **race** command → PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/catalog/forget.go internal/catalog/forget_test.go internal/ingest/manager.go internal/ingest/manager_test.go internal/staging/forget.go internal/staging/forget_test.go
git commit -m "Forget an import, and forget culls so a card can bring them back

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: API for rename, merge, move, cull and discard; `staging.changed`; thumbnail cleanup

Suggested implementer: sonnet (reviewer: opus — the 409 mapping and events).

**Files:**
- Modify: `internal/api/staging.go`, `internal/api/events.go`, `internal/api/full_test.go`, `internal/thumbs/thumbs.go`, `internal/thumbs/thumbs_test.go`, `cmd/sd2nas/main.go`
- Create: `internal/api/stagingops_test.go`

**Interfaces:**
- Consumes: `staging.Service` and its `Rename`, `Merge`, `Move`, `Cull`, `Discard`, `Status`, `OnChange`, `Touch`, `Close`, `NewService`, `Config`, `NewLocks`, `BusyError`, `Holder*`, `ErrBad*`, `ErrChanged`, `ErrPromoteRunning`, `ErrIngestRunning`, `ErrAlreadyForgotten`, `ErrClosed`, `Reconcile` (tests); `catalog.ErrNameTaken`, `ErrNotFound`; `ingest.ErrBusy`, `ingest.Config.Locks`; existing `writeJSON`, `writeError`, `positiveInt`, `Server.addFinishedHook`, `s.hub`, `s.closers`; test helpers `newFullServer`, `post`, `cardBody`, `decodeInto`, `wantJSONError`, `pollUntil`, `getFull`, `writeJPEG` (thumbs_test.go), `thumbPath`, `sseFrames`, `nextFrame`, `eventsServer`.
- Produces:

```go
// package api
const EventStagingChanged = "staging.changed"
// MountStaging(svc) now also registers PATCH/DELETE /api/staging/{id}, POST /api/staging/merge|move|cull,
// the staging.changed snapshot and publisher, a finished-ingest hook calling svc.Touch, and svc.Close as a closer.
// Unexported, used by Task 12: writeOpError(w, log, what, err), decodeBody(w, r, v, max), opError.

// package thumbs
func (c *Cache) Remove(sha string) error // deletes sha's cached thumbnail and preview; missing files are fine
```

- [ ] **Step 1: `thumbs.Cache.Remove`** — test first in `thumbs_test.go`: `TestRemove` renders a thumbnail and a preview of a JPEG source, `Remove(sha)` deletes both files (`c.path(sha, size)`), a second `Remove` returns nil, `Remove("xyz")` → `ErrBadSHA`. Run `go test ./internal/thumbs/ -run TestRemove` → fails. Then:

```go
// Remove deletes sha's cached thumbnail and preview (a culled or forgotten
// file, Decision 12). A render still running for sha may write its file
// afterwards; that one stays until the cache is cleared.
func (c *Cache) Remove(sha string) error {
	if !ValidSHA(sha) {
		return ErrBadSHA
	}
	var errs []error
	for _, size := range []int{ThumbSize, PreviewSize} {
		if err := os.Remove(c.path(sha, size)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
```

- [ ] **Step 2: Share the locks and the service in the test server** — `newFullServer` (`full_test.go`) builds `locks := staging.NewLocks()`, passes `Locks: locks` to `ingest.Config`, creates the thumbnail cache **before** the service (with `discard := slog.New(slog.DiscardHandler)`), and

```go
	svc := staging.NewService(staging.Config{DB: db, PhotosRoot: photos, Locks: locks, Log: discard,
		Removed: func(shas []string) {
			for _, sha := range shas {
				if err := cache.Remove(sha); err != nil {
					t.Errorf("remove thumbnails of %s: %v", sha, err)
				}
			}
		}})
	s.MountStaging(svc)
```

`fullServer` gains `locks *staging.Locks`, `svc *staging.Service` and `mgr *ingest.Manager`. (`s.Close` closes `svc`; don't register another `Close`.)

- [ ] **Step 3: Write the failing API tests** in `internal/api/stagingops_test.go`. Helpers:

```go
// send makes a guarded request (X-SD2NAS: 1) with an optional JSON body.
func send(t *testing.T, s *Server, method, url, body string) *httptest.ResponseRecorder { … }

// ingested runs the SONY card import and returns its one folder.
func ingested(t *testing.T, f fullServer) staging.Folder { … POST ?wait=1, then GET /api/staging … }

// wantCode checks the status and, for an error, the code field.
func wantCode(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) { … }
```

Tests:
- `TestStagingRenameAPI`: `PATCH /api/staging/{id}` `{"title":"Trip"}` → 200, `folder.name` `2025-05-30 - Trip`; `GET /api/staging` lists that name; the files are on disk under it. `{}` → 400 (title required); `{"title":"a/b"}` → 400; `{"title":"x","extra":1}` → 400; `PATCH /api/staging/999` → 404; `PATCH /api/staging/abc` → 400; without `X-SD2NAS` → 403 (the guard).
- `TestStagingRenameBusy409` (Review Focus 1): `f.locks.TryLock(staging.HolderIngest, id)` held → PATCH → 409, `Content-Type: application/json`, `code` `folder_busy`, `holder` `ingest`, `folder_id` id, `error` exactly `2025-05-30 is being imported into. Try again when the import finishes.`; with `HolderOperation` held → the "busy with another change" message.
- `TestStagingRenameNameTaken`: an untracked `Staging/2025-05-30 - Trip/` directory → PATCH `{"title":"Trip"}` → 409 `name_taken`.
- `TestStagingMergeAPI`: write a JPEG (`writeJPEG`) at `Staging/2025-06-01 - Beach/a.JPG`, `f.svc.Reconcile(ctx)` adopts it → `POST /api/staging/merge` `{"ids":[A,B],"title":"Trip"}` → 200 `folder.name` `2025-05-30 - Trip`, 3 files; the listing has one folder. `{"ids":[A]}` → 400.
- `TestStagingMoveAndCullAPI`: `GET …/files` → the JPG and ARW hashes; `POST /api/staging/move` `{"fileShas":[arw],"newTitle":"Raw"}` → 200, `folder.name` `2025-05-30 - Raw`, `moved` 1, `removed` []; fetch `/api/thumbs/{jpg}` (creates the cache file), then `POST /api/staging/cull` `{"fileShas":[jpg]}` → 200 `{"culled":1,"removed":[A]}` (A emptied), and the JPG's cache file is gone (`thumbPath`). Move with both `toId` and `newTitle`, or neither → 400; a `fileShas` entry `"nope"` → 400; an unknown hash → 400; 20001 hashes → 400; a body over 4 MiB → 400.
- `TestStagingDiscardAPI`: `DELETE /api/staging/{id}` → 200 `{"culled":2,"removed":true,"kept":[]}`; then `GET …/files` → 404; with an untracked `notes.txt` in a second folder → `removed:false`, `kept:["notes.txt"]`.
- `TestStagingChangedEvent` (Review Focus 7): `ch, unsub := f.s.hub.subscribe()`; PATCH → a frame `event: staging.changed` whose data has a `version` greater than the listing's `version` before the PATCH, `rescanning:false`; and over real HTTP (`eventsServer` + `sseFrames`) the connect snapshot includes a `staging.changed` frame with the current version.
- `TestStagingTouchedWhenImportFinishes`: subscribe, then `POST /api/ingest?wait=1` → a `staging.changed` frame arrives within 10 s (the feed's finished hook).

- [ ] **Step 4: Run them and confirm they fail**: `go test ./internal/api/ -run 'Staging'` → failures (405/404 for the new routes).

- [ ] **Step 5: Implement**

In `events.go` add `EventStagingChanged = "staging.changed"` to the constants (and update the comment above them).

In `staging.go` (imports gain `context`, `encoding/json`, `strings` and `internal/ingest`):

```go
type stagingHandlers struct {
	svc *staging.Service
	log *slog.Logger
}

const (
	smallBody = 64 << 10 // rename, merge, promote, forget
	largeBody = 4 << 20  // move, cull: up to staging.MaxFiles hashes
)

// MountStaging registers the staging endpoints and the staging.changed
// event, and makes Close close svc. Call it before serving.
func (s *Server) MountStaging(svc *staging.Service) {
	h := &stagingHandlers{svc: svc, log: s.log}
	s.mux.HandleFunc("GET /api/staging", h.list)
	s.mux.HandleFunc("GET /api/staging/{id}/files", h.files)
	s.mux.HandleFunc("PATCH /api/staging/{id}", h.rename)
	s.mux.HandleFunc("DELETE /api/staging/{id}", h.discard)
	s.mux.HandleFunc("POST /api/staging/merge", h.merge)
	s.mux.HandleFunc("POST /api/staging/move", h.move)
	s.mux.HandleFunc("POST /api/staging/cull", h.cull)
	s.hub.AddSnapshot(func() (string, any, error) { return EventStagingChanged, svc.Status(), nil })
	svc.OnChange(func(st staging.Status) {
		if err := s.hub.Publish(EventStagingChanged, st); err != nil {
			s.log.Error("publish staging.changed", "err", err)
		}
	})
	s.addFinishedHook(func(catalog.Ingest) { svc.Touch() }) // new files may have landed
	s.closers = append(s.closers, svc.Close)
}

// opError is the JSON body of a failed operation.
type opError struct {
	Error    string `json:"error"`
	Code     string `json:"code,omitempty"`
	FolderID int64  `json:"folder_id,omitempty"`
	Holder   string `json:"holder,omitempty"`
}

func busyMessage(b *staging.BusyError) string {
	name := b.Name
	if name == "" {
		name = fmt.Sprintf("Folder %d", b.ID)
	}
	switch b.Holder {
	case staging.HolderIngest:
		return name + " is being imported into. Try again when the import finishes."
	case staging.HolderRescan:
		return name + " is being rescanned. Try again in a moment."
	}
	return name + " is busy with another change. Try again in a moment."
}

// writeOpError answers a failed staging operation; only unexpected
// failures are logged.
func writeOpError(w http.ResponseWriter, log *slog.Logger, what string, err error) {
	var busy *staging.BusyError
	conflict := func(code, msg string) { writeJSON(w, http.StatusConflict, opError{Error: msg, Code: code}) }
	switch {
	case errors.As(err, &busy):
		writeJSON(w, http.StatusConflict, opError{Error: busyMessage(busy), Code: "folder_busy", FolderID: busy.ID, Holder: string(busy.Holder)})
	case errors.Is(err, catalog.ErrNameTaken):
		conflict("name_taken", err.Error())
	case errors.Is(err, staging.ErrChanged):
		conflict("changed", "Some files moved while you were choosing. Reload and try again.")
	case errors.Is(err, staging.ErrPromoteRunning):
		conflict("promote_running", "Another promote is running. Wait for it to finish.")
	case errors.Is(err, staging.ErrIngestRunning), errors.Is(err, ingest.ErrBusy):
		conflict("ingest_running", "An import is running or waiting. Try again when it finishes.")
	case errors.Is(err, staging.ErrAlreadyForgotten):
		conflict("already_forgotten", "This import was already forgotten.")
	case errors.Is(err, catalog.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, staging.ErrBadTitle), errors.Is(err, staging.ErrBadPattern), errors.Is(err, staging.ErrBadRequest):
		writeError(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, staging.ErrClosed):
		writeError(w, http.StatusServiceUnavailable, "SD2NAS is shutting down")
	default:
		log.Error(what, "err", err)
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// decodeBody reads a JSON body of at most max bytes into v, refusing
// unknown fields and trailing data.
func decodeBody(w http.ResponseWriter, r *http.Request, v any, max int64) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, max))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad request body: %w", err)
	}
	if dec.More() {
		return errors.New("bad request body: more than one JSON value")
	}
	return nil
}

// opContext keeps an operation running when the client goes away (a phone
// sleeping mid-merge must not leave it half done).
func opContext(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }

func (h *stagingHandlers) pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := positiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id: "+err.Error())
		return 0, false
	}
	return id, true
}

func (h *stagingHandlers) rename(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	var body struct {
		Title *string `json:"title"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Title == nil {
		writeError(w, http.StatusBadRequest, "title is required (\"\" removes the title)")
		return
	}
	f, err := h.svc.Rename(opContext(r), id, *body.Title)
	if err != nil {
		writeOpError(w, h.log, "rename staging folder", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folder": f})
}

func (h *stagingHandlers) discard(w http.ResponseWriter, r *http.Request) {
	id, ok := h.pathID(w, r)
	if !ok {
		return
	}
	res, err := h.svc.Discard(opContext(r), id)
	if err != nil {
		writeOpError(w, h.log, "discard staging folder", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *stagingHandlers) merge(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs   []int64 `json:"ids"`
		Title string  `json:"title"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	f, err := h.svc.Merge(opContext(r), body.IDs, body.Title)
	if err != nil {
		writeOpError(w, h.log, "merge staging folders", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"folder": f})
}

func (h *stagingHandlers) move(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileShas []string `json:"fileShas"`
		ToID     *int64   `json:"toId"`
		NewTitle *string  `json:"newTitle"`
	}
	if err := decodeBody(w, r, &body, largeBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if (body.ToID == nil) == (body.NewTitle == nil) {
		writeError(w, http.StatusBadRequest, "give exactly one of toId and newTitle")
		return
	}
	req := staging.MoveRequest{SHAs: body.FileShas}
	if body.ToID != nil {
		if *body.ToID <= 0 {
			writeError(w, http.StatusBadRequest, "toId must be a positive integer")
			return
		}
		req.ToID = *body.ToID
	} else {
		req.NewTitle = *body.NewTitle
		if strings.TrimSpace(req.NewTitle) == "" {
			writeError(w, http.StatusBadRequest, "a new folder needs a title")
			return
		}
	}
	res, err := h.svc.Move(opContext(r), req)
	if err != nil {
		writeOpError(w, h.log, "move staged files", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *stagingHandlers) cull(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileShas []string `json:"fileShas"`
	}
	if err := decodeBody(w, r, &body, largeBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.svc.Cull(opContext(r), body.FileShas)
	if err != nil {
		writeOpError(w, h.log, "cull staged files", err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
```

(The existing `list` and `files` handlers stay. `MountStaging` is called after `MountIngest` in `main`, so `addFinishedHook` lands on the feed; hooks are read at each tick, so the order doesn't matter anyway.)

- [ ] **Step 6: Wire `main`** (`cmd/sd2nas/main.go`), in the `else` branch after the catalog opens:

```go
		locks := staging.NewLocks() // shared: imports and staging operations lock the same folders
		mgr = ingest.NewManager(ingest.NewRunner(ingest.Config{Catalog: db, PhotosRoot: cfg.Photos, Log: log, Locks: locks}), cfg.Cards)
		if err := mgr.Recover(ctx); err != nil {
			log.Error("startup recovery failed", "err", err)
			srv.AddProblem("startup recovery: " + err.Error())
		}
		cache, cacheErr := thumbs.New(thumbs.Config{Dir: filepath.Join(cfg.Data, "thumbs"), Log: log})
		svc := staging.NewService(staging.Config{DB: db, PhotosRoot: cfg.Photos, Locks: locks, Log: log,
			Removed: func(shas []string) {
				if cacheErr != nil {
					return
				}
				for _, sha := range shas {
					if err := cache.Remove(sha); err != nil {
						log.Warn("could not remove cached thumbnails", "sha", sha, "err", err)
					}
				}
			}})
		mgr.Start()
		srv.MountIngest(mgr, db, api.DefaultMaxWait)
		srv.MountStaging(svc) // srv.Close closes svc
		if cacheErr != nil {
			log.Error("thumbnail cache unavailable", "err", cacheErr)
			srv.AddProblem("thumbnails: " + cacheErr.Error())
		} else {
			for _, w := range cache.MissingTools() {
				log.Warn(w)
				srv.AddWarning(w)
			}
			srv.MountThumbs(db, cache, cfg.Photos) // srv.Close closes the cache
		}
```

(The shutdown order stays: `mgr.Close` → `srv.Close` (which closes `svc`, then the cache) → `httpSrv.Shutdown`.)

- [ ] **Step 7: Run the tests, the full set and the race run**: `go test ./internal/api/ ./internal/thumbs/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command, the **race** command → PASS; `go build ./cmd/sd2nas && rm -f sd2nas sd2nas.exe`.

- [ ] **Step 8: Commit**

```bash
git add internal/api/ internal/thumbs/thumbs.go internal/thumbs/thumbs_test.go cmd/sd2nas/main.go
git commit -m "Serve rename, merge, move, cull and discard, and publish staging.changed

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: API for promote, rescan, forget and re-import; `promote.progress`; startup rescan

Suggested implementer: sonnet (reviewer: opus).

**Files:**
- Modify: `internal/api/staging.go`, `internal/api/events.go`, `internal/api/full_test.go`, `cmd/sd2nas/main.go`
- Create: `internal/api/forget.go`, `internal/api/promote_test.go`, `internal/api/forget_test.go`

**Interfaces:**
- Consumes: Task 11's `writeOpError`, `decodeBody`, `opContext`, `stagingHandlers`; `staging.Service.Promote`, `LastPromote`, `OnPromote`, `StartRescan`, `ForgetSummary`, `Forget`, `ForgetCulled`; `ingest.Manager.Exclusive`, `Submit`, `ErrBadCardPath`, `CardName`; `viewOf`, `ingestView`, `s.feed` (`watch`).
- Produces:

```go
const EventPromoteProgress = "promote.progress"
// MountStaging also registers POST/GET /api/staging/promote and POST /api/staging/rescan, and the promote.progress snapshot/publisher.
// MountForget registers GET/POST /api/ingest/{id}/forget and POST /api/ingest/{id}/reimport-culled. Call it after MountIngest.
func (s *Server) MountForget(m *ingest.Manager, db *catalog.DB, svc *staging.Service)
```

- [ ] **Step 1: Write the failing tests.** In `full_test.go`, `newFullServer` also calls `s.MountForget(m, db, svc)`.

`promote_test.go`:
- `TestPromoteAPI` (Review Focus 5, 7): subscribe to the hub; ingest; `POST /api/staging/promote` `{"ids":[id],"pattern":""}` → 202 with `job.state` `running` and `job.folders[0].target` `Archive/2025-05-30`; poll `GET /api/staging/promote` until `job.state` is `done`; `Archive/2025-05-30/<jpg>` and `…/Capture/<arw>` exist; `GET /api/staging` lists no folders; `GET /api/thumbs/{jpg}` still answers 200 (the thumbnail follows the hash); the frames include `promote.progress` with `running` and later `done`, and fewer than 12 `promote.progress` frames in all.
- `TestPromoteAPIErrors`: `{"ids":[]}` → 400; `{"ids":[999]}` → 404; `{"ids":[id],"pattern":"{day}"}` → 400; the folder held by an import → 409 `folder_busy`; `GET /api/staging/promote` before any job → `{"job":null}`.
- `TestPromoteSnapshot`: after a finished promote, a new SSE connection's snapshot includes a `promote.progress` frame with that job; before any promote, its data is `null`.
- `TestRescanAPI`: write a JPEG into the imported folder (as if over SMB) → `POST /api/staging/rescan` → 202 `{"started":true}`; poll `f.svc.Status()` until `rescanning` is false; the listing shows 3 files; the `staging.changed` frames include `rescanning:true` and later one with `last_rescan.added_files` 1.

`forget_test.go` (Review Focus 6):
- `TestForgetAPIRetestsCard`: ingest (2 new) → `GET /api/ingest/{id}/forget` → `{"staged":2,…,"forgotten":false}`; `POST /api/ingest/{id}/forget` `{"deleteFiles":true}` → 200, `result.deleted_files` 2, `result.forgotten` 2, `ingest.forgotten_at` set; `Staging/` holds no files; `GET /api/ingests` shows `forgotten_at` on that row; a second `POST …/forget` → 409 `already_forgotten`; the card imported again → `2 new`.
- `TestForgetAPIBadRequests`: `{}` → 400 (`deleteFiles` required); id `abc` → 400; 999 → 404.
- `TestForgetAPIRefusesWhileImporting`: hold `f.mgr.Exclusive()` in the test, submit the card (it stays queued) → `POST …/forget` → 409 `ingest_running`; release the hold.
- `TestReimportCulledAPI`: ingest #1; cull its JPG via the API; ingest #2 (JPG `seen_before`) → `GET /api/ingest/2/forget` `reimportable` 1 → `POST /api/ingest/2/reimport-culled` → 200 `forgotten` 1 and `started` non-null (the card is still there); wait for that import → the JPG is back in staging (an asset `staged` with that hash). Then cull it again, ingest #4, remove the card directory, `POST /api/ingest/4/reimport-culled` → `started` null and `message` contains "next time"; with nothing culled → `forgotten` 0 and `message` contains "Nothing to re-import".

- [ ] **Step 2: Run them and confirm they fail**: `go test ./internal/api/ -run 'Promote|Rescan|Forget|Reimport'` → build failure (`MountForget` undefined).

- [ ] **Step 3: Implement**

`events.go`: `EventPromoteProgress = "promote.progress"`.

In `MountStaging` add:

```go
	s.mux.HandleFunc("POST /api/staging/promote", h.promote)
	s.mux.HandleFunc("GET /api/staging/promote", h.promoteStatus)
	s.mux.HandleFunc("POST /api/staging/rescan", h.rescan)
	s.hub.AddSnapshot(func() (string, any, error) {
		if j, ok := svc.LastPromote(); ok {
			return EventPromoteProgress, j, nil
		}
		return EventPromoteProgress, nil, nil // "null": no promote since SD2NAS started
	})
	svc.OnPromote(func(j staging.PromoteJob) {
		if err := s.hub.Publish(EventPromoteProgress, j); err != nil {
			s.log.Error("publish promote.progress", "err", err)
		}
	})
```

and the handlers:

```go
func (h *stagingHandlers) promote(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs     []int64 `json:"ids"`
		Pattern string  `json:"pattern"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	job, err := h.svc.Promote(opContext(r), body.IDs, body.Pattern)
	if err != nil {
		writeOpError(w, h.log, "promote staging folders", err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

func (h *stagingHandlers) promoteStatus(w http.ResponseWriter, r *http.Request) {
	if j, ok := h.svc.LastPromote(); ok {
		writeJSON(w, http.StatusOK, map[string]any{"job": j})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": nil})
}

func (h *stagingHandlers) rescan(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]bool{"started": h.svc.StartRescan()})
}
```

`internal/api/forget.go`:

```go
package api

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/staging"
)

type forgetHandlers struct {
	m    *ingest.Manager
	db   *catalog.DB
	svc  *staging.Service
	feed *ingestFeed // MountIngest's; nil when it wasn't mounted
	log  *slog.Logger
}

// MountForget registers "Forget this import" and "Re-import anyway" (§8,
// §10, Decisions 10 and 11). Call it after MountIngest.
func (s *Server) MountForget(m *ingest.Manager, db *catalog.DB, svc *staging.Service) {
	h := &forgetHandlers{m: m, db: db, svc: svc, feed: s.feed, log: s.log}
	s.mux.HandleFunc("GET /api/ingest/{id}/forget", h.summary)
	s.mux.HandleFunc("POST /api/ingest/{id}/forget", h.forget)
	s.mux.HandleFunc("POST /api/ingest/{id}/reimport-culled", h.reimport)
}

func (h *forgetHandlers) id(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := positiveInt(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "id: "+err.Error())
		return 0, false
	}
	return id, true
}

func (h *forgetHandlers) summary(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	sum, err := h.svc.ForgetSummary(r.Context(), id)
	if err != nil {
		writeOpError(w, h.log, "forget summary", err)
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *forgetHandlers) forget(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	var body struct {
		DeleteFiles *bool `json:"deleteFiles"`
	}
	if err := decodeBody(w, r, &body, smallBody); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.DeleteFiles == nil {
		writeError(w, http.StatusBadRequest, "deleteFiles is required")
		return
	}
	release, err := h.m.Exclusive()
	if err != nil {
		writeOpError(w, h.log, "forget import", err)
		return
	}
	defer release()
	ctx := opContext(r)
	res, err := h.svc.Forget(ctx, id, *body.DeleteFiles)
	if err != nil {
		writeOpError(w, h.log, "forget import", err)
		return
	}
	row, err := h.db.Ingest(ctx, id)
	if err != nil {
		writeOpError(w, h.log, "forget import", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": res, "ingest": viewOf(row, nil)})
}

func (h *forgetHandlers) reimport(w http.ResponseWriter, r *http.Request) {
	id, ok := h.id(w, r)
	if !ok {
		return
	}
	release, err := h.m.Exclusive()
	if err != nil {
		writeOpError(w, h.log, "re-import culled files", err)
		return
	}
	ctx := opContext(r)
	n, err := h.svc.ForgetCulled(ctx, id)
	release() // before Submit, so the new import can start
	if err != nil {
		writeOpError(w, h.log, "re-import culled files", err)
		return
	}
	row, err := h.db.Ingest(ctx, id)
	if err != nil {
		writeOpError(w, h.log, "re-import culled files", err)
		return
	}
	name := ingest.CardName(row)
	resp := map[string]any{"forgotten": n, "started": nil}
	if n == 0 {
		resp["message"] = "Nothing to re-import: none of this import's skipped files is still marked as culled."
		writeJSON(w, http.StatusOK, resp)
		return
	}
	job, err := h.m.Submit(ctx, ingest.Request{Path: row.CardPath, Label: row.CardLabel, UUID: row.CardUUID})
	switch {
	case err == nil:
		if h.feed != nil {
			h.feed.watch(job)
		}
		v := viewOf(job.Result(), job)
		resp["started"] = v
		resp["message"] = fmt.Sprintf("Importing %d %s again from %s.", n, plural(n, "file", "files"), name)
	case errors.Is(err, ingest.ErrBadCardPath):
		resp["message"] = fmt.Sprintf("%d %s will be imported again the next time you insert %s.", n, plural(n, "file", "files"), name)
	default:
		h.log.Error("start re-import", "ingest", id, "err", err)
		resp["message"] = fmt.Sprintf("%d %s will be imported again the next time you insert %s (starting an import now failed: %v).",
			n, plural(n, "file", "files"), name, err)
	}
	writeJSON(w, http.StatusOK, resp)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
```

In `main.go`, after `srv.MountStaging(svc)`:

```go
		srv.MountForget(mgr, db, svc)
		svc.StartRescan() // §8: fix drift from SMB edits made while SD2NAS was down
```

- [ ] **Step 4: Run the tests, the full set and the race run**: `go test ./internal/api/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...`, the Linux command, the **race** command → PASS. `go run ./cmd/sd2nas --cards ./testdata/cards --photos <tmp>/photos --config <tmp>/config` starts and logs `staging rescan`; stop it with Ctrl-C (it exits within a second).

- [ ] **Step 5: Commit**

```bash
git add internal/api/ cmd/sd2nas/main.go
git commit -m "Serve promote, rescan, forget and re-import, and publish promote.progress

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 13: Web logic: naming, selection and messages (node tests)

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/lib/naming.js`, `web/src/lib/naming.test.js`, `web/src/lib/selection.js`, `web/src/lib/selection.test.js`, `web/src/lib/messages.js`, `web/src/lib/messages.test.js`

**Interfaces:**
- Consumes: `formatCount` from `web/src/lib/format.js`; the Go rules in `internal/staging/names.go` (Task 4), which `naming.js` mirrors.
- Produces:

```js
// naming.js
export const MAX_NAME_BYTES = 200;
export const DEFAULT_PATTERN = '{name}';
export function splitName(name)               // → { date, title }  (date '' when the name has none; title = whole name then)
export function folderName(date, title)       // "2025-05-30 - Trip" | date | title
export function folderDate(folder)            // name's date, else folder.dates[0], else ''
export function earliestDate(folders)         // min folderDate, '' when none
export function nameProblem(s)                // '' or why s can't be a folder name (same rules and words as Go's checkName)
export function checkTitle(raw)               // → { title (trimmed), error ('' when fine; '' title is fine) }
export function promoteTarget(folder, pattern) // → { path, error } (path relative to Archive/)

// selection.js
export const LONG_PRESS_MS = 450;
export const MOVE_SLOP = 10;
export function clickAction({ selecting, shift, toggle }) // → 'open' | 'toggle' | 'range'
export function toggleKey(selected, key)                  // new Set
export function rangeKeys(order, a, b)                    // inclusive slice of order, [] if either is missing
export function selectRange(selected, order, anchor, key) // adds anchor..key (just key without a usable anchor)
export function dragSelection(base, order, anchor, over, adding) // base ± anchor..over
export function prune(selected, order)                    // drops keys no longer shown (same Set when nothing dropped)
export function autoScrollSpeed(y, height, edge = 64, max = 18) // px per frame while dragging near an edge; negative = up

// messages.js
export function plural(n, one, many)   // "1 file", "1,204 files"
export function joinList(parts)        // "a", "a and b", "a, b and c"
export function describeError(err)     // sentence for an ApiError (or any Error)
export function rescanSummary(r)       // RescanResult → sentence
export function promoteSummary(job)    // PromoteJob → sentence
```

- [ ] **Step 1: Write the failing tests**

`naming.test.js`:

```js
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { splitName, folderName, folderDate, earliestDate, nameProblem, checkTitle, promoteTarget, MAX_NAME_BYTES } from './naming.js';

test('splitName reads the archive convention as text', () => {
  assert.deepEqual(splitName('2025-05-30 - Trip'), { date: '2025-05-30', title: 'Trip' });
  assert.deepEqual(splitName('2025-05-30'), { date: '2025-05-30', title: '' });
  assert.deepEqual(splitName('2007-09-31 - Random Party'), { date: '2007-09-31', title: 'Random Party' });
  assert.deepEqual(splitName('Holiday'), { date: '', title: 'Holiday' });
  assert.deepEqual(splitName('2025-05-30 - A - B'), { date: '2025-05-30', title: 'A - B' });
});

test('folderName joins date and title', () => {
  assert.equal(folderName('2025-05-30', 'Trip'), '2025-05-30 - Trip');
  assert.equal(folderName('2025-05-30', ''), '2025-05-30');
  assert.equal(folderName('', 'Trip'), 'Trip');
});

test('folderDate and earliestDate', () => {
  assert.equal(folderDate({ name: '2025-05-30 - Trip', dates: ['2025-05-29'] }), '2025-05-30');
  assert.equal(folderDate({ name: 'Holiday', dates: ['2025-06-01', '2025-06-02'] }), '2025-06-01');
  assert.equal(folderDate({ name: 'Holiday', dates: [] }), '');
  assert.equal(earliestDate([{ name: '2025-05-31', dates: [] }, { name: 'X', dates: ['2025-05-30'] }, { name: 'Y', dates: [] }]), '2025-05-30');
  assert.equal(earliestDate([{ name: 'Y', dates: [] }]), '');
});

test('nameProblem mirrors the server rules', () => {
  assert.equal(nameProblem('Zürich'), '');
  for (const bad of ['a/b', 'a\\b', 'a:b', 'a*b', 'a?b', 'a"b', 'a<b', 'a>b', 'a|b', 'a\tb', '.hidden', 'trailing.', 'trailing ', '', '.', '..']) {
    assert.notEqual(nameProblem(bad), '', bad);
  }
  assert.equal(nameProblem('é'.repeat(MAX_NAME_BYTES / 2)), '');
  assert.match(nameProblem('é'.repeat(MAX_NAME_BYTES / 2 + 1)), /longer than 200 bytes/);
});

test('checkTitle trims and allows an empty title', () => {
  assert.deepEqual(checkTitle('  Trip '), { title: 'Trip', error: '' });
  assert.deepEqual(checkTitle('   '), { title: '', error: '' });
  assert.match(checkTitle('a/b').error, /^The title contains "\/"/);
});

test('promoteTarget expands the tokens like the server', () => {
  const f = { name: '2025-05-30 - Trip', dates: ['2025-05-30'] };
  assert.deepEqual(promoteTarget(f, ''), { path: '2025-05-30 - Trip', error: '' });
  assert.deepEqual(promoteTarget(f, '{name}'), { path: '2025-05-30 - Trip', error: '' });
  assert.deepEqual(promoteTarget(f, '{year}/{month}/{name}'), { path: '2025/05/2025-05-30 - Trip', error: '' });
  assert.match(promoteTarget(f, '{day}').error, /Unknown token \{day\}/);
  assert.match(promoteTarget(f, '{year}/{name').error, /braces/);
  assert.notEqual(promoteTarget(f, '../{name}').error, '');
  assert.notEqual(promoteTarget(f, '{year}//{name}').error, '');
  assert.match(promoteTarget({ name: 'Holiday', dates: [] }, '{year}').error, /needs a date/);
  assert.notEqual(promoteTarget({ name: 'What?', dates: [] }, '{name}').error, '');
});
```

`selection.test.js`:

```js
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { clickAction, toggleKey, rangeKeys, selectRange, dragSelection, prune, autoScrollSpeed } from './selection.js';

const order = ['a', 'b', 'c', 'd', 'e'];
const set = (...k) => new Set(k);

test('a click opens until something is selected, then toggles', () => {
  assert.equal(clickAction({ selecting: false }), 'open');
  assert.equal(clickAction({ selecting: true }), 'toggle');
  assert.equal(clickAction({ selecting: false, toggle: true }), 'toggle'); // Ctrl/⌘-click starts selecting
  assert.equal(clickAction({ selecting: false, shift: true }), 'toggle'); // nothing to extend yet
  assert.equal(clickAction({ selecting: true, shift: true }), 'range');
});

test('toggleKey returns a new set', () => {
  const s = set('a');
  const t = toggleKey(s, 'b');
  assert.deepEqual([...t].sort(), ['a', 'b']);
  assert.deepEqual([...s], ['a']);
  assert.deepEqual([...toggleKey(t, 'a')], ['b']);
});

test('ranges work both ways and need both ends', () => {
  assert.deepEqual(rangeKeys(order, 'b', 'd'), ['b', 'c', 'd']);
  assert.deepEqual(rangeKeys(order, 'd', 'b'), ['b', 'c', 'd']);
  assert.deepEqual(rangeKeys(order, 'x', 'b'), []);
  assert.deepEqual([...selectRange(set('a'), order, 'b', 'd')].sort(), ['a', 'b', 'c', 'd']);
  assert.deepEqual([...selectRange(set(), order, null, 'c')], ['c']);
});

test('dragging adds or removes the range from the starting selection', () => {
  const base = set('a', 'e');
  assert.deepEqual([...dragSelection(base, order, 'b', 'd', true)].sort(), ['a', 'b', 'c', 'd', 'e']);
  // Dragging back shrinks the range again: it is always computed from base.
  assert.deepEqual([...dragSelection(base, order, 'b', 'b', true)].sort(), ['a', 'b', 'e']);
  assert.deepEqual([...dragSelection(set('a', 'b', 'c'), order, 'c', 'a', false)], []);
});

test('prune drops hidden keys and keeps the same set when nothing changes', () => {
  const s = set('a', 'z');
  assert.deepEqual([...prune(s, order)], ['a']);
  const t = set('a', 'b');
  assert.equal(prune(t, order), t);
});

test('auto-scroll speeds up towards the edges', () => {
  assert.equal(autoScrollSpeed(400, 800), 0);
  assert.ok(autoScrollSpeed(10, 800) < 0);
  assert.ok(autoScrollSpeed(10, 800) < autoScrollSpeed(50, 800));
  assert.ok(autoScrollSpeed(790, 800) > 0);
  assert.equal(autoScrollSpeed(-50, 800), -18);
  assert.equal(autoScrollSpeed(900, 800), 18);
});
```

`messages.test.js`:

```js
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { plural, joinList, describeError, rescanSummary, promoteSummary } from './messages.js';
import { ApiError } from './api.js';

const scan = (o) => ({ adopted_folders: 0, added_files: 0, moved_files: 0, restored_files: 0, culled_files: 0, removed_folders: 0,
  duplicates: 0, busy_folders: 0, errors: [], skipped: '', ...o });

test('plural and joinList', () => {
  assert.equal(plural(1, 'file', 'files'), '1 file');
  assert.equal(plural(1204, 'file', 'files'), '1,204 files');
  assert.equal(joinList(['a']), 'a');
  assert.equal(joinList(['a', 'b']), 'a and b');
  assert.equal(joinList(['a', 'b', 'c']), 'a, b and c');
});

test('describeError keeps server sentences and explains the rest', () => {
  assert.equal(describeError(new ApiError(409, '2025-05-30 is being imported into. Try again when the import finishes.', { code: 'folder_busy' })),
    '2025-05-30 is being imported into. Try again when the import finishes.');
  assert.match(describeError(new ApiError(503, 'catalog: boom')), /^SD2NAS can't do that right now: catalog: boom/);
  assert.match(describeError(new ApiError(0, 'SD2NAS is not reachable: x')), /^SD2NAS is not reachable/);
  assert.equal(describeError(new Error('boom')), 'boom');
});

test('rescanSummary', () => {
  assert.equal(rescanSummary(scan({})), 'Staging already matched the disk.');
  assert.equal(rescanSummary(scan({ adopted_folders: 1, added_files: 3, culled_files: 1 })),
    'Rescan found 1 new folder, 3 new files and 1 file gone.');
  assert.match(rescanSummary(scan({ busy_folders: 2 })), /2 folders were busy and skipped/);
  assert.match(rescanSummary(scan({ errors: ['x changed while it was read'] })), /1 problem: x changed while it was read/);
  assert.equal(rescanSummary(scan({ skipped: 'Staging/ is missing' })), 'Rescan skipped: Staging/ is missing');
});

test('promoteSummary', () => {
  const folders = [{ state: 'done' }, { state: 'moving' }];
  assert.equal(promoteSummary({ state: 'running', folders, files_done: 10, files_total: 40 }), 'Promoting folder 2 of 2 · 10 of 40 files');
  assert.equal(promoteSummary({ state: 'done', folders: [{ state: 'done' }, { state: 'done' }], files_done: 40, files_total: 40 }),
    'Promoted 2 folders (40 files) to the archive.');
  assert.equal(promoteSummary({ state: 'failed', folders: [{ state: 'done' }, { state: 'failed' }], files_done: 20, files_total: 40 }),
    '1 of 2 folders could not be promoted.');
});
```

- [ ] **Step 2: Run them and confirm they fail**: in `web/`, `npm test` → module-not-found failures.

- [ ] **Step 3: Implement**

`naming.js`:

```js
// Folder names follow the archive: "YYYY-MM-DD - Title" (PLAN §8). This
// mirrors internal/staging/names.go, so dialogs preview exactly what the
// server will do; the server still checks everything.
const NAME_RE = /^([0-9]{4}-[0-9]{2}-[0-9]{2})(?: - (.+))?$/;
const BAD_CHARS = /[/\\<>:"|?*\u0000-\u001f\u007f-\u009f]/;
const TOKEN_RE = /\{[^{}]*\}/g;
const encoder = new TextEncoder();

export const MAX_NAME_BYTES = 200;
export const DEFAULT_PATTERN = '{name}';

// Names are text: the date is never checked as a calendar date.
export function splitName(name) {
  const m = NAME_RE.exec(name);
  return m ? { date: m[1], title: m[2] ?? '' } : { date: '', title: name };
}

export function folderName(date, title) {
  if (!date) return title;
  if (!title) return date;
  return `${date} - ${title}`;
}

export function folderDate(folder) {
  return splitName(folder.name).date || (folder.dates?.[0] ?? '');
}

export function earliestDate(folders) {
  let best = '';
  for (const f of folders) {
    const d = folderDate(f);
    if (d && (!best || d < best)) best = d;
  }
  return best;
}

// The same rules and words as checkName in internal/staging/names.go.
export function nameProblem(s) {
  if (s === '') return 'is empty';
  if (s === '.' || s === '..') return 'is not a folder name';
  if (s.startsWith('.')) return 'starts with a dot (it would be hidden)';
  if (s.endsWith('.') || s.endsWith(' ')) return "ends with a dot or a space, which Windows can't use";
  if (encoder.encode(s).length > MAX_NAME_BYTES) return `is longer than ${MAX_NAME_BYTES} bytes`;
  const m = BAD_CHARS.exec(s);
  if (m) return `contains ${JSON.stringify(m[0])}, which can't be used in a folder name`;
  return '';
}

export function checkTitle(raw) {
  const title = raw.trim();
  if (title === '') return { title, error: '' };
  const p = nameProblem(title);
  return { title, error: p ? `The title ${p}.` : '' };
}

export function promoteTarget(folder, pattern) {
  const p = pattern.trim() || DEFAULT_PATTERN;
  const date = folderDate(folder);
  let error = '';
  const path = p.replace(TOKEN_RE, (tok) => {
    if (tok === '{name}') return folder.name;
    if (tok === '{year}' || tok === '{month}') {
      if (!date) {
        error ||= `${tok} needs a date, and “${folder.name}” has none`;
        return '';
      }
      return tok === '{year}' ? date.slice(0, 4) : date.slice(5, 7);
    }
    error ||= `Unknown token ${tok} (use {name}, {year} or {month})`;
    return '';
  });
  if (error) return { path: '', error };
  if (/[{}]/.test(path)) return { path: '', error: 'The pattern has unbalanced braces' };
  for (const el of path.split('/')) {
    const prob = nameProblem(el);
    if (prob) return { path: '', error: `In “${path}” a folder name ${prob}` };
  }
  return { path, error: '' };
}
```

`selection.js`:

```js
// Selection rules shared by the folder list and the file grid (Decision 18).
// Keys are folder ids (as strings) or file hashes; `order` is the keys in
// the order they are shown.
export const LONG_PRESS_MS = 450; // a finger resting this long starts selecting
export const MOVE_SLOP = 10; // px a resting finger may drift

// What a click or tap on an item does. Ctrl/⌘ (toggle) and Shift are
// desktop modifiers; on touch, long-press is what starts selecting.
export function clickAction({ selecting = false, shift = false, toggle = false } = {}) {
  if (shift && selecting) return 'range';
  if (shift || toggle || selecting) return 'toggle';
  return 'open';
}

export function toggleKey(selected, key) {
  const out = new Set(selected);
  if (out.has(key)) out.delete(key);
  else out.add(key);
  return out;
}

export function rangeKeys(order, a, b) {
  const i = order.indexOf(a);
  const j = order.indexOf(b);
  if (i < 0 || j < 0) return [];
  return order.slice(Math.min(i, j), Math.max(i, j) + 1);
}

export function selectRange(selected, order, anchor, key) {
  const keys = rangeKeys(order, anchor, key);
  const out = new Set(selected);
  for (const k of keys.length ? keys : [key]) out.add(k);
  return out;
}

// The selection while a finger drags from anchor to over: always computed
// from base, so dragging back shrinks the range again.
export function dragSelection(base, order, anchor, over, adding) {
  const out = new Set(base);
  const keys = rangeKeys(order, anchor, over);
  for (const k of keys.length ? keys : [anchor]) {
    if (adding) out.add(k);
    else out.delete(k);
  }
  return out;
}

export function prune(selected, order) {
  const shown = new Set(order);
  const out = new Set([...selected].filter((k) => shown.has(k)));
  return out.size === selected.size ? selected : out;
}

export function autoScrollSpeed(y, height, edge = 64, max = 18) {
  if (y < edge) return -Math.min(max, Math.ceil(((edge - y) / edge) * max));
  if (y > height - edge) return Math.min(max, Math.ceil(((y - (height - edge)) / edge) * max));
  return 0;
}
```

`messages.js`:

```js
import { formatCount } from './format.js';

export const plural = (n, one, many) => `${formatCount(n)} ${n === 1 ? one : many}`;

export function joinList(parts) {
  if (parts.length <= 1) return parts.join('');
  return `${parts.slice(0, -1).join(', ')} and ${parts.at(-1)}`;
}

// The server already writes whole sentences for 4xx answers (409s name the
// folder and say why); only unavailability gets a lead-in.
export function describeError(err) {
  if (!err) return '';
  if (err.status === 503) return `SD2NAS can't do that right now: ${err.message}`;
  return err.message || String(err);
}

export function rescanSummary(r) {
  if (r.skipped) return `Rescan skipped: ${r.skipped}`;
  const found = [];
  if (r.adopted_folders) found.push(plural(r.adopted_folders, 'new folder', 'new folders'));
  if (r.added_files) found.push(plural(r.added_files, 'new file', 'new files'));
  if (r.moved_files) found.push(`${plural(r.moved_files, 'file', 'files')} moved`);
  if (r.restored_files) found.push(`${plural(r.restored_files, 'culled file', 'culled files')} back`);
  if (r.culled_files) found.push(`${plural(r.culled_files, 'file', 'files')} gone`);
  if (r.removed_folders) found.push(`${plural(r.removed_folders, 'folder', 'folders')} gone`);
  let s = found.length ? `Rescan found ${joinList(found)}.` : 'Staging already matched the disk.';
  if (r.busy_folders) s += ` ${plural(r.busy_folders, 'folder was', 'folders were')} busy and skipped.`;
  if (r.errors?.length) s += ` ${plural(r.errors.length, 'problem', 'problems')}: ${r.errors[0]}`;
  return s;
}

export function promoteSummary(job) {
  const n = job.folders.length;
  if (job.state === 'running') {
    const at = Math.min(n, job.folders.filter((f) => f.state === 'done' || f.state === 'failed').length + 1);
    return `Promoting folder ${at} of ${n} · ${formatCount(job.files_done)} of ${formatCount(job.files_total)} files`;
  }
  const failed = job.folders.filter((f) => f.state === 'failed').length;
  if (failed) return `${failed} of ${plural(n, 'folder', 'folders')} could not be promoted.`;
  return `Promoted ${plural(n, 'folder', 'folders')} (${plural(job.files_done, 'file', 'files')}) to the archive.`;
}
```

- [ ] **Step 4: Run the tests**: in `web/`, `npm test` → all pass; `npm run build` → succeeds (nothing imports the new modules yet).

- [ ] **Step 5: Commit**

```bash
git add web/src/lib/naming.js web/src/lib/naming.test.js web/src/lib/selection.js web/src/lib/selection.test.js web/src/lib/messages.js web/src/lib/messages.test.js
git commit -m "Add the web naming, selection and message helpers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 14: Web building blocks: dialog, toasts, selection bar, long-press, live events

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/components/Dialog.svelte`, `web/src/components/Toast.svelte`, `web/src/components/SelectionBar.svelte`, `web/src/lib/toast.svelte.js`, `web/src/lib/press.js`
- Modify: `web/src/lib/live.svelte.js`, `web/src/App.svelte`, `web/src/app.css`, `web/src/components/Thumb.svelte`

**Interfaces:**
- Consumes: `LONG_PRESS_MS`, `MOVE_SLOP`, `autoScrollSpeed` (Task 13); the `staging.changed` and `promote.progress` events (Tasks 11, 12).
- Produces:

```js
// live.svelte.js — live gains:
staging: { version: 0, rescanning: false, last_rescan: null }, // the last staging.changed
promote: null,                                               // the last promote.progress (a job) or null
// toast.svelte.js
export const toasts; // $state({ items: [{ id, text, kind }] })
export function toast(text, kind = 'ok', ms = 4000) // kind: ok | warn | bad
export function dismiss(id)
// press.js — a Svelte action for touch selection
export function longPressSelect(node, { onlong, ondrag, ondragend })
```

```svelte
<Dialog open title onclose busy={false} danger={false}> {children} {#snippet actions()}…{/snippet} </Dialog>
<SelectionBar count total onclear onall> {children: action buttons} </SelectionBar>
<Toast />
```

- [ ] **Step 1: `live.svelte.js`** — add to `live`:

```js
  staging: { version: 0, rescanning: false, last_rescan: null }, // the last staging.changed
  promote: null, // the last promote.progress (a promote job), null if none since SD2NAS started
```

and in `connectEvents`:

```js
  source.addEventListener('staging.changed', (e) => {
    live.staging = JSON.parse(e.data);
  });
  source.addEventListener('promote.progress', (e) => {
    live.promote = JSON.parse(e.data);
  });
```

- [ ] **Step 2: `toast.svelte.js` and `Toast.svelte`**

```js
// Short confirmations of finished actions (the mockup's toast).
export const toasts = $state({ items: [] });
let seq = 0;

export function toast(text, kind = 'ok', ms = 4000) {
  const id = ++seq;
  toasts.items.push({ id, text, kind });
  setTimeout(() => dismiss(id), ms);
}

export function dismiss(id) {
  toasts.items = toasts.items.filter((t) => t.id !== id);
}
```

```svelte
<script>
  import { toasts, dismiss } from '../lib/toast.svelte.js';
</script>

<div class="toasts" aria-live="polite">
  {#each toasts.items as t (t.id)}
    <button class="toast {t.kind}" onclick={() => dismiss(t.id)}>{t.text}</button>
  {/each}
</div>

<style>
  .toasts { position: fixed; left: 50%; transform: translateX(-50%); bottom: calc(20px + env(safe-area-inset-bottom)); z-index: 40;
    display: flex; flex-direction: column; gap: 8px; width: min(480px, calc(100% - 32px)); pointer-events: none; }
  .toast { pointer-events: auto; border: 0; border-radius: 8px; padding: 10px 16px; font: inherit; font-weight: 600; text-align: left;
    background: var(--ok); color: #06210f; box-shadow: 0 6px 20px rgba(0,0,0,.35); min-height: 44px; cursor: pointer; }
  .toast.warn { background: var(--warn); color: #1b1b1b; }
  .toast.bad { background: var(--bad); color: #fff; }
  @media (max-width: 760px) {
    .toasts { bottom: calc(var(--tabbar) + 76px + env(safe-area-inset-bottom)); } /* above the tab bar and a selection bar */
  }
</style>
```

Mount `<Toast />` once in `App.svelte`, after `</div>` of `.app`.

- [ ] **Step 3: `Dialog.svelte`** — the native `<dialog>` gives the focus trap, Escape and an inert page for free (iOS 15.4+):

```svelte
<script>
  // A modal on the native <dialog>. Escape and a click on the backdrop
  // close it, except while busy (a request is running).
  let { open = false, title, onclose, busy = false, danger = false, children, actions } = $props();
  let el;
  const id = `dlg-${Math.random().toString(36).slice(2)}`;

  $effect(() => {
    if (open && !el.open) el.showModal();
    else if (!open && el.open) el.close();
  });

  function oncancel(e) {
    e.preventDefault(); // Escape: we decide
    if (!busy) onclose();
  }
  function onclick(e) {
    if (e.target === el && !busy) onclose(); // the backdrop, not the box
  }
</script>

<dialog bind:this={el} {oncancel} {onclick} aria-labelledby={id} class:danger>
  <div class="box">
    <h2 {id}>{title}</h2>
    {@render children?.()}
    <div class="actions">{@render actions?.()}</div>
  </div>
</dialog>

<style>
  dialog { padding: 0; border: 1px solid var(--line); border-radius: 12px; background: var(--panel); color: var(--text);
    width: min(520px, calc(100% - 32px)); max-height: calc(100dvh - 32px - env(safe-area-inset-top) - env(safe-area-inset-bottom)); }
  dialog::backdrop { background: rgba(5, 9, 15, .65); }
  .box { padding: 20px; overflow: auto; }
  h2 { margin: 0 0 12px; }
  .danger h2 { color: var(--bad); }
  .actions { display: flex; gap: 10px; justify-content: flex-end; flex-wrap: wrap; margin-top: 18px; }
  @media (max-width: 760px) {
    dialog { width: 100%; max-width: none; margin: auto 0 0; border-radius: 14px 14px 0 0; border-bottom: 0; }
    .box { padding: 18px 16px calc(18px + env(safe-area-inset-bottom)); }
    .actions { flex-direction: column-reverse; }
    .actions :global(.btn) { justify-content: center; width: 100%; }
  }
</style>
```

(On a phone the dialog is a bottom sheet with full-width buttons; the primary action ends up on top of Cancel.)

- [ ] **Step 4: `SelectionBar.svelte`**

```svelte
<script>
  // While anything is selected: a sticky toolbar under the page header on a
  // desktop, a bar above the tab bar on a phone (Global Constraints).
  let { count, total, onclear, onall, children } = $props();
</script>

<div class="selbar" role="toolbar" aria-label="Selection">
  <span class="count">{count} selected</span>
  {#if count < total}<button class="btn link" onclick={onall}>Select all</button>{/if}
  <button class="btn link" onclick={onclear}>Clear</button>
  <span class="spacer"></span>
  <div class="acts">{@render children()}</div>
</div>
<div class="room" aria-hidden="true"></div>

<style>
  .selbar { position: sticky; top: env(safe-area-inset-top); z-index: 6; display: flex; align-items: center; gap: 10px; flex-wrap: wrap;
    background: var(--bg); padding: 10px 0; margin-bottom: 10px; border-bottom: 1px solid var(--accent); }
  .count { font-weight: 600; }
  .acts { display: flex; gap: 8px; flex-wrap: wrap; }
  .room { display: none; }
  @media (max-width: 760px) {
    .selbar { position: fixed; top: auto; left: 0; right: 0; bottom: calc(var(--tabbar) + env(safe-area-inset-bottom)); margin: 0;
      padding: 8px calc(12px + env(safe-area-inset-right)) 8px calc(12px + env(safe-area-inset-left));
      background: var(--panel); border-top: 1px solid var(--accent); border-bottom: 0; flex-wrap: nowrap; overflow-x: auto; }
    .spacer { display: none; }
    .acts { flex-wrap: nowrap; }
    .room { display: block; height: 64px; } /* so the bar never covers the last row */
  }
</style>
```

- [ ] **Step 5: `press.js`** — touch only; desktop selects with clicks and checkboxes:

```js
import { LONG_PRESS_MS, MOVE_SLOP, autoScrollSpeed } from './selection.js';

// longPressSelect: on a list or grid whose items carry data-key, a finger
// resting LONG_PRESS_MS on an item calls onlong(key). While that finger
// stays down, dragging it calls ondrag(key) for each item it crosses, the
// page does not scroll (except by auto-scroll near the screen edges), and
// lifting it calls ondragend(). The click a long press would produce is
// swallowed. Mouse and pen are ignored.
export function longPressSelect(node, params) {
  let p = params;
  let timer = 0;
  let start = null; // { x, y, key } of the resting finger
  let active = false; // the long press fired; the finger is selecting
  let swallow = false; // eat the click that ends a long press
  let lastKey = null;
  let pos = { x: 0, y: 0 };
  let raf = 0;

  const keyAt = (x, y) => document.elementFromPoint(x, y)?.closest('[data-key]')?.dataset.key ?? null;
  const cancelTimer = () => {
    clearTimeout(timer);
    timer = 0;
  };

  function onstart(e) {
    swallow = false;
    if (e.touches.length !== 1) {
      cancelTimer();
      start = null;
      return;
    }
    const item = e.target.closest?.('[data-key]');
    if (!item || !node.contains(item)) return;
    const t = e.touches[0];
    start = { x: t.clientX, y: t.clientY, key: item.dataset.key };
    cancelTimer();
    timer = setTimeout(() => {
      timer = 0;
      active = true;
      swallow = true;
      lastKey = start.key;
      pos = { x: start.x, y: start.y };
      navigator.vibrate?.(10);
      p.onlong?.(start.key);
    }, LONG_PRESS_MS);
  }

  function onmove(e) {
    const t = e.touches[0];
    if (!active) {
      if (start && Math.hypot(t.clientX - start.x, t.clientY - start.y) > MOVE_SLOP) {
        cancelTimer(); // the finger is scrolling, not resting
        start = null;
      }
      return;
    }
    e.preventDefault(); // hold the page still while selecting
    pos = { x: t.clientX, y: t.clientY };
    hit();
    if (!raf) raf = requestAnimationFrame(scrollStep);
  }

  function hit() {
    const key = keyAt(pos.x, pos.y);
    if (key && key !== lastKey) {
      lastKey = key;
      p.ondrag?.(key);
    }
  }

  function scrollStep() {
    raf = 0;
    if (!active) return;
    const v = autoScrollSpeed(pos.y, window.innerHeight);
    if (v) {
      window.scrollBy(0, v);
      hit();
      raf = requestAnimationFrame(scrollStep);
    }
  }

  function onend() {
    cancelTimer();
    start = null;
    if (active) {
      active = false;
      p.ondragend?.();
    }
  }

  function onclick(e) {
    if (swallow) {
      swallow = false;
      e.preventDefault();
      e.stopPropagation();
    }
  }

  function oncontextmenu(e) {
    if (active || timer) e.preventDefault(); // Android's long-press menu
  }

  node.addEventListener('touchstart', onstart, { passive: true });
  node.addEventListener('touchmove', onmove, { passive: false });
  node.addEventListener('touchend', onend);
  node.addEventListener('touchcancel', onend);
  node.addEventListener('click', onclick, true);
  node.addEventListener('contextmenu', oncontextmenu);
  return {
    update(next) {
      p = next;
    },
    destroy() {
      cancelTimer();
      cancelAnimationFrame(raf);
      node.removeEventListener('touchstart', onstart);
      node.removeEventListener('touchmove', onmove);
      node.removeEventListener('touchend', onend);
      node.removeEventListener('touchcancel', onend);
      node.removeEventListener('click', onclick, true);
      node.removeEventListener('contextmenu', oncontextmenu);
    },
  };
}
```

- [ ] **Step 6: `app.css` additions** (the mockup's button variants and form fields, plus touch rules):

```css
.btn:disabled { opacity: .4; cursor: not-allowed; }
.btn:disabled:hover { border-color: var(--line); }
.btn.primary { background: var(--accent); border-color: var(--accent); color: #fff; }
.btn.primary:hover:not(:disabled) { background: var(--accent2); }
.btn.danger { color: var(--bad); }
.btn.bad { background: var(--bad); border-color: var(--bad); color: #fff; }
.btn.link { background: none; border: 0; color: var(--accent); padding: 0 6px; }
input[type=text], select { background: var(--bg); color: var(--text); border: 1px solid var(--line); border-radius: 7px; padding: 9px 10px; font: inherit; width: 100%; min-height: 44px; }
input[type=checkbox], input[type=radio] { accent-color: var(--accent); width: 18px; height: 18px; margin: 0; }
label.f { display: block; margin: 12px 0 4px; color: var(--muted); font-size: 12px; }
.prefixed { display: flex; gap: 6px; align-items: center; }
.prefixed .mono { white-space: nowrap; color: var(--muted); }
.preview { background: var(--bg); border: 1px solid var(--line); border-radius: 7px; padding: 8px 10px; margin-top: 8px; max-height: 160px; overflow: auto; }
.field-error { color: var(--bad); font-size: 12px; margin-top: 4px; }
/* Long-press selects on touch: no text selection, callout or image drag. */
.selectable { -webkit-user-select: none; user-select: none; -webkit-touch-callout: none; }
```

- [ ] **Step 7: `Thumb.svelte`**: add `draggable="false"` to the `<img>` (a long press must not start an image drag).

- [ ] **Step 8: Check**: in `web/`, `npm test` → pass; `npm run build` → succeeds without new compiler warnings; `git status --short web/dist` prints nothing.

- [ ] **Step 9: Commit**

```bash
git add web/src/components/Dialog.svelte web/src/components/Toast.svelte web/src/components/SelectionBar.svelte web/src/lib/toast.svelte.js web/src/lib/press.js web/src/lib/live.svelte.js web/src/App.svelte web/src/app.css web/src/components/Thumb.svelte
git commit -m "Add dialogs, toasts, the selection bar and touch selection to the web app

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 15: Staging page: select folders, rename, merge, discard, promote, rescan

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/components/PromoteProgress.svelte`
- Replace: `web/src/pages/Staging.svelte`

**Interfaces:**
- Consumes: `api`, `live` (with `staging`, `promote`), `navigate`, `formatBytes`, `formatCount`, `dateRange` (M3); Task 13's `naming.js`, `selection.js`, `messages.js`; Task 14's `Dialog`, `SelectionBar`, `toast`, `longPressSelect`; `Thumb`. The API (Tasks 9, 11, 12): `GET /api/staging` → `{folders, untracked, version, stale_after_days, stale}`; `PATCH /api/staging/{id}`; `POST /api/staging/merge`; `DELETE /api/staging/{id}`; `POST /api/staging/promote`; `POST /api/staging/rescan`.
- Produces: the page at `/staging`; `PromoteProgress` (`{ job, ondismiss }`), also usable elsewhere.

- [ ] **Step 1: `PromoteProgress.svelte`**

```svelte
<script>
  import { formatBytes } from '../lib/format.js';
  import { promoteSummary } from '../lib/messages.js';
  let { job, ondismiss } = $props();
  const running = $derived(job.state === 'running');
  const pct = $derived(job.files_total ? Math.round((job.files_done / job.files_total) * 100) : running ? 0 : 100);
  const icon = { waiting: '○', moving: '●', done: '✓', failed: '✗' };
</script>

<div class="card promote" class:failed={job.state === 'failed'} role="status">
  <div class="row">
    <h2>{running ? 'Promoting to the archive' : job.state === 'failed' ? 'Promote finished with problems' : 'Promoted to the archive'}</h2>
    <span class="spacer"></span>
    {#if !running}<button class="btn link" onclick={ondismiss}>Dismiss</button>{/if}
  </div>
  <p class="muted small" style="margin:0">{promoteSummary(job)}{#if job.bytes_total} · {formatBytes(job.bytes_done)} of {formatBytes(job.bytes_total)}{/if}</p>
  <div class="bar" aria-hidden="true"><i style="width:{pct}%"></i></div>
  <ul>
    {#each job.folders as f (f.id)}
      <li class={f.state}>
        <span class="icon" aria-hidden="true">{icon[f.state]}</span>
        <span class="what"><b>{f.name}</b> → <span class="mono">{f.target}/</span>
          {#if f.merged}<span class="tag">merged into an existing folder</span>{/if}
          {#if f.copied}<span class="tag">copied across disks</span>{/if}
          {#if f.error}<span class="err">{f.error}</span>{/if}
        </span>
      </li>
    {/each}
  </ul>
</div>

<style>
  .promote h2 { margin: 0; }
  ul { list-style: none; margin: 8px 0 0; padding: 0; display: grid; gap: 6px; }
  li { display: flex; gap: 8px; align-items: baseline; }
  .icon { width: 1.2em; flex: none; color: var(--muted); }
  li.done .icon { color: var(--ok); } li.failed .icon { color: var(--bad); } li.moving .icon { color: var(--accent); }
  .what { min-width: 0; overflow-wrap: anywhere; }
  .err { display: block; color: var(--bad); font-size: 12px; }
  .failed { border-color: rgba(229,99,107,.5); }
</style>
```

- [ ] **Step 2: `Staging.svelte`** (replace the file; M3's load/refetch pattern is kept and extended):

```svelte
<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { formatBytes, formatCount, dateRange } from '../lib/format.js';
  import { clickAction, toggleKey, selectRange, prune } from '../lib/selection.js';
  import { splitName, folderName, folderDate, earliestDate, checkTitle, promoteTarget, DEFAULT_PATTERN } from '../lib/naming.js';
  import { describeError, rescanSummary, plural } from '../lib/messages.js';
  import { longPressSelect } from '../lib/press.js';
  import { toast } from '../lib/toast.svelte.js';
  import Thumb from '../components/Thumb.svelte';
  import Dialog from '../components/Dialog.svelte';
  import SelectionBar from '../components/SelectionBar.svelte';
  import PromoteProgress from '../components/PromoteProgress.svelte';

  const PATTERN_KEY = 'sd2nas.promotePattern';
  const RECENT_MS = 10 * 60_000; // how long a finished promote stays on screen

  let data = $state(null);
  let error = $state('');
  let ctl = null; // the load in flight; a newer one aborts it

  let selected = $state(new Set()); // folder ids as strings
  let anchor = null; // the last clicked id, for Shift-click ranges

  let dialog = $state(''); // '' | 'rename' | 'merge' | 'discard' | 'promote'
  let busy = $state(false);
  let dialogError = $state('');
  let title = $state('');
  let pattern = $state(DEFAULT_PATTERN);
  let dismissedJob = $state(0);
  let rescanAsked = false;
  let rescanSeen = null; // finished_at of the last rescan already handled

  async function load() {
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const res = await api('/api/staging', { signal: mine.signal });
      if (mine.signal.aborted) return;
      data = res;
      error = '';
      selected = prune(selected, res.folders.map((f) => String(f.id)));
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      error = err.message;
    }
  }

  // Load on mount, when an import finishes, when the stream comes back after
  // a real drop, and when staging.changed carries a version other than the
  // one on screen (Decision 14).
  let started = false;
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    const version = live.staging.version;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen;
      finishedSeen = finishedCount;
      const stale = data !== null && version !== data.version;
      if (!started || reconnected || finished || stale) {
        started = true;
        load();
      }
    });
  });
  $effect(() => () => ctl?.abort());

  // Report a rescan this page asked for (the startup one stays quiet).
  $effect(() => {
    const r = live.staging.last_rescan;
    untrack(() => {
      if (!r || r.finished_at === rescanSeen) return;
      rescanSeen = r.finished_at;
      if (!rescanAsked) return;
      rescanAsked = false;
      toast(rescanSummary(r), r.skipped || r.errors?.length ? 'warn' : 'ok', 6000);
    });
  });

  const folders = $derived(data?.folders ?? []);
  const order = $derived(folders.map((f) => String(f.id)));
  const chosen = $derived(folders.filter((f) => selected.has(String(f.id))));
  const totals = $derived({ files: folders.reduce((s, f) => s + f.files, 0), bytes: folders.reduce((s, f) => s + f.bytes, 0) });
  const job = $derived(live.promote);
  const promoting = $derived(job?.state === 'running');
  const showJob = $derived(job && (promoting || (job.id !== dismissedJob && Date.now() - Date.parse(job.finished_at) < RECENT_MS)));

  function toggle(key) {
    selected = toggleKey(selected, key);
    anchor = key;
  }
  function clear() {
    selected = new Set();
    anchor = null;
  }
  function selectAll() {
    selected = new Set(order);
  }

  // Desktop: click opens, Ctrl/⌘-click toggles, Shift-click ranges; while
  // anything is selected a click toggles. Touch: tap opens, long-press
  // selects, then taps toggle (Decision 18).
  function onFolderClick(e, f) {
    if (e.target.closest('.check')) return; // the checkbox handles itself
    const key = String(f.id);
    const act = clickAction({ selecting: selected.size > 0, shift: e.shiftKey, toggle: e.ctrlKey || e.metaKey });
    if (act === 'open') {
      if (!e.target.closest('a')) navigate(`/staging/${f.id}`); // a click on the name is a link click already
      return;
    }
    e.preventDefault(); // keeps App's link handler from opening the folder
    selected = act === 'range' ? selectRange(selected, order, anchor, key) : toggleKey(selected, key);
    anchor = key;
  }
  const press = { onlong: (key) => { if (!selected.has(key)) toggle(key); } };

  function onkeydown(e) {
    if (dialog || e.target.closest?.('input, textarea, select')) return;
    if (e.key === 'Escape' && selected.size) clear();
    else if ((e.ctrlKey || e.metaKey) && e.key === 'a' && folders.length) {
      e.preventDefault();
      selectAll();
    }
  }

  function loadPattern() {
    try {
      return localStorage.getItem(PATTERN_KEY) || DEFAULT_PATTERN;
    } catch {
      return DEFAULT_PATTERN; // storage unavailable (private mode): use the default
    }
  }
  function savePattern(p) {
    try {
      localStorage.setItem(PATTERN_KEY, p);
    } catch {
      // storage unavailable (private mode): the next promote starts from the default again
    }
  }

  function openDialog(kind) {
    dialogError = '';
    if (kind === 'rename') {
      const n = splitName(chosen[0].name);
      title = n.date ? n.title : chosen[0].name;
    } else if (kind === 'merge') {
      title = mergeSorted.map((f) => (splitName(f.name).date ? splitName(f.name).title : f.name)).find(Boolean) ?? '';
    } else if (kind === 'promote') {
      pattern = loadPattern();
    }
    dialog = kind;
  }
  function closeDialog() {
    if (!busy) dialog = '';
  }

  // run sends one action; on success the dialog closes, on failure it stays
  // open with the reason (409s name the busy folder).
  async function run(fn) {
    busy = true;
    dialogError = '';
    try {
      await fn();
      dialog = '';
    } catch (err) {
      dialogError = describeError(err);
    } finally {
      busy = false;
      load();
    }
  }

  const titleCheck = $derived(checkTitle(title));

  // Rename
  const renaming = $derived(chosen.length === 1 ? chosen[0] : null);
  const renameName = $derived(renaming ? folderName(folderDate(renaming), titleCheck.title) : '');
  function doRename() {
    const f = renaming;
    run(async () => {
      const res = await api(`/api/staging/${f.id}`, { method: 'PATCH', body: { title: titleCheck.title } });
      toast(`Renamed to “${res.folder.name}”`);
      clear();
    });
  }

  // Merge
  const mergeSorted = $derived([...chosen].sort((a, b) => (folderDate(a) || '9999').localeCompare(folderDate(b) || '9999') || a.id - b.id));
  const mergeName = $derived(folderName(earliestDate(chosen), titleCheck.title));
  const mergeFiles = $derived(chosen.reduce((s, f) => s + f.files, 0));
  function doMerge() {
    run(async () => {
      const res = await api('/api/staging/merge', { method: 'POST', body: { ids: chosen.map((f) => f.id), title: titleCheck.title } });
      toast(`Merged into “${res.folder.name}”`);
      selected = new Set([String(res.folder.id)]);
      anchor = null;
    });
  }

  // Discard (one request per folder; stops at the first failure)
  function doDiscard() {
    const list = [...chosen];
    run(async () => {
      let files = 0;
      let removed = 0;
      const kept = [];
      try {
        for (const f of list) {
          const res = await api(`/api/staging/${f.id}`, { method: 'DELETE' });
          files += res.culled;
          if (res.removed) removed++;
          else kept.push(`“${f.name}” (${res.kept.join(', ')})`);
        }
      } finally {
        if (removed || files) toast(`Discarded ${plural(removed, 'folder', 'folders')} (${plural(files, 'file', 'files')})`);
        if (kept.length) toast(`Kept ${kept.join('; ')}: SD2NAS doesn't track those files. Rescan, then discard again.`, 'warn', 9000);
      }
      clear();
    });
  }

  // Promote
  const targets = $derived(chosen.map((f) => ({ f, ...promoteTarget(f, pattern) })));
  const patternError = $derived(targets.find((t) => t.error)?.error ?? '');
  const undated = $derived(chosen.filter((f) => !splitName(f.name).title || !splitName(f.name).date));
  const promoteBytes = $derived(chosen.reduce((s, f) => s + f.bytes, 0));
  function doPromote() {
    const p = pattern.trim() || DEFAULT_PATTERN;
    run(async () => {
      const res = await api('/api/staging/promote', { method: 'POST', body: { ids: chosen.map((f) => f.id), pattern: p } });
      live.promote = res.job; // show it now; promote.progress events take over
      savePattern(p);
      clear();
    });
  }

  async function rescan() {
    rescanAsked = true;
    try {
      const res = await api('/api/staging/rescan', { method: 'POST' });
      if (!res.started) toast('A rescan is already running.', 'warn');
    } catch (err) {
      rescanAsked = false;
      toast(describeError(err), 'bad');
    }
  }
</script>

<svelte:window {onkeydown} />

<div class="row head">
  <h1>Staging</h1>
  <span class="spacer"></span>
  <button class="btn" onclick={rescan} disabled={live.staging.rescanning}>{live.staging.rescanning ? 'Rescanning…' : 'Rescan staging'}</button>
</div>
{#if error && data}
  <div class="note bad" role="alert">Couldn't refresh staging: {error}</div>
{/if}
{#if error && !data}
  <div class="note bad">Couldn't load staging: {error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  <p class="sub">{plural(folders.length, 'folder', 'folders')} · {formatCount(totals.files)} files · {formatBytes(totals.bytes)}</p>
  {#if showJob}
    <PromoteProgress {job} ondismiss={() => (dismissedJob = job.id)} />
  {/if}
  {#if data.stale}
    <div class="note warn">{plural(data.stale, 'folder has', 'folders have')} been in staging longer than {data.stale_after_days} days. Promote or discard {data.stale === 1 ? 'it' : 'them'}.</div>
  {/if}
  {#if data.untracked.length}
    <div class="note warn row">
      <span>Found {plural(data.untracked.length, 'folder', 'folders')} SD2NAS doesn't track yet: {data.untracked.join(', ')}.</span>
      <button class="btn" onclick={rescan} disabled={live.staging.rescanning}>Rescan to add {data.untracked.length === 1 ? 'it' : 'them'}</button>
    </div>
  {/if}
  {#if folders.length === 0}
    <div class="waiting">
      <h2>Staging is empty</h2>
      <p class="muted" style="margin:0">New imports appear here.</p>
    </div>
  {:else}
    {#if selected.size}
      <SelectionBar count={selected.size} total={folders.length} onclear={clear} onall={selectAll}>
        <button class="btn" disabled={selected.size < 2 || promoting} onclick={() => openDialog('merge')}>Merge</button>
        <button class="btn" disabled={selected.size !== 1 || promoting} onclick={() => openDialog('rename')}>Rename</button>
        <button class="btn danger" disabled={promoting} onclick={() => openDialog('discard')}>Discard</button>
        <button class="btn primary" disabled={promoting} title={promoting ? 'A promote is running' : ''} onclick={() => openDialog('promote')}>Promote →</button>
      </SelectionBar>
    {:else}
      <div class="toolbar row">
        <span class="muted only-desktop">Select folders to merge, rename, discard or promote them.</span>
        <span class="muted only-phone">Long-press a folder to select it.</span>
        <span class="spacer"></span>
        <button class="btn link only-desktop" onclick={selectAll}>Select all</button>
      </div>
    {/if}
    <div class="folders" use:longPressSelect={press}>
      {#each folders as f (f.id)}
        {@const key = String(f.id)}
        <!-- The name link is the keyboard path; a click anywhere on the card is a mouse/touch shortcut. -->
        <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
        <div class="folder selectable" class:sel={selected.has(key)} data-key={key} onclick={(e) => onFolderClick(e, f)}>
          <div class="top">
            <label class="check" title="Select">
              <input type="checkbox" checked={selected.has(key)} onchange={() => toggle(key)} aria-label="Select {f.name}" />
            </label>
            <div class="title">
              <a class="name" href="/staging/{f.id}">{f.name}</a>
              <div class="muted small">{dateRange(f.dates)}</div>
            </div>
            {#if f.stale}<span class="pill warn" title="In staging for {f.age_days} days">{f.age_days} days</span>{/if}
          </div>
          {#if f.samples.length}
            <div class="strip">
              {#each f.samples as s (s.sha256)}<Thumb sha={s.sha256} kind={s.kind} />{/each}
            </div>
          {/if}
          <div class="row small">
            <span>
              {#if f.counts.raw}<span class="tag">{f.counts.raw} RAW</span>{/if}
              {#if f.counts.jpg}<span class="tag">{f.counts.jpg} JPG</span>{/if}
              {#if f.counts.video}<span class="tag">{f.counts.video} video</span>{/if}
            </span>
            <span class="spacer"></span>
            <span class="muted">{plural(f.files, 'file', 'files')} · {formatBytes(f.bytes)}</span>
          </div>
          {#each f.warnings as w}<div class="warn small">⚠ {w}</div>{/each}
        </div>
      {/each}
    </div>
  {/if}
{/if}

<Dialog open={dialog === 'rename'} title="Rename folder" onclose={closeDialog} {busy}>
  {#if renaming}
    <label class="f" for="rename-title">Folder title</label>
    <div class="prefixed">
      {#if folderDate(renaming)}<span class="mono">{folderDate(renaming)} -</span>{/if}
      <!-- svelte-ignore a11y_autofocus -->
      <input id="rename-title" type="text" bind:value={title} placeholder="e.g. Yosemite" autofocus
        onkeydown={(e) => e.key === 'Enter' && !titleCheck.error && renameName && renameName !== renaming.name && doRename()} />
    </div>
    {#if titleCheck.error}<div class="field-error">{titleCheck.error}</div>{/if}
    <p class="muted small">The date stays at the front so the archive stays sorted, e.g. <span class="mono">2007-11-10 - SMU Homecoming</span>.</p>
    {#if renameName}<p class="small">New name: <span class="mono">{renameName}</span></p>{/if}
  {/if}
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doRename} disabled={busy || !!titleCheck.error || !renameName || renameName === renaming?.name}>{busy ? 'Renaming…' : 'Rename'}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'merge'} title="Merge {chosen.length} folders" onclose={closeDialog} {busy}>
  <ul class="muted list">
    {#each mergeSorted as f (f.id)}<li>{f.name} <span class="small">({plural(f.files, 'file', 'files')})</span></li>{/each}
  </ul>
  <label class="f" for="merge-title">Folder title</label>
  <div class="prefixed">
    {#if earliestDate(chosen)}<span class="mono">{earliestDate(chosen)} -</span>{/if}
    <input id="merge-title" type="text" bind:value={title} placeholder="e.g. Yosemite" />
  </div>
  {#if titleCheck.error}<div class="field-error">{titleCheck.error}</div>{/if}
  {#if mergeName}<p class="small">Merged folder: <span class="mono">{mergeName}</span></p>{/if}
  <div class="note">{plural(mergeFiles, 'file ends', 'files end')} up in one folder, still sorted into <span class="mono">Capture/</span> and <span class="mono">Videos/</span>. Filenames keep their timestamps; if two names clash, the second gets a -1 suffix.</div>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doMerge} disabled={busy || !!titleCheck.error || !mergeName}>{busy ? 'Merging…' : 'Merge'}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'discard'} title="Discard {plural(chosen.length, 'folder', 'folders')}?" onclose={closeDialog} {busy} danger>
  <p class="muted">{plural(mergeFiles, 'file is', 'files are')} deleted from staging: {chosen.map((f) => f.name).join(', ')}.</p>
  <div class="note warn">These files stay in the ingest history and won't be re-imported from the card. To get them back, use “Re-import anyway” on that import's page.</div>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn bad" onclick={doDiscard} disabled={busy}>{busy ? 'Discarding…' : `Discard ${plural(mergeFiles, 'file', 'files')}`}</button>
  {/snippet}
</Dialog>

<Dialog open={dialog === 'promote'} title="Promote {plural(chosen.length, 'folder', 'folders')} to the archive" onclose={closeDialog} {busy}>
  <label class="f" for="promote-pattern">Destination pattern</label>
  <input id="promote-pattern" type="text" bind:value={pattern} spellcheck="false" autocapitalize="off" />
  <p class="muted small" style="margin:4px 0 0">Tokens: <span class="mono">{'{year}'}</span> <span class="mono">{'{month}'}</span> <span class="mono">{'{name}'}</span></p>
  <div class="preview mono">
    {#each targets as t (t.f.id)}<div>{t.error ? `${t.f.name}: ${t.error}` : `Archive/${t.path}/`}</div>{/each}
  </div>
  {#if undated.length}
    <div class="note warn">{plural(undated.length, 'folder has', 'folders have')} only a date or only a title for a name. Rename first if you want “date - title” in the archive.</div>
  {/if}
  <div class="note">{plural(mergeFiles, 'file', 'files')} ({formatBytes(promoteBytes)}) move into the archive. Within the photos share that is an instant rename; a folder that already exists in the archive is merged file by file.</div>
  {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
  {#snippet actions()}
    <button class="btn" onclick={closeDialog} disabled={busy}>Cancel</button>
    <button class="btn primary" onclick={doPromote} disabled={busy || !!patternError}>{busy ? 'Starting…' : 'Promote'}</button>
  {/snippet}
</Dialog>

<style>
  .head { margin-bottom: 4px; }
  .head h1 { margin: 0; }
  .folders { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 14px; }
  .folder { display: flex; flex-direction: column; gap: 4px; background: var(--panel); border: 1px solid var(--line); border-radius: var(--r);
    padding: 8px 12px 12px; color: var(--text); min-width: 0; cursor: pointer; transition: border-color .1s; }
  .folder:hover { border-color: #41567a; }
  .folder.sel { border-color: var(--accent); box-shadow: 0 0 0 1px var(--accent); }
  .top { display: flex; gap: 4px; align-items: flex-start; }
  .check { display: grid; place-items: center; width: 44px; height: 44px; margin: -2px 0 0 -10px; flex: none; cursor: pointer; }
  .title { flex: 1; min-width: 0; padding-top: 10px; }
  .name { font-weight: 600; color: var(--text); text-decoration: none; overflow-wrap: anywhere; }
  .name:hover { color: var(--accent); text-decoration: underline; }
  .top .pill { margin-top: 12px; }
  .strip { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 4px; margin: 6px 0; }
  .warn { color: var(--warn); }
  .list { margin: 0; padding-left: 18px; }
  @media (max-width: 760px) {
    .folders { grid-template-columns: minmax(0, 1fr); }
  }
</style>
```

Notes for the implementer:
- `undated` flags names that are only a date (`2025-05-30`) or only a title (an adopted `Holiday`), matching §8 "Promoting a folder with no title shows a warning but is allowed".
- `live.promote = res.job` mutates the shared store on purpose: the progress card shows at once even if the first `promote.progress` event is a moment away.
- Keep the M3 refetch pattern exactly (one AbortController, last good data stays on screen).

- [ ] **Step 3: Check**: in `web/`, `npm test` and `npm run build` → pass, no new compiler warnings (the two `svelte-ignore` comments are deliberate). Then a short manual check against a dev server (Task 16, Step 5 covers the full checklist).

- [ ] **Step 4: Commit**

```bash
git add web/src/components/PromoteProgress.svelte web/src/pages/Staging.svelte
git commit -m "Select, rename, merge, discard and promote staging folders in the UI

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 16: Folder page: select files (long-press and swipe), move, new folder, cull

Suggested implementer: sonnet.

**Files:**
- Replace: `web/src/pages/Folder.svelte`

**Interfaces:**
- Consumes: M3's `Folder.svelte` (load pattern, chips, `Lightbox` with its history entry), `Thumb`, `extLabel`, `kindChips`; Tasks 13–14 (`clickAction`, `toggleKey`, `selectRange`, `dragSelection`, `prune`, `folderDate`, `checkTitle`, `folderName`, `describeError`, `plural`, `longPressSelect`, `Dialog`, `SelectionBar`, `toast`); `navigate`. API: `GET /api/staging/{id}/files` (now with `version`), `GET /api/staging`, `POST /api/staging/move`, `POST /api/staging/cull`.
- Produces: the page at `/staging/{id}`.

- [ ] **Step 1: Replace `Folder.svelte`.** Keep M3's `load`, the refetch `$effect`, the `onMount` history fix, `show`/`hide`/`onpopstate` and the Lightbox exactly as they are, and add selection, the bar and the dialogs. The full file:

```svelte
<script>
  import { onMount, untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { navigate } from '../lib/router.svelte.js';
  import { isId } from '../lib/paths.js';
  import { formatBytes, formatCount, dateRange, kindChips, extLabel } from '../lib/format.js';
  import { clickAction, toggleKey, selectRange, dragSelection, prune } from '../lib/selection.js';
  import { folderDate, folderName, checkTitle } from '../lib/naming.js';
  import { describeError, plural } from '../lib/messages.js';
  import { longPressSelect } from '../lib/press.js';
  import { toast } from '../lib/toast.svelte.js';
  import Thumb from '../components/Thumb.svelte';
  import Lightbox from '../components/Lightbox.svelte';
  import Dialog from '../components/Dialog.svelte';
  import SelectionBar from '../components/SelectionBar.svelte';

  let { id } = $props();
  let kind = $state(''); // the chip asked for; '' = all
  let data = $state(null);
  let shownKind = $state(''); // the kind `data` was loaded for
  let error = $state('');
  let gone = $state(false); // the folder no longer exists (merged, promoted, discarded)
  let open = $state(-1); // index into data.files shown full screen; -1 = none
  let ctl = null;

  let selected = $state(new Set()); // file hashes
  let anchor = null;
  let drag = null; // { base, anchor, adding } while a finger drag-selects

  let dialog = $state(''); // '' | 'move' | 'cull'
  let busy = $state(false);
  let dialogError = $state('');
  let others = $state(null); // the other staging folders, for "Move to…"
  let dest = $state('new'); // 'new' or a folder id as a string
  let title = $state('');

  async function load(k) {
    if (!isId(id)) {
      error = 'There is no such staging folder.';
      return;
    }
    ctl?.abort();
    const mine = (ctl = new AbortController());
    try {
      const res = await api(`/api/staging/${id}/files` + (k ? `?kind=${k}` : ''), { signal: mine.signal });
      if (mine.signal.aborted) return;
      const sha = open >= 0 ? data?.files[open]?.sha256 : null;
      data = res;
      shownKind = k;
      error = '';
      selected = prune(selected, res.files.map((f) => f.sha256));
      if (open >= 0) {
        const j = res.files.findIndex((x) => x.sha256 === sha);
        if (j >= 0) open = j;
        else hide();
      }
    } catch (err) {
      if (err.name === 'AbortError' || mine.signal.aborted) return;
      if (err.status === 404) {
        gone = true;
        data = null;
      }
      error = err.message;
    }
  }

  // M3's triggers, plus staging.changed with a version other than the one
  // on screen (another tab or device changed this folder).
  let started = false;
  let lastKind = '';
  let wasConnected = false;
  let wasConnectedEver = false;
  let finishedSeen = 0;
  $effect(() => {
    const k = kind;
    const connected = live.connected;
    const finishedCount = live.finishedCount;
    const version = live.staging.version;
    untrack(() => {
      const reconnected = connected && !wasConnected && wasConnectedEver;
      if (connected) wasConnectedEver = true;
      wasConnected = connected;
      const finished = finishedCount !== finishedSeen;
      finishedSeen = finishedCount;
      const changed = k !== lastKind;
      lastKind = k;
      const stale = data !== null && version !== data.version;
      if (!gone && (!started || changed || reconnected || finished || stale)) {
        started = true;
        load(k);
      }
    });
  });
  $effect(() => () => ctl?.abort());

  onMount(() => {
    if (history.state?.lightbox) history.back();
  });

  function show(i) {
    if (open >= 0) return;
    open = i;
    history.pushState({ lightbox: true }, '');
  }
  function hide() {
    if (history.state?.lightbox) history.back();
    else open = -1;
  }
  function onpopstate() {
    if (!history.state?.lightbox) open = -1;
  }

  const files = $derived(data?.files ?? []);
  const order = $derived(files.map((f) => f.sha256));
  const selecting = $derived(selected.size > 0);

  function toggle(key) {
    selected = toggleKey(selected, key);
    anchor = key;
  }
  function clear() {
    selected = new Set();
    anchor = null;
  }
  function selectAll() {
    selected = new Set(order);
  }

  function onTileClick(e, i) {
    const key = files[i].sha256;
    const act = clickAction({ selecting, shift: e.shiftKey, toggle: e.ctrlKey || e.metaKey });
    if (act === 'open') return show(i);
    selected = act === 'range' ? selectRange(selected, order, anchor, key) : toggleKey(selected, key);
    anchor = key;
  }

  // Touch: long-press selects (or unselects) a tile; keep the finger down
  // and drag to do the same to every tile between it and the finger.
  const press = {
    onlong(key) {
      const adding = !selected.has(key);
      drag = { base: new Set(selected), anchor: key, adding };
      selected = dragSelection(drag.base, order, key, key, adding);
    },
    ondrag(key) {
      if (drag) selected = dragSelection(drag.base, order, drag.anchor, key, drag.adding);
    },
    ondragend() {
      if (drag) anchor = drag.anchor;
      drag = null;
    },
  };

  function onkeydown(e) {
    if (dialog || open >= 0 || e.target.closest?.('input, textarea, select')) return;
    if (e.key === 'Escape' && selecting) clear();
    else if ((e.ctrlKey || e.metaKey) && e.key === 'a' && files.length) {
      e.preventDefault();
      selectAll();
    }
  }

  // run sends one action. fn returns true when it navigated away.
  async function run(fn) {
    busy = true;
    dialogError = '';
    try {
      const left = await fn();
      dialog = '';
      if (!left) load(shownKind);
    } catch (err) {
      dialogError = describeError(err);
      load(shownKind);
    } finally {
      busy = false;
    }
  }

  const titleCheck = $derived(checkTitle(title));
  const prefix = $derived(data ? folderDate(data.folder) : '');
  const newName = $derived(folderName(prefix, titleCheck.title));
  const moveReady = $derived(dest === 'new' ? !!titleCheck.title && !titleCheck.error : dest !== '');

  async function openMove() {
    dialogError = '';
    title = '';
    dest = 'new';
    others = null;
    dialog = 'move';
    try {
      const res = await api('/api/staging');
      others = res.folders.filter((f) => f.id !== data.folder.id);
    } catch (err) {
      others = [];
      dialogError = `Couldn't list the other folders: ${describeError(err)}`;
    }
  }

  function doMove() {
    const shas = [...selected];
    const body = dest === 'new' ? { fileShas: shas, newTitle: titleCheck.title } : { fileShas: shas, toId: Number(dest) };
    run(async () => {
      const res = await api('/api/staging/move', { method: 'POST', body });
      toast(`Moved ${plural(res.moved, 'file', 'files')} to “${res.folder.name}”`);
      clear();
      if (res.removed.includes(data.folder.id)) {
        navigate('/staging', { replace: true });
        return true;
      }
      return false;
    });
  }

  function doCull() {
    const shas = [...selected];
    run(async () => {
      const res = await api('/api/staging/cull', { method: 'POST', body: { fileShas: shas } });
      toast(`Culled ${plural(res.culled, 'file', 'files')}`);
      clear();
      if (res.removed.includes(data.folder.id)) {
        navigate('/staging', { replace: true });
        return true;
      }
      return false;
    });
  }
</script>

<svelte:window {onpopstate} {onkeydown} />

<p><a href="/staging" class="btn">← Staging</a></p>

{#if gone}
  <div class="note warn">This folder no longer exists: it was merged, promoted or discarded. <a href="/staging">Back to staging</a></div>
{:else if error && !data}
  <div class="note bad">{error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  {@const f = data.folder}
  <h1>{f.name}</h1>
  <p class="sub">{dateRange(f.dates)} · {plural(f.files, 'file', 'files')} · {formatBytes(f.bytes)}</p>
  {#if error}
    <div class="note bad" role="alert">Couldn't refresh this folder: {error}</div>
  {/if}
  {#each f.warnings as w}<div class="note warn">{w}</div>{/each}
  {#if selecting}
    <SelectionBar count={selected.size} total={files.length} onclear={clear} onall={selectAll}>
      <button class="btn" onclick={openMove}>Move to…</button>
      <button class="btn danger" onclick={() => { dialogError = ''; dialog = 'cull'; }}>Cull</button>
    </SelectionBar>
  {/if}
  <div class="toolbar row" class:hidden-phone={selecting}>
    <div class="chips">
      {#each kindChips(f.counts) as c (c.kind)}
        <button class="chip" class:on={shownKind === c.kind} onclick={() => (kind = c.kind)}>{c.label} {c.count}</button>
      {/each}
    </div>
    <span class="spacer"></span>
    {#if !selecting}
      <span class="muted small only-desktop">Ctrl-click or tick files to select them</span>
      <span class="muted small only-phone">Long-press to select; drag to select more</span>
    {/if}
  </div>
  {#if files.length === 0}
    <p class="muted">No files here.</p>
  {:else}
    <div class="grid" class:selecting use:longPressSelect={press}>
      {#each files as file, i (file.sha256)}
        {@const on = selected.has(file.sha256)}
        <div class="cell selectable" class:sel={on} data-key={file.sha256}>
          <button class="tile" onclick={(e) => onTileClick(e, i)} aria-label={selecting ? `${on ? 'Unselect' : 'Select'} ${file.name}` : `Show ${file.name}`}>
            <Thumb sha={file.sha256} kind={file.kind} label={extLabel(file)} />
            <span class="fn">{#if file.clock_warnings?.length}<span class="warn" title={file.clock_warnings.join('; ')}>⚠ </span>{/if}{file.name}</span>
          </button>
          <button class="pick" class:on onclick={() => toggle(file.sha256)} aria-pressed={on} aria-label="{on ? 'Unselect' : 'Select'} {file.name}"><span>✓</span></button>
        </div>
      {/each}
    </div>
  {/if}
  {#if open >= 0 && open < files.length}
    <Lightbox files={data.files} bind:index={open} onclose={hide} />
  {/if}

  <Dialog open={dialog === 'move'} title="Move {plural(selected.size, 'file', 'files')}" onclose={() => !busy && (dialog = '')} {busy}>
    <div class="dests" role="radiogroup" aria-label="Destination">
      <label class="dest"><input type="radio" bind:group={dest} value="new" /> <span>New folder…</span></label>
      {#if dest === 'new'}
        <div class="newfolder">
          <div class="prefixed">
            {#if prefix}<span class="mono">{prefix} -</span>{/if}
            <input type="text" bind:value={title} placeholder="Title, e.g. Picks" aria-label="New folder title" />
          </div>
          {#if titleCheck.error}<div class="field-error">{titleCheck.error}</div>{/if}
          {#if titleCheck.title && !titleCheck.error}<p class="small" style="margin:6px 0 0">New folder: <span class="mono">{newName}</span></p>{/if}
        </div>
      {/if}
      {#if others === null}
        <p class="muted small">Loading folders…</p>
      {:else}
        {#each others as o (o.id)}
          <label class="dest"><input type="radio" bind:group={dest} value={String(o.id)} /> <span>{o.name} <span class="muted small">({plural(o.files, 'file', 'files')})</span></span></label>
        {/each}
      {/if}
    </div>
    <p class="muted small">Files keep their names and their <span class="mono">Capture/</span> or <span class="mono">Videos/</span> subfolder; a clash gets a -1 suffix.</p>
    {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
    {#snippet actions()}
      <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
      <button class="btn primary" onclick={doMove} disabled={busy || !moveReady}>{busy ? 'Moving…' : 'Move'}</button>
    {/snippet}
  </Dialog>

  <Dialog open={dialog === 'cull'} title="Cull {plural(selected.size, 'file', 'files')}?" onclose={() => !busy && (dialog = '')} {busy} danger>
    <p class="muted">They are deleted from staging.</p>
    <div class="note">They stay in the ingest history, so they won't be re-imported the next time this card goes in.</div>
    {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
    {#snippet actions()}
      <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
      <button class="btn bad" onclick={doCull} disabled={busy}>{busy ? 'Culling…' : `Cull ${plural(selected.size, 'file', 'files')}`}</button>
    {/snippet}
  </Dialog>
{/if}

<style>
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 8px; }
  .cell { position: relative; min-width: 0; border-radius: 6px; }
  .tile { display: block; width: 100%; background: none; border: 0; padding: 0; color: inherit; font: inherit; text-align: left; cursor: pointer; border-radius: 6px; }
  .tile:hover { outline: 2px solid var(--accent); outline-offset: 1px; }
  .cell.sel :global(.thumb) { outline: 3px solid var(--accent); outline-offset: 1px; }
  .cell.sel .tile :global(img) { opacity: .8; }
  .fn { display: block; font-size: 11px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; margin-top: 3px; }
  .warn { color: var(--warn); }
  /* The tick: a 44 px hit area with a 24 px circle, top-left on the thumbnail.
     Shown on hover with a mouse, and always while selecting. */
  .pick { position: absolute; top: 0; left: 0; width: 44px; height: 44px; padding: 0; border: 0; background: none; cursor: pointer;
    display: grid; place-items: center; opacity: 0; transition: opacity .1s; }
  .pick span { width: 24px; height: 24px; border-radius: 50%; display: grid; place-items: center; font-size: 13px; font-weight: 700;
    background: rgba(0,0,0,.45); border: 2px solid #fff; color: transparent; }
  .pick.on span { background: var(--accent); border-color: var(--accent); color: #fff; }
  .selecting .pick, .pick.on, .pick:focus-visible { opacity: 1; }
  @media (hover: hover) { .cell:hover .pick { opacity: 1; } }
  .dests { display: grid; gap: 2px; max-height: 50dvh; overflow: auto; margin: 0 -4px; padding: 0 4px; }
  .dest { display: flex; gap: 10px; align-items: center; min-height: 44px; cursor: pointer; overflow-wrap: anywhere; }
  .newfolder { margin: 0 0 8px 28px; }
  @media (max-width: 760px) {
    .grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 4px; }
    .hidden-phone { display: none; }
  }
</style>
```

Notes:
- With nothing selected a click or tap opens the preview as in M3; the tick (hover on desktop) or a long-press (touch) starts selecting; while selecting, clicks and taps toggle and the preview doesn't open.
- Switching chips prunes the selection to the files shown (a hidden file is never moved or culled by surprise).
- `navigate('/staging', { replace: true })` when the folder itself was emptied, so Back doesn't land on a page that 404s.

- [ ] **Step 2: Check**: in `web/`, `npm test` and `npm run build` → pass, no new compiler warnings.

- [ ] **Step 3: Run the app** for the manual checks: `go run ./cmd/sd2nas --cards ./testdata/cards --photos <tmp>/photos --config <tmp>/config` (with `npm run build` done first so the UI is embedded), import both fixture cards with `curl -s -X POST -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' 'http://localhost:8080/api/ingest?wait=1&format=text' -d '{"path":"<abs>/testdata/cards/SONY_A7IV"}'` (and `GOPRO`), and add a few extra JPEGs by copying the fixture JPG under new names into `<tmp>/photos/Staging/2025-06-01 - Beach/` then pressing **Rescan staging** (so there are several folders and a grid worth selecting). Open it in the built-in browser pane (preview_start with the URL).

- [ ] **Step 4: Fix what the checks find**, then `npm test`, `npm run build`.

- [ ] **Step 5: Manual checklist** (Review Focus 8). At desktop width (≥ 1200 px) **and** at 375×812 (`resize_window` preset mobile; reload after switching; reset to desktop at the end). Record the result of each line in the report:
  - Staging list, desktop: click on a card opens it; Ctrl-click and the checkbox select; Shift-click selects a range; Esc clears; Ctrl-A selects all; the sticky selection bar shows the right buttons (Merge only with ≥ 2, Rename only with 1).
  - Staging list, phone: tap opens; long-press selects (no iOS callout/text selection, no context menu); then taps toggle; the selection bar sits above the tab bar and never covers the last card; dialogs open as bottom sheets with full-width buttons.
  - Rename keeps the date prefix and previews the new name; a bad character shows the error and disables Rename; a 409 (hold the lock by starting an import of the GOPRO card with a large file, or trust Task 11's test) shows the server's sentence in the dialog.
  - Merge previews `<earliest date> - <title>`; afterwards one folder remains and it is selected.
  - Discard asks in red and lists the folders; the toast reports the result.
  - Promote previews `Archive/<target>/` per folder, warns for date-only names, rejects `{day}`; the progress card appears at once, fills, and turns into the done summary; Promote is disabled while it runs.
  - Folder grid, desktop: hover shows the tick; clicking it selects; Ctrl/Shift-click work; clicking a thumbnail with nothing selected opens the preview (←/→/Esc still work).
  - Folder grid, phone: long-press a tile then drag across others → they select without the page scrolling; dragging back unselects the overshoot; dragging near the bottom edge auto-scrolls; lifting the finger doesn't open the preview; tapping then toggles.
  - Move to a new folder and to an existing one; moving every file away returns to the staging list; Cull asks in red and the culled files' thumbnails stop loading (404).
  - Rescan staging: the button shows "Rescanning…", then a toast summarizes; untracked folders get the "Rescan to add" note.
  - The age banner and pill are optional here (Task 9's tests pin the data, and the dev catalog has no old folders); say in the report whether you looked at them.

- [ ] **Step 6: Commit**

```bash
git add web/src/pages/Folder.svelte
git commit -m "Select, move and cull files in a staging folder, by mouse or touch

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 17: Import detail: "Forget this import" and "Re-import anyway"; history shows forgotten imports

Suggested implementer: sonnet.

**Files:**
- Modify: `web/src/pages/IngestDetail.svelte`, `web/src/pages/Ingest.svelte`

**Interfaces:**
- Consumes: Task 12's `GET /api/ingest/{id}/forget` → `{staged, staged_bytes, culled, archived, reimportable, forgotten}`, `POST /api/ingest/{id}/forget {deleteFiles}` → `{result: {forgotten, deleted_files, kept_archived, removed}, ingest}`, `POST /api/ingest/{id}/reimport-culled` → `{forgotten, started, message}`; the ingest view's `forgotten_at`; `Dialog`, `toast`, `describeError`, `plural`, `navigate`, `formatWhen`, `formatBytes`, `live.current`.
- Produces: UI only.

- [ ] **Step 1: `IngestDetail.svelte`** — add to the script:

```js
  import { navigate } from '../lib/router.svelte.js';
  import { describeError, plural } from '../lib/messages.js';
  import { toast } from '../lib/toast.svelte.js';
  import Dialog from '../components/Dialog.svelte';

  let forget = $state(null); // GET /api/ingest/{id}/forget, for finished imports
  let dialog = $state(''); // '' | 'forget' | 'reimport'
  let busy = $state(false);
  let dialogError = $state('');
  let deleteFiles = $state(false); // Decision 10: starts unticked

  // What forgetting would do; fetched after every successful load of a
  // finished import (call it from load(), right after `ingest = data`, when
  // data.status is neither queued nor running). A failure only hides the
  // two actions: the page itself already shows API trouble.
  async function loadForget() {
    try {
      forget = await api(`/api/ingest/${id}/forget`);
    } catch (err) {
      forget = null;
      console.warn('could not load what forgetting this import would do', err);
    }
  }

  const importing = $derived(live.current.running !== null || live.current.queued.length > 0);

  async function run(fn) {
    busy = true;
    dialogError = '';
    try {
      await fn();
      dialog = '';
    } catch (err) {
      dialogError = describeError(err);
    } finally {
      busy = false;
      load();
    }
  }

  function doForget() {
    run(async () => {
      const res = await api(`/api/ingest/${id}/forget`, { method: 'POST', body: { deleteFiles } });
      const r = res.result;
      toast(deleteFiles
        ? `Forgot this import and deleted ${plural(r.deleted_files, 'file', 'files')} from staging. The card imports them again next time.`
        : `Forgot this import. Its files stay in staging untracked until the next import or rescan.`, 'ok', 7000);
    });
  }

  function doReimport() {
    run(async () => {
      const res = await api(`/api/ingest/${id}/reimport-culled`, { method: 'POST' });
      toast(res.message, res.forgotten ? 'ok' : 'warn', 7000);
      if (res.started) navigate('/ingest');
    });
  }
```

In `load()`, after `ingest = data; error = '';`, add `if (data.status !== 'queued' && data.status !== 'running') loadForget();` (so the summary is refreshed after a forget, a re-import or a finished import, and never fetched while the import is live).

In the markup:
- In the header, after `<h1>`, when `ingest.forgotten_at`: `<span class="pill">Forgotten {formatWhen(ingest.forgotten_at)}</span>`.
- In the "imported before but are no longer kept" `<details>`, after the paragraph: when `forget?.reimportable`:

```svelte
      <button class="btn" disabled={importing} onclick={() => { dialogError = ''; dialog = 'reimport'; }}>Re-import these anyway</button>
      {#if importing}<span class="muted small"> Wait for the running import to finish.</span>{/if}
```

- At the end of the page, for a finished import that isn't forgotten:

```svelte
  {#if forget && !forget.forgotten}
    <div class="card forget">
      <h2>Forget this import</h2>
      <p class="muted small" style="margin:0 0 10px">SD2NAS forgets the files this import brought in or skipped as culled, so inserting the same card imports them again (to retest a card, say). Files already in the archive stay known, and the import stays in this history.</p>
      <button class="btn danger" disabled={importing} onclick={() => { dialogError = ''; deleteFiles = false; dialog = 'forget'; }}>Forget this import…</button>
      {#if importing}<span class="muted small"> Wait for the running import to finish.</span>{/if}
    </div>
  {/if}

  <Dialog open={dialog === 'forget'} title="Forget this import?" onclose={() => !busy && (dialog = '')} {busy} danger>
    {#if forget}
      <ul class="muted list">
        <li>{plural(forget.staged, 'file', 'files')} still in staging ({formatBytes(forget.staged_bytes)})</li>
        <li>{plural(forget.culled, 'culled file', 'culled files')}</li>
        <li>{plural(forget.archived, 'file', 'files')} already in the archive stay known</li>
      </ul>
      {#if forget.staged}
        <label class="toggle"><input type="checkbox" bind:checked={deleteFiles} /> <span>Also delete the {plural(forget.staged, 'file', 'files')} still in staging</span></label>
      {/if}
      <div class="note warn">{deleteFiles
        ? 'The files are deleted from staging and SD2NAS forgets them. The next time this card goes in, they are imported again as new.'
        : 'SD2NAS forgets these files. Their copies stay in staging, untracked, until the next import of this card or a rescan picks them up again.'}</div>
    {/if}
    {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
    {#snippet actions()}
      <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
      <button class="btn bad" onclick={doForget} disabled={busy}>{busy ? 'Forgetting…' : deleteFiles ? `Forget and delete ${plural(forget?.staged ?? 0, 'file', 'files')}` : 'Forget'}</button>
    {/snippet}
  </Dialog>

  <Dialog open={dialog === 'reimport'} title="Re-import {plural(forget?.reimportable ?? 0, 'file', 'files')}?" onclose={() => !busy && (dialog = '')} {busy}>
    <p class="muted">SD2NAS forgets that you culled {forget?.reimportable === 1 ? 'this file' : 'these files'}. If the card is still inserted they are imported right away; otherwise they come back the next time it goes in.</p>
    {#if dialogError}<div class="note bad" role="alert">{dialogError}</div>{/if}
    {#snippet actions()}
      <button class="btn" onclick={() => (dialog = '')} disabled={busy}>Cancel</button>
      <button class="btn primary" onclick={doReimport} disabled={busy}>{busy ? 'Working…' : 'Re-import'}</button>
    {/snippet}
  </Dialog>
```

and styles: `.list { margin: 0 0 10px; padding-left: 18px; }`, `.toggle { display: flex; gap: 10px; align-items: center; min-height: 44px; cursor: pointer; }`, `.forget { margin-top: 16px; }`.

- [ ] **Step 2: `Ingest.svelte`** — in both the desktop table's status cell and the phone list, after the status pill: `{#if i.forgotten_at}<span class="pill" title="Forgotten {formatWhen(i.forgotten_at)}">Forgotten</span>{/if}`.

- [ ] **Step 3: Check and look**: `npm test`, `npm run build` → pass. With the dev setup from Task 16, Step 3, at desktop width and 375×812: import the SONY card, open its import page → "Forget this import…" → the dialog's counts are right; tick "Also delete" → the button reads "Forget and delete 2 files"; confirm → the toast, the "Forgotten" pill on the page and in the history, the staging list is empty; import the card again → 2 new. Cull the JPG, import again → the "seen before" section offers "Re-import these anyway"; confirm → the import starts (you land on Card ingest) and the JPG is back in staging. Both buttons are disabled while an import runs.

- [ ] **Step 4: Commit**

```bash
git add web/src/pages/IngestDetail.svelte web/src/pages/Ingest.svelte
git commit -m "Forget an import or re-import its culled files from the import page

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 18: PLAN.md: tick M4, record the decisions; final verification

Suggested implementer: haiku.

**Files:**
- Modify: `docs/PLAN.md`

**Interfaces:**
- Consumes: this plan's Decisions.
- Produces: documentation only.

- [ ] **Step 1: Edit `docs/PLAN.md`**

1. In §16 **M4**, tick all seven items (`- [x]`) and add under them:

```markdown
Implementation plan: `docs/plans/m4-staging-ops.md`. Imports hold the lock
of every staging folder they write into until they end; operations take
their locks without waiting and answer 409 (`folder_busy`, naming who holds
it) when a folder is busy; the rescan skips busy folders. Merge and promote
move every file of a folder, keeping its path inside the folder (so
`Capture/`, `Videos/` and any sidecars survive); discard and cull delete only
catalogued files, so a folder holding untracked files stays. Promote is one
background job at a time (`promote.progress`), an instant rename when the
archive folder doesn't exist, a file-by-file merge with the collision loop
when it does, copy-verify-delete across filesystems. The startup reconcile
also runs from "Rescan staging": it adopts untracked folders and media,
follows files moved by hand, culls files deleted by hand, and does nothing
when `Staging/` is missing. "Forget this import" deletes the import's
staged and culled assets with their fingerprints, aliases and clock
warnings (optionally its files in Staging/); the ingest row gets
`forgotten_at` (schema version 2). "Re-import anyway" forgets the culls of
an import's seen-before files and starts an import if the card is still
mounted. Culled, discarded and forgotten files lose their cached
thumbnails. Folders older than `staging_warn_days` (default 30) are
flagged. The UI selects by click, Ctrl/⌘-click, Shift-click and checkboxes
on desktop, and by long-press and drag on touch.
```

2. In §6 under **Culled stays remembered**, add: ``"Forget this import" (§8) is the one exception to "never deleted": it deletes the forgotten assets' fingerprints so the card imports them again. Schema version 2 adds `ingests.forgotten_at`.``
3. In §8, replace the paragraph that starts `**M3 (read-only) lists from the catalog.**` with:

```markdown
**Listing and reconcile.** A staging folder's files are the `staged`
assets under `Staging/<name>/` in the catalog (thumbnails and clock
warnings need the content hash). The reconcile pass, at startup and from
"Rescan staging", brings the catalog in line with the disk: it adopts
directories with no row and media files the catalog doesn't know, follows
files moved by hand, culls files deleted by hand, and drops rows whose
directory is gone. Non-media files are left alone, and nothing is changed
when `Staging/` itself is missing.
```

4. In §10, change the event sentence to: ``Event types: `ingest.current` and `ingest.finished` (M3); `staging.changed` and `promote.progress` (M4); `indexer.progress` is reserved for M5.`` Add these lines to the endpoint block, next to their neighbours: `GET    /api/ingest/{id}/forget           what forgetting would do (M4)`, `GET    /api/staging/promote              the running or last promote job`, `POST   /api/staging/rescan               reconcile Staging/ with the disk`.
5. In §3, after "The threshold is a setting.", add: ``It is `staging_warn_days` (default 30, 0 turns it off); the daily notification comes with M6's card script.``
6. Add these follow-ups as unchecked items:
   - Under **M5**: ``- [ ] When the indexer finds content in `Archive/` whose asset is `culled` (a promote interrupted between the rename and the catalog write), mark it `archived` at that path``
   - Under **M6**: ``- [ ] Daily notification for folders in staging longer than `staging_warn_days` ``
   - Under **M7**: ``- [ ] Settings page: `staging_warn_days`, the default promote pattern (now remembered per device), `day_starts_at` ``

- [ ] **Step 2: Check CI and Docker need nothing** — `ci.yml` already runs `go vet`, `go test` with the tools, `npm ci/test/build` and `go test ./web/` with `SD2NAS_REQUIRE_UI=1`; the Dockerfile's node stage runs `npm test` and the build. No new files outside `cmd/`, `internal/` and `web/` need copying. If anything does, stop and report instead of changing `.github/` or `deploy/`.

- [ ] **Step 3: Final verification of the whole branch** — run each and paste the tail of its output in the report:
- `gofmt -l .` prints nothing.
- `go vet ./...` and `go test ./...` pass on Windows.
- The Linux command and the race command from the Global Constraints pass.
- In `web/`: `npm ci && npm test && npm run build`, then `SD2NAS_REQUIRE_UI=1 go test ./web/ -v` passes.
- Image smoke run: `docker build -f deploy/Dockerfile -t sd2nas:m4 .`; run it with `testdata/cards` read-only at `/cards`, temp dirs at `/photos` and `/config`, `-p 18080:8080`; `POST /api/ingest?wait=1&format=text` with `{"path":"/cards/SONY_A7IV"}` → "2 new … Card safe to format."; `PATCH /api/staging/1` `{"title":"Smoke"}` → 200; `POST /api/staging/promote` `{"ids":[1],"pattern":""}` → 202; `GET /api/staging/promote` soon says `done` and `/photos/Archive/2025-05-30 - Smoke/` holds the files; `POST /api/ingest/1/forget` `{"deleteFiles":true}` → 200 (`kept_archived` 2, nothing forgotten: they are archived); `docker stop` exits within 2 s. Remove the container and image afterwards.
- `git status --short` shows only `HANDOFF.md` untracked.
- `git log --format=%B main..HEAD | grep -c 'Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>'` equals `git rev-list --count main..HEAD`, and `git log --format=%B main..HEAD | grep -ci 'co-authored-by'` equals the same number.

- [ ] **Step 4: Commit**

```bash
git add docs/PLAN.md
git commit -m "Tick M4 in the plan and record its decisions and follow-ups

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
