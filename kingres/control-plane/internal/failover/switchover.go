// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// The planned switchover (design ha-phase5-part2.md, section 5, mode graceful). The steps replace F1 to F6 of a failover: the old primary
// stops writing, hands over its WAL, and the new primary is promoted only when it is proven to hold all of it; F7 to F13 are the same.
// The point of no return is the promotion of the standby; until then the data of the old primary is the cluster's truth and a failure or
// a cancel rolls back (rollbackSwitchover).

// Step names of the switchover.
const (
	StepQuiesce   = "Stop the writes"
	StepDrain     = "Let distributed transactions end"
	StepStopClean = "Stop the old primary cleanly"
	StepVerify    = "Verify the new primary"
)

// switchoverSteps is the order of the steps of one group in mode graceful.
var switchoverSteps = []string{StepFreeze, StepChoose, StepQuiesce, StepDrain, StepStopClean, StepVerify, StepLease, StepHandOver, StepPromote,
	StepCatalog, StepCheckCat, StepRepoint, StepRecords, StepSyncNames, StepPrepared, StepClients, StepUnfreeze}

// switchoverStepNames are the names of a switchover's steps, for the plan.
func switchoverStepNames() []string {
	out := []string{StepCheck}
	out = append(out, switchoverSteps...)
	return append(out, StepWriteReport)
}

// graceful tells whether the input is a planned switchover (a restore is a switchover per group).
func (in Input) graceful() bool { return in.Mode == ModeGraceful || in.Mode == ModeRestore }

// addGraceful appends the steps of one group of a switchover.
func (r *Runner) addGraceful(out *[]task.Step, g int32) {
	add := func(name string, irreversible bool, f stepFunc) {
		st := r.step(StepName(g, name), g, name, irreversible, f)
		// A cancel ends a step before the promotion while it runs, not at its end (F4-08); the promotion and what follows are never interrupted.
		st.Interruptible = !pastPromotion(name)
		*out = append(*out, st)
	}
	add(StepFreeze, false, r.freeze)
	add(StepChoose, false, r.chooseGraceful)
	add(StepQuiesce, false, r.quiesce)
	add(StepDrain, false, r.drain)
	add(StepStopClean, false, r.stopClean)
	add(StepVerify, false, r.verify)
	add(StepLease, false, r.takeFence)
	add(StepHandOver, false, r.handOver)
	add(StepPromote, true, r.promote)
	add(StepCatalog, false, r.catalogFollows)
	add(StepCheckCat, false, r.checkCatalog)
	add(StepRepoint, false, r.repoint)
	add(StepRecords, false, r.records)
	add(StepSyncNames, false, r.syncNames)
	add(StepPrepared, false, r.prepared)
	add(StepClients, false, r.clients)
	add(StepUnfreeze, false, r.unfreeze)
}

// pauseKey is the key of the time at which the writes of a group stopped (kept in memory: a restart of the leader loses only the
// measurement of the pause, never a decision).
func pauseKey(taskID int64, g int32) string { return fmt.Sprintf("%d/%d", taskID, g) }

