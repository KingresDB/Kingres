// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package otsproof reads the OpenTimestamps fragments of an anchor and checks a Bitcoin attestation against a block
// header the auditor supplies. It is written from the description of the format in the design (ledger-phase7.md 6.5)
// and from the serialisation of the reference library (python-opentimestamps: core/timestamp.py, core/op.py and
// core/notary.py, read as text; nothing of it is copied), and shares no code with the publisher's library.
//
// A fragment is a tree rooted at the digest: each node is the message it stands for; its serialisation is a list of
// items, each either an attestation (0x00, an 8-byte tag, the payload as a length-prefixed string) or an edge (an
// operation tag, the argument of append and prepend as a length-prefixed string, then the child node). Every item
// but the last of a node is introduced by 0xff. A length and a number are a little-endian base 128 varuint, at most
// nine bytes. Operations understood: append (0xf0), prepend (0xf1), SHA-256 (0x08) and SHA-1 (0x02). Any other
// operation (RIPEMD-160, reverse, hexlify, Keccak-256, anything unknown) is refused by name as an "unsupported
// operation", never skipped: a message that depends on it cannot be computed here.
//
// Attestations: Bitcoin (tag 0588960d73d71901, the payload is the varuint height), pending (tag 83dfe30d2ef90c8e, the
// payload is a length-prefixed calendar URI) and any other tag, kept as opaque. Only a Bitcoin attestation can be
// verified, and only against a header of the auditor's: the header's double SHA-256, read as a little-endian number,
// must not exceed the target its own bits field names (a work check, not a proof that the block is on the best chain),
// its merkle root (bytes 36 to 68, as they stand in the header) must be the message the attestation sits on, and its time
// (the uint32 at offset 68) is the bound printed as "no later than". A header the auditor did not give is never taken
// from the evidence.
package otsproof

