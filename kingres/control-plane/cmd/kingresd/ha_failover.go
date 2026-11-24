// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/standby"
)

// The read calls of the failover (design ha-phase5-part2.md, 3.1.2) and the calls of
// the freeze. None of them changes the state of an instance; the freeze and the
// "gone" set are in the memory of the standby agent and lapse by themselves.

const (
	callPosition = "GET /failover/position"
	callLiveness = "GET /liveness"
	callState    = "GET /failover/state"
)

// startFailoverCalls registers the calls. They are answered also while the feature
// that backs them is not running, with what is known (the call table's not-started
// answer otherwise).
func startFailoverCalls(d *haDeps) error {
	if d.Calls == nil || !d.Svc.Enabled() {
		return nil
	}
	d.Calls.Register(callPosition, func(ctx context.Context, _ string, body []byte) (any, error) {
		g, err := groupOfQuery(body)
		if err != nil {
			return nil, err
		}
		return failoverPosition(ctx, d, g)
	})
	d.Calls.Register(callLiveness, func(context.Context, string, []byte) (any, error) {
		return livenessView(d), nil
	})
	d.Calls.Register("GET /catalog/nodes", func(ctx context.Context, _ string, _ []byte) (any, error) {
		return catalogNodes(ctx, d)
	})
	d.Calls.Register(callState, func(ctx context.Context, _ string, body []byte) (any, error) {
		g, err := groupOfQuery(body)
		if err != nil {
			return nil, err
		}
		return failoverState(ctx, d, g), nil
	})
	registerFailoverCallee(d)
	if d.Standby != nil {
		for _, key := range standby.FreezeCalls {
			d.Calls.Register(key, func(ctx context.Context, _ string, body []byte) (any, error) {
				return d.Standby.agent.HandleCall(ctx, key, body)
			})
		}
	}
	return nil
}

// groupOfQuery reads group=<n> from the query string a GET call carries as its body.
func groupOfQuery(body []byte) (int32, error) {
	q, err := url.ParseQuery(string(body))
	if err != nil {
		return 0, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "The query is not valid."}
	}
	n, err := strconv.ParseInt(q.Get("group"), 10, 32)
	if err != nil || n < 0 {
		return 0, &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: "A group is required."}
	}
	return int32(n), nil
}

// PositionAnswer is the answer of GET /failover/position: what the instance of
// this server that serves a group knows of its WAL, read-only. The leader reads it
// from every standby of a group after the old primary was fenced, to choose the
// standby that has the most.
type PositionAnswer struct {
	Server           string `json:"server"`
	Group            int32  `json:"group"`
	InstanceID       int    `json:"instanceId"`
	Role             string `json:"role"`
	InRecovery       bool   `json:"inRecovery"`
	Timeline         int64  `json:"timeline"`
	ReceiveLsn       string `json:"receiveLsn"`
	ReplayLsn        string `json:"replayLsn"`
	ReceiverState    string `json:"receiverState"`
	SystemIdentifier string `json:"systemIdentifier"`
	CatalogTerm      *int64 `json:"catalogTerm"`
}

func notFound(msg string) error {
	return &internalapi.CallError{Status: http.StatusNotFound, Code: "no_instance", Message: msg}
}

// instanceOfGroup is the instance of this server that serves the group: the
// standby of it in preference (the leader asks for standbys), else the primary.
func instanceOfGroup(d *haDeps, group int32) (cluster.LocalInstance, bool) {
	var found *cluster.LocalInstance
	for _, li := range d.Svc.LocalInstances() {
		li := li
		if li.Role.Group != group {
			continue
		}
		if found == nil || (li.Role.Role == instance.RoleStandby && found.Role.Role != instance.RoleStandby) {
			found = &li
		}
	}
	if found == nil {
		return cluster.LocalInstance{}, false
	}
	return *found, true
}

