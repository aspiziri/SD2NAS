# M3 Web UI (Read-Only) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A phone-first, read-only web UI embedded in the sd2nas binary. It has an ingest page with live progress over Server-Sent Events plus the import history, a staging list and folder view with thumbnails and full-screen previews for JPEG, RAW and video files, and a PWA manifest so it can be added to a phone's home screen.

**Architecture:** The Go side gains five pieces. `web/embed.go` (package `web`) embeds `web/dist`. `internal/api/ui.go` serves the built SPA with the right cache headers and a fallback to `index.html`. `internal/api/events.go` is an SSE hub with `GET /api/events`, and `internal/api/ingestfeed.go` publishes ingest progress to it. `internal/staging` builds the read-only staging model from the catalog and the `Staging/` directory. `internal/thumbs` renders and caches thumbnails and previews: Go for JPEG/PNG, `exiftool` for RAW, `ffmpeg` for video. The frontend in `web/` is a Svelte 5 (runes) SPA built by Vite, with no router or other libraries. Plain JS modules (`api.js`, `format.js`, `paths.js`) are tested with `node --test`. The Dockerfile gains a node stage, and CI gains a web job.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `net/http`, `log/slog`, `github.com/disintegration/imaging v1.6.2` (+ `golang.org/x/image v0.36.0`), exiftool and ffmpeg (already in the runtime image), Svelte 5.59.1, Vite 8.3.8, `@sveltejs/vite-plugin-svelte` 7.3.1, Node 24.

**Spec:** `docs/PLAN.md`. Relevant sections: §2, §3 ("safe to format" wording), §4 (layout: `internal/thumbs`, `internal/staging`, `web/`), §5 (thumbs row), §6 (schema; `internal/catalog/schema.go` is authoritative), §7 step 7 (queue thumbnails after an ingest), §8 (folder naming `YYYY-MM-DD - Title`; "List folders: read `Staging/`, join counts and sizes from `assets`"), §9 (folder names parsed as text, never as dates), §10 (API, SSE, request guard), §13, §14 (Dockerfile, CI, local development), §16 M3 checklist. UI design: `docs/ui-mockup.html` (the owner approved it; phone use matters most).

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`, `go 1.24.0` in `go.mod` with no `toolchain` line. CI installs the Go version from `go.mod`, so **use no API newer than Go 1.24**: no `sync.WaitGroup.Go`, `testing/synctest`, or `T.Output`. Fine to use: `http.NewResponseController`, `filepath.IsLocal`, `context.WithoutCancel`, `min`/`max`, `slices`, `t.Context()`, `slog.DiscardHandler`, `omitzero`, `range` over an int.
- **Dependencies, and nothing else.** Go: `github.com/disintegration/imaging v1.6.2` and `golang.org/x/image v0.36.0` (Task 5). npm (all `devDependencies`, exact versions): `svelte 5.59.1`, `vite 8.3.8`, `@sveltejs/vite-plugin-svelte 7.3.1` (Task 8). No router, test framework (vitest, svelte-check, playwright), CSS framework, icon pack, or SSE library. Frontend tests use the built-in `node --test`. Icons are inline SVG.
- M3 touches: `web/` (new), `internal/api/`, `internal/catalog/staging.go` (+ test), `internal/staging/` (new), `internal/thumbs/` (new), `cmd/sd2nas/main.go`, `deploy/Dockerfile`, `.github/workflows/ci.yml`, `.gitignore`, `.gitattributes`, `go.mod`, `go.sum`, and `docs/PLAN.md` (Task 13 only). Do not modify `internal/ingest/`, `internal/media/` or `internal/config/`. **Read-only:** no endpoint or UI control changes staging (no rename, merge, cull or promote, not even disabled buttons). Leave out the mockup's "Re-import anyway" and "Unmount" buttons too.
- **Binding API values** (reviewers check them verbatim):
  - `GET /api/events`: `Content-Type: text/event-stream`, `Cache-Control: no-store`, `X-Accel-Buffering: no`. The first bytes are `retry: 3000\n\n`, then one frame per registered snapshot, then live frames. A frame is exactly `event: <type>\ndata: <one-line JSON>\n\n`. Keepalive is the comment `: keepalive\n\n` every 25 s (`DefaultKeepalive`). M3 event types: `ingest.current` (data = the `GET /api/ingest/current` body `{"running": view|null, "queued": [view…]}`) and `ingest.finished` (data = the ingest view of the job that just finished, without `progress`/`files`/`clock_details`). Reserved for later milestones: `staging.changed`, `promote.progress`, `indexer.progress`.
  - `GET /api/staging` → `{"folders":[Folder…],"untracked":["dir",…]}`. `GET /api/staging/{id}/files?kind=jpg|raw|video` → `{"folder":Folder,"files":[File…]}`. JSON fields of Folder: `id, name, date, title, dates, created_at, files, bytes, counts{jpg,raw,video}, first_capture?, last_capture?, samples[{sha256,kind}], clock_warnings, warnings[]`. JSON fields of File: `sha256, name, path, kind, size, capture_time?, tz_offset?, clock_warnings?`. API kinds are `jpg` (catalog kind `image`, which includes png/gif/tif), `raw` and `video`. Times are wall clock `2006-01-02T15:04:05` (`catalog.CaptureLayout`). An unknown kind → 400; an unknown folder → 404.
  - `GET /api/thumbs/{sha}` makes a JPEG whose longest edge is at most **400** px (`thumbs.ThumbSize`), and `GET /api/preview/{sha}` one of at most **1600** px (`thumbs.PreviewSize`). Neither ever upscales. JPEG quality is 80 for thumbs and 85 for previews. Success headers: `Content-Type: image/jpeg`, `Cache-Control: public, max-age=31536000, immutable`, `ETag: "<sha>-<size>"`. Errors are JSON `{"error": …}` with `Cache-Control: no-store`. The cache file is `<config>/thumbs/<sha[0:2]>/<sha>-<size>.jpg`, i.e. `/config/thumbs/…` in the container. `sha` must be 64 lowercase hex digits, or the response is 400.
  - UI files: `assets/*` (Vite's content-hashed files) get `Cache-Control: public, max-age=31536000, immutable`. Everything else, `index.html` included, gets `Cache-Control: no-cache` plus a strong `ETag`. A GET for an extension-less path outside `/api/` serves `index.html` (SPA fallback). A missing path that has an extension → 404. An unknown `/api/…` path → JSON 404, or JSON 503 naming the `/healthz` problems when the server is degraded.
  - Request guard (exists, `internal/api/guard.go`): every non-GET/HEAD request needs the header `X-SD2NAS: 1`. The UI's fetch wrapper sends it on every non-GET request.
- **UI look:** the mockup's tokens, verbatim: `--bg:#141c27; --nav:#0f151e; --panel:#1b2636; --panel2:#223047; --line:#2e3d55; --text:#e6edf6; --muted:#8d9bb0; --accent:#4f9cf9; --accent2:#3a7fd6; --ok:#3fbf7f; --warn:#e8b04a; --bad:#e5636b; --r:10px`, and the system font stack `system-ui,-apple-system,"Segoe UI",Roboto,sans-serif`. The phone breakpoint is `max-width: 760px`. On a phone there is a bottom tab bar, one-column folder cards and a full-screen preview. On desktop there is a 220 px left sidebar. Svelte 5 runes only (`$state`, `$derived`, `$effect`, `$props`), with no `{@html}` and no `export let`. Plain CSS (global tokens in `web/src/app.css`, component styles scoped).
- Tests pass on Windows and Linux. Use `path/filepath` for OS paths and `path` for slash-separated `rel_path` values. Close every file, `*sql.Rows` and `*catalog.DB`. In tests, call `t.TempDir()` before registering a `Close` cleanup.
- **External tools in tests:** a test that needs `exiftool` or `ffmpeg` calls the `needTool` helper (Task 6). The helper `t.Skip`s with the reason when `exec.LookPath` fails, unless `SD2NAS_REQUIRE_TOOLS=1`, in which case it fails. The Windows dev box has ffmpeg but no exiftool. The Linux run below installs both and sets the variable, so those tests really run there.
- **Commands.** Windows (Git Bash, repo root): `go test ./...`, `gofmt -l .` (must print nothing), `go vet ./...`. Linux (Docker Desktop must be running):
  `MSYS_NO_PATHCONV=1 docker run --rm -e TZ=Pacific/Chatham -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q exiftool ffmpeg && go vet ./... && go test ./...'`
  Race (Tasks 2, 3, 5, 7): `MSYS_NO_PATHCONV=1 docker run --rm -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q build-base exiftool ffmpeg && CGO_ENABLED=1 go test -race ./internal/... ./web/...'`
  Web (from Task 8, in `web/`): `npm test`, `npm run build`.
- **Go tests and `go vet` pass on a fresh checkout without npm.** `web/dist/.placeholder` is committed, and `npm run build` must keep it (Task 1, Task 8).
- Never silently swallow an error. Return it wrapped with `%w`, answer the request with it, or log it with `slog` (key `"err"`). The one exception: a failed write or flush to an HTTP client that has gone away ends that response, and its handler just returns.
- Export only the identifiers listed in each task's **Produces** block. Unexported helpers are free unless a later task's **Consumes** names them.
- Tests that wait on another goroutine use a 10 s deadline and fail with a clear message. Never rely on a sleep for correctness (short sleeps inside a polling loop are fine).
- Commit on branch `m3-web-ui`, one commit per task (more is fine), with messages in the repo's style (an imperative sentence, e.g. "Serve the embedded web UI with SPA fallback"). Every message ends with a blank line and then `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. Never commit `HANDOFF.md`, `web/node_modules/` or `web/dist/` contents other than `.placeholder`. Never push.

## Review Focus

1. **SSE streams that never end.** A client that disconnects, a phone that sleeps, and server shutdown must each end the handler and its subscription. `http.Server.Shutdown` waits for open handlers, so `Server.Close` must end every stream first. A slow client must never block a publisher. Pinned in Task 2 (`TestEventsClientDisconnect`, `TestServerCloseEndsStreams`, `TestHubDropsSlowSubscriber`) and Task 3 (`TestServerCloseStopsFeed`).
2. **Missed or stale ingest state.** A small card can finish between two feed ticks, and an EventSource that reconnects must not show stale progress. Expected: an `ingest.finished` event for every job submitted through the API, and a fresh `ingest.current` snapshot as the first event of every connection. Pinned in Task 3 (`TestFeedFinishedViaWatch`, `TestEventsSnapshotIngestCurrent`).
3. **Running exiftool/ffmpeg on owner files.** Risks are paths read as options or protocols (`-x.ARW`, `pipe:`), hung tools, unbounded output, and too many processes at once on the NAS. Expected: `--` before the path for exiftool, a `file:` prefix for ffmpeg, a per-render timeout that kills the process, capped output, and at most `Workers` renders at once. Pinned in Task 5 (`TestGetWorkersBound`, `TestGetTimeout`) and Task 6 (`TestRawPreviewArgs`, `TestVideoShortClipRetriesAtZero`, `TestToolTimeoutKills`, `TestToolOutputCapped`, `TestToolFailureSurfacesStderr`).
4. **Path traversal and odd names.** A sha that isn't 64 lowercase hex, a catalog `rel_path` that escapes the photos root, folder names containing `%`, `_` or ` - `, and a staging folder named like a prefix of another (`2025-05-30` vs `2025-05-30 - Trip`). Pinned in Task 4 (`TestAssetsUnderPrefixTrap`, `TestListGroupsByFolder`), Task 5 (`TestGetBadInput`) and Task 7 (`TestThumbBadSHA`, `TestThumbSourceOutsideRoot`).
5. **Embedded UI fallback.** A fresh checkout must build without npm. An unknown `/api/` path must not get `index.html`. A missing asset must 404 and never come back as HTML. Hashed assets are cached forever, while `index.html` is always revalidated. The build must keep `.placeholder`. Pinned in Task 1 (`TestUI*`) and Task 8 (`TestBuiltUI`).
6. **Concurrent thumbnail requests.** A grid of 80 tiles can ask for the same file twice, a phone can scroll away mid-render, and the container can stop mid-write. Expected: one render per file and size, a render that continues while anyone still waits and stops when nobody does, never a half-written JPEG at the final path, and stale `*.tmp` files removed at startup. Pinned in Task 5 (`TestGetSingleFlight`, `TestGetCallerCancelKeepsSharedRender`, `TestGetLastCallerCancelCancelsRender`, `TestNewRemovesStaleTemps`, `TestCloseCancelsRenders`).
7. **Staging out of sync with the catalog.** Cases: a folder added over SMB (not in the catalog), a catalog folder deleted over SMB, no `Staging/` yet (fresh install). Expected: listed as untracked, flagged "Not found on disk", and an empty list, respectively, with no error. Pinned in Task 4 (`TestListUntrackedAndMissing`, `TestListNoStagingDir`).
8. **Catalog unavailable.** `/healthz` is 503 and the ingest/staging/thumbnail routes aren't mounted. Expected: every page says why (a banner with the problems, and API calls answered 503 with the reason), never a blank page or an `index.html` body parsed as JSON. Pinned in Task 1 (`TestUIAPIUnknown`) and Task 8 (`api.test.js`, HealthBanner).
9. **Phone layout.** The bottom tab bar must not cover the last row of content or the iPhone home indicator. Tap targets are at least 44 px. The back gesture closes the full-screen preview instead of leaving the folder. Thumbnails load lazily, and one SSE connection plus thumbnails stay under the browser's 6-connection limit. These are checked manually in Task 11, Step 6 (no browser test tooling is allowed).

## Decisions

1. **Dependencies.** Svelte 5.59.1, Vite 8.3.8 and `@sveltejs/vite-plugin-svelte` 7.3.1 are the current latest releases (checked with `npm view`, 2026-11-23), and the plugin's peer range `vite ^8, svelte ^5.46.4` fits. All three are exact-pinned `devDependencies`, and `package-lock.json` is committed. **`@sveltejs/vite-plugin-svelte` is approved by the owner ("Sure approved to use the official one").** Go: `imaging v1.6.2` (2019, the latest) pulls a 2019 `golang.org/x/image` whose TIFF decoder has known CVEs, and `.tif` is an imported image format. So `golang.org/x/image` is pinned to **v0.36.0**, the newest whose `go.mod` declares `go 1.24.0` (v0.37.0+ declare 1.25). `go get github.com/disintegration/imaging@v1.6.2 golang.org/x/image@v0.36.0` keeps `go 1.24.0` (verified).
2. **Embedding without npm.** Package `web` (`web/embed.go`) holds `//go:embed all:dist` over a directory that always contains the committed `web/dist/.placeholder`, so `go build`, `go vet` and `go test` work on a fresh checkout. `.gitignore` becomes `/web/dist/*` plus `!/web/dist/.placeholder`. `npm run build` is `node scripts/clean-dist.mjs && vite build`: the script empties `dist/` except `.placeholder`, and Vite runs with `emptyOutDir: false` (Vite's own emptying would delete the placeholder and dirty the tree). Without `index.html`, `/` serves a short "web UI not built" page with the version, as today's placeholder does. Rejected: a `ui` build tag (a binary built without it silently ships no UI), and a Vite plugin that recreates the placeholder (a failed build leaves the tree dirty and the embed broken). The Dockerfile checks `web/dist/index.html` exists before `go build` (Task 12).
3. **Serving.** `MountUI` walks the embedded FS once and computes strong ETags (SHA-256 prefix). Caching follows the Global Constraints. `.webmanifest` is served as `application/manifest+json`, because Go's MIME table lacks it. Dot-files (`.placeholder`) are never served. `New` no longer registers `GET /{$}`: the placeholder page moved into `ui.go`, and `main` always calls `MountUI(web.Dist())`.
4. **SSE design.** One `Hub` per `Server` fans out pre-encoded frames. Each subscriber has a buffer of 16 frames, and a full buffer drops that subscriber (its stream ends and EventSource reconnects 3 s later), so a publisher never blocks. On connect, the handler subscribes first and then writes each registered snapshot, so a client never misses state: an event racing the snapshot can arrive twice, and both are idempotent states. `GET /api/events` is mounted in `New`, so it exists even when the catalog failed. `Server.Close()` ends all streams and background goroutines, and `main` calls it after `mgr.Close` and **before** `httpSrv.Shutdown`. New event types for M4+ are one `Publish` call each; EventSource ignores types a page doesn't listen for.
5. **Ingest feed polls the manager.** `internal/ingest` is not touched. A goroutine started by `MountIngest` ticks every 500 ms (`DefaultFeedInterval`). It publishes `ingest.current` when the encoded body changes, so progress reaches clients at most twice a second, and `ingest.finished` for every watched job whose `Done()` is closed. `POST /api/ingest` registers each job it submits with the feed (`watch`), so a job that starts and finishes between two ticks still produces `ingest.finished`. The alternative was a change hook in `ingest.Progress`, which would touch M2 code and fire per file. The M6 fallback poller must also call the feed's `watch` (or rely on the tick, which only sees jobs alive at tick time). Note: progress updates once per file, so a single 4 GB video shows no byte progress until it finishes (follow-up, Task 13).
6. **What a staging folder is, and its id.** A folder is a `staging_folders` row, and its `id` is the row id. Its files are the assets in state `staged` whose `rel_path` starts with `Staging/<name>/`, found with a range scan on the `assets_path` index (`rel_path >= 'Staging/<name>/' AND rel_path < 'Staging/<name>0'`; `0` is the byte after `/`). The range has no LIKE wildcards to escape, and it stays fast once M5 puts 150k archived rows in `assets`. This follows §8 ("read `Staging/`, join counts and sizes from `assets`"). The listing reads `Staging/` on disk only to find directories with no row (`untracked`, listed by name, not openable) and rows with no directory (warning "Not found on disk in Staging/"). Files are not scanned or hashed on disk: thumbnails, clock warnings and later cull/move need the content hash, which only the catalog has, and hashing on every page load is out of the question. Drift from SMB edits is M4's startup reconcile. `internal/staging` is created now (read-only `List`/`Files`); M4 adds the operations there.
7. **Samples:** up to 5 per folder card (the mockup's strip), spread evenly over the folder in name (= time) order. Files sharing a name stem (a RAW+JPEG pair) count as one shot, shown by the JPEG if there is one (cheapest to render), else the RAW, else the video.
8. **Thumbnail cache.** It is keyed by the resolved asset hash (`AssetBySHA` follows aliases) and the size. Renders are single-flight per file and size. Each render has a waiter count: a caller that leaves decrements it, and the last one to leave cancels the render and removes the entry, so the next request starts afresh. At most `Workers` renders run at once (default `min(NumCPU, 4)`), covering both exec'd tools and Go decoding (a 33 MP JPEG decode needs ~50 MB). The per-render timeout defaults to 60 s, enforced through the context with `exec.CommandContext` and `WaitDelay`. A result is written to `<name>.*.tmp` in the same directory, then renamed. `New` removes stale `*.tmp`. Failures are not cached: the next view retries, and the UI never retries by itself. Responses are cacheable forever because the URL is content-addressed.
9. **RAW previews.** The first call is `exiftool -json -b -n -Orientation -PreviewImage -- <path>`. If `PreviewImage` is empty, a second call is `exiftool -json -b -n -Orientation -JpgFromRaw -- <path>` (two calls, so the large `JpgFromRaw` is only extracted when needed). Binary tags arrive as `"base64:…"` in the JSON. The embedded JPEG is turned upright with the RAW's own `Orientation` (the embedded preview usually carries none). A missing preview is `ErrNoImage` (404). Output is capped at 128 MiB. Verified with exiftool 13.30 in `alpine:3.22`: a TIFF named `.ARW` with `Make=SONY` and IFD0 tags 0x0201/0x0202 reports `PreviewImage`, and a missing tag gives exit status 0 with no field. Follow-up: `-stay_open` to save exiftool's ~0.2 s Perl start-up per file.
10. **Video frames.** The call is `ffmpeg -nostdin -hide_banner -loglevel error -ss 1 -i file:<path> -frames:v 1 -an -sn -dn -f image2pipe -c:v mjpeg -q:v 2 pipe:1`. Empty output means the clip is shorter than 1 s, so the call is retried with `-ss 0`. ffmpeg applies rotation metadata itself, and Go resizes. `file:` keeps a path from being read as a protocol. Verified with ffmpeg 6.1.2 (Alpine) and 7.1 (Windows, drive-letter paths work).
11. **Test inputs.** The committed fixtures have no embedded preview (`DSC00001.ARW` is a bare TIFF) and no frames (`GX010001.MP4` has no `mdat`), and they must stay byte-exact for M1/M2. So thumbnail tests make their own inputs. Exec behaviour (arguments, fallback, timeout, stderr, output cap) is tested with a fake tool: the test binary re-executes itself via `TestMain` when `SD2NAS_FAKE_TOOL` is set. Real-tool tests synthesize an ARW with a 64×32 preview and Orientation 6, and create clips with `ffmpeg -f lavfi -i testsrc=…`.
12. **Warming after an ingest** (§7 step 7; M2 deferred it to M3). A finished ingest with new files queues its id (buffer 16; when full, log and skip). One goroutine then makes the 400 px thumbnail of each new file in turn, using at most one worker slot. Failures are logged at Warn.
13. **Health.** A missing `exiftool`/`ffmpeg` becomes a `/healthz` **warning** ("exiftool not found: RAW thumbnails are unavailable"), so `/healthz` stays 200. A thumbnail cache that can't be created is a **problem** (503), like the catalog. `Server` gains `SetLogger`, `AddWarning` and `Close`.
14. **Frontend routing.** A 30-line History API router (`router.svelte.js`) handles the routes `/` and `/ingest` (ingest page), `/ingest/{id}`, `/staging` and `/staging/{id}`. The default tab is Card ingest, as in the mockup. Same-origin `<a>` clicks are intercepted in `App.svelte`. The full-screen preview pushes a history entry, so the phone's back gesture closes it.
15. **Frontend tests.** The fetch wrapper (`api.js`: the guard header and error mapping), formatting and grouping (`format.js`) and path matching (`paths.js`) are plain ES modules tested with `node --test` (built in, no dependency). Components hold no logic worth a test runner. They are checked by `npm run build` (the Svelte compiler), Go tests of the served build output (`TestBuiltUI`), and the manual phone checklist in Task 11.
16. **PWA.** The pieces are `manifest.webmanifest`, an SVG icon, and PNGs at 192, 512, maskable 512 and apple-touch 180. A build-ignored Go program (`go run web/icons/gen.go`) draws the PNGs, which are committed. There is **no service worker**: offline support is useless for a NAS console and would complicate SSE and caching. Full "install" needs HTTPS (e.g. `tailscale serve`). Over plain `http://tower:8080`, phones still add a home-screen shortcut, iOS honours the `apple-mobile-web-app-*` tags, and Android opens it as a browser tab. That is recorded in PLAN.md.
17. **Request guard.** M3 adds no mutating endpoint; the guard already wraps the whole mux. `api.js` sends `X-SD2NAS: 1` on every non-GET request, ready for M4. The Vite dev proxy does not set `changeOrigin`, so `Host` stays `localhost:5173` and matches `Origin` (the guard passes). A Host-rewriting reverse proxy would still 403 browser POSTs (M7 note, unchanged).
18. **CI.** The `go` job installs `libimage-exiftool-perl` and `ffmpeg` and runs the tests with `SD2NAS_REQUIRE_TOOLS=1`. The new `web` job runs `npm ci`, `npm test`, `npm run build`, then `go test ./web/` with `SD2NAS_REQUIRE_UI=1` (so the built output is really checked). The `docker` job is unchanged (it now builds the node stage).

## File Structure

```
web/
  embed.go                Dist() fs.FS over web/dist                                   (Task 1)
  embed_test.go           TestDistHasPlaceholder (1); TestBuiltUI (8, extended 9)
  icons_test.go           TestIcons: manifest + committed PNG sizes                     (Task 9)
  dist/.placeholder       committed; keeps go:embed valid without a build               (Task 1)
  package.json, package-lock.json, vite.config.js, index.html                          (Task 8)
  scripts/clean-dist.mjs  empty dist/ except .placeholder                               (Task 8)
  icons/gen.go            //go:build ignore — draws the PNG icons                       (Task 9)
  public/                 manifest.webmanifest, icon.svg, icon-192.png, icon-512.png,
                          icon-maskable-512.png, apple-touch-icon.png                   (Task 9)
  src/main.js, src/App.svelte, src/app.css                                             (Task 8)
  src/lib/api.js, format.js, paths.js (+ *.test.js), router.svelte.js, live.svelte.js  (Task 8)
  src/components/Nav.svelte, HealthBanner.svelte                                       (Task 8)
  src/components/LiveIngest.svelte                                                     (Task 10)
  src/components/Thumb.svelte, Lightbox.svelte                                         (Task 11)
  src/pages/Ingest.svelte, IngestDetail.svelte  (stub 8, real 10)
  src/pages/Staging.svelte, Folder.svelte       (stub 8, real 11)
internal/api/
  ui.go                   MountUI, not-built page                                       (Task 1)
  server.go               SetLogger, AddWarning, Close, hub/bg fields                   (Tasks 1, 2, 3, 7)
  events.go               Hub, Event* consts, GET /api/events                           (Task 2)
  ingestfeed.go           ingest feed, currentOf                                        (Task 3)
  ingest.go               uses currentOf; POST watches the job                          (Task 3)
  staging.go              MountStaging                                                  (Task 4)
  thumbs.go               MountThumbs, warm-up                                          (Task 7)
internal/catalog/staging.go   StagingFolderByID, AssetsUnder, ClockWarningsUnder        (Task 4)
internal/staging/staging.go   Service, List, Files, SplitName, KindName                 (Task 4)
internal/thumbs/
  thumbs.go               Cache, Config, Source, Get, Close, ValidSHA                   (Task 5)
  tools.go                exiftool/ffmpeg renderers, MissingTools                       (Task 6)
cmd/sd2nas/main.go        MountUI (1), SetLogger + Close order (2), MountStaging (4), thumbs (7)
deploy/Dockerfile, .github/workflows/ci.yml                                            (Task 12)
.gitignore, .gitattributes                                                              (Task 1)
docs/PLAN.md                                                                            (Task 13)
```

---

### Task 1: Embedded UI serving with SPA fallback and cache headers

Suggested implementer: sonnet.

**Files:**
- Create: `web/embed.go`, `web/embed_test.go`, `web/dist/.placeholder`, `internal/api/ui.go`, `internal/api/ui_test.go`
- Modify: `internal/api/server.go` (drop `GET /{$}` and `handleIndex`), `internal/api/server_test.go` (`TestIndexAndUnknownPaths`), `cmd/sd2nas/main.go`, `.gitignore`, `.gitattributes`

**Interfaces:**
- Consumes (existing; read `internal/api/server.go` and `ingest.go`): `type Server struct{ version string; mux *http.ServeMux; problems []string; … }`, `New(version string, mounts []Mount) *Server` (registers `GET /healthz` and `GET /{$}`), `(s *Server) AddProblem(p string)`, `writeError(w, code int, msg string)` (JSON `{"error":msg}`, `Cache-Control: no-store`), and the test helper `get(t, s, path) *httptest.ResponseRecorder` in `server_test.go` (don't define another `get`).
- Produces:

```go
// package web (web/embed.go)
func Dist() fs.FS // the built UI rooted at web/dist; without a build it holds only .placeholder

// package api (internal/api/ui.go)
// MountUI serves the single-page app in dist. Call it before serving.
func (s *Server) MountUI(dist fs.FS) error
```

- [ ] **Step 1: Placeholder, ignore rules and line endings**

Create `web/dist/.placeholder` with exactly this content (one line, LF):

```
Placeholder so go:embed matches on a fresh checkout. npm run build (in web/) writes the UI here and keeps this file.
```

In `.gitignore`, replace the two lines under `# Web build output and dependencies (from M3)` with:

```
# Web build output (only the placeholder go:embed needs is tracked) and dependencies
/web/dist/*
!/web/dist/.placeholder
node_modules/
```

Append to `.gitattributes`:

```
# Web sources and config stay LF on every OS (npm, Vite and reviews see the same bytes).
web/**/*.svelte text eol=lf
web/**/*.js text eol=lf
web/**/*.mjs text eol=lf
web/**/*.json text eol=lf
web/**/*.css text eol=lf
web/**/*.html text eol=lf
web/**/*.svg text eol=lf
web/**/*.webmanifest text eol=lf
web/dist/.placeholder text eol=lf
*.png binary
```

Check: `git check-ignore -v web/dist/.placeholder` prints nothing (exit status 1), and `git check-ignore -v web/dist/index.html` names the `/web/dist/*` rule.

- [ ] **Step 2: `web/embed.go` and its test**

```go
// Package web holds the built web UI (web/dist, written by `npm run build`
// in this directory) for embedding in the sd2nas binary.
package web

import (
	"embed"
	"io/fs"
)

// dist always contains dist/.placeholder (committed), so this package
// compiles on a fresh checkout before the UI has ever been built.
//
//go:embed all:dist
var dist embed.FS

// Dist returns the built UI rooted at web/dist. Without a build it holds
// only .placeholder and has no index.html.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err) // "dist" is a valid literal path, so fs.Sub cannot fail
	}
	return sub
}
```

`web/embed_test.go` (package `web`): `TestDistHasPlaceholder` checks that `fs.Stat(Dist(), ".placeholder")` succeeds.

- [ ] **Step 3: Write the failing UI tests** in `internal/api/ui_test.go`

Helpers:

```go
// builtUI stands in for a Vite build.
func builtUI() fstest.MapFS {
	return fstest.MapFS{
		"index.html":               {Data: []byte(`<!doctype html><div id="app"></div>`)},
		"assets/index-AbC12345.js": {Data: []byte("console.log(1)")},
		"manifest.webmanifest":     {Data: []byte(`{"name":"SD2NAS"}`)},
		"icon-192.png":             {Data: []byte("png")},
		".placeholder":             {Data: []byte("x")},
	}
}

func uiServer(t *testing.T, files fs.FS) *Server {
	t.Helper()
	s := New("v9", nil)
	if err := s.MountUI(files); err != nil {
		t.Fatal(err)
	}
	return s
}
```

Tests (use the existing `get` and read headers from `rec.Header()`):
- `TestUIServesIndex`: `GET /` → 200, the body equals the `index.html` data, `Content-Type` is `text/html; charset=utf-8`, `Cache-Control` is `no-cache`, and `ETag` is a non-empty quoted string.
- `TestUISPAFallback`: `/ingest`, `/staging/3` and `/ingest/12` each → 200 with the `index.html` body and `Cache-Control: no-cache`.
- `TestUIHashedAssetsImmutable`: `/assets/index-AbC12345.js` → 200, body `console.log(1)`, `Cache-Control: public, max-age=31536000, immutable`, and a `Content-Type` starting with `text/javascript`.
- `TestUIManifestAndIcons`: `/manifest.webmanifest` → `Content-Type: application/manifest+json` and `Cache-Control: no-cache`. `/icon-192.png` → 200 with `Cache-Control: no-cache`.
- `TestUIConditional`: a GET of `/` with `If-None-Match` set to the `ETag` from an earlier GET → 304. The same holds for `/assets/index-AbC12345.js`.
- `TestUIMissingFile` (Review Focus 5): `/assets/nope.js`, `/favicon.ico` and `/.placeholder` → 404, and the body does not contain `id="app"`.
- `TestUIAPIUnknown` (Review Focus 5, 8): `/api/nope` → 404, a JSON error, and `Cache-Control: no-store`. On a fresh `uiServer` after `s.AddProblem("catalog: boom")`, `/api/ingests` → 503 with a JSON error containing `catalog: boom`.
- `TestUINotBuilt`: `uiServer(t, fstest.MapFS{".placeholder": {Data: []byte("x")}})`. `GET /` → 200, the body contains `not built` and `v9`, and `Cache-Control` is `no-store`. `/staging` serves the same page, and `/app.js` gets 404.
- `TestUIHealthzUnaffected`: `GET /healthz` on a `uiServer` still returns the health JSON.
- `TestUIPostIsNotServed`: `POST /` with `X-SD2NAS: 1` → 405.

Rewrite `TestIndexAndUnknownPaths` in `server_test.go` to use `uiServer(t, fstest.MapFS{".placeholder": …})`: `/` → 200 containing `v9`, `/nope.txt` → 404, and `POST /healthz` with `X-SD2NAS: 1` → 405.

- [ ] **Step 4: Run them and confirm they fail**

Run: `go test ./internal/api/ -run 'TestUI|TestIndexAndUnknownPaths'`
Expected: a build failure, `s.MountUI undefined`.

- [ ] **Step 5: Implement `internal/api/ui.go` and remove the old index route**

In `server.go`, delete the line `s.mux.HandleFunc("GET /{$}", s.handleIndex)` and the `handleIndex` method. Its page moves into `notBuilt` below.

```go
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

const (
	cacheImmutable  = "public, max-age=31536000, immutable" // Vite's content-hashed assets/ files
	cacheRevalidate = "no-cache"                            // everything else: revalidate with the ETag
)

type uiHandler struct {
	version  string
	files    fs.FS
	etags    map[string]string // file path → strong ETag, computed once at mount
	index    []byte            // index.html; nil when the UI isn't built
	problems func() []string   // the server's permanent problems (catalog, …)
}

// MountUI serves the single-page app in dist (web.Dist()). Files are served
// by path, and every other extension-less GET outside /api/ gets index.html,
// so client-side routes such as /staging/3 survive a reload. Without
// index.html, "/" shows a page saying the UI isn't built. Call it before
// serving.
func (s *Server) MountUI(dist fs.FS) error {
	h := &uiHandler{version: s.version, files: dist, etags: map[string]string{},
		problems: func() []string { return s.problems }}
	err := fs.WalkDir(dist, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		b, err := fs.ReadFile(dist, p)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(b)
		h.etags[p] = `"` + hex.EncodeToString(sum[:12]) + `"`
		if p == "index.html" {
			h.index = b
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read embedded web UI: %w", err)
	}
	s.mux.Handle("GET /", h)
	return nil
}

func (h *uiHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/") // "/" → ""
	if p == "api" || strings.HasPrefix(p, "api/") {
		if probs := h.problems(); len(probs) > 0 {
			writeError(w, http.StatusServiceUnavailable, "not available while SD2NAS is degraded: "+strings.Join(probs, "; "))
			return
		}
		writeError(w, http.StatusNotFound, "no such API endpoint: "+r.URL.Path)
		return
	}
	if etag, ok := h.etags[p]; ok && !hidden(p) {
		h.serveFile(w, r, p, etag)
		return
	}
	if path.Ext(p) != "" { // a missing file, not a client-side route
		http.NotFound(w, r)
		return
	}
	if h.index == nil {
		h.notBuilt(w)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", cacheRevalidate)
	w.Header().Set("ETag", h.etags["index.html"])
	http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(h.index))
}

// hidden reports whether any element of p starts with a dot (.placeholder).
func hidden(p string) bool {
	for _, el := range strings.Split(p, "/") {
		if strings.HasPrefix(el, ".") {
			return true
		}
	}
	return false
}

func (h *uiHandler) serveFile(w http.ResponseWriter, r *http.Request, p, etag string) {
	f, err := h.files.Open(p)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "open "+p+": "+err.Error())
		return
	}
	defer f.Close() // an embedded, read-only, in-memory file: Close cannot fail
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		writeError(w, http.StatusInternalServerError, p+" is not seekable")
		return
	}
	cache := cacheRevalidate
	if strings.HasPrefix(p, "assets/") {
		cache = cacheImmutable
	}
	w.Header().Set("Cache-Control", cache)
	w.Header().Set("ETag", etag)
	if strings.HasSuffix(p, ".webmanifest") {
		w.Header().Set("Content-Type", "application/manifest+json") // not in Go's MIME table
	}
	http.ServeContent(w, r, path.Base(p), time.Time{}, rs)
}

func (h *uiHandler) notBuilt(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The write fails only when the client has gone.
	_, _ = fmt.Fprintf(w, `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>SD2NAS</title>
<style>body{font-family:system-ui,sans-serif;background:#141c27;color:#e6edf6;margin:0;padding:24px 16px}
a{color:#4f9cf9}code{background:#223047;padding:2px 6px;border-radius:4px}</style>
</head><body>
<h1>SD2NAS</h1>
<p>Version <code>%s</code>. The web UI is not built into this binary: run <code>npm run build</code> in <code>web/</code>, then rebuild sd2nas.</p>
<p><a href="/healthz">/healthz</a> shows the time zone, user and mount checks.</p>
</body></html>
`, h.version)
}
```

- [ ] **Step 6: Wire it into `cmd/sd2nas/main.go`**

Import `github.com/aspiziri/SD2NAS/web`, and right after `srv := api.New(...)` add:

```go
	if err := srv.MountUI(web.Dist()); err != nil {
		return err
	}
```

- [ ] **Step 7: Run the tests and the full set**

Run `go test ./internal/api/ ./web/` → PASS. Then run `gofmt -l .` (prints nothing), `go vet ./...`, `go test ./...` and the Linux command from the Global Constraints → PASS. `go build ./cmd/sd2nas && rm -f sd2nas sd2nas.exe` succeeds.

- [ ] **Step 8: Commit**

```bash
git add web/embed.go web/embed_test.go web/dist/.placeholder internal/api/ui.go internal/api/ui_test.go internal/api/server.go internal/api/server_test.go cmd/sd2nas/main.go .gitignore .gitattributes
git commit -m "Serve the embedded web UI with SPA fallback and cache headers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: SSE hub, `GET /api/events` and `Server.Close`

Suggested implementer: sonnet (reviewer: opus, because of the concurrency).

**Files:**
- Create: `internal/api/events.go`, `internal/api/events_test.go`
- Modify: `internal/api/server.go`, `cmd/sd2nas/main.go`

**Interfaces:**
- Consumes: `Server` and `New` as Task 1 left them, and `writeError`. The test helper `pollUntil(t, what string, cond func() bool)` exists in `internal/api/ingest_test.go`.
- Produces:

```go
const (
	EventIngestCurrent  = "ingest.current"
	EventIngestFinished = "ingest.finished"
)
const DefaultKeepalive = 25 * time.Second

type Hub struct{ /* unexported */ }
func NewHub() *Hub
func (h *Hub) Publish(typ string, data any) error                  // never blocks
func (h *Hub) AddSnapshot(fn func() (typ string, data any, err error))
func (h *Hub) Close()
func (h *Hub) Len() int
func (h *Hub) subscribe() (<-chan []byte, func())                   // unexported; used by the handler and tests

