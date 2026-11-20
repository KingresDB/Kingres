// SPDX-License-Identifier: AGPL-3.0-only

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
	// CheckAuthority says whether this server holds the key of the certificate authority (pki/ca.key). The leader holds it and no other
	// server does: the new leader of a map group failover has none unless an operator put it there (design 3.4, 11.2).
	CheckAuthority(ctx context.Context) (bool, error)
	// MoveSystem points what this server reads the system database with (the task engine, the store) at the promoted instance: from
	// here on this server is the leader and its system database is the instance it promoted.
	MoveSystem(ctx context.Context) error
	Alert(ctx context.Context, kind, severity, subject, message string, raise bool)
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
}

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

	FreezeTTL     time.Duration // default 10 min
	LeaseWait     time.Duration // the wait for the old lease, default 25 s
	LeasePoll     time.Duration // default 1 s
	QuorumWait    time.Duration // default 60 s
	ReadBound     time.Duration // the positions must stop moving within this, default 15 s
	PromoteWait   time.Duration // the promoted instance is waited for this long, default 3 min
	CatalogWait   time.Duration // promote_node of the map group, repeated this long while the majority rule refuses, default 10 min
	CatalogPause  time.Duration // default 5 s
	LeaderWait    time.Duration // the other servers' answers show the new term within this, default 60 s
	AlertAfter    time.Duration // failoverBlocked is raised after this, default 60 s
	PromoteNodeOp failover.PromoteOptions
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
		// The old server answers the liveness view: a partition on the leader's side, or a request of an operator for a switchover of the
		// leader. Only an operator may go on: an automatic start for a server that is heard is refused (F4-05). B3 asks it to stop first.
		if nonEmpty(req.Trigger, "manual") == "automatic" {
			return journal.Journal{}, &Refusal{Code: "conflict", Status: 409, Message: "The old server of the map group is live for this server; an automatic failover does not start."}
		}
		r.logf("mapgroup: the old server %s is live for this server; B3 asks it to demote", req.OldServer)
	}
	id := strconv.FormatInt(r.now().UnixNano(), 36)
	return r.Journal.Begin(id, req.OldServer, term, nonEmpty(req.Trigger, "manual"), req.RequestedBy, req.AllowDataLoss)
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
		if err := r.fault(s.name + ":before"); err != nil {
			return res, err
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

// B3: as F3. A old server that answers is asked to demote first (design 3.5); then the fence lease is taken from a majority of all
// the servers, which proves that a majority talks to this server, that the old holder's lease ran out at a majority and that it
// can never be granted one again.
func (r *Runner) takeFence(ctx context.Context, j journal.Journal) (bool, error) {
	if st, err := r.P.State(ctx, j.OldServer); err == nil && len(st.OutOfRecovery()) > 0 {
		ans, derr := r.P.Demote(ctx, j.OldServer, failover.DemoteRequest{Group: Group, Mode: "immediate", ExpectedTerm: j.Term, TransferTo: r.P.Self()})
		_ = r.Journal.Event(journal.StepLease, "demote", map[string]any{"stopped": ans.Stopped, "transferred": ans.Transferred, "error": errText(derr)})
	}
	members, err := r.P.Members(ctx)
	if err != nil {
		return false, err
	}
	started := r.now()
	var quorumSince time.Time
	for {
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
			switch {
			case err != nil && failover.IsCode(err, "stale_term"):
				return false, errors.New("another failover changed the catalog term of the cluster; nothing else is done")
			case err == nil && ans.Held:
				waited := int(r.now().Sub(started).Milliseconds())
				return false, r.Journal.Event(journal.StepLease, "leaseHeld", map[string]any{"waitMs": waited})
			case err == nil && r.now().Sub(started) > dur(r.LeaseWait, 25*time.Second):
				for h := range ans.RefusedBy {
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
	for i := range cands {
		cands[i].Preferred = false
	}
	ch := failover.Choose(failover.ChooseInput{Candidates: cands, Facts: facts})
	if ch.Chosen == nil {
		return false, failover.Fail(failover.CodeNoCandidate, true, "no standby of the map group can be promoted: %s", ch.Message)
	}
	if !failover.Accepts(ch.Verdict, j.AllowDataLoss) {
		r.P.Alert(ctx, "failoverNeedsDecision", "critical", "group:0", "The failover of the map group could lose data: "+ch.Message, true)
		return false, &failover.PauseError{Reason: fmt.Sprintf("The failover of the map group could lose data (%s): %s Repeat the request with allowDataLoss set to true if you accept it.", ch.Verdict, ch.Message)}
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
		if err := r.P.Continue(ctx, pinned.Server, cur); err != nil {
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
		if ans, err := r.P.Lease(ctx, r.P.Self(), failover.LeaseRequest{Group: Group, Role: failover.LeaseHolder, ExpectedTerm: j.Term}); err == nil && ans.Held {
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
	for try := 0; ; try++ {
		ans, err := r.P.Promote(ctx, r.P.Self(), req)
		switch {
		case err == nil:
			return false, r.Journal.Event(journal.StepPromote, "promoted", map[string]any{"status": ans.Status, "timeline": ans.Timeline, "lsn": ans.Lsn})
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
		id, err := r.P.RecordTask(ctx, cur)
		if err != nil {
			return false, fmt.Errorf("recording the task of the failover: %w", err)
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
	// The new leader issues and revokes certificates only with ca.key. The key is not escrowed (the construction is an open point of
	// the design): when this server has none, the product says so and what the operator must do, and the renewal jobs of the other
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

// AuthorityMessage is what the alert says: the key is not escrowed, so the operator restores it by hand.
const AuthorityMessage = "The map group was failed over and this server, the new manager leader, does not hold the key of the certificate authority: no certificate can be issued, renewed or revoked. Restore pki/ca.key and pki/revoked.json of the old leader from its backup into the pki directory of this server and restart kingresd; the certificates of the servers last a year, and their renewal fails until then."
