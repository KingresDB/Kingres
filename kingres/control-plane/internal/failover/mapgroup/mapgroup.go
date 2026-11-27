// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package mapgroup is the bootstrap that promotes the standby of the map group when the manager leader and the primary of the map
// group are lost together (design ha-phase5-part2.md, 3.4). The system database, the task engine and the leader are on the map
// group, so no task can be recorded until the new primary of group 0 is writable: the steps B1 to B8 run on the server of the
// standby, under its own authority, and are recorded in the local journal (package journal) instead of in task rows. The step
// functions are the ones of the task in their meaning (take the fence lease, read and pin, promote, make the catalog follow) with
// the differences of the design: everything is local, the leader is not asked for anything, and the catalog is moved by
// kingres.promote_node on the promoted instance itself.
//
// Every step is idempotent and may be interrupted at its boundary: a restarted kingresd reads the journal and goes on.
package mapgroup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
)

// Group is the map group.
const Group int32 = 0

// Ports is what the bootstrap needs of its server and of the cluster. All of it is local to the candidate except the calls to
// other servers.
type Ports interface {
	Self() string
	// CatalogTerm reads the local copy of the catalog of the system database: its term and the host it names as the primary of
	// the map group.
	CatalogTerm(ctx context.Context) (term int64, primaryHost string, err error)
	// Members are the servers of the cluster as the local copy of the catalog knows them.
	Members(ctx context.Context) ([]string, error)
	Live(ctx context.Context, server string) string
	// Standbys are the standby records of group 0 in the local copy of the system database.
	Standbys(ctx context.Context) ([]failover.StandbyRec, error)
	Facts(ctx context.Context) (failover.GroupFacts, error)
	Position(ctx context.Context, server string) (failover.Position, error)
	State(ctx context.Context, server string) (failover.State, error)
	Demote(ctx context.Context, server string, r failover.DemoteRequest) (failover.DemoteAnswer, error)
	Lease(ctx context.Context, server string, r failover.LeaseRequest) (failover.LeaseAnswer, error)
	Promote(ctx context.Context, server string, r failover.PromoteRequest) (failover.PromoteAnswer, error)
	// FreezeLocal marks group 0 frozen on this server (the synchronous list is not emptied while it lasts).
	FreezeLocal(ctx context.Context, ttl time.Duration) error
	// Continue asks another candidate to resume the bootstrap at B4 with the same journal (the lease was handed to it).
	Continue(ctx context.Context, server string, j journal.Journal) error
	// Catalog is the extension's side on the promoted instance of this server (valid from B5 on).
	Catalog() failover.Catalog
	// PauseRebalance pauses a rebalance that the old leader left pending or running and says how many it paused.
	PauseRebalance(ctx context.Context) (int, error)
	// RecordTask inserts the task of the failover into the (now writable) system database, steps up to F7 done, and writes the
	// report rows from the journal. It is idempotent for one journal id and returns the id of the task.
	RecordTask(ctx context.Context, j journal.Journal) (int64, error)
	// RecordPlanned marks the group 0 row of the old leader's task (a planned switchover) promoted, with the term and the timeline of the promotion, in the system
	// database that this server promoted: the steps that the new leader's engine runs again read the term from it. It is idempotent.
	// A hard failover of a live leader's server passes the standby that was pinned after the stop and the verdict (a switchover pinned in its task and passes nil).
	RecordPlanned(ctx context.Context, taskID, termAfter, timeline int64, pin *journal.Pin, dataLoss []byte) error
	// PlannedRefused is what a planned switchover of the map group does when the standby could not prove the old leader's stop: this server lets go of the fence
	// lease it holds (so that the old leader can win its own lease again) and tells the old leader, whose task rolls back and starts its primary again.
	PlannedRefused(ctx context.Context, oldServer string, taskID int64, reason string) error
	// ResumeOldLeader asks the old leader's server to end the hold of its primary of the map group, once this bootstrap is given up (F6-01): its task may be gone, and the system
	// database is the primary that is held back, so nothing else would. Best effort; the old leader's own loop does it as well.
	ResumeOldLeader(ctx context.Context, oldServer string) error
	// PullProof reads how the stopped primary of the map group was left from the old leader's server (its kingresd answers while its instance is held back), for the
	// case that the old leader's task, which sends the proof, did not (its kingresd ended after the stop).
	PullProof(ctx context.Context, oldServer string) (checkpoint, clusterState string, err error)
	// CheckAuthority says whether this server holds the key of the certificate authority (pki/ca.key). When the escrow of the key is on, the
	// server first opens its sealed copy and makes the key from it; otherwise it has none unless an operator put it there (design 3.4, 11.2).
	CheckAuthority(ctx context.Context) (bool, error)
	// MoveSystem points what this server reads the system database with (the task engine, the store) at the promoted instance: from
	// here on this server is the leader and its system database is the instance it promoted.
	MoveSystem(ctx context.Context) error
	Alert(ctx context.Context, kind, severity, subject, message string, raise bool)
	// Audit writes an audit event of the bootstrap (M3: the row of the old leader's task was missing).
	Audit(ctx context.Context, action string, group int32, details map[string]any)
}

// Request is what a manual request says.
type Request struct {
	// OldServer is the server that runs the primary of the map group (and the leader).
	OldServer string
	Trigger   string
	// RequestedBy is the operator (manual).
	RequestedBy   string
	AllowDataLoss bool
	Comment       string
	// Planned is set by the old leader's own task for a planned switchover of the map group: the old server is alive, the task stops it cleanly
	// and sends the proof afterwards, and the task row is the task's (the bootstrap records none). Nothing else may start a bootstrap for a live
	// old server.
	Planned *journal.Planned
}