// Server additions
func (s *Server) SetLogger(l *slog.Logger) // call before any Mount method
func (s *Server) Close()                   // ends streams and background work; idempotent
// unexported fields later tasks use:
//   s.log *slog.Logger; s.hub *Hub; s.keepalive time.Duration
//   s.bgCtx context.Context; s.bg sync.WaitGroup; s.closers []func()
// test helpers (events_test.go), used by Task 3:
//   sseFrames(t, url) (<-chan string, context.CancelFunc); nextFrame(t, frames) string; eventsServer(t, s) string
```

- [ ] **Step 1: Write the failing tests** (`internal/api/events_test.go`)

Helpers:

```go
// sseFrames connects to url and delivers each frame: its lines joined by
// "\n", without the blank line that ends it. The channel closes when the
// stream ends; cancel stops the client.
func sseFrames(t *testing.T, url string) (<-chan string, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		cancel()
		t.Fatalf("status %d", res.StatusCode)
	}
	frames := make(chan string, 64)
	go func() {
		defer close(frames)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		var lines []string
		for sc.Scan() {
			if sc.Text() == "" {
				frames <- strings.Join(lines, "\n")
				lines = nil
				continue
			}
			lines = append(lines, sc.Text())
		}
	}()
	return frames, cancel
}

func nextFrame(t *testing.T, frames <-chan string) string {
	t.Helper()
	select {
	case f, ok := <-frames:
		if !ok {
			t.Fatal("stream ended")
		}
		return f
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for an event")
	}
	return ""
}

// eventsServer serves s over real HTTP. s.Close runs before the test
// server closes (cleanups run in reverse), or ts.Close would wait for the
// open streams.
func eventsServer(t *testing.T, s *Server) string {
	t.Helper()
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	t.Cleanup(s.Close)
	return ts.URL
}
```

Tests:
- `TestHubPublishFanOut`: with two `subscribe()` channels, `Publish("x", map[string]int{"a": 1})` → both receive exactly `"event: x\ndata: {\"a\":1}\n\n"`.
- `TestHubDropsSlowSubscriber` (Review Focus 1): subscriber A never reads, while subscriber B is drained by a goroutine. Publish 17 events. A's channel holds 16 frames and is then closed (drain it: 16 values, then `!ok`), B received all 17, and `Len()` is 1 afterwards. `Publish` never blocked, or the test would hang.
- `TestHubClose`: after `Close()`, the existing subscriber's channel is closed, `Len()` is 0, `Publish` returns nil, a new `subscribe()` returns an already closed channel, and a second `Close()` does not panic.
- `TestHubRejectsBadEvents`: `Publish("a\nb", 1)`, `Publish("", 1)` and `Publish("x", func() {})` (not JSON-encodable) all return errors.
- `TestEventsStream`: `s := New("test", nil)` with `s.hub.AddSnapshot(func() (string, any, error) { return "snap", map[string]int{"n": 1}, nil })`, then `sseFrames(eventsServer(t, s)+"/api/events")`. In a separate sub-check, a plain `http.Get` (body closed right away) has the headers `Content-Type: text/event-stream`, `Cache-Control: no-store` and `X-Accel-Buffering: no`. The frames are `retry: 3000` and then `event: snap\ndata: {"n":1}`, in order. Then `s.hub.Publish("x", 2)` makes the next frame `event: x\ndata: 2`.
- `TestEventsKeepalive`: set `s.keepalive = 20 * time.Millisecond` before serving. After the retry frame, a frame equal to `: keepalive` arrives.
- `TestEventsSnapshotError`: call `s.SetLogger(slog.New(slog.NewTextHandler(buf, nil)))`, where `buf` is a tiny mutex-guarded `syncBuffer` (`Write` and `String`). One snapshot returns `errors.New("boom")` and another returns `("ok", 1, nil)`. The stream sends `retry: 3000` and then `event: ok\ndata: 1`, skipping the bad one, and the log contains `boom`.
- `TestEventsClientDisconnect` (Review Focus 1): connect, read the retry frame, `cancel()`, then `pollUntil(t, "the handler to unsubscribe", func() bool { return s.hub.Len() == 0 })`.
- `TestServerCloseEndsStreams` (Review Focus 1): connect, read the retry frame, `s.Close()`. The frames channel closes within 10 s, `s.hub.Len() == 0`, and a second `s.Close()` does nothing.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/api/ -run 'TestHub|TestEvents|TestServerClose'`
Expected: a build failure (`s.hub` undefined).

- [ ] **Step 3: Implement `internal/api/events.go`**

```go
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Event types sent on /api/events. Later milestones add staging.changed,
// promote.progress and indexer.progress; EventSource clients ignore the
// types they don't listen for.
const (
	EventIngestCurrent  = "ingest.current"
	EventIngestFinished = "ingest.finished"
)

// DefaultKeepalive is how often an idle stream gets a comment line, so
// proxies and phones keep it open and a dead client is noticed.
const DefaultKeepalive = 25 * time.Second

const (
	subscriberBuffer = 16   // frames a slow client may lag before it is dropped
	sseRetryMillis   = 3000 // EventSource reconnect delay
)

// Hub fans server-sent events out to every connected /api/events client.
type Hub struct {
	mu        sync.Mutex
	subs      map[chan []byte]struct{}
	closed    bool
	snapshots []func() (string, any, error)
}

func NewHub() *Hub { return &Hub{subs: map[chan []byte]struct{}{}} }

// frame encodes one event. json.Marshal escapes control characters, so
// the data is always a single line.
func frame(typ string, data any) ([]byte, error) {
	if typ == "" || strings.ContainsAny(typ, "\r\n") {
		return nil, fmt.Errorf("bad event type %q", typ)
	}
	b, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("encode %s event: %w", typ, err)
	}
	return []byte("event: " + typ + "\ndata: " + string(b) + "\n\n"), nil
}

// Publish sends one event to every subscriber without blocking. A
// subscriber whose buffer is full is dropped: its stream ends, and the
// browser reconnects and gets fresh snapshots.
func (h *Hub) Publish(typ string, data any) error {
	msg, err := frame(typ, data)
	if err != nil {
		return err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- msg:
		default:
			delete(h.subs, ch)
			close(ch)
		}
	}
	return nil
}

// AddSnapshot registers current state that every new subscriber receives
// right after connecting. fn must not block.
func (h *Hub) AddSnapshot(fn func() (typ string, data any, err error)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.snapshots = append(h.snapshots, fn)
}

// subscribe returns a channel of frames, closed when the subscriber is
// dropped or the hub closes, and a func that unsubscribes (idempotent).
func (h *Hub) subscribe() (<-chan []byte, func()) {
	ch := make(chan []byte, subscriberBuffer)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return ch, func() {}
	}
	h.subs[ch] = struct{}{}
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, ok := h.subs[ch]; ok {
			delete(h.subs, ch)
			close(ch)
		}
	}
}

// snapshotFrames encodes every registered snapshot. Failing ones are
// skipped and reported together.
func (h *Hub) snapshotFrames() ([][]byte, error) {
	h.mu.Lock()
	fns := append([]func() (string, any, error)(nil), h.snapshots...)
	h.mu.Unlock()
	var out [][]byte
	var errs []error
	for _, fn := range fns {
		typ, data, err := fn()
		if err == nil {
			var b []byte
			if b, err = frame(typ, data); err == nil {
				out = append(out, b)
				continue
			}
		}
		errs = append(errs, err)
	}
	return out, errors.Join(errs...)
}

// Close ends every stream; later subscribers get a closed channel.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for ch := range h.subs {
		delete(h.subs, ch)
		close(ch)
	}
}

// Len is the number of connected subscribers.
func (h *Hub) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}

// handleEvents streams the hub to one client until the client leaves,
// falls too far behind, or the server closes. A failed write or flush
// means the client has gone, so the stream just ends.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	ch, unsubscribe := s.hub.subscribe()
	defer unsubscribe()
	rc := http.NewResponseController(w)
	hdr := w.Header()
	hdr.Set("Content-Type", "text/event-stream")
	hdr.Set("Cache-Control", "no-store")
	hdr.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	if !write([]byte(fmt.Sprintf("retry: %d\n\n", sseRetryMillis))) {
		return
	}
	frames, err := s.hub.snapshotFrames()
	if err != nil {
		s.log.Error("event snapshot failed", "err", err)
	}
	for _, f := range frames {
		if !write(f) {
			return
		}
	}
	keepalive := time.NewTicker(s.keepalive)
	defer keepalive.Stop()
	for {
		select {
		case f, ok := <-ch:
			if !ok || !write(f) {
				return
			}
		case <-keepalive.C:
			if !write([]byte(": keepalive\n\n")) {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}
```

- [ ] **Step 4: Add the server fields, `SetLogger`, `Close` and the route**

In `server.go`, add these fields to `Server`, initialize them in `New`, and register the route there, before `s.handler = guard(s.mux)`. Registering it in `New` means `/api/events` exists even when the catalog fails.

```go
	log       *slog.Logger       // SetLogger; slog.Default() until then
	hub       *Hub
	keepalive time.Duration      // DefaultKeepalive; tests shorten it
	bgCtx     context.Context    // cancelled by Close; background goroutines stop on it
	bgCancel  context.CancelFunc
	bg        sync.WaitGroup     // background goroutines (ingest feed, thumbnail warm-up)
	closers   []func()           // run by Close once bg has stopped (e.g. the thumbnail cache)
	closeOnce sync.Once
```

```go
	// in New, after the mux is created:
	s.log = slog.Default()
	s.hub = NewHub()
	s.keepalive = DefaultKeepalive
	s.bgCtx, s.bgCancel = context.WithCancel(context.Background())
	s.mux.HandleFunc("GET /api/events", s.handleEvents)
```

```go
// SetLogger sets the logger for errors the server can only log (event
// snapshots, thumbnail failures). Call it before any Mount method.
func (s *Server) SetLogger(l *slog.Logger) { s.log = l }

// Close ends every /api/events stream, stops the server's background work
// and runs the registered closers (the thumbnail cache). Call it before
// http.Server.Shutdown, which would otherwise wait for the open streams.
// Calling it again does nothing.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		s.bgCancel()
		s.bg.Wait()
		s.hub.Close()
		for _, c := range s.closers {
			c()
		}
	})
}
```

- [ ] **Step 5: In `main.go`, set the logger and fix the shutdown order**

Right after `srv := api.New(...)` (before `MountUI`), add `srv.SetLogger(log)`. In the `case err := <-errc:` branch, call `srv.Close()` before `return err` (after the manager block). In the shutdown path, replace the final return with:

```go
	// Ends /api/events streams and background work, which Shutdown would
	// otherwise wait for until its deadline.
	srv.Close()
	return errors.Join(mgrErr, wrap("stopping http server", httpSrv.Shutdown(shutdownCtx)))
```

- [ ] **Step 6: Run the tests**

Run `go test ./internal/api/ -run 'TestHub|TestEvents|TestServerClose' -count=3`, then the full Windows set, the Linux command and the race command from the Global Constraints → PASS with no race reports.

- [ ] **Step 7: Commit**

```bash
git add internal/api/events.go internal/api/events_test.go internal/api/server.go cmd/sd2nas/main.go
git commit -m "Add the server-sent event hub at /api/events and close streams before shutdown

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Ingest events feed (`ingest.current`, `ingest.finished`)

Suggested implementer: sonnet (reviewer: opus, because of the concurrency).

**Files:**
- Create: `internal/api/ingestfeed.go`, `internal/api/ingestfeed_test.go`
- Modify: `internal/api/ingest.go` (use `currentOf`, have POST watch its job, start the feed in `MountIngest`), `internal/api/server.go` (feed interval, finished hooks), `internal/api/ingest_test.go` (`newIngestServer` adds `t.Cleanup(s.Close)` after mounting)

**Interfaces:**
- Consumes from Task 2: `Hub.Publish`, `Hub.AddSnapshot`, `Hub.subscribe`, `EventIngestCurrent`, `EventIngestFinished`, `s.log`, `s.hub`, `s.bgCtx`, `s.bg`, `Server.Close`, and the test helpers `sseFrames`, `nextFrame`, `eventsServer`. Existing in `ingest.go`: `ingestView`, `viewOf(row catalog.Ingest, job *ingest.Job) ingestView`, `ingestHandlers{m, db, maxWait}`, `MountIngest`. From `internal/ingest`: `Manager.Current() (running *Job, queued []*Job)` and `(*Job).ID/Done/Result`. Test helpers `newIngestServer`, `post`, `cardBody`, `plenty`, `decodeInto`, `pollUntil`.
- Produces:

```go
const DefaultFeedInterval = 500 * time.Millisecond

type currentBody struct {
	Running *ingestView  `json:"running"`
	Queued  []ingestView `json:"queued"` // never null
}
func currentOf(running *ingest.Job, queued []*ingest.Job) currentBody

type ingestFeed struct{ /* m, hub, log, hooks, mu, watched, last */ }
func (f *ingestFeed) watch(j *ingest.Job)
func (f *ingestFeed) tick()
func (f *ingestFeed) snapshot() (string, any, error)

