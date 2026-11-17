// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// pendingRef is a pending attestation of a fragment, with the node it sits
// on and the operations from the root of the fragment to that node.
type pendingRef struct {
	node *Node
	att  Attestation
	path []Op
}

func pendingOf(root *Node) []pendingRef {
	var out []pendingRef
	var walk func(n *Node, path []Op)
	walk = func(n *Node, path []Op) {
		n.sort()
		for _, a := range n.Attestations {
			if a.Kind == AttPending {
				out = append(out, pendingRef{node: n, att: a, path: append([]Op(nil), path...)})
			}
		}
		for _, e := range n.Edges {
			walk(e.Next, append(path, e.Op))
		}
	}
	walk(root, nil)
	return out
}

func removePending(n *Node, a Attestation) {
	out := n.Attestations[:0]
	for _, x := range n.Attestations {
		if compareAtt(x, a) != 0 {
			out = append(out, x)
		}
	}
	n.Attestations = out
}

type look struct {
	owner  int
	ref    pendingRef
	base   string
	got    *Node
	found  bool
	err    error
	prefix []Op
}

// bestClass is the class of the failure that holds the most hope: retryable
// before refused before permanent.
func bestClass(errs []error) anchor.Class {
	c := anchor.ClassPermanent
	for _, e := range errs {
		switch anchor.ClassOf(e) {
		case anchor.ClassRetryable:
			return anchor.ClassRetryable
		case anchor.ClassRefused:
			c = anchor.ClassRefused
		}
	}
	return c
}

// allFailed is the error of a round in which every lookup failed and nothing
// changed. Its class is the most hopeful one among the failures, and a round
// whose failures were all permanent (a gateway that answered once with HTML,
// a 410, a 400 during a deployment) is retryable all the same: the fragments
// are our own, kept and valid, and another poll costs one request per
// calendar. The record ends by its expiry (or the limits of the retries), not
// by one bad round. Only a fragment that cannot be read from the store is
// permanent (see Upgrade).
func allFailed(failures []error) *anchor.Error {
	class := bestClass(failures)
	if class == anchor.ClassPermanent {
		class = anchor.ClassRetryable
	}
	last := failures[len(failures)-1]
	e := &anchor.Error{Class: class, Target: anchor.KindOTS, Op: "upgrade", Code: "upgrade_failed",
		Text: "no calendar could be asked: " + last.Error(), Err: last}
	e.RetryAfter = anchor.RetryAfterOf(last)
	return e
}

// headerCache is what one Upgrade call has asked of the header source.
type headerCache struct {
	asked  int
	hdrs   map[uint64][]byte
	failed map[uint64]bool
}

// headerPass checks every claim that passed the header-free checks against
// the header of its height, asking the source at most MaxHeaderChecks times in
// all (the cache is kept across passes). It returns true when some claim could
// not be checked (no source answer, too many heights, a header that does not
// pass the work check, or a block time ahead of the clock): those claims stay
// as they were, to be checked again; they are not contradicted.
func (t *Target) headerPass(ctx context.Context, claims []ClaimCheck, submitted, now time.Time, hc *headerCache) (unavailable bool) {
	for i := range claims {
		c := &claims[i]
		if c.Problem != "" {
			continue
		}
		if _, done := hc.hdrs[c.Height]; !done && !hc.failed[c.Height] {
			if hc.asked >= MaxHeaderChecks {
				unavailable = true
				continue
			}
			hc.asked++
			cctx, cancel := context.WithTimeout(ctx, CallTimeout)
			h, herr := t.headers.Header(cctx, c.Height)
			cancel()
			if herr != nil || len(h) != HeaderSize {
				hc.failed[c.Height] = true
			} else {
				hc.hdrs[c.Height] = h
			}
		}
		h, ok := hc.hdrs[c.Height]
		if !ok {
			unavailable = true
			continue
		}
		applyHeader(c, h, submitted, now, t.work)
		if c.HeaderTransient {
			// the fault is the source's or the clock's, not the claim's
			c.HeaderProblem, c.Header, c.HeaderTime = "", nil, time.Time{}
			unavailable = true
		}
	}
	return unavailable
}