// chooseGraceful pins the standby that takes over: the one the request named, or the best one that streams from the old primary
// (design S0). The verdict is none by construction: the switchover completes with a proof or does not happen.
func (r *Runner) chooseGraceful(ctx context.Context, env *task.Env, sc *stepCtx) error {
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	gr, err := sc.row(ctx)
	if err != nil {
		return err
	}
	if gr.ChosenStandby != 0 {
		return task.Skip("the standby was chosen already")
	}
	recs, err := r.Plan.Standbys(ctx, sc.group)
	if err != nil {
		return err
	}
	var ready []StandbyRec
	for _, s := range recs {
		if s.State == "ready" {
			ready = append(ready, s)
		}
	}
	if len(ready) == 0 {
		return fail(CodeNoCandidate, true, "shard group %d has no ready standby", sc.group)
	}
	reads := r.readPositions(ctx, sc.group, ready)
	cands := make([]Candidate, 0, len(reads))
	for _, c := range reads {
		cd := Candidate{StandbyID: c.rec.ID, Server: c.rec.Server, Readable: c.err == nil, Pos: c.pos,
			Preferred: r.Plan.Preferred(ctx, sc.group, c.rec.Server), RingDist: r.Plan.Ring(sc.old(), c.rec.Server)}
		switch {
		case c.err != nil:
			cd.Err = c.err.Error()
		case c.pos.ReceiverState != "streaming":
			// A standby that does not stream from the old primary cannot be proven to hold what the primary wrote.
			cd.Readable, cd.Err = false, "it does not stream from the primary"
		}
		cands = append(cands, cd)
	}
	ch := Choose(ChooseInput{Candidates: cands, Target: sc.in.Targets[sc.group]})
	decision, _ := marshalDecision(ch, GroupFacts{SyncStreaming: true}, cands)
	if ch.Chosen == nil {
		return fail(CodeNoCandidate, true, "no standby of shard group %d streams from its primary and can take over: %s", sc.group, ch.Message)
	}
	dl, _ := json.Marshal(DataLoss{Verdict: VerdictNone, Message: "A switchover loses nothing: the new primary is proven to hold everything the old one wrote, or the switchover does not happen.",
		AsyncDatabases: []string{}, UnreadableServerIDs: []string{}})
	var chosen StandbyRec
	for _, s := range ready {
		if s.ID == ch.Chosen.StandbyID {
			chosen = s
		}
	}
	pinned, _, err := sc.rows.Pin(ctx, sc.group, chosen, decision, dl)
	if err != nil {
		return err
	}
	return sc.afterPin(ctx, pinned)
}

// rollbackSwitchover undoes what a switchover did to the old primary, as far as the promotion has not happened (design 5.3). Every part
// is idempotent and best effort: the leases of the run are released, the writes are let in again and the old primary, when it was
// stopped, is started again by its lease manager (the wait marker). It never runs for a group that was promoted.
func (r *Runner) rollbackSwitchover(ctx context.Context, taskID int64, in Input, g int32, newServer string, term int64) (handedOver bool) {
	ctx = context.WithoutCancel(ctx)
	if g == 0 && newServer != "" {
		// The bootstrap of the candidate that this task started is asked FIRST (F5-03): it is given up (and lets go of its lease), or it answers that it promoted or
		// may have, in which case nothing is rolled back: the old leader stays held back and the candidate goes on. Only then are the leases released and the old
		// leader resumed.
		if err := r.Peers.MapAbandon(ctx, newServer, "the planned switchover of the map group was rolled back"); err != nil {
			if IsCode(err, "conflict") {
				r.log().Warn("failover: the candidate of the map group promoted; the old leader stays held back and nothing is rolled back", "server", newServer)
				return true
			}
			// An answer other than a conflict (the candidate cannot be reached) does not say that nothing was promoted, and the rows that would say so are in the
			// stopped system database (F6-04). The rollback goes on all the same, and that is safe against two primaries only because of the lease: the candidate's
			// own kingresd renews its lease and attaches a promoted instance to it, so this server's primary, started again, cannot win the lease from a promoted
			// candidate and waits for it (the wait marker). The risk is therefore named in the log and never taken silently.
			r.log().Warn("failover: the bootstrap of the map group could not be given up and it is not known whether the candidate promoted; the old leader is started again and the lease decides which of the two runs",
				"server", newServer, "error", err)
		}
	}
	_, _ = r.Peers.Lease(ctx, r.Plan.Self(), LeaseRequest{Group: g, Role: LeaseRelease})
	if newServer != "" && newServer != r.Plan.Self() {
		_, _ = r.Peers.Lease(ctx, newServer, LeaseRequest{Group: g, Role: LeaseRelease})
	}
	// An error is logged, never swallowed: a server that cannot be reached keeps its hold and its marker, and the loop of Repair undoes them when
	// the server answers again (F4-02), whatever became of this task. The map group is not in that loop: its old leader resolves a hold that nothing owns
	// by itself (F6-01, resolveHeldBack of kingresd). POST /failover/resume stays leader-only on purpose, unlike unquiesce, unfreeze and state: it starts a
	// primary, which a held-back server must not do on the word of its peers; it answers 503 until its instance can be read, and Repair repeats it.
	if err := r.Peers.Resume(ctx, in.oldOf(g), ResumeRequest{Group: g, ExpectedTerm: term}); err != nil {
		r.log().Warn("failover: the old primary could not be resumed; the repair of switchovers will try again", "group", g, "server", in.oldOf(g), "error", err)
	}
	if err := r.Peers.Unquiesce(ctx, in.oldOf(g), QuiesceRequest{Group: g, ExpectedTerm: term}); err != nil {
		r.log().Warn("failover: the writes could not be let in again; the repair of switchovers will try again", "group", g, "server", in.oldOf(g), "error", err)
	}
	r.mu.Lock()
	delete(r.paused, pauseKey(taskID, g))
	r.mu.Unlock()
	return false
}

