// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// The findings of a closed epoch (ledger-phase7.md 9.4, step 21). When the operator acknowledges that the chain of a
// database was restored or lost its tail, the publisher signs an epoch-closed event, freezes the log of the old epoch and
// opens the log of the next one. Each epoch verifies on its own. What an auditor must not miss is that the two histories are
// not one: comparing the new epoch with a head of the old one fails, and says why. --accept-epoch-closed SEQ turns that
// failure into a printed finding for the event with that number, and never hides it.

// epochAccept is the set of events the auditor accepted with --accept-epoch-closed, and which of them a run used.
type epochAccept struct {
	want map[uint64]bool
	used map[uint64]bool
}

// newEpochAccept reads the values of the repeated flag: canonical decimal event numbers.
func newEpochAccept(vals []string) (*epochAccept, error) {
	a := &epochAccept{want: map[uint64]bool{}, used: map[uint64]bool{}}
	for _, v := range vals {
		bad := fmt.Errorf("--accept-epoch-closed %q is not the number of an event (a decimal without leading zeros, as in the name of the file events/000003)", v)
		if v == "" || len(v) > 10 || (len(v) > 1 && v[0] == '0') {
			return nil, bad
		}
		n, err := strconv.ParseUint(v, 10, 31)
		if err != nil {
			return nil, bad
		}
		a.want[n] = true
	}
	return a, nil
}

// take says whether the event was accepted, and remembers that the run needed the answer.
func (a *epochAccept) take(seq uint64) bool {
	if a == nil || !a.want[seq] {
		return false
	}
	a.used[seq] = true
	return true
}

// unusedNotes names the numbers that no finding of the run needed: a typo must not pass for an acceptance.
func (a *epochAccept) unusedNotes() []string {
	if a == nil {
		return nil
	}
	var ns []uint64
	for n := range a.want {
		if !a.used[n] {
			ns = append(ns, n)
		}
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i] < ns[j] })
	var out []string
	for _, n := range ns {
		out = append(out, fmt.Sprintf("--accept-epoch-closed %d accepted nothing: no finding of this run is about the event %06d (a wrong number, an event that is not a verified epoch-closed event, or no closed epoch is compared here)", n, n))
	}
	return out
}

// epochClosedText says what a verified epoch-closed event states, in the words of 9.4.
func epochClosedText(k memberKey, ec events.EpochClosed) string {
	var above string
	switch {
	case !ec.CommonKnown:
		above = fmt.Sprintf("the blocks above the common height of the two histories existed in epoch %d and are not in epoch %d (the event does not state the common height: the old blocks were no longer in the database)", ec.Epoch, ec.Epoch+1)
	case ec.CommonHeight < 0:
		above = fmt.Sprintf("all %d block(s) of epoch %d existed only there and are not in epoch %d", ec.FinalSize, ec.Epoch, ec.Epoch+1)
	default:
		above = fmt.Sprintf("%d block(s) above height %d (heights %d to %d) existed in epoch %d and are not in epoch %d", int64(ec.FinalSize)-1-ec.CommonHeight, ec.CommonHeight, ec.CommonHeight+1, ec.FinalSize-1, ec.Epoch, ec.Epoch+1) // #nosec G115 -- below 2^62
	}
	return fmt.Sprintf("epoch %d of chain %s was closed by event %06d (%s, %s, acknowledged by %s, reason %q): the log of that epoch published %d block(s) (root %x), and %s",
		ec.Epoch, k, ec.Seq, ec.Kind, ec.CommonText(), ec.AcknowledgedBy, ec.Reason, ec.FinalSize, ec.FinalRoot[:8], above)
}

func epochHint(ec events.EpochClosed) string {
	return fmt.Sprintf("If you accept that this history was cut, and want to verify the new epoch alone, run again with --accept-epoch-closed %d: the finding is then printed and is not an error", ec.Seq)
}

