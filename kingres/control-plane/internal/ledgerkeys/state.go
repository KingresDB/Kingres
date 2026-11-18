// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// State is the state of a signing key (design 3.3).
type State uint8

// The states of a key. The zero State is invalid.
const (
	// StateNext: generated, not yet signing. It may sign only the statement
	// that proves it holds its key (the key-added event).
	StateNext State = iota + 1
	// StateActive: signs checkpoints. At most two keys are active at a time.
	StateActive
	// StateRetired: signs nothing more; what it signed stays valid.
	StateRetired
	// StateRevoked: compromised; its signatures are not trusted unless
	// anchors show they are older than the compromise.
	StateRevoked
)

var stateNames = [...]string{"", "next", "active", "retired", "revoked"}

// String returns the name of the state as the system database stores it.
func (s State) String() string {
	if s >= 1 && int(s) < len(stateNames) {
		return stateNames[s]
	}
	return "invalid"
}

// ParseState parses the name of a state.
func ParseState(name string) (State, error) {
	for i := 1; i < len(stateNames); i++ {
		if stateNames[i] == name {
			return State(i), nil
		}
	}
	return 0, fmt.Errorf("ledgerkeys: %q is not a key state, expected next, active, retired or revoked", clip(name))
}

// Limits of the key set.
const (
	// MaxKeys is the most keys one set holds: the most the trust file the
	// verifier reads allows (retired keys count).
	MaxKeys = 16
	// MaxActive is the most keys that sign at a time (an overlap).
	MaxActive = 2
	// MinOverlapDays and MaxOverlapDays bound the overlap of a rotation.
	MinOverlapDays = 1
	MaxOverlapDays = 365
	// DefaultOverlapDays is the default overlap.
	DefaultOverlapDays = 30
	// MaxReasonSize and MaxLabelSize bound the free text of a key.
	MaxReasonSize = 200
	MaxLabelSize  = 200
	// MaxTrustTreeSize is the largest max_tree_size the trust file may hold:
	// the verifier reads that member as an integer from 1 to 2^62-1 and
	// refuses the whole file for any other value.
	MaxTrustTreeSize uint64 = 1<<62 - 1
	// MaxActivationLeadDays is how far after the latest time the set records
	// (a creation, activation, retirement or revocation of any key) Activate
	// accepts an activation time: 366 days. See Activate.
	MaxActivationLeadDays = 366
)

// Errors of transitions. A caller tests them with errors.Is.
var (
	ErrUnknownKey      = errors.New("ledgerkeys: no such key")
	ErrDuplicateKey    = errors.New("ledgerkeys: the key is already in the set")
	ErrTransition      = errors.New("ledgerkeys: the transition is not allowed from this state")
	ErrTooManyKeys     = errors.New("ledgerkeys: the set holds the most keys the trust file allows")
	ErrTooManyActive   = errors.New("ledgerkeys: two keys are already active")
	ErrNoOtherSigner   = errors.New("ledgerkeys: the change needs another active key to sign it")
	ErrInvalidArgument = errors.New("ledgerkeys: invalid argument")
	ErrNotAllowed      = errors.New("ledgerkeys: the key may not sign this")
	// ErrInvalidTime marks a time that a transition refuses: zero, outside the
	// years 1 to 9999 (the trust file cannot write others), or an activation
	// too far after everything the set records. It is also an
	// ErrInvalidArgument.
	ErrInvalidTime = errors.New("ledgerkeys: invalid time")
	// ErrNoActiveKey: no key of the set may sign checkpoints. Signers returns
	// it when no key is active at the time asked. Validate returns it for a
	// set that has been in service (some key has left the state next) and
	// holds no active key. An empty set, and a set whose keys are all next
	// (before its first activation), are legal and do not return it.
	ErrNoActiveKey = errors.New("ledgerkeys: no key of the set is active")
)

