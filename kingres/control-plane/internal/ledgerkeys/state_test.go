// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC)

func pubN(n int) ed25519.PublicKey {
	seed := make([]byte, SeedSize)
	seed[0], seed[1] = byte(n), byte(n>>8)
	return ed25519.NewKeyFromSeed(seed).Public().(ed25519.PublicKey)
}

func fp(n int) string { return Fingerprint(pubN(n)) }

func TestStateNames(t *testing.T) {
	for _, s := range []State{StateNext, StateActive, StateRetired, StateRevoked} {
		got, err := ParseState(s.String())
		if err != nil || got != s {
			t.Errorf("%v: %v %v", s, got, err)
		}
	}
	if _, err := ParseState("Active"); err == nil {
		t.Error("a wrong case was accepted")
	}
	if State(0).String() != "invalid" || State(9).String() != "invalid" {
		t.Error("an invalid state has a name")
	}
}

func TestPlannedRotation(t *testing.T) {
	var s Set
	s = okSet(t)(mustAdd(s, 1, "2026", t0))
	if got, err := s.Signers(t0); !errors.Is(err, ErrNoActiveKey) || len(got) != 0 {
		t.Fatalf("a next key signs: %v", got)
	}
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	if got, err := s.Signers(t0); err != nil || len(got) != 1 {
		t.Fatalf("signers %d", len(got))
	}
	// The one active key cannot be retired: nothing would sign.
	if _, err := s.Retire(fp(1), t0, Unbounded()); !errors.Is(err, ErrNoOtherSigner) {
		t.Fatalf("retire of the only signer: %v", err)
	}
	// Step 1 of 3.3: K2 is generated, state next. It may prove possession and
	// not sign checkpoints; K1 authorises.
	s = okSet(t)(mustAdd(s, 2, "2027", t0.Add(time.Hour)))
	if err := s.CanSign(fp(2), PurposeCheckpoint, "", t0.Add(time.Hour)); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a next key signs a checkpoint: %v", err)
	}
	if err := s.CanSign(fp(2), PurposeKeyAdded, fp(2), t0); err != nil {
		t.Fatalf("a next key proves possession: %v", err)
	}
	if err := s.CanSign(fp(1), PurposeKeyAdded, fp(2), t0); err != nil {
		t.Fatalf("the active key authorises: %v", err)
	}
	if err := s.CanSign(fp(2), PurposeKeyAdded, fp(1), t0); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a next key authorises another key: %v", err)
	}
	// Step 3: K2 active, K1 gets an overlap of 30 days.
	act := t0.Add(2 * time.Hour)
	s = okSet(t)(s.Activate(fp(2), act, 0))
	k1, _ := s.Get(fp(1))
	if want := act.AddDate(0, 0, DefaultOverlapDays); !k1.OverlapEnds.Equal(want) {
		t.Fatalf("overlap ends %v, want %v", k1.OverlapEnds, want)
	}
	if got, err := s.Signers(act); err != nil || len(got) != 2 || got[0].Fingerprint() != fp(1) || got[1].Fingerprint() != fp(2) {
		t.Fatalf("signers during the overlap: %v", got)
	}
	if got := s.DueForRetirement(act.AddDate(0, 0, 29)); len(got) != 0 {
		t.Fatalf("due too early: %v", got)
	}
	if got := s.DueForRetirement(k1.OverlapEnds); len(got) != 1 || got[0] != fp(1) {
		t.Fatalf("not due at the end: %v", got)
	}
	// A third key cannot be activated while two are.
	s = okSet(t)(mustAdd(s, 3, "", act))
	if _, err := s.Activate(fp(3), act, 0); !errors.Is(err, ErrTooManyActive) {
		t.Fatalf("third active key: %v", err)
	}
	// Step 4: K1 retires, signed by K2.
	if err := s.CanSign(fp(2), PurposeKeyRetired, fp(1), act); err != nil {
		t.Fatal(err)
	}
	if err := s.CanSign(fp(1), PurposeKeyRetired, fp(1), act); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a key signs its own retirement: %v", err)
	}
	end := k1.OverlapEnds
	s = okSet(t)(s.Retire(fp(1), end, Unbounded()))
	k1, _ = s.Get(fp(1))
	if k1.State != StateRetired || !k1.RetiredAt.Equal(end) || !k1.OverlapEnds.IsZero() {
		t.Fatalf("retired key: %+v", k1)
	}
	if got, err := s.Signers(end); err != nil || len(got) != 1 || got[0].Fingerprint() != fp(2) {
		t.Fatalf("signers after the retirement: %v", got)
	}
	if err := s.CanSign(fp(1), PurposeCheckpoint, "", end); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a retired key signs: %v", err)
	}
	if s.DueForRetirement(end.AddDate(1, 0, 0)) != nil {
		t.Fatal("a due key after the retirement")
	}
}

