// SPDX-License-Identifier: AGPL-3.0-only

package worm

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

const aggID = "ab12cd34"

func testNote(t testing.TB, size string, rootByte byte) []byte {
	t.Helper()
	root := base64.StdEncoding.EncodeToString([]byte(strings.Repeat(string([]byte{rootByte}), 32)))
	return []byte("kingres.example/aggregate\n" + size + "\n" + root + "\n\n\xe2\x80\x94 kingres.example/aggregate AAAAAA==\n")
}

func testPayload(t testing.TB, size string, rootByte byte) anchor.Payload {
	t.Helper()
	p, err := anchor.NewPayload(aggID, testNote(t, size, rootByte), clockNow())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

var fixedNow = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func clockNow() time.Time { return time.Now().UTC() }

type opt func(*Config, *Options)

func newTarget(t testing.TB, f *fakeS3, opts ...opt) *Target {
	t.Helper()
	cfg := Config{Endpoint: f.srv.URL, Region: fakeRegion, Bucket: fakeBucket, AllowInsecure: true, RetentionDays: 30}
	o := Options{AccessKeyID: fakeKeyID, SecretAccessKey: fakeSecret, Clock: clockNow}
	for _, fn := range opts {
		fn(&cfg, &o)
	}
	o.Config = cfg
	tg, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	return tg
}

func vhost(f *fakeS3) opt {
	return func(c *Config, o *Options) {
		c.Endpoint = "http://s3.test:9000"
		pf := false
		c.PathStyle = &pf
		o.Client = f.dialer()
	}
}

func mode(m string) opt             { return func(c *Config, o *Options) { c.Mode = m } }
func inm(m string) opt              { return func(c *Config, o *Options) { c.IfNoneMatch = m } }
func withClient(h *http.Client) opt { return func(c *Config, o *Options) { o.Client = h } }

func mustSubmit(t *testing.T, tg *Target, p anchor.Payload) (anchor.Receipt, anchor.WORMEvidence) {
	t.Helper()
	rc, err := tg.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	ev, err := anchor.ParseWORMEvidence(rc.Evidence)
	if err != nil {
		t.Fatal(err)
	}
	return rc, ev
}

func expectErr(t *testing.T, err error, class anchor.Class, code string) *anchor.Error {
	t.Helper()
	var ae *anchor.Error
	if !errors.As(err, &ae) {
		t.Fatalf("want *anchor.Error, got %T %v", err, err)
	}
	if ae.Class != class || (code != "" && ae.Code != code) {
		t.Fatalf("want class %v code %q, got class %v code %q: %v", class, code, ae.Class, ae.Code, err)
	}
	return ae
}

func noSigErrors(t *testing.T, f *fakeS3) {
	t.Helper()
	if e := f.signatureErrors(); len(e) != 0 {
		t.Fatalf("the independent verifier rejected %d requests: %v", len(e), e)
	}
}

func TestSubmitPathStyle(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "17", 1)
	rc, ev := mustSubmit(t, tg, p)
	noSigErrors(t, f)
	key := "kingres-anchors/" + aggID + "/00000000000000000017.note"
	if ev.Key != key || ev.Bucket != fakeBucket || ev.LockMode != "COMPLIANCE" || ev.RecordedBy != "cluster" || ev.Endpoint != f.srv.URL {
		t.Fatalf("evidence %+v", ev)
	}
	vs := f.stored(key)
	if len(vs) != 1 || string(vs[0].body) != string(p.SignedNote) || ev.VersionID != vs[0].id {
		t.Fatal("stored object")
	}
	if rc.State != anchor.ReceiptAnchored || rc.EvidenceTime == nil || anchor.FormatTime(*rc.EvidenceTime) != ev.LastModified {
		t.Fatalf("receipt %+v", rc)
	}
	// the headers of the put are those of 6.3 step 2
	put := f.requests()[0]
	h := put.header
	if put.method != "PUT" || h.Get("If-None-Match") != "*" || h.Get("Content-Type") != "text/plain; charset=utf-8" ||
		h.Get("X-Amz-Object-Lock-Mode") != "COMPLIANCE" {
		t.Fatalf("put headers %v", h)
	}
	sum := sha256.Sum256(p.SignedNote)
	if h.Get("X-Amz-Checksum-Sha256") != base64.StdEncoding.EncodeToString(sum[:]) {
		t.Fatal("checksum header")
	}
	until, err := time.Parse(time.RFC3339, h.Get("X-Amz-Object-Lock-Retain-Until-Date"))
	if err != nil || until.Before(time.Now().Add(29*24*time.Hour)) || until.After(time.Now().Add(31*24*time.Hour)) {
		t.Fatalf("retain until %v %v", until, err)
	}
	if !strings.HasPrefix(put.uri, "/"+fakeBucket+"/kingres-anchors/") {
		t.Fatal(put.uri)
	}
	// evidence is in canonical form and round trips
	again, _ := anchor.EncodeEvidence(ev)
	if string(again) != string(rc.Evidence) {
		t.Fatal("not canonical")
	}
}