// TreeBound is the decision about the largest tree a retired key may still
// sign, which Retire needs: either a size (AtMost) or an explicit Unbounded.
// The zero TreeBound is no decision and Retire refuses it, so a bound is never
// left out by passing a bare 0.
type TreeBound struct {
	kind uint8 // 0: no decision, 1: bounded, 2: unbounded
	size uint64
}

// AtMost bounds a retired key to checkpoints of at most size leaves: the trust
// file gets max_tree_size = size, which the verifier enforces. size must be 1 to
// MaxTrustTreeSize; Retire refuses another value. A key signs many logs, so
// size must be at least the size of the largest log the key ever signed.
func AtMost(size uint64) TreeBound { return TreeBound{kind: 1, size: size} }

// Unbounded leaves a retired key without max_tree_size. The verifier then
// accepts a checkpoint of any size signed by that key for ever (a retired key
// has no not_after, correction 14), so if the key leaks, whoever holds it can
// sign a larger tree that every verifier accepts as genuine. The only remedy
// is Revoke, which makes the checkpoints that rest only on the key unreadable.
// Choose it only when no bound can be stated.
func Unbounded() TreeBound { return TreeBound{kind: 2} }

// check returns the value that goes into Key.MaxTreeSize (0 for unbounded).
func (b TreeBound) check() (uint64, error) {
	switch b.kind {
	case 1:
		if b.size < 1 || b.size > MaxTrustTreeSize {
			return 0, fmt.Errorf("%w: max tree size %d, the trust file allows 1 to %d", ErrInvalidArgument, b.size, MaxTrustTreeSize)
		}
		return b.size, nil
	case 2:
		return 0, nil
	}
	return 0, fmt.Errorf("%w: Retire needs a decision about the largest tree size: AtMost(size) or Unbounded()", ErrInvalidArgument)
}

// Key is the public record of a signing key and its state. It holds no
// secret. Times are UTC whole seconds; the zero time means not set. The
// caller persists Keys (kingres_cp.ledger_signing_key) and passes them back.
type Key struct {
	Public ed25519.PublicKey
	// Label is the member name of the key in the trust file; empty gives
	// "key-" and the first 16 hex characters of the fingerprint.
	Label            string
	State            State
	CreatedAt        time.Time
	ActivatedAt      time.Time
	OverlapEnds      time.Time // set on the older of two active keys: when it is due to retire
	RetiredAt        time.Time
	RevokedAt        time.Time
	CompromisedSince time.Time
	// MaxTreeSize, when not zero on a retired key, becomes max_tree_size of
	// the trust file (1 to MaxTrustTreeSize; zero is unbounded, see
	// Unbounded). A key signs many logs, so one size is meaningful only when
	// the caller knows it bounds all of them.
	MaxTreeSize uint64
	Reason      string
}

// Fingerprint is the fingerprint of the key.
func (k Key) Fingerprint() string { return Fingerprint(k.Public) }

// TrustLabel is the label the key has in the trust file.
func (k Key) TrustLabel() string {
	if k.Label != "" {
		return k.Label
	}
	return "key-" + k.Fingerprint()[:16]
}

func norm(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Second)
}

// timeInRange reports whether t is a time the trust file can write: year 1 to
// 9999 (the verifier reads RFC 3339 times, whose year has four digits).
func timeInRange(t time.Time) bool {
	y := t.UTC().Year()
	return y >= 1 && y <= 9999
}

// checkTime refuses the zero time and a time outside the years 1 to 9999,
// which no later call can repair: the zero time makes Validate refuse the set
// for ever, and a year of five digits makes the verifier refuse the trust
// file.
func checkTime(what string, t time.Time) error {
	if t.IsZero() {
		return fmt.Errorf("%w: %w: %s is the zero time", ErrInvalidArgument, ErrInvalidTime, what)
	}
	if !timeInRange(t) {
		return fmt.Errorf("%w: %w: %s is in the year %d, expected 1 to 9999", ErrInvalidArgument, ErrInvalidTime, what, t.UTC().Year())
	}
	return nil
}

