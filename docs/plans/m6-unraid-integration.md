# M6 Unraid Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make SD2NAS run unattended on Unraid. Inserting a card runs `deploy/unassigned-devices.sh`. The script imports the card, unmounts it when SD2NAS answers 200, and passes SD2NAS's own message to Unraid's `notify`. SD2NAS queues its own notices (stale staging folders once a day, a failed nightly backup, files the archive scan marked missing). The card script delivers them after every card and hourly from User Scripts. A panic in an import fails that import and the server keeps serving. A second SD2NAS on the same `/config` stops at startup with exit code 3. An optional in-container poller imports cards for setups without the host script. The template is finalized and a README documents the install.

**Architecture:**
- **`catalog.tx`** rolls back in a defer before anything else, so a panic can't leak the single SQLite connection.
- **Panics:** `media.ReadExif`/`ReadVideoMeta` turn a third-party parser panic into an error. The ingest worker and its classify goroutines turn a panic into a failed import or a failed file.
- **`internal/lockfile`** takes a non-blocking exclusive lock on `/config/sd2nas.lock`, using `flock` on Linux and `LockFileEx` through `syscall` on Windows. `main` takes it before anything opens the catalog.
- **Catalog schema v4** adds a `notifications` table (unique `key`, `taken_at`).
- **`internal/notices`:** a goroutine that reads `indexer.Status()` and the staging listing every 30 s. It never runs inside a service callback. It queues one notice per event, and each event's key keeps a restart from repeating it.
- **API:** `GET /api/notifications` and `POST /api/notifications/take` (atomic, `?format=text`), plus `Server.SubmitIngest`, which submits an import and registers it with the ingest feed the way `POST /api/ingest` does.
- **`internal/cardwatch`:** the opt-in poller (`SD2NAS_POLL_CARDS`). It treats what is already mounted at startup as a baseline and never imports it. A new card must be a mount point with `DCIM/` or `PRIVATE/` and stay for 10 s; each appearance is submitted once.
- **The card script:** bash + curl only. Its Go test runs it with real bash and curl against an `httptest` server, with stub `blkid`, `rc.unassigned`, `logger` and `notify`.

**Tech Stack:** Go (module `github.com/aspiziri/SD2NAS`, `go 1.24.0`), `net/http`, `log/slog`, `syscall`, `modernc.org/sqlite`; bash + curl (Unraid host). No new dependencies. No web UI changes.

**Spec:** `docs/PLAN.md` §16 M6 (the checklist), §11 (card script outline, fallback poller, hardware results), §3 ("safe to format", the daily stale-staging notification, the backup), §7 (trigger, failure handling, the ignore list), §10 (API, request guard, optional token), §13, §14 (template, install, updating). Context: `HANDOFF.md` ("M6 must build on / must not break", the lock-order rules, NAS facts). Structure template: `docs/plans/m5-archive-indexer.md`.

## Global Constraints

- Module path `github.com/aspiziri/SD2NAS`, `go 1.24.0` in `go.mod`, with no `toolchain` line.
  - **Use no API newer than Go 1.24**: no `sync.WaitGroup.Go`, `testing/synctest`, `T.Output` or 1.25 `os.Root` methods.
  - Allowed: `context.WithoutCancel`, `context.AfterFunc`, `min`/`max`, `slices`, `maps`, `t.Context()`, `slog.DiscardHandler`, range over an int, `sync.Mutex.TryLock`.
- **No new dependencies.** `go.mod`, `go.sum`, `web/package.json` and `web/package-lock.json` must not change.
  - `golang.org/x/sys` is only an indirect dependency and **must not be imported**. Locking uses `syscall` (Linux `syscall.Flock`, Windows `kernel32.dll` `LockFileEx`/`UnlockFileEx` through `syscall.NewLazyDLL`, as `internal/ingest/freespace_windows.go` does).
  - The card script uses only bash, curl, sed, mktemp, rm, logger and the Unraid commands named below. No jq.
- **M6 touches:**
  - `internal/catalog/` (`catalog.go`, `schema.go`, new `notifications.go`)
  - `internal/media/` (`exif.go`, `video.go`: panic recovery only)
  - `internal/ingest/` (`manager.go`, `run.go`)
  - the new `internal/lockfile/`, `internal/notices/` and `internal/cardwatch/`
  - `internal/config/config.go`
  - `internal/api/` (new `notifications.go`, `SubmitIngest` in `ingestfeed.go`)
  - `cmd/sd2nas/`
  - `deploy/unassigned-devices.sh` (new), `deploy/doc.go`, `deploy/*_test.go`, `deploy/unraid/sd2nas.xml`
  - `README.md` (new) and `docs/PLAN.md` (last task)

  Do not modify `web/`, `internal/indexer/`, `internal/staging/`, `internal/thumbs/`, `.github/`, `deploy/Dockerfile` or `deploy/entrypoint.sh`, with one exception: Task 5 changes the schema-version assertion in `internal/indexer/backup_test.go` (test-only). The indexer and staging packages are otherwise only *read* (`indexer.Status`, `staging.Listing`, `staging.Status`).
