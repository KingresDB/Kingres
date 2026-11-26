// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package proof

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/chain"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
)

// A proof of a row carries every block header after the block of the row up to
// the block of the digest it was made for: one header per block, about 335 bytes
// of JSON each, so a row a year old on a ledger that makes a block every 10
// seconds has about 3.2 million of them, a file of about a gigabyte. A cap on the
// size of the file would refuse honest proofs of old rows, and holding the
// headers in memory (about 100 bytes each, plus the file) would use gigabytes.
// So a proof is read in two passes over the file, with a memory that does not
// depend on its size:
//
//   - ParseStream reads the whole file once. Every element of chain is parsed and
//     its types are checked, then dropped; the rest of the proof is kept, and so
//     is the SHA-256 of the bytes read and the number of elements.
//   - VerifyStream runs the steps that need no chain, and then reads the file
//     again and walks the elements as they come, from the block of the proof
//     towards the digest, keeping one header. The member chain may come before
//     the member block in the file, which is why the walk cannot be done in the
//     first pass.
//
// The two readings must give the same bytes (the hash is compared at the end of
// the second), so a file that changes between them is refused and the verdict is
// about the very bytes that were parsed. Nothing is reported as verified before
// the second reading has ended and its hash was compared.

// Source opens the proof file; it is called once for each reading.
type Source func() (io.ReadCloser, error)

// Sum is the SHA-256 of the bytes of a proof file as ParseStream read them.
type Sum [sha256.Size]byte

// ParseStream is Parse over a file read in pieces, without keeping the headers
// of chain (the returned proof has ChainLen and no Chain).
func ParseStream(open Source) (*Proof, Sum, error) {
	var sum Sum
	rc, err := open()
	if err != nil {
		return nil, sum, err
	}
	defer rc.Close()
	h := sha256.New()
	var n uint64
	v, err := jsonx.ParseReader(io.TeeReader(rc, h), jsonx.DefaultLimits, "chain", func(el jsonx.Value) error {
		_, err := chain.ParseHeader(el)
		n++
		return err
	})
	if err != nil {
		return nil, sum, err
	}
	p, err := fromValue(v)
	if err != nil {
		return nil, sum, err
	}
	p.ChainLen = n
	copy(sum[:], h.Sum(nil))
	return p, sum, nil
}

// VerifyStream is Verify for a proof read by ParseStream: the walk from the
// block of the proof to the digest reads the file again through open and compares
// its hash with sum.
func VerifyStream(p *Proof, sum Sum, d chain.Digest, exp Expect, open Source) (*Result, error) {
	return verifyWith(p, d, exp, func() (format.Header, error) { return reachStream(p, sum, d, open) })
}

var errChanged = errors.New("the proof file is not the same on the second reading as on the first: nothing was verified, give a file that does not change")

func reachStream(p *Proof, sum Sum, d chain.Digest, open Source) (format.Header, error) {
	if d.Height < p.Block.Height {
		return format.Header{}, fail(StepChainWalk, "digest is at height %d, older than the proof's block %d: it cannot prove anything about this block", d.Height, p.Block.Height)
	}
	need := d.Height - p.Block.Height
	if p.ChainLen < need {
		return format.Header{}, fail(StepChainWalk, "chain has %d header(s), expected at least %d to reach height %d", p.ChainLen, need, d.Height)
	}
	rc, err := open()
	if err != nil {
		return format.Header{}, &Error{Step: StepChainWalk, Err: err}
	}
	defer rc.Close()
	hash := sha256.New()
	cur := p.Block
	var i uint64
	_, err = jsonx.ParseReader(io.TeeReader(rc, hash), jsonx.DefaultLimits, "chain", func(el jsonx.Value) error {
		defer func() { i++ }()
		if i >= need {
			return nil // beyond the digest: read, hashed, not used
		}
		next, err := chain.ParseHeader(el)
		if err != nil {
			return errChanged
		}
		if next.HashVersion != format.HashVersion {
			return fail(StepChainWalk, "chain[%d]: hash_version is %d, expected %d", i, next.HashVersion, format.HashVersion)
		}
		if err := chain.CheckLink(cur, next); err != nil {
			return &Error{Step: StepChainWalk, Err: fmt.Errorf("chain[%d]: %w", i, err)}
		}
		if err := chain.CheckHeader(next); err != nil {
			return &Error{Step: StepChainWalk, Err: fmt.Errorf("chain[%d]: %w", i, err)}
		}
		cur = next
		return nil
	})
	if err != nil {
		var pe *Error
		if errors.As(err, &pe) {
			return format.Header{}, pe
		}
		return format.Header{}, &Error{Step: StepParse, Err: errChanged}
	}
	var again Sum
	copy(again[:], hash.Sum(nil))
	if again != sum || i != p.ChainLen {
		return format.Header{}, &Error{Step: StepParse, Err: errChanged}
	}
	if cur.BlockHash != d.BlockHash {
		return format.Header{}, fail(StepChainWalk, "block at height %d has hash %x, the digest says %x", d.Height, cur.BlockHash, d.BlockHash)
	}
	return cur, nil
}
