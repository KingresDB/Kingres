// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"crypto/ed25519"
	"errors"
	"math/rand"
	"strings"
	"testing"
	"time"
)

// The tests of this file are the checks of the third round of fixes (the
// numbering is that of the third review: K3-nn; correction 107 of the design).

// clampToLimit is what a caller passes for a time that may be beyond what the
// set accepts: the real time, or RecordLimit when the real time is later.
func clampToLimit(s Set, now time.Time) time.Time {
	if l := s.RecordLimit(); !l.IsZero() && now.After(l) {
		return l
	}
	return now
}

func day(d int) time.Time { return t0.Add(time.Duration(d) * 24 * time.Hour) }

// ---- K3-03: no operation records a far time ----

// The limit is checked against literal dates, not against the constant: the
// tolerance is 366 days, t0 is 2026-12-01T10:00:00Z.
func TestRecordLimitIsTheLatestRecordPlus366Days(t *testing.T) {
	if MaxActivationLeadDays != 366 {
		t.Fatalf("the tolerance is %d days, the documentation says 366", MaxActivationLeadDays)
	}
	if got := (Set{}).RecordLimit(); !got.IsZero() {
		t.Errorf("an empty set has a limit: %v", got)
	}
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	if want := time.Date(2027, 12, 2, 10, 0, 0, 0, time.UTC); !s.RecordLimit().Equal(want) {
		t.Errorf("limit %v, want %v", s.RecordLimit(), want)
	}
	// Every kind of record moves it: an activation, a retirement, a revocation.
	two := twoActive(t, t0.Add(time.Hour))
	if want := time.Date(2027, 12, 2, 11, 0, 0, 0, time.UTC); !two.RecordLimit().Equal(want) {
		t.Errorf("limit after an activation %v, want %v", two.RecordLimit(), want)
	}
	ret := okSet(t)(two.Retire(fp(1), t0.Add(48*time.Hour), Unbounded()))
	if want := time.Date(2027, 12, 4, 10, 0, 0, 0, time.UTC); !ret.RecordLimit().Equal(want) {
		t.Errorf("limit after a retirement %v, want %v", ret.RecordLimit(), want)
	}
	rev := okSet(t)(two.Revoke(fp(1), t0.Add(72*time.Hour), t0, "x"))
	if want := time.Date(2027, 12, 5, 10, 0, 0, 0, time.UTC); !rev.RecordLimit().Equal(want) {
		t.Errorf("limit after a revocation %v, want %v", rev.RecordLimit(), want)
	}
}

// The attack of the review: a far time recorded by one operation must not move
// the limit of the next. Add, Retire and Revoke refuse it, as Activate does.
func TestNoOperationRecordsAFarTime(t *testing.T) {
	far := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)
	two := twoActive(t, t0.Add(time.Hour))
	check := func(what string, out Set, err error) {
		t.Helper()
		if !errors.Is(err, ErrInvalidTime) || !errors.Is(err, ErrInvalidArgument) || out != nil {
			t.Errorf("%s: %v (set returned: %v)", what, err, out != nil)
		}
	}
	out, err := two.Add(pubN(3), "", far)
	check("Add in 2099", out, err)
	out, err = two.Retire(fp(1), far, Unbounded())
	check("Retire in 2099", out, err)
	out, err = two.Revoke(fp(1), far, t0, "x")
	check("Revoke in 2099", out, err)
	out, err = two.Revoke(fp(1), far, far, "x")
	check("Revoke in 2099 since 2099", out, err)
	// So the sequence of the review does not open the guard: after the refused
	// Add, an activation in 2098 is refused too.
	s3 := okSet(t)(mustAdd(okSet(t)(mustAdd(nil, 1, "", t0)), 2, "", t0))
	s3 = okSet(t)(s3.Activate(fp(1), t0, 0))
	if _, err := s3.Add(pubN(3), "", far); err == nil {
		t.Fatal("the far Add was accepted")
	}
	if _, err := s3.Activate(fp(2), time.Date(2098, 6, 1, 0, 0, 0, 0, time.UTC), 0); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("an activation in 2098: %v", err)
	}
	// The edge, by literal dates: the limit of two is 2027-12-02T11:00:00Z.
	edge := time.Date(2027, 12, 2, 11, 0, 0, 0, time.UTC)
	over := edge.Add(time.Second)
	if _, err := two.Add(pubN(3), "", edge); err != nil {
		t.Errorf("Add at the limit: %v", err)
	}
	if _, err := two.Add(pubN(3), "", over); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("Add a second beyond the limit: %v", err)
	}
	if _, err := two.Retire(fp(1), edge, Unbounded()); err != nil {
		t.Errorf("Retire at the limit: %v", err)
	}
	if _, err := two.Retire(fp(1), over, Unbounded()); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("Retire a second beyond the limit: %v", err)
	}
	if _, err := two.Revoke(fp(1), edge, t0, "x"); err != nil {
		t.Errorf("Revoke at the limit: %v", err)
	}
	if _, err := two.Revoke(fp(1), over, t0, "x"); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("Revoke a second beyond the limit: %v", err)
	}
	three := okSet(t)(mustAdd(two, 3, "", t0.Add(2*time.Hour))) // limit 2027-12-02T12:00:00Z
	one := okSet(t)(three.Retire(fp(1), t0.Add(2*time.Hour), Unbounded()))
	if _, err := one.Activate(fp(3), time.Date(2027, 12, 2, 12, 0, 0, 0, time.UTC), 0); err != nil {
		t.Errorf("Activate at the limit: %v", err)
	}
	if _, err := one.Activate(fp(3), time.Date(2027, 12, 2, 12, 0, 1, 0, time.UTC), 0); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("Activate a second beyond the limit: %v", err)
	}
}