// dropClaims returns the fragments without the Bitcoin attestations that the
// claims name (by calendar, height and the message they sit on), and without
// the fragments that nothing is left of. The pending attestation that the
// claim replaced is gone: that calendar has said what it had to say.
func dropClaims(roots []*Node, uris []string, claims []ClaimCheck) ([]*Node, []string) {
	var outRoots []*Node
	var outURIs []string
	for i, root := range roots {
		kept := dropLeaves(root, func(_ []Op, msg []byte, a Attestation) bool {
			if a.Kind != AttBitcoin {
				return false
			}
			for _, c := range claims {
				if c.CalendarURI == uris[i] && c.Height == a.Height && bytes.Equal(c.Root[:], msg) {
					return true
				}
			}
			return false
		})
		if kept != nil {
			outRoots, outURIs = append(outRoots, kept), append(outURIs, uris[i])
		}
	}
	return outRoots, outURIs
}

// Upgrade implements anchor.Target.
//
// For a "submitted" or a "claimed" receipt it asks the calendars named by the
// pending attestations of the stored fragments (only calendars of the
// configuration; one outside it is never asked) for the Bitcoin attestation.
// A claimed receipt goes on asking while a fragment still holds a pending
// attestation: a claim is a calendar's word, and another calendar may deliver
// the proof that a header can check. Every fragment has its own budget of
// PerFragmentLookups requests per call, so every calendar is asked in every
// round and a calendar that fills its fragment with pending attestations
// spends only its own share. The calendars are asked in parallel. One
// calendar's failure, malformed or oversized answer, or implausible claim is
// that calendar's failure: its answer is dropped and the others are kept. A
// 404 means "not yet" and is not an error. An error is returned when every
// lookup failed and nothing changed; its class is the most hopeful one among
// the failures, and a round of permanent failures is retryable (see
// allFailed); only a stored fragment that cannot be read is permanent.
//
// What a Bitcoin attestation makes of the receipt (see claims.go): a claim
// that passed the checks that need no header makes it "claimed"; a claim that
// matched the header of the configured HeaderSource makes it "anchored" and
// EvidenceTime the header's time. A claim that a header contradicts is dropped
// from the fragments like an implausible one (a header that is unusable, or a
// block time ahead of the clock, contradicts nothing). When no plausible claim
// is left the receipt is "submitted" again (and says so: the record falls
// back to submitted) as long as a calendar can still deliver; when nothing
// can, and a header contradicted a claim, the error is of class conflict
// (code "claim_contradicted": a calendar lied) and the receipt is unchanged.
// A "claimed" receipt with nothing pending and no HeaderSource is returned as
// it is. An "anchored" receipt is returned as it is.
func (t *Target) Upgrade(ctx context.Context, r anchor.Receipt) (anchor.Receipt, error) {
	if r.State == anchor.ReceiptAnchored {
		return r, nil
	}
	if r.State != anchor.ReceiptSubmitted && r.State != anchor.ReceiptClaimed {
		return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "state", "the receipt is neither submitted, claimed nor anchored")
	}
	ev, err := anchor.ParseOTSEvidence(r.Evidence)
	if err != nil {
		return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "the evidence is not readable")
	}
	_, roots, uris, submitted, err := parseEvidenceTrees(ev)
	if err != nil {
		return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "a stored fragment is not readable")
	}
	if r.State == anchor.ReceiptClaimed && t.headers == nil && !t.hasLookups(roots) {
		return r, nil
	}
	now := t.now()

	changed := false
	var lookupErr *anchor.Error
	if t.hasLookups(roots) {
		var ok int
		var failures []error
		changed, ok, failures = t.lookups(ctx, roots, uris, submitted, now)
		if len(failures) > 0 && ok == 0 && !changed {
			lookupErr = allFailed(failures)
			if r.State == anchor.ReceiptSubmitted {
				return r, lookupErr
			}
		}
	}

	// the claims of the (possibly grown) fragments, and the headers; a claim
	// that a header contradicts is dropped, and the rest is judged again
	hc := &headerCache{hdrs: map[uint64][]byte{}, failed: map[uint64]bool{}}
	var check *EvidenceCheck
	unavailable, dropped := false, false
	var contradiction string
	for {
		check = &EvidenceCheck{Claims: checkFragments(roots, uris, submitted, now)}
		unavailable = false
		if t.headers != nil {
			unavailable = t.headerPass(ctx, check.Claims, submitted, now, hc)
		}
		var bad []ClaimCheck
		for _, c := range check.Claims {
			if c.HeaderProblem != "" {
				bad = append(bad, c)
			}
		}
		if len(bad) == 0 {
			break
		}
		if contradiction == "" {
			contradiction = fmt.Sprintf("the block header at height %d contradicts the claim of a calendar: %s", bad[0].Height, bad[0].HeaderProblem)
		}
		roots, uris = dropClaims(roots, uris, bad)
		dropped, changed = true, true
	}
	check.summarise()
	pendingLeft := t.hasLookups(roots)
	next := now.Add(FirstCheck)

	if check.Status == "" {
		if dropped && !pendingLeft {
			return r, anchor.Errorf(anchor.ClassConflict, anchor.KindOTS, "upgrade", "claim_contradicted", "%s", contradiction)
		}
		if !changed && r.State == anchor.ReceiptSubmitted {
			out := r
			if out.NextCheck == nil {
				out.NextCheck = &next
			}
			return out, nil
		}
		// a claimed receipt with no plausible claim left is submitted again
		raw, err := t.encode(ev, roots, uris, nil)
		if err != nil {
			return r, err
		}
		return anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw, NextCheck: &next}, nil
	}

	best := check.Claims[check.Best]
	sum := &anchor.OTSBitcoin{Height: best.Height, CalendarURI: best.CalendarURI,
		MerkleRoot: hex.EncodeToString(best.Root[:]), Status: check.Status}
	if check.Status == anchor.BitcoinHeaderChecked {
		sum.Header = hex.EncodeToString(best.Header)
	}
	raw, err := t.encode(ev, roots, uris, sum)
	if err != nil {
		return r, err
	}
	if check.Status == anchor.BitcoinHeaderChecked {
		et := best.HeaderTime.UTC()
		return anchor.Receipt{State: anchor.ReceiptAnchored, Evidence: raw, EvidenceTime: &et}, nil
	}
	if r.State == anchor.ReceiptClaimed && lookupErr != nil && !changed {
		// nothing could be asked and nothing changed: the claim stands, the
		// failure is reported so that the record waits and tries again
		return r, lookupErr
	}
	rc := anchor.Receipt{State: anchor.ReceiptClaimed, Evidence: raw}
	if (t.headers != nil && unavailable) || pendingLeft {
		rc.NextCheck = &next
	}
	return rc, nil
}

