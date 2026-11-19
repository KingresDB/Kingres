// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// aggregateGlobalSnapshot is the sentence of ledger-phase7.md 5.6 that the console and the output of
// `aggregate` reproduce.
const aggregateGlobalSnapshot = "that the heads were read at one instant, that a cross-group transaction is seen in all its groups at the same time, or an order of commits between groups (ADR 0009: no global snapshot); " +
	"a statement about a consistent state of the whole database at one time needs the global snapshot of phase 8"

// aggOptions are what a caller of verifyAggregate may ask of it.
type aggOptions struct {
	// only limits the member checks to one chain (the log leg of a proof): the others are not checked.
	only *memberKey
	// chainSrc is where the chain logs are read (the source of the publication when nil).
	chainSrc pubsrc.Source
	// previous is a checkpoint of this aggregate log the auditor holds (verified by the caller).
	previous *checkpoint.Checkpoint
}

type memberKey struct {
	chain [32]byte
	epoch uint32
}

func (k memberKey) String() string {
	return hex.EncodeToString(k.chain[:]) + ":e" + strconv.FormatUint(uint64(k.epoch), 10)
}

func (k memberKey) spec() logSpec { return chainSpec(hex.EncodeToString(k.chain[:]), uint64(k.epoch)) }

// short is the form of a member in a step name: eight hex characters and the epoch.
func (k memberKey) short() string {
	return hex.EncodeToString(k.chain[:4]) + ":e" + strconv.FormatUint(uint64(k.epoch), 10)
}

// aggReport is what verifyAggregate established.
type aggReport struct {
	log     *openedLog
	leaf    *aggregate.Leaf
	index   uint64
	chains  map[memberKey]*openedLog // the chain logs that were opened and verified
	checked map[memberKey]bool       // the members that were checked against their chain log
	// incomplete lists what could not be checked although nothing failed (a stale member).
	incomplete []string
	notes      []string
	info       []map[string]any
	previous   *aggregate.Leaf
}

func flagNames(m aggregate.Member) []string {
	var out []string
	if m.Stale() {
		out = append(out, "stale")
	}
	if m.Retired() {
		out = append(out, "retired")
	}
	if m.Diverged() {
		out = append(out, "diverged")
	}
	return out
}

