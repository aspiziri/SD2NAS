// Command sd2nas imports SD cards into a staging folder on the NAS and serves
// the web UI for sorting staging into the archive.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/aspiziri/SD2NAS/internal/api"
	"github.com/aspiziri/SD2NAS/internal/catalog"
	"github.com/aspiziri/SD2NAS/internal/config"
	"github.com/aspiziri/SD2NAS/internal/indexer"
	"github.com/aspiziri/SD2NAS/internal/ingest"
	"github.com/aspiziri/SD2NAS/internal/lockfile"
	"github.com/aspiziri/SD2NAS/internal/staging"
	"github.com/aspiziri/SD2NAS/internal/thumbs"
	"github.com/aspiziri/SD2NAS/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(exitCode(err))
	}
}

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

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Args[1:], os.Getenv)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	if err != nil {
		return err
	}
	if cfg.Umask != nil {
		if err := setUmask(*cfg.Umask); err != nil {
			return err
		}
	}
	// Absolute paths, so the request path the card script sends and the
	// logged paths agree.
	for _, p := range []*string{&cfg.Cards, &cfg.Photos, &cfg.Data} {
		abs, err := filepath.Abs(*p)
		if err != nil {
			return err
		}
		*p = abs
	}
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
	log.Info("starting sd2nas", "version", version, "addr", cfg.Addr,
		"cards", cfg.Cards, "photos", cfg.Photos, "config", cfg.Data,
		"tz", os.Getenv("TZ"), "uid", os.Getuid(), "gid", os.Getgid())

	srv := api.New(version, []api.Mount{
		{Name: "cards", Path: cfg.Cards},
		{Name: "photos", Path: cfg.Photos, NeedWrite: true},
		{Name: "config", Path: cfg.Data, NeedWrite: true},
	})
	srv.SetLogger(log)
	if errors.Is(lockErr, lockfile.ErrUnsupported) {
		msg := fmt.Sprintf("could not lock %s (%v): a second SD2NAS using this folder would not be stopped", cfg.Data, lockErr)
		log.Warn(msg)
		srv.AddWarning(msg)
	}
	if err := srv.MountUI(web.Dist()); err != nil {
		return err
	}
	// Keep serving so /healthz can explain the problem, but make it loud.
	for _, p := range srv.WriteProblems() {
		log.Error("directory is not usable", "detail", p)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var mgr *ingest.Manager
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
			// Keep serving so /healthz can explain; without the routes the card
			// script's request fails and the card stays mounted.
			log.Error("catalog unavailable; ingest is disabled", "err", err)
			srv.AddProblem("catalog: " + err.Error())
		}
	} else {
		defer func() {
			if err := db.Close(); err != nil {
				log.Error("closing catalog", "err", err)
			}
		}()
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
		srv.MountForget(mgr, db, svc)
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
		svc.StartRescan() // §8: fix drift from SMB edits made while SD2NAS was down
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
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errc:
		if mgr != nil {
			// The server never ran, so no handler waits on a job.
			closeCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			err = errors.Join(err, wrap("stopping ingest", mgr.Close(closeCtx)))
		}
		srv.Close()
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	// One budget inside Docker's default 10 s stop timeout. The manager goes
	// first: it cancels the running copy so ?wait=1 handlers return before
	// the HTTP server waits for them. It is nil when the catalog failed to open.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var mgrErr error
	if mgr != nil {
		mgrErr = wrap("stopping ingest", mgr.Close(shutdownCtx))
	}
	// Ends /api/events streams and background work, which Shutdown would
	// otherwise wait for until its deadline.
	srv.Close()
	return errors.Join(mgrErr, wrap("stopping http server", httpSrv.Shutdown(shutdownCtx)))
}

// wrap adds what was being stopped to err; a nil err stays nil.
func wrap(what string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", what, err)
}