// epochFinding records an accepted epoch-closed event: a step that says what was accepted, and a member of the info.
func (rep *aggReport) epochFinding(r *result, k memberKey, ec events.EpochClosed, how string) {
	r.ok("finding epoch-closed "+k.short(), epochClosedText(k, ec)+"; ACCEPTED by --accept-epoch-closed "+strconv.FormatUint(ec.Seq, 10)+" ("+how+"): this is a finding and not a pass, the history of that epoch is not the history of the next")
	rep.closed = append(rep.closed, map[string]any{"chain_id": fmt.Sprintf("%x", k.chain[:]), "epoch": ec.Epoch, "event": ec.Seq, "kind": ec.Kind, "final_size": ec.FinalSize, "accepted": true})
}

type epochState int

const (
	epochNone     epochState = iota // no verified epoch-closed event for the epoch
	epochAccepted                   // a verified event, and the auditor accepted it
	epochRefused                    // a verified event, not accepted
)

func eventsKey(m aggregate.Member) string { return events.ChainKey(m.ChainID, m.Epoch) }

// closedMember judges a member that the leaf flags diverged against the epoch-closed events. text is what to tell the reader
// (the finding when refused, a remark about an event that is not authorised when there is none).
func (p *pubInputs) closedMember(r *result, rep *aggReport, k memberKey, m aggregate.Member, cl *openedLog, acc *epochAccept) (epochState, string, outcome) {
	w, err := p.events()
	if err != nil || w == nil {
		return epochNone, "", outcome{}
	}
	key := eventsKey(m)
	ec, ok := w.EpochClosed[key]
	if !ok {
		if seq, bad := w.UnauthorisedEpochClosed[key]; bad {
			return epochNone, fmt.Sprintf("the event %06d says that epoch %d of this chain is closed and is signed by no key that the trust chain authorises: it is refused, and the member stays diverged", seq, m.Epoch), outcome{}
		}
		return epochNone, "", outcome{}
	}
	if ec.FinalSize != m.Size() || ec.FinalRoot != m.LogRoot {
		return epochNone, "", failedOut("epoch-closed", fmt.Errorf("the event %06d closes epoch %d of chain %s at size %d with root %x, and the aggregate member says size %d, root %x: the event and the aggregate describe two different last states of the log", ec.Seq, ec.Epoch, k, ec.FinalSize, ec.FinalRoot[:8], m.Size(), m.LogRoot[:8]))
	}
	if cl != nil && cl.cp.Size > ec.FinalSize {
		return epochNone, "", failedOut("epoch-closed", fmt.Errorf("the event %06d closes epoch %d of chain %s at size %d and the signed log of that epoch has size %d: a closed log continues, which the event says it does not", ec.Seq, ec.Epoch, k, ec.FinalSize, cl.cp.Size))
	}
	if acc.take(ec.Seq) {
		rep.epochFinding(r, k, ec, "the member is flagged diverged")
		return epochAccepted, "", outcome{}
	}
	return epochRefused, epochClosedText(k, ec) + ". " + epochHint(ec), outcome{}
}

