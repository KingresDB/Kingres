// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconfig"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// configParts are the two halves of phase 13: the agent that runs on every server and the leader's
// side (design config-phase13.md).
type configParts struct {
	agent  *pgconfig.Agent
	leader *pgconfig.Leader
}

// newConfigParts builds the configuration agent and the leader's side. members is nil for a server
// without an internal API: the cluster is then this server alone and no call leaves the process.
func newConfigParts(svc *cluster.Service, self string, log *slog.Logger, members *membership.Manager, opsSvc *ops.Ops, hook *fault.Hook) *configParts {
	// The wall clock is the one the fault hook of the tests steps (nil hook: the real clock).
	agent := &pgconfig.Agent{Svc: svc, Self: self, Log: log, Now: clock.Stepped{Base: clock.NewReal(), Offset: hook.ClockOffset}.Wall}
	sys := func(ctx context.Context, f func(c *pgx.Conn) error) error {
		c, err := svc.ServiceConn(ctx, sysdb.Database)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
		return f(c)
	}
	l := &pgconfig.Leader{Self: self, Sys: sys, Agent: agent, Log: log, StartTask: opsSvc.StartTask,
		// Like the standby side, the leader is read from the catalog with a query of its own.
		IsLeader: func(ctx context.Context) bool {
			var ok bool
			err := sys(ctx, func(c *pgx.Conn) error {
				return c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.node WHERE group_id = 0 AND role = 'primary' AND is_active AND host = $1)`, self).Scan(&ok)
			})
			return err == nil && ok
		}}
	if members != nil {
		l.Call = members.Client.Do
	}
	return &configParts{agent: agent, leader: pgconfig.NewLeader(l)}
}

// start registers the internal calls, gives instances created before this version their include
// and starts the leader's loop that brings servers up to date.
func (p *configParts) start(d *haDeps, calls *callTable) {
	for _, key := range pgconfig.Calls {
		calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) {
			return p.agent.HandleCall(ctx, key, body)
		})
	}
	if m := d.Svc.Manager(); m != nil {
		for _, id := range m.IDs() {
			if _, err := m.EnsureConfig(id); err != nil {
				d.Log.Warn("preparing the instance for configuration failed", "instance", id, "error", err)
			}
		}
	}
	d.WG.Add(1)
	go func() { defer d.WG.Done(); p.leader.Run(d.BG) }()
}
