// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/lease"
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

// quiescedNow says that the writes of the group are stopped on this server: the memory of this process, or the hold rules on disk (a restart
// of kingresd in the middle of a switchover keeps the rules, F4-12, so the entry point and the state call follow the file).
func quiescedNow(d *haDeps, g int32) bool {
	if quiesced.has(g) {
		return true
	}
	if d == nil || d.Svc == nil || d.Svc.Manager() == nil {
		return false
	}
	if li, ok := d.Svc.PrimaryOf(g); ok {
		return d.Svc.Manager().Quiesced(li.ID)
	}
	return false
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
	// The undo calls cannot carry the fence of a task run (the repair of a switchover has none): the term is what refuses a caller that is behind (F4-09).
	if err := staleTerm(ctx, d, r.ExpectedTerm, false); err != nil {
		return nil, err
	}
	li, ok := d.Svc.PrimaryOf(r.Group)
	if !ok {
		return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "not_primary", Message: "This server runs no primary of the shard group."}
	}
	c, err := d.Svc.InstanceConn(ctx, li.ID, "postgres")
	if err != nil {
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The instance cannot be reached."}
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	quiesced.set(r.Group, true)
	first := !d.Svc.Manager().Quiesced(li.ID)
	var loadedBefore time.Time
	_ = c.QueryRow(ctx, `SELECT pg_conf_load_time()`).Scan(&loadedBefore)
	if err := d.Svc.Manager().Quiesce(ctx, li.ID); err != nil {
		return nil, err
	}
	// The reload is asked, not done: the answer waits until the postmaster has loaded the hold rules (F4-11).
	for deadline := time.Now().Add(10 * time.Second); first && time.Now().Before(deadline); {
		var loaded time.Time
		if c.QueryRow(ctx, `SELECT pg_conf_load_time()`).Scan(&loaded) == nil && loaded.After(loadedBefore) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The sessions of kingresd (the service role) stay; every other client session is ended, on the local socket as well (F4-11).
	const where = `backend_type = 'client backend' AND pid <> pg_backend_pid() AND usename <> $1`
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
	// The undo calls cannot carry the fence of a task run (the repair of a switchover has none): the term is what refuses a caller that is behind (F4-09).
	err, unread := staleTermRead(ctx, d, r.ExpectedTerm, false)
	if err != nil {
		return nil, err
	}
	if unread {
		// Held back: the term of the catalog cannot be read, and only the peers' word and the fence of a run say that the caller is the leader.
		if err := unquiesceUnreadable(ctx, r.Group, func(g int32) (int, bool) {
			li, ok := d.Svc.PrimaryOf(g)
			return li.ID, ok
		}, d.Svc.Manager().Running); err != nil {
			return nil, err
		}
	}
	// The rules first, the entry point after: a failed restore leaves the hold in place (and visible), for the repair to finish.
	if li, ok := d.Svc.PrimaryOf(r.Group); ok {
		if err := d.Svc.Manager().Unquiesce(ctx, li.ID); err != nil {
			return nil, err
		}
	}
	quiesced.set(r.Group, false)
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
	if r.ExpectedTerm < 0 {
		return nil, badBody("a mark request with the term of the run")
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
	// The undo calls cannot carry the fence of a task run (the repair of a switchover has none): the term is what refuses a caller that is behind (F4-09).
	if err := staleTerm(ctx, d, r.ExpectedTerm, false); err != nil {
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
	return cleanStop(ctx, d.Svc.Manager(), d.LeaseMgr, d.Fault, d.Log, li.ID, r.Group)
}

// The parts of the instance manager and of the lease manager that the clean stop uses, so that a test can stand in for them.
type stopManager interface {
	Stop(ctx context.Context, id int) error
	Running(ctx context.Context, id int) (bool, error)
	ControlData(ctx context.Context, id int) (map[string]string, error)
}

type stopLeases interface {
	HoldBack(group int32) error
	Relinquish(group int32) error
	ReleaseGroup(ctx context.Context, group int32) (lease.TransferResult, error)
}

// cleanStop is the stop of demoteClean for the instance id of group.
func cleanStop(ctx context.Context, mgr stopManager, lm stopLeases, fh *fault.Hook, log *slog.Logger, id int, group int32) (any, error) {
	// The primary is held back from starting, but the lease is renewed until the stop is over: a slow clean stop must not run into the end of the lease and
	// be fenced (it would then not be a clean one). A stop that does not end cleanly stops the renewal below, and the lease lapses and fences as before.
	if err := lm.HoldBack(group); err != nil {
		return nil, err
	}
	out := failover.DemoteAnswer{}
	stopCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	// A test: the stop takes longer than the lease the old primary has left, as it does when a walsender or a checkpoint holds the shutdown.
	if fh.FailoverInjection("demote:slowstop") == "slow" {
		select {
		case <-time.After(20 * time.Second):
		case <-stopCtx.Done():
		}
	}
	// A fast shutdown, never an immediate one: an immediate stop is not a clean one, and the proof would be the only protection.
	if err := mgr.Stop(stopCtx, id); err != nil {
		log.Warn("failover: stopping the old primary cleanly", "group", group, "instance", id, "error", err)
	}
	running, err := mgr.Running(ctx, id)
	out.Stopped = err == nil && !running
	if !out.Stopped {
		_ = lm.Relinquish(group) // not stopped: the renewal ends, the lease lapses and the fence stops what runs
		return out, nil
	}
	cd, err := mgr.ControlData(ctx, id)
	if fh.FailoverInjection("demote:controldata") == "fail" {
		// A test: pg_controldata cannot be read after the stop.
		cd, err = nil, errors.New("injected: pg_controldata failed")
	}
	if err != nil {
		// The instance is stopped and the answer cannot be given: the renewal ends here too (F5-07), or the lease of a primary that does not run would be renewed until a repair.
		_ = lm.Relinquish(group)
		return nil, &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "pg_controldata cannot be read: " + err.Error()}
	}
	out.ClusterState = cd["Database cluster state"]
	if fh.FailoverInjection("demote:state") == "fail" {
		out.ClusterState = "in production" // a test: the stop was not a clean one
	}
	out.Checkpoint = cd["Latest checkpoint location"]
	if out.ClusterState == "shut down" {
		res, rerr := lm.ReleaseGroup(ctx, group)
		out.Released = rerr == nil && res.Granted >= res.Need
		if rerr != nil {
			log.Warn("failover: the lease was not released; it runs out by itself", "group", group, "error", rerr)
		}
		// A test: kingresd ends right after the clean stop, before the leader's task can send the proof (the leader changes at the stop).
		if fh.FailoverInjection("demote:exit") == "exit" {
			os.Exit(137)
		}
	} else {
		_ = lm.Relinquish(group) // not a clean stop: no renewal, the lease runs out by itself
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

// primaryRuns says that the instance of the primary of the group on this server runs: its catalog can then be read, and a term that
// cannot be read is a refusal.
func primaryRuns(ctx context.Context, d *haDeps, group int32) bool {
	li, ok := d.Svc.PrimaryOf(group)
	if !ok {
		return false
	}
	up, err := d.Svc.Manager().Running(ctx, li.ID)
	return err == nil && up
}

// The calls of the planned switchover of the map group: the old leader's task starts the bootstrap of the candidate, sends it the proof of its clean
// stop, and gives it up when the switchover is rolled back.
func (p *foPeers) MapStart(ctx context.Context, server string, r failover.MapStartRequest) error {
	if r.Planned != nil {
		mapPlanned.started(server, r.Planned.TaskID)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/map/start", r, nil)
}

func (p *foPeers) MapProof(ctx context.Context, server string, r failover.MapProofRequest) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/map/proof", r, nil)
}

func (p *foPeers) MapAbandon(ctx context.Context, server, reason string) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return p.do(ctx, server, "POST", "/failover/map/abandon", mapAbandonBody{Reason: reason}, nil)
}

// MapRefusal is the refusal of the proof that the candidate of the planned switchover of the map group reported to this server (the old leader's).
func (p *foPeers) MapRefusal(_ context.Context, taskID int64) (string, bool) {
	return mapPlanned.refusal(taskID)
}

// unquiesceUnreadable is the verdict on an unquiesce whose callee cannot read its catalog term: the writes are let in again only when there is nothing here
// that they could reach, that is when this server runs no primary of the group or its instance is known not to run. A primary that runs, or one that cannot
// be looked at, stays quiesced and the call is refused with 503 (the leader repeats it).
func unquiesceUnreadable(ctx context.Context, group int32, primaryOf func(int32) (int, bool), running func(context.Context, int) (bool, error)) error {
	id, ok := primaryOf(group)
	if !ok {
		return nil
	}
	if up, err := running(ctx, id); err == nil && !up {
		return nil
	}
	return &internalapi.CallError{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "The catalog term cannot be read and the primary of the group may run; repeat the call."}
}