// epochContinuity is the comparison of a round you hold, in which the chain was a member of epoch e, with a round in which that
// epoch is gone (retired and closed, or left out). It returns handled false when no verified epoch-closed event is about the
// member, and the caller goes on with the other rules. Otherwise the held head is proved a prefix of the frozen log of the closed
// epoch, and the finding is a failure unless accepted.
func (p *pubInputs) epochContinuity(r *result, rep *aggReport, cs *chainChecker, om aggregate.Member, k memberKey, acc *epochAccept) (bool, outcome) {
	w, err := p.events()
	if err != nil || w == nil {
		return false, outcome{}
	}
	ec, ok := w.EpochClosed[eventsKey(om)]
	if !ok {
		return false, outcome{}
	}
	cl, out := cs.open(k, r)
	if !out.ok() {
		return true, out
	}
	if cl.cp.Size != ec.FinalSize || cl.cp.Root != ec.FinalRoot {
		return true, failedOut("epoch-closed", fmt.Errorf("the event %06d closes epoch %d of chain %s at size %d with root %x, and the frozen log has size %d, root %x: the event and the log it closes disagree", ec.Seq, ec.Epoch, k, ec.FinalSize, ec.FinalRoot[:8], cl.cp.Size, cl.cp.Root[:8]))
	}
	if om.Size() > cl.cp.Size {
		return true, failedOut("continuity", fmt.Errorf("chain %s: the round that you hold names %d block(s) and the closed log of the epoch has %d: the closed log is shorter than what was published before it", k, om.Size(), cl.cp.Size))
	}
	if om.Size() == cl.cp.Size && om.LogRoot != cl.cp.Root {
		return true, failedOut("continuity", fmt.Errorf("chain %s: the round that you hold names root %x for %d block(s) and the frozen log of epoch %d has root %x at that size: the head you hold is not in the closed log, so the closed log is not the history you hold", k, om.LogRoot[:8], om.Size(), ec.Epoch, cl.cp.Root[:8]))
	}
	pth, err := cl.reader.ConsistencyProofBetween(om.Size(), cl.cp.Size)
	if err != nil {
		return true, tileOutcome(err, cl.rel())
	}
	if err := merkle.VerifyConsistency(om.Size(), cl.cp.Size, om.LogRoot, cl.cp.Root, pth); err != nil {
		step := "tiles"
		if errors.Is(err, merkle.ErrOldRoot) {
			step = "continuity"
		}
		return true, failedOut(step, fmt.Errorf("chain %s: the frozen log of epoch %d at size %d is not an extension of the log at size %d that the round you hold named: %w", k, ec.Epoch, cl.cp.Size, om.Size(), err))
	}
	if acc.take(ec.Seq) {
		rep.epochFinding(r, k, ec, "the head you hold is a prefix of the frozen log of the closed epoch")
		return true, outcome{}
	}
	return true, failedOut("epoch-closed", fmt.Errorf("%s. The head you hold (%d block(s)) is a prefix of that closed log, so the closed epoch is genuine and the new epoch is not its continuation. %s", epochClosedText(k, ec), om.Size(), epochHint(ec)))
}

// remarkTail appends what a failure may add about an event that was refused.
func remarkTail(remarks []string) string {
	if len(remarks) == 0 {
		return ""
	}
	return ". " + strings.Join(remarks, "; ")
}

// closedEpochNotes tells, for a run that read the last leaf of an aggregate, which verified epoch-closed events of the publication
// are about chains of that leaf, so that a verification of the new epoch alone does not read as "the history was never cut".
func (p *pubInputs) closedEpochNotes(rep *aggReport) []string {
	w, err := p.events()
	if err != nil || w == nil || len(w.EpochClosed) == 0 || rep == nil || rep.leaf == nil {
		return nil
	}
	inLeaf := map[[32]byte]bool{}
	for _, m := range rep.leaf.Members {
		inLeaf[m.ChainID] = true
	}
	keys := make([]string, 0, len(w.EpochClosed))
	for k := range w.EpochClosed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var out []string
	for _, k := range keys {
		ec := w.EpochClosed[k]
		if !inLeaf[ec.Chain] {
			continue
		}
		out = append(out, fmt.Sprintf("the publication holds a verified epoch-closed event about a chain of this aggregate: %s. This run verified the members of the last round (epoch %d or later); it compared nothing with epoch %d. To compare, give --previous with an aggregate checkpoint you hold from before the epoch was closed", epochClosedText(memberKey{ec.Chain, ec.Epoch}, ec), ec.Epoch+1, ec.Epoch))
	}
	return out
}

// commonRemark says whether a block at height h is below the common height that an epoch-closed event states.
func commonRemark(ec events.EpochClosed, h uint64) string {
	if ec.CommonKnown && int64(h) <= ec.CommonHeight { // #nosec G115 -- a height is below 2^62
		return fmt.Sprintf(" (the event states common height %d and this block has height %d: the log of the next epoch was not opened by this command, so this run did not test that the block is in the history that goes on)", ec.CommonHeight, h)
	}
	return ""
}