// The stated exception: a set whose latest record is old is brought forward by
// ordinary operations, each recording at most RecordLimit, again and again.
func TestIdleSetIsBroughtForwardInSteps(t *testing.T) {
	// Idle for 700 days, then a rotation: Add at the limit, Activate at the real
	// time (within the new limit).
	one := okSet(t)(mustAdd(nil, 1, "", t0))
	one = okSet(t)(one.Activate(fp(1), t0, 0))
	now := day(700)
	if _, err := one.Add(pubN(2), "", now); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("a rotation after 700 days with the real time: %v", err)
	}
	s := okSet(t)(one.Add(pubN(2), "", clampToLimit(one, now)))
	if k, _ := s.Get(fp(2)); !k.CreatedAt.Equal(time.Date(2027, 12, 2, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("the creation was recorded at %v", k.CreatedAt)
	}
	s = okSet(t)(s.Activate(fp(2), now, 0))
	if ks, err := s.Signers(now); err != nil || len(ks) != 2 {
		t.Fatalf("signers after the rotation: %v %v", ks, err)
	}

	// Idle for five years: the activation itself is recorded at the limit, so the
	// set is brought forward by the operations of the rotation, and again by the
	// retirement; no key slot is used up.
	now = day(5 * 365)
	s = okSet(t)(one.Add(pubN(2), "", clampToLimit(one, now)))
	s = okSet(t)(s.Activate(fp(2), clampToLimit(s, now), 0))
	if k, _ := s.Get(fp(2)); !k.ActivatedAt.Before(now) {
		t.Errorf("the activation was recorded at the real time %v", k.ActivatedAt)
	}
	if ks, err := s.Signers(now); err != nil || len(ks) != 2 {
		t.Fatalf("signers: %v %v", ks, err)
	}
	if len(s) != 2 {
		t.Errorf("%d keys, expected 2", len(s))
	}
	// The overlap runs from the recorded time, so it is already over: the older
	// key is due at once (the doc says so), and the retirement is again a step.
	if due := s.DueForRetirement(now); len(due) != 1 || due[0] != fp(1) {
		t.Fatalf("due: %v", due)
	}
	if _, err := s.Retire(fp(1), now, Unbounded()); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("a retirement at the real time: %v", err)
	}
	s = okSet(t)(s.Retire(fp(1), clampToLimit(s, now), AtMost(10)))

	// An emergency in an idle set: the revocation is not refused for its time, it
	// is made at the limit, with a compromised-since that is not later.
	two := twoActive(t, t0.Add(time.Hour))
	now = day(4 * 365)
	if _, err := two.Revoke(fp(1), now, now.Add(-time.Hour), "leaked"); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("a revocation after 4 idle years with the real time: %v", err)
	}
	tc := clampToLimit(two, now)
	rev := okSet(t)(two.Revoke(fp(1), tc, tc, "leaked"))
	es, err := TrustEntries(rev, TrustOptions{})
	if err != nil || len(es) != 2 || es[0].Revoked == es[1].Revoked {
		t.Fatalf("the trust file after the revocation: %v %+v", err, es)
	}
	for _, e := range es {
		if e.Revoked != (e.Fingerprint == fp(1)) {
			t.Errorf("the entry of %s: revoked=%v", e.Fingerprint[:16], e.Revoked)
		}
	}

	// A set restored from a backup: the latest record is the time of the backup,
	// and the real time is a month later: nothing is refused.
	if _, err := two.Add(pubN(3), "", day(30)); err != nil {
		t.Errorf("Add a month after the backup: %v", err)
	}
	// A first key: an empty set has no limit; the key may wait a year to be
	// activated, and not a second longer.
	first := time.Date(1999, 3, 4, 5, 6, 7, 0, time.UTC)
	f := okSet(t)(mustAdd(nil, 1, "", first))
	if _, err := f.Activate(fp(1), first.AddDate(0, 0, 366), 0); err != nil {
		t.Errorf("the first key at the tolerance: %v", err)
	}
	if _, err := f.Activate(fp(1), first.AddDate(0, 0, 366).Add(time.Second), 0); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("the first key a second beyond the tolerance: %v", err)
	}
}