// Set is the set of signing keys of a cluster. Its methods do not change the
// receiver: a transition returns the new set, and the caller stores it.
type Set []Key

func (s Set) clone() Set {
	out := make(Set, len(s))
	for i, k := range s {
		k.Public = append(ed25519.PublicKey(nil), k.Public...)
		out[i] = k
	}
	return out
}

func (s Set) find(fp string) int {
	for i, k := range s {
		if k.Fingerprint() == fp {
			return i
		}
	}
	return -1
}

// Get returns the key with the fingerprint.
func (s Set) Get(fp string) (Key, bool) {
	if i := s.find(fp); i >= 0 {
		return s[i], true
	}
	return Key{}, false
}

func (s Set) countState(st State) int {
	n := 0
	for _, k := range s {
		if k.State == st {
			n++
		}
	}
	return n
}

func printableASCII(s string, max int) error {
	if len(s) > max {
		return fmt.Errorf("%w: %d bytes, expected at most %d", ErrInvalidArgument, len(s), max)
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return fmt.Errorf("%w: only printable ASCII is allowed", ErrInvalidArgument)
		}
	}
	return nil
}

func validLabel(s string) error {
	if len(s) > MaxLabelSize || !utf8.ValidString(s) {
		return fmt.Errorf("%w: a label is at most %d bytes of UTF-8", ErrInvalidArgument, MaxLabelSize)
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%w: a label has no control characters", ErrInvalidArgument)
		}
	}
	return nil
}

