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

// WORMEvidence is the evidence of a WORM anchor (design 6.3 step 5).
type WORMEvidence struct {
	Endpoint     string `json:"endpoint"` // "" for AWS
	Bucket       string `json:"bucket"`
	Key          string `json:"key"`
	VersionID    string `json:"version_id"`
	ETag         string `json:"etag"`
	LockMode     string `json:"lock_mode"`     // "GOVERNANCE" or "COMPLIANCE"
	RetainUntil  string `json:"retain_until"`  // TimeLayout
	LastModified string `json:"last_modified"` // TimeLayout, the store's clock
	RecordedBy   string `json:"recorded_by"`   // "cluster"
}

// TSAEvidence is the evidence of an RFC 3161 anchor (design 6.4 step 5).
type TSAEvidence struct {
	TSAURL      string `json:"tsa_url"`
	TokenDERB64 string `json:"token_der_b64"` // standard base64, padded, of the DER ContentInfo
	GenTime     string `json:"gen_time"`      // TimeLayout (sub-seconds of genTime are dropped)
	Serial      string `json:"serial"`        // decimal
	Policy      string `json:"policy"`        // dotted OID
	RecordedBy  string `json:"recorded_by"`   // "cluster"
}

// OTSCalendar is one calendar's fragment.
type OTSCalendar struct {
	URI         string `json:"uri"`
	FragmentB64 string `json:"fragment_b64"` // standard base64, padded, of the serialized timestamp
}

// OTSBitcoin states which fragment carries a Bitcoin attestation.
type OTSBitcoin struct {
	Height      uint64 `json:"height"`
	CalendarURI string `json:"calendar_uri"`
}

// OTSEvidence is the evidence of an OpenTimestamps anchor (design 6.5 step 2).
type OTSEvidence struct {
	Digest    string        `json:"digest"` // hex of the 32-byte digest
	Calendars []OTSCalendar `json:"calendars"`
	Bitcoin   *OTSBitcoin   `json:"bitcoin"` // null until a Bitcoin attestation exists
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
		!printableASCII(e.Key, 1024) || e.Key == "" || !printableASCII(e.VersionID, 1024) ||
		!printableASCII(e.ETag, 256) {
		return errors.New("anchor: worm evidence has a missing or malformed text field")
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
	tok, err := base64.StdEncoding.DecodeString(e.TokenDERB64)
	if err != nil || len(tok) == 0 || len(tok) > MaxTokenDER {
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
		if f, err := base64.StdEncoding.DecodeString(c.FragmentB64); err != nil || len(f) == 0 {
			return errors.New("anchor: ots evidence has a malformed fragment")
		}
	}
	if e.Bitcoin != nil && (!seen[e.Bitcoin.CalendarURI] || e.Bitcoin.Height > 1<<31) {
		return errors.New("anchor: ots evidence bitcoin names no known calendar or an impossible height")
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

// ParseWORMEvidence parses and validates WORM evidence. Unknown members are rejected.
func ParseWORMEvidence(raw []byte) (WORMEvidence, error) {
	var e WORMEvidence
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	return e, e.Validate()
}

// ParseTSAEvidence parses and validates TSA evidence.
func ParseTSAEvidence(raw []byte) (TSAEvidence, error) {
	var e TSAEvidence
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	return e, e.Validate()
}

// ParseOTSEvidence parses and validates OpenTimestamps evidence.
func ParseOTSEvidence(raw []byte) (OTSEvidence, error) {
	var e OTSEvidence
	if err := strictDecode(raw, &e); err != nil {
		return e, err
	}
	return e, e.Validate()
}

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

func checkEvidenceKind(kind string, raw []byte) error {
	var err error
	switch kind {
	case KindWORM:
		_, err = ParseWORMEvidence(raw)
	case KindTSA:
		_, err = ParseTSAEvidence(raw)
	case KindOTS:
		_, err = ParseOTSEvidence(raw)
	default:
		err = fmt.Errorf("anchor: unknown evidence kind %q", kind)
	}
	return err
}

// EncodeEvidenceFile validates f and encodes it canonically (members in the
// order format, kind, origin, size, payload_sha256, created_at, evidence;
// compact; no trailing newline). The evidence member is re-encoded from its
// parsed form, so the bytes are canonical even if f.Evidence was not.
func EncodeEvidenceFile(f EvidenceFile) ([]byte, error) {
	if !printableASCII(f.Origin, MaxOriginLen) || f.Origin == "" {
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

func canonicalEvidence(kind string, raw []byte) (json.RawMessage, error) {
	switch kind {
	case KindWORM:
		e, err := ParseWORMEvidence(raw)
		if err != nil {
			return nil, err
		}
		return EncodeEvidence(e)
	case KindTSA:
		e, err := ParseTSAEvidence(raw)
		if err != nil {
			return nil, err
		}
		return EncodeEvidence(e)
	case KindOTS:
		e, err := ParseOTSEvidence(raw)
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
	if err := checkEvidenceKind(j.Kind, j.Evidence); err != nil {
		return f, err
	}
	t, err := ParseTime(j.CreatedAt)
	if err != nil {
		return f, err
	}
	f = EvidenceFile{Kind: j.Kind, Origin: j.Origin, Size: j.Size, PayloadSHA256: j.PayloadSHA256, CreatedAt: t, Evidence: j.Evidence}
	// reject anything EncodeEvidenceFile would reject (origin, digest)
	if _, err := EncodeEvidenceFile(f); err != nil {
		return EvidenceFile{}, err
	}
	return f, nil
}
