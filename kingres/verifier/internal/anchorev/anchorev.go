// SPDX-License-Identifier: AGPL-3.0-only

// Package anchorev reads the evidence file of an anchor, a/<aggregate id>/anchors/<size>/<target id>.json
// (ledger-phase7.md 6.2 and corrections 179 to 186; the formats are the ones of the documentation of the
// publisher's anchor library, doc.go "Evidence: the formats"). It is written from that text and shares no code
// with the publisher.
//
// The file is the cluster's CLAIM about what it anchored: every member of it is read strictly (an unknown
// member, a missing one, a value of the wrong type or shape, a time that is not whole seconds in UTC ending in
// Z, upper case hexadecimal or non canonical base64 is refused) and nothing in it is believed by this
// package: the checks are made by the packages that read the token, the proof and the object, from the
// auditor's own inputs. The order of the members and the white space are not judged (only values are): a
// file the publisher would write differently is read the same way, and nothing depends on it.
package anchorev

import (
	"fmt"
	"regexp"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/jsonx"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
)

// Limits.
const (
	// MaxFileSize is the largest evidence file read: sixteen calendar fragments of 64 KiB in base64 and some more.
	MaxFileSize = 4 << 20
	// MaxToken is the largest time-stamp token (DER) read.
	MaxToken = 64 << 10
	// MaxFragment is the largest OpenTimestamps fragment read.
	MaxFragment = 64 << 10
	// MaxCalendars is the most calendars of one record.
	MaxCalendars = 16
	maxList      = 4096
)

// Kinds.
const (
	KindWORM = "worm"
	KindTSA  = "tsa"
	KindOTS  = "ots"
)

// File is the evidence file.
type File struct {
	Kind          string
	Origin        string
	Size          uint64
	PayloadSHA256 [32]byte
	CreatedAt     time.Time
	WORM          *WORM
	TSA           *TSA
	OTS           *OTS
}

// WORM is the evidence of an object under Object Lock.
type WORM struct {
	Endpoint, Bucket, Key, VersionID, ETag string
	ObjectSHA256                           [32]byte
	ContentLength                          uint64
	LockMode                               string // GOVERNANCE or COMPLIANCE
	RetainUntil, LastModified, SubmitStart time.Time
	Attempt                                *time.Time
	Generation                             uint64
	Found                                  bool
}

// TSA is the evidence of an RFC 3161 time-stamp token.
type TSA struct {
	URL             string
	Token           []byte // DER of the TimeStampToken (the CMS ContentInfo)
	GenTime         time.Time
	Serial          string
	Policy          string
	SignerKeySHA256 [32]byte
}

// Calendar is one OpenTimestamps fragment.
type Calendar struct {
	URI      string
	Fragment []byte
}

// Claim is a Bitcoin attestation the cluster names.
type Claim struct {
	Height      uint64
	CalendarURI string
	MerkleRoot  [32]byte
	Status      string // "claimed" or "header_checked"
	Header      []byte // 80 bytes, or nil
}

// OTS is the evidence of an OpenTimestamps record.
type OTS struct {
	Digest      [32]byte
	SubmittedAt time.Time
	Calendars   []Calendar
	Bitcoin     *Claim
	// Contradicted and Dropped are the numbers of claims the cluster set aside, and dropped for good, after a header
	// of its own source contradicted them.
	Contradicted, Dropped int
}

var (
	hex64RE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
	hex160RE = regexp.MustCompile(`^[0-9a-f]{160}$`)
	decRE    = regexp.MustCompile(`^(0|[1-9][0-9]{0,79})$`)
	oidRE    = regexp.MustCompile(`^[0-2](\.(0|[1-9][0-9]{0,18})){1,31}$`)
)

func hexBytes(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := range b {
		b[i] = unhex(s[2*i])<<4 | unhex(s[2*i+1])
	}
	return b
}

func unhex(c byte) byte {
	if c >= 'a' {
		return c - 'a' + 10
	}
	return c - '0'
}

func hex32(o jsonx.Object, name string) ([32]byte, error) {
	var out [32]byte
	s, err := o.String(name)
	if err != nil {
		return out, err
	}
	if !hex64RE.MatchString(s) {
		return out, fmt.Errorf("%s.%s is not 64 lower case hexadecimal digits", o.Path(), name)
	}
	copy(out[:], hexBytes(s))
	return out, nil
}

// timeZ reads a time of the form 2006-01-02T15:04:05Z.
func timeZ(v jsonx.Value) (time.Time, error) {
	s, err := v.String()
	if err != nil {
		return time.Time{}, err
	}
	t, err := time.Parse("2006-01-02T15:04:05Z", s)
	if err != nil || len(s) != 20 {
		return time.Time{}, fmt.Errorf("%s: %q is not a time of the form 2006-01-02T15:04:05Z (whole seconds, UTC)", v.Path(), s)
	}
	return t, nil
}

func timeMember(o jsonx.Object, name string) (time.Time, error) {
	v, err := o.Field(name)
	if err != nil {
		return time.Time{}, err
	}
	return timeZ(v)
}