// rollbackFailed rolls back and returns the failure of the step.
func (sc *stepCtx) rollbackFailed(ctx context.Context, code string, retry bool, format string, a ...any) error {
	// The group row is not readable after the clean stop of the map group (the system database is the stopped instance), so the server of the new primary comes
	// from the step when it knows it (F5-01); a term that cannot be read is -1 on purpose: the calls of the rollback then do not judge by it.
	newServer := sc.newServer
	gr, gerr := sc.row(ctx)
	if newServer == "" {
		newServer = gr.NewServer
	}
	if gerr == nil && (gr.State == "promoted" || gr.State == "done") {
		// A group that was promoted is never rolled back (F5-06); the step fails with its own error.
		sc.r.log().Warn("failover: a rollback was asked for a group that was promoted; nothing was undone", "group", sc.group, "code", code)
		return fail(code, retry, format, a...)
	}
	term := int64(-1)
	if run, err := sc.runRow(ctx); err == nil {
		term = run.ExpectedTerm
	}
	if sc.r.rollbackSwitchover(ctx, sc.env.Task.ID, sc.in, sc.group, newServer, term) {
		// The candidate answered that it promoted (or may have): the group is handed over, not rolled back, and the step ends as the hand-over it is (F5-03).
		sc.event(ctx, "handedOver", map[string]any{"code": code})
		return nil
	}
	sc.event(ctx, "rolledBack", map[string]any{"code": code})
	return fail(code, retry, format, a...)
}

// quiesce stops the writes of the group on the old primary (design S2): the entry point stops sending connections to it, the access
// rules are replaced by the hold rules, and every other session is ended. The callee answers when none is left; the step repeats the
// call for two minutes and then rolls back.
func (r *Runner) quiesce(ctx context.Context, env *task.Env, sc *stepCtx) error {
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	run, rerr := sc.runRow(ctx)
	if rerr != nil {
		return rerr
	}
	qterm := run.ExpectedTerm
	deadline := r.now().Add(orDur(r.QuiesceWait, 2*time.Minute))
	for {
		ans, err := r.Peers.Quiesce(ctx, sc.old(), QuiesceRequest{Group: sc.group, ExpectedTerm: qterm})
		if err == nil && ans.Quiesced {
			r.mu.Lock()
			if r.paused == nil {
				r.paused = map[string]time.Time{}
			}
			if _, ok := r.paused[pauseKey(env.Task.ID, sc.group)]; !ok {
				r.paused[pauseKey(env.Task.ID, sc.group)] = r.now()
			}
			r.mu.Unlock()
			sc.event(ctx, "quiesced", nil)
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !r.now().Before(deadline) {
			why := "sessions remain"
			if err != nil {
				why = err.Error()
			}
			return sc.rollbackFailed(ctx, CodeCannotQuiesce, true, "the writes of shard group %d on %s could not be stopped (%s); nothing was changed", sc.group, sc.old(), why)
		}
		if err == nil {
			sc.detail(ctx, "waiting for %d sessions to end", ans.Remaining)
		}
		if err := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); err != nil {
			return err
		}
	}
}

// drain waits until no prepared transaction of the extension's distributed commits is left on the old primary (design S3), asking the map
// group to recover them every ten seconds.
func (r *Runner) drain(ctx context.Context, env *task.Env, sc *stepCtx) error {
	deadline := r.now().Add(orDur(r.DrainWait, time.Minute))
	last := r.now().Add(-time.Hour)
	for {
		st, err := r.Peers.State(ctx, sc.old(), sc.group)
		if err == nil && (st.Prepared == nil || *st.Prepared == 0) {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r.now().Sub(last) >= orDur(r.DrainEvery, 10*time.Second) {
			last = r.now()
			for _, db := range r.catalogDatabases(ctx, sc) {
				_ = r.Cat.RecoverPrepared(ctx, db)
			}
		}
		if !r.now().Before(deadline) {
			return sc.rollbackFailed(ctx, CodePreparedPending, true, "distributed transactions of shard group %d did not end within the bound; nothing was changed", sc.group)
		}
		sc.detail(ctx, "waiting for prepared transactions to end")
		if err := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); err != nil {
			return err
		}
	}
}

