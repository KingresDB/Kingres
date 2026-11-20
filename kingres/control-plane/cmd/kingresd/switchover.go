// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
)

// The planned switchover on the server of the old primary and on the server of the standby (design ha-phase5-part2.md, 5.2): stop the
// writes of a group, mark the position, stop the instance cleanly and release its lease, answer the proof on the standby, and undo all of
// it for a switchover that does not reach the promotion.

const (
	callFailoverQuiesce   = "POST /failover/quiesce"
	callFailoverUnquiesce = "POST /failover/unquiesce"
	callFailoverMark      = "POST /failover/mark"
	callFailoverVerify    = "POST /failover/verify"
	callFailoverResume    = "POST /failover/resume"
)

// quiesceSet is the set of groups whose writes are stopped on this server: the client entry point does not send new connections to the
// primary of such a group (the proxy sees the group as one whose lease is not held), and the state call says so.
type quiesceSet struct {
	mu     sync.Mutex
	groups map[int32]bool
}

var quiesced = &quiesceSet{}

func (q *quiesceSet) set(g int32, on bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.groups == nil {
		q.groups = map[int32]bool{}
	}
	if on {
		q.groups[g] = true
	} else {
		delete(q.groups, g)
	}
}

func (q *quiesceSet) has(g int32) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.groups[g]
}

func (p *foPeers) Quiesce(ctx context.Context, server string, r failover.QuiesceRequest) (failover.QuiesceAnswer, error) {
	var out failover.QuiesceAnswer
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	err := p.do(ctx, server, "POST", "/failover/quiesce", r, &out)
	return out, err
}

func (p *foPeers) Unquiesce(ctx context.Context, server string, r failover.QuiesceRequest) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/unquiesce", r, nil)
}

func (p *foPeers) Mark(ctx context.Context, server string, r failover.MarkRequest) (failover.MarkAnswer, error) {
	var out failover.MarkAnswer
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := p.do(ctx, server, "POST", "/failover/mark", r, &out)
	return out, err
}

func (p *foPeers) Verify(ctx context.Context, server string, r failover.VerifyRequest) (failover.VerifyAnswer, error) {
	var out failover.VerifyAnswer
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	err := p.do(ctx, server, "POST", "/failover/verify", r, &out)
	return out, err
}

func (p *foPeers) Resume(ctx context.Context, server string, r failover.ResumeRequest) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/resume", r, nil)
}

// handleFailoverQuiesce stops the writes of the group: the entry point holds, the hold rules replace the access rules, and every client
// session is ended. It answers how many are left; the leader asks again until none is.
func handleFailoverQuiesce(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.QuiesceRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a quiesce request")
	}
	if err := requireGroup(body, "a quiesce request"); err != nil {
		return nil, err
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	quiesced.set(r.Group, true)
	if err := d.Svc.Manager().Quiesce(ctx, li.ID); err != nil {
		return nil, err
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, "postgres")
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// The sessions of kingresd (the service role, and the private socket) stay; every other client session is ended.
	const where = `backend_type = 'client backend' AND pid <> pg_backend_pid() AND client_addr IS NOT NULL AND usename <> $1`
	if _, err := c.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE `+where, instance.ServiceRole); err != nil {
		return nil, err
	}
	var left int
	if err := c.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE `+where, instance.ServiceRole).Scan(&left); err != nil {
		return nil, err
	}
	// A test keeps a session that cannot be ended (the stop of the writes never succeeds).
	if d.Fault.FailoverInjection("quiesce:stall") == "fail" {
		return failover.QuiesceAnswer{Quiesced: false, Remaining: 1}, nil
	}
	return failover.QuiesceAnswer{Quiesced: left == 0, Remaining: left}, nil
}

// handleFailoverUnquiesce lets the writes in again: the access rules are put back and the entry point sends connections again.
func handleFailoverUnquiesce(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.QuiesceRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a quiesce request")
	}
	if err := requireGroup(body, "a quiesce request"); err != nil {
		return nil, err
	}
	quiesced.set(r.Group, false)
	if li, ok := d.Svc.PrimaryOf(r.Group); ok {
		if err := d.Svc.Manager().Unquiesce(ctx, li.ID); err != nil {
			return nil, err
		}
	}
	return struct{}{}, nil
}

// handleFailoverMark reads the position M that the standby must replay (kingres.switchover_mark()).
func handleFailoverMark(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.MarkRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a mark request")
	}
	if err := requireGroup(body, "a mark request"); err != nil {
		return nil, err
	}
	if err := staleTerm(ctx, d, r.ExpectedTerm, true); err != nil {
		return nil, err
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, dbOrSystem(""))
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var mark string
	if err := c.QueryRow(ctx, `SELECT kingres.switchover_mark()::text`).Scan(&mark); err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The position cannot be marked: " + err.Error()}
	}
	return failover.MarkAnswer{Mark: mark}, nil
}