// ErrNoPlannedRow is what RecordPlanned answers when the group 0 row of the old leader's task is not there to be marked.
var ErrNoPlannedRow = errors.New("mapgroup: the group 0 row of the task is not in the system database")

// Result says how the run ended when it did not fail.
type Result struct {
	// HandedTo is the server the bootstrap was handed over to at B4 (another candidate is better); empty when this server
	// finished it.
	HandedTo string
	TaskID   int64
}

// Runner runs the bootstrap. The bounds are named, as in the task's Runner.
type Runner struct {
	P       Ports
	Journal *journal.Store
	Log     func(format string, a ...any)
	Sleep   func(ctx context.Context, d time.Duration) error
	Now     func() time.Time
	// Fault is the crash hook of the tests: it is called at "<step>:before" and "<step>:after", and an error it returns ends the
	// run as a crash of kingresd would (the journal says where).
	Fault func(point string) error

	FreezeTTL      time.Duration // default 10 min
	LeaseWait      time.Duration // the wait for the old lease, default 25 s
	VerifyWait     time.Duration // a standby that has not replayed the shutdown checkpoint yet is asked again for this long, default 30 s
	PlannedWait    time.Duration // the wait for the old leader's lease in a planned switchover (it is released by its clean stop), default 4 min
	ProofWait      time.Duration // the wait for the proof of the clean stop in a planned switchover, default 4 min
	ProofPullAfter time.Duration // the proof is read from the old leader's server when its task did not send it by then, default 20 s
	LeasePoll      time.Duration // default 1 s
	QuorumWait     time.Duration // default 60 s
	ReadBound      time.Duration // the positions must stop moving within this, default 15 s
	PromoteWait    time.Duration // the promoted instance is waited for this long, default 3 min
	CatalogWait    time.Duration // promote_node of the map group, repeated this long while the majority rule refuses, default 10 min
	CatalogPause   time.Duration // default 5 s
	LeaderWait     time.Duration // the other servers' answers show the new term within this, default 60 s
	AlertAfter     time.Duration // failoverBlocked is raised after this, default 60 s
	PromoteNodeOp  failover.PromoteOptions
}

func (r *Runner) logf(f string, a ...any) {
	if r.Log != nil {
		r.Log(f, a...)
	}
}

