# M7 Polish Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Finish v1. The owner can protect SD2NAS with an optional shared token, change every catalog setting (including a new time for the daily stale-staging notice) from a Settings page, see staging as cards, a list or a timeline, fix a wrong camera clock with a "Shift times" tool, re-import files whose archive copy went missing, and watch long videos copy byte by byte. Thumbnails stop re-rendering corrupt files and reuse one exiftool process. The Docker base images are pinned and the template follows `:latest`, so pushing `v0.1.0` (the owner's step) ships the first release.

**Architecture:**
- **Token auth** (`internal/api/auth.go`): one middleware between the request guard and the mux. No token configured → it does nothing. With `SD2NAS_TOKEN`, every `/api/` request needs `Authorization: Bearer <token>` (the card script, unchanged) or the `sd2nas_session` cookie, which `POST /api/login` sets once per browser. Cookies also cover `EventSource` and `<img>`, which can't send headers. `/healthz` and the UI's static files stay open.
- **Settings:** catalog key `staging_notice_at` (default `09:00`). `PUT /api/settings` calls `idx.SettingsChanged()` and then the server's settings hooks; `MountNotifications` registers `notices.Service.SettingsChanged`, which makes the notices loop re-read the time and check at once.
- **Notices:** the missing-files notice moves from "the last pass's count" to a catalog watermark (`assets` marked missing after the newest `missing:` notice), which also covers a pass that crashed and two passes finishing within one 30 s check.
- **Thumbnails:** an in-memory per-file failure cache, and one `exiftool -stay_open True -@ -` process (`internal/thumbs/exifserver.go`) that serves RAW previews one request at a time and is killed on timeout, protocol doubt or `Close`.
- **Ingest:** `media.CopyHashedProgress` reports bytes as they are read; the run writes them into `ProgressSnapshot` (`current`, `current_size`, `current_done`, `waiting_folder`, `waiting_holder`). The existing 500 ms ingest feed is the throttle; no new event and no new hub call.
- **Re-import missing:** catalog **schema v5** adds `restore_requests`. `POST /api/ingest/{id}/reimport-missing` records requests (and imports now if the card is still mounted); the next import of that card copies requested files and flips the asset `missing → staged` when the copy's hash equals it.
- **Shift times** (`internal/staging/shift.go`): staged files of one folder, by a signed number of seconds, on the wall clock. Renames only the leading timestamp, re-files by the day cutoff, never touches bytes, never overwrites, takes locks without waiting.
- **Web:** Staging page Cards/List/Timeline, Folder page Grid/List, a Shift dialog, a Settings page (new nav tab), a Login page, live copy detail, and a "Re-import missing files" button.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `net/http`, `crypto/hmac`, `crypto/sha256`, `crypto/subtle`, `os/exec`, `log/slog`, `modernc.org/sqlite`; Svelte 5 + Vite (runes), `node --test`. No new dependencies.

**Spec:** `docs/PLAN.md` §16 M7 (10 items), §10 (API, request guard, optional token), §12 (camera clocks, the shift tool), §8 (staging rules, locks, names), §3 (stale-staging notice, backup), §6 (schema), §14 (container, template). Context: `HANDOFF.md` ("M7 must build on / must not break", "Follow-ups worth folding into M7", lock-order rules). UI: `docs/ui-mockup.html`. Structure template: `docs/plans/m6-unraid-integration.md`.

## Global Constraints

- Module `github.com/aspiziri/SD2NAS`, `go 1.24.0`, no `toolchain` line.
  - **No API newer than Go 1.24:** no `sync.WaitGroup.Go`, `testing/synctest`, `T.Output`, 1.25 `os.Root` methods. Allowed: `context.WithoutCancel`, `context.AfterFunc`, `min`/`max`, `slices`, `maps`, `t.Context()`, `slog.DiscardHandler`, range over int, `sync.Mutex.TryLock`, `exec.Cmd.WaitDelay`, `http.SameSiteStrictMode`.
- **No new dependencies.** `go.mod`, `go.sum`, `web/package.json` and `web/package-lock.json` must not change. No `golang.org/x/...` import. No router, date, auth, chart or test library in `web/`.
- **M7 touches:** `internal/api/`, `internal/catalog/`, `internal/notices/`, `internal/thumbs/`, `internal/ingest/` (`progress.go`, `run.go`, `manager.go` only if a test seam needs it), `internal/media/copy.go`, `internal/staging/` (new `shift.go`, `forget.go`, `staging.go` `diskFolders`), `internal/config/config.go`, `cmd/sd2nas/`, `web/src/`, `deploy/Dockerfile`, `deploy/unraid/sd2nas.xml`, `deploy/template_test.go`, new `deploy/dockerfile_test.go`, `README.md`, `docs/PLAN.md`.
  - **Do not modify** `internal/indexer/` (non-test files), `internal/cardwatch/`, `internal/lockfile/`, `deploy/unassigned-devices.sh`, `deploy/script_test.go`, `deploy/entrypoint.sh`, `.github/`. The card-script contract (`POST /api/ingest?wait=1&format=text` and `POST /api/notifications/take?format=text` answering `level\nmessage\n`) keeps its shape.