// Validate checks the set as a whole, for a caller that loaded it from
// storage: unique 32 byte keys, at most MaxKeys keys and MaxActive active
// ones, and in each state the timestamps that state needs, in order.
func (s Set) Validate() error {
	if len(s) > MaxKeys {
		return fmt.Errorf("%w: %d keys", ErrTooManyKeys, len(s))
	}
	seen := map[string]bool{}
	labels := map[string]bool{}
	for _, k := range s {
		if len(k.Public) != ed25519.PublicKeySize {
			return fmt.Errorf("%w: a public key has %d bytes, expected %d", ErrInvalidArgument, len(k.Public), ed25519.PublicKeySize)
		}
		fp := k.Fingerprint()
		if seen[fp] || labels[k.TrustLabel()] {
			return fmt.Errorf("%w: %s", ErrDuplicateKey, fp[:16])
		}
		seen[fp], labels[k.TrustLabel()] = true, true
		if err := validLabel(k.Label); err != nil {
			return err
		}
		if err := printableASCII(k.Reason, MaxReasonSize); err != nil {
			return err
		}
		if k.CreatedAt.IsZero() {
			return fmt.Errorf("%w: key %s has no creation time", ErrInvalidArgument, fp[:16])
		}
		for _, tt := range []struct {
			name string
			t    time.Time
		}{{"CreatedAt", k.CreatedAt}, {"ActivatedAt", k.ActivatedAt}, {"OverlapEnds", k.OverlapEnds}, {"RetiredAt", k.RetiredAt}, {"RevokedAt", k.RevokedAt}, {"CompromisedSince", k.CompromisedSince}} {
			if !tt.t.IsZero() && !timeInRange(tt.t) {
				return fmt.Errorf("%w: %w: key %s: %s is in the year %d, expected 1 to 9999", ErrInvalidArgument, ErrInvalidTime, fp[:16], tt.name, tt.t.UTC().Year())
			}
		}
		if k.State != StateRevoked && (!k.CompromisedSince.IsZero() || k.Reason != "") {
			return fmt.Errorf("%w: key %s in state %s has a CompromisedSince or a Reason, which only a revoked key has", ErrInvalidArgument, fp[:16], k.State)
		}
		need := func(name string, t time.Time, want bool) error {
			if t.IsZero() == want {
				return fmt.Errorf("%w: key %s in state %s: %s must be %s", ErrInvalidArgument, fp[:16], k.State, name, map[bool]string{true: "set", false: "unset"}[want])
			}
			return nil
		}
		var err error
		switch k.State {
		case StateNext:
			err = firstError(need("ActivatedAt", k.ActivatedAt, false), need("RetiredAt", k.RetiredAt, false), need("RevokedAt", k.RevokedAt, false))
		case StateActive:
			err = firstError(need("ActivatedAt", k.ActivatedAt, true), need("RetiredAt", k.RetiredAt, false), need("RevokedAt", k.RevokedAt, false))
		case StateRetired:
			err = firstError(need("ActivatedAt", k.ActivatedAt, true), need("RetiredAt", k.RetiredAt, true), need("RevokedAt", k.RevokedAt, false))
			if err == nil && k.RetiredAt.Before(k.ActivatedAt) {
				err = fmt.Errorf("%w: key %s was retired before it was activated", ErrInvalidArgument, fp[:16])
			}
		case StateRevoked:
			err = firstError(need("RevokedAt", k.RevokedAt, true), need("CompromisedSince", k.CompromisedSince, true))
			if err == nil && k.CompromisedSince.After(k.RevokedAt) {
				err = fmt.Errorf("%w: key %s is compromised since a time after the revocation", ErrInvalidArgument, fp[:16])
			}
			if err == nil && k.RevokedAt.Before(k.CreatedAt) {
				err = fmt.Errorf("%w: key %s was revoked before it was created", ErrInvalidArgument, fp[:16])
			}
			if err == nil && !k.RetiredAt.IsZero() && k.RetiredAt.Before(k.ActivatedAt) {
				err = fmt.Errorf("%w: key %s was retired before it was activated", ErrInvalidArgument, fp[:16])
			}
		default:
			err = fmt.Errorf("%w: key %s has no valid state", ErrInvalidArgument, fp[:16])
		}
		if err != nil {
			return err
		}
		if k.State != StateRetired && k.MaxTreeSize != 0 {
			return fmt.Errorf("%w: MaxTreeSize is only for a retired key", ErrInvalidArgument)
		}
		if k.MaxTreeSize > MaxTrustTreeSize {
			return fmt.Errorf("%w: key %s has MaxTreeSize %d, the trust file allows 1 to %d", ErrInvalidArgument, fp[:16], k.MaxTreeSize, MaxTrustTreeSize)
		}
		if !k.OverlapEnds.IsZero() && k.State != StateActive {
			return fmt.Errorf("%w: key %s in state %s has an OverlapEnds", ErrInvalidArgument, fp[:16], k.State)
		}
	}
	active := s.countState(StateActive)
	if active > MaxActive {
		return ErrTooManyActive
	}
	var act []Key
	marked := 0
	for _, k := range s {
		if k.State == StateActive {
			act = append(act, k)
		}
		if !k.OverlapEnds.IsZero() {
			marked++
		}
	}
	for _, k := range s {
		if !k.OverlapEnds.IsZero() && active < 2 {
			return fmt.Errorf("%w: key %s has an OverlapEnds and is the only active key", ErrInvalidArgument, k.Fingerprint()[:16])
		}
	}
	if active == 2 {
		// Two active keys are a rotation: exactly one of them, the one that is
		// due to retire, holds the end of the overlap, and Activate set it to
		// the activation of the other key (the one activated last) plus a whole
		// number of days, MinOverlapDays to MaxOverlapDays.
		if marked != 1 {
			return fmt.Errorf("%w: two keys are active and %d of them have an OverlapEnds, expected exactly one", ErrInvalidArgument, marked)
		}
		for i, k := range act {
			if k.OverlapEnds.IsZero() {
				continue
			}
			d := k.OverlapEnds.Sub(act[1-i].ActivatedAt)
			if d < MinOverlapDays*24*time.Hour || d > MaxOverlapDays*24*time.Hour || d%(24*time.Hour) != 0 {
				return fmt.Errorf("%w: key %s has an OverlapEnds that is not the activation of the other active key plus %d to %d whole days", ErrInvalidArgument, k.Fingerprint()[:16], MinOverlapDays, MaxOverlapDays)
			}
		}
	}
	if active == 0 && s.inService() {
		return fmt.Errorf("%w: the set holds %d keys, some have left the state next, and none is active", ErrNoActiveKey, len(s))
	}
	return nil
}

