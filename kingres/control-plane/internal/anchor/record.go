// SPDX-License-Identifier: AGPL-3.0-only

package anchor

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// State is the state of an anchor record (design 6.2).
type State string

// The states of an anchor record.
const (
	StatePending   State = "pending"
	StateSubmitted State = "submitted"
	// StateClaimed is an OpenTimestamps record whose calendars claim a Bitcoin
	// attestation that passed every check that needs no block header, and
	// that was NOT checked against a block header: "a calendar says so". It
	// is not anchored and must never be shown or counted as anchored. It
	// becomes anchored only through a receipt that carries a checked header.
	StateClaimed    State = "claimed"
	StateAnchored   State = "anchored"
	StateFailed     State = "failed"
	StateSuperseded State = "superseded"
	StateExpired    State = "expired"
)

// States lists the seven states, in a fixed order.
var States = []State{StatePending, StateSubmitted, StateClaimed, StateAnchored, StateFailed, StateSuperseded, StateExpired}

// Valid reports whether s is one of the seven states.
func (s State) Valid() bool {
	for _, x := range States {
		if x == s {
			return true
		}
	}
	return false
}

// Terminal reports whether no automatic event moves a record out of s.
// (failed also leaves by the operator's Retry, claimed by a receipt that
// carries a checked header.)
func (s State) Terminal() bool {
	switch s {
	case StateAnchored, StateFailed, StateSuperseded, StateExpired:
		return true
	}
	return false
}

// ContractStatus maps the state to the contract's DigestAnchor.status:
// pending, submitted and claimed to "pending" (a claim that nothing checked
// is not an anchor), anchored to "anchored", failed and expired to "failed";
// superseded is not shown ("" and false).
func (s State) ContractStatus() (string, bool) {
	switch s {
	case StatePending, StateSubmitted, StateClaimed:
		return "pending", true
	case StateAnchored:
		return "anchored", true
	case StateFailed, StateExpired:
		return "failed", true
	}
	return "", false
}

// Event is what can happen to a record. The whole behaviour of the state
// machine is the table transitions: every state and every event has either
// a next state or a refusal (Next reports false), and every method of
// Record that changes the state consults that table and nothing else.
type Event string

// The events.
const (
	// EventReceiptSubmitted: Submit or Upgrade returned a "submitted" receipt.
	EventReceiptSubmitted Event = "receipt_submitted"
	// EventReceiptClaimed: Upgrade returned a "claimed" receipt.
	EventReceiptClaimed Event = "receipt_claimed"
	// EventReceiptAnchored: a receipt in the state "anchored".
	EventReceiptAnchored Event = "receipt_anchored"
	// EventWait: a failed attempt that is tried again (a retryable error
	// within its limits, a refusal within its budget).
	EventWait Event = "error_wait"
	// EventFail: a failed attempt that ends the record (a permanent error,
	// the fourth refusal in a row, a retryable error past its limits).
	EventFail Event = "error_fail"
	// EventConflict: the store holds other content under the key.
	EventConflict Event = "error_conflict"
	// EventSupersede: a newer checkpoint replaced a record not yet submitted.
	EventSupersede Event = "supersede"
	// EventExpire: an OpenTimestamps record waited too long for Bitcoin.
	EventExpire Event = "expire"
	// EventRetry: the operator's "Retry" of a failed record.
	EventRetry Event = "operator_retry"
)

// Events lists the nine events, in a fixed order.
var Events = []Event{EventReceiptSubmitted, EventReceiptClaimed, EventReceiptAnchored, EventWait, EventFail,
	EventConflict, EventSupersede, EventExpire, EventRetry}

// transitions is the state machine (state, event) -> next state. A pair that
// is not in the table is refused: the record is returned unchanged with an
// error. anchored, superseded and expired are final for every event; failed
// is left only by EventRetry.
var transitions = map[State]map[Event]State{
	StatePending: {
		EventReceiptSubmitted: StateSubmitted,
		EventReceiptAnchored:  StateAnchored,
		EventWait:             StatePending,
		EventFail:             StateFailed,
		EventConflict:         StateFailed,
		EventSupersede:        StateSuperseded,
	},
	StateSubmitted: {
		EventReceiptSubmitted: StateSubmitted,
		EventReceiptClaimed:   StateClaimed,
		EventReceiptAnchored:  StateAnchored,
		EventWait:             StateSubmitted,
		EventFail:             StateFailed,
		EventConflict:         StateFailed,
		EventExpire:           StateExpired,
	},
	StateClaimed: {
		EventReceiptClaimed:  StateClaimed,
		EventReceiptAnchored: StateAnchored,
		EventWait:            StateClaimed,
		EventFail:            StateFailed,
		EventConflict:        StateFailed,
	},
	StateFailed: {
		EventRetry: StatePending,
	},
}

// Next returns the state that event ev leads to from s, and false when the
// pair is refused.
func Next(s State, ev Event) (State, bool) {
	to, ok := transitions[s][ev]
	return to, ok
}

