// SPDX-License-Identifier: PostgreSQL

// Command kingresd is the control-plane server of KingresDB. It serves the
// management REST API (kingres/api/openapi.yaml) and the web panel.
package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/httpapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/localsettings"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconninfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sdnotify"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store/pgstore"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/webui"
)

// version is the KingresDB version; release builds set it with
// -ldflags "-X main.version=...".
var version = "0.2.0-dev"

// commit is the build commit, injected with -ldflags "-X main.commit=...".
var commit = "unknown"

// contractRevision is the revision of kingres/api/openapi.yaml implemented.
const contractRevision = "0.5.0"

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
	// Two modes are not settings: -version prints and exits; -stop-instances
	// stops the managed instances (used as ExecStop by the service unit).
	var rest []string
	stopOnly := false
	for _, a := range args {
		switch a {
		case "-version", "--version":
			fmt.Fprintf(os.Stdout, "kingresd %s (contract %s, commit %s)\n", version, contractRevision, commit)
			return config.ErrHelp
		case "-stop-instances", "--stop-instances":
			stopOnly = true
		default:
			rest = append(rest, a)
		}
	}
	args = rest
	cfg, err := config.Load(args, env, stderr)
	if err != nil {
		return err
	}
	// The level can be changed while running (updateThisServerSettings).
	var logLevel slog.LevelVar
	logLevel.Set(cfg.SlogLevel())
	log := newLogger(cfg, stderr, &logLevel)
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
	serverHost := cfg.ServerHost
	if serverHost == "" {
		serverHost = hostname
	}
	binDir, found, err := cluster.ResolveBinDir(cfg.PGBinDir)
	if err != nil {
		return err
	}
	if found {
		vctx, vcancel := context.WithTimeout(ctx, 30*time.Second)
		err := cluster.VerifyBinaries(vctx, binDir)
		vcancel()
		if err != nil {
			return fmt.Errorf("%w (setting pg-bin-dir / KINGRES_PG_BIN_DIR)", err)
		}
		log.Info("PostgreSQL programs", "dir", binDir)
	} else {
		log.Warn("no PostgreSQL programs found (set pg-bin-dir): this server cannot create or manage a cluster")
	}
	internalPort := 0
	if cfg.InternalListen != "" {
		_, ip, perr := net.SplitHostPort(cfg.InternalListen)
		if perr != nil {
			return fmt.Errorf("internal-listen: %w", perr)
		}
		internalPort, _ = strconv.Atoi(ip)
		if rp, _ := strconv.Atoi(port); internalPort != rp+1 {
			log.Warn("the internal API is not on the port above the REST API: other servers find it there when they join or add this server", "listen", cfg.Listen, "internal-listen", cfg.InternalListen)
		}
	}
	svc, err := cluster.New(cluster.Config{
		DataDir: cfg.DataDir, BinDir: binDir, SocketDir: cfg.SocketDir, PGPort: cfg.PGPort, PGListen: cfg.PGListen,
		ServerHost: serverHost, TLSCertFile: cfg.TLSCertFile, TLSKeyFile: cfg.TLSKeyFile, Logger: log,
		InternalPort: internalPort, Version: version, CertificateLifetime: cfg.CertificateLifetime,
	}, node)
	if err != nil {
		return fmt.Errorf("preparing instance management: %w", err)
	}
	if stopOnly {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer scancel()
		return svc.StopInstance(sctx)
	}
	dsn := cfg.PostgresDSN
	if svc.Enabled() {
		// The managed instance is the target: its service credentials are
		// created by the setup task and read from the data directory.
		if d := svc.ServiceDSN("kingres_system"); d != "" {
			dsn = d
		} else {
			dsn, err = pgconninfo.Format(pgconninfo.Pair{Key: "host", Value: "127.0.0.1"}, pgconninfo.Pair{Key: "port", Value: strconv.Itoa(cfg.PGPort)}, pgconninfo.Pair{Key: "dbname", Value: "kingres_system"}, pgconninfo.Pair{Key: "user", Value: "kingresd"})
			if err != nil {
				return err
			}
		}
	}
	pg, err := pgstore.New(pgstore.Options{
		DSN: dsn, Logger: log, PanelPort: port, Version: version, Started: started, NodeTLS: svc.NodeTLSConfig,
	})
	if err != nil {
		return err
	}
	defer pg.Close()
	log.Info("PostgreSQL target", "target", pg.Target())

	authn, err := auth.NewPostgres(dsn, 5*time.Second)
	if err != nil {
		return err
	}
	sessions := auth.NewStore(cfg.SessionTTL, cfg.MaxSessions, time.Now)
	web, err := webui.Handler()
	if err != nil {
		return fmt.Errorf("loading embedded panel: %w", err)
	}

	var opsSvc *ops.Ops
	var ring *monitor.Ring
	var proxy *cluster.Proxy
	if svc.Enabled() {
		if proxy, err = svc.NewProxy(); err != nil {
			return err
		}
		ring = monitor.NewRing(1440, 2000)
		opsSvc = &ops.Ops{Service: svc, Roles: proxy, Instances: svc.Manager(), ServerHost: serverHost, PGPort: cfg.PGPort, Log: log}
	}
	deps := httpapi.Deps{
		Logger: log, Store: pg, Setup: node, Sessions: sessions, Authenticator: authn, Audit: sink,
		Web: web, Version: version, Hostname: hostname, ListenAddr: cfg.Listen,
		LoopbackOnly: !cfg.AllowRemoteSetup, SetupService: svc, DataDir: cfg.DataDir, Ops: opsSvc, Started: started, Metrics: ring, MetricsToken: cfg.MetricsToken,
		LogDir: filepath.Join(cfg.DataDir, "instances", "1", "pgdata", "log"), MaxBodyBytes: cfg.MaxBodyBytes,
		RequestTimeout: cfg.RequestTimeout, LoginRateLimit: cfg.LoginRateLimit,
		LoginRateWindow: cfg.LoginRateWindow, SessionTTL: cfg.SessionTTL, TLS: cfg.TLSCertFile != "",
	}
	if proxy != nil {
		deps.Proxy = proxy
	}
	if svc.Enabled() {
		deps.LocalSettings, err = localsettings.New(localsettings.Options{
			DataDir: cfg.DataDir, Instances: svc.Manager(), InstanceID: cluster.NodeID, PGPort: cfg.PGPort, Level: &logLevel, Log: log})
		if err != nil {
			return err
		}
	}
	leaders := leader.New(pg.ManagerLeader, 2*time.Second, nil)
	lastLeader := &leaderMemory{}
	var members *membership.Manager
	var receiver *membership.Receiver
	peerDir, err := peers.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	if cfg.InternalListen != "" && svc.Enabled() {
		_, iport, perr := net.SplitHostPort(cfg.InternalListen)
		if perr != nil {
			return fmt.Errorf("internal-listen: %w", perr)
		}
		if err := peerDir.Put(peers.Peer{ServerID: serverHost, InternalAddr: net.JoinHostPort(serverHost, iport)}); err != nil {
			return fmt.Errorf("recording this server in the peer directory: %w", err)
		}
		pc := peerclient.New(peerclient.Config{PKIDir: svc.PKIDir(), Peers: peerDir, Revoked: svc.RevokedSerials})
		deps.Forwarder = &peerclient.Forwarder{Self: serverHost, Leaders: leaders, Client: pc}
		opsSvc.Recorder = opsSvc.NewPeerRecorder(func(ctx context.Context) (string, bool, error) {
			return lastLeader.resolve(ctx, leaders, serverHost)
		}, pc)
		members = &membership.Manager{Svc: svc, Ops: opsSvc, Peers: peerDir, Client: peerclient.New(peerclient.Config{PKIDir: svc.PKIDir(), Peers: peerDir, Revoked: svc.RevokedSerials, Timeout: 10 * time.Minute}), Leaders: leaders, Self: serverHost, Version: version, Log: log,
			SelfPeer: peers.Peer{ServerID: serverHost, InternalAddr: net.JoinHostPort(serverHost, iport)},
			Audit: func(ctx context.Context, ev audit.Event) {
				if _, err := sink.Append(ctx, ev); err != nil {
					log.Error("writing the audit log", "error", err)
				}
			}}
		receiver = &membership.Receiver{M: members, Authn: authn, VerifyToken: svc.VerifyToken}
		members.OnCertificateInstalled = func() { pc.ForgetAll(); members.Client.ForgetAll() }
		opsSvc.Fanout, opsSvc.Local = members, svc
		deps.Members = members
		deps.Gatherer, deps.GatherTimeout = membership.Gatherer{M: members}, cfg.GatherTimeout
	}
	api, err := httpapi.New(deps)
	if err != nil {
		return err
	}
	if opsSvc != nil {
		opsSvc.SessionEnder = api.RoleChanged
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
	var engineOnce sync.Once
	startEngine := func(dsn string) error {
		if opsSvc == nil {
			return nil
		}
		var err error
		engineOnce.Do(func() {
			var pool *pgxpool.Pool
			if pool, err = pgxpool.New(bgCtx, dsn); err != nil {
				return
			}
			var eng *task.Engine
			defs := opsSvc.Definitions()
			defs = append(defs, opsSvc.InstanceDefinitions()...)
			if members != nil {
				defs = append(defs, members.Definitions()...)
			}
			eng, err = task.NewEngine(task.NewPGStore(pool), defs, task.Options{
				Holder: fmt.Sprintf("%s-%d", hostname, os.Getpid()), Logger: log})
			if err != nil {
				return
			}
			opsSvc.BG = bgCtx
			opsSvc.SetEngine(eng)
			run := &monitor.Runner{Src: monSource{svc}, Ring: ring, Host: serverHost, DataDir: filepath.Join(cfg.DataDir, "instances"), Log: log, System: "kingres_system",
				Interval: cfg.MonitorInterval, LedgerPendingMaxAge: cfg.LedgerPendingMaxAge, RenewFraction: cfg.CertificateRenewFraction,
				Cert: svc.ServerCertificate, CA: func() *x509.Certificate {
					if ca, err := svc.CA(); err == nil {
						return ca.Cert()
					}
					return nil
				}}
			wg.Add(1)
			go func() { defer wg.Done(); run.Run(bgCtx) }()
			wg.Add(1)
			go func() {
				defer wg.Done()
				runEngineOnLeader(bgCtx, eng, leaders, serverHost, members, log)
				pool.Close()
			}()
		})
		return err
	}
	svc.OnDSN(func(d string) error {
		if err := startEngine(d); err != nil {
			return err
		}
		if err := pg.SetDSN(d); err != nil {
			return err
		}
		return authn.SetDSN(d)
	})
	if proxy != nil && svc.Manager().Exists(cluster.NodeID) {
		if err := proxy.Reset(); err != nil {
			cancelBG()
			wg.Wait()
			return fmt.Errorf("resetting the role mapping: %w", err)
		}
	}
	if err := svc.Resume(bgCtx); err != nil {
		cancelBG()
		wg.Wait()
		return err
	}
	if members != nil {
		members.ResumeLeave() // a leave that was under way when kingresd stopped
	}

	if opsSvc != nil && node.State() == setup.Clustered {
		// The stored network rules are the truth; rewrite pg_hba.conf from them.
		rctx, rcancel := context.WithTimeout(bgCtx, 30*time.Second)
		if err := opsSvc.ApplyStoredRules(rctx); err != nil {
			log.Warn("applying the stored network rules", "error", err)
		} else if err := svc.Manager().Reload(rctx, cluster.NodeID); err != nil {
			log.Warn("reloading the instance", "error", err)
		}
		rcancel()
	}

	if cfg.InternalListen != "" && svc.Enabled() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveInternal(bgCtx, internalDeps{addr: cfg.InternalListen, svc: svc, node: node, ring: ring, log: log, started: started,
				serverHost: serverHost, version: version, dataDir: cfg.DataDir, leaders: leaders, ops: opsSvc, lastLeader: lastLeader, members: members, forward: func(ctx context.Context, peer string, fr internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
					// A forwarding server must be a member of the cluster (CP-07); its certificate was
					// already checked against the CA and the revocation list by the internal API.
					if _, ok := peerDir.Get(peer); !ok {
						return internalapi.ForwardResponse{}, fmt.Errorf("%q is not a server of this cluster", peer)
					}
					return api.ServeForwarded(ctx, peer, fr)
				}, gather: func(ctx context.Context, peer string, fr internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
					// Every server of the cluster may ask; it must be a member (CP-07).
					if _, ok := peerDir.Get(peer); !ok {
						return internalapi.ForwardResponse{}, fmt.Errorf("%q is not a server of this cluster", peer)
					}
					return api.ServeGathered(ctx, peer, fr)
				}, receiver: receiver})
		}()
	}

	if members != nil && cfg.InternalListen != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			runVersionCheck(bgCtx, members, leaders, serverHost, cfg.VersionCheckInterval, log)
		}()
	}
	if members != nil && cfg.InternalListen != "" {
		job := members.NewRenewalJob(cfg.CertificateRenewFraction, cfg.CertificateCheckInterval, cfg.CertificateRetryInterval)
		wg.Add(1)
		go func() { defer wg.Done(); job.Run(bgCtx) }()
	}

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
	if _, err := sdnotify.Notify(os.Getenv("NOTIFY_SOCKET"), "READY=1"); err != nil {
		log.Warn("notifying systemd", "error", err)
	}
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
	_, _ = sdnotify.Notify(os.Getenv("NOTIFY_SOCKET"), "STOPPING=1")
	shutCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutCtx); err != nil {
		log.Warn("graceful shutdown incomplete, closing connections", "error", err)
		_ = srv.Close()
	}
	cancelBG()
	svc.Wait()
	wg.Wait()
	log.Info("stopped")
	return runErr
}

func newLogger(cfg config.Config, out io.Writer, level *slog.LevelVar) *slog.Logger {
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(out, opts))
	}
	return slog.New(slog.NewTextHandler(out, opts))
}

// monSource adapts the cluster service to the monitor.
type monSource struct{ svc *cluster.Service }

func (m monSource) ServiceConn(ctx context.Context, db string) (*pgx.Conn, error) {
	return m.svc.ServiceConn(ctx, db)
}

func (m monSource) InstanceRunning(ctx context.Context) (bool, error) {
	return m.svc.Manager().Running(ctx, cluster.NodeID)
}