// stopClean marks the position, waits until the new primary has replayed the mark, and stops the old primary with a fast shutdown (design
// S4 and S5). The call that stops it releases the lease and answers the position of the shutdown checkpoint.
func (r *Runner) stopClean(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, _, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	sc.newServer = nw.Server
	if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
		return herr
	} else if handed {
		return task.Skip("the map group was handed over already")
	}
	if sc.group == 0 {
		// The bootstrap of the candidate starts first: it waits for the lease that the clean stop releases and for the proof that this step sends once the old
		// leader is stopped. A request that was made before (a repeated run) finds the journal in progress.
		if err := r.Peers.MapStart(ctx, nw.Server, MapStartRequest{Server: sc.old(), RequestedBy: sc.in.RequestedBy, Comment: sc.in.Comment,
			Planned: &MapPlanned{TaskID: env.Task.ID, StandbyID: nw.ID}}); err != nil {
			if IsCode(err, "conflict") {
				// The candidate gave this very switchover up: it was rolled back while the system database was stopped, so the engine could not record it and runs
				// the step again. It ends as the rollback it was, with the code of a switchover that could not be proven.
				return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the planned switchover of the map group was given up and rolled back before; the old leader was started again (%v)", err)
			}
			return sc.rollbackFailed(ctx, CodeTargetUnavailable, true, "the bootstrap of the map group could not be started on %s: %v", nw.Server, err)
		}
	}
	// A repeat after the stop finds no primary to mark; the stop below is idempotent and reads the proof again.
	mk, merr := r.Peers.Mark(ctx, sc.old(), MarkRequest{Group: sc.group, ExpectedTerm: run.ExpectedTerm})
	if merr == nil {
		deadline := r.now().Add(orDur(r.MarkWait, 10*time.Second))
		for {
			v, verr := r.Peers.Verify(ctx, nw.Server, VerifyRequest{Group: sc.group, InstanceID: nw.InstanceID, Mark: mk.Mark})
			if verr == nil && v.Ok {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !r.now().Before(deadline) {
				return sc.rollbackFailed(ctx, CodeStandbyBehind, true, "the standby on %s did not replay the position %s of the old primary within the bound; nothing was changed", nw.Server, mk.Mark)
			}
			if err := r.sleep(ctx, 500*time.Millisecond); err != nil {
				return err
			}
		}
	} else if st, serr := r.Peers.State(ctx, sc.old(), sc.group); !IsCode(merr, "not_primary") && !(serr == nil && len(st.OutOfRecovery()) == 0) {
		return sc.rollbackFailed(ctx, CodeStandbyBehind, true, "the position of the old primary of shard group %d could not be marked: %v", sc.group, merr)
	}
	ans, err := r.Peers.Demote(ctx, sc.old(), DemoteRequest{Group: sc.group, Mode: "fast", ExpectedTerm: run.ExpectedTerm, Candidate: sc.candidateOf(nw)})
	if err != nil {
		if IsCode(err, "stale_term") {
			return sc.rollbackFailed(ctx, CodeSuperseded, false, "another failover changed the catalog term of the cluster")
		}
		return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the old primary of shard group %d could not be stopped cleanly: %v", sc.group, err)
	}
	if !ans.Stopped || ans.ClusterState != "shut down" || ans.Checkpoint == "" || !ans.Released {
		return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the stop of the old primary of shard group %d was not clean (state %q, released %v)", sc.group, ans.ClusterState, ans.Released)
	}
	sc.event(ctx, "stoppedClean", map[string]any{"checkpoint": ans.Checkpoint, "clusterState": ans.ClusterState})
	if sc.group == 0 {
		// The old leader is stopped: nothing can be recorded by this task until the new leader's engine claims it. The proof goes to the candidate, which
		// promotes on it after the standby has checked it against what it replayed.
		// The lease of the task is in the stopped system database and cannot be renewed, so the engine cancels the context of the step ten seconds later. What
		// follows must not end with it (found by the cluster tests of the review of round 2): the old primary is held back, the candidate may be given up or may
		// never promote, and only this step can say that the switchover is rolled back. It runs on a context that is not cancelled, bounded by the verdict wait.
		ctx = context.WithoutCancel(ctx)
		var perr error
		for i := 0; i < 5; i++ {
			if perr = r.Peers.MapProof(ctx, nw.Server, MapProofRequest{Checkpoint: ans.Checkpoint, ClusterState: ans.ClusterState}); perr == nil {
				break
			}
			if err := r.sleep(ctx, time.Second); err != nil {
				return err
			}
		}
		if perr != nil {
			// The candidate cannot take the proof (it was given up, or it cannot be reached): the rollback asks it first and starts the old leader again.
			return sc.rollbackFailed(ctx, CodeTargetUnavailable, true, "the proof of the clean stop could not be sent to %s: %v", nw.Server, perr)
		}
		// The verdict: the candidate promotes on the proof (the standby is out of recovery), or it refuses (the standby cannot prove the stop) and tells this server.
		// A refusal rolls this task back: the old leader's primary is started again by its lease manager, which wins the lease because the candidate let go
		// of it; a candidate that says nothing is waited for, and the lease decides who may run.
		deadline := r.now().Add(orDur(r.MapVerdictWait, 6*time.Minute))
		for {
			if reason, refused := r.Peers.MapRefusal(ctx, env.Task.ID); refused {
				return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the standby of the map group could not prove the clean stop of the old leader (%s); the old leader is started again", reason)
			}
			if st, serr := r.Peers.State(ctx, nw.Server, sc.group); serr == nil && len(st.OutOfRecovery()) > 0 {
				return nil
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !r.now().Before(deadline) {
				return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the candidate of the map group did not promote after the clean stop of the old leader; the old leader is started again")
			}
			if err := r.sleep(ctx, time.Second); err != nil {
				return err
			}
		}
	}
	return nil
}

// verify asks the new primary's server for the proof (design S6): switchover_verify(X, state) must be true; it is repeated for 30 s while
// the standby may still be receiving. A proof that can never be made, or one that stays false, rolls back: a switchover never turns itself
// into a failover.
func (r *Runner) verify(ctx context.Context, env *task.Env, sc *stepCtx) error {
	run, err := sc.runRow(ctx)
	if err != nil {
		return err
	}
	nw, _, err := sc.pinnedStandby(ctx)
	if err != nil {
		return err
	}
	if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
		return herr
	} else if handed {
		return task.Skip("the map group was handed over already")
	}
	// Read the shutdown checkpoint again (the stop is idempotent): it is what the proof is made of.
	ans, err := r.Peers.Demote(ctx, sc.old(), DemoteRequest{Group: sc.group, Mode: "fast", ExpectedTerm: run.ExpectedTerm, Candidate: sc.candidateOf(nw)})
	if err != nil || !ans.Stopped || ans.ClusterState != "shut down" || ans.Checkpoint == "" {
		return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the shutdown checkpoint of the old primary of shard group %d cannot be read (%v)", sc.group, err)
	}
	deadline := r.now().Add(orDur(r.VerifyWait, 30*time.Second))
	for {
		v, verr := r.Peers.Verify(ctx, nw.Server, VerifyRequest{Group: sc.group, InstanceID: nw.InstanceID, Checkpoint: ans.Checkpoint, ClusterState: ans.ClusterState})
		switch {
		case verr == nil && v.Ok:
			sc.event(ctx, "verified", map[string]any{"checkpoint": ans.Checkpoint})
			return nil
		case IsCode(verr, "unprovable"):
			return sc.rollbackFailed(ctx, CodeSwitchoverUnproven, true, "the switchover of shard group %d cannot be proven: %v", sc.group, verr)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !r.now().Before(deadline) {
			return sc.rollbackFailed(ctx, CodeStandbyBehind, true, "the standby on %s does not hold everything the old primary of shard group %d wrote (%s); it was not promoted", nw.Server, sc.group, ans.Checkpoint)
		}
		if err := r.sleep(ctx, time.Second); err != nil {
			return err
		}
	}
}

// writesResumed records how long the writes of the group were stopped, when clients arrive at the new primary.
func (r *Runner) writesResumed(ctx context.Context, sc *stepCtx) {
	if !sc.in.graceful() {
		return
	}
	r.mu.Lock()
	t, ok := r.paused[pauseKey(sc.env.Task.ID, sc.group)]
	delete(r.paused, pauseKey(sc.env.Task.ID, sc.group))
	r.mu.Unlock()
	if ok {
		sc.event(ctx, "writesResumed", map[string]any{"pauseMs": r.now().Sub(t).Milliseconds()})
	}
}

// preflightGraceful is what a switchover needs besides the checks of every failover (design S0): the server is live and runs the
// primary of each group under its lease, and a standby of each group streams from it (and is the target when one was named).
func (r *Runner) preflightGraceful(ctx context.Context, in Input) *Refusal {
	for _, g := range in.Groups {
		g := g
		if g == 0 && in.oldOf(0) != r.Plan.Self() {
			return &Refusal{Code: "unsupported_operation", Status: 422, Group: &g,
				Message: "The map group is handed over by the task that runs on the server of its primary (the manager leader); ask the leader."}
		}
	}
	for _, g := range in.Groups {
		g := g
		old := in.oldOf(g)
		if r.Plan.Live(ctx, old) != "live" {
			return &Refusal{Code: "node_state_invalid", Status: 422, Group: &g, Message: "Server " + old + " is not live; a planned switchover needs a live server (use mode hard)."}
		}
		st, err := r.Peers.State(ctx, old, g)
		if err != nil || len(st.OutOfRecovery()) == 0 || !st.Holds {
			return &Refusal{Code: "node_state_invalid", Status: 422, Group: &g,
				Message: fmt.Sprintf("The primary of shard group %d does not run under its lease on %s; a planned switchover needs a running primary.", g, old)}
		}
		recs, err := r.Plan.Standbys(ctx, g)
		if err != nil {
			return &Refusal{Code: "service_unavailable", Status: 503, Message: "The standbys cannot be read: " + err.Error()}
		}
		streaming := false
		for _, s := range recs {
			if s.State != "ready" || (in.Targets[g] != "" && s.Server != in.Targets[g]) {
				continue
			}
			cctx, cancel := context.WithTimeout(ctx, orDur(r.PositionTimeout, 5*time.Second))
			p, perr := r.Peers.Position(cctx, s.Server, g)
			cancel()
			if perr == nil && p.InRecovery && p.ReceiverState == "streaming" {
				streaming = true
			}
		}
		if !streaming {
			return &Refusal{Code: "group_degraded", Status: 409, Group: &g,
				Message: fmt.Sprintf("No standby of shard group %d streams from its primary, so the group cannot be handed over without a loss; repair the replication first.", g)}
		}
	}
	return nil
}

// rollbackUnpromoted is the cleanup of a switchover that the engine recorded as failed or cancelled: every group that was begun and not
// promoted is rolled back (the steps that fail with a code have done it already; a cancel and a crash have not).
func (r *Runner) rollbackUnpromoted(ctx context.Context, t task.Task, in Input) {
	rows := r.NewRows(t.ID)
	for _, g := range in.Groups {
		gr, ok, err := rows.Group(ctx, g)
		if err != nil || !ok {
			continue
		}
		switch gr.State {
		case "pending", "promoted", "done", "failed", "skipped":
			continue
		}
		// A step from the promotion on that failed is past the point of no return (design 5.3, F4-03): the promote call may have taken effect
		// although it did not answer (promote_timeout), so the old primary is not resumed and no lease is released.
		if t.Error != nil && failedPastPromotion(t.Error.Step, g) {
			r.log().Warn("failover: not rolling back a group whose promotion began", "group", g, "step", t.Error.Step)
			continue
		}
		term := int64(-1)
		if run, rerr := runRowOf(ctx, rows, in, g); rerr == nil {
			term = run.ExpectedTerm
		}
		_ = r.rollbackSwitchover(ctx, t.ID, in, g, gr.NewServer, term)
	}
}

// pastPromotion says whether the step key is the promotion or one of the steps after it.
func pastPromotion(key string) bool {
	seen := false
	for _, k := range switchoverSteps {
		if k == StepPromote {
			seen = true
		}
		if k == key {
			return seen
		}
	}
	return false
}

// failedPastPromotion says whether the step name ("Group 3: Promote the standby") is a step of group g from the promotion on.
func failedPastPromotion(step string, g int32) bool {
	prefix := StepName(g, "")
	if !strings.HasPrefix(step, prefix) {
		return false
	}
	return pastPromotion(strings.TrimPrefix(step, prefix))
}

// restoreContinues turns the failure of a step of one group of a restore into a skip, when the group was not promoted: the group is
// rolled back, recorded failed with the reason, and the run goes on with the next group (design 5.5, 4). A failure after the promotion,
// a pause, a lost lease and a stop of the context are not turned: they are the task's.
func (sc *stepCtx) restoreContinues(ctx context.Context, err error) error {
	var fe *Error
	if !errors.As(err, &fe) || ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return nil
	}
	if pastPromotion(sc.key) {
		return nil // from the promotion on a failure is the task's (F4-03)
	}
	gr, ok, gerr := sc.rows.Group(ctx, sc.group)
	if gerr != nil || !ok {
		return nil
	}
	switch gr.State {
	case "pending", "frozen", "fenced", "pinned":
	default:
		return nil
	}
	term := int64(-1)
	if run, rerr := sc.runRow(ctx); rerr == nil {
		term = run.ExpectedTerm
	}
	_ = sc.r.rollbackSwitchover(ctx, sc.env.Task.ID, sc.in, sc.group, gr.NewServer, term)
	_ = sc.r.Peers.Unfreeze(ctx, sc.old(), sc.group)
	text := fe.Code + ": " + fe.Message
	if serr := sc.rows.Set(ctx, sc.group, GroupSet{State: ptr("failed"), Error: &text, Finished: true}); serr != nil {
		return nil
	}
	sc.event(ctx, "groupFailed", map[string]any{"code": fe.Code, "message": fe.Message})
	sc.r.Plan.Audit(ctx, "failoverGroupFailed", sc.group, map[string]any{"error": text})
	return task.Skip("the group failed and was rolled back: " + text)
}

