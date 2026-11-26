// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/bundleidx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
)

var regexp16 = regexp.MustCompile(`^[0-9a-f]{32}$`)

// maxBundleProof is the largest proof file read from a bundle (a proof of the log leg has no chain of headers).
const maxBundleProof = 64 << 20

func (e *env) runBundle(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "kingres-verify bundle: expected verify or fetch")
		return exit(ExitUsage)
	}
	switch args[0] {
	case "verify":
		return e.runBundleVerify(args[1:])
	case "fetch":
		return e.runBundleFetch(args[1:])
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, "Usage: kingres-verify bundle verify|fetch [flags]\n\n  verify  check everything that applies in a bundle (a directory or .tar.gz of the layout of a publication, with bundle.json)\n  fetch   copy a publication, or some logs of it, from an HTTP address to a directory\n\nExit status: see the help of each.\n")
		return exit(ExitOK)
	}
	fmt.Fprintf(e.stderr, "kingres-verify bundle: unknown subcommand %s, expected verify or fetch\n", untrusted(fmt.Sprintf("%q", args[0])))
	return exit(ExitUsage)
}

// ---- bundle verify ----

type bundleVerifyArgs struct {
	trust, bundle      string
	requireAnchors     int
	requireAttestation bool
	attTable           int64
	acceptEpoch        []string
	wormURLs, headers  []string
	now                nowArg
}