// encode serialises the fragments and the summary into evidence.
func (t *Target) encode(ev anchor.OTSEvidence, roots []*Node, uris []string, sum *anchor.OTSBitcoin) (json.RawMessage, error) {
	cals := make([]anchor.OTSCalendar, 0, len(roots))
	for i, root := range roots {
		b, err := root.Serialize()
		if err != nil || len(b) > t.fragLimit {
			return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "fragment", "the upgraded fragment is empty or larger than %d bytes", MaxFragment)
		}
		cals = append(cals, anchor.OTSCalendar{URI: uris[i], FragmentB64: base64.StdEncoding.EncodeToString(b)})
	}
	ev.Calendars = cals
	ev.Bitcoin = sum
	raw, err := anchor.EncodeEvidence(ev)
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "cannot encode the evidence")
	}
	return raw, nil
}

// planLookups chooses the requests of one round: a budget of PerFragmentLookups
// distinct commitments for every fragment, in the order of the fragments,
// among the pending attestations of calendars of the configuration.
func (t *Target) planLookups(roots []*Node) []*look {
	allowed := map[string]bool{}
	for _, c := range t.cfg.Calendars {
		allowed[c] = true
	}
	var looks []*look
	seen := map[string]bool{}
	for i, root := range roots {
		n := 0
		for _, ref := range pendingOf(root) {
			if n >= PerFragmentLookups {
				break
			}
			base, err := NormalizeCalendarURL(ref.att.URI, t.cfg.AllowInsecure)
			if err != nil || !allowed[base] {
				continue
			}
			key := base + "|" + hex.EncodeToString(ref.node.Msg)
			if seen[key] {
				continue
			}
			seen[key] = true
			n++
			looks = append(looks, &look{owner: i, ref: ref, base: base, prefix: ref.path})
		}
	}
	return looks
}