func TestSubmitVirtualHostAndCompliance(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f, vhost(f), mode("compliance"))
	_, ev := mustSubmit(t, tg, testPayload(t, "3", 2))
	noSigErrors(t, f)
	if ev.LockMode != "COMPLIANCE" || ev.Endpoint != "http://s3.test:9000" {
		t.Fatalf("%+v", ev)
	}
	r := f.requests()[0]
	if r.host != fakeBucket+".s3.test:9000" || strings.HasPrefix(r.uri, "/"+fakeBucket) {
		t.Fatalf("host %q uri %q", r.host, r.uri)
	}
}

func TestSessionToken(t *testing.T) {
	f := newFakeS3()
	f.needToken = true
	defer f.close()
	tg := newTarget(t, f, func(c *Config, o *Options) { o.SessionToken = fakeToken })
	mustSubmit(t, tg, testPayload(t, "1", 1))
	noSigErrors(t, f)
	// without the token the independent verifier refuses
	tg2 := newTarget(t, f)
	_, err := tg2.Submit(context.Background(), testPayload(t, "2", 1))
	expectErr(t, err, anchor.ClassRefused, "")
}

func TestSubmitIdempotent(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "5", 1)
	rc1, ev1 := mustSubmit(t, tg, p)
	rc2, ev2 := mustSubmit(t, tg, p)
	noSigErrors(t, f)
	// the second call finds the version: the evidence is the same but for the
	// member that says so (A3-08)
	if ev1.Found || !ev2.Found {
		t.Fatalf("found: %v then %v", ev1.Found, ev2.Found)
	}
	ev2.Found = false
	if ev1 != ev2 || rc1.State != rc2.State {
		t.Fatalf("evidence differs:\n%s\n%s", rc1.Evidence, rc2.Evidence)
	}
	if n := len(f.stored(ev1.Key)); n != 1 {
		t.Fatalf("%d versions", n)
	}
}

func TestConflictDifferentNote(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	mustSubmit(t, tg, testPayload(t, "9", 1))
	_, err := tg.Submit(context.Background(), testPayload(t, "9", 2)) // other root, same size
	expectErr(t, err, anchor.ClassConflict, "worm_conflict")
	if !anchor.IsConflict(err) || anchor.IsRetryable(err) {
		t.Fatal("classification")
	}
	key := "kingres-anchors/" + aggID + "/00000000000000000009.note"
	if vs := f.stored(key); len(vs) != 1 {
		t.Fatalf("an object was overwritten or added: %d versions", len(vs))
	}
	noSigErrors(t, f)
}

func TestConflictHugeObject(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	mustSubmit(t, tg, testPayload(t, "9", 1))
	f.bigGet = anchor.MaxSignedNote + 1
	_, err := tg.Submit(context.Background(), testPayload(t, "9", 1))
	expectErr(t, err, anchor.ClassConflict, "worm_conflict")
}

func TestExistingNotLockedIsFailure(t *testing.T) {
	f := newFakeS3()
	f.noLock = true
	f.retentionStatus = 403 // and the store does not take the retention afterwards either
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "4", 1)
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "not_locked")
	// the object is there, same bytes, still unlocked and the store refuses the
	// retention: the retry reports it, with what the operator must do
	_, err = tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "not_locked")
}