// verifyAggregate verifies the aggregate log id of the publication (the checks are listed in the
// README, "aggregate"): its checkpoint, its last leaf as an entry of the signed tree, and every
// member of that leaf against its chain log; with a previous checkpoint, the continuity of the
// aggregate log and of every chain. Steps are added to r as they pass.
func (p *pubInputs) verifyAggregate(r *result, id string, o aggOptions) (*aggReport, outcome) {
	ol, knotes, out := p.openLog(aggSpec(id), nil)
	r.notes = appendNew(r.notes, knotes)
	if !out.ok() {
		return nil, out
	}
	rep := &aggReport{log: ol, chains: map[memberKey]*openedLog{}, checked: map[memberKey]bool{}}
	r.ok("aggregate-checkpoint", fmt.Sprintf("%s, tree size %d, root %x, signed by %s of the trust file", ol.cp.Origin, ol.cp.Size, ol.cp.Root, describeSigners(ol.signers)))
	if ol.cp.Size == 0 {
		rep.notes = append(rep.notes, "the aggregate log is empty (size 0): there is no leaf and no member to check")
		return rep, outcome{}
	}
	rep.index = ol.cp.Size - 1
	leaf, out := p.readLeaf(ol, rep.index)
	if !out.ok() {
		return nil, out
	}
	rep.leaf = leaf
	r.ok("aggregate-leaf", fmt.Sprintf("entry %d of %d is the leaf of round %d with %d member(s), generated at %s (the leader's clock), and leads to the signed root", rep.index, ol.cp.Size, leaf.Seq, len(leaf.Members), chain.FormatUS(leaf.GeneratedAtUS)))
	if o.only != nil {
		found := false
		for _, m := range leaf.Members {
			if (memberKey{m.ChainID, m.Epoch}) == *o.only {
				found = true
			}
		}
		if !found {
			return nil, failedOut("member", fmt.Errorf("the chain %s is not a member of the last round of the aggregate (round %d): the aggregate does not cover it", o.only, leaf.Seq))
		}
	}
	cs := &chainChecker{p: p, src: o.chainSrc, rep: rep}
	var diverged []string
	for _, m := range leaf.Members {
		k := memberKey{m.ChainID, m.Epoch}
		rep.info = append(rep.info, map[string]any{"chain_id": hex.EncodeToString(m.ChainID[:]), "epoch": m.Epoch, "height": m.Height, "flags": flagNames(m),
			"block_hash": hex.EncodeToString(m.BlockHash[:]), "log_root": hex.EncodeToString(m.LogRoot[:])})
		if o.only != nil && k != *o.only {
			continue
		}
		cl, out := cs.check(r, m)
		if !out.ok() {
			return nil, out
		}
		rep.checked[k] = true
		detail := fmt.Sprintf("block %d, hash %x, is the leaf %d of the chain log whose size-%d root %x the member names; that root is a prefix of the signed chain log of size %d", m.Height, m.BlockHash[:8], m.Height, m.Size(), m.LogRoot[:8], cl.cp.Size)
		switch {
		case m.Retired():
			if out := p.checkRetired(r, m); !out.ok() {
				return nil, out
			}
		case m.Stale():
			rep.incomplete = append(rep.incomplete, fmt.Sprintf("the group of chain %s was not read in round %d (flag stale): its head is the last good head, old news and not a claim about now", k, leaf.Seq))
			detail += "; flagged stale"
		}
		if m.Diverged() {
			diverged = append(diverged, k.String())
		}
		r.ok("member "+k.short(), detail)
	}
	if len(diverged) > 0 {
		return nil, failedOut("diverged", fmt.Errorf("the aggregate reports %s as diverged: the publisher stopped that chain log because it was rolled back or rewritten (a restore, or blocks lost at a failover), and the head shown is the last good one", strings.Join(diverged, ", ")))
	}
	if o.previous != nil {
		if out := p.continuity(r, rep, o); !out.ok() {
			return nil, out
		}
	}
	return rep, outcome{}
}

// readLeaf reads and parses the entry idx of the aggregate log and proves it is the leaf idx of the signed tree.
func (p *pubInputs) readLeaf(ol *openedLog, idx uint64) (*aggregate.Leaf, outcome) {
	e, out := verifiedEntry(ol.files, ol.reader, ol.cp.Size, idx, aggregate.MaxLeafSize, ol.cp.Root)
	if !out.ok() {
		return nil, out
	}
	leaf, err := aggregate.Parse(e)
	if err != nil {
		return nil, failedOut("aggregate-leaf", fmt.Errorf("entry %d of the aggregate log: %w", idx, err))
	}
	if leaf.Seq != idx {
		return nil, failedOut("aggregate-leaf", fmt.Errorf("entry %d of the aggregate log says it is round %d: the sequence number equals the leaf index", idx, leaf.Seq))
	}
	return leaf, outcome{}
}

// chainChecker checks the members of a leaf against their chain logs, opening each chain log once.
type chainChecker struct {
	p   *pubInputs
	src pubsrc.Source
	rep *aggReport
}

func (c *chainChecker) open(k memberKey, r *result) (*openedLog, outcome) {
	if cl := c.rep.chains[k]; cl != nil {
		return cl, outcome{}
	}
	cl, notes, out := c.p.openLog(k.spec(), c.src)
	r.notes = appendNew(r.notes, notes)
	if !out.ok() {
		if out.kind == outFailed {
			out.err = fmt.Errorf("chain %s: %w", k, out.err)
		}
		return nil, out
	}
	c.rep.chains[k] = cl
	return cl, outcome{}
}

