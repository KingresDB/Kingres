// SPDX-License-Identifier: PostgreSQL

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// internalDeps are what the internal listener needs.
type internalDeps struct {
	addr       string
	svc        *cluster.Service
	node       *setup.Manager
	ring       *monitor.Ring
	log        *slog.Logger
	started    time.Time
	serverHost string
	version    string
	dataDir    string
	// leaders reads the manager leader from the catalog; forward runs a
	// forwarded operation (only ever called on the leader).
	leaders *leader.Resolver
	forward func(ctx context.Context, peer string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error)
	// receiver answers the membership calls.
	receiver *membership.Receiver
}

// serveInternal serves the internal API with mutual TLS until ctx ends. A
// server that has no certificate of a cluster yet presents a self-signed
// bootstrap certificate, which a joining or adding server pins or records
// (design D4); only the bootstrap calls are possible then. The certificate is
// chosen at every handshake, so a server that receives its certificate does
// not restart.
func serveInternal(ctx context.Context, d internalDeps) {
	dir := d.svc.PKIDir()
	var logOnce sync.Once
	get := func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
		if pair, err := tls.LoadX509KeyPair(filepath.Join(dir, cluster.ServerCertFile), filepath.Join(dir, cluster.ServerKeyFile)); err == nil {
			return &pair, nil
		}
		if d.node.State() != setup.Uninitialized {
			return nil, errors.New("no server certificate")
		}
		pair, fp, err := pki.EnsureBootstrapCert(dir, d.svc.ServerNames(), time.Now())
		if err != nil {
			return nil, err
		}
		logOnce.Do(func() {
			d.log.Info("bootstrap certificate of the internal API", "sha256", pki.FormatFingerprint(fp))
		})
		return &pair, nil
	}
	if d.node.State() == setup.Uninitialized {
		// Log the fingerprint at start so that the administrator can pin it.
		if _, fp, err := pki.EnsureBootstrapCert(dir, d.svc.ServerNames(), time.Now()); err == nil && !d.svc.Joining() {
			d.log.Info("internal API bootstrap certificate", "sha256", pki.FormatFingerprint(fp),
				"hint", "give this fingerprint in the header X-Kingres-Pin-Sha256 when adding or joining this server to pin it")
		}
	}
	me := d.serverHost
	be := internalapi.Backend{
		State: func(context.Context) internalapi.State {
			return internalapi.State{ServerID: me, NodeState: string(d.node.State()), Version: d.version,
				InternalVersion: internalapi.Version, UptimeSeconds: int64(time.Since(d.started).Seconds())}
		},
		// The leader is the server holding the primary of shard group 0.
		Leader:    d.leaders.Current,
		Self:      me,
		Forward:   d.forward,
		Revoked:   d.svc.RevokedSerials,
		Handle:    d.receiver.Handle,
		Bootstrap: d.receiver.Bootstrap,
		Joining:   d.svc.Joining,
		Resources: func(context.Context) (any, error) {
			return serverinfo.Resources(map[string]string{"data": filepath.Join(d.dataDir, "instances")}, nil), nil
		},
		Logs: func(context.Context, map[string]string) (any, error) {
			return serverinfo.ReadLogs(filepath.Join(d.dataDir, "instances", "1", "pgdata", "log"), &me, 1)
		},
		Metrics: func(context.Context) (any, error) {
			out := map[string]float64{}
			for s, p := range d.ring.Latest() {
				out[s.Metric+"|"+s.ServerID+"|"+s.Database] = *p.V
			}
			return out, nil
		},
		Exec: func(ctx context.Context, r internalapi.ExecRequest) (internalapi.ExecResult, error) {
			return execLocal(ctx, d.svc, r)
		},
	}
	srv := &http.Server{
		Addr: d.addr, Handler: internalapi.New(d.svc, be, d.log, nil), TLSConfig: internalapi.TLSConfigFunc(get),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 64 << 10, ErrorLog: slog.NewLogLogger(d.log.Handler(), slog.LevelWarn),
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", d.addr)
	if err != nil {
		d.log.Error("internal API: listening", "address", d.addr, "error", err)
		return
	}
	d.log.Info("internal API listening", "address", ln.Addr().String())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			d.log.Error("internal API", "error", err)
		}
	}()
	<-ctx.Done()
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(sctx)
	wg.Wait()
}

// execLocal runs one statement on a local database as the service role through
// the local connection and returns at most RowLimit rows. The caller (the
// leader) is authorized by the internal API; the statement is whatever the
// leader sends and is never logged.
func execLocal(ctx context.Context, svc *cluster.Service, r internalapi.ExecRequest) (internalapi.ExecResult, error) {
	limit := r.RowLimit
	if limit == 0 {
		limit = 1000
	}
	c, err := svc.ServiceConn(ctx, r.Database)
	if err != nil {
		return internalapi.ExecResult{}, errors.New("cannot connect to the database")
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	rows, err := c.Query(ctx, r.SQL, pgx.QueryExecModeExec) // one statement only
	if err != nil {
		return internalapi.ExecResult{}, errors.New("the statement failed")
	}
	defer rows.Close()
	res := internalapi.ExecResult{Columns: []string{}, Rows: [][]any{}}
	for _, fd := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, fd.Name)
	}
	total := 0
	for rows.Next() && len(res.Rows) < limit {
		// The size of a result is bounded (CP-04): 1 MiB per value, 16 MiB in all.
		for _, raw := range rows.RawValues() {
			total += len(raw) + 8
			if len(raw) > 1<<20 || total > 16<<20 {
				return res, errors.New("the result is too large")
			}
		}
		v, err := rows.Values()
		if err != nil {
			return res, errors.New("the statement failed")
		}
		res.Rows = append(res.Rows, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return res, errors.New("the statement failed")
	}
	res.Tag = rows.CommandTag().String()
	return res, nil
}

// runEngineOnLeader runs the task engine on the manager leader only: tasks are
// rows of the leader's system database, and a follower's copy holds none. A
// leader that stops being the leader finishes the step in hand (its lease
// protects the row) and then stops claiming. The leader also sweeps abandoned
// pending servers at start and then hourly.
func runEngineOnLeader(ctx context.Context, eng *task.Engine, leaders *leader.Resolver, self string, members *membership.Manager, log *slog.Logger) {
	var lastSweep time.Time
	for ctx.Err() == nil {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		id, err := leaders.Get(lctx)
		cancel()
		if err == nil && id == self {
			if members != nil && time.Since(lastSweep) > time.Hour {
				members.SweepAbandoned(ctx)
				lastSweep = time.Now()
			}
			if ran, rerr := eng.RunOnce(ctx); rerr != nil && ctx.Err() == nil {
				log.Error("task engine", "error", rerr)
			} else if ran {
				continue
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
		}
	}
}