func (r *Runner) sleep(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		return r.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (r *Runner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func dur(v, d time.Duration) time.Duration {
	if v > 0 {
		return v
	}
	return d
}

func (r *Runner) fault(point string) error {
	if r.Fault != nil {
		return r.Fault(point)
	}
	return nil
}

// Refusal is a request that cannot start, with the problem code the API answers.
type Refusal struct {
	Code    string
	Status  int
	Message string
}

func (e *Refusal) Error() string { return e.Code + ": " + e.Message }

// Decide is B1: it checks that this server may run the bootstrap and begins the journal. It writes nothing else.
func (r *Runner) Decide(ctx context.Context, req Request) (journal.Journal, error) {
	term, primary, err := r.P.CatalogTerm(ctx)
	if err != nil {
		return journal.Journal{}, &Refusal{Code: "service_unavailable", Status: 503, Message: "The local copy of the catalog cannot be read: " + err.Error()}
	}
	if primary != req.OldServer {
		return journal.Journal{}, &Refusal{Code: "unprocessable", Status: 422,
			Message: fmt.Sprintf("Server %s does not run the primary of the map group in the catalog this server holds (it names %q).", req.OldServer, primary)}
	}
	if req.OldServer == r.P.Self() {
		return journal.Journal{}, &Refusal{Code: "unprocessable", Status: 422, Message: "This server runs the primary of the map group; ask a server that hosts its standby."}
	}
	recs, err := r.P.Standbys(ctx)
	if err != nil {
		return journal.Journal{}, &Refusal{Code: "service_unavailable", Status: 503, Message: "The standbys of the map group cannot be read: " + err.Error()}
	}
	mine := false
	for _, s := range recs {
		if s.Server == r.P.Self() && s.State == "ready" && s.CatalogNode != 0 {
			mine = true
		}
	}
	if !mine {
		return journal.Journal{}, &Refusal{Code: "no_replica_available", Status: 409, Message: "This server hosts no ready, registered standby of the map group."}
	}
	members, err := r.P.Members(ctx)
	if err != nil {
		return journal.Journal{}, &Refusal{Code: "service_unavailable", Status: 503, Message: "The members cannot be read: " + err.Error()}
	}
	if !r.hearsMajority(ctx, members) {
		return journal.Journal{}, &Refusal{Code: "quorum_unavailable", Status: 503, Message: "This server cannot reach a majority of the servers; the map group is not promoted from a minority."}
	}
	if r.P.Live(ctx, req.OldServer) == "live" {
		// The old server answers the liveness view: a partition on the leader's side, or a request for a switchover of the leader. Neither is built
		// (design, "the map group with a live old leader"): the calls that stop the old primary are the leader's, and this server is not the leader,
		// so nothing here may pretend to ask it. An automatic start for a server that is heard stays the conflict of F4-05.
		if nonEmpty(req.Trigger, "manual") == "automatic" {
			return journal.Journal{}, &Refusal{Code: "conflict", Status: 409, Message: "The old server of the map group is live for this server; an automatic failover does not start."}
		}
		if req.Planned == nil && !r.hardInProgress(req.OldServer) {
			return journal.Journal{}, &Refusal{Code: "unsupported_operation", Status: 422, Message: "The failover of the map group while its server is alive goes through the manager leader (its task), which stops the old primary fenced and then asks this server; it cannot be started here."}
		}
		// A planned switchover: the old leader's own task made the request (the caller is checked by the receiver), it stops its primary cleanly
		// and sends the proof; this server waits for its lease to be released and for the proof. A hard failover of a live leader's server is the
		// same request with Planned.Hard: the old leader stops its primary fenced (an immediate stop, the lease handed to this server), and this server
		// waits for that lease; no proof exists. The repeated request of an operator for a hard failover that paused (the loss was not accepted
		// after the stop) is the other case that is accepted: its journal is in progress on this server.
	} else if req.Planned != nil {
		return journal.Journal{}, &Refusal{Code: "conflict", Status: 409, Message: "A planned switchover of the map group needs the old leader alive; this server does not hear it."}
	}
	id := strconv.FormatInt(r.now().UnixNano(), 36)
	j, err := r.Journal.Begin(id, req.OldServer, term, nonEmpty(req.Trigger, "manual"), req.RequestedBy, req.AllowDataLoss)
	if err != nil || req.Planned == nil {
		return j, err
	}
	return r.Journal.SetPlanned(*req.Planned)
}

// hardInProgress says that a hard failover of the map group of oldServer, begun by the old leader's task, is in progress on this server.
func (r *Runner) hardInProgress(oldServer string) bool {
	cur, ok := r.Journal.Current()
	return ok && r.Journal.Active() && cur.OldServer == oldServer && cur.Planned != nil && cur.Planned.Hard
}

func nonEmpty(s, d string) string {
	if s != "" {
		return s
	}
	return d
}

func (r *Runner) hearsMajority(ctx context.Context, members []string) bool {
	live := 0
	for _, m := range members {
		if m == r.P.Self() || r.P.Live(ctx, m) == "live" {
			live++
		}
	}
	return live*2 > len(members)
}

// Run is B1 to B8 for a request, or the rest of them for a journal in progress. It returns a *failover.PauseError when the
// bootstrap waits for an operator (the journal keeps where it is), a *Refusal when it cannot start, and an error of its own
// for a defect. A crash at any point is followed by Resume.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	var j journal.Journal
	if cur, ok := r.Journal.Current(); ok && r.Journal.Active() {
		j = cur
		// A repeated request of an operator for the same server carries the decision (allowDataLoss) that the pause asked for (F4-01): the
		// journal takes it, as long as nothing is pinned.
		if req.OldServer == cur.OldServer && nonEmpty(req.Trigger, "manual") == "manual" {
			var err error
			if j, err = r.Journal.Begin(cur.ID, cur.OldServer, cur.Term, "manual", req.RequestedBy, req.AllowDataLoss); err != nil {
				return Result{}, err
			}
		}
	} else {
		var err error
		if j, err = r.Decide(ctx, req); err != nil {
			return Result{}, err
		}
	}
	return r.resume(ctx, j)
}

// Resume goes on with the journal in progress after a restart of kingresd; it does nothing when none is.
func (r *Runner) Resume(ctx context.Context) (Result, bool, error) {
	j, ok := r.Journal.Current()
	if !ok || !r.Journal.Active() {
		return Result{}, false, nil
	}
	res, err := r.resume(ctx, j)
	return res, true, err
}

func (r *Runner) resume(ctx context.Context, j journal.Journal) (Result, error) {
	steps := []struct {
		name string
		f    func(context.Context, journal.Journal) (bool, error)
	}{
		{journal.StepDecide, func(context.Context, journal.Journal) (bool, error) { return false, nil }},
		{journal.StepFreeze, r.freeze},
		{journal.StepLease, r.takeFence},
		{journal.StepPin, r.pin},
		{journal.StepPromote, r.promote},
		{journal.StepCatalog, r.catalogFollows},
		{journal.StepLeader, r.makeLeader},
		{journal.StepGateOpen, r.openGate},
	}
	var res Result
	for _, s := range steps {
		if r.Journal.IsDone(s.name) {
			continue
		}
		// A bootstrap that was given up while a step ran does not go on to the next one (F6-02): the abandon released the lease and let the old leader start again.
		if !r.Journal.Active() {
			return res, failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up; nothing more is done")
		}
		if err := r.fault(s.name + ":before"); err != nil {
			return res, err
		}
		// The freeze of B2 lapses (ten minutes) and is memory of the agent that a restart may have lost: every step after it asks for it again,
		// so a bootstrap that waits long (the old lease, a promoted instance that starts) or that a restart interrupted never runs without it.
		if s.name != journal.StepDecide && s.name != journal.StepFreeze && r.Journal.IsDone(journal.StepFreeze) {
			if err := r.P.FreezeLocal(ctx, dur(r.FreezeTTL, 10*time.Minute)); err != nil {
				return res, err
			}
		}
		cur, _ := r.Journal.Current()
		stop, err := s.f(ctx, cur)
		if err != nil {
			return res, err
		}
		if stop {
			// handed over to another candidate: this server is done and the journal says so
			cur, _ = r.Journal.Current()
			return Result{HandedTo: handedTo(cur)}, nil
		}
		if err := r.Journal.Done(s.name); err != nil {
			return res, err
		}
		if err := r.fault(s.name + ":after"); err != nil {
			return res, err
		}
	}
	cur, _ := r.Journal.Current()
	res.TaskID = cur.TaskID
	return res, nil
}

func handedTo(j journal.Journal) string {
	for _, e := range j.Events {
		if e.Kind == "handedOver" {
			if s, ok := e.Detail["to"].(string); ok {
				return s
			}
		}
	}
	return ""
}

// B2: the group is frozen locally (the leader is dead: there is no old agent to tell).
func (r *Runner) freeze(ctx context.Context, j journal.Journal) (bool, error) {
	if err := r.P.FreezeLocal(ctx, dur(r.FreezeTTL, 10*time.Minute)); err != nil {
		return false, err
	}
	return false, r.Journal.Event(journal.StepFreeze, "frozen", nil)
}

// B3: as F3. The fence lease is taken from a majority of all the servers, which proves that a majority talks to this server, that the
// old holder's lease ran out at a majority and that it can never be granted one again. An old server that is heard is not asked to
// demote: the calls that stop a primary are the leader's, this server is not the leader, and Decide refuses a live old server until the
// design "the map group with a live old leader" is built.
func (r *Runner) takeFence(ctx context.Context, j journal.Journal) (bool, error) {
	members, err := r.P.Members(ctx)
	if err != nil {
		return false, err
	}
	started := r.now()
	// A planned switchover waits for the old leader to stop cleanly (it releases its lease then): the lease cannot be won before, and the wait
	// is the stop, the replay of the mark and the verification, not the lapse of a lease.
	leaseWait := dur(r.LeaseWait, 25*time.Second)
	if j.Planned != nil {
		leaseWait = dur(r.PlannedWait, 4*time.Minute)
	}
	var quorumSince time.Time
	for {
		if !r.Journal.Active() {
			return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up while it waited for the lease; nothing more is done")
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if !r.hearsMajority(ctx, members) {
			if quorumSince.IsZero() {
				quorumSince = r.now()
			}
			if r.now().Sub(quorumSince) > dur(r.QuorumWait, time.Minute) {
				r.P.Alert(ctx, "failoverBlocked", "warning", "group:0", "The server that promotes the map group cannot hear a majority of the servers.", true)
				return false, &failover.PauseError{Reason: "This server cannot hear a majority of the servers (quorum_unavailable); nothing was done. Repair the network or repeat the request from a server that can."}
			}
		} else {
			quorumSince = time.Time{}
			ans, err := r.P.Lease(ctx, r.P.Self(), failover.LeaseRequest{Group: Group, Role: failover.LeaseFence, ExpectedTerm: j.Term})
			// A crash point of the tests: the lease is adopted and renewed, and the bootstrap has not looked at the journal again. A test that gives the bootstrap up here, and
			// lets it go on, makes the order of the race that the look at the journal and letGoIfGivenUp close.
			if ferr := r.fault(journal.StepLease + ":asked"); ferr != nil {
				return false, ferr
			}
			if gerr := r.letGoIfGivenUp(ctx, j.ID); gerr != nil {
				return false, gerr
			}
			switch {
			case err != nil && failover.IsCode(err, "stale_term"):
				return false, errors.New("another failover changed the catalog term of the cluster; nothing else is done")
			case err == nil && ans.Held:
				waited := int(r.now().Sub(started).Milliseconds())
				return false, r.Journal.Event(journal.StepLease, "leaseHeld", map[string]any{"waitMs": waited})
			case err == nil && r.now().Sub(started) > leaseWait:
				for _, h := range ans.RefusedBy {
					if h == j.OldServer {
						r.P.Alert(ctx, "failoverBlocked", "warning", "group:0", "A majority of the servers still hear the holder of the lease of the map group ("+h+"); its primary is alive for the cluster.", true)
						return false, failover.Fail(failover.CodeLeaseStillHeld, true, "the lease of the map group is still held by %s and renewed", h)
					}
				}
				return false, failover.Fail(failover.CodeLeaseHeldElsewhere, true, "the lease of the map group is held by another server (%v)", ans.RefusedBy)
			}
		}
		if err := r.sleep(ctx, dur(r.LeasePoll, time.Second)); err != nil {
			return false, err
		}
	}
}

// letGoIfGivenUp closes the race between the abandon and a call that asks this server's lease manager to take the lease of the map group. The abandon marks the
// journal and then lets go of the lease; a call of the runner that was already on its way adopts the lease after that release, and the adoption is renewed for as
// long as nothing releases it, which keeps the old leader (whose held-back primary the abandon started again) from ever winning the lease. The journal is marked
// before the release, so a call that returns while the journal is still active is covered by the release that follows it, and one that returns after the mark is
// let go of here. It is done only for the bootstrap that was given up (the id of the journal), never for a later one.
func (r *Runner) letGoIfGivenUp(ctx context.Context, id string) error {
	cur, ok := r.Journal.Current()
	if !ok || cur.ID != id || cur.Abandoned.IsZero() {
		return nil
	}
	var rerr error
	for try := 0; try < 5; try++ {
		if _, rerr = r.P.Lease(context.WithoutCancel(ctx), r.P.Self(), failover.LeaseRequest{Group: Group, Role: failover.LeaseRelease}); rerr == nil {
			return failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up while it asked for the lease; the lease was let go of and nothing more is done")
		}
		if err := r.sleep(ctx, dur(r.LeasePoll, time.Second)); err != nil {
			break
		}
	}
	return failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up while it asked for the lease, and the lease could not be let go of (%v)", rerr)
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// B4: the positions of every ready standby of group 0, the ranking of F4, the pin in the journal. When another candidate is better
// the lease is handed to it, it is told to resume at B4 with this journal, and this server stops.
func (r *Runner) pin(ctx context.Context, j journal.Journal) (bool, error) {
	if j.Pin != nil {
		return false, nil
	}
	recs, err := r.P.Standbys(ctx)
	if err != nil {
		return false, err
	}
	if j.Planned != nil && !j.Planned.Hard {
		// The old leader's task chose this standby and had the mark replayed on it: no other one can be proven, so no other one is a choice.
		for _, s := range recs {
			if s.ID == j.Planned.StandbyID && s.State == "ready" && s.Server == r.P.Self() {
				pinned, err := r.Journal.SetPin(journal.Pin{StandbyID: s.ID, Server: s.Server, Node: s.CatalogNode, Instance: s.InstanceID})
				if err != nil {
					return false, err
				}
				return false, r.Journal.Event(journal.StepPin, "pinned", map[string]any{"standby": pinned.StandbyID, "server": pinned.Server, "verdict": failover.VerdictNone,
					"message": "A planned switchover loses nothing: the standby is proven to hold everything the old leader wrote, or it is not promoted."})
			}
		}
		return false, failover.Fail(failover.CodeNoCandidate, true, "the standby %d that the old leader chose is not a ready standby of the map group on this server", j.Planned.StandbyID)
	}
	var ready []failover.StandbyRec
	for _, s := range recs {
		if s.State == "ready" {
			ready = append(ready, s)
		}
	}
	if len(ready) == 0 {
		return false, failover.Fail(failover.CodeNoCandidate, true, "the map group has no ready standby")
	}
	cands := r.readPositions(ctx, ready)
	var promoted []failover.StandbyRec
	for _, c := range cands {
		if c.Readable && !c.Pos.InRecovery {
			promoted = append(promoted, ready[indexOf(ready, c.StandbyID)])
		}
	}
	if len(promoted) > 0 {
		r.P.Alert(ctx, "failoverNeedsDecision", "critical", "group:0", "An instance of the map group is out of recovery on "+promoted[0].Server+"; this server does not promote its own.", true)
		return false, &failover.PauseError{Reason: fmt.Sprintf("an instance of the map group is out of recovery on %s (a bootstrap that did not finish, or a promotion by hand); this server does not promote its own. Restart that server, or declare it lost with allowDataLoss", promoted[0].Server)}
	}
	facts, err := r.P.Facts(ctx)
	if err != nil {
		return false, err
	}
	if j.Planned != nil && j.Planned.Hard && j.Planned.Facts != nil {
		// The old leader read these just before it stopped its primary, and no check can follow the stop: the record in the system database grows old from
		// that moment, and the verdict would turn into "possible" after thirty seconds for a group that streamed a synchronous standby when it stopped.
		f := j.Planned.Facts
		facts = failover.GroupFacts{SyncStreaming: f.SyncStreaming, LastKnownLsn: f.LastKnownLsn, PrimaryPlanned: f.PrimaryPlanned, AsyncDatabases: f.AsyncDatabases}
	}
	for i := range cands {
		cands[i].Preferred = false
	}
	target := ""
	if j.Planned != nil && j.Planned.Hard {
		target = j.Planned.Target // the server the request named: ranked and judged as for the ordinary groups
	}
	ch := failover.Choose(failover.ChooseInput{Candidates: cands, Target: target, Facts: facts})
	if ch.Chosen == nil {
		return false, failover.Fail(failover.CodeNoCandidate, true, "no standby of the map group can be promoted: %s", ch.Message)
	}
	if !failover.Accepts(ch.Verdict, j.AllowDataLoss) {
		r.P.Alert(ctx, "failoverNeedsDecision", "critical", "group:0", "The failover of the map group could lose data: "+ch.Message, true)
		reason := fmt.Sprintf("The failover of the map group could lose data (%s): %s Repeat the request with allowDataLoss set to true if you accept it.", ch.Verdict, ch.Message)
		if j.Planned != nil && j.Planned.Hard {
			// The old leader is stopped and its task cannot take a request: the one that runs this bootstrap does.
			reason += " Send the request to server " + r.P.Self() + ", which runs the bootstrap; the old leader was stopped."
		}
		return false, &failover.PauseError{Reason: reason}
	}
	chosen := ready[indexOf(ready, ch.Chosen.StandbyID)]
	p := journal.Pin{StandbyID: chosen.ID, Server: chosen.Server, Node: chosen.CatalogNode, Instance: chosen.InstanceID}
	pinned, err := r.Journal.SetPin(p)
	if err != nil {
		return false, err
	}
	_ = r.Journal.Event(journal.StepPin, "pinned", map[string]any{"standby": pinned.StandbyID, "server": pinned.Server, "verdict": ch.Verdict, "message": ch.Message})
	if pinned.Server != r.P.Self() {
		// Another candidate is better: the lease goes to it with no free moment, and it resumes at B4 (its pin is in the journal).
		ans, terr := r.P.Lease(ctx, r.P.Self(), failover.LeaseRequest{Group: Group, Role: failover.LeaseFence, TransferTo: pinned.Server, ExpectedTerm: j.Term})
		if terr != nil || !ans.Transferred {
			return false, failover.Fail(failover.CodeLeaseLost, true, "the lease of the map group could not be handed to %s", pinned.Server)
		}
		cur, _ := r.Journal.Current()
		if err := r.P.Continue(ctx, pinned.Server, cur); failover.IsCode(err, "conflict") {
			// P3: another bootstrap of this term is in progress on the better candidate (an operator's request, or its own monitor): this one gives up instead of
			// repeating a call that cannot succeed. The lease it handed over lapses or is used by that bootstrap; nothing was promoted here.
			_ = r.Journal.Event(journal.StepPin, "handOverRefused", map[string]any{"to": pinned.Server, "error": err.Error()})
			if aerr := r.Journal.AbandonUnpromoted("another bootstrap of this term runs on " + pinned.Server); aerr != nil {
				return false, aerr
			}
			return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "another bootstrap of the map group is in progress on %s; this one was given up", pinned.Server)
		} else if err != nil {
			return false, fmt.Errorf("asking %s to continue the bootstrap: %w", pinned.Server, err)
		}
		_ = r.Journal.Event(journal.StepPin, "handedOver", map[string]any{"to": pinned.Server})
		return true, nil
	}
	return false, nil
}

func indexOf(recs []failover.StandbyRec, id int64) int {
	for i, s := range recs {
		if s.ID == id {
			return i
		}
	}
	return 0
}

func (r *Runner) readPositions(ctx context.Context, recs []failover.StandbyRec) []failover.Candidate {
	read := func() []failover.Candidate {
		out := make([]failover.Candidate, len(recs))
		for i, rec := range recs {
			cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			p, err := r.P.Position(cctx, rec.Server)
			cancel()
			out[i] = failover.Candidate{StandbyID: rec.ID, Server: rec.Server, Readable: err == nil, Pos: p}
			if err != nil {
				out[i].Err = err.Error()
			}
		}
		return out
	}
	deadline := r.now().Add(dur(r.ReadBound, 15*time.Second))
	prev := read()
	for {
		if r.sleep(ctx, time.Second) != nil {
			return prev
		}
		cur := read()
		same := true
		for i := range cur {
			a, _ := prev[i].Pos.Flushed()
			b, _ := cur[i].Pos.Flushed()
			if prev[i].Readable != cur[i].Readable || a != b {
				same = false
			}
		}
		if same || r.now().After(deadline) {
			sort.SliceStable(cur, func(i, j int) bool { return cur[i].StandbyID < cur[j].StandbyID })
			return cur
		}
		prev = cur
	}
}

// B5: the lease is held by this server (the holder adoption), then the standby is promoted by the same callee as the task's F6.
func (r *Runner) promote(ctx context.Context, j journal.Journal) (bool, error) {
	if j.Pin == nil || j.Pin.Server != r.P.Self() {
		return false, errors.New("the journal's pin is not this server's standby; the bootstrap must resume on the pinned server")
	}
	deadline := r.now().Add(dur(r.PromoteWait, 3*time.Minute))
	for {
		if !r.Journal.Active() {
			return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up before the promotion; nothing more is done")
		}
		ans, err := r.P.Lease(ctx, r.P.Self(), failover.LeaseRequest{Group: Group, Role: failover.LeaseHolder, ExpectedTerm: j.Term})
		if gerr := r.letGoIfGivenUp(ctx, j.ID); gerr != nil {
			return false, gerr
		}
		if err == nil && ans.Held {
			break
		}
		if !r.now().Before(deadline) {
			return false, failover.Fail(failover.CodeLeaseLost, true, "this server did not hold the lease of the map group")
		}
		if err := r.sleep(ctx, dur(r.LeasePoll, time.Second)); err != nil {
			return false, err
		}
	}
	req := failover.PromoteRequest{Group: Group, StandbyID: j.Pin.StandbyID, InstanceID: j.Pin.Instance, ExpectedTerm: j.Term}
	if j.Planned != nil && !j.Planned.Hard {
		// A planned switchover promotes only with the proof of the old leader's clean stop; the callee checks it against what the standby replayed.
		cp, state, err := r.waitProof(ctx)
		if err != nil {
			return false, err
		}
		req.Checkpoint, req.ClusterState = cp, state
	}
	// The intent is on disk before the call (F5-03): a promotion that ran and is not yet recorded cannot be given up as if it had not happened.
	if err := r.Journal.PromotionIntent(); err != nil {
		if errors.Is(err, journal.ErrAbandoned) {
			return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up before the promotion; nothing more is done")
		}
		return false, err
	}
	behindUntil := r.now().Add(dur(r.VerifyWait, 30*time.Second))
	for try := 0; ; try++ {
		if try > 0 && !r.Journal.Active() {
			// The bootstrap was given up while the call was repeated (F6-02): another call would promote a standby whose lease was let go of, behind the back of the journal.
			return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "the bootstrap of the map group was given up while the promotion was repeated; nothing more is done")
		}
		ans, err := r.P.Promote(ctx, r.P.Self(), req)
		switch {
		case err == nil:
			return false, r.Journal.Event(journal.StepPromote, "promoted", map[string]any{"status": ans.Status, "timeline": ans.Timeline, "lsn": ans.Lsn})
		case failover.IsCode(err, "standby_behind") && r.now().Before(behindUntil):
			// The standby has not replayed the shutdown checkpoint yet (replication lag, a loaded machine): the ordinary groups wait for it for VerifyWait and so does
			// this bootstrap; a proof that can never be made (unprovable) is refused at once (F5-04).
		case failover.IsCode(err, "unprovable"), failover.IsCode(err, "standby_behind"):
			// Nothing was promoted, and the old leader is stopped: this server cannot start it again (that is the leader's call). It asks for a decision.
			if j.Planned == nil || j.Planned.Hard {
				return false, &failover.PauseError{Reason: "the standby of the map group cannot prove what it must (" + err.Error() + "); nothing was promoted"}
			}
			// The old leader is stopped and nothing was promoted: this server lets go of the fence lease and tells the old leader, whose task rolls back and
			// starts its primary again (the lease decides who may run: it is free now). The bootstrap is given up.
			reason := "the standby of the map group cannot prove that it holds everything the old leader wrote (" + err.Error() + ")"
			r.P.Alert(ctx, "failoverNeedsDecision", "critical", "group:0", "The planned switchover of the map group was refused after the old leader stopped: "+reason+"; the old leader is started again.", true)
			// The journal is given up whether or not the old leader could be told (F5-09): the lease is let go of already, and a journal that stays active would keep the
			// gate of this server closed and a later repeat could promote after the old leader's task was rolled back.
			perr := r.P.PlannedRefused(ctx, j.OldServer, j.Planned.TaskID, reason)
			_ = r.Journal.Event(journal.StepPromote, "refused", map[string]any{"reason": reason, "toldTheOldLeader": perr == nil})
			if aerr := r.Journal.AbandonUnpromoted(reason); aerr != nil {
				return false, aerr
			}
			// Only now can the old leader resolve its hold: a bootstrap that names it and is active is what makes it wait (F6-01).
			if rerr := r.P.ResumeOldLeader(ctx, j.OldServer); rerr != nil {
				_ = r.Journal.Event(journal.StepPromote, "oldLeaderNotResumed", map[string]any{"error": rerr.Error()})
			}
			if perr != nil {
				return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "%s; the old leader could not be told (%v): its task rolls back by its own wait", reason, perr)
			}
			return false, failover.Fail(failover.CodeSwitchoverUnproven, false, "%s", reason)
		case failover.IsCode(err, "stale_term"):
			return false, errors.New("another failover changed the catalog term of the cluster; nothing else is done")
		case failover.IsCode(err, "lease_not_held"), failover.IsCode(err, "unavailable"):
			if !r.now().Before(deadline) {
				return false, failover.Fail(failover.CodePromoteTimeout, true, "the promotion of the standby of the map group did not finish: %v", err)
			}
		default:
			return false, err
		}
		if err := r.sleep(ctx, 2*time.Second); err != nil {
			return false, err
		}
	}
}