// Server additions (unexported)
//   s.feedInterval time.Duration (DefaultFeedInterval; set it before MountIngest)
//   s.feed *ingestFeed (set by MountIngest)
func (s *Server) addFinishedHook(fn func(catalog.Ingest)) // fn must not block; it runs on the feed goroutine
func (s *Server) finishedHooks() []func(catalog.Ingest)
```

- [ ] **Step 1: Write the failing tests** (`internal/api/ingestfeed_test.go`)

```go
// newFeedServer is newIngestServer with the feed's ticker effectively off,
// so tests drive s.feed.tick() themselves.
func newFeedServer(t *testing.T, free func(string) (uint64, error)) (s *Server, card string)
```

It copies `newIngestServer`'s setup, except that it sets `s.feedInterval = time.Hour` before `s.MountIngest(m, db, time.Minute)` and registers `t.Cleanup(s.Close)` last.

Frame helpers: `takeFrames(ch <-chan []byte) []string` drains whatever is buffered without blocking. `tick` publishes synchronously, so after it returns its frames are already in the channel. `parseFrame(f string) (typ string, data json.RawMessage)` parses one frame.

- `TestFeedFinishedViaWatch` (Review Focus 2): `ch, unsub := s.hub.subscribe()`. `post(t, s, "?wait=1", cardBody(card))` → 200, so the job has finished and the feed never saw it running. `s.feed.tick()`. Exactly two frames are buffered: `ingest.current` with data `{"running":null,"queued":[]}`, then `ingest.finished` whose data has `id` equal to the POST's id, `status` `done`, `counts.new` 2, and no `progress` key.
- `TestFeedDedupes`: after a first `tick()` publishes the initial `ingest.current`, a second `tick()` with nothing changed publishes nothing (`len(ch) == 0`).
- `TestFeedProgressAndFinish`: FreeSpace blocks on a `release` channel, as in `TestIngestWaitTimeout`. Close it through a `sync.Once` so the test's own close and the deferred close don't clash. `post(t, s, "", body)` → 202. `pollUntil` until `GET /api/ingest/current` shows `running.progress.phase == "classifying"`. `tick()` → an `ingest.current` frame with `running.status == "running"` and `running.progress.phase == "classifying"`. Close `release`, then `pollUntil` until `GET /api/ingests/{id}` has status `done`. `tick()` → the frames `ingest.current` (running null) and `ingest.finished` (that id).
- `TestFeedFinishedHook`: `s.addFinishedHook(func(in catalog.Ingest) { got <- in.ID })` with `got` buffered at 4. POST `?wait=1`, `tick()`, and `got` holds the id.
- `TestEventsSnapshotIngestCurrent` (Review Focus 2): with `newIngestServer` (real ticker), `sseFrames(eventsServer(t, s)+"/api/events")` gives `retry: 3000`, then `event: ingest.current\ndata: {"running":null,"queued":[]}`.
- `TestFeedLive`: `newIngestServer` and `ch, _ := s.hub.subscribe()`. POST `?wait=1`. Within 10 s, an `ingest.finished` frame for that id arrives on `ch`, driven by the real 500 ms ticker.
- `TestServerCloseStopsFeed`: with `newIngestServer`, run `done := make(chan struct{}); go func() { s.Close(); close(done) }()`. `done` closes within 10 s.
- The existing ingest tests pass unchanged, so the `GET /api/ingest/current` body is byte-for-byte the same.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/api/ -run 'TestFeed|TestEventsSnapshotIngestCurrent|TestServerCloseStopsFeed'`
Expected: a build failure (`s.feed` undefined).

- [ ] **Step 3: Implement `internal/api/ingestfeed.go`**

```go
package api

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/ingest"
)

// DefaultFeedInterval is how often the ingest feed looks at the manager;
// progress reaches /api/events clients at most this often.
const DefaultFeedInterval = 500 * time.Millisecond

// currentBody is the body of GET /api/ingest/current and the data of the
// ingest.current event.
type currentBody struct {
	Running *ingestView  `json:"running"`
	Queued  []ingestView `json:"queued"`
}

func currentOf(running *ingest.Job, queued []*ingest.Job) currentBody {
	out := currentBody{Queued: []ingestView{}}
	if running != nil {
		v := viewOf(running.Result(), running)
		out.Running = &v
	}
	for _, j := range queued {
		out.Queued = append(out.Queued, viewOf(j.Result(), j))
	}
	return out
}

// ingestFeed turns the manager's state into events. It polls instead of
// hooking into internal/ingest (Decision 5).
type ingestFeed struct {
	m       *ingest.Manager
	hub     *Hub
	log     *slog.Logger
	hooks   func() []func(catalog.Ingest)
	mu      sync.Mutex // serialises tick and watch
	watched map[int64]*ingest.Job
	last    []byte // the last ingest.current body published
}

// watch makes sure j gets an ingest.finished event, even if it finishes
// before a tick sees it running.
func (f *ingestFeed) watch(j *ingest.Job) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.watched[j.ID()] = j
}

func (f *ingestFeed) snapshot() (string, any, error) {
	running, queued := f.m.Current()
	return EventIngestCurrent, currentOf(running, queued), nil
}

// tick publishes ingest.current if it changed, then ingest.finished (and
// runs the finished hooks) for every watched job that has finished, oldest
// first.
func (f *ingestFeed) tick() {
	f.mu.Lock()
	defer f.mu.Unlock()
	running, queued := f.m.Current()
	for _, j := range queued {
		f.watched[j.ID()] = j
	}
	if running != nil {
		f.watched[running.ID()] = running
	}
	var finished []*ingest.Job
	for id, j := range f.watched {
		select {
		case <-j.Done():
			finished = append(finished, j)
			delete(f.watched, id)
		default:
		}
	}
	slices.SortFunc(finished, func(a, b *ingest.Job) int { return cmp.Compare(a.ID(), b.ID()) })

	b, err := json.Marshal(currentOf(running, queued))
	if err != nil {
		f.log.Error("encode ingest.current", "err", err)
	} else if !bytes.Equal(b, f.last) {
		f.last = b
		if err := f.hub.Publish(EventIngestCurrent, json.RawMessage(b)); err != nil {
			f.log.Error("publish ingest.current", "err", err)
		}
	}
	for _, j := range finished {
		row := j.Result()
		if err := f.hub.Publish(EventIngestFinished, viewOf(row, nil)); err != nil {
			f.log.Error("publish ingest.finished", "ingest", row.ID, "err", err)
		}
		for _, fn := range f.hooks() {
			fn(row)
		}
	}
}

func (f *ingestFeed) run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			f.tick()
		}
	}
}
```

- [ ] **Step 4: Add the server hooks and change `MountIngest`**

In `server.go`, add the fields `feedInterval time.Duration` (set to `DefaultFeedInterval` in `New`), `feed *ingestFeed`, `hookMu sync.Mutex` and `finished []func(catalog.Ingest)`, plus:

```go
// addFinishedHook registers fn to run, on the feed goroutine, for every
// ingest that finishes. fn must not block.
func (s *Server) addFinishedHook(fn func(catalog.Ingest)) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	s.finished = append(s.finished, fn)
}

func (s *Server) finishedHooks() []func(catalog.Ingest) {
	s.hookMu.Lock()
	defer s.hookMu.Unlock()
	return append([]func(catalog.Ingest)(nil), s.finished...)
}
```

In `ingest.go`:
- `ingestHandlers` gains a `feed *ingestFeed` field.
- `MountIngest` creates the feed (`&ingestFeed{m: m, hub: s.hub, log: s.log, hooks: s.finishedHooks, watched: map[int64]*ingest.Job{}}`), sets `s.feed`, calls `s.hub.AddSnapshot(f.snapshot)`, registers the routes as before, and then starts the feed:

```go
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		f.run(s.bgCtx, s.feedInterval)
	}()
```

- In `post`, right after a successful `Submit` (before the `!wait` check), call `h.feed.watch(job)`.
- `current` becomes `running, queued := h.m.Current(); writeJSON(w, http.StatusOK, currentOf(running, queued))`. Delete the inline struct.

In `ingest_test.go`'s `newIngestServer`, add `t.Cleanup(s.Close)` after `s.MountIngest(...)`. Cleanups run in reverse, so it runs before the manager and catalog cleanups.

- [ ] **Step 5: Run the tests**

Run `go test ./internal/api/ -count=2`, then the full Windows set, the Linux command and the race command → PASS with no races.

- [ ] **Step 6: Commit**

```bash
git add internal/api/ingestfeed.go internal/api/ingestfeed_test.go internal/api/ingest.go internal/api/ingest_test.go internal/api/server.go
git commit -m "Publish ingest progress and finished imports on /api/events

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Staging read model and `GET /api/staging[/{id}/files]`

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/catalog/staging.go`, `internal/catalog/staging_test.go`, `cmd/sd2nas/main.go`
- Create: `internal/staging/staging.go`, `internal/staging/staging_test.go`, `internal/staging/dir_test.go`, `internal/api/staging.go`, `internal/api/staging_test.go`, `internal/api/full_test.go`

**Interfaces:**
- Consumes the existing `internal/catalog` (read `assets.go`, `staging.go`, `ingests.go`): `Asset{SHA256, Size, Kind, CaptureTime, TZOffset, State, RelPath, …}`, `assetCols`, `scanAsset`, `scanner`, `StagingFolder{ID, Name, Dates, CreatedAt}`, `stagingCols`, `scanStagingFolder`, `StagingFolders(ctx)` (ordered by name), `StagingFolderForDate`, `InsertAsset`, `CreateIngest(ctx, cardPath, label, uuid, at)`, `AddClockWarnings(ctx, ingestID, []ClockWarning)`, `ClockWarning{CardPath, SHA256, Kind, Detail}`, `ClockFuture`, `ClockWarnings(ctx, ingestID)` (copy its scan code), `ErrNotFound`, `CaptureLayout`, `StateStaged`, `StateArchived`, and the test helpers `openTest(t) *DB` and `hexN(n byte) string`. Also `media.KindImage`/`KindRaw`/`KindVideo` and `ingest.StagingDir == "Staging"`. In `internal/api`: `writeJSON`, `writeError`, `positiveInt`, `s.log`, and the setup pattern of `newIngestServer` (Task 3 state).
- Produces:

```go
// internal/catalog
func (d *DB) StagingFolderByID(ctx context.Context, id int64) (StagingFolder, error) // ErrNotFound
func (d *DB) AssetsUnder(ctx context.Context, prefix string, state AssetState) ([]Asset, error)
func (d *DB) ClockWarningsUnder(ctx context.Context, prefix string, state AssetState) ([]ClockWarning, error)

// internal/staging
const Dir = "Staging"
const MaxSamples = 5
var ErrBadKind = errors.New("staging: kind must be jpg, raw or video")
type Service struct{ /* db, photosRoot */ }
func New(db *catalog.DB, photosRoot string) *Service
type KindCounts struct{ JPG, Raw, Video int }       // json jpg, raw, video
type Sample struct{ SHA256, Kind string }            // json sha256, kind
type Folder struct{ … }                              // fields in Step 4
type Listing struct{ Folders []Folder; Untracked []string }
type File struct{ … }
type FolderFiles struct{ Folder Folder; Files []File }
func (s *Service) List(ctx context.Context) (Listing, error)
func (s *Service) Files(ctx context.Context, id int64, kind string) (FolderFiles, error)
func SplitName(name string) (date, title string)
func KindName(k media.Kind) string

// internal/api
func (s *Server) MountStaging(svc *staging.Service)
// test helper (full_test.go), extended in Task 7:
type fullServer struct{ s *Server; card, photos, config string; db *catalog.DB }
func newFullServer(t *testing.T) fullServer
```

- [ ] **Step 1: Catalog: write the failing tests** (append to `internal/catalog/staging_test.go`)

Insert assets with `InsertAsset`, using `UpdatedAt: time.Now()`, `Kind: media.KindImage` and `State: StateStaged` unless the table says otherwise, and sha `hexN(i)`:

| i | rel_path | state |
|---|---|---|
| 1 | `Staging/2025-05-30/2025-05-30 10.00.00.JPG` | staged |
| 2 | `Staging/2025-05-30/Capture/2025-05-30 10.00.00.ARW` (kind raw) | staged |
| 3 | `Staging/2025-05-30 - Trip/a.JPG` | staged |
| 4 | `Staging/50%_off/b.JPG` | staged |
| 5 | `Archive/2025-05-30 - Old/c.JPG` | archived |
| 6 | `Staging/2025-05-30/d.JPG` | archived (state no longer staged) |

- `TestAssetsUnderPrefixTrap` (Review Focus 4):
  - `AssetsUnder(ctx, "Staging/2025-05-30/", StateStaged)` gives the shas `[hexN(2), hexN(1)]` in that order, because rel_path order puts `…/Capture/…` before `…/2025…`.
  - `AssetsUnder(ctx, "Staging/50%_off/", StateStaged)` gives only `hexN(4)`.
  - `AssetsUnder(ctx, "Staging/", StateStaged)` gives 1, 2, 3 and 4.
  - `AssetsUnder(ctx, "Staging", …)` and `AssetsUnder(ctx, "/", …)` return an error.
- `TestStagingFolderByID`: a folder from `StagingFolderForDate(ctx, "2025-05-30", now)` round-trips by its id. Id 999 → `errors.Is(err, ErrNotFound)`.
- `TestClockWarningsUnder`: `in, _ := CreateIngest(ctx, "/cards/X", "X", "", now)`, then `AddClockWarnings(ctx, in.ID, []ClockWarning{{CardPath: "DCIM/a.JPG", SHA256: hexN(1), Kind: ClockFuture, Detail: "in the future"}, {CardPath: "DCIM/c.JPG", SHA256: hexN(5), Kind: ClockFuture, Detail: "x"}})`. `ClockWarningsUnder(ctx, "Staging/2025-05-30/", StateStaged)` gives exactly one warning, with `SHA256 == hexN(1)` and `Detail == "in the future"`.

- [ ] **Step 2: Catalog: implement** (in `internal/catalog/staging.go`)

```go
// prefixRange is [prefix, prefix with its final "/" replaced by "0").
// "0" is the byte after "/", so the range holds exactly the paths below
// prefix, as a range scan on the assets_path index with no LIKE wildcards.
func prefixRange(prefix string) (lo, hi string, err error) {
	if len(prefix) < 2 || !strings.HasSuffix(prefix, "/") {
		return "", "", fmt.Errorf("catalog: path prefix %q must be a folder ending in /", prefix)
	}
	return prefix, prefix[:len(prefix)-1] + "0", nil
}
```

- `AssetsUnder`: `SELECT ` + assetCols + ` FROM assets WHERE rel_path >= ? AND rel_path < ? AND state = ? ORDER BY rel_path`. Read all the rows, close them, then return (the same loop shape as `AssetsByFingerprint`). Wrap errors as `"assets under %s: %w"`.
- `ClockWarningsUnder`: `SELECT w.card_path, w.sha256, w.kind, w.detail FROM clock_warnings w JOIN assets a ON a.sha256 = w.sha256 WHERE a.state = ? AND a.rel_path >= ? AND a.rel_path < ? ORDER BY a.rel_path, w.kind`. Scan the rows as `ClockWarnings` does, with the sha BLOB turned into lowercase hex.
- `StagingFolderByID`: `SELECT ` + stagingCols + ` FROM staging_folders WHERE id = ?`. `sql.ErrNoRows` → `fmt.Errorf("staging folder %d: %w", id, ErrNotFound)`.

Run: `go test ./internal/catalog/` → PASS.

- [ ] **Step 3: `internal/staging`: write the failing tests**

`dir_test.go` (package `staging_test`) has `TestDirMatchesIngest`, which checks `staging.Dir == ingest.StagingDir`.

`staging_test.go` (package `staging`) builds its fixture with `setup(t) (*Service, *catalog.DB, string /*photos*/)`. It creates the photos and config temp dirs, opens the catalog with a close cleanup, and uses a fixed `now`. Shas are `fmt.Sprintf("%064x", n)`, and `CaptureTime` values are built with `time.Date(…, time.Local)`.
- Folder rows: `f1 := StagingFolderForDate(ctx, "2025-05-30", now)` and `f2 := StagingFolderForDate(ctx, "2025-06-01", now)`.
- Assets (staged unless noted):
  1. `Staging/2025-05-30/2025-05-30 10.00.00.JPG`: image, size 100, captured 2025-05-30 10:00:00.
  2. `Staging/2025-05-30/Capture/2025-05-30 10.00.00.ARW`: raw, size 1000, captured 10:00:00.
  3. `Staging/2025-05-30/2025-05-30 11.00.00.JPG`: image, size 100, captured 11:00:00.
  4. `Staging/2025-05-30/Videos/2025-05-30 12.00.00 - 3840w - 29.970 - 12sec.MP4`: video, size 5000, captured 12:00:00, TZOffset `+02:00`.
  5. `Archive/2025-05-30 - Old/x.JPG`: archived.
  6. `Staging/2025-05-30 - Trip/a.JPG`: staged, in a directory that has no catalog row.
- One clock warning, on asset 3, with detail `capture time is after the NAS clock`.
- On disk: the directories `<photos>/Staging/2025-05-30/`, `<photos>/Staging/2025-05-30 - Trip/` and `<photos>/Staging/.hidden/`, and the file `<photos>/Staging/readme.txt`. There is no `2025-06-01` directory.

Tests:
- `TestListGroupsByFolder` (Review Focus 4): `List` returns two folders, `2025-05-30` then `2025-06-01`. f1 has:
  - `ID` f1.ID, `Date "2025-05-30"`, `Title ""`, `Dates ["2025-05-30"]`
  - `Files 4`, `Bytes 6200`, `Counts {JPG:2, Raw:1, Video:1}`
  - `FirstCapture "2025-05-30T10:00:00"`, `LastCapture "2025-05-30T12:00:00"`
  - `ClockWarnings 1`, `Warnings ["1 file has a suspicious capture time"]`
  - `Samples` `[{sha1,"jpg"},{sha3,"jpg"},{sha4,"video"}]`: the RAW+JPEG pair at 10.00.00 shows once, by its JPEG.
- `TestListUntrackedAndMissing` (Review Focus 7): `Untracked` is `["2025-05-30 - Trip"]`, without `.hidden` or `readme.txt`. f2 has `Files 0`, `Samples []` (non-nil, so it encodes as `[]`) and `Warnings ["Not found on disk in Staging/"]`.
- `TestListNoStagingDir` (Review Focus 7): with a fresh catalog and a photos dir that has no `Staging/`, `List` returns no error, `Folders []` and `Untracked []`, both non-nil.
- `TestPickSamplesSpread`: 12 single-file shots named `…01.JPG` through `…12.JPG` → 5 samples, at indices 0, 2, 4, 7 and 9. The shots `A.ARW`, `A.JPG` and `B.ARW` → samples `[A.JPG (jpg), B.ARW (raw)]`. Call the unexported `pickSamples(sorted []catalog.Asset, n int) []Sample` directly.
- `TestFiles`:
  - `Files(ctx, f1.ID, "")` lists 4 files with these `Path`s, in order: `Capture/2025-05-30 10.00.00.ARW` (kind `raw`), `2025-05-30 10.00.00.JPG`, `2025-05-30 11.00.00.JPG`, `Videos/2025-05-30 12.00.00 - 3840w - 29.970 - 12sec.MP4`. They are sorted by base name, then by path, so `.ARW` comes before `.JPG`.
  - `Name` is the base name. The video has `TZOffset "+02:00"` and `CaptureTime "2025-05-30T12:00:00"`. File 3 has `ClockWarnings ["capture time is after the NAS clock"]`. `Folder` equals f1's `List` entry.
  - `Files(ctx, f1.ID, "raw")` gives only the ARW, while `Folder.Files` is still 4.
  - `Files(ctx, f1.ID, "png")` → `errors.Is(err, ErrBadKind)`. `Files(ctx, 999, "")` → `errors.Is(err, catalog.ErrNotFound)`. `Files(ctx, f2.ID, "")` → `Files` is `[]` (non-nil).
- `TestSplitName`: names are text and are never validated as dates (§9).
  - `2025-05-30` → (`2025-05-30`, ``)
  - `2025-05-30 - Trip to Rome` → (`2025-05-30`, `Trip to Rome`)
  - `2007-09-31 - Random Party` → (`2007-09-31`, `Random Party`)
  - `Random` → (``, `Random`)
  - `2025-05-30 -` → (``, `2025-05-30 -`)
- `TestKindName`: image → `jpg`, raw → `raw`, video → `video`.

- [ ] **Step 4: Implement `internal/staging/staging.go`**

```go
// Package staging reads the folders under Staging/ for the web UI. M4 adds
// the operations that change them.
package staging

// Dir is the folder under the photos root that imports land in. It must
// equal ingest.StagingDir (dir_test.go checks).
const Dir = "Staging"

// MaxSamples is how many thumbnails a folder card shows.
const MaxSamples = 5

var ErrBadKind = errors.New("staging: kind must be jpg, raw or video")

type Service struct {
	db         *catalog.DB
	photosRoot string
}

func New(db *catalog.DB, photosRoot string) *Service { return &Service{db: db, photosRoot: photosRoot} }

type KindCounts struct {
	JPG   int `json:"jpg"`
	Raw   int `json:"raw"`
	Video int `json:"video"`
}

type Sample struct {
	SHA256 string `json:"sha256"`
	Kind   string `json:"kind"` // jpg | raw | video
}

// Folder is one staging folder as the UI shows it.
type Folder struct {
	ID            int64      `json:"id"`
	Name          string     `json:"name"`
	Date          string     `json:"date"`  // the name's leading YYYY-MM-DD, "" if none
	Title         string     `json:"title"` // the text after "YYYY-MM-DD - ", or the whole name if it has no date
	Dates         []string   `json:"dates"`
	CreatedAt     time.Time  `json:"created_at"`
	Files         int        `json:"files"`
	Bytes         int64      `json:"bytes"`
	Counts        KindCounts `json:"counts"`
	FirstCapture  string     `json:"first_capture,omitempty"` // wall clock, catalog.CaptureLayout
	LastCapture   string     `json:"last_capture,omitempty"`
	Samples       []Sample   `json:"samples"`
	ClockWarnings int        `json:"clock_warnings"` // files with at least one clock warning
	Warnings      []string   `json:"warnings"`
}

type Listing struct {
	Folders   []Folder `json:"folders"`   // ordered by name
	Untracked []string `json:"untracked"` // directories in Staging/ with no catalog row (M4 adopts them)
}

type File struct {
	SHA256        string   `json:"sha256"`
	Name          string   `json:"name"` // base name
	Path          string   `json:"path"` // slash path inside the folder, e.g. "Capture/x.ARW"
	Kind          string   `json:"kind"`
	Size          int64    `json:"size"`
	CaptureTime   string   `json:"capture_time,omitempty"`
	TZOffset      string   `json:"tz_offset,omitempty"`
	ClockWarnings []string `json:"clock_warnings,omitempty"`
}

type FolderFiles struct {
	Folder Folder `json:"folder"`
	Files  []File `json:"files"`
}
```

Write these bodies:
- `SplitName`: match `regexp.MustCompile(`^([0-9]{4}-[0-9]{2}-[0-9]{2})(?: - (.+))?$`)`. On a match return (m[1], m[2]), otherwise ("", name).
- `KindName`: image → "jpg", raw → "raw", video → "video", anything else → `string(k)`.
- `List`:
  - Call `StagingFolders`, `AssetsUnder(ctx, Dir+"/", StateStaged)`, `ClockWarningsUnder(ctx, Dir+"/", StateStaged)` and `diskFolders()`.
  - Group the assets by the first path element after `Staging/`, using `strings.Cut(strings.TrimPrefix(rel, Dir+"/"), "/")`. An asset with no second `/` sits directly in `Staging/`; ingest never puts one there, so skip it and say so in a comment.
  - Build one `Folder` per row with `summarize`.
  - `Untracked` holds the names on disk that have no row, sorted. Both slices are non-nil.
- `diskFolders() (map[string]bool, error)`: `os.ReadDir(filepath.Join(photosRoot, Dir))`. `fs.ErrNotExist` means no import has happened yet: return an empty map and nil. Any other error is returned wrapped. Keep the directories whose names don't start with `.`.
- `sortFiles(assets) []catalog.Asset`: sort a copy with `slices.SortStableFunc`, by `path.Base(RelPath)` and then by `RelPath`.
- `summarize(row catalog.StagingFolder, sorted []catalog.Asset, warned map[string][]string, onDisk bool) Folder`:
  - Totals and kind counts.
  - The min and max non-zero `CaptureTime`, formatted with `catalog.CaptureLayout`.
  - `ClockWarnings` = the number of files whose sha is in `warned`.
  - `Samples = pickSamples(sorted, MaxSamples)`.
  - `Warnings` (non-nil), in this order: `"Not found on disk in Staging/"` when `!onDisk`; then, when n > 0, `"1 file has a suspicious capture time"` or `"%d files have a suspicious capture time"`.
  - `Dates` is non-nil.
- `pickSamples`: walk the sorted files and group consecutive files whose base name without the extension (the stem) is equal. Each group keeps its best file by rank (image 0, raw 1, video 2). With `m := min(n, len(shots))`, return `shots[i*len(shots)/m]` for each `i` in `0..m-1`. The result is non-nil.
- `warnedFiles(ws []catalog.ClockWarning) map[string][]string`: maps each sha to its details.
- `Files`:
  - Validate `kind`: `""`, `jpg`, `raw` and `video` are allowed, anything else returns `fmt.Errorf("%w, not %q", ErrBadKind, kind)`.
  - Call `StagingFolderByID` and pass its error through (it already wraps `ErrNotFound`).
  - Reject a row whose name is `""`, `.` or `..`, or contains `/` or `\`: `"staging folder %d has a name that is not a single folder name: %q"`.
  - Use `prefix := Dir + "/" + name + "/"` for `AssetsUnder` and `ClockWarningsUnder`.
  - Set `onDisk` by `os.Stat`-ing the directory: `fs.ErrNotExist` → false, and any other error is returned.
  - `Folder = summarize(…)` over all files. `Files` holds the files matching `kind`, in sorted order, with `Path = strings.TrimPrefix(rel, prefix)`, and is non-nil.

Run: `go test ./internal/staging/` → PASS.

- [ ] **Step 5: API: write the failing tests**

`internal/api/full_test.go`:

```go
// fullServer serves ingest and staging (and, from Task 7, thumbnails) over
// a fresh catalog, with the committed SONY_A7IV card under a temp cards root.
type fullServer struct {
	s                    *Server
	card, photos, config string
	db                   *catalog.DB
}

func newFullServer(t *testing.T) fullServer
```

Set it up like `newIngestServer`:
- Create the cards, photos and config temp dirs first, then `CopyCard`.
- `catalog.Open`, with a close cleanup.
- A runner with `FreeSpace: plenty` and `slog.DiscardHandler`, and a manager with `Start` and a close cleanup.
- `New("test", nil)` and `MountIngest(m, db, time.Minute)`, plus `MountStaging(staging.New(db, photos))`.
- Register `t.Cleanup(s.Close)` last.

`internal/api/staging_test.go`:
- `TestStagingAPI`: `f := newFullServer(t)`, then `post(t, f.s, "?wait=1", cardBody(f.card))` → 200. (Don't name the variable `fs`; it would shadow `io/fs` in this package's tests.)
  - `GET /api/staging` → 200 with `Cache-Control: no-store`, and `untracked []`. Its one folder is named `2025-05-30`, with `counts {"jpg":1,"raw":1,"video":0}`, `files 2` and one sample of kind `jpg`.
  - `GET /api/staging/{id}/files` → 2 files and a `folder` object.
  - `?kind=raw` → 1 file whose `path` starts with `Capture/`.
- `TestStagingAPIErrors`: `/api/staging/999/files` → 404 with a JSON error. `/api/staging/abc/files` and `/api/staging/0/files` → 400. `?kind=bogus` on a real id → 400 with a JSON error that mentions `kind`.

- [ ] **Step 6: Implement `internal/api/staging.go`**

```go
type stagingHandlers struct {
	svc *staging.Service
	log *slog.Logger
}