// check is the check of one member against the signed chain log: the checkpoint of the chain log,
// the root the member names for the size height+1 as a prefix of that log (RFC 9162 2.1.4.2, from the
// tiles), and the block the member names as the leaf at its height of that prefix.
func (c *chainChecker) check(r *result, m aggregate.Member) (*openedLog, outcome) {
	k := memberKey{m.ChainID, m.Epoch}
	cl, out := c.open(k, r)
	if !out.ok() {
		return nil, out
	}
	where := cl.rel()
	if cl.cp.Size < m.Size() {
		return nil, failedOut("member", fmt.Errorf("the chain %s has a signed checkpoint of size %d, smaller than the %d blocks the aggregate member names (height %d): the chain log went back, or the aggregate claims blocks that were not published", k, cl.cp.Size, m.Size(), m.Height))
	}
	root, err := cl.reader.RootAt(m.Size())
	if err != nil {
		return nil, tileOutcome(err, where)
	}
	if root != m.LogRoot {
		return nil, failedOut("member-root", fmt.Errorf("the chain log %s has root %x at size %d, the aggregate member says %x: the aggregate does not describe the log that is published", k, root, m.Size(), m.LogRoot))
	}
	path, err := cl.reader.ConsistencyProofBetween(m.Size(), cl.cp.Size)
	if err != nil {
		return nil, tileOutcome(err, where)
	}
	if err := merkle.VerifyConsistency(m.Size(), cl.cp.Size, m.LogRoot, cl.cp.Root, path); err != nil {
		step := "tiles"
		if errors.Is(err, merkle.ErrOldRoot) {
			step = "member-root"
		}
		return nil, failedOut(step, fmt.Errorf("the chain log %s: the member's root at size %d is not a prefix of the signed log of size %d: %w", k, m.Size(), cl.cp.Size, err))
	}
	entry, out := verifiedEntry(cl.files, cl.reader, cl.cp.Size, m.Height, format.BlockLeafSize, cl.cp.Root)
	if !out.ok() {
		if out.kind == outFailed {
			out.err = fmt.Errorf("chain %s: %w", k, out.err)
		}
		return nil, out
	}
	h, err := format.ParseBlockLeaf(entry)
	if err != nil {
		return nil, failedOut("member-block", fmt.Errorf("chain %s: the entry %d: %w", k, m.Height, err))
	}
	if h.Height != m.Height {
		return nil, failedOut("member-block", fmt.Errorf("chain %s: the entry %d is the block of height %d", k, m.Height, h.Height))
	}
	if h.BlockHash != m.BlockHash {
		return nil, failedOut("member-block", fmt.Errorf("chain %s: the block at height %d has hash %x, the aggregate member says %x", k, m.Height, h.BlockHash, m.BlockHash))
	}
	return cl, outcome{}
}

// checkRetired checks a member flagged retired against the chain-retired event of the publication: the
// event must be signed by keys that the trust chain authorises, and name the head the member names.
func (p *pubInputs) checkRetired(r *result, m aggregate.Member) outcome {
	k := memberKey{m.ChainID, m.Epoch}
	w, err := p.events()
	if err != nil {
		return eventsOutcome(err, "the events of the publication could not be read for the chain-retired event of "+k.String())
	}
	ev, ok := w.Retired[events.ChainKey(m.ChainID, m.Epoch)]
	if !ok {
		if seq, bad := w.UnauthorisedRetired[events.ChainKey(m.ChainID, m.Epoch)]; bad {
			return failedOut("chain-retired", fmt.Errorf("the event %06d says that chain %s is retired and is signed by no key that the trust chain authorises (a key of the trust file, or one that an event signed by such a key added): it is refused, and so is the retired flag it should explain", seq, k))
		}
		return incompleteOut(fmt.Sprintf("the aggregate flags chain %s retired and the publication holds no chain-retired event for it (%d event(s) were read)", k, w.Count))
	}
	if ev.FinalHeight != m.Height || ev.FinalBlockHash != m.BlockHash {
		return failedOut("chain-retired", fmt.Errorf("the event %06d retires chain %s at height %d, block %x, and the aggregate member says height %d, block %x", ev.Seq, k, ev.FinalHeight, ev.FinalBlockHash[:8], m.Height, m.BlockHash[:8]))
	}
	r.ok("chain-retired "+k.short(), fmt.Sprintf("event %06d (%s), signed by %d authorised key(s), names the head of the member", ev.Seq, ev.Reason, len(ev.SignedBy)))
	return outcome{}
}