- **Binding values** (reviewers check them verbatim):
  - **Config lock:**
    - The file is `<config>/sd2nas.lock` (`/config/sd2nas.lock` in the container). It is created if missing and never deleted.
    - The holder writes one line into it: `pid <pid> on <hostname> since <RFC 3339 UTC>`.
    - `main` takes the lock after resolving paths and applying `UMASK`, and **before** `api.New`, `catalog.Open`, `mgr.Recover`, `indexer.New` and `svc.StartRescan`.
    - **Held** → `run` returns an error whose `Error()` is exactly `<config> is in use by another SD2NAS (<holder>); stop it first`, where `<holder>` is the info line or `unknown`. It is logged as `exiting`, and the process exits with code **3**. Every other startup error still exits **1**.
    - **Unsupported** (Linux `ENOLCK`/`EOPNOTSUPP`/`ENOSYS`, or an OS without locks) → log at Warn, and add the `/healthz` warning `could not lock <config> (<err>): a second SD2NAS using this folder would not be stopped`. Startup continues.
    - **Any other lock error** → log at Error `cannot lock the config folder; ingest is disabled` (key `err`), add the `/healthz` problem `config lock: <err>`, and don't open the catalog. It serves degraded, exactly like a catalog that failed to open.
  - **Panics:**
    - **A panic in an import:**
      - The import is recorded as `failed` with the error `internal error: <panic value>` and the counts it had reached.
      - The card script's text is therefore `<card>: import failed: internal error: <v>.`[` <N> new copied before it stopped.`]` Card left mounted.` (level `alert`, HTTP 500). The bracketed part appears only when `Counts.New > 0`, as `Describe` already words it.
      - If recording the failure itself fails, the error is `internal error: <v>; record the failed ingest <id>: <err>`, so the panic text is never lost.
      - The worker goes on to the next queued job.
      - The log line is `ingest panicked`, with keys `ingest`, `panic` and `stack`.
    - **A panic while classifying a file:** that file gets outcome `failed` with the error `internal error: <v>`, and the log line is `classifying a file panicked` (keys `file`, `panic`, `stack`).
    - **`media.ReadExif`:** a panic becomes the error `exif <path>: the decoder panicked: <v>`.
    - **`media.ReadVideoMeta`:** a panic becomes the error `video <path>: the parser panicked: <v>`.
  - **Notices** (catalog table `notifications`, schema version **4**):
    - Levels are `normal`, `warning` and `alert`. Messages are one line (control characters become spaces, trimmed), at most 1000 bytes (cut at a rune boundary and ended with `…`).
    - `catalog.AddNotification` enforces this for every caller, because the text take's two-line framing depends on it.
    - **Keys and texts:**

      | Key | Level | Message |
      |---|---|---|
      | `backup:<BackupResult.At, UTC, RFC 3339 nano>` | `alert` | `Nightly catalog backup failed: <error without trailing ". ">. Older backups were kept.` |
      | `missing:<ScanResult.FinishedAt, UTC, RFC 3339 nano>` | `alert` | `The archive scan marked 1 file missing from Archive/. See the Archive index page.` / `The archive scan marked <N> files missing from Archive/. See the Archive index page.` |
      | `staging:<local date YYYY-MM-DD>` | `warning` | see the stale-staging texts below |

    - **Stale-staging texts:**
      - one folder: `1 folder has been in Staging for <D> days or more: <name>.`
      - several: `<N> folders have been in Staging for <D> days or more: <list>.`
      - `<D>` is `staging_warn_days`; with 1 it reads `1 day or more`.
      - `<list>` is the names, oldest first (`AgeDays` descending, then name), joined as `a and b`, `a, b and c`, and so on up to 5. With more than 5 it is the first 5 joined by `, ` plus ` and <N-5> more`.
      - A folder is stale exactly when `staging.Folder.Stale` is true (the M4 rule `AgeDays >= staging_warn_days`, never when the setting is 0).
    - **Checks:**
      - Every **30 s**, plus once at `Start`.
      - Archive notices are queued only for events at most **24 h** old when first seen.
      - The service remembers in memory each key it has already stored or found, so the 30 s checks don't open a write transaction each time.
      - The stale-staging check runs once per local day, at or after **09:00** container time (`time.Local`).
      - It runs only once the startup staging reconcile has finished (`staging.Status().Rescanning` false and `LastRescan` set).
      - If it fails, it is retried after 1 h.
      - Notices are deleted **30 days** after `created_at`.
      - A take returns at most **100** notices, oldest first.
  - **Endpoints:**
    - `GET /api/notifications` → 200 `{"pending": [Notification…]}`.
    - `POST /api/notifications/take` (the guard requires `X-SD2NAS: 1`; the body is not read) marks every pending notice (up to 100) taken in one transaction and returns them:
      - JSON: 200 `{"notifications": [Notification…]}`;
      - with `?format=text`: 200 `text/plain; charset=utf-8`, two lines per notice (`<level>\n<message>\n`), and an empty body when there are none.
    - `Notification` is `{"id": int, "key": string, "level": string, "message": string, "created_at": RFC 3339}`. The lists are never null.
    - After `Close`, both answer 503 `SD2NAS is shutting down` (with `?format=text`, `writeFail`'s two lines `alert` / that sentence). A catalog error → a logged 500.
  - **Fallback poller:**
    - Flag `--poll-cards`, env `SD2NAS_POLL_CARDS`, default off.
    - The env value goes through `strconv.ParseBool`, with `""` = off. A bad value → `config.Load` error `SD2NAS_POLL_CARDS "<v>" is not true or false`. The flag overrides the env.
    - It reads the cards folder every **5 s**.
    - Entries present at its first read are never imported while they stay.
    - A new entry is imported when all of these hold:
      - it is a directory not starting with `.`;
      - it is a mount point (a different `st_dev` than the cards folder; always true on non-Linux/darwin);
      - it has a top-level directory named `DCIM` or `PRIVATE` (case-insensitive);
      - it has qualified for **10 s**.
    - The import is submitted with `Path` = the entry's path and `Label` = its name, through `api.Server.SubmitIngest`.
    - Each appearance is submitted once, whatever the result. An entry that disappears and comes back counts as new.
    - Log lines: `card poller: importing` (keys `card`, `ingest`); `card poller: could not start an import` (keys `card`, `err`); `card poller: not importing what was already mounted at startup` (key `names`).
  - **Card script** `deploy/unassigned-devices.sh` (full text in Task 7). Variables and defaults:
    - `SD2NAS_CONF=/boot/config/plugins/unassigned.devices/sd2nas.cfg` (sourced when readable)
    - `SD2NAS_URL=http://localhost:8080`
    - `SD2NAS_TOKEN=` (empty)
    - `SD2NAS_NOTIFY=/usr/local/emhttp/webGui/scripts/notify`
    - `SD2NAS_MAX_TIME=7260`
    - `PATH` gets `:/usr/local/sbin:/usr/sbin:/sbin:/usr/bin:/bin` appended

    Behaviour:
    - `ACTION=ADD` → POST `/api/ingest?wait=1&format=text` with `X-SD2NAS: 1`, `Content-Type: application/json` and the body `{"path":"/cards/<basename of MOUNTPOINT>","label":"<LABEL>","uuid":"<blkid UUID>"}` (JSON-escaped; control characters → spaces).
      - It unmounts with `rc.unassigned umount "$DEVICE"` **only** when the reply is HTTP 200 with the two-line text.
      - It then always delivers notices.
    - `ACTION=NOTIFY` → deliver notices only.
    - `ACTION=ERROR_MOUNT` → one alert.
    - Every other `ACTION` → nothing.
    - It always exits 0.
    - Notification command: `"$SD2NAS_NOTIFY" -e "SD2NAS" -s "SD2NAS" -d "<message>" -i "<level>"`.
    - Its own texts (`<name>` = `LABEL`, else the mount point's basename, else `DEVICE`):
      - `Unassigned Devices gave no mount point for <DEVICE>, so the card was not imported.` (alert)
      - `SD2NAS did not finish importing <name> in time. Card left mounted. Check SD2NAS.` (alert, curl exit 28)
      - `SD2NAS did not answer, so <name> was not imported. Card left mounted. Is the container running?` (alert, any other curl failure)
      - `SD2NAS could not import <name> (HTTP <code>). Card left mounted. Check SD2NAS.` (alert, a reply that isn't SD2NAS's two lines)
      - the suffix ` Unmounting failed: unmount it in Unassigned Devices before removing the card.` (a `normal` level becomes `warning`)
      - `Unassigned Devices could not mount the card in <DEVICE>, so it was not imported.` (alert)
  - **Template** `deploy/unraid/sd2nas.xml` (Task 11 lists every value): `<ExtraParams>--restart=unless-stopped</ExtraParams>`, `<Icon>https://raw.githubusercontent.com/aspiziri/SD2NAS/main/web/public/icon-512.png</Icon>`, a `<Requires>`, the new `SD2NAS_POLL_CARDS` variable (default `false`), and no `SD2NAS_TOKEN` entry.
- **Lock-order rules (HANDOFF):**
  - `indexer.OnChange` and the staging service's `OnChange` callbacks run under service mutexes. M6 adds **no** callback to either.
  - `internal/notices` reads `indexer.Status()` and `staging.Service.List` from its own goroutine.
  - Ingest-feed finished hooks run under the feed mutex; M6 adds none.
  - Ingest stays the only party that waits on folder locks. The poller submits; it never locks.
- SQLite has **one connection**: never call a `catalog.DB` method while a `*sql.Rows` from it is open, and inside `d.tx(…)` use only `tx`.
- **Tests pass on Windows and Linux.**
  - Use `path/filepath` for OS paths.
  - Close everything you open, and call `t.TempDir()` before registering a `Close` cleanup.
  - Waiting tests use a 10 s deadline. Sleeps are allowed only inside polling loops.
  - Tests that need bash and curl skip on Windows, and skip elsewhere when either is missing, unless `SD2NAS_REQUIRE_TOOLS=1` (then they fail).
  - Time-dependent logic takes `Now`/`now` seams; tests never wait for wall-clock 09:00 or 10 s.
- **Commands.**
  - Windows (Git Bash, repo root): `gofmt -l .` (prints nothing), `go vet ./...`, `go test ./...`.
  - **Linux** (Docker Desktop running). It adds `bash curl` to M5's command, and the gate script's Linux step must be updated to match:

    `MSYS_NO_PATHCONV=1 docker run --rm -e TZ=Pacific/Chatham -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q bash curl exiftool ffmpeg && go vet ./... && go test ./...'`
  - **Race** (Tasks 3, 6, 8, 9): `MSYS_NO_PATHCONV=1 docker run --rm -e SD2NAS_REQUIRE_TOOLS=1 -v "$(pwd -W):/src" -v sd2nas-gomod:/go/pkg/mod -w /src golang:1.26-alpine sh -c 'apk add -q build-base bash curl exiftool ffmpeg && CGO_ENABLED=1 go test -race ./internal/... ./cmd/... ./deploy/...'`
  - No web step: `web/` must not change.
- **Never silently swallow an error.** Return it wrapped with `%w`, answer the request with it, or log it with `slog` (key `"err"`). In the script, `log` it to syslog. The exceptions:
  - a failed write to an HTTP client that has gone;
  - `os.Hostname` failing in the lock's info line (it becomes `unknown host`);
  - an unreadable holder line in `HeldError` (it becomes `""`, and the message says `unknown`).
- Export only the identifiers in each task's **Produces** block. **Each task must compile and pass on its own**, applied in order.
- **Commits:** on branch `m6-unraid-integration`, one commit per task (more is fine), with the message given in the task.
  - Every message ends with a blank line and then exactly `Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>`. No other trailer, no other model name.
  - Never commit `HANDOFF.md`, `tmp/`, `web/node_modules/` or anything in `web/dist/` other than `.placeholder`.
  - Write files as UTF-8 with LF line endings, including `deploy/unassigned-devices.sh`, which `.gitattributes` already forces to LF. Check that `§`, `…` and `—` survive.
  - Never push.

## Review Focus

1. **The single SQLite connection can't leak, and a panicking import doesn't wedge anything.**
   - `tx` defers its rollback before calling `fn`.
   - A panic in `Run` releases the folder locks (deferred `releaseFolders`), records the import `failed`, finishes the job (so `?wait=1` answers 500 `alert`), and the next queued job runs.
   - A panic in a classify goroutine fails one file.

   Pinned in Task 1 (`TestTxRollsBackOnPanic`), Task 2 (`TestReadExifPanic`, `TestReadVideoMetaPanic`) and Task 3 (`TestWorkerSurvivesPanic`, `TestPanicReleasesFolderLocks`, `TestClassifyPanicFailsFile`).
2. **"Safe to format" comes only from the server.**
   - The script unmounts only on HTTP 200 with SD2NAS's two lines. It passes the server's level and message through unchanged (apart from the unmount-failed suffix), and never inspects the message text.
   - LABEL and the mount point are JSON-escaped.
   - Container down, a timeout, a non-SD2NAS reply and a refusal all leave the card mounted.

   Pinned in Task 7 (every `TestScript*`).
3. **A second instance stops before it touches anything.**
   - The lock is taken before `catalog.Open`, `Recover` (which fails "running" imports and deletes partials) and `indexer.New` (which closes "unfinished" scans).
   - Held → exit 3 with the holder's line.
   - The lock is released after `db.Close` (defer order).
   - An unsupported filesystem doesn't block startup but says so.

   Pinned in Task 4 (`TestAcquireHeld`, `TestHeldAcrossProcesses`, `TestExitCode`) and the Task 12 two-container smoke run.
4. **Notices never run under a service lock, never repeat, never skip a day.**
   - No `OnChange` hook is added.
   - Keys are unique in the catalog, so a restart re-checks without re-queuing. An event older than 24 h is ignored.
   - The staging check runs at 09:00 or later, once per day; a restart after 09:00 runs it (deduplicated by key); a failure retries hourly.
   - Take is one transaction, so two concurrent script runs never both deliver a notice.

   Pinned in Task 5 (`TestTakeNotifications*`, `TestAddNotificationOnce`) and Task 6 (every `TestCheck*`).
5. **The poller can't loop or import the wrong disk.**
   - What is mounted at startup is never imported.
   - It imports only mount points with `DCIM/` or `PRIVATE/` that stayed 10 s, one submit per appearance.
   - Its jobs are watched by the ingest feed, so they send `ingest.finished`.

   Pinned in Task 8 (`TestSubmitIngestWatched`) and Task 9 (every `TestPoller*`).
6. **Shutdown.** The notices loop and the poller stop on `Close`. The poller closes before the manager. The notices service closes first among `srv.Close`'s closers, before staging and the indexer. `docker stop` stays under 2 s (Task 12 smoke run).
7. **Docs match reality.** The README and §16's owner steps name the exact Unassigned Devices, Notification Settings and User Scripts settings, and the script's config file. The owner-only items stay unticked.

## Decisions

1. **Notices reach Unraid through the card script, which takes them from SD2NAS** (option a + c). (owner may override)
   - SD2NAS queues notices in the catalog.
   - `deploy/unassigned-devices.sh` takes them (`POST /api/notifications/take?format=text`) and calls the host's `notify` for each:
     - after every card (`ACTION=ADD`);
     - hourly from a User Scripts entry that runs it with `ACTION=NOTIFY`.
   - Why not the others:
     - **(b) bind-mounting Unraid's notification folders** and writing `.notify` files: Unraid's `notify` (PHP, emhttp) is what runs the agents (Pushover, ntfy, Telegram…). A file dropped into `/tmp/notifications/unread` would show in the web UI but never reach the phone, and its format is undocumented.
     - **(d) a webhook:** it would mean a second notification setup outside Unraid.
     - **(c) alone:** with no card for a week, nothing would arrive.
   - **Cost of the alternative (d):** a URL setting, an HTTP client with retries, and the owner configuring ntfy/Gotify separately.
   - **What the owner must verify on the NAS:**
     - the User Scripts plugin is installed (or a `.cron` file is used instead; see the README);
     - `/usr/local/emhttp/webGui/scripts/notify` exists (M0 confirmed it);
     - a manual `ACTION=NOTIFY bash …/sd2nas.sh` run delivers a queued notice.
2. **Take is at-most-once:** marking notices taken and returning them is one transaction. (owner may override)
   - Two script runs at once (two slots, or a card during the hourly run) can't both deliver one notice.
   - If `notify` fails on the host, the notice is lost but logged to syslog. `notify` is a local script and practically never fails.
   - **Cost of the alternative** (fetch, then ack): two requests, and duplicates when runs overlap unless the script adds `flock`.
3. **Hourly delivery** (the User Scripts "Hourly" schedule). (owner may override) A daily notice doesn't need minutes. Any schedule works; the cost of a shorter one is a negligible localhost request.
4. **The stale-staging check runs once a day at 09:00 container time, a constant.** (owner may override)
   - "Daily" means the first 30 s check at or after 09:00 local, so the hourly delivery lands in the morning, not at night.
   - Restarts don't repeat it (key `staging:<date>`) and don't skip it: a restart after 09:00 runs it if today's key doesn't exist.
   - It waits for the startup staging reconcile to finish, so folders adopted at startup are in the list.
   - **Cost of the alternative** (a `notify_at` setting): one settings key, its validation, and an update to `settings_test`, which hard-codes 7 keys.
5. **Missing-file notices come from any pass that newly marks files missing, not only the nightly one.** (owner may override) Missing files block "safe to format" and may mean a dropped disk, which matters however the pass started. Cost of nightly-only: one condition, but a manual pass the owner walked away from would stay silent.
6. **Levels:** stale staging `warning`, backup failed `alert`, files missing `alert`. (owner may override) The cost is trivial; the levels decide which notices reach the phone under the Notification Settings agent ticks.
7. **Archive events older than 24 h when first seen are not notified.** That way the first M6 start doesn't announce last week's failed backup. With a 30 s check and keys stored at once, a live event is never older than 24 h unless SD2NAS was down that long.
8. **Notices are kept 30 days**, and the catalog backup carries them. Pending ones older than that are stale, which happens when no script ever collects them.
9. **The poller is switched on by env/flag (`SD2NAS_POLL_CARDS` / `--poll-cards`), not a catalog setting.** (owner may override)
   - Whether a host script exists is an install-time fact, and it belongs in the template next to the mounts.
   - A setting would need live start/stop and a settings-test change.
   - Cost of a setting: about 40 lines plus tests, plus a UI toggle in M7.
10. **Which folders the poller imports.** (owner may override the `DCIM/`/`PRIVATE/` rule)
    - **Startup baseline:** `/cards` is all of `/mnt/disks`, so permanently mounted Unassigned Devices disks sit next to cards. The baseline also means a card left in the reader across a container update isn't imported in a loop.
    - **Mount-point check:** it skips the folder Unassigned Devices creates before mounting.
    - **`DCIM`/`PRIVATE` rule:** a USB backup drive mounted later is not imported into Staging. Every camera card has `DCIM/` (DCF); Sony video-only cards have `PRIVATE/M4ROOT`.
    - **10 s settle:** the mount gets a moment to complete.
    - **One submit per appearance:** a still-mounted card (the poller can't unmount) is never re-imported.
    - Cost of dropping the folder rule: any later-mounted disk would be imported.
11. **The poller polls every 5 s instead of using inotify.** Mount events through a `rslave` bind mount don't reliably reach inotify, there is no dependency, and one `ReadDir` every 5 s is free.
12. **Panic-recovery scope.** (owner may override)
    - **Recovered:**
      - the ingest worker (§16);
      - its classify goroutines, because a goroutine's panic can't reach the worker's recover and would end the process;
      - the two third-party parsers, `media.ReadExif` (goexif) and `media.ReadVideoMeta` (go-mp4 with type assertions). These are the only code that parses untrusted file contents, and covering them also protects the staging rescan and `adopt`, which read capture times.
    - Thumbnails already recover (M3).
    - **Not recovered:** promote, the staging rescan goroutine, the indexer pass, the nightly loop, the ingest feed, notices and the poller. They parse nothing untrusted, and recovering a goroutine that holds service mutexes, staging locks or the pass lock would leave the service wedged.
    - A crash restarts the container (Decision 13), and every startup recovery path already exists: failed imports, partials, unfinished scans, staging reconcile.
    - Cost of wider recovery: per-service unwind code and tests, and a risk of silent wedges.
    - `*sql.Rows` loops outside `tx` are not hardened. They only scan columns, and a panic there would be a bug that the restart clears.
13. **`--restart=unless-stopped` goes in the template's Extra Parameters.** (owner may override)
    - Unraid sets no restart policy, so without it a crash leaves SD2NAS down until someone notices. The card script would then say "SD2NAS did not answer".
    - Existing installs must add it by hand (README).
    - With it, Docker restarts a second instance that exits 3 on the config lock, with back-off, and it logs the same line each time. That is harmless; the README tells the owner to stop the duplicate.
    - Cost of leaving it out: no self-healing after a crash.
14. **The config lock.**
    - It is `/config/sd2nas.lock`, taken with `flock(LOCK_EX|LOCK_NB)` or `LockFileEx(EXCLUSIVE|FAIL_IMMEDIATELY)` on 1 byte at offset 2³², so the holder line at the start stays readable. The OS drops it when the process dies.
    - Held → exit 3.
    - Unsupported → warn and continue. (owner may override: refuse to start instead) Unraid's appdata is an exclusive ZFS share, where `flock` works; refusing would only hurt odd setups. Cost of refusing: SD2NAS won't start on a filesystem without locks.
    - The lock is taken before `api.New`. The `/config` write probe that follows only creates and removes a temp file.
15. **The script reads `sd2nas.cfg`.** (owner may override)
    - The script lives at `/boot/config/plugins/unassigned.devices/sd2nas.sh`, and its settings in `sd2nas.cfg` next to it (`SD2NAS_URL=http://localhost:4040` on the owner's NAS, whose host port is 4040). The default URL matches the template's default host port, 8080.
    - Updating the script is then a single `curl` that keeps the settings.
    - Cost of the alternative (editing variables at the top of the script): the settings are lost on every update.
16. **How the script reads the reply.**
    - It uses curl without `-f`, writing the body to a temp file and `%{http_code}` to stdout, so a non-2xx reply's explanation (refused for space, a bad path) reaches the notification.
    - It unmounts only on 200. `--max-time 7260` sits 60 s past the server's own 2-hour reply (`DefaultMaxWait`), so that reply wins.
    - `--connect-timeout 10` makes a stopped container fail fast.
17. **The script's test is a Go test** (`deploy/script_test.go`).
    - It runs real bash and real curl against an `httptest` server, with stub `blkid`, `rc.unassigned` and `logger` first on `PATH`, and `notify` through `SD2NAS_NOTIFY`.
    - It runs in CI (ubuntu has bash and curl) and in the Linux gate (`apk add bash curl`), and skips on Windows.
    - Task 12 also runs the script against the real image.
    - Why Go: the repo's test runner and gate already run it; there is no bats or shellcheck dependency.
18. **The `SD2NAS_TOKEN` entry leaves the template until M7 implements tokens.** (owner may override: keep it, described as "not used yet") Its current description claims protection that doesn't exist. The script still sends `Authorization: Bearer` when a token is configured (§11), so M7 needs no script change.
19. **Schema version 4** adds `notifications`. Older SD2NAS versions refuse a v4 catalog, so downgrading means restoring a backup from `Management/SD2NAS` (README).
20. **No UI in M6.** (owner may override: a notices list on a page) Notices go to the phone through Unraid, and `GET /api/notifications` shows what's waiting. Cost of a page: a new view, its node tests and the desktop/phone check.
21. **Actions:**
    - `ERROR_MOUNT` sends an alert. Otherwise an unreadable card would give no feedback at all.
    - `UNMOUNT`, `REMOVE` and `ERROR_UNMOUNT` do nothing. They are Unassigned Devices' own housekeeping, and our unmount's failure is detected from its exit status.

## File Structure

```
internal/catalog/
  catalog.go          tx: rollback deferred first (Task 1); SchemaVersion = 4 (Task 5)
  schema.go           schemaV4: notifications                                     (Task 5)
  notifications.go    Notification, Notice* levels, Add/Has/Pending/TakeNotifications (Task 5)
internal/media/
  exif.go, video.go   decodeExif / readBoxStructure seams, panic → error          (Task 2)
internal/ingest/
  manager.go          runRecovered: a panicking import fails, the worker goes on  (Task 3)
  run.go              deferred releaseFolders; classifyRecovered                  (Task 3)
internal/lockfile/
  lockfile.go         ErrHeld, ErrUnsupported, HeldError, Lock, Acquire, Release  (Task 4)
  lock_unix.go        flock (linux || darwin)                                     (Task 4)
  lock_windows.go     LockFileEx / UnlockFileEx via syscall                       (Task 4)
  lock_other.go       ErrUnsupported                                              (Task 4)
cmd/sd2nas/
  main.go             config lock + exit code 3 (Task 4); notices, poller wiring (Task 10)
  main_test.go        TestExitCode                                                (Task 4)
internal/notices/
  notices.go          Config, Service, New, Start, Close, Pending, Take, the checks (Task 6)
deploy/
  doc.go              package deploy (so ./... has a package for the tests)      (Task 7)
  unassigned-devices.sh   the card script                                         (Task 7)
  script_test.go      runs the script with bash + curl against httptest           (Task 7)
  template_test.go    checks sd2nas.xml                                           (Task 11)
  unraid/sd2nas.xml   finalized template                                          (Task 11)
internal/api/
  notifications.go    MountNotifications: GET /api/notifications, POST …/take    (Task 8)
  ingestfeed.go       Server.SubmitIngest                                         (Task 8)
internal/config/config.go   PollCards (--poll-cards, SD2NAS_POLL_CARDS)           (Task 9)
internal/cardwatch/
  cardwatch.go        Config, Watcher, New, Start, Close, the tick                (Task 9)
  mount_unix.go / mount_other.go   isMount                                        (Task 9)
README.md             install, card script, notifications, poller, updating       (Task 11)
docs/PLAN.md                                                                      (Task 12)
```

---

### Task 1: `catalog.tx` rolls back in a defer first

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/catalog/catalog.go`, `internal/catalog/catalog_test.go`

**Interfaces:**
- Consumes: `DB.tx`.
- Produces: no new exports. `tx` keeps its signature and its error behaviour: fn's error is returned, joined with a rollback failure other than `sql.ErrTxDone`, and a Commit error is returned as is.

- [ ] **Step 1: Write the failing test** in `catalog_test.go`:
  - `TestTxRollsBackOnPanic`:
    - Open a test DB (`openTest`).
    - Call `d.tx` with an fn that inserts `('x','y')` into `settings` through `tx` and then panics with `"boom"`. The call is wrapped in a func with `defer recover()` that asserts the panic value is `"boom"`, so the panic still reaches the caller.
    - Then, with `ctx, cancel := context.WithTimeout(ctx, 5*time.Second)`: `d.db.QueryRowContext(ctx, "SELECT count(*) FROM settings WHERE key = 'x'")` scans `0`. Without the fix, the leaked transaction keeps the only connection and this fails with a deadline error.
    - Then `d.SetSetting(ctx, KeyStagingWarnDays, "7")` succeeds.
  - `TestTxErrorStillRollsBack`: an fn that inserts and returns `errors.New("no")` → `d.tx` returns an error matching it, and the row is absent.
- [ ] **Step 2: Run and confirm the panic test fails**: `go test ./internal/catalog/ -run 'TestTx' -v` → `TestTxRollsBackOnPanic` fails (deadline exceeded).
- [ ] **Step 3: Implement** — replace `tx` exactly:

```go
// tx runs fn in a transaction: commit if fn succeeds, else roll back and
// return fn's error joined with any rollback failure. The rollback is
// deferred before fn runs, so a panic in fn (or in Commit) still rolls
// back and gives the single connection back before the panic goes on
// (§16 M6). After a successful Commit the deferred Rollback is
// sql.ErrTxDone, which is ignored.
func (d *DB) tx(ctx context.Context, fn func(*sql.Tx) error) (err error) {
	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			err = errors.Join(err, rbErr)
		}
	}()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
```

- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/catalog.go internal/catalog/catalog_test.go
git commit -m "Roll back a catalog transaction in a defer so a panic can't leak the connection

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 2: Media parsers turn a panic into an error

Suggested implementer: sonnet.

**Files:**
- Modify: `internal/media/exif.go`, `internal/media/video.go`, `internal/media/exif_test.go`, `internal/media/video_test.go`

**Interfaces:**
- Consumes: `exif.Decode`, `mp4.ReadBoxStructure`.
- Produces: no new exports. Two unexported seams, set to the library functions, which only tests replace:

```go
var decodeExif = exif.Decode               // exif.go
var readBoxStructure = mp4.ReadBoxStructure // video.go
```

- [ ] **Step 1: Write the failing tests**
  - `TestReadExifPanic`: replace `decodeExif` with a func that panics `"bad ifd"` (restore it in `t.Cleanup`). `ReadExif(<any fixture JPG path>)` returns a nil `*ExifData` and an error whose text is exactly `exif <path>: the decoder panicked: bad ifd`. Also check that the file was closed: on Windows, `os.Remove` of a temp copy succeeds afterwards.
  - `TestReadVideoMetaPanic`: the same with `readBoxStructure` and the fixture MP4 → `video <path>: the parser panicked: bad box`.
  - `TestCaptureTimeSurvivesParserPanic`: with `decodeExif` panicking, `CaptureTime(<fixture JPG>, KindImage)` returns no error, and `Source == SourceMtime` with `Fallback` non-nil. That is the existing fallback path.
- [ ] **Step 2: Run them and confirm they fail** (the test process panics): `go test ./internal/media/ -run 'Panic' -v`.
- [ ] **Step 3: Implement.**
  - Call `decodeExif(f)` instead of `exif.Decode(f)`, and `readBoxStructure(...)` instead of `mp4.ReadBoxStructure(...)`.
  - In each function, add this deferred recover **after** the deferred close (deferred calls run last-in-first-out, so it runs first and sets the named `err`):

```go
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("exif %s: the decoder panicked: %v", path, p) // video.go: "video %s: the parser panicked: %v"
		}
	}()
```

  Keep the blank first result `_`. It stays nil after a panic, so the recover assigns only `err`. Renaming it would turn the existing `d := &ExifData{…}` / `meta := &VideoMeta{}` into a compile error. Keep every other behaviour unchanged.
- [ ] **Step 4: Run**: `go test ./internal/media/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/media/
git commit -m "Turn a panic in the EXIF or MP4 parser into an error for that file

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 3: Panic recovery in the ingest worker

Suggested implementer: sonnet (opus reviewer: locks and the job lifecycle).

**Files:**
- Modify: `internal/ingest/manager.go`, `internal/ingest/run.go`
- Create: `internal/ingest/panic_test.go`

**Interfaces:**
- Consumes: `Runner` seams `fingerprint`, `copyFile`; `Job.Progress()`; `catalog.FinishIngest`; `ingestRun.releaseFolders`.
- Produces: no new exports.

- [ ] **Step 1: Write the failing tests** (`panic_test.go`, using `newEnv` and a real `Manager` over the env's card):
  - `TestWorkerSurvivesPanic`:
    - Set `r.copyFile` to panic `"boom"` on the first call and work normally afterwards.
    - Submit card A.
    - `<-job.Done()` → the result is `Status failed`, `Error == "internal error: boom"`, `SafeToFormat` false. `Describe` gives level `alert` and a message ending `import failed: internal error: boom. Card left mounted.` The catalog's `Ingest(id)` agrees (status failed, same error).
    - Then submit the same card again → it finishes `done`. The worker kept going.
    - `m.Close` returns nil.
  - `TestPanicReleasesFolderLocks`:
    - With `copyFile` panicking:
    - `newEnv` passes no `Locks`, so the Runner has a private set: use `e.r.locks`.
    - After `Done`, `e.r.locks.TryLock(staging.HolderOperation, <the staging folder id the run created>)` succeeds. Look the folder up with `StagingFolders`, and release the lock afterwards.
  - `TestClassifyPanicFailsFile`:
    - Set `r.fingerprint` to panic for one file of the card (match by name suffix) and to call `media.QuickFingerprint` otherwise.
    - The import finishes `done` with `Counts.Failed == 1` and `SafeToFormat` false.
    - That file's `IngestFiles` row has outcome `failed` and error `internal error: <value>`.
    - The process didn't crash.
- [ ] **Step 2: Run them and confirm they fail** (the test binary panics): `go test ./internal/ingest/ -run 'Panic' -v`.
- [ ] **Step 3: Implement.**
  - **`run.go` `Run`:** right after `StartIngest` succeeds, add `defer j.releaseFolders() // a panic in the pipeline must not leave staging folders locked`. Keep the explicit `j.releaseFolders()` after `pipeline()`, since `releaseFolders` empties the map and so is idempotent.
  - **`run.go` `classifyAll`:** the goroutine calls `j.classifyRecovered(files[i])` instead of `j.classifyOne(files[i])`:

```go
// classifyRecovered is classifyOne with a panic turned into a failed file:
// a panic on a classify goroutine can't reach the worker's recover and
// would end the process (Decision 12).
func (j *ingestRun) classifyRecovered(f CardFile) (c classified) {
	defer func() {
		if p := recover(); p != nil {
			j.log.Error("classifying a file panicked", "file", f.Rel, "panic", p, "stack", string(debug.Stack()))
			c = classified{CardFile: f, outcome: catalog.OutcomeFailed, err: fmt.Errorf("internal error: %v", p)}
		}
	}()
	return j.classifyOne(f)
}
```

  - **`manager.go`:** `run` calls `out, err := m.runRecovered(row, j)` instead of `m.r.Run(m.ctx, row, j.progress)`. The rest of `run` is unchanged.

```go
// runRecovered runs the import and turns a panic into a failed import with
// the counts reached so far, so the worker goes on with the next job
// (§16 M6). Run's deferred folder releases and catalog rollbacks have run
// by the time recover sees the panic.
func (m *Manager) runRecovered(row catalog.Ingest, j *Job) (out catalog.Ingest, err error) {
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		m.r.log().Error("ingest panicked", "ingest", row.ID, "panic", p, "stack", string(debug.Stack()))
		out = row
		out.Status = catalog.StatusFailed
		out.Error = fmt.Sprintf("internal error: %v", p)
		out.Counts = j.Progress().Counts
		out.SafeToFormat = false
		out.FinishedAt = m.stamp()
		// Not cancellable: the failure must be recorded even during shutdown.
		if ferr := m.r.cfg.Catalog.FinishIngest(context.WithoutCancel(m.ctx), out); ferr != nil {
			// run replaces out.Error with err's text, so keep the panic in it (PF14).
			err = fmt.Errorf("internal error: %v; record the failed ingest %d: %w", p, row.ID, ferr)
		}
	}()
	return m.r.Run(m.ctx, row, j.progress)
}
```

- [ ] **Step 4: Run**: `go test ./internal/ingest/` → PASS; the race command → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/ingest/
git commit -m "Fail an import that panics and keep the ingest worker running

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 4: Exclusive lock on `/config` at startup

Suggested implementer: sonnet (opus reviewer: startup order and the Windows syscall).

**Files:**
- Create: `internal/lockfile/lockfile.go`, `lock_unix.go`, `lock_windows.go`, `lock_other.go`, `lockfile_test.go`; `cmd/sd2nas/main_test.go`
- Modify: `cmd/sd2nas/main.go`

**Interfaces:**
- Consumes: `syscall`, `os`.
- Produces:

```go
package lockfile
var ErrHeld = errors.New("lockfile: held by another process")         // every *HeldError matches it
var ErrUnsupported = errors.New("lockfile: locking is not supported here")
type HeldError struct{ Path, Holder string }                            // Holder: the holder's info line, "" if unreadable
func (e *HeldError) Error() string                                      // "<path> is locked by another process (<holder or unknown>)"
func (e *HeldError) Is(target error) bool                               // target == ErrHeld
type Lock struct{ /* f *os.File */ }
func Acquire(path, info string) (*Lock, error)                          // never waits
func (l *Lock) Release() error                                          // unlock, then close
```

- [ ] **Step 1: Write the failing tests**
  - `lockfile_test.go`:
    - `TestAcquireHeld`:
      - `Acquire(p, "pid 1 on test since x")` → ok, and the file's first line is the info.
      - A second `Acquire(p, "other")` in the same process → `errors.Is(err, ErrHeld)`, with `errors.As` giving a `*HeldError` whose `Holder == "pid 1 on test since x"`. On Linux, `flock` locks belong to the open file description, so two opens conflict in one process; on Windows, `LockFileEx` locks belong to the handle.
      - `Release()` → nil. `Acquire(p, "third")` → ok. The file still exists after every `Release`.
    - `TestHeldAcrossProcesses`:
      - The helper is the test binary itself: `exec.Command(os.Args[0], "-test.run=^TestHelperHoldLock$")`, with env `SD2NAS_LOCKFILE_HELPER=<path>`. In the child, `TestHelperHoldLock` acquires the lock, prints `locked\n`, then blocks reading stdin; it returns immediately when the env var is unset.
      - The parent waits for `locked`, then `Acquire` → `ErrHeld`.
      - The parent closes the child's stdin and waits for the child; then `Acquire` succeeds. Retry for up to 10 s, because Windows releases a dead process's locks asynchronously.
    - `TestAcquireMissingDir`: a path in a missing directory → error, and neither `ErrHeld` nor `ErrUnsupported`.
  - `cmd/sd2nas/main_test.go` `TestExitCode`: `exitCode(nil) == 0`; `exitCode(errors.New("x")) == 1`; `exitCode(&configInUseError{msg: "m", err: &lockfile.HeldError{}}) == 3`, and its `Error()` is `"m"`.
- [ ] **Step 2: Run and confirm they fail**: `go test ./internal/lockfile/ ./cmd/sd2nas/` → undefined names.
- [ ] **Step 3: Implement `internal/lockfile`.**
  - Package doc: `// Package lockfile takes an exclusive, non-blocking lock on a file, so a second SD2NAS on the same config folder stops at startup (§16 M6). The operating system drops the lock when the process ends, however it ends.`
  - **`Acquire`:**
    - `os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)`; on error return `fmt.Errorf("lockfile: %w", err)`.
    - `lockFile(f)`. On error, close `f`. `ErrHeld` → `errors.Join(&HeldError{Path: path, Holder: holderOf(path)}, closeErr)`. Anything else → `errors.Join(fmt.Errorf("lockfile: lock %s: %w", path, err), closeErr)`, keeping `ErrUnsupported` matchable.
    - On success: `f.Truncate(0)`, then `f.WriteAt([]byte(info+"\n"), 0)`. If either fails, return `errors.Join(fmt.Errorf("lockfile: write %s: %w", path, err), unlockFile(f), f.Close())`.
  - `holderOf` reads at most 256 bytes and returns the first line trimmed (`""` on any error).
  - **`Release`:** `errors.Join(unlockFile(l.f), l.f.Close())`.
  - **`lock_unix.go`** (`//go:build linux || darwin`):

```go
func lockFile(f *os.File) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EWOULDBLOCK):
		return ErrHeld
	case errors.Is(err, syscall.ENOLCK), errors.Is(err, syscall.EOPNOTSUPP), errors.Is(err, syscall.ENOSYS):
		return fmt.Errorf("%w: %v", ErrUnsupported, err)
	default:
		return err
	}
}

func unlockFile(f *os.File) error { return syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }
```

  - **`lock_windows.go`:**

```go
var (
	modkernel32      = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = modkernel32.NewProc("LockFileEx")
	procUnlockFileEx = modkernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately               = 0x1
	lockfileExclusiveLock                 = 0x2
	errorLockViolation      syscall.Errno = 33 // ERROR_LOCK_VIOLATION
)

// lockRange is the one byte locked: far past the end of the file, so the
// holder line at the start stays readable by a refused process (Decision 14).
func lockRange() *syscall.Overlapped { return &syscall.Overlapped{OffsetHigh: 1} }

func lockFile(f *os.File) error {
	r, _, err := procLockFileEx.Call(f.Fd(), lockfileExclusiveLock|lockfileFailImmediately, 0, 1, 0,
		uintptr(unsafe.Pointer(lockRange())))
	if r != 0 {
		return nil
	}
	if errors.Is(err, errorLockViolation) {
		return ErrHeld
	}
	return err
}

func unlockFile(f *os.File) error {
	r, _, err := procUnlockFileEx.Call(f.Fd(), 0, 1, 0, uintptr(unsafe.Pointer(lockRange())))
	if r == 0 {
		return err
	}
	return nil
}
```

  - **`lock_other.go`** (`//go:build !linux && !darwin && !windows`): `lockFile` returns `ErrUnsupported`, and `unlockFile` returns nil.
- [ ] **Step 4: Wire `main.go`.**
  - `main` exits with `os.Exit(exitCode(err))`.
  - Add the helpers:

```go
// exitConfigInUse is the exit code when another SD2NAS holds the config lock.
const exitConfigInUse = 3

// lockName is the config lock's file name inside the config folder.
const lockName = "sd2nas.lock"

// configInUseError is the startup error when another SD2NAS holds the lock;
// its text is the one sentence the log shows.
type configInUseError struct {
	msg string
	err error
}

func (e *configInUseError) Error() string { return e.msg }
func (e *configInUseError) Unwrap() error { return e.err }

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, lockfile.ErrHeld):
		return exitConfigInUse
	default:
		return 1
	}
}

// holderInfo is the line a refused SD2NAS shows about this one.
func holderInfo() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown host" // only decorates the lock file
	}
	return fmt.Sprintf("pid %d on %s since %s", os.Getpid(), host, time.Now().UTC().Format(time.RFC3339))
}
```

  - **In `run`**, right after the absolute-path loop and before the `starting sd2nas` log line and `api.New`:

```go
	// §16 M6: one SD2NAS per config folder, locked before anything opens the
	// catalog, fails "running" imports, deletes partials or closes "unfinished"
	// archive scans (Decision 14). Released after db.Close (defer order).
	lock, lockErr := lockfile.Acquire(filepath.Join(cfg.Data, lockName), holderInfo())
	var lockProblem error
	switch {
	case lockErr == nil:
		defer func() {
			if err := lock.Release(); err != nil {
				log.Error("releasing the config lock", "err", err)
			}
		}()
	case errors.Is(lockErr, lockfile.ErrHeld):
		holder := "unknown"
		var he *lockfile.HeldError
		if errors.As(lockErr, &he) && he.Holder != "" {
			holder = he.Holder
		}
		return &configInUseError{msg: fmt.Sprintf("%s is in use by another SD2NAS (%s); stop it first", cfg.Data, holder), err: lockErr}
	case errors.Is(lockErr, lockfile.ErrUnsupported):
	default:
		lockProblem = lockErr
	}
```

  - After `srv.SetLogger(log)`, if `errors.Is(lockErr, lockfile.ErrUnsupported)`: build `msg := fmt.Sprintf("could not lock %s (%v): a second SD2NAS using this folder would not be stopped", cfg.Data, lockErr)`, then `log.Warn(msg)` and `srv.AddWarning(msg)`.
  - Replace `db, err := catalog.Open(...)` and its `if err != nil` with:

```go
	var db *catalog.DB
	err = lockProblem
	if err == nil {
		db, err = catalog.Open(filepath.Join(cfg.Data, "sd2nas.db"))
	}
	if err != nil {
		if lockProblem != nil {
			log.Error("cannot lock the config folder; ingest is disabled", "err", err)
			srv.AddProblem("config lock: " + err.Error())
		} else {
			// (the two existing lines, unchanged)
		}
	} else {
```

  Leave the `else` body unchanged.
- [ ] **Step 5: Run**: `go test ./internal/lockfile/ ./cmd/sd2nas/ -v` on Windows, then the Linux command → PASS. `GOOS=darwin go vet ./internal/lockfile/` and `GOOS=freebsd go vet ./internal/lockfile/` compile (cross-vet only). `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 6: Commit**

```bash
git add internal/lockfile/ cmd/sd2nas/
git commit -m "Lock the config folder at startup and exit with code 3 when another SD2NAS holds it

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 5: Catalog schema 4: notifications

Suggested implementer: sonnet.

**Files:**
- Create: `internal/catalog/notifications.go`, `internal/catalog/notifications_test.go`
- Modify: `internal/catalog/schema.go`, `internal/catalog/catalog.go` (`SchemaVersion` only), `internal/catalog/catalog_test.go` (the table list), `internal/catalog/ingests_test.go` (a v3→v4 migration test; `TestMigrateV2ToV3`'s `v != 3` at line 461 becomes `v != SchemaVersion`), `internal/catalog/archive_test.go` (`TestBackupVacuumInto`'s `v != 3` at line 762 → `SchemaVersion`), `internal/indexer/backup_test.go` (the backup's `v != 3` at line 88 → `catalog.SchemaVersion`; the only edit allowed in `internal/indexer/`)

**Interfaces:**
- Consumes: `d.tx`, `formatTime`/`parseTime`, the migration runner.
- Produces:

```go
const SchemaVersion = 4
const (
	NoticeNormal  = "normal"
	NoticeWarning = "warning"
	NoticeAlert   = "alert"
)
// NoticeKeep is how long a notification is kept after it was created.
const NoticeKeep = 30 * 24 * time.Hour
// MaxTake is the most notifications one take returns.
const MaxTake = 100

// Notification is a notice for the Unraid host, queued until the card script takes it.
type Notification struct {
	ID        int64     `json:"id"`
	Key       string    `json:"key"`   // unique: the same event is never queued twice
	Level     string    `json:"level"` // NoticeNormal | NoticeWarning | NoticeAlert
	Message   string    `json:"message"`
	CreatedAt time.Time `json:"created_at"`
}

// AddNotification stores n unless one with n.Key exists (added = false). In
// the same transaction it deletes notifications created more than
// NoticeKeep before n.CreatedAt. It stores the message as one line
// (control characters become spaces, trimmed) of at most 1000 bytes, cut
// at a rune boundary and ended with "…"; the text take relies on it. An
// empty key, a message empty after that, or an unknown level is an error.
func (d *DB) AddNotification(ctx context.Context, n Notification) (added bool, err error)
func (d *DB) HasNotification(ctx context.Context, key string) (bool, error)
// PendingNotifications: not taken yet, oldest (lowest id) first, at most MaxTake. Never nil.
func (d *DB) PendingNotifications(ctx context.Context) ([]Notification, error)
// TakeNotifications marks the pending ones (oldest first, at most MaxTake)
// taken at now and returns them, in one transaction. Never nil.
func (d *DB) TakeNotifications(ctx context.Context, now time.Time) ([]Notification, error)
```

- [ ] **Step 1: Write the failing tests**
  - `catalog_test.go` `TestOpenCreatesSchema`: add `"notifications"` to the expected table list, in name order.
  - `ingests_test.go` `TestMigrateV3ToV4`:
    - Build a v3 database by hand, the same way `TestMigrateV2ToV3` builds v2: `schemaV1`, `schemaV2`, `schemaV3`, then `PRAGMA user_version = 3`.
    - Insert one setting.
    - `Open` → `Version` 4, the setting is intact, and `pragma_index_list('notifications')` contains `notifications_pending`.
  - `notifications_test.go`:
    - `TestAddNotificationOnce`: adding key `k` twice → `true`, then `false`; `PendingNotifications` has one row, with every field read back (time at ms precision).
    - `TestAddNotificationRejects`: an empty key, an empty (or all-control-character) message, or level `"loud"` → error, and nothing stored.
    - `TestAddNotificationSanitizes` (the 1000-byte cut is tested here, PF19):
      - the message `"a\nb\tc "` is stored as `a b c`;
      - a 1500-byte message of multi-byte runes is stored as at most 1000 bytes of valid UTF-8, ending with `…`.
    - `TestTakeNotifications`:
      - Add 3 notices; `TakeNotifications(now)` returns them in id order.
      - A second take returns `[]` (not nil).
      - `HasNotification` stays true for each.
      - Pending is empty: a non-nil `[]Notification{}` (assert `!= nil` and `len == 0`).
    - `TestTakeNotificationsCap`: add 105; the first take returns 100 (ids 1–100), the second 5.
    - `TestNotificationsPruned`:
      - Add one with `CreatedAt = now-31d`, then one with `CreatedAt = now`.
      - The old one is gone (`HasNotification` false), the new one remains.
      - A key that was pruned can be added again.
    - `TestTakeConcurrent`: two goroutines take at once → together they return each notice exactly once.
- [ ] **Step 2: Run and confirm they fail**: `go test ./internal/catalog/ -run 'Schema|Migrate|Notification|Take' -v`.
- [ ] **Step 3: Implement.** Append `schemaV4` to `migrations` exactly as below, and set `SchemaVersion = 4`.

```go
// schemaV4 (M6): notices for the Unraid host — a stale staging folder, a
// failed catalog backup, files missing from the archive. The card script
// takes them (POST /api/notifications/take) and hands them to Unraid's
// notify. key is unique, so a restart never queues the same event twice.
const schemaV4 = `
CREATE TABLE notifications (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  key        TEXT NOT NULL UNIQUE,
  level      TEXT NOT NULL CHECK (level IN ('normal','warning','alert')),
  message    TEXT NOT NULL,
  created_at TEXT NOT NULL,
  taken_at   TEXT              -- NULL = waiting for the card script
);
CREATE INDEX notifications_pending ON notifications(taken_at, id);
`
```

  - **`AddNotification`:** inside `d.tx`, first `DELETE FROM notifications WHERE created_at < ?` with `formatTime(n.CreatedAt.Add(-NoticeKeep))`, then `INSERT INTO notifications(key, level, message, created_at) VALUES (?,?,?,?) ON CONFLICT(key) DO NOTHING`. `added` = rows affected == 1.
  - **`TakeNotifications`:** inside `d.tx`:
    - `SELECT id, key, level, message, created_at FROM notifications WHERE taken_at IS NULL ORDER BY id LIMIT ?` with `MaxTake`, read fully and close the rows;
    - if any, `UPDATE notifications SET taken_at = ? WHERE taken_at IS NULL AND id <= ?` with the highest id read.

    The transaction is `BEGIN IMMEDIATE` (`_txlock=immediate`), so no other take can interleave.
- [ ] **Step 4: Run**: `go test ./internal/catalog/` → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/catalog/ internal/indexer/backup_test.go
git commit -m "Add catalog schema 4 with a notifications queue for the Unraid host

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 6: `internal/notices`: queue the stale-staging, backup and missing-file notices

Suggested implementer: sonnet (opus reviewer: restarts, day boundaries, no callbacks).

**Files:**
- Create: `internal/notices/notices.go`, `internal/notices/notices_test.go`

**Interfaces:**
- Consumes: Task 5's catalog API; `indexer.Status` (`LastBackup *BackupResult{At, Error}`, `Last *ScanResult{FinishedAt, Missing}`); `staging.Listing` (`StaleAfterDays`, `Folders[]{Name, AgeDays, Stale}`).
- Produces:

```go
package notices
const (
	CheckEvery     = 30 * time.Second
	StagingCheckAt = 9 * time.Hour   // container local time (Decision 4)
	MaxAge         = 24 * time.Hour  // older archive events are not notified (Decision 7)
)
var ErrClosed = errors.New("notices: shutting down")
type Config struct {
	DB      *catalog.DB
	Archive func() indexer.Status                           // nil: no archive notices
	Staging func(context.Context) (staging.Listing, error)  // nil: no stale-staging notice
	StagingReady func() bool                                // the startup staging reconcile has finished; nil: always ready
	Now     func() time.Time                                // nil: time.Now
	Log     *slog.Logger                                    // nil: slog.Default()
	Every   time.Duration                                   // 0: CheckEvery
}
type Service struct{ /* … */ }
func New(cfg Config) (*Service, error)                                   // error only for a nil DB
func (s *Service) Start()                                                // checks now, then every Every; no-op after Close or twice
func (s *Service) Close()                                                // stops and waits; safe twice
func (s *Service) Pending(ctx context.Context) ([]catalog.Notification, error) // ErrClosed after Close
func (s *Service) Take(ctx context.Context) ([]catalog.Notification, error)    // db.TakeNotifications(ctx, now); ErrClosed after Close
```

  Imports: the standard library, `internal/catalog`, `internal/indexer` and `internal/staging` only.
- [ ] **Step 1: Write the failing tests**
  - Use a real catalog in `t.TempDir()`, fake `Archive`/`Staging` funcs (the staging fake counts its calls), and a settable `now` that returns times in `time.LoadLocation("Pacific/Chatham")`. Import `time/tzdata` in the test file. The service uses `now`'s own location for "09:00" and the date, and never mutates `time.Local`.
  - Call the unexported `s.check(ctx)` directly.
  - `TestCheckBackupFailureOnce`:
    - `LastBackup{At: now-1h, Error: "disk full."}`; check twice → one pending notice: key `backup:<At UTC RFC3339Nano>`, level `alert`, message exactly `Nightly catalog backup failed: disk full. Older backups were kept.`
    - A **new** `Service` on the same DB (a restart) checks → still one.
  - `TestCheckOldOrSuccessfulBackupIgnored`: `At: now-25h` with an error → none; `At: now-1h` with `Error: ""` → none.
  - `TestCheckMissing`: `Last{FinishedAt: now-5m, Missing: 3}` → `The archive scan marked 3 files missing from Archive/. See the Archive index page.`; `Missing: 1` (another `FinishedAt`) → the singular text; `Missing: 0` → none.
  - `TestCheckStagingDaily`:
    - At 08:59 local with 2 stale folders → no notice, and Staging is not called.
    - At 09:00 → one notice with key `staging:<date>` and level `warning`. The text for folders `B` (40 days) and `A` (35 days), with `StaleAfterDays: 30`, is `2 folders have been in Staging for 30 days or more: B and A.`
    - At 15:00 the same day → Staging is not called again.
    - The next day at 09:00 → a second notice.
    - A new `Service` the second day at 10:00 → Staging is not called (the key exists).
  - `TestCheckStagingNoneStale`:
    - No stale folders at 09:00 → no notice, and Staging is called once that day.
    - A new Service the same day calls it once more (accepted: nothing is stored when nothing is stale).
    - `StaleAfterDays: 0` → never a notice.
  - `TestStaleTexts` (a table over the unexported text function):
    - 1 folder, 1 day → `1 folder has been in Staging for 1 day or more: X.`
    - 3 folders → `a, b and c`
    - 7 folders → `…: f1, f2, f3, f4, f5 and 2 more.`
    - ordering by `AgeDays` descending, then name
  - `TestCheckArchiveNoRepeatWrites`:
    - A backup failure stays in the status. After the first check queues it, ten more checks make no `add` call (count them through an unexported counter in `add`).
    - A new `Service` makes one `add` (it finds the key stored), then none.
  - `TestCheckStagingWaitsForRescan`:
    - At 09:00, `StagingReady` returns false → Staging is not called, and no day is marked done.
    - Once it returns true (09:01) → the check runs.
  - `TestCheckStagingErrorRetriesHourly`:
    - Staging returns an error at 09:00 → no notice, and it is logged.
    - At 09:30 → not called. At 10:00 → called again, succeeds, and the notice is queued.
  - `TestTakeAndClose`: two queued notices; `Take` returns both and `Pending` is then empty. After `Close`, `Take` and `Pending` return `ErrClosed`.
  - `TestStartChecksAndStops`: `Every: 10ms`, with a backup failure in Archive → a pending notice appears within 10 s. `Close` returns, and a second `Close` is a no-op.
- [ ] **Step 2: Run and confirm they fail**: `go test ./internal/notices/` → undefined.
- [ ] **Step 3: Implement.** The core, which pins the behaviour:

```go
func (s *Service) check(ctx context.Context) {
	s.checkArchive(ctx)
	s.checkStaging(ctx)
}

func (s *Service) checkArchive(ctx context.Context) {
	if s.cfg.Archive == nil {
		return
	}
	st, now := s.cfg.Archive(), s.now()
	if b := st.LastBackup; b != nil && b.Error != "" && fresh(now, b.At) {
		s.addOnce(ctx, "backup:"+b.At.UTC().Format(time.RFC3339Nano), catalog.NoticeAlert,
			"Nightly catalog backup failed: "+strings.TrimRight(b.Error, ". ")+". Older backups were kept.")
	}
	if l := st.Last; l != nil && l.Missing > 0 && fresh(now, l.FinishedAt) {
		s.addOnce(ctx, "missing:"+l.FinishedAt.UTC().Format(time.RFC3339Nano), catalog.NoticeAlert, missingText(l.Missing))
	}
}

// addOnce adds a notice unless this service already stored or found its
// key, so an event that stays in the status doesn't cost a write
// transaction every 30 s (PF13). add logs a failure, and the next check
// retries it.
func (s *Service) addOnce(ctx context.Context, key, level, msg string) {
	if s.known[key] {
		return
	}
	if err := s.add(ctx, key, level, msg); err == nil {
		s.known[key] = true
	}
}

// fresh: t is set and at most MaxAge before now.
func fresh(now, t time.Time) bool { return !t.IsZero() && now.Sub(t) <= MaxAge }

func (s *Service) checkStaging(ctx context.Context) {
	if s.cfg.Staging == nil {
		return
	}
	now := s.now() // time.Now is in time.Local, the container's TZ
	day := now.Format("2006-01-02")
	y, m, d := now.Date()
	due := time.Date(y, m, d, int(StagingCheckAt/time.Hour), int(StagingCheckAt%time.Hour/time.Minute), 0, 0, now.Location())
	if day == s.stagingDay || now.Before(due) || now.Before(s.stagingRetry) {
		return
	}
	if s.cfg.StagingReady != nil && !s.cfg.StagingReady() {
		return // the startup reconcile may still adopt folders (PF21)
	}
	key := "staging:" + day
	has, err := s.cfg.DB.HasNotification(ctx, key)
	if err != nil {
		s.retryStaging(now, err)
		return
	}
	if has {
		s.stagingDay = day
		return
	}
	l, err := s.cfg.Staging(ctx)
	if err != nil {
		s.retryStaging(now, err)
		return
	}
	var stale []staging.Folder
	for _, f := range l.Folders {
		if f.Stale {
			stale = append(stale, f)
		}
	}
	if l.StaleAfterDays > 0 && len(stale) > 0 {
		if err := s.add(ctx, key, catalog.NoticeWarning, staleText(l.StaleAfterDays, stale)); err != nil {
			s.retryStaging(now, err)
			return
		}
	}
	s.stagingDay = day
}

// retryStaging logs err and tries again in an hour.
func (s *Service) retryStaging(now time.Time, err error) {
	s.log.Error("stale-staging check failed; retrying in an hour", "err", err)
	s.stagingRetry = now.Add(time.Hour)
}
```

  - `stagingDay`, `stagingRetry` and `known` are touched only by the loop goroutine (and by tests calling `check`), so they need no lock. `known` is a `map[string]bool` holding a few keys a day; entries older than `MaxAge` may be dropped.
  - `add` calls `AddNotification` with `CreatedAt: s.now()`; `AddNotification` sanitises the message (Task 5). `add` logs `notice queued` (keys `key`, `level`) when added, logs and returns any error, and increments an unexported counter the tests read.
  - `staleText` sorts a copy of the folders by `AgeDays` descending, then by name, and builds the Global Constraints text.
  - `Start` adds to a `sync.WaitGroup` under the mutex, the same way the indexer does, and runs `check(ctx)` at once and then on a `time.Ticker`. `Close` sets `closed`, cancels and waits.
- [ ] **Step 4: Run**: `go test ./internal/notices/` and the race command → PASS; `go list -f '{{join .Imports "\n"}}' ./internal/notices/ | grep -E 'internal/(api|ingest)'` prints nothing; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/notices/
git commit -m "Queue daily stale-staging, failed-backup and missing-file notices

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 7: The Unassigned Devices card script and its test

Suggested implementer: sonnet (opus reviewer: quoting, escaping, failure paths).

**Files:**
- Create: `deploy/unassigned-devices.sh`, `deploy/doc.go`, `deploy/script_test.go`

**Interfaces:**
- Consumes: the reply formats of `POST /api/ingest?wait=1&format=text` (M2) and `POST /api/notifications/take?format=text` (Task 8, specified in Global Constraints; the test fakes it).
- Produces: the script. `deploy/doc.go` holds `// Package deploy holds the container image files and the Unraid card script and template; its tests run the script and check the template.` and `package deploy`.

- [ ] **Step 1: Write the failing tests** (`deploy/script_test.go`, `package deploy`).
  - **The harness.**
    - Skip on Windows. `exec.LookPath("bash")` and `("curl")`: when either is missing, skip, unless `SD2NAS_REQUIRE_TOOLS=1`, which makes it fail.
    - `runScript(t, env map[string]string) []call` writes stubs into a temp dir. Each stub appends its name and arguments, separated by `\x1f` and ended by `\x1e`, to `$SD2NAS_TEST_CALLS`:
      - `blkid` prints `1234-ABCD` for exactly `-s UUID -o value /dev/sdz1`, and otherwise exits 2;
      - `rc.unassigned` exits `${STUB_UMOUNT_EXIT:-0}`;
      - `logger` and `notify` exit 0.
    - The script runs with `exec.CommandContext` (30 s) and an **explicit** env:
      - `PATH=<stubs>:<os PATH>`, `SD2NAS_CONF=<tmp>/none.cfg`, `SD2NAS_NOTIFY=<stubs>/notify`, `SD2NAS_TEST_CALLS`;
      - defaults `ACTION=ADD`, `DEVICE=/dev/sdz1`, `LABEL=SONY_A7IV`, `SERIAL=reader`, `MOUNTPOINT=/mnt/disks/SONY_A7IV`, `SD2NAS_URL=<server>`;
      - overridden by `env`. An empty value in `env` removes that key from the environment.
    - The script must exit 0. It returns the calls; a helper filters them by name.
    - The fake SD2NAS is an `httptest.Server` that records each request (method, path, raw query, headers, body) and answers per test.
  - `TestScriptSyntax`: `bash -n unassigned-devices.sh` succeeds (the test's working directory is `deploy/`).
  - `TestScriptImportsAndUnmounts`:
    - Ingest replies 200 `normal\nSONY_A7IV: 2 new, 0 already known. Card safe to format.\n`. Take replies 200 `warning\n2 folders have been in Staging for 30 days or more: B and A.\nalert\nNightly catalog backup failed: x. Older backups were kept.\n`.
    - **Requests, in order:**
      1. `POST /api/ingest` with raw query `wait=1&format=text`, `X-SD2NAS: 1`, `Content-Type: application/json`, no `Authorization`. The body decodes (with `DisallowUnknownFields`) into exactly `{path: "/cards/SONY_A7IV", label: "SONY_A7IV", uuid: "1234-ABCD"}`.
      2. `POST /api/notifications/take` with raw query `format=text` and `X-SD2NAS: 1`.
    - **Calls, other than `logger`, in order:**
      1. `blkid -s UUID -o value /dev/sdz1`;
      2. `rc.unassigned umount /dev/sdz1`;
      3. `notify -e SD2NAS -s SD2NAS -d "SONY_A7IV: 2 new, 0 already known. Card safe to format." -i normal`;
      4. `notify` … `-i warning`;
      5. `notify` … `-i alert`, each with the exact message.
  - `TestScriptEscapesLabel`:
    - `LABEL` = `a"b\c&d<TAB>e<LF>f\` and `MOUNTPOINT=/mnt/disks/we"ird&\`.
    - The body is valid JSON (decoded with `DisallowUnknownFields`), with label `a"b\c&d e f\` and path `/cards/we"ird&\`.
    - This covers `"`, `\`, a trailing `\`, `&` (bash 5.2's `patsub_replacement`) and control characters.
  - `TestScriptRefused`: ingest replies 507 `alert\nSONY_A7IV: not imported, staging would drop below the 50.0 GB free-space reserve (…). Card left mounted.\n` → no `rc.unassigned`, and `notify … -d <that message> -i alert`.
  - `TestScriptNotSD2NASReply`:
    - 404 `{"error":"not found"}` → `SD2NAS could not import SONY_A7IV (HTTP 404). Card left mounted. Check SD2NAS.` (alert), no unmount.
    - 200 `hello` → the same text with `HTTP 200`, no unmount.
  - `TestScriptContainerDown`:
    - Close the server first → `SD2NAS did not answer, so SONY_A7IV was not imported. Card left mounted. Is the container running?` (alert).
    - Exactly one `notify`, plus a `logger` call containing `could not fetch notices from SD2NAS`.
  - `TestScriptTimeout`: `SD2NAS_MAX_TIME=1`, and the ingest handler blocks until its request context ends or 5 s pass → `SD2NAS did not finish importing SONY_A7IV in time. Card left mounted. Check SD2NAS.`
  - `TestScriptUnmountFails`: `STUB_UMOUNT_EXIT=1` and a 200 `normal` reply → `notify … -d "<server message> Unmounting failed: unmount it in Unassigned Devices before removing the card." -i warning`.
  - `TestScriptNoMountPoint`: `MOUNTPOINT=""` → `Unassigned Devices gave no mount point for /dev/sdz1, so the card was not imported.` (alert), and no ingest request.
  - `TestScriptOtherActions`:
    - `UNMOUNT`, `REMOVE`, `ERROR_UNMOUNT` and `""` → no requests and no calls.
    - `ERROR_MOUNT` → no requests, and the only call other than `logger` is `notify … -d "Unassigned Devices could not mount the card in /dev/sdz1, so it was not imported." -i alert`.
  - `TestScriptNotifyAction`:
    - `ACTION=NOTIFY` → only the take request. An empty 200 body → no `notify`.
    - A body `loud\nx\n` → no `notify`, and a `logger` call containing `ignored a notice`.
    - A take answering 503 → only a `logger` call containing `HTTP 503`.
  - `TestScriptConfigFileAndToken`: `SD2NAS_CONF` points to a file containing `SD2NAS_URL=<server>` and `SD2NAS_TOKEN=abc`, and `env` sets `SD2NAS_URL: ""` (removing it) → both requests carry `Authorization: Bearer abc`.
- [ ] **Step 2: Run and confirm they fail** (the script doesn't exist): Linux command, or `go test ./deploy/` in Docker.
- [ ] **Step 3: Write `deploy/unassigned-devices.sh`** exactly. It is LF and executable (`git add --chmod=+x`).

```bash
#!/bin/bash
# SD2NAS card script for Unassigned Devices (docs/PLAN.md §11, README.md).
#
# Attach it to every slot of the card reader with Automount on, Read Only on,
# Share off and "Run Device Script File in the background" on. Unassigned
# Devices sets ACTION, DEVICE, LABEL, SERIAL and MOUNTPOINT. ACTION=NOTIFY
# (User Scripts, hourly) only delivers the notices SD2NAS has queued.
#
# Settings go in sd2nas.cfg (SD2NAS_CONF), so updating this script keeps them:
#   SD2NAS_URL=http://localhost:4040     # the container's host port
#
# Whether the card is safe to format comes from SD2NAS's reply; this script
# never decides it.

SD2NAS_CONF="${SD2NAS_CONF:-/boot/config/plugins/unassigned.devices/sd2nas.cfg}"
if [ -r "$SD2NAS_CONF" ]; then
	# shellcheck source=/dev/null
	. "$SD2NAS_CONF"
fi
SD2NAS_URL="${SD2NAS_URL:-http://localhost:8080}"
SD2NAS_TOKEN="${SD2NAS_TOKEN:-}"
SD2NAS_NOTIFY="${SD2NAS_NOTIFY:-/usr/local/emhttp/webGui/scripts/notify}"
SD2NAS_MAX_TIME="${SD2NAS_MAX_TIME:-7260}" # SD2NAS answers by itself after 2 hours
PATH="$PATH:/usr/local/sbin:/usr/sbin:/sbin:/usr/bin:/bin"

# log TEXT: a syslog line (stderr if logger fails).
log() {
	logger -t sd2nas -- "$1" 2>/dev/null || printf 'sd2nas: %s\n' "$1" >&2
}

# notify LEVEL TEXT: an Unraid notification, plus a syslog line.
notify() {
	log "$1: $2"
	if ! "$SD2NAS_NOTIFY" -e "SD2NAS" -s "SD2NAS" -d "$2" -i "$1" </dev/null; then
		log "the notification above could not be sent"
	fi
}

# json_string TEXT: TEXT as a JSON string; control characters become spaces.
json_string() {
	local bs='\' q='"' s=$1
	s=${s//"$bs"/"$bs$bs"}
	s=${s//"$q"/"$bs$q"}
	s=${s//[[:cntrl:]]/ }
	printf '"%s"' "$s"
}

# post PATH MAX_TIME DATA: POST to SD2NAS. Sets CODE (the HTTP status, 000
# without a reply) and BODY; returns curl's exit status.
post() {
	local tmp rc auth=()
	CODE=000
	BODY=""
	tmp=$(mktemp) || return 1
	if [ -n "$SD2NAS_TOKEN" ]; then
		auth=(-H "Authorization: Bearer $SD2NAS_TOKEN")
	fi
	CODE=$(curl -sS --connect-timeout 10 --max-time "$2" -o "$tmp" -w '%{http_code}' \
		-X POST -H "X-SD2NAS: 1" -H "Content-Type: application/json" "${auth[@]}" \
		--data-binary "$3" "$SD2NAS_URL$1")
	rc=$?
	BODY=$(<"$tmp")
	rm -f "$tmp"
	return "$rc"
}

# server_reply: true when BODY is SD2NAS's two lines, a level and a message;
# sets LEVEL and MSG.
server_reply() {
	LEVEL=$(printf '%s\n' "$BODY" | sed -n 1p)
	MSG=$(printf '%s\n' "$BODY" | sed -n 2p)
	case "$LEVEL" in
		normal | warning | alert) [ -n "$MSG" ] ;;
		*) return 1 ;;
	esac
}

# ingest: import the card at MOUNTPOINT; unmount it only when SD2NAS answers
# 200, and pass SD2NAS's message on.
ingest() {
	local mp="${MOUNTPOINT%/}" card name uuid rc
	card="${mp##*/}"
	name="${LABEL:-$card}"
	name="${name:-$DEVICE}"
	name="${name//[[:cntrl:]]/ }"
	if [ -z "$card" ]; then
		notify alert "Unassigned Devices gave no mount point for $DEVICE, so the card was not imported."
		return
	fi
	if ! uuid=$(blkid -s UUID -o value "$DEVICE"); then
		log "could not read the filesystem UUID of $DEVICE"
		uuid=""
	fi
	post "/api/ingest?wait=1&format=text" "$SD2NAS_MAX_TIME" \
		"{\"path\":$(json_string "/cards/$card"),\"label\":$(json_string "$LABEL"),\"uuid\":$(json_string "$uuid")}"
	rc=$?
	if [ "$rc" -eq 28 ]; then
		notify alert "SD2NAS did not finish importing $name in time. Card left mounted. Check SD2NAS."
	elif [ "$rc" -ne 0 ]; then
		notify alert "SD2NAS did not answer, so $name was not imported. Card left mounted. Is the container running?"
	elif ! server_reply; then
		notify alert "SD2NAS could not import $name (HTTP $CODE). Card left mounted. Check SD2NAS."
	elif [ "$CODE" = 200 ]; then
		if ! rc.unassigned umount "$DEVICE"; then
			[ "$LEVEL" = normal ] && LEVEL=warning
			MSG="$MSG Unmounting failed: unmount it in Unassigned Devices before removing the card."
		fi
		notify "$LEVEL" "$MSG"
	else
		notify "$LEVEL" "$MSG" # SD2NAS's own explanation; the card stays mounted
	fi
}

# deliver: pass SD2NAS's queued notices (stale staging folders, a failed
# catalog backup, files missing from the archive) to Unraid. SD2NAS hands
# each notice out once.
deliver() {
	local rc level msg
	post "/api/notifications/take?format=text" 30 ""
	rc=$?
	if [ "$rc" -ne 0 ]; then
		log "could not fetch notices from SD2NAS (curl exit $rc)"
		return
	fi
	if [ "$CODE" != 200 ]; then
		log "could not fetch notices from SD2NAS (HTTP $CODE)"
		return
	fi
	while IFS= read -r level && IFS= read -r msg; do
		case "$level" in
			normal | warning | alert) notify "$level" "$msg" ;;
			*) log "ignored a notice SD2NAS sent in an unknown form" ;;
		esac
	done <<<"$BODY"
}

case "$ACTION" in
	ADD)
		ingest
		deliver
		;;
	ERROR_MOUNT)
		notify alert "Unassigned Devices could not mount the card in $DEVICE, so it was not imported."
		;;
	NOTIFY)
		deliver
		;;
	*) # UNMOUNT, REMOVE, ERROR_UNMOUNT: nothing to do
		;;