func TestStoreDoesNotLock(t *testing.T) {
	for name, set := range map[string]func(*fakeS3){
		"no-lock-headers": func(f *fakeS3) { f.noLock = true },
		"other-mode":      func(f *fakeS3) { f.forceMode = "GOVERNANCE" },
		"short-retention": func(f *fakeS3) { f.shortRetain = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3()
			set(f)
			defer f.close()
			tg := newTarget(t, f, mode("compliance"))
			_, err := tg.Submit(context.Background(), testPayload(t, "4", 1))
			ae := expectErr(t, err, anchor.ClassPermanent, "not_locked")
			if !strings.Contains(ae.Error(), "did not lock") {
				t.Fatal(ae)
			}
		})
	}
}

func TestReadBackMismatch(t *testing.T) {
	for name, set := range map[string]func(*fakeS3){
		"checksum": func(f *fakeS3) { f.badSumHeader = true },
		"length":   func(f *fakeS3) { f.badLength = true },
		"bytes":    func(f *fakeS3) { f.badGetBody = true },
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeS3()
			set(f)
			defer f.close()
			tg := newTarget(t, f)
			_, err := tg.Submit(context.Background(), testPayload(t, "4", 1))
			expectErr(t, err, anchor.ClassPermanent, "readback_mismatch")
		})
	}
	t.Run("no-last-modified", func(t *testing.T) {
		f := newFakeS3()
		f.noLastModified = true
		defer f.close()
		_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
		expectErr(t, err, anchor.ClassPermanent, "no_last_modified")
	})
}

// A store that returns no version id cannot hold an object lock and cannot
// give evidence that names a version: not anchored (permanent, with a text
// that says why).
func TestNoVersionIDIsNotAnAnchor(t *testing.T) {
	f := newFakeS3()
	f.noVersionID = true
	defer f.close()
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassPermanent, "no_version_id")
}

func TestPutAnswers(t *testing.T) {
	cases := []struct {
		name, code, msg string
		status          int
		retryAfter      string
		class           anchor.Class
		wantAfter       time.Duration
	}{
		{"403-denied", "AccessDenied", "Access Denied", 403, "", anchor.ClassRefused, 0},
		{"403-sig", "SignatureDoesNotMatch", "no", 403, "", anchor.ClassRefused, 0},
		{"401", "", "", 401, "", anchor.ClassRefused, 0},
		{"404-bucket", "NoSuchBucket", "missing", 404, "", anchor.ClassRefused, 0},
		{"400-lock", "InvalidRequest", "Bucket is missing Object Lock Configuration", 400, "", anchor.ClassRefused, 0},
		{"400-other", "MalformedHeader", "bad", 400, "", anchor.ClassPermanent, 0},
		{"409-other", "BucketAlreadyOwnedByYou", "x", 409, "", anchor.ClassPermanent, 0},
		{"500", "InternalError", "oops", 500, "", anchor.ClassRetryable, 0},
		{"503-slowdown", "SlowDown", "slow", 503, "7", anchor.ClassRetryable, 7 * time.Second},
		{"429", "", "", 429, "11", anchor.ClassRetryable, 11 * time.Second},
		{"502-no-body", "", "", 502, "", anchor.ClassRetryable, 0},
		{"408", "RequestTimeout", "", 408, "", anchor.ClassRetryable, 0},
		{"413", "EntityTooLarge", "", 413, "", anchor.ClassPermanent, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeS3()
			f.putStatus, f.putCode, f.putMsg, f.putRetryAfter = tc.status, tc.code, tc.msg, tc.retryAfter
			defer f.close()
			_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
			ae := expectErr(t, err, tc.class, "")
			if ae.Status != tc.status {
				t.Fatalf("status %d", ae.Status)
			}
			if anchor.RetryAfterOf(err) != tc.wantAfter {
				t.Fatalf("retry after %v", anchor.RetryAfterOf(err))
			}
		})
	}
}

