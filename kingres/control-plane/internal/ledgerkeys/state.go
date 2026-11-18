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
	for _, k := range s {
		if !k.OverlapEnds.IsZero() && active < 2 {
			return fmt.Errorf("%w: key %s has an OverlapEnds and is the only active key", ErrInvalidArgument, k.Fingerprint()[:16])
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

// Add enters a new key in state next.
func (s Set) Add(pub ed25519.PublicKey, label string, now time.Time) (Set, error) {
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
	return out, nil
}

// Activate makes a next key active. When another key is active it is the
// older key of a rotation: it gets OverlapEnds = now + overlapDays (1 to 365;
// the default is DefaultOverlapDays when overlapDays is 0), after which it is
// due to retire. A third active key is refused.
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
	if active == 1 {
		for j := range out {
			if out[j].State == StateActive {
				out[j].OverlapEnds = now.AddDate(0, 0, overlapDays)
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
	if !s.otherActive(fp, now) {
		return nil, s.noOtherSigner(fp)
	}
	if now.Before(s[i].ActivatedAt) {
		return nil, fmt.Errorf("%w: the key is retired before it was activated", ErrInvalidArgument)
	}
	out := s.clone()
	out[i].State, out[i].RetiredAt, out[i].OverlapEnds, out[i].MaxTreeSize = StateRetired, now, time.Time{}, maxTreeSize
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
	if !s.otherActive(fp, now) {
		return nil, s.noOtherSigner(fp)
	}
	if err := printableASCII(reason, MaxReasonSize); err != nil {
		return nil, err
	}
	if compromisedSince.IsZero() || compromisedSince.After(now) {
		return nil, fmt.Errorf("%w: compromised-since must be set and not after the time of the revocation", ErrInvalidArgument)
	}
	out := s.clone()
	k := &out[i]
	k.State, k.RevokedAt, k.CompromisedSince, k.Reason = StateRevoked, now, compromisedSince, reason
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
		return nil
	case PurposeKeyRetired, PurposeKeyRevoked:
		if subject == k.Fingerprint() {
			return no("a key does not sign the end of its own use")
		}
		if k.State != StateActive {
			return no("only an active key signs this event")
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