esac
exit 0
```

- [ ] **Step 4: Run**:
  - the Linux command → PASS, with the script tests running rather than skipping (check with `go test ./deploy/ -v` inside the same container);
  - Windows `go test ./deploy/` → SKIP;
  - `git ls-files --stage deploy/unassigned-devices.sh` shows mode `100755`;
  - `git show HEAD:deploy/unassigned-devices.sh | grep -c $'\r'` → 0 after commit;
  - `gofmt -l .`, `go vet ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add --chmod=+x deploy/unassigned-devices.sh
git add deploy/doc.go deploy/script_test.go
git commit -m "Add the Unassigned Devices card script: import, unmount, notify, deliver notices

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 8: API: notifications endpoints and `Server.SubmitIngest`

Suggested implementer: sonnet.

**Files:**
- Create: `internal/api/notifications.go`, `internal/api/notifications_test.go`
- Modify: `internal/api/ingestfeed.go` (add `SubmitIngest`), `internal/api/ingest.go` (`post` uses `SubmitIngest`; the `DefaultMaxWait` comment), `internal/api/ingest_test.go` or a new `internal/api/submit_test.go`

**Interfaces:**
- Consumes: `notices.Service` (`Pending`, `Take`, `Close`, `ErrClosed`), `writeJSON`, `writeError`, `writeFail`, `wantsText`, `s.feed`.
- Produces:

