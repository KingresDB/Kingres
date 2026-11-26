// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
)

// The epochs of a bundle (design 9.4, step 21). A bundle of a database whose chain was restored holds the frozen log of the old
// epoch, the log of the new one, and the signed epoch-closed event. Each log verifies on its own; this audit says what that
// leaves open.

// bundleChains are the chain logs a bundle verification opened, by the key of the chain and epoch, and the epochs of each chain.
type bundleChains struct {
	logs   map[string]*openedLog
	epochs map[[32]byte][]uint32
}

func newBundleChains() *bundleChains {
	return &bundleChains{logs: map[string]*openedLog{}, epochs: map[[32]byte][]uint32{}}
}

func (b *bundleChains) add(chain [32]byte, epoch uint32, ol *openedLog) {
	b.logs[events.ChainKey(chain, epoch)] = ol
	b.epochs[chain] = append(b.epochs[chain], epoch)
	sort.Slice(b.epochs[chain], func(i, j int) bool { return b.epochs[chain][i] > b.epochs[chain][j] }) // newest first
}

// proofInClosed is a proof of the bundle whose block was found in an epoch that an event closed.
type proofInClosed struct {
	label  string
	chain  [32]byte
	epoch  uint32
	height uint64
	leaf   merkle.Hash // the leaf hash of the block, as the proof shows it
}

// proofEpochs shows a proof in the epochs of its chain that the bundle holds, newest first, and returns the epoch that shows the
// block. With one epoch it is the plain check (the epoch is the only one). With several, an epoch whose log does not hold the block at
// its height gives way to the next older one: the block of an old epoch is not in the new log.
func (e *env) proofEpochs(r *result, p *pubInputs, data []byte, bc *bundleChains, pr *proof.Proof) (uint32, *result, outcome) {
	probe, _, perr := proof.ParseStream(bytesOpener(data))
	var eps []uint32
	if perr == nil {
		eps = bc.epochs[probe.ChainID]
	}
	if len(eps) < 2 {
		sub := &result{cmd: r.cmd}
		out := e.proofLegCore(sub, p, bytesOpener(data), proof.Expect{}, proofLegArgs{leg: "chain"}, pr)
		ep := uint32(0)
		if len(eps) == 1 {
			ep = eps[0]
		}
		return ep, sub, out
	}
	var first outcome
	var firstSub *result
	for i, ep := range eps {
		sub := &result{cmd: r.cmd}
		out := e.proofLegCore(sub, p, bytesOpener(data), proof.Expect{}, proofLegArgs{leg: "chain", epoch: uint64(ep)}, pr)
		if out.ok() {
			return ep, sub, outcome{}
		}
		if i == 0 {
			first, firstSub = out, sub
		}
		if out.kind != outFailed || out.step != "log-inclusion" {
			return 0, sub, out
		}
	}
	return 0, firstSub, first
}