// MountStaging registers the read-only staging endpoints. Call it before serving.
func (s *Server) MountStaging(svc *staging.Service) {
	h := &stagingHandlers{svc: svc, log: s.log}
	s.mux.HandleFunc("GET /api/staging", h.list)
	s.mux.HandleFunc("GET /api/staging/{id}/files", h.files)
}
```

- `list`: call `svc.List(r.Context())`. On error, log `h.log.Error("list staging", "err", err)` and answer 500 with the message. Otherwise answer 200 with the `Listing`.
- `files`: `positiveInt(r.PathValue("id"))` errors → 400 `"id: …"`. Then call `svc.Files(ctx, id, r.URL.Query().Get("kind"))`: `ErrBadKind` → 400, `catalog.ErrNotFound` → 404 `fmt.Sprintf("no staging folder %d", id)`, and anything else is logged and answered with 500.

- [ ] **Step 7: Wire it into `main.go`**

After `srv.MountIngest(mgr, db, api.DefaultMaxWait)`, add `srv.MountStaging(staging.New(db, cfg.Photos))`.

- [ ] **Step 8: Run everything**

Run the full Windows set and the Linux command → PASS.

- [ ] **Step 9: Commit**

```bash
git add internal/catalog/staging.go internal/catalog/staging_test.go internal/staging internal/api/staging.go internal/api/staging_test.go internal/api/full_test.go cmd/sd2nas/main.go
git commit -m "List staging folders and their files from the catalog for the UI

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Thumbnail cache core (`internal/thumbs`): Go images, single flight, bounded workers

Suggested implementer: sonnet (reviewer: opus, because of the concurrency).

**Files:**
- Create: `internal/thumbs/thumbs.go`, `internal/thumbs/thumbs_test.go`
- Modify: `go.mod`, `go.sum`

**Interfaces:**
- Consumes: `media.Kind` with `media.KindImage`, `media.KindRaw` and `media.KindVideo` (`internal/media/formats.go`).
- Produces:

```go
const ThumbSize = 400
const PreviewSize = 1600
var ErrBadSHA, ErrBadSize, ErrNoImage, ErrClosed error
type Source struct {
	SHA256 string     // content hash: the cache key
	Path   string     // OS path of the file
	Kind   media.Kind // image, raw or video
}
type Config struct {
	Dir      string        // cache root, e.g. /config/thumbs (created if missing)
	Exiftool string        // default "exiftool"
	FFmpeg   string        // default "ffmpeg"
	Workers  int           // renders at once; default max(1, min(runtime.NumCPU(), 4))
	Timeout  time.Duration // per render; default 60 s
	Log      *slog.Logger  // default slog.Default()
}
type Cache struct{ /* … */ }
func New(cfg Config) (*Cache, error)
func (c *Cache) Get(ctx context.Context, src Source, size int) (path string, err error)
func (c *Cache) Close()
func ValidSHA(s string) bool
// unexported, used by Task 6:
//   c.cfg Config; c.renderSource(ctx, src) (image.Image, error) — its switch gains raw and video cases
//   test helpers redBlueImage(w, h int) *image.RGBA; encodeJPEG(img image.Image, orientation uint16) ([]byte, error)
//   reddish(c color.Color) bool; bluish(c color.Color) bool; decodeFile(t, path) image.Image
```

- [ ] **Step 1: Add the dependency**

Run: `go get github.com/disintegration/imaging@v1.6.2 golang.org/x/image@v0.36.0`
Check that `go.mod` still says `go 1.24.0` with no `toolchain` line. After the code below imports `imaging`, `go mod tidy` must list `github.com/disintegration/imaging v1.6.2` as a direct requirement and `golang.org/x/image v0.36.0` as indirect.

- [ ] **Step 2: Write the failing tests** (`internal/thumbs/thumbs_test.go`, package `thumbs`)

Helpers (Task 6 reuses them):

```go
// redBlueImage is w×h, red on the left half and blue on the right.
func redBlueImage(w, h int) *image.RGBA

// encodeJPEG encodes img (quality 95) and, when orientation > 0, inserts an
// APP1 EXIF segment right after SOI whose IFD0 holds only Orientation
// (tag 0x0112, SHORT, 1).
func encodeJPEG(img image.Image, orientation uint16) ([]byte, error)
```

Build the EXIF segment like this:

```go
	tiff := []byte{'I', 'I', 42, 0, 8, 0, 0, 0, // little-endian header, IFD0 at offset 8
		1, 0, // one entry
		0x12, 0x01, 3, 0, 1, 0, 0, 0, byte(orientation), byte(orientation >> 8), 0, 0,
		0, 0, 0, 0} // no next IFD
	payload := append([]byte("Exif\x00\x00"), tiff...)
	// out = FF D8, FF E1, big-endian uint16(len(payload)+2), payload, then the encoder's output without its SOI
```

Every test that replaces `c.render` still needs `Source.Path` to exist, because `generate` stats it first: use `writeSource` with any bytes.

`reddish(c)` is `r > 2*b` and `bluish(c)` is `b > 2*r`, on the 16-bit values from `c.RGBA()`. `decodeFile(t, path)` opens the file and returns `jpeg.Decode` of it. `newCache(t, cfg Config) *Cache` sets `cfg.Dir = t.TempDir()` when it is empty and registers `t.Cleanup(c.Close)`. `writeSource(t, dir, name string, data []byte) string` writes a file and returns its path. `shaN(n int) string` is `fmt.Sprintf("%064x", n)`.

Tests:
- `TestGetJPEGThumb`: a 1200×800 JPEG (no orientation). `Get(ctx, Source{shaN(1), path, media.KindImage}, ThumbSize)` returns `filepath.Join(dir, sha[:2], sha+"-400.jpg")`, and that file decodes to exactly 400×267.
- `TestGetPreviewNotUpscaled`: a 300×200 JPEG at `PreviewSize` → 300×200.
- `TestGetAppliesOrientation`: a 64×32 JPEG with orientation 6 at `ThumbSize` → 32×64, with pixel (16, 8) reddish and (16, 56) bluish. Turning it 90° clockwise puts the left half on top.
- `TestGetPNG`: a 500×500 PNG (`image/png`) at `ThumbSize` → 400×400.
- `TestGetCached`: replace `c.render` with a counting wrapper around the original. Two `Get`s for the same source and size render once. A `Get` at the other size renders again (2 in total).
- `TestGetSingleFlight` (Review Focus 6): `c.render` blocks on a `release` channel and counts its calls. 5 goroutines `Get` the same source and size. `pollUntil` the count is 1, then close `release`. All 5 return the same path with nil error, and the count stays 1.
- `TestGetWorkersBound` (Review Focus 3): `Workers: 2`. `c.render` raises an atomic `active` counter, records the maximum, blocks on `release`, and then lowers it. 6 goroutines each `Get` a different sha. `pollUntil` `active == 2`, close `release`, wait for all. The maximum is exactly 2.
- `TestGetCallerCancelKeepsSharedRender` (Review Focus 6): `c.render` blocks until `release` and records `ctx.Err()` when it returns. Caller A uses a cancellable ctx and caller B uses `context.Background()`; wait until both are waiting (`c.calls[…].waiters == 2` under `c.mu`, polled). Cancel A, and its `Get` returns `context.Canceled`. Close `release`. B gets the path, and the render saw `ctx.Err() == nil`.
- `TestGetLastCallerCancelCancelsRender` (Review Focus 6): `c.render` waits on `ctx.Done()` and signals on a channel. The only caller cancels. The render's ctx is done within 10 s, and the cache map no longer holds the entry (polled under `c.mu`).
- `TestGetTimeout` (Review Focus 3): `Timeout: 50 * time.Millisecond`, and `c.render` waits on `ctx.Done()` and returns `ctx.Err()`. `Get` returns an error `errors.Is(err, context.DeadlineExceeded)`. No `.jpg` file and no `*.tmp` file exists under `Dir` (walk it).
- `TestGetRenderErrorNotCached`: the first render returns `errors.New("broken")`, and later ones delegate to the original. The first `Get` returns an error containing `broken`, and the second succeeds.
- `TestGetBadInput` (Review Focus 4): the shas `../../etc/passwd`, `ABC…` (64 uppercase hex digits), 63 hex digits and `""` → `errors.Is(err, ErrBadSHA)`. Size 500 → `ErrBadSize`. Afterwards `Dir` is still empty.
- `TestGetMissingSource`: a `Path` that doesn't exist → `errors.Is(err, fs.ErrNotExist)`.
- `TestGetUnknownKind`: `Kind: "bogus"` → an error containing `no renderer`.
- `TestNewRemovesStaleTemps` (Review Focus 6): before `New`, create `<dir>/ab/x.jpg.123.tmp` and `<dir>/ab/keep.jpg`. After `New`, the `.tmp` file is gone and `keep.jpg` remains.
- `TestCloseCancelsRenders` (Review Focus 6): `c.render` waits on `ctx.Done()`. Start a `Get` in a goroutine and poll until it is waiting. `c.Close()` returns within 10 s. That `Get` returns an error `errors.Is(err, context.Canceled)`, and a later `Get` returns `ErrClosed`.
- `TestValidSHA`: the table above, plus a valid lowercase 64-digit sha → true.

- [ ] **Step 3: Run them and confirm they fail**

Run: `go test ./internal/thumbs/`
Expected: a build failure (the package has no non-test files).

- [ ] **Step 4: Implement `internal/thumbs/thumbs.go`**

```go
// Package thumbs renders and caches the JPEG thumbnails (ThumbSize) and
// previews (PreviewSize) the web UI shows for staged files. JPEG, PNG, GIF
// and TIFF are decoded and resized in Go; RAW files give up their embedded
// preview through exiftool, and videos a frame at 1 s through ffmpeg.
// Results are cached under Config.Dir by content hash and size.
package thumbs

import (
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/disintegration/imaging"

	"github.com/aspiziri/SD2NAS/internal/media"
)

// Longest edge, in pixels, of the two sizes the UI asks for.
const (
	ThumbSize   = 400
	PreviewSize = 1600
)

var (
	ErrBadSHA  = errors.New("thumbs: hash must be 64 lowercase hex digits")
	ErrBadSize = errors.New("thumbs: size must be ThumbSize or PreviewSize")
	ErrNoImage = errors.New("thumbs: the file has no embedded preview or video frame")
	ErrClosed  = errors.New("thumbs: cache closed")
)

// Source is a file to render.
type Source struct {
	SHA256 string     // content hash: the cache key
	Path   string     // OS path of the file
	Kind   media.Kind // image, raw or video
}

// Config configures a Cache; zero values get the defaults.
type Config struct {
	Dir      string        // cache root, e.g. /config/thumbs (created if missing)
	Exiftool string        // command for RAW previews; default "exiftool"
	FFmpeg   string        // command for video frames; default "ffmpeg"
	Workers  int           // renders at once; default max(1, min(NumCPU, 4))
	Timeout  time.Duration // per render; default 60 s
	Log      *slog.Logger  // default slog.Default()
}

// call is one render that any number of Get calls wait for.
type call struct {
	done    chan struct{} // closed when the render has finished
	err     error         // set before done is closed
	waiters int           // Get calls still waiting; guarded by Cache.mu
	cancel  context.CancelFunc
}

// Cache renders thumbnails on demand and keeps them on disk.
type Cache struct {
	cfg    Config
	sem    chan struct{} // one slot per running render
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup // running renders
	mu     sync.Mutex
	calls  map[string]*call // by destination path
	closed bool
	render func(ctx context.Context, src Source) (image.Image, error) // renderSource; tests replace it
}

// New creates the cache directory and removes unfinished files a crash
// left behind.
func New(cfg Config) (*Cache, error) {
	if cfg.Dir == "" {
		return nil, errors.New("thumbs: Config.Dir is empty")
	}
	if cfg.Exiftool == "" {
		cfg.Exiftool = "exiftool"
	}
	if cfg.FFmpeg == "" {
		cfg.FFmpeg = "ffmpeg"
	}
	if cfg.Workers <= 0 {
		cfg.Workers = max(1, min(runtime.NumCPU(), 4))
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 60 * time.Second
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0o777); err != nil {
		return nil, fmt.Errorf("thumbnail cache: %w", err)
	}
	removed, err := removeTemps(cfg.Dir)
	if removed > 0 {
		cfg.Log.Info("removed unfinished thumbnails", "count", removed)
	}
	if err != nil {
		return nil, fmt.Errorf("thumbnail cache: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c := &Cache{cfg: cfg, sem: make(chan struct{}, cfg.Workers), ctx: ctx, cancel: cancel, calls: map[string]*call{}}
	c.render = c.renderSource
	return c, nil
}

// removeTemps deletes *.tmp files below dir.
func removeTemps(dir string) (int, error) {
	n := 0
	var errs []error
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			errs = append(errs, err)
			return nil
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".tmp") {
			if err := os.Remove(p); err != nil {
				errs = append(errs, err)
			} else {
				n++
			}
		}
		return nil
	})
	return n, errors.Join(append(errs, err)...)
}

// ValidSHA reports whether s is 64 lowercase hex digits, the only form
// that is ever turned into a cache path.
func ValidSHA(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !('0' <= c && c <= '9' || 'a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func (c *Cache) path(sha string, size int) string {
	return filepath.Join(c.cfg.Dir, sha[:2], fmt.Sprintf("%s-%d.jpg", sha, size))
}

// Get returns the path of src's cached JPEG at size, rendering it first
// when needed. Concurrent calls for the same file and size share one
// render, which is cancelled only once every caller has gone. At most
// Workers renders run at once.
func (c *Cache) Get(ctx context.Context, src Source, size int) (string, error) {
	if !ValidSHA(src.SHA256) {
		return "", fmt.Errorf("%w: %q", ErrBadSHA, src.SHA256)
	}
	if size != ThumbSize && size != PreviewSize {
		return "", fmt.Errorf("%w: %d", ErrBadSize, size)
	}
	dst := c.path(src.SHA256, size)
	if _, err := os.Stat(dst); err == nil {
		return dst, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("thumbnail cache: %w", err)
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", ErrClosed
	}
	cl := c.calls[dst]
	if cl == nil {
		rctx, cancel := context.WithTimeout(c.ctx, c.cfg.Timeout)
		cl = &call{done: make(chan struct{}), cancel: cancel}
		c.calls[dst] = cl
		c.wg.Add(1)
		go c.run(rctx, cl, src, size, dst)
	}
	cl.waiters++
	c.mu.Unlock()

	select {
	case <-cl.done:
		if cl.err != nil {
			return "", cl.err
		}
		return dst, nil
	case <-ctx.Done():
		c.mu.Lock()
		cl.waiters--
		if cl.waiters == 0 {
			// Nobody wants it any more: stop the render, and let the next
			// Get start afresh instead of joining a cancelled one.
			cl.cancel()
			if c.calls[dst] == cl {
				delete(c.calls, dst)
			}
		}
		c.mu.Unlock()
		return "", ctx.Err()
	}
}

func (c *Cache) run(ctx context.Context, cl *call, src Source, size int, dst string) {
	defer c.wg.Done()
	err := c.generate(ctx, src, size, dst)
	cl.cancel()
	c.mu.Lock()
	if c.calls[dst] == cl {
		delete(c.calls, dst)
	}
	c.mu.Unlock()
	cl.err = err
	close(cl.done)
}

func (c *Cache) generate(ctx context.Context, src Source, size int, dst string) error {
	if _, err := os.Stat(src.Path); err != nil {
		return err // fs.ErrNotExist reaches the caller as is
	}
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()
	img, err := c.render(ctx, src)
	if err != nil {
		return fmt.Errorf("%s: %w", src.Path, err)
	}
	if err := ctx.Err(); err != nil {
		return err // cancelled or timed out while decoding in Go
	}
	return writeJPEG(dst, imaging.Fit(img, size, size, imaging.Lanczos), quality(size))
}

func quality(size int) int {
	if size == ThumbSize {
		return 80
	}
	return 85
}

// writeJPEG encodes img to a temporary file next to dst and renames it
// into place, so a reader never sees a half-written thumbnail.
func writeJPEG(dst string, img image.Image, quality int) (err error) {
	dir := filepath.Dir(dst)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, filepath.Base(dst)+".*.tmp")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			if rmErr := os.Remove(f.Name()); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
				err = errors.Join(err, rmErr)
			}
		}
	}()
	if err = jpeg.Encode(f, img, &jpeg.Options{Quality: quality}); err != nil {
		return errors.Join(err, f.Close())
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), dst)
}

// renderSource decodes src into an upright image of any size. Task 6 adds
// the raw and video cases.
func (c *Cache) renderSource(ctx context.Context, src Source) (image.Image, error) {
	switch src.Kind {
	case media.KindImage:
		return imaging.Open(src.Path, imaging.AutoOrientation(true))
	}
	return nil, fmt.Errorf("no renderer for kind %q", src.Kind)
}

// Close cancels running renders, waits for them, and makes later Gets fail
// with ErrClosed.
func (c *Cache) Close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.cancel()
	c.wg.Wait()
}
```

- [ ] **Step 5: Run the tests**

Run `go test ./internal/thumbs/ -count=3`, then `go mod tidy` (check `go.mod` as in Step 1), the full Windows set, the Linux command and the race command → PASS with no races.

- [ ] **Step 6: Commit**