```go
// MountNotifications registers GET /api/notifications and
// POST /api/notifications/take, and makes Close close n. Call it before serving.
func (s *Server) MountNotifications(n *notices.Service)

// SubmitIngest queues an import as POST /api/ingest does, and registers it
// with the ingest feed, so a short import still sends ingest.finished. It
// fails when MountIngest hasn't run.
func (s *Server) SubmitIngest(ctx context.Context, req ingest.Request) (*ingest.Job, error)
```

- [ ] **Step 1: Write the failing tests**
  - `TestNotificationsAPI`:
    - Use a catalog with 2 queued notices (via `AddNotification`) and `notices.New` (not started), mounted on `New("test", nil)`.
    - `GET /api/notifications` → 200 `{"pending": [2 items with id, key, level, message, created_at]}`.
    - `POST /api/notifications/take?format=text` with `X-SD2NAS: 1` → 200, `Content-Type: text/plain; charset=utf-8`, `Cache-Control: no-store`, body exactly `"<level1>\n<msg1>\n<level2>\n<msg2>\n"`.
    - A second take → 200 with an empty body.
    - JSON take with nothing pending → `{"notifications": []}`.
    - Pending → `{"pending": []}`.
  - `TestNotificationsTakeGuard`: a POST without `X-SD2NAS` → the guard's 403. Nothing is taken: pending still lists the notice.
  - `TestNotificationsAfterClose`: `s.Close()` → GET → 503 `{"error": "SD2NAS is shutting down"}`; take with `?format=text` → 503, body `alert\nSD2NAS is shutting down\n`.
  - `TestSubmitIngestWatched`:
    - Build a minimal ingest server: `s := New("test", nil); s.feedInterval = time.Hour; s.MountIngest(m, db, time.Minute)` over a started manager and a copy of the SONY card (as `newFullServer` builds them). The feed's loop then never ticks during the test, so only manual ticks run.
    - Register a finished hook (`s.addFinishedHook`) that records rows.
    - `card` comes from `mediatest.CopyCard(filepath.Join("..", "..", "testdata"), "SONY_A7IV", cards)`, with the manager built over `cards` as `newFullServer` builds it.
    - `j, err := s.SubmitIngest(ctx, ingest.Request{Path: card})`, then wait for `<-j.Done()`, then call `s.feed.tick()` once → the hook saw `j.ID()`.
    - Without the `watch` in `SubmitIngest` the job, never seen running by a tick, would get no `ingest.finished`; this test pins that.
  - `TestSubmitIngestWithoutMount`: `New("test", nil).SubmitIngest(...)` → an error, and no panic.