// B6: kingres.promote_node on the promoted instance itself, in the catalog of every database, by the rules of sequence 3: while the
// extension refuses because only k of the n other groups can be reached it is repeated every five seconds (failoverBlocked after a
// minute, a pause after CatalogWait); a catalog that holds a newer term means another server promoted the map group.
func (r *Runner) catalogFollows(ctx context.Context, j journal.Journal) (bool, error) {
	cat := r.P.Catalog()
	dbs, err := cat.Databases(ctx)
	if err != nil {
		return false, err
	}
	sys, err := cat.Nodes(ctx, failover.SystemDB)
	if err != nil {
		return false, err
	}
	var newN *failover.Node
	for i := range sys {
		if sys[i].ID == j.Pin.Node {
			newN = &sys[i]
		}
	}
	if newN == nil {
		return false, failover.Fail(failover.CodeBadPromoteTarget, false, "the system catalog has no node %d", j.Pin.Node)
	}
	started := r.now()
	raised := false
	for _, db := range dbs {
		nodes, err := cat.Nodes(ctx, db)
		if err != nil {
			return false, err
		}
		var id int32
		for _, n := range nodes {
			if n.Group == Group && n.Host == newN.Host && n.Port == newN.Port {
				id = n.ID
			}
		}
		if id == 0 {
			return false, failover.Fail(failover.CodeBadPromoteTarget, false, "the catalog of database %s does not have the standby of the map group (%s:%d)", db, newN.Host, newN.Port)
		}
		for {
			o := r.PromoteNodeOp
			o.NewNode, o.Group, o.DB = id, Group, db
			o.Event = func(kind string, d map[string]any) { _ = r.Journal.Event(journal.StepCatalog, kind, d) }
			out, err := failover.RunPromoteNode(ctx, cat, o)
			if err == nil {
				_ = r.Journal.Event(journal.StepCatalog, "catalogPromoted", map[string]any{"database": db, "term": out.Term, "unreached": out.Unreached})
				break
			}
			var pe *failover.PauseError
			if !errors.As(err, &pe) {
				return false, err
			}
			if r.now().Sub(started) > dur(r.CatalogWait, 10*time.Minute) {
				return false, err
			}
			if !raised && r.now().Sub(started) > dur(r.AlertAfter, time.Minute) {
				r.P.Alert(ctx, "failoverBlocked", "critical", "group:0", "The catalog of the map group cannot be moved: "+pe.Reason, true)
				raised = true
			}
			if err := r.sleep(ctx, dur(r.CatalogPause, 5*time.Second)); err != nil {
				return false, err
			}
		}
	}
	if raised {
		r.P.Alert(ctx, "failoverBlocked", "critical", "group:0", "", false)
	}
	return false, nil
}

