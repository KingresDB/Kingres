// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// leaseDirName is the directory below the data directory that holds the grants,
// the fence marker and the cached topology.
const leaseDirName = "lease"

// watchdogBinary is the name of the watchdog program.
const watchdogBinary = "kingres-watchdog"

// startLeases starts the lease manager and the grantor, and registers the
// internal calls POST /lease/grant and GET /lease/state. Without an internal
// API or a managed instance there is nothing to protect and nothing starts.
func startLeases(d *haDeps) error {
	if !d.Svc.Enabled() || d.Cfg.InternalListen == "" {
		return nil
	}
	leaseDir := filepath.Join(d.Cfg.DataDir, leaseDirName)
	clk := clock.Stepped{Base: clock.NewReal(), Offset: d.Fault.ClockOffset}
	grantor, err := lease.NewGrantor(clk, d.Cfg.HA.LeaseMargin, leaseDir)
	if err != nil {
		return fmt.Errorf("opening the lease grantor: %w", err)
	}
	// A call of its own client: the short bound of a lease round, not the ten
	// minutes of the membership client.
	pc := peerclient.New(peerclient.Config{Self: d.ServerHost, PKIDir: d.Svc.PKIDir(), Peers: d.Peers, Revoked: d.Svc.RevokedSerials,
		Timeout: d.Cfg.HA.LeaseDuration - d.Cfg.HA.LeaseMargin, Fault: d.Fault})
	d.OnCertificateInstalled(pc.ForgetAll)
	wd := &watchdogs{d: d, leaseDir: leaseDir, byInstance: map[int]*watchdogRunner{}}
	mgr, err := lease.New(lease.Config{
		Self: d.ServerHost, Clock: clk, Duration: d.Cfg.HA.LeaseDuration, Renew: d.Cfg.HA.LeaseRenew, Margin: d.Cfg.HA.LeaseMargin,
		Grantor: grantor, Voter: peerVoter{pc: pc, fault: d.Fault}, Source: catalogTopology{d: d},
		InstanceFor: func(g int32) (lease.Instance, bool) {
			li, ok := d.Svc.PrimaryOf(g)
			return localInstance{d.Svc, li.ID}, ok
		},
		// Self-fencing is enforced only for a group that has a standby: with none,
		// nothing could be promoted, and stopping the only copy would only cost
		// availability. The standby manager answers from local knowledge.
		Enforced: func(g int32) bool { return d.Fault.ForceEnforce() || d.Registry.Standbys().HasStandby(g) },
		Dir:      leaseDir, WatchdogFileFor: wd.leaseFile, EnsureWatchdogFor: wd.ensure, Log: d.Log,
		Audit: func(ctx context.Context, action string, group int32, detail string) {
			d.Audit(ctx, audit.Event{Actor: "system", Source: "system", Action: action, ResourceType: "shardGroup", ResourceID: fmt.Sprint(group),
				Outcome: audit.OutcomeSuccess, Details: map[string]any{"detail": detail}})
		},
	})
	if err != nil {
		return err
	}
	d.Registry.SetLeases(mgr)
	d.LeaseMgr = mgr

	d.Calls.Register("POST /lease/grant", func(_ context.Context, peer string, body []byte) (any, error) {
		if _, ok := d.Peers.Get(peer); !ok {
			return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "not_member", Message: "The caller is not a server of this cluster."}
		}
		var req lease.Request
		if err := json.Unmarshal(body, &req); err != nil || req.DurationMs <= 0 || req.DurationMs > int64(time.Minute/time.Millisecond) {
			return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "A group and a duration of at most one minute are required."}
		}
		dur := time.Duration(req.DurationMs) * time.Millisecond
		var rep lease.Reply
		var err error
		switch {
		case req.Release:
			rep, err = grantor.Release(req.Group, peer)
		case req.TransferTo != "":
			if _, ok := d.Peers.Get(req.TransferTo); !ok {
				return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "The server to hand the lease to is not a server of this cluster."}
			}
			rep, err = grantor.Transfer(req.Group, peer, req.TransferTo, dur)
		default:
			rep, err = grantor.Grant(req.Group, peer, dur, req.DryRun)
		}
		if err != nil {
			// The grant could not be recorded: it is not granted.
			d.Log.Error("recording a lease grant", "error", err)
			return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The grant could not be recorded."}
		}
		return rep, nil
	})
	d.Calls.Register("GET /lease/state", func(context.Context, string, []byte) (any, error) {
		return leaseState{Server: d.ServerHost, Grants: grantor.Snapshot(), Leases: mgr.Info(),
			DurationMs: d.Cfg.HA.LeaseDuration.Milliseconds(), MarginMs: grantor.Margin().Milliseconds()}, nil
	})

	d.WG.Add(1)
	go func() { defer d.WG.Done(); mgr.Run(d.BG) }()
	return nil
}

