// SPDX-License-Identifier: AGPL-3.0-only

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
	now                nowArg
}

func (e *env) runBundleVerify(args []string) int {
	fs, asJSON := e.newFlagsWithExit("bundle verify", "--trust FILE BUNDLE [--require-anchors N] [--require-attestation] [--attestation-table-id N] [--now TIME]",
		"Verify everything that applies in a bundle (a directory, a .tar.gz or an https address): bundle.json is only an\n"+
			"index, and every fact is re-derived. Every checkpoint of every log in the layout is verified, the tiles are\n"+
			"bound to the signed root, every entry is checked (block headers: heights, links, times; aggregate leaves:\n"+
			"sequence and times), every aggregate's last leaf against its chain logs, every proof against the signed\n"+
			"chain log (and against the aggregate when it names the chain), every attestation. Anchors are not verified\n"+
			"by this version: a bundle that lists them, or --require-anchors above 0, ends with status 3.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	reqAnch := fs.Int("require-anchors", 0, "fail (status 3) unless this many anchors were verified: anchors are a later step, so any N above 0 ends with 3")
	reqAtt := fs.Bool("require-attestation", false, "a proof in the bundle without an attestation is a failure")
	attTable := fs.Int64("attestation-table-id", -1, "the table_id of kingres.ledger_attestation, needed to check the attestations of the bundle")
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
		o = e.bundleVerifyRun(r, bundleVerifyArgs{trust: *trustPath, bundle: fs.Arg(0), requireAnchors: *reqAnch, requireAttestation: *reqAtt, attTable: *attTable, now: nowOf(fs, nowFlag)})
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
		rep, out := p.verifyAggregate(r, fl.spec.id, aggOptions{})
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
		sub := &result{cmd: r.cmd}
		var pr proof.Proof
		out := e.proofLegCore(sub, p, bytesOpener(data), proof.Expect{}, proofLegArgs{leg: "chain"}, &pr)
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
		if aggID != "" {
			sub2 := &result{cmd: r.cmd}
			out := e.proofLegCore(sub2, p, bytesOpener(data), proof.Expect{}, proofLegArgs{leg: "aggregate", aggID: aggID, epoch: uint64(epoch)}, nil)
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
		out = e.attestCore(sub3, attestInput{an: &anchorer{p: p, leg: "chain"}, rowOpen: bytesOpener(data), attOpen: bytesOpener(attData), attCols: colsData,
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
	if len(ix.Anchors) > 0 {
		inc = append(inc, fmt.Sprintf("the bundle lists %d anchor file(s) and this version does not verify anchors (WORM, RFC 3161, OpenTimestamps): they were not checked", len(ix.Anchors)))
	}
	if a.requireAnchors > 0 {
		inc = append(inc, fmt.Sprintf("--require-anchors %d: no anchor was verified, because this version cannot verify one", a.requireAnchors))
	}
	r.notProved = appendNew(r.notProved, []string{
		"when anything in the bundle was made: no anchor was checked, every time is the signer's claim",
		"that the signer shows the same history to everyone: a split view needs witnesses or a monitor",
		"that these are the latest checkpoints: an older genuine bundle verifies too; compare with a checkpoint you hold (checkpoint --state, monitor)",
		aggregateGlobalSnapshot,
		"that the trust file was obtained outside the cluster: that is your responsibility"})
	r.info = map[string]any{"chain_logs": nChains, "aggregate_logs": nAggs, "proofs": len(ix.Proofs), "anchors_listed": len(ix.Anchors), "anchors_verified": 0}
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
