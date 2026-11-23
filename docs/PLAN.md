# SD2NAS Build Plan

SD2NAS runs as a container on an always-on Unraid NAS. When an SD card is
mounted, it automatically imports every new photo and video into a staging area,
using the same naming and sorting rules as the CameraImport desktop app. A
phone-friendly web UI is then used to merge, rename, cull and promote staging
folders into the permanent archive.

A hash catalog remembers everything ever imported and everything already in the
archive, so re-inserting a card that was never wiped only imports what's new.

The UI mockup is in [`ui-mockup.html`](ui-mockup.html).

---

## 1. Scope

### v1 does
- Detect a mounted card and ingest it with no clicks.
- Skip files already in the archive, already in staging, or ingested before and
  later culled.
- Name and sort files exactly like CameraImport: `YYYY-MM-DD HH.mm.ss.ext`, videos
  with resolution, frame rate and duration, and `Capture/` and `Videos/`
  subfolders.
- Manage staging from a browser, phone first: browse, merge, rename, move files,
  cull, discard, promote.
- Keep a hash index of the existing archive, fill it once in the background, and
  rescan it nightly.
- Unmount the card when done and send a notification saying it's safe to format.

### v1 does not
- Delete or modify anything on a card. Never.
- Edit photos or write metadata.
- Have user accounts. It's reachable only on the LAN or Tailscale, with an
  optional shared token (see §9).
- Replace CameraImport, which stays as the desktop tool.

---

## 2. Architecture

```
 Unraid host                                   SD2NAS container
 ───────────                                   ────────────────
 SD card inserted
   │
 Unassigned Devices auto-mounts
   /mnt/disks/SONY_A7IV
   │
 Device script (deploy/unassigned-devices.sh)
   │  POST /api/ingest?wait=1  ───────────────►  ingest job
   │                                              ├─ scan /cards/SONY_A7IV
   │                                              ├─ fingerprint → catalog lookup
   │                                              ├─ copy new → /photos/Staging/…
   │                                              └─ record in SQLite (/config)
   │  ◄─────────────────── JSON summary ────────
   ├─ rc.unassigned umount
   └─ Unraid notify "112 new, card safe to format"

 Phone / desktop browser  ── HTTP + SSE ───────►  web UI (embedded Svelte SPA)
                                                  staging ops, archive index
```

**Mounts inside the container**

| Container path | Host path (example) | Mode | Purpose |
|---|---|---|---|
| `/cards` | `/mnt/disks` | RW/Slave, used read-only | Cards mounted by Unassigned Devices. Slave propagation is required so cards mounted after the container starts are visible. |
| `/photos` | `/mnt/user/Photos` | RW | Holds `Staging/` and `Archive/` (both configurable). One mount keeps promotes as instant renames. |
| `/config` | `/mnt/cache/appdata/sd2nas` | RW | SQLite database, thumbnail cache, settings. Use the cache path directly, not `/mnt/user`, to avoid SQLite locking problems on Unraid's share filesystem. |

The container runs as `99:100` (nobody:users) with `UMASK=000` so files stay
editable over SMB, and must have `TZ` set (see §12).

---

## 3. Repository layout

```
cmd/sd2nas/            main: flags/env, wiring, HTTP server, background workers
internal/media/        naming, EXIF date, video metadata, copy-with-hash, RAW preview
internal/catalog/      SQLite schema, migrations, queries
internal/ingest/       card ingest job (scan → fingerprint → copy → record)
internal/staging/      staging folders: list, merge, rename, move, cull, promote
internal/indexer/      archive backfill + nightly rescan
internal/thumbs/       thumbnail generation + cache
internal/api/          HTTP handlers, SSE event hub
internal/config/       settings (import rules, folder names) + env
web/                   Svelte 5 + Vite SPA, built into web/dist and embedded
deploy/
  Dockerfile
  unraid/sd2nas.xml    Unraid container template
  unassigned-devices.sh
  docker-compose.dev.yml
.github/workflows/
  ci.yml               go test + web build on every push
  release.yml          build + push ghcr.io/aspiziri/sd2nas on tags and main
docs/
  PLAN.md, ui-mockup.html
testdata/              tiny real JPEG/ARW/MP4 fixtures with known EXIF
```

