// SPDX-License-Identifier: PostgreSQL

// Command kingresd is the control-plane server of KingresDB. It serves the
// management REST API (kingres/api/openapi.yaml) and the web panel.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/httpapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store/pgstore"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/webui"
)

// version is the KingresDB version; release builds set it with
// -ldflags "-X main.version=...".
var version = "0.1.0-dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err := run(ctx, os.Args[1:], os.LookupEnv, os.Stderr, nil)
	stop()
	if err != nil {
		if errors.Is(err, config.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, "kingresd:", err)
		os.Exit(1)
	}
}

// run starts the server and blocks until ctx is canceled or serving fails. When
// ready is not nil, the listen address is sent on it once the server accepts
// connections (used by tests).
func run(ctx context.Context, args []string, env config.Env, stderr io.Writer, ready chan<- string) error {
	cfg, err := config.Load(args, env, stderr)
	if err != nil {
		return err
	}
	log := newLogger(cfg, stderr)
	log.Info("starting kingresd", "version", version, "config", cfg)

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "localhost"
	}
	started := time.Now()

	node, err := setup.Open(cfg.DataDir, time.Now)
	if err != nil {
		return fmt.Errorf("opening node state: %w", err)
	}
	log.Info("node state", "state", node.State(), "state_file", node.StatePath())
	if node.State() == setup.Uninitialized {
		// The token itself is never logged; only where to read it.
		log.Info("setup token available", "file", node.TokenPath())
	}

	sink, err := audit.OpenFile(cfg.AuditFile, time.Now)
	if err != nil {
		return fmt.Errorf("opening audit log: %w", err)
	}
	defer sink.Close()

	_, port, err := net.SplitHostPort(cfg.Listen)
	if err != nil {
		return fmt.Errorf("parsing listen address: %w", err)
	}
	pg, err := pgstore.New(pgstore.Options{
		DSN: cfg.PostgresDSN, Logger: log, PanelPort: port, Version: version, Started: started,
	})
	if err != nil {
		return err
	}
	defer pg.Close()
	log.Info("PostgreSQL target", "target", pg.Target())

	authn, err := auth.NewPostgres(cfg.PostgresDSN, 5*time.Second)
	if err != nil {
		return err
	}
	sessions := auth.NewStore(cfg.SessionTTL, cfg.MaxSessions, time.Now)
	web, err := webui.Handler()
	if err != nil {
		return fmt.Errorf("loading embedded panel: %w", err)
	}

	api, err := httpapi.New(httpapi.Deps{
		Logger: log, Store: pg, Setup: node, Sessions: sessions, Authenticator: authn, Audit: sink,
		Web: web, Version: version, Hostname: hostname, ListenAddr: cfg.Listen,
		LoopbackOnly: cfg.ListenIsLoopback(), MaxBodyBytes: cfg.MaxBodyBytes,
		RequestTimeout: cfg.RequestTimeout, LoginRateLimit: cfg.LoginRateLimit,
		LoginRateWindow: cfg.LoginRateWindow, SessionTTL: cfg.SessionTTL,
	})
	if err != nil {
		return err
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
		MaxHeaderBytes:    64 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.Listen, err)
	}

	// Background work has one owner (this function) that waits for it.
	var wg sync.WaitGroup
	bgCtx, cancelBG := context.WithCancel(ctx)
	wg.Add(1)
	go func() { defer wg.Done(); sessions.Run(bgCtx, time.Minute) }()

	serveErr := make(chan error, 1)
	go func() {
		var err error
		if cfg.TLSCertFile != "" {
			err = srv.ServeTLS(ln, cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			err = srv.Serve(ln)
		}
		serveErr <- err
	}()
	api.SetReady(true)
	log.Info("listening", "address", ln.Addr().String(), "tls", cfg.TLSCertFile != "")
	if ready != nil {
		ready <- ln.Addr().String()
	}

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutdown requested")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serving: %w", err)
		}
	}

	api.SetReady(false)
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("graceful shutdown incomplete, closing connections", "error", err)
		_ = srv.Close()
	}
	cancelBG()
	wg.Wait()
	log.Info("stopped")
	return runErr
}

func newLogger(cfg config.Config, out io.Writer) *slog.Logger {
	opts := &slog.HandlerOptions{Level: cfg.SlogLevel()}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(out, opts))
	}
	return slog.New(slog.NewTextHandler(out, opts))
}
