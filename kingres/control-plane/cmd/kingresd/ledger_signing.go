// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
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
	leaders *leader.Resolver, auditEv func(context.Context, audit.Event), calls *callTable) (*ledgerParts, error) {
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
	sched, err := scheduler.New(scheduler.Config{
		Self: self, Leader: leaders.Get, Margin: cfg.HA.LeaseMargin, Bump: store.BumpTerm, Log: log.With("component", "scheduler"),
	})
	if err != nil {
		return nil, err
	}
	lc := ledgersign.Config{
		Self: self, Store: store, Keyring: kr, Term: sched.Term, DefaultPrefix: defaultOriginPrefix(self),
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
		Alert: func(ctx context.Context, kind, severity, subject, message string, raise bool) {
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
		},
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
		for _, key := range ledgersign.Calls {
			calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) {
				return s.Agent().Handle(ctx, key, body)
			})
		}
	}
	return &ledgerParts{svc: s, sched: sched}, nil
}

// run is the loop of the scheduler; it ends when ctx ends and zeroes the opened keys then.
func (p *ledgerParts) run(ctx context.Context) {
	p.sched.Run(ctx)
	p.svc.Close()
}