- [ ] **Step 2: Run and confirm they fail**: `go test ./internal/api/ -run 'Notifications|SubmitIngest' -v`.
- [ ] **Step 3: Implement.**

```go
func (s *Server) SubmitIngest(ctx context.Context, req ingest.Request) (*ingest.Job, error) {
	if s.feed == nil {
		return nil, errors.New("api: ingest is not mounted")
	}
	j, err := s.feed.m.Submit(ctx, req)
	if err != nil {
		return nil, err
	}
	s.feed.watch(j)
	return j, nil
}
```

  - **`ingest.go` (PF18):**
    - `ingestHandlers` gets a field `submit func(context.Context, ingest.Request) (*ingest.Job, error)`, set to `s.SubmitIngest` in `MountIngest`.
    - `post` calls `h.submit(r.Context(), …)` instead of `h.m.Submit` followed by `h.feed.watch`, so submit-and-watch lives in one place. Its error handling is unchanged.
    - The existing ingest API tests must still pass unchanged.
  - **`ingest.go` (PF11):** the `DefaultMaxWait` comment becomes `// DefaultMaxWait is the ?wait=1 ceiling. The card script's curl --max-time 7260 waits 60 s longer, so this reply reaches it first.`

  Handlers in `notifications.go`:
  - **`pending`:** `n.Pending(r.Context())`, then `writeJSON(200, map[string]any{"pending": list})`.
  - **`take`:** `n.Take(r.Context())`.
    - JSON → `{"notifications": list}`.
    - Text → set the headers, `WriteHeader(200)`, then `fmt.Fprintf(w, "%s\n%s\n", n.Level, n.Message)` per notice.
  - **Errors:** `notices.ErrClosed` → `writeFail(w, r, 503, "SD2NAS is shutting down")`. Anything else → log (`take notices` / `list notices`, key `err`) and `writeFail(w, r, 500, err.Error())`. `writeFail` gives JSON unless `?format=text`.
  - `s.closers = append([]func(){n.Close}, s.closers...)` (PF16). Notices close **first**, before the staging service and the indexer that `MountStaging`/`MountArchive` registered earlier, so a late tick never reads a closed service.
  - Test `TestNotificationsCloseFirst`: append a test closer that records the order before calling `MountNotifications`; `s.Close()` runs `n.Close` first (check that `Take` already returns `ErrClosed` inside the test closer).
