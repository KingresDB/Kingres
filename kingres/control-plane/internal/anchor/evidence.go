// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strconv"
	"strings"
	"time"
)

// EvidenceFormat is the value of the "format" member of an evidence file.
const EvidenceFormat = 1

// Limits on evidence.
const (
	// MaxEvidenceFile is the largest evidence file ParseEvidenceFile reads.
	MaxEvidenceFile = 4 << 20
	// MaxOTSCalendars is the most calendar fragments in one OTS evidence.
	MaxOTSCalendars = 16
	// MaxOTSContradictionsPerCalendar is the share of one calendar in the claims
	// kept aside (correction 106): a calendar over its share loses its own surplus
	// claims, never another calendar's claim, and a claim that a header contradicts
	// for the first time is never dropped because other calendars fill the list.
	MaxOTSContradictionsPerCalendar = 8
	// MaxOTSContradictions is the most contradicted claims kept aside in one OTS
	// evidence: the shares of all calendars together, so that the evidence stays
	// within its size limit.
	MaxOTSContradictions = MaxOTSCalendars * MaxOTSContradictionsPerCalendar
	// MaxOTSDroppedPerCalendar is the most dropped claims remembered for one
	// calendar (correction 106); when it is full the oldest of that calendar is
	// forgotten, and no other calendar's memory is touched.
	MaxOTSDroppedPerCalendar = 32
	// MaxOTSDropped is the most dropped claims remembered in one OTS evidence (16
	// calendars with their full share); when it is full the oldest is forgotten.
	MaxOTSDropped = MaxOTSCalendars * MaxOTSDroppedPerCalendar
	// MaxGeneration is the largest key generation of a payload (correction 95).
	MaxGeneration = 1000
	// MaxTokenDER is the largest TSA token (DER) stored in evidence.
	MaxTokenDER = 256 << 10
)

// RecordedByCluster is the value of "recorded_by" in WORM and TSA evidence:
// the fields were written by the cluster, not fetched by a verifier.
const RecordedByCluster = "cluster"

// TimeLayout is the only time format in evidence: RFC 3339, UTC, whole
// seconds, the letter Z.
const TimeLayout = "2006-01-02T15:04:05Z"

// FormatTime formats t in TimeLayout (UTC, truncated to whole seconds).
func FormatTime(t time.Time) string { return t.UTC().Format(TimeLayout) }

// ParseTime parses TimeLayout strictly.
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(TimeLayout, s)
	if err != nil || FormatTime(t) != s {
		return time.Time{}, fmt.Errorf("anchor: %q is not a UTC time with whole seconds", s)
	}
	return t, nil
}

// WORMEvidence is the evidence of a WORM anchor (design 6.3 step 5 and
// correction 61).
type WORMEvidence struct {
	Endpoint  string `json:"endpoint"` // "" for AWS
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	VersionID string `json:"version_id"` // the version that was read back; never empty
	ETag      string `json:"etag"`
	// ObjectSHA256 is the lower case hex SHA-256 of the object's bytes (the
	// signed note) that the cluster wrote and read back from VersionID, and
	// ContentLength their number: a verifier compares them with what it
	// fetches or holds.
	ObjectSHA256  string `json:"object_sha256"`
	ContentLength uint64 `json:"content_length"`
	LockMode      string `json:"lock_mode"`     // "GOVERNANCE" or "COMPLIANCE"
	RetainUntil   string `json:"retain_until"`  // TimeLayout
	LastModified  string `json:"last_modified"` // TimeLayout, the store's clock, bounded against the cluster's clock
	// SubmitStart is the start of the first Submit call for the key that the
	// caller stated (Payload.SubmitStart) and that was applied to the time of the
	// store: the earliest time accepted for an existing object was this less the
	// tolerance. An auditor reads the window the cluster accepted from it and
	// from last_modified.
	SubmitStart string `json:"submit_start"` // TimeLayout
	// Attempt is the attempt time that was used to bound the store's time
	// (correction 95): the value of the user metadata kingres-attempt of the
	// version, the start of the Submit call that wrote it (TimeLayout), and null
	// when the version carried none. With a value, the window that was applied
	// is [max(submit_start, attempt) less the tolerance, attempt plus the call
	// timeout plus the tolerance] (for a version this call wrote, the tolerance
	// around the clock of the call, with the value read back equal to the value
	// sent); with null it is [submit_start less the tolerance, now plus the
	// tolerance] and nothing narrower: the version was written by another
	// principal, or by a writer that kept no metadata.
	Attempt *string `json:"attempt"`
	// Generation is the generation of the key (Payload.Generation; 0 is the key
	// "<prefix>/<aggregate>/<size>.note", n is "<size>.g<n>.note").
	Generation uint32 `json:"generation"`
	// Found is false when the version was written by the Submit call that
	// produced this evidence, and true when that call found it (an earlier call
	// of this library that did not finish, or another principal wrote it).
	Found      bool   `json:"found"`
	RecordedBy string `json:"recorded_by"` // "cluster"
}

