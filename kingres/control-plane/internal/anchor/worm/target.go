// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/sigv4"
)

// Options of New.
type Options struct {
	Config          Config
	AccessKeyID     anchor.Secret
	SecretAccessKey anchor.Secret
	SessionToken    anchor.Secret // optional
	// Client is the caller's HTTP client (its transport carries the CA
	// bundle); nil means a client with the default transport. A copy that
	// follows no redirect is used.
	Client *http.Client
	// Clock defaults to time.Now.
	Clock anchor.Clock
	// Rand is the source of the probe names; defaults to crypto/rand.
	Rand io.Reader
}

// Target is the WORM target. It is safe for concurrent use.
type Target struct {
	s    *s3
	rand io.Reader
}

var _ anchor.Target = (*Target)(nil)

// New returns a Target. The configuration is validated again.
func New(o Options) (*Target, error) {
	cfg, err := o.Config.Normalize()
	if err != nil {
		return nil, err
	}
	if o.AccessKeyID == "" || o.SecretAccessKey == "" {
		return nil, errors.New("worm: access key id and secret access key are required")
	}
	clk := o.Clock
	if clk == nil {
		clk = time.Now
	}
	rd := o.Rand
	if rd == nil {
		rd = rand.Reader
	}
	return &Target{
		rand: rd,
		s: &s3{
			cfg: cfg,
			signer: sigv4.Signer{
				Credentials: sigv4.Credentials{AccessKeyID: o.AccessKeyID, SecretAccessKey: o.SecretAccessKey, SessionToken: o.SessionToken},
				Region:      cfg.Region, Service: "s3",
			},
			hc:    anchor.Client(o.Client, 0),
			clock: clk,
			red:   anchor.NewRedactor(o.AccessKeyID, o.SecretAccessKey, o.SessionToken),
		},
	}, nil
}

// Kind implements anchor.Target.
func (t *Target) Kind() string { return anchor.KindWORM }

// Config returns the normalised configuration.
func (t *Target) Config() Config { return t.s.cfg }

// Validate implements anchor.Target.
func (t *Target) Validate(cfg json.RawMessage) error {
	_, err := ParseConfig(cfg)
	return err
}

// Upgrade implements anchor.Target: the identity (a WORM receipt is final).
func (t *Target) Upgrade(ctx context.Context, r anchor.Receipt) (anchor.Receipt, error) {
	return r, nil
}

func withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, CallTimeout)
}

const (
	hdrMode    = "X-Amz-Object-Lock-Mode"
	hdrRetain  = "X-Amz-Object-Lock-Retain-Until-Date"
	hdrVersion = "X-Amz-Version-Id"
	hdrSum     = "X-Amz-Checksum-Sha256"
)

func sum64(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func (t *Target) put(ctx context.Context, key string, body []byte, retain time.Time, ifNoneMatch, lock bool) (*response, error) {
	req, err := t.s.request(ctx, http.MethodPut, key, "", body)
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "put", "request", "request could not be built")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	req.Header.Set("X-Amz-Checksum-Sha256", sum64(body))
	if ifNoneMatch {
		req.Header.Set("If-None-Match", "*")
	}
	if lock {
		req.Header.Set("X-Amz-Object-Lock-Mode", t.s.cfg.lockMode())
		req.Header.Set("X-Amz-Object-Lock-Retain-Until-Date", anchor.FormatTime(retain))
	}
	return t.s.do(ctx, "put", req, body, MaxErrorBody)
}

func (t *Target) head(ctx context.Context, op, key, version string) (*response, error) {
	q := ""
	if version != "" {
		q = "versionId=" + sigv4.Escape(version, false)
	}
	req, err := t.s.request(ctx, http.MethodHead, key, q, nil)
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, op, "request", "request could not be built")
	}
	req.Header.Set("X-Amz-Checksum-Mode", "ENABLED")
	return t.s.do(ctx, op, req, nil, 0)
}