func TestConditionalConflict409(t *testing.T) {
	f := newFakeS3()
	f.putStatus, f.putCode, f.putMsg = 409, "ConditionalRequestConflict", "a conflicting operation"
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "6", 1)
	// not there yet: a request in flight: retry
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRetryable, "conflicting_request")
	// there with the same bytes: success; with other bytes: conflict
	g := newFakeS3()
	defer g.close()
	mustSubmit(t, newTarget(t, g), p)
	g.putStatus, g.putCode = 409, "ConditionalRequestConflict"
	mustSubmit(t, newTarget(t, g), p)
	_, err = newTarget(t, g).Submit(context.Background(), testPayload(t, "6", 7))
	expectErr(t, err, anchor.ClassConflict, "")
}

func TestIfNoneMatchFallbacks(t *testing.T) {
	t.Run("auto-falls-back-on-501", func(t *testing.T) {
		f := newFakeS3()
		f.rejectINM, f.rejectINMCode = 501, "NotImplemented"
		defer f.close()
		tg := newTarget(t, f)
		p := testPayload(t, "8", 1)
		_, ev := mustSubmit(t, tg, p)
		noSigErrors(t, f)
		// the second submission finds the key by HEAD and compares
		mustSubmit(t, tg, p)
		if len(f.stored(ev.Key)) != 1 {
			t.Fatal("duplicate object")
		}
		_, err := tg.Submit(context.Background(), testPayload(t, "8", 3))
		expectErr(t, err, anchor.ClassConflict, "")
	})
	t.Run("auto-falls-back-on-400", func(t *testing.T) {
		f := newFakeS3()
		f.rejectINM, f.rejectINMCode = 400, "InvalidArgument"
		defer f.close()
		mustSubmit(t, newTarget(t, f), testPayload(t, "8", 1))
	})
	t.Run("auto-400-about-lock-is-not-a-fallback", func(t *testing.T) {
		f := newFakeS3()
		f.lockEnabled = false
		defer f.close()
		_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "8", 1))
		expectErr(t, err, anchor.ClassRefused, "object_lock_unavailable")
	})
	t.Run("on-refuses-501", func(t *testing.T) {
		f := newFakeS3()
		f.rejectINM, f.rejectINMCode = 501, "NotImplemented"
		defer f.close()
		_, err := newTarget(t, f, inm("on")).Submit(context.Background(), testPayload(t, "8", 1))
		expectErr(t, err, anchor.ClassRefused, "if_none_match_unsupported")
	})
	t.Run("off-sends-no-header", func(t *testing.T) {
		f := newFakeS3()
		defer f.close()
		p := testPayload(t, "8", 1)
		tg := newTarget(t, f, inm("off"))
		mustSubmit(t, tg, p)
		mustSubmit(t, tg, p)
		for _, r := range f.requests() {
			if r.header.Get("If-None-Match") != "" {
				t.Fatal("header sent")
			}
		}
		noSigErrors(t, f)
	})
	t.Run("store-ignoring-the-header-never-overwrites-with-check", func(t *testing.T) {
		f := newFakeS3()
		f.ignoreINM = true
		defer f.close()
		tg := newTarget(t, f, inm("off"))
		mustSubmit(t, tg, testPayload(t, "8", 1))
		_, err := tg.Submit(context.Background(), testPayload(t, "8", 2))
		expectErr(t, err, anchor.ClassConflict, "")
	})
}

func TestVerifyReadFailureIsRetryableAndRetryCompletes(t *testing.T) {
	f := newFakeS3()
	f.headStatus = 500
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "12", 1)
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRetryable, "")
	f.headStatus = 0
	_, ev := mustSubmit(t, tg, p) // 412 path
	if len(f.stored(ev.Key)) != 1 {
		t.Fatal("extra versions")
	}
	// a 403 on the read-back stays refused, not retried forever as transient
	f.headStatus = 403
	_, err = tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRefused, "")
}

func TestGetFailureAfterExisting(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "13", 1)
	mustSubmit(t, tg, p)
	f.getStatus = 503
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassRetryable, "")
}

func TestSlowStoreHonoursContext(t *testing.T) {
	f := newFakeS3()
	f.hang = make(chan struct{})
	defer f.close()
	defer close(f.hang)
	tg := newTarget(t, f)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := tg.Submit(ctx, testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassRetryable, "transport")
	if time.Since(start) > 3*time.Second {
		t.Fatal("did not stop at the deadline")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("not a deadline error: %v", err)
	}
}

