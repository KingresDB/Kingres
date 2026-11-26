// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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
	// MaxExtensionLines is the most extension lines (lines after the root) of
	// a note text (verifier: checkpoint.MaxExtensionLines). The log library can
	// write them.
	MaxExtensionLines = 16
	// minSigBytes is the fewest decoded bytes of a signature line: the 4-byte
	// key id (verifier: the length check of signednote.Parse).
	minSigBytes = 4
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
	// SubmitStart is the start of the FIRST Submit call that this checkpoint
	// had for this target and this key. The caller makes it durable before it
	// makes that call and never changes it afterwards: not on a Retry, not on a
	// later attempt, not when the process restarts. It is required (NewPayload
	// takes it and Check refuses a zero value). The WORM target bounds the time
	// of the store by it (package worm: an existing object is accepted only
	// when its time is not earlier than SubmitStart less the tolerance); the
	// other targets do not use it. It is NOT the creation of the record and NOT
	// the time of the current attempt. A value earlier than the true start
	// widens the window of an object that was not written by this library (the
	// evidence states the value that was applied); a value later than the true
	// start can make an object that the first call wrote fail for good. Whole
	// seconds, UTC. A caller that wants a new start must use a new key (another
	// key prefix, or a new Generation).
	SubmitStart time.Time
	// Generation is the generation of the key (correction 95), persisted by the
	// caller with the record: 0 for every record that exists, and the key of
	// generation 0 is exactly the key that was always used. The WORM target puts
	// it in the object key ("<size>.g<n>.note" for n above 0); the other targets
	// ignore it, so that every target sees the same payload. The caller raises it
	// by one, with a new SubmitStart (WithGeneration), only on an operator's
	// explicit choice after a permanent refusal of the time of the store, never
	// automatically: every generation is one more object in the bucket that
	// claims to be the anchor of this checkpoint.
	Generation uint32
}

// Resumer is implemented by a target that can resume from the evidence of an
// earlier attempt, so that a scheduler that holds the Target interface can ask
// for it without knowing the concrete type (correction 106). ots.Target
// implements it: a Retry of an OpenTimestamps record that failed with
// claim_contradicted, or that stopped after a partial Submit, goes through Resume
// with the evidence that was kept, and not through Submit (which starts again
// from the calendars and loses the proof). prior is the evidence object in its
// canonical encoding (Receipt.Evidence, or what the target's own helper encodes
// from an error). A target that is not a Resumer has nothing to resume from: its
// Submit is idempotent by itself.
type Resumer interface {
	Resume(ctx context.Context, p Payload, prior json.RawMessage) (Receipt, error)
}

// checkNextGeneration is the rule of the step to a new key generation
// (correction 105): exactly the next generation, below the limit, with a start
// (whole seconds) later than the start of the generation before.
func checkNextGeneration(oldGen uint32, oldStart time.Time, g uint32, newStart time.Time) error {
	if newStart.IsZero() {
		return fmt.Errorf("anchor: the start of the first Submit call is required")
	}
	if oldGen >= MaxGeneration || g != oldGen+1 {
		return fmt.Errorf("anchor: the next generation after %d is %d, not %d (one step at a time, at most %d)", oldGen, oldGen+1, g, MaxGeneration)
	}
	if !newStart.UTC().Truncate(time.Second).After(oldStart.UTC().Truncate(time.Second)) {
		return fmt.Errorf("anchor: the start of generation %d must be later than the start of generation %d (%s)", g, oldGen, FormatTime(oldStart))
	}
	return nil
}

// WithGeneration returns p with the generation g and the start of the first
// Submit call for that generation, submitStart (required, as in NewPayload: a
// new generation is a new key and has a new first call). g must be exactly
// p.Generation plus one, at most MaxGeneration, and submitStart (whole seconds)
// later than p.SubmitStart: a lowered, repeated or skipped generation is
// refused (correction 105). The caller that keeps a Record uses
// Record.NextGeneration, which moves both values in one step.
func (p Payload) WithGeneration(g uint32, submitStart time.Time) (Payload, error) {
	if err := checkNextGeneration(p.Generation, p.SubmitStart, g, submitStart); err != nil {
		return Payload{}, err
	}
	p.Generation = g
	p.SubmitStart = submitStart.UTC().Truncate(time.Second)
	return p, nil
}

