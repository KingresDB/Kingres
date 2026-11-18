// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The tests of this file are the checks that the second round of fixes needs
// (the numbering is that of the second review: K2-nn).

// ---- K2-02: key id collisions, by the verifier's rule ----

// collisionSet builds a set holding a (activated at t0) and b in the given way,
// the rows of the matrix of the review.
func collisionRows(t *testing.T) map[string]struct {
	set                Set
	opt                TrustOptions
	wantFresh, wantAny bool // the rule: collision on the fresh path, on any path
} {
	a, b := collidingKeys(t)
	build := func(steps func(s Set) (Set, error)) Set {
		var s Set
		s = okSet(t)(s.Add(a, "a", t0))
		s = okSet(t)(s.Activate(Fingerprint(a), t0, 0))
		s = okSet(t)(s.Add(b, "b", t0))
		return okSet(t)(steps(s))
	}
	bAfterA := func(s Set) (Set, error) { return s.Activate(Fingerprint(b), t0.Add(time.Hour), 0) }
	type row = struct {
		set                Set
		opt                TrustOptions
		wantFresh, wantAny bool
	}
	return map[string]row{
		"both active": {build(bAfterA), TrustOptions{}, true, true},
		"A retired, B active": {build(func(s Set) (Set, error) {
			s, err := bAfterA(s)
			if err != nil {
				return nil, err
			}
			return s.Retire(Fingerprint(a), t0.Add(2*time.Hour), Unbounded())
		}), TrustOptions{}, true, true},
		"A revoked, B active": {build(func(s Set) (Set, error) {
			s, err := bAfterA(s)
			if err != nil {
				return nil, err
			}
			return s.Revoke(Fingerprint(a), t0.Add(2*time.Hour), t0, "test")
		}), TrustOptions{}, false, true},
		"B next, not listed":   {build(func(s Set) (Set, error) { return s, nil }), TrustOptions{}, false, false},
		"B next, IncludeNext":  {build(func(s Set) (Set, error) { return s, nil }), TrustOptions{IncludeNext: true}, true, true},
		"B next and revoked":   {build(func(s Set) (Set, error) { return s.Revoke(Fingerprint(b), t0.Add(time.Hour), t0, "test") }), TrustOptions{}, false, true},
		"no collision, 2 keys": {build(func(s Set) (Set, error) { return s, nil }), TrustOptions{IncludeNext: false}, false, false},
	}
}

// The rule without the verifier (TestKeyIDCollisionMatrixAgainstTheVerifier in
// ledgerlog checks it against the built verifier): a collision is reported
// whenever the two keys are both listed in the trust file, whatever their
// state, and Fresh says whether both are usable for a fresh checkpoint.
func TestCheckSetKeyIDsFollowsTheVerifiersRule(t *testing.T) {
	for name, row := range collisionRows(t) {
		err := CheckSetKeyIDs(row.set, row.opt, collisionOrigin())
		var c *KeyIDCollision
		got := errors.As(err, &c)
		if got != row.wantAny {
			t.Errorf("%s: collision=%v (%v), expected %v", name, got, err, row.wantAny)
			continue
		}
		if !got {
			continue
		}
		if !errors.Is(err, ErrKeyIDCollision) || !c.Held || c.Fresh != row.wantFresh {
			t.Errorf("%s: Fresh=%v Held=%v, expected Fresh=%v Held=true", name, c.Fresh, c.Held, row.wantFresh)
		}
		if c.A == c.B || c.A == "" || c.B == "" {
			t.Errorf("%s: the fingerprints are %q and %q", name, c.A, c.B)
		}
	}
	// No false report: the same key twice (a set cannot hold it twice, but the
	// function must not call it a collision), another origin, no keys.
	a, _ := collidingKeys(t)
	dup := Set{{Public: a, State: StateActive}, {Public: a, State: StateRetired}}
	if err := CheckSetKeyIDs(dup, TrustOptions{}, collisionOrigin()); err != nil {
		t.Errorf("the same key twice: %v", err)
	}
	if err := CheckSetKeyIDs(collisionRows(t)["both active"].set, TrustOptions{}, "another/name"); err != nil {
		t.Errorf("another origin: %v", err)
	}
	if err := CheckSetKeyIDs(nil, TrustOptions{}, collisionOrigin()); err != nil {
		t.Errorf("an empty set: %v", err)
	}
	if err := CheckSetKeyIDs(Set{{Public: a[:5]}}, TrustOptions{}, collisionOrigin()); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a short key: %v", err)
	}
	if err := CheckSetKeyIDs(nil, TrustOptions{}, "a b"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a bad origin: %v", err)
	}
}

