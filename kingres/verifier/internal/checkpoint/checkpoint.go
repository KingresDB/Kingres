// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package checkpoint parses and verifies transparency log checkpoints as C2SP
// "tlog-checkpoint" defines them (https://c2sp.org/tlog-checkpoint): a signed
// note whose text is the origin, the tree size and the root hash.
package checkpoint

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
)

// MaxOriginLength is the longest origin: tlog-cosignature says a name, and
// so an origin, is at most 255 bytes.
const MaxOriginLength = 255

// MaxExtensionLines is the most extension lines a checkpoint may carry. They are
// opaque to the verifier, and the state file keeps the whole note, so they are
// bounded (with the line length of signednote) instead of being stored as sent.
const MaxExtensionLines = 16

// Checkpoint is the parsed text of a checkpoint.
type Checkpoint struct {
	Origin string
	Size   uint64
	Root   merkle.Hash
	// Extensions are the extension lines. tlog-checkpoint calls them opaque
	// and not recommended; they are signed by Ed25519 log signatures, but no
	// check here gives them meaning.
	Extensions []string
}

// Step names of Error, which the command line prints.
const (
	StepParse      = "parse"
	StepSignature  = "signature"
	StepCheckpoint = "checkpoint"
	StepOrigin     = "origin"
)

// Error is a verification failure with the step that failed.
type Error struct {
	Step string
	Err  error
}

func (e *Error) Error() string { return e.Err.Error() }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// ValidOrigin reports whether s can be an origin: the rules of a name in
// tlog-cosignature, non-empty UTF-8 of at most 255 bytes without Unicode
// spaces, plus signs or control characters. (tlog-checkpoint says the
// origin is "as defined in" that document.)
func ValidOrigin(s string) bool {
	if len(s) > MaxOriginLength {
		return false
	}
	return signednote.ValidName(s)
}

// Parse parses the text of a checkpoint, the note text after the signatures
// have been verified. It needs at least three non-empty lines, a valid origin,
// a tree size written as a canonical decimal (no sign, no leading zero, below
// 2^62), and a root of exactly 32 bytes in canonical base64; the root of the
// empty tree must be the hash of the empty string (RFC 9162 section 2.1.1).
// Extension lines must be non-empty.
func Parse(text []byte) (*Checkpoint, error) {
	if !utf8.Valid(text) {
		return nil, errors.New("checkpoint is not valid UTF-8")
	}
	s := string(text)
	body, ok := strings.CutSuffix(s, "\n")
	if !ok {
		return nil, errors.New("checkpoint text does not end with a newline")
	}
	lines := strings.Split(body, "\n")
	if len(lines) < 3 {
		return nil, fmt.Errorf("checkpoint has %d lines, expected at least 3 (origin, size, root)", len(lines))
	}
	for i, l := range lines {
		if l == "" {
			return nil, fmt.Errorf("checkpoint line %d is empty, expected every line to be non-empty", i+1)
		}
	}
	cp := &Checkpoint{Origin: lines[0]}
	if !ValidOrigin(cp.Origin) {
		return nil, fmt.Errorf("checkpoint origin %q is not valid, expected 1 to %d bytes without spaces, plus signs or control characters", cp.Origin, MaxOriginLength)
	}
	size, err := parseSize(lines[1])
	if err != nil {
		return nil, err
	}
	cp.Size = size
	rootBytes, err := signednote.DecodeBase64(lines[2])
	if err != nil {
		return nil, fmt.Errorf("checkpoint root: %w", err)
	}
	if len(rootBytes) != len(cp.Root) {
		return nil, fmt.Errorf("checkpoint root has %d bytes, expected %d", len(rootBytes), len(cp.Root))
	}
	copy(cp.Root[:], rootBytes)
	if cp.Size == 0 && cp.Root != merkle.EmptyRoot() {
		return nil, errors.New("checkpoint of size 0 does not have the root of the empty tree (the hash of the empty string)")
	}
	if len(lines)-3 > MaxExtensionLines {
		return nil, fmt.Errorf("checkpoint has %d extension lines, expected at most %d", len(lines)-3, MaxExtensionLines)
	}
	cp.Extensions = lines[3:]
	return cp, nil
}

func parseSize(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') || len(s) > 19 {
		return 0, fmt.Errorf("checkpoint size %q is not a canonical decimal, expected digits without leading zeros", s)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, fmt.Errorf("checkpoint size %q is not a canonical decimal, expected digits only", s)
		}
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil || n >= tlog.MaxTreeSize {
		return 0, fmt.Errorf("checkpoint size %q is out of range, expected less than %d", s, tlog.MaxTreeSize)
	}
	return n, nil
}

// Text returns the note text of the checkpoint.
func (c *Checkpoint) Text() string {
	var b strings.Builder
	b.WriteString(c.Origin + "\n" + strconv.FormatUint(c.Size, 10) + "\n")
	b.WriteString(base64.StdEncoding.EncodeToString(c.Root[:]) + "\n")
	for _, e := range c.Extensions {
		b.WriteString(e + "\n")
	}
	return b.String()
}

// Verify parses a signed note, verifies it against keys, checks that the
// origin is wantOrigin and returns the checkpoint with the keys that
// signed. The text is parsed only after a trusted signature has verified,
// because the specification says an unsigned note text must be ignored. The
// keys must be the ones for wantOrigin (the key name of a log's signature is
// its origin, ledger-phase7.md 3.2.4).
func Verify(note []byte, keys []signednote.Key, wantOrigin string) (*Checkpoint, []signednote.Key, error) {
	n, err := signednote.Parse(note)
	if err != nil {
		return nil, nil, &Error{StepParse, err}
	}
	signers, err := signednote.Verify(n, keys)
	if err != nil {
		return nil, nil, &Error{StepSignature, err}
	}
	cp, err := Parse(n.Text)
	if err != nil {
		return nil, nil, &Error{StepCheckpoint, err}
	}
	if cp.Origin != wantOrigin {
		return nil, nil, &Error{StepOrigin, fmt.Errorf("checkpoint origin is %q, expected %q", cp.Origin, wantOrigin)}
	}
	return cp, signers, nil
}

// VerifyHeld is Verify for a checkpoint the auditor holds: it judges the note by
// the signatures that verify (signednote.VerifyHeld) and returns, besides the
// checkpoint and the keys that verified, how many signature lines named a key of
// keys and did not verify. It also returns the parsed note, so that the caller
// can write the canonical form of what was verified (signednote.Canonical).
func VerifyHeld(note []byte, keys []signednote.Key, wantOrigin string) (*Checkpoint, []signednote.Key, int, *signednote.Note, error) {
	n, err := signednote.Parse(note)
	if err != nil {
		return nil, nil, 0, nil, &Error{StepParse, err}
	}
	signers, wrong, err := signednote.VerifyHeld(n, keys)
	if err != nil {
		return nil, nil, wrong, nil, &Error{StepSignature, err}
	}
	cp, err := Parse(n.Text)
	if err != nil {
		return nil, nil, wrong, nil, &Error{StepCheckpoint, err}
	}
	if cp.Origin != wantOrigin {
		return nil, nil, wrong, nil, &Error{StepOrigin, fmt.Errorf("checkpoint origin is %q, expected %q", cp.Origin, wantOrigin)}
	}
	return cp, signers, wrong, n, nil
}