func (t *Target) get(ctx context.Context, op, key, version string) (*response, error) {
	q := ""
	if version != "" {
		q = "versionId=" + sigv4.Escape(version, false)
	}
	req, err := t.s.request(ctx, http.MethodGet, key, q, nil)
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, op, "request", "request could not be built")
	}
	return t.s.do(ctx, op, req, nil, anchor.MaxSignedNote)
}

func checkPayload(p anchor.Payload) error {
	i := strings.IndexByte(p.ID, '/')
	if i <= 0 || p.ID[i+1:] != strconv.FormatUint(p.Size, 10) || len(p.NoteText) == 0 ||
		!bytes.HasPrefix(p.SignedNote, p.NoteText) || len(p.SignedNote) > anchor.MaxSignedNote ||
		sha256.Sum256(p.NoteText) != p.Digest {
		return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "submit", "bad_payload", "payload is inconsistent (use anchor.NewPayload)")
	}
	for _, c := range p.AggregateID() {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "submit", "bad_payload", "payload id is not lower case hex")
		}
	}
	return nil
}

// Submit implements anchor.Target (design 6.3, steps 1 to 5). It is
// idempotent: when the key exists with the same bytes it verifies and
// returns that object (a crash before the record was written). A note that
// differs gives an *anchor.Error of ClassConflict and nothing is overwritten.
// A failed verification read after a successful put is a retryable error:
// the retry finds the key (412) and completes the verification.
func (t *Target) Submit(ctx context.Context, p anchor.Payload) (anchor.Receipt, error) {
	if err := checkPayload(p); err != nil {
		return anchor.Receipt{}, err
	}
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cfg := t.s.cfg
	key := ObjectKey(cfg.KeyPrefix, p.AggregateID(), p.Size)
	now := t.s.clock()
	retain := now.Add(time.Duration(cfg.RetentionDays) * 24 * time.Hour).UTC().Truncate(time.Second)
	body := p.SignedNote

	useINM := cfg.IfNoneMatch != IfNoneMatchOff
	if !useINM {
		return t.checkThenPut(ctx, key, body, retain, now)
	}
	r, err := t.put(ctx, key, body, retain, true, true)
	if err != nil {
		return anchor.Receipt{}, err
	}
	switch {
	case r.status == 200:
		return t.verify(ctx, key, body, r.header.Get(hdrVersion), r.header.Get("ETag"), retain, 5*time.Minute, true)
	case r.status == 412 || r.status == 409:
		code, _ := ParseS3Error(r.body)
		if r.status == 409 && code != "ConditionalRequestConflict" {
			return anchor.Receipt{}, t.s.statusError("put", r)
		}
		return t.existing(ctx, key, body, retain, r.status == 409)
	case (r.status == 501 || r.status == 400) && cfg.IfNoneMatch == IfNoneMatchAuto:
		code, msg := ParseS3Error(r.body)
		if r.status == 400 && isLockComplaint(code, msg) {
			return anchor.Receipt{}, t.s.statusError("put", r)
		}
		return t.checkThenPut(ctx, key, body, retain, now)
	case (r.status == 501 || r.status == 400) && cfg.IfNoneMatch == IfNoneMatchOn:
		code, msg := ParseS3Error(r.body)
		if r.status == 400 && isLockComplaint(code, msg) {
			return anchor.Receipt{}, t.s.statusError("put", r)
		}
		e := t.s.statusError("put", r)
		e.Class, e.Code = anchor.ClassRefused, "if_none_match_unsupported"
		return anchor.Receipt{}, e
	}
	return anchor.Receipt{}, t.s.statusError("put", r)
}

// checkThenPut is the fallback without If-None-Match: HEAD the key, then
// put when it is absent. It is not atomic; object lock is what keeps an
// existing version safe.
func (t *Target) checkThenPut(ctx context.Context, key string, body []byte, retain, now time.Time) (anchor.Receipt, error) {
	h, err := t.head(ctx, "check", key, "")
	if err != nil {
		return anchor.Receipt{}, err
	}
	switch h.status {
	case 200:
		return t.existing(ctx, key, body, retain, false)
	case 404:
	default:
		return anchor.Receipt{}, t.s.statusError("check", h)
	}
	r, err := t.put(ctx, key, body, retain, false, true)
	if err != nil {
		return anchor.Receipt{}, err
	}
	if r.status != 200 {
		return anchor.Receipt{}, t.s.statusError("put", r)
	}
	return t.verify(ctx, key, body, r.header.Get(hdrVersion), r.header.Get("ETag"), retain, 5*time.Minute, true)
}