func printable(o jsonx.Object, name string, max int, empty bool) (string, error) {
	s, err := o.String(name)
	if err != nil {
		return "", err
	}
	if (s == "" && !empty) || len(s) > max {
		return "", fmt.Errorf("%s.%s has %d bytes, expected 1 to %d", o.Path(), name, len(s), max)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "", fmt.Errorf("%s.%s is not printable ASCII", o.Path(), name)
		}
	}
	return s, nil
}

func b64(o jsonx.Object, name string, max int) ([]byte, error) {
	s, err := o.String(name)
	if err != nil {
		return nil, err
	}
	b, err := signednote.DecodeBase64(s)
	if err != nil || len(b) == 0 || len(b) > max {
		return nil, fmt.Errorf("%s.%s is not canonical base64 of 1 to %d bytes", o.Path(), name, max)
	}
	return b, nil
}

// Parse reads an evidence file.
func Parse(data []byte) (*File, error) {
	if len(data) > MaxFileSize {
		return nil, fmt.Errorf("the evidence file has %d bytes, expected at most %d", len(data), MaxFileSize)
	}
	lim := jsonx.DefaultLimits
	lim.MaxValues = 1 << 16
	v, err := jsonx.ParseLimits(data, lim)
	if err != nil {
		return nil, err
	}
	o, err := v.Object()
	if err != nil {
		return nil, err
	}
	if err := o.OnlyKeys("format", "kind", "origin", "size", "payload_sha256", "created_at", "evidence"); err != nil {
		return nil, err
	}
	if _, err := o.Int64("format", 1, 1); err != nil {
		return nil, fmt.Errorf("%w (this program reads evidence of format 1 only)", err)
	}
	f := &File{}
	if f.Kind, err = o.String("kind"); err != nil {
		return nil, err
	}
	if f.Origin, err = printable(o, "origin", 255, false); err != nil {
		return nil, err
	}
	n, err := o.Int64("size", 0, 1<<62-1)
	if err != nil {
		return nil, err
	}
	f.Size = uint64(n) // #nosec G115 -- range checked
	if f.PayloadSHA256, err = hex32(o, "payload_sha256"); err != nil {
		return nil, err
	}
	if f.CreatedAt, err = timeMember(o, "created_at"); err != nil {
		return nil, err
	}
	ev, err := o.Object("evidence")
	if err != nil {
		return nil, err
	}
	switch f.Kind {
	case KindWORM:
		f.WORM, err = parseWORM(ev)
	case KindTSA:
		f.TSA, err = parseTSA(ev)
	case KindOTS:
		f.OTS, err = parseOTS(ev)
	default:
		err = fmt.Errorf("%s.kind is %q, expected worm, tsa or ots", o.Path(), f.Kind)
	}
	if err != nil {
		return nil, err
	}
	return f, nil
}

func recordedBy(o jsonx.Object) error {
	s, err := o.String("recorded_by")
	if err != nil {
		return err
	}
	if s != "cluster" {
		return fmt.Errorf("%s.recorded_by is %q, expected \"cluster\"", o.Path(), s)
	}
	return nil
}

func parseWORM(o jsonx.Object) (*WORM, error) {
	if err := o.OnlyKeys("endpoint", "bucket", "key", "version_id", "etag", "object_sha256", "content_length", "lock_mode", "retain_until", "last_modified", "submit_start", "attempt", "generation", "found", "recorded_by"); err != nil {
		return nil, err
	}
	w := &WORM{}
	var err error
	if w.Endpoint, err = printable(o, "endpoint", 1024, true); err != nil {
		return nil, err
	}
	if w.Bucket, err = printable(o, "bucket", 255, false); err != nil {
		return nil, err
	}
	if w.Key, err = printable(o, "key", 1024, false); err != nil {
		return nil, err
	}
	if w.VersionID, err = printable(o, "version_id", 1024, false); err != nil {
		return nil, err
	}
	if w.ETag, err = printable(o, "etag", 256, true); err != nil {
		return nil, err
	}
	if w.ObjectSHA256, err = hex32(o, "object_sha256"); err != nil {
		return nil, err
	}
	n, err := o.Int64("content_length", 1, 1<<20)
	if err != nil {
		return nil, err
	}
	w.ContentLength = uint64(n) // #nosec G115 -- range checked
	if w.LockMode, err = o.String("lock_mode"); err != nil {
		return nil, err
	}
	if w.LockMode != "GOVERNANCE" && w.LockMode != "COMPLIANCE" {
		return nil, fmt.Errorf("%s.lock_mode is %q, expected GOVERNANCE or COMPLIANCE", o.Path(), w.LockMode)
	}
	if w.RetainUntil, err = timeMember(o, "retain_until"); err != nil {
		return nil, err
	}
	if w.LastModified, err = timeMember(o, "last_modified"); err != nil {
		return nil, err
	}
	if w.SubmitStart, err = timeMember(o, "submit_start"); err != nil {
		return nil, err
	}
	av, err := o.Field("attempt")
	if err != nil {
		return nil, err
	}
	if av.Kind() != jsonx.KindNull {
		t, err := timeZ(av)
		if err != nil {
			return nil, err
		}
		w.Attempt = &t
	}
	g, err := o.Int64("generation", 0, 1<<31-1)
	if err != nil {
		return nil, err
	}
	w.Generation = uint64(g) // #nosec G115 -- range checked
	fv, err := o.Field("found")
	if err != nil {
		return nil, err
	}
	if w.Found, err = fv.Bool(); err != nil {
		return nil, err
	}
	return w, recordedBy(o)
}

