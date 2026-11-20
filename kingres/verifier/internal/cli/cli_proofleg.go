// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
)

// proofLegArgs are the flags of `proof --bundle`.
type proofLegArgs struct {
	proofPath, digest, bundle, leg, trust, aggID string
	epoch                                        uint64
	tableID                                      int64
	rowHash, op                                  string
	acceptEpoch                                  []string
	accept                                       *epochAccept
	now                                          nowArg
}

// parseExpect reads --table-id, --row-hash and --op as `proof` does.
func parseExpect(tableID int64, rowHash, opFlag string) (proof.Expect, error) {
	var exp proof.Expect
	if tableID != -1 {
		if tableID < 0 || tableID > chain.MaxInt4 {
			return exp, fmt.Errorf("--table-id %d is out of range, expected 0 to %d", tableID, chain.MaxInt4)
		}
		t := uint32(tableID) // #nosec G115 -- range checked
		exp.TableID = &t
	}
	if rowHash != "" {
		h, err := format.DecodeHash(rowHash)
		if err != nil {
			return exp, fmt.Errorf("--row-hash: %w", err)
		}
		exp.RowHash = &h
	}
	switch opFlag {
	case "any":
	case "I":
		op := format.OpInsert
		exp.Op = &op
	case "D":
		op := format.OpDelete
		exp.Op = &op
	default:
		return exp, fmt.Errorf("--op %q is not I, D or any", opFlag)
	}
	return exp, nil
}

// runProofLeg is `proof --bundle B --log-leg chain|aggregate`: the steps 0 to 3 of the README on the
// row, then the block as a leaf of a signed log (design 7.4, "What proof --bundle --log-leg chain does").
func (e *env) runProofLeg(asJSON bool, a proofLegArgs) int {
	r := &result{cmd: "proof"}
	out := e.proofLegRun(r, a)
	switch out.kind {
	case outInput:
		return e.inputErrorInfo(r.cmd, asJSON, out.err, r.inputInfo())
	case outFailed:
		return e.finishFailed(r, asJSON, out.step, out.err)
	case outIncomplete:
		return e.finishIncomplete(r, asJSON, out.missing)
	}
	return e.finishOK(r, asJSON)
}

func (e *env) proofLegRun(r *result, a proofLegArgs) outcome {
	switch {
	case a.proofPath == "":
		return inputOut(errors.New("missing --proof"))
	case a.digest != "":
		return inputOut(errors.New("--digest and --bundle are two ways to anchor a proof: give one"))
	case a.leg != "chain" && a.leg != "aggregate":
		return inputOut(fmt.Errorf("--log-leg %q is not chain or aggregate: say which log the block must be shown in", a.leg))
	case a.epoch > 1<<31-1:
		return inputOut(errors.New("--epoch is out of range"))
	}
	exp, err := parseExpect(a.tableID, a.rowHash, a.op)
	if err != nil {
		return inputOut(err)
	}
	acc, err := newEpochAccept(a.acceptEpoch)
	if err != nil {
		return inputOut(err)
	}
	a.accept = acc
	p, err := loadPubInputs(a.trust, a.bundle, a.now)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	if o := p.gate(); !o.ok() {
		return o
	}
	r.notes = append(r.notes, p.notes...)
	open := func() (io.ReadCloser, error) { return safefile.OpenArg(a.proofPath, maxProofSize) }
	probe, err := open()
	if err != nil {
		return inputOut(err)
	}
	_ = probe.Close()
	return e.proofLegCore(r, p, open, exp, a, nil)
}

