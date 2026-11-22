// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"io"

	"context"
	"fmt"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgeranchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerround"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// ledgerParts are the signing keys of the ledger on this server (design ledger-phase7.md, steps 3 and 4 of the
// plan): the service with its tasks and operations, and the scheduler that runs its jobs on the manager leader.
type ledgerParts struct {
	svc   *ledgersign.Service
	sched *scheduler.Scheduler
	// coll is the collector of the publication (steps 5 and 6); listener serves the published files, nil when none is configured.
	coll     *ledgerround.Collector
	listener *ledgerpub.Listener
	// anchor is the anchoring of the aggregate checkpoints (step 10).
	anchor *ledgeranchor.Service
	log    *slog.Logger
}

// defaultOriginPrefix is the origin prefix used until the operator sets one: the address of this server and
// "kingres-ledger" (4.2), made a valid name (no space, no plus sign, no control character, no scheme).
func defaultOriginPrefix(serverHost string) string {
	var b strings.Builder
	for _, r := range serverHost {
		switch {
		case r <= ' ' || r == '+' || r == '/' || r == 0x7f || r > 0x7e:
			b.WriteByte('-')
		default:
			b.WriteRune(r)
		}
	}
	h := strings.Trim(b.String(), "-.")
	if h == "" {
		h = "kingres"
	}
	p := h + "/" + ledgersign.DefaultPrefixName
	if len(p) > ledgerkeys.MaxPrefixSize {
		p = p[len(p)-ledgerkeys.MaxPrefixSize:]
		p = strings.TrimLeft(p, "/-.")
	}
	return p
}