// mapHandedOver says that the map group of this task was handed over already: the standby the old leader's task chose is out of recovery (the
// bootstrap of the candidate promoted it). The steps of the map group that the old leader's task could not record (its instance is the system
// database, and it was stopped) run again on the new leader's engine, and each of them that would act on the old leader or on the promotion
// skips when this is true.
func (sc *stepCtx) mapHandedOver(ctx context.Context, nw StandbyRec) (bool, error) {
	if sc.group != 0 {
		return false, nil
	}
	// A state that cannot be read is not "not handed over" (F5-06): the steps that are right only when the group was not handed over (the start of a bootstrap,
	// the clean stop) must not run on a failed read, so the read is repeated and then the step fails and is retried.
	var err error
	for i := 0; i < 3; i++ {
		var st State
		if st, err = sc.r.Peers.State(ctx, nw.Server, sc.group); err == nil {
			return len(st.OutOfRecovery()) > 0, nil
		}
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		if serr := sc.r.sleep(ctx, time.Second); serr != nil {
			return false, serr
		}
	}
	return false, err
}

// mapLast is the order of the groups of a planned switchover, and of a hard failover that includes the map group of a live leader's server: the map group
// is the last. Its hand-over ends the old leader's task (its system database is stopped), and the new leader's engine finishes what is left, so nothing
// after it may depend on the old leader. Any other hard failover keeps the order of the groups as they are.
func mapLast(in Input) []int32 {
	if !in.graceful() && !in.MapLive {
		return in.Groups
	}
	out := make([]int32, 0, len(in.Groups))
	map0 := false
	for _, g := range in.Groups {
		if g == 0 {
			map0 = true
			continue
		}
		out = append(out, g)
	}
	if map0 {
		out = append(out, 0)
	}
	return out
}

// mapHandedOverByTheCandidate says that this step is a step of the map group whose hand-over is made by the bootstrap of the candidate's server: a
// planned switchover (and a restore) and a hard failover of a live leader's server. Its steps run again on the new leader's engine and skip what the
// bootstrap did.
func (sc *stepCtx) mapHandedOverByTheCandidate() bool {
	return sc.group == 0 && (sc.in.graceful() || sc.in.MapLive)
}

// candidateOf is the server that the old leader records as the one the map group is handed to; the other groups record none.
func (sc *stepCtx) candidateOf(nw StandbyRec) string {
	if sc.group != 0 {
		return ""
	}
	return nw.Server
}
