// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math/big"
	"time"
)

// HeaderSize is the size of a Bitcoin block header.
const HeaderSize = 80

// powLimitBits is the compact form of the proof-of-work limit of the Bitcoin
// main chain (the easiest target any block may have).
const powLimitBits = 0x1d00ffff

// HeaderResult is what CheckAgainstHeader found.
type HeaderResult struct {
	// Time is the header's time field: the miner-set time, an upper bound
	// on when the committed data existed (with an error of the order of hours).
	Time time.Time
	// Height is the height of the attestation that matched.
	Height uint64
	// Matches is the number of attestations of that height whose message
	// equals the header's merkle root.
	Matches int
	// Hash is the double SHA-256 of the header in internal byte order (the
	// block id is this value reversed).
	Hash [32]byte
	// Bits is the compact target of the header (bytes 72 to 76).
	Bits uint32
	// Work is the expected number of double SHA-256 evaluations that a header
	// of the header's target costs: 2^256 / (target + 1). Difficulty 1 is about
	// 2^32; a block of 2025 is about 2^79. It is what the header proves, as
	// far as it proves anything: that someone spent about that much work on
	// some header (not that the header is in the chain).
	Work *big.Int
}

// MerkleRoot returns the merkle root field of a block header (bytes 36 to 68,
// in the byte order of the header). The reference library compares the
// message of the attestation, as computed by the operations, with this
// field directly (hashMerkleRoot of python-bitcoinlib is the raw field), and
// so does CheckAgainstHeader: no reversal.
func MerkleRoot(header []byte) ([]byte, error) {
	if len(header) != HeaderSize {
		return nil, fmt.Errorf("%w: %d bytes, want %d", ErrBadHeader, len(header), HeaderSize)
	}
	return header[36:68], nil
}

// CompactToTarget decodes the "bits" field of a header. It refuses a negative
// target, a zero target and an overflowing exponent, as Bitcoin Core's
// SetCompact reports them.
func CompactToTarget(bits uint32) (*big.Int, error) {
	exp := int(bits >> 24)
	mant := bits & 0x007fffff
	if mant != 0 && bits&0x00800000 != 0 {
		return nil, fmt.Errorf("%w: negative target", ErrBadHeader)
	}
	if mant != 0 && (exp > 34 || (mant > 0xff && exp > 33) || (mant > 0xffff && exp > 32)) {
		return nil, fmt.Errorf("%w: target overflows", ErrBadHeader)
	}
	t := new(big.Int).SetUint64(uint64(mant))
	if exp <= 3 {
		t.Rsh(t, uint(8*(3-exp)))
	} else {
		t.Lsh(t, uint(8*(exp-3)))
	}
	if t.Sign() == 0 {
		return nil, fmt.Errorf("%w: zero target", ErrBadHeader)
	}
	return t, nil
}

// CheckHeader checks that the header hashes to a value not above its own
// target and that the target is not easier than the main chain's limit
// (0x1d00ffff, difficulty 1: about 2^32 evaluations). It does not and cannot
// show that the header is on the best chain, and it proves very little work
// (a header of difficulty 1 passes): the check that Upgrade and CheckEvidence
// apply is CheckHeaderAt, which adds a floor tied to the height.
func CheckHeader(header []byte) (HeaderResult, error) {
	limit, _ := CompactToTarget(powLimitBits)
	return checkHeaderWith(header, limit, nil)
}

// checkHeaderWith is the work check with its two bounds as arguments: limit is
// the easiest target any header may have (the chain's limit), maxTarget, when not
// nil, the floor (the easiest target accepted). The tests use it with bounds
// low enough to mine a header in a second.
func checkHeaderWith(header []byte, limit, maxTarget *big.Int) (HeaderResult, error) {
	var res HeaderResult
	if len(header) != HeaderSize {
		return res, fmt.Errorf("%w: %d bytes, want %d", ErrBadHeader, len(header), HeaderSize)
	}
	bits := binary.LittleEndian.Uint32(header[72:76])
	target, err := CompactToTarget(bits)
	if err != nil {
		return res, err
	}
	if target.Cmp(limit) > 0 {
		return res, fmt.Errorf("%w: target easier than the proof-of-work limit", ErrBadHeader)
	}
	if maxTarget != nil && target.Cmp(maxTarget) > 0 {
		return res, fmt.Errorf("%w: the work of the header is below the floor for its height", ErrBadHeader)
	}
	h1 := sha256.Sum256(header)
	res.Hash = sha256.Sum256(h1[:])
	var be [32]byte
	for i := range be {
		be[i] = res.Hash[31-i]
	}
	if new(big.Int).SetBytes(be[:]).Cmp(target) > 0 {
		return res, fmt.Errorf("%w: hash does not meet the target", ErrBadHeader)
	}
	res.Time = time.Unix(int64(binary.LittleEndian.Uint32(header[68:72])), 0).UTC()
	res.Bits = bits
	res.Work = new(big.Int).Div(new(big.Int).Lsh(big.NewInt(1), 256), new(big.Int).Add(target, big.NewInt(1)))
	return res, nil
}