```bash
git add go.mod go.sum internal/thumbs
git commit -m "Add the thumbnail cache: resize in Go, one render per file, bounded workers

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: RAW previews with exiftool and video frames with ffmpeg

Suggested implementer: sonnet.

**Files:**
- Create: `internal/thumbs/tools.go`, `internal/thumbs/tools_test.go`
- Modify: `internal/thumbs/thumbs.go` (add the raw and video cases to `renderSource`)

**Interfaces:**
- Consumes (Task 5): `Cache` (fields `cfg`, `render`), `renderSource`, `Get`, `ErrNoImage`, `ThumbSize`, `Source`, `Config`, and the test helpers `redBlueImage`, `encodeJPEG`, `reddish`, `bluish`, `decodeFile`, `newCache`, `writeSource`, `shaN`, `pollUntil`-style loops. Exiftool and ffmpeg arguments: Decisions 9 and 10.
- Produces:

```go
func (c *Cache) MissingTools() []string // "<tool> not found: RAW thumbnails are unavailable" / "…: video thumbnails are unavailable"
// unexported:
//   var maxToolOutput = 128 << 20
//   func (c *Cache) runTool(ctx context.Context, name string, args ...string) ([]byte, error)
//   func (c *Cache) rawPreview(ctx context.Context, path string) (image.Image, error)
//   func (c *Cache) videoFrame(ctx context.Context, path string) (image.Image, error)
//   func orient(img image.Image, o int) image.Image
// test helper (exported to other tasks by name only): needTool(t, name) string
```

- [ ] **Step 1: Write the failing tests** (`internal/thumbs/tools_test.go`, package `thumbs`)

A fake tool via the test binary:

```go
func TestMain(m *testing.M) {
	if mode := os.Getenv("SD2NAS_FAKE_TOOL"); mode != "" {
		os.Exit(fakeTool(mode, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeTool stands in for exiftool or ffmpeg when a test sets
// SD2NAS_FAKE_TOOL and points Config.Exiftool/FFmpeg at os.Args[0]. It
// appends its arguments as one JSON line to $SD2NAS_FAKE_ARGS.
func fakeTool(mode string, args []string) int
```

Modes, where `jpg` is `encodeJPEG(redBlueImage(64, 32), 0)` and `b64 := "base64:" + base64.StdEncoding.EncodeToString(jpg)`:
- `exif-preview`: print `[{"SourceFile":"x","Orientation":6,"PreviewImage":"<b64>"}]`.
- `exif-jpgfromraw`: if the args contain `-JpgFromRaw`, print `[{"SourceFile":"x","JpgFromRaw":"<b64>"}]`; otherwise print `[{"SourceFile":"x","Orientation":1}]`.
- `exif-none`: print `[{"SourceFile":"x"}]`.
- `garbage`: print `not json`.
- `fail`: write `boom: bad file` to stderr and exit 3.
- `hang`: `time.Sleep(time.Minute)`.
- `ffmpeg-short`: write the `jpg` bytes to stdout only when the argument after `-ss` is `0`; otherwise write nothing and exit 0.
- `big`: write 2 MiB of zero bytes to stdout.
- An unknown mode prints to stderr and exits 98. A failure to record the args prints to stderr and exits 99.

`fakeCache(t, mode string, cfg Config) (*Cache, string /*args file*/)` sets `cfg.Exiftool = os.Args[0]`, `cfg.FFmpeg = os.Args[0]`, `t.Setenv("SD2NAS_FAKE_TOOL", mode)` and `t.Setenv("SD2NAS_FAKE_ARGS", <tempdir>/args.jsonl)`, and returns `newCache(t, cfg)`. `recordedArgs(t, file) [][]string` reads the JSON lines back.

```go
// needTool returns the path of a real tool, skipping the test when it is
// not installed, or failing it when SD2NAS_REQUIRE_TOOLS=1 (CI and the
// Linux Docker run set it, so these tests can't silently stop running).
func needTool(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		if os.Getenv("SD2NAS_REQUIRE_TOOLS") == "1" {
			t.Fatalf("%s is required (SD2NAS_REQUIRE_TOOLS=1): %v", name, err)
		}
		t.Skipf("%s is not installed: %v", name, err)
	}
	return p
}
```

`writeARW(t, path string, preview []byte, orientation uint16)` writes a little-endian TIFF. Its IFD0 has five entries: Make `SONY` (0x010F, ASCII, count 5), Model `ILCE-7M4` (0x0110, ASCII, count 9), Orientation (0x0112, SHORT), `0x0201` (LONG, the preview's offset) and `0x0202` (LONG, the preview's length). After the IFD come the strings (`"SONY\x00\x00"` and `"ILCE-7M4\x00\x00"`) and then the preview bytes. This layout was verified to make exiftool 13.30 report `PreviewImage` for a `.ARW` file. Layout: header 8 bytes, IFD at 8 (2 + 5×12 + 4 = 66 bytes), Make at 74, Model at 80, preview at 90.

Tests (any source path the fakes get just has to exist; write a few bytes there):
- `TestRawPreviewArgs` (Review Focus 3): mode `exif-preview`. `Get(…, Source{shaN(1), path, media.KindRaw}, ThumbSize)` → 32×64 (Orientation 6 applied), with the top reddish. The recorded args are exactly one call: `["-json","-b","-n","-Orientation","-PreviewImage","--",<path>]`.
- `TestRawFallsBackToJpgFromRaw`: mode `exif-jpgfromraw` → 64×32. Two calls were recorded, and the second is `["-json","-b","-n","-Orientation","-JpgFromRaw","--",<path>]`.
- `TestRawNoPreview`: mode `exif-none` → `errors.Is(err, ErrNoImage)`.
- `TestToolFailureSurfacesStderr` (Review Focus 3): mode `fail` → an error containing `boom: bad file`.
- `TestToolGarbage`: mode `garbage` → an error containing `exiftool output`.
- `TestToolTimeoutKills` (Review Focus 3): mode `hang` with `Timeout: 300 * time.Millisecond`. `Get` returns within 10 s with `errors.Is(err, context.DeadlineExceeded)`.
- `TestToolOutputCapped` (Review Focus 3): set `maxToolOutput = 1 << 20` (restore it with `t.Cleanup`), mode `big`, `Kind: media.KindVideo` → an error containing `larger than`.
- `TestVideoShortClipRetriesAtZero` (Review Focus 3): mode `ffmpeg-short`, `Kind: media.KindVideo` → 64×32. Two calls were recorded. The first is exactly `["-nostdin","-hide_banner","-loglevel","error","-ss","1","-i","file:"+path,"-frames:v","1","-an","-sn","-dn","-f","image2pipe","-c:v","mjpeg","-q:v","2","pipe:1"]`, and the second is the same with `-ss` `0`.
- `TestVideoBadFrame`: mode `garbage` with `Kind: media.KindVideo` (ffmpeg's stdout is not a JPEG) → an error containing `decode video frame`.
- `TestOrientMatchesImaging`: for each `o` from 1 to 8, `b, _ := encodeJPEG(redBlueImage(8, 4), uint16(o))`. `want, _ := imaging.Decode(bytes.NewReader(b), imaging.AutoOrientation(true))`, `raw, _ := imaging.Decode(bytes.NewReader(b))` and `got := orient(raw, o)`. Both have the same bounds, and `imaging.Clone(got).Pix` equals `imaging.Clone(want).Pix`. This proves `orient` matches imaging's own EXIF handling.
- `TestMissingTools`: `newCache(t, Config{Exiftool: "sd2nas-no-such-tool", FFmpeg: os.Args[0]})` → `MissingTools()` is exactly `["sd2nas-no-such-tool not found: RAW thumbnails are unavailable"]`.
- `TestRealExiftoolARW`: `needTool(t, "exiftool")`. Write `x.ARW` with `writeARW` (a 64×32 red/blue preview, orientation 6). A default-config `Get` at `ThumbSize` → 32×64 with the top reddish.
- `TestRealFFmpegVideo`: `ff := needTool(t, "ffmpeg")`. Create the clips with `exec.Command(ff, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "testsrc=duration=2:size=320x240:rate=10", "-pix_fmt", "yuv420p", clip)` (and `duration=0.5` for a second `short.mp4`). A path with a space (`"my clip.mp4"`) works. `Get` at `ThumbSize` on each → 320×240 (Fit never upscales). The short clip proves the retry at 0 s.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/thumbs/ -run 'TestRaw|TestTool|TestVideo|TestOrient|TestMissing|TestReal'`
Expected: a build failure (`orient`, `MissingTools` undefined).

- [ ] **Step 3: Implement `internal/thumbs/tools.go`**

```go
package thumbs

// maxToolOutput caps what is read from exiftool or ffmpeg: a full-size
// JpgFromRaw of a 61 MP body is ~20 MB and ~27 MB as base64. It is a var
// so a test can lower it.
var maxToolOutput = 128 << 20

// capBuffer keeps up to max bytes and drops the rest, so a chatty tool is
// never blocked on a full pipe.
type capBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (b *capBuffer) Write(p []byte) (int, error) {
	if room := b.max - b.buf.Len(); room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return b.buf.Write(p)
}

// runTool runs name with args (never through a shell) and returns stdout.
// The context's deadline kills it. Its stderr goes into the error, or is
// logged when it exits 0.
func (c *Cache) runTool(ctx context.Context, name string, args ...string) ([]byte, error) {
	tool := filepath.Base(name)
	cmd := exec.CommandContext(ctx, name, args...)
	stdout := &capBuffer{max: maxToolOutput}
	stderr := &capBuffer{max: 4 << 10}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.WaitDelay = 5 * time.Second // don't hang on pipes held open by a killed tool's children
	err := cmd.Run()
	msg := strings.TrimSpace(stderr.buf.String())
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("%s: %w", tool, ctxErr)
	}
	if err != nil {
		if msg != "" {
			return nil, fmt.Errorf("%s: %w: %s", tool, err, msg)
		}
		return nil, fmt.Errorf("%s: %w", tool, err)
	}
	if stdout.overflow {
		return nil, fmt.Errorf("%s: output is larger than %d bytes", tool, maxToolOutput)
	}
	if msg != "" {
		c.cfg.Log.Warn("tool wrote to stderr", "tool", tool, "args", args, "stderr", msg)
	}
	return stdout.buf.Bytes(), nil
}

// rawPreview returns the RAW file's embedded preview (falling back to
// JpgFromRaw), turned upright with the RAW's own Orientation tag.
func (c *Cache) rawPreview(ctx context.Context, path string) (image.Image, error) {
	for _, tag := range []string{"PreviewImage", "JpgFromRaw"} {
		out, err := c.runTool(ctx, c.cfg.Exiftool, "-json", "-b", "-n", "-Orientation", "-"+tag, "--", path)
		if err != nil {
			return nil, err
		}
		var res []map[string]any
		if err := json.Unmarshal(out, &res); err != nil {
			return nil, fmt.Errorf("exiftool output: %w", err)
		}
		if len(res) != 1 {
			return nil, fmt.Errorf("exiftool output: %d results, want 1", len(res))
		}
		enc, _ := res[0][tag].(string) // absent when the file has no such image
		if enc == "" {
			continue
		}
		b64, ok := strings.CutPrefix(enc, "base64:")
		if !ok {
			return nil, fmt.Errorf("exiftool output: %s is not base64 binary", tag)
		}
		data, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			return nil, fmt.Errorf("exiftool output: %s: %w", tag, err)
		}
		img, err := imaging.Decode(bytes.NewReader(data)) // no auto-orientation: the RAW's tag rules
		if err != nil {
			return nil, fmt.Errorf("decode embedded %s: %w", tag, err)
		}
		o, _ := res[0]["Orientation"].(float64) // -n makes it a number; absent means upright
		return orient(img, int(o)), nil
	}
	return nil, ErrNoImage
}

// videoFrame returns the frame at 1 s, or at 0 s for a clip shorter than
// that (ffmpeg then writes nothing). ffmpeg applies rotation metadata
// itself. The "file:" prefix stops a path from being read as a protocol.
func (c *Cache) videoFrame(ctx context.Context, path string) (image.Image, error) {
	for _, at := range []string{"1", "0"} {
		out, err := c.runTool(ctx, c.cfg.FFmpeg, "-nostdin", "-hide_banner", "-loglevel", "error",
			"-ss", at, "-i", "file:"+path, "-frames:v", "1", "-an", "-sn", "-dn",
			"-f", "image2pipe", "-c:v", "mjpeg", "-q:v", "2", "pipe:1")
		if err != nil {
			return nil, err
		}
		if len(out) == 0 {
			continue
		}
		img, err := imaging.Decode(bytes.NewReader(out))
		if err != nil {
			return nil, fmt.Errorf("decode video frame: %w", err)
		}
		return img, nil
	}
	return nil, ErrNoImage
}

// orient turns img upright for EXIF orientation o (1–8; anything else is
// returned as is), the same way imaging.AutoOrientation does.
func orient(img image.Image, o int) image.Image {
	switch o {
	case 2:
		return imaging.FlipH(img)
	case 3:
		return imaging.Rotate180(img)
	case 4:
		return imaging.FlipV(img)
	case 5:
		return imaging.Transpose(img)
	case 6:
		return imaging.Rotate270(img)
	case 7:
		return imaging.Transverse(img)
	case 8:
		return imaging.Rotate90(img)
	}
	return img
}

// MissingTools describes each external tool that can't be found.
func (c *Cache) MissingTools() []string {
	var out []string
	for _, t := range []struct{ cmd, what string }{
		{c.cfg.Exiftool, "RAW thumbnails"},
		{c.cfg.FFmpeg, "video thumbnails"},
	} {
		if _, err := exec.LookPath(t.cmd); err != nil {
			out = append(out, fmt.Sprintf("%s not found: %s are unavailable", filepath.Base(t.cmd), t.what))
		}
	}
	return out
}
```

The `enc, _ :=` and `o, _ :=` type assertions are not swallowed errors: a missing or non-string field means "no such image" or "upright", and both are handled.

In `thumbs.go`, `renderSource` gains:

```go
	case media.KindRaw:
		return c.rawPreview(ctx, src.Path)
	case media.KindVideo:
		return c.videoFrame(ctx, src.Path)
```

If `TestOrientMatchesImaging` fails for some `o`, fix the mapping in `orient`; the test is the authority.

- [ ] **Step 4: Run the tests**

On Windows, `go test ./internal/thumbs/ -v -run 'TestReal'` shows `TestRealExiftoolARW` SKIP (no exiftool) and `TestRealFFmpegVideo` PASS (ffmpeg is installed). Then run the full Windows set and the **Linux command**, where both real-tool tests must PASS (look for them in `go test -v ./internal/thumbs/ -run TestReal` inside the same container command if in doubt).

- [ ] **Step 5: Commit**

```bash
git add internal/thumbs
git commit -m "Render RAW previews with exiftool and video frames with ffmpeg

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: Thumbnail endpoints, warm-up after an ingest, and tool warnings in `/healthz`

Suggested implementer: sonnet.

**Files:**
- Create: `internal/api/thumbs.go`, `internal/api/thumbs_test.go`
- Modify: `internal/api/server.go` (`AddWarning`), `internal/api/server_test.go` (one test), `internal/api/full_test.go` (mount thumbnails), `cmd/sd2nas/main.go`

**Interfaces:**
- Consumes: `thumbs.New`, `thumbs.Config`, `thumbs.Source`, `(*thumbs.Cache).Get/Close/MissingTools`, `thumbs.ValidSHA`, `thumbs.ThumbSize`, `thumbs.PreviewSize`, `thumbs.ErrNoImage`, `thumbs.ErrClosed` (Tasks 5–6). `catalog.AssetBySHA(ctx, sha) (Asset, error)` (follows aliases; `ErrNotFound`), `catalog.IngestFiles(ctx, id) ([]IngestFile, error)`, `catalog.OutcomeNew`. From Tasks 2–4: `s.addFinishedHook`, `s.bg`, `s.bgCtx`, `s.closers`, `s.log`, `writeError`, `fullServer`/`newFullServer`, `post`, `cardBody`, `pollUntil`, `decodeInto`.
- Produces:

```go
// MountThumbs registers GET /api/thumbs/{sha} and GET /api/preview/{sha} and
// warms the thumbnails of each finished ingest's new files. The server owns
// cache from then on: Close closes it.
func (s *Server) MountThumbs(db *catalog.DB, cache *thumbs.Cache, photosRoot string)
// AddWarning adds a permanent /healthz warning (healthz stays 200). Call it before serving.
func (s *Server) AddWarning(w string)
// fullServer gains: cache *thumbs.Cache (Dir = <config>/thumbs)
```

- [ ] **Step 1: Write the failing tests**

In `full_test.go`, `newFullServer` also creates `thumbs.New(thumbs.Config{Dir: filepath.Join(config, "thumbs"), Log: slog.New(slog.DiscardHandler)})` (a creation error fails the test) and calls `s.MountThumbs(db, cache, photos)` before registering `t.Cleanup(s.Close)`. It stores `cache` in the struct.

`thumbs_test.go` (package `api`). Helper `stageJPEG(t, f fullServer, rel string, w, h int) string`: it writes a real `w×h` JPEG (encode an `image.NewRGBA` with `image/jpeg`) at `filepath.Join(f.photos, filepath.FromSlash(rel))`, inserts a staged asset with a fresh sha (`fmt.Sprintf("%064x", counter)`) and that `rel_path`, and returns the sha.
- `TestThumbServesJPEG`: `sha := stageJPEG(t, f, "Staging/2025-05-30/a.JPG", 2000, 1000)`. `GET /api/thumbs/<sha>` → 200, `Content-Type: image/jpeg`, `Cache-Control: public, max-age=31536000, immutable`, `ETag: "<sha>-400"`, and a body that decodes to 400×200. The same request with `If-None-Match: "<sha>-400"` → 304. `GET /api/preview/<sha>` → 1600×800 and `ETag: "<sha>-1600"`. The file `<config>/thumbs/<sha[:2]>/<sha>-400.jpg` exists.
- `TestThumbBadSHA` (Review Focus 4): `/api/thumbs/abc`, `/api/thumbs/` + 64 uppercase hex digits, and `/api/preview/zz…` → 400 with a JSON error and `Cache-Control: no-store`.
- `TestThumbUnknownSHA`: a valid sha that isn't in the catalog → 404.
- `TestThumbCulledAsset`: insert an asset with state `culled` and no `rel_path` → 404 with an error mentioning `no file`.
- `TestThumbMissingFile`: stage a JPEG and delete the file → 404 with an error mentioning `missing`.
- `TestThumbSourceOutsideRoot` (Review Focus 4): insert a staged asset whose `rel_path` is `../outside.JPG` → 500, and nothing outside the photos root is opened. Logging is fine.
- `TestThumbFailure`: unit-test the pure mapper `thumbFailure(err) (code int, msg string, logIt bool)`:
  - a wrapped `fs.ErrNotExist` → 404, not logged
  - `thumbs.ErrNoImage` → 404, not logged
  - `thumbs.ErrClosed` and `context.Canceled` → 503, not logged
  - `context.DeadlineExceeded` and `errors.New("x")` → 500, logged
- `TestThumbWarmAfterIngest`: `post(t, f.s, "?wait=1", cardBody(f.card))` → 200. Read the new JPEG's sha from `f.db.IngestFiles(ctx, id)`: the outcome-`new` row whose `card_path` ends in `.JPG`. `pollUntil(t, "the warmed thumbnail", …)` until `<config>/thumbs/<sha[:2]>/<sha>-400.jpg` exists. The ARW fixture has no preview, so warming it fails and is logged; nothing else must break.
- `TestThumbsAfterClose`: stage a JPEG, `f.s.Close()`, then `GET /api/thumbs/<sha>` → 503.
- In `server_test.go`, `TestHealthzReportsWarning`: `s.AddWarning("exiftool not found: RAW thumbnails are unavailable")` → `/healthz` is 200, and `warnings` contains that string.

- [ ] **Step 2: Run them and confirm they fail**

Run: `go test ./internal/api/ -run 'TestThumb|TestHealthzReportsWarning'`
Expected: a build failure (`MountThumbs` undefined).

- [ ] **Step 3: Implement `internal/api/thumbs.go` and `AddWarning`**

```go
// warmQueue is how many finished ingests may wait for their thumbnails.
const warmQueue = 16

var errNoFile = errors.New("this hash has no file in the photos share (culled or never stored)")

type thumbHandlers struct {
	db     *catalog.DB
	cache  *thumbs.Cache
	photos string
	log    *slog.Logger
	warm   chan int64 // ingest ids
}

func (s *Server) MountThumbs(db *catalog.DB, cache *thumbs.Cache, photosRoot string) {
	h := &thumbHandlers{db: db, cache: cache, photos: photosRoot, log: s.log, warm: make(chan int64, warmQueue)}
	s.mux.HandleFunc("GET /api/thumbs/{sha}", h.serve(thumbs.ThumbSize))
	s.mux.HandleFunc("GET /api/preview/{sha}", h.serve(thumbs.PreviewSize))
	s.addFinishedHook(h.enqueue)
	s.closers = append(s.closers, cache.Close)
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		h.warmLoop(s.bgCtx)
	}()
}

// source resolves sha (following an alias) to the file to render. A
// rel_path that isn't local to the photos root is refused.
func (h *thumbHandlers) source(ctx context.Context, sha string) (thumbs.Source, error) {
	a, err := h.db.AssetBySHA(ctx, sha)
	if err != nil {
		return thumbs.Source{}, err
	}
	if a.RelPath == "" {
		return thumbs.Source{}, errNoFile
	}
	rel := filepath.FromSlash(a.RelPath)
	if !filepath.IsLocal(rel) {
		return thumbs.Source{}, fmt.Errorf("asset %s has a rel_path outside the photos share: %q", a.SHA256, a.RelPath)
	}
	return thumbs.Source{SHA256: a.SHA256, Path: filepath.Join(h.photos, rel), Kind: a.Kind}, nil
}

// thumbFailure maps a Cache.Get error to a response, and says whether it
// deserves an error log (an expected property of the file does not).
func thumbFailure(err error) (code int, msg string, logIt bool) {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return http.StatusNotFound, "the file is missing from the photos share: " + err.Error(), false
	case errors.Is(err, thumbs.ErrNoImage):
		return http.StatusNotFound, err.Error(), false
	case errors.Is(err, thumbs.ErrClosed), errors.Is(err, context.Canceled):
		return http.StatusServiceUnavailable, "SD2NAS is shutting down", false
	}
	return http.StatusInternalServerError, err.Error(), true
}

func (h *thumbHandlers) serve(size int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sha := r.PathValue("sha")
		if !thumbs.ValidSHA(sha) {
			writeError(w, http.StatusBadRequest, "sha must be 64 lowercase hex digits")
			return
		}
		src, err := h.source(r.Context(), sha)
		switch {
		case errors.Is(err, catalog.ErrNotFound):
			writeError(w, http.StatusNotFound, "no file with hash "+sha)
			return
		case errors.Is(err, errNoFile):
			writeError(w, http.StatusNotFound, err.Error())
			return
		case err != nil:
			h.log.Error("thumbnail source", "sha", sha, "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		path, err := h.cache.Get(r.Context(), src, size)
		if err != nil {
			if r.Context().Err() != nil {
				return // the client went away; there is nobody to answer
			}
			code, msg, logIt := thumbFailure(err)
			if logIt {
				h.log.Error("thumbnail failed", "sha", sha, "size", size, "path", src.Path, "err", err)
			}
			writeError(w, code, msg)
			return
		}
		f, err := os.Open(path)
		if err != nil {
			h.log.Error("open thumbnail", "path", path, "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer func() {
			if err := f.Close(); err != nil {
				h.log.Warn("close thumbnail", "path", path, "err", err)
			}
		}()
		fi, err := f.Stat()
		if err != nil {
			h.log.Error("stat thumbnail", "path", path, "err", err)
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable") // the URL is content-addressed
		w.Header().Set("ETag", fmt.Sprintf(`"%s-%d"`, sha, size))
		http.ServeContent(w, r, "", fi.ModTime(), f)
	}
}

// enqueue runs on the ingest feed goroutine and must not block.
func (h *thumbHandlers) enqueue(in catalog.Ingest) {
	if in.Counts.New == 0 {
		return
	}
	select {
	case h.warm <- in.ID:
	default:
		h.log.Warn("thumbnail warm-up queue is full; thumbnails will be made when first viewed", "ingest", in.ID)
	}
}

func (h *thumbHandlers) warmLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case id := <-h.warm:
			h.warmIngest(ctx, id)
		}
	}
}

// warmIngest makes the small thumbnail of each new file of ingest id, one
// at a time, so it never takes more than one render slot.
func (h *thumbHandlers) warmIngest(ctx context.Context, id int64) {
	files, err := h.db.IngestFiles(ctx, id)
	if err != nil {
		if ctx.Err() == nil {
			h.log.Error("thumbnail warm-up: read ingest files", "ingest", id, "err", err)
		}
		return
	}
	made := 0
	for _, f := range files {
		if f.Outcome != catalog.OutcomeNew || f.SHA256 == "" {
			continue
		}
		src, err := h.source(ctx, f.SHA256)
		if err == nil {
			_, err = h.cache.Get(ctx, src, thumbs.ThumbSize)
		}
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			h.log.Warn("thumbnail warm-up failed", "ingest", id, "sha", f.SHA256, "err", err)
			continue
		}
		made++
	}
	h.log.Info("thumbnails ready", "ingest", id, "count", made)
}
```

In `server.go`, add a `warnings []string` field and `AddWarning`. In `handleHealthz`, append `s.warnings...` to `h.Warnings` after the TZ check.

- [ ] **Step 4: Wire it into `main.go`**

After `srv.MountStaging(...)`:

```go
		cache, err := thumbs.New(thumbs.Config{Dir: filepath.Join(cfg.Data, "thumbs"), Log: log})
		if err != nil {
			log.Error("thumbnail cache unavailable", "err", err)
			srv.AddProblem("thumbnails: " + err.Error())
		} else {
			for _, w := range cache.MissingTools() {
				log.Warn(w)
				srv.AddWarning(w)
			}
			srv.MountThumbs(db, cache, cfg.Photos) // srv.Close closes the cache
		}
```

- [ ] **Step 5: Run everything**

Run the full Windows set, the Linux command and the race command → PASS.

Smoke-test on Windows. `mkdir -p tmp/photos tmp/config`, then `go run ./cmd/sd2nas --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config --addr :18080` in the background. Then:

```bash
curl -s -X POST "http://localhost:18080/api/ingest?wait=1&format=text" -H "X-SD2NAS: 1" -H "Content-Type: application/json" -d "{\"path\":\"$(pwd -W)/testdata/cards/SONY_A7IV\"}"
curl -s http://localhost:18080/api/staging
curl -s http://localhost:18080/healthz   # warnings include "exiftool not found: RAW thumbnails are unavailable"
```

The `samples[0].sha256` from `/api/staging` gives 200 `image/jpeg` at `/api/thumbs/<sha>`. `curl -sN --max-time 2 http://localhost:18080/api/events` prints `retry: 3000` and an `ingest.current` frame. Stop the server with Ctrl-C (or `kill`). It must exit within a couple of seconds, which proves SSE doesn't block shutdown. Then `rm -rf tmp/photos tmp/config`.

- [ ] **Step 6: Commit**

```bash
git add internal/api/thumbs.go internal/api/thumbs_test.go internal/api/server.go internal/api/server_test.go internal/api/full_test.go cmd/sd2nas/main.go
git commit -m "Serve cached thumbnails and previews, warm them after each import, and warn when tools are missing

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: Web app scaffold: Vite + Svelte, app shell, router, API and live-event modules

Suggested implementer: sonnet.

**Files:**
- Create: `web/package.json`, `web/package-lock.json` (by npm), `web/vite.config.js`, `web/index.html`, `web/scripts/clean-dist.mjs`, `web/src/main.js`, `web/src/app.css`, `web/src/App.svelte`, `web/src/lib/api.js`, `web/src/lib/api.test.js`, `web/src/lib/format.js`, `web/src/lib/format.test.js`, `web/src/lib/paths.js`, `web/src/lib/paths.test.js`, `web/src/lib/router.svelte.js`, `web/src/lib/live.svelte.js`, `web/src/components/Nav.svelte`, `web/src/components/HealthBanner.svelte`, and the stub pages `web/src/pages/Ingest.svelte`, `IngestDetail.svelte`, `Staging.svelte`, `Folder.svelte`
- Modify: `web/embed_test.go` (add `TestBuiltUI`)

**Interfaces:**
- Consumes: the HTTP API in the Global Constraints. `/healthz` returns JSON `{status:"ok"|"degraded", problems?:[], warnings?:[], version, …}` with 200 or 503. `GET /api/ingest/current` and the `ingest.current`/`ingest.finished` events (Tasks 2–3). `web.Dist()` (Task 1).
- Produces (Tasks 10–11 import these; every name is exact):

```js
// src/lib/api.js
export class ApiError extends Error { status }            // status 0 = network failure
export async function api(path, { method = 'GET', body, signal } = {}) // → parsed JSON; throws ApiError
export async function getHealth()                          // → /healthz JSON for 200 or 503
// src/lib/paths.js
export function match(pattern, path)                       // '/staging/:id' → { id } | null
export function isId(s)                                    // positive integer string
// src/lib/format.js
export function formatBytes(n), formatCount(n), formatWhen(iso), formatCapture(s), dateRange(dates)
export function phaseSteps(phase), progressFraction(p)
export function statusLabel(status), levelClass(level), statusClass(ingest), cardName(ingest), knownCount(counts)
export function sessionFolder(relPath), groupKnown(files), outcomeFiles(files, outcome)
export function kindChips(counts), extLabel(file)
// src/lib/router.svelte.js
export const route        // $state { path }
export function navigate(path, { replace = false } = {})
// src/lib/live.svelte.js
export const live         // $state { connected, current: {running, queued}, finished, finishedCount }
export function connectEvents()
```

- [ ] **Step 1: The npm project**

`web/package.json`:

```json
{
  "name": "sd2nas-web",
  "private": true,
  "version": "0.0.0",
  "type": "module",
  "scripts": {
    "dev": "vite",
    "build": "node scripts/clean-dist.mjs && vite build",
    "test": "node --test \"src/lib/*.test.js\""
  },
  "devDependencies": {
    "@sveltejs/vite-plugin-svelte": "7.3.1",
    "svelte": "5.59.1",
    "vite": "8.3.8"
  }
}
```

In `web/`, run `npm install`. It creates `package-lock.json` (commit it) and `node_modules/` (ignored). No other package may appear in `package.json`.

`web/scripts/clean-dist.mjs`:

```js
// Empties dist/ before a build but keeps dist/.placeholder, which go:embed
// needs on a fresh checkout (vite.config.js sets emptyOutDir: false).
import { mkdirSync, readdirSync, rmSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

const dist = join(dirname(fileURLToPath(import.meta.url)), '..', 'dist');
mkdirSync(dist, { recursive: true });
for (const name of readdirSync(dist)) {
  if (name !== '.placeholder') rmSync(join(dist, name), { recursive: true, force: true });
}
```

`web/vite.config.js`:

```js
import { defineConfig } from 'vite';
import { svelte } from '@sveltejs/vite-plugin-svelte';

// `npm run dev` proxies the API to a Go server started with
// `go run ./cmd/sd2nas … --addr :8080` (override with SD2NAS_API). The Host
// header is kept (no changeOrigin), so the request guard's Origin check passes.
const api = process.env.SD2NAS_API || 'http://localhost:8080';

export default defineConfig({
  plugins: [svelte({ compilerOptions: { runes: true } })],
  build: {
    outDir: 'dist',
    assetsDir: 'assets',
    emptyOutDir: false, // scripts/clean-dist.mjs empties it but keeps .placeholder
  },
  server: {
    proxy: {
      '/api': { target: api },
      '/healthz': { target: api },
    },
  },
});
```

`web/index.html`:

```html
<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1, viewport-fit=cover">
<meta name="theme-color" content="#0f151e">
<title>SD2NAS</title>
</head>
<body>
<div id="app"></div>
<script type="module" src="/src/main.js"></script>
</body>
</html>
```

`web/src/main.js`:

```js
import { mount } from 'svelte';
import './app.css';
import App from './App.svelte';

mount(App, { target: document.getElementById('app') });
```

- [ ] **Step 2: Write the failing JS tests**

`web/src/lib/paths.test.js`:

```js
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { match, isId } from './paths.js';

test('match', () => {
  assert.deepEqual(match('/staging/:id', '/staging/3'), { id: '3' });
  assert.deepEqual(match('/ingest/:id', '/ingest/a%20b'), { id: 'a b' });
  assert.equal(match('/staging/:id', '/staging'), null);
  assert.equal(match('/staging/:id', '/staging/'), null);
  assert.equal(match('/staging/:id', '/staging/3/x'), null);
  assert.equal(match('/staging/:id', '/ingest/3'), null);
  assert.equal(match('/staging/:id', '/staging/%E0'), null); // malformed escape
});

test('isId', () => {
  assert.equal(isId('1'), true);
  assert.equal(isId('42'), true);
  for (const s of ['0', '-1', '01', '1.5', 'abc', '', undefined]) assert.equal(isId(s), false, String(s));
});
```

`web/src/lib/api.test.js` stubs `globalThis.fetch` and records its calls:

```js
import { test, beforeEach } from 'node:test';
import assert from 'node:assert/strict';
import { api, getHealth, ApiError } from './api.js';

let calls;
function respond(body, init) {
  globalThis.fetch = async (url, opts) => {
    calls.push({ url, opts });
    return new Response(body, init);
  };
}
beforeEach(() => { calls = []; });

test('GET sends no guard header', async () => {
  respond('{"ok":true}', { status: 200 });
  assert.deepEqual(await api('/api/staging'), { ok: true });
  assert.equal(calls[0].opts.method, 'GET');
  assert.equal(calls[0].opts.headers['X-SD2NAS'], undefined);
});

test('every non-GET sends X-SD2NAS: 1', async () => {
  for (const method of ['POST', 'PATCH', 'PUT', 'DELETE']) {
    respond('{}', { status: 200 });
    await api('/api/x', { method, body: { a: 1 } });
    const { opts } = calls.at(-1);
    assert.equal(opts.headers['X-SD2NAS'], '1', method);
    assert.equal(opts.headers['Content-Type'], 'application/json');
    assert.equal(opts.body, '{"a":1}');
  }
});

test('a JSON error becomes ApiError with the server message', async () => {
  respond('{"error":"no ingest 9"}', { status: 404 });
  await assert.rejects(api('/api/ingests/9'), (err) => err instanceof ApiError && err.status === 404 && err.message === 'no ingest 9');
});

test('a non-JSON error uses the status line', async () => {
  respond('oops', { status: 500, statusText: 'Internal Server Error' });
  await assert.rejects(api('/api/x'), { name: 'ApiError', status: 500, message: '500 Internal Server Error' });
});

test('a 200 that is not JSON is an error', async () => {
  respond('<!doctype html>', { status: 200 });
  await assert.rejects(api('/api/x'), { name: 'ApiError', status: 200 });
});

test('a network failure is status 0', async () => {
  globalThis.fetch = async () => { throw new TypeError('fetch failed'); };
  await assert.rejects(api('/api/x'), (err) => err.status === 0 && err.message.startsWith('SD2NAS is not reachable'));
});

test('getHealth returns the report for 503 too', async () => {
  respond('{"status":"degraded","problems":["catalog: boom"]}', { status: 503 });
  assert.deepEqual(await getHealth(), { status: 'degraded', problems: ['catalog: boom'] });
});
```

`web/src/lib/format.test.js` (set the zone first, so `formatWhen` is deterministic):

```js
process.env.TZ = 'Pacific/Chatham'; // +13:45 in November
import { test } from 'node:test';
import assert from 'node:assert/strict';
import * as f from './format.js';

test('formatBytes', () => {
  assert.equal(f.formatBytes(0), '0 B');
  assert.equal(f.formatBytes(999), '999 B');
  assert.equal(f.formatBytes(1000), '1.0 KB');
  assert.equal(f.formatBytes(1234567), '1.2 MB');
  assert.equal(f.formatBytes(5e10), '50.0 GB');
  assert.equal(f.formatBytes(undefined), '0 B');
});

test('formatWhen and formatCapture', () => {
  assert.equal(f.formatWhen('2026-11-21T06:42:05.000Z'), '2026-11-21 20:27');
  assert.equal(f.formatWhen(''), '');
  assert.equal(f.formatCapture('2025-05-31T01:30:12'), '2025-05-31 01:30:12');
});

test('dateRange', () => {
  assert.equal(f.dateRange([]), '');
  assert.equal(f.dateRange(['2025-05-30']), '2025-05-30');
  assert.equal(f.dateRange(['2025-06-01', '2025-05-30']), '2025-05-30 → 2025-06-01');
});

test('phaseSteps', () => {
  const states = (p) => f.phaseSteps(p).map((s) => s.state).join(',');
  assert.equal(states('queued'), 'todo,todo,todo,todo');
  assert.equal(states('scanning'), 'current,todo,todo,todo');
  assert.equal(states('copying'), 'done,done,current,todo');
  assert.equal(states('done'), 'done,done,done,done');
  assert.deepEqual(f.phaseSteps('classifying').map((s) => s.key), ['scanning', 'classifying', 'copying', 'finishing']);
});

test('progressFraction', () => {
  assert.equal(f.progressFraction(null), 0);
  assert.equal(f.progressFraction({ phase: 'classifying', files_done: 1, files_total: 4 }), 0.25);
  assert.equal(f.progressFraction({ phase: 'copying', files_done: 1, files_total: 2, bytes_done: 30, bytes_total: 40 }), 0.75);
  assert.equal(f.progressFraction({ phase: 'copying', files_done: 1, files_total: 2, bytes_done: 0, bytes_total: 0 }), 0.5);
  assert.equal(f.progressFraction({ phase: 'classifying', files_done: 0, files_total: 0 }), 0);
  assert.equal(f.progressFraction({ phase: 'finishing' }), 1);
});

test('labels and classes', () => {
  assert.equal(f.statusLabel('running'), 'Importing');
  assert.equal(f.statusLabel('refused'), 'Refused');
  assert.equal(f.levelClass('normal'), 'ok');
  assert.equal(f.levelClass('warning'), 'warn');
  assert.equal(f.levelClass('alert'), 'bad');
  assert.equal(f.statusClass({ status: 'running', level: 'normal' }), 'live');
  assert.equal(f.statusClass({ status: 'done', level: 'warning' }), 'warn');
  assert.equal(f.cardName({ card_label: 'SONY_A7IV', card_path: '/cards/X' }), 'SONY_A7IV');
  assert.equal(f.cardName({ card_label: '', card_path: '/cards/GOPRO' }), 'GOPRO');
  assert.equal(f.knownCount({ in_archive: 3, in_staging: 2, seen_before: 1 }), 6);
});

test('sessionFolder and groupKnown', () => {
  assert.equal(f.sessionFolder('Archive/2026-11-08 - Big Sur/Capture/x.ARW'), 'Archive/2026-11-08 - Big Sur');
  assert.equal(f.sessionFolder('Staging/2025-05-30/a.JPG'), 'Staging/2025-05-30');
  assert.equal(f.sessionFolder('Archive/x.JPG'), 'Archive');
  assert.equal(f.sessionFolder(''), '');
  const files = [
    { outcome: 'in_archive', rel_path: 'Archive/B/x.JPG' },
    { outcome: 'in_archive', rel_path: 'Archive/A/Capture/y.ARW' },
    { outcome: 'in_archive', rel_path: 'Archive/A/z.JPG' },
    { outcome: 'in_staging', rel_path: 'Staging/2025-05-30/a.JPG' },
    { outcome: 'seen_before' },
    { outcome: 'in_archive' },
  ];
  assert.deepEqual(f.groupKnown(files), [
    { folder: 'Archive/A', count: 2 },
    { folder: '(location unknown)', count: 1 },
    { folder: 'Archive/B', count: 1 },
    { folder: 'Staging/2025-05-30', count: 1 },
  ]);
  assert.deepEqual(f.outcomeFiles(files, 'seen_before'), [{ outcome: 'seen_before' }]);
});

test('kindChips and extLabel', () => {
  assert.deepEqual(f.kindChips({ jpg: 2, raw: 0, video: 1 }), [
    { kind: '', label: 'All', count: 3 },
    { kind: 'jpg', label: 'JPG', count: 2 },
    { kind: 'video', label: 'Video', count: 1 },
  ]);
  assert.equal(f.extLabel({ name: '2025-05-31 01.30.12.ARW' }), 'ARW');
  assert.equal(f.extLabel({ name: 'clip - 3840w - 29.970 - 12sec.mp4' }), 'MP4');
  assert.equal(f.extLabel({ name: 'noext' }), '');
});
```

Run `npm test` in `web/`. Expected: FAIL, because the modules don't exist yet.

- [ ] **Step 3: Implement the plain modules**

`web/src/lib/paths.js`:

```js
// match('/staging/:id', '/staging/3') → { id: '3' }; null when path doesn't fit.
export function match(pattern, path) {
  const ps = pattern.split('/');
  const xs = path.split('/');
  if (ps.length !== xs.length) return null;
  const params = {};
  for (let i = 0; i < ps.length; i++) {
    if (ps[i].startsWith(':')) {
      if (xs[i] === '') return null;
      try {
        params[ps[i].slice(1)] = decodeURIComponent(xs[i]);
      } catch {
        return null; // a malformed %-escape is just a path that doesn't match
      }
    } else if (ps[i] !== xs[i]) {
      return null;
    }
  }
  return params;
}

// isId reports whether s is a positive integer without leading zeros.
export function isId(s) {
  return typeof s === 'string' && /^[1-9][0-9]*$/.test(s);
}
```

`web/src/lib/api.js`:

```js
// fetch wrapper for the SD2NAS API. Every request that isn't GET or HEAD
// carries X-SD2NAS: 1, without which the server's request guard refuses it.
// Failures become ApiError with the server's message.
export class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.name = 'ApiError';
    this.status = status; // 0 = no response (network failure)
  }
}