// K3-04: a next key that waited longer than the tolerance. The order that works
// is: add a fresh key at RecordLimit, activate it at the real time, then revoke
// the stale one; revoking first has no other key to sign it.
func TestStaleNextKeyOrderThatWorks(t *testing.T) {
	stale := okSet(t)(mustAdd(nil, 1, "stale", t0))
	now := day(400)
	if _, err := stale.Activate(fp(1), now, 0); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("a stale next key was activated: %v", err)
	}
	if _, err := stale.Add(pubN(2), "fresh", now); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("Add at the real time: %v", err)
	}
	s := okSet(t)(stale.Add(pubN(2), "fresh", clampToLimit(stale, now)))
	if _, err := s.Revoke(fp(1), now, t0, "stale"); !errors.Is(err, ErrNoOtherSigner) {
		t.Fatalf("the stale key revoked before the fresh one is active: %v", err)
	}
	s = okSet(t)(s.Activate(fp(2), now, 0))
	s = okSet(t)(s.Revoke(fp(1), now, t0, "stale, never used"))
	if ks, err := s.Signers(now); err != nil || len(ks) != 1 || ks[0].Fingerprint() != fp(2) {
		t.Fatalf("signers: %v %v", ks, err)
	}
	es, err := TrustEntries(s, TrustOptions{IncludeNext: true})
	if err != nil || len(es) != 2 || !es[0].Revoked || es[1].Revoked {
		t.Errorf("the trust file lists the stale key as revoked and the fresh one: %v %+v", err, es)
	}
}

// ---- K3-05: the times of a key are in the order a key can have ----

func TestTimesOfAKeyAreInOrder(t *testing.T) {
	// Activate: not before the creation (the boundary is allowed).
	s := okSet(t)(mustAdd(nil, 1, "", day(10)))
	if _, err := s.Activate(fp(1), day(10).Add(-time.Second), 0); !errors.Is(err, ErrInvalidTime) {
		t.Errorf("an activation before the creation: %v", err)
	}
	if _, err := s.Activate(fp(1), day(10), 0); err != nil {
		t.Errorf("an activation at the creation: %v", err)
	}
	// Revoke: compromised-since not before the creation, when the clock is not
	// behind the creation too.
	two := twoActive(t, t0.Add(time.Hour))
	three := okSet(t)(mustAdd(two, 3, "", t0.Add(2*time.Hour)))
	if _, err := three.Revoke(fp(3), t0.Add(3*time.Hour), t0.Add(time.Hour), "x"); !errors.Is(err, ErrInvalidTime) || !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("compromised since before the creation: %v", err)
	}
	r := okSet(t)(three.Revoke(fp(3), t0.Add(3*time.Hour), t0.Add(2*time.Hour), "x"))
	if k, _ := r.Get(fp(3)); !k.CompromisedSince.Equal(k.CreatedAt) {
		t.Errorf("compromised since the creation: %+v", k)
	}
	// A clock behind the creation of the key: the revocation is not refused for
	// its time; compromised-since and the revocation are recorded as the
	// creation.
	late := okSet(t)(mustAdd(two, 3, "", day(10)))
	now := t0.Add(2 * time.Hour)
	r = okSet(t)(late.Revoke(fp(3), now, now, "leaked"))
	if k, _ := r.Get(fp(3)); !k.CompromisedSince.Equal(day(10)) || !k.RevokedAt.Equal(day(10)) {
		t.Errorf("revoked with a clock behind the creation: %+v", k)
	}
	// A key that is active and not in force, revoked with a clock behind its
	// activation: the revocation is recorded at the activation at the earliest.
	ahead := okSet(t)(mustAdd(okSet(t)(mustAdd(nil, 1, "", t0)), 2, "", t0))
	ahead = okSet(t)(ahead.Activate(fp(1), t0, 0))
	ahead = okSet(t)(ahead.Activate(fp(2), day(100), 0))
	r = okSet(t)(ahead.Revoke(fp(2), t0.Add(time.Hour), t0, "x"))
	if k, _ := r.Get(fp(2)); !k.RevokedAt.Equal(day(100)) {
		t.Errorf("revoked at %v, activated at %v", k.RevokedAt, k.ActivatedAt)
	}
	// A revoked key that was retired first: the revocation is not before the
	// retirement.
	ret := okSet(t)(two.Retire(fp(1), day(5), Unbounded()))
	r = okSet(t)(ret.Revoke(fp(1), day(2), t0, "found later"))
	if k, _ := r.Get(fp(1)); !k.RevokedAt.Equal(day(5)) {
		t.Errorf("revoked at %v", k.RevokedAt)
	}
	// Validate refuses a stored key with its times out of order.
	good := Key{Public: pubN(1), State: StateRevoked, CreatedAt: t0, ActivatedAt: day(1), RetiredAt: day(2), RevokedAt: day(3), CompromisedSince: day(1)}
	other := Key{Public: pubN(9), State: StateActive, CreatedAt: t0, ActivatedAt: t0}
	if err := (Set{good, other}).Validate(); err != nil {
		t.Fatalf("the base key is refused: %v", err)
	}
	for name, mod := range map[string]func(k *Key){
		"activated before created":   func(k *Key) { k.ActivatedAt = t0.Add(-time.Second) },
		"compromised before created": func(k *Key) { k.CompromisedSince = t0.Add(-time.Second) },
		"revoked before activated":   func(k *Key) { k.RetiredAt = time.Time{}; k.RevokedAt = day(1).Add(-time.Second) },
		"revoked before retired":     func(k *Key) { k.RevokedAt = day(2).Add(-time.Second) },
		"retired before activated":   func(k *Key) { k.RetiredAt = day(1).Add(-time.Second) },
	} {
		k := good
		mod(&k)
		if err := (Set{k, other}).Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	act := Key{Public: pubN(1), State: StateActive, CreatedAt: day(1), ActivatedAt: day(1).Add(-time.Second)}
	if err := (Set{act}).Validate(); err == nil {
		t.Error("an active key activated before it was created: accepted")
	}
}

