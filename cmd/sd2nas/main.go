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
	"github.com/aspiziri/SD2NAS/internal/ingest"
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
		os.Exit(1)
	}
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
	log.Info("starting sd2nas", "version", version, "addr", cfg.Addr,
		"cards", cfg.Cards, "photos", cfg.Photos, "config", cfg.Data,
		"tz", os.Getenv("TZ"), "uid", os.Getuid(), "gid", os.Getgid())

	srv := api.New(version, []api.Mount{
		{Name: "cards", Path: cfg.Cards},
		{Name: "photos", Path: cfg.Photos, NeedWrite: true},
		{Name: "config", Path: cfg.Data, NeedWrite: true},
	})
	srv.SetLogger(log)
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