// existing handles "the key is already there": compare the bytes.
func (t *Target) existing(ctx context.Context, key string, body []byte, retain time.Time, transient bool) (anchor.Receipt, error) {
	g, err := t.get(ctx, "get", key, "")
	if err != nil {
		var ae *anchor.Error
		if errors.As(err, &ae) && errors.Is(ae.Err, anchor.ErrTooLarge) {
			return anchor.Receipt{}, conflict()
		}
		return anchor.Receipt{}, err
	}
	switch {
	case g.status == 404 && transient:
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassRetryable, anchor.KindWORM, "get", "conflicting_request", "a conflicting request is in progress")
	case g.status == 404:
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "get", "inconsistent", "the store reports the key as existing and as absent")
	case g.status != 200:
		return anchor.Receipt{}, t.s.statusError("get", g)
	}
	if !bytes.Equal(g.body, body) {
		return anchor.Receipt{}, conflict()
	}
	// an object made by an earlier attempt: its retention was set up to a
	// week ago
	return t.verify(ctx, key, body, g.header.Get(hdrVersion), g.header.Get("ETag"), retain, 7*24*time.Hour, false)
}

func conflict() *anchor.Error {
	return anchor.Errorf(anchor.ClassConflict, anchor.KindWORM, "put", "worm_conflict",
		"the bucket already holds a different note under this key: a different checkpoint of the same size exists")
}

func notLocked(format string, args ...any) *anchor.Error {
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "not_locked", "the store did not lock the object: "+format, args...)
}

func mismatch(what string) *anchor.Error {
	return anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "readback_mismatch", "read back differs from what was written: %s", what)
}

// verify reads the version back (design 6.3 step 3): HEAD for the lock mode,
// the retention date, Last-Modified, length, checksum and version id; GET for
// the bytes when readBytes. tol is how much earlier than retain the
// retention date may be.
func (t *Target) verify(ctx context.Context, key string, body []byte, version, etag string, retain time.Time, tol time.Duration, readBytes bool) (anchor.Receipt, error) {
	retry := func(e *anchor.Error) (anchor.Receipt, error) {
		if e.Class == anchor.ClassPermanent {
			// a failed read-back after a good write is not a verdict on the object
			if e.Code != "redirect" {
				e = &anchor.Error{Class: anchor.ClassRetryable, Target: e.Target, Op: "verify", Code: "verify_read_failed",
					Text: "the object was written but could not be read back: " + e.Text, Status: e.Status, Err: e}
			}
		}
		return anchor.Receipt{}, e
	}
	h, err := t.head(ctx, "verify-head", key, version)
	if err != nil {
		return retry(err.(*anchor.Error))
	}
	if h.status != 200 {
		return retry(t.s.statusError("verify-head", h))
	}
	if version == "" {
		version = h.header.Get(hdrVersion)
	} else if v := h.header.Get(hdrVersion); v != "" && v != version {
		return anchor.Receipt{}, mismatch("version id")
	}
	mode := h.header.Get(hdrMode)
	if mode == "" {
		return anchor.Receipt{}, notLocked("no lock mode on the object")
	}
	if mode != t.s.cfg.lockMode() {
		return anchor.Receipt{}, notLocked("lock mode is %s, configured %s", sanitize(mode), t.s.cfg.lockMode())
	}
	ru := h.header.Get(hdrRetain)
	if ru == "" {
		return anchor.Receipt{}, notLocked("no retain-until date on the object")
	}
	rt, err2 := time.Parse(time.RFC3339Nano, ru)
	if err2 != nil {
		return anchor.Receipt{}, notLocked("retain-until date is not RFC 3339")
	}
	rt = rt.UTC().Truncate(time.Second)
	min := retain.Add(-tol)
	if now := t.s.clock(); min.Before(now) {
		min = now
	}
	if rt.Before(min) {
		return anchor.Receipt{}, notLocked("retain-until date %s is earlier than requested", anchor.FormatTime(rt))
	}
	lm := h.header.Get("Last-Modified")
	lmt, err2 := http.ParseTime(lm)
	if lm == "" || err2 != nil {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "no_last_modified", "the store returned no usable Last-Modified")
	}
	if cl := h.header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		return anchor.Receipt{}, mismatch("content length")
	}
	if s := h.header.Get(hdrSum); s != "" && s != sum64(body) {
		return anchor.Receipt{}, mismatch("checksum")
	}
	if he := h.header.Get("ETag"); he != "" {
		etag = he
	}
	if readBytes {
		g, err := t.get(ctx, "verify-get", key, version)
		if err != nil {
			return retry(err.(*anchor.Error))
		}
		if g.status != 200 {
			return retry(t.s.statusError("verify-get", g))
		}
		if !bytes.Equal(g.body, body) {
			return anchor.Receipt{}, mismatch("bytes")
		}
	}
	cfg := t.s.cfg
	ev := anchor.WORMEvidence{
		Endpoint: cfg.Endpoint, Bucket: cfg.Bucket, Key: key, VersionID: t.s.red.Clean(version, 1024),
		ETag: t.s.red.Clean(etag, 256), LockMode: mode, RetainUntil: anchor.FormatTime(rt),
		LastModified: anchor.FormatTime(lmt), RecordedBy: anchor.RecordedByCluster,
	}
	raw, err2 := anchor.EncodeEvidence(ev)
	if err2 != nil {
		return anchor.Receipt{}, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "verify", "bad_evidence", "the store's answer cannot be recorded as evidence")
	}
	lt := lmt.UTC().Truncate(time.Second)
	return anchor.Receipt{State: anchor.ReceiptAnchored, Evidence: raw, EvidenceTime: &lt}, nil
}