// ---- K3-06: a key that is not in force has its own error ----

func TestSignersSaysWhenTheKeyBecomesUsable(t *testing.T) {
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0.Add(time.Hour), 0))
	_, err := s.Signers(t0.Add(time.Hour - time.Second)) // the clock stepped back by a second
	var nf *NotInForceError
	if !errors.Is(err, ErrNotInForce) || !errors.As(err, &nf) {
		t.Fatalf("a clock one second behind: %v", err)
	}
	if errors.Is(err, ErrNoActiveKey) {
		t.Error("the error of a damaged set was given for a clock that stepped back")
	}
	if !nf.From.Equal(t0.Add(time.Hour)) || nf.Fingerprint != fp(1) || !strings.Contains(err.Error(), "2026-12-01T11:00:00Z") || !strings.Contains(err.Error(), "do not sign") {
		t.Errorf("the error does not say when: %v", err)
	}
	if ks, err := s.Signers(t0.Add(time.Hour)); err != nil || len(ks) != 1 {
		t.Errorf("at the activation: %v %v", ks, err)
	}
	// The earliest of several waiting keys is named.
	w := okSet(t)(mustAdd(nil, 1, "", t0))
	w = okSet(t)(mustAdd(w, 2, "", t0))
	w = okSet(t)(w.Activate(fp(1), day(5), 0))
	w = okSet(t)(w.Activate(fp(2), day(3), 0))
	if _, err := w.Signers(t0); !errors.As(err, &nf) || !nf.From.Equal(day(3)) || nf.Fingerprint != fp(2) {
		t.Errorf("two waiting keys: %v", err)
	}
	// The other cases keep ErrNoActiveKey: an empty set, a next key only, a set
	// in service with no active key.
	if _, err := (Set{}).Signers(t0); !errors.Is(err, ErrNoActiveKey) || errors.Is(err, ErrNotInForce) {
		t.Errorf("empty set: %v", err)
	}
	nextOnly := okSet(t)(mustAdd(nil, 1, "", t0))
	if _, err := nextOnly.Signers(day(1)); !errors.Is(err, ErrNoActiveKey) || errors.Is(err, ErrNotInForce) {
		t.Errorf("next only: %v", err)
	}
	damaged := Set{{Public: pubN(1), State: StateRetired, CreatedAt: t0, ActivatedAt: t0, RetiredAt: day(1)}}
	if _, err := damaged.Signers(day(2)); !errors.Is(err, ErrNoActiveKey) || errors.Is(err, ErrNotInForce) {
		t.Errorf("retired only: %v", err)
	}
}

// ---- K3-02: AddChecked looks at the new key; CheckSetKeyIDs at every pair ----

func mustPub(t testing.TB, seed uint64) ed25519.PublicKey {
	t.Helper()
	k, err := NewFileSigner(collisionSeed(seed))
	if err != nil {
		t.Fatal(err)
	}
	return k.Public()
}

// secondCollidingKeys is a second pair with one id under collisionOrigin (found
// by the same search as the first, from the seed 1,000,000 on).
func secondCollidingKeys(t testing.TB) (c, d ed25519.PublicKey) {
	t.Helper()
	c, d = mustPub(t, 1018872), mustPub(t, 1022306)
	if KeyID(collisionOrigin(), c) != KeyID(collisionOrigin(), d) {
		t.Fatal("the stored seeds do not collide any more")
	}
	if KeyID(collisionOrigin(), c) == KeyID(collisionOrigin(), mustPub(t, 12362)) {
		t.Fatal("the two pairs have one id")
	}
	return c, d
}