// DefaultWorkSlack is how many times easier than the target of the nearest
// earlier checkpoint of the table (the real headers of testdata/headers.txt) a
// header's target may be: the default minimum-work rule is "the difficulty is at
// least 1/16 of that of the last known real header at or below the height". The
// factor leaves room for a fall of the difficulty (the largest fall in the
// history of the chain, in 2021, was about 28 percent in one step) and is
// small beside the rise since the checkpoint was taken.
const DefaultWorkSlack = 16

// MaxTargetAt is the easiest target that CheckHeaderAt accepts for a header
// of the block at height: slack times the target of the nearest checkpoint
// at or below height (the table is facts of the public chain, see claims.go).
// slack below 1 is taken as 1.
func MaxTargetAt(height uint64, slack uint) *big.Int {
	if slack < 1 {
		slack = 1
	}
	var bits uint32
	for _, c := range checkpoints {
		if c.height <= height {
			bits = c.bits
		}
	}
	t, _ := CompactToTarget(bits)
	return t.Mul(t, new(big.Int).SetUint64(uint64(slack)))
}

// CheckHeaderFloor is CheckHeader plus a minimum of work: the target of the
// header must not exceed maxTarget (a target above it is an error that wraps
// ErrBadHeader). It is the work rule with the floor as an argument; the tests
// use it with a floor low enough to mine a header in a second.
func CheckHeaderFloor(header []byte, maxTarget *big.Int) (HeaderResult, error) {
	limit, _ := CompactToTarget(powLimitBits)
	return checkHeaderWith(header, limit, maxTarget)
}

// CheckHeaderAt is the work rule of Upgrade and CheckEvidence (correction 91):
// CheckHeader, and a target no easier than DefaultWorkSlack times that of the
// nearest checkpoint at or below height. A header of difficulty 1, which costs
// about 2^32 evaluations, is refused for every height of the chain's present
// era (its target is more than 2^40 times above the floor); a real header of
// that era passes. What the check proves and what it does not is in the
// package documentation ("What header_checked proves").
func CheckHeaderAt(header []byte, height uint64) (HeaderResult, error) {
	return CheckHeaderFloor(header, MaxTargetAt(height, DefaultWorkSlack))
}

// CheckAgainstHeader checks the proof against a block header the caller
// trusts as the header of the block at height: the header must satisfy
// CheckHeader, the tree must hold at least one Bitcoin attestation of that
// height, and the message of such a node (the value the operations from the
// digest lead to, the merkle root the proof commits to) must equal the
// header's merkle root field. The result carries the header time.
func CheckAgainstHeader(root *Node, height uint64, header []byte) (HeaderResult, error) {
	res, err := CheckHeader(header)
	if err != nil {
		return res, err
	}
	mr, _ := MerkleRoot(header)
	res.Height = height
	seen := false
	for _, l := range root.Leaves() {
		if l.Att.Kind != AttBitcoin || l.Att.Height != height {
			continue
		}
		seen = true
		if len(l.Msg) == 32 && bytes.Equal(l.Msg, mr) {
			res.Matches++
		}
	}
	if !seen {
		return res, fmt.Errorf("%w: the proof has no bitcoin attestation for height %d", ErrNoMatch, height)
	}
	if res.Matches == 0 {
		return res, fmt.Errorf("%w: merkle root differs", ErrNoMatch)
	}
	return res, nil
}

// BitcoinClaims returns the Bitcoin attestations of the tree with the merkle
// root each commits to, in canonical order.
func BitcoinClaims(root *Node) []Leaf {
	var out []Leaf
	for _, l := range root.Leaves() {
		if l.Att.Kind == AttBitcoin {
			out = append(out, l)
		}
	}
	return out
}