func sanitize(s string) string { return (anchor.Redactor{}).Clean(s, 40) }

// Test implements anchor.Target (design 6.8): reachability, versioning,
// object lock and its default retention, and a probe object.
func (t *Target) Test(ctx context.Context) anchor.TestResult {
	var res anchor.TestResult
	ctx, cancel := withTimeout(ctx)
	defer cancel()
	cfg := t.s.cfg

	vr, err := t.bucketGet(ctx, "versioning")
	switch {
	case err != nil:
		res.Add("bucket", anchor.CheckFailed, "%s", err.Error())
		return res
	case vr.status != 200:
		res.Add("bucket", anchor.CheckFailed, "%s", t.s.statusError("get-versioning", vr).Error())
		return res
	}
	res.Add("bucket", anchor.CheckOK, "bucket reachable")
	if xmlValue(vr.body, "Status") == "Enabled" {
		res.Add("versioning", anchor.CheckOK, "versioning is Enabled")
	} else {
		res.Add("versioning", anchor.CheckFailed, "versioning is not Enabled")
	}

	lr, err := t.bucketGet(ctx, "object-lock")
	switch {
	case err != nil:
		res.Add("object-lock", anchor.CheckFailed, "%s", err.Error())
	case lr.status != 200:
		res.Add("object-lock", anchor.CheckFailed, "%s", t.s.statusError("get-object-lock", lr).Error())
	case xmlValue(lr.body, "ObjectLockEnabled") != "Enabled":
		res.Add("object-lock", anchor.CheckFailed, "object lock is not Enabled on the bucket")
	default:
		res.Add("object-lock", anchor.CheckOK, "object lock is Enabled")
		mode := xmlValue(lr.body, "Mode")
		days, years := xmlValue(lr.body, "Days"), xmlValue(lr.body, "Years")
		if mode == "" {
			res.Add("default-retention", anchor.CheckOK, "the bucket has no default retention")
		} else {
			d := days + " days"
			if years != "" {
				d = years + " years"
			}
			st := anchor.CheckOK
			txt := "default retention " + sanitize(mode) + " " + sanitize(d)
			if mode == "COMPLIANCE" {
				st = anchor.CheckWarning
				txt += ": the probe object cannot be deleted before it ends"
			}
			res.Add("default-retention", st, "%s", txt)
		}
	}

	// probe
	var rb [8]byte
	if _, err := io.ReadFull(t.rand, rb[:]); err != nil {
		res.Add("probe", anchor.CheckFailed, "no random source")
		return res
	}
	key := cfg.KeyPrefix + "/_probe/" + hex.EncodeToString(rb[:])
	r, err := t.put(ctx, key, []byte{}, time.Time{}, true, false)
	switch {
	case err != nil:
		res.Add("probe-put", anchor.CheckFailed, "%s", err.Error())
		return res
	case r.status == 200:
		res.Add("probe-put", anchor.CheckOK, "a probe object was written with If-None-Match and the checksum header")
	case r.status == 501 || r.status == 400:
		st := anchor.CheckWarning
		if cfg.IfNoneMatch == IfNoneMatchOn {
			st = anchor.CheckFailed
		}
		res.Add("probe-put", st, "the store rejects If-None-Match or the checksum header (http %d): a check before the write is used", r.status)
		return res
	default:
		res.Add("probe-put", anchor.CheckFailed, "%s", t.s.statusError("put", r).Error())
		return res
	}
	if h, err := t.head(ctx, "probe-head", key, r.header.Get(hdrVersion)); err != nil {
		res.Add("probe-head", anchor.CheckFailed, "%s", err.Error())
	} else if h.status != 200 {
		res.Add("probe-head", anchor.CheckFailed, "%s", t.s.statusError("probe-head", h).Error())
	} else {
		res.Add("probe-head", anchor.CheckOK, "the probe object can be read back")
	}
	r2, err := t.put(ctx, key, []byte{}, time.Time{}, true, false)
	switch {
	case err != nil:
		res.Add("if-none-match", anchor.CheckWarning, "second probe write failed: %s", err.Error())
	case r2.status == 412 || r2.status == 409:
		res.Add("if-none-match", anchor.CheckOK, "the store refuses to replace an existing key")
	case r2.status == 200:
		st := anchor.CheckWarning
		if cfg.IfNoneMatch == IfNoneMatchOn {
			st = anchor.CheckFailed
		}
		res.Add("if-none-match", st, "the store ignores If-None-Match: only object lock protects an existing note")
	default:
		res.Add("if-none-match", anchor.CheckWarning, "second probe write answered http %d", r2.status)
	}
	if cfg.Mode == ModeCompliance {
		res.Add("mode", anchor.CheckWarning, "compliance mode cannot be shortened or removed before the retention date")
	}
	return res
}

