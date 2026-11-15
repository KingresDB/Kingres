// SPDX-License-Identifier: PostgreSQL

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
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
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
	instDir, err := d.Svc.Manager().Dir(cluster.NodeID)
	if err != nil {
		return err
	}
	wd := &watchdogRunner{d: d, leaseDir: leaseDir, instDir: instDir}
	mgr, err := lease.New(lease.Config{
		Self: d.ServerHost, Clock: clk, Duration: d.Cfg.HA.LeaseDuration, Renew: d.Cfg.HA.LeaseRenew, Margin: d.Cfg.HA.LeaseMargin,
		Grantor: grantor, Voter: peerVoter{pc}, Source: catalogTopology{d: d}, Instance: localInstance{d.Svc},
		// Self-fencing is enforced only for a group that has a standby: with none,
		// nothing could be promoted, and stopping the only copy would only cost
		// availability. The standby manager answers from local knowledge.
		Enforced: func(g int32) bool { return d.Fault.ForceEnforce() || d.Registry.Standbys().HasStandby(g) },
		Dir:      leaseDir, WatchdogFile: filepath.Join(instDir, "lease.json"), EnsureWatchdog: wd.ensure, Log: d.Log,
		Audit: func(ctx context.Context, action string, group int32, detail string) {
			d.Audit(ctx, audit.Event{Actor: "system", Source: "system", Action: action, ResourceType: "shardGroup", ResourceID: fmt.Sprint(group),
				Outcome: audit.OutcomeSuccess, Details: map[string]any{"detail": detail}})
		},
	})
	if err != nil {
		return err
	}
	d.Registry.SetLeases(mgr)

	d.Calls.Register("POST /lease/grant", func(_ context.Context, peer string, body []byte) (any, error) {
		if _, ok := d.Peers.Get(peer); !ok {
			return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "not_member", Message: "The caller is not a server of this cluster."}
		}
		var req lease.Request
		if err := json.Unmarshal(body, &req); err != nil || req.DurationMs <= 0 || req.DurationMs > int64(time.Minute/time.Millisecond) {
			return nil, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "A group and a duration of at most one minute are required."}
		}
		rep, err := grantor.Grant(req.Group, peer, time.Duration(req.DurationMs)*time.Millisecond, req.DryRun)
		if err != nil {
			// The grant could not be recorded: it is not granted.
			d.Log.Error("recording a lease grant", "error", err)
			return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The grant could not be recorded."}
		}
		return rep, nil
	})
	d.Calls.Register("GET /lease/state", func(context.Context, string, []byte) (any, error) {
		return leaseState{Server: d.ServerHost, Grants: grantor.Snapshot(), Leases: mgr.Info()}, nil
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
}

// peerVoter asks another server's grantor over the internal API.
type peerVoter struct{ pc *peerclient.Client }

func (v peerVoter) Grant(ctx context.Context, server string, req lease.Request) (lease.Reply, error) {
	var r lease.Reply
	err := v.pc.Do(ctx, server, http.MethodPost, "/lease/grant", req, &r)
	return r, err
}

// localInstance is the primary instance of this server.
type localInstance struct{ svc *cluster.Service }

func (l localInstance) Running(ctx context.Context) (bool, error) {
	return l.svc.Manager().Running(ctx, cluster.NodeID)
}
func (l localInstance) Start(ctx context.Context) error {
	return l.svc.Manager().Start(ctx, cluster.NodeID)
}
func (l localInstance) StopImmediate(ctx context.Context) error {
	return l.svc.Manager().StopImmediate(ctx, cluster.NodeID)
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
	conn, err := c.d.Svc.ServiceConn(ctx, sysdb.Database)
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
	var groups []int32
	for rows.Next() {
		var host, role string
		var port int
		var group int32
		if err := rows.Scan(&host, &port, &group, &role); err != nil {
			return lease.Topology{}, fmt.Errorf("reading kingres.node: %w", err)
		}
		hosts[host] = true
		if host == c.d.ServerHost && port == c.d.Cfg.PGPort && role == "primary" {
			groups = append(groups, group)
		}
	}
	if err := rows.Err(); err != nil {
		return lease.Topology{}, fmt.Errorf("reading kingres.node: %w", err)
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

	mu     sync.Mutex
	exited chan struct{}
	warned bool
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
		"-pg-ctl", filepath.Join(binDir, "pg_ctl"), "-marker", filepath.Join(w.leaseDir, lease.FencedMarker),
		"-lock", filepath.Join(w.leaseDir, "watchdog.lock"), "-standdown", filepath.Join(w.leaseDir, lease.StandDownFile))
	cmd.Stdout, cmd.Stderr = logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		_ = logf.Close()
		w.d.Log.Warn("starting the lease watchdog", "error", err)
		return
	}
	exited := make(chan struct{})
	w.exited = exited
	go func() { _ = cmd.Wait(); _ = logf.Close(); close(exited) }()
}
