// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The old leader of a planned switchover of the map group is held back from starting from the clean stop on: its primary is the system database, so while it is
// held back no task can be read or written, and nothing that runs from the task engine can end the hold when the switchover is given up. These calls and the
// loop below end it (F6-01): the candidate asks the old leader to resolve its hold when it gives its bootstrap up, an operator can ask every server to do so, and
// the old leader asks the servers that host a standby, by itself, from the time its kingresd starts.
const (
	callMapBootstrap = "GET /failover/map/bootstrap"
	callMapResume    = "POST /failover/map/resume"
)

// mapBootstrapStatus is what a server answers about the bootstrap of the map group that names the asking server as the old leader.
type mapBootstrapStatus struct {
	// Active says that a bootstrap of this server names the old server and has neither finished nor been given up.
	Active bool `json:"active"`
	// Standby is the state of the standby of the map group that this server hosts: "in_recovery" (it is a standby, shown by the instance or by its control data),
	// "primary" (it was promoted), "unknown" (it cannot be read) or "none" (this server hosts none).
	Standby string `json:"standby"`
}

// mapResumeAnswer is the answer of a server that was asked to resolve the hold of its primary of the map group.
type mapResumeAnswer struct {
	Resumed bool   `json:"resumed"`
	Detail  string `json:"detail,omitempty"`
}

// recoveryFromControlData reads the state of a standby that does not run from the "Database cluster state" of pg_controldata: a standby that was stopped
// says "in archive recovery" (or "shut down in recovery" after a clean stop); a primary that was stopped says "shut down" and a running one "in production".
// Any other state (a crash, a shutdown in progress) is not an answer.
func recoveryFromControlData(state string) (bool, error) {
	switch state {
	case "in archive recovery", "shut down in recovery":
		return true, nil
	case "in production", "shut down":
		return false, nil
	}
	return false, fmt.Errorf("the control data of the instance say %q, which does not tell a standby from a primary", state)
}

// instanceInRecovery asks the instance whether it is in recovery; when it cannot be reached (it is down) its control data answer (F6-01): an unreachable standby
// is not a promoted one, and reading it as "may have promoted" would leave the old leader held back for ever.
func (m *mapFailover) instanceInRecovery(ctx context.Context, id int) (bool, error) {
	rec, qerr := m.probeInRecovery(ctx, id)
	if qerr == nil {
		return rec, nil
	}
	cd, err := m.controlData(ctx, id)
	if err != nil {
		return false, qerr
	}
	rec, err = recoveryFromControlData(cd["Database cluster state"])
	if err != nil {
		return false, fmt.Errorf("%v; %w", qerr, err)
	}
	return rec, nil
}

// handleMapBootstrap answers the old leader's question: does a bootstrap that names it as the old leader run here, and what is the standby of the map group that
// this server hosts?
func handleMapBootstrap(ctx context.Context, d *haDeps, caller string, body []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	q, err := url.ParseQuery(string(body))
	if err != nil || q.Get("old") == "" || len(q.Get("old")) > 255 {
		return nil, badBody("a query that names the old server")
	}
	if old := q.Get("old"); old != caller {
		return nil, &internalapi.CallError{Status: http.StatusForbidden, Code: "forbidden", Message: "A server asks about the bootstrap that names itself."}
	}
	out := mapBootstrapStatus{Standby: mf.localStandbyState(ctx)}
	if cur, ok := mf.journal.Current(); ok && mf.journal.Active() && cur.OldServer == caller {
		out.Active = true
	}
	return out, nil
}

// localStandbyState is the state of the instances of the map group that are not the primary this server holds.
func (m *mapFailover) localStandbyState(ctx context.Context) string {
	d := m.fd.d
	if d == nil || d.Svc == nil {
		return "none"
	}
	state := "none"
	for _, li := range d.Svc.LocalInstances() {
		if li.Role.Group != 0 {
			continue
		}
		rec, err := m.instanceInRecovery(ctx, li.ID)
		switch {
		case err != nil:
			if state == "none" {
				state = "unknown"
			}
		case rec:
			if state != "primary" {
				state = "in_recovery"
			}
		default:
			state = "primary"
		}
	}
	return state
}

// handleMapResume asks this server to resolve the hold of its primary of the map group. The caller is any member: the callee proves what it needs itself.
func handleMapResume(ctx context.Context, d *haDeps, _ string, _ []byte) (any, error) {
	mf := d.MapFailover
	if mf == nil {
		return nil, notStarted
	}
	resumed, detail := mf.resolveHeldBack(ctx)
	return mapResumeAnswer{Resumed: resumed, Detail: detail}, nil
}