import (
	"crypto/sha1" // #nosec G505 -- SHA-1 is an operation of the format
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// Limits (all enforced while reading, before anything is allocated beyond them).
const (
	MaxFragment   = 64 << 10
	MaxDepth      = 256
	MaxNodes      = 1 << 14
	MaxMessageLen = 1 << 13
	MaxArg        = 4096
	MaxPayload    = 8192
	MaxURI        = 1000
	MaxHeight     = 1<<31 - 1
)

// Attestation kinds.
const (
	Bitcoin = "bitcoin"
	Pending = "pending"
	Other   = "other"
)

var (
	tagBitcoin = [8]byte{0x05, 0x88, 0x96, 0x0d, 0x73, 0xd7, 0x19, 0x01}
	tagPending = [8]byte{0x83, 0xdf, 0xe3, 0x0d, 0x2e, 0xf9, 0x0c, 0x8e}
)

// UnsupportedOpError says that an operation the format defines, or that the file holds, is not computed here.
type UnsupportedOpError struct {
	Tag  byte
	Name string
}

func (e *UnsupportedOpError) Error() string {
	return fmt.Sprintf("unsupported operation %s (tag 0x%02x): a message that depends on it cannot be computed here", e.Name, e.Tag)
}

// Attestation is a leaf of a fragment.
type Attestation struct {
	Kind   string
	Tag    [8]byte
	Height uint64 // Bitcoin
	URI    string // Pending
	// Message is what the attestation sits on: the digest after the operations of its path.
	Message []byte
	// Hashes is the number of SHA-256 operations on the path, and EndsInDoubleSHA256 says that the last two operations
	// of the path are SHA-256 (a Bitcoin merkle node is a double SHA-256; a path that does not end so reaches a merkle
	// root some other way and is not what a calendar writes).
	Hashes             int
	EndsInDoubleSHA256 bool
}

type reader struct {
	b     []byte
	pos   int
	nodes int
}

var errEOF = errors.New("the fragment ends in the middle of an item")

func (r *reader) byte() (byte, error) {
	if r.pos >= len(r.b) {
		return 0, errEOF
	}
	c := r.b[r.pos]
	r.pos++
	return c, nil
}

func (r *reader) varuint() (uint64, error) {
	var v uint64
	for i := 0; i < 9; i++ {
		c, err := r.byte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << (7 * uint(i))
		if c&0x80 == 0 {
			if c == 0 && i > 0 {
				return 0, errors.New("a varuint is not minimal (it ends in a zero byte)")
			}
			return v, nil
		}
	}
	return 0, errors.New("a varuint has more than 9 bytes")
}

func (r *reader) varbytes(max int) ([]byte, error) {
	n, err := r.varuint()
	if err != nil {
		return nil, err
	}
	if n > uint64(max) {
		return nil, fmt.Errorf("a string of %d bytes, expected at most %d", n, max)
	}
	if r.pos+int(n) > len(r.b) {
		return nil, errEOF
	}
	out := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}

// Walk reads a fragment rooted at digest and returns every attestation with the message it sits on. The whole input must be
// consumed. A fragment that leaves bytes over, nests deeper than MaxDepth, has more than MaxNodes items or builds a message
// longer than MaxMessageLen is refused.
func Walk(fragment []byte, digest []byte) ([]Attestation, error) {
	if len(fragment) == 0 || len(fragment) > MaxFragment {
		return nil, fmt.Errorf("a fragment of %d bytes, expected 1 to %d", len(fragment), MaxFragment)
	}
	r := &reader{b: fragment}
	var out []Attestation
	if err := r.node(digest, 0, 0, 0, &out); err != nil {
		return nil, err
	}
	if r.pos != len(r.b) {
		return nil, fmt.Errorf("%d byte(s) follow the fragment", len(r.b)-r.pos)
	}
	return out, nil
}

// node reads one node: items until one that is not introduced by 0xff.
func (r *reader) node(msg []byte, depth, hashes, tail int, out *[]Attestation) error {
	if depth > MaxDepth {
		return fmt.Errorf("the fragment nests deeper than %d", MaxDepth)
	}
	tag, err := r.byte()
	if err != nil {
		return err
	}
	for tag == 0xff {
		t, err := r.byte()
		if err != nil {
			return err
		}
		if err := r.item(t, msg, depth, hashes, tail, out); err != nil {
			return err
		}
		if tag, err = r.byte(); err != nil {
			return err
		}
	}
	return r.item(tag, msg, depth, hashes, tail, out)
}

func (r *reader) item(tag byte, msg []byte, depth, hashes, tail int, out *[]Attestation) error {
	if r.nodes++; r.nodes > MaxNodes {
		return fmt.Errorf("the fragment has more than %d items", MaxNodes)
	}
	if tag == 0x00 {
		var at Attestation
		raw, err := r.varbytesTag()
		if err != nil {
			return err
		}
		copy(at.Tag[:], raw)
		payload, err := r.varbytes(MaxPayload)
		if err != nil {
			return err
		}
		at.Message = append([]byte(nil), msg...)
		at.Hashes = hashes
		at.EndsInDoubleSHA256 = tail >= 2
		switch at.Tag {
		case tagBitcoin:
			pr := &reader{b: payload}
			h, err := pr.varuint()
			if err != nil || pr.pos != len(payload) || h > MaxHeight {
				return errors.New("a Bitcoin attestation whose payload is not one varuint height below 2^31")
			}
			at.Kind, at.Height = Bitcoin, h
		case tagPending:
			pr := &reader{b: payload}
			uri, err := pr.varbytes(MaxURI)
			if err != nil || pr.pos != len(payload) {
				return errors.New("a pending attestation whose payload is not one length-prefixed URI")
			}
			for _, c := range uri {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '/' || c == ':') {
					return errors.New("a pending attestation whose URI has a character outside letters, digits and -._/:")
				}
			}
			at.Kind, at.URI = Pending, string(uri)
		default:
			at.Kind = Other
		}
		*out = append(*out, at)
		return nil
	}
	var next []byte
	newTail := 0
	switch tag {
	case 0xf0, 0xf1:
		arg, err := r.varbytes(MaxArg)
		if err != nil {
			return err
		}
		if len(arg) == 0 {
			return errors.New("an append or prepend with an empty argument")
		}
		if len(msg)+len(arg) > MaxMessageLen {
			return fmt.Errorf("a message of more than %d bytes", MaxMessageLen)
		}
		if tag == 0xf0 {
			next = append(append(make([]byte, 0, len(msg)+len(arg)), msg...), arg...)
		} else {
			next = append(append(make([]byte, 0, len(msg)+len(arg)), arg...), msg...)
		}
	case 0x08:
		s := sha256.Sum256(msg)
		next = s[:]
		hashes++
		newTail = tail + 1
	case 0x02:
		s := sha1.Sum(msg) // #nosec G401 -- an operation of the format
		next = s[:]
	case 0x03:
		return &UnsupportedOpError{tag, "RIPEMD-160"}
	case 0xf2:
		return &UnsupportedOpError{tag, "reverse"}
	case 0xf3:
		return &UnsupportedOpError{tag, "hexlify"}
	case 0x67:
		return &UnsupportedOpError{tag, "Keccak-256"}
	default:
		return fmt.Errorf("unknown operation tag 0x%02x", tag)
	}
	return r.node(next, depth+1, hashes, newTail, out)
}