func TestAddCheckedIsNotStoppedByAnOldPair(t *testing.T) {
	a, b := collidingKeys(t)
	c, d := secondCollidingKeys(t)
	o := collisionOrigin()
	// An old pair in the set (a key added with Add, or found after the fact).
	old := okSet(t)(Set{}.Add(a, "a", t0))
	old = okSet(t)(old.Activate(Fingerprint(a), t0, 0))
	old = okSet(t)(old.Add(b, "b", t0))
	if err := CheckSetKeyIDs(old, TrustOptions{IncludeNext: true}, o); !errors.Is(err, ErrKeyIDCollision) {
		t.Fatalf("the old pair is not seen: %v", err)
	}
	// A key that collides with neither is added, and the set keeps the pair.
	withC, err := old.AddChecked(c, "c", t0.Add(time.Hour), o)
	if err != nil || len(withC) != 3 {
		t.Fatalf("a clean key was refused because of an old pair: %v", err)
	}
	if _, err := old.AddChecked(pubN(7), "x", t0.Add(time.Hour), o, "another/origin"); err != nil {
		t.Errorf("a second clean key: %v", err)
	}
	// A key that collides with a key of the set is refused, and the error names
	// the new key and its partner, never the old pair.
	out, err := withC.AddChecked(d, "d", t0.Add(2*time.Hour), o)
	var col *KeyIDCollision
	if !errors.Is(err, ErrKeyIDCollision) || !errors.As(err, &col) || out != nil {
		t.Fatalf("a colliding key was added: %v", err)
	}
	if col.A != Fingerprint(c) || col.B != Fingerprint(d) || len(col.Also) != 0 || !col.Held || !col.Fresh {
		t.Errorf("the error names %s and %s (also %d), expected %s and %s: %v", shortFP(col.A), shortFP(col.B), len(col.Also), shortFP(Fingerprint(c)), shortFP(Fingerprint(d)), err)
	}
	if strings.Contains(err.Error(), shortFP(Fingerprint(a))) || strings.Contains(err.Error(), shortFP(Fingerprint(b))) {
		t.Errorf("the error names the old pair: %v", err)
	}
	// The unchecked Add still takes it, as documented.
	if _, err := withC.Add(d, "d", t0.Add(2*time.Hour)); err != nil {
		t.Errorf("Add: %v", err)
	}
}

func TestCheckSetKeyIDsReportsEveryPair(t *testing.T) {
	a, b := collidingKeys(t)
	c, d := secondCollidingKeys(t)
	o := collisionOrigin()
	key := func(p ed25519.PublicKey, st State) Key {
		return Key{Public: p, State: st, CreatedAt: t0, ActivatedAt: t0}
	}
	heldOnly := []Key{key(a, StateRevoked), key(b, StateRetired)} // the old pair: a revoked key
	fresh := []Key{key(c, StateActive), key(d, StateActive)}
	samePair := func(x, y string, p, q ed25519.PublicKey) bool {
		return (x == Fingerprint(p) && y == Fingerprint(q)) || (x == Fingerprint(q) && y == Fingerprint(p))
	}
	for name, set := range map[string]Set{
		"held pair first":  append(append(Set{}, heldOnly...), fresh...),
		"fresh pair first": append(append(Set{}, fresh...), heldOnly...),
		"interleaved":      {heldOnly[0], fresh[0], heldOnly[1], fresh[1]},
	} {
		err := CheckSetKeyIDs(set, TrustOptions{}, o)
		var col *KeyIDCollision
		if !errors.As(err, &col) {
			t.Fatalf("%s: %v", name, err)
		}
		if !col.Fresh {
			t.Errorf("%s: the pair that stops fresh checkpoints is hidden behind one that does not (A=%s B=%s)", name, shortFP(col.A), shortFP(col.B))
		}
		if !samePair(col.A, col.B, c, d) {
			t.Errorf("%s: the main pair is %s and %s", name, shortFP(col.A), shortFP(col.B))
		}
		if len(col.Also) != 1 || col.Also[0].Fresh || !col.Also[0].Held || !samePair(col.Also[0].A, col.Also[0].B, a, b) {
			t.Errorf("%s: the other pairs are %+v", name, col.Also)
		}
		if !strings.Contains(err.Error(), "1 other colliding pair") || !strings.Contains(err.Error(), shortFP(Fingerprint(a))) {
			t.Errorf("%s: the text does not name the other pair: %v", name, err)
		}
	}
	// Only held pairs: the first is the main one and Fresh is false.
	err := CheckSetKeyIDs(Set{key(a, StateRevoked), key(b, StateRetired), key(c, StateRevoked), key(d, StateRetired)}, TrustOptions{}, o)
	var col *KeyIDCollision
	if !errors.As(err, &col) || col.Fresh || len(col.Also) != 1 || !samePair(col.A, col.B, a, b) {
		t.Errorf("two held pairs: %+v %v", col, err)
	}
	// No pair, no error; the next keys count only with IncludeNext.
	if err := CheckSetKeyIDs(Set{key(a, StateActive), key(c, StateActive)}, TrustOptions{}, o); err != nil {
		t.Errorf("no pair: %v", err)
	}
	nx := Set{key(a, StateActive), {Public: b, State: StateNext, CreatedAt: t0}}
	if err := CheckSetKeyIDs(nx, TrustOptions{}, o); err != nil {
		t.Errorf("a next key that is not listed: %v", err)
	}
	if err := CheckSetKeyIDs(nx, TrustOptions{IncludeNext: true}, o); !errors.Is(err, ErrKeyIDCollision) {
		t.Errorf("a next key that is listed: %v", err)
	}
}

