// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// What "anchored" means for an OpenTimestamps record (correction 70):
//
//   - kingresd holds no block headers, so it cannot verify a Bitcoin
//     attestation by itself. A calendar's Bitcoin attestation is a CLAIM.
//   - A claim is accepted as a claim (receipt state "claimed", evidence
//     status "claimed") only if every check that needs no header passes:
//     the message it sits on is 32 bytes; the operations from the digest to
//     it are recomputed from the digest and end in two SHA-256 operations (a
//     Bitcoin merkle node is a double SHA-256); the height is above the
//     highest known block that was mined before the digest was made and not
//     above what the chain can have reached by now; and no other claim at
//     the same height names another merkle root.
//   - A receipt is "anchored" only when a header source configured by the
//     operator returned the 80-byte header of the block at that height and
//     the claim matched it: merkle root, proof of work, and a block time
//     that fits the time of submission and the clock. The header is stored
//     in the evidence.

// A checkpoint is a block of the main chain whose height and time field are
// facts of the public chain (the headers in testdata/headers.txt, which a
// test compares with this table). They give the bounds on the height of an
// attestation without any network access.
type checkpoint struct {
	height uint64
	time   int64 // the header's time field, Unix seconds
}

var checkpoints = []checkpoint{
	{0, 1231006505},
	{129405, 1307549734},
	{358391, 1432827678},
	{465751, 1494420461},
	{466906, 1495071945},
	{912095, 1756386670},
}

// Bounds of the plausibility checks.
const (
	// BlockTimeSlack is how much earlier than the submission a block's time
	// field may be and still attest it: a miner's time field is only
	// constrained by the median of the last 11 blocks (up to about an hour
	// behind) and by two hours ahead of the network's time; 3 hours covers
	// both.
	BlockTimeSlack = 3 * time.Hour
	// FutureSlack is how far ahead of the clock a block's time field may be.
	FutureSlack = 2 * time.Hour
	// minBlockInterval is the fastest average block rate the height bound
	// allows (the main chain targets 600 s; 120 s is five times faster than
	// the difficulty adjustment allows to last).
	minBlockInterval = 120 * time.Second
	// heightMargin is added to the upper bound (one difficulty period).
	heightMargin = 2016
	// MinClaimOps is the fewest operations from the digest to a Bitcoin
	// attestation: the last two are the double SHA-256 of a merkle node.
	MinClaimOps = 2
)

// HeightBounds returns the lowest and the highest block height that can
// attest a digest made at submitted, judged at now. A block that was mined
// before the digest existed cannot attest it, so lo is one above the highest
// checkpoint whose time is at least BlockTimeSlack before submitted; a block
// far in the future does not exist yet, so hi is the last checkpoint before
// now plus one block per two minutes since, plus a difficulty period.
func HeightBounds(submitted, now time.Time) (lo, hi uint64) {
	before := submitted.Add(-BlockTimeSlack).Unix()
	for _, c := range checkpoints {
		if c.time <= before {
			lo = c.height + 1
		}
	}
	hi = heightMargin
	nowU := now.Unix()
	for _, c := range checkpoints {
		if c.time <= nowU {
			hi = c.height + heightMargin
			if d := nowU - c.time; d > 0 {
				hi += uint64(d / int64(minBlockInterval/time.Second))
			}
		}
	}
	return lo, hi
}

// ClaimCheck is the result of the checks for one Bitcoin attestation in the
// fragment of one calendar.
type ClaimCheck struct {
	CalendarURI string
	Height      uint64
	// Root is the message the attestation sits on (valid when RootOK).
	Root   [32]byte
	RootOK bool
	// Problem is "" when the claim passed every check that needs no header,
	// else the first check that failed.
	Problem string
	// Conflict is not "" when another claim at the same height names another
	// merkle root. Unless a header settles it, neither claim is plausible.
	Conflict string
	// Header is the 80-byte header the claim was checked against (nil when
	// no header was supplied for its height).
	Header []byte
	// HeaderTime is the time field of Header (zero without a header).
	HeaderTime time.Time
	// HeaderChecked is true when a header was supplied and the claim matched
	// it. HeaderProblem is the reason when a header was supplied and the
	// claim did not match.
	HeaderChecked bool
	HeaderProblem string
}

// Plausible reports whether the claim passed the checks without a header, was
// not contradicted by a header, and is not in conflict with another claim
// unless a header settled it.
func (c ClaimCheck) Plausible() bool {
	return c.Problem == "" && c.HeaderProblem == "" && (c.Conflict == "" || c.HeaderChecked)
}

// HeaderFunc returns the header of the block at a height for a verifier
// that holds some, and false when it holds none for that height.
type HeaderFunc func(height uint64) (header []byte, ok bool)