// TSAEvidence is the evidence of an RFC 3161 anchor (design 6.4 step 5).
type TSAEvidence struct {
	TSAURL      string `json:"tsa_url"`
	TokenDERB64 string `json:"token_der_b64"` // standard base64, padded, of the DER ContentInfo
	GenTime     string `json:"gen_time"`      // TimeLayout (sub-seconds of genTime are dropped)
	Serial      string `json:"serial"`        // decimal
	Policy      string `json:"policy"`        // dotted OID
	// SignerKeySHA256 is the lower case hex SHA-256 of the DER SubjectPublicKeyInfo
	// of the signing certificate (the pin; correction 97).
	SignerKeySHA256 string `json:"signer_key_sha256"`
	RecordedBy      string `json:"recorded_by"` // "cluster"
}

// OTSCalendar is one calendar's fragment.
type OTSCalendar struct {
	URI         string `json:"uri"`
	FragmentB64 string `json:"fragment_b64"` // standard base64, padded, of the serialized timestamp
}

// Values of OTSBitcoin.Status.
const (
	// BitcoinClaimed: a calendar claims the attestation; it passed every check
	// that needs no block header; nothing checked it against the chain.
	BitcoinClaimed = "claimed"
	// BitcoinHeaderChecked: the attestation was checked against the 80-byte
	// block header carried in the member "header" (merkle root, proof of
	// work, time). The header's source is the operator's, not the chain's
	// consensus: an auditor still checks the header against a node.
	BitcoinHeaderChecked = "header_checked"
)

// MaxBitcoinHeight is the largest block height accepted anywhere (the parser
// of package ots and OTSEvidence.Validate use the same limit).
const MaxBitcoinHeight = 1<<31 - 1

// OTSBitcoin summarises the Bitcoin attestation of an anchor. It names ONE
// attestation, the one with the lowest height among those that passed the
// checks; the fragments may hold others, and a verifier checks every Bitcoin
// attestation of every fragment, not only this one.
type OTSBitcoin struct {
	Height      uint64 `json:"height"`
	CalendarURI string `json:"calendar_uri"` // the calendar whose fragment holds it
	// MerkleRoot is the lower case hex of the 32-byte message that the
	// attestation sits on (what the operations lead to from the digest), in
	// the byte order in which it sits in a block header (bytes 36 to 68).
	MerkleRoot string `json:"merkle_root"`
	Status     string `json:"status"` // BitcoinClaimed or BitcoinHeaderChecked
	// Header is the 160 hex digits of the block header that was checked, and
	// "" when Status is BitcoinClaimed.
	Header string `json:"header"`
}

// OTSContradiction is a claim of a calendar that a block header contradicted
// and that is kept aside (correction 90): it stays in the fragment of its
// calendar, is neither plausible nor anchored, and the calendar keeps being
// asked in later rounds while its fragment holds a pending attestation (a
// target with a header source keeps it after a merge). It is dropped for good
// only when the header source contradicts it again in a round at least the
// confirmation interval after "at". The claim is named by calendar, height and
// merkle root.
type OTSContradiction struct {
	CalendarURI string `json:"calendar_uri"` // a calendar of "calendars"
	Height      uint64 `json:"height"`
	MerkleRoot  string `json:"merkle_root"` // hex of the message the claim sits on
	// Header is the 160 hex digits of the block header that contradicted the
	// claim (the first one).
	Header string `json:"header"`
	// At is the cluster's clock at the first contradiction (TimeLayout).
	At string `json:"at"`
	// Reason is why the header contradicted the claim (printable ASCII).
	Reason string `json:"reason"`
}

