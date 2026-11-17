// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"encoding/json"
	"fmt"
	"time"
)

// State is the state of an anchor record (design 6.2).
type State string

// The states of an anchor record.
const (
	StatePending    State = "pending"
	StateSubmitted  State = "submitted"
	StateAnchored   State = "anchored"
	StateFailed     State = "failed"
	StateSuperseded State = "superseded"
	StateExpired    State = "expired"
)

// Valid reports whether s is one of the six states.
func (s State) Valid() bool {
	switch s {
	case StatePending, StateSubmitted, StateAnchored, StateFailed, StateSuperseded, StateExpired:
		return true
	}
	return false
}

// Terminal reports whether no automatic transition leaves s. (failed also
// leaves by the operator's Retry, see Record.Retry.)
func (s State) Terminal() bool {
	switch s {
	case StateAnchored, StateFailed, StateSuperseded, StateExpired:
		return true
	}
	return false
}

// ContractStatus maps the state to the contract's DigestAnchor.status:
// pending and submitted to "pending", anchored to "anchored", failed and
// expired to "failed"; superseded is not shown ("" and false).
func (s State) ContractStatus() (string, bool) {
	switch s {
	case StatePending, StateSubmitted:
		return "pending", true
	case StateAnchored:
		return "anchored", true
	case StateFailed, StateExpired:
		return "failed", true
	}
	return "", false
}

var allowed = map[State][]State{
	StatePending:   {StatePending, StateSubmitted, StateAnchored, StateFailed, StateSuperseded},
	StateSubmitted: {StateSubmitted, StateAnchored, StateFailed, StateExpired},
}

// CanTransition reports whether the automatic transition from -> to is allowed:
//
//	pending   -> pending (retry), submitted, anchored, failed, superseded
//	submitted -> submitted (upgrade not final yet), anchored, failed, expired
//
// Every other state is final. Retry (operator) is the one extra edge,
// failed -> pending.
func CanTransition(from, to State) bool {
	for _, t := range allowed[from] {
		if t == to {
			return true
		}
	}
	return false
}

// Record is the value of a row of kingres_cp.ledger_anchor. All methods are
// pure: they return a changed copy and touch nothing.
type Record struct {
	ID            int64
	TargetID      int64
	LogID         string
	Size          uint64
	PayloadSHA256 [32]byte
	State         State
	Attempts      int
	// NextAttemptAt is the next Submit time for pending records and the next
	// Upgrade time (Receipt.NextCheck) for submitted ones.
	NextAttemptAt *time.Time
	Evidence      json.RawMessage
	EvidenceTime  *time.Time
	AnchoredAt    *time.Time
	LastError     string
	CreatedAt     time.Time
}

// NewRecord returns a pending record due at once.
func NewRecord(targetID int64, logID string, size uint64, digest [32]byte, now time.Time) Record {
	n := now
	return Record{TargetID: targetID, LogID: logID, Size: size, PayloadSHA256: digest,
		State: StatePending, NextAttemptAt: &n, CreatedAt: now}
}

// Transition returns r moved to state to, or an error when not allowed.
func (r Record) Transition(to State) (Record, error) {
	if !CanTransition(r.State, to) {
		return r, fmt.Errorf("anchor: record cannot go from %s to %s", r.State, to)
	}
	r.State = to
	return r, nil
}

// Due reports whether the record is waiting for work at now.
func (r Record) Due(now time.Time) bool {
	return (r.State == StatePending || r.State == StateSubmitted) &&
		(r.NextAttemptAt == nil || !r.NextAttemptAt.After(now))
}

// Supersede marks a pending record replaced by a newer checkpoint. Only
// pending records are superseded; a submitted one keeps upgrading.
func (r Record) Supersede() (Record, error) { return r.Transition(StateSuperseded) }

// Retry is the operator's "Retry": a failed record becomes pending again,
// due at once, with the attempts and the error cleared.
func (r Record) Retry(now time.Time) (Record, error) {
	if r.State != StateFailed {
		return r, fmt.Errorf("anchor: only a failed record can be retried, not %s", r.State)
	}
	n := now
	r.State, r.Attempts, r.NextAttemptAt, r.LastError = StatePending, 0, &n, ""
	return r, nil
}