func mustAdd(s Set, n int, label string, now time.Time) (Set, error) {
	return s.Add(pubN(n), label, now)
}

func TestTransitionsMatrix(t *testing.T) {
	// Every transition from every state, against the rules of 3.3.
	build := func(state State) Set {
		var s Set
		s = okSet(t)(mustAdd(s, 1, "", t0))
		s = okSet(t)(s.Activate(fp(1), t0, 0))
		s = okSet(t)(mustAdd(s, 9, "", t0)) // a second key to be the other signer
		s = okSet(t)(s.Activate(fp(9), t0, 0))
		s = okSet(t)(mustAdd(s, 2, "", t0))
		switch state {
		case StateNext:
		case StateActive:
			s = okSet(t)(s.Retire(fp(1), t0, Unbounded())) // keep two... fp(9) and then activate 2
			s = okSet(t)(s.Activate(fp(2), t0, 0))
		case StateRetired:
			s = okSet(t)(s.Retire(fp(1), t0, Unbounded()))
			s = okSet(t)(s.Activate(fp(2), t0, 0))
			s = okSet(t)(s.Retire(fp(2), t0, Unbounded()))
		case StateRevoked:
			s = okSet(t)(s.Revoke(fp(2), t0, t0, "leaked"))
		}
		return s
	}
	target := func(state State) int { // which key is in `state`
		if state == StateNext || state == StateActive || state == StateRetired || state == StateRevoked {
			return 2
		}
		return 0
	}
	for _, from := range []State{StateNext, StateActive, StateRetired, StateRevoked} {
		s := build(from)
		k, _ := s.Get(fp(target(from)))
		if k.State != from {
			t.Fatalf("test bug: %v is %v", from, k.State)
		}
		f := fp(2)
		_, errAct := s.Activate(f, t0, 0)
		_, errRet := s.Retire(f, t0, Unbounded())
		_, errRev := s.Revoke(f, t0, t0, "r")
		wantAct := from == StateNext
		wantRet := from == StateActive
		wantRev := from != StateRevoked
		if (errAct == nil) != wantAct && !(wantAct && errors.Is(errAct, ErrTooManyActive)) {
			t.Errorf("activate from %v: %v", from, errAct)
		}
		if (errRet == nil) != wantRet {
			t.Errorf("retire from %v: %v", from, errRet)
		}
		if (errRev == nil) != wantRev {
			t.Errorf("revoke from %v: %v", from, errRev)
		}
		if !wantRet && errRet != nil && !errors.Is(errRet, ErrTransition) {
			t.Errorf("retire from %v is not a transition error: %v", from, errRet)
		}
	}
}