func failoverPosition(ctx context.Context, d *haDeps, group int32) (any, error) {
	li, ok := instanceOfGroup(d, group)
	if !ok {
		return nil, notFound("This server runs no instance of the shard group.")
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, "postgres")
	if err != nil {
		d.Log.Warn("failover: the position cannot be read: the instance cannot be reached", "group", group, "instance", li.ID, "error", err)
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	out := PositionAnswer{Server: d.ServerHost, Group: group, InstanceID: li.ID, Role: li.Role.Role}
	err = c.QueryRow(ctx, `SELECT pg_is_in_recovery(),
			coalesce((SELECT received_tli FROM pg_stat_wal_receiver LIMIT 1), (SELECT timeline_id FROM pg_control_checkpoint()))::bigint,
			coalesce(CASE WHEN pg_is_in_recovery() THEN pg_last_wal_receive_lsn() ELSE pg_current_wal_flush_lsn() END::text, ''),
			coalesce(pg_last_wal_replay_lsn()::text, ''),
			coalesce((SELECT status FROM pg_stat_wal_receiver LIMIT 1), ''),
			(SELECT system_identifier::text FROM pg_control_system())`).
		Scan(&out.InRecovery, &out.Timeline, &out.ReceiveLsn, &out.ReplayLsn, &out.ReceiverState, &out.SystemIdentifier)
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The position cannot be read."}
	}
	out.CatalogTerm = catalogTerm(ctx, d, li.ID)
	return out, nil
}