// ApplyReceipt applies the successful result of Submit or Upgrade.
func (r Record) ApplyReceipt(rc Receipt, now time.Time) (Record, error) {
	var to State
	switch rc.State {
	case ReceiptSubmitted:
		to = StateSubmitted
	case ReceiptAnchored:
		to = StateAnchored
	default:
		return r, fmt.Errorf("anchor: unknown receipt state %q", rc.State)
	}
	r, err := r.Transition(to)
	if err != nil {
		return r, err
	}
	r.Evidence = rc.Evidence
	if rc.EvidenceTime != nil {
		t := *rc.EvidenceTime
		r.EvidenceTime = &t
	}
	r.LastError = ""
	r.Attempts = 0
	if to == StateAnchored {
		n := now
		r.AnchoredAt, r.NextAttemptAt = &n, nil
	} else if rc.NextCheck != nil {
		t := *rc.NextCheck
		r.NextAttemptAt = &t
	}
	return r, nil
}

// Alert kinds that a failure raises (design 6.6).
const (
	AlertNone     = ""
	AlertFailed   = "ledgerAnchorFailed"
	AlertConflict = "ledgerAnchorConflict"
)

// Backoff constants (design 6.6).
const (
	BackoffBase   = 30 * time.Second
	BackoffMax    = time.Hour
	JitterPercent = 20
)

// RefusedRetryDelays are the waits after the 1st, 2nd and 3rd refusal
// (credentials, authorisation, configuration); the 4th refusal fails the record.
var RefusedRetryDelays = [3]time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// Backoff returns the wait after a retryable failure when the record had
// attempts failed attempts before this one: min(30 s * 2^attempts, 1 h) with
// jitter of +-20 percent; u is a uniform value in [0,1) from the caller's
// random source. A server's Retry-After wins when larger.
func Backoff(attempts int, u float64, retryAfter time.Duration) time.Duration {
	d := BackoffMax
	if attempts < 7 { // 30 s * 2^7 > 1 h
		if attempts < 0 {
			attempts = 0
		}
		d = BackoffBase << uint(attempts)
		if d > BackoffMax {
			d = BackoffMax
		}
	}
	if u < 0 {
		u = 0
	}
	if u >= 1 {
		u = 0.999999
	}
	f := 1 + float64(JitterPercent)/100*(2*u-1)
	d = time.Duration(float64(d) * f)
	if retryAfter > d {
		d = retryAfter
	}
	return d
}

// MaxLastError is the longest LastError stored.
const MaxLastError = 500

// AfterError applies the failure err of an attempt to the record, as table
// 6.6 says, and returns the alert to raise (AlertNone, AlertFailed,
// AlertConflict). Attempts counts every failed attempt since the last success.
//
//   - retryable: stays as it is (pending or submitted), attempts+1, next
//     attempt after Backoff.
//   - refused: attempts+1; the 1st, 2nd and 3rd refusal retry after
//     RefusedRetryDelays; the 4th fails the record (AlertFailed).
//   - permanent: failed (AlertFailed).
//   - conflict: failed (AlertConflict), no retry.
//
// u is a uniform random value in [0,1). The error text is stored cut to
// MaxLastError and in printable ASCII; the caller makes sure err holds no
// secret (the types of this package never do).
func (r Record) AfterError(err error, now time.Time, u float64) (Record, string) {
	r.LastError = (Redactor{}).Clean(err.Error(), MaxLastError)
	prev := r.Attempts
	r.Attempts++
	fail := func(alert string) (Record, string) {
		r.State, r.NextAttemptAt = StateFailed, nil
		return r, alert
	}
	switch ClassOf(err) {
	case ClassConflict:
		return fail(AlertConflict)
	case ClassRefused:
		if prev >= len(RefusedRetryDelays) {
			return fail(AlertFailed)
		}
		t := now.Add(RefusedRetryDelays[prev])
		r.NextAttemptAt = &t
		return r, AlertNone
	case ClassRetryable:
		t := now.Add(Backoff(prev, u, RetryAfterOf(err)))
		r.NextAttemptAt = &t
		return r, AlertNone
	}
	return fail(AlertFailed)
}

// UpgradeInterval is how often a submitted record is checked, by its age:
// every 10 minutes for the first 3 hours, then every hour.
func UpgradeInterval(age time.Duration) time.Duration {
	if age < 3*time.Hour {
		return 10 * time.Minute
	}
	return time.Hour
}

// ExpireIfDue moves a submitted record to expired when it is older than
// timeout (OpenTimestamps: upgradeTimeoutDays) and reports whether it did.
func (r Record) ExpireIfDue(now time.Time, timeout time.Duration) (Record, bool) {
	if r.State != StateSubmitted || now.Sub(r.CreatedAt) < timeout {
		return r, false
	}
	r.State, r.NextAttemptAt = StateExpired, nil
	return r, true
}