// A key that collides with ANY key of the set is refused where it enters the
// set, whatever the state of the other key, because no later transition cures
// a collision with a revoked or retired key.
func TestAddCheckedRefusesACollisionWithAnyKey(t *testing.T) {
	a, b := collidingKeys(t)
	other := collisionOrigin()
	states := map[string]func(t *testing.T) Set{
		"active": func(t *testing.T) Set {
			s := okSet(t)(Set{}.Add(a, "a", t0))
			return okSet(t)(s.Activate(Fingerprint(a), t0, 0))
		},
		"next": func(t *testing.T) Set { return okSet(t)(Set{}.Add(a, "a", t0)) },
		"retired": func(t *testing.T) Set {
			s := okSet(t)(Set{}.Add(a, "a", t0))
			s = okSet(t)(s.Activate(Fingerprint(a), t0, 0))
			s = okSet(t)(mustAdd(s, 5, "", t0))
			s = okSet(t)(s.Activate(fp(5), t0.Add(time.Hour), 0))
			return okSet(t)(s.Retire(Fingerprint(a), t0.Add(2*time.Hour), Unbounded()))
		},
		"revoked": func(t *testing.T) Set {
			s := okSet(t)(Set{}.Add(a, "a", t0))
			s = okSet(t)(s.Activate(Fingerprint(a), t0, 0))
			s = okSet(t)(mustAdd(s, 5, "", t0))
			s = okSet(t)(s.Activate(fp(5), t0.Add(time.Hour), 0))
			return okSet(t)(s.Revoke(Fingerprint(a), t0.Add(2*time.Hour), t0, "test"))
		},
	}
	for name, mk := range states {
		s := mk(t)
		n := len(s)
		out, err := s.AddChecked(b, "b", t0.Add(3*time.Hour), other)
		if !errors.Is(err, ErrKeyIDCollision) || out != nil {
			t.Errorf("%s: a colliding key was added: %v", name, err)
		}
		if len(s) != n {
			t.Errorf("%s: the set was changed", name)
		}
		// The unchecked Add takes it (the doc says so), and another origin is fine.
		if _, err := s.Add(b, "b", t0.Add(3*time.Hour)); err != nil {
			t.Errorf("%s: Add: %v", name, err)
		}
		if _, err := s.AddChecked(b, "b", t0.Add(3*time.Hour), "another/name"); err != nil {
			t.Errorf("%s: another origin: %v", name, err)
		}
		// Several origins: one of them collides, and it is the one named.
		_, err = s.AddChecked(b, "b", t0.Add(3*time.Hour), "another/name", other)
		var c *KeyIDCollision
		if !errors.As(err, &c) || c.Name != other {
			t.Errorf("%s: several origins: %v", name, err)
		}
	}
	// No false report: the same key again is a duplicate, not a collision.
	s := okSet(t)(Set{}.Add(a, "a", t0))
	if _, err := s.AddChecked(a, "again", t0, other); !errors.Is(err, ErrDuplicateKey) || errors.Is(err, ErrKeyIDCollision) {
		t.Errorf("the same key: %v", err)
	}
	if _, err := s.AddChecked(pubN(7), "x", t0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("no origin: %v", err)
	}
	if _, err := s.AddChecked(pubN(7), "x", t0, "bad name"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a bad origin: %v", err)
	}
	if _, err := s.AddChecked(pubN(7), "x", t0, other); err != nil {
		t.Errorf("a key that collides with nothing: %v", err)
	}
}

// ---- K2-04: a key that is active but not in force does not wedge the set ----

