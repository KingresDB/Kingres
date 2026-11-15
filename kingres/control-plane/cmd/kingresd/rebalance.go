// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/rebalance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// auditFn returns the function that writes an audit event; a failure is logged.
func auditFn(sink interface {
	Append(ctx context.Context, ev audit.Event) (int64, error)
}, log *slog.Logger) func(context.Context, audit.Event) {
	return func(ctx context.Context, ev audit.Event) {
		if _, err := sink.Append(ctx, ev); err != nil {
			log.Error("writing the audit log", "error", err)
		}
	}
}

// newRebalance builds the rebalance service of the manager leader: the extension's
// functions run on this server's own instance (the leader runs the primary of the
// map group), the task runs in the engine, and the conditions that a move checks
// come from the lease registry and the servers' reported disk usage.
func newRebalance(svc *cluster.Service, o *ops.Ops, st store.Store, reg *ha.Registry, log *slog.Logger,
	auditFn func(context.Context, audit.Event)) *rebalance.Service {
	pgb := &rebalance.PG{
		Conns: svc, Log: log,
		Leases: func(group int32) string {
			for _, li := range reg.Leases().Info() {
				if li.Group == group {
					return li.State
				}
			}
			return ""
		},
		// A server that does not report its disk (the fields are null) is not checked.
		Disk: func(ctx context.Context, server string) (*int64, *int64) {
			d, err := st.Server(ctx, server)
			if err != nil {
				return nil, nil
			}
			return d.Resources.DiskUsedBytes, d.Resources.DiskTotalBytes
		},
	}
	return &rebalance.Service{
		Conns: svc, Backend: pgb, Engine: o.Engine, Log: log, Audit: auditFn,
		Background: func(f func()) { o.Go(f) },
		BGFn:       o.Background,
		Summary: func(ctx context.Context) (bool, []string, error) {
			sum, err := st.ClusterSummary(ctx)
			return sum.RebalanceNeeded, sum.RebalanceReasons, err
		},
	}
}