// B7: this server is the new leader once the local catalog names it the primary of group 0 and the other servers show the new term.
// Then what the old leader left running is paused, and the task of the failover is recorded.
func (r *Runner) makeLeader(ctx context.Context, j journal.Journal) (bool, error) {
	deadline := r.now().Add(dur(r.LeaderWait, time.Minute))
	for {
		term, primary, err := r.P.CatalogTerm(ctx)
		if err == nil && primary == r.P.Self() && term > j.Term {
			break
		}
		if !r.now().Before(deadline) {
			return false, &failover.PauseError{Reason: "the catalog of this server does not name it the primary of the map group after the promotion; read the journal events, then repeat the request"}
		}
		if err := r.sleep(ctx, time.Second); err != nil {
			return false, err
		}
	}
	if err := r.P.MoveSystem(ctx); err != nil {
		return false, fmt.Errorf("moving the system database of this server to the promoted instance: %w", err)
	}
	n, err := r.P.PauseRebalance(ctx)
	if err != nil {
		return false, fmt.Errorf("pausing the rebalance of the old leader: %w", err)
	}
	if n > 0 {
		_ = r.Journal.Event(journal.StepLeader, "rebalancePaused", map[string]any{"count": n})
	}
	cur, _ := r.Journal.Current()
	if !cur.Recorded {
		id := int64(0)
		if cur.Planned != nil {
			// The task is the old leader's: its row was replicated to this server before the stop and the engine claims it once the gate is open. Its group 0 row
			// says promoted, with the new term, so that the steps the engine runs again use the term that now holds.
			id = cur.Planned.TaskID
			term, _, terr := r.P.CatalogTerm(ctx)
			if terr != nil {
				return false, fmt.Errorf("reading the catalog term: %w", terr)
			}
			var timeline int64
			for _, e := range cur.Events {
				if e.Kind == "promoted" {
					if tl, ok := e.Detail["timeline"].(float64); ok {
						timeline = int64(tl)
					}
				}
			}
			// A hard failover of a live leader's server pinned its standby here, after the stop; the old leader's task could not (its system database was the
			// stopped instance), so the row of group 0 gets the pin and the verdict now. A switchover pinned before and keeps its own row.
			var pin *journal.Pin
			var dl []byte
			if cur.Planned.Hard && cur.Pin != nil {
				pin = cur.Pin
				v := failover.DataLoss{Verdict: failover.VerdictUnknown, AsyncDatabases: []string{}, UnreadableServerIDs: []string{}}
				for _, e := range cur.Events {
					if e.Kind == "pinned" {
						if s, ok := e.Detail["verdict"].(string); ok {
							v.Verdict = s
						}
						if s, ok := e.Detail["message"].(string); ok {
							v.Message = s
						}
					}
				}
				dl, _ = json.Marshal(v)
			}
			if err := r.P.RecordPlanned(ctx, id, term, timeline, pin, dl); errors.Is(err, ErrNoPlannedRow) {
				// M3: the row of the old leader's task is not in the system database this server promoted (the task was never replicated, or was removed): nothing may be
				// claimed on it, and the operator has to know.
				r.P.Alert(ctx, "failoverNeedsDecision", "critical", "group:0", "The map group was promoted, but the task "+strconv.FormatInt(id, 10)+" of the old leader has no row for it in the new leader's system database.", true)
				r.P.Audit(ctx, "failoverGroupPromoted", Group, map[string]any{"from": cur.OldServer, "to": r.P.Self(), "task": id, "taskRowMissing": true, "pin": pin != nil})
				_ = r.Journal.Event(journal.StepLeader, "plannedRowMissing", map[string]any{"task": id, "pin": pin != nil, "verdict": string(dl)})
				return false, &failover.PauseError{Reason: "the map group was promoted, but the row of the old leader's task is missing in the new system database; read the journal events, then repeat the request"}
			} else if err != nil {
				return false, fmt.Errorf("marking the map group of the task promoted: %w", err)
			}
		} else {
			var err error
			if id, err = r.P.RecordTask(ctx, cur); err != nil {
				return false, fmt.Errorf("recording the task of the failover: %w", err)
			}
		}
		if err := r.Journal.Recorded(id); err != nil {
			return false, err
		}
		if err := r.fault("B7 recorded"); err != nil {
			return false, err
		}
	}
	return false, nil
}