// auditEpochs is the audit of the epochs of a bundle. It returns what to put in the info, and the outcome: a failure unless every
// closed epoch has an event that the auditor accepted.
func (p *pubInputs) auditEpochs(r *result, bc *bundleChains, found []proofInClosed, acc *epochAccept) ([]map[string]any, outcome) {
	if len(bc.logs) == 0 {
		return nil, outcome{}
	}
	w, err := p.events()
	if err != nil {
		return nil, eventsOutcome(err, "the events of the bundle could not be read, and the epochs of its chains depend on them")
	}
	chains := make([][32]byte, 0, len(bc.epochs))
	for c := range bc.epochs {
		chains = append(chains, c)
	}
	sort.Slice(chains, func(i, j int) bool { return string(chains[i][:]) < string(chains[j][:]) })
	// a log of a later epoch needs the event that closed the one before it
	for _, c := range chains {
		for _, ep := range bc.epochs[c] {
			if ep < 2 {
				continue
			}
			key := events.ChainKey(c, ep-1)
			if _, ok := w.EpochClosed[key]; ok {
				continue
			}
			why := "the publication holds no epoch-closed event for it"
			if seq, bad := w.UnauthorisedEpochClosed[key]; bad {
				why = fmt.Sprintf("the event %06d says so and is signed by no key that the trust chain authorises", seq)
			}
			return nil, failedOut("epoch-closed", fmt.Errorf("the bundle holds the log of epoch %d of chain %x and epoch %d was not closed by a verified event: %s. A new epoch is begun only by the acknowledgement of a divergence (9.4), which is signed and published as an event: the event was withheld, or this is not that publication", ep, c[:8], ep-1, why))
		}
	}
	keys := make([]string, 0, len(w.EpochClosed))
	for k := range w.EpochClosed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var info []map[string]any
	var refused []string
	for _, k := range keys {
		ec := w.EpochClosed[k]
		if len(bc.epochs[ec.Chain]) == 0 {
			continue // an event about a chain that this bundle does not hold
		}
		mk := memberKey{ec.Chain, ec.Epoch}
		how := "the closed log is not in the bundle: the final size and root of the event were not compared with it"
		if ol := bc.logs[k]; ol != nil {
			if ol.cp.Size != ec.FinalSize || ol.cp.Root != ec.FinalRoot {
				return nil, failedOut("epoch-closed", fmt.Errorf("the event %06d closes epoch %d of chain %s at size %d with root %x, and the signed log of that epoch in the bundle has size %d, root %x: the event and the log it closes disagree", ec.Seq, ec.Epoch, mk, ec.FinalSize, ec.FinalRoot[:8], ol.cp.Size, ol.cp.Root[:8]))
			}
			how = "the frozen log of the epoch in the bundle has the size and root the event states"
		}
		if len(bc.logs) > 0 && bc.logs[events.ChainKey(ec.Chain, ec.Epoch+1)] == nil {
			how += "; the log of epoch " + strconv.FormatUint(uint64(ec.Epoch)+1, 10) + " is not in the bundle"
		}
		accepted := acc.take(ec.Seq)
		info = append(info, map[string]any{"chain_id": fmt.Sprintf("%x", ec.Chain[:]), "epoch": ec.Epoch, "event": ec.Seq, "kind": ec.Kind, "final_size": ec.FinalSize, "accepted": accepted})
		if accepted {
			if hasStep(r, "finding epoch-closed "+mk.short()) {
				continue // the aggregate check has already printed it
			}
			r.ok("finding epoch-closed "+mk.short(), epochClosedText(mk, ec)+"; ACCEPTED by --accept-epoch-closed "+strconv.FormatUint(ec.Seq, 10)+" ("+how+"): this is a finding and not a pass, the history of that epoch is not the history of the next")
			continue
		}
		refused = append(refused, epochClosedText(mk, ec)+" ("+how+"). "+epochHint(ec))
	}
	for _, f := range found {
		ec, ok := w.EpochClosed[events.ChainKey(f.chain, f.epoch)]
		if !ok {
			continue
		}
		if ec.CommonKnown && int64(f.height) <= ec.CommonHeight { // #nosec G115 -- a height is below 2^62
			// The event states that the two histories share this height. The statement is tested, not believed (P7R-02): when
			// the log of the next epoch is in the bundle, the block must be in it at that height.
			next := bc.logs[events.ChainKey(f.chain, f.epoch+1)]
			if next == nil {
				r.notes = append(r.notes, fmt.Sprintf("%s: the block has height %d, at or below the common height %d that the event %06d states for the closed epoch %d; the log of epoch %d is not in the bundle, so this run did not test that statement and does not show that the block is in the history that goes on", f.label, f.height, ec.CommonHeight, ec.Seq, ec.Epoch, f.epoch+1))
				continue
			}
			if why := blockNotInLog(next, f.height, f.leaf); why != "" {
				return info, failedOut("epoch-closed", fmt.Errorf("%s shows its block (height %d) in epoch %d, and the event %06d states common height %d for that epoch, which says the block is in epoch %d as well; it is not: %s. The event and the logs disagree", f.label, f.height, ec.Epoch, ec.Seq, ec.CommonHeight, f.epoch+1, why))
			}
			// There is no success note here (P7F-03): proofEpochs tries the newest epoch first, so a block that is also leaf h of the log of the
			// next epoch is found there and is never listed as found in the closed one. A block that is listed here has failed the inclusion in the
			// next log already, so the check above fails for it; the check stays as a second line behind proofEpochs.
			continue
		}
		if !acc.want[ec.Seq] {
			refused = append(refused, fmt.Sprintf("%s shows its block (height %d) in epoch %d, which the event %06d closed: that epoch's history above the common height was cut, and this run does not show that the block is in the history that goes on. %s", f.label, f.height, ec.Epoch, ec.Seq, epochHint(ec)))
			continue
		}
		acc.take(ec.Seq)
		r.notes = append(r.notes, fmt.Sprintf("finding: %s shows its block (height %d) in epoch %d, which the event %06d closed; accepted by --accept-epoch-closed %d: this run does not show that the block is in the history that goes on", f.label, f.height, ec.Epoch, ec.Seq, ec.Seq))
	}
	if len(refused) > 0 {
		return info, failedOut("epoch-closed", errors.New(strings.Join(refused, "; ")))
	}
	return info, outcome{}
}

func hasStep(r *result, name string) bool {
	for _, s := range r.steps {
		if s.Name == name {
			return true
		}
	}
	return false
}

// blockNotInLog says why the block with leaf hash leaf is not leaf h of the signed log ol, or "" when it is.
func blockNotInLog(ol *openedLog, h uint64, leaf merkle.Hash) string {
	if h >= ol.cp.Size {
		return fmt.Sprintf("that log has %d block(s), heights 0 to %d", ol.cp.Size, ol.cp.Size-1)
	}
	path, err := ol.reader.InclusionProof(h)
	if err != nil {
		return "the tiles of that log could not give the path of the height: " + err.Error()
	}
	if err := merkle.VerifyPath(leaf, h, ol.cp.Size, path, ol.cp.Root); err != nil {
		return fmt.Sprintf("leaf %d of the signed log of size %d is another block", h, ol.cp.Size)
	}
	return ""
}