// The sequence of the review, with an activation that is wrong by less than the
// tolerance: A is activated far ahead, B is added and activated now.
func TestFutureActivationDoesNotWedgeTheSet(t *testing.T) {
	future := t0.Add(300 * 24 * time.Hour)
	var s Set
	s = okSet(t)(mustAdd(s, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), future, 0)) // the clock error
	s = okSet(t)(mustAdd(s, 2, "", t0.Add(time.Hour)))
	now := t0.Add(2 * time.Hour)
	s = okSet(t)(s.Activate(fp(2), now, 0))
	// B signs, A does not (not in force).
	ks, err := s.Signers(now.Add(time.Minute))
	if err != nil || len(ks) != 1 || ks[0].Fingerprint() != fp(2) {
		t.Fatalf("signers: %v, %v", ks, err)
	}
	// After the overlap A is due, Retire and CanSign agree about it.
	later := now.Add(60 * 24 * time.Hour)
	if due := s.DueForRetirement(later); len(due) != 1 || due[0] != fp(1) {
		t.Fatalf("due for retirement: %v", due)
	}
	if err := s.CanSign(fp(1), PurposeKeyRetired, fp(2), later); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("a key that is not in force signs the retirement of the other: %v", err)
	}
	if err := s.CanSign(fp(2), PurposeKeyRetired, fp(1), later); err != nil {
		t.Errorf("the key in force cannot sign the retirement: %v", err)
	}
	// The way out: retire A without a revocation.
	out, err := s.Retire(fp(1), later, Unbounded())
	if err != nil {
		t.Fatalf("the key that was never in force cannot be retired: %v", err)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	if k, _ := out.Get(fp(1)); k.State != StateRetired || !k.RetiredAt.Equal(future) {
		t.Errorf("A after it: %+v", k)
	}
	if _, err := TrustEntries(out, TrustOptions{}); err != nil {
		t.Errorf("the trust file after it: %v", err)
	}
	// The set is not stuck: another key can be activated.
	out = okSet(t)(mustAdd(out, 3, "", later))
	if _, err := out.Activate(fp(3), later, 0); err != nil {
		t.Errorf("a third key after the way out: %v", err)
	}
	// The other way round: the key in force keeps signing while the other waits.
	var u Set
	u = okSet(t)(mustAdd(u, 1, "", t0))
	u = okSet(t)(u.Activate(fp(1), t0, 0))
	u = okSet(t)(mustAdd(u, 2, "", t0))
	u = okSet(t)(u.Activate(fp(2), t0.Add(100*24*time.Hour), 0)) // wrong ahead of time
	if due := u.DueForRetirement(t0.Add(60 * 24 * time.Hour)); len(due) != 0 {
		t.Errorf("the only key in force is due for retirement: %v", due)
	}
	if _, err := u.Retire(fp(1), t0.Add(60*24*time.Hour), Unbounded()); !errors.Is(err, ErrNoOtherSigner) {
		t.Errorf("the only key in force was retired: %v", err)
	}
	// The mistaken key can go.
	if _, err := u.Retire(fp(2), t0.Add(time.Hour), Unbounded()); err != nil {
		t.Errorf("the mistaken key cannot be retired: %v", err)
	}
}

// The activation time is checked against what the set records: the year 2099
// of a clock error is refused when it happens, a time within the tolerance is
// not, and what the rule cannot see stays accepted (it is in the doc).
func TestActivationToleranceAgainstTheRecordedTimes(t *testing.T) {
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	if _, err := s.Activate(fp(1), time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC), 0); !errors.Is(err, ErrInvalidTime) || !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("an activation in 2099 after a creation in 2026: %v", err)
	}
	edge := t0.AddDate(0, 0, MaxActivationLeadDays)
	if _, err := s.Activate(fp(1), edge, 0); err != nil {
		t.Errorf("an activation at the tolerance: %v", err)
	}
	if _, err := s.Activate(fp(1), edge.Add(time.Second), 0); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("an activation a second beyond the tolerance: %v", err)
	}
	// The latest time of the set counts, not only the creation of the key: a
	// set that was changed a year ago accepts an activation now.
	var u Set
	u = okSet(t)(mustAdd(u, 1, "", t0))
	u = okSet(t)(u.Activate(fp(1), t0, 0))
	nowLate := t0.AddDate(0, 0, 700) // idle for almost two years
	u = okSet(t)(mustAdd(u, 2, "", u.RecordLimit()))
	if _, err := u.Activate(fp(2), nowLate, 0); err != nil {
		t.Errorf("an activation of a key added at the limit, in a set that was idle for 700 days: %v", err)
	}
	// What it cannot see: a time that is wrong but within the tolerance of the
	// records, and a time that is earlier than the records.
	if _, err := s.Activate(fp(1), t0.Add(200*24*time.Hour), 0); err != nil {
		t.Errorf("a wrong time within the tolerance is accepted by design: %v", err)
	}
	later := okSet(t)(mustAdd(s, 2, "", t0.Add(100*24*time.Hour)))
	if _, err := later.Activate(fp(1), t0.Add(10*24*time.Hour), 0); err != nil {
		t.Errorf("a time before the latest record, after the creation, is accepted by design: %v", err)
	}
}