function parseJSON(text) {
  try {
    return { ok: true, value: JSON.parse(text) };
  } catch (error) {
    return { ok: false, error };
  }
}

async function send(path, init) {
  try {
    return await fetch(path, init);
  } catch (err) {
    if (err.name === 'AbortError') throw err;
    throw new ApiError(0, `SD2NAS is not reachable: ${err.message}`);
  }
}

export async function api(path, { method = 'GET', body, signal } = {}) {
  const headers = { Accept: 'application/json' };
  if (method !== 'GET' && method !== 'HEAD') headers['X-SD2NAS'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await send(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body), signal });
  const parsed = parseJSON(await res.text());
  if (!res.ok) {
    const msg = parsed.ok && parsed.value && typeof parsed.value.error === 'string' ? parsed.value.error : `${res.status} ${res.statusText}`;
    throw new ApiError(res.status, msg);
  }
  if (!parsed.ok) throw new ApiError(res.status, `${path} did not answer with JSON: ${parsed.error.message}`);
  return parsed.value;
}

// getHealth returns /healthz's report: it is JSON for 200 (ok) and 503 (degraded).
export async function getHealth() {
  const res = await send('/healthz', { headers: { Accept: 'application/json' } });
  const parsed = parseJSON(await res.text());
  if (!parsed.ok) throw new ApiError(res.status, `/healthz answered ${res.status} without JSON`);
  return parsed.value;
}
```

`web/src/lib/format.js`:

```js
// Pure formatting helpers for the UI: no DOM and no Svelte, tested with node --test.

const UNITS = ['KB', 'MB', 'GB', 'TB'];
const pad = (n) => String(n).padStart(2, '0');

// formatBytes(1234567) === '1.2 MB' (decimal units, like the ingest messages).
export function formatBytes(n) {
  if (!Number.isFinite(n) || n < 1000) return `${Number.isFinite(n) ? Math.max(0, Math.round(n)) : 0} B`;
  let v = n;
  let i = -1;
  while (v >= 1000 && i < UNITS.length - 1) {
    v /= 1000;
    i++;
  }
  return `${v.toFixed(1)} ${UNITS[i]}`;
}

export const formatCount = (n) => Number(n ?? 0).toLocaleString('en-US');