- [ ] **Step 4: Run**: `go test ./internal/api/` and the race command → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/api/
git commit -m "Serve queued notices to the card script and let the poller submit watched imports

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 9: The fallback card poller and its switch

Suggested implementer: sonnet.

**Files:**
- Create: `internal/cardwatch/cardwatch.go`, `mount_unix.go`, `mount_other.go`, `cardwatch_test.go`
- Modify: `internal/config/config.go`, `internal/config/config_test.go`

**Interfaces:**
- Consumes: `ingest.Request`, `*ingest.Job` (`ID()`).
- Produces:

```go
// config
type Config struct { …; PollCards bool } // --poll-cards / SD2NAS_POLL_CARDS (Decision 9)

package cardwatch
const (
	DefaultEvery  = 5 * time.Second
	DefaultSettle = 10 * time.Second
)
type Config struct {
	Root   string                                                     // the cards folder
	Submit func(context.Context, ingest.Request) (*ingest.Job, error) // api.Server.SubmitIngest
	Every  time.Duration                                              // 0: DefaultEvery
	Settle time.Duration                                              // 0: DefaultSettle
	Log    *slog.Logger                                               // nil: slog.Default()
}
type Watcher struct{ /* … */ }
func New(cfg Config) *Watcher
func (w *Watcher) Start() // no-op after Close or twice
func (w *Watcher) Close() // stops and waits; safe twice
```

