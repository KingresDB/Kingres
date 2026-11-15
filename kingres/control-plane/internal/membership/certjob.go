// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// AlertRenewalFailed is the alert kind of a renewal job that keeps failing
// (the contract's AlertKind certificateRenewalFailed, from 0.5.0 on).
const AlertRenewalFailed = "certificateRenewalFailed"

// FailuresBeforeAlert is the number of renewal attempts in a row that fail
// before the job raises an alert. One failure is usually the leader being
// restarted; the job tries again after the retry interval.
const FailuresBeforeAlert = 3

// RenewalJob renews the certificates of this server before they expire. It is
// a loop with injected steps so that the logic is tested without a cluster.
//
// Each round: read this server's certificate; when less than Fraction of its
// lifetime is left, ask the manager leader (the CA holder) for a new one and
// install it; on a failure try again after Retry (doubling up to Check); after
// FailuresBeforeAlert failures in a row raise the alert certificateRenewalFailed
// and clear it with the first success. Every streak of failures and its end are
// audited.
type RenewalJob struct {
	// Self is this server's id.
	Self string
	// Fraction, Check and Retry are the configuration (see package config).
	Fraction     float64
	Check, Retry time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Cert reads this server's certificate; cluster.ErrNoCertificate when it
	// has none (not part of a cluster).
	Cert func() (*x509.Certificate, error)
	// Renew renews the certificate; it returns the new end of validity.
	Renew func(ctx context.Context) (time.Time, error)
	// Raise sets the alert (condition) and Clear resolves it.
	Raise func(ctx context.Context, cond monitor.Condition) error
	Clear func(ctx context.Context) error
	// Audit records an audit event; nil means none.
	Audit func(ctx context.Context, ev audit.Event)
	Log   *slog.Logger
	// CRLThisUpdate returns when the revocation list on this server was issued; ok is
	// false on a server that does not hold the CA (only the CA holder issues the
	// list). RefreshCRL issues a new list and sends it to every server and instance.
	// The list is valid for pki.CRLValidity (30 days) and PostgreSQL refuses a list
	// past its end, so it is issued again when it is older than CRLRefreshAfter.
	CRLThisUpdate func() (t time.Time, ok bool)
	RefreshCRL    func(ctx context.Context) error
	// CRLRefreshAfter is the age at which the list is issued again (default pki.CRLRefreshAfter).
	CRLRefreshAfter time.Duration

	failures  int
	delay     time.Duration
	lastError string
	alerted   bool
	// cleared says that the alert is known to be resolved: a job that starts has no memory of the
	// alert a previous process raised, so it resolves it once, when it first sees a good certificate.
	cleared bool
}

func (j *RenewalJob) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now()
}

func (j *RenewalJob) log() *slog.Logger {
	if j.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return j.Log
}

// Failures is the number of renewal attempts in a row that failed.
func (j *RenewalJob) Failures() int { return j.failures }

