// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package failover

import (
	"context"
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// stopOldMap is the step "Stop the old primary" of the map group in a hard failover of a live leader's server (design ha-phase5-part2.md, "a hard
// failover of a live leader's server"). It is the last thing this task does on the old leader: the old primary of the map group is the system database that
// holds the task, so once it is stopped nothing can be recorded here and the standby's server finishes the work.
//
// The order is what makes it safe, and it is the order of the failover of any other group with a live old server, with the lease in the hands of the
// candidate instead of the task's server:
//
//  1. Nothing is changed before the loss is judged: a verdict that the operator did not accept pauses the task here, with the old leader running.
//  2. The bootstrap of the candidate starts and waits at B3 for the lease. It cannot promote before it holds the lease of the map group at a majority.
//  3. The old primary is fenced: the demote call ends its lease renewal and writes the marker that keeps it from starting (before anything is stopped),
//     stops it with an immediate shutdown, and only when it is stopped hands the lease to the candidate. When the stop did not happen, or the hand-over did
//     not, the lease is no longer renewed and runs out, the watchdog stops the primary with it, and the candidate takes the lease at B3 after that.
//  4. The candidate promotes after B3 and ranks the standbys then (the positions have stopped moving), under the verdict of the facts read in step 1.
//
// A step that finds its standby promoted already (the engine of the new leader runs it again) ends at once.
func (r *Runner) stopOldMap(ctx context.Context, env *task.Env, sc *stepCtx, run RunRow) error {
	if nw, _, perr := sc.pinnedStandby(ctx); perr == nil {
		if handed, herr := sc.mapHandedOver(ctx, nw); herr != nil {
			return herr
		} else if handed {
			return task.Skip("the map group was handed over already")
		}
	}
	if err := sc.ensureFrozen(ctx); err != nil {
		return err
	}
	recs, err := r.Plan.Standbys(ctx, 0)
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
		return fail(CodeNoCandidate, true, "the map group has no ready standby, so its primary on %s was not stopped; nothing was changed", sc.old())
	}
	ch, facts, err := r.judgeMapHard(ctx, sc, ready)
	if err != nil {
		return err
	}
	if ch.Chosen == nil {
		return fail(CodeNoCandidate, true, "no standby of the map group can be promoted: %s; its primary on %s was not stopped and nothing was changed", ch.Message, sc.old())
	}
	if !Accepts(ch.Verdict, sc.in.AllowDataLoss) {
		r.Plan.Alert(ctx, "failoverNeedsDecision", "critical", subjectOf(0, sc.old()), fmt.Sprintf("The failover of the map group could lose data: %s", ch.Message), true)
		return &PauseError{Reason: fmt.Sprintf("The failover of the map group could lose data (%s): %s Nothing was stopped. Repeat the request with allowDataLoss set to true if you accept it.", ch.Verdict, ch.Message)}
	}
	var chosen StandbyRec
	for _, s := range ready {
		if s.ID == ch.Chosen.StandbyID {
			chosen = s
		}
	}
	if chosen.Server == r.Plan.Self() {
		return fail(CodeTargetUnavailable, true, "the standby of the map group on %s is on the server of its primary; nothing was changed", chosen.Server)
	}
	sc.event(ctx, "mapHardStop", map[string]any{"candidate": chosen.Server, "standby": chosen.ID, "verdict": ch.Verdict})
	mf := &MapFacts{SyncStreaming: facts.SyncStreaming && facts.LastKnownAge >= 0 && facts.LastKnownAge <= MaxCheckAge, LastKnownLsn: facts.LastKnownLsn, PrimaryPlanned: facts.PrimaryPlanned, AsyncDatabases: facts.AsyncDatabases}
	if r.Inject != nil && r.Inject("maphard:facts") == "async" {
		// A test: the facts the candidate judges with differ from those this task accepted (a synchronous standby that stopped confirming): the candidate pauses.
		mf.SyncStreaming = false
	}
	// The candidate's bootstrap first: it waits for the lease, which only the stop below hands to it. A request that was made before (a repeated run) finds
	// the journal in progress.
	if err := r.Peers.MapStart(ctx, chosen.Server, MapStartRequest{Server: sc.old(), RequestedBy: sc.in.RequestedBy, Comment: sc.in.Comment, AllowDataLoss: sc.in.AllowDataLoss,
		Planned: &MapPlanned{TaskID: env.Task.ID, StandbyID: chosen.ID, Hard: true, Facts: mf, Target: sc.in.Targets[0]}}); err != nil {
		return fail(CodeTargetUnavailable, true, "the bootstrap of the map group could not be started on %s: %v; nothing was changed", chosen.Server, err)
	}
	// M2: the facts are read again right before the fence (the bootstrap took time to start, and the primary is alive and writing): the verdict that was accepted must still
	// hold, for the same standby, or nothing is stopped and the bootstrap is given up.
	if ch2, f2, jerr := r.judgeMapHard(ctx, sc, ready); jerr != nil || ch2.Chosen == nil || ch2.Chosen.StandbyID != chosen.ID || !Accepts(ch2.Verdict, sc.in.AllowDataLoss) {
		_ = r.Peers.MapAbandon(context.WithoutCancel(ctx), chosen.Server, "the loss changed before the old primary was fenced")
		if jerr != nil {
			return jerr
		}
		msg := "the choice changed"
		if ch2.Chosen != nil {
			msg = ch2.Message
		}
		r.Plan.Alert(ctx, "failoverNeedsDecision", "critical", subjectOf(0, sc.old()), "The failover of the map group could lose data: "+msg, true)
		return &PauseError{Reason: fmt.Sprintf("The loss of the failover of the map group changed before the old primary was stopped (%s). Nothing was stopped. Repeat the request.", msg)}
	} else {
		mf.SyncStreaming = f2.SyncStreaming && f2.LastKnownAge >= 0 && f2.LastKnownAge <= MaxCheckAge && mf.SyncStreaming
		mf.LastKnownLsn, mf.PrimaryPlanned, mf.AsyncDatabases = f2.LastKnownLsn, f2.PrimaryPlanned, f2.AsyncDatabases
	}
	ans, err := r.Peers.Demote(ctx, sc.old(), DemoteRequest{Group: 0, Mode: "immediate", ExpectedTerm: run.ExpectedTerm, TransferTo: chosen.Server, Candidate: chosen.Server})
	if err != nil {
		// The call answers an error only before it ends the lease renewal of the primary (the term is stale, or the marker could not be written): nothing
		// was stopped, so the bootstrap is given up (a transport error that hides a stop is resolved by the old leader's own loop: the candidate is
		// given up and the marker shows the primary held back, which the loop ends when no bootstrap names it and the standby is still a standby).
		_ = r.Peers.MapAbandon(context.WithoutCancel(ctx), chosen.Server, "the old primary of the map group could not be stopped: "+err.Error())
		if IsCode(err, "stale_term") {
			return fail(CodeSuperseded, false, "another failover changed the catalog term of the cluster")
		}
		return fail(CodeTargetUnavailable, true, "the old primary of the map group could not be fenced on %s: %v", sc.old(), err)
	}
	// From here on the system database may be the instance that was stopped: nothing is recorded, and the step must not end with the context of the task,
	// whose lease can no longer be renewed.
	ctx = context.WithoutCancel(ctx)
	if !ans.Stopped {
		actx, acancel := context.WithTimeout(ctx, 5*time.Second) // P5: the system database may be the stopped instance
		defer acancel()
		r.Plan.Alert(actx, "oldPrimaryNotStopped", "critical", subjectOf(0, sc.old()), fmt.Sprintf("The old primary of the map group could not be stopped on %s; its lease is not renewed and the watchdog stops it when it runs out.", sc.old()), true)
	}
	deadline := r.now().Add(orDur(r.MapVerdictWait, 6*time.Minute))
	for {
		for _, s := range ready {
			if st, serr := r.Peers.State(ctx, s.Server, 0); serr == nil && len(st.OutOfRecovery()) > 0 {
				return nil
			}
		}
		if !r.now().Before(deadline) {
			return fail(CodePromoteTimeout, true, "no standby of the map group was promoted after the old primary was fenced; the bootstrap of %s waits for a decision", chosen.Server)
		}
		if err := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); err != nil {
			return err
		}
	}
}