// bitcoinLeaves walks the tree from its root and returns every Bitcoin
// attestation with the message it sits on and the operations that lead to it
// from the root. The messages are the ones the parser computed from the root
// message; a stored tree has no other value.
func bitcoinLeaves(root *Node) (out []leafPath) {
	var walk func(n *Node, path []Op)
	walk = func(n *Node, path []Op) {
		n.sort()
		for _, a := range n.Attestations {
			if a.Kind == AttBitcoin {
				out = append(out, leafPath{msg: n.Msg, height: a.Height, ops: append([]Op(nil), path...)})
			}
		}
		for _, e := range n.Edges {
			walk(e.Next, append(path, e.Op))
		}
	}
	walk(root, nil)
	return out
}

type leafPath struct {
	msg    []byte
	height uint64
	ops    []Op
}

// checkLeaf runs the checks that need no header on one attestation.
func checkLeaf(l leafPath, lo, hi uint64) (root [32]byte, ok bool, problem string) {
	if len(l.msg) != 32 {
		return root, false, fmt.Sprintf("the attestation sits on a message of %d bytes, a merkle root has 32", len(l.msg))
	}
	copy(root[:], l.msg)
	n := len(l.ops)
	if n < MinClaimOps || l.ops[n-1].Tag != TagSHA256 || l.ops[n-2].Tag != TagSHA256 {
		return root, true, "the operations before the attestation do not end in two SHA-256 operations"
	}
	if l.height < lo {
		return root, true, fmt.Sprintf("height %d is not above the blocks that were mined before the digest was made (%d)", l.height, lo-1)
	}
	if l.height > hi {
		return root, true, fmt.Sprintf("height %d is above what the chain can have reached by now (%d)", l.height, hi)
	}
	return root, true, ""
}

// checkFragments checks every Bitcoin attestation of the given fragments
// (the roots are the parsed trees, uris their calendars) and then marks the
// claims that other calendars contradict (the same height, another merkle
// root: a block has one root, so at least one of them is false and nobody
// here can tell which).
func checkFragments(roots []*Node, uris []string, submitted, now time.Time) []ClaimCheck {
	lo, hi := HeightBounds(submitted, now)
	var out []ClaimCheck
	for i, root := range roots {
		for _, l := range bitcoinLeaves(root) {
			r, rok, problem := checkLeaf(l, lo, hi)
			out = append(out, ClaimCheck{CalendarURI: uris[i], Height: l.height, Root: r, RootOK: rok, Problem: problem})
		}
	}
	byHeight := map[uint64]map[[32]byte]bool{}
	for _, c := range out {
		if c.Problem == "" {
			if byHeight[c.Height] == nil {
				byHeight[c.Height] = map[[32]byte]bool{}
			}
			byHeight[c.Height][c.Root] = true
		}
	}
	for i, c := range out {
		if c.Problem == "" && len(byHeight[c.Height]) > 1 {
			out[i].Conflict = fmt.Sprintf("claims at height %d name different merkle roots (a block has one)", c.Height)
		}
	}
	return out
}

// applyHeader checks one claim against a header: the header's own work
// check, the merkle root, and a block time that fits the submission and the
// clock.
func applyHeader(c *ClaimCheck, header []byte, submitted, now time.Time) {
	c.Header = append([]byte(nil), header...)
	res, err := headerWork(header)
	if err != nil {
		c.HeaderProblem = "the header is not valid: " + shortErr(err)
		return
	}
	c.HeaderTime = res.Time
	if !c.RootOK || !bytes.Equal(header[36:68], c.Root[:]) {
		c.HeaderProblem = "the merkle root of the header is not the message of the attestation"
		return
	}
	if res.Time.Before(submitted.Add(-BlockTimeSlack)) {
		c.HeaderProblem = "the block is older than the digest"
		return
	}
	if res.Time.After(now.Add(FutureSlack)) {
		c.HeaderProblem = "the block time is in the future"
		return
	}
	c.HeaderChecked = true
}

// EvidenceCheck is what CheckEvidence found.
type EvidenceCheck struct {
	// Claims is every Bitcoin attestation of every fragment, with the result
	// of its checks, in the order of the calendars and of the tree.
	Claims []ClaimCheck
	// Status is anchor.BitcoinHeaderChecked when at least one claim passed
	// every check including a header, anchor.BitcoinClaimed when at least one
	// claim passed the checks that need no header, and "" when none did.
	Status string
	// Best is the index in Claims of the claim the evidence summary names:
	// the lowest height among the header-checked claims, or else among the
	// plausible ones; -1 when none.
	Best int
}