// continuity compares the last leaf with the leaf of a checkpoint of this aggregate log that the
// auditor holds (design 5.6, "continuity"): the aggregate log is an extension of the held one,
// every chain of the older leaf is in the newer one or was retired, no height goes back, and every
// chain log extends the log the older leaf named.
func (p *pubInputs) continuity(r *result, rep *aggReport, o aggOptions) outcome {
	ol, prev := rep.log, o.previous
	switch {
	case prev.Size > ol.cp.Size:
		return failedOut("rollback", fmt.Errorf("the aggregate checkpoint has size %d, smaller than %d, the size of the checkpoint you hold: the aggregate log went back, or an old checkpoint is being shown", ol.cp.Size, prev.Size))
	case prev.Size == ol.cp.Size && prev.Root != ol.cp.Root:
		return failedOut("fork", fmt.Errorf("two trusted checkpoints of the aggregate log have the same size %d and different roots (%x held, %x served): the aggregate log has two histories", prev.Size, prev.Root, ol.cp.Root))
	}
	path, err := ol.reader.ConsistencyProof(prev.Size)
	if err != nil {
		return tileOutcome(err, ol.rel())
	}
	if err := merkle.VerifyConsistency(prev.Size, ol.cp.Size, prev.Root, ol.cp.Root, path); err != nil {
		step := "tiles"
		if errors.Is(err, merkle.ErrOldRoot) {
			step = "fork"
		}
		return failedOut(step, fmt.Errorf("the aggregate log at size %d does not extend the log at size %d that you hold: %w", ol.cp.Size, prev.Size, err))
	}
	r.ok("aggregate-consistency", fmt.Sprintf("RFC 9162 2.1.4.2: the aggregate log at size %d extends the one you hold at size %d", ol.cp.Size, prev.Size))
	if prev.Size == 0 || prev.Size == ol.cp.Size {
		return outcome{}
	}
	old, out := p.readLeaf(ol, prev.Size-1)
	if !out.ok() {
		return out
	}
	rep.previous = old
	if rep.leaf.GeneratedAtUS < old.GeneratedAtUS {
		return failedOut("continuity", fmt.Errorf("the round %d was generated at %s, before the round %d (%s) that you hold: the leader's times went back", rep.leaf.Seq, chain.FormatUS(rep.leaf.GeneratedAtUS), old.Seq, chain.FormatUS(old.GeneratedAtUS)))
	}
	byKey := map[memberKey]aggregate.Member{}
	for _, m := range rep.leaf.Members {
		byKey[memberKey{m.ChainID, m.Epoch}] = m
	}
	cs := &chainChecker{p: p, src: o.chainSrc, rep: rep}
	n := 0
	for _, om := range old.Members {
		k := memberKey{om.ChainID, om.Epoch}
		if o.only != nil && k != *o.only {
			continue
		}
		nm, still := byKey[k]
		switch {
		case om.Retired() && still:
			return failedOut("continuity", fmt.Errorf("chain %s was flagged retired in round %d and is in round %d again: a retired chain is left out of the next round", k, old.Seq, rep.leaf.Seq))
		case om.Retired():
			continue
		case !still:
			// retired in a round between the two, which this run did not read: the event says so
			w, werr := p.events()
			if werr == nil {
				if ev, ok := w.Retired[events.ChainKey(om.ChainID, om.Epoch)]; ok && ev.FinalHeight >= om.Height {
					rep.notes = append(rep.notes, fmt.Sprintf("chain %s of round %d is not in round %d: the event %06d retired it (%s)", k, old.Seq, rep.leaf.Seq, ev.Seq, ev.Reason))
					continue
				}
			}
			return failedOut("continuity", fmt.Errorf("chain %s was in round %d and is not in round %d, without a retired flag and without a chain-retired event that this run could verify: a chain removed without record", k, old.Seq, rep.leaf.Seq))
		}
		if nm.Height < om.Height {
			return failedOut("continuity", fmt.Errorf("chain %s has head height %d in round %d, lower than %d in the round %d that you hold: the chain went back", k, nm.Height, rep.leaf.Seq, om.Height, old.Seq))
		}
		cl, out := cs.open(k, r)
		if !out.ok() {
			return out
		}
		pth, err := cl.reader.ConsistencyProofBetween(om.Size(), nm.Size())
		if err != nil {
			return tileOutcome(err, cl.rel())
		}
		if err := merkle.VerifyConsistency(om.Size(), nm.Size(), om.LogRoot, nm.LogRoot, pth); err != nil {
			step := "tiles"
			if errors.Is(err, merkle.ErrOldRoot) {
				step = "continuity"
			}
			return failedOut(step, fmt.Errorf("chain %s: the log at size %d (round %d) is not an extension of the log at size %d that round %d named: %w", k, nm.Size(), rep.leaf.Seq, om.Size(), old.Seq, err))
		}
		n++
	}
	for k := range byKey {
		known := false
		for _, om := range old.Members {
			if (memberKey{om.ChainID, om.Epoch}) == k {
				known = true
			}
		}
		if !known && (o.only == nil || k == *o.only) {
			rep.notes = append(rep.notes, fmt.Sprintf("new chain %s: it is in round %d and was not in round %d", k, rep.leaf.Seq, old.Seq))
		}
	}
	r.ok("continuity", fmt.Sprintf("round %d against round %d that you hold: %d chain(s) extend what the older round named, none went back, none vanished without a record", rep.leaf.Seq, old.Seq, n))
	return outcome{}
}