- [ ] **Step 1: Write the failing tests**
  - **`config_test.go` `TestPollCards`:**
    - unset → false; `true`, `1` → true; `false`, `0`, `""` → false;
    - `maybe` → error exactly `SD2NAS_POLL_CARDS "maybe" is not true or false`;
    - `--poll-cards` with env `false` → true; `--poll-cards=false` with env `true` → false.
  - **`cardwatch_test.go`** (package-internal):
    - **Harness:**
      - Call `w.tick(now)` directly with a fake clock.
      - Set the seam field `w.isMount` to a func that reads a test map.
      - `Submit` records each request and returns a real `*ingest.Job`: it calls `Submit` on a real `ingest.Manager` that is never started, whose cards root is the test's `Root`, over a temp catalog. `Submit` never returns a nil job without an error, and the watcher may rely on that.
    - `TestPollerStartupMountsNotImported`: `Root` has `CARD1` (a mount point with `DCIM/`) at the first tick. Ticks at +0, +20 s and +40 s → no submit, and the log has `not importing what was already mounted at startup`.
    - `TestPollerImportsNewCardAfterSettle`:
      - At the first tick `Root` is empty.
      - At t1 `CARD` appears (a mount point with `dcim/`, lower case) → no submit.
      - At t1+5 s → no submit. At t1+10 s → one submit, with `Path = filepath.Join(Root, "CARD")` and `Label = "CARD"`.
      - At t1+20 s and t1+60 s → still one.
    - `TestPollerReinsertedCardImportedAgain`: after its submit, `CARD` is removed (tick), then re-created (tick, then +10 s) → a second submit.
    - `TestPollerIgnoresNonCards` (each is never submitted, at any tick):
      - a directory that isn't a mount point;
      - a mount point without `DCIM`/`PRIVATE`;
      - a file named `CARD`;
      - `.hidden` (a mount point with `DCIM`).
    - `TestPollerPrivateCounts`: a mount point with only `PRIVATE/M4ROOT` → submitted.
    - `TestPollerSubmitErrorNotRetried`: `Submit` returns an error → logged with `could not start an import`, and not retried on later ticks while the card stays.
    - `TestPollerMountAppearsLate`: `CARD` exists but is not yet a mount point for 2 ticks, then becomes one → the settle counts from when it first qualified.
    - `TestPollerRootUnreadable`: `Root` missing → one error log per distinct error (two ticks with the same error → one line), and no panic.
    - `TestPollerStartClose`: real `Start` with `Every: 10ms` and a real `isMount` (no seam). `Close` returns within 10 s, and a second `Close` is fine.
- [ ] **Step 2: Run and confirm they fail**: `go test ./internal/config/ ./internal/cardwatch/`.
- [ ] **Step 3: Implement.**
  - **`config.go`:**
    - Read `SD2NAS_POLL_CARDS` with `strconv.ParseBool`, `""` = false. A bad value is an error with the exact text above.
    - Register the flag with `fs.BoolVar(&c.PollCards, "poll-cards", c.PollCards, "import cards that appear under the cards folder without the host script (env SD2NAS_POLL_CARDS)")`.
    - The env must be parsed **before** `fs.Parse`, so the flag overrides it.
  - **`mount_unix.go`** (`//go:build linux || darwin`): `isMount(dir, root string) (bool, error)` compares `syscall.Stat_t.Dev` of the two.
  - **`mount_other.go`** (`//go:build !linux && !darwin`): returns `true, nil`. Those hosts can't tell, so they rely on the `DCIM`/`PRIVATE` rule.
  - **The tick**, which pins the behaviour:

```go
// tick reads the cards folder once (Decision 10). Only the loop goroutine
// (or a test) calls it.
func (w *Watcher) tick(now time.Time) {
	entries, err := os.ReadDir(w.cfg.Root)
	if err != nil {
		if msg := err.Error(); msg != w.lastErr {
			w.log.Error("card poller: cannot read the cards folder", "path", w.cfg.Root, "err", err)
			w.lastErr = msg
		}
		return
	}
	w.lastErr = ""
	present := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			present[e.Name()] = true
		}
	}
	for name := range w.handled {
		if !present[name] {
			delete(w.handled, name) // gone: next time it is a new card
		}
	}
	for name := range w.since {
		if !present[name] {
			delete(w.since, name)
		}
	}
	if !w.baselined {
		w.baselined = true
		names := slices.Sorted(maps.Keys(present))
		for _, name := range names {
			w.handled[name] = true
		}
		if len(names) > 0 {
			w.log.Info("card poller: not importing what was already mounted at startup", "names", names)
		}
		return
	}
	for _, name := range slices.Sorted(maps.Keys(present)) {
		if w.handled[name] {
			continue
		}
		dir := filepath.Join(w.cfg.Root, name)
		ok, err := w.looksLikeCard(dir)
		if err != nil {
			w.log.Warn("card poller: cannot look into a mount", "path", dir, "err", err)
		}
		if !ok {
			delete(w.since, name)
			continue
		}
		first, seen := w.since[name]
		if !seen {
			w.since[name] = now
			continue
		}
		if now.Sub(first) < w.cfg.Settle {
			continue
		}
		delete(w.since, name)
		w.handled[name] = true // one submit per appearance, whatever happens
		job, err := w.cfg.Submit(w.ctx, ingest.Request{Path: dir, Label: name})
		if err != nil {
			w.log.Error("card poller: could not start an import", "card", dir, "err", err)
			continue
		}
		w.log.Info("card poller: importing", "card", dir, "ingest", job.ID())
	}
}
```

  - `looksLikeCard(dir)` = `w.isMount(dir, w.cfg.Root)` **and** a top-level directory entry whose name equals `DCIM` or `PRIVATE` under `strings.EqualFold`. Return `false, err` on a stat or read error.
  - The `cannot look into a mount` warning may repeat every tick while it persists. That is acceptable, but if you add per-name de-duplication, clear it when the name disappears.
- [ ] **Step 4: Run**: `go test ./internal/config/ ./internal/cardwatch/` on Windows and the Linux and race commands → PASS; `gofmt -l .`, `go vet ./...`, `go test ./...` → PASS.
- [ ] **Step 5: Commit**

```bash
git add internal/config/ internal/cardwatch/
git commit -m "Add the opt-in card poller for setups without the host script

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 10: Wire notices and the poller in `main`

Suggested implementer: sonnet.

**Files:**
- Modify: `cmd/sd2nas/main.go`

**Interfaces:**
- Consumes: `notices.New/Start`, `srv.MountNotifications`, `srv.SubmitIngest`, `cardwatch.New/Start/Close`, `cfg.PollCards`, `idx.Status`, `svc.List`, `svc.Status`.
- Produces: no exports.

- [ ] **Step 1: Implement.**
  - Add `"poll_cards", cfg.PollCards` to the `starting sd2nas` log line.
  - Declare `var watcher *cardwatch.Watcher` next to `var mgr`.
  - Inside the catalog-open `else` block, after the indexer block and before `svc.StartRescan()`:

```go
		ncfg := notices.Config{DB: db, Staging: svc.List, Log: log,
			StagingReady: func() bool { st := svc.Status(); return !st.Rescanning && st.LastRescan != nil }} // PF21
		if idxErr == nil {
			ncfg.Archive = idx.Status
		}
		if notes, err := notices.New(ncfg); err != nil {
			log.Error("notices unavailable", "err", err)
			srv.AddProblem("notices: " + err.Error())
		} else {
			srv.MountNotifications(notes) // srv.Close closes notes
			notes.Start()                 // reads the indexer and staging status from its own goroutine (lock-order rules)
		}
		if cfg.PollCards {
			watcher = cardwatch.New(cardwatch.Config{Root: cfg.Cards, Submit: srv.SubmitIngest, Log: log})
			watcher.Start()
			log.Info("card poller on: cards mounted under the cards folder after startup are imported", "cards", cfg.Cards)
		}
```

  - In **both** exit paths (`case err := <-errc` and after `<-ctx.Done()`), close the watcher first, before `mgr.Close`, so it can't submit into a closing manager: `if watcher != nil { watcher.Close() }`.
  - When `cfg.PollCards` is true but the catalog failed (no manager), log at Warn `card poller off: ingest is disabled`.
- [ ] **Step 2: Check by running it.**
  - `go build ./cmd/sd2nas`.
  - In Git Bash, run `./sd2nas.exe --cards ./testdata/cards --photos <scratch>/photos --config <scratch>/config --addr 127.0.0.1:18081 --poll-cards &`.
  - `curl -s localhost:18081/api/notifications` → `{"pending":[]}`.
  - The log shows `not importing what was already mounted at startup` with the fixture card names.
  - A second instance on the same `--config` exits with code 3 and logs `… is in use by another SD2NAS (pid …); stop it first`.
  - Stop both, and delete `sd2nas.exe` (the gate requires a clean tree).
- [ ] **Step 3: Run**: `gofmt -l .`, `go vet ./...`, `go test ./...`, and the Linux command → PASS.
- [ ] **Step 4: Commit**

```bash
git add cmd/sd2nas/main.go
git commit -m "Start the notices queue and the optional card poller

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 11: Finalize the Unraid template; write the README

Suggested implementer: sonnet.

**Files:**
- Modify: `deploy/unraid/sd2nas.xml`
- Create: `deploy/template_test.go`, `README.md`