// varbytesTag reads the eight bytes of an attestation tag (not length-prefixed).
func (r *reader) varbytesTag() ([]byte, error) {
	if r.pos+8 > len(r.b) {
		return nil, errEOF
	}
	b := r.b[r.pos : r.pos+8]
	r.pos += 8
	return b, nil
}

// Header is a Bitcoin block header that passed the work check.
type Header struct {
	MerkleRoot [32]byte
	Time       time.Time
	Bits       uint32
	Hash       [32]byte // the double SHA-256, in the order of the bytes it was computed in
}

// powLimit is the easiest target a header may name: that of difficulty 1 (bits 0x1d00ffff), the limit of the main network.
var powLimit = new(big.Int).Lsh(big.NewInt(0xffff), 8*(0x1d-3))

// ParseHeader reads an 80-byte block header and checks its proof of work against its own bits field: the double SHA-256,
// read as a little-endian number, must not exceed the target, and the target must not be easier than difficulty 1 and
// must not be negative, zero or overflowing. It does not show that the block is on the best chain.
func ParseHeader(h []byte) (*Header, error) { return ParseHeaderLimit(h, powLimit) }

// ParseHeaderLimit is ParseHeader with another easiest target; production code passes the one of difficulty 1, and a test
// that must build a header without mining 2^32 hashes passes an easier one.
func ParseHeaderLimit(h []byte, limit *big.Int) (*Header, error) {
	if len(h) != 80 {
		return nil, fmt.Errorf("a block header has 80 bytes, this has %d", len(h))
	}
	bits := uint32(h[72]) | uint32(h[73])<<8 | uint32(h[74])<<16 | uint32(h[75])<<24
	exp := bits >> 24
	mant := bits & 0x007fffff
	if bits&0x00800000 != 0 || mant == 0 {
		return nil, fmt.Errorf("the bits field 0x%08x names a negative or zero target", bits)
	}
	var target *big.Int
	if exp <= 3 {
		target = big.NewInt(int64(mant >> (8 * (3 - exp))))
	} else {
		if exp > 34 {
			return nil, fmt.Errorf("the bits field 0x%08x names a target that overflows 256 bits", bits)
		}
		target = new(big.Int).Lsh(big.NewInt(int64(mant)), 8*uint(exp-3))
	}
	if target.Sign() == 0 || target.Cmp(limit) > 0 {
		return nil, fmt.Errorf("the bits field 0x%08x names a target easier than difficulty 1 (or none)", bits)
	}
	first := sha256.Sum256(h)
	second := sha256.Sum256(first[:])
	var be [32]byte
	for i := range second {
		be[i] = second[31-i]
	}
	if new(big.Int).SetBytes(be[:]).Cmp(target) > 0 {
		return nil, errors.New("the header's hash does not meet the target of its own bits field: it is not a mined block")
	}
	out := &Header{Bits: bits, Hash: second}
	copy(out.MerkleRoot[:], h[36:68])
	out.Time = time.Unix(int64(uint32(h[68])|uint32(h[69])<<8|uint32(h[70])<<16|uint32(h[71])<<24), 0).UTC()
	return out, nil
}

// Matches reports whether the message of an attestation is the merkle root of the header.
func (h *Header) Matches(msg []byte) bool {
	return len(msg) == 32 && string(msg) == string(h.MerkleRoot[:])
}