func TestCutConnection(t *testing.T) {
	f := newFakeS3()
	f.cut = true
	defer f.close()
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassRetryable, "transport")
}

func TestRedirectToOtherHostRefused(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer other.Close()
	f := newFakeS3()
	f.redirectTo = other.URL + "/x"
	defer f.close()
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassPermanent, "redirect")
	if hits.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
	// a redirect to the same host is not followed either
	f.redirectTo = f.srv.URL + "/again"
	n := len(f.requests())
	_, err = newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassPermanent, "redirect")
	if len(f.requests())-n != 1 {
		t.Fatal("followed a same-host redirect")
	}
	if strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatal("redirect target in the error")
	}
}

func TestResponseSizeCaps(t *testing.T) {
	// error body: exactly the cap is read, one more is refused
	f := newFakeS3()
	defer f.close()
	f.putStatus, f.putCode = 500, "InternalError"
	base := len(`<?xml version="1.0" encoding="UTF-8"?><Error><Code>InternalError</Code><Message></Message><RequestId>R1</RequestId></Error>`)
	f.bigError = MaxErrorBody - base
	_, err := newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassRetryable, "")
	if errors.Is(err, anchor.ErrTooLarge) {
		t.Fatal("body at the cap refused")
	}
	f.bigError = MaxErrorBody - base + 1
	_, err = newTarget(t, f).Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassPermanent, "transport")
	if !errors.Is(err, anchor.ErrTooLarge) {
		t.Fatalf("%v", err)
	}
}

func TestNoteSizeBoundary(t *testing.T) {
	// the signed note limit is a constant of package anchor; a payload at
	// the limit is accepted by the put and read back, one over is refused
	// by NewPayload
	if _, err := anchor.NewPayload(aggID, testNote(t, "1", 1), clockNow()); err != nil {
		t.Fatal(err)
	}
	big := testNote(t, "1", 1)
	// 16 signature lines of 1,000 bytes: above the limit, within the limits of lines and of their number
	for len(big) <= anchor.MaxSignedNote {
		big = append(big, []byte("\xe2\x80\x94 k "+strings.Repeat("A", 992)+"\n")...)
	}
	if _, err := anchor.NewPayload(aggID, big, clockNow()); err == nil {
		t.Fatal("oversized note accepted")
	}
}

func TestTLSRequired(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	cfg := Config{Endpoint: f.srv.URL, Region: fakeRegion, Bucket: fakeBucket}
	if _, err := New(Options{Config: cfg, AccessKeyID: fakeKeyID, SecretAccessKey: fakeSecret}); err == nil {
		t.Fatal("http endpoint accepted without allowInsecure")
	}
	// TLS store with a certificate the client does not trust
	var ok atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { ok.Add(1); w.WriteHeader(500) }))
	defer srv.Close()
	cfg.Endpoint, cfg.AllowInsecure = srv.URL, false
	tg, err := New(Options{Config: cfg, AccessKeyID: fakeKeyID, SecretAccessKey: fakeSecret})
	if err != nil {
		t.Fatal(err)
	}
	_, err = tg.Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassPermanent, "transport")
	if ok.Load() != 0 {
		t.Fatal("request reached a server with an untrusted certificate")
	}
	// the same store with the caller's trust
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	tg, _ = New(Options{Config: cfg, AccessKeyID: fakeKeyID, SecretAccessKey: fakeSecret,
		Client: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}})
	_, err = tg.Submit(context.Background(), testPayload(t, "4", 1))
	expectErr(t, err, anchor.ClassRetryable, "") // the fake answers 500
	if ok.Load() == 0 {
		t.Fatal("trusted TLS request did not arrive")
	}
}

func TestBadPayload(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	p := testPayload(t, "4", 1)
	p.Digest[0] ^= 1
	_, err := tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "bad_payload")
	p = testPayload(t, "4", 1)
	p.ID = aggID + "/5"
	_, err = tg.Submit(context.Background(), p)
	expectErr(t, err, anchor.ClassPermanent, "bad_payload")
	if len(f.requests()) != 0 {
		t.Fatal("request sent for a bad payload")
	}
}

