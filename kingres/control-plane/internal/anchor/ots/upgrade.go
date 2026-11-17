// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
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

// Upgrade implements anchor.Target.
//
// For a "submitted" receipt it asks the calendars named by the pending
// attestations of the stored fragments (only calendars of the configuration;
// one outside it is never asked) for the Bitcoin attestation. Every fragment
// has its own budget of PerFragmentLookups requests per call, so every
// calendar is asked in every round and a calendar that fills its fragment with
// pending attestations spends only its own share. The calendars are asked in
// parallel. One calendar's failure, malformed or oversized answer, or
// implausible claim is that calendar's failure: its answer is dropped and the
// others are kept. A 404 means "not yet" and is not an error. An error is
// returned only when every lookup failed and nothing changed; its class is
// the most hopeful one among the failures.
//
// What a Bitcoin attestation makes of the receipt (see claims.go): a claim
// that passed the checks that need no header makes it "claimed"; a claim that
// matched the header of the configured HeaderSource makes it "anchored" and
// EvidenceTime the header's time. A "claimed" receipt is checked again, with
// no calendar request, when a HeaderSource is configured; a claim that a
// header contradicts, with no other plausible claim left, is an error of
// class conflict (a calendar lied). An "anchored" receipt is returned as it is.
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
	if r.State == anchor.ReceiptClaimed && t.headers == nil {
		return r, nil
	}
	digest, roots, uris, submitted, err := parseEvidenceTrees(ev)
	if err != nil {
		return r, anchor.Errorf(anchor.ClassPermanent, anchor.KindOTS, "upgrade", "evidence", "a stored fragment is not readable")
	}
	now := t.now()
	_ = digest

	changed := false
	var lastErr error
	if r.State == anchor.ReceiptSubmitted {
		var failures []error
		ok := 0
		changed, ok, failures = t.lookups(ctx, roots, uris, submitted, now)
		if len(failures) > 0 && ok == 0 && !changed {
			last := failures[len(failures)-1]
			e := &anchor.Error{Class: bestClass(failures), Target: anchor.KindOTS, Op: "upgrade", Code: "upgrade_failed",
				Text: "no calendar could be asked: " + last.Error(), Err: last}
			e.RetryAfter = anchor.RetryAfterOf(last)
			return r, e
		}
		if len(failures) > 0 {
			lastErr = failures[len(failures)-1]
		}
	}
	_ = lastErr

	// the claims of the (possibly grown) fragments, and the headers
	claims := checkFragments(roots, uris, submitted, now)
	check := &EvidenceCheck{Claims: claims}
	unavailable := false
	if t.headers != nil {
		asked := 0
		hdrs := map[uint64][]byte{}
		failed := map[uint64]bool{}
		for i := range check.Claims {
			c := &check.Claims[i]
			if c.Problem != "" {
				continue
			}
			if _, done := hdrs[c.Height]; !done && !failed[c.Height] {
				if asked >= MaxHeaderChecks {
					unavailable = true
					continue
				}
				asked++
				cctx, cancel := context.WithTimeout(ctx, CallTimeout)
				h, herr := t.headers.Header(cctx, c.Height)
				cancel()
				if herr != nil || len(h) != HeaderSize {
					failed[c.Height] = true
				} else {
					hdrs[c.Height] = h
				}
			}
			if h, ok := hdrs[c.Height]; ok {
				applyHeader(c, h, submitted, now)
			} else {
				unavailable = true
			}
		}
	}
	check.summarise()

	if r.State == anchor.ReceiptClaimed && check.Status == "" {
		for _, c := range check.Claims {
			if c.HeaderProblem != "" {
				return r, anchor.Errorf(anchor.ClassConflict, anchor.KindOTS, "upgrade", "claim_contradicted",
					"the block header at height %d contradicts the claim of a calendar: %s", c.Height, c.HeaderProblem)
			}
		}
		return r, nil
	}

	next := now.Add(FirstCheck)
	if check.Status == "" {
		if !changed {
			out := r
			out.State = anchor.ReceiptSubmitted
			if out.NextCheck == nil {
				out.NextCheck = &next
			}
			return out, nil
		}
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
	rc := anchor.Receipt{State: anchor.ReceiptClaimed, Evidence: raw}
	if t.headers != nil && unavailable {
		rc.NextCheck = &next
	}
	return rc, nil
}

// encode serialises the fragments and the summary into evidence.
func (t *Target) encode(ev anchor.OTSEvidence, roots []*Node, uris []string, sum *anchor.OTSBitcoin) (json.RawMessage, error) {
	cals := make([]anchor.OTSCalendar, 0, len(roots))
	for i, root := range roots {
		b, err := root.Serialize()
		if err != nil || len(b) > fragmentLimit {
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

// lookups asks the calendars for the pending attestations of the fragments
// and merges what is acceptable into the trees (in place). It returns
// whether any tree changed, how many lookups got an answer (404 included),
// and the failures.
func (t *Target) lookups(ctx context.Context, roots []*Node, uris []string, submitted, now time.Time) (changed bool, ok int, failures []error) {
	allowed := map[string]bool{}
	for _, c := range t.cfg.Calendars {
		allowed[c] = true
	}
	// fair choice: a budget of PerFragmentLookups distinct commitments for
	// every fragment, in the order of the fragments
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
		if b, err := roots[l.owner].Serialize(); err != nil || len(b) > fragmentLimit {
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

// fragmentLimit is MaxFragment; a test lowers it to reach the case of a
// fragment that grows past it, which the sizes of the other limits (a calendar
// answer is at most 10,000 bytes, four answers per fragment per call) keep a
// real call from reaching.
var fragmentLimit = MaxFragment