// A clock that steps back: the second key is activated at a time before the
// activation of the first.
func TestClockStepsBack(t *testing.T) {
	var s Set
	s = okSet(t)(mustAdd(s, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0.Add(10*24*time.Hour), 0))
	s = okSet(t)(mustAdd(s, 2, "", t0))
	s = okSet(t)(s.Activate(fp(2), t0.Add(24*time.Hour), 0)) // 9 days before fp(1) was activated
	// Both are in force after the later activation; the order is by activation.
	ks, err := s.Signers(t0.Add(11 * 24 * time.Hour))
	if err != nil || len(ks) != 2 || ks[0].Fingerprint() != fp(2) || ks[1].Fingerprint() != fp(1) {
		t.Fatalf("signers: %v, %v", ks, err)
	}
	// The overlap is on the key that was active first and is due to retire.
	k1, _ := s.Get(fp(1))
	if k1.OverlapEnds.IsZero() {
		t.Fatal("the key that was active first has no overlap")
	}
	due := s.DueForRetirement(k1.OverlapEnds)
	if len(due) != 1 || due[0] != fp(1) {
		t.Fatalf("due: %v", due)
	}
	out, err := s.Retire(fp(1), k1.OverlapEnds, AtMost(5))
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Validate(); err != nil {
		t.Fatal(err)
	}
	// And the retirement itself at a time before the activation of the retired
	// key (the clock steps back again): allowed, recorded at its activation.
	out2, err := s.Retire(fp(1), t0.Add(2*24*time.Hour), AtMost(5))
	if err != nil {
		t.Fatalf("a retirement with a clock that went back: %v", err)
	}
	if k, _ := out2.Get(fp(1)); !k.RetiredAt.Equal(t0.Add(10 * 24 * time.Hour)) {
		t.Errorf("retired at %v", k.RetiredAt)
	}
	if err := out2.Validate(); err != nil {
		t.Fatal(err)
	}
	// A revocation is never refused for the time: a clock behind the creation
	// of the key is recorded as the creation.
	s = okSet(t)(mustAdd(s, 3, "", t0.Add(20*24*time.Hour))) // a key created later than the clock below
	now := t0.Add(30 * time.Hour)                            // both active keys are in force
	r, err := s.Revoke(fp(3), now, now.Add(-time.Hour), "leaked")
	if err != nil {
		t.Fatalf("a revocation with a clock behind the key creation: %v", err)
	}
	if k, _ := r.Get(fp(3)); !k.RevokedAt.Equal(k.CreatedAt) {
		t.Errorf("revoked at %v, created at %v", k.RevokedAt, k.CreatedAt)
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

// Whatever the operations do with times that go forward, back and far ahead,
// the set stays valid; the keys that are due can always be retired; Retire,
// Revoke and CanSign agree about which keys are in force; and a set that has
// an active key has a way out of a future activation.
func TestOperationsWithAnyTimesKeepTheSetValidAndUsable(t *testing.T) {
	rng := rand.New(rand.NewSource(20261118))
	for round := 0; round < 60; round++ {
		var s Set
		clock := t0
		next := 1
		step := func() time.Time {
			switch rng.Intn(5) {
			case 0:
				clock = clock.Add(-time.Duration(rng.Intn(20*24)) * time.Hour) // a step back
			case 1:
				clock = clock.Add(time.Duration(rng.Intn(400*24)) * time.Hour) // far ahead
			default:
				clock = clock.Add(time.Duration(rng.Intn(48)) * time.Hour)
			}
			return clock
		}
		for i := 0; i < 80; i++ {
			now := step()
			var out Set
			var err error
			switch op := rng.Intn(5); {
			case op == 0 && len(s) < MaxKeys:
				out, err = mustAdd(s, next, "", now)
				next++
			case op == 1 && len(s) > 0:
				k := s[rng.Intn(len(s))]
				out, err = s.Activate(k.Fingerprint(), now, 1+rng.Intn(MaxOverlapDays))
			case op == 2 && len(s) > 0:
				k := s[rng.Intn(len(s))]
				bound := Unbounded()
				if rng.Intn(2) == 0 {
					bound = AtMost(uint64(1 + rng.Intn(100)))
				}
				out, err = s.Retire(k.Fingerprint(), now, bound)
			case op == 3 && len(s) > 0:
				k := s[rng.Intn(len(s))]
				out, err = s.Revoke(k.Fingerprint(), now, now.Add(-time.Duration(rng.Intn(100))*time.Hour), "x")
			default:
				// The keys that are due can be retired now (at the latest time the
				// set accepts when the clock is far ahead of the records).
				tn := clampToLimit(s, now)
				for _, fp := range s.DueForRetirement(tn) {
					if _, err := s.Retire(fp, tn, Unbounded()); err != nil {
						t.Fatalf("round %d: a key that is due cannot be retired: %v\n%s", round, err, s.Summary())
					}
				}
				continue
			}
			if err != nil {
				continue // refused: the set is unchanged
			}
			if err := out.Validate(); err != nil {
				t.Fatalf("round %d step %d: the set is not valid after an accepted operation: %v\n%s", round, i, err, out.Summary())
			}
			s = out
			// Retire and CanSign agree: a key can be retired now exactly when
			// another active key may sign the retirement now.
			now = clampToLimit(s, now)
			for _, k := range s {
				if k.State != StateActive {
					continue
				}
				signer := false
				for _, o := range s {
					if o.Fingerprint() != k.Fingerprint() && s.CanSign(o.Fingerprint(), PurposeKeyRetired, k.Fingerprint(), now) == nil {
						signer = true
					}
				}
				_, err := s.Retire(k.Fingerprint(), now, Unbounded())
				if (err == nil) != signer {
					t.Fatalf("round %d step %d: Retire says %v, CanSign says a signer exists: %v\n%s", round, i, err, signer, s.Summary())
				}
			}
		}
	}
}

// ---- K2-05: times that no later call can repair ----

func TestTimesOutsideTheTrustFileAreRefused(t *testing.T) {
	zero := time.Time{}
	y10k := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	yNeg := time.Date(-5, 1, 1, 0, 0, 0, 0, time.UTC)
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	s = okSet(t)(mustAdd(s, 2, "", t0))
	two := okSet(t)(s.Activate(fp(2), t0.Add(time.Hour), 0))
	for name, bad := range map[string]time.Time{"zero": zero, "year 10000": y10k, "year -5": yNeg} {
		check := func(what string, out Set, err error) {
			t.Helper()
			if !errors.Is(err, ErrInvalidTime) || !errors.Is(err, ErrInvalidArgument) || out != nil {
				t.Errorf("%s, %s: %v (set returned: %v)", name, what, err, out != nil)
			}
		}
		out, err := s.Add(pubN(9), "", bad)
		check("Add", out, err)
		out, err = s.Activate(fp(2), bad, 0)
		check("Activate", out, err)
		out, err = two.Retire(fp(1), bad, Unbounded())
		check("Retire", out, err)
		out, err = two.Revoke(fp(1), bad, t0, "x")
		check("Revoke", out, err)
		out, err = two.Revoke(fp(1), t0.Add(time.Hour), bad, "x")
		check("Revoke, compromised since", out, err)
	}
	// An overlap that would end in the year 10000.
	late := time.Date(9999, 12, 20, 0, 0, 0, 0, time.UTC)
	l := okSet(t)(mustAdd(nil, 1, "", late))
	l = okSet(t)(l.Activate(fp(1), late, 0))
	l = okSet(t)(mustAdd(l, 2, "", late))
	if _, err := l.Activate(fp(2), late.Add(time.Hour), 30); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("an overlap that ends in the year 10000: %v", err)
	}
	if _, err := l.Activate(fp(2), late.Add(time.Hour), 5); err != nil {
		t.Errorf("an overlap that ends in 9999: %v", err)
	}
	// Validate refuses a stored key with such a time, field by field.
	for name, mod := range map[string]func(k *Key){
		"CreatedAt":   func(k *Key) { k.CreatedAt = y10k },
		"ActivatedAt": func(k *Key) { k.ActivatedAt = y10k },
		"OverlapEnds": func(k *Key) { k.OverlapEnds = y10k },
		"RetiredAt":   func(k *Key) { k.State = StateRetired; k.RetiredAt = y10k },
		"RevokedAt":   func(k *Key) { k.State = StateRevoked; k.RevokedAt = y10k; k.CompromisedSince = t0 },
		"CompromisedSince": func(k *Key) {
			k.State = StateRevoked
			k.RevokedAt = t0
			k.CompromisedSince = yNeg
		},
	} {
		c := two.clone()
		mod(&c[0])
		if err := c.Validate(); err == nil {
			t.Errorf("%s in the year 10000: accepted", name)
		}
	}
	// The zero time of an unset column: Add refuses it, so no set with a key
	// that has no creation time can be made (Validate refused it already).
	if _, err := Set(nil).Add(pubN(1), "", time.Time{}); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("Add with a zero time: %v", err)
	}
}

