// SPDX-License-Identifier: PostgreSQL

package membership

import (
	"context"
	"crypto/x509"
	"errors"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

var jobT0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// jobFixture is a job whose certificate lives 100 seconds and whose clock the
// test moves.
type jobFixture struct {
	job     *RenewalJob
	cert    *x509.Certificate
	now     time.Time
	renews  int
	failFor int // the next failFor renewals fail
	raised  []monitor.Condition
	cleared int
	events  []audit.Event
}

func newJobFixture(t *testing.T) *jobFixture {
	t.Helper()
	ca, err := pki.CreateCA(t.TempDir(), "t", jobT0)
	if err != nil {
		t.Fatal(err)
	}
	ca.SetServerLifetime(100 * time.Second)
	names := pki.Names{CommonName: "srv1"}
	_, csr, err := pki.NewKeyAndCSR(names)
	if err != nil {
		t.Fatal(err)
	}
	crt, _, err := ca.Sign(csr, names, jobT0)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := pki.ParseCertPEM(crt)
	if err != nil {
		t.Fatal(err)
	}
	f := &jobFixture{cert: cert, now: jobT0}
	f.job = &RenewalJob{Self: "srv1", Fraction: 0.3, Check: 10 * time.Second, Retry: time.Second,
		Now:  func() time.Time { return f.now },
		Cert: func() (*x509.Certificate, error) { return f.cert, nil },
		Renew: func(context.Context) (time.Time, error) {
			f.renews++
			if f.failFor > 0 {
				f.failFor--
				return time.Time{}, &peerclient.APIError{Status: 503, Code: "ca_unavailable"}
			}
			_, csr, _ := pki.NewKeyAndCSR(names)
			c2, _, _ := ca.Sign(csr, names, f.now)
			f.cert, _ = pki.ParseCertPEM(c2)
			return f.cert.NotAfter, nil
		},
		Raise: func(_ context.Context, c monitor.Condition) error { f.raised = append(f.raised, c); return nil },
		Clear: func(context.Context) error { f.cleared++; return nil },
		Audit: func(_ context.Context, ev audit.Event) { f.events = append(f.events, ev) },
	}
	return f
}

func TestRenewalJobDoesNothingBeforeTheWindow(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(10 * time.Second) // 90 % left
	wait := f.job.Once(context.Background())
	if f.renews != 0 {
		t.Fatalf("renewed %d times with 90%% of the lifetime left", f.renews)
	}
	// The window starts at 70 s (30 % of 100 s left, lifetime counted from the back-dated start):
	// the job looks again before it, not a full check interval later.
	if wait <= 0 || wait > f.job.Check {
		t.Errorf("wait = %v", wait)
	}
}

func TestRenewalJobRenewsInsideTheWindowOnceAndInstallsTheNewCertificate(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(80 * time.Second) // 20 % left
	old := f.cert.SerialNumber.String()
	f.job.Once(context.Background())
	if f.renews != 1 || f.cert.SerialNumber.String() == old {
		t.Fatalf("renews = %d, serial changed = %v", f.renews, f.cert.SerialNumber.String() != old)
	}
	// The new certificate is young: the next round does nothing.
	f.job.Once(context.Background())
	if f.renews != 1 || len(f.raised) != 0 || len(f.events) != 0 {
		t.Errorf("renews %d, alerts %v, events %v", f.renews, f.raised, f.events)
	}
}

func TestRenewalJobRetriesAlertsAfterThreeFailuresAndClearsOnSuccess(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(80 * time.Second)
	f.failFor = 3
	var waits []time.Duration
	for i := 0; i < 3; i++ {
		waits = append(waits, f.job.Once(context.Background()))
		switch i {
		case 0, 1:
			if len(f.raised) != 0 {
				t.Fatalf("alert after %d failures", i+1)
			}
		case 2:
			if len(f.raised) != 1 {
				t.Fatalf("no alert after three failures")
			}
		}
	}
	if waits[0] != time.Second || waits[1] != 2*time.Second || waits[2] != 4*time.Second {
		t.Errorf("retry delays %v, want 1s 2s 4s", waits)
	}
	c := f.raised[0]
	if c.Kind != "certificateRenewalFailed" || c.Subject != "server:srv1" || c.Severity != "warning" {
		t.Errorf("alert %+v", c)
	}
	if len(f.events) != 1 || f.events[0].Action != "certificateRenewalFailed" || f.events[0].Outcome != audit.OutcomeFailure {
		t.Errorf("audit after the first failure: %+v", f.events)
	}
	// The fourth attempt succeeds: the alert is cleared, the streak audited as recovered.
	f.job.Once(context.Background())
	if f.cleared != 1 || f.job.Failures() != 0 {
		t.Errorf("cleared %d, failures %d", f.cleared, f.job.Failures())
	}
	if len(f.events) != 2 || f.events[1].Details["phase"] != "recovered" || f.events[1].Details["failedAttempts"] != 3 {
		t.Errorf("audit after the recovery: %+v", f.events)
	}
	// The delay starts over after a recovery.
	f.now = f.now.Add(70 * time.Second)
	f.failFor = 1
	if w := f.job.Once(context.Background()); w != time.Second {
		t.Errorf("first retry delay after a recovery = %v", w)
	}
}

func TestRenewalJobAlertIsCriticalWhenAlmostExpired(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(95 * time.Second) // 5 % left
	f.failFor = 99
	for i := 0; i < 3; i++ {
		f.job.Once(context.Background())
	}
	if len(f.raised) != 1 || f.raised[0].Severity != "critical" {
		t.Errorf("alerts %+v", f.raised)
	}
}

func TestRenewalJobIgnoresAServerWithoutACertificate(t *testing.T) {
	f := newJobFixture(t)
	f.job.Cert = func() (*x509.Certificate, error) { return nil, cluster.ErrNoCertificate }
	f.job.Once(context.Background())
	if f.renews != 0 || len(f.raised) != 0 {
		t.Error("a server that is not in a cluster renewed or alerted")
	}
}

func TestRenewalJobClearsTheAlertWhenTheCertificateWasRenewedByOtherMeans(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(80 * time.Second)
	f.failFor = 99
	for i := 0; i < 3; i++ {
		f.job.Once(context.Background())
	}
	// An administrator renews the certificate (operation renewServerCertificates).
	f.failFor = 0
	_ = f.job.Renew
	f.cert, _ = func() (*x509.Certificate, error) {
		names := pki.Names{CommonName: "srv1"}
		ca, _ := pki.CreateCA(t.TempDir(), "x", jobT0)
		ca.SetServerLifetime(100 * time.Second)
		_, csr, _ := pki.NewKeyAndCSR(names)
		c, _, _ := ca.Sign(csr, names, f.now)
		return pki.ParseCertPEM(c)
	}()
	renews := f.renews
	f.job.Once(context.Background())
	if f.renews != renews || f.cleared != 1 {
		t.Errorf("renews %d -> %d, cleared %d", renews, f.renews, f.cleared)
	}
}

func TestRenewalJobErrorTextIsBoundedAndCarriesNoPeerText(t *testing.T) {
	long := errors.New(string(make([]byte, 5000)))
	if n := len(errorText(long)); n > 200 {
		t.Errorf("error text of %d bytes", n)
	}
	if got := errorText(&peerclient.APIError{Status: 503, Code: "ca_unavailable", Message: "peer text"}); got != "the manager leader answered 503 ca_unavailable" {
		t.Errorf("text %q", got)
	}
}

// CP2-04: the revocation list is valid for 30 days; the job of the server that
// holds the CA issues it again when it is a third of that old, whatever the
// certificates need, and again after that; a server without the CA never does.
func TestRenewalJobIssuesTheRevocationListAgainBeforeItEnds(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(5 * time.Second) // the certificate needs nothing
	issued := jobT0
	holdsCA := true
	refreshes := 0
	f.job.CRLThisUpdate = func() (time.Time, bool) { return issued, holdsCA }
	f.job.RefreshCRL = func(context.Context) error {
		refreshes++
		issued = f.now
		return nil
	}
	f.job.Once(context.Background())
	if refreshes != 0 {
		t.Fatalf("a young list was issued again (%d)", refreshes)
	}
	f.now = jobT0.Add(pki.CRLRefreshAfter - time.Hour)
	f.job.Once(context.Background())
	if refreshes != 0 {
		t.Fatalf("issued again before a third of its validity")
	}
	f.now = jobT0.Add(pki.CRLRefreshAfter + time.Minute)
	f.job.Once(context.Background())
	f.job.Once(context.Background()) // the same round again: the list is young now
	if refreshes != 1 {
		t.Fatalf("issued %d times at a third of its validity, want 1", refreshes)
	}
	// Months pass with a job that runs every hour: a new list well before each end.
	for h := 0; h < 24*100; h++ {
		f.now = f.now.Add(time.Hour)
		if age := f.now.Sub(issued); age >= pki.CRLValidity {
			t.Fatalf("the list ended (age %v) without being issued again", age)
		}
		f.job.Once(context.Background())
	}
	if refreshes < 9 {
		t.Errorf("%d new lists in 100 days", refreshes)
	}
	// A failure is retried at the next round, the list is still valid.
	failing := true
	f.job.RefreshCRL = func(context.Context) error {
		refreshes++
		if failing {
			return errors.New("a server cannot be reached")
		}
		issued = f.now
		return nil
	}
	f.now = issued.Add(pki.CRLRefreshAfter + time.Minute)
	before := refreshes
	f.job.Once(context.Background())
	f.job.Once(context.Background())
	if refreshes-before != 2 {
		t.Errorf("a failed refresh was not tried again: %d calls", refreshes-before)
	}
	failing = false
	f.job.Once(context.Background())
	f.job.Once(context.Background())
	if refreshes-before != 3 {
		t.Errorf("after the success it was tried again: %d calls", refreshes-before)
	}
	// A server that does not hold the CA does not issue lists.
	holdsCA = false
	f.now = f.now.Add(60 * 24 * time.Hour)
	n := refreshes
	f.job.Once(context.Background())
	if refreshes != n {
		t.Error("a server without the CA issued a list")
	}
}

// An alert that a previous process raised is resolved by the first round of the
// next one that sees a good certificate (the job has no memory of it), once.
func TestRenewalJobResolvesTheAlertOfAPreviousProcessOnce(t *testing.T) {
	f := newJobFixture(t)
	f.now = jobT0.Add(5 * time.Second) // nothing to renew
	f.job.Once(context.Background())
	f.job.Once(context.Background())
	f.job.Once(context.Background())
	if f.cleared != 1 {
		t.Errorf("the alert was resolved %d times, want 1", f.cleared)
	}
	// A failed clearing is tried again at the next round.
	g := newJobFixture(t)
	g.now = jobT0.Add(5 * time.Second)
	calls := 0
	g.job.Clear = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("the database is not up yet")
		}
		return nil
	}
	g.job.Once(context.Background())
	g.job.Once(context.Background())
	g.job.Once(context.Background())
	if calls != 2 {
		t.Errorf("clear called %d times, want 2 (one failure, one success)", calls)
	}
}