// handleFailoverVerify answers, on the server of the standby, whether the standby replayed the mark, or whether it holds everything the
// old primary wrote (kingres.switchover_verify). A proof that can never be made is the answer "unprovable" (SQLSTATE KS006).
func handleFailoverVerify(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.VerifyRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a verify request")
	}
	if err := requireGroup(body, "a verify request"); err != nil {
		return nil, err
	}
	id := 0
	for _, x := range d.Svc.LocalInstances() {
		if x.Role.Group == r.Group && x.Role.IsStandby() && (r.InstanceID == 0 || x.ID == r.InstanceID) {
			id = x.ID
		}
	}
	if id == 0 {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_standby", Message: "This server runs no standby of the shard group."}
	}
	c, err := d.Svc.InstanceConn(ctx, id, dbOrSystem(""))
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The standby cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var ok bool
	if r.Checkpoint == "" {
		err = c.QueryRow(ctx, `SELECT kingres.switchover_replayed($1::pg_lsn)`, r.Mark).Scan(&ok)
	} else {
		err = c.QueryRow(ctx, `SELECT kingres.switchover_verify($1::pg_lsn, $2)`, r.Checkpoint, r.ClusterState).Scan(&ok)
	}
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "KS006" {
			return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "unprovable", Message: pe.Message}
		}
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The proof cannot be asked: " + err.Error()}
	}
	// A test answers that the standby does not hold everything the old primary wrote.
	if r.Checkpoint != "" && d.Fault.FailoverInjection("verify:false") == "fail" {
		ok = false
	}
	return failover.VerifyAnswer{Ok: ok}, nil
}

// handleFailoverResume is the rollback of a switchover whose old primary was stopped: the hand-over is withdrawn and the lease rounds start
// the primary again when this server holds the lease.
func handleFailoverResume(ctx context.Context, d *haDeps, body []byte) (any, error) {
	var r failover.ResumeRequest
	if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
		return nil, badBody("a resume request")
	}
	if err := requireGroup(body, "a resume request"); err != nil {
		return nil, err
	}
	if d.LeaseMgr == nil {
		return nil, notStarted
	}
	if err := d.LeaseMgr.Resume(ctx, r.Group); err != nil {
		return nil, err
	}
	return struct{}{}, nil
}

// demoteClean is the stop of a switchover (mode fast): the lease is given up, the instance stops with a fast shutdown, the shutdown
// checkpoint and the state of the cluster are read from pg_controldata, and the lease is released at the grantors (it is free at once).
// It is idempotent: a stopped instance is read again, which is how the proof is asked for twice.
func demoteClean(ctx context.Context, d *haDeps, r failover.DemoteRequest) (any, error) {
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	if err := d.LeaseMgr.Relinquish(r.Group); err != nil {
		return nil, err
	}
	out := failover.DemoteAnswer{}
	stopCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// A fast shutdown, never an immediate one: an immediate stop is not a clean one, and the proof would be the only protection.
	if err := d.Svc.Manager().Stop(stopCtx, li.ID); err != nil {
		d.Log.Warn("failover: stopping the old primary cleanly", "group", r.Group, "instance", li.ID, "error", err)
	}
	running, err := d.Svc.Manager().Running(ctx, li.ID)
	out.Stopped = err == nil && !running
	if !out.Stopped {
		return out, nil
	}
	cd, err := d.Svc.Manager().ControlData(ctx, li.ID)
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "pg_controldata cannot be read: " + err.Error()}
	}
	out.ClusterState = cd["Database cluster state"]
	if d.Fault.FailoverInjection("demote:state") == "fail" {
		out.ClusterState = "in production" // a test: the stop was not a clean one
	}
	out.Checkpoint = cd["Latest checkpoint location"]
	if out.ClusterState == "shut down" {
		res, rerr := d.LeaseMgr.ReleaseGroup(ctx, r.Group)
		out.Released = rerr == nil && res.Granted >= res.Need
		if rerr != nil {
			d.Log.Warn("failover: the lease was not released; it runs out by itself", "group", r.Group, "error", rerr)
		}
	}
	return out, nil
}

func registerSwitchoverCallee(reg func(key string, h func(ctx context.Context, d *haDeps, body []byte) (any, error))) {
	reg(callFailoverQuiesce, handleFailoverQuiesce)
	reg(callFailoverUnquiesce, handleFailoverUnquiesce)
	reg(callFailoverMark, handleFailoverMark)
	reg(callFailoverVerify, handleFailoverVerify)
	reg(callFailoverResume, handleFailoverResume)
}