// OTSDropped is a claim that was dropped for good (the header source
// contradicted it again at least the confirmation interval after the first
// time) and is remembered, so that the same claim answered again by a calendar
// is put aside at once and not counted as a new one (correction 96). It is named
// as a contradicted claim is: by calendar, height and merkle root. The calendar
// need not be in "calendars" any more: the fragment that held the claim may be
// gone with it.
type OTSDropped struct {
	CalendarURI string `json:"calendar_uri"`
	Height      uint64 `json:"height"`
	MerkleRoot  string `json:"merkle_root"` // hex of the message the claim sat on
	// At is the cluster's clock when the claim was dropped (TimeLayout).
	At string `json:"at"`
}

// OTSEvidence is the evidence of an OpenTimestamps anchor (design 6.5 step
// 2 and corrections 60 and 61).
type OTSEvidence struct {
	Digest string `json:"digest"` // hex of the 32-byte digest
	// SubmittedAt is the cluster's clock when the digest was first accepted
	// by enough calendars (TimeLayout). It bounds the heights that can
	// attest the digest: a block older than this cannot.
	SubmittedAt string        `json:"submitted_at"`
	Calendars   []OTSCalendar `json:"calendars"`
	Bitcoin     *OTSBitcoin   `json:"bitcoin"` // null until a Bitcoin attestation exists
	// Contradicted lists the claims kept aside (an empty array when none).
	Contradicted []OTSContradiction `json:"contradicted"`
	// Dropped lists the claims dropped for good that are still remembered (an
	// empty array when none), at most MaxOTSDroppedPerCalendar per calendar and
	// MaxOTSDropped in all: when a calendar's share is full the oldest entry of
	// that calendar is forgotten (correction 106).
	Dropped []OTSDropped `json:"dropped"`
}

