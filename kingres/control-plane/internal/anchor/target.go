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
	"unicode"
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
	// ReceiptClaimed is a receipt whose attestation passed every check that
	// needs no outside data but was not checked against a block header (kind
	// ots only). It is not an anchor.
	ReceiptClaimed  = "claimed"
	ReceiptAnchored = "anchored"
)

// Clock is the time source of the package: every function that needs the
// time takes one (or a time.Time). time.Now is a valid Clock.
type Clock func() time.Time

// Limits of the payload. The verifier (kingres-verify, module kingres/verifier)
// enforces the same limits on every note it reads; the two modules cannot
// import each other, so the values are repeated here and a test of this
// package reads the verifier's source constants and compares them.
const (
	// MaxSignedNote is the largest signed note accepted as a payload (the
	// verifier accepts up to 64 KiB; a note of the ledger is far smaller).
	MaxSignedNote = 16 << 10
	// MaxOriginLen is the longest origin accepted (verifier: MaxOriginLength).
	MaxOriginLen = 255
	// MaxSignatureLines is the most signature lines of a note (verifier:
	// signednote.MaxSignatures).
	MaxSignatureLines = 32
	// MaxNoteLineLen is the longest line of a note, text or signature, without
	// its newline (verifier: signednote.MaxLineLength).
	MaxNoteLineLen = 1024
	// TreeSizeLimit is the exclusive upper bound of a tree size (verifier:
	// tlog.MaxTreeSize, 2^62).
	TreeSizeLimit uint64 = 1 << 62
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
	// FirstAttempt is the time of the first attempt to anchor this checkpoint
	// at the target for this key (the record's first Submit, or its creation,
	// which is never later). It is required: NewPayload takes it and Check
	// refuses a zero value. The WORM target bounds the time of an object that
	// an earlier attempt wrote by it. For a Retry of a failed record it stays
	// the time of the original first attempt. Whole seconds, UTC.
	FirstAttempt time.Time
}

// AggregateID returns the part of ID before the slash.
func (p Payload) AggregateID() string {
	i := strings.IndexByte(p.ID, '/')
	if i < 0 {
		return p.ID
	}
	return p.ID[:i]
}

// ValidOrigin reports whether s can be the origin of a note, by the rule of
// the verifier (kingres-verify): 1 to MaxOriginLen bytes of valid UTF-8 with
// no Unicode white space, no plus sign and no control character.
func ValidOrigin(s string) bool {
	if s == "" || len(s) > MaxOriginLen || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == '+' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// NewPayload builds the payload of the signed note of aggregate aggregateIDHex
// (lower case hexadecimal, 1 to 128 digits) for the checkpoint whose first
// attempt to be anchored at the target was at firstAttempt (required, see
// Payload.FirstAttempt). The shape rules are those of ParsePayload.
func NewPayload(aggregateIDHex string, signedNote []byte, firstAttempt time.Time) (Payload, error) {
	if firstAttempt.IsZero() {
		return Payload{}, fmt.Errorf("anchor: the time of the first attempt is required")
	}
	p, err := ParsePayload(aggregateIDHex, signedNote)
	if err != nil {
		return Payload{}, err
	}
	p.FirstAttempt = firstAttempt.UTC().Truncate(time.Second)
	return p, nil
}

// ParsePayload checks the shape of a signed note and returns its payload
// without a time of first attempt (a zero FirstAttempt, which Check refuses):
// for a reader of stored notes, such as an independent verifier, that has no
// attempt to speak of. It checks the shape the anchoring depends on, with the
// limits of the verifier (kingres-verify), so that nothing is anchored that
// the verifier would call malformed: valid UTF-8 without control characters
// other than newline; no line longer than MaxNoteLineLen; text of exactly
// three non-empty lines (an origin by ValidOrigin, a size in ASCII decimal
// without sign or leading zeros and below TreeSizeLimit, a root as standard
// base64 of 32 bytes); a blank line; one to MaxSignatureLines signature lines
// (U+2014, a space, a name, a space, base64). It does not verify a signature:
// that is the signer's and the verifier's job.
func ParsePayload(aggregateIDHex string, signedNote []byte) (Payload, error) {
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
	sigLines := strings.Split(strings.TrimSuffix(string(sigs), "\n"), "\n")
	if len(sigLines) > MaxSignatureLines {
		return Payload{}, fmt.Errorf("anchor: signed note has more than %d signature lines", MaxSignatureLines)
	}
	for _, l := range strings.Split(string(signedNote), "\n") {
		if len(l) > MaxNoteLineLen {
			return Payload{}, fmt.Errorf("anchor: signed note has a line longer than %d bytes", MaxNoteLineLen)
		}
	}
	for _, l := range sigLines {
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
	if !ValidOrigin(lines[0]) {
		return Payload{}, fmt.Errorf("anchor: origin is not valid: 1 to %d bytes of UTF-8 without white space, plus signs or control characters", MaxOriginLen)
	}
	sz, err := strconv.ParseUint(lines[1], 10, 64)
	if err != nil || strconv.FormatUint(sz, 10) != lines[1] || sz >= TreeSizeLimit {
		return Payload{}, fmt.Errorf("anchor: note size is not a canonical decimal below 2^62")
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

// Check reports whether p is exactly what NewPayload builds from p.SignedNote
// and the aggregate id of p.ID: every field, the digest included. A zero
// Payload, a digest that is not SHA-256 of the note text and a text that is
// not the head of the signed note are all refused.
func (p Payload) Check() error {
	if p.FirstAttempt.IsZero() {
		return fmt.Errorf("anchor: payload has no time of first attempt (use anchor.NewPayload)")
	}
	q, err := ParsePayload(p.AggregateID(), p.SignedNote)
	if err != nil {
		return err
	}
	if q.ID != p.ID || q.Origin != p.Origin || q.Size != p.Size || q.Digest != p.Digest ||
		!bytes.Equal(q.NoteText, p.NoteText) || !bytes.Equal(q.SignedNote, p.SignedNote) {
		return fmt.Errorf("anchor: payload fields are not those of its signed note")
	}
	return nil
}

// CheckPayload is Check as an *Error of class permanent with the code
// "bad_payload", for a target to return at the start of Submit: every target
// refuses a payload that was not built consistently (use NewPayload).
func CheckPayload(target string, p Payload) error {
	if err := p.Check(); err != nil {
		return Errorf(ClassPermanent, target, "submit", "bad_payload", "payload is inconsistent (use anchor.NewPayload): %s", err.Error())
	}
	return nil
}

// DigestHex returns the digest in lower case hexadecimal (payload_sha256).
func (p Payload) DigestHex() string { return hex.EncodeToString(p.Digest[:]) }

// Receipt is the result of Submit and Upgrade.
type Receipt struct {
	// State is ReceiptSubmitted, ReceiptClaimed or ReceiptAnchored.
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
