// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Kinds of target.
const (
	KindWORM = "worm"
	KindTSA  = "tsa"
	KindOTS  = "ots"
)

// Receipt states (a subset of the record states).
const (
	ReceiptSubmitted = "submitted"
	ReceiptAnchored  = "anchored"
)

// Clock is the time source of the package: every function that needs the
// time takes one (or a time.Time). time.Now is a valid Clock.
type Clock func() time.Time

// Limits of the payload.
const (
	// MaxSignedNote is the largest signed note accepted as a payload.
	MaxSignedNote = 16 << 10
	// MaxOriginLen is the longest origin accepted.
	MaxOriginLen = 256
)

// Target is the interface of section 6.2 of the design. Implementations are
// safe for concurrent use. Every method takes the context as its timeout and
// cancellation; none stores anything or starts a goroutine that outlives the
// call.
type Target interface {
	// Kind returns "worm", "tsa" or "ots".
	Kind() string
	// Validate checks the shape of the non-secret configuration (JSON).
	Validate(cfg json.RawMessage) error
	// Test is what the Test button runs: a list of named checks.
	Test(ctx context.Context) TestResult
	// Submit anchors the payload. It is idempotent per Payload.ID. A nil
	// error means the Receipt is valid; an error is an *Error.
	Submit(ctx context.Context, p Payload) (Receipt, error)
	// Upgrade turns a "submitted" receipt into a final one when the target
	// can; for worm and tsa it returns the receipt unchanged.
	Upgrade(ctx context.Context, r Receipt) (Receipt, error)
}

// Payload is what is anchored: one signed aggregate checkpoint.
type Payload struct {
	// ID is "<aggregate_id hex>/<size>".
	ID string
	// Origin and Size are those of the note text.
	Origin string
	Size   uint64
	// NoteText is the three lines of the checkpoint, each ending in a newline.
	NoteText []byte
	// SignedNote is NoteText, a blank line, and the signature lines.
	SignedNote []byte
	// Digest is SHA-256(NoteText).
	Digest [32]byte
}

// AggregateID returns the part of ID before the slash.
func (p Payload) AggregateID() string {
	i := strings.IndexByte(p.ID, '/')
	if i < 0 {
		return p.ID
	}
	return p.ID[:i]
}

// NewPayload builds the payload of the signed note of aggregate aggregateIDHex
// (lower case hexadecimal, 1 to 128 digits). It checks only the shape the
// anchoring depends on: valid UTF-8 without control characters other than
// newline; text of exactly three non-empty lines (origin, size in ASCII
// decimal without sign or leading zeros, root as standard base64 of 32 bytes);
// a blank line; at least one signature line (U+2014, a space, a name, a space,
// base64). It does not verify a signature: that is the signer's and the
// verifier's job.
func NewPayload(aggregateIDHex string, signedNote []byte) (Payload, error) {
	if len(aggregateIDHex) == 0 || len(aggregateIDHex) > 128 {
		return Payload{}, fmt.Errorf("anchor: aggregate id must have 1 to 128 hex digits")
	}
	for _, c := range aggregateIDHex {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return Payload{}, fmt.Errorf("anchor: aggregate id must be lower case hexadecimal")
		}
	}
	if len(signedNote) == 0 || len(signedNote) > MaxSignedNote {
		return Payload{}, fmt.Errorf("anchor: signed note must have 1 to %d bytes", MaxSignedNote)
	}
	if !utf8.Valid(signedNote) {
		return Payload{}, fmt.Errorf("anchor: signed note is not valid UTF-8")
	}
	for _, b := range signedNote {
		if b != '\n' && (b < 0x20 || b == 0x7f) {
			return Payload{}, fmt.Errorf("anchor: signed note contains a control character")
		}
	}
	i := bytes.Index(signedNote, []byte("\n\n"))
	if i < 0 {
		return Payload{}, fmt.Errorf("anchor: signed note has no blank line before its signatures")
	}
	text := signedNote[:i+1]
	sigs := signedNote[i+2:]
	if len(sigs) == 0 || sigs[len(sigs)-1] != '\n' {
		return Payload{}, fmt.Errorf("anchor: signed note signatures must end in a newline")
	}
	for _, l := range strings.Split(strings.TrimSuffix(string(sigs), "\n"), "\n") {
		rest, ok := strings.CutPrefix(l, "\xe2\x80\x94 ")
		name, b64, ok2 := strings.Cut(rest, " ")
		if !ok || !ok2 || name == "" || b64 == "" {
			return Payload{}, fmt.Errorf("anchor: signed note has a malformed signature line")
		}
		if _, err := base64.StdEncoding.DecodeString(b64); err != nil {
			return Payload{}, fmt.Errorf("anchor: signed note has a malformed signature line")
		}
	}
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) != 3 || lines[0] == "" || lines[1] == "" || lines[2] == "" {
		return Payload{}, fmt.Errorf("anchor: note text must be three non-empty lines")
	}
	if len(lines[0]) > MaxOriginLen {
		return Payload{}, fmt.Errorf("anchor: origin is longer than %d bytes", MaxOriginLen)
	}
	if strings.ContainsAny(lines[0], " \t") {
		return Payload{}, fmt.Errorf("anchor: origin contains a space")
	}
	sz, err := strconv.ParseUint(lines[1], 10, 64)
	if err != nil || strconv.FormatUint(sz, 10) != lines[1] {
		return Payload{}, fmt.Errorf("anchor: note size is not canonical decimal")
	}
	if root, err := base64.StdEncoding.DecodeString(lines[2]); err != nil || len(root) != 32 {
		return Payload{}, fmt.Errorf("anchor: note root is not base64 of 32 bytes")
	}
	return Payload{
		ID:         aggregateIDHex + "/" + strconv.FormatUint(sz, 10),
		Origin:     lines[0],
		Size:       sz,
		NoteText:   append([]byte(nil), text...),
		SignedNote: append([]byte(nil), signedNote...),
		Digest:     sha256.Sum256(text),
	}, nil
}

// DigestHex returns the digest in lower case hexadecimal (payload_sha256).
func (p Payload) DigestHex() string { return hex.EncodeToString(p.Digest[:]) }

// Receipt is the result of Submit and Upgrade.
type Receipt struct {
	// State is ReceiptSubmitted or ReceiptAnchored.
	State string
	// Evidence is the evidence object in its canonical encoding (the
	// "evidence" member of the evidence file, see EncodeEvidence).
	Evidence json.RawMessage
	// EvidenceTime is the time the target vouches for, when known.
	EvidenceTime *time.Time
	// NextCheck is when Upgrade should be called next (submitted only).
	NextCheck *time.Time
}

// Check statuses of a TestResult.
const (
	CheckOK      = "ok"
	CheckWarning = "warning"
	CheckFailed  = "failed"
)

// Check is one named line of a Test.
type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Text   string `json:"text"`
}

// TestResult is the answer of Target.Test.
type TestResult struct {
	Checks []Check `json:"checks"`
}

// OK reports whether no check failed.
func (r TestResult) OK() bool {
	for _, c := range r.Checks {
		if c.Status == CheckFailed {
			return false
		}
	}
	return true
}

// Add appends a check.
func (r *TestResult) Add(name, status, format string, args ...any) {
	r.Checks = append(r.Checks, Check{Name: name, Status: status, Text: fmt.Sprintf(format, args...)})
}