// ---- K3-08: TrustFile refuses a collision that Add let in ----

func TestTrustFileRefusesACollision(t *testing.T) {
	a, b := collidingKeys(t)
	o := collisionOrigin()
	build := func(revoke bool) Set {
		s := okSet(t)(Set{}.Add(a, "a", t0))
		s = okSet(t)(s.Activate(Fingerprint(a), t0, 0))
		s = okSet(t)(s.Add(b, "b", t0))
		s = okSet(t)(s.Activate(Fingerprint(b), t0.Add(time.Hour), 0))
		if revoke {
			s = okSet(t)(s.Revoke(Fingerprint(a), t0.Add(2*time.Hour), t0, "x"))
		}
		return s
	}
	for _, revoke := range []bool{false, true} {
		s := build(revoke)
		// The origin is a prefix of itself: a valid origin of the verifier.
		_, err := TrustFile(o, s, TrustOptions{})
		var col *KeyIDCollision
		if !errors.Is(err, ErrKeyIDCollision) || !errors.As(err, &col) || !col.Held {
			t.Errorf("revoked=%v, the prefix is the origin: %v", revoke, err)
		}
		// An origin below the prefix, given by the caller.
		if _, err := TrustFile("ledger.test/kingres", s, TrustOptions{}, "another/origin", o); !errors.Is(err, ErrKeyIDCollision) {
			t.Errorf("revoked=%v, an origin given: %v", revoke, err)
		}
		// Without that origin nothing is known to collide (the doc says to pass them).
		if _, err := TrustFile("ledger.test/kingres", s, TrustOptions{}); err != nil {
			t.Errorf("revoked=%v, no origin given: %v", revoke, err)
		}
		if _, err := TrustFile("ledger.test/kingres", s, TrustOptions{}, "another/origin"); err != nil {
			t.Errorf("revoked=%v, another origin: %v", revoke, err)
		}
	}
	// A set built with AddChecked cannot get there, and a clean set is written.
	clean := okSet(t)(Set{}.AddChecked(a, "a", t0, o))
	clean = okSet(t)(clean.Activate(Fingerprint(a), t0, 0))
	if _, err := clean.AddChecked(b, "b", t0, o); !errors.Is(err, ErrKeyIDCollision) {
		t.Errorf("AddChecked: %v", err)
	}
	if _, err := TrustFile(o, clean, TrustOptions{}, o); err != nil {
		t.Errorf("a clean set: %v", err)
	}
	// A next key counts when the file lists it.
	next := okSet(t)(Set{}.Add(a, "a", t0))
	next = okSet(t)(next.Activate(Fingerprint(a), t0, 0))
	next = okSet(t)(next.Add(b, "b", t0))
	if _, err := TrustFile(o, next, TrustOptions{}); err != nil {
		t.Errorf("a next key that is not listed: %v", err)
	}
	if _, err := TrustFile(o, next, TrustOptions{IncludeNext: true}); !errors.Is(err, ErrKeyIDCollision) {
		t.Errorf("a next key that is listed: %v", err)
	}
}

// ---- K3-07: the survivors of the third review, and constants against literals ----

func TestLimitsAreTheDocumentedNumbers(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want uint64
	}{
		{"MaxKeys", MaxKeys, 16}, {"MaxActive", MaxActive, 2}, {"MinOverlapDays", MinOverlapDays, 1},
		{"MaxOverlapDays", MaxOverlapDays, 365}, {"DefaultOverlapDays", DefaultOverlapDays, 30},
		{"MaxReasonSize", MaxReasonSize, 200}, {"MaxLabelSize", MaxLabelSize, 200},
		{"MaxTrustTreeSize", MaxTrustTreeSize, 4611686018427387903}, {"MaxActivationLeadDays", MaxActivationLeadDays, 366},
		{"MaxPrefixSize", MaxPrefixSize, 160},
	} {
		if c.got != c.want {
			t.Errorf("%s is %d, the documentation says %d", c.name, c.got, c.want)
		}
	}
}