func (e *env) runBundleVerify(args []string) int {
	fs, asJSON := e.newFlagsWithExit("bundle verify", "--trust FILE BUNDLE [--require-anchors N] [--require-attestation] [--attestation-table-id N] [--accept-epoch-closed SEQ ...] [--worm-url [ID=]URL ...] [--btc-header HEIGHT:HEX80 ...] [--now TIME]",
		"Verify everything that applies in a bundle (a directory, a .tar.gz or an https address): bundle.json is only an\n"+
			"index, and every fact is re-derived. Every checkpoint of every log in the layout is verified, the tiles are\n"+
			"bound to the signed root, every entry is checked (block headers: heights, links, times; aggregate leaves:\n"+
			"sequence and times), every aggregate's last leaf against its chain logs, every proof against the signed\n"+
			"chain log (and against the aggregate when it names the chain), every attestation. The anchors of every\n"+
			"aggregate checkpoint of the bundle are verified as the anchors command does (the means are --worm-url, --btc-header\n"+
			"and the tsa_roots of the trust file: an anchor without its means is status 3, or a note when --require-anchors N is\n"+
			"given and N anchors verified). An epoch that a verified epoch-closed event closed is a failure at step epoch-closed\n"+
			"that says what the event states, unless --accept-epoch-closed SEQ names the event: then it is printed as a finding.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	reqAnch := fs.Int("require-anchors", 0, "status 3 unless at least this many anchors were verified (then an anchor that could not be verified is a note)")
	reqAtt := fs.Bool("require-attestation", false, "a proof in the bundle without an attestation is a failure")
	attTable := fs.Int64("attestation-table-id", -1, "the table_id of kingres.ledger_attestation, needed to check the attestations of the bundle")
	var acceptEpoch, wormURLs, headers logListFlag
	fs.Var(&acceptEpoch, "accept-epoch-closed", "the number of an epoch-closed event whose finding you accept (repeatable)")
	fs.Var(&wormURLs, "worm-url", "URL of the WORM object version (anonymous or presigned), or TARGET=URL (repeatable)")
	fs.Var(&headers, "btc-header", "HEIGHT:HEX of a Bitcoin block header (80 bytes, 160 hex digits) you obtained yourself (repeatable)")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "bundle verify"
	r := &result{cmd: cmd}
	var o outcome
	if fs.NArg() != 1 {
		o = inputOut(errors.New("give exactly one bundle: a directory, a .tar.gz file or an https address"))
	} else {
		o = e.bundleVerifyRun(r, bundleVerifyArgs{trust: *trustPath, bundle: fs.Arg(0), requireAnchors: *reqAnch, requireAttestation: *reqAtt, attTable: *attTable, acceptEpoch: acceptEpoch, wormURLs: wormURLs, headers: headers, now: nowOf(fs, nowFlag)})
	}
	switch o.kind {
	case outInput:
		return e.inputErrorInfo(cmd, *asJSON, o.err, r.inputInfo())
	case outFailed:
		return e.finishFailed(r, *asJSON, o.step, o.err)
	case outIncomplete:
		return e.finishIncomplete(r, *asJSON, o.missing)
	}
	return e.finishOK(r, *asJSON)
}

type foundLog struct {
	spec    logSpec
	claimed *bundleidx.Log
}

func (e *env) bundleVerifyRun(r *result, a bundleVerifyArgs) outcome {
	if a.requireAnchors < 0 {
		return inputOut(errors.New("--require-anchors must not be negative"))
	}
	acc, err := newEpochAccept(a.acceptEpoch)
	if err != nil {
		return inputOut(err)
	}
	aopts, err := parseAnchorMeans(a.wormURLs, a.headers)
	if err != nil {
		return inputOut(err)
	}
	p, err := loadPubInputs(a.trust, a.bundle, a.now)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	if o := p.gate(); !o.ok() {
		return o
	}
	r.notes = append(r.notes, p.notes...)
	raw, err := p.src.ReadFile("bundle.json", bundleidx.MaxSize)
	if err != nil {
		if absent(err) {
			return inputOut(errors.New("not a bundle: bundle.json is missing (a publication is read with checkpoint, aggregate and monitor)"))
		}
		return pubOutcome(classifyPub("bundle.json", err))
	}
	ix, err := bundleidx.Parse(raw)
	if err != nil {
		return failedOut("bundle-index", fmt.Errorf("bundle.json: %w", err))
	}
	if ix.OriginPrefix != p.trust.OriginPrefix {
		return failedOut("bundle-index", fmt.Errorf("bundle.json says the origin prefix is %q and the trust file pins %q: this is a bundle of another cluster, or of another generation", ix.OriginPrefix, p.trust.OriginPrefix))
	}
	r.ok("bundle-index", fmt.Sprintf("bundle.json is well formed (created %s by its own account, %d log(s), %d proof(s), %d anchor file(s)); it is an index and nothing in it was believed", ix.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"), len(ix.Logs), len(ix.Proofs), len(ix.Anchors)))
	if ix.Notes != "" {
		r.notes = append(r.notes, "the bundle's own notes (not verified): "+ix.Notes)
	}
	logs := map[string]*foundLog{}
	for i := range ix.Logs {
		l := &ix.Logs[i]
		var s logSpec
		if l.Kind == "chain" {
			s = chainSpec(l.ChainID, l.Epoch)
		} else {
			// an aggregate's id is the last element of its origin
			id := strings.TrimPrefix(l.Origin, ix.OriginPrefix+"/a/")
			if !hex64RE.MatchString(id) {
				return failedOut("bundle-index", fmt.Errorf("the aggregate log %q has no origin of the form <prefix>/a/<id>", l.Origin))
			}
			s = aggSpec(id)
		}
		if l.Origin != s.origin(ix.OriginPrefix) || l.Checkpoint != s.rel()+"/checkpoint" {
			return failedOut("bundle-index", fmt.Errorf("bundle.json names the log %q with the checkpoint %q, and the layout of a publication has the origin %q with the checkpoint %q", l.Origin, l.Checkpoint, s.origin(ix.OriginPrefix), s.rel()+"/checkpoint"))
		}
		logs[slugOf(s)] = &foundLog{spec: s, claimed: l}
	}
	// the layout: logs the index does not name are verified as well
	if dirs, err := e.layoutLogs(p); err != nil {
		r.notes = append(r.notes, "the layout was not searched for logs the index does not name: "+err.Error())
	} else {
		for _, s := range dirs {
			if logs[slugOf(s)] == nil {
				logs[slugOf(s)] = &foundLog{spec: s}
				r.notes = append(r.notes, "the log "+s.kindAndShort()+" is in the layout and not in bundle.json: it is verified anyway")
			}
		}
	}
	if len(logs) == 0 {
		return inputOut(errors.New("the bundle holds no log: nothing to verify"))
	}
	slugs := make([]string, 0, len(logs))
	for s := range logs {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	var inc []string
	addInc := func(o outcome) { inc = append(inc, o.missing...) }
	aggMembers := map[string]map[string]uint32{} // aggregate id -> chain id -> epoch of a member of its last leaf
	nChains, nAggs := 0, 0
	bc := newBundleChains()
	var aggLogs []*openedLog
	var inClosed []proofInClosed
	// chain logs first, then aggregates
	for _, s := range slugs {
		fl := logs[s]
		if fl.spec.kind != "chain" {
			continue
		}
		ol, notes, out := p.openLog(fl.spec, nil)
		r.notes = appendNew(r.notes, notes)
		if out.kind == outIncomplete {
			addInc(out)
			continue
		}
		if !out.ok() {
			return out
		}
		if fl.claimed != nil && fl.claimed.Size != ol.cp.Size {
			return failedOut("bundle-index", fmt.Errorf("bundle.json says the log %s has size %d and its signed checkpoint says %d", fl.spec.rel(), fl.claimed.Size, ol.cp.Size))
		}
		cid, _ := hex.DecodeString(fl.spec.id)
		var c32 [32]byte
		copy(c32[:], cid)
		bc.add(c32, uint32(fl.spec.epoch), ol) // #nosec G115 -- the epoch has at most 9 digits
		res, out := auditChainLog(ol, c32, 0)
		if out.kind == outIncomplete {
			addInc(out)
			continue
		}
		if !out.ok() {
			return out
		}
		nf, listed, lo := auditLayout(ol)
		if lo.kind == outIncomplete {
			addInc(lo)
		} else if !lo.ok() {
			return lo
		}
		if !listed {
			r.notes = appendNew(r.notes, []string{"the tile directories of " + fl.spec.rel() + " were not listed (this source cannot list): tiles of older sizes were not checked"})
		}
		_ = nf
		r.ok("log "+fl.spec.kindAndShort(), fmt.Sprintf("checkpoint size %d, root %x, signed by %s; %d block header(s) checked and bound to the signed root", ol.cp.Size, ol.cp.Root[:8], describeSigners(ol.signers), res.checked))
		nChains++
	}
	for _, s := range slugs {
		fl := logs[s]
		if fl.spec.kind != "aggregate" {
			continue
		}
		rep, out := p.verifyAggregate(r, fl.spec.id, aggOptions{accept: acc})
		if out.kind == outIncomplete {
			addInc(out)
			continue
		}
		if !out.ok() {
			return out
		}
		inc = append(inc, rep.incomplete...)
		if fl.claimed != nil && fl.claimed.Size != rep.log.cp.Size {
			return failedOut("bundle-index", fmt.Errorf("bundle.json says the log %s has size %d and its signed checkpoint says %d", fl.spec.rel(), fl.claimed.Size, rep.log.cp.Size))
		}
		if _, out := auditAggregateLog(rep.log, 0); !out.ok() {
			if out.kind == outIncomplete {
				addInc(out)
				continue
			}
			return out
		}
		if _, listed, lo := auditLayout(rep.log); lo.kind == outIncomplete {
			addInc(lo)
		} else if !lo.ok() {
			return lo
		} else if !listed {
			r.notes = appendNew(r.notes, []string{"the tile directories of " + fl.spec.rel() + " were not listed (this source cannot list): tiles of older sizes were not checked"})
		}
		if rep.leaf != nil {
			m := map[string]uint32{}
			for _, mb := range rep.leaf.Members {
				m[hex.EncodeToString(mb.ChainID[:])] = mb.Epoch
			}
			aggMembers[fl.spec.id] = m
		}
		aggLogs = append(aggLogs, rep.log)
		nAggs++
	}
	// proofs and attestations
	for i, pe := range ix.Proofs {
		label := fmt.Sprintf("proof %d", i+1)
		data, err := p.src.ReadFile(pe.File, maxBundleProof)
		if err != nil {
			o := pubOutcome(classifyPub(pe.File, err))
			if o.kind == outIncomplete {
				addInc(o)
				continue
			}
			return o
		}
		var pr proof.Proof
		chosen, sub, out := e.proofEpochs(r, p, data, bc, &pr)
		if out.kind == outIncomplete {
			addInc(out)
			continue
		}
		if !out.ok() {
			if out.kind == outFailed {
				out.err = fmt.Errorf("%s (%s): %w", label, pe.File, out.err)
			}
			return out
		}
		for _, st := range sub.steps {
			r.ok(label+" "+st.Name, st.Detail)
		}
		cidHex := hex.EncodeToString(pr.ChainID[:])
		var aggID string
		var epoch uint32
		for id, m := range aggMembers {
			if ep, ok := m[cidHex]; ok {
				aggID, epoch = id, ep
			}
		}
		if chosen > 0 && len(bc.epochs[pr.ChainID]) > 1 {
			r.notes = append(r.notes, fmt.Sprintf("%s: the block is shown in epoch %d of the chain, of %d epoch(s) in the bundle", label, chosen, len(bc.epochs[pr.ChainID])))
		}
		if chosen > 0 {
			if w, werr := p.events(); werr == nil && w != nil {
				if _, closed := w.EpochClosed[events.ChainKey(pr.ChainID, chosen)]; closed {
					inClosed = append(inClosed, proofInClosed{label: label, chain: pr.ChainID, epoch: chosen, height: pr.Block.Height, leaf: merkle.LeafHash(pr.Block.BlockHashInput())})
				}
			}
		}
		if aggID != "" && chosen > 0 && epoch != chosen {
			r.notes = append(r.notes, fmt.Sprintf("%s: the aggregate log %s covers epoch %d of the chain and the block is in epoch %d, so the aggregate leg was not made", label, aggID[:8], epoch, chosen))
		} else if aggID != "" {
			sub2 := &result{cmd: r.cmd}
			out := e.proofLegCore(sub2, p, bytesOpener(data), proof.Expect{}, proofLegArgs{leg: "aggregate", aggID: aggID, epoch: uint64(epoch), accept: acc}, nil)
			if !out.ok() {
				if out.kind == outFailed {
					out.err = fmt.Errorf("%s (%s) against the aggregate: %w", label, pe.File, out.err)
				}
				if out.kind == outIncomplete {
					addInc(out)
				} else {
					return out
				}
			} else {
				r.ok(label+" aggregate-leg", "the block is also shown through the aggregate log "+aggID[:8])
			}
		} else {
			r.notes = append(r.notes, label+": no aggregate of the bundle names the chain of the proof, so the aggregate leg was not made")
		}
		if pe.AttestationFile == "" {
			if a.requireAttestation {
				return failedOut("attestation-required", fmt.Errorf("%s (%s) has no attestation in the bundle, and --require-attestation was given (a row that was never presented cannot be seen)", label, pe.File))
			}
			continue
		}
		if a.attTable < 0 || a.attTable > 1<<31-1 {
			inc = append(inc, fmt.Sprintf("%s has an attestation and --attestation-table-id was not given: the attestation was not checked", label))
			continue
		}
		attData, err := p.src.ReadFile(pe.AttestationFile, maxBundleProof)
		var colsData []byte
		if err == nil {
			colsData, err = p.src.ReadFile(pe.AttestationColumns, maxFileSize)
		}
		if err != nil {
			o := pubOutcome(classifyPub(pe.AttestationFile, err))
			if o.kind == outIncomplete {
				addInc(o)
				continue
			}
			return o
		}
		sub3 := &result{cmd: r.cmd}
		out = e.attestCore(sub3, attestInput{an: &anchorer{p: p, leg: "chain", epoch: uint64(chosen)}, rowOpen: bytesOpener(data), attOpen: bytesOpener(attData), attCols: colsData,
			attTableID: uint32(a.attTable), trust: p, exp: proof.Expect{}}) // #nosec G115 -- range checked
		if !out.ok() {
			if out.kind == outFailed {
				out.err = fmt.Errorf("%s attestation: %w", label, out.err)
			}
			if out.kind == outIncomplete {
				addInc(out)
				continue
			}
			return out
		}
		r.ok(label+" attestation", sub3.headline)
		r.notes = appendNew(r.notes, sub3.notes)
		r.notProved = appendNew(r.notProved, sub3.notProved)
	}
	// the epochs: a closed epoch is a finding, a failure unless accepted (the verdict is held until the end, so that a failure of
	// anchors that is found below is not hidden by it)
	epochInfo, epochOut := p.auditEpochs(r, bc, inClosed, acc)
	if epochOut.kind == outIncomplete {
		addInc(epochOut)
		epochOut = outcome{}
	}
	// events: a bundle that carries them is checked as `aggregate` checks them
	if w, werr := p.events(); werr == nil && w.Count > 0 {
		unverified := len(w.Skipped)
		r.ok("events", fmt.Sprintf("%d event(s) read: %d verified under the trust file, %d NOT verified (no authorised key signed them), %d chain(s) retired by a verified event", w.Count, w.Count-unverified, unverified, len(w.Retired)))
		if unverified > 0 {
			inc = append(inc, fmt.Sprintf("%d event(s) of the bundle were read and are not verified: no key the trust chain authorises signed them (they are listed in the notes); a key they add or a key they revoke or retire is not known to this run", unverified))
		}
	} else if werr != nil {
		if o := eventsOutcome(werr, "the events of the bundle could not be read"); o.kind == outIncomplete {
			addInc(o)
		} else {
			return o
		}
	}
	// anchors
	anchorsVerified, anchorsUnver, ao := e.bundleAnchors(r, p, aggLogs, ix.Anchors, aopts)
	if !ao.ok() {
		return ao
	}
	if anchorsVerified > 0 {
		r.ok("anchors", fmt.Sprintf("%d anchor(s) of the aggregate checkpoint(s) of the bundle verified (each digest recomputed from the signed log, the evidence believed in nothing)", anchorsVerified))
		r.notes = appendNew(r.notes, []string{"anchors: every time printed is what the outside party says (a store, a time-stamp authority, a Bitcoin miner); no revocation status of a time-stamp certificate was read, and a Bitcoin header is shown to meet its own work target only"})
	}
	if a.requireAnchors > 0 {
		// the count decides: what else could not be checked is a note
		for _, u := range anchorsUnver {
			r.notes = append(r.notes, "not required, and not verified: "+u)
		}
		if anchorsVerified < a.requireAnchors {
			inc = append(inc, fmt.Sprintf("--require-anchors %d: %d anchor(s) verified", a.requireAnchors, anchorsVerified))
		}
	} else {
		inc = append(inc, anchorsUnver...)
	}
	if !epochOut.ok() {
		return epochOut
	}
	r.notes = append(r.notes, acc.unusedNotes()...)
	r.notes = append(r.notes, p.restoreNotes()...)
	timeClaim := "when anything in the bundle was made: no anchor was verified, every time is the signer's claim"
	if anchorsVerified > 0 {
		timeClaim = "when anything in the bundle was made that no verified anchor covers: block and event times are the signer's claim, and an anchor shows only that one digest existed by its time"
	}
	r.notProved = appendNew(r.notProved, []string{
		timeClaim,
		"that the signer shows the same history to everyone: a split view needs witnesses or a monitor",
		"that these are the latest checkpoints: an older genuine bundle verifies too; compare with a checkpoint you hold (checkpoint --state, monitor)",
		aggregateGlobalSnapshot,
		"that the trust file was obtained outside the cluster: that is your responsibility"})
	r.info = map[string]any{"chain_logs": nChains, "aggregate_logs": nAggs, "proofs": len(ix.Proofs), "anchors_listed": len(ix.Anchors), "anchors_verified": anchorsVerified}
	if len(epochInfo) > 0 {
		r.info["epochs_closed"] = epochInfo
	}
	r.headline = fmt.Sprintf("the bundle holds %d chain log(s), %d aggregate log(s) and %d proof(s), and everything in it that was checked is consistent.", nChains, nAggs, len(ix.Proofs))
	if len(inc) > 0 {
		return outcome{kind: outIncomplete, missing: inc}
	}
	return outcome{}
}

// layoutLogs lists the logs of the layout of a source (c/<id>/e<N>, a/<id>).
func (e *env) layoutLogs(p *pubInputs) ([]logSpec, error) {
	var out []logSpec
	ids, _, err := p.src.SubDirs("c")
	if err != nil && !absent(err) {
		return nil, err
	}
	for _, id := range ids {
		if !hex64RE.MatchString(id) {
			continue
		}
		eps, _, err := p.src.SubDirs("c/" + id)
		if err != nil {
			return nil, err
		}
		for _, ep := range eps {
			if m := epochDirRE.FindStringSubmatch(ep); m != nil {
				n, _ := strconv.ParseUint(m[1], 10, 64)
				out = append(out, chainSpec(id, n))
			}
		}
	}
	aggs, _, err := p.src.SubDirs("a")
	if err != nil && !absent(err) {
		return nil, err
	}
	for _, id := range aggs {
		if hex64RE.MatchString(id) {
			out = append(out, aggSpec(id))
		}
	}
	return out, nil
}