// B8: the engine gate opens (the journal says the bootstrap is over); the engine claims the task and runs what is left.
func (r *Runner) openGate(ctx context.Context, j journal.Journal) (bool, error) {
	// The new leader issues and revokes certificates only with ca.key. With the escrow of the key on, CheckAuthority has made the key from the
	// sealed copy of this server. When there is none, the product says so and what the operator must do, and the renewal jobs of the other
	// servers fail visibly until it is done (their certificates last a year).
	if ok, err := r.P.CheckAuthority(ctx); err == nil && !ok {
		r.P.Alert(ctx, AlertAuthority, "critical", "cluster", AuthorityMessage, true)
		_ = r.Journal.Event(journal.StepGateOpen, "certificateAuthorityMissing", nil)
	} else if err == nil {
		r.P.Alert(ctx, AlertAuthority, "critical", "cluster", "", false)
	}
	return false, r.Journal.Event(journal.StepGateOpen, "gateOpened", nil)
}

// AlertAuthority is the alert of a new leader that has no key of the certificate authority.
const AlertAuthority = "certificateAuthorityUnavailable"

// AuthorityMessage is what the alert says: there is no sealed copy of the key here (the escrow was off, or its copy was refused), so the operator restores it by hand.
const AuthorityMessage = "The map group was failed over and this server, the new manager leader, does not hold the key of the certificate authority: no certificate can be issued, renewed or revoked. Switch the escrow of the CA key on (setCaEscrow) before the next failover. Now restore pki/ca.key and pki/revoked.json of the old leader from its backup into the pki directory of this server and restart kingresd; the certificates of the servers last a year, and their renewal fails until then."