// ---- the command ----

func (e *env) runAggregate(args []string) int {
	fs, asJSON := e.newFlagsWithExit("aggregate", "--trust FILE --aggregate-id HEX [--source SOURCE] [--previous NOTEFILE] [--chains-from SOURCE] [--now TIME] [--json]",
		"Verify the aggregate log of a database: its checkpoint (a signature of a pinned key), its last\n"+
			"leaf as an entry of the signed tree, and each member of that leaf against the chain log it names\n"+
			"(the checkpoint of the chain log, the member's root as a prefix of that log, the block as the\n"+
			"leaf at the member's height). A member flagged retired needs a chain-retired event signed by\n"+
			"keys that the trust file authorises; a member flagged diverged is a failure; a member flagged\n"+
			"stale is status 3. With --previous (a checkpoint of this aggregate log you hold: a note or a state\n"+
			"file) it also proves that the aggregate log extends it, that no chain vanished without a record,\n"+
			"that no head went back and that every chain log extends the log the older round named.\n"+
			"--source is a directory, a .tar.gz file or an https address; the chain logs are read from the\n"+
			"same place unless --chains-from names another.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	source := fs.String("source", ".", "directory, .tar.gz or https address of the publication or bundle")
	aggID := fs.String("aggregate-id", "", "the aggregate id, 64 lower case hex characters")
	previous := fs.String("previous", "", "a checkpoint of this aggregate log that you hold (a note, or a state file)")
	chainsFrom := fs.String("chains-from", "", "where the chain logs are read when not in --source")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "aggregate"
	r := &result{cmd: cmd}
	out := e.aggregateRun(r, *trustPath, *source, *aggID, *previous, *chainsFrom, nowOf(fs, nowFlag))
	switch out.kind {
	case outInput:
		return e.inputErrorInfo(cmd, *asJSON, out.err, r.inputInfo())
	case outFailed:
		return e.finishFailed(r, *asJSON, out.step, out.err)
	case outIncomplete:
		return e.finishIncomplete(r, *asJSON, out.missing)
	}
	return e.finishOK(r, *asJSON)
}

var hex64RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// aggregateRun does the work of the command and fills r; its outcome is how it ended.
func (e *env) aggregateRun(r *result, trustPath, source, aggID, previous, chainsFrom string, now nowArg) outcome {
	if !hex64RE.MatchString(aggID) {
		return inputOut(errors.New("--aggregate-id is required: 64 lower case hex characters"))
	}
	p, err := loadPubInputs(trustPath, source, now)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	r.notes = append(r.notes, p.notes...)
	var o aggOptions
	if chainsFrom != "" {
		cs, err := pubsrc.Open(chainsFrom, pubsrc.Options{})
		if err != nil {
			return inputOut(fmt.Errorf("--chains-from: %w", err))
		}
		defer cs.Close()
		o.chainSrc = cs
	}
	var held *heldNote
	if previous != "" {
		fileData, err := readArg(previous, maxStateFile)
		if err != nil {
			return inputOut(err)
		}
		oldData, rec, err := decodeHeld(fileData)
		if err != nil {
			return inputOut(fmt.Errorf("--previous %s: %w", previous, err))
		}
		if _, err := signednote.Parse(oldData); err != nil {
			return inputOut(fmt.Errorf("--previous %s is not a signed note: %w", previous, err))
		}
		in := p.logIn(aggSpec(aggID), nil)
		h, hnotes, verr := in.verifyHeld(oldData, rec, "--previous "+previous, rec != nil)
		if verr != nil {
			var ie *inputErr
			if errors.As(verr, &ie) {
				return inputOut(ie.error)
			}
			return inputOut(verr)
		}
		r.notes = appendNew(r.notes, hnotes)
		held = h
		o.previous = h.cp
		r.keepInfo = true
		r.info = map[string]any{}
		oldWeakInfo(r.info, held)
	}
	rep, out := p.verifyAggregate(r, aggID, o)
	if !out.ok() {
		return out
	}
	if r.info == nil {
		r.info = map[string]any{}
	}
	cp := rep.log.cp
	r.info["origin"], r.info["size"], r.info["root"] = cp.Origin, cp.Size, hex.EncodeToString(cp.Root[:])
	r.info["signers"] = signerPrints(rep.log.signers)
	r.notes = append(r.notes, rep.notes...)
	r.notProved = []string{
		"when the round was made: no anchor is checked in this command, and generated_at is the leader's clock",
		"that the signer shows the same history to everyone: a split view needs witnesses or a monitor",
		aggregateGlobalSnapshot,
		"that the set of chains is the set the database has: the catalog is the leader's claim; only a chain that was present and vanished is noticed, and only against a round you hold (--previous)",
		"the earlier leaves of the aggregate log: only the last one (and, with --previous, the one you hold) was read; the consistency of the log shows that no earlier round was changed",
		"that the trust file was obtained outside the cluster: that is your responsibility",
	}
	if rep.leaf == nil {
		r.headline = fmt.Sprintf("the aggregate checkpoint of %s is signed by a trusted key; the log is empty.", cp.Origin)
		return outcome{}
	}
	r.info["leaf_index"], r.info["round"], r.info["generated_at"] = rep.index, rep.leaf.Seq, chain.FormatUS(rep.leaf.GeneratedAtUS)
	r.info["members"] = rep.info
	if held != nil {
		r.info["previous_size"] = o.previous.Size
	} else {
		r.notProved = append(r.notProved, "continuity with an earlier round: no --previous was given, so a chain that went back or vanished since a checkpoint you hold is not noticed")
	}
	r.headline = fmt.Sprintf("the aggregate checkpoint of %s at size %d is signed by a trusted key and its last leaf (round %d, %d member(s)) matches the chain logs.", cp.Origin, cp.Size, rep.leaf.Seq, len(rep.leaf.Members))
	if len(rep.incomplete) > 0 {
		return outcome{kind: outIncomplete, missing: rep.incomplete}
	}
	return outcome{}
}
