// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package main

import (
	"context"
	"log/slog"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
)

// repairPorts reads the cluster for the loop that undoes what a switchover left held (failover.Repair, review F4-02). It runs on every
// server and acts only on the manager leader.
type repairPorts struct {
	mf      *mapFailover
	fos     *failover.Service
	leaders *leader.Resolver
	self    string
	log     *slog.Logger
}

func newSwitchoverRepair(mf *mapFailover, fos *failover.Service, leaders *leader.Resolver, self string, log *slog.Logger) *failover.Repair {
	return &failover.Repair{P: &repairPorts{mf: mf, fos: fos, leaders: leaders, self: self, log: log}}
}

func (p *repairPorts) Log(msg string, kv ...any) { p.log.Info(msg, kv...) }

func (p *repairPorts) Primaries(ctx context.Context) (map[int32]string, error) {
	if id, err := p.leaders.Get(ctx); err != nil || id != p.self {
		return nil, nil // not the leader: nothing is repaired from here
	}
	nodes, err := p.mf.cat.Nodes(ctx, failover.SystemDB)
	if err != nil {
		return nil, err
	}
	out := map[int32]string{}
	for _, n := range nodes {
		if n.Role == "primary" && n.Group != 0 {
			out[n.Group] = n.Host
		}
	}
	return out, nil
}

func (p *repairPorts) State(ctx context.Context, server string, group int32) (failover.State, error) {
	return p.mf.peers.State(ctx, server, group)
}

func (p *repairPorts) Term(ctx context.Context) (int64, error) {
	term, _, err := p.mf.cat.Status(ctx, failover.SystemDB)
	return term, err
}

func (p *repairPorts) Resume(ctx context.Context, server string, group int32, term int64) error {
	return p.mf.peers.Resume(ctx, server, failover.ResumeRequest{Group: group, ExpectedTerm: term})
}

func (p *repairPorts) Unquiesce(ctx context.Context, server string, group int32, term int64) error {
	return p.mf.peers.Unquiesce(ctx, server, failover.QuiesceRequest{Group: group, ExpectedTerm: term})
}

func (p *repairPorts) Unfinished(ctx context.Context) (bool, error) { return p.fos.Unfinished(ctx) }