// newLedgerParts builds the signing keys. The key directory is created (mode 0700) and the passphrase file, when
// one is configured, is read once now, so that a wrong path or a file others can read stops kingresd at the start
// and not at the first signature.
func newLedgerParts(cfg config.Config, svc *cluster.Service, self string, log *slog.Logger, members *membership.Manager, opsSvc *ops.Ops,
	leaders *leader.Resolver, auditEv func(context.Context, audit.Event), calls *callTable, faultHook *fault.Hook) (*ledgerParts, error) {
	dir := cfg.LedgerKeysDir
	if dir == "" {
		dir = filepath.Join(cfg.DataDir, "ledger", "keys")
	}
	if cfg.LedgerKeyPassphraseFile != "" {
		p, err := ledgerkeys.ReadPassphraseFile(cfg.LedgerKeyPassphraseFile)
		if err != nil {
			return nil, fmt.Errorf("ledger-key-passphrase-file: %w", err)
		}
		p.Destroy()
	} else {
		log.Warn("the signing keys of the ledger are kept in clear text (no ledger-key-passphrase-file), like the key of the certificate authority; the host and the backups protect them")
	}
	kr, err := ledgersign.NewKeyring(dir, cfg.LedgerKeyPassphraseFile)
	if err != nil {
		return nil, err
	}
	sys := func(ctx context.Context, f func(c *pgx.Conn) error) error {
		c, err := svc.ServiceConn(ctx, sysdb.Database)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return f(c)
	}
	store := ledgersign.PGStore{Sys: sys}
	if faultHook != nil {
		store.OnWrite = faultHook.LedgerWrite
	}
	sched, err := scheduler.New(scheduler.Config{
		Self: self, Leader: leaders.Get, Margin: cfg.HA.LeaseMargin, Bump: store.BumpTerm, Log: log.With("component", "scheduler"),
	})
	if err != nil {
		return nil, err
	}
	var coll *ledgerround.Collector
	alertFn := func(ctx context.Context, kind, severity, subject, message string, raise bool) {
		err := sys(ctx, func(c *pgx.Conn) error {
			if !raise {
				_, err := c.Exec(ctx, `UPDATE kingres_cp.alert SET cleared_at = now() WHERE kind = $1 AND subject = $2 AND cleared_at IS NULL`, kind, subject)
				return err
			}
			_, err := c.Exec(ctx, `INSERT INTO kingres_cp.alert (severity, kind, subject, message, raised_at)
				VALUES ($1, $2, $3, $4, now()) ON CONFLICT (kind, subject) WHERE cleared_at IS NULL DO UPDATE SET message = $4, severity = $1`, severity, kind, subject, message)
			return err
		})
		if err != nil {
			log.Warn("ledger signing: raising an alert", "kind", kind, "error", err)
		}
	}
	pubState, err := ledgerstate.Open(dir)
	if err != nil {
		return nil, err
	}
	holderHosts := func(ctx context.Context) []string {
		var hosts []string
		_ = sys(ctx, func(c *pgx.Conn) error {
			rows, err := c.Query(ctx, `SELECT DISTINCT host FROM kingres.node WHERE group_id = 0 AND is_active ORDER BY host`)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var h string
				if err := rows.Scan(&h); err != nil {
					return err
				}
				if h != self {
					hosts = append(hosts, h)
				}
			}
			return rows.Err()
		})
		return hosts
	}
	lc := ledgersign.Config{
		Published: pubState, PublishedCheck: func(ctx context.Context) error { return coll.CheckPublished(ctx) },
		Self: self, Store: store, Keyring: kr, Term: sched.Term, DefaultPrefix: defaultOriginPrefix(self),
		// A rotation signs the latest checkpoint of every log again with the keys that sign now (correction 47).
		Resign:             func(ctx context.Context, set ledgerkeys.Set) error { return coll.Resign(ctx, set) },
		PublicationFailure: func() string { return coll.SignFailure() },
		// The candidates of the manager leader are the servers that run an instance of shard group 0 (3.2.2).
		Holders: func(ctx context.Context) ([]string, error) {
			var hosts []string
			err := sys(ctx, func(c *pgx.Conn) error {
				rows, err := c.Query(ctx, `SELECT DISTINCT host FROM kingres.node WHERE group_id = 0 AND is_active ORDER BY host`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var h string
					if err := rows.Scan(&h); err != nil {
						return err
					}
					hosts = append(hosts, h)
				}
				return rows.Err()
			})
			return hosts, err
		},
		StartTask: opsSvc.StartTask,
		Audit:     auditEv,
		Log:       log.With("component", "ledger-signing"),
		Alert:     alertFn,
	}
	if members != nil {
		lc.Call = members.Client.Do
	}
	s, err := ledgersign.New(lc)
	if err != nil {
		return nil, err
	}
	for _, j := range s.Jobs() {
		if err := sched.Add(j); err != nil {
			return nil, err
		}
	}
	// The internal calls: this server answers the leader's install, remove and list of key files.
	if calls != nil {
		stAgent := &ledgerstate.Agent{File: pubState}
		for _, key := range ledgerstate.Calls {
			calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) { return stAgent.Handle(ctx, key, body) })
		}
		for _, key := range ledgersign.Calls {
			calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) {
				return s.Agent().Handle(ctx, key, body)
			})
		}
	}
	// The collector of the publication (steps 5 and 6): it reads the groups, extends the logs and builds the aggregate.
	pub := ledgerpub.PGStore{Sys: ledgerpub.SysFunc(sys)}
	if faultHook != nil {
		pub.OnWrite = faultHook.LedgerWrite
	}
	reader := &ledgerround.Reader{
		Conn: func(ctx context.Context, group int32, db string) (*pgx.Conn, error) {
			return svc.PrimaryConn(ctx, group, db)
		},
		HasPrimary: func(group int32) bool {
			_, ok := svc.PrimaryOf(group)
			return ok
		},
	}
	src := &ledgerround.ClusterSource{Self: self, Conn: svc.ServiceConn, Local: reader,
		SysConn: func(ctx context.Context) (*pgx.Conn, error) { return svc.ServiceConn(ctx, sysdb.Database) }}
	if members != nil {
		src.Call = members.Client.Do
	}
	files := &ledgerround.Files{Store: pub, Prefix: s.Prefix,
		Leader: func(ctx context.Context) (string, bool, error) {
			id, err := leaders.Get(ctx)
			return id, err == nil && id == self, err
		}}
	if members != nil {
		files.Call = members.Client.Do
	}
	var listener *ledgerpub.Listener
	if cfg.LedgerPublicationListen != "" {
		listener = &ledgerpub.Listener{Addr: cfg.LedgerPublicationListen, TLSCert: cfg.LedgerPublicationTLSCert, TLSKey: cfg.LedgerPublicationTLSKey,
			Handler: &ledgerpub.Handler{Objects: files.Objects, Log: log.With("component", "ledger-publication")}, Log: log.With("component", "ledger-publication")}
	}
	coll, err = ledgerround.New(ledgerround.Config{
		Store: pub, Source: src, Keys: s, Log: log.With("component", "ledger-round"), Alert: alertFn,
		Published: pubState, PullState: pullState(members, holderHosts, log.With("component", "ledger-state")),
		MissingFor: missingFor(log),
		OnNewChain: opsSvc.SyncClientKeys,
		Listener:   listener.Status,
		DatabaseExists: func(ctx context.Context, db string) (bool, error) {
			var ok bool
			err := sys(ctx, func(c *pgx.Conn) error {
				return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)`, db).Scan(&ok)
			})
			return ok, err
		},
		Databases: func(ctx context.Context) ([]ledgerround.Database, error) {
			var out []ledgerround.Database
			err := sys(ctx, func(c *pgx.Conn) error {
				rows, err := c.Query(ctx, `SELECT database_name, coalesce(ledger_digest_interval, 0) FROM kingres_cp.database_setting WHERE ledger_mode <> 'off' ORDER BY database_name`)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var d ledgerround.Database
					var secs int
					if err := rows.Scan(&d.Name, &secs); err != nil {
						return err
					}
					d.DigestInterval = time.Duration(secs) * time.Second
					out = append(out, d)
				}
				return rows.Err()
			})
			return out, err
		},
	})
	if err != nil {
		return nil, err
	}
	for _, j := range coll.Jobs() {
		if err := sched.Add(j); err != nil {
			return nil, err
		}
	}
	if err := sched.Add(stateSyncJob(pubState, members, holderHosts, log.With("component", "ledger-state"))); err != nil {
		return nil, err
	}
	opsSvc.LedgerDigest, opsSvc.LedgerDigests = coll.Digest, coll.Digests
	opsSvc.LedgerRetireDatabase = func(ctx context.Context, db string) error {
		return coll.RetireDatabase(ctx, db, ledgerround.ReasonDropped, true)
	}
	opsSvc.LedgerAudit = pub.AuditPublication
	opsSvc.LedgerChainDiverged = coll.IsDiverged
	opsSvc.LedgerEpochStart = coll.StartEpoch
	opsSvc.BundleDir = filepath.Join(cfg.DataDir, "ledger", "bundles")
	opsSvc.LedgerPublished = func(ctx context.Context, db string) (bool, error) {
		logs, err := pub.Logs(ctx)
		for _, l := range logs {
			if l.Database == db && l.Kind == ledgerpub.KindAggregate && l.Size > 0 {
				return true, nil
			}
		}
		return false, err
	}
	opsSvc.LedgerBundleBuild = func(ctx context.Context, db string, req api.LedgerBundleRequest, files map[string][]byte, w io.Writer) error {
		br := ledgerpub.BundleRequest{Logs: req.Logs, IncludeAnchors: req.IncludeAnchors != nil && *req.IncludeAnchors, Files: files}
		if req.SinceSize != nil {
			br.SinceSize = *req.SinceSize
		}
		prefix, err := s.Prefix(ctx)
		if err != nil {
			return err
		}
		_, err = pub.WriteBundle(ctx, db, br, prefix, time.Now(), w)
		if errors.Is(err, ledgerpub.ErrNoLogs) {
			return problem.New(problem.LedgerNotPublished, "The database has no published ledger yet.")
		}
		if errors.Is(err, ledgerpub.ErrUnknownOrigin) {
			return problem.Validation("The request is invalid.", problem.FieldError{Field: "/logs", Message: "an origin is not a log of this database", Code: "not_found"})
		}
		return err
	}
	if members != nil {
		members.LedgerRetire = coll.RetireServer
	}
	if calls != nil {
		calls.Register(ledgerround.CallRead, func(ctx context.Context, _ string, body []byte) (any, error) { return reader.Handle(ctx, body) })
		calls.Register(ledgerround.CallPublicationGet, func(ctx context.Context, _ string, body []byte) (any, error) { return files.Handle(ctx, body) })
	}
	anch, err := newLedgerAnchor(cfg, dir, sys, sched, members, calls, self, log, alertFn)
	if err != nil {
		return nil, err
	}
	return &ledgerParts{svc: s, sched: sched, coll: coll, listener: listener, anchor: anch, log: log}, nil
}

// run is the loop of the scheduler; it ends when ctx ends and zeroes the opened keys then. The publication listener, when one is configured,
// runs while the loop runs.
func (p *ledgerParts) run(ctx context.Context) {
	if p.listener != nil {
		if err := p.listener.Start(ctx); err != nil {
			p.log.Error("the listener of the ledger publication cannot start", "address", p.listener.Addr, "error", err)
		} else {
			p.log.Info("the ledger publication is served", "address", p.listener.BoundAddr(), "tls", p.listener.TLSCert != "")
		}
	}
	p.sched.Run(ctx)
	if p.listener != nil {
		p.listener.Stop()
	}
	p.svc.Close()
}