// DueForRetirement returns a key only while another active key is in force.
// (Validate keeps a stored set from having such a state; the function is
// checked directly on records, as the doc states its rule.)
func TestDueForRetirementNeedsTheOtherActiveKey(t *testing.T) {
	a := Key{Public: pubN(1), State: StateActive, CreatedAt: t0, ActivatedAt: t0, OverlapEnds: day(1)}
	notYet := Key{Public: pubN(2), State: StateActive, CreatedAt: t0, ActivatedAt: day(100)}
	retired := Key{Public: pubN(2), State: StateRetired, CreatedAt: t0, ActivatedAt: t0, RetiredAt: day(1)}
	inForce := Key{Public: pubN(2), State: StateActive, CreatedAt: t0, ActivatedAt: t0}
	if due := (Set{a, inForce}).DueForRetirement(day(2)); len(due) != 1 || due[0] != fp(1) {
		t.Errorf("with the other key in force: %v", due)
	}
	if due := (Set{a, notYet}).DueForRetirement(day(2)); len(due) != 0 {
		t.Errorf("the other key is not in force: %v", due)
	}
	if due := (Set{a, retired}).DueForRetirement(day(2)); len(due) != 0 {
		t.Errorf("the other key is retired: %v", due)
	}
	if due := (Set{a}).DueForRetirement(day(2)); len(due) != 0 {
		t.Errorf("no other key: %v", due)
	}
}

func TestValidateOverlapRules(t *testing.T) {
	act := t0.Add(time.Hour)
	base := twoActive(t, act)
	for _, tc := range []struct {
		name string
		ends time.Time
		ok   bool
	}{
		{"zero days", act, false},
		{"a day before", act.Add(-24 * time.Hour), false},
		{"one day", act.Add(24 * time.Hour), true},
		{"365 days", act.Add(365 * 24 * time.Hour), true},
		{"366 days", act.Add(366 * 24 * time.Hour), false},
		{"25 hours", act.Add(25 * time.Hour), false},
	} {
		s := base.clone()
		s[0].OverlapEnds = tc.ends
		if err := s.Validate(); (err == nil) != tc.ok {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
	// The marker on a key that is not active, with two active keys and none on
	// them: exactly one marker is counted, so only the rule about the state of
	// the key refuses it.
	s := base.clone()
	s[0].OverlapEnds = time.Time{}
	s = append(s, Key{Public: pubN(5), State: StateRetired, CreatedAt: t0, ActivatedAt: t0, RetiredAt: act, OverlapEnds: act.Add(24 * time.Hour)})
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "has an OverlapEnds") {
		t.Errorf("a retired key with an OverlapEnds: %v", err)
	}
	s = base.clone()
	s[0].OverlapEnds = time.Time{}
	s = append(s, Key{Public: pubN(5), State: StateNext, CreatedAt: t0, OverlapEnds: act.Add(24 * time.Hour)})
	if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "has an OverlapEnds") {
		t.Errorf("a next key with an OverlapEnds: %v", err)
	}
	// A revoked key retired before it was activated.
	act2 := Key{Public: pubN(9), State: StateActive, CreatedAt: t0, ActivatedAt: t0}
	rev := Key{Public: pubN(1), State: StateRevoked, CreatedAt: t0, ActivatedAt: day(2), RetiredAt: day(1), RevokedAt: day(3), CompromisedSince: day(3)}
	if err := (Set{rev, act2}).Validate(); err == nil || !strings.Contains(err.Error(), "retired before it was activated") {
		t.Errorf("a revoked key retired before it was activated: %v", err)
	}
	rev.RetiredAt = day(2)
	if err := (Set{rev, act2}).Validate(); err != nil {
		t.Errorf("the same key retired at its activation: %v", err)
	}
}

// parsePlain clears both of its working buffers (the decoded seed and its text
// in base64). The seam shows the buffers after the function has returned.
func TestParsePlainClearsItsBuffers(t *testing.T) {
	seed := make([]byte, SeedSize)
	for i := range seed {
		seed[i] = byte(i*7 + 1)
	}
	rest := marshalPlain(seed)[len(FileMagic)+1+len(plainHead):]
	var gotSeed, gotAgain []byte
	called := 0
	parsePlainDone = func(s, a []byte) { gotSeed, gotAgain, called = s, a, called+1 }
	defer func() { parsePlainDone = nil }()
	s, err := parsePlain(rest)
	if err != nil {
		t.Fatal(err)
	}
	s.destroy()
	if called != 1 || len(gotSeed) < SeedSize || len(gotAgain) < 44 {
		t.Fatalf("the seam was called %d times with buffers of %d and %d bytes", called, len(gotSeed), len(gotAgain))
	}
	for i, b := range gotSeed {
		if b != 0 {
			t.Fatalf("byte %d of the decoding buffer was not cleared", i)
		}
	}
	for i, b := range gotAgain {
		if b != 0 {
			t.Fatalf("byte %d of the re-encoding buffer was not cleared", i)
		}
	}
	// A refused input clears too.
	bad := append([]byte(nil), rest...)
	bad[3] = '!'
	called = 0
	if _, err := parsePlain(bad); err == nil {
		t.Fatal("a damaged seed was accepted")
	}
	for _, b := range gotSeed {
		if b != 0 {
			t.Fatal("the decoding buffer of a refused file was not cleared")
		}
	}
	if called != 1 {
		t.Errorf("the seam was called %d times for a refused file", called)
	}
}