Go module: `github.com/aspiziri/SD2NAS`. HTTP routing uses the standard
library (`net/http` with Go 1.22 path patterns), so no router dependency.

---

## 4. Code borrowed from CameraImport

This is a one-time copy, not a shared module (Go blocks importing another
module's `internal/` packages). The two repos are free to drift.

| CameraImport source | SD2NAS destination | Changes |
|---|---|---|
| `internal/exif/exif.go` | `internal/media/exif.go` | Keep `ReadExif`, `GetCaptureDate`, `FileModDate`. |
| `importer.go`: `canCarryExif`, filename building, collision loop | `internal/media/naming.go` | Pull out pure functions: `CaptureTime(path, kind)`, `DestFileName(t, kind, videoMeta, ext)`, `TypeSubdir(kind, cfg)`. No service struct, no progress state. |
| `importer.go`: `getVideoMetadata` | `internal/media/video.go` | Also read `mvhd` creation time so videos get a real capture time instead of the file's modified time (see §12 about GoPro time zones). |
| `importer.go`: `copyFile` | `internal/media/copy.go` | Becomes `CopyHashed(src, dst) (sha256, error)`. Writes to `dst.partial` with `O_EXCL`, hashes while copying, fsyncs, then renames into place. Keeps the original modified time. |
| `importer.go`: `filesAreIdentical`, `fileHash` | `internal/media/hash.go` | Plus `QuickFingerprint(path)` (see §5). |
| `internal/preview/preview.go` | `internal/thumbs/` | JPEG/PNG resized in Go as before. RAW: `exiftool -b -PreviewImage` (falling back to `JpgFromRaw`). Video: `ffmpeg` frame at 1s. Results cached on disk under `/config/thumbs/` by hash. |
| `internal/config/config.go` | `internal/config/` | Same format lists and subfolder paths. Source and destination come from mounts, not settings. Add `cr3`. |
| `importer_test.go` | `internal/media/*_test.go`, `internal/ingest/*_test.go` | Port the collision, duplicate and overwrite tests as they are. |

Not brought over: Wails, `ImportService`, `usbdetector`, the Svelte components
(only styling and colors are reused).

---

## 5. Catalog (SQLite)

Driver: `modernc.org/sqlite`, pure Go so no cgo, which keeps the static binary
and the small image. **This is a new dependency to approve.** WAL mode, one
writer goroutine.

### Tables

```sql
-- One row per unique file content ever seen, whether imported or indexed.
CREATE TABLE assets (
  sha256        BLOB PRIMARY KEY,
  size          INTEGER NOT NULL,
  kind          TEXT NOT NULL,          -- image | raw | video
  capture_time  TEXT,                   -- local time, as named
  state         TEXT NOT NULL,          -- staged | archived | culled | missing
  rel_path      TEXT,                   -- relative to /photos; NULL when culled
  mtime         INTEGER,                -- for cheap rescans
  first_ingest  INTEGER REFERENCES ingests(id),  -- NULL if found by indexer
  updated_at    TEXT NOT NULL
);
CREATE INDEX assets_path ON assets(rel_path);

-- Quick fingerprints, many-to-one. Never deleted, so an old fingerprint still
-- matches after an archived file has its metadata edited (see "aliases").
CREATE TABLE fingerprints (
  quick_fp  BLOB NOT NULL,              -- see below
  sha256    BLOB NOT NULL REFERENCES assets(sha256),
  PRIMARY KEY (quick_fp, sha256)
);

-- Content that used to be an asset but changed in place (e.g. metadata edit).
CREATE TABLE aliases (
  old_sha256 BLOB PRIMARY KEY,
  sha256     BLOB NOT NULL REFERENCES assets(sha256),
  seen_at    TEXT NOT NULL
);

CREATE TABLE ingests (
  id           INTEGER PRIMARY KEY,
  card_label   TEXT, card_serial TEXT, card_size INTEGER,
  started_at   TEXT NOT NULL, finished_at TEXT,
  status       TEXT NOT NULL,           -- running | done | failed | cancelled
  seen INTEGER, new INTEGER, in_archive INTEGER, in_staging INTEGER,
  seen_before INTEGER, failed INTEGER,
  error        TEXT
);

-- What happened to each file on the card, for the "where are the skipped
-- files" breakdown and debugging.
CREATE TABLE ingest_files (
  ingest_id  INTEGER NOT NULL REFERENCES ingests(id),
  card_path  TEXT NOT NULL,
  size       INTEGER NOT NULL,
  quick_fp   BLOB,
  sha256     BLOB,
  outcome    TEXT NOT NULL,             -- new | in_archive | in_staging | seen_before | failed
  error      TEXT
);

CREATE TABLE staging_folders (
  id      INTEGER PRIMARY KEY,
  name    TEXT NOT NULL UNIQUE,         -- directory name under Staging/
  dates   TEXT NOT NULL,                -- JSON array of YYYY-MM-DD it covers
  created_at TEXT NOT NULL
);

CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
```

### Duplicate detection

**Quick fingerprint** = SHA-256 of `size || first 64 KB || last 64 KB`. That's
128 KB read per file regardless of size.

| Situation | What happens | Cost |
|---|---|---|
| Fingerprint not in catalog | **New.** Copy to staging while computing the full SHA-256, then insert the asset and its fingerprint. | One read of the file, which the copy needs anyway |
| Fingerprint matches an asset | **Known.** Record the outcome from the asset's state (archived, staged, culled → "seen before"). Don't copy. | 128 KB |
| Full hash of a new copy matches an existing asset (fingerprint collision or an alias) | Delete the copy, record it as known. | Rare |

> Correction to the earlier discussion: matches are **not** fully re-hashed by
> default. Fully hashing the ~1,000 known files on every insert is the slow part
> we're trying to avoid. Same size plus identical first and last 64 KB is a safe
> match for camera files. A `verify_known=full` setting can force full hashing
> for anyone who wants it.

**Aliases:** when the indexer finds an archived file whose content changed (same
path, new hash), it stores the new hash and keeps the old one in `aliases` and
the old fingerprint in `fingerprints`. The untouched original on the card still
matches.

**Culled stays remembered:** culling or discarding deletes the file and sets
`state = culled`, `rel_path = NULL`. The row and fingerprint stay, so it's
skipped on future ingests. The ingest summary offers "Re-import anyway".

---

## 6. Ingest pipeline (`internal/ingest`)

1. **Trigger.** `POST /api/ingest {path, label, serial}` from the Unassigned
   Devices script, with an optional in-container poller as a fallback (§10). One
   ingest runs at a time; others queue. The same card path already running
   returns the running job.
2. **Scan.** Walk the card, keeping files whose extension matches the import
   rules. Ignore `DCIM/.Trashes`, `MISC/`, `.thumbnails`, and GoPro `.LRV`/`.THM`
   (low-res proxies and thumbnails).
3. **Fingerprint and classify** each file against the catalog. Card reads are
   sequential-ish; run 2–4 workers.
4. **Copy new files**, one at a time to keep card reads sequential:
   - capture time: EXIF for images and RAW; `mvhd` creation time for video;
     modified time as a last resort
   - target folder: the staging folder whose `dates` contains that date, else a
     new folder named `YYYY-MM-DD`
   - target name and subfolder from `internal/media`, with CameraImport's
     collision loop (identical file → skip, else add `-1`, `-2`)
   - `CopyHashed` to `.partial`, fsync, rename
   - insert the asset and fingerprint, update `staging_folders`
5. **Finish.** Write ingest totals, publish an SSE event, return the JSON summary
   (the script uses it for the notification). Queue thumbnail generation for the
   new assets.

**Failure handling**
- Unreadable file → `outcome = failed`, continue, show it in the summary. The
  card is **not** reported as safe to format if anything failed.
- Card pulled mid-ingest → job `failed`, finished files stay recorded. Partial
  files are removed. Re-inserting the card picks up where it left off, because
  done files now match.
- On startup: delete stray `*.partial` under Staging, mark `running` ingests as
  `failed`.

---

## 7. Staging operations (`internal/staging`)

The filesystem is the truth for what files exist. The database is updated after
each filesystem step. A startup reconcile pass fixes drift: files added or
removed by hand over SMB.

| Operation | Filesystem | Catalog |
|---|---|---|
| List folders | read `Staging/` | join counts and sizes from `assets` |
| Rename | rename the directory | update `staging_folders.name`, `assets.rel_path` prefix |
| Merge N → 1 | move files into the target folder (collision loop), remove empty sources | union `dates`, update paths |
| Move files | same as merge, per file; "New folder…" creates one | update paths and dates |
| Cull files / discard folder | delete files | `state = culled`, `rel_path = NULL` |
| Promote | `os.Rename(Staging/X, Archive/<pattern>)`. If the target exists: merge into it with the collision loop. If rename returns `EXDEV`: copy, verify hash, then delete. | `state = archived`, new paths, drop the `staging_folders` row |

**Folder naming follows the existing archive:** `YYYY-MM-DD - Title`, e.g.
`2007-11-10 - SMU Homecoming`. New staging folders start as just `YYYY-MM-DD`.
The UI's rename asks only for the title, keeps the date prefix, and inserts the
` - ` separator. A merged folder takes the earliest date of its sources.

Promote pattern defaults to `{name}`, meaning straight into the archive root,
matching the flat layout. Tokens `{year}` and `{month}` are available if that
changes, e.g. `{year}/{name}`. Promoting a folder that still has no title shows
a warning but is allowed.

Destructive operations (cull, discard) need a confirmation in the UI, like the
mockup. Everything else is reversible by doing the opposite operation.

---

## 8. Archive indexer (`internal/indexer`)

- **Backfill (first run):** walk `Archive/`, hash every media file, insert as
  `archived` with `first_ingest = NULL`. Resumable (skips paths already indexed
  with matching size and modified time), low priority, progress in the UI. The
  archive is about 150k files; older JPEGs are a few MB and recent RAWs about
  25 MB, so expect roughly 1–2 TB and a few hours of reading from the array at
  ~150 MB/s. Run it overnight. The first run also builds thumbnails, which can
  be deferred to a separate background pass.
- Folder names give a date hint but are **parsed as text, never as a strict
  date**: the archive contains names like `2007-09-31 - Random Party`
  (September has 30 days), which a strict date parser would reject.
- **Nightly rescan** (default 03:00):
  - size and modified time unchanged → skip
  - changed → re-hash, store the old hash as an alias, list it under
    "changed since indexed"
  - gone → `state = missing`, still counted as seen
  - new file not from SD2NAS (copied in by hand) → index it
- **Manual "Rescan now"** from the UI.

---

## 9. HTTP API (`internal/api`)

JSON throughout. Server-sent events at `/api/events` push ingest progress,
staging changes and indexer progress, which replaces CameraImport's polling.

```
POST   /api/ingest                       {path,label,serial}  ?wait=1 blocks until done
GET    /api/ingest/current
POST   /api/ingest/{id}/reimport-culled
GET    /api/ingests                      history
GET    /api/ingests/{id}                 summary + skipped-file breakdown

GET    /api/staging                      folders with counts, sizes, sample thumbs
GET    /api/staging/{id}/files           ?kind=raw|jpg|video
PATCH  /api/staging/{id}                 {name}
POST   /api/staging/merge                {ids[], name}
POST   /api/staging/move                 {fileShas[], toId | newName}
POST   /api/staging/cull                 {fileShas[]}
DELETE /api/staging/{id}                 discard folder
POST   /api/staging/promote              {ids[], pattern}

GET    /api/thumbs/{sha}                 small (~400px), cached
GET    /api/preview/{sha}                large (~1600px) for the phone lightbox

GET    /api/archive/stats
POST   /api/archive/rescan
GET    /api/archive/changed
POST   /api/archive/changed/accept

GET    /api/settings
PUT    /api/settings
GET    /healthz
```

**Auth:** none by default. Optional `SD2NAS_TOKEN`: when set, every request
needs it, the UI asks once and remembers it, and the script sends it as a
header. Never port-forward; use Tailscale for remote access.

---

## 10. Card trigger and unmount

`deploy/unassigned-devices.sh` is attached to the card reader in Unassigned
Devices with auto-mount on:

```bash
#!/bin/bash
# Unassigned Devices passes ACTION, DEVICE, LABEL, SERIAL, MOUNTPOINT
PORT=8080
TOKEN=""   # set if SD2NAS_TOKEN is configured on the container
case "$ACTION" in
  ADD)
    # format=text returns two lines: level (normal|warning) and message,
    # so the script doesn't depend on jq being installed on the host
    if RESULT=$(curl -fsS --max-time 7200 -X POST \
        "http://localhost:$PORT/api/ingest?wait=1&format=text" \
        -H "Content-Type: application/json" \
        ${TOKEN:+-H "Authorization: Bearer $TOKEN"} \
        -d "{\"path\":\"/cards/$(basename "$MOUNTPOINT")\",\"label\":\"$LABEL\",\"serial\":\"$SERIAL\"}"); then
      LEVEL=$(echo "$RESULT" | sed -n 1p)
      MSG=$(echo "$RESULT" | sed -n 2p)
      /usr/local/sbin/rc.unassigned umount "$DEVICE"
    else
      # Container down or ingest errored: leave the card mounted so it can be retried
      LEVEL="alert"
      MSG="Ingest of $LABEL failed. Card left mounted. Check the SD2NAS logs."
    fi
    /usr/local/emhttp/webGui/scripts/notify -s "SD2NAS" -d "$MSG" -i "$LEVEL"
    ;;
esac
```

Unraid's built-in notifications are pushed to the phone by whatever agent is
configured there (Pushover, ntfy, Telegram…), so SD2NAS doesn't need its own
notification setup. Unmounting is done by the host script because a container
can't unmount host drives without privileged mode.

**Fallback poller** (setting, off by default): the container watches `/cards`
for new mount points, for setups that don't want a host script. It can't
unmount or notify through Unraid.

**Test the hardware before writing code** (milestone 0): check whether your
card reader triggers Unassigned Devices on *card insert* while the reader stays
plugged in, or only when the reader itself is plugged in.

---

## 11. Container, CI and Unraid install

**Dockerfile** (multi-stage): node builds `web/dist` → Go builds a static binary
with the UI embedded → final `alpine` image containing the binary plus
`tzdata`, `exiftool` (~20 MB, RAW previews for every format including CR3) and
`ffmpeg` (~80 MB, video poster frames). Both are installed with `apk` from
Alpine's packages. Expected image size is about 120 MB. Exposes 8080, runs as `99:100`, with a `HEALTHCHECK` on `/healthz`.

**GitHub Actions**
- `ci.yml`: `go vet`, `go test ./...`, `npm ci && npm run build` on every push
  and PR.
- `release.yml`: on push to `main` publish `ghcr.io/aspiziri/sd2nas:edge`; on a
  `v*` tag publish `:X.Y.Z` and `:latest`. linux/amd64 only. The package is
  public because the repo is. ghcr image names must be lowercase.

**Unraid template** `deploy/unraid/sd2nas.xml`: repository, web UI link,
port, the three paths (with `/cards` as RW/Slave), `TZ`, `UMASK=000`, optional
`SD2NAS_TOKEN`, and `--user 99:100` in Extra Parameters. The image isn't
linuxserver-based, so it doesn't use their `PUID`/`PGID` convention; the
binary applies `UMASK` itself at startup. Install by adding the repo's `deploy/unraid`
URL under Docker → Template repositories, or by pasting the values into Add
Container. Updates show up in the Docker tab like any CA app.

**Local development**
- `go run ./cmd/sd2nas --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config`
- `npm run dev` in `web/` with a Vite proxy to the Go server
- `deploy/docker-compose.dev.yml` runs the real image on the desktop against
  test folders before deploying

---

## 12. Gotchas to handle in code

- **Time zones.** EXIF `DateTimeOriginal` has no time zone and is treated as
  local. File modified times and MP4 `mvhd` times are absolute and get converted
  with the container's `TZ`. A container left on UTC would name videos hours off.
  The template sets `TZ`, and `/healthz` reports it.
- **GoPro `mvhd` times.** Some GoPro firmware writes local time into the
  UTC field. Add a per-camera "video time is local" setting, detected from
  the `udta` make/model.
- **exFAT modified times** on cards are local with no zone, so they're
  unreliable as a fallback. Prefer EXIF and `mvhd`.
- **RAW + JPEG pairs** share a timestamp and differ only by extension, so they
  don't collide (they go in different subfolders anyway).
- **Burst shots** collide on the one-second timestamp. CameraImport's `-1`,
  `-2` suffixes handle it; keep the order stable by sorting on original card
  filename within the same second.
- **Cross-mount moves** (`EXDEV`): always fall back to copy, verify, delete.

---

## 13. Decisions

| Topic | Decision |
|---|---|
| Archive layout | Flat folders named `YYYY-MM-DD - Title`, about 150k photos. Promote pattern defaults to `{name}`. |
| RAW previews | `exiftool` in the image |
| Video thumbnails | `ffmpeg` in the image |
| Frontend | Svelte 5 + Vite, single-page app with no SvelteKit. The UI is written from scratch, so there's no reason to start on the older version. |
| Dependencies | Approved: `modernc.org/sqlite`. Carried over: `goexif`, `go-mp4`, `imaging`. Frontend: `svelte`, `vite`. |

Still to confirm: whether the date folders sit directly in the archive root or
inside year folders (the pattern is a one-line setting either way).

---

## 14. Milestones

Each milestone ends with tests passing and a commit. The checklist under each
is what "done" means.

### M0: Skeleton and hardware check
- [ ] Card reader tested with Unassigned Devices: insert-triggered or reader-plug only
- [ ] Confirm the variables Unassigned Devices passes to device scripts (`LABEL`, `SERIAL`, `MOUNTPOINT`) on your Unraid version
- [ ] Go module, `cmd/sd2nas` serving "hello" and `/healthz`
- [ ] Dockerfile builds; image runs locally
- [ ] CI and release workflows publish `ghcr.io/aspiziri/sd2nas:edge`
- [ ] Image installed on Unraid from the template and reachable at `http://tower:8080`

### M1: Media core
- [ ] `internal/media` ported with CameraImport's tests passing
- [ ] `CopyHashed`, `QuickFingerprint`, `mvhd` capture time
- [ ] Fixtures in `testdata/` (real tiny JPEG, ARW, MP4)

### M2: Catalog and ingest (headless)
- [ ] Schema and migrations; catalog tests on a temp database
- [ ] Ingest job against a folder of fixtures: new, known, seen before, failed
- [ ] `POST /api/ingest?wait=1` returns the summary
- [ ] Re-running on the same folder imports 0 files

### M3: Web UI, read-only
- [ ] Svelte app embedded; ingest page with live SSE progress and history
- [ ] Staging list and folder view with thumbnails (JPEG first)
- [ ] Phone layout: bottom tab bar, one-column cards, full-screen preview
- [ ] PWA manifest and icon (Add to Home Screen)

### M4: Staging operations
- [ ] Rename, merge, move files, cull, discard, promote, all with tests on temp dirs
- [ ] Startup reconcile for changes made over SMB
- [ ] Touch-friendly selection (long-press to start selecting, swipe-select in grids)

### M5: Archive indexer
- [ ] Resumable backfill with progress
- [ ] Nightly rescan, aliases, "changed since indexed" view
- [ ] Backfill run against the real archive

### M6: Unraid integration
- [ ] `unassigned-devices.sh`: ingest, unmount, notify
- [ ] Template finalized; install doc in README
- [ ] End-to-end test on the NAS with a real card, twice (second run imports 0)

### M7: Polish
- [ ] Extra staging views (timeline, list)
- [ ] RAW and video thumbnails (exiftool, ffmpeg)
- [ ] Optional token auth
- [ ] `v0.1.0` tag → `:latest`