// Record is the value of a row of kingres_cp.ledger_anchor. All methods are
// pure: they return a changed copy and touch nothing; a method that refuses
// returns the record unchanged and an error.
type Record struct {
	ID            int64
	TargetID      int64
	LogID         string
	Size          uint64
	PayloadSHA256 [32]byte
	State         State
	// Attempts counts the failed attempts since the last success (any class).
	// It is what the attempt limit bounds.
	Attempts int
	// Refusals counts the consecutive refusals; any other failure and any
	// success sets it to 0. The fourth refusal in a row fails the record.
	Refusals int
	// Retryables counts the consecutive retryable failures (the exponent of
	// the backoff); any other failure and any success sets it to 0.
	Retryables int
	// FailingSince is the time of the first failed attempt since the last
	// success (nil when the last attempt succeeded). The age limit counts
	// from it.
	FailingSince *time.Time
	// NextAttemptAt is the next Submit time for pending records and the next
	// Upgrade time (Receipt.NextCheck) for submitted and claimed ones.
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

// step returns r moved by ev, or r and an error when the table refuses.
func (r Record) step(ev Event) (Record, error) {
	to, ok := Next(r.State, ev)
	if !ok {
		return r, fmt.Errorf("%w: a record in state %q cannot take the event %q", ErrNotAllowed, r.State, ev)
	}
	r.State = to
	return r, nil
}

// Due reports whether the record is waiting for work at now: pending and
// submitted records whose NextAttemptAt has come (or is not set), and claimed
// records whose NextAttemptAt is set and has come (the receipt asked for a
// new check because a header source was unavailable). A claimed record with
// no NextAttemptAt is never due: a header source configured later is applied
// by an explicit Upgrade.
func (r Record) Due(now time.Time) bool {
	switch r.State {
	case StatePending, StateSubmitted:
		return r.NextAttemptAt == nil || !r.NextAttemptAt.After(now)
	case StateClaimed:
		return r.NextAttemptAt != nil && !r.NextAttemptAt.After(now)
	}
	return false
}

// Supersede marks a pending record replaced by a newer checkpoint. Only
// pending records are superseded; a submitted one keeps upgrading.
func (r Record) Supersede() (Record, error) {
	out, err := r.step(EventSupersede)
	if err != nil {
		return r, err
	}
	out.NextAttemptAt = nil
	return out, nil
}

// Retry is the operator's "Retry": a failed record becomes pending again,
// due at once, with the counters and the error cleared.
func (r Record) Retry(now time.Time) (Record, error) {
	out, err := r.step(EventRetry)
	if err != nil {
		return r, err
	}
	n := now
	out.Attempts, out.Refusals, out.Retryables, out.FailingSince = 0, 0, 0, nil
	out.NextAttemptAt, out.LastError = &n, ""
	return out, nil
}

// ApplyReceipt applies the successful result of Submit or Upgrade. It
// refuses (error, record unchanged) a receipt that the table does not allow
// from the record's state: nothing leaves anchored, and a claimed record is
// not made submitted again.
func (r Record) ApplyReceipt(rc Receipt, now time.Time) (Record, error) {
	var ev Event
	switch rc.State {
	case ReceiptSubmitted:
		ev = EventReceiptSubmitted
	case ReceiptClaimed:
		ev = EventReceiptClaimed
	case ReceiptAnchored:
		ev = EventReceiptAnchored
	default:
		return r, fmt.Errorf("anchor: unknown receipt state %q", rc.State)
	}
	out, err := r.step(ev)
	if err != nil {
		return r, err
	}
	out.Evidence = rc.Evidence
	if rc.EvidenceTime != nil {
		t := *rc.EvidenceTime
		out.EvidenceTime = &t
	}
	out.LastError = ""
	out.Attempts, out.Refusals, out.Retryables, out.FailingSince = 0, 0, 0, nil
	switch {
	case out.State == StateAnchored:
		n := now
		out.AnchoredAt, out.NextAttemptAt = &n, nil
	case rc.NextCheck != nil:
		t := *rc.NextCheck
		out.NextAttemptAt = &t
	default:
		out.NextAttemptAt = nil
	}
	return out, nil
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

// MaxRefusals is how many refusals in a row are waited out; the next one
// (the fourth) fails the record.
const MaxRefusals = 3

// RefusedRetryDelay is the wait after the nth refusal in a row (n = 1, 2, 3:
// 1, 5 and 15 minutes; credentials and permissions may take time to
// propagate); it is 0 for any other n, because the fourth fails the record.
func RefusedRetryDelay(n int) time.Duration {
	switch n {
	case 1:
		return time.Minute
	case 2:
		return 5 * time.Minute
	case 3:
		return 15 * time.Minute
	}
	return 0
}

// Defaults of Limits.
const (
	DefaultMaxAttempts = 200
	DefaultMaxAge      = 7 * 24 * time.Hour
)

// Limits bound the retryable failures of a record (design 6.6 and the
// settings anchor.maxRetryAttempts and anchor.maxRetryAgeHours of the
// wiring). A zero field means its default. When the limit is reached the
// record is failed with the alert AlertFailed and its last error: a target
// that answers 503 for ever, or asks for a day's wait for ever, does not keep
// a record pending for ever.
type Limits struct {
	// MaxAttempts is the most failed attempts since the last success (the
	// failure that makes the count exceed it fails the record).
	MaxAttempts int
	// MaxAge is how long the failures may go on without one success (from
	// the first failed attempt of the streak).
	MaxAge time.Duration
}

// WithDefaults returns l with the zero fields replaced by the defaults.
func (l Limits) WithDefaults() Limits {
	if l.MaxAttempts <= 0 {
		l.MaxAttempts = DefaultMaxAttempts
	}
	if l.MaxAge <= 0 {
		l.MaxAge = DefaultMaxAge
	}
	return l
}

// Backoff returns the wait after a retryable failure when the record had
// attempts consecutive retryable failures before this one: min(30 s *
// 2^attempts, 1 h) with jitter of +-20 percent; u is a uniform value in [0,1)
// from the caller's random source. A server's Retry-After wins when larger.
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

// ErrNotAllowed is wrapped by the error of every refused transition.
var ErrNotAllowed = errors.New("anchor: transition not allowed")

// AfterError applies the failure err of an attempt to the record, as table
// 6.6 says, and returns the alert to raise (AlertNone, AlertFailed,
// AlertConflict). It refuses, with an error and the record unchanged and no
// alert, a record that is not pending, submitted or claimed (anchored,
// superseded, expired and failed records are not touched by a late failure of
// a stale job), and a nil err.
//
//   - retryable: the record stays as it is, Attempts and Retryables +1,
//     Refusals 0, the next attempt after Backoff(Retryables before this one)
//     and RetryAfterOf when larger; when Attempts exceeds lim.MaxAttempts or
//     the failures have gone on for lim.MaxAge the record is failed
//     (AlertFailed) instead.
//   - refused: Attempts and Refusals +1, Retryables 0; the 1st, 2nd and 3rd
//     refusal in a row retry after RefusedRetryDelay; the 4th fails the
//     record (AlertFailed). Any other failure or success resets Refusals.
//   - permanent: failed (AlertFailed).
//   - conflict: failed (AlertConflict), no retry.
//
// u is a uniform random value in [0,1). The error text is stored cut to
// MaxLastError and in printable ASCII; the caller makes sure err holds no
// secret (the types of this package never do).
func (r Record) AfterError(err error, now time.Time, u float64, lim Limits) (Record, string, error) {
	if err == nil {
		return r, AlertNone, errors.New("anchor: AfterError needs an error")
	}
	if _, ok := Next(r.State, EventWait); !ok {
		return r, AlertNone, fmt.Errorf("%w: a record in state %q takes no failure", ErrNotAllowed, r.State)
	}
	lim = lim.WithDefaults()
	out := r
	out.LastError = (Redactor{}).Clean(err.Error(), MaxLastError)
	out.Attempts++
	if out.FailingSince == nil {
		n := now
		out.FailingSince = &n
	}
	ev := EventWait
	alert := AlertNone
	var wait time.Duration
	switch ClassOf(err) {
	case ClassConflict:
		ev, alert = EventConflict, AlertConflict
	case ClassRefused:
		out.Refusals++
		out.Retryables = 0
		if out.Refusals > MaxRefusals {
			ev, alert = EventFail, AlertFailed
		} else {
			wait = RefusedRetryDelay(out.Refusals)
		}
	case ClassRetryable:
		wait = Backoff(out.Retryables, u, RetryAfterOf(err))
		out.Retryables++
		out.Refusals = 0
		if out.Attempts > lim.MaxAttempts || now.Sub(*out.FailingSince) >= lim.MaxAge {
			ev, alert = EventFail, AlertFailed
		}
	default:
		out.Refusals, out.Retryables = 0, 0
		ev, alert = EventFail, AlertFailed
	}
	out, serr := out.step(ev)
	if serr != nil { // cannot happen: the table has the pair for every state tested above
		return r, AlertNone, serr
	}
	if ev == EventWait {
		t := now.Add(wait)
		out.NextAttemptAt = &t
	} else {
		out.NextAttemptAt = nil
	}
	return out, alert, nil
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
// Only a submitted record expires: a claimed one has its claim already.
func (r Record) ExpireIfDue(now time.Time, timeout time.Duration) (Record, bool) {
	if r.State != StateSubmitted || now.Sub(r.CreatedAt) < timeout {
		return r, false
	}
	out, err := r.step(EventExpire)
	if err != nil {
		return r, false
	}
	out.NextAttemptAt = nil
	return out, true
}
