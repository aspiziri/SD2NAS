// Command sd2nas imports SD cards into a staging folder on the NAS and serves
// the web UI for sorting staging into the archive.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/aspiziri/SD2NAS/internal/api"
	"github.com/aspiziri/SD2NAS/internal/config"
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
	log.Info("starting sd2nas", "version", version, "addr", cfg.Addr,
		"cards", cfg.Cards, "photos", cfg.Photos, "config", cfg.Data,
		"tz", os.Getenv("TZ"), "uid", os.Getuid(), "gid", os.Getgid())

	srv := api.New(version, []api.Mount{
		{Name: "cards", Path: cfg.Cards},
		{Name: "photos", Path: cfg.Photos, NeedWrite: true},
		{Name: "config", Path: cfg.Data, NeedWrite: true},
	})
	// Keep serving so /healthz can explain the problem, but make it loud.
	for _, p := range srv.WriteProblems() {
		log.Error("directory is not usable", "detail", p)
	}

	httpSrv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           srv,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
}