**Interfaces:**
- Consumes: Global Constraints (paths, env, the script's install paths, endpoints).
- Produces: documentation and the template.

- [ ] **Step 1: Write the failing test** `deploy/template_test.go` `TestUnraidTemplate`. It parses `unraid/sd2nas.xml` with `encoding/xml` into a struct (child elements by name; `Config` entries with attributes `Name, Target, Default, Mode, Type, Display, Required, Mask` and chardata value) and asserts:
  - **Elements:**
    - `Repository` = `ghcr.io/aspiziri/sd2nas:edge`;
    - `TemplateURL` = `https://raw.githubusercontent.com/aspiziri/SD2NAS/main/deploy/unraid/sd2nas.xml`;
    - `Icon` = `https://raw.githubusercontent.com/aspiziri/SD2NAS/main/web/public/icon-512.png`, and `../web/public/icon-512.png` exists;
    - `ExtraParams` = `--restart=unless-stopped`;
    - `Privileged` = `false`;
    - `WebUI` = `http://[IP]:[PORT:8080]/`;
    - `Requires` contains `Unassigned Devices` and `User Scripts`;
    - `Overview` contains `https://github.com/aspiziri/SD2NAS#readme`.
  - **Config targets:** exactly `8080`, `/cards`, `/photos`, `/config`, `PUID`, `PGID`, `UMASK`, `SD2NAS_POLL_CARDS`, in that order, with no `SD2NAS_TOKEN`.
    - `8080`: Type `Port`, Mode `tcp`.
    - `/cards`: Mode `rw,slave`, Default `/mnt/disks`.
    - `/photos`: Mode `rw`, Default `/mnt/user/photos`.
    - `/config`: Mode `rw`, Default `/mnt/user/appdata/sd2nas`.
    - `PUID` 99, `PGID` 100, `UMASK` 000.
    - `SD2NAS_POLL_CARDS`: Type `Variable`, Display `advanced`, Default `false`.
    - Every Config's value equals its Default.
- [ ] **Step 2: Run and confirm it fails**: `go test ./deploy/ -run Template`.
- [ ] **Step 3: Edit the template.** Keep every existing element, and change only these:
  - `<Overview>`: keep the first paragraph. Replace the last line with `Set up the card script and notifications as the README describes: https://github.com/aspiziri/SD2NAS#readme. After starting, open /healthz to check the time zone, user and mounts.`
  - Add, after `<Overview>`: `<Requires>Unassigned Devices and Unassigned Devices Plus (they mount the cards), and User Scripts (hourly notices). See https://github.com/aspiziri/SD2NAS#readme</Requires>`.
  - `<Icon>…icon-512.png</Icon>` and `<ExtraParams>--restart=unless-stopped</ExtraParams>`.
  - Port Config description: `Host port for the web UI. The card script reaches SD2NAS on it: set SD2NAS_URL=http://localhost:<this port> in sd2nas.cfg (see the README).`
  - Remove the `SD2NAS_TOKEN` Config (Decision 18).
  - Add, last: `<Config Name="Card poller" Target="SD2NAS_POLL_CARDS" Default="false" Mode="" Description="Only for setups without the Unassigned Devices script: true imports cards that get mounted under /cards after the container starts (it can't unmount them or send Unraid notifications). Leave false when the script is installed." Type="Variable" Display="advanced" Required="false" Mask="false">false</Config>`
- [ ] **Step 4: Write `README.md`.** It is plain and short (about 150 lines), for a public repo: no owner IPs, use `<unraid-ip>` and `<port>`. Sections, with these exact commands and settings:
  1. **What it does:** one paragraph, plus a link to `docs/PLAN.md`.
  2. **Install on Unraid:**
     - Docker → Template repositories: add `https://github.com/aspiziri/SD2NAS`, then Add Container → SD2NAS. The owner checks this works on the NAS (Task 12, step 1); if Unraid doesn't find the template that way, the README gives only the direct download below.
     - Or download the template directly: `curl -fsSL https://raw.githubusercontent.com/aspiziri/SD2NAS/main/deploy/unraid/sd2nas.xml -o /boot/config/plugins/dockerMan/templates-user/my-SD2NAS.xml`, then Add Container → Template → `SD2NAS`.
     - Paths: `/cards` = `/mnt/disks` (RW/Slave), `/photos` = the photos share, `/config` = appdata on an exclusive share or pool path.
     - `TZ` comes from Unraid; check `/healthz`.
  3. **Card script:**
     1. Install **Unassigned Devices** and **Unassigned Devices Plus** from Community Apps.
     2. `curl -fsSL https://raw.githubusercontent.com/aspiziri/SD2NAS/main/deploy/unassigned-devices.sh -o /boot/config/plugins/unassigned.devices/sd2nas.sh`
     3. `echo 'SD2NAS_URL=http://localhost:<port>' > /boot/config/plugins/unassigned.devices/sd2nas.cfg`, using the container's host port.
     4. Insert a card into **each slot** of the reader once. In Main → Unassigned Devices, open that slot's settings and set:
        - **Automount** on;
        - **Read Only** on;
        - **Share** off;
        - the device script to `/boot/config/plugins/unassigned.devices/sd2nas.sh`;
        - **Run Device Script File in the background** on.

        Unassigned Devices keeps settings per slot.
     5. Insert a card: you get `<card>: N new, M already known. Card safe to format.` and the card is unmounted.
  4. **Notifications:**
     - Settings → Notification Settings: enable your agent (Pushover, ntfy, Telegram…) and tick **Agents** for **Notices, Warnings and Alerts**. Successful imports are Notices.
     - SD2NAS's own notices (stale staging folders after 09:00 container time, a failed nightly backup, files missing from the archive) are delivered after each card and by a **User Scripts** entry: Community Apps → User Scripts → Add New Script `SD2NAS notices`, with the content `#!/bin/bash` and `ACTION=NOTIFY bash /boot/config/plugins/unassigned.devices/sd2nas.sh`, on the schedule **Hourly**.
     - Without User Scripts: a file `/boot/config/plugins/dynamix/sd2nas.cron` containing `0 * * * * ACTION=NOTIFY bash /boot/config/plugins/unassigned.devices/sd2nas.sh >/dev/null 2>&1`, then run `update_cron`.
     - To see what's waiting: `curl -s http://localhost:<port>/api/notifications`.
  5. **Without the host script (card poller):** set the template's Card poller to `true`.
     - It imports cards mounted after the container starts. A card needs `DCIM/` or `PRIVATE/` and must stay mounted for 10 s.
     - What was mounted at startup is never imported.
     - It can't unmount the card or send Unraid notifications; check the web UI.
  6. **Updating:**
     - Docker → Check for Updates → Apply Update. It restarts the container, so wait for running imports, promotes and archive scans first.
     - Re-run step 2's `curl` when the release notes say the script changed; `sd2nas.cfg` keeps your settings.
     - Existing installs: Edit the container → Advanced View → Extra Parameters: `--restart=unless-stopped`.
     - Downgrading is not supported: an older SD2NAS refuses a newer catalog, so restore `catalog-YYYY-MM-DD.db` from `Management/SD2NAS` into appdata as `sd2nas.db` with the container stopped.
  7. **Troubleshooting:**
     - `grep sd2nas /var/log/syslog` (the script logs every result there).
     - Open `/healthz`.
     - A container that exits with code 3 found another SD2NAS using the same appdata folder. The log line names it: `/config is in use by another SD2NAS (pid … on … since …); stop it first`.
     - With `--restart=unless-stopped`, Docker keeps restarting that container and the line repeats. That is harmless, but stop the duplicate container.
     - An import can be started by hand: `curl -s -X POST -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' 'http://localhost:<port>/api/ingest?wait=1&format=text' -d '{"path":"/cards/<folder under /mnt/disks>"}'`.
  8. **Development:** point to `docs/PLAN.md` §14 (`go run ./cmd/sd2nas --cards ./testdata/cards --photos ./tmp/photos --config ./tmp/config`, `npm run dev` in `web/`).
- [ ] **Step 5: Run**: `go test ./deploy/` (Windows: the template test runs and the script tests skip) and the Linux command → PASS. No `xmllint` is needed, because the Go test parses the template.
- [ ] **Step 6: Commit**

```bash
git add deploy/unraid/sd2nas.xml deploy/template_test.go README.md
git commit -m "Finalize the Unraid template and document the install in a README

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

---

### Task 12: PLAN.md: tick M6, record the decisions and the owner's steps; final verification

Suggested implementer: sonnet (the image smoke run needs Docker).

**Files:**
- Modify: `docs/PLAN.md`

- [ ] **Step 1: Edit `docs/PLAN.md`.**
  1. **§16 M6:**
     - Tick: `unassigned-devices.sh`; Template + README; panic recovery; the `/config` lock; the fallback poller; the daily stale-staging notification; the backup/missing notifications.
     - **Leave unticked** `End-to-end test on the NAS with a real card, twice`.
     - Add an unticked item: `- [ ] Real-card check of the ignore list (the owner's): after an import, look at the import page's "not imported" list; anything harmless (e.g. a new sidecar type) goes on the ignore list (§7)`.
     - Under the list add: `Implementation plan: docs/plans/m6-unraid-integration.md.`, a summary paragraph of the delivered behaviour (Decisions 1, 4, 9, 10, 12, 14), and **"On the NAS (the owner's)"** with these numbered steps:
       1. Update the container. Edit it → Advanced View → Extra Parameters `--restart=unless-stopped`; set Card poller `false`. Check whether Docker → Template repositories with `https://github.com/aspiziri/SD2NAS` finds the template; if not, remove that method from the README.
       2. Install the script and `sd2nas.cfg` (`SD2NAS_URL=http://localhost:4040`) as the README says.
       3. Set both reader slots in Unassigned Devices: Automount on, Read Only on, Share off, script `/boot/config/plugins/unassigned.devices/sd2nas.sh`, run in the background. Check that the option names match the README.
       4. Notification Settings: Agents ticked for Notices, Warnings and Alerts.
       5. User Scripts: `SD2NAS notices`, Hourly, `ACTION=NOTIFY bash /boot/config/plugins/unassigned.devices/sd2nas.sh`. Run it once by hand; `grep sd2nas /var/log/syslog` shows no `could not fetch notices` line.
       6. Insert a real card → a Notice `<card>: N new, … Card safe to format.`, and the card is unmounted in Unassigned Devices. Reinsert it → `0 new, N already known. … Card safe to format.` Tick the end-to-end item with the date and counts.
       7. On that import's page, check the "not imported" list (`Thumbs.db`, `desktop.ini`, `.DSC`, `.SRT`, anything else) and tick the ignore-list item, noting the result.
       8. Optional: `curl -s -X PUT -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' http://localhost:4040/api/settings -d '{"backup_dir":"/proc/sd2nas-test","archive_rescan_at":"<the next minute>"}'`. At that minute SD2NAS runs its nightly sequence: the failing backup **and then an archive pass**. After the M5 backfill, that pass is a quick stat-only rescan. If the backfill hasn't run yet, this pass **is** the backfill (hours of array reads): let it run as the backfill, or Pause it from the Archive index page. Wait 2 minutes and run the User Scripts entry → an Alert `Nightly catalog backup failed: …`. Then restore `backup_dir` to `""` and `archive_rescan_at` to `03:00`.
  2. **§11:**
     - Replace the bash block with: ``The script is `deploy/unassigned-devices.sh` (README: install, `sd2nas.cfg`).``, followed by a bullet summary of Global Constraints' card-script behaviour: `SD2NAS_URL`; unmount only on 200; its own texts for no answer, timeout and a non-SD2NAS reply; `ERROR_MOUNT`; `ACTION=NOTIFY`; `--max-time 7260`.
     - Replace the **Fallback poller** paragraph with Decision 10's rules and the `SD2NAS_POLL_CARDS` switch.
  3. **§3:** replace `the daily notification comes with M6's card script` with ``SD2NAS queues a notice once a day after 09:00 (container time) listing the stale folders; the card script delivers it (§11).``. After the backup paragraph's last sentence add: `A failed backup also queues an Unraid notification.`
  4. **§10:** add to the endpoint block `GET    /api/notifications                notices waiting for the card script (M6)` and `POST   /api/notifications/take           hand them out once; ?format=text for the script`.
  5. **§6:** after the schema-3 sentence add: ``Schema version 4 adds `notifications` (the notices queue: unique key, taken_at).``
  6. **§14:** in **Unraid setup**:
     - the template now has `--restart=unless-stopped`, an icon and the Card poller variable, and no token variable until M7;
     - the install steps are in `README.md`;
     - startup takes `/config/sd2nas.lock`, and a second instance exits with code 3.
  7. **§15 table:** add a row `| Notifications | SD2NAS queues notices; the card script delivers them to Unraid's notify after each card and hourly from User Scripts (no bind mount of the host's notify, no webhook) |`.
  8. **M7:**
     - extend the token item to ``- [ ] Optional token auth (add `SD2NAS_TOKEN` back to the template; the card script already sends it)``;
     - add ``- [ ] Setting for the time of the daily stale-staging notice (fixed at 09:00 in M6)``.
- [ ] **Step 2: Check that CI needs nothing.**
  - `ci.yml`'s Go job runs `go test ./...` on ubuntu with `SD2NAS_REQUIRE_TOOLS=1`, so the script tests run with the runner's bash and curl.
  - If bash or curl is missing there, stop and report. Don't edit `.github/`.
- [ ] **Step 3: Final verification of the whole branch.** Paste the tail of each output in the report.
  - `gofmt -l .` prints nothing. `go vet ./...` and `go test ./...` pass on Windows. The Linux and race commands pass, and `go test ./deploy/ -v` inside the Linux container shows the `TestScript*` tests ran (not skipped).
  - `git diff --stat main..HEAD -- web/ internal/indexer/ internal/staging/ internal/thumbs/ .github/ deploy/Dockerfile deploy/entrypoint.sh go.mod go.sum` prints nothing.
  - **The image smoke run** (Git Bash, Docker Desktop). Use a **named volume** for `/config` (`sd2nas-m6-config`), so `flock` runs on the VM's own filesystem.
    1. `docker build -f deploy/Dockerfile -t sd2nas:m6 .`
    2. `docker run -d --name sd2nas-m6 -e TZ=America/Los_Angeles -p 18080:8080 -v "$(pwd -W)/testdata/cards:/cards:ro" -v <scratch>/photos:/photos -v sd2nas-m6-config:/config sd2nas:m6`.
    3. **Script against the real image.**
       - Put stub `blkid`, `rc.unassigned`, `logger` and `notify` scripts (each appending its arguments to `/stubs/calls.log`) into `<scratch>/stubs`.
       - Run: `docker run --rm --network container:sd2nas-m6 -v "$(pwd -W)/deploy:/deploy:ro" -v <scratch>/stubs:/stubs -e ACTION=ADD -e DEVICE=/dev/sdz1 -e LABEL=SONY_A7IV -e MOUNTPOINT=/mnt/disks/SONY_A7IV -e SD2NAS_CONF=/none -e SD2NAS_NOTIFY=/stubs/notify -e PATH=/stubs:/usr/local/sbin:/usr/sbin:/sbin:/usr/bin:/bin alpine:3.22 sh -c 'apk add -q bash curl && bash /deploy/unassigned-devices.sh'`.
       - `calls.log` shows `rc.unassigned umount /dev/sdz1` and a `notify … -i normal` whose message ends `Card safe to format.` (2 new).
       - Run it again → the message has `0 new, 2 already known` and ends `Card safe to format.`.
    4. **Backup-failure notice.**
       - `curl -s -X PUT -H 'X-SD2NAS: 1' -H 'Content-Type: application/json' localhost:18080/api/settings -d '{"backup_dir":"/proc/sd2nas-test","archive_rescan_at":"<HH:MM of the next minute, America/Los_Angeles>"}'` → 200.
       - This also starts an archive pass at that minute; the smoke `/photos` has no `Archive/`, so the pass is skipped at once.
       - Within 2 minutes, `GET /api/notifications` lists a `backup:` notice starting `Nightly catalog backup failed:`.
       - Run the script with `ACTION=NOTIFY` → `notify … -i alert` with that message, and a second `GET` shows `{"pending":[]}`.
    5. **Lock.**
       - `docker run --name sd2nas-m6b -v sd2nas-m6-config:/config -v <scratch>/photos:/photos sd2nas:m6`. It exits by itself; `docker wait sd2nas-m6b` prints `3`.
       - `docker logs sd2nas-m6b` shows `/config is in use by another SD2NAS (pid … on … since …); stop it first`.
       - Remove that container.
    6. **Poller.**
       - `docker run -d --name sd2nas-m6p -e SD2NAS_POLL_CARDS=true -p 18082:8080 -v <scratch>/cards-empty:/cards -v "$(pwd -W)/testdata/cards:/seed:ro" -v <scratch>/photos2:/photos -v sd2nas-m6p-config:/config sd2nas:m6`
       - `docker exec --privileged -u 0 sd2nas-m6p sh -c 'mkdir -p /cards/CARD && mount -t tmpfs tmpfs /cards/CARD && cp -r /seed/SONY_A7IV/. /cards/CARD/'`.
       - Within 30 s, `GET localhost:18082/api/ingests` lists an import of `/cards/CARD`, and the log has `card poller: importing`.
       - If Docker Desktop refuses the mount, say so in the report and rely on the unit tests.
    7. `time docker stop sd2nas-m6` (and `sd2nas-m6p`) → each under 2 s, with `shutting down` logged.
    8. Remove the containers, the volumes `sd2nas-m6-config` and `sd2nas-m6p-config`, the image `sd2nas:m6`, and the scratch folders.
  - `git status --short` shows only `?? HANDOFF.md`.
  - **Trailer counts:** `git log --format=%B main..HEAD | grep -c 'Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>'` equals `git rev-list --count main..HEAD`, and `git log --format=%B main..HEAD | grep -ci 'co-authored-by'` equals the same number.
- [ ] **Step 4: Commit**

```bash
git add docs/PLAN.md
git commit -m "Tick M6 in the plan, record its decisions and the owner's NAS steps

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>"
```

- [ ] **Step 5: Final report.** List:
  - Decisions 1–21, marking the owner-overridable ones;
  - the owner's NAS steps;
  - the M4/M5 follow-ups that are still open (none was touched; M6 changed no staging, indexer or web code);
  - this follow-up: a `/healthz` warning when notices sit pending for more than a day, which would mean no script is collecting them.
