// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cli

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/anchorev"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/otsproof"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tsatoken"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/wormv"
)

// `anchors` (design 6 and 7.4, corrections 179 to 186 and 196 onwards): the anchors of one aggregate checkpoint. An
// anchor is evidence that the digest of a checkpoint existed at a time an outside party vouches for. Nothing in an evidence
// file is believed: the digest is recomputed from the signed aggregate log, and each kind is checked from the auditor's own inputs.

// parseHeader is replaced by tests that build a header without mining it.
var parseHeader = otsproof.ParseHeader

var btcHeaderRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,9}):([0-9a-fA-F]{160})$`)

type anchorsArgs struct {
	trust, source, aggID string
	size                 string
	require              string
	wormURLs             []string
	headers              []string
	evidence             []string
	targets              []string
	now                  nowArg
}

func (e *env) runAnchors(args []string) int {
	fs, asJSON := e.newFlagsWithExit("anchors", "--trust FILE --aggregate-id HEX [--source SOURCE] [--size N] [--target ID ...] [--require worm,tsa,ots|any] [--worm-url [ID=]URL ...] [--btc-header HEIGHT:HEX80 ...] [--now TIME] [--json]",
		"Verify the anchors of an aggregate checkpoint of the publication (the evidence files a/<id>/anchors/<size>/<target>.json). The\n"+
			"digest an anchor must stamp is recomputed from the signed aggregate log (SHA-256 of the checkpoint text); the evidence\n"+
			"file is never believed. worm: the object key is compared with the checkpoint; the object itself is checked only with\n"+
			"--worm-url (the bytes, the signature on the note, the version and lock headers the store returns): without it the anchor is\n"+
			"status 3. tsa: the RFC 3161 token is verified offline against the tsa_roots of YOUR trust file (no root: status 3), at its\n"+
			"genTime; the time printed is the authority's word. ots: the operation tree from the digest is computed, and a Bitcoin\n"+
			"attestation is checked against a block header YOU give with --btc-header (without it status 3; a header whose merkle root is\n"+
			"not the one the tree reaches, or that is not a mined header, is status 1). --size defaults to the size of the signed\n"+
			"aggregate checkpoint. An HTTP source cannot list: name the targets with --target. Without --require every evidence file read\n"+
			"must verify; with it, each named kind (or any one for 'any') must have a verified anchor, and a failure of any is status 1.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON), with tsa_roots for time-stamp anchors")
	source := fs.String("source", ".", "directory, .tar.gz or https address of the publication or bundle")
	aggID := fs.String("aggregate-id", "", "the aggregate id, 64 lower case hex characters")
	size := fs.String("size", "", "the size of the anchored checkpoint (default: the signed checkpoint's size)")
	require := fs.String("require", "", "comma list of worm, tsa, ots that must each have a verified anchor, or any")
	var wormURLs, headers, targets logListFlag
	fs.Var(&wormURLs, "worm-url", "URL of the WORM object version (anonymous or presigned), or TARGET=URL (repeatable)")
	fs.Var(&headers, "btc-header", "HEIGHT:HEX of a Bitcoin block header (80 bytes, 160 hex digits) you obtained yourself (repeatable)")
	fs.Var(&targets, "target", "an anchor target id whose evidence file to read (repeatable; needed for an HTTP source)")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "anchors"
	r := &result{cmd: cmd}
	o := e.anchorsRun(r, anchorsArgs{trust: *trustPath, source: *source, aggID: *aggID, size: *size, require: *require, wormURLs: wormURLs, headers: headers, targets: targets, now: nowOf(fs, nowFlag)})
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

// anchorsState collects what the checks of the anchors found.
type anchorsState struct {
	verified map[string]int // by kind
	unver    []string
	times    []string
}

func (e *env) anchorsRun(r *result, a anchorsArgs) outcome {
	if !hex64RE.MatchString(a.aggID) {
		return inputOut(errors.New("--aggregate-id is required: 64 lower case hex characters"))
	}
	need := map[string]bool{}
	anyKind := false
	if a.require != "" {
		for _, k := range strings.Split(a.require, ",") {
			switch k {
			case "any":
				anyKind = true
			case "worm", "tsa", "ots":
				need[k] = true
			default:
				return inputOut(fmt.Errorf("--require %q: expected worm, tsa, ots or any", a.require))
			}
		}
	}
	headers := map[uint64][]byte{}
	for _, h := range a.headers {
		m := btcHeaderRE.FindStringSubmatch(h)
		if m == nil {
			return inputOut(fmt.Errorf("--btc-header %q is not HEIGHT:<160 hex digits>", h))
		}
		n, _ := strconv.ParseUint(m[1], 10, 64)
		raw, _ := hex.DecodeString(m[2])
		if old, ok := headers[n]; ok && string(old) != string(raw) {
			return inputOut(fmt.Errorf("--btc-header: two different headers for height %d", n))
		}
		headers[n] = raw
	}
	p, err := loadPubInputs(a.trust, a.source, a.now)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	if o := p.gate(); !o.ok() {
		return o
	}
	r.notes = append(r.notes, p.notes...)
	ol, knotes, out := p.openLog(aggSpec(a.aggID), nil)
	r.notes = appendNew(r.notes, knotes)
	if !out.ok() {
		return out
	}
	r.ok("aggregate-checkpoint", fmt.Sprintf("%s, tree size %d, root %x, signed by %s of the trust file", ol.cp.Origin, ol.cp.Size, ol.cp.Root, describeSigners(ol.signers)))
	n := ol.cp.Size
	if a.size != "" {
		v, err := strconv.ParseUint(a.size, 10, 62)
		if err != nil || v == 0 {
			return inputOut(fmt.Errorf("--size %q is not a size from 1", a.size))
		}
		n = v
	}
	if n == 0 {
		return incompleteOut("the signed aggregate checkpoint has size 0: there is nothing anchored")
	}
	if n > ol.cp.Size {
		return incompleteOut(fmt.Sprintf("the anchored size %d is larger than the signed aggregate checkpoint you were served (size %d): the digest of that checkpoint cannot be recomputed from this log, so nothing was checked (the publication may be showing a log older than the anchor: get a newer one)", n, ol.cp.Size))
	}
	text, out := anchoredText(ol, n)
	if !out.ok() {
		return out
	}
	digest := sha256.Sum256(text)
	r.ok("digest", fmt.Sprintf("SHA-256 of the text of the aggregate checkpoint of size %d is %x (recomputed from the signed log, not read from any evidence)", n, digest[:8]))
	// the evidence files
	rel := fmt.Sprintf("a/%s/anchors/%d", a.aggID, n)
	names := map[string]bool{}
	for _, t := range a.targets {
		if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(t) {
			return inputOut(fmt.Errorf("--target %q is not a target id (letters, digits, - and _)", t))
		}
		names[t] = true
	}
	if len(names) == 0 {
		files, lerr := p.src.Files(rel)
		switch {
		case lerr == nil:
			for _, f := range files {
				if t, ok := strings.CutSuffix(f, ".json"); ok && regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(t) {
					names[t] = true
				}
			}
		case errors.Is(lerr, pubsrc.ErrNoListing):
			return inputOut(errors.New("this source cannot list directories: name the evidence files with --target ID"))
		case absent(lerr):
		default:
			return pubOutcome(classifyPub(rel, lerr))
		}
	}
	if len(names) == 0 {
		return incompleteOut(fmt.Sprintf("the publication holds no anchor for the aggregate checkpoint of size %d (%s): the checkpoint is not anchored, or the evidence was withheld", n, rel))
	}
	ids := make([]string, 0, len(names))
	for t := range names {
		ids = append(ids, t)
	}
	sort.Strings(ids)
	roots, rootsErr := tsaRoots(p.base.TSARoots)
	if rootsErr != nil {
		return inputOut(rootsErr)
	}
	wormTo, wormAll, err := parseWormURLs(a.wormURLs)
	if err != nil {
		return inputOut(err)
	}
	st, o := e.checkEvidenceFiles(r, p, ol, a.aggID, n, rel, digest, ids, roots, wormTo, wormAll, headers)
	if !o.ok() {
		return o
	}
	r.info = map[string]any{"size": n, "digest": hex.EncodeToString(digest[:]), "verified": st.verified, "unverified": len(st.unver)}
	r.notProved = []string{
		"that the time is true: every time printed is what the outside party says (a store's clock, a time-stamp authority, a Bitcoin miner), within the accuracy of that party",
		"that the anchored checkpoint is the latest, or that the signer shows the same history to everyone: a split view needs witnesses or a monitor",
		"that no other history was anchored elsewhere: an anchor shows that this digest existed by that time, not that no other digest did",
		"the revocation status of a time-stamp certificate (no OCSP or CRL is read), or that a Bitcoin header is on the best chain",
		aggregateGlobalSnapshot,
	}
	total := 0
	for _, c := range st.verified {
		total += c
	}
	r.headline = fmt.Sprintf("%d anchor(s) of the aggregate checkpoint of size %d verified.", total, n)
	var missing []string
	missing = append(missing, st.unver...)
	switch {
	case a.require == "":
	default:
		// only the required kinds decide; what else could not be checked is a note
		for _, u := range st.unver {
			r.notes = append(r.notes, "not required, and not verified: "+u)
		}
		missing = nil
		if anyKind && total == 0 {
			missing = append(missing, "--require any: no anchor was verified")
		}
		for k := range need {
			if st.verified[k] == 0 {
				missing = append(missing, fmt.Sprintf("--require %s: no %s anchor was verified", k, k))
			}
		}
		sort.Strings(missing)
	}
	if len(missing) > 0 {
		return outcome{kind: outIncomplete, missing: missing}
	}
	return outcome{}
}

func parseWormURLs(list []string) (map[string]string, string, error) {
	by := map[string]string{}
	all := ""
	for _, v := range list {
		if t, u, ok := strings.Cut(v, "="); ok && regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(t) {
			by[t] = u
			if _, err := wormv.CheckURL(u); err != nil {
				return nil, "", fmt.Errorf("--worm-url %s: %w", t, err)
			}
			continue
		}
		if all != "" {
			return nil, "", errors.New("--worm-url: give one plain URL at most, or TARGET=URL for each target")
		}
		if _, err := wormv.CheckURL(v); err != nil {
			return nil, "", fmt.Errorf("--worm-url: %w", err)
		}
		all = v
	}
	return by, all, nil
}

func tsaRoots(pems []string) (*x509.CertPool, error) {
	if len(pems) == 0 {
		return nil, nil
	}
	pool := x509.NewCertPool()
	for i, p := range pems {
		if !pool.AppendCertsFromPEM([]byte(p)) {
			return nil, fmt.Errorf("trust file: tsa_roots[%d] holds no certificate", i)
		}
	}
	return pool, nil
}

// anchoredText is the text of the aggregate checkpoint of size n, recomputed from the signed log: the very text of the
// signed note when n is its size, else the three lines for the root of the first n leaves, proved a prefix of the signed log.
func anchoredText(ol *openedLog, n uint64) ([]byte, outcome) {
	if n == ol.cp.Size {
		note, err := signednote.Parse(ol.note)
		if err != nil {
			return nil, inputOut(err)
		}
		return note.Text, outcome{}
	}
	root, err := ol.reader.RootAt(n)
	if err != nil {
		return nil, tileOutcome(err, ol.rel())
	}
	path, err := ol.reader.ConsistencyProofBetween(n, ol.cp.Size)
	if err != nil {
		return nil, tileOutcome(err, ol.rel())
	}
	if err := merkle.VerifyConsistency(n, ol.cp.Size, root, ol.cp.Root, path); err != nil {
		return nil, failedOut("tiles", fmt.Errorf("the first %d leaves of the aggregate log are not a prefix of the signed log of size %d: %w", n, ol.cp.Size, err))
	}
	return []byte(fmt.Sprintf("%s\n%d\n%s\n", ol.cp.Origin, n, base64.StdEncoding.EncodeToString(root[:]))), outcome{}
}

func (e *env) checkWORM(r *result, p *pubInputs, st *anchorsState, id string, f *anchorev.File, aggID string, digest [32]byte, n uint64, url string) outcome {
	label := "anchor worm " + id
	if err := wormv.CheckKey(f, aggID); err != nil {
		return failedOut("anchor-worm", fmt.Errorf("%s: %w", id, err))
	}
	if url == "" {
		st.unver = append(st.unver, fmt.Sprintf("worm anchor %s: the object was not fetched (no --worm-url): the evidence is the cluster's claim (version %q, lock %s until %s, last modified %s), and a claim that nobody re-fetched is not an anchor", id, f.WORM.VersionID, f.WORM.LockMode, f.WORM.RetainUntil.Format(time.RFC3339), f.WORM.LastModified.Format(time.RFC3339)))
		return outcome{}
	}
	obj, err := wormv.Fetch(url)
	if err != nil {
		var es *wormv.ErrStatus
		if errors.Is(err, wormv.ErrUnreachable) {
			st.unver = append(st.unver, fmt.Sprintf("worm anchor %s: the store could not be reached: %v", id, err))
			return outcome{}
		}
		if errors.As(err, &es) {
			return failedOut("anchor-worm", fmt.Errorf("%s: %w", id, err))
		}
		return inputOut(fmt.Errorf("--worm-url for %s: %w", id, err))
	}
	if probs, notes := wormv.Compare(f.WORM, obj); len(probs) > 0 {
		return failedOut("anchor-worm", fmt.Errorf("%s: %s", id, strings.Join(probs, "; ")))
	} else {
		for _, nn := range notes {
			r.notes = append(r.notes, id+": "+nn)
		}
	}
	in := p.logIn(aggSpec(aggID), nil)
	cp, _, _, verr := in.verifyNote(obj.Body)
	if verr != nil {
		return failedOut("anchor-worm", fmt.Errorf("%s: the object fetched is not a checkpoint of this aggregate signed by a key of your trust file: %w", id, verr))
	}
	if cp.Size != n {
		return failedOut("anchor-worm", fmt.Errorf("%s: the object holds a checkpoint of size %d and the anchor is for size %d", id, cp.Size, n))
	}
	note, _ := signednote.Parse(obj.Body)
	if sha256.Sum256(note.Text) != digest {
		return failedOut("anchor-worm", fmt.Errorf("%s: the text of the checkpoint in the object does not hash to the digest recomputed from the signed log", id))
	}
	when := "the store returned no Last-Modified: no time is given"
	if obj.LastModified != nil {
		when = "the store's own Last-Modified is " + obj.LastModified.Format(time.RFC3339) + " (the store's word)"
	}
	lock := "the lock was NOT confirmed by the store's answer (it is the cluster's word)"
	if wormv.Confirmed(obj) {
		lock = fmt.Sprintf("the store confirms the lock %s until %s", obj.LockMode, obj.RetainUntil.Format(time.RFC3339))
	}
	r.ok(label, fmt.Sprintf("the object fetched has the bytes, SHA-256 and length of the evidence, holds the checkpoint of size %d signed by a key of your trust file, and its text is the digest; %s; %s", n, lock, when))
	st.verified[anchorev.KindWORM]++
	return outcome{}
}

func checkTSA(r *result, st *anchorsState, id string, f *anchorev.File, digest [32]byte, roots *x509.CertPool) outcome {
	if roots == nil {
		st.unver = append(st.unver, fmt.Sprintf("tsa anchor %s: your trust file has no tsa_roots, so the token cannot be verified (the evidence names the authority %s; pin its certificate yourself)", id, f.TSA.URL))
		return outcome{}
	}
	res, err := tsatoken.Verify(f.TSA.Token, tsatoken.Options{Digest: digest, Roots: roots})
	if err != nil {
		return failedOut("anchor-tsa", fmt.Errorf("%s: %w", id, err))
	}
	if !res.GenTime.Truncate(time.Second).Equal(f.TSA.GenTime) || res.Serial != f.TSA.Serial || res.Policy != f.TSA.Policy || res.SignerKeySHA256 != f.TSA.SignerKeySHA256 {
		return failedOut("anchor-tsa", fmt.Errorf("%s: the evidence's gen_time, serial, policy or signer key does not match the token it carries", id))
	}
	acc := ""
	if res.Accuracy != "" {
		acc = ", accuracy " + res.Accuracy + " (printed, not used)"
	}
	r.ok("anchor tsa "+id, fmt.Sprintf("the token stamps the digest; it is signed by a certificate (key SHA-256 %x) that chains to a root of yours with id-kp-timeStamping, valid at genTime; genTime %s%s (the authority's word)", res.SignerKeySHA256[:8], res.GenTime.Format(time.RFC3339), acc))
	r.notes = appendNew(r.notes, []string{"tsa: no revocation status of the time-stamp certificate was checked (no OCSP, no CRL)"})
	st.verified[anchorev.KindTSA]++
	return outcome{}
}

func checkOTS(r *result, st *anchorsState, id string, f *anchorev.File, digest [32]byte, headers map[uint64][]byte) outcome {
	o := f.OTS
	if o.Digest != digest {
		return failedOut("anchor-ots", fmt.Errorf("%s: the evidence digest %x is not the digest recomputed from the signed log %x", id, o.Digest[:8], digest[:8]))
	}
	var atts []otsproof.Attestation
	pending := 0
	for _, c := range o.Calendars {
		as, err := otsproof.Walk(c.Fragment, digest[:])
		if err != nil {
			// an operation that is not computed here (otsproof.UnsupportedOpError) is a failure like any fragment that cannot be walked, by name
			return failedOut("anchor-ots", fmt.Errorf("%s: calendar %s: %w", id, c.URI, err))
		}
		for _, a := range as {
			switch a.Kind {
			case otsproof.Bitcoin:
				atts = append(atts, a)
			case otsproof.Pending:
				pending++
			}
		}
	}
	if o.Bitcoin != nil {
		found := false
		for _, a := range atts {
			if a.Height == o.Bitcoin.Height && string(a.Message) == string(o.Bitcoin.MerkleRoot[:]) {
				found = true
			}
		}
		if !found {
			return failedOut("anchor-ots", fmt.Errorf("%s: the evidence names a Bitcoin attestation at height %d on merkle root %x and no fragment holds it", id, o.Bitcoin.Height, o.Bitcoin.MerkleRoot[:8]))
		}
	}
	if len(atts) == 0 {
		st.unver = append(st.unver, fmt.Sprintf("ots anchor %s: no Bitcoin attestation yet (%d pending): the calendars had not confirmed when the evidence was written", id, pending))
		return outcome{}
	}
	var best *otsproof.Header
	bestH := uint64(0)
	var unchecked []string
	for _, a := range atts {
		raw, ok := headers[a.Height]
		if !ok {
			unchecked = append(unchecked, strconv.FormatUint(a.Height, 10))
			continue
		}
		h, err := parseHeader(raw)
		if err != nil {
			return failedOut("anchor-ots", fmt.Errorf("%s: the header you gave for height %d: %w", id, a.Height, err))
		}
		if !h.Matches(a.Message) {
			return failedOut("anchor-ots", fmt.Errorf("%s: the tree reaches the merkle root %x at height %d and the header you gave for that height has the merkle root %x: a different header, or the evidence is of another object", id, a.Message[:8], a.Height, h.MerkleRoot[:8]))
		}
		if best == nil || h.Time.Before(best.Time) {
			best, bestH = h, a.Height
		}
	}
	if best == nil {
		st.unver = append(st.unver, fmt.Sprintf("ots anchor %s: the commitment tree is valid and reaches a Bitcoin attestation at height(s) %s, and no --btc-header was given for it: not checked against Bitcoin (the header the cluster may have kept in the evidence was not used)", id, strings.Join(unchecked, ", ")))
		return outcome{}
	}
	r.ok("anchor ots "+id, fmt.Sprintf("the operation tree from the digest reaches the merkle root %x of the header you gave for block %d, which meets its own proof-of-work target; the digest existed no later than %s (the miner's time field, an error of the order of hours)", best.MerkleRoot[:8], bestH, best.Time.Format(time.RFC3339)))
	r.notes = appendNew(r.notes, []string{"ots: that the header is on the best chain is not shown (a work check only); you obtained it yourself"})
	if len(unchecked) > 0 {
		r.notes = append(r.notes, fmt.Sprintf("ots anchor %s: attestation(s) at height(s) %s had no header and were not checked", id, strings.Join(unchecked, ", ")))
	}
	st.verified[anchorev.KindOTS]++
	return outcome{}
}
