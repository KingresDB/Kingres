// SPDX-License-Identifier: PostgreSQL

package main

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/serverinfo"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
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
}

// serveInternal waits until the server certificate exists (it is created by
// the setup task on the first server), then serves the internal API with
// mutual TLS until ctx ends. The CA of this server is loaded when it holds one
// (the leader); the revocation list is then live, otherwise the CA certificate
// alone is trusted.
func serveInternal(ctx context.Context, d internalDeps) {
	dir := d.svc.PKIDir()
	for ctx.Err() == nil {
		if _, err := os.Stat(filepath.Join(dir, cluster.ServerCertFile)); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(500 * time.Millisecond):
		}
	}
	if ctx.Err() != nil {
		return
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(dir, cluster.ServerCertFile), filepath.Join(dir, cluster.ServerKeyFile))
	if err != nil {
		d.log.Error("internal API: loading the server certificate", "error", err)
		return
	}
	var verifier internalapi.PeerVerifier
	var revoked func() []string
	if ca, err := pki.LoadCA(dir); err == nil {
		verifier, revoked = ca, ca.Revoked
	} else if caPEM, rerr := os.ReadFile(filepath.Join(dir, pki.CACertFile)); rerr == nil { //nolint:gosec // path below the pki directory
		v, verr := pki.NewVerifier(caPEM)
		if verr != nil {
			d.log.Error("internal API: reading the CA certificate", "error", verr)
			return
		}
		verifier, revoked = v, func() []string { return nil }
	} else {
		d.log.Error("internal API: no CA certificate", "error", rerr)
		return
	}
	me := d.serverHost
	be := internalapi.Backend{
		State: func(context.Context) internalapi.State {
			return internalapi.State{ServerID: me, NodeState: string(d.node.State()), Version: d.version,
				InternalVersion: internalapi.Version, UptimeSeconds: int64(time.Since(d.started).Seconds())}
		},
		// The leader is the server holding the primary of shard group 0.
		Leader:  d.leaders.Current,
		Self:    me,
		Forward: d.forward,
		Revoked: revoked,
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
		Addr: d.addr, Handler: internalapi.New(verifier, be, d.log, nil), TLSConfig: internalapi.TLSConfig(pair),
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
	for rows.Next() && len(res.Rows) < limit {
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