// leaseState is the answer of GET /lease/state.
type leaseState struct {
	Server string         `json:"server"`
	Grants []lease.Entry  `json:"grants"`
	Leases []ha.LeaseInfo `json:"leases"`
	// DurationMs and MarginMs are the lease duration and the margin of the answering server: the
	// safety argument needs them equal on every server, so a failover compares them first.
	DurationMs int64 `json:"durationMs"`
	MarginMs   int64 `json:"marginMs"`
}

// peerVoter asks another server's grantor over the internal API.
type peerVoter struct {
	pc    *peerclient.Client
	fault *fault.Hook
}

func (v peerVoter) Grant(ctx context.Context, server string, req lease.Request) (lease.Reply, error) {
	if d := v.fault.GrantDelay(); d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
		}
	}
	var r lease.Reply
	err := v.pc.Do(ctx, server, http.MethodPost, "/lease/grant", req, &r)
	return r, err
}

// localInstance is one primary instance of this server (the directory id is what the
// role files say serves the group).
type localInstance struct {
	svc *cluster.Service
	id  int
}

func (l localInstance) Running(ctx context.Context) (bool, error) {
	return l.svc.Manager().Running(ctx, l.id)
}
func (l localInstance) Start(ctx context.Context) error { return l.svc.Manager().Start(ctx, l.id) }
func (l localInstance) StopImmediate(ctx context.Context) error {
	return l.svc.Manager().StopImmediate(ctx, l.id)
}

// catalogTopology reads which group this server runs the primary of, and the
// servers of the cluster, from the local copy of the catalog. The majority is
// counted over every distinct host of kingres.node, active or not: a server that
// is down still counts as a member, which is what keeps two partitions from both
// having a majority. A server that is being added is not in the catalog until its
// registration step, and one that is being removed leaves it at the end; for
// the one-server step of a change the majorities of the old and the new set
// always intersect (the sets differ by one server).
type catalogTopology struct{ d *haDeps }