// summarise fills Status and Best.
func (e *EvidenceCheck) summarise() {
	e.Best, e.Status = -1, ""
	pick := func(want func(ClaimCheck) bool) int {
		best := -1
		for i, c := range e.Claims {
			if want(c) && (best < 0 || c.Height < e.Claims[best].Height) {
				best = i
			}
		}
		return best
	}
	if i := pick(func(c ClaimCheck) bool { return c.Plausible() && c.HeaderChecked }); i >= 0 {
		e.Best, e.Status = i, anchor.BitcoinHeaderChecked
		return
	}
	if i := pick(func(c ClaimCheck) bool { return c.Plausible() }); i >= 0 {
		e.Best, e.Status = i, anchor.BitcoinClaimed
	}
}

// parseEvidenceTrees parses every fragment of the evidence from its digest.
func parseEvidenceTrees(ev anchor.OTSEvidence) (digest []byte, roots []*Node, uris []string, submitted time.Time, err error) {
	if err = ev.Validate(); err != nil {
		return
	}
	digest, _ = hex.DecodeString(ev.Digest)
	submitted, _ = anchor.ParseTime(ev.SubmittedAt)
	for _, c := range ev.Calendars {
		f, _ := base64.StdEncoding.DecodeString(c.FragmentB64)
		var n *Node
		if n, err = ParseFragment(f, digest); err != nil {
			err = fmt.Errorf("ots: fragment of %s: %w", c.URI, err)
			return
		}
		roots = append(roots, n)
		uris = append(uris, c.URI)
	}
	return
}

// CheckEvidence is the check an independent verifier makes of stored
// evidence, using nothing but the evidence and what it supplies: every
// fragment is parsed and every operation recomputed from the digest (the
// calendar's tree shape is never trusted), every Bitcoin attestation of every
// fragment is checked (see "What anchored means" above), and, for the heights
// for which headers returns a header, the claim is checked against it. now is
// the verifier's clock. It returns an error when the evidence does not parse
// or its "bitcoin" summary does not match the fragments (or, when the summary
// says header_checked, the header it carries).
func CheckEvidence(ev anchor.OTSEvidence, now time.Time, headers HeaderFunc) (*EvidenceCheck, error) {
	_, roots, uris, submitted, err := parseEvidenceTrees(ev)
	if err != nil {
		return nil, err
	}
	res := &EvidenceCheck{Claims: checkFragments(roots, uris, submitted, now)}
	for i := range res.Claims {
		c := &res.Claims[i]
		if c.Problem != "" || headers == nil {
			continue
		}
		if h, ok := headers(c.Height); ok {
			applyHeader(c, h, submitted, now)
		}
	}
	if b := ev.Bitcoin; b != nil {
		found := false
		for i := range res.Claims {
			c := &res.Claims[i]
			if c.CalendarURI != b.CalendarURI || c.Height != b.Height || hex.EncodeToString(c.Root[:]) != b.MerkleRoot || c.Problem != "" {
				continue
			}
			found = true
			if b.Status == anchor.BitcoinHeaderChecked {
				hb, _ := hex.DecodeString(b.Header)
				applyHeader(c, hb, submitted, now)
				if !c.HeaderChecked {
					return nil, fmt.Errorf("ots: the header in the evidence does not check: %s", c.HeaderProblem)
				}
			}
		}
		if !found {
			return nil, errors.New("ots: the bitcoin summary of the evidence matches no plausible attestation of the fragments")
		}
	}
	res.summarise()
	return res, nil
}

// HeaderSource gives the 80-byte header of the block at a height of the best
// chain. It is the operator's: the wiring implements it over a node it trusts.
// An error means "unavailable" (the claim then stays a claim and is checked
// again later); a header that does not match the claim makes the claim false.
type HeaderSource interface {
	Header(ctx context.Context, height uint64) ([]byte, error)
}

// dropLeaves returns a copy of the tree without the attestations for which
// drop is true, and without the branches that are left with nothing; nil
// when nothing is left. drop gets the operations from the root to the node.
func dropLeaves(n *Node, drop func(path []Op, msg []byte, a Attestation) bool) *Node {
	var rec func(n *Node, path []Op) *Node
	rec = func(n *Node, path []Op) *Node {
		out := &Node{Msg: append([]byte(nil), n.Msg...)}
		for _, a := range n.Attestations {
			if !drop(path, n.Msg, a) {
				out.Attestations = append(out.Attestations, a)
			}
		}
		for _, e := range n.Edges {
			if c := rec(e.Next, append(path, e.Op)); c != nil {
				out.Edges = append(out.Edges, Edge{Op: Op{Tag: e.Op.Tag, Arg: append([]byte(nil), e.Op.Arg...)}, Next: c})
			}
		}
		if len(out.Attestations) == 0 && len(out.Edges) == 0 {
			return nil
		}
		return out
	}
	return rec(n, nil)
}

// hasBitcoin reports whether the tree holds a Bitcoin attestation.
func hasBitcoin(n *Node) bool { return len(BitcoinClaims(n)) > 0 }
