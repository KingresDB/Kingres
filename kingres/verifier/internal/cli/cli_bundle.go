// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/bundleidx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/proof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
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
		r.ok("events", fmt.Sprintf("%d event(s) read and verified under the trust file, %d chain(s) retired", w.Count, len(w.Retired)))
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

// ---- bundle fetch ----

type bundleFetchArgs struct {
	source, out, trust string
	logs               []string
	since              uint64
	now                nowArg
}

func (e *env) runBundleFetch(args []string) int {
	fs, asJSON := e.newFlagsWithExit("bundle fetch", "--source URL --out DIR --log LOG [--log LOG ...] [--trust FILE] [--since-size N] [--now TIME]",
		"Mirror logs of a publication to a directory in the layout of the publication: keyring.json, events/*, and for each\n"+
			"log its checkpoint, the tiles of that size and the entry bundles. --log is chain:<64 hex>:e<N> or\n"+
			"aggregate:<64 hex> (an HTTP source cannot be listed). With --since-size N the tiles and bundles that end\n"+
			"at or before leaf N are not fetched (you hold them from an earlier fetch). Names are made from numbers\n"+
			"only, never from what the server says. With --trust every checkpoint is verified and the tiles are bound\n"+
			"to the signed root; without it the copy is NOT verified and the output says so. Run bundle verify on the\n"+
			"copy. Redirects are never followed; every request has a timeout.", exitTextWith3)
	source := fs.String("source", "", "https address of the publication (or a directory)")
	out := fs.String("out", "", "directory to write")
	trustPath := fs.String("trust", "", "trust file: verify the checkpoints and bind the tiles to them")
	var logs logListFlag
	fs.Var(&logs, "log", "a log to fetch (repeatable)")
	since := fs.Uint64("since-size", 0, "skip tiles and bundles that end at or before this leaf")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "bundle fetch"
	r := &result{cmd: cmd, word: "FETCHED"}
	o := e.bundleFetchRun(r, bundleFetchArgs{source: *source, out: *out, trust: *trustPath, logs: logs, since: *since, now: nowOf(fs, nowFlag)})
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

// fetcher writes what it reads below one directory (os.Root: no path leaves it).
type fetcher struct {
	src   pubsrc.Source
	root  *os.Root
	files int
	bytes int64
}

func (f *fetcher) get(rel string, limit int64, skipIfSize int64) (data []byte, err error) {
	if skipIfSize > 0 {
		if st, serr := f.root.Stat(rel); serr == nil && st.Mode().IsRegular() && st.Size() == skipIfSize {
			return nil, nil // immutable and already here
		}
	}
	data, err = f.src.ReadFile(rel, limit)
	if err != nil {
		return nil, err
	}
	if dir := filepath.Dir(filepath.FromSlash(rel)); dir != "." {
		if err := f.root.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("writing %s: %w", rel, err)
		}
	}
	tmp := rel + ".part"
	if err := f.root.WriteFile(tmp, data, 0o644); err != nil {
		return nil, fmt.Errorf("writing %s: %w", rel, err)
	}
	if err := f.root.Rename(tmp, rel); err != nil {
		_ = f.root.Remove(tmp)
		return nil, fmt.Errorf("writing %s: %w", rel, err)
	}
	f.files++
	f.bytes += int64(len(data))
	return data, nil
}