// hasLookups reports whether some fragment holds a pending attestation of a
// calendar of the configuration, that is, whether a calendar can still deliver.
func (t *Target) hasLookups(roots []*Node) bool { return len(t.planLookups(roots)) > 0 }

// lookups asks the calendars for the pending attestations of the fragments
// and merges what is acceptable into the trees (in place). It returns
// whether any tree changed, how many lookups got an answer (404 included),
// and the failures.
func (t *Target) lookups(ctx context.Context, roots []*Node, uris []string, submitted, now time.Time) (changed bool, ok int, failures []error) {
	looks := t.planLookups(roots)
	var wg sync.WaitGroup
	sem := make(chan struct{}, MaxParallel)
	for _, l := range looks {
		wg.Add(1)
		sem <- struct{}{}
		go func(l *look) {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, CallTimeout)
			defer cancel()
			l.got, l.found, l.err = t.cal.GetTimestamp(cctx, l.base, l.ref.node.Msg)
		}(l)
	}
	wg.Wait()

	lo, hi := HeightBounds(submitted, now)
	badOwner := make([]bool, len(roots))
	for _, l := range looks {
		if badOwner[l.owner] {
			continue
		}
		if l.err != nil {
			failures = append(failures, l.err)
			continue
		}
		ok++
		if !l.found {
			continue
		}
		prefix := l.prefix
		pruned := dropLeaves(l.got, func(path []Op, msg []byte, a Attestation) bool {
			if a.Kind != AttBitcoin {
				return false
			}
			full := append(append([]Op(nil), prefix...), path...)
			_, _, problem := checkLeaf(leafPath{msg: msg, height: a.Height, ops: full}, lo, hi)
			return problem != ""
		})
		if pruned == nil || !hasBitcoin(pruned) {
			if hasBitcoin(l.got) {
				failures = append(failures, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "implausible_claim",
					"calendar %s claimed a Bitcoin attestation that fails the checks that need no header", hostOf(l.base)))
			}
			continue
		}
		before := roots[l.owner].Clone()
		if err := l.ref.node.Merge(pruned); err != nil {
			roots[l.owner], badOwner[l.owner] = before, true
			failures = append(failures, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "merge", "cannot merge the answer of calendar %s: %s", hostOf(l.base), shortErr(err)))
			continue
		}
		removePending(l.ref.node, l.ref.att)
		if b, err := roots[l.owner].Serialize(); err != nil || len(b) > t.fragLimit {
			roots[l.owner], badOwner[l.owner] = before, true
			failures = append(failures, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "fragment",
				"the fragment of calendar %s would be larger than %d bytes: its answer is dropped", hostOf(l.base), MaxFragment))
			continue
		}
		changed = true
	}
	return changed, ok, failures
}

var errNoCalendars = errors.New("ots: evidence holds no fragment")

// BuildFile merges every calendar fragment of ev into one tree and returns
// the detached timestamp file for the digest (file hash operation SHA-256).
func BuildFile(ev anchor.OTSEvidence) (*File, error) {
	_, roots, _, _, err := parseEvidenceTrees(ev)
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, errNoCalendars
	}
	root := roots[0]
	for _, n := range roots[1:] {
		if err := root.Merge(n); err != nil {
			return nil, fmt.Errorf("ots: %w", err)
		}
	}
	return &File{HashOp: TagSHA256, Root: root}, nil
}
