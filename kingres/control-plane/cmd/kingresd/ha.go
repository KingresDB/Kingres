// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"log/slog"
	"sync"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/config"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/leader"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/membership"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peers"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// haDeps is what the features of high availability need from main. Each feature
// has one start function in its own file (ha_liveness.go, ha_leases.go,
// ha_standby.go, ha_proxy.go); startHA calls them in dependency order. A feature
// that has nothing to do (no internal API, no managed instance) returns nil
// without starting anything.
type haDeps struct {
	Cfg        config.Config
	Log        *slog.Logger
	Svc        *cluster.Service
	Node       *setup.Manager
	ServerHost string
	// PC calls the other servers (nil without an internal API).
	PC      *peerclient.Client
	Peers   *peers.Directory
	Leaders *leader.Resolver
	Members *membership.Manager
	Ops     *ops.Ops
	Ring    *monitor.Ring
	// Registry connects the features to each other and to the store.
	Registry *ha.Registry
	// OnCertificateInstalled registers a function run after this server's certificate
	// was replaced (drop cached connections that were opened with the old one).
	OnCertificateInstalled func(func())
	// LastLeader remembers the leader the catalog named last.
	LastLeader *leaderMemory
	// Fault is the fault-injection hook of the tests (nil in production); a feature
	// that makes its own peer client passes it on in peerclient.Config.
	Fault *fault.Hook
	// Calls is where a feature registers the internal calls it answers.
	Calls *callTable
	// Audit appends an event to the audit log of this server (every state-changing
	// operation of a feature is audited through it).
	Audit func(ctx context.Context, ev audit.Event)
	// Standby is the standby machinery (nil without an internal API).
	Standby *standbyParts
	// LeaseMgr is the lease manager (nil until startLeases ran); EntryDestination says where the
	// client entry point sends a new connection now (nil without an entry point).
	LeaseMgr *lease.Manager
	// MapFailover is the server's part of the failover of the map group (nil without an internal API).
	MapFailover *mapFailover
	// Watchdogs are the watchdogs of the primary instances of this server (nil until startLeases ran).
	Watchdogs        *watchdogs
	EntryDestination func(ctx context.Context) string
	// BG is the context of background work; background goroutines are added to WG
	// and must end when BG ends.
	BG context.Context
	WG *sync.WaitGroup
}

// startHA starts the features of high availability in dependency order:
// liveness first (leases and standbys read it), then leases, standbys and the
// client entry point.
func startHA(d *haDeps) error {
	for _, f := range []func(*haDeps) error{startLiveness, startLeases, startStandbys, startClientProxy, startFailoverCalls} {
		if err := f(d); err != nil {
			return err
		}
	}
	return nil
}

// hookList is a list of functions run when this server's certificate was
// replaced: a feature that keeps its own peer client registers its ForgetAll here.
type hookList struct {
	mu sync.Mutex
	fs []func()
}

func (h *hookList) add(f func()) { h.mu.Lock(); h.fs = append(h.fs, f); h.mu.Unlock() }

func (h *hookList) run() {
	h.mu.Lock()
	fs := append([]func(){}, h.fs...)
	h.mu.Unlock()
	for _, f := range fs {
		f()
	}
}