func (t *Target) bucketGet(ctx context.Context, sub string) (*response, error) {
	req, err := t.s.request(ctx, http.MethodGet, "", sub, nil)
	if err != nil {
		return nil, anchor.Errorf(anchor.ClassPermanent, anchor.KindWORM, "test", "request", "request could not be built")
	}
	return t.s.do(ctx, "get-"+sub, req, nil, MaxErrorBody)
}

// xmlValue returns the text of the first element named name (shape check
// of the small answers of Test only).
func xmlValue(b []byte, name string) string {
	s := string(b)
	i := strings.Index(s, "<"+name+">")
	if i < 0 {
		return ""
	}
	s = s[i+len(name)+2:]
	j := strings.Index(s, "</")
	if j < 0 || j > 64 {
		return ""
	}
	return strings.TrimSpace(s[:j])
}

// Format implements fmt.Formatter: a Target never prints its credentials,
// whichever verb is used (fmt cannot call the methods of the unexported
// fields it holds, so the type hides them as a whole).
func (t Target) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("worm.Target{...}")) }

// Format implements fmt.Formatter.
func (s s3) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("worm.s3{...}")) }

// Format implements fmt.Formatter: the options carry credentials.
func (o Options) Format(f fmt.State, verb rune) { _, _ = f.Write([]byte("worm.Options{...}")) }

// MarshalJSON implements json.Marshaler.
func (t Target) MarshalJSON() ([]byte, error) { return []byte(`"worm.Target"`), nil }