func (c catalogTopology) Topology(ctx context.Context) (lease.Topology, error) {
	if c.d.Node.State() != setup.Clustered {
		return lease.Topology{}, nil
	}
	conn, err := c.d.Svc.SystemConn(ctx)
	if err != nil {
		return lease.Topology{}, fmt.Errorf("reading the catalog: %w", err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	rows, err := conn.Query(ctx, `SELECT host, port, group_id, role FROM kingres.node`)
	if err != nil {
		return lease.Topology{}, fmt.Errorf("reading kingres.node: %w", err)
	}
	defer rows.Close()
	hosts := map[string]bool{}
	var catalog []cluster.CatalogRow
	for rows.Next() {
		var r cluster.CatalogRow
		if err := rows.Scan(&r.Host, &r.Port, &r.Group, &r.Role); err != nil {
			return lease.Topology{}, fmt.Errorf("reading kingres.node: %w", err)
		}
		hosts[r.Host] = true
		catalog = append(catalog, r)
	}
	if err := rows.Err(); err != nil {
		return lease.Topology{}, fmt.Errorf("reading kingres.node: %w", err)
	}
	// The role files follow the catalog (a directory made before they existed gets one, an instance whose
	// group was not known gets it), and the groups this server holds the lease of are the catalog's
	// primaries on a port of one of its primary instances.
	c.d.Svc.SyncRoles(c.d.ServerHost, catalog)
	var groups []int32
	for _, r := range catalog {
		if r.Host != c.d.ServerHost || r.Role != "primary" {
			continue
		}
		for _, li := range c.d.Svc.WritableInstances() {
			if li.Port == r.Port {
				groups = append(groups, r.Group)
			}
		}
	}
	t := lease.Topology{Groups: groups}
	for h := range hosts {
		t.Members = append(t.Members, h)
	}
	sort.Strings(t.Members)
	sort.Slice(t.Groups, func(i, j int) bool { return t.Groups[i] < t.Groups[j] })
	return t, nil
}

// watchdogRunner starts the watchdog next to the instance. The watchdog is
// detached (its own session): it must outlive kingresd, and a signal to
// kingresd's process group must not reach it, because it exists for the case
// that kingresd is gone. Only one runs per instance (the program takes a lock).
type watchdogRunner struct {
	d        *haDeps
	leaseDir string
	instDir  string
	group    int32
	instance int

	mu     sync.Mutex
	exited chan struct{}
	warned bool
	proc   *os.Process
}

func (w *watchdogRunner) binary() string {
	var dirs []string
	if w.d.Cfg.PGBinDir != "" {
		dirs = append(dirs, w.d.Cfg.PGBinDir)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, dir := range dirs {
		p := filepath.Join(dir, watchdogBinary)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

func (w *watchdogRunner) ensure() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.exited != nil {
		select {
		case <-w.exited:
		default:
			return // our own watchdog still runs
		}
	}
	bin := w.binary()
	binDir, found, err := cluster.ResolveBinDir(w.d.Cfg.PGBinDir)
	if bin == "" || err != nil || !found {
		if !w.warned {
			w.warned = true
			w.d.Log.Warn("the lease watchdog program was not found: a hung or dead kingresd cannot be covered", "program", watchdogBinary)
		}
		return
	}
	logf, err := os.OpenFile(filepath.Join(w.leaseDir, "watchdog.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		w.d.Log.Warn("opening the watchdog log", "error", err)
		return
	}
	cmd := exec.CommandContext(context.Background(), bin, //nolint:gosec // our own program, arguments are paths of this server
		"-lease-file", filepath.Join(w.instDir, "lease.json"), "-data", filepath.Join(w.instDir, "pgdata"),
		"-pg-ctl", filepath.Join(binDir, "pg_ctl"), "-marker", lease.FencedPath(w.leaseDir, w.group),
		"-lock", filepath.Join(w.leaseDir, "watchdog."+strconv.Itoa(w.instance)+".lock"), "-standdown", lease.StandDownPath(w.leaseDir, w.instance))
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		w.d.Log.Warn("starting the lease watchdog", "error", err)
		return
	}
	exited := make(chan struct{})
	w.exited = exited
	w.proc = cmd.Process
	go func() { _ = cmd.Wait(); _ = logf.Close(); close(exited) }()
}

// watchdogs keeps one watchdog per primary instance of this server: each has its own
// lease file, its own fence marker (the marker of its group), its own lock and its own
// stand-down file, so that the lease of one group never stops or releases the
// instance of another.
type watchdogs struct {
	d          *haDeps
	leaseDir   string
	mu         sync.Mutex
	byInstance map[int]*watchdogRunner
}

// leaseFile is the file the watchdog of the group's primary instance reads; empty when this server
// runs no primary of the group.
func (w *watchdogs) leaseFile(group int32) string {
	li, ok := w.d.Svc.PrimaryOf(group)
	if !ok {
		return ""
	}
	dir, err := w.d.Svc.Manager().Dir(li.ID)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "lease.json")
}

func (w *watchdogs) ensure(group int32) {
	li, ok := w.d.Svc.PrimaryOf(group)
	if !ok {
		return
	}
	dir, err := w.d.Svc.Manager().Dir(li.ID)
	if err != nil {
		return
	}
	w.mu.Lock()
	r := w.byInstance[li.ID]
	if r != nil && r.group != group {
		r.kill()
	}
	if r == nil || r.group != group {
		r = &watchdogRunner{d: w.d, leaseDir: w.leaseDir, instDir: dir, group: group, instance: li.ID}
		w.byInstance[li.ID] = r
	}
	w.mu.Unlock()
	r.ensure()
}

// kill ends the watchdog process this runner started, and waits a little for its lock to be released. It is for a
// watchdog that was started for another group than the instance serves now: its fence marker is the old group's, so
// it would write the wrong marker (FO-16). The new runner then starts one with the right marker.
func (w *watchdogRunner) kill() {
	w.mu.Lock()
	proc, exited := w.proc, w.exited
	w.mu.Unlock()
	if proc == nil || exited == nil {
		return
	}
	_ = proc.Kill()
	select {
	case <-exited:
	case <-time.After(2 * time.Second):
	}
}