// The extreme legal times are written to a trust file that the verifier reads.
func TestTrustFileExtremeTimesAgainstTheVerifier(t *testing.T) {
	verifierBin(t)
	dir := t.TempDir()
	first := time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC)
	last := time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	for name, base := range map[string]time.Time{"year1": first, "year9999": last.Add(-48 * time.Hour)} {
		var s Set
		s = okSet(t)(mustAdd(s, 1, "", base))
		s = okSet(t)(s.Activate(fp(1), base, 0))
		s = okSet(t)(mustAdd(s, 2, "", base))
		s = okSet(t)(s.Activate(fp(2), base.Add(time.Second), 1))
		s = okSet(t)(mustAdd(s, 3, "", base))
		if name == "year9999" {
			s = okSet(t)(s.Retire(fp(1), last, AtMost(1<<62-1)))
			s = okSet(t)(s.Revoke(fp(3), last, last, "test"))
		} else {
			s = okSet(t)(s.Retire(fp(1), base.Add(2*time.Second), AtMost(1)))
			s = okSet(t)(s.Revoke(fp(3), base.Add(3*time.Second), base, "test"))
		}
		b, err := TrustFile("ledger.example.com/kingres", s, TrustOptions{})
		if err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(dir, name+".json")
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		now := "2027-01-01T00:00:00Z"
		if name == "year9999" {
			now = last.Format(time.RFC3339) // a key that is not valid yet is not usable, and the file needs one
		}
		if st, out := runVerifier(t, "trust", "show", "--trust", p, "--now", now); st != 0 {
			t.Errorf("%s: the verifier refuses the trust file: %d\n%s\n%s", name, st, out, b)
		}
	}
	// What the package must never write, as the review showed: year 10000.
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	s = okSet(t)(mustAdd(s, 2, "", t0)) // a next key, listed below, is usable: without the year 10000 the file reads
	entries, err := TrustEntries(s, TrustOptions{IncludeNext: true})
	if err != nil {
		t.Fatal(err)
	}
	y10k := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	entries[0].NotBefore = &y10k
	b, err := marshalTrust("ledger.example.com/kingres", entries)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "y10k.json")
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if st, _ := runVerifier(t, "trust", "show", "--trust", p, "--now", "2027-01-01T00:00:00Z"); st == 0 {
		t.Error("the verifier accepts a year of five digits: the bound of the package has no reason")
	} else if st != 2 {
		t.Errorf("a year of five digits: status %d, expected 2", st)
	}
}

