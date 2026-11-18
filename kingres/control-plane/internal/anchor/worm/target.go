// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
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
	// hdrAttempt is the user metadata that every put of Submit sets: the clock of
	// this server at the start of the call that wrote the version (correction 89).
	hdrAttempt = "X-Amz-Meta-Kingres-Attempt"
)

func sum64(b []byte) string {
	s := sha256.Sum256(b)
	return base64.StdEncoding.EncodeToString(s[:])
}

func (t *Target) put(ctx context.Context, key string, body []byte, retain time.Time, ifNoneMatch, lock bool, attempt time.Time) (*response, error) {
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
	if !attempt.IsZero() {
		req.Header.Set(hdrAttempt, anchor.FormatTime(attempt))
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
	r, err := t.put(ctx, key, []byte{}, time.Time{}, true, false, time.Time{})
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
	r2, err := t.put(ctx, key, []byte{}, time.Time{}, true, false, time.Time{})
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
	switch cfg.Mode {
	case ModeCompliance:
		res.Add("mode", anchor.CheckWarning, "compliance mode cannot be shortened or removed before the retention date")
	case ModeGovernance:
		for _, w := range cfg.Warnings() {
			res.Add("mode", anchor.CheckWarning, "%s", w)
		}
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

func sha256Sum(b []byte) [32]byte { return sha256.Sum256(b) }
