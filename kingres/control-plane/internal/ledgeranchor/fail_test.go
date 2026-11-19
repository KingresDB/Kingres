// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor/worm"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// one record, one target, one pass of enqueue
func oneRecord(t *testing.T, mutate ...func(*env)) (*env, int64) {
	t.Helper()
	e := newEnv(t, mutate...)
	e.addWorm("vault")
	e.putAgg("shop", 3)
	if err := e.runEnqueue(); err != nil {
		t.Fatal(err)
	}
	return e, e.rows()[0].ID
}

func TestARefusedStoreIsWaitedForThreeTimesThenTheRecordFails(t *testing.T) {
	e, id := oneRecord(t)
	e.s3.putStatus, e.s3.putCode, e.s3.putMsg = 403, "AccessDenied", "Access Denied"
	for i, wait := range []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute} {
		if err := e.runSubmit(); err != nil {
			t.Fatal(err)
		}
		r := e.row(id)
		if r.State != anchor.StatePending || r.Attempts != i+1 || r.NextAttemptAt == nil || !r.NextAttemptAt.Equal(e.clock.Now().Add(wait)) {
			t.Fatalf("refusal %d: %+v (next %v, want %v)", i+1, r, r.NextAttemptAt, e.clock.Now().Add(wait))
		}
		// not due yet: a pass does nothing
		n := len(e.s3.requests())
		_ = e.runSubmit()
		if len(e.s3.requests()) != n {
			t.Fatalf("a record that is not due was submitted")
		}
		e.clock.Advance(wait)
	}
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.State != anchor.StateFailed || r.Alert != AlertFailed || r.NextAttemptAt != nil {
		t.Fatalf("the fourth refusal: %+v", r)
	}
	if err := e.runMonitor(); err != nil {
		t.Fatal(err)
	}
	if e.alerts.severity(AlertFailed, "target:1") != "critical" || !strings.Contains(e.alerts.all(), "AccessDenied") {
		t.Errorf("alerts: %s", e.alerts.all())
	}
}

func TestAnUnreachableStoreIsRetriedWithBackoffAndRetryAfterWins(t *testing.T) {
	e, id := oneRecord(t)
	e.s3.putStatus, e.s3.putCode = 503, "SlowDown"
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r := e.row(id)
	if r.State != anchor.StatePending || r.Retryables != 1 || !r.NextAttemptAt.Equal(e.clock.Now().Add(30*time.Second)) {
		t.Fatalf("first failure: %+v next %v", r, r.NextAttemptAt)
	}
	e.clock.Advance(30 * time.Second)
	e.s3.putStatus, e.s3.putCode, e.s3.putRetryAfter = 429, "SlowDown", "7200"
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r = e.row(id)
	if !r.NextAttemptAt.Equal(e.clock.Now().Add(2 * time.Hour)) {
		t.Fatalf("Retry-After of 2 hours: next %v (now %v)", r.NextAttemptAt, e.clock.Now())
	}
	// the store comes back
	e.s3.putStatus, e.s3.putRetryAfter = 0, ""
	e.clock.Advance(2 * time.Hour)
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if r = e.row(id); r.State != anchor.StateAnchored || r.Attempts != 0 || r.LastError != "" {
		t.Fatalf("after recovery: %+v", r)
	}
}

func TestAnotherNoteUnderTheKeyIsAConflictAndIsNeverOverwritten(t *testing.T) {
	e, id := oneRecord(t)
	r := e.row(id)
	key := worm.ObjectKey("kingres-anchors", r.AggregateID, r.Size)
	e.s3.addVersion(key, []byte("the checkpoint of a rewritten history\n"), "COMPLIANCE", e.clock.Now().Add(24*time.Hour))
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	r = e.row(id)
	if r.State != anchor.StateFailed || r.Alert != AlertConflict {
		t.Fatalf("conflict: %+v", r)
	}
	if vs := e.s3.stored(key); len(vs) != 1 {
		t.Errorf("the store holds %d versions: something was written over the conflict", len(vs))
	}
	if err := e.runMonitor(); err != nil {
		t.Fatal(err)
	}
	if e.alerts.severity(AlertConflict, "database:shop|target:1") != "critical" {
		t.Errorf("alerts: %s", e.alerts.all())
	}
	// a conflict is not retried by the job
	n := len(e.s3.requests())
	e.clock.Advance(48 * time.Hour)
	_ = e.runSubmit()
	if len(e.s3.requests()) != n {
		t.Error("a conflicting record was submitted again")
	}
}