// catalogTerm reads the catalog term the instance holds; nil when it has no
// catalog or it cannot be read.
func catalogTerm(ctx context.Context, d *haDeps, id int) *int64 {
	c, err := d.Svc.InstanceConn(ctx, id, "kingres_system")
	if err != nil {
		return nil
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var t int64
	if err := c.QueryRow(ctx, `SELECT term FROM kingres.catalog_status`).Scan(&t); err != nil {
		return nil
	}
	return &t
}

// LivenessAnswer is the answer of GET /liveness: this server's own view of the
// heartbeats. A failover asks every server for it to corroborate that a server is
// dead before it acts on it.
type LivenessAnswer struct {
	Server string         `json:"server"`
	Peers  []LivenessPeer `json:"peers"`
}

// LivenessPeer is the state of one server as the answering server sees it.
type LivenessPeer struct {
	Server    string `json:"server"`
	State     string `json:"state"`
	SilenceMs *int64 `json:"silenceMs"`
}

func livenessView(d *haDeps) LivenessAnswer {
	out := LivenessAnswer{Server: d.ServerHost, Peers: []LivenessPeer{}}
	lv := d.Registry.Liveness()
	if d.Peers == nil {
		return out
	}
	for _, p := range d.Peers.All() {
		if p.ServerID == d.ServerHost {
			continue
		}
		lp := LivenessPeer{Server: p.ServerID, State: peerStateName(lv.State(p.ServerID))}
		if s, ok := lv.Silence(p.ServerID); ok {
			ms := s.Milliseconds()
			lp.SilenceMs = &ms
		}
		out.Peers = append(out.Peers, lp)
	}
	return out
}

func peerStateName(s ha.PeerState) string {
	switch s {
	case ha.PeerLive:
		return "live"
	case ha.PeerSuspect:
		return "suspect"
	case ha.PeerDead:
		return "dead"
	}
	return "unknown"
}

// StateInstance is one instance of this server in the answer of GET /failover/state.
type StateInstance struct {
	ID      int    `json:"id"`
	Port    int    `json:"port"`
	Role    string `json:"role"`
	Origin  string `json:"origin"`
	Group   int32  `json:"group"`
	Term    int64  `json:"term"`
	Running bool   `json:"running"`
	// RunningUnknown: the instance manager could not say whether the instance runs; Running is false then and means nothing.
	RunningUnknown bool  `json:"runningUnknown,omitempty"`
	InRecovery     *bool `json:"inRecovery"`
}

// StateAnswer is the answer of GET /failover/state: this server's knowledge of one
// group. A task that is not sure what its last call did asks it, and never trusts
// its own memory.
type StateAnswer struct {
	Server       string          `json:"server"`
	Group        int32           `json:"group"`
	Instances    []StateInstance `json:"instances"`
	Holds        bool            `json:"holds"`
	LeaseState   string          `json:"leaseState"`
	Adopted      string          `json:"adopted,omitempty"`
	Attached     bool            `json:"attached,omitempty"`
	Relinquished bool            `json:"relinquished"`
	Quiesced     bool            `json:"quiesced"`
	Frozen       bool            `json:"frozen"`
	CatalogTerm  *int64          `json:"catalogTerm"`
	Prepared     *int            `json:"preparedTransactions"`
	Destination  string          `json:"destination,omitempty"`
	// Watchdog says whether a watchdog of the group's primary instance runs here (nil: this server runs no primary of the
	// group, or does not know). A failover reads it before it counts the primary as fenceable by a lease alone.
	Watchdog *bool `json:"watchdog"`
	// Enforced: the lease manager of this server stops a primary of the group whose lease lapsed (it knows of a standby).
	Enforced *bool `json:"enforced"`
}

func failoverState(ctx context.Context, d *haDeps, group int32) StateAnswer {
	out := StateAnswer{Server: d.ServerHost, Group: group, Instances: []StateInstance{}, LeaseState: "none"}
	var serving *cluster.LocalInstance
	for _, li := range d.Svc.LocalInstances() {
		li := li
		si := StateInstance{ID: li.ID, Port: li.Port, Role: li.Role.Role, Origin: li.Role.Origin, Group: li.Role.Group, Term: li.Role.Term}
		var rerr error
		if si.Running, rerr = d.Svc.Manager().Running(ctx, li.ID); rerr != nil {
			si.Running, si.RunningUnknown = false, true
		}
		if si.Running && li.Role.Group == group {
			if c, err := d.Svc.InstanceConn(ctx, li.ID, "postgres"); err == nil {
				var rec bool
				if c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&rec) == nil {
					si.InRecovery = &rec
				}
				_ = c.Close(context.WithoutCancel(ctx))
			}
		}
		if li.Role.Group == group && (serving == nil || li.Role.Writable()) {
			serving = &li
		}
		out.Instances = append(out.Instances, si)
	}
	out.Holds = d.Registry.Leases().Holds(group)
	for _, l := range d.Registry.Leases().Info() {
		if l.Group == group && l.Holder == d.ServerHost {
			out.LeaseState = l.State
		}
	}
	if d.LeaseMgr != nil {
		out.Adopted, out.Attached, _ = d.LeaseMgr.Adopted(group)
		out.Relinquished = d.LeaseMgr.Relinquished(group)
	}
	out.Quiesced = quiescedNow(d, group)
	if d.Standby != nil {
		for _, g := range d.Standby.agent.FrozenGroups() {
			if g == group {
				out.Frozen = true
			}
		}
	}
	if serving != nil && serving.Role.Writable() && d.Watchdogs != nil {
		w := d.Watchdogs.running(group)
		out.Watchdog = &w
	}
	if serving != nil && serving.Role.Writable() {
		e := d.Fault.ForceEnforce() || d.Registry.Standbys().HasStandby(group)
		out.Enforced = &e
	}
	if serving != nil {
		out.CatalogTerm = catalogTerm(ctx, d, serving.ID)
		if serving.Role.Writable() {
			if c, err := d.Svc.InstanceConn(ctx, serving.ID, "postgres"); err == nil {
				var n int
				if c.QueryRow(ctx, `SELECT count(*) FROM pg_prepared_xacts WHERE gid LIKE 'kingres\_h%'`).Scan(&n) == nil {
					out.Prepared = &n
				}
				_ = c.Close(context.WithoutCancel(ctx))
			}
		}
	}
	if d.EntryDestination != nil {
		out.Destination = d.EntryDestination(ctx)
	}
	return out
}

var _ = fmt.Sprint
