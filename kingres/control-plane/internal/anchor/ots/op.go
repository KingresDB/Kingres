// SPDX-License-Identifier: AGPL-3.0-only

package ots

import (
	"bytes"
	"crypto/sha1" //nolint:gosec // SHA-1 is an operation of the OpenTimestamps format
	"crypto/sha256"
	"errors"
	"fmt"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// Limits of the format (taken from the reference library) and of this package.
const (
	// MaxMessageLen is the longest message an operation accepts and the
	// longest result it may give (Op.MAX_MSG_LENGTH and MAX_RESULT_LENGTH).
	MaxMessageLen = 4096
	// MaxPayloadLen is the longest attestation payload (MAX_PAYLOAD_SIZE).
	MaxPayloadLen = 8192
	// MaxURILen is the longest pending-attestation URI.
	MaxURILen = 1000
	// MaxDepth is the number of node levels the parser accepts (the reference
	// recursion limit of 256: a root and 255 chained operations).
	MaxDepth = 256
	// MaxHeight is the highest block height of a blockchain attestation.
	MaxHeight = anchor.MaxBitcoinHeight
	// MaxProofSize is the largest detached file or fragment parsed (this
	// package's own limit; the reference has none).
	MaxProofSize = 1 << 20
	// MaxNodes is the most nodes in a tree (this package's own limit; it
	// bounds memory, since every node keeps its message of up to 4096 bytes).
	MaxNodes = 8192
	// MaxVarUintBytes is the longest encoding of a varuint (limits it to 63 bits).
	MaxVarUintBytes = 9
)

// Operation tags.
const (
	TagSHA1      byte = 0x02
	TagRIPEMD160 byte = 0x03
	TagSHA256    byte = 0x08
	TagAppend    byte = 0xf0
	TagPrepend   byte = 0xf1
)

// Tags the format defines and this package refuses by name.
var refusedOps = map[byte]string{
	0xf2: "reverse",
	0xf3: "hexlify",
	0x67: "keccak256",
}

// UnsupportedOpError is returned for an operation tag that this package does
// not implement: one the format defines (Name is set: reverse, hexlify,
// keccak256) or one it does not know (Name is empty).
type UnsupportedOpError struct {
	Tag  byte
	Name string
}

func (e *UnsupportedOpError) Error() string {
	if e.Name != "" {
		return fmt.Sprintf("ots: unsupported operation %s (tag 0x%02x)", e.Name, e.Tag)
	}
	return fmt.Sprintf("ots: unknown operation tag 0x%02x", e.Tag)
}

// Op is one edge of the timestamp tree.
type Op struct {
	Tag byte
	Arg []byte // for append and prepend only
}

// Name returns the lower case name of the operation.
func (o Op) Name() string {
	switch o.Tag {
	case TagSHA1:
		return "sha1"
	case TagRIPEMD160:
		return "ripemd160"
	case TagSHA256:
		return "sha256"
	case TagAppend:
		return "append"
	case TagPrepend:
		return "prepend"
	}
	return fmt.Sprintf("tag0x%02x", o.Tag)
}

func (o Op) binary() bool { return o.Tag == TagAppend || o.Tag == TagPrepend }

// IsCrypto reports whether the operation is a hash (the only kind a detached
// file may use as its file hash operation).
func (o Op) IsCrypto() bool {
	return o.Tag == TagSHA1 || o.Tag == TagRIPEMD160 || o.Tag == TagSHA256
}

// DigestLen returns the output length of a hash operation, else 0.
func DigestLen(tag byte) int {
	switch tag {
	case TagSHA1, TagRIPEMD160:
		return 20
	case TagSHA256:
		return 32
	}
	return 0
}

// Apply applies the operation to msg. The message and the result are limited
// to MaxMessageLen bytes; the result is never empty.
func (o Op) Apply(msg []byte) ([]byte, error) {
	if len(msg) > MaxMessageLen {
		return nil, fmt.Errorf("ots: message too long for %s: %d > %d", o.Name(), len(msg), MaxMessageLen)
	}
	var r []byte
	switch o.Tag {
	case TagAppend:
		r = append(append(make([]byte, 0, len(msg)+len(o.Arg)), msg...), o.Arg...)
	case TagPrepend:
		r = append(append(make([]byte, 0, len(msg)+len(o.Arg)), o.Arg...), msg...)
	case TagSHA256:
		s := sha256.Sum256(msg)
		r = s[:]
	case TagSHA1:
		s := sha1.Sum(msg) //nolint:gosec
		r = s[:]
	case TagRIPEMD160:
		h := newRIPEMD160()
		h.Write(msg)
		r = h.Sum(nil)
	default:
		return nil, opError(o.Tag)
	}
	if len(r) == 0 || len(r) > MaxMessageLen {
		return nil, fmt.Errorf("ots: result too long for %s: %d > %d", o.Name(), len(r), MaxMessageLen)
	}
	return r, nil
}

func opError(tag byte) error {
	return &UnsupportedOpError{Tag: tag, Name: refusedOps[tag]}
}

// compareOps orders operations as the reference does: by tag, then by argument bytes.
func compareOps(a, b Op) int {
	if a.Tag != b.Tag {
		if a.Tag < b.Tag {
			return -1
		}
		return 1
	}
	return bytes.Compare(a.Arg, b.Arg)
}

// Errors of the parsers.
var (
	ErrTruncated   = errors.New("ots: truncated data")
	ErrTrailing    = errors.New("ots: trailing data after the proof")
	ErrBadMagic    = errors.New("ots: bad file magic")
	ErrVersion     = errors.New("ots: unsupported major version")
	ErrDepth       = errors.New("ots: proof is nested too deeply")
	ErrTooLarge    = errors.New("ots: proof is too large")
	ErrMalformed   = errors.New("ots: malformed proof")
	ErrEmptyNode   = errors.New("ots: a timestamp node with no operation and no attestation")
	ErrNodeBudget  = errors.New("ots: proof has too many nodes")
	ErrBadHashOp   = errors.New("ots: file hash operation is not a hash")
	ErrURI         = errors.New("ots: invalid pending attestation uri")
	ErrBadHeader   = errors.New("ots: bad block header")
	ErrNoMatch     = errors.New("ots: no attestation matches the block header")
	ErrProofDigest = errors.New("ots: proof is for another digest")
)