// judgeMapHard reads the positions of the ready standbys of the map group and the leader's facts, and runs the ordinary choice with the target of the request. The primary
// is alive while it runs, so it is called again right before the fence (M2).
func (r *Runner) judgeMapHard(ctx context.Context, sc *stepCtx, ready []StandbyRec) (Choice, GroupFacts, error) {
	cands := make([]Candidate, 0, len(ready))
	for _, rec := range ready {
		cctx, cancel := context.WithTimeout(ctx, orDur(r.PositionTimeout, 5*time.Second))
		p, perr := r.Peers.Position(cctx, rec.Server, 0)
		cancel()
		cd := Candidate{StandbyID: rec.ID, Server: rec.Server, Readable: perr == nil, Pos: p, Preferred: r.Plan.Preferred(ctx, 0, rec.Server), RingDist: r.Plan.Ring(sc.old(), rec.Server)}
		if perr != nil {
			cd.Err = perr.Error()
		}
		cands = append(cands, cd)
	}
	facts, err := r.Plan.Facts(ctx, 0)
	if err != nil {
		return Choice{}, facts, err
	}
	// M2: the primary is alive, so it is asked now, with a time bound, what it streams to whom: that replaces the recorded check, which is as old as the last round of the
	// leader. A primary that does not answer leaves the recorded facts (and their age) to judge with.
	if live, lerr := r.Plan.LiveReplication(ctx); lerr == nil {
		facts.SyncStreaming = live.SyncStreaming && live.Streaming > 0
		facts.LastKnownLsn, facts.LastKnownAge = live.FlushLsn, 0
	}
	return Choose(ChooseInput{Candidates: cands, Target: sc.in.Targets[0], Facts: facts}), facts, nil
}