// ---- A random walk of the key operations against Validate and TrustEntries ----

func TestRandomWalkOfTheKeyOperations(t *testing.T) {
	edges := []time.Time{
		{}, time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(1, 1, 1, 0, 0, 1, 0, time.UTC),
		time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC), time.Date(9999, 12, 31, 23, 59, 58, 0, time.UTC),
		time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC),
		t0.Add(500 * time.Millisecond), time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	rng := rand.New(rand.NewSource(20261119))
	pick := func(s Set) time.Time {
		switch rng.Intn(6) {
		case 0:
			return edges[rng.Intn(len(edges))]
		case 1: // around the limit
			if l := s.RecordLimit(); !l.IsZero() {
				return l.Add(time.Duration(rng.Intn(3)-1) * time.Second)
			}
			return t0
		case 2: // back
			return t0.Add(-time.Duration(rng.Intn(400*24)) * time.Hour)
		case 3: // far ahead
			return t0.Add(time.Duration(rng.Intn(900*24)) * time.Hour)
		default:
			return t0.Add(time.Duration(rng.Intn(60*24)) * time.Hour)
		}
	}
	accepted := map[string]int{}
	for round := 0; round < 120; round++ {
		var s Set
		next := 1
		for i := 0; i < 90; i++ {
			now := pick(s)
			var out Set
			var err error
			op := rng.Intn(5)
			switch {
			case op == 0 && len(s) < MaxKeys:
				out, err = mustAdd(s, next, "", now)
				next++
			case op == 1 && len(s) > 0:
				out, err = s.Activate(s[rng.Intn(len(s))].Fingerprint(), now, rng.Intn(MaxOverlapDays+3)-1)
			case op == 2 && len(s) > 0:
				bound := Unbounded()
				if rng.Intn(2) == 0 {
					bound = AtMost(uint64(rng.Intn(50)))
				}
				k := s[rng.Intn(len(s))]
				for _, c := range s {
					if c.State == StateActive && rng.Intn(3) != 0 {
						k = c
					}
				}
				out, err = s.Retire(k.Fingerprint(), now, bound)
			case op == 3 && len(s) > 0:
				since := now.Add(-time.Duration(rng.Intn(500)-100) * time.Hour)
				if rng.Intn(4) == 0 {
					since = pick(s)
				}
				out, err = s.Revoke(s[rng.Intn(len(s))].Fingerprint(), now, since, "x")
			default:
				continue
			}
			if err != nil {
				continue
			}
			accepted[[]string{"Add", "Activate", "Retire", "Revoke", "?"}[op]]++
			if err := out.Validate(); err != nil {
				t.Fatalf("round %d step %d: op %d at %v gave a set that does not validate: %v\n%s", round, i, op, now, err, out.Summary())
			}
			listed := false
			for _, k := range out {
				listed = listed || k.State != StateNext
			}
			if _, err := TrustEntries(out, TrustOptions{IncludeNext: true}); len(out) > 0 && err != nil {
				t.Fatalf("round %d step %d: the trust entries: %v", round, i, err)
			}
			if _, err := TrustEntries(out, TrustOptions{}); listed && err != nil {
				t.Fatalf("round %d step %d: the trust entries without next keys: %v", round, i, err)
			}
			s = out
			// A way forward, at the latest record (a time that no operation
			// refuses for being far): a set in service has a signer or says that
			// its active keys wait; two active keys can come down to one; a next
			// key can go up.
			at := s.latest()
			if _, err := s.Signers(at); err != nil && s.inService() && !errors.Is(err, ErrNotInForce) {
				t.Fatalf("round %d step %d: no way to sign at the latest record: %v\n%s", round, i, err, s.Summary())
			}
			active := s.countState(StateActive)
			if active == 2 {
				ok := false
				for _, k := range s {
					if k.State == StateActive {
						if _, err := s.Retire(k.Fingerprint(), at, Unbounded()); err == nil {
							ok = true
						}
					}
				}
				if !ok {
					t.Fatalf("round %d step %d: two active keys and no way to retire one at %v\n%s", round, i, at, s.Summary())
				}
			}
			if active < 2 {
				for _, k := range s {
					if k.State == StateNext {
						if _, err := s.Activate(k.Fingerprint(), at, 0); err != nil && at.Year() < 9999 {
							t.Fatalf("round %d step %d: a next key cannot be activated at the latest record: %v\n%s", round, i, err, s.Summary())
						}
						break
					}
				}
			}
		}
	}
	for _, op := range []string{"Add", "Activate", "Retire", "Revoke"} {
		if accepted[op] < 100 {
			t.Errorf("the walk accepted only %d %s operations", accepted[op], op)
		}
	}
}