// resolveHeldBack ends the hold of this server's primary of the map group when nothing owns it any more: the primary is held back for a hand-over (the marker of
// a planned switchover), no bootstrap that names this server as the old leader is active on any server, no standby of the map group was promoted, and a standby of
// the map group is shown to be in recovery. The lease stays the final authority for who may run: the primary starts again only when this server wins it.
func (m *mapFailover) resolveHeldBack(ctx context.Context) (bool, string) {
	if !m.isHeldBack() {
		return false, "the primary of the map group is not held back for a hand-over on this server"
	}
	members, err := m.members(ctx)
	if err != nil {
		return false, "the servers of the cluster cannot be read: " + err.Error()
	}
	proven := 0
	for _, s := range members {
		if s == m.self {
			continue
		}
		var st mapBootstrapStatus
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := m.call(cctx, s, http.MethodGet, "/failover/map/bootstrap?"+url.Values{"old": {m.self}}.Encode(), nil, &st)
		cancel()
		if err != nil {
			continue
		}
		switch {
		case st.Active:
			return false, "a bootstrap that names this server as the old leader is active on " + s
		case st.Standby == "primary":
			return false, "the standby of the map group on " + s + " was promoted"
		case st.Standby == "in_recovery":
			proven++
		}
	}
	if proven == 0 {
		return false, "no standby of the map group could be shown to be in recovery"
	}
	if err := m.endTheHold(ctx); err != nil {
		return false, err.Error()
	}
	m.log.Warn("the primary of the map group was held back for a switchover that nothing owns any more; it is started again")
	return true, ""
}

// heldBackEvery is how often the old leader asks whether the hold of its primary of the map group has an owner.
var heldBackEvery = 10 * time.Second

// WatchHeldBack runs until ctx ends: while the primary of the map group is held back for a hand-over on this server, it resolves the hold when nothing owns it (a
// restarted kingresd finds the marker and no task, and the candidate may be gone or may have given up while this server was away).
func (m *mapFailover) WatchHeldBack(ctx context.Context) {
	t := time.NewTicker(heldBackEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if !m.isHeldBack() {
			continue
		}
		if ok, why := m.resolveHeldBack(ctx); !ok {
			m.log.Debug("the hold of the primary of the map group has an owner or cannot be resolved yet", "why", why)
		}
	}
}

// resumeOldLeaderOf asks the old leader's server to resolve its hold (a planned bootstrap that was given up): best effort; the loop of that server does it as well.
func (m *mapFailover) resumeOldLeaderOf(ctx context.Context, oldServer string) (bool, error) {
	var out mapResumeAnswer
	if err := m.call(ctx, oldServer, http.MethodPost, "/failover/map/resume", struct{}{}, &out); err != nil {
		return false, err
	}
	if !out.Resumed && out.Detail != "" {
		return false, errors.New(out.Detail)
	}
	return out.Resumed, nil
}

// queryInRecovery asks the running instance, by a connection to it.
func (m *mapFailover) queryInRecovery(ctx context.Context, id int) (bool, error) {
	c, err := m.svc.InstanceConn(ctx, id, sysdb.Database)
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var rec bool
	if err := c.QueryRow(ctx, `SELECT pg_is_in_recovery()`).Scan(&rec); err != nil {
		return false, err
	}
	return rec, nil
}

// call is a call to another server (a test stands in for the network).
func (m *mapFailover) call(ctx context.Context, server, method, path string, in, out any) error {
	if m.callPeer != nil {
		return m.callPeer(ctx, server, method, path, in, out)
	}
	return m.peers.do(ctx, server, method, path, in, out)
}

// members lists the servers of the cluster (a test stands in for the catalog and the peer directory).
func (m *mapFailover) members(ctx context.Context) ([]string, error) {
	if m.memberList != nil {
		return m.memberList(ctx)
	}
	return (&mapPorts{m: m}).Members(ctx)
}

// isHeldBack says that the primary of the map group is held back on this server for a hand-over.
func (m *mapFailover) isHeldBack() bool {
	if m.heldBack != nil {
		return m.heldBack()
	}
	d := m.fd.d
	return d != nil && d.LeaseMgr != nil && d.LeaseMgr.HandOverMarked(0)
}

// endTheHold ends the hold of the primary: the same two calls as the rollback of a switchover. The hand-over marker ends and the primary starts when the lease is
// won; the hold rules of the writes end.
func (m *mapFailover) endTheHold(ctx context.Context) error {
	if m.endHold != nil {
		return m.endHold(ctx)
	}
	if err := m.peers.Resume(ctx, m.self, failover.ResumeRequest{Group: 0, ExpectedTerm: -1}); err != nil {
		return fmt.Errorf("the hold of the primary could not be ended: %w", err)
	}
	if err := m.peers.Unquiesce(ctx, m.self, failover.QuiesceRequest{Group: 0, ExpectedTerm: -1}); err != nil {
		m.log.Warn("the writes of the map group could not be let in again after its hold was ended", "error", err)
	}
	return nil
}

// testInstanceUnreachable is set by the fault "promote:unreachable": the question whether the standby is in recovery then fails as if its instance were down.
var testInstanceUnreachable atomic.Bool

func (m *mapFailover) probeInRecovery(ctx context.Context, id int) (bool, error) {
	if testInstanceUnreachable.Load() {
		return false, errors.New("the instance cannot be reached (injected)")
	}
	if m.queryInRec != nil {
		return m.queryInRec(ctx, id)
	}
	return m.queryInRecovery(ctx, id)
}

func (m *mapFailover) controlData(ctx context.Context, id int) (map[string]string, error) {
	if m.controlDataOf != nil {
		return m.controlDataOf(ctx, id)
	}
	return m.svc.Manager().ControlData(ctx, id)
}