func TestUpgradeIsIdentity(t *testing.T) {
	f := newFakeS3()
	defer f.close()
	tg := newTarget(t, f)
	rc, _ := mustSubmit(t, tg, testPayload(t, "4", 1))
	got, err := tg.Upgrade(context.Background(), rc)
	if err != nil || string(got.Evidence) != string(rc.Evidence) || got.State != rc.State {
		t.Fatal("upgrade changed the receipt")
	}
}

func TestTestMethod(t *testing.T) {
	status := func(r anchor.TestResult, name string) string {
		for _, c := range r.Checks {
			if c.Name == name {
				return c.Status
			}
		}
		return "absent"
	}
	t.Run("healthy", func(t *testing.T) {
		f := newFakeS3()
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		noSigErrors(t, f)
		if !r.OK() || status(r, "versioning") != "ok" || status(r, "object-lock") != "ok" || status(r, "if-none-match") != "ok" || status(r, "probe-head") != "ok" {
			t.Fatalf("%+v", r)
		}
		// the probe has no lock headers, a zero body, and If-None-Match
		var put *fakeReq
		for _, q := range f.requests() {
			if q.method == "PUT" {
				q := q
				put = &q
				break
			}
		}
		if put == nil || put.header.Get("X-Amz-Object-Lock-Mode") != "" || put.header.Get("If-None-Match") != "*" ||
			!strings.Contains(put.uri, "/_probe/") || put.header.Get("X-Amz-Content-Sha256") != anchorEmpty {
			t.Fatalf("%+v", put)
		}
	})
	t.Run("compliance-default-retention-warns", func(t *testing.T) {
		f := newFakeS3()
		f.defaultMode, f.defaultDays = "COMPLIANCE", 30
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if status(r, "default-retention") != "warning" || !r.OK() {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("versioning-off", func(t *testing.T) {
		f := newFakeS3()
		f.versioning = "Suspended"
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if r.OK() || status(r, "versioning") != "failed" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("lock-off", func(t *testing.T) {
		f := newFakeS3()
		f.lockEnabled = false
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if r.OK() || status(r, "object-lock") != "failed" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("if-none-match-ignored", func(t *testing.T) {
		f := newFakeS3()
		f.ignoreINM = true
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if status(r, "if-none-match") != "warning" {
			t.Fatalf("%+v", r)
		}
		r = newTarget(t, f, inm("on")).Test(context.Background())
		if status(r, "if-none-match") != "failed" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("if-none-match-rejected", func(t *testing.T) {
		f := newFakeS3()
		f.rejectINM, f.rejectINMCode = 501, "NotImplemented"
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if status(r, "probe-put") != "warning" || !r.OK() {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("bad-credentials", func(t *testing.T) {
		f := newFakeS3()
		f.failSig = true
		defer f.close()
		r := newTarget(t, f).Test(context.Background())
		if r.OK() || status(r, "bucket") != "failed" {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("bucket-missing", func(t *testing.T) {
		f := newFakeS3()
		defer f.close()
		r := newTarget(t, f, func(c *Config, o *Options) { c.Bucket = "other-bucket" }).Test(context.Background())
		if r.OK() {
			t.Fatalf("%+v", r)
		}
	})
}

const anchorEmpty = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestConfig(t *testing.T) {
	base := `{"region":"r1","bucket":"my-bucket"`
	bad := []string{
		`{}`, base + `,"extra":1}`, base + `,"mode":"x"}`, base + `,"retentionDays":-1}`, base + `,"retentionDays":36501}`,
		base + `,"ifNoneMatch":"maybe"}`, base + `,"keyPrefix":"a//b"}`, base + `,"keyPrefix":"a/../b"}`,
		base + `,"keyPrefix":"sp ace"}`, base + `,"endpoint":"http://h"}`, base + `,"endpoint":"ftp://h"}`,
		base + `,"endpoint":"https://u:p@h"}`, base + `,"endpoint":"https://h/path"}`, base + `,"endpoint":"https://h?x=1"}`,
		`{"region":"r1","bucket":"ab"}`, `{"region":"r1","bucket":"a b c"}`, `{"region":"","bucket":"my-bucket"}`, `[]`, ``,
	}
	for _, c := range bad {
		if _, err := ParseConfig(json.RawMessage(c)); err == nil {
			t.Errorf("accepted %q", c)
		}
	}
	c, err := ParseConfig(json.RawMessage(base + `}`))
	if err != nil || c.KeyPrefix != DefaultKeyPrefix || c.Mode != "compliance" || len(c.Warnings()) != 0 || c.RetentionDays != 3650 || c.IfNoneMatch != "auto" || *c.PathStyle {
		t.Fatalf("defaults %+v %v", c, err)
	}
	c, err = ParseConfig(json.RawMessage(base + `,"endpoint":"https://Store.Example:9000","keyPrefix":"/p/q/"}`))
	if err != nil || !*c.PathStyle || c.Endpoint != "https://store.example:9000" || c.KeyPrefix != "p/q" {
		t.Fatalf("%+v %v", c, err)
	}
	for _, d := range []string{"1", "36500"} {
		if _, err := ParseConfig(json.RawMessage(base + `,"retentionDays":` + d + `}`)); err != nil {
			t.Errorf("retentionDays %s: %v", d, err)
		}
	}
	if _, err := ParseConfig(json.RawMessage(base + `,"endpoint":"http://h:1","allowInsecure":true}`)); err != nil {
		t.Error(err)
	}
	huge := json.RawMessage(base + `,"caBundlePath":"` + strings.Repeat("a", MaxConfigBytes) + `"}`)
	if _, err := ParseConfig(huge); err == nil {
		t.Error("huge config accepted")
	}
	if _, err := New(Options{Config: Config{Region: "r", Bucket: "bbb"}}); err == nil {
		t.Error("no credentials accepted")
	}
}

func TestObjectKey(t *testing.T) {
	if k := ObjectKey("/p/", "ab", 17); k != "p/ab/00000000000000000017.note" {
		t.Fatal(k)
	}
	if k := ObjectKey("p", "ab", 18446744073709551615); k != "p/ab/18446744073709551615.note" || len(k) != len("p/ab/")+20+5 {
		t.Fatal(k)
	}
	if k := ObjectKey("p", "ab", 0); k != "p/ab/00000000000000000000.note" {
		t.Fatal(k)
	}
}

func TestParseS3Error(t *testing.T) {
	cases := []struct{ in, code, msg string }{
		{`<Error><Code>NoSuchKey</Code><Message>The key does not exist</Message></Error>`, "NoSuchKey", "The key does not exist"},
		{`<?xml version="1.0"?><Error><Message>m</Message><Code>X-1.a_b</Code><Resource>/r</Resource></Error>`, "X-1.a_b", "m"},
		{`<Error><Code>bad code</Code></Error>`, "", ""},
		{`<Error><Code>` + strings.Repeat("a", 65) + `</Code></Error>`, "", ""},
		{`<Html><Error><Code>X</Code></Error></Html>`, "", ""},
		{`not xml`, "", ""},
		{``, "", ""},
		{`<Error><Code>A</Code><Code>B</Code></Error>`, "A", ""},
		{"<Error><Message>a\xc3\xa9" + strings.Repeat("z", 300) + "</Message></Error>", "", "a??" + strings.Repeat("z", MaxMessage-3)},
		{"<Error><Code>X</Code><Message>ab</Message></Error>", "X", ""},
		{`<Error><Code>A</Code><Message><![CDATA[x<y]]></Message></Error>`, "A", "x<y"},
		{`<!DOCTYPE x [<!ENTITY a "bbbb">]><Error><Code>&a;</Code></Error>`, "", ""},
	}
	for i, c := range cases {
		code, msg := ParseS3Error([]byte(c.in))
		if code != c.code || msg != c.msg {
			t.Errorf("case %d: got %q %q want %q %q", i, code, msg, c.code, c.msg)
		}
	}
	// only MaxErrorBody bytes are read
	in := `<Error><Code>A</Code>` + strings.Repeat(" ", MaxErrorBody) + `<Message>late</Message></Error>`
	if _, m := ParseS3Error([]byte(in)); m != "" {
		t.Fatal("read past the limit")
	}
}