func TestRevokeNeedsAnotherActiveKey(t *testing.T) {
	var s Set
	s = okSet(t)(mustAdd(s, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	if _, err := s.Revoke(fp(1), t0, t0, "compromised"); !errors.Is(err, ErrNoOtherSigner) {
		t.Fatalf("revoking the only active key: %v", err)
	}
	s = okSet(t)(mustAdd(s, 2, "", t0))
	// A next key is not an active signer.
	if _, err := s.Revoke(fp(1), t0, t0, "compromised"); !errors.Is(err, ErrNoOtherSigner) {
		t.Fatalf("revoking with only a next key: %v", err)
	}
	s = okSet(t)(s.Activate(fp(2), t0.Add(time.Minute), 0))
	since := t0
	s = okSet(t)(s.Revoke(fp(1), t0.Add(time.Hour), since, "laptop stolen"))
	k, _ := s.Get(fp(1))
	if k.State != StateRevoked || !k.CompromisedSince.Equal(since) || k.Reason != "laptop stolen" || !k.OverlapEnds.IsZero() {
		t.Fatalf("revoked key: %+v", k)
	}
	if err := s.CanSign(fp(2), PurposeKeyRevoked, fp(1), t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := s.CanSign(fp(1), PurposeCheckpoint, "", t0); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("a revoked key signs: %v", err)
	}
	for _, p := range []Purpose{PurposeKeyAdded, PurposeKeyRetired, PurposeKeyRevoked} {
		if err := s.CanSign(fp(1), p, fp(2), t0); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("a revoked key signs purpose %d: %v", p, err)
		}
	}
	// Compromise after the time of the revocation, or unset: refused.
	t1 := t0.Add(time.Hour)
	s2 := okSet(t)(mustAdd(s, 3, "", t0))
	if _, err := s2.Revoke(fp(3), t1, t1.Add(time.Second), "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("compromised-since in the future: %v", err)
	}
	if _, err := s2.Revoke(fp(3), t1, time.Time{}, "x"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("compromised-since unset: %v", err)
	}
	if _, err := s2.Revoke(fp(3), t1, t1, "bad\ttext"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a control character in the reason: %v", err)
	}
	if _, err := s2.Revoke(fp(3), t1, t1, "caf\u00e9"); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a non-ASCII reason: %v", err)
	}
}

func TestLimitsOfTheSet(t *testing.T) {
	// The reason: 200 printable ASCII bytes pass, 201 do not.
	var s Set
	s = okSet(t)(mustAdd(s, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	s = okSet(t)(mustAdd(s, 2, "", t0))
	s = okSet(t)(s.Activate(fp(2), t0, 0))
	s = okSet(t)(mustAdd(s, 3, "", t0))
	if _, err := s.Revoke(fp(3), t0, t0, strings.Repeat("r", MaxReasonSize)); err != nil {
		t.Errorf("MaxReasonSize: %v", err)
	}
	if _, err := s.Revoke(fp(3), t0, t0, strings.Repeat("r", MaxReasonSize+1)); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("MaxReasonSize+1: %v", err)
	}
	// The label.
	if _, err := mustAdd(nil, 4, strings.Repeat("l", MaxLabelSize), t0); err != nil {
		t.Errorf("MaxLabelSize: %v", err)
	}
	if _, err := mustAdd(nil, 4, strings.Repeat("l", MaxLabelSize+1), t0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("MaxLabelSize+1: %v", err)
	}
	if _, err := mustAdd(nil, 4, "a\nb", t0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("control character in a label: %v", err)
	}
	// The overlap, in days.
	for _, tc := range []struct {
		days int
		ok   bool
	}{{1, true}, {365, true}, {0, true}, {-1, false}, {366, false}} {
		b := okSet(t)(mustAdd(nil, 1, "", t0))
		b = okSet(t)(b.Activate(fp(1), t0, 0))
		b = okSet(t)(mustAdd(b, 2, "", t0))
		_, err := b.Activate(fp(2), t0, tc.days)
		if (err == nil) != tc.ok {
			t.Errorf("overlap %d days: %v", tc.days, err)
		}
	}
	// The number of keys: MaxKeys fit, one more does not.
	var many Set
	for i := 0; i < MaxKeys; i++ {
		var err error
		many, err = mustAdd(many, 100+i, "", t0)
		if err != nil {
			t.Fatalf("key %d: %v", i, err)
		}
	}
	if _, err := mustAdd(many, 200, "", t0); !errors.Is(err, ErrTooManyKeys) {
		t.Errorf("MaxKeys+1: %v", err)
	}
	if err := many.Validate(); err != nil {
		t.Error(err)
	}
	// Duplicates.
	if _, err := mustAdd(many[:1], 100, "", t0); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("same key twice: %v", err)
	}
	if _, err := mustAdd(many[:1], 300, many[0].TrustLabel(), t0); !errors.Is(err, ErrDuplicateKey) {
		t.Errorf("same label twice: %v", err)
	}
	if _, err := (Set{}).Add(make([]byte, 31), "", t0); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("short key: %v", err)
	}
	if _, err := (Set{}).Activate("00", t0, 0); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}
}