// ---- K2-06: Validate refuses the damaged sets ----

func TestValidateRefusesDamagedSets(t *testing.T) {
	act := t0.Add(time.Hour)
	base := twoActive(t, act) // fp(1) older with OverlapEnds, fp(2) younger
	mod := func(f func(c Set)) Set {
		c := base.clone()
		f(c)
		return c
	}
	k1 := func(c Set) *Key { i := c.find(fp(1)); return &c[i] }
	k2 := func(c Set) *Key { i := c.find(fp(2)); return &c[i] }
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Set{
		"two active, no overlap":           mod(func(c Set) { k1(c).OverlapEnds = time.Time{} }),
		"two active, two overlaps":         mod(func(c Set) { k2(c).OverlapEnds = k1(c).ActivatedAt.AddDate(0, 0, 30) }), // each fine on its own
		"overlap on the younger only":      mod(func(c Set) { k2(c).OverlapEnds, k1(c).OverlapEnds = k1(c).OverlapEnds.Add(time.Hour), time.Time{} }),
		"overlap before the other's start": mod(func(c Set) { k1(c).OverlapEnds = act.Add(-24 * time.Hour) }),
		"overlap of no whole days":         mod(func(c Set) { k1(c).OverlapEnds = act.Add(36 * time.Hour) }),
		"overlap of 400 days":              mod(func(c Set) { k1(c).OverlapEnds = act.AddDate(0, 0, 400) }),
		"retired with CompromisedSince": mod(func(c Set) {
			k := k1(c)
			k.State, k.RetiredAt, k.OverlapEnds, k.CompromisedSince = StateRetired, act, time.Time{}, t0
		}),
		"retired with a Reason": mod(func(c Set) {
			k := k1(c)
			k.State, k.RetiredAt, k.OverlapEnds, k.Reason = StateRetired, act, time.Time{}, "why"
		}),
		"active with a Reason": mod(func(c Set) { k2(c).Reason = "why" }),
		"next with a Reason": mod(func(c Set) {
			k := k2(c)
			k.State, k.ActivatedAt, k.Reason = StateNext, time.Time{}, "why"
			k1(c).OverlapEnds = time.Time{}
		}),
		"revoked before it was created": mod(func(c Set) {
			k := k1(c)
			k.State, k.RevokedAt, k.CompromisedSince, k.OverlapEnds = StateRevoked, t0.Add(-time.Hour), t0.Add(-2*time.Hour), time.Time{}
		}),
		"revoked and retired before activated": mod(func(c Set) {
			k := k1(c)
			k.State, k.RevokedAt, k.CompromisedSince, k.OverlapEnds, k.RetiredAt = StateRevoked, act, t0, time.Time{}, t0.Add(-time.Hour)
		}),
	} {
		if err := s.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// The sets the operations make stay valid: a retired key with its times, a
	// revoked key that was retired first.
	out := okSet(t)(base.Retire(fp(1), act.Add(time.Hour), AtMost(10)))
	out = okSet(t)(out.Revoke(fp(1), act.Add(2*time.Hour), t0, "later"))
	// What Validate cannot tell apart, and the doc says: every key flipped back
	// to next with its times cleared looks like a new set that was never used.
	flipped := base.clone()
	for i := range flipped {
		flipped[i].State = StateNext
		flipped[i].ActivatedAt, flipped[i].OverlapEnds = time.Time{}, time.Time{}
	}
	if err := flipped.Validate(); err != nil {
		t.Errorf("a set of next keys (indistinguishable from a new one): %v", err)
	}
}

// ---- K2-11: the raw seed ----

// Nothing that parsePlain makes keeps the seed after its secret is destroyed:
// its decoding buffer is cleared (the test of the raw seed in the memory of the
// process is in secretscan_linux_test.go).
func TestParsePlainResultIsTheSeedAndNotMore(t *testing.T) {
	seed := make([]byte, SeedSize)
	for i := range seed {
		seed[i] = byte(i * 7)
	}
	rest := marshalPlain(seed)[len(FileMagic)+1+len(plainHead):]
	s, err := parsePlain(rest)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	_ = s.use(func(b []byte) error { got = append(got, b...); return nil })
	if string(got) != string(seed) {
		t.Error("the parsed seed differs")
	}
	s.destroy()

}

// MaySign agrees with Retire about who is in force: a key that is active but
// not yet in force signs no statement about another key, but it may still
// prove that it holds its own key.
func TestMaySignOfAKeyThatIsNotInForce(t *testing.T) {
	act := t0.Add(time.Hour)
	k := Key{Public: pubN(1), State: StateActive, CreatedAt: t0, ActivatedAt: act}
	before, at := act.Add(-time.Second), act
	for _, p := range []Purpose{PurposeCheckpoint, PurposeKeyAdded, PurposeKeyRetired, PurposeKeyRevoked} {
		if err := k.MaySign(p, fp(2), before); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("purpose %d before the activation: %v", p, err)
		}
		if p == PurposeCheckpoint {
			continue
		}
		if err := k.MaySign(p, fp(2), at); err != nil {
			t.Errorf("purpose %d at the activation: %v", p, err)
		}
	}
	if err := k.MaySign(PurposeKeyAdded, k.Fingerprint(), before); err != nil {
		t.Errorf("a key proving that it holds its key before it is in force: %v", err)
	}
}