func parseTSA(o jsonx.Object) (*TSA, error) {
	if err := o.OnlyKeys("tsa_url", "token_der_b64", "gen_time", "serial", "policy", "signer_key_sha256", "recorded_by"); err != nil {
		return nil, err
	}
	t := &TSA{}
	var err error
	if t.URL, err = printable(o, "tsa_url", 1024, false); err != nil {
		return nil, err
	}
	if t.Token, err = b64(o, "token_der_b64", MaxToken); err != nil {
		return nil, err
	}
	if t.GenTime, err = timeMember(o, "gen_time"); err != nil {
		return nil, err
	}
	if t.Serial, err = o.String("serial"); err != nil || !decRE.MatchString(t.Serial) {
		return nil, fmt.Errorf("%s.serial is not a decimal number", o.Path())
	}
	if t.Policy, err = o.String("policy"); err != nil || !oidRE.MatchString(t.Policy) {
		return nil, fmt.Errorf("%s.policy is not a dotted OID", o.Path())
	}
	if t.SignerKeySHA256, err = hex32(o, "signer_key_sha256"); err != nil {
		return nil, err
	}
	return t, recordedBy(o)
}

func parseOTS(o jsonx.Object) (*OTS, error) {
	if err := o.OnlyKeys("digest", "submitted_at", "calendars", "bitcoin", "contradicted", "dropped"); err != nil {
		return nil, err
	}
	s := &OTS{}
	var err error
	if s.Digest, err = hex32(o, "digest"); err != nil {
		return nil, err
	}
	if s.SubmittedAt, err = timeMember(o, "submitted_at"); err != nil {
		return nil, err
	}
	cals, err := o.Array("calendars")
	if err != nil {
		return nil, err
	}
	if len(cals) == 0 || len(cals) > MaxCalendars {
		return nil, fmt.Errorf("%s.calendars has %d entries, expected 1 to %d", o.Path(), len(cals), MaxCalendars)
	}
	for _, cv := range cals {
		co, err := cv.Object()
		if err != nil {
			return nil, err
		}
		if err := co.OnlyKeys("uri", "fragment_b64"); err != nil {
			return nil, err
		}
		var c Calendar
		if c.URI, err = printable(co, "uri", 1024, false); err != nil {
			return nil, err
		}
		if c.Fragment, err = b64(co, "fragment_b64", MaxFragment); err != nil {
			return nil, err
		}
		s.Calendars = append(s.Calendars, c)
	}
	bv, err := o.Field("bitcoin")
	if err != nil {
		return nil, err
	}
	if bv.Kind() != jsonx.KindNull {
		bo, err := bv.Object()
		if err != nil {
			return nil, err
		}
		if err := bo.OnlyKeys("height", "calendar_uri", "merkle_root", "status", "header"); err != nil {
			return nil, err
		}
		c := &Claim{}
		h, err := bo.Int64("height", 0, 1<<31-1)
		if err != nil {
			return nil, err
		}
		c.Height = uint64(h) // #nosec G115 -- range checked
		if c.CalendarURI, err = printable(bo, "calendar_uri", 1024, false); err != nil {
			return nil, err
		}
		if c.MerkleRoot, err = hex32(bo, "merkle_root"); err != nil {
			return nil, err
		}
		if c.Status, err = bo.String("status"); err != nil || (c.Status != "claimed" && c.Status != "header_checked") {
			return nil, fmt.Errorf("%s.status is not claimed or header_checked", bo.Path())
		}
		hs, err := bo.String("header")
		if err != nil {
			return nil, err
		}
		switch {
		case hs == "":
		case hex160RE.MatchString(hs):
			c.Header = hexBytes(hs)
		default:
			return nil, fmt.Errorf("%s.header is neither empty nor 160 lower case hexadecimal digits", bo.Path())
		}
		if (c.Status == "header_checked") != (c.Header != nil) {
			return nil, fmt.Errorf("%s: a header is given exactly when the status is header_checked", bo.Path())
		}
		s.Bitcoin = c
	}
	for _, name := range []string{"contradicted", "dropped"} {
		arr, err := o.Array(name)
		if err != nil {
			return nil, err
		}
		if len(arr) > maxList {
			return nil, fmt.Errorf("%s.%s has %d entries, expected at most %d", o.Path(), name, len(arr), maxList)
		}
		for _, e := range arr {
			if _, err := e.Object(); err != nil {
				return nil, err
			}
		}
		if name == "contradicted" {
			s.Contradicted = len(arr)
		} else {
			s.Dropped = len(arr)
		}
	}
	return s, nil
}
