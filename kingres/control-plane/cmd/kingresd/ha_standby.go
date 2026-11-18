// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"time"

	"context"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/standby"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// standbyParts are the two halves of the standby machinery: the agent that runs on
// every server and the leader's side. They are made before the task engine (the
// engine needs the task definition) and started by startStandbys.
type standbyParts struct {
	agent  *standby.Agent
	leader *standby.Leader
}

// newStandbyParts builds the agent and the leader's side and connects them to the
// rest: the registry (the replication view reads the list through it), the
// membership manager (a server that goes takes its standbys with it) and the
// settings (a new replica count is acted on at once).
func newStandbyParts(cfg config.Config, svc *cluster.Service, self string, log *slog.Logger, auditFn func(context.Context, audit.Event),
	members *membership.Manager, opsSvc *ops.Ops, reg *ha.Registry) (*standbyParts, error) {
	agent, err := standby.NewAgent(&standby.Agent{Svc: svc, Self: self, DataDir: cfg.DataDir, Log: log, Audit: auditFn,
		Grace: cfg.HA.StandbySyncGrace, SlotWarn: cfg.HA.SlotWarnBytes, SlotLimit: cfg.HA.SlotLimitBytes, PortBase: cfg.HA.StandbyPortBase})
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
	l := standby.NewLeader(&standby.Leader{
		Self: self, Sys: sys, Agent: agent, Log: log, Audit: auditFn,
		Call:        members.Client.Do,
		StartTask:   opsSvc.StartTask,
		ServiceConn: svc.ServiceConn,
		// The catalog is written on the leader, which runs the primary of the map group.
		Registrar: &standby.PGRegistrar{Log: log, Conn: func(ctx context.Context) (*pgx.Conn, error) { return svc.ServiceConn(ctx, sysdb.Database) },
			Remote: func(ctx context.Context, host string, port int) (*pgx.Conn, error) {
				return svc.RemoteConn(ctx, host, port, sysdb.Database)
			}},
		// The leader is read from the catalog with a query of its own, not through the shared
		// resolver: its cache must only ever be filled by the code that is meant to use it (a
		// server that is still joining would otherwise cache an empty answer from this loop).
		IsLeader: func(ctx context.Context) bool {
			var ok bool
			err := sys(ctx, func(c *pgx.Conn) error {
				return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE group_id = 0 AND role = 'primary' AND is_active AND host = $1)`, self).Scan(&ok)
			})
			return err == nil && ok
		},
	})
	reg.SetStandbys(agent)
	members.Standbys = l
	opsSvc.OnReplicaCount = func(ctx context.Context) (*task.Task, error) {
		eng := opsSvc.Engine()
		if eng == nil {
			l.Kick()
			return nil, nil
		}
		return l.ReconcileNow(ctx, eng.Get)
	}
	return &standbyParts{agent: agent, leader: l}, nil
}

// startStandbys registers the internal calls and starts the supervision of this
// server's standbys and, on whichever server is the leader, the reconcile loop.
func startStandbys(d *haDeps) error {
	p := d.Standby
	if p == nil {
		return nil
	}
	for _, key := range standby.Calls {
		d.Calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) {
			return p.agent.HandleCall(ctx, key, body)
		})
	}
	d.WG.Add(2)
	go func() { defer d.WG.Done(); p.agent.Run(d.BG) }()
	go func() { defer d.WG.Done(); p.leader.Run(d.BG) }()
	return nil
}

// probesOf returns the monitor probes of the standby side (none without standbys).
func probesOf(p *standbyParts) []func(context.Context, time.Time) ([]monitor.Condition, map[string]bool) {
	if p == nil {
		return nil
	}
	return []func(context.Context, time.Time) ([]monitor.Condition, map[string]bool){p.agent.Probe}
}