- **Binding values** (reviewers check them verbatim):
  - **Token:**
    - Env `SD2NAS_TOKEN` only (no flag, so it never shows in `ps`). Empty = no token. Otherwise it must match `^[A-Za-z0-9._~+/=-]+$` (safe inside the bash-sourced `sd2nas.cfg`); anything else → `config.Load` error exactly `SD2NAS_TOKEN may use only letters, digits and . _ ~ + / = -`, which `main` logs as `exiting` (key `err`) and SD2NAS **refuses to start** (exit 1).
    - Template and README tell the owner to generate it with `openssl rand -hex 24` and to write it single-quoted in `sd2nas.cfg` (`SD2NAS_TOKEN='…'`, appended with the `printf` form in Task 13).
    - A token shorter than 16 bytes adds the `/healthz` warning `SD2NAS_TOKEN is shorter than 16 characters: pick a longer, random one`.
    - Open without the token: `GET|HEAD /api/auth`, `POST /api/login`, `POST /api/logout`, `/healthz`, and every path outside `/api/` (the UI's files). A request is open only when both `path.Clean("/" + r.URL.Path)` and `path.Clean("/" + r.URL.EscapedPath())` are open paths (so percent-encoded slashes or dots can never make an `/api/` route look open).
    - Accepted credentials: header `Authorization: Bearer <token>` (scheme case-insensitive) or cookie `sd2nas_session` = lowercase hex of HMAC-SHA256(key = token, message = `sd2nas-session-v1`). Both compared with `crypto/subtle` (the token as SHA-256 digests, so its length doesn't leak).
    - Refusal: 401, header `WWW-Authenticate: Bearer realm="SD2NAS"`, `Cache-Control: no-store`, JSON `{"error":"This SD2NAS needs its password.","code":"token_required"}`; with `?format=text` the two lines `alert` / `SD2NAS refused the request: the token is missing or wrong. Set SD2NAS_TOKEN in sd2nas.cfg to the container's token.`
    - `POST /api/login {"token": "…"}` (through the guard: `X-SD2NAS: 1`, same origin): right → 204 with `Set-Cookie: sd2nas_session=<hmac>; Path=/; Max-Age=34560000; HttpOnly; SameSite=Strict` (no `Secure`: the LAN is plain HTTP). Wrong → 401 `{"error":"That password is not right.","code":"bad_token"}`. Missing/empty token field → 400 `Enter the password.` No token configured → 400 `SD2NAS has no token set.`
    - `POST /api/logout` → 204 with `Set-Cookie: sd2nas_session=; Path=/; Max-Age=0; HttpOnly; SameSite=Strict`.
    - `GET /api/auth` → 200 `{"required": bool, "authorized": bool}`.
    - **No token configured = behaviour exactly as today**, apart from the three new routes and the `HEAD /api/events` fix.
  - **`HEAD /api/events`** → 200 with the stream's headers and no body, returning at once.
  - **Settings:**
    - New key `staging_notice_at`, appended to `SettingKeys()` (8 keys), field `Settings.StagingNoticeAt time.Duration`, default `09:00`, reason `want HH:MM, 00:00 to 23:59`.
    - `GET /api/settings` → `{"settings": {...}, "defaults": {...}}`; `defaults` is `DefaultSettings().Strings()` with `backup_dir` = `idx.BackupDir(catalog.DefaultSettings())`.
    - Existing 400 sentences unchanged: `Name at least one setting.`, `Unknown setting <key>.`, `Setting <key> must be a string.`, `Setting <key>: <reason>.`, `Setting backup_dir: pick a folder outside Archive/ and Staging/.` (`CheckBackupDir` returns only `nil` or `ErrBackupInside`, so no other branch is added.)
    - `POST /api/archive/changed/accept`: a non-integer `upTo` (`1.5`, `"3"`, `true`) → 400 `upTo must be a positive whole number.` (not the JSON sentence).
  - **Notices:** the stale-staging check runs at the first check at or after `staging_notice_at` (container local time), keyed `staging:<YYYY-MM-DD>`, once a day. A settings change applies at once (the loop is nudged). The missing notice key is `missing:<newest updated_at of the counted rows, UTC, RFC 3339 nano>`; texts unchanged (`The archive scan marked 1 file missing from Archive/. See the Archive index page.` / `… marked <N> files missing …`).
  - **Thumbnails:** failures cached per sha (both sizes) only for `ErrUndecodable`, `ErrTooLarge`, `ErrNoImage`; at most **10000** entries (the map is cleared when full); `Remove(sha)` forgets it; a cached failure is returned wrapped as `fmt.Errorf("%w: %w", ErrRemembered, original)` with `ErrRemembered = errors.New("thumbs: an earlier render of this file failed")`. exiftool: one process started with `-stay_open True -@ -` on the first RAW render; each request ends `-echo4` / `{ready<N>}` / `-execute<N>`; `Close` sends `-stay_open` / `False`, closes stdin, waits at most **1 s**, then kills.
  - **Copy progress fields** (`ProgressSnapshot`): `current` (card path), `current_size`, `current_done`, `waiting_folder`, `waiting_holder`, all `omitempty`.
  - **Re-import missing:** schema version **5** (`restore_requests`). `POST /api/ingest/{id}/reimport-missing` answers 200 `{"requested": n, "started": <ingest view>|null, "message": "…"}`, messages:
    - An unknown import id → 404 `That import no longer exists.`; a non-positive one → 400 `The import id must be a positive whole number.`
    - `Nothing to re-import: none of this import's files is still missing from the archive.`
    - `Importing <n> file(s) again from <card>.` (`file`/`files` by `plural`)
    - `<n> file(s) will be imported again the next time you insert <card>.`
    - `<n> file(s) will be imported again the next time you insert <card> (starting an import now failed: <err>).`
  - **Shift times:** `POST /api/staging/shift {"fileShas": [...], "seconds": n}` (body limit `largeBody`). 400 sentences: `Say how far to shift: seconds is required.`, `Shift by at least one second.`, `Shift by at most 100 years.` (|n| > 3155760000), `Shift files from one folder at a time.`, `Shifting by that much would put <file name> outside 1970–2099.`; plus `checkSHAs`' existing sentences. Busy → the existing 409 `folder_busy`; moved meanwhile → the existing `ErrChanged` 409.
- **Lock-order rules (HANDOFF):** service callbacks only call `hub.Publish`; new code adds no `OnChange` callback. `notices.SettingsChanged` and the settings hooks never block. Ingest stays the only party that waits on folder locks; shift uses `TryLock*`. Don't wrap new goroutines that hold service mutexes in `recover`.
- **SQLite has one connection:** no `catalog.DB` call while a `*sql.Rows` is open; inside `d.tx` use only `tx`.
- **States** stay `staged | archived | culled | missing`. `missing` and `archived` rows are never forgotten. Never change file bytes.
- **UI:** every new or changed view works at desktop width and at 375×812 with no sideways scroll; tap targets ≥ 44 px; runes only, no `{@html}`, no `export let`; testable logic in `web/src/lib/*.js` with `node --test`; `localStorage` access wrapped in `try/catch`.
- **Tests:** pass on Windows and Linux; `path/filepath` for OS paths; `t.TempDir()` before a `Close` cleanup; waiting tests use a 10 s deadline, sleeps only in polling loops; `Now` seams instead of wall-clock waits.
- **Commands** (Git Bash, repo root):
  - `gofmt -l .` (empty), `go vet ./...`, `go test ./...`.
  - Linux: `MSYS_NO_PATHCONV=1 docker run --rm -e TZ=Pacific/Chatham -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q bash curl exiftool ffmpeg && go vet ./... && go test ./...'`
  - Race (Tasks 2, 3, 4, 5, 6, 7): `MSYS_NO_PATHCONV=1 docker run --rm -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q build-base bash curl exiftool ffmpeg && CGO_ENABLED=1 go test -race ./internal/... ./cmd/... ./deploy/... ./web/...'`
  - Web (Tasks 8–12): `cd web && npm test && npm run build` (npm only inside `web/`); afterwards `web/dist/.placeholder` exists and the tree is clean.
- **Never silently swallow an error:** return it wrapped with `%w`, answer the request with it, or log it with `slog` (key `"err"`). Exceptions: a write to an HTTP client that has gone; a failed `localStorage` access (falls back to the default); errors caused by shutdown cancelling a context (not logged as Error).
- Export only what each task's **Produces** lists. Each task compiles and passes on its own, in order.
- **Commits:** on branch `m7-polish`, one commit per task (more is fine) with the task's message. Every message ends with a blank line and exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`, no other trailer. UTF-8, LF endings (check `§ … — –` survive; `git show <commit>:<file> | grep -c $'\r'` is 0). Stage only the files you changed. Never commit `HANDOFF.md`, `tmp/`, `web/node_modules/` or `web/dist/*` other than `.placeholder`. Never push, tag or delete branches.

## Review Focus

1. **No token = no change.** Every existing `internal/api` and `deploy` test passes, changed only where a task names the change (the settings key count and `TestSettingsAPI`'s `len == 7` in Task 1, the `{"upTo":"x"}` sentence in Task 1, `wantTargets`/Repository in Tasks 3 and 13). With a token, nothing under `/api/` answers without it except the three auth routes; the card script's Bearer + `X-SD2NAS: 1` + no `Origin` still works; a wrong token gives the script two text lines. Cookie flags exact; compares constant-time; cleaned paths (always 401, never a redirect); a token outside `[A-Za-z0-9._~+/=-]` stops startup with the pinned sentence, so the bash-sourced `sd2nas.cfg` can always carry it. Pinned in Task 3.
2. **Shift never loses or rewrites a file.** Bytes and hashes unchanged; renames go through `moveFile` (FreeName, never overwrites, a candidate that is the file itself keeps it); wall-clock arithmetic (no DST jump); only `staged` rows; filesystem step then catalog step per file, and a failed catalog step moves the file back; a failure mid-way (or shutdown, between files) leaves every file either fully shifted or untouched. Pinned in Task 7.
3. **Re-import missing.** `missing` rows are never deleted; `missing → staged` happens only when the copy's full hash equals the asset, in one transaction, guarded by `state = 'missing'`; otherwise the normal new/duplicate paths run. A request counts only while its asset is missing and was marked missing before the request (no stale restores). "Safe to format" follows. Ingest is still the only lock waiter; the re-import goes through `Server.SubmitIngest`. Pinned in Task 6.
4. **Notices never repeat, never skip.** Changing `staging_notice_at` mid-day neither sends a second `staging:<date>` nor loses today's; the missing watermark survives a restart (newest `missing:` key) and covers crashed passes and two passes per check; no DB write or full scan per 30 s tick. Pinned in Task 2.
5. **exiftool lifecycle.** No orphan process after a timeout, a cancelled render, oversized output, a dead process, a recovered panic or `Close`; a reply is never read as another request's (marker per request, a foreign marker is doubt, kill unless the request completed cleanly); `docker stop` stays under 2 s. The failure cache holds only file faults (never timeouts, cancels, a missing tool, a missing file). Pinned in Task 4.
6. **Copy progress** publishes nothing itself; the feed's 500 ms poll stays the only publisher of `ingest.current`; `bytes_done` ends each file at `start + size` whatever the outcome. Pinned in Task 5.
7. **UI** at 375×812 and desktop for every new view (controller's browser check).
8. **Docs match reality:** README, template, PLAN §16; the `v0.1.0` item stays unticked with exact owner commands.

## Decisions

1. **The browser authenticates with a cookie set by `POST /api/login`; the card script keeps Bearer.** **[override?]** `EventSource` and `<img src=/api/thumbs/…>` can't send headers, so a header-only scheme would need the token in every URL. Cost of a `?token=` query parameter instead: the token lands in browser history, server logs and every thumbnail URL, and thumbnails couldn't be cached by URL.
2. **What stays open with a token:** `/healthz`, the UI's static files, `GET /api/auth`, `POST /api/login`, `POST /api/logout`. **[override?]** Docker's `HEALTHCHECK` (`wget …/healthz`) and Unraid's health dot have no token; `/healthz` reveals version, TZ, uid/gid and mount paths, not photos. Cost of protecting it: the Dockerfile health check needs the token (`wget --header` from env), and a forgotten token shows the container unhealthy.
3. **The cookie is a fixed HMAC of the token, valid 400 days (browsers' cap); no server-side sessions.** **[override?]** Changing `SD2NAS_TOKEN` signs every browser out; "Sign out on this device" clears one. Cost of real sessions: a table, expiry and a revoke list.
4. **No login rate limit; a `/healthz` warning for tokens under 16 characters; the charset `[A-Za-z0-9._~+/=-]`, refused at startup otherwise.** **[override?]** LAN only, and a random 48-hex token (README: `openssl rand -hex 24`) can't be guessed. The charset keeps the token intact in the bash-sourced `sd2nas.cfg` (the frozen card script `source`s it; `$`, quotes, `;` would be mangled or executed), and it is written single-quoted. Cost of a limiter: shared state, a 429 path, and a lock-out risk for the owner. Cost of a wider charset: tokens the card script silently sends wrong, or a cfg line bash executes.
5a. **Owner ruling: user-facing text says "password".** The env var, cfg key, header and code identifiers stay `SD2NAS_TOKEN` / token (the card script already reads it). The Login page, its errors, the API's JSON error sentences and the template field name say "password" and point at `SD2NAS_TOKEN`; the script's `format=text` refusal keeps naming `SD2NAS_TOKEN in sd2nas.cfg`.
5. **`SD2NAS_TOKEN` is env-only** (no `--token` flag), so it never appears in a process list.
6. **`staging_notice_at` (HH:MM, default 09:00) is a catalog setting, applied at once.** **[override?]** "Once a day" is unchanged: the day's key decides, so moving the time later after today's notice sends nothing more today, and moving it earlier before it ran sends it now. Cost of an env var instead: a container restart to change it, and no Settings field.
7. **The missing-files notice comes from a catalog watermark, not the last pass's count** (folds in T6-m4 and the crashed-pass follow-up). Rows with `state = 'missing'` and `updated_at` after the newest `missing:` notice (or after now − 24 h) are counted once no pass is running; the query runs only when `Status.Missing` or `Last.FinishedAt` changed, or at the first check. Cost of keeping per-pass counts: both follow-ups stay open.
8. **The Settings page sends only changed keys** (all-or-nothing PUT), and `GET /api/settings` adds `defaults`, so "Use default" works and an untouched `backup_dir` stays `""` (following the photos root) instead of being saved as a path. `GET` returns the resolved `backup_dir`, never `""`, so the page shows "(default)" when `settings.backup_dir === defaults.backup_dir`; "Use default" sends `""`.
9. **The Settings page shows all 8 catalog keys**, including `verify_known` and `free_space_reserve_gb`, which M7's list didn't name. **[override?]** Cost of hiding them: they stay reachable only by `curl`.
10. **The default promote pattern stays per device** (`localStorage`, shown under "This device"). **[override?]** Cost of a catalog setting: a ninth key, a pattern validator in Go (`staging.CheckPattern` exists) and migrating each device's remembered value.
11. **Thumbnail failures are remembered in memory, per file, until restart.** **[override?]** A restart (an update, usually) retries each once, which also picks up a newer exiftool/ffmpeg. Cost of on-disk markers: files under `/config/thumbs`, invalidation on tool upgrades, and `Remove` changes.
12. **One exiftool process for the container, requests serialized, started lazily, restarted after any failure; no idle stop.** **[override?]** Perl startup (~0.3–0.5 s per RAW today) is paid once; a request takes ~20–50 ms, so serializing is faster than four processes. Paths containing CR or LF (an argfile can't carry them) fall back to today's one-shot run. Cost of a pool: N × ~30 MB RSS and N protocols to keep in sync. Cost of an idle stop: a timer and a restart race.
13. **Byte progress needs no new event:** the 500 ms ingest feed already publishes `ingest.current` only when it changed, so copying publishes at most 2 frames/s, the same ceiling as today. F11 folds in as `waiting_folder` / `waiting_holder`.
14. **Re-import missing records a persistent request (schema v5 `restore_requests`).** **[override?]** The card script unmounts the card when SD2NAS answers 200, which a `missing` import does, so "import now while it's mounted" alone would almost never work. The request applies to the next import of any card holding that content; if the card is still mounted (poller setups, or the owner mounted it by hand) the import starts at once. A request counts only while its asset is `missing` with `updated_at <= requested_at`: content that reappeared and went missing again needs a fresh request. Cost of no persistence: the button only works with a hand-mounted card.
15. **A restored file becomes an ordinary staged asset**: same sha, `first_ingest` kept, counted as `new` in that import (so "N new … Card safe to format." reads right), forgettable like any staged file. **[override?]** Cost of a separate `restored` outcome: a CHECK-constraint rebuild of `ingest_files`, new counts, and a change to `Describe`'s text that the card script passes through.
16. **The re-import dialog warns about a dropped disk** ("if an array disk dropped out, fix that first: its files come back by themselves at the next archive scan") instead of an indexer threshold. The dropped-disk threshold stays deferred, since it is indexer core.
17. **Shift times applies to staged files only.** **[override?]** Archived files are the indexer's, live inside Capture One sessions whose `.cosessiondb` references file names, and renaming them would break sessions. Cost of allowing it: Capture One sessions to repair by hand and an indexer interaction (moves vs. changes).
18. **Shift works on one folder per request**, from the Folder page (selection, or "Shift all times…", which sends every staged file whatever the kind filter shows). A folder of more than `MaxFiles` (20000) files can't be shifted whole; the button is disabled with a sentence. **[override?]** Cost of multi-folder: locking and re-filing across many sources, and a harder preview. Cost of a `folderId` whole mode for huge folders: a second request shape and its validation.
19. **Where shifted files go.** **[override?]**
    - If the request covers **every staged file of the folder**, the files stay: the folder's dates become their new folder dates, and a name starting with a date takes the earliest new date (title kept). If that name is taken, the folder keeps its name and the result says which name it wanted.
    - Otherwise a file **stays** when its new folder date is one of the folder's dates, and is **re-filed** like an import when not: into the folder whose dates contain it, else the folder named exactly that date (both `StagingFolderForDate`'s choices, read-only, so shift never tries to create a name that exists), else a new `YYYY-MM-DD` folder with the source's `created_at`.
    - The shift stops between files on shutdown (`shifted N of M files: SD2NAS is shutting down`), and `Service.Close` waits for it, like the other long jobs.
    - Cost of always re-filing: shifting a titled folder by a day would empty it into an untitled one. Cost of never re-filing: a camera's files stay in a wrong-date folder until moved by hand.
20. **Shift renames only the leading `YYYY-MM-DD HH.mm.ss` stamp, and only when it equals the file's old capture time;** everything after it (video details, `-1` burst suffixes) is kept, so burst order survives. Names without the stamp (files adopted from SMB) keep their name. Files are processed latest-first for a positive shift and earliest-first for a negative one, so a batch never collides with itself. Files with no capture time are skipped and counted.
21. **Shifting adds seconds to the wall clock** (capture times are naive local wall clock), never through `time.Local`'s DST rules; limits ±100 years, results within 1970–2099.
22. **Shift deletes the shifted files' clock warnings.** **[override?]** Shifting is the answer to them, and a GPS mismatch can't be re-checked (the GPS time isn't stored). The import row's `clock_warnings` count stays as history. Cost of keeping them: stale warnings on the folder until promote.
23. **Views:** Staging gets Cards | List | Timeline, Folder gets Grid | List, remembered per device. The timeline is chronological (oldest first, like the cards), grouped by month of the folder's date, undated folders last. **[override?]** Cost of a file-level timeline too: another layout to check on phone and desktop.
24. **The template moves to `:latest` in this PR; the owner tags `v0.1.0` right after merging.** **[override?]** The template URL points at `main`, so a fresh install between merge and tag would find no `:latest` (minutes, and the owner is the only user). README documents `:edge` for following `main`. Cost of keeping `:edge`: every merge to main updates the NAS on the next "Check for Updates".
25. **Base images are pinned as `name:<exact tag>@sha256:<digest>`**, checked by a Go test; no Dependabot config. **[override?]** Cost of Dependabot (a `.github/dependabot.yml`, not a dependency): a weekly PR per image to review.
26. **Follow-ups folded in:** M5 settings nits (the `upTo` sentence, the key-count tests; the "generic backup-dir error" nit is dropped because `CheckBackupDir` has no other error), the 24 px `<summary>`, T6-m4 and the crashed-pass notice, F9 (`diskFolders` uses `skipName`), F10 (staging first-load reload), "1 files", F11, M-6 (forget counts `missing`), `HEAD /api/events`. **Deferred** (unchanged code or not cheap): F6, F7, F8, F12, F14, the SSE write deadline, duplicate `ingest.finished`, a 500 carrying paths (only token holders see them now), client routes with a dot, history truncation at 1000, duplicate clock-warning strings, the dropped-disk threshold, the poller's repeated Warn, the open-rows panic hang, the shutdown log line, the M5 indexer-progress/logging details and missing tests, the Archive page load nits.
27. **The JS shift preview (`lib/shift.js`) and the JS settings checks (`lib/settings.js` `checkField`) are preview-only mirrors of the Go rules; the server stays the judge.** The browser never blocks a request on them (a `checkField` complaint is shown, but Save still sends and the server's 400 sentence wins); there is no dry-run endpoint. To keep the mirrors honest, one case table, `web/src/lib/testdata/rules-cases.json`, is read by both the Go tests (`internal/staging/shift_test.go` and `internal/catalog/settings_test.go`, via `../../web/src/lib/testdata/rules-cases.json`) and the node tests (`shift.test.js`, `settings.test.js`): same inputs → same outputs. It lives under `web/` because the Dockerfile's web stage copies only `web/` before `npm test`. Shape:
    ```json
    {
      "shiftWall":   [{"capture": "2025-03-09T01:30:00", "seconds": 3600, "want": "2025-03-09T02:30:00"}],
      "shiftedName": [{"name": "2025-05-30 14.02.11-1.JPG", "capture": "2025-05-30T14:02:11", "seconds": 3600, "want": "2025-05-30 15.02.11-1.JPG"}],
      "folderDate":  [{"capture": "2025-05-31T03:59:59", "dayStartsAt": "04:00", "want": "2025-05-30"}],
      "wholeName":   [{"name": "2025-05-30 - Trip", "dates": ["2025-05-31", "2025-06-01"], "want": "2025-05-31 - Trip"}],
      "settings":    [{"key": "staging_notice_at", "value": "24:00", "ok": false}]
    }
    ```
    Task 1 creates the file with the `settings` section (one valid and one invalid value per key; the Go side checks `SetSettings` accepts or refuses each) and empty arrays for the rest; Task 7 fills in at least the five `shiftedName` cases of Task 7 Step 1, the two `shiftWall` cases, three `folderDate` cases (before, at and after the cutoff) and two `wholeName` cases (dated, `Untitled`). Tasks 9 and 10 read it. Cost of a dry-run endpoint instead: a second code path through `Shift` and a request per keystroke.

## File Structure

```
internal/catalog/
  settings.go          KeyStagingNoticeAt, Settings.StagingNoticeAt                    (Task 1)
  notifications.go     LatestNotificationTime                                         (Task 2)
  archive.go           MissingSince                                                   (Task 2)
  schema.go, catalog.go   schemaV5 restore_requests, SchemaVersion = 5                (Task 6)
  restore.go           RequestRestore, RestoreRequested, RestoreMissing, RestorableMissing (Task 6)
  stagingops.go        ShiftAsset, StagingFolderWithDate                              (Task 7)
internal/api/
  server.go            settings hooks fields (Task 1); auth field, chain, routes (Task 3)
  settings.go          defaults, runs the settings hooks                              (Task 1)
  archive.go           upTo sentence                                                  (Task 1)
  notifications.go     registers notes.SettingsChanged                                (Task 2)
  auth.go              token middleware, /api/auth, /api/login, /api/logout           (Task 3)
  events.go            HEAD /api/events                                               (Task 3)
  thumbs.go            no Warn for ErrRemembered                                      (Task 4)
  forget.go            POST /api/ingest/{id}/reimport-missing                          (Task 6)
  staging.go           POST /api/staging/shift                                        (Task 7)
internal/notices/notices.go   notice time setting, nudge, missing watermark           (Task 2)
internal/config/config.go     Token (SD2NAS_TOKEN)                                    (Task 3)
cmd/sd2nas/main.go            SetToken, short-token warning                           (Task 3)
internal/thumbs/
  thumbs.go            failure cache, ErrRemembered                                   (Task 4)
  exifserver.go        the -stay_open process                                         (Task 4)
  tools.go             rawPreview through the server                                  (Task 4)
internal/media/copy.go        CopyHashedProgress                                      (Task 5)
internal/ingest/progress.go, run.go   byte progress, waiting; restore path            (Tasks 5, 6)
internal/staging/
  forget.go            Missing / Restorable / RestoreRequested / KeptMissing          (Task 6)
  shift.go             ShiftRequest, ShiftResult, Service.Shift                        (Task 7)
  staging.go           diskFolders skips skipName                                     (Task 8)
web/src/
  lib/testdata/rules-cases.json    Go/JS shared rule cases (Decision 27)       (Tasks 1, 7)
  lib/views.js, lib/prefs.js       view sorting/grouping, per-device prefs            (Task 8)
  pages/Staging.svelte, pages/Folder.svelte   views                                   (Task 8)
  lib/shift.js, Folder.svelte      Shift dialog                                       (Task 9)
  lib/settings.js, pages/Settings.svelte, components/Nav.svelte, App.svelte, app.css   (Task 10)
  lib/auth.svelte.js, pages/Login.svelte, lib/api.js, lib/live.svelte.js, App.svelte  (Task 11)
  components/LiveIngest.svelte, pages/IngestDetail.svelte, lib/messages.js            (Task 12)
deploy/
  Dockerfile           pinned base images                                             (Task 13)
  dockerfile_test.go   every FROM pinned                                              (Task 13)
  unraid/sd2nas.xml, template_test.go   SD2NAS_TOKEN (Task 3); :latest (Task 13)
README.md                                                                             (Task 13)
docs/PLAN.md                                                                          (Task 14)
```

| Task | Title | Implementer / reviewer |
|---|---|---|
| 1 | Settings: notice time key, defaults, API nits, hooks | sonnet / sonnet |
| 2 | Notices: configurable time, nudge, missing watermark | opus / opus (data-safety: notices) |
| 3 | Token auth (backend + template) | opus / opus (security) |
| 4 | Thumbnail failure cache + exiftool `-stay_open` | opus / opus (concurrency, process lifecycle) |
| 5 | Byte-level copy progress + waiting state | sonnet / opus (ingest run) |
| 6 | Re-import missing (schema v5) | opus / opus (data-safety core) |
| 7 | Shift times backend | opus / opus (data-safety core) |
| 8 | Staging views + F9/F10/"1 files" | sonnet / sonnet |
| 9 | Shift times UI | sonnet / sonnet |
| 10 | Settings page | sonnet / sonnet |
| 11 | Login page and token handling in the UI | sonnet / sonnet |
| 12 | Ingest UI: live copy detail, re-import missing | sonnet / sonnet |
| 13 | Release prep: pinned images, `:latest`, README | sonnet / sonnet |
| 14 | PLAN.md, final verification | sonnet / opus (final review) |

---

### Task 1: Settings: notice time key, defaults, API nits, hooks

**Produces:** `catalog.KeyStagingNoticeAt`, `Settings.StagingNoticeAt`; `GET /api/settings` `defaults`; unexported `Server.onSettingsChanged(fn func())` and `Server.settingsHooks() []func()`.
**Consumes:** `catalog.SetSettings`, `indexer.Service.BackupDir`.

- [ ] **Step 1: Catalog tests (RED).** In `internal/catalog/settings_test.go`:
  - `TestSettingsStrings`: replace the literal `7` with `len(want)`; `want` gains `KeyStagingNoticeAt: "09:00"`; `SettingKeys()` ends with `staging_notice_at` (8 keys).
  - `TestSettingsDefaults`: its `want` struct literal gains `StagingNoticeAt: 9 * time.Hour` (it compares `got != want`).
  - `TestSettingsCases` (Decision 27): create `web/src/lib/testdata/rules-cases.json` with the `settings` section — one valid and one invalid value per key (8 × 2), plus `"shiftWall": [], "shiftedName": [], "folderDate": [], "wholeName": []` — and check each entry with `SetSetting` on a fresh DB (`ok` true → nil error, false → `*SettingError`). Use only platform-independent values (for `backup_dir`: `""` valid, `relative/dir` invalid; an absolute path differs between Windows and Linux). The file is read via `../../web/src/lib/testdata/rules-cases.json`.
  - `TestStagingNoticeAt`: `SetSetting(KeyStagingNoticeAt, "18:30")` → `Settings().StagingNoticeAt == 18h30m`; `"24:00"`, `"9:00"`, `""` → `*SettingError` with Reason `want HH:MM, 00:00 to 23:59`.
- [ ] **Step 2: Implement.** In `settings.go`: the constant, append to `settingKeys`, the field, `DefaultSettings()` `StagingNoticeAt: 9 * time.Hour`, an `apply` case sharing `parseClock` and the reason, a `Strings()` entry.
- [ ] **Step 3: API tests (RED)** in `internal/api/settings_test.go` / `archive_test.go`:
  - Existing tests to update: `TestSettingsAPI`'s two `len(...) != 7` checks become `len(catalog.SettingKeys())`; `TestArchiveChangedAndAccept`'s table entry `{"upTo":"x"}` now expects `upTo must be a positive whole number.` (it was the JSON sentence).
  - `TestSettingsGetDefaults`: `GET` → `defaults` has all 8 keys, `defaults.backup_dir == filepath.Join(photos, "Management", "SD2NAS")` (what `BackupDir` gives), `defaults.staging_notice_at == "09:00"`.
  - `TestSettingsPutNoticeAt`: `{"staging_notice_at":"18:30"}` → 200 and the value; `{"staging_notice_at":"25:00"}` → 400 `Setting staging_notice_at: want HH:MM, 00:00 to 23:59.`
  - `TestSettingsHooks`: a hook registered with `srv.onSettingsChanged` runs once after a 200 PUT, not after a 400; a hook registered **after** `MountSettings` still runs (hooks are read per request).
  - `TestAcceptUpToNotWhole`: `{"upTo":1.5}`, `{"upTo":"3"}`, `{"upTo":true}`, `{"upTo":0}`, `{"upTo":null}`, `{}` → 400 `upTo must be a positive whole number.`; `{"upTo":2}` still works; `{"upTo":` (bad JSON) → the JSON sentence.
- [ ] **Step 4: Implement.**
  - `Server` gains `settingsMu sync.Mutex` and `settingsFns []func()`; `onSettingsChanged` appends under the mutex; `settingsHooks` returns a copy.
  - These fields live on `Server` in `internal/api/server.go`. `settingsHandlers` **gains** a `srv *Server` field (it has none today); after `h.idx.SettingsChanged()` it runs every hook from `h.srv.settingsHooks()`. `write` adds `"defaults"`.
  - The backup-dir check stays as it is (`errors.Is(…, indexer.ErrBackupInside)`): `CheckBackupDir` returns nothing else, and the indexer is frozen.
  - `accept`: decode `UpTo json.RawMessage`; `n, err := strconv.ParseInt(string(raw), 10, 64)`; `len(raw) == 0 || err != nil || n < 1` → the sentence.
- [ ] **Step 5:** `gofmt -l .`, `go vet ./...`, `go test ./internal/catalog/ ./internal/api/`; commit `Add the staging notice time setting and settings defaults`.

### Task 2: Notices: configurable time, nudge, missing watermark

**Produces:** `notices.Service.SettingsChanged()`; `catalog.DB.LatestNotificationTime(ctx, prefix string) (time.Time, error)`; `catalog.DB.MissingSince(ctx, after time.Time) (n int, latest time.Time, err error)`. Removes `notices.StagingCheckAt`.
**Consumes:** Task 1 (`StagingNoticeAt`, `onSettingsChanged`); `indexer.Status` (`Running`, `Last.FinishedAt`, `Missing`).

- [ ] **Step 1: Catalog tests (RED).**
  - `TestMissingSince`: three archived assets; mark two missing at t1 < t2 (`MarkMissing`); `MissingSince(t1 − 1ms)` → `(2, t2)`; `MissingSince(t1)` → `(1, t2)`; `MissingSince(t2)` → `(0, zero)`. A missing row that became archived again isn't counted.
  - `TestLatestNotificationTime`: none → zero, nil; keys `missing:2026-01-01T00:00:00.5Z` then `missing:2026-01-02T00:00:00Z` and `backup:…` → the second; an unparsable suffix → error naming the key.
- [ ] **Step 2: Implement.**
  - `MissingSince`: `SELECT COUNT(*), COALESCE(MAX(updated_at), '') FROM assets WHERE state = 'missing' AND updated_at > ?` with `formatTime(after)` (fixed width, so text order is time order); parse `latest` with `parseTime` when non-empty.
  - `LatestNotificationTime`: `SELECT key FROM notifications WHERE key LIKE ? ESCAPE '\' ORDER BY id DESC LIMIT 1` (prefix escaped, `%` appended); `time.Parse(time.RFC3339Nano, strings.TrimPrefix(key, prefix))`.
- [ ] **Step 3: Notices tests (RED)** in `internal/notices/notices_test.go`. Rewrite the existing missing tests to mark rows missing in the catalog (the notice no longer reads `Last.Missing`). New:
  - `TestStagingNoticeAtSetting`: setting `18:00`; stale folder; checks at 17:59 → nothing; 18:00 → `staging:<date>`.
  - `TestStagingNoticeLaterAfterSent`: sent at 09:00; set `20:00`, `SettingsChanged()`; check at 20:01 → no second notice (`adds` unchanged).
  - `TestStagingNoticeEarlierRunsNow`: `09:00`, now 08:00, nothing sent; set `07:00`, `SettingsChanged()`; the loop checks within the 10 s deadline without a tick (use `Every: time.Hour`) → notice queued.
  - `TestNoticeAtReadOnce`: before today's check is done (now ≥ the notice time, `StagingReady` false so the day never completes), 100 checks read settings once (`settingsReads`); after `SettingsChanged`, once more.
  - `TestNoticeAtReadFailure`: settings read fails (a stored bad value) → logged at Error, the default `09:00` used, retried at the next check.
  - `TestMissingTwoPassesOneCheck`: two passes mark 1 then 2 rows before one check → one notice `The archive scan marked 3 files missing from Archive/. See the Archive index page.` keyed `missing:<newest updated_at>`.
  - `TestMissingAfterCrash`: rows marked missing 1 h ago, `Status{Last: nil, Missing: 2}` (a crashed pass) → one notice for 2 at the first check.
  - `TestMissingNotRepeatedAfterRestart`: a stored `missing:<t>` key, rows marked at or before `t` → a new service queues nothing.
  - `TestMissingOlderThanADay`: rows marked 25 h ago, no keys → nothing.
  - `TestMissingWaitsForPass`: `Status.Running != nil` → no query (`missingQueries == 0`); after it ends → notice.
  - `TestMissingQueriesOnlyOnChange`: unchanged `Missing` and `Last.FinishedAt` → `missingQueries` stays 1 over 50 checks.
- [ ] **Step 4: Implement** (`notices.go`):
  ```go
  // added to Service; touched only by the loop goroutine unless noted
  nudge  chan struct{} // cap 1; SettingsChanged sends without blocking
  reread atomic.Bool   // set by SettingsChanged (any goroutine)
  at     time.Duration // staging_notice_at; -1 until read

  missingSeen  int       // Status.Missing at the last missing check; -1 before the first
  lastFinished time.Time // Status.Last.FinishedAt at that check
  watermark    time.Time // rows marked missing at or before it are notified or too old

  // test-only counters, like adds
  settingsReads  int // DB.Settings calls made by noticeAt
  missingQueries int // MissingSince calls made by checkMissing

  func (s *Service) loop() {
      defer s.wg.Done()
      t := time.NewTicker(s.every)
      defer t.Stop()
      for {
          s.check(s.ctx)
          select {
          case <-s.ctx.Done():
              return
          case <-t.C:
          case <-s.nudge:
          }
      }
  }

  // SettingsChanged makes the loop re-read staging_notice_at and check now.
  // It never blocks (PUT /api/settings calls it).
  func (s *Service) SettingsChanged() {
      s.reread.Store(true)
      select {
      case s.nudge <- struct{}{}:
      default:
      }
  }

  func (s *Service) noticeAt(ctx context.Context) time.Duration {
      if s.at >= 0 && !s.reread.Swap(false) {
          return s.at
      }
      s.settingsReads++
      set, err := s.cfg.DB.Settings(ctx)
      if err != nil {
          s.reread.Store(true) // try again at the next check
          if s.at < 0 {
              s.at = catalog.DefaultSettings().StagingNoticeAt
          }
          if ctx.Err() == nil { // not shutting down
              s.log.Error("could not read staging_notice_at; using the last value", "at", clock(s.at), "err", err)
          }
          return s.at
      }
      s.at = set.StagingNoticeAt
      return s.at
  }
  ```
  - `clock(d)` is a small helper formatting `HH:MM` (as `Settings.Strings` does).
  - `checkStaging`: after the `day == s.stagingDay` early return, `at := s.noticeAt(ctx)`; `due := time.Date(y, m, d, int(at/time.Hour), int(at%time.Hour/time.Minute), 0, 0, now.Location())`. Everything else (retry after 1 h, `StagingReady`, the key) unchanged.
  - `checkArchive` keeps the backup branch (`addOnce`) and calls `checkMissing(ctx, st, now)`:
    ```go
    if st.Running != nil {
        return // a pass may still be marking rows
    }
    var finished time.Time
    if st.Last != nil {
        finished = st.Last.FinishedAt
    }
    if !s.watermark.IsZero() && st.Missing == s.missingSeen && finished.Equal(s.lastFinished) {
        return
    }
    if s.watermark.IsZero() {
        w, err := s.cfg.DB.LatestNotificationTime(ctx, "missing:")
        if err != nil {
            s.log.Error("could not read the last missing-files notice", "err", err)
            return
        }
        s.watermark = w
        if floor := now.Add(-MaxAge); w.Before(floor) {
            s.watermark = floor
        }
    }
    s.missingQueries++
    n, latest, err := s.cfg.DB.MissingSince(ctx, s.watermark)
    if err != nil {
        s.log.Error("could not count files marked missing", "err", err)
        return
    }
    if n > 0 {
        if err := s.add(ctx, "missing:"+latest.UTC().Format(time.RFC3339Nano), catalog.NoticeAlert, missingText(n)); err != nil {
            return // add logged it; the next check retries
        }
        s.watermark = latest
    }
    s.missingSeen, s.lastFinished = st.Missing, finished
    ```
  - `New` sets `at: -1`, `missingSeen: -1`, `nudge: make(chan struct{}, 1)`.
- [ ] **Step 5: Wire.** `MountNotifications(notes)` calls `s.onSettingsChanged(notes.SettingsChanged)`. `TestSettingsPutNudgesNotices` (api): `MountNotifications` takes the concrete `*notices.Service`, so use a real one — `Every: time.Hour`, a `Staging` func that signals a channel on each call, `Now` at or after the notice time; `Start`, wait for the first signal, `PUT /api/settings {"staging_notice_at":"00:00"}`, and expect a second signal within 10 s (no tick could have caused it).
- [ ] **Step 6:** gofmt/vet/test the three packages; race command; commit `Make the stale-staging notice time a setting and count missing files from the catalog`.

### Task 3: Token auth (backend + template)

**Produces:** `config.Config.Token`; `api.Server.SetToken(token string)`; routes `GET /api/auth`, `POST /api/login`, `POST /api/logout`.
**Consumes:** `guard`, `writeFail`, `wantsText`, `decodeBody`, `smallBody`.

- [ ] **Step 1: Config tests (RED)** (`internal/config/config_test.go`): `TestTokenEnv`: unset → `""`; `abc123`, `0f3a…` (48 hex), `A-Z_a.z~+/=` → kept as is; `"abc def"`, `"a$b"`, `"a'b"`, `"a;b"`, `"abc\x7f"`, `"é"` → error exactly `SD2NAS_TOKEN may use only letters, digits and . _ ~ + / = -`. Implement in `Load` (env only) with `regexp.MustCompile("^[A-Za-z0-9._~+/=-]+$")`. `TestExitCode` (cmd) gains a case: a bad token → exit 1 and the `exiting` log line carrying that sentence.
- [ ] **Step 2: API tests (RED)** (`internal/api/auth_test.go`, using the existing test server helpers):
  - `TestNoTokenUnchanged`: no `SetToken` → `GET /api/staging` 200, a guarded POST 200/4xx as before, `GET /api/auth` → `{"required":false,"authorized":true}`, `POST /api/login` → 400 `SD2NAS has no token set.`
  - `TestTokenRequired`: `SetToken("0123456789abcdef")`. Without credentials: `GET /api/settings`, `GET /api/thumbs/<sha>`, `GET /api/events`, `POST /api/staging/rescan` (with `X-SD2NAS: 1`) → 401 with the binding JSON, `WWW-Authenticate` and `no-store`. Open: `/healthz` 200, `/` 200, `/staging/3` 200 (SPA), `GET /api/auth` → `{"required":true,"authorized":false}`.
  - `TestTokenBearer`: `Authorization: Bearer <tok>` → 200; `bearer <tok>` → 200; `Bearer wrong`, `Basic <tok>`, `Bearer` → 401.
  - `TestTokenTextRefusal`: `POST /api/ingest?wait=1&format=text` with `X-SD2NAS: 1` and a wrong Bearer → 401, body exactly `alert\nSD2NAS refused the request: the token is missing or wrong. Set SD2NAS_TOKEN in sd2nas.cfg to the container's token.\n`.
  - `TestGuardBeforeToken`: a POST with the right Bearer but no `X-SD2NAS` → 403 (guard first).
  - `TestLogin`: right token → 204 and a cookie with exactly `Path=/`, `Max-Age=34560000`, `HttpOnly`, `SameSite=Strict`, no `Secure`, value = hex HMAC; then `GET /api/settings` with it → 200. Wrong → 401 `That password is not right.` code `bad_token`; `{}` and `{"token":""}` → 400 `Enter the password.`; without `X-SD2NAS` → 403; unknown field → 400.
  - `TestLogout`: 204, `Max-Age=0` (the `Set-Cookie` text contains `Max-Age=0`).
  - `TestCookieOfOtherToken`: a cookie computed from another token → 401.
  - `TestTokenCleanedPath`: `GET /x/../api/settings` and `GET /api/./settings` → exactly 401 with `code: token_required` (`requireToken` runs before the mux and cleans the path).
  - `TestHeadEvents`: `HEAD /api/events` → 200, `Content-Type: text/event-stream`, returns within 1 s with an empty body (no token set).
- [ ] **Step 3: Implement** `internal/api/auth.go`:
  ```go
  const (
      sessionCookie  = "sd2nas_session"
      sessionMaxAge  = 400 * 24 * 60 * 60 // seconds; browsers cap cookie lifetimes at 400 days
      sessionMessage = "sd2nas-session-v1"
      tokenTextRefusal = "SD2NAS refused the request: the token is missing or wrong. Set SD2NAS_TOKEN in sd2nas.cfg to the container's token."
  )

  // auth is the optional shared token (SD2NAS_TOKEN). The zero value means none.
  type auth struct {
      digest  []byte // sha256 of the token; nil = no token
      session string // the cookie value: hex HMAC-SHA256(token, sessionMessage)
  }

  func newAuth(token string) auth {
      if token == "" {
          return auth{}
      }
      sum := sha256.Sum256([]byte(token))
      mac := hmac.New(sha256.New, []byte(token))
      mac.Write([]byte(sessionMessage))
      return auth{digest: sum[:], session: hex.EncodeToString(mac.Sum(nil))}
  }

  func (a auth) required() bool { return a.digest != nil }

  func (a auth) tokenOK(given string) bool {
      sum := sha256.Sum256([]byte(given))
      return subtle.ConstantTimeCompare(sum[:], a.digest) == 1
  }

  func (a auth) authorized(r *http.Request) bool {
      if !a.required() {
          return true
      }
      if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); ok && strings.EqualFold(scheme, "Bearer") && a.tokenOK(strings.TrimSpace(tok)) {
          return true
      }
      c, err := r.Cookie(sessionCookie)
      return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(a.session)) == 1
  }

  // openPath: what answers without the token.
  func openPath(r *http.Request) bool {
      p := path.Clean("/" + r.URL.Path)
      switch {
      case p == "/api/auth" && (r.Method == http.MethodGet || r.Method == http.MethodHead),
          (p == "/api/login" || p == "/api/logout") && r.Method == http.MethodPost:
          return true
      case p == "/api" || strings.HasPrefix(p, "/api/"):
          return false
      }
      return true // /healthz and the web UI's files
  }

  func (s *Server) requireToken(next http.Handler) http.Handler {
      return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
          if openPath(r) || s.auth.authorized(r) {
              next.ServeHTTP(w, r)
              return
          }
          w.Header().Set("WWW-Authenticate", `Bearer realm="SD2NAS"`)
          if wantsText(r) {
              writeFail(w, r, http.StatusUnauthorized, tokenTextRefusal)
              return
          }
          writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "This SD2NAS needs its password.", "code": "token_required"})
      })
  }
  ```
  - `Server` gains `auth auth`; `New` builds `s.handler = guard(s.requireToken(s.mux))` and registers the three routes. `SetToken(token)` sets `s.auth = newAuth(token)`; "call it before serving", like `SetLogger`.
  - Handlers: `handleAuth` → `{"required": s.auth.required(), "authorized": s.auth.authorized(r)}`; `handleLogin` / `handleLogout` per the binding values (cookie via `http.SetCookie`, `MaxAge: -1` for logout, which Go writes as `Max-Age=0`; `Cache-Control: no-store`).
  - `handleEvents`: `if r.Method == http.MethodHead { set the three headers; w.WriteHeader(200); return }` before subscribing.
- [ ] **Step 4: Wire** (`cmd/sd2nas/main.go`): `srv.SetToken(cfg.Token)` right after `SetLogger`; `if w := tokenWarning(cfg.Token); w != "" { srv.AddWarning(w) }` with `tokenWarning` in package main returning the binding sentence for `0 < len < 16`. `TestTokenWarning` in `main_test.go`.
- [ ] **Step 5: Template.** Add after the Card poller entry:
  ```xml
  <Config Name="Password (SD2NAS_TOKEN)" Target="SD2NAS_TOKEN" Default="" Mode="" Description="Optional password for the web UI and the card script. Generate one with: openssl rand -hex 24 (letters, digits and . _ ~ + / = - only, or SD2NAS won't start). When set, the web UI asks for it once per browser, and the card script must send it: add it single-quoted to sd2nas.cfg as the README shows. Leave empty for no token: then anyone on your network can use SD2NAS." Type="Variable" Display="advanced" Required="false" Mask="true"></Config>
  ```
  `template_test.go`: `wantTargets` gains `"SD2NAS_TOKEN"` last; assert its `Mask == "true"`, `Default == ""`, `Display == "advanced"`, and that the description mentions `sd2nas.cfg` and `openssl rand -hex 24`.
- [ ] **Step 6:** gofmt/vet/test (`./internal/config/ ./internal/api/ ./cmd/... ./deploy/...`), Linux and race commands; commit `Add optional token auth`.

### Task 4: Thumbnail failure cache + exiftool `-stay_open`

**Produces:** `thumbs.ErrRemembered`. Unexported `exifServer` (`run`, `close`).
**Consumes:** `Cache.generate`, `runTool`, `rawPreview`, the test `fakeTool` (`SD2NAS_FAKE_TOOL`).

- [ ] **Step 1: Failure cache tests (RED)** (`thumbs_test.go`, replacing `Cache.render` with a counting stub):
  - `TestFailureRemembered`: render returns `ErrUndecodable` → the second `Get` (same size or the other size) returns an error that `errors.Is` both `ErrRemembered` and `ErrUndecodable`, render count 1. Same for `ErrNoImage` and `ErrTooLarge`.
  - `TestFailureNotRemembered`: `context.DeadlineExceeded` (per-render timeout), a cancelled caller, `exec.ErrNotFound`, `fs.ErrNotExist`, a plain `errors.New` → the next `Get` renders again.
  - `TestRemoveForgetsFailure`: after `Remove(sha)` the next `Get` renders.
  - `TestFailureCap`: with the cap lowered (a var, 3) the fourth failure clears the map first.
  - Existing test to update: `TestGetRejectsHugeImage` renders two different files under one `shaN(20)`; give each iteration its own sha (`shaN(20+i)`), or the second gets the first's remembered error.
- [ ] **Step 2: Implement** in `thumbs.go`: `failed map[string]error` (by sha) under `c.mu`, `var maxFailures = 10000`. In `Get` after the cached-file check: a remembered failure → `fmt.Errorf("%w: %w", ErrRemembered, err)`. In `run`, when `err` is one of the three file-fault classes and not a context error, store it (clear first when `len == maxFailures`). `Remove` deletes the entry. `internal/api/thumbs.go`: skip the Warn log when `errors.Is(err, thumbs.ErrRemembered)` (the status mapping is unchanged because the original class is still wrapped).
- [ ] **Step 3: exiftool server tests (RED)** (`exifserver_test.go`). Extend `fakeTool`: when its args contain `-stay_open`, it reads argfile lines from stdin and, at each `-execute<N>`, acts on the `SD2NAS_FAKE_TOOL` mode, then prints `{ready<N>}` to stdout and the `-echo4` text to stderr; `-stay_open` + `False` exits 0. Each start appends a line to `SD2NAS_FAKE_TOOL_LOG`. Modes: `preview` (JSON with a small base64 JPEG and `Orientation: 6`), `hang`, `die`, `huge` (more than `maxToolOutput`, lowered by the test), `stderr` (empty stdout, `Error: File format error` on stderr).
  - The stay_open loop also dispatches **every existing exiftool mode** of the fake (`exif-none`, `exif-huge`, `garbage`, …) per `-execute<N>`: it writes what the one-shot mode prints, then the markers, instead of exiting. It records each request's args as one JSON line in `SD2NAS_FAKE_ARGS`, so `wantArgs` checks requests, not the process argv.
  - Existing tests rewritten accordingly: `TestRawPreviewArgs` and `TestRawFallsBackToJpgFromRaw` (expected args gain `-charset`, `filename=utf8` first; read from `SD2NAS_FAKE_ARGS`), `TestRawNoPreview`, `TestRawRejectsHugePreview`, `TestToolGarbage` (same modes through the loop), and `TestToolFailureSurfacesStderr`, which moves to the `stderr` mode (an exiting process is now the `die` case, which must not be `ErrUndecodable`). ffmpeg tests are untouched.
  - `TestExifServerReuses`: three RAW renders → correct images, one start.
  - `TestExifServerTimeout`: `hang` with a 200 ms render timeout → `context.DeadlineExceeded`, the process is gone (its pid no longer running, or `cmd.ProcessState` set), and the next render (mode switched to `preview`) starts a second process and succeeds.
  - `TestExifServerDies`: `die` → an error that is **not** `ErrUndecodable` (not remembered); the next render restarts.
  - `TestExifServerHuge`: → `errTooLarge`, process killed.
  - `TestExifServerFileError`: `stderr` → `ErrUndecodable` carrying `File format error`.
  - `TestExifServerCancelWhileQueued`: two renders, the first hangs; the second's context ends while waiting for the lock → returns `context.Canceled` without touching the process.
  - `TestExifServerClose`: after one render, `Cache.Close()` returns within 2 s and the process has exited; a `Get` afterwards → `ErrClosed`.
  - `TestExifServerNewlinePath`: a RAW path containing `\n` (skip on Windows) goes through the one-shot `runTool` (no start logged).
  - `TestExifServerMissingTool`: `Exiftool: "sd2nas-no-such-tool"` → an error matching `exec.ErrNotFound` (the API keeps answering 404).
  - `TestRealExiftoolARW` and the other real-tool tests pass unchanged through the server.
- [ ] **Step 4: Implement** `internal/thumbs/exifserver.go`. Behaviour to pin:
  ```go
  // exifServer keeps one "exiftool -stay_open True -@ -" process and runs
  // one request at a time through it. Any doubt about the stream (timeout,
  // cancel, overflow, a write or read error, the process exiting) kills the
  // process; the next request starts a new one.
  type exifServer struct {
      cmd    string
      log    *slog.Logger
      lock   chan struct{} // cap 1: held for a request, a start or close
      proc   *exifProc     // nil before the first request and after a kill
      seq    int
      closed bool
      starts int // for tests
  }

  func (e *exifServer) run(ctx context.Context, args []string) (stdout []byte, stderr string, err error) {
      select {
      case e.lock <- struct{}{}:
      case <-ctx.Done():
          return nil, "", ctx.Err()
      }
      defer func() { <-e.lock }()
      if e.closed {
          return nil, "", ErrClosed
      }
      // start if e.proc == nil (exec.Command(e.cmd, "-stay_open", "True", "-@", "-"),
      // StdinPipe/StdoutPipe/StderrPipe, Start; a start error wraps as "exiftool: %w")
      e.seq++
      marker := fmt.Sprintf("{ready%d}", e.seq)
      // request: each arg on its own line, then "-echo4\n" + marker + "\n-execute<seq>\n"
      // two goroutines read stdout (cap maxToolOutput) and stderr (keep 4 KiB,
      // keep scanning) until a line equal to marker ("\n" or "\r\n" ending)
      // wait for both, or ctx.Done() → kill, wait for both readers, return ctx.Err()
      ...
  }
  ```
  - Requests carry `-charset`, `filename=utf8`, `-json`, `-b`, `-n`, `-Orientation`, `-<Tag>`, `--`, `<path>` (today's args plus the charset).
  - The marker counts only at the start of a line and at the very end of what was read (exiftool writes nothing after it until the next request). A `{ready<M>}` line with M ≠ N is doubt → kill.
  - `run` defers "kill unless this request completed cleanly" (both markers read, nothing left unread), so a panic recovered by `generateRecovered`, or any early return between the write and the markers, never leaves a live process with an unread reply. `TestExifServerPanicKills`: a seam panics after the write → the next request starts a new process and gets its own answer.
  - Result handling in `rawPreview`: empty stdout with stderr text → `fmt.Errorf("exiftool: %w: %s", ErrUndecodable, msg)`; JSON parsing as today; a JSON `Error` field → `ErrUndecodable` with it; non-empty stderr alongside good output → the existing Warn log.
  - `kill`: `Process.Kill`, close stdin, wait for `Wait` (bounded by `WaitDelay = 5 s`), `e.proc = nil`.
  - `close` (called by `Cache.Close` after `c.wg.Wait()`): takes the lock, `closed = true`, writes `-stay_open\nFalse\n`, closes stdin, waits ≤ 1 s for exit, else `kill`.
  - `Cache` gets `exif *exifServer` built in `New` (lazy process). `rawPreview` uses `c.exif.run` unless the path contains `\r` or `\n`, then `runTool` as today.
- [ ] **Step 5:** gofmt/vet/test `./internal/thumbs/ ./internal/api/`, Linux (real exiftool) and race commands; commit `Remember thumbnail failures and keep one exiftool process`.

### Task 5: Byte-level copy progress + waiting state

**Produces:** `media.CopyHashedProgress(ctx context.Context, src, dst string, progress func(done int64)) (string, error)`; `ProgressSnapshot.Current`, `CurrentSize`, `CurrentDone`, `WaitingFolder`, `WaitingHolder`.
**Consumes:** `media.CopyHashed` internals, `ingestRun.importOne`, `folderFor`, `staging.Locks.TryLock`, `*staging.BusyError`.

- [ ] **Step 1: Media tests (RED)** (`copy_test.go`): `TestCopyHashedProgress`: a 1 MiB source → `progress` called with strictly increasing values ending at exactly 1 MiB, on the calling goroutine; the hash equals `CopyHashed`'s. `nil` progress works. A cancelled copy stops calling it.
- [ ] **Step 2: Implement:** `CopyHashed` = `CopyHashedProgress(ctx, src, dst, nil)`; `ctxReader` gains `progress func(int64)` and `done int64`, calling it after every read with `n > 0`. It needs a **pointer receiver** and a keyed literal (`&ctxReader{ctx: ctx, r: in, progress: progress}`); today's value receiver would reset `done` on every `Read`.
- [ ] **Step 3: Ingest tests (RED)** (`internal/ingest`, replacing `Runner.copyFile`, whose signature becomes `func(ctx context.Context, src, dst string, progress func(int64)) (string, error)`; update the existing fakes):
  - `TestProgressBytesDuringCopy`: two new files of 10 and 20 bytes; the fake calls `progress(5)` on the second and blocks on a channel. The snapshot then has `Current == <second's card path>`, `CurrentSize == 20`, `CurrentDone == 5`, `BytesDone == 15`, `FilesDone == 1`. After release: `BytesDone == 30`, `Current == ""`, `CurrentSize == 0`.
  - `TestProgressBytesAfterFailure`: a copy that reports 7 bytes then fails → `BytesDone` ends at the file's size (today's per-file semantics), `Current` cleared.
  - `TestProgressBytesRetry`: a `CopyFailDestTaken` retry reports from 0 again; `CurrentDone` never exceeds `CurrentSize`.
  - `TestProgressWaitingForFolder`: the shared `Locks` hold the date's folder as `HolderPromote`; while the import waits, the snapshot has `WaitingFolder == <folder name>`, `WaitingHolder == "promote"`; after the release both are empty and the copy runs.
- [ ] **Step 4: Implement** (`progress.go`, `run.go`):
  - In `importOne`, before `copyNew`: `base := j.p.Snapshot().BytesDone` (only this goroutine writes it during copying) and set `Current`, `CurrentSize`, `CurrentDone = 0`. The callback: `s.CurrentDone = min(done, f.Size); s.BytesDone = base + s.CurrentDone`.
  - The pipeline's per-file update becomes `s.FilesDone++; s.BytesDone = base + f.Size; s.Current, s.CurrentSize, s.CurrentDone = "", 0, 0` (pass `base` back through `fileResult`, or compute it in the loop before `importOne`).
  - `folderFor`: before `Locks.Lock`, try `TryLock(staging.HolderIngest, f.ID)`; success → use it. A `*staging.BusyError` → set `WaitingFolder = f.Name`, `WaitingHolder = string(busy.Holder)`, then `Lock` (wait), then clear both whatever the result. Other errors as today.
- [ ] **Step 5: Shape test** (package `ingest`; package `api` can't hold a copy, because `Runner.copyFile` is unexported and `FreeSpace` runs before copying): `TestProgressSnapshotJSON`: `json.Marshal(ProgressSnapshot{…})` with the five fields set gives `current`, `current_size`, `current_done`, `waiting_folder`, `waiting_holder`; with them zero, none of the five keys appears. Step 3 covers the live values.
- [ ] **Step 6:** gofmt/vet/test `./internal/media/ ./internal/ingest/ ./internal/api/`, race command; commit `Report copy progress per byte and show what an import waits for`.

### Task 6: Re-import missing (schema v5)

**Produces:** `catalog.SchemaVersion = 5`; `catalog.DB.RequestRestore(ctx, ingestID int64, at time.Time) (int, error)`, `catalog.DB.RestoreRequested(ctx) (map[string]bool, error)`, `catalog.DB.RestoreMissing(ctx, ingestID int64, a Asset, fp string, row IngestFile) error`, `catalog.DB.RestorableMissing(ctx, ingestID int64) (restorable, requested int, err error)`; `staging.ForgetSummary.Missing` (`json:"missing"`), `.Restorable` (`json:"restorable"`), `.RestoreRequested` (`json:"restore_requested"`), `staging.ForgetResult.KeptMissing` (`json:"kept_missing"`); route `POST /api/ingest/{id}/reimport-missing`; `forgetHandlers.submit` (= `Server.SubmitIngest`).
**Consumes:** `knownOutcome`, `classifyOne`, `importOne`, `dropCopy`, `ErrWrongState`, `ingest.ErrBadCardPath`, `ingest.CardName`, `Server.SubmitIngest`, `catalog.DB.Ingest`.

- [ ] **Step 1: Schema (RED → GREEN).**
  ```go
  // schemaV5 (M7): "Re-import missing files". A row asks the next import of
  // a card holding this content to copy it into Staging again. No foreign
  // key: a request counts only while its asset is missing and was marked
  // missing before the request; RequestRestore and RestoreRequested clear
  // the rest.
  const schemaV5 = `
  CREATE TABLE restore_requests (
    sha256       BLOB PRIMARY KEY CHECK (length(sha256) = 32),
    requested_at TEXT NOT NULL
  );
  `
  ```
  `migrations` gains it; `SchemaVersion = 5`. Existing tests to update (the M6 failure class): `TestOpenCreatesSchema` (`catalog_test.go`) adds `restore_requests` to its table list; `TestMigrateV3ToV4` (`ingests_test.go`) compares with `SchemaVersion` instead of the literal `4`, keeping its notifications-index check. Then add `TestMigrateV4ToV5`.
- [ ] **Step 2: Catalog tests (RED)** (`restore_test.go`):
  - `TestRequestRestore`: an import with `missing` file rows for assets A (missing), B (missing) and C (archived again) → returns 2, rows for A and B; calling again → 2, no duplicates; a stale request for C is deleted.
  - `TestRestoreMissing`: A missing → `RestoreMissing` with `State: staged`, a Staging rel path, mtime and capture → A is staged there with `first_ingest` unchanged, `checked_at` NULL, the request gone, the fingerprint present, the ingest row inserted (outcome `new`) — one transaction. Again → `ErrWrongState`, nothing written. A culled/archived asset → `ErrWrongState`.
  - `TestRestoreRequestedFresh`: request A → A reappears (archived) → A goes missing again (`MarkMissing`, so `updated_at > requested_at`) → `RestoreRequested` doesn't return A, and its stale row is deleted.
  - `TestRestorableMissing`: an import with 3 `missing` rows (2 assets still missing, 1 of them requested) → `(2, 1)`.
- [ ] **Step 3: Implement** `internal/catalog/restore.go`:
  - `RestoreRequested` (one `tx`): delete every request whose asset isn't `missing` or has `updated_at > requested_at`, then return the remaining shas (`SELECT r.sha256 FROM restore_requests r JOIN assets a ON a.sha256 = r.sha256 WHERE a.state = 'missing' AND a.updated_at <= r.requested_at`).
  - `RestorableMissing`: two `QueryRow` counts over the import's `missing`-outcome rows whose asset is missing (all / with a fresh request). No catalog call inside a rows loop.
  - `RequestRestore` (one `tx`): `DELETE FROM restore_requests WHERE sha256 NOT IN (SELECT sha256 FROM assets WHERE state = 'missing')`; `INSERT OR IGNORE INTO restore_requests (sha256, requested_at) SELECT DISTINCT f.sha256, ? FROM ingest_files f JOIN assets a ON a.sha256 = f.sha256 WHERE f.ingest_id = ? AND f.outcome = 'missing' AND a.state = 'missing'`; return `SELECT COUNT(*) …` of that import's missing shas now requested.
  - `RestoreMissing`: `UPDATE assets SET state = 'staged', rel_path = ?, mtime = ?, capture_time = ?, tz_offset = ?, checked_at = NULL, updated_at = ? WHERE sha256 = ? AND state = 'missing'` (0 rows → `ErrWrongState`), `DELETE FROM restore_requests WHERE sha256 = ?`, `INSERT OR IGNORE` the fingerprint, insert the ingest file row (the same insert `RecordNew` uses).
- [ ] **Step 4: Ingest tests (RED):**
  - `TestImportRestoresRequested`: the catalog has asset A `missing` with the card file's fingerprint and a request → the import copies it into the date folder, outcome `new`, A `staged` at that path with the same sha, `safe_to_format` true, the card script text starts `<card>: 1 new`.
  - `TestImportMissingWithoutRequest`: no request → outcome `missing` as today, nothing copied.
  - `TestImportRestoreOtherContent`: requested, but the copy's full hash differs (fingerprint-collision seam) → recorded as a new asset; A stays `missing`.
  - `TestImportRestoreLostRace`: A turns `archived` between classify and record (seam) → the copy is removed, outcome `in_archive`, A untouched.
  - `TestImportRestoreFullVerify`: with `verify_known = full` the same restore happens.
- [ ] **Step 5: Implement** (`run.go`): the run loads `RestoreRequested` once after settings; in `classifyOne`, a match whose asset is `missing` and requested becomes `OutcomeNew` (in both the quick and full branches). In `importOne`, after the copy: `existing.State == StateMissing && j.restore[sum]` → `restoreMissing`, which calls `RestoreMissing` and, on `ErrWrongState`, re-reads the asset and falls back to `dropCopy`. Clock warnings as for a new file.
- [ ] **Step 6: Staging/forget tests and code (M-6):** `ForgetSummary` counts `Missing` (assets of the import in state missing, from the existing `IngestAssets` slice), `Restorable` and `RestoreRequested` (from `RestorableMissing`, called after that slice is built); `ForgetResult.KeptMissing`; JSON tags as in Produces. Extend `TestForgetSummary`/`TestForget`; missing rows are still never deleted.
- [ ] **Step 7: API tests and handler** (`forget.go`, `h.reimportMissing`, mirroring `reimport` without `Exclusive`): first `h.db.Ingest(ctx, id)` (`ErrNotFound` → `writeOpError`'s 404 `That import no longer exists.`); then `RequestRestore(opContext(r), id, now)`; `n == 0` → the "Nothing to re-import" message; else `h.submit(ctx, ingest.Request{…})` with the import's path/label/uuid — `forgetHandlers` gains `submit func(context.Context, ingest.Request) (*ingest.Job, error)` set to `s.SubmitIngest` in `MountForget` (the one submit + watch path, HANDOFF PF18) — choosing among the binding messages exactly as `reimport` does. Tests: `TestReimportMissingStarts` (the job is watched: `ingest.finished` arrives), `TestReimportMissingNotMounted` (`ErrBadCardPath`), `TestReimportMissingNothing`, `TestReimportMissingUnknownID` (404), `TestReimportMissingBadID` (400 `The import id must be a positive whole number.`).
- [ ] **Step 8:** gofmt/vet/test `./internal/catalog/ ./internal/ingest/ ./internal/staging/ ./internal/api/ ./internal/indexer/`, Linux and race commands; commit `Offer to re-import files whose archive copy is missing`.

### Task 7: Shift times backend

**Produces:** `staging.ShiftRequest{SHAs []string; Seconds *int64}`, `staging.ShiftResult`, `staging.Service.Shift(ctx, ShiftRequest) (ShiftResult, error)`, `staging.MaxShift`; `catalog.DB.ShiftAsset(ctx, sha, relPath string, capture, at time.Time) error`, `catalog.DB.StagingFolderWithDate(ctx, date string) (StagingFolder, bool, error)`; route `POST /api/staging/shift`.
**Consumes:** `checkSHAs`, `lockLocated`, `lockName`, `nameFree`, `moveFile`, `renameFolder`, `dropIfEmpty`, `syncDirs`, `summary`, `CreateStagingFolder`, `SetStagingFolderInfo`, `media.FolderDate`, `SplitName`, `FolderName`, `oldestCreated`.

- [ ] **Step 1: Pure helpers with tests (RED → GREEN)** in `shift.go` / `shift_test.go`:
  ```go
  // MaxShift is the largest shift, in seconds (100 years of 365.25 days).
  const MaxShift = 3155760000

  const stampLayout = "2006-01-02 15.04.05"

  // shiftWall adds d to t's wall clock, ignoring t's location's DST rules:
  // capture times are naive wall clocks (catalog.CaptureLayout).
  func shiftWall(t time.Time, d time.Duration) time.Time {
      w := time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), time.UTC).Add(d)
      return time.Date(w.Year(), w.Month(), w.Day(), w.Hour(), w.Minute(), w.Second(), w.Nanosecond(), t.Location())
  }

  // shiftedName replaces base's leading stamp when it is old's; anything
  // after it (video details, a "-1" burst suffix, the extension) is kept.
  func shiftedName(base string, old, new time.Time) (string, bool) {
      s := old.Format(stampLayout)
      if !strings.HasPrefix(base, s) {
          return base, false
      }
      return new.Format(stampLayout) + base[len(s):], true
  }
  ```
  Tests: `TestShiftWall` (with `time.LoadLocation("America/Los_Angeles")`, skip if unavailable: `2025-03-09 01:30` + 1 h → wall `02:30`, a time that doesn't exist in that zone but is kept as a wall value; −24 h across a month end); `TestShiftedName` (`2025-05-30 14.02.11.JPG`, `2025-05-30 14.02.11-1.JPG`, `2025-05-30 14.02.11 - 3840x2160 - 25p - 12sec.MP4`, `IMG_0001.JPG` unchanged, a stamp that isn't the capture time unchanged).
- [ ] **Step 2: Catalog (RED → GREEN):** `ShiftAsset` updates `capture_time`, `rel_path`, `updated_at` `WHERE sha256 = ? AND state = 'staged'` (0 rows → `ErrWrongState`) and `DELETE FROM clock_warnings WHERE sha256 = ?`, in one `tx`. `StagingFolderWithDate` is `StagingFolderForDate`'s two lookups without writing: the folder whose dates contain the date (lowest id), else the folder **named** exactly the date (returned as is, its dates unchanged; the caller adds the date), else `false`. Tests for all three outcomes.
  - Add the `testdata` cases (Decision 27) to `web/src/lib/testdata/rules-cases.json`: `shiftWall`, `shiftedName`, `folderDate`, `wholeName` as listed there. `TestShiftRuleCases` runs `shiftWall`, `shiftedName`, `media.FolderDate` and the whole-folder name rule (a small unexported `wholeFolderName(name string, dates []string) string`, also used by Step 4.6) over them.
- [ ] **Step 3: Service tests (RED)** (`shift_test.go`, real temp dirs, the existing staging helpers):
  - `TestShiftRenamesKeepsBytes`: two JPGs in `2025-05-30`, +3600 → names +1 h, file hashes unchanged, catalog capture times +1 h and rel paths updated, `Status().Version` bumped, clock warnings of those shas gone.
  - `TestShiftWholeFolderFollowsDate`: all files of `2025-05-30 - Trip`, +86400 → the same folder id renamed `2025-05-31 - Trip`, dates `["2025-05-31"]`.
  - `TestShiftWholeFolderNameTaken`: `2025-05-31 - Trip` exists → files shifted, the folder keeps its name, dates updated, `WantedName == "2025-05-31 - Trip"`.
  - `TestShiftWholeUntitled`: `Untitled` (no date prefix) → name kept, dates updated.
  - `TestShiftPartialStays`: folder dates `[05-30, 05-31]`, one 05-30 file +86400 → stays, renamed.
  - `TestShiftPartialRefilesExisting`: folder B holds 05-31 → the shifted file moves into B; A keeps the rest.
  - `TestShiftPartialNewFolder`: no folder holds 06-02 → new `2025-06-02` with A's `created_at`, the file inside, `Moved == 1`.
  - `TestShiftDayCutoff`: `2025-05-31 03:30` +20 min stays in the 05-30 folder; +40 min (04:10) → 05-31.
  - `TestShiftBurst`: `12.00.05.JPG` and `12.00.05-1.JPG` +60 → `12.01.05.JPG` and `12.01.05-1.JPG`.
  - `TestShiftOrder`: `12.00.00.JPG`, `12.00.05.JPG` +5 → `12.00.05.JPG`, `12.00.10.JPG` with no `-1`; and −5 on `12.00.05`, `12.00.10` → `12.00.00`, `12.00.05`.
  - `TestShiftCollision`: the target name holds other content → `-1` added, the other file's bytes unchanged.
  - `TestShiftNoStamp`: `IMG_0001.JPG` → name kept, capture time shifted, `Renamed == 0`.
  - `TestShiftNoCapture`: an asset with a zero capture time → `Skipped == 1`, untouched.
  - `TestShiftRefusals`: `Seconds == nil`, `0`, `MaxShift+1`, files from two folders, an archived sha, a result in 1969 or 2100 → `ErrBadRequest` with the binding sentences (`plainReason` text).
  - `TestShiftBusy`: the folder held as `HolderIngest` → `*BusyError`, nothing changed. A re-file target held → `*BusyError`.
  - `TestShiftPartialFailure`: the service's `rename` seam fails on the second file → the first is renamed and recorded, the second untouched on disk and in the catalog, the error starts `shifted 1 of 2 files: `.
  - `TestShiftCatalogFailureMovesBack`: a `shiftAsset` seam (defaults to `db.ShiftAsset`) fails on the second file after its rename → that file is moved back to its old name (on disk, same hash) and its row is unchanged; the error joins both and starts `shifted 1 of 2 files: `.
  - `TestShiftRefilesIntoFolderNamedDate`: a folder named `2025-06-02` whose dates don't list 06-02 → the shifted file goes into it (its dates gain 06-02), no `ErrNameTaken`.
  - `TestShiftStopsOnClose`: a seam blocks after the first file; `Close()` is called in another goroutine; the shift returns `shifted 1 of 3 files: SD2NAS is shutting down`, files 2–3 untouched, and `Close` returned only after the shift did.
- [ ] **Step 4: Implement `Service.Shift`.** Order (each step's code follows `Move`):
  0. Register with the service like promote and rescan: under `s.mu`, `closed` → `ErrClosed`, else `s.wg.Add(1)` (released by a defer), so `Close` waits for it. Before each file, `s.bgCtx.Err() != nil` stops the loop with `shifted N of M files: SD2NAS is shutting down`. The filesystem calls use `s.bgCtx`-independent `ctx` (the request's `opContext`), so a file in progress finishes.
  1. Validate: `Seconds` nil/0/over `MaxShift` (`fmt.Errorf("%w: Shift by …", ErrBadRequest)` so `plainReason` gives the sentence); `checkSHAs`.
  2. `locate`; all files in one folder, else `Shift files from one folder at a time.` `whole` = the request covers every staged asset under that folder (`AssetsUnder`).
  3. Compute each file's new capture (`shiftWall`) and refuse a result outside 1970–2099 naming `path.Base(rel)`. Files with a zero capture time are skipped.
  4. Unless `whole`, find re-file targets for dates not in the source's dates with `StagingFolderWithDate`; `lockLocated(shas, targetIDs...)`; under the locks recompute the plan; a different target → `ErrChanged`. New date folders: `lockName(date)`, `nameFree`, `MkdirAll`, `CreateStagingFolder(date, [date], src.CreatedAt)`, lock the new row — as `Move` does.
  5. Process files sorted by old capture time (then name): descending for a positive shift, ascending for a negative one. Per file: destination dir = same subpath in the target folder; new base from `shiftedName`; if the dir and base are unchanged skip the rename; else `moveFile(ctx, from, dir, base, sha)`; then `ShiftAsset(sha, newRel, newCapture, now)`. If `ShiftAsset` fails after a rename, move the file back with `moveFile(ctx, to, oldDir, oldBase, sha)` and join both errors (a failed move-back is logged and joined too; the next rescan follows the file). On an error: stop, keep what was done, return `shifted N of M files: <err>` (joined with `syncDirs`). A new date folder that ends up with no file (a failure before its first move) is removed with `dropIfEmpty`.
  6. Dates: `whole` → `SetStagingFolderInfo(src, new folder dates)`; then if `wholeFolderName(src.Name, newDates)` differs from `src.Name`, `lockName` + `nameFree` + `renameFolder` to it; `ErrNameTaken`/busy name → keep, set `WantedName`. Partial → each target folder's dates gain its files' dates; `dropIfEmpty(src)`.
  7. `syncDirs(touched)`; `s.changed()`; `Folder = summary(src)` unless removed.
  ```go
  type ShiftRequest struct {
      SHAs    []string
      Seconds *int64 // required; ±MaxShift
  }

  type ShiftResult struct {
      Shifted    int    `json:"shifted"`  // capture times changed
      Renamed    int    `json:"renamed"`  // of those, files whose name changed
      Moved      int    `json:"moved"`    // of those, files re-filed into another folder
      Skipped    int    `json:"skipped"`  // files with no capture time, left alone
      Folder     Folder `json:"folder"`   // the source folder afterwards; zero when removed
      Removed    bool   `json:"removed"`  // the source folder was left empty and removed
      WantedName string `json:"wanted_name,omitempty"` // a whole-folder shift's new name, when it was taken
  }
  ```
- [ ] **Step 5: API** (`staging.go`): `POST /api/staging/shift`, body `{"fileShas": [...], "seconds": n}` (`largeBody`, unknown fields 400; a missing `seconds` → the "Say how far" sentence), `opContext`, errors through `writeOpError`. Tests: `TestShiftEndpoint` (200 body fields), `TestShiftEndpointSentences` (each 400 sentence), `TestShiftEndpointBusy` (409 `folder_busy`).
- [ ] **Step 6:** gofmt/vet/test `./internal/staging/ ./internal/catalog/ ./internal/api/`, Linux (TZ=Pacific/Chatham) and race commands; commit `Add the shift times tool for staged files`.

### Task 8: Staging views + F9/F10/"1 files"

**Produces:** `web/src/lib/views.js` (`sortFolders`, `groupByMonth`, `folderLine`, `needsReload`), `web/src/lib/prefs.js` (`loadPref(key, allowed, fallback)`, `savePref(key, value)`, `PATTERN_KEY`, `STAGING_VIEW_KEY`, `FOLDER_VIEW_KEY`).
**Consumes:** `GET /api/staging`, `GET /api/staging/{id}/files`, `lib/selection.js`, `lib/press.js`, `Thumb.svelte`.

- [ ] **Step 1: F9 (Go, RED → GREEN):** `TestListSkipsNASMetadata`: `Staging/@eaDir`, `Staging/#recycle`, `Staging/.x` → not in `Untracked`. `diskFolders` uses `!skipName(e.Name())` (reconcile's rule). Commit with the task.
- [ ] **Step 2: Node tests (RED)** `views.test.js`, `prefs.test.js`:
  - `sortFolders(folders, 'name'|'age'|'size'|'files')`: name ascending; age, size, files descending; ties by name; the input isn't mutated.
  - `groupByMonth`: `2025-05-30 - Trip` and `2025-05-02` → one group `{key: '2025-05', label: 'May 2025', folders}`; groups oldest first; folders without a date (`date === ''`) in a last group `No date`; folders inside a group in name order.
  - `folderLine(f)` → `1 file · 2.1 MB`, `3 files · …` (fixes "1 files").
  - `needsReload(dataVersion, liveVersion, connected)` → true only when connected and the versions differ and live isn't 0.
  - `loadPref`: an unknown stored value → fallback; `localStorage` throwing → fallback, no throw; `savePref` swallows only storage errors (returns false).
- [ ] **Step 3: Staging page.** A segmented control `Cards | List | Timeline` in the toolbar (buttons with `aria-pressed`, ≥ 44 px), remembered with `STAGING_VIEW_KEY`.
  - **List:** one row per folder: checkbox, name link, date range, `folderLine`, kind tags, age, warning/in-archive badges; a sort `<select>` (Name, Oldest, Largest, Most files). At ≤ 760 px each row wraps to two lines; no table, no sideways scroll. Rows ≥ 56 px; the same click/Ctrl/Shift/long-press selection as the cards (`clickAction`, `longPressSelect`), held folders not selectable.
  - **Timeline:** month headings; under each, a vertical rule with one entry per folder (date, name link, `folderLine`, up to 5 sample thumbs, badges); same selection.
  - Cards view uses `folderLine` too.
  - F10: after a load, `if (needsReload(res.version, live.staging.version, live.connected) && !reloadedOnce) { reloadedOnce = true; load(); }` (Staging and Folder pages).
  - `PATTERN_KEY` moves to `prefs.js`; the stored value keeps working.
- [ ] **Step 4: Folder page.** `Grid | List` control (`FOLDER_VIEW_KEY`). List rows: 56 px thumb, name, capture time, kind, size, clock-warning and in-archive markers; tap opens the lightbox unless selecting; the existing selection pattern; the kind chips filter both views.
- [ ] **Step 5:** `cd web && npm test && npm run build`; `go test ./internal/staging/ ./web/...`; commit `Add list and timeline views to staging`. Controller: browser check at 375×812 and desktop for all five views.

### Task 9: Shift times UI

**Produces:** `web/src/lib/shift.js` (`parseAmount`, `secondsBetween`, `shiftWall`, `shiftedName`, `folderDateOf`, `wholeFolderName`); the Shift dialog on the Folder page.
**Consumes:** `POST /api/staging/shift`, `GET /api/settings` (`day_starts_at`, for the preview only).

- [ ] **Step 1: Node tests (RED)** `shift.test.js` (all arithmetic on `Date.UTC`, so results don't depend on the test machine's TZ). First, `TestRuleCases`-style: read `src/lib/testdata/rules-cases.json` (`fs.readFileSync(new URL('./testdata/rules-cases.json', import.meta.url))`) and check `shiftWall`, `shiftedName`, `folderDateOf` and `wholeFolderName` give every case's `want` (Decision 27: the same table the Go test uses). Then:
  - `parseAmount({sign:'+', days:'0', hours:'1', minutes:'30', seconds:'0'})` → `{seconds: 5400}`; `'-'` → `-5400`; all zero → `{error: 'Shift by at least one second.'}`; `'1.5'` or `'x'` → `{error: 'Use whole numbers.'}`; more than 100 years → `{error: 'Shift by at most 100 years.'}`; empty fields count as 0.
  - `secondsBetween('2000-01-01T00:03:10', '2025-05-30T14:02')` → the exact difference (a target without seconds means `:00`).
  - `shiftWall('2025-03-09T01:30:00', 3600)` → `'2025-03-09T02:30:00'`.
  - `shiftedName` mirrors Go's (same five cases as Task 7).
  - `folderDateOf('2025-05-31T03:59:59', '04:00')` → `'2025-05-30'`.
  - `wholeFolderName('2025-05-30 - Trip', ['2025-05-31','2025-06-01'])` → `'2025-05-31 - Trip'`; `'Untitled'` unchanged.
- [ ] **Step 2: Dialog.** Entry points: the SelectionBar's `Shift times…` and a header button `Shift all times…`, which sends **every** staged file of the folder whatever the kind chips show (so the server sees a whole-folder shift). With more than 20000 files it is disabled with the hint `Folders over 20,000 files can't be shifted whole; select fewer files.` The preview is a preview only (Decision 27); the server's answer decides. Two modes (radio, ≥ 44 px):
  - **By an amount:** sign toggle `+`/`−`, inputs Days, Hours, Minutes, Seconds (`inputmode="numeric"`).
  - **So that a file reads:** the first selected file (by capture time) is named with its current time, and a `datetime-local` input (`step="1"`) sets its correct time; the amount is `secondsBetween`.
  - Preview: the first and last file's new name, and — when `day_starts_at` loaded — for a whole folder the new folder name, else how many files leave this folder. Explanation text: "Only names and capture times change; the files' contents stay as they are. Files whose new date isn't one of this folder's dates move to the folder for that date."
  - Submit → `POST /api/staging/shift`; success toast `Shifted N files` (+ `; M moved to other folders`, `; K had no capture time`), plus `wanted_name` → `Kept the name <name>: <wanted> is taken. Merge them if they belong together.`; when `removed`, navigate to `/staging`; else reload. Errors in the dialog via `describeError`.
- [ ] **Step 3:** `cd web && npm test && npm run build`; commit `Add the shift times dialog`. Controller: browser check at 375×812 (bottom sheet) and desktop (centred): no sideways scroll, every control ≥ 44 px.

### Task 10: Settings page

**Produces:** `web/src/lib/settings.js` (`FIELDS`, `changedKeys(loaded, form)`, `fieldOfError(message)`, `checkField(key, value)`); `pages/Settings.svelte` at `/settings`; a Settings tab in `Nav.svelte`; the `<summary>` tap target.
**Consumes:** `GET/PUT /api/settings` (`settings`, `defaults`), `GET /api/auth`, `POST /api/logout`, `prefs.js`, `naming.js` (`promoteTarget`, `DEFAULT_PATTERN`).

- [ ] **Step 1: Node tests (RED)** `settings.test.js`:
  - `changedKeys({a:'1',b:'2'}, {a:'1',b:'3'})` → `{b:'3'}`; nothing changed → `{}`.
  - `fieldOfError('Setting staging_notice_at: want HH:MM, 00:00 to 23:59.')` → `'staging_notice_at'`; `'Setting backup_dir: pick a folder outside Archive/ and Staging/.'` → `'backup_dir'`; `'Unknown setting x.'` → `'x'`; anything else → `''`.
  - `checkField`: the HH:MM keys reject `9:00`/`24:00`; `staging_warn_days` 0–3650 digits only; `free_space_reserve_gb` digits only; `backup_dir` empty, starting with `/`, or matching `^[A-Za-z]:\\` (Windows dev); others accept. It is **advisory** (Decision 27): its message shows under the field but never disables Save; the server's 400 sentence wins.
  - Rule cases: read the `settings` section of `src/lib/testdata/rules-cases.json` and check `checkField(key, value)` agrees with each case's `ok` (the same table the Go `TestSettingsCases` uses).
- [ ] **Step 2: Page.** Cards following the mockup's `card`/`form2` style:
  - **Imports:** Day starts at (`day_starts_at`, `type="time"`), Check known files (`verify_known`: Quick / Full), Free-space reserve in GB.
  - **Staging:** Flag folders older than N days (`staging_warn_days`, 0 = off), Daily notice at (`staging_notice_at`).
  - **Archive index:** Nightly scan and backup at (`archive_rescan_at`), Backup folder (`backup_dir`; `GET` returns the resolved path, never `""`, so the field shows `(default)` beside it when `settings.backup_dir === defaults.backup_dir`; a "Use default" button sends `""`), Skip in the archive (`archive_skip`, a textarea).
  - Each field shows its default ("Default 09:00") and a per-field error. One **Save changes** button (disabled when `changedKeys` is empty) sends only changed keys; **Discard** restores. A 400 marks the field `fieldOfError` names and shows the sentence; success toasts `Settings saved` and reloads.
  - **This device:** Default promote pattern (validated with `promoteTarget`, saved to `PATTERN_KEY`, applied by the Staging promote dialog) and, when `GET /api/auth` says `required`, **Sign out on this device** → `POST /api/logout` → reload.
- [ ] **Step 3: Nav and routes.** `App.svelte` maps `/settings` → `{tab: 'settings', view: 'settings'}`; `Nav.svelte` gains a fourth tab with a gear icon (phone bar: four tabs, each ≥ 44 px).
- [ ] **Step 4: Tap target (M5-5)** in `app.css`:
  ```css
  summary { cursor: pointer; min-height: 44px; box-sizing: border-box; padding: 10px 0; margin: -10px 0; }
  details[open] > summary { margin-bottom: 0; }
  ```
- [ ] **Step 5:** `cd web && npm test && npm run build`; commit `Add the settings page`. Controller: browser check at 375×812 and desktop (no sideways scroll, ≥ 44 px), including the Archive page's problems `<details>` and the four-tab phone bar.

### Task 11: Login page and token handling in the UI

**Produces:** `web/src/lib/auth.svelte.js` (`auth` state, `checkAuth()`), `pages/Login.svelte`, `setUnauthorizedHandler(fn)` in `lib/api.js`.
**Consumes:** `GET /api/auth`, `POST /api/login`.

- [ ] **Step 1: Node tests (RED)** `api.test.js`: a 401 with `code: 'token_required'` calls the registered handler once and still throws `ApiError` (status 401); a 401 `bad_token` and other errors don't call it.
- [ ] **Step 2: Implement.**
  - `App.svelte`: on mount `await checkAuth()`; when `required && !authorized`, render only `<Login />` (no Nav, no `connectEvents`); otherwise as today. The unauthorized handler sets `auth.authorized = false` (cookie expired, token changed), which closes the `EventSource` and shows Login.
  - `live.svelte.js` (in this task's files): export `disconnectEvents()`, which sets a `stopped` flag, closes the source and clears the 5 s retry timer; `connectEvents()` clears `stopped`; the `onerror` retry and the `visibilitychange` listener do nothing while `stopped`. Otherwise the Login screen would loop 401s on `/api/events` every 5 s and on every return to the tab. An `EventSource` 401 never reaches the api.js handler: an expired cookie is noticed at the next `api()` call (every page loads through it).
  - `Login.svelte`: heading `SD2NAS`, text "This SD2NAS is protected by a password. Enter it once; this browser remembers it for a year.", a hint under the field "It's the SD2NAS_TOKEN value in the container's settings.", a password input labelled **Password** (`autocomplete="current-password"`, Enter submits), **Sign in**. 401 → `That password is not right.`; success → `location.reload()`. Centred card on desktop, full width with 16 px gutters on a phone.
  - A `GET /api/auth` failure (network) shows the app as today (the API's own errors surface).
- [ ] **Step 3:** `cd web && npm test && npm run build`; commit `Ask for the token in the web UI`. Controller: run with `SD2NAS_TOKEN` set and check login, reload, sign out, at 375×812 and desktop (no sideways scroll, ≥ 44 px), and that the network log shows no repeating `/api/events` 401s on the Login screen; and without a token (no login screen).

### Task 12: Ingest UI: live copy detail, re-import missing

**Produces:** `messages.js` `waitingText(folder, holder)`, `copyDetail(p)`; IngestDetail's re-import-missing button and dialog.
**Consumes:** Task 5 fields, Task 6 route and `ForgetSummary` fields.

- [ ] **Step 1: Node tests (RED)** `messages.test.js`:
  - `waitingText('2025-05-30 - Trip', 'promote')` → `Waiting for 2025-05-30 - Trip: a promote is moving it.`; `operation` → `… a staging change is in progress.`; `rescan` → `… the staging rescan is checking it.`; anything else → `… it is busy.`
  - `copyDetail({current:'DCIM/100MSDCF/C0001.MP4', current_size: 8e9, current_done: 2e9})` → `Copying C0001.MP4 · 2.0 GB of 8.0 GB`; `null` when `current_size < 50_000_000` or no `current`.
- [ ] **Step 2: LiveIngest.svelte:** under the copying line, `copyDetail(p)` when non-null and `waitingText` when `waiting_folder` is set. The bar now moves within a file (no other change).
- [ ] **Step 3: IngestDetail.svelte:**
  - In the missing `<details>`, when `forget.restorable > forget.restore_requested`: **Re-import N missing files…** → a dialog: "SD2NAS copies these files from the card into Staging again. If an array disk dropped out, fix that first: its files come back by themselves at the next archive scan." → `POST /api/ingest/{id}/reimport-missing` → toast with the server's `message`, reload.
  - When `forget.restore_requested > 0`: `N files will be imported again the next time you insert the card.`
  - The forget dialog lists `N files whose archive copy is missing stay known` when `forget.missing > 0`.
- [ ] **Step 4:** `cd web && npm test && npm run build`; commit `Show copy progress detail and offer to re-import missing files`. Controller: browser check at 375×812 and desktop (no sideways scroll; the new button and the dialog's buttons ≥ 44 px) with a long fake copy (a large file on a temp card) and the missing flow from the smoke run.

### Task 13: Release prep: pinned images, `:latest`, README

**Produces:** pinned `deploy/Dockerfile`, `deploy/dockerfile_test.go`, template `:latest`, README sections.

- [ ] **Step 1: Test (RED)** `deploy/dockerfile_test.go` `TestDockerfilePinned`: every `FROM` line of `Dockerfile` matches `^FROM [a-z0-9./-]+:[A-Za-z0-9._-]+@sha256:[0-9a-f]{64}( AS [a-z]+)?$`; there are exactly three; the runtime stage is still Alpine 3.22.
- [ ] **Step 2: Pin.** For each of `node:24-alpine`, `golang:1.26-alpine`, `alpine:3.22`: `docker pull <tag>`, then `docker image inspect --format '{{index .RepoDigests 0}}' <tag>` for the digest, and write the most specific tag that shares it (e.g. `node:24.<x>.<y>-alpine3.22`; find it with `docker buildx imagetools inspect`). A comment above the first `FROM`: how to refresh the pins (the two commands). Build the image (`docker build -f deploy/Dockerfile -t sd2nas:m7 .`) to prove it.
- [ ] **Step 3: Template.** `<Repository>ghcr.io/aspiziri/sd2nas:latest</Repository>`; drop the `:edge` comment; `template_test.go` expects `:latest`.
- [ ] **Step 4: README.**
  - **Optional password (`SD2NAS_TOKEN`)** (after Card script): generate and install it on Unraid without a placeholder —
    ```
    TOKEN=$(openssl rand -hex 24); echo "$TOKEN"
    printf "\nSD2NAS_TOKEN='%s'\n" "$TOKEN" >> /boot/config/plugins/unassigned.devices/sd2nas.cfg
    ```
    (the leading `\n` keeps it off the last line of a cfg without a final newline; the single quotes keep bash from reading anything into it). A hand-picked token may use only letters, digits and `. _ ~ + / = -`, or SD2NAS refuses to start and logs `SD2NAS_TOKEN may use only letters, digits and . _ ~ + / = -`. Then Docker → SD2NAS → Edit: a new install has the **Token** field (Advanced View); an existing container gets **Add another Path, Port, Variable…** → Variable, Key `SD2NAS_TOKEN`, Value = the printed token. The web UI asks once per browser (the home-screen app on iOS asks once too); Settings → This device → Sign out. `/healthz` stays open. Changing the token signs every browser out.
  - **Settings:** the Settings page replaces the terminal `curl` (keep the curl as a fallback example).
  - **Updating:** `:latest` follows releases; `:edge` follows `main`; switching = Edit → Repository. Existing installs keep their saved repository.
  - Remove any remaining "until M7" wording.
- [ ] **Step 5:** `go test ./deploy/...`, the Linux command; commit `Pin the base images and point the template at :latest`.

### Task 14: PLAN.md, final verification

**Produces:** `docs/PLAN.md` updated (CRLF in the working tree is fine; LF in the index).

- [ ] **Step 1: PLAN.md edits.**
  - §1 "v1 does not": `Have user accounts. It's reachable only on the LAN, with an optional shared token (§10).` (drops "or Tailscale").
  - §3: the stale-staging sentence names `staging_notice_at` (default 09:00) instead of "after 09:00".
  - §6: schema version 5 adds `restore_requests`.
  - §10: list `GET /api/auth`, `POST /api/login`, `POST /api/logout`, `POST /api/ingest/{id}/reimport-missing`, `POST /api/staging/shift`; the settings line says the Settings page exists; the **Optional token** paragraph states Decisions 1–5 (Bearer for the script, the cookie for browsers, what stays open).
  - §12 "M7: Shift times": rewrite to Decisions 17–22 (staged only, one folder, the stay/re-file rule, stamp-only renames, wall clock, clock warnings cleared).
  - §14: base images pinned by digest; the template's Token variable; `:latest`.
  - §16 M7: tick items 1–9 (each with a short note where useful); add `Implementation plan: docs/plans/m7-polish.md.`, a **What M7 delivers** paragraph (as M6 has) and the deferred follow-ups list from Decision 26. Item 10 stays `- [ ]` with: "Code side done (pinned images, template on `:latest`). Owner's step below."
  - **On the NAS (the owner's)** for M7:
    1. Merge the M7 PR, then tag at once from an up-to-date `main`:
       ```
       git checkout main && git pull --ff-only
       git tag -a v0.1.0 -m "SD2NAS 0.1.0"
       git push origin v0.1.0
       gh run list --repo aspiziri/SD2NAS --workflow release.yml --limit 1
       docker buildx imagetools inspect ghcr.io/aspiziri/sd2nas:latest
       ```
       The run must succeed and `:latest`, `:0.1.0` and `:0.1` must exist. Then tick the `v0.1.0` item with the date.
    2. On Unraid: Docker → SD2NAS → Edit → Repository `ghcr.io/aspiziri/sd2nas:latest` → Apply. `/healthz` shows `"version": "0.1.0"`.
    3. Optional: the token, as the README says; then insert a card and check the notice still arrives (the script sends the token).
    4. Open Settings; set "Daily notice at" if 09:00 doesn't suit.
    5. With a real card: Shift times on a staged folder (+1 h, then −1 h back), and the List/Timeline views on the phone.
- [ ] **Step 2: README** cross-check against PLAN (token, settings, `:latest`); no `<…>` placeholders in commands.
- [ ] **Step 3: Verification.** `gofmt -l .` empty; `go vet ./...`; `go test ./...` (Windows); the Linux and race commands; `cd web && npm test && npm run build`; no CR bytes in changed files; `git diff --stat main...HEAD` shows no `go.mod`/`go.sum`/`package*.json` change.
- [ ] **Step 4: Smoke run (controller).** Build `sd2nas:m7`; run as in HANDOFF (`-p 18080:8080`, `testdata/cards` read-only). Check: the SONY card imports "2 new … Card safe to format."; with `-e SD2NAS_TOKEN=<random 32 hex>` the card script (with `SD2NAS_TOKEN` in its cfg) still imports, a POST without it gets the two-line refusal, the browser asks once; a RAW thumbnail renders and `ps` in the container shows one `exiftool` after several; the archive-missing flow ("1 file's archive copy is missing") → Re-import → reinsert (re-POST) → "1 new … Card safe to format."; Shift times on a staged folder; `docker stop` under 2 s; a second container on the same `/config` exits 3. Browser: every new view at 375×812 and desktop.
- [ ] **Step 5:** commit `Record M7 in the plan`.