// formatWhen renders an RFC 3339 instant in the browser's zone: "2026-11-21 19:42".
export function formatWhen(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

// formatCapture shows a capture wall clock ("2025-05-31T01:30:12") as named, with no zone conversion.
export const formatCapture = (s) => (s ? s.replace('T', ' ') : '');

export function dateRange(dates) {
  const d = [...(dates ?? [])].sort();
  if (d.length === 0) return '';
  return d.length === 1 ? d[0] : `${d[0]} → ${d[d.length - 1]}`;
}

const PHASES = [
  ['scanning', 'Reading the card'],
  ['classifying', 'Checking against the catalog'],
  ['copying', 'Copying new files'],
  ['finishing', 'Finishing'],
];

// phaseSteps lists the import phases with state 'done' | 'current' | 'todo'.
export function phaseSteps(phase) {
  const at = phase === 'done' ? PHASES.length : PHASES.findIndex(([k]) => k === phase);
  return PHASES.map(([key, label], i) => ({
    key,
    label,
    state: at < 0 ? 'todo' : i < at ? 'done' : i === at ? 'current' : 'todo',
  }));
}

const frac = (done, total) => (total > 0 ? Math.min(1, Math.max(0, done / total)) : 0);

// progressFraction is the progress bar value for a ProgressSnapshot (0–1).
export function progressFraction(p) {
  if (!p) return 0;
  switch (p.phase) {
    case 'classifying':
      return frac(p.files_done, p.files_total);
    case 'copying':
      return p.bytes_total > 0 ? frac(p.bytes_done, p.bytes_total) : frac(p.files_done, p.files_total);
    case 'finishing':
    case 'done':
      return 1;
    default:
      return 0;
  }
}

const STATUS = { queued: 'Waiting', running: 'Importing', done: 'Done', failed: 'Failed', refused: 'Refused', cancelled: 'Cancelled' };
export const statusLabel = (s) => STATUS[s] ?? s;
export const levelClass = (level) => (level === 'alert' ? 'bad' : level === 'warning' ? 'warn' : 'ok');
export const statusClass = (i) => (i.status === 'queued' || i.status === 'running' ? 'live' : levelClass(i.level));

export function cardName(i) {
  if (i.card_label) return i.card_label;
  const parts = (i.card_path ?? '').split(/[\\/]/).filter(Boolean);
  return parts.at(-1) ?? 'card';
}

export const knownCount = (c) => (c.in_archive ?? 0) + (c.in_staging ?? 0) + (c.seen_before ?? 0);

// sessionFolder: 'Archive/2026-11-08 - Big Sur/Capture/x.ARW' → 'Archive/2026-11-08 - Big Sur'.
export function sessionFolder(relPath) {
  if (!relPath) return '';
  const parts = relPath.split('/');
  return parts.length >= 3 ? parts.slice(0, 2).join('/') : parts.slice(0, -1).join('/');
}

// groupKnown counts the already-known files (in_archive, in_staging) by the
// session folder they live in, biggest first, then by name.
export function groupKnown(files) {
  const counts = new Map();
  for (const f of files) {
    if (f.outcome !== 'in_archive' && f.outcome !== 'in_staging') continue;
    const folder = sessionFolder(f.rel_path) || '(location unknown)';
    counts.set(folder, (counts.get(folder) ?? 0) + 1);
  }
  return [...counts]
    .map(([folder, count]) => ({ folder, count }))
    .sort((a, b) => b.count - a.count || (a.folder < b.folder ? -1 : a.folder > b.folder ? 1 : 0));
}

export const outcomeFiles = (files, outcome) => files.filter((f) => f.outcome === outcome);

// kindChips are the folder view's filter chips: All, then each kind present.
export function kindChips(c) {
  const chips = [{ kind: '', label: 'All', count: c.jpg + c.raw + c.video }];
  for (const [kind, label] of [['raw', 'RAW'], ['jpg', 'JPG'], ['video', 'Video']]) {
    if (c[kind]) chips.push({ kind, label, count: c[kind] });
  }
  return chips;
}

export function extLabel(file) {
  const dot = file.name.lastIndexOf('.');
  return dot < 0 ? '' : file.name.slice(dot + 1).toUpperCase();
}
```

Run `npm test`. Expected: PASS. Every test passes, including `formatWhen` under `Pacific/Chatham`.

- [ ] **Step 4: The Svelte state modules**

`web/src/lib/router.svelte.js`:

```js
// A tiny History API router. App.svelte intercepts same-origin link
// clicks, so a plain <a href="/staging/3"> navigates in place.
export const route = $state({ path: location.pathname });

export function navigate(path, { replace = false } = {}) {
  if (path === route.path) return;
  if (replace) history.replaceState(null, '', path);
  else history.pushState(null, '', path);
  route.path = path;
  window.scrollTo(0, 0);
}

window.addEventListener('popstate', () => {
  route.path = location.pathname;
});
```

`web/src/lib/live.svelte.js`:

```js
// Live server state pushed over /api/events (server-sent events).
export const live = $state({
  connected: false,
  current: { running: null, queued: [] }, // the last ingest.current
  finished: null, // the last ingest.finished
  finishedCount: 0, // bumps on every ingest.finished; pages reload on it
});

let source = null;
let retry = 0;

export function connectEvents() {
  if (source) return;
  source = new EventSource('/api/events');
  source.onopen = () => {
    live.connected = true;
  };
  source.onerror = () => {
    live.connected = false;
    // While readyState is CONNECTING, EventSource retries by itself. A
    // CLOSED stream (an HTTP error or wrong content type) is reopened here.
    if (source.readyState === EventSource.CLOSED) {
      source = null;
      clearTimeout(retry);
      retry = setTimeout(connectEvents, 5000);
    }
  };
  source.addEventListener('ingest.current', (e) => {
    live.current = JSON.parse(e.data);
  });
  source.addEventListener('ingest.finished', (e) => {
    live.finished = JSON.parse(e.data);
    live.finishedCount += 1;
  });
}

// Phones kill background connections; reconnect when the app is shown again.
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible' && !source) connectEvents();
});
```

- [ ] **Step 5: Global styles** (`web/src/app.css`)

```css
:root {
  --bg: #141c27; --nav: #0f151e; --panel: #1b2636; --panel2: #223047; --line: #2e3d55;
  --text: #e6edf6; --muted: #8d9bb0; --accent: #4f9cf9; --accent2: #3a7fd6;
  --ok: #3fbf7f; --warn: #e8b04a; --bad: #e5636b; --r: 10px;
  --tabbar: 64px;
  color-scheme: dark;
}
* { box-sizing: border-box; }
html, body { margin: 0; background: var(--bg); color: var(--text); }
body { font: 14px/1.45 system-ui, -apple-system, "Segoe UI", Roboto, sans-serif; -webkit-tap-highlight-color: transparent; }
body.noscroll { overflow: hidden; }
a { color: var(--accent); }
h1 { font-size: 20px; margin: 0 0 4px; overflow-wrap: anywhere; }
h2 { font-size: 15px; margin: 0 0 12px; }
.sub { color: var(--muted); margin: 0 0 18px; }
.muted { color: var(--muted); }
.small { font-size: 12px; }
.mono { font-family: ui-monospace, Consolas, monospace; font-size: 12px; overflow-wrap: anywhere; }
.card { background: var(--panel); border: 1px solid var(--line); border-radius: var(--r); padding: 16px; margin-bottom: 16px; }
.row { display: flex; gap: 10px; align-items: center; flex-wrap: wrap; }
.spacer { flex: 1; }
.btn { background: var(--panel2); color: var(--text); border: 1px solid var(--line); padding: 7px 13px; border-radius: 8px; cursor: pointer; font: inherit; white-space: nowrap; text-decoration: none; display: inline-flex; align-items: center; gap: 6px; min-height: 40px; }
.btn:hover { border-color: var(--accent); }
.stats { display: grid; grid-template-columns: repeat(auto-fit, minmax(140px, 1fr)); gap: 12px; }
.stat { background: var(--panel2); border-radius: 8px; padding: 12px 14px; }
.stat b { display: block; font-size: 22px; }
.stat.ok b { color: var(--ok); } .stat.warn b { color: var(--warn); } .stat.bad b { color: var(--bad); } .stat.acc b { color: var(--accent); }
.bar { height: 8px; background: var(--panel2); border-radius: 99px; overflow: hidden; margin: 8px 0; }
.bar > i { display: block; height: 100%; background: var(--accent); transition: width .3s; }
.waiting { border: 2px dashed var(--line); border-radius: var(--r); padding: 28px 16px; text-align: center; }
.phases { display: flex; gap: 6px 18px; flex-wrap: wrap; margin: 10px 0 4px; font-size: 13px; }
.phase { color: var(--muted); } .phase.current { color: var(--accent); font-weight: 600; } .phase.done { color: var(--ok); }
table { width: 100%; border-collapse: collapse; }
th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--line); }
th { color: var(--muted); font-weight: 500; font-size: 12px; }
td.num, th.num { text-align: right; font-variant-numeric: tabular-nums; }
.tag { display: inline-block; background: var(--panel2); border: 1px solid var(--line); border-radius: 6px; padding: 0 6px; font-size: 11px; color: var(--muted); margin-right: 4px; }
.pill { display: inline-block; border-radius: 999px; font-size: 11px; padding: 1px 8px; background: var(--panel2); color: var(--muted); white-space: nowrap; }
.pill.ok { background: rgba(63,191,127,.15); color: var(--ok); }
.pill.warn { background: rgba(232,176,74,.15); color: var(--warn); }
.pill.bad { background: rgba(229,99,107,.15); color: var(--bad); }
.pill.live { background: var(--warn); color: #1b1b1b; }
details { background: var(--panel2); border-radius: 8px; padding: 10px 12px; margin-top: 10px; }
summary { cursor: pointer; min-height: 24px; }
details ul { margin: 8px 0 0; padding-left: 18px; color: var(--muted); }
.note { background: rgba(79,156,249,.1); border: 1px solid rgba(79,156,249,.35); border-radius: 8px; padding: 9px 11px; font-size: 13px; color: #b9d6fb; margin: 10px 0; }
.note.ok { background: rgba(63,191,127,.1); border-color: rgba(63,191,127,.35); color: #a9e7c6; }
.note.warn { background: rgba(232,176,74,.1); border-color: rgba(232,176,74,.35); color: #f0d49b; }
.note.bad { background: rgba(229,99,107,.1); border-color: rgba(229,99,107,.4); color: #f3b3b7; }
.chips { display: flex; gap: 6px; flex-wrap: wrap; }
.chip { background: var(--panel2); border: 1px solid var(--line); color: var(--muted); border-radius: 99px; padding: 6px 12px; cursor: pointer; font: inherit; font-size: 13px; min-height: 40px; }
.chip.on { color: var(--text); border-color: var(--accent); }
.toolbar { position: sticky; top: 0; z-index: 5; background: var(--bg); padding: 10px 0; margin-bottom: 10px; border-bottom: 1px solid var(--line); }
.only-phone { display: none; }
@media (max-width: 760px) {
  .only-desktop { display: none; }
  .only-phone { display: block; }
}
```

- [ ] **Step 6: The app shell**

`web/src/App.svelte`:

```svelte
<script>
  import { onMount } from 'svelte';
  import { route, navigate } from './lib/router.svelte.js';
  import { match } from './lib/paths.js';
  import { connectEvents } from './lib/live.svelte.js';
  import { getHealth } from './lib/api.js';
  import Nav from './components/Nav.svelte';
  import HealthBanner from './components/HealthBanner.svelte';
  import Ingest from './pages/Ingest.svelte';
  import IngestDetail from './pages/IngestDetail.svelte';
  import Staging from './pages/Staging.svelte';
  import Folder from './pages/Folder.svelte';

  let health = $state(null);
  let healthError = $state('');

  async function refreshHealth() {
    try {
      health = await getHealth();
      healthError = '';
    } catch (err) {
      healthError = err.message;
    }
  }

  onMount(() => {
    connectEvents();
    refreshHealth();
    const timer = setInterval(refreshHealth, 60_000);
    return () => clearInterval(timer);
  });

  // Same-origin links navigate in place. Modified clicks, new tabs,
  // downloads, /api/ URLs and /healthz are left to the browser.
  function onclick(e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target instanceof Element ? e.target.closest('a[href]') : null;
    if (!a || a.target || a.hasAttribute('download')) return;
    const url = new URL(a.href);
    if (url.origin !== location.origin || url.pathname.startsWith('/api/') || url.pathname === '/healthz') return;
    e.preventDefault();
    navigate(url.pathname);
  }

  const page = $derived.by(() => {
    const p = route.path;
    if (p === '/' || p === '/ingest') return { tab: 'ingest', view: 'ingest' };
    let m = match('/ingest/:id', p);
    if (m) return { tab: 'ingest', view: 'ingest-detail', id: m.id };
    if (p === '/staging') return { tab: 'staging', view: 'staging' };
    m = match('/staging/:id', p);
    if (m) return { tab: 'staging', view: 'folder', id: m.id };
    return { tab: '', view: 'not-found' };
  });
</script>

<svelte:document {onclick} />

<div class="app">
  <Nav tab={page.tab} version={health?.version ?? ''} />
  <main>
    <HealthBanner {health} error={healthError} />
    {#if page.view === 'ingest'}
      <Ingest />
    {:else if page.view === 'ingest-detail'}
      {#key page.id}<IngestDetail id={page.id} />{/key}
    {:else if page.view === 'staging'}
      <Staging />
    {:else if page.view === 'folder'}
      {#key page.id}<Folder id={page.id} />{/key}
    {:else}
      <h1>Page not found</h1>
      <p class="sub">Nothing lives at <span class="mono">{route.path}</span>. <a href="/">Go to Card ingest</a></p>
    {/if}
  </main>
</div>

<style>
  .app { display: grid; grid-template-columns: 220px minmax(0, 1fr); min-height: 100vh; min-height: 100dvh; }
  main { padding: 22px 28px 60px; max-width: 1240px; width: 100%; }
  @media (max-width: 760px) {
    .app { grid-template-columns: minmax(0, 1fr); }
    main { padding: 16px 16px calc(var(--tabbar) + 24px + env(safe-area-inset-bottom)); }
  }
</style>
```

`web/src/components/Nav.svelte`: a sidebar on desktop and a bottom tab bar on phones. Card ingest and Staging are the only M3 tabs.

```svelte
<script>
  import { live } from '../lib/live.svelte.js';

  let { tab, version } = $props();
  const importing = $derived(live.current.running !== null || live.current.queued.length > 0);
</script>

<nav class="nav" aria-label="Main">
  <div class="brand"><span class="dot" class:off={!live.connected}></span>SD2NAS</div>
  <a href="/ingest" class="tab" class:active={tab === 'ingest'} aria-current={tab === 'ingest' ? 'page' : undefined}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M7 2h8l4 4v15a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1V3a1 1 0 0 1 1-1z" /><path d="M9 5v3M12 5v3M15 5v3" /><path d="M12 11v7M9 15l3 3 3-3" /></svg>
    <span class="label">Card ingest</span>
    {#if importing}<span class="pill live">live</span>{/if}
  </a>
  <a href="/staging" class="tab" class:active={tab === 'staging'} aria-current={tab === 'staging' ? 'page' : undefined}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M3 6a1 1 0 0 1 1-1h5l2 2h9a1 1 0 0 1 1 1v10a1 1 0 0 1-1 1H4a1 1 0 0 1-1-1z" /></svg>
    <span class="label">Staging</span>
  </a>
  <div class="foot">
    {live.connected ? 'Live updates on' : 'Reconnecting…'}
    {#if version}<br />Version <span class="mono">{version}</span>{/if}
  </div>
</nav>

<style>
  .nav { background: var(--nav); border-right: 1px solid var(--line); padding: 18px 12px; display: flex; flex-direction: column; gap: 4px; position: sticky; top: 0; height: 100vh; height: 100dvh; }
  .brand { font-weight: 700; font-size: 16px; padding: 4px 10px 16px; display: flex; align-items: center; gap: 8px; }
  .dot { width: 10px; height: 10px; border-radius: 50%; background: var(--ok); box-shadow: 0 0 8px var(--ok); }
  .dot.off { background: var(--muted); box-shadow: none; }
  .tab { color: var(--muted); text-decoration: none; padding: 9px 12px; border-radius: 8px; display: flex; align-items: center; gap: 10px; min-height: 44px; }
  .tab:hover { background: var(--panel); color: var(--text); }
  .tab.active { background: var(--panel2); color: var(--text); }
  .label { flex: 1; }
  svg { width: 20px; height: 20px; fill: none; stroke: currentColor; stroke-width: 1.8; stroke-linecap: round; stroke-linejoin: round; flex: none; }
  .foot { margin-top: auto; font-size: 12px; color: var(--muted); padding: 10px; line-height: 1.6; }
  @media (max-width: 760px) {
    .nav { position: fixed; top: auto; left: 0; right: 0; bottom: 0; height: auto; z-index: 10; flex-direction: row; justify-content: space-around; gap: 0; padding: 6px 8px calc(6px + env(safe-area-inset-bottom)); border-right: 0; border-top: 1px solid var(--line); }
    .brand, .foot { display: none; }
    .tab { flex: 1; max-width: 160px; flex-direction: column; justify-content: center; gap: 2px; padding: 6px 4px; min-height: 52px; font-size: 11px; position: relative; }
    .label { flex: none; }
    .tab.active { background: none; color: var(--accent); }
    .pill { position: absolute; top: 2px; left: calc(50% + 8px); }
  }
</style>
```

`web/src/components/HealthBanner.svelte`:

```svelte
<script>
  let { health, error } = $props();
</script>

{#if error}
  <div class="note bad" role="alert">SD2NAS is not reachable ({error}). Checking again every minute.</div>
{:else if health && health.status !== 'ok'}
  <div class="note bad" role="alert">
    <b>SD2NAS needs attention.</b> Imports and staging may be unavailable until this is fixed:
    <ul>
      {#each health.problems ?? [] as p}<li>{p}</li>{/each}
    </ul>
  </div>
{/if}
{#if health?.warnings?.length}
  <div class="note warn">
    {#each health.warnings as w}<div>{w}</div>{/each}
  </div>
{/if}

<style>
  ul { margin: 6px 0 0; padding-left: 18px; }
</style>
```

Stub pages, which Tasks 10 and 11 replace whole. `Ingest.svelte` is `<h1>Card ingest</h1>`, and `Staging.svelte` is `<h1>Staging</h1>`. `IngestDetail.svelte` and `Folder.svelte` are:

```svelte
<script>
  let { id } = $props();
</script>

<h1>Import {id}</h1>
```

(use `Staging folder {id}` in `Folder.svelte`).

- [ ] **Step 7: Build, and check the Go side sees the build**

In `web/`, run `npm run build`. Expected: it succeeds with no errors, and `dist/` holds `index.html`, `assets/index-<hash>.js`, `assets/index-<hash>.css` and still `.placeholder`. Fix any Svelte warning the build prints (a11y or otherwise), or explain in your report why it can't be fixed. `git status --short web/` must list only the new source files and `package-lock.json`: no `dist/` contents, and no change to `dist/.placeholder`.

Check that `node_modules` doesn't leak into Go. From the repo root, `go list ./... | grep -c node_modules` prints `0`. If it doesn't, stop and report: an npm package ships `.go` files.

Add to `web/embed_test.go`:

```go
// TestBuiltUI checks a real `npm run build` when one is present. CI's web
// job builds first and sets SD2NAS_REQUIRE_UI=1, so it can't be skipped there.
func TestBuiltUI(t *testing.T) {
	d := Dist()
	index, err := fs.ReadFile(d, "index.html")
	if errors.Is(err, fs.ErrNotExist) {
		if os.Getenv("SD2NAS_REQUIRE_UI") == "1" {
			t.Fatal("web/dist/index.html is missing: run npm run build in web/")
		}
		t.Skip("the UI is not built (npm run build in web/)")
	}
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`(?:src|href)="/(assets/[^"]+)"`).FindAllStringSubmatch(string(index), -1)
	if len(refs) == 0 {
		t.Fatalf("index.html references no /assets/ files:\n%s", index)
	}
	hashed := regexp.MustCompile(`-[A-Za-z0-9_-]{6,}\.(js|css)$`)
	for _, m := range refs {
		if _, err := fs.Stat(d, m[1]); err != nil {
			t.Errorf("index.html references %s, which is not in dist: %v", m[1], err)
		}
		if !hashed.MatchString(m[1]) {
			t.Errorf("%s has no content hash, so it can't be cached as immutable", m[1])
		}
	}
	if !strings.Contains(string(index), `<div id="app"></div>`) {
		t.Error(`index.html has no <div id="app"></div>`)
	}
	if _, err := fs.Stat(d, ".placeholder"); err != nil {
		t.Errorf("the build removed dist/.placeholder; go:embed fails on a fresh checkout without it: %v", err)
	}
}
```

Run `go test ./web/ -v`. With the build present, `TestBuiltUI` PASSes. Then delete `web/dist/*` except `.placeholder` (`node scripts/clean-dist.mjs`), and it SKIPs. Rebuild afterwards.

Manual smoke check (report what you saw). Run `go build -o tmp/sd2nas.exe ./cmd/sd2nas`, then `tmp/sd2nas.exe --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config --addr :18080`. `curl -s http://localhost:18080/ | grep -c 'id="app"'` prints 1, and `curl -sI http://localhost:18080/staging/1` is 200 `text/html`. Stop the server and remove `tmp/photos tmp/config tmp/sd2nas.exe`.

- [ ] **Step 8: Run everything**

In `web/`, run `npm test` and `npm run build` → OK. At the root, run the full Windows Go set and the Linux command → PASS. (The Linux container has no build, so `TestBuiltUI` skips there; CI's web job covers it.)

- [ ] **Step 9: Commit**

```bash
git add web/package.json web/package-lock.json web/vite.config.js web/index.html web/scripts web/src web/embed_test.go
git commit -m "Scaffold the Svelte web app: shell, router, API client and live events

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: PWA manifest and app icons

Suggested implementer: haiku.

**Files:**
- Create: `web/icons/gen.go` (`//go:build ignore`), `web/public/manifest.webmanifest`, `web/public/icon.svg`, `web/public/icon-192.png`, `web/public/icon-512.png`, `web/public/icon-maskable-512.png`, `web/public/apple-touch-icon.png` (generated), `web/icons_test.go`
- Modify: `web/index.html`, `web/embed_test.go` (extend `TestBuiltUI`)

**Interfaces:**
- Consumes: `web.Dist()` and `TestBuiltUI` (Tasks 1, 8), and Vite copying `web/public/*` to `dist/` unchanged (no hash). The UI handler serves `.webmanifest` as `application/manifest+json`, with no-cache (Task 1).
- Produces: the files above. `/manifest.webmanifest`, `/icon.svg`, `/icon-192.png`, `/icon-512.png`, `/icon-maskable-512.png` and `/apple-touch-icon.png` are served from the build.

- [ ] **Step 1: The icon generator** (`web/icons/gen.go`)

The design: a dark rounded square (`#141c27`) holding a blue (`#4f9cf9`) SD card with its top-right corner cut, four dark contacts, and a light (`#e6edf6`) down arrow. All geometry is in fractions of the icon size and matches `icon.svg`. The card's farthest corner is 0.36 from the centre, inside the 0.4 maskable safe zone, so the maskable icon is the same art on a full-bleed square.

```go
//go:build ignore

// Command gen draws SD2NAS's PNG app icons into web/public. Run it from the
// repo root after changing the design:  go run web/icons/gen.go
// The shapes match web/public/icon.svg.
package main

import (
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

var (
	bg    = color.NRGBA{0x14, 0x1c, 0x27, 0xff}
	card  = color.NRGBA{0x4f, 0x9c, 0xf9, 0xff}
	arrow = color.NRGBA{0xe6, 0xed, 0xf6, 0xff}
)

type pt struct{ x, y float64 }

// Card geometry in a unit square.
const (
	w, h   = 0.44, 0.56
	x0, y0 = 0.5 - w/2, 0.5 - h/2
	x1, y1 = x0 + w, y0 + h
	cut    = 0.3 * w
)

// shade is the colour at (x, y) in the unit square; ok is false where the
// icon is transparent (outside the rounded corners).
func shade(x, y float64, rounded bool) (c color.NRGBA, ok bool) {
	if rounded && !inRoundedSquare(x, y, 0.22) {
		return color.NRGBA{}, false
	}
	if !inConvex(x, y, []pt{{x0, y0}, {x1 - cut, y0}, {x1, y0 + cut}, {x1, y1}, {x0, y1}}) {
		return bg, true
	}
	s := (x1 - cut - 0.05*w - (x0 + 0.1*w)) / 5.5 // contact width; gaps are half a contact
	for i := 0; i < 4; i++ {
		sx := x0 + 0.1*w + float64(i)*1.5*s
		if x >= sx && x < sx+s && y >= y0+0.08*h && y < y0+0.28*h {
			return bg, true
		}
	}
	cx := (x0 + x1) / 2
	if x >= cx-0.06*w && x < cx+0.06*w && y >= y0+0.42*h && y < y0+0.66*h {
		return arrow, true
	}
	if inConvex(x, y, []pt{{cx - 0.22*w, y0 + 0.62*h}, {cx + 0.22*w, y0 + 0.62*h}, {cx, y0 + 0.86*h}}) {
		return arrow, true
	}
	return card, true
}

func inConvex(x, y float64, poly []pt) bool {
	var pos, neg bool
	for i := range poly {
		a, b := poly[i], poly[(i+1)%len(poly)]
		cross := (b.x-a.x)*(y-a.y) - (b.y-a.y)*(x-a.x)
		if cross > 0 {
			pos = true
		} else if cross < 0 {
			neg = true
		}
		if pos && neg {
			return false
		}
	}
	return true
}

func inRoundedSquare(x, y, r float64) bool {
	dx := math.Max(math.Max(r-x, x-(1-r)), 0)
	dy := math.Max(math.Max(r-y, y-(1-r)), 0)
	return dx*dx+dy*dy <= r*r
}

// render draws the icon at size px with 4×4 supersampling.
func render(size int, rounded bool) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	const ss = 4
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var r, g, b, n float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) / float64(size)
					y := (float64(py) + (float64(sy)+0.5)/ss) / float64(size)
					c, ok := shade(x, y, rounded)
					if !ok {
						continue
					}
					r, g, b, n = r+float64(c.R), g+float64(c.G), b+float64(c.B), n+1
				}
			}
			if n == 0 {
				continue
			}
			img.SetNRGBA(px, py, color.NRGBA{uint8(r/n + 0.5), uint8(g/n + 0.5), uint8(b/n + 0.5), uint8(n/(ss*ss)*255 + 0.5)})
		}
	}
	return img
}

func main() {
	dir := filepath.Join("web", "public")
	if _, err := os.Stat(dir); err != nil {
		log.Fatalf("run from the repo root: %v", err)
	}
	for _, icon := range []struct {
		name    string
		size    int
		rounded bool
	}{
		{"icon-192.png", 192, true},
		{"icon-512.png", 512, true},
		{"icon-maskable-512.png", 512, false}, // launchers apply their own mask
		{"apple-touch-icon.png", 180, false},  // iOS rounds it; transparency would turn black
	} {
		f, err := os.Create(filepath.Join(dir, icon.name))
		if err != nil {
			log.Fatal(err)
		}
		if err := png.Encode(f, render(icon.size, icon.rounded)); err != nil {
			f.Close()
			log.Fatal(err)
		}
		if err := f.Close(); err != nil {
			log.Fatal(err)
		}
	}
}
```

Run `mkdir -p web/public && go run web/icons/gen.go`. The four PNGs appear. Open `icon-512.png` and describe it in your report: a blue SD card with a light down-arrow on a dark rounded square. `go vet ./...` still passes, because the ignore tag keeps `web/icons` out of builds.

- [ ] **Step 2: The SVG icon and the manifest**

`web/public/icon.svg` (the same geometry at 512 px):

```svg
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 512 512">
  <rect width="512" height="512" rx="112.6" fill="#141c27"/>
  <path d="M143.4 112.6H301.1L368.6 180.2V399.4H143.4Z" fill="#4f9cf9"/>
  <g fill="#141c27">
    <rect x="165.9" y="135.6" width="22.5" height="57.3"/>
    <rect x="199.7" y="135.6" width="22.5" height="57.3"/>
    <rect x="233.5" y="135.6" width="22.5" height="57.3"/>
    <rect x="267.3" y="135.6" width="22.5" height="57.3"/>
  </g>
  <g fill="#e6edf6">
    <rect x="242.5" y="233.1" width="27" height="68.8"/>
    <path d="M206.4 290.4H305.6L256 359.2Z"/>
  </g>
</svg>
```

`web/public/manifest.webmanifest`:

```json
{
  "name": "SD2NAS",
  "short_name": "SD2NAS",
  "description": "Camera card imports and staging on the NAS",
  "id": "/",
  "start_url": "/",
  "scope": "/",
  "display": "standalone",
  "background_color": "#141c27",
  "theme_color": "#0f151e",
  "icons": [
    { "src": "/icon-192.png", "sizes": "192x192", "type": "image/png", "purpose": "any" },
    { "src": "/icon-512.png", "sizes": "512x512", "type": "image/png", "purpose": "any" },
    { "src": "/icon-maskable-512.png", "sizes": "512x512", "type": "image/png", "purpose": "maskable" },
    { "src": "/icon.svg", "sizes": "any", "type": "image/svg+xml", "purpose": "any" }
  ]
}
```

In `web/index.html`, after the `theme-color` meta, add:

```html
<link rel="manifest" href="/manifest.webmanifest">
<link rel="icon" href="/icon.svg" type="image/svg+xml">
<link rel="apple-touch-icon" href="/apple-touch-icon.png">
<meta name="mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-capable" content="yes">
<meta name="apple-mobile-web-app-status-bar-style" content="black-translucent">
<meta name="apple-mobile-web-app-title" content="SD2NAS">
```

- [ ] **Step 3: Tests**

`web/icons_test.go` (package `web`; it reads `public/` straight from disk, so it runs without npm):
- `TestIcons`:
  - `public/manifest.webmanifest` parses as JSON with `name` `SD2NAS`, `display` `standalone`, `start_url` `/`, and at least one `maskable` icon.
  - For every PNG icon, `public/<src>` exists and `image.DecodeConfig` gives the size in `sizes`.
  - `public/apple-touch-icon.png` is 180×180, and its pixel (0, 0) is opaque (alpha 255).
  - The pixel (0, 0) of `public/icon-192.png` is transparent (alpha 0: the rounded corner).
  - `public/icon.svg` exists and starts with `<svg`.

In `embed_test.go`, extend `TestBuiltUI`. `index.html` contains `rel="manifest" href="/manifest.webmanifest"` and `rel="apple-touch-icon"`. `dist` contains `manifest.webmanifest`, `icon.svg` and all four PNGs.

- [ ] **Step 4: Run**

In `web/`, run `npm run build`, then `go test ./web/ -v`: `TestIcons` and `TestBuiltUI` PASS. Run `gofmt -l .`, `go vet ./...` and the Linux command → PASS.

- [ ] **Step 5: Commit**

```bash
git add web/icons/gen.go web/icons_test.go web/public web/index.html web/embed_test.go
git commit -m "Add the PWA manifest and app icons for Add to Home Screen

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Ingest page: live progress, history and import detail

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/components/LiveIngest.svelte`
- Replace: `web/src/pages/Ingest.svelte`, `web/src/pages/IngestDetail.svelte`

**Interfaces:**
- Consumes (Task 8; every name is exact): `api` from `../lib/api.js`; `live` from `../lib/live.svelte.js`; `isId` from `../lib/paths.js`; and from `../lib/format.js`: `formatBytes`, `formatCount`, `formatWhen`, `phaseSteps`, `progressFraction`, `statusLabel`, `levelClass`, `statusClass`, `cardName`, `knownCount`, `groupKnown`, `outcomeFiles`. Global classes from `app.css`: `.card .row .spacer .btn .stats .stat(.ok/.warn/.bad/.acc) .bar .waiting .phases .phase(.done/.current) .pill(.ok/.warn/.bad/.live) .note(.ok/.warn/.bad) .muted .small .mono .sub .only-desktop .only-phone`.
- The API: `GET /api/ingests?limit=20[&before=<id>]` → `{ingests:[view]}`, newest first. `GET /api/ingests/{id}` → the view plus `files` (every outcome except `new`; each has `card_path, size, outcome, error?, rel_path?`) and `clock_details` (`card_path, kind, detail`). A view holds `id, card_path, card_label, card_size, started_at, finished_at?, status, counts{seen,new,in_archive,in_staging,seen_before,failed,unsupported}, unsupported_exts?, clock_warnings, safe_to_format, error?, level, message, progress?`, and `progress` holds `{phase, files_total, files_done, bytes_total, bytes_done, counts}`.
- Produces: the pages at `/`, `/ingest` and `/ingest/{id}`.

- [ ] **Step 1: `web/src/components/LiveIngest.svelte`**

```svelte
<script>
  import { live } from '../lib/live.svelte.js';
  import { phaseSteps, progressFraction, formatCount, formatBytes, cardName, knownCount, levelClass } from '../lib/format.js';

  const running = $derived(live.current.running);
  const queued = $derived(live.current.queued);
  const p = $derived(running?.progress ?? null);
  const steps = $derived(phaseSteps(p?.phase ?? 'queued'));
  const pct = $derived(Math.round(progressFraction(p) * 100));
  const last = $derived(live.finished);
</script>

<div class="card">
  {#if running}
    <div class="row">
      <h2 style="margin:0">{cardName(running)}</h2>
      <span class="muted small mono">{running.card_path}</span>
    </div>
    <div class="phases">
      {#each steps as s (s.key)}
        <span class="phase {s.state}">{s.state === 'done' ? '✓' : s.state === 'current' ? '●' : '○'} {s.label}</span>
      {/each}
    </div>
    <div class="bar" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={pct}><i style:width="{pct}%"></i></div>
    <div class="small muted">
      {#if p?.phase === 'classifying'}
        {formatCount(p.files_done)} / {formatCount(p.files_total)} files checked
      {:else if p?.phase === 'copying'}
        {formatCount(p.files_done)} / {formatCount(p.files_total)} new files copied · {formatBytes(p.bytes_done)} of {formatBytes(p.bytes_total)}
      {:else if p?.phase === 'scanning'}
        Reading the card's file list…
      {:else}
        Finishing…
      {/if}
    </div>
    {#if p}
      <div class="stats" style="margin-top:12px">
        <div class="stat acc"><b>{formatCount(p.counts.new)}</b>new</div>
        <div class="stat"><b>{formatCount(knownCount(p.counts))}</b>already known</div>
        {#if p.counts.failed}<div class="stat bad"><b>{formatCount(p.counts.failed)}</b>failed</div>{/if}
        {#if p.counts.unsupported}<div class="stat warn"><b>{formatCount(p.counts.unsupported)}</b>not imported</div>{/if}
      </div>
    {/if}
  {:else}
    <div class="waiting">
      <h2 style="margin:0 0 4px">No card importing</h2>
      <p class="muted" style="margin:0">Insert an SD card into the NAS. Unassigned Devices mounts it and the import starts on its own.</p>
    </div>
  {/if}
  {#if queued.length}
    <p class="small muted">Waiting: {queued.map(cardName).join(', ')}</p>
  {/if}
  {#if last && !running}
    <div class="note {levelClass(last.level)}">
      <b>Last import:</b> {last.message} <a href="/ingest/{last.id}">Details</a>
    </div>
  {/if}
  {#if !live.connected}<p class="small muted">Reconnecting to live updates…</p>{/if}
</div>
```

- [ ] **Step 2: `web/src/pages/Ingest.svelte`**

```svelte
<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { formatCount, formatWhen, formatBytes, statusLabel, statusClass, cardName, knownCount } from '../lib/format.js';
  import LiveIngest from '../components/LiveIngest.svelte';

  const PAGE = 20;
  let ingests = $state([]);
  let loading = $state(true);
  let error = $state('');
  let more = $state(false);

  async function load(before = 0) {
    try {
      const data = await api(`/api/ingests?limit=${PAGE}` + (before ? `&before=${before}` : ''));
      ingests = before ? [...ingests, ...data.ingests] : data.ingests;
      more = data.ingests.length === PAGE;
      error = '';
    } catch (err) {
      error = err.message;
    } finally {
      loading = false;
    }
  }

  // Load now, and again whenever an import finishes.
  $effect(() => {
    live.finishedCount;
    untrack(() => load());
  });
</script>

<h1>Card ingest</h1>
<p class="sub">Imports start on their own when a card is mounted. Nothing is ever deleted from the card.</p>

<LiveIngest />

<div class="card">
  <h2>Recent imports</h2>
  {#if error}
    <div class="note bad">Couldn't load the import history: {error}</div>
  {:else if loading}
    <p class="muted">Loading…</p>
  {:else if ingests.length === 0}
    <p class="muted">No cards have been imported yet.</p>
  {:else}
    <div class="only-desktop" style="overflow-x:auto">
      <table>
        <thead>
          <tr><th>When</th><th>Card</th><th class="num">On card</th><th class="num">New</th><th class="num">Known</th><th class="num">Seen before</th><th>Status</th></tr>
        </thead>
        <tbody>
          {#each ingests as i (i.id)}
            <tr>
              <td><a href="/ingest/{i.id}">{formatWhen(i.started_at)}</a></td>
              <td>{cardName(i)}{#if i.card_size}<span class="muted small"> {formatBytes(i.card_size)}</span>{/if}</td>
              <td class="num">{formatCount(i.counts.seen)}</td>
              <td class="num" style="color:var(--ok)">{formatCount(i.counts.new)}</td>
              <td class="num">{formatCount(i.counts.in_archive + i.counts.in_staging)}</td>
              <td class="num">{formatCount(i.counts.seen_before)}</td>
              <td><span class="pill {statusClass(i)}" title={i.message}>{statusLabel(i.status)}</span></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
    <ul class="list only-phone">
      {#each ingests as i (i.id)}
        <li>
          <a href="/ingest/{i.id}">
            <span class="row"><b>{cardName(i)}</b><span class="spacer"></span><span class="pill {statusClass(i)}">{statusLabel(i.status)}</span></span>
            <span class="muted small">{formatWhen(i.started_at)} · {formatCount(i.counts.new)} new · {formatCount(knownCount(i.counts))} known</span>
          </a>
        </li>
      {/each}
    </ul>
    {#if more}
      <p><button class="btn" onclick={() => load(ingests[ingests.length - 1].id)}>Load more</button></p>
    {/if}
  {/if}
</div>

<style>
  .list { list-style: none; margin: 0; padding: 0; }
  .list li + li { border-top: 1px solid var(--line); }
  .list a { display: flex; flex-direction: column; gap: 2px; padding: 10px 2px; color: var(--text); text-decoration: none; min-height: 44px; }
</style>
```

- [ ] **Step 3: `web/src/pages/IngestDetail.svelte`**

```svelte
<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { isId } from '../lib/paths.js';
  import { cardName, formatWhen, formatBytes, formatCount, levelClass, groupKnown, outcomeFiles } from '../lib/format.js';
  import LiveIngest from '../components/LiveIngest.svelte';

  let { id } = $props();
  let ingest = $state(null);
  let error = $state('');
  const MAX = 200; // longest file list shown

  async function load() {
    if (!isId(id)) {
      error = 'There is no such import.';
      return;
    }
    try {
      ingest = await api(`/api/ingests/${id}`);
      error = '';
    } catch (err) {
      error = err.message;
    }
  }

  // Load once, then again when this import finishes.
  $effect(() => {
    const f = live.finished;
    untrack(() => {
      if (!ingest || (f && String(f.id) === id)) load();
    });
  });

  const files = $derived(ingest?.files ?? []);
  const groups = $derived(groupKnown(files));
  const seenBefore = $derived(outcomeFiles(files, 'seen_before'));
  const failed = $derived(outcomeFiles(files, 'failed'));
  const unsupported = $derived(outcomeFiles(files, 'unsupported'));
  const clock = $derived(ingest?.clock_details ?? []);
  const live_ = $derived(ingest && (ingest.status === 'queued' || ingest.status === 'running'));
</script>

<p><a href="/ingest" class="btn">← Card ingest</a></p>

{#if error}
  <div class="note bad">{error}</div>
{:else if !ingest}
  <p class="muted">Loading…</p>
{:else}
  {@const c = ingest.counts}
  <h1>{cardName(ingest)}</h1>
  <p class="sub">
    {formatWhen(ingest.started_at)} · <span class="mono">{ingest.card_path}</span>{#if ingest.card_size} · {formatBytes(ingest.card_size)} of photos and videos{/if}
  </p>
  {#if live_}
    <LiveIngest />
  {:else}
    <div class="note {levelClass(ingest.level)}">{ingest.message}</div>
  {/if}
  <div class="stats">
    <div class="stat ok"><b>{formatCount(c.new)}</b>new → staging</div>
    <div class="stat"><b>{formatCount(c.in_archive)}</b>already in the archive</div>
    <div class="stat"><b>{formatCount(c.in_staging)}</b>already in staging</div>
    <div class="stat warn"><b>{formatCount(c.seen_before)}</b>imported before, since removed</div>
    {#if c.failed}<div class="stat bad"><b>{formatCount(c.failed)}</b>failed</div>{/if}
    {#if c.unsupported}<div class="stat warn"><b>{formatCount(c.unsupported)}</b>not imported</div>{/if}
  </div>

  {#if groups.length}
    <details>
      <summary>Where the {formatCount(c.in_archive + c.in_staging)} known files already are</summary>
      <ul>
        {#each groups as g (g.folder)}<li>{formatCount(g.count)} → <span class="mono">{g.folder}</span></li>{/each}
      </ul>
    </details>
  {/if}
  {#if seenBefore.length}
    <details>
      <summary>{formatCount(seenBefore.length)} files were imported before but are no longer kept</summary>
      <p class="muted small" style="margin:8px 0">They were culled or discarded after an earlier import, so they're skipped and deleted shots don't come back.</p>
      <ul>
        {#each seenBefore.slice(0, MAX) as f (f.card_path)}<li class="mono">{f.card_path}</li>{/each}
        {#if seenBefore.length > MAX}<li>…and {formatCount(seenBefore.length - MAX)} more</li>{/if}
      </ul>
    </details>
  {/if}
  {#if failed.length}
    <details open>
      <summary>{formatCount(failed.length)} files failed</summary>
      <ul>
        {#each failed.slice(0, MAX) as f (f.card_path)}<li><span class="mono">{f.card_path}</span>: {f.error}</li>{/each}
        {#if failed.length > MAX}<li>…and {formatCount(failed.length - MAX)} more</li>{/if}
      </ul>
    </details>
  {/if}
  {#if unsupported.length}
    <details open>
      <summary>{formatCount(unsupported.length)} files not imported (format not supported)</summary>
      <ul>
        {#each unsupported.slice(0, MAX) as f (f.card_path)}<li class="mono">{f.card_path}</li>{/each}
        {#if unsupported.length > MAX}<li>…and {formatCount(unsupported.length - MAX)} more</li>{/if}
      </ul>
    </details>
  {/if}
  {#if clock.length}
    <details open>
      <summary>{formatCount(clock.length)} suspicious capture times</summary>
      <ul>
        {#each clock.slice(0, MAX) as w, i (i)}<li><span class="mono">{w.card_path}</span>: {w.detail}</li>{/each}
      </ul>
    </details>
  {/if}
{/if}
```

Notes for the implementer:
- `card_path` is unique within one ingest's files, so it is a safe key. If Svelte reports a duplicate key, key on the index instead.
- `{@const}` directly inside `{:else}` is valid Svelte 5.
- The variable is named `live_` because `live` is the imported store.

- [ ] **Step 4: Build and check by hand**

In `web/`, `npm run build` must finish with no errors and no Svelte warnings (fix or explain any). Then manual check. Run `go build -o tmp/sd2nas.exe ./cmd/sd2nas && tmp/sd2nas.exe --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config --addr :18080`, open `http://localhost:18080/` in a browser if you have one, or exercise it with curl:
1. With no import running, the page shows "No card importing" and "No cards have been imported yet".
2. `curl -s -X POST "http://localhost:18080/api/ingest?wait=1" -H "X-SD2NAS: 1" -H "Content-Type: application/json" -d "{\"path\":\"$(pwd -W)/testdata/cards/SONY_A7IV\"}"` (use the absolute cards path the server logged). Without a reload, the history gains a row (the `ingest.finished` event), and "Last import: SONY_A7IV: 2 new, 0 already known. Card safe to format." appears.
3. Clicking the row opens `/ingest/1`, which shows the message and the stats. Reloading `/ingest/1` keeps it there (the server's SPA fallback).
4. A second POST shows "2 already in staging" and the "Where the 2 known files already are" list naming `Staging/2025-05-30`.

Report what you checked. Clean up `tmp/`.

- [ ] **Step 5: Commit**

```bash
git add web/src/components/LiveIngest.svelte web/src/pages/Ingest.svelte web/src/pages/IngestDetail.svelte
git commit -m "Show live import progress, import history and import details

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Staging pages: folder list, folder grid and full-screen preview

Suggested implementer: sonnet.

**Files:**
- Create: `web/src/components/Thumb.svelte`, `web/src/components/Lightbox.svelte`
- Replace: `web/src/pages/Staging.svelte`, `web/src/pages/Folder.svelte`

**Interfaces:**
- Consumes (Task 8; exact names): `api`; `live`; `isId`; and from `format.js`: `formatBytes`, `formatCount`, `formatCapture`, `dateRange`, `kindChips`, `extLabel`. The API (Task 4) is `GET /api/staging` → `{folders:[Folder], untracked:[name]}`, and `GET /api/staging/{id}/files[?kind=jpg|raw|video]` → `{folder: Folder, files: [File]}`. Folder fields: `id, name, date, title, dates, files, bytes, counts{jpg,raw,video}, samples[{sha256,kind}], clock_warnings, warnings[]`. File fields: `sha256, name, path, kind, size, capture_time?, tz_offset?, clock_warnings?`. Images are `/api/thumbs/{sha}` (≤400 px) and `/api/preview/{sha}` (≤1600 px). Either may be 404/500 for a file with no preview, in which case a placeholder shows.
- Produces: the pages at `/staging` and `/staging/{id}`.

- [ ] **Step 1: `web/src/components/Thumb.svelte`**

```svelte
<script>
  let { sha, kind, label = '' } = $props();
  let failed = $state(false);
  const badge = $derived(label || (kind === 'raw' ? 'RAW' : kind === 'video' ? 'VIDEO' : 'JPG'));
</script>

<div class="thumb">
  {#if !failed}
    <img src="/api/thumbs/{sha}" alt="" loading="lazy" decoding="async" onerror={() => (failed = true)} />
  {/if}
  {#if kind === 'video'}<span class="play" aria-hidden="true">▶</span>{/if}
  <span class="b">{badge}</span>
</div>

<style>
  .thumb { aspect-ratio: 3 / 2; border-radius: 5px; position: relative; overflow: hidden; background: var(--panel2); }
  img { width: 100%; height: 100%; object-fit: cover; display: block; }
  .b { position: absolute; left: 4px; bottom: 3px; font-size: 9px; font-weight: 700; background: rgba(0,0,0,.55); color: #fff; padding: 0 4px; border-radius: 3px; }
  .play { position: absolute; inset: 0; display: grid; place-items: center; font-size: 16px; color: #fff; opacity: .85; }
</style>
```

- [ ] **Step 2: `web/src/pages/Staging.svelte`**

```svelte
<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { live } from '../lib/live.svelte.js';
  import { formatBytes, formatCount, dateRange } from '../lib/format.js';
  import Thumb from '../components/Thumb.svelte';

  let data = $state(null);
  let error = $state('');

  async function load() {
    try {
      data = await api('/api/staging');
      error = '';
    } catch (err) {
      error = err.message;
    }
  }

  // Load now, and again whenever an import finishes (new files may have landed).
  $effect(() => {
    live.finishedCount;
    untrack(load);
  });

  const totals = $derived.by(() => {
    const fs = data?.folders ?? [];
    return { folders: fs.length, files: fs.reduce((s, f) => s + f.files, 0), bytes: fs.reduce((s, f) => s + f.bytes, 0) };
  });
</script>

<h1>Staging</h1>
{#if error}
  <div class="note bad">Couldn't load staging: {error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  <p class="sub">{totals.folders} {totals.folders === 1 ? 'folder' : 'folders'} · {formatCount(totals.files)} files · {formatBytes(totals.bytes)}</p>
  {#if data.untracked.length}
    <div class="note warn">Not shown because SD2NAS didn't create them: {data.untracked.join(', ')}. They were probably added over the network share.</div>
  {/if}
  {#if data.folders.length === 0}
    <div class="waiting">
      <h2>Staging is empty</h2>
      <p class="muted" style="margin:0">New imports appear here.</p>
    </div>
  {:else}
    <div class="folders">
      {#each data.folders as f (f.id)}
        <a class="folder" href="/staging/{f.id}">
          <div class="name">{f.name}</div>
          <div class="muted small">{dateRange(f.dates)}</div>
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
            <span class="muted">{formatCount(f.files)} files · {formatBytes(f.bytes)}</span>
          </div>
          {#each f.warnings as w}<div class="warn small">⚠ {w}</div>{/each}
        </a>
      {/each}
    </div>
  {/if}
{/if}

<style>
  .folders { display: grid; grid-template-columns: repeat(auto-fill, minmax(300px, 1fr)); gap: 14px; }
  .folder { display: flex; flex-direction: column; gap: 4px; background: var(--panel); border: 1px solid var(--line); border-radius: var(--r); padding: 12px; color: var(--text); text-decoration: none; }
  .folder:hover { border-color: #41567a; }
  .name { font-weight: 600; overflow-wrap: anywhere; }
  .strip { display: grid; grid-template-columns: repeat(5, minmax(0, 1fr)); gap: 4px; margin: 6px 0; }
  .warn { color: var(--warn); }
  @media (max-width: 760px) {
    .folders { grid-template-columns: minmax(0, 1fr); }
  }
</style>
```

- [ ] **Step 3: `web/src/components/Lightbox.svelte`**

```svelte
<script>
  import { formatBytes, formatCapture, extLabel } from '../lib/format.js';

  let { files, index = $bindable(), onclose } = $props();

  const file = $derived(files[index]);
  let loaded = $state(false);
  let failed = $state(false);
  let chrome = $state(true); // top and bottom bars

  // A new file: reset the load state and warm the neighbours' previews.
  $effect(() => {
    file.sha256;
    loaded = false;
    failed = false;
    for (const j of [index - 1, index + 1]) {
      if (j >= 0 && j < files.length) new Image().src = `/api/preview/${files[j].sha256}`;
    }
  });

  $effect(() => {
    document.body.classList.add('noscroll');
    return () => document.body.classList.remove('noscroll');
  });

  function prev() {
    if (index > 0) index -= 1;
  }
  function next() {
    if (index < files.length - 1) index += 1;
  }

  function onkeydown(e) {
    if (e.key === 'ArrowLeft') prev();
    else if (e.key === 'ArrowRight') next();
    else if (e.key === 'Escape') onclose();
    else return;
    e.preventDefault();
  }

  // Swipe left or right for the next or previous file, down to close. A tap
  // on the left or right third steps; in the middle it shows or hides the bars.
  let start = null;
  function onpointerdown(e) {
    if (e.isPrimary) start = { x: e.clientX, y: e.clientY };
  }
  function onpointerup(e) {
    if (!start || !e.isPrimary) return;
    const dx = e.clientX - start.x;
    const dy = e.clientY - start.y;
    start = null;
    if (Math.abs(dx) > 50 && Math.abs(dx) > Math.abs(dy)) {
      if (dx < 0) next();
      else prev();
    } else if (dy > 80 && dy > Math.abs(dx)) {
      onclose();
    } else if (Math.abs(dx) < 10 && Math.abs(dy) < 10) {
      const third = e.currentTarget.clientWidth / 3;
      if (e.clientX < third) prev();
      else if (e.clientX > 2 * third) next();
      else chrome = !chrome;
    }
  }
</script>

<svelte:window {onkeydown} />

<div class="lightbox" role="dialog" aria-modal="true" aria-label={file.name}>
  <div class="stage" role="presentation" {onpointerdown} {onpointerup} onpointercancel={() => (start = null)}>
    {#key file.sha256}
      <img src="/api/preview/{file.sha256}" alt={file.name} draggable="false" class:hidden={!loaded}
        onload={() => (loaded = true)} onerror={() => (failed = true)} />
    {/key}
    {#if failed}
      <div class="msg">No preview for this file.</div>
    {:else if !loaded}
      <div class="msg">Loading…</div>
    {/if}
  </div>
  {#if chrome}
    <div class="top">
      <button class="icon" onclick={onclose} aria-label="Close">✕</button>
      <span>{index + 1} / {files.length}</span>
      {#if file.kind === 'video'}<span class="pill">Video · frame at 1 s</span>{/if}
    </div>
    <div class="bottom">
      <div class="fname">{file.name}</div>
      <div class="meta">
        {file.capture_time ? formatCapture(file.capture_time) : 'No capture time'}{file.tz_offset ? ` (UTC${file.tz_offset})` : ''} · {formatBytes(file.size)} · {extLabel(file)}
      </div>
      {#each file.clock_warnings ?? [] as w}<div class="warn">⚠ {w}</div>{/each}
    </div>
    <button class="step prev" onclick={prev} disabled={index === 0} aria-label="Previous">‹</button>
    <button class="step next" onclick={next} disabled={index === files.length - 1} aria-label="Next">›</button>
  {/if}
</div>

<style>
  .lightbox { position: fixed; inset: 0; z-index: 50; background: #000; color: var(--text); }
  .stage { position: absolute; inset: 0; display: grid; place-items: center; touch-action: none; user-select: none; }
  .stage img { max-width: 100%; max-height: 100%; object-fit: contain; -webkit-user-drag: none; }
  .hidden { visibility: hidden; }
  .msg { position: absolute; color: var(--muted); }
  .top { position: absolute; top: 0; left: 0; right: 0; display: flex; align-items: center; gap: 12px; padding: calc(8px + env(safe-area-inset-top)) 12px 8px; background: linear-gradient(rgba(0,0,0,.7), transparent); }
  .bottom { position: absolute; left: 0; right: 0; bottom: 0; padding: 16px 16px calc(14px + env(safe-area-inset-bottom)); background: linear-gradient(transparent, rgba(0,0,0,.75)); font-size: 13px; }
  .fname { font-weight: 600; overflow-wrap: anywhere; }
  .meta { color: var(--muted); }
  .warn { color: var(--warn); }
  .icon { background: rgba(255,255,255,.12); border: 0; color: #fff; width: 44px; height: 44px; border-radius: 50%; font-size: 18px; cursor: pointer; }
  .step { position: absolute; top: 50%; transform: translateY(-50%); background: rgba(255,255,255,.12); border: 0; color: #fff; width: 44px; height: 64px; font-size: 32px; border-radius: 8px; cursor: pointer; }
  .step:disabled { opacity: .25; cursor: default; }
  .prev { left: 8px; }
  .next { right: 8px; }
  @media (hover: none) { .step { display: none; } }
</style>
```

- [ ] **Step 4: `web/src/pages/Folder.svelte`**

```svelte
<script>
  import { untrack } from 'svelte';
  import { api } from '../lib/api.js';
  import { isId } from '../lib/paths.js';
  import { formatBytes, formatCount, dateRange, kindChips, extLabel } from '../lib/format.js';
  import Thumb from '../components/Thumb.svelte';
  import Lightbox from '../components/Lightbox.svelte';

  let { id } = $props();
  let kind = $state(''); // '' = all
  let data = $state(null);
  let error = $state('');
  let open = $state(-1); // index into data.files shown full screen; -1 = none

  async function load(k) {
    if (!isId(id)) {
      error = 'There is no such staging folder.';
      return;
    }
    try {
      data = await api(`/api/staging/${id}/files` + (k ? `?kind=${k}` : ''));
      error = '';
    } catch (err) {
      error = err.message;
    }
  }

  $effect(() => {
    const k = kind;
    untrack(() => load(k));
  });

  // The preview takes a history entry, so the phone's back gesture closes
  // it instead of leaving the folder.
  function show(i) {
    open = i;
    history.pushState({ lightbox: true }, '');
  }
  function hide() {
    if (history.state?.lightbox) history.back(); // popstate below closes it
    else open = -1;
  }
  function onpopstate() {
    if (!history.state?.lightbox) open = -1;
  }
</script>

<svelte:window {onpopstate} />

<p><a href="/staging" class="btn">← Staging</a></p>

{#if error}
  <div class="note bad">{error}</div>
{:else if !data}
  <p class="muted">Loading…</p>
{:else}
  {@const f = data.folder}
  <h1>{f.name}</h1>
  <p class="sub">{dateRange(f.dates)} · {formatCount(f.files)} files · {formatBytes(f.bytes)}</p>
  {#each f.warnings as w}<div class="note warn">{w}</div>{/each}
  <div class="toolbar">
    <div class="chips">
      {#each kindChips(f.counts) as c (c.kind)}
        <button class="chip" class:on={kind === c.kind} onclick={() => (kind = c.kind)}>{c.label} {c.count}</button>
      {/each}
    </div>
  </div>
  {#if data.files.length === 0}
    <p class="muted">No files here.</p>
  {:else}
    <div class="grid">
      {#each data.files as file, i (file.sha256)}
        <button class="tile" onclick={() => show(i)} aria-label="Show {file.name}">
          <Thumb sha={file.sha256} kind={file.kind} label={extLabel(file)} />
          <span class="fn">{#if file.clock_warnings}<span class="warn" title={file.clock_warnings.join('; ')}>⚠ </span>{/if}{file.name}</span>
        </button>
      {/each}
    </div>
  {/if}
  {#if open >= 0 && open < data.files.length}
    <Lightbox files={data.files} bind:index={open} onclose={hide} />
  {/if}
{/if}

<style>
  .grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(140px, 1fr)); gap: 8px; }
  .tile { background: none; border: 0; padding: 0; color: inherit; font: inherit; text-align: left; cursor: pointer; min-width: 0; }
  .fn { display: block; font-size: 11px; color: var(--muted); white-space: nowrap; overflow: hidden; text-overflow: ellipsis; margin-top: 3px; }
  .warn { color: var(--warn); }
  @media (max-width: 760px) {
    .grid { grid-template-columns: repeat(3, minmax(0, 1fr)); gap: 4px; }
  }
</style>
```

- [ ] **Step 5: Build**

In `web/`, `npm run build` must finish with no errors and no Svelte warnings (fix or explain any). From the repo root, run `go test ./web/` → PASS.

- [ ] **Step 6: Phone and desktop check** (Review Focus 9)

The implementer runs what they can and reports the rest, which the controller or owner checks in a browser at 375×812 and at desktop width. Setup: `go build -o tmp/sd2nas.exe ./cmd/sd2nas`, run it on `testdata/cards` as in Task 10, and import `SONY_A7IV` once.
1. `/staging` lists one card, `2025-05-30`, with tags `1 RAW` and `1 JPG`, "2 files", and a one-thumb strip (the JPEG). On a phone it is one column, and the bottom tab bar shows Card ingest and Staging with Staging highlighted. The last card is not hidden under the bar.
2. Tapping the card opens `/staging/<id>`. The chips are All 2, RAW 1 and JPG 1, and tapping RAW shows only the ARW. Its tile shows the RAW placeholder: the fixture has no embedded preview, so `/api/thumbs` answers 404 and the badge is still visible.
3. Tapping the JPEG tile opens the full-screen preview. Swiping left or right steps, as do the ←/→ keys and the ‹ › buttons on desktop. Escape, ✕, swiping down and the browser/phone back gesture each close it and leave you on the folder page.
4. Reloading `/staging/<id>` stays there. Every button and tab is at least 44 px tall, and nothing scrolls sideways at 375 px.
5. With a real RAW and video (optional, if the implementer has one): the ARW thumbnail shows the embedded preview on Linux/Docker with exiftool, and an MP4 shows a frame.

Clean up `tmp/`.

- [ ] **Step 7: Commit**

```bash
git add web/src/components/Thumb.svelte web/src/components/Lightbox.svelte web/src/pages/Staging.svelte web/src/pages/Folder.svelte
git commit -m "Show staging folders with thumbnails and a swipeable full-screen preview

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: Dockerfile node stage, CI web job, thumbnail tools in CI, image smoke test

Suggested implementer: sonnet.

**Files:**
- Modify: `deploy/Dockerfile`, `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: `web/package.json` scripts `test` and `build` (Task 8); `web/*.go` (package `web`, Task 1); `SD2NAS_REQUIRE_TOOLS` (Task 6) and `SD2NAS_REQUIRE_UI` (Task 8). `.dockerignore` already excludes `web/node_modules` and `web/dist`, so the context carries neither. Leave it as it is.
- Produces: an image whose binary embeds the UI, and CI that builds and tests the web app and runs the thumbnail tool tests for real.

- [ ] **Step 1: `deploy/Dockerfile`**

Replace the top of the file, through the Go `RUN … go build` line, with the block below. Everything from `FROM alpine:3.22` down stays the same.

```dockerfile
# Build from the repo root:  docker build -f deploy/Dockerfile -t sd2nas .

# The web UI: web/dist, embedded into the binary by package web.
FROM node:24-alpine AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm test && npm run build

FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY web/*.go ./web/
COPY --from=web /web/dist ./web/dist
# Fail loudly rather than ship a binary whose UI is the "not built" page.
RUN test -f web/dist/index.html
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/sd2nas ./cmd/sd2nas
```

- [ ] **Step 2: `.github/workflows/ci.yml`**

```yaml
name: CI

on:
  push:
  pull_request:

permissions:
  contents: read

jobs:
  go:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - name: Install exiftool and ffmpeg (thumbnail tests run against the real tools)
        run: sudo apt-get update && sudo apt-get install -y --no-install-recommends libimage-exiftool-perl ffmpeg
      - name: gofmt
        run: |
          unformatted=$(gofmt -l .)
          if [ -n "$unformatted" ]; then echo "Run gofmt on:"; echo "$unformatted"; exit 1; fi
      - run: go vet ./...
      - run: go test ./...
        env:
          SD2NAS_REQUIRE_TOOLS: "1"

  web:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: web
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-node@v5
        with:
          node-version: 24
          cache: npm
          cache-dependency-path: web/package-lock.json
      - run: npm ci
      - run: npm test
      - run: npm run build
      - uses: actions/setup-go@v6
        with:
          go-version-file: go.mod
      - name: Check the built UI the binary embeds
        run: go test ./web/
        working-directory: .
        env:
          SD2NAS_REQUIRE_UI: "1"

  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: docker/setup-buildx-action@v3
      - name: Build image (no push)
        uses: docker/build-push-action@v6
        with:
          context: .
          file: deploy/Dockerfile
          push: false
          cache-from: type=gha
          cache-to: type=gha,mode=max
```

Check that the `actions/setup-node` tag exists before committing: `git ls-remote https://github.com/actions/setup-node refs/tags/v5` prints a line. If it prints nothing, use `@v4` and say so in your report.

- [ ] **Step 3: Build and smoke-test the image** (Docker Desktop running)

```bash
docker build -f deploy/Dockerfile -t sd2nas:m3 .
mkdir -p tmp/smoke/photos tmp/smoke/config
MSYS_NO_PATHCONV=1 docker run -d --name sd2nas-m3 -p 18080:8080 -e TZ=America/Los_Angeles \
  -v "$(pwd -W)/testdata/cards:/cards:ro" -v "$(pwd -W)/tmp/smoke/photos:/photos" -v "$(pwd -W)/tmp/smoke/config:/config" sd2nas:m3
sleep 3   # or poll /healthz
curl -s http://localhost:18080/healthz                       # "status": "ok", no warnings (both tools are in the image)
curl -s http://localhost:18080/ | grep -c 'id="app"'         # 1
curl -sI http://localhost:18080/staging/1 | grep -i 'content-type: text/html'
ASSET=$(curl -s http://localhost:18080/ | grep -o '/assets/[^"]*\.js' | head -1)
curl -sI "http://localhost:18080$ASSET" | grep -i 'cache-control: public, max-age=31536000, immutable'
curl -sI http://localhost:18080/manifest.webmanifest | grep -i 'application/manifest+json'
curl -s -X POST "http://localhost:18080/api/ingest?wait=1&format=text" -H "X-SD2NAS: 1" -H "Content-Type: application/json" -d '{"path":"/cards/SONY_A7IV"}'
#   → "normal" / "SONY_A7IV: 2 new, 0 already known. Card safe to format."
curl -s http://localhost:18080/api/staging                   # one folder "2025-05-30", counts jpg 1 raw 1
JPG=$(curl -s http://localhost:18080/api/staging | grep -o '"sha256":"[0-9a-f]*","kind":"jpg"' | head -1 | cut -d'"' -f4)
curl -s -o /dev/null -w '%{http_code} %{content_type}\n' "http://localhost:18080/api/thumbs/$JPG"   # 200 image/jpeg
curl -sN --max-time 2 http://localhost:18080/api/events | head -3                                    # retry: 3000, event: ingest.current …
time docker stop sd2nas-m3                                   # well under 10 s: open SSE streams don't block shutdown
docker logs sd2nas-m3 | tail -20                             # the "thumbnails ready" and warm-up lines; no panics
docker rm sd2nas-m3 && rm -rf tmp/smoke
```

Record each result in your report. Two outcomes are expected, not failures: the RAW fixture's thumbnail is a 404 ("no embedded preview": the committed ARW has none), and the warm-up log warns about it.

Also run the Linux test command from the Global Constraints once more. The real-tool thumbnail tests must PASS there, not SKIP: check with `go test -v ./internal/thumbs/ -run TestReal` inside the same container command.

- [ ] **Step 4: Commit**

```bash
git add deploy/Dockerfile .github/workflows/ci.yml
git commit -m "Build the web UI in the image and CI, and run thumbnail tool tests in CI

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 13: PLAN.md: tick M3 and record what was decided

Suggested implementer: haiku.

**Files:**
- Modify: `docs/PLAN.md`

**Interfaces:**
- Consumes: this plan's Decisions (read them in `docs/plans/m3-web-ui.md`).
- Produces: documentation only.

- [ ] **Step 1: Edit `docs/PLAN.md`**

1. In §16 **M3**, tick all five items (`- [x]`). Under them, add this paragraph:

```markdown
Implementation plan: `docs/plans/m3-web-ui.md`. The UI is embedded from
`web/dist` by package `web`. A committed `web/dist/.placeholder` keeps
`go build` and `go test` working without npm, and `npm run build` keeps
it. `/api/events` sends `ingest.current` (the `GET /api/ingest/current`
body) and `ingest.finished`, plus a snapshot on every connect and a
keepalive every 25 s. Staging folders are the `staging_folders` rows; their
files are the `staged` assets under `Staging/<name>/`. Directories in
`Staging/` without a row are reported as untracked until M4's reconcile
adopts them. Thumbnails (400 px) and previews (1600 px) are cached under
`/config/thumbs/<sha[:2]>/<sha>-<size>.jpg` and warmed after each import.
There is no service worker. A full PWA install needs HTTPS (for example
`tailscale serve`); over plain `http://tower:8080`, phones add a home-screen
shortcut.
```

2. In §10, after the paragraph that starts `JSON throughout.`, add: ``Event types: `ingest.current` and `ingest.finished` (M3); `staging.changed`, `promote.progress` and `indexer.progress` are reserved for M4 and M5.``
3. In §14, under **Local development**, replace the `npm run dev` line with: ``- `npm run dev` in `web/` proxies `/api` and `/healthz` to the Go server on `:8080`; `npm run build` then `go build` embeds the UI (without a build the binary serves a "web UI not built" page)``. In the `ci.yml` bullet, add that the Go job installs exiftool and ffmpeg so the thumbnail tests run for real.
4. In §15, the **Dependencies** row becomes: ``Approved: `modernc.org/sqlite`. Carried over: `goexif`, `go-mp4`, `imaging` (with `golang.org/x/image` pinned to v0.36.0 for its TIFF fixes). Frontend: `svelte`, `vite`, and `@sveltejs/vite-plugin-svelte` (approved by the owner for M3).``
5. Add these follow-ups as unchecked items:
   - Under **M4**: ``- [ ] Adopt staging directories that have no catalog row (shown as "untracked" since M3) in the startup reconcile``
   - Under **M6**: ``- [ ] The fallback poller must register its jobs with the API's ingest feed (`watch`), as `POST /api/ingest` does, so short imports still send `ingest.finished` ``
   - Under **M7**:
     - ``- [ ] Byte-level copy progress for long videos (progress currently updates once per file)``
     - ``- [ ] Keep one `exiftool -stay_open` process for RAW thumbnails instead of one process per file``
     - ``- [ ] HTTPS (e.g. `tailscale serve`) so the PWA installs fully``

- [ ] **Step 2: Final verification of the whole branch**

Run all of these and paste the tail of each output into your report:
- `gofmt -l .` prints nothing.
- `go vet ./...` and `go test ./...` pass on Windows.
- The Linux command and the race command from the Global Constraints pass.
- In `web/`: `npm ci && npm test && npm run build`, then `go test ./web/ -v` with `SD2NAS_REQUIRE_UI=1` passes.
- `git status --short` shows only `HANDOFF.md` untracked: nothing from `web/dist` or `web/node_modules`.
- `git log --format=%B main..HEAD | grep -c 'Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>'` equals the number of commits on the branch.

- [ ] **Step 3: Commit**

```bash
git add docs/PLAN.md
git commit -m "Tick M3 in the plan and record its decisions and follow-ups

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```