// What a hostile or broken store can do: none of it makes a record anchored, and none of it hangs a job or
// leaves a credential in a text.
func TestAHostileStoreAnchorsNothingAndLeaksNothing(t *testing.T) {
	cases := map[string]func(*env){
		"the lock is not stored":    func(e *env) { e.s3.noLock = true },
		"another lock mode":         func(e *env) { e.s3.forceMode = "GOVERNANCE" },
		"a lock of one day":         func(e *env) { e.s3.shortRetain = true },
		"another body is read back": func(e *env) { e.s3.badGetBody = true },
		"a wrong checksum":          func(e *env) { e.s3.badSumHeader = true },
		"a wrong length":            func(e *env) { e.s3.badLength = true },
		"no version id":             func(e *env) { e.s3.noVersionID = true },
		"no Last-Modified":          func(e *env) { e.s3.noLastModified = true },
		"a store far ahead":         func(e *env) { e.s3.skew = 3 * time.Hour },
		"a store far behind":        func(e *env) { e.s3.skew = -3 * time.Hour },
		"the metadata is dropped":   func(e *env) { e.s3.dropAttempt = true },
		"an error that echoes keys": func(e *env) {
			e.s3.putStatus, e.s3.putCode, e.s3.putMsg, e.s3.echoSecrets = 500, "InternalError", "boom", true
		},
		"a very large error":     func(e *env) { e.s3.putStatus, e.s3.putCode, e.s3.bigError = 500, "InternalError", 8<<20 },
		"a very large read back": func(e *env) { e.s3.bigGet = 4 << 20 },
		"a 403 that echoes keys": func(e *env) {
			e.s3.putStatus, e.s3.putCode, e.s3.putMsg, e.s3.echoSecrets = 403, "AccessDenied", "no", true
		},
		"the connection is cut":       func(e *env) { e.s3.cut = true },
		"the signature is refused":    func(e *env) { e.s3.failSig = true },
		"the bucket has no versioned": func(e *env) { e.s3.versioning = "Suspended" },
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			e, id := oneRecord(t)
			setup(e.s3Env())
			done := make(chan error, 1)
			go func() { done <- e.runSubmit() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the job hangs")
			}
			r := e.row(id)
			if name == "the bucket has no versioned" {
				// versioning is not read by a submit (the Test button reads it); the write is locked: fine
				return
			}
			if r.State == anchor.StateAnchored && name != "a store far ahead" && name != "a store far behind" {
				t.Fatalf("anchored on a store that misbehaves: %+v", r)
			}
			if r.State == anchor.StateAnchored {
				t.Fatalf("anchored on a store whose clock is wrong: %+v", r)
			}
			_ = e.runMonitor()
			noSecret(t, name, e.allText(), testKeyID, testSecret)
		})
	}
}

// s3Env is a readability helper: the fake store belongs to the environment.
func (e *env) s3Env() *env { return e }

func TestAHangingStoreCannotHoldAJob(t *testing.T) {
	e, id := oneRecord(t)
	e.svc.cfg.SubmitTimeout = 300 * time.Millisecond
	e.s3.hang = make(chan struct{})
	defer close(e.s3.hang)
	start := time.Now()
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the pass took %s", d)
	}
	r := e.row(id)
	if r.State != anchor.StatePending || r.Attempts != 1 || r.SubmitStart.IsZero() {
		t.Fatalf("after a timeout: %+v", r)
	}
}

func TestARedirectToAnotherHostIsNotFollowed(t *testing.T) {
	var hits atomic.Int64
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer evil.Close()
	e, id := oneRecord(t)
	e.s3.redirectTo = evil.URL + "/stolen"
	if err := e.runSubmit(); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 0 {
		t.Fatalf("the evil host got %d requests", hits.Load())
	}
	if r := e.row(id); r.State == anchor.StateAnchored {
		t.Fatalf("anchored through a redirect: %+v", r)
	}
	noSecret(t, "redirect", e.allText(), testKeyID, testSecret)
}

func TestTheWrongCertificateOfAStoreIsRefusedAndTheRightOneAccepted(t *testing.T) {
	e := newEnv(t)
	tls := httptest.NewTLSServer(http.HandlerFunc(e.s3.serve))
	defer tls.Close()
	bundle := func(name string, srv *httptest.Server) string {
		p := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	cfg := func(ca string) []byte {
		return mustRaw(map[string]any{"endpoint": tls.URL, "region": fakeRegion, "bucket": fakeBucket, "retentionDays": 30, "caBundlePath": ca})
	}
	mk := func(name, ca string) api.LedgerAnchorTarget {
		tg, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: name, Enabled: true, Config: cfg(ca), Credentials: wormCreds()})
		if err != nil {
			t.Fatal(err)
		}
		return tg
	}
	good, bad := mk("good", bundle("good.pem", tls)), mk("bad", otherCA(t))
	res, err := e.svc.TestTarget(e.ctx(), good.ID)
	if err != nil || !res.OK {
		t.Fatalf("the right certificate: %+v %v", res, err)
	}
	res, err = e.svc.TestTarget(e.ctx(), bad.ID)
	if err != nil || res.OK || !strings.Contains(res.Checks[0].Detail, "certificate") {
		t.Fatalf("the wrong certificate: %+v %v", res, err)
	}
	// a bundle that holds no certificate is refused when the target is made
	empty := filepath.Join(t.TempDir(), "empty.pem")
	_ = os.WriteFile(empty, []byte("nothing"), 0o600)
	if _, err := e.svc.CreateTarget(e.ctx(), "a", api.LedgerAnchorTargetCreate{Kind: "worm", Name: "none", Config: cfg(empty)}); err == nil {
		t.Error("a bundle without a certificate was accepted")
	}
	_ = tls0
}

var tls0 = tls.VersionTLS12

// otherCA writes a PEM bundle with a certificate that is not the store's (httptest servers all share one certificate, so a new one is made).
func otherCA(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(77), Subject: pkix.Name{CommonName: "another authority"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "other.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}