// proofLegCore is the verification of one proof against the signed logs of an opened publication; the
// proof is read through open (a file, or the bytes of a bundle). It adds its steps to r.
func (e *env) proofLegCore(r *result, p *pubInputs, open proof.Source, exp proof.Expect, a proofLegArgs, sink *proof.Proof) outcome {
	pr, _, err := proof.ParseStream(open)
	if err != nil {
		return failedOut(proof.StepParse, err)
	}
	if sink != nil {
		*sink = *pr
	}
	if pr.ChainLen > 0 {
		r.notes = append(r.notes, fmt.Sprintf("the proof carries %d later header(s) of the chain: they are not used, the block is shown in the log", pr.ChainLen))
	}
	res, err := proof.VerifyRow(pr, pr.ChainID, exp)
	// the chain id of the proof names the log it is checked against, so the check of the id is the one of the log below
	if res != nil {
		for _, s := range res.Steps {
			r.ok(s.Name, s.Detail)
		}
	}
	if err != nil {
		var pe *proof.Error
		if errors.As(err, &pe) {
			return failedOut(pe.Step, pe.Err)
		}
		return failedOut("proof", err)
	}
	cid := hex.EncodeToString(pr.ChainID[:])
	spec := logSpec{kind: "chain", id: cid}
	if a.epoch > 0 {
		spec.epoch, spec.hasEpoch = a.epoch, true
	} else {
		in := p.logIn(spec, nil)
		if err := in.resolveEpoch(); err != nil {
			return pubOutcome(err)
		}
		spec = in.spec
	}
	ol, knotes, out := p.openLog(spec, nil)
	r.notes = appendNew(r.notes, knotes)
	if !out.ok() {
		return out
	}
	r.ok("chain-checkpoint", fmt.Sprintf("%s, tree size %d, root %x, signed by %s of the trust file", ol.cp.Origin, ol.cp.Size, ol.cp.Root, describeSigners(ol.signers)))
	h := pr.Block.Height
	leaf := merkle.LeafHash(pr.Block.BlockHashInput())
	if h >= ol.cp.Size {
		return failedOut("log-inclusion", fmt.Errorf("the signed checkpoint of the chain log covers %d block(s), heights 0 to %d, and the block of the proof has height %d: this log cannot show the block (give a later checkpoint)", ol.cp.Size, ol.cp.Size-1, h))
	}
	path, err := ol.reader.InclusionProof(h)
	if err != nil {
		return tileOutcome(err, ol.rel())
	}
	if err := merkle.VerifyPath(leaf, h, ol.cp.Size, path, ol.cp.Root); err != nil {
		return failedOut("log-inclusion", fmt.Errorf("the block of height %d is not leaf %d of the signed chain log of size %d: %w", h, h, ol.cp.Size, err))
	}
	r.ok("log-inclusion", fmt.Sprintf("block %d: SHA-256 of its 87-byte leaf data is the block hash %x, and its leaf hash leads to the root of the signed chain log of size %d through %d hash(es) computed from the tiles (RFC 9162 2.1.3.2)", h, pr.Block.BlockHash[:8], ol.cp.Size, len(path)))
	if w, werr := p.events(); werr == nil && w != nil {
		if ec, ok := w.EpochClosed[events.ChainKey(pr.ChainID, uint32(spec.epoch))]; ok { // #nosec G115 -- range checked
			r.notes = append(r.notes, "finding: the block is shown in epoch "+strconv.FormatUint(spec.epoch, 10)+", which a verified event closed: "+epochClosedText(memberKey{pr.ChainID, uint32(spec.epoch)}, ec)+". This run does not show that the block is in the history that goes on"+commonRemark(ec, h))
		}
	}
	r.info = map[string]any{"table_id": pr.TableID, "op": proof.OpName(pr.Op), "row_hash": format.EncodeHex(pr.RowHash[:]), "block_height": h,
		"log_leg": a.leg, "chain_log": ol.cp.Origin, "chain_size": ol.cp.Size}
	root := "signature of key " + describeSigners(ol.signers) + " on the checkpoint of the chain log"
	var legInc []string // what the aggregate leg could not confirm (a stale or unconfirmed retired member)
	if a.leg == "aggregate" {
		aggID := a.aggID
		if aggID == "" {
			d, _, err := p.src.SubDirs("a")
			ids := []string{}
			if err == nil {
				ids = d
			}
			if err == nil && len(ids) != 1 {
				return inputOut(fmt.Errorf("the source has %d aggregate logs, expected exactly one: give --aggregate-id", len(ids)))
			}
			if err != nil {
				return inputOut(fmt.Errorf("no --aggregate-id was given and the source cannot be searched for one (%v): give --aggregate-id", err))
			}
			aggID = ids[0]
		}
		if !hex64RE.MatchString(aggID) {
			return inputOut(errors.New("--aggregate-id must be 64 lower case hex characters"))
		}
		key := memberKey{pr.ChainID, uint32(spec.epoch)} // #nosec G115 -- the epoch was range checked
		rep, out := p.verifyAggregate(r, aggID, aggOptions{only: &key, accept: a.accept})
		if !out.ok() {
			return out
		}
		legInc = rep.incomplete
		var member aggregate.Member
		for _, m := range rep.leaf.Members {
			if (memberKey{m.ChainID, m.Epoch}) == key {
				member = m
			}
		}
		if member.Size() <= h {
			return failedOut("aggregate-leg", fmt.Errorf("the aggregate names the head of the chain at height %d, and the block has height %d: the aggregate round does not cover this block", member.Height, h))
		}
		cl := rep.chains[key]
		pth, err := cl.reader.InclusionProofIn(h, member.Size())
		if err != nil {
			return tileOutcome(err, cl.rel())
		}
		if err := merkle.VerifyPath(leaf, h, member.Size(), pth, member.LogRoot); err != nil {
			return failedOut("aggregate-leg", fmt.Errorf("the block of height %d is not leaf %d of the chain log of size %d whose root %x the aggregate member names: %w", h, h, member.Size(), member.LogRoot[:8], err))
		}
		r.ok("aggregate-leg", fmt.Sprintf("block %d leads to the root %x that the member of round %d names for the chain log of size %d, and the aggregate checkpoint of size %d is signed by %s", h, member.LogRoot[:8], rep.leaf.Seq, member.Size(), rep.log.cp.Size, describeSigners(rep.log.signers)))
		root = "signature of key " + describeSigners(rep.log.signers) + " on the aggregate checkpoint (no anchor was checked: anchors are verified by a later command)"
		r.info["aggregate_log"], r.info["aggregate_size"], r.info["aggregate_round"] = rep.log.cp.Origin, rep.log.cp.Size, rep.leaf.Seq
	}
	r.info["root_of_trust"] = root
	r.ok("root-of-trust", root)
	r.headline = fmt.Sprintf("the proof is valid: the block of height %d is leaf %d of the signed chain log of size %d.", h, h, ol.cp.Size)
	if exp.TableID == nil || exp.RowHash == nil {
		r.notProved = append(r.notProved, "no --table-id and --row-hash were given: this shows that an entry with this row_hash exists in the ledger, not that it is the row you hold")
	}
	r.notProved = append(r.notProved,
		"that this is the latest checkpoint of the log: an older genuine one also verifies; record it with checkpoint --state or watch the log with monitor",
		"when the block was made: block times are the server's clock, and no anchor was checked",
		"the row hash must be recomputed from data you hold (kingres-verify row-hash)",
		"that the trust file was obtained outside the cluster: that is your responsibility")
	if len(legInc) > 0 {
		return outcome{kind: outIncomplete, missing: legInc}
	}
	return outcome{}
}