func isLowerHex(s string, n int) bool {
	if len(s) != 2*n {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func printableASCII(s string, max int) bool {
	if len(s) > max {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// Validate checks the evidence.
func (e WORMEvidence) Validate() error {
	if !printableASCII(e.Endpoint, 2048) || !printableASCII(e.Bucket, 255) || e.Bucket == "" ||
		!printableASCII(e.Key, 1024) || e.Key == "" || !printableASCII(e.VersionID, 1024) || e.VersionID == "" ||
		!printableASCII(e.ETag, 256) {
		return errors.New("anchor: worm evidence has a missing or malformed text field")
	}
	if !isLowerHex(e.ObjectSHA256, 32) {
		return errors.New("anchor: worm evidence object_sha256 must be 64 lower case hex digits")
	}
	if e.LockMode != "GOVERNANCE" && e.LockMode != "COMPLIANCE" {
		return errors.New("anchor: worm evidence lock_mode must be GOVERNANCE or COMPLIANCE")
	}
	if _, err := ParseTime(e.RetainUntil); err != nil {
		return err
	}
	if _, err := ParseTime(e.LastModified); err != nil {
		return err
	}
	if _, err := ParseTime(e.SubmitStart); err != nil {
		return err
	}
	if e.Attempt != nil {
		if _, err := ParseTime(*e.Attempt); err != nil {
			return err
		}
	}
	if e.Generation > MaxGeneration {
		return errors.New("anchor: worm evidence generation is too large")
	}
	if e.RecordedBy != RecordedByCluster {
		return errors.New("anchor: worm evidence recorded_by must be cluster")
	}
	return nil
}

// Validate checks the evidence (shape only; the token is verified by package tsa).
func (e TSAEvidence) Validate() error {
	if !printableASCII(e.TSAURL, 2048) || e.TSAURL == "" || !printableASCII(e.Policy, 256) || e.Policy == "" {
		return errors.New("anchor: tsa evidence has a missing or malformed text field")
	}
	if !isLowerHex(e.SignerKeySHA256, 32) {
		return errors.New("anchor: tsa evidence signer_key_sha256 must be 64 lower case hex digits")
	}
	tok, err := base64.StdEncoding.DecodeString(e.TokenDERB64)
	if err != nil || len(tok) == 0 || len(tok) > MaxTokenDER || base64.StdEncoding.EncodeToString(tok) != e.TokenDERB64 {
		return errors.New("anchor: tsa evidence token_der_b64 is malformed or too large")
	}
	if _, err := ParseTime(e.GenTime); err != nil {
		return err
	}
	n, ok := new(big.Int).SetString(e.Serial, 10)
	if !ok || n.Sign() < 0 || n.String() != e.Serial {
		return errors.New("anchor: tsa evidence serial must be canonical decimal")
	}
	if e.RecordedBy != RecordedByCluster {
		return errors.New("anchor: tsa evidence recorded_by must be cluster")
	}
	return nil
}

// Validate checks the evidence (shape only; fragments are parsed by package ots).
func (e OTSEvidence) Validate() error {
	if d, err := hex.DecodeString(e.Digest); err != nil || len(d) != 32 || strings.ToLower(e.Digest) != e.Digest {
		return errors.New("anchor: ots evidence digest must be 64 lower case hex digits")
	}
	if len(e.Calendars) > MaxOTSCalendars {
		return errors.New("anchor: ots evidence has too many calendars")
	}
	seen := map[string]bool{}
	for _, c := range e.Calendars {
		if !printableASCII(c.URI, 1000) || c.URI == "" || seen[c.URI] {
			return errors.New("anchor: ots evidence has a malformed or repeated calendar uri")
		}
		seen[c.URI] = true
		if f, err := base64.StdEncoding.DecodeString(c.FragmentB64); err != nil || len(f) == 0 || base64.StdEncoding.EncodeToString(f) != c.FragmentB64 {
			return errors.New("anchor: ots evidence has a malformed fragment")
		}
	}
	if _, err := ParseTime(e.SubmittedAt); err != nil {
		return err
	}
	if b := e.Bitcoin; b != nil {
		if !seen[b.CalendarURI] || b.Height > MaxBitcoinHeight {
			return errors.New("anchor: ots evidence bitcoin names no known calendar or an impossible height")
		}
		if !isLowerHex(b.MerkleRoot, 32) {
			return errors.New("anchor: ots evidence bitcoin merkle_root must be 64 lower case hex digits")
		}
		switch b.Status {
		case BitcoinClaimed:
			if b.Header != "" {
				return errors.New("anchor: ots evidence bitcoin header must be empty for a claim")
			}
		case BitcoinHeaderChecked:
			if !isLowerHex(b.Header, 80) || b.Header[72:136] != b.MerkleRoot {
				return errors.New("anchor: ots evidence bitcoin header must be 160 hex digits holding the merkle root")
			}
		default:
			return errors.New("anchor: ots evidence bitcoin status must be claimed or header_checked")
		}
	}
	if len(e.Contradicted) > MaxOTSContradictions {
		return errors.New("anchor: ots evidence has too many contradicted claims")
	}
	seenC := map[string]bool{}
	perCal := map[string]int{}
	for _, c := range e.Contradicted {
		if perCal[c.CalendarURI]++; perCal[c.CalendarURI] > MaxOTSContradictionsPerCalendar {
			return errors.New("anchor: ots evidence has too many contradicted claims of one calendar")
		}
		if !seen[c.CalendarURI] || c.Height > MaxBitcoinHeight || !isLowerHex(c.MerkleRoot, 32) || !isLowerHex(c.Header, 80) ||
			!printableASCII(c.Reason, 300) {
			return errors.New("anchor: ots evidence has a malformed contradicted claim")
		}
		if _, err := ParseTime(c.At); err != nil {
			return err
		}
		k := c.CalendarURI + "|" + strconv.FormatUint(c.Height, 10) + "|" + c.MerkleRoot
		if seenC[k] {
			return errors.New("anchor: ots evidence repeats a contradicted claim")
		}
		seenC[k] = true
	}
	if len(e.Dropped) > MaxOTSDropped {
		return errors.New("anchor: ots evidence has too many dropped claims")
	}
	seenD := map[string]bool{}
	perDrop := map[string]int{}
	for _, d := range e.Dropped {
		if perDrop[d.CalendarURI]++; perDrop[d.CalendarURI] > MaxOTSDroppedPerCalendar {
			return errors.New("anchor: ots evidence remembers too many dropped claims of one calendar")
		}
		if !printableASCII(d.CalendarURI, 1000) || d.CalendarURI == "" || d.Height > MaxBitcoinHeight || !isLowerHex(d.MerkleRoot, 32) {
			return errors.New("anchor: ots evidence has a malformed dropped claim")
		}
		if _, err := ParseTime(d.At); err != nil {
			return err
		}
		k := d.CalendarURI + "|" + strconv.FormatUint(d.Height, 10) + "|" + d.MerkleRoot
		if seenD[k] {
			return errors.New("anchor: ots evidence repeats a dropped claim")
		}
		seenD[k] = true
	}
	return nil
}

func canonicalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// EncodeEvidence validates and encodes one of WORMEvidence, TSAEvidence,
// OTSEvidence (values or pointers) in the canonical encoding: compact JSON,
// members in the order of the type's declaration, no trailing newline,
// strings unescaped except as JSON requires. The same value always gives the
// same bytes.
func EncodeEvidence(v any) (json.RawMessage, error) {
	switch e := v.(type) {
	case WORMEvidence:
		if err := e.Validate(); err != nil {
			return nil, err
		}
	case *WORMEvidence:
		if e == nil {
			return nil, errors.New("anchor: nil evidence")
		}
		return EncodeEvidence(*e)
	case TSAEvidence:
		if err := e.Validate(); err != nil {
			return nil, err
		}
	case *TSAEvidence:
		if e == nil {
			return nil, errors.New("anchor: nil evidence")
		}
		return EncodeEvidence(*e)
	case OTSEvidence:
		if err := e.Validate(); err != nil {
			return nil, err
		}
		if e.Calendars == nil {
			e.Calendars = []OTSCalendar{}
		}
		if e.Contradicted == nil {
			e.Contradicted = []OTSContradiction{}
		}
		if e.Dropped == nil {
			e.Dropped = []OTSDropped{}
		}
		v = e
	case *OTSEvidence:
		if e == nil {
			return nil, errors.New("anchor: nil evidence")
		}
		return EncodeEvidence(*e)
	default:
		return nil, fmt.Errorf("anchor: cannot encode evidence of type %T", v)
	}
	return canonicalJSON(v)
}

func strictDecode(raw []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("anchor: evidence: %w", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("anchor: evidence: trailing data")
	}
	return nil
}

// requireCanonical refuses raw unless it is, byte for byte, what
// canonicalJSON writes for the parsed value v: member names in the exact case
// and order, no repeated member, no white space, no escape that the encoder
// would not write, no null where a value is required. A reader that wrote
// from the text of doc.go rejects the same input.
func requireCanonical(raw []byte, v any) error {
	canon, err := canonicalJSON(v)
	if err != nil || !bytes.Equal(canon, raw) {
		return errors.New("anchor: evidence is not in the canonical encoding")
	}
	return nil
}

func parseWORM(raw []byte, strict bool) (WORMEvidence, error) {
	var e WORMEvidence
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	if strict {
		return e, requireCanonical(raw, e)
	}
	return e, nil
}

func parseTSA(raw []byte, strict bool) (TSAEvidence, error) {
	var e TSAEvidence
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	if strict {
		return e, requireCanonical(raw, e)
	}
	return e, nil
}

func parseOTS(raw []byte, strict bool) (OTSEvidence, error) {
	var e OTSEvidence
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	if e.Calendars == nil || e.Contradicted == nil || e.Dropped == nil {
		return e, errors.New("anchor: ots evidence calendars, contradicted and dropped must be arrays")
	}
	if err := e.Validate(); err != nil {
		return e, err
	}
	if strict {
		return e, requireCanonical(raw, e)
	}
	return e, nil
}

// ParseWORMEvidence parses and validates WORM evidence. The input must be in
// the canonical encoding (what EncodeEvidence writes) byte for byte: unknown
// members, a member name in another case or spelled with an escape, a
// repeated member, white space and any other variation are rejected.
func ParseWORMEvidence(raw []byte) (WORMEvidence, error) { return parseWORM(raw, true) }

// ParseTSAEvidence parses and validates TSA evidence (canonical input only,
// see ParseWORMEvidence).
func ParseTSAEvidence(raw []byte) (TSAEvidence, error) { return parseTSA(raw, true) }

// ParseOTSEvidence parses and validates OpenTimestamps evidence (canonical
// input only, see ParseWORMEvidence).
func ParseOTSEvidence(raw []byte) (OTSEvidence, error) { return parseOTS(raw, true) }

// EvidenceFile is the public file a/<aggregate_id>/anchors/<size>/<target id>.json
// of the publication (design 6.2).
type EvidenceFile struct {
	Kind          string
	Origin        string
	Size          uint64
	PayloadSHA256 string // hex
	CreatedAt     time.Time
	// Evidence is the canonical evidence object (from EncodeEvidence).
	Evidence json.RawMessage
}

type evidenceFileJSON struct {
	Format        int             `json:"format"`
	Kind          string          `json:"kind"`
	Origin        string          `json:"origin"`
	Size          uint64          `json:"size"`
	PayloadSHA256 string          `json:"payload_sha256"`
	CreatedAt     string          `json:"created_at"`
	Evidence      json.RawMessage `json:"evidence"`
}

// EncodeEvidenceFile validates f and encodes it canonically (members in the
// order format, kind, origin, size, payload_sha256, created_at, evidence;
// compact; no trailing newline). The evidence member is re-encoded from its
// parsed form, so the bytes are canonical even if f.Evidence was not.
func EncodeEvidenceFile(f EvidenceFile) ([]byte, error) {
	if !ValidOrigin(f.Origin) {
		return nil, errors.New("anchor: evidence file origin is missing or malformed")
	}
	if d, err := hex.DecodeString(f.PayloadSHA256); err != nil || len(d) != 32 || strings.ToLower(f.PayloadSHA256) != f.PayloadSHA256 {
		return nil, errors.New("anchor: evidence file payload_sha256 must be 64 lower case hex digits")
	}
	ev, err := canonicalEvidence(f.Kind, f.Evidence)
	if err != nil {
		return nil, err
	}
	return canonicalJSON(evidenceFileJSON{
		Format: EvidenceFormat, Kind: f.Kind, Origin: f.Origin, Size: f.Size,
		PayloadSHA256: f.PayloadSHA256, CreatedAt: FormatTime(f.CreatedAt), Evidence: ev,
	})
}

// CanonicalEvidence reads evidence of the given kind in any JSON spelling (a
// jsonb column of PostgreSQL returns the members in another order and with
// white space), validates it, and returns the canonical encoding that the
// parsers and the evidence file require. Unknown members, trailing data and
// values that break the rules are still refused.
func CanonicalEvidence(kind string, raw []byte) (json.RawMessage, error) {
	return canonicalEvidence(kind, raw)
}

func canonicalEvidence(kind string, raw []byte) (json.RawMessage, error) {
	switch kind {
	case KindWORM:
		e, err := parseWORM(raw, false)
		if err != nil {
			return nil, err
		}
		return EncodeEvidence(e)
	case KindTSA:
		e, err := parseTSA(raw, false)
		if err != nil {
			return nil, err
		}
		return EncodeEvidence(e)
	case KindOTS:
		e, err := parseOTS(raw, false)
		if err != nil {
			return nil, err
		}
		return EncodeEvidence(e)
	}
	return nil, fmt.Errorf("anchor: unknown evidence kind %q", kind)
}

// ParseEvidenceFile parses an evidence file: at most MaxEvidenceFile bytes,
// format 1, a known kind, no unknown member, the evidence valid for the kind.
func ParseEvidenceFile(b []byte) (EvidenceFile, error) {
	var f EvidenceFile
	if len(b) > MaxEvidenceFile {
		return f, errors.New("anchor: evidence file is too large")
	}
	var j evidenceFileJSON
	if err := strictDecode(b, &j); err != nil {
		return f, err
	}
	if j.Format != EvidenceFormat {
		return f, fmt.Errorf("anchor: unsupported evidence format %d", j.Format)
	}
	t, err := ParseTime(j.CreatedAt)
	if err != nil {
		return f, err
	}
	f = EvidenceFile{Kind: j.Kind, Origin: j.Origin, Size: j.Size, PayloadSHA256: j.PayloadSHA256, CreatedAt: t, Evidence: j.Evidence}
	// reject anything EncodeEvidenceFile would reject (kind, origin, digest,
	// the evidence for the kind) and anything that is not exactly what it
	// writes: the file must be canonical byte for byte
	canon, err := EncodeEvidenceFile(f)
	if err != nil {
		return EvidenceFile{}, err
	}
	if !bytes.Equal(canon, b) {
		return EvidenceFile{}, errors.New("anchor: evidence file is not in the canonical encoding")
	}
	return f, nil
}