// inService reports whether some key has left the state next: the set is not a
// new one waiting for its first activation. Every sequence of transitions keeps
// an active key once one was active (Retire and Revoke need another active
// key), so a set in service without one was damaged in storage.
func (s Set) inService() bool {
	for _, k := range s {
		if k.State != StateNext {
			return true
		}
	}
	return false
}

func firstError(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// Add enters a new key in state next, without looking at key id collisions
// (see AddChecked, which a caller that publishes logs uses instead). now must
// be a time of the years 1 to 9999 (ErrInvalidTime otherwise).
func (s Set) Add(pub ed25519.PublicKey, label string, now time.Time) (Set, error) {
	return s.add(pub, label, now, nil)
}

// AddChecked is Add that also refuses (with the *KeyIDCollision, which is
// ErrKeyIDCollision) a new key whose key id under one of the origins equals
// that of ANY key of the set, whatever its state: next, retired and revoked
// keys count too. The verifier reads every key of the trust file, whatever its
// standing, when it checks a note it already holds, and no key ever leaves the
// trust file, so a collision with a revoked key cannot be cured by a later
// transition; this is the one place where it can be refused. origins are the
// key names of every log the key will sign (the origins of all the logs of the
// cluster, one per log and epoch); at least one is needed.
func (s Set) AddChecked(pub ed25519.PublicKey, label string, now time.Time, origins ...string) (Set, error) {
	if len(origins) == 0 {
		return nil, fmt.Errorf("%w: AddChecked needs at least one origin to check the key id under", ErrInvalidArgument)
	}
	for _, o := range origins {
		if !ValidKeyName(o) {
			return nil, fmt.Errorf("%w: %q is not a valid key name", ErrInvalidArgument, clip(o))
		}
	}
	return s.add(pub, label, now, origins)
}

func (s Set) add(pub ed25519.PublicKey, label string, now time.Time, origins []string) (Set, error) {
	if err := checkTime("the creation time", norm(now)); err != nil {
		return nil, err
	}
	if len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: a public key has %d bytes, expected %d", ErrInvalidArgument, len(pub), ed25519.PublicKeySize)
	}
	if err := validLabel(label); err != nil {
		return nil, err
	}
	if len(s) >= MaxKeys {
		return nil, ErrTooManyKeys
	}
	k := Key{Public: append(ed25519.PublicKey(nil), pub...), Label: label, State: StateNext, CreatedAt: norm(now)}
	if s.find(k.Fingerprint()) >= 0 {
		return nil, ErrDuplicateKey
	}
	for _, o := range s {
		if o.TrustLabel() == k.TrustLabel() {
			return nil, fmt.Errorf("%w: the label %q is used", ErrDuplicateKey, clip(k.TrustLabel()))
		}
	}
	out := append(s.clone(), k)
	for _, o := range origins {
		if err := CheckSetKeyIDs(out, TrustOptions{IncludeNext: true}, o); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// latest is the latest time the set records: the creation, activation,
// retirement or revocation of any key (the overlap ends are plans, not events).
func (s Set) latest() time.Time {
	var t time.Time
	for _, k := range s {
		for _, u := range []time.Time{k.CreatedAt, k.ActivatedAt, k.RetiredAt, k.RevokedAt} {
			if u.After(t) {
				t = u
			}
		}
	}
	return t
}

// Activate makes a next key active. When another key is active it is the
// older key of a rotation: it gets OverlapEnds = now + overlapDays (1 to 365;
// the default is DefaultOverlapDays when overlapDays is 0), after which it is
// due to retire. A third active key is refused.
//
// now must be a time of the years 1 to 9999, and not later than the latest
// time the set records (creations, activations, retirements and revocations of
// any key) plus MaxActivationLeadDays. The package has no clock, so this is
// all it can say about a clock error: it catches an activation time that is
// far beyond everything the set holds (the year 2099 for a set that was last
// changed in 2026, which would make the key "not in force" for decades), at
// the moment it happens. It cannot catch a wrong time within
// MaxActivationLeadDays of the last record, a time that is wrong but earlier
// than the records, or a set that is simply old; a key that waited more than
// MaxActivationLeadDays as next is refused too (add a fresh key, whose creation
// time is recent, and revoke the stale one if it must go). A key that is
// active but not yet in force (activation later than the time of the call)
// does not sign, does not count as the other key of Retire or Revoke, and can
// itself be retired (see Retire).
func (s Set) Activate(fp string, now time.Time, overlapDays int) (Set, error) {
	i := s.find(fp)
	if i < 0 {
		return nil, ErrUnknownKey
	}
	if s[i].State != StateNext {
		return nil, fmt.Errorf("%w: %s to active", ErrTransition, s[i].State)
	}
	if overlapDays == 0 {
		overlapDays = DefaultOverlapDays
	}
	if overlapDays < MinOverlapDays || overlapDays > MaxOverlapDays {
		return nil, fmt.Errorf("%w: the overlap is %d days, expected %d to %d", ErrInvalidArgument, overlapDays, MinOverlapDays, MaxOverlapDays)
	}
	active := s.countState(StateActive)
	if active >= MaxActive {
		return nil, ErrTooManyActive
	}
	out := s.clone()
	now = norm(now)
	if err := checkTime("the activation time", now); err != nil {
		return nil, err
	}
	if limit := s.latest().AddDate(0, 0, MaxActivationLeadDays); now.After(limit) {
		return nil, fmt.Errorf("%w: %w: the activation time %s is more than %d days after the latest time the set records (%s): a clock error? An activation that far ahead would leave the key not in force for a long time",
			ErrInvalidArgument, ErrInvalidTime, now.Format(time.RFC3339), MaxActivationLeadDays, s.latest().Format(time.RFC3339))
	}
	if active == 1 {
		end := now.AddDate(0, 0, overlapDays)
		if !timeInRange(end) {
			return nil, fmt.Errorf("%w: %w: the overlap would end in the year %d, expected at most 9999", ErrInvalidArgument, ErrInvalidTime, end.Year())
		}
		for j := range out {
			if out[j].State == StateActive {
				out[j].OverlapEnds = end
			}
		}
	}
	out[i].State, out[i].ActivatedAt = StateActive, now
	return out, nil
}

// otherActive reports whether an active key other than fp exists whose
// activation time is not later than now: a key that is active in the record
// but not yet in force cannot sign the event, so it does not count.
func (s Set) otherActive(fp string, now time.Time) bool {
	for _, k := range s {
		if k.State == StateActive && k.Fingerprint() != fp && !now.Before(k.ActivatedAt) {
			return true
		}
	}
	return false
}

// noOtherSigner is ErrNoOtherSigner with the reason.
func (s Set) noOtherSigner(fp string) error {
	for _, k := range s {
		if k.State == StateActive && k.Fingerprint() != fp {
			return fmt.Errorf("%w: key %s is active only from its activation time, which is later than now", ErrNoOtherSigner, k.Fingerprint()[:16])
		}
	}
	return ErrNoOtherSigner
}

// settle makes the overlap of the set consistent after a key left the active
// state: with fewer than two active keys no overlap is running, so no key keeps
// an OverlapEnds.
func (s Set) settle() {
	if s.countState(StateActive) >= MaxActive {
		return
	}
	for i := range s {
		s[i].OverlapEnds = time.Time{}
	}
}

// Retire makes an active key retired (end of an overlap, or "retire now"). It
// needs another active key whose activation time is not later than now, which
// signs the key-retired event and keeps the cluster able to sign. bound is the
// decision about the largest tree the retired key may still sign: AtMost(size)
// writes max_tree_size to the trust file (1 to MaxTrustTreeSize), Unbounded()
// writes none, and the zero TreeBound is refused. After it no key keeps an
// OverlapEnds that belongs to an overlap that is over.
func (s Set) Retire(fp string, now time.Time, bound TreeBound) (Set, error) {
	maxTreeSize, err := bound.check()
	if err != nil {
		return nil, err
	}
	i := s.find(fp)
	if i < 0 {
		return nil, ErrUnknownKey
	}
	if s[i].State != StateActive {
		return nil, fmt.Errorf("%w: %s to retired", ErrTransition, s[i].State)
	}
	now = norm(now)
	if err := checkTime("the time of the retirement", now); err != nil {
		return nil, err
	}
	if !s.otherActive(fp, now) {
		return nil, s.noOtherSigner(fp)
	}
	// A key that is active but not yet in force (its activation is later than
	// now: a clock error at its activation, or a clock that stepped back) never
	// signed anything, and retiring it is the way out that needs no revocation.
	// It is retired at the time it would have come into force, so that the
	// record stays in order (RetiredAt is not before ActivatedAt).
	retiredAt := now
	if now.Before(s[i].ActivatedAt) {
		retiredAt = s[i].ActivatedAt
	}
	out := s.clone()
	out[i].State, out[i].RetiredAt, out[i].OverlapEnds, out[i].MaxTreeSize = StateRetired, retiredAt, time.Time{}, maxTreeSize
	out.settle()
	return out, nil
}

// Revoke marks a key as compromised since compromisedSince (not after now;
// pass now for "now"). A key in any state but revoked can be revoked, and the
// event is signed by a different active key whose activation time is not later
// than now, so one must exist. reason is at most MaxReasonSize bytes of
// printable ASCII.
func (s Set) Revoke(fp string, now, compromisedSince time.Time, reason string) (Set, error) {
	i := s.find(fp)
	if i < 0 {
		return nil, ErrUnknownKey
	}
	if s[i].State == StateRevoked {
		return nil, fmt.Errorf("%w: revoked to revoked", ErrTransition)
	}
	now, compromisedSince = norm(now), norm(compromisedSince)
	if err := checkTime("the time of the revocation", now); err != nil {
		return nil, err
	}
	if err := checkTime("compromised-since", compromisedSince); err != nil {
		return nil, err
	}
	if !s.otherActive(fp, now) {
		return nil, s.noOtherSigner(fp)
	}
	if err := printableASCII(reason, MaxReasonSize); err != nil {
		return nil, err
	}
	if compromisedSince.After(now) {
		return nil, fmt.Errorf("%w: compromised-since must not be after the time of the revocation", ErrInvalidArgument)
	}
	out := s.clone()
	k := &out[i]
	// A revocation must not fail because the clock is behind the creation of
	// the key: RevokedAt is never before CreatedAt (Validate), so a time before
	// it is recorded as the creation time.
	revokedAt := now
	if revokedAt.Before(k.CreatedAt) {
		revokedAt = k.CreatedAt
	}
	k.State, k.RevokedAt, k.CompromisedSince, k.Reason = StateRevoked, revokedAt, compromisedSince, reason
	k.OverlapEnds, k.MaxTreeSize = time.Time{}, 0
	out.settle()
	return out, nil
}

// Signers returns the keys that sign checkpoints at now: the active keys
// whose activation is not later than now, the older first. At most two. It
// returns ErrNoActiveKey (and no keys) when none may sign, so a caller cannot
// sign with an empty list by forgetting to check its length.
func (s Set) Signers(now time.Time) ([]Key, error) {
	var out []Key
	for _, k := range s {
		if k.MaySign(PurposeCheckpoint, "", now) == nil {
			out = append(out, k)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w at %s (%d keys in the set)", ErrNoActiveKey, norm(now).Format(time.RFC3339), len(s))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ActivatedAt.Before(out[j].ActivatedAt) })
	return out, nil
}

// DueForRetirement returns the fingerprints of active keys whose overlap has
// ended at now. A key is returned only while another active key exists.
func (s Set) DueForRetirement(now time.Time) []string {
	var out []string
	for _, k := range s {
		if k.State == StateActive && !k.OverlapEnds.IsZero() && !now.Before(k.OverlapEnds) && s.otherActive(k.Fingerprint(), now) {
			out = append(out, k.Fingerprint())
		}
	}
	sort.Strings(out)
	return out
}

// Purpose is what a key is asked to sign.
type Purpose uint8

// The purposes of a signature (design 3.2.4 and 3.3).
const (
	// PurposeCheckpoint: a checkpoint of a log. Active keys only.
	PurposeCheckpoint Purpose = iota + 1
	// PurposeKeyAdded: the key-added event of subject. Signed by an active key
	// (authorises the new key) and by the subject itself, which is next (or
	// already active), to prove it holds its private key.
	PurposeKeyAdded
	// PurposeKeyRetired: the key-retired event of subject, signed by an active
	// key other than the subject.
	PurposeKeyRetired
	// PurposeKeyRevoked: the key-revoked event of subject, signed by an
	// active key other than the subject.
	PurposeKeyRevoked
)

// MaySign reports whether the key may sign a statement of the purpose at now
// (nil) or why not (ErrNotAllowed). subject is the fingerprint the statement
// is about (empty for a checkpoint). It looks at this key only; the set
// checks that a retiring or revoked subject is not its own signer.
func (k Key) MaySign(p Purpose, subject string, now time.Time) error {
	no := func(why string) error {
		return fmt.Errorf("%w: key %s is %s: %s", ErrNotAllowed, k.Fingerprint()[:16], k.State, why)
	}
	switch p {
	case PurposeCheckpoint:
		if k.State != StateActive {
			return no("only an active key signs checkpoints")
		}
		if now.Before(k.ActivatedAt) {
			return no("it is not active yet")
		}
		return nil
	case PurposeKeyAdded:
		if subject == k.Fingerprint() {
			if k.State != StateNext && k.State != StateActive {
				return no("only a next or active key proves possession of its key")
			}
			return nil
		}
		if k.State != StateActive {
			return no("only an active key authorises a new key")
		}
		if now.Before(k.ActivatedAt) {
			return no("it is not in force yet")
		}
		return nil
	case PurposeKeyRetired, PurposeKeyRevoked:
		if subject == k.Fingerprint() {
			return no("a key does not sign the end of its own use")
		}
		if k.State != StateActive {
			return no("only an active key signs this event")
		}
		if now.Before(k.ActivatedAt) {
			return no("it is not in force yet")
		}
		return nil
	}
	return fmt.Errorf("%w: unknown purpose", ErrInvalidArgument)
}

// CanSign checks MaySign for the key with the fingerprint.
func (s Set) CanSign(fp string, p Purpose, subject string, now time.Time) error {
	k, ok := s.Get(fp)
	if !ok {
		return ErrUnknownKey
	}
	if subject != "" && s.find(subject) < 0 {
		return fmt.Errorf("%w: the subject", ErrUnknownKey)
	}
	return k.MaySign(p, subject, now)
}

// Summary describes the set in one line per key, without secrets, for logs.
func (s Set) Summary() string {
	var parts []string
	for _, k := range s {
		parts = append(parts, k.Fingerprint()[:16]+"="+k.State.String())
	}
	return strings.Join(parts, " ")
}