// PayloadFor builds the payload of a record from the record and the signed note
// of its checkpoint (correction 105): the SubmitStart and the Generation come from
// the record, so that a caller cannot forget them or pass the time of the current
// attempt. The record must have its start (Record.WithSubmitStart, persisted
// before the first Submit call), and the note must be the one the record stands
// for (its size and the SHA-256 of its text).
func PayloadFor(r Record, aggregateIDHex string, signedNote []byte) (Payload, error) {
	if r.SubmitStart.IsZero() {
		return Payload{}, fmt.Errorf("anchor: the record has no SubmitStart: set and persist it (Record.WithSubmitStart) before the first Submit call")
	}
	p, err := NewPayload(aggregateIDHex, signedNote, r.SubmitStart)
	if err != nil {
		return Payload{}, err
	}
	if p.Size != r.Size || p.Digest != r.PayloadSHA256 {
		return Payload{}, fmt.Errorf("anchor: the note is not the checkpoint of the record (size or digest differ)")
	}
	if r.Generation > MaxGeneration {
		return Payload{}, fmt.Errorf("anchor: record generation must be at most %d", MaxGeneration)
	}
	p.Generation = r.Generation
	return p, nil
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
// Submit call for this key started at submitStart (required: persist it before
// that call and never change it, see Payload.SubmitStart). The shape rules are
// those of ParsePayload.
func NewPayload(aggregateIDHex string, signedNote []byte, submitStart time.Time) (Payload, error) {
	if submitStart.IsZero() {
		return Payload{}, fmt.Errorf("anchor: the start of the first Submit call is required")
	}
	p, err := ParsePayload(aggregateIDHex, signedNote)
	if err != nil {
		return Payload{}, err
	}
	p.SubmitStart = submitStart.UTC().Truncate(time.Second)
	return p, nil
}

// ParsePayload checks the shape of a signed note and returns its payload
// without a SubmitStart (a zero one, which Check refuses): for a reader of
// stored notes, such as an independent verifier, that has no call to speak of.
// It applies one set of rules, that of the verifier (kingres-verify,
// signednote.Parse and checkpoint.Parse), so that nothing is anchored that the
// verifier would call malformed:
//
//   - the note: valid UTF-8; no control character other than newline; no line
//     longer than MaxNoteLineLen; a blank line (the last one) between the text
//     and the signatures; at least one and at most MaxSignatureLines signature
//     lines;
//   - a signature line: U+2014, a space, a key name, a space, standard base64.
//     The name has no Unicode white space, no plus sign and no control
//     character; the base64 is strict and canonical (padded, no trailing bits,
//     re-encoding gives the same text) and decodes to at least 4 bytes (the key
//     id); no two lines have the same name and key id;
//   - the text: three to 3+MaxExtensionLines non-empty lines: an origin by
//     ValidOrigin, a size in ASCII decimal without sign or leading zeros and
//     below TreeSizeLimit, a root as strict canonical base64 of 32 bytes (the
//     root of the empty tree, SHA-256 of nothing, when the size is 0), and up
//     to MaxExtensionLines extension lines (opaque to this package, as to the
//     verifier).
//
// This package is stricter than the verifier on purpose in two places: it
// refuses the byte 0x7f (DEL) anywhere in the note (the verifier refuses
// control characters below 0x20 only), and it refuses a note over
// MaxSignedNote (16 KiB, the verifier takes 64 KiB: a note of the ledger is far
// smaller, and what is stored under a key is bounded). Everything else is the
// verifier's rule; a corpus in testdata lists accepted and refused notes with
// the verdict of each rule set, and a test holds this parser to it. It does not
// verify a signature: that is the signer's and the verifier's job.
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
	if signedNote[len(signedNote)-1] != '\n' {
		return Payload{}, fmt.Errorf("anchor: signed note must end in a newline")
	}
	i := bytes.LastIndex(signedNote, []byte("\n\n"))
	if i < 0 {
		return Payload{}, fmt.Errorf("anchor: signed note has no blank line before its signatures")
	}
	text := signedNote[:i+1]
	sigs := signedNote[i+2:]
	if len(sigs) == 0 {
		return Payload{}, fmt.Errorf("anchor: signed note has no signature line")
	}
	for _, l := range strings.Split(string(signedNote), "\n") {
		if len(l) > MaxNoteLineLen {
			return Payload{}, fmt.Errorf("anchor: signed note has a line longer than %d bytes", MaxNoteLineLen)
		}
	}
	sigLines := strings.Split(strings.TrimSuffix(string(sigs), "\n"), "\n")
	if len(sigLines) > MaxSignatureLines {
		return Payload{}, fmt.Errorf("anchor: signed note has more than %d signature lines", MaxSignatureLines)
	}
	seen := map[string]bool{}
	for _, l := range sigLines {
		name, id, err := parseSigLine(l)
		if err != nil {
			return Payload{}, err
		}
		k := name + " " + id
		if seen[k] {
			return Payload{}, fmt.Errorf("anchor: signed note has two signature lines with the same key name and key id")
		}
		seen[k] = true
	}
	lines := strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
	if len(lines) < 3 || len(lines) > 3+MaxExtensionLines {
		return Payload{}, fmt.Errorf("anchor: note text must be three lines and at most %d extension lines", MaxExtensionLines)
	}
	for _, l := range lines {
		if l == "" {
			return Payload{}, fmt.Errorf("anchor: note text has an empty line")
		}
	}
	if !ValidOrigin(lines[0]) {
		return Payload{}, fmt.Errorf("anchor: origin is not valid: 1 to %d bytes of UTF-8 without white space, plus signs or control characters", MaxOriginLen)
	}
	sz, err := strconv.ParseUint(lines[1], 10, 64)
	if err != nil || strconv.FormatUint(sz, 10) != lines[1] || sz >= TreeSizeLimit {
		return Payload{}, fmt.Errorf("anchor: note size is not a canonical decimal below 2^62")
	}
	root, err := decodeStrictBase64(lines[2])
	if err != nil || len(root) != 32 {
		return Payload{}, fmt.Errorf("anchor: note root is not canonical base64 of 32 bytes")
	}
	if sz == 0 && sha256.Sum256(nil) != [32]byte(root) {
		return Payload{}, fmt.Errorf("anchor: a note of size 0 must carry the root of the empty tree")
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

// decodeStrictBase64 is the verifier's DecodeBase64: standard alphabet, padding,
// no non-zero trailing bits, no newline; the result re-encodes to the input.
func decodeStrictBase64(s string) ([]byte, error) {
	b, err := base64.StdEncoding.Strict().DecodeString(s)
	if err != nil || base64.StdEncoding.EncodeToString(b) != s {
		return nil, fmt.Errorf("anchor: base64 is not canonical")
	}
	return b, nil
}

// validKeyName is the verifier's ValidName (non-empty valid UTF-8, no Unicode
// white space, no plus sign, no control character), with DEL refused as well
// (the whole note refuses it above).
func validKeyName(s string) bool {
	if s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || r == '+' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// parseSigLine checks one signature line and returns its key name and key id
// (hex, from the first four decoded bytes).
func parseSigLine(line string) (name, keyID string, err error) {
	bad := fmt.Errorf("anchor: signed note has a malformed signature line")
	rest, ok := strings.CutPrefix(line, "\xe2\x80\x94 ")
	if !ok {
		return "", "", bad
	}
	name, b64, ok := strings.Cut(rest, " ")
	if !ok || b64 == "" || strings.Contains(b64, " ") || !validKeyName(name) {
		return "", "", bad
	}
	raw, derr := decodeStrictBase64(b64)
	if derr != nil || len(raw) < minSigBytes {
		return "", "", bad
	}
	return name, hex.EncodeToString(raw[:minSigBytes]), nil
}

// Check reports whether p is exactly what NewPayload builds from p.SignedNote
// and the aggregate id of p.ID: every field, the digest included. A zero
// Payload, a digest that is not SHA-256 of the note text and a text that is
// not the head of the signed note are all refused.
func (p Payload) Check() error {
	if p.SubmitStart.IsZero() {
		return fmt.Errorf("anchor: payload has no SubmitStart (use anchor.NewPayload)")
	}
	if p.Generation > MaxGeneration {
		return fmt.Errorf("anchor: payload generation must be at most %d", MaxGeneration)
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
	// NextCheck is when Upgrade should be called next (submitted and claimed
	// only).
	NextCheck *time.Time
	// The three counts below are set by Upgrade of an OpenTimestamps receipt
	// (zero for every other result), so that the caller can alert: a block
	// header source that contradicts claims of a calendar, or a calendar that
	// makes claims that a header source contradicts, is an operator matter.
	// ContradictedClaims is how many claims a block header contradicted in this
	// call (a claim contradicted again is counted again), ClaimsAside how many
	// contradicted claims are kept aside in the evidence after this call (they
	// are neither plausible nor anchored, and the calendar is asked again), and
	// ClaimsDropped how many claims were dropped for good in this call (the
	// header source contradicted them again after the confirmation interval).
	ContradictedClaims int
	ClaimsAside        int
	ClaimsDropped      int
	// ClaimsRepeated is how many claims that had been dropped for good (they are
	// remembered in OTSEvidence.Dropped) a calendar answered again in this call
	// and that were put aside at once, before they entered a fragment: they are
	// not counted as contradicted, aside or dropped again (correction 96). A
	// number above zero for round after round is a calendar that insists on a
	// claim that a header source contradicted; it is an operator matter.
	ClaimsRepeated int
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