func TestTransitionsDoNotChangeTheReceiver(t *testing.T) {
	s := okSet(t)(mustAdd(nil, 1, "", t0))
	before := s.Summary()
	_ = okSet(t)(s.Activate(fp(1), t0, 0))
	if s.Summary() != before {
		t.Fatal("Activate changed its receiver")
	}
	s2 := okSet(t)(s.Activate(fp(1), t0, 0))
	s2[0].Public[0] ^= 0xff
	if s[0].Public[0] == s2[0].Public[0] {
		t.Fatal("the new set shares the public key bytes of the old one")
	}
}

func TestValidateRefusesInconsistentRecords(t *testing.T) {
	k := func(mod func(*Key)) Set {
		x := Key{Public: pubN(1), State: StateActive, CreatedAt: t0, ActivatedAt: t0}
		mod(&x)
		return Set{x}
	}
	bad := map[string]Set{
		"no state":                 k(func(x *Key) { x.State = 0 }),
		"no created":               k(func(x *Key) { x.CreatedAt = time.Time{} }),
		"active without activated": k(func(x *Key) { x.ActivatedAt = time.Time{} }),
		"active retired":           k(func(x *Key) { x.RetiredAt = t0 }),
		"next activated":           k(func(x *Key) { x.State = StateNext }),
		"retired no time":          k(func(x *Key) { x.State = StateRetired }),
		"retired before active":    k(func(x *Key) { x.State = StateRetired; x.RetiredAt = t0.Add(-time.Hour) }),
		"revoked no since":         k(func(x *Key) { x.State = StateRevoked; x.RevokedAt = t0 }),
		"since after revoke":       k(func(x *Key) { x.State = StateRevoked; x.RevokedAt = t0; x.CompromisedSince = t0.Add(time.Hour) }),
		"size on active":           k(func(x *Key) { x.MaxTreeSize = 5 }),
		"short key":                k(func(x *Key) { x.Public = x.Public[:31] }),
		"non-ascii reason":         k(func(x *Key) { x.Reason = "\u00e9" }),
		"three active": {
			{Public: pubN(1), State: StateActive, CreatedAt: t0, ActivatedAt: t0},
			{Public: pubN(2), State: StateActive, CreatedAt: t0, ActivatedAt: t0},
			{Public: pubN(3), State: StateActive, CreatedAt: t0, ActivatedAt: t0},
		},
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := k(func(*Key) {}).Validate(); err != nil {
		t.Error(err)
	}
}

func TestTimesAreNormalised(t *testing.T) {
	odd := time.Date(2026, 12, 1, 10, 0, 0, 999, time.FixedZone("x", 3600))
	s := okSet(t)(mustAdd(nil, 1, "", odd))
	if s[0].CreatedAt.Location() != time.UTC || s[0].CreatedAt.Nanosecond() != 0 {
		t.Fatalf("created at %v", s[0].CreatedAt)
	}
}

func okSet(t *testing.T) func(Set, error) Set {
	return func(s Set, err error) Set {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		if err := s.Validate(); err != nil {
			t.Fatalf("the set does not validate: %v", err)
		}
		return s
	}
}
