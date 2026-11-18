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
	"strconv"
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

// answered reports whether the node of a pending attestation already holds the
// calendar's answer (a subtree below it, or an attestation beside it): the
// pending attestation is then only kept, for a header source that may contradict
// the claim, now or in the future (correction 96).
func answered(n *Node) bool {
	if len(n.Edges) > 0 {
		return true
	}
	for _, a := range n.Attestations {
		if a.Kind != AttPending {
			return true
		}
	}
	return false
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
// claims name (by calendar, height and the message they sit on), without the
// pending attestations that sit on the way to them (the one that a target with a
// header source kept when it merged the answer: that calendar has said what it
// had to say, and is not asked for the same claim again), and without the
// fragments that nothing is left of.
func dropClaims(roots []*Node, uris []string, claims []ClaimCheck) ([]*Node, []string) {
	var outRoots []*Node
	var outURIs []string
	for i, root := range roots {
		isDropped := func(msg []byte, a Attestation) bool {
			if a.Kind != AttBitcoin {
				return false
			}
			for _, c := range claims {
				if c.CalendarURI == uris[i] && c.Height == a.Height && bytes.Equal(c.Root[:], msg) {
					return true
				}
			}
			return false
		}
		// the messages of the nodes above a dropped claim
		above := map[string]bool{}
		var walk func(n *Node, path []string) bool
		walk = func(n *Node, path []string) bool {
			hit := false
			for _, a := range n.Attestations {
				if isDropped(n.Msg, a) {
					hit = true
				}
			}
			for _, e := range n.Edges {
				if walk(e.Next, append(path, string(n.Msg))) {
					hit = true
				}
			}
			if hit {
				above[string(n.Msg)] = true
			}
			return hit
		}
		walk(root, nil)
		kept := dropLeaves(root, func(_ []Op, msg []byte, a Attestation) bool {
			if a.Kind == AttPending && above[string(msg)] {
				return true
			}
			return isDropped(msg, a)
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
// allFailed); only a stored fragment that cannot be read is permanent. The
// whole call is bounded by Options.UpgradeTimeout (default
// DefaultUpgradeTimeout) besides the caller's context.
//
// What a Bitcoin attestation makes of the receipt (see claims.go): a claim
// that passed the checks that need no header makes it "claimed"; a claim that
// matched the header of the configured HeaderSource makes it "anchored" and
// EvidenceTime the header's time.
//
// A claim that a header contradicts is NOT dropped on that one answer
// (correction 90): the source may have been wrong once (a header of an
// orphaned block, a node that is still synchronising, another network). The
// claim is kept aside in the evidence (OTSEvidence.Contradicted, with the
// header that contradicted it and the time), stays in the fragment of its
// calendar, is neither plausible nor anchored, and the calendar is asked about
// its commitment again in later rounds. It is dropped for good only when the
// header source contradicts it again in a round at least the confirmation
// interval (Config.ContradictionConfirmMinutes, default 60) after the first
// contradiction; a header that checks it in a later round puts it back among
// the claims. To be able to ask again, a target with a header source keeps the
// pending attestation of a calendar whose answer it merged (the target without
// one removes it, as nothing can contradict a claim then); dropping a claim for
// good removes that pending attestation too, so that the calendar is not asked
// for the same claim again. A header that is unusable (it fails the work check, with its
// floor) or a block time ahead of the clock contradicts nothing. The receipt
// carries the counts (ContradictedClaims, ClaimsAside, ClaimsDropped) so that
// the caller can alert: a calendar that lied and a source that is wrong are
// both operator matters. A hostile calendar still cannot keep a record alive
// beyond its expiry (the record's own timeout counts from its creation), and
// what is kept aside is bounded per calendar (anchor.MaxOTSContradictionsPerCalendar
// entries each; a calendar over its share loses its own surplus claims, never
// another calendar's claim, and an answer with more than MaxAnswerClaims Bitcoin
// attestations is refused as a whole; correction 106).
//
// When no plausible claim is left the receipt is "submitted" again (and says
// so: the record falls back to submitted) as long as a calendar can still
// deliver or a claim is kept aside; when claims were dropped for good and
// nothing can deliver and nothing is kept aside, the error is of class conflict
// (code "claim_contradicted": a calendar lied) and the receipt is unchanged.
// A "claimed" receipt with nothing pending, nothing kept aside and no
// HeaderSource is returned as it is. An "anchored" receipt is returned as it is.
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
	aside := append([]anchor.OTSContradiction(nil), ev.Contradicted...)
	remembered := append([]anchor.OTSDropped(nil), ev.Dropped...)
	repeated := 0
	if r.State == anchor.ReceiptClaimed && t.headers == nil && !t.hasLookups(roots) {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, t.upgradeTimeout)
	defer cancel()
	now := t.now()

	changed := false
	var lookupErr *anchor.Error
	if t.hasLookups(roots) {
		var ok int
		var failures []error
		changed, ok, failures, repeated = t.lookups(ctx, roots, uris, submitted, now, remembered)
		if len(failures) > 0 && ok == 0 && !changed {
			lookupErr = allFailed(failures)
			if r.State == anchor.ReceiptSubmitted {
				return r, lookupErr
			}
		}
	}

	// the claims of the (possibly grown) fragments, and the headers. A claim
	// that a header contradicts is kept aside, not dropped; one that was kept
	// aside and is contradicted again after the confirmation interval is
	// dropped, and the rest is judged again
	hc := &headerCache{hdrs: map[uint64][]byte{}, failed: map[uint64]bool{}}
	var check *EvidenceCheck
	unavailable := false
	contradicted, dropped := 0, 0
	counted := map[string]bool{}
	var contradiction string
	for {
		check = &EvidenceCheck{Claims: checkFragments(roots, uris, submitted, now, aside)}
		unavailable = false
		if t.headers != nil {
			unavailable = t.headerPass(ctx, check.Claims, submitted, now, hc)
		}
		var drop []ClaimCheck
		modified := false
		for _, c := range check.Claims {
			idx := findAside(aside, c.CalendarURI, c.Height, c.Root)
			switch {
			case c.HeaderChecked && idx >= 0:
				// the header source is right now: the claim stands again
				aside = removeAside(aside, idx)
				modified, changed = true, true
			case c.HeaderProblem != "":
				if k := asideKey(c.CalendarURI, c.Height, c.Root); !counted[k] {
					counted[k] = true
					contradicted++
				}
				if contradiction == "" {
					contradiction = fmt.Sprintf("the block header at height %d contradicts the claim of a calendar: %s", c.Height, c.HeaderProblem)
				}
				switch {
				case idx >= 0:
					if at, err := anchor.ParseTime(aside[idx].At); err != nil || !now.Before(at.Add(t.confirm)) {
						drop = append(drop, c)
					}
				case asideOf(aside, c.CalendarURI) >= anchor.MaxOTSContradictionsPerCalendar:
					// the calendar is over ITS share: it loses its own surplus claim; a
					// claim of another calendar is never dropped because this one fills
					// its share (correction 106)
					drop = append(drop, c)
				default:
					aside = append(aside, anchor.OTSContradiction{
						CalendarURI: c.CalendarURI, Height: c.Height,
						MerkleRoot: hex.EncodeToString(c.Root[:]), Header: hex.EncodeToString(c.Header),
						At: anchor.FormatTime(now), Reason: c.HeaderProblem,
					})
					modified, changed = true, true
				}
			}
		}
		if len(drop) > 0 {
			roots, uris = dropClaims(roots, uris, drop)
			for _, c := range drop {
				if i := findAside(aside, c.CalendarURI, c.Height, c.Root); i >= 0 {
					aside = removeAside(aside, i)
				}
			}
			for _, c := range drop {
				remembered = rememberDropped(remembered, c, now)
			}
			dropped += len(drop)
			changed = true
			continue
		}
		if !modified {
			break
		}
	}
	check.summarise()
	pendingLeft := t.hasLookups(roots)
	next := now.Add(FirstCheck)
	counts := func(rc anchor.Receipt) anchor.Receipt {
		rc.ContradictedClaims, rc.ClaimsAside, rc.ClaimsDropped, rc.ClaimsRepeated = contradicted, len(aside), dropped, repeated
		return rc
	}

	if check.Status == "" {
		if dropped > 0 && !pendingLeft && len(aside) == 0 {
			return r, &ContradictedError{Evidence: ev, Err: anchor.Errorf(anchor.ClassConflict, anchor.KindOTS, "upgrade", "claim_contradicted",
				"%s (%d claims dropped for good after the header source contradicted them again at least %s later): the evidence before this call still holds the claims, and the error carries it (PriorEvidence): if the header source was wrong, keep it and pass it to SubmitWithPrior on Retry; a Submit without it starts again from the calendars",
				contradiction, dropped, t.confirm)}
		}
		if !changed && r.State == anchor.ReceiptSubmitted {
			out := r
			if out.NextCheck == nil {
				out.NextCheck = &next
			}
			return counts(out), nil
		}
		// a claimed receipt with no plausible claim left is submitted again
		raw, err := t.encode(ev, roots, uris, nil, aside, remembered)
		if err != nil {
			return r, err
		}
		return counts(anchor.Receipt{State: anchor.ReceiptSubmitted, Evidence: raw, NextCheck: &next}), nil
	}

	best := check.Claims[check.Best]
	sum := &anchor.OTSBitcoin{Height: best.Height, CalendarURI: best.CalendarURI,
		MerkleRoot: hex.EncodeToString(best.Root[:]), Status: check.Status}
	if check.Status == anchor.BitcoinHeaderChecked {
		sum.Header = hex.EncodeToString(best.Header)
	}
	raw, err := t.encode(ev, roots, uris, sum, aside, remembered)
	if err != nil {
		return r, err
	}
	if check.Status == anchor.BitcoinHeaderChecked {
		et := best.HeaderTime.UTC()
		return counts(anchor.Receipt{State: anchor.ReceiptAnchored, Evidence: raw, EvidenceTime: &et}), nil
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
	return counts(rc), nil
}

// asideOf counts the claims kept aside that belong to a calendar.
func asideOf(aside []anchor.OTSContradiction, uri string) int {
	n := 0
	for _, a := range aside {
		if a.CalendarURI == uri {
			n++
		}
	}
	return n
}

// rememberDropped adds a claim dropped for good to the list of those remembered
// (a claim already in it is not added twice). The memory is bounded per calendar
// (anchor.MaxOTSDroppedPerCalendar: when a calendar's share is full the oldest
// entry of that calendar is forgotten, and no other calendar's memory is touched)
// and in all (anchor.MaxOTSDropped: the oldest of all is forgotten), correction
// 106: one calendar's answers cannot evict what is remembered of another, and
// one answer (at most MaxAnswerClaims claims) cannot evict what is remembered of
// its own calendar while the share has room for it.
func rememberDropped(list []anchor.OTSDropped, c ClaimCheck, now time.Time) []anchor.OTSDropped {
	if isRemembered(list, c.CalendarURI, c.Height, c.Root[:]) {
		return list
	}
	list = append(list, anchor.OTSDropped{CalendarURI: c.CalendarURI, Height: c.Height,
		MerkleRoot: hex.EncodeToString(c.Root[:]), At: anchor.FormatTime(now)})
	own := 0
	for _, d := range list {
		if d.CalendarURI == c.CalendarURI {
			own++
		}
	}
	if own > anchor.MaxOTSDroppedPerCalendar {
		// the new entry is the last: the first entry of the same calendar is its oldest
		for i, d := range list {
			if d.CalendarURI == c.CalendarURI {
				list = append(append([]anchor.OTSDropped(nil), list[:i]...), list[i+1:]...)
				break
			}
		}
	}
	if n := len(list) - anchor.MaxOTSDropped; n > 0 {
		list = append([]anchor.OTSDropped(nil), list[n:]...)
	}
	return list
}

func isRemembered(list []anchor.OTSDropped, uri string, height uint64, msg []byte) bool {
	h := hex.EncodeToString(msg)
	for _, d := range list {
		if d.CalendarURI == uri && d.Height == height && d.MerkleRoot == h {
			return true
		}
	}
	return false
}

// asideKey names a claim for the counts of one call.
func asideKey(uri string, height uint64, root [32]byte) string {
	return uri + "|" + strconv.FormatUint(height, 10) + "|" + hex.EncodeToString(root[:])
}

func removeAside(aside []anchor.OTSContradiction, i int) []anchor.OTSContradiction {
	out := append([]anchor.OTSContradiction(nil), aside[:i]...)
	return append(out, aside[i+1:]...)
}

// encode serialises the fragments and the summary into evidence.
func (t *Target) encode(ev anchor.OTSEvidence, roots []*Node, uris []string, sum *anchor.OTSBitcoin, aside []anchor.OTSContradiction, remembered []anchor.OTSDropped) (json.RawMessage, error) {
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
	ev.Contradicted = append([]anchor.OTSContradiction{}, aside...)
	ev.Dropped = append([]anchor.OTSDropped{}, remembered...)
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
			if t.headers == nil && answered(ref.node) {
				// nothing can contradict the claim without a source, and the
				// calendar's answer is merged: no request is made (the pending
				// attestation is kept for a source configured later)
				continue
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
func (t *Target) lookups(ctx context.Context, roots []*Node, uris []string, submitted, now time.Time, remembered []anchor.OTSDropped) (changed bool, ok int, failures []error, repeated int) {
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
		// an answer that carries more Bitcoin attestations than MaxAnswerClaims is
		// refused as a whole, as an implausible answer of that calendar (correction
		// 106): an honest answer holds a few, and 10,000 bytes hold about 400 false
		// ones, with which one calendar could fill the claims kept aside
		if n := len(BitcoinClaims(l.got)); n > MaxAnswerClaims {
			failures = append(failures, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "implausible_answer",
				"calendar %s answered with %d Bitcoin attestations, more than the %d an answer may carry: the whole answer is refused", hostOf(l.base), n, MaxAnswerClaims))
			continue
		}
		// a claim that was dropped for good and is answered again is put aside at
		// once, before it can enter the fragment, and is not counted as a new one
		// (correction 96)
		if len(remembered) > 0 {
			l.got = dropLeaves(l.got, func(_ []Op, msg []byte, a Attestation) bool {
				if a.Kind == AttBitcoin && isRemembered(remembered, uris[l.owner], a.Height, msg) {
					repeated++
					return true
				}
				return false
			})
			if l.got == nil {
				continue
			}
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
		beforeBytes, _ := before.Serialize()
		if err := l.ref.node.Merge(pruned); err != nil {
			roots[l.owner], badOwner[l.owner] = before, true
			failures = append(failures, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "merge", "cannot merge the answer of calendar %s: %s", hostOf(l.base), shortErr(err)))
			continue
		}
		// the pending attestation stays in every configuration (correction 96): a
		// header source may be configured later, or may contradict the claim, and
		// the calendar must then be asked again (see Upgrade); it goes only when
		// the claim above it is dropped for good
		b, err := roots[l.owner].Serialize()
		if err != nil || len(b) > t.fragLimit {
			roots[l.owner], badOwner[l.owner] = before, true
			failures = append(failures, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "fragment",
				"the fragment of calendar %s would be larger than %d bytes: its answer is dropped", hostOf(l.base), MaxFragment))
			continue
		}
		if !bytes.Equal(b, beforeBytes) {
			changed = true
		}
	}
	return changed, ok, failures, repeated
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