// Run loops until ctx ends.
func (j *RenewalJob) Run(ctx context.Context) {
	for {
		wait := j.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Once runs one round and returns how long to wait for the next.
func (j *RenewalJob) Once(ctx context.Context) time.Duration {
	j.refreshCRL(ctx)
	cert, err := j.Cert()
	switch {
	case errors.Is(err, cluster.ErrNoCertificate):
		return j.Check
	case err != nil:
		j.fail(ctx, nil, fmt.Errorf("reading the certificate: %w", err))
		return j.nextDelay()
	}
	if !pki.NeedsRenewalAt(cert, j.now(), j.Fraction) {
		if j.failures > 0 || j.alerted || !j.cleared {
			// Renewed by an administrator meanwhile, or the first round of a process.
			j.recovered(ctx, "the certificate was renewed by other means")
		}
		return j.untilWindow(cert)
	}
	notAfter, err := j.Renew(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return j.Check
		}
		j.fail(ctx, cert, err)
		return j.nextDelay()
	}
	j.log().Info("certificate renewed", "not_after", notAfter.UTC().Format(time.RFC3339))
	if j.failures > 0 || j.alerted || !j.cleared {
		j.recovered(ctx, "renewed")
	}
	return j.untilWindow(nil)
}

// refreshCRL issues the revocation list again when it is old (CP2-04). A failure is
// logged and tried again at the next round, which comes long before the list ends.
func (j *RenewalJob) refreshCRL(ctx context.Context) {
	if j.CRLThisUpdate == nil || j.RefreshCRL == nil {
		return
	}
	this, ok := j.CRLThisUpdate()
	after := j.CRLRefreshAfter
	if after <= 0 {
		after = pki.CRLRefreshAfter
	}
	if !ok || j.now().Sub(this) < after {
		return
	}
	if err := j.RefreshCRL(ctx); err != nil {
		j.log().Warn("issuing the revocation list again failed", "error", err)
		return
	}
	j.log().Info("revocation list issued again", "previous", this.UTC().Format(time.RFC3339))
}

// untilWindow is how long to wait: the check interval, shortened so that the
// start of the renewal window is not missed by more than a tenth of a short
// lifetime (matters for tests and for lifetimes of minutes).
func (j *RenewalJob) untilWindow(cert *x509.Certificate) time.Duration {
	d := j.Check
	if cert != nil {
		life := cert.NotAfter.Sub(cert.NotBefore)
		start := cert.NotAfter.Add(-time.Duration(float64(life) * j.Fraction))
		if until := start.Sub(j.now()); until > 0 && until < d {
			d = until + 10*time.Millisecond
		}
	}
	return d
}

func (j *RenewalJob) nextDelay() time.Duration {
	if j.delay == 0 {
		j.delay = j.Retry
	} else {
		j.delay = min(j.delay*2, j.Check)
	}
	return j.delay
}

func (j *RenewalJob) fail(ctx context.Context, cert *x509.Certificate, err error) {
	j.failures++
	j.lastError = errorText(err)
	j.log().Warn("certificate renewal failed", "attempt", j.failures, "error", err)
	if j.failures == 1 {
		j.audit(ctx, audit.OutcomeFailure, map[string]any{"reason": j.lastError, "phase": "started"})
	}
	if j.failures >= FailuresBeforeAlert && j.Raise != nil {
		cond := monitor.Condition{Kind: AlertRenewalFailed, Severity: "warning", Subject: "server:" + j.Self,
			Message: fmt.Sprintf("Server %s could not renew its certificates in %d attempts in a row: %s.", j.Self, j.failures, j.lastError)}
		if cert != nil {
			cond.Message += " They expire at " + cert.NotAfter.UTC().Format(time.RFC3339) + "."
			if pki.RemainingFraction(cert, j.now()) < 0.1 {
				cond.Severity = "critical"
			}
		}
		if err := j.Raise(ctx, cond); err != nil {
			j.log().Warn("raising the certificate renewal alert", "error", err)
		} else {
			j.alerted, j.cleared = true, false
		}
	}
}

func (j *RenewalJob) recovered(ctx context.Context, how string) {
	if j.failures > 0 {
		j.audit(ctx, audit.OutcomeSuccess, map[string]any{"phase": "recovered", "failedAttempts": j.failures, "how": how})
	}
	if (j.alerted || !j.cleared) && j.Clear != nil {
		if err := j.Clear(ctx); err != nil {
			j.log().Warn("clearing the certificate renewal alert", "error", err)
			return // tried again by the next round
		}
	}
	j.failures, j.delay, j.lastError, j.alerted, j.cleared = 0, 0, "", false, true
}

func (j *RenewalJob) audit(ctx context.Context, outcome string, details map[string]any) {
	if j.Audit == nil {
		return
	}
	j.Audit(ctx, audit.Event{Actor: "system", Source: "system", Action: AuditCertificateRenewalFailed, ResourceType: "server", ResourceID: j.Self,
		Outcome: outcome, Details: details})
}

// errorText is a short text of an error that is safe to store (the text of
// another server is bounded and cleaned by the peer client already).
func errorText(err error) string {
	var ae *peerclient.APIError
	var p *problem.Problem
	switch {
	case errors.As(err, &ae):
		return fmt.Sprintf("the manager leader answered %d %s", ae.Status, ae.Code)
	case errors.As(err, &p):
		return string(p.Code)
	case errors.Is(err, peerclient.ErrUnreachable):
		return "the manager leader cannot be reached"
	}
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// NewRenewalJob builds the job of this server from the configuration.
func (m *Manager) NewRenewalJob(fraction float64, check, retry time.Duration) *RenewalJob {
	j := &RenewalJob{Self: m.Self, Fraction: fraction, Check: check, Retry: retry, Log: m.log(),
		Cert:  m.Svc.ServerCertificate,
		Audit: func(ctx context.Context, ev audit.Event) { m.auditEvent(ctx, ev) },
	}
	j.CRLThisUpdate = m.Svc.CRLThisUpdate
	j.RefreshCRL = m.distributeRevocations
	j.Renew = func(ctx context.Context) (time.Time, error) {
		lctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		leaderID, err := m.Leaders.Get(lctx)
		cancel()
		if err != nil || leaderID == "" {
			return time.Time{}, errors.New("the manager leader is not known")
		}
		rctx, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		return m.RenewViaLeader(rctx, leaderID)
	}
	j.Raise = func(ctx context.Context, cond monitor.Condition) error {
		return m.reconcileAlert(ctx, []monitor.Condition{cond}, map[string]bool{AlertRenewalFailed: true})
	}
	j.Clear = func(ctx context.Context) error {
		return m.reconcileAlert(ctx, nil, map[string]bool{AlertRenewalFailed: true})
	}
	return j
}

// reconcileAlert writes this server's own alert table (its local copy of the
// system database; a server whose leader cannot be reached still raises it).
func (m *Manager) reconcileAlert(ctx context.Context, conds []monitor.Condition, evaluated map[string]bool) error {
	c, err := m.Svc.ServiceConn(ctx, sysdb.Database)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return monitor.Reconcile(ctx, c, m.Self, conds, evaluated, time.Now())
}

// auditEvent records an event, if the manager has an audit sink.
func (m *Manager) auditEvent(ctx context.Context, ev audit.Event) {
	if m.Audit != nil {
		m.Audit(ctx, ev)
	}
}