// otherEpochOf says whether a note is a checkpoint of the same chain at another epoch than the log asked for: the origin line names
// <prefix>/c/<id>/e<M> with M different. The note is not verified here.
func (in *logInputs) otherEpochOf(note []byte) (uint64, bool) {
	if in.spec.kind != "chain" || !in.spec.hasEpoch {
		return 0, false
	}
	n, err := signednote.Parse(note)
	if err != nil {
		return 0, false
	}
	origin, _, _ := strings.Cut(string(n.Text), "\n")
	prefix := in.trust.OriginPrefix + "/c/" + in.spec.id + "/e"
	rest, ok := strings.CutPrefix(origin, prefix)
	if !ok {
		return 0, false
	}
	m := epochDirRE.FindStringSubmatch("e" + rest)
	if m == nil {
		return 0, false
	}
	v, _ := strconv.ParseUint(m[1], 10, 64)
	return v, v != in.spec.epoch
}

// closedEpochNote tells a single-log command that the log it checks is of an epoch that a verified event closed: the checkpoint is
// genuine for that epoch, and the history of the chain went on in another log.
func (in *logInputs) closedEpochNote(w *events.Walk) []string {
	if in.spec.kind != "chain" || !in.spec.hasEpoch || w == nil {
		return nil
	}
	raw, err := hex.DecodeString(in.spec.id)
	if err != nil || len(raw) != 32 || in.spec.epoch > 1<<31-1 {
		return nil
	}
	var c [32]byte
	copy(c[:], raw)
	ec, ok := w.EpochClosed[events.ChainKey(c, uint32(in.spec.epoch))] // #nosec G115 -- range checked
	if !ok {
		return nil
	}
	return []string{"finding: this log is of an epoch that a verified event closed, and it is frozen: " + epochClosedText(memberKey{c, ec.Epoch}, ec) + ". A checkpoint of it is genuine for that epoch; the chain went on in the log of the next epoch, which is another log"}
}

// closedEvent is the verified epoch-closed event for epoch e of the chain of this log, when the events were read.
func (in *logInputs) closedEvent(e uint64) (events.EpochClosed, bool) {
	var none events.EpochClosed
	if in.walk == nil || in.spec.kind != "chain" || e > 1<<31-1 {
		return none, false
	}
	raw, err := hex.DecodeString(in.spec.id)
	if err != nil || len(raw) != 32 {
		return none, false
	}
	var c [32]byte
	copy(c[:], raw)
	ec, ok := in.walk.EpochClosed[events.ChainKey(c, uint32(e))] // #nosec G115 -- range checked
	return ec, ok
}

// crossEpochFailure is the failure of a consistency check between a checkpoint of an epoch that a verified event closed and the log
// of a later epoch: two different logs, the tree of one is not a prefix of the other, and the event says why (design 9.4). Both
// checkpoints are verified before this is reached. A held size or root that contradicts the event is said first.
func (in *logInputs) crossEpochFailure(ec events.EpochClosed, held *checkpoint.Checkpoint) (string, error) {
	k := memberKey{epoch: ec.Epoch}
	if raw, err := hex.DecodeString(in.spec.id); err == nil && len(raw) == 32 {
		copy(k.chain[:], raw)
	}
	switch {
	case held.Size > ec.FinalSize:
		return "epoch-closed", fmt.Errorf("the checkpoint you hold has size %d, larger than the final size %d that the verified event %06d states for epoch %d: the closed epoch published fewer blocks than you hold, so the event and your checkpoint cannot both be right", held.Size, ec.FinalSize, ec.Seq, ec.Epoch)
	case held.Size == ec.FinalSize && held.Root != ec.FinalRoot:
		return "epoch-closed", fmt.Errorf("the checkpoint you hold has the final size %d of epoch %d and root %x, and the verified event %06d states root %x: two different histories of the same size", ec.FinalSize, ec.Epoch, held.Root[:8], ec.Seq, ec.FinalRoot[:8])
	}
	return "epoch-closed", fmt.Errorf("%s. Epoch %d and epoch %d are different logs and the tree of one is not a prefix of the other, so no consistency exists to prove; the checkpoint you hold (size %d) is evidence of the history that was cut. To verify the new epoch alone use aggregate, proof or bundle verify with --accept-epoch-closed %d", epochClosedText(k, ec), ec.Epoch, in.spec.epoch, held.Size, ec.Seq)
}