// waitProof waits until the old leader of a planned switchover has delivered the proof of its clean stop (the shutdown checkpoint and the state the
// cluster was left in): a pause when it does not come, because a standby is promoted on that proof only.
func (r *Runner) waitProof(ctx context.Context) (checkpoint, state string, err error) {
	start := r.now()
	deadline := start.Add(dur(r.ProofWait, 4*time.Minute))
	for {
		cur, ok := r.Journal.Current()
		if ok && cur.Planned != nil && cur.Planned.Checkpoint != "" {
			return cur.Planned.Checkpoint, cur.Planned.ClusterState, nil
		}
		// The old leader's task sends the proof; when it does not (its kingresd ended after the clean stop and is not back), the proof is read from its server:
		// the primary does not run there, and what pg_controldata says is checked by the standby against what it replayed, as for the proof that is sent.
		if ok && r.now().Sub(start) >= dur(r.ProofPullAfter, 20*time.Second) {
			if cp, st, perr := r.P.PullProof(ctx, cur.OldServer); perr == nil && st == "shut down" && cp != "" {
				if serr := r.Journal.SetProof(cp, st); serr == nil {
					_ = r.Journal.Event(journal.StepPromote, "proofRead", map[string]any{"from": cur.OldServer, "checkpoint": cp})
					return cp, st, nil
				}
			}
		}
		if !r.now().Before(deadline) {
			return "", "", &failover.PauseError{Reason: "the old leader of the map group did not deliver the proof of its clean stop; nothing was promoted. " +
				"Start the old leader's server again, or repeat the request"}
		}
		if err := r.sleep(ctx, dur(r.LeasePoll, time.Second)); err != nil {
			return "", "", err
		}
	}
}

// HardInProgress says that a hard failover of the map group of oldServer, begun by the old leader's task, is in progress on this server: the request of
// an operator that repeats it (a pause for a loss that was not accepted) is taken here.
func (r *Runner) HardInProgress(oldServer string) bool { return r.hardInProgress(oldServer) }