func (e *env) bundleFetchRun(r *result, a bundleFetchArgs) outcome {
	switch {
	case a.source == "":
		return inputOut(errors.New("missing --source"))
	case a.out == "":
		return inputOut(errors.New("missing --out"))
	case len(a.logs) == 0:
		return inputOut(errors.New("name the logs to fetch with --log: an HTTP source has no listing (a publication may serve logs.json, whose format is not defined yet)"))
	}
	var specs []logSpec
	for _, l := range a.logs {
		s, err := parseLogSpec(l)
		if err != nil {
			return inputOut(err)
		}
		if s.kind == "chain" && !s.hasEpoch {
			return inputOut(fmt.Errorf("--log %s: give the epoch, chain:%s:e<N>", l, s.id))
		}
		specs = append(specs, s)
	}
	src, err := pubsrc.Open(a.source, pubsrc.Options{})
	if err != nil {
		return inputOut(fmt.Errorf("--source: %w", err))
	}
	defer src.Close()
	if err := os.MkdirAll(a.out, 0o755); err != nil {
		return inputOut(fmt.Errorf("--out %s: %w", a.out, err))
	}
	root, err := os.OpenRoot(a.out)
	if err != nil {
		return inputOut(fmt.Errorf("--out %s: %w", a.out, err))
	}
	defer root.Close()
	f := &fetcher{src: src, root: root}
	var p *pubInputs
	if a.trust != "" {
		if p, err = loadPubInputs(a.trust, "", a.now); err != nil {
			return inputOut(err)
		}
		r.notes = append(r.notes, p.notes...)
	}
	// keyring and events
	if _, err := f.get("keyring.json", 1<<20, 0); err != nil && !absent(err) {
		return fetchOutcome(err, "keyring.json")
	}
	nev := 0
	for seq := uint64(0); seq < events.MaxEvents; seq++ {
		name := fmt.Sprintf("events/%06d", seq)
		if _, err := f.get(name, signednote.MaxNoteSize, 0); err != nil {
			if absent(err) {
				break
			}
			return fetchOutcome(err, name)
		}
		nev++
	}
	r.ok("events", fmt.Sprintf("%d event(s) copied", nev))
	for _, s := range specs {
		rel := s.rel()
		note, err := f.get(rel+"/checkpoint", signednote.MaxNoteSize, 0)
		if err != nil {
			return fetchOutcome(err, rel+"/checkpoint")
		}
		var size uint64
		if p != nil {
			in := p.logIn(s, src)
			cp, _, notes, verr := in.verifyNote(note)
			r.notes = appendNew(r.notes, notes)
			if verr != nil {
				var ie *inputErr
				if errors.As(verr, &ie) {
					return inputOut(ie.error)
				}
				step := "checkpoint"
				var ce *checkpoint.Error
				if errors.As(verr, &ce) {
					step = ce.Step
				}
				return failedOut(step, fmt.Errorf("%s: %w", rel, verr))
			}
			size = cp.Size
		} else {
			n, err := signednote.Parse(note)
			if err != nil {
				return failedOut("parse", fmt.Errorf("%s: %w", rel, err))
			}
			cp, err := checkpoint.Parse(n.Text)
			if err != nil || !strings.HasSuffix(cp.Origin, "/"+rel) {
				return failedOut("checkpoint", fmt.Errorf("%s: the note is not a checkpoint of this log (the signature was not checked: no --trust)", rel))
			}
			size = cp.Size
		}
		nt, err := f.fetchTiles(rel, size, a.since)
		if err != nil {
			return fetchOutcome(err, rel)
		}
		r.ok("log "+s.kindAndShort(), fmt.Sprintf("checkpoint of size %d and %d tile/bundle file(s) copied", size, nt))
		if p != nil {
			mp := &pubInputs{trust: p.trust, spec: a.out, now: p.now}
			if mp.src, err = pubsrc.OpenDir(a.out); err != nil {
				return inputOut(err)
			}
			ol, _, out := mp.openLog(s, nil)
			if out.ok() {
				out = bindLog(ol, a.since)
			}
			mp.close()
			if !out.ok() {
				return out
			}
			r.ok("bind "+s.kindAndShort(), "the tiles copied lead to the signed root")
		}
	}
	r.info = map[string]any{"files": f.files, "bytes": f.bytes, "verified": p != nil, "out": a.out}
	r.headline = fmt.Sprintf("%d file(s), %d byte(s) copied to %s.", f.files, f.bytes, a.out)
	if p == nil {
		r.notes = append(r.notes, "the copy is NOT verified: no --trust was given")
	}
	if p == nil {
		r.notProved = append(r.notProved, "everything: no --trust was given, so no signature was verified and the tiles were not bound to a root; run bundle verify on the copy")
	} else {
		r.notProved = append(r.notProved, "the entries behind the tiles and the events: bundle verify checks them")
	}
	return outcome{}
}

func fetchOutcome(err error, what string) outcome {
	switch {
	case errors.Is(err, pubsrc.ErrUnreachable) || absent(err):
		return incompleteOut(fmt.Sprintf("%s could not be fetched: %v", what, err))
	case errors.Is(err, pubsrc.ErrRedirect), errors.Is(err, pubsrc.ErrNoListing):
		return inputOut(err)
	}
	return failedOut("fetch", fmt.Errorf("%s: %w", what, err))
}

// fetchTiles copies the tiles and entry bundles of the tree of a size.
func (f *fetcher) fetchTiles(rel string, size, since uint64) (int, error) {
	n := 0
	get := func(name string, want int, immutable bool) error {
		skip := int64(0)
		if immutable {
			skip = int64(want)
		}
		if _, err := f.get(rel+"/"+name, int64(want), skip); err != nil {
			if absent(err) && strings.Contains(name, ".p/") {
				return nil // a partial tile may be gone once the full tile exists (tlog-tiles)
			}
			return err
		}
		n++
		return nil
	}
	for level := 0; tlog.TileHeight*level < 63; level++ {
		count := size >> (tlog.TileHeight * uint(level))
		if count == 0 {
			break
		}
		full := count / tlog.TileWidth
		for i := uint64(0); i < full; i++ {
			if since > 0 && ((i+1)<<(tlog.TileHeight*uint(level+1))) <= since {
				continue
			}
			if err := get(tlog.TilePath(level, i, tlog.TileWidth), tlog.FullTile, true); err != nil {
				return n, err
			}
		}
		if w := int(count % tlog.TileWidth); w > 0 {
			if err := get(tlog.TilePath(level, full, w), w*tlog.HashSize, false); err != nil {
				return n, err
			}
		}
	}
	spec := rel
	maxEntry := format.BlockLeafSize
	if strings.HasPrefix(spec, "a/") {
		maxEntry = 1 << 16
	}
	full := size / tlog.TileWidth
	for i := uint64(0); i < full; i++ {
		if since > 0 && (i+1)*tlog.TileWidth <= since {
			continue
		}
		if _, err := f.get(rel+"/"+tlog.EntriesPath(i, tlog.TileWidth), int64(tlog.TileWidth)*int64(2+maxEntry), 0); err != nil {
			return n, err
		}
		n++
	}
	if w := int(size % tlog.TileWidth); w > 0 {
		if _, err := f.get(rel+"/"+tlog.EntriesPath(full, w), int64(w)*int64(2+maxEntry), 0); err != nil && !absent(err) {
			return n, err
		} else if err == nil {
			n++
		}
	}
	return n, nil
}

var _ = fs.ErrNotExist
