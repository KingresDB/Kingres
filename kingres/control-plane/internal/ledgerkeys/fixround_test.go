// SPDX-License-Identifier: AGPL-3.0-only

package ledgerkeys

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The tests of this file are the checks that the fixes of the review of the
// key library need (the numbering is that of the review).

// twoActive returns a set with fp(1) active from t0 and fp(2) active from act,
// so fp(1) is the older key and has an overlap.
func twoActive(t *testing.T, act time.Time) Set {
	t.Helper()
	var s Set
	s = okSet(t)(mustAdd(s, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0, 0))
	s = okSet(t)(mustAdd(s, 2, "", t0))
	return okSet(t)(s.Activate(fp(2), act, 0))
}

// K1-02: the bound of a retired key is the verifier's: 1 to 2^62-1.
func TestRetireBoundRange(t *testing.T) {
	act := t0.Add(time.Hour)
	s := twoActive(t, act)
	now := act.Add(time.Hour)
	for _, b := range []TreeBound{{}, AtMost(0), AtMost(1 << 62), AtMost(1<<63 + 5), AtMost(^uint64(0))} {
		if _, err := s.Retire(fp(1), now, b); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("bound %+v: %v", b, err)
		}
	}
	got := okSet(t)(s.Retire(fp(1), now, AtMost(MaxTrustTreeSize)))
	if k, _ := got.Get(fp(1)); k.MaxTreeSize != 1<<62-1 {
		t.Errorf("the largest bound was not kept: %d", k.MaxTreeSize)
	}
	got = okSet(t)(s.Retire(fp(1), now, AtMost(1)))
	if k, _ := got.Get(fp(1)); k.MaxTreeSize != 1 {
		t.Errorf("the smallest bound was not kept: %d", k.MaxTreeSize)
	}
	got = okSet(t)(s.Retire(fp(1), now, Unbounded()))
	if k, _ := got.Get(fp(1)); k.MaxTreeSize != 0 || k.State != StateRetired {
		t.Errorf("unbounded: %+v", k)
	}
	// A key loaded from storage is held to the same range.
	for _, size := range []uint64{1 << 62, 1<<62 + 1, ^uint64(0)} {
		loaded := append(Set(nil), got...)
		loaded[0].MaxTreeSize = size
		if err := loaded.Validate(); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("a loaded size of %d: %v", size, err)
		}
		if _, err := TrustFile("p", loaded, TrustOptions{}); err == nil {
			t.Errorf("a trust file was written for a loaded size of %d", size)
		}
	}
	loaded := append(Set(nil), got...)
	loaded[0].MaxTreeSize = 1<<62 - 1
	if err := loaded.Validate(); err != nil {
		t.Errorf("the largest size is refused on load: %v", err)
	}
}

// K1-02: the verifier reads what Retire writes, and refuses the value the
// package used to let through.
func TestRetireBoundAgainstTheVerifier(t *testing.T) {
	verifierBin(t)
	dir := t.TempDir()
	act := t0.Add(time.Hour)
	s := twoActive(t, act)
	now := act.Add(time.Hour)
	show := func(name string, b []byte) (int, string) {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
		return runVerifier(t, "trust", "show", "--trust", p, "--now", "2027-01-01T00:00:00Z")
	}
	for name, bound := range map[string]TreeBound{"max.json": AtMost(1<<62 - 1), "one.json": AtMost(1), "none.json": Unbounded()} {
		set := okSet(t)(s.Retire(fp(1), now, bound))
		b, err := TrustFile("ledger.example.com/kingres", set, TrustOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if st, out := show(name, b); st != 0 {
			t.Errorf("%s: the verifier refuses the trust file: %d\n%s\n%s", name, st, out, b)
		}
	}
	// What the package must never write: 2^62. Written by hand, the verifier
	// refuses the whole file, which is why Retire and Validate refuse it.
	set := okSet(t)(s.Retire(fp(1), now, AtMost(1)))
	entries, err := TrustEntries(set, TrustOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		if entries[i].MaxTreeSize != nil {
			v := uint64(1) << 62
			entries[i].MaxTreeSize = &v
		}
	}
	b, err := marshalTrust("ledger.example.com/kingres", entries)
	if err != nil {
		t.Fatal(err)
	}
	if st, out := show("big.json", b); st != 2 || !strings.Contains(out, "max_tree_size") {
		t.Errorf("a size of 2^62 in the trust file: status %d\n%s", st, out)
	}
}

// K1-09 (a): the other key must be in force at now.
func TestRetireAndRevokeNeedAnotherKeyInForce(t *testing.T) {
	act := t0.Add(48 * time.Hour)
	s := twoActive(t, act) // fp(2) is active in the record from t0+48h
	early := t0.Add(time.Hour)
	if _, err := s.Retire(fp(1), early, Unbounded()); !errors.Is(err, ErrNoOtherSigner) {
		t.Errorf("Retire with the other key not yet in force: %v", err)
	}
	if _, err := s.Revoke(fp(1), early, early, "x"); !errors.Is(err, ErrNoOtherSigner) {
		t.Errorf("Revoke with the other key not yet in force: %v", err)
	}
	// Signers is empty for fp(2) until its time: only fp(1) signs.
	if got, err := s.Signers(early); err != nil || len(got) != 1 || got[0].Fingerprint() != fp(1) {
		t.Errorf("signers before the activation: %v %v", got, err)
	}
	if _, err := s.Retire(fp(1), act, Unbounded()); err != nil {
		t.Errorf("Retire when the other key is in force: %v", err)
	}
	if _, err := s.Revoke(fp(1), act, act, "x"); err != nil {
		t.Errorf("Revoke when the other key is in force: %v", err)
	}
}

// K1-12 and K2-04: a key that is active but not in force at the time of the
// call (its activation is later) never signed, so it can be retired without a
// revocation; it is retired at the time it would have come into force, so the
// record stays in order.
func TestRetireBeforeActivation(t *testing.T) {
	// fp(2) is in force from t0; fp(1) is the one activated later, at t0+2h.
	var s Set
	s = okSet(t)(mustAdd(s, 2, "", t0))
	s = okSet(t)(s.Activate(fp(2), t0, 0))
	s = okSet(t)(mustAdd(s, 1, "", t0))
	s = okSet(t)(s.Activate(fp(1), t0.Add(2*time.Hour), 0))
	early, err := s.Retire(fp(1), t0.Add(time.Hour), Unbounded())
	if err != nil {
		t.Fatalf("a retirement before the activation: %v", err)
	}
	if k, _ := early.Get(fp(1)); k.State != StateRetired || !k.RetiredAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("the key retired before it was in force: %+v", k)
	}
	if err := early.Validate(); err != nil {
		t.Errorf("the set after it: %v", err)
	}
	if _, err := s.Retire(fp(1), t0.Add(2*time.Hour), Unbounded()); err != nil {
		t.Fatalf("a retirement at the activation time: %v", err)
	}
}

// K1-12: MaySign refuses a checkpoint before the activation time, and allows
// it at that time.
func TestMaySignBeforeActivation(t *testing.T) {
	k := Key{Public: pubN(1), State: StateActive, CreatedAt: t0, ActivatedAt: t0.Add(time.Hour)}
	if err := k.MaySign(PurposeCheckpoint, "", t0.Add(time.Hour-time.Second)); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("a key signs before its activation: %v", err)
	}
	if err := k.MaySign(PurposeCheckpoint, "", t0.Add(time.Hour)); err != nil {
		t.Errorf("a key does not sign at its activation: %v", err)
	}
}

// K1-09 (b): what Retire and Revoke leave behind is consistent.
func TestStateAfterRetireAndRevokeIsConsistent(t *testing.T) {
	act := t0.Add(time.Hour)
	now := act.Add(time.Hour)
	s := twoActive(t, act)
	if k, _ := s.Get(fp(1)); k.OverlapEnds.IsZero() {
		t.Fatal("the older key has no overlap")
	}
	check := func(what string, got Set, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("%s: the set does not validate: %v", what, err)
		}
		for _, k := range got {
			if !k.OverlapEnds.IsZero() {
				t.Errorf("%s: key %s keeps an overlap end (state %s)", what, k.Fingerprint()[:8], k.State)
			}
		}
	}
	// The younger key goes: the older one is the only active key again and
	// its overlap is over.
	got, err := s.Retire(fp(2), now, Unbounded())
	check("retire of the younger key", got, err)
	got, err = s.Revoke(fp(2), now, now, "x")
	check("revoke of the younger key", got, err)
	// The older key goes.
	got, err = s.Retire(fp(1), now, Unbounded())
	check("retire of the older key", got, err)
	got, err = s.Revoke(fp(1), now, now, "x")
	check("revoke of the older key", got, err)
	// A revoked next key, with two keys active, does not touch the overlap.
	s3 := okSet(t)(mustAdd(s, 3, "", t0))
	got, err = s3.Revoke(fp(3), now, now, "x")
	if err != nil {
		t.Fatal(err)
	}
	if k, _ := got.Get(fp(1)); k.OverlapEnds.IsZero() {
		t.Error("revoking a next key ended a running overlap")
	}
	// An overlap end on a revoked key is refused even while two keys are active.
	withEnd := append(Set(nil), got...)
	for i := range withEnd {
		if withEnd[i].State == StateRevoked {
			withEnd[i].OverlapEnds = now
		}
	}
	if err := withEnd.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("an overlap end on a revoked key: %v", err)
	}
	// A stale overlap in a loaded set is refused.
	stale := append(Set(nil), got...)
	stale, err = stale.Retire(fp(2), now, Unbounded())
	if err != nil {
		t.Fatal(err)
	}
	stale[0].OverlapEnds = now.Add(time.Hour)
	if err := stale.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a stale overlap end on the only active key: %v", err)
	}
	stale[0].OverlapEnds = time.Time{}
	for i := range stale {
		if stale[i].State == StateRetired {
			stale[i].OverlapEnds = now
		}
	}
	if err := stale.Validate(); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("an overlap end on a retired key: %v", err)
	}
}

// K1-09 (c): a set without an active key.
func TestNoActiveKey(t *testing.T) {
	// Signers: an error, not an empty list.
	var empty Set
	if got, err := empty.Signers(t0); !errors.Is(err, ErrNoActiveKey) || got != nil {
		t.Errorf("empty set: %v %v", got, err)
	}
	// A new set before its first activation is legal, and cannot sign yet.
	fresh := okSet(t)(mustAdd(nil, 1, "", t0))
	fresh = okSet(t)(mustAdd(fresh, 2, "", t0))
	if err := fresh.Validate(); err != nil {
		t.Errorf("a new set was refused: %v", err)
	}
	if err := empty.Validate(); err != nil {
		t.Errorf("the empty set was refused: %v", err)
	}
	if _, err := fresh.Signers(t0); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("a new set signs: %v", err)
	}
	// A key that is active in the record but not yet in force does not sign.
	future := okSet(t)(okSet(t)(mustAdd(nil, 1, "", t0)).Activate(fp(1), t0.Add(time.Hour), 0))
	if _, err := future.Signers(t0); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("a key signs before its time: %v", err)
	}
	if err := future.Validate(); err != nil {
		t.Errorf("a set with a key that waits for its time is legal: %v", err)
	}
	// A set in service (a key left the state next) without an active key is
	// damaged: Validate says so with its own error.
	s := twoActive(t, t0.Add(time.Hour))
	now := t0.Add(2 * time.Hour)
	damaged := okSet(t)(s.Retire(fp(1), now, Unbounded()))
	for i := range damaged {
		if damaged[i].State == StateActive {
			damaged[i].State, damaged[i].RetiredAt = StateRetired, now
		}
	}
	if err := damaged.Validate(); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("a set in service without an active key: %v", err)
	}
	if _, err := TrustFile("p", damaged, TrustOptions{}); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("a trust file for it: %v", err)
	}
	revokedOnly := Set{{Public: pubN(5), State: StateRevoked, CreatedAt: t0, RevokedAt: now, CompromisedSince: t0}}
	if err := revokedOnly.Validate(); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("a set with a revoked key only: %v", err)
	}
	if _, err := damaged.Signers(now); !errors.Is(err, ErrNoActiveKey) {
		t.Errorf("signers of it: %v", err)
	}
}

// K1-09 (d): Retire cannot be called without a decision about the bound.
func TestRetireNeedsADecision(t *testing.T) {
	s := twoActive(t, t0.Add(time.Hour))
	if _, err := s.Retire(fp(1), t0.Add(2*time.Hour), TreeBound{}); !errors.Is(err, ErrInvalidArgument) || !strings.Contains(err.Error(), "Unbounded") {
		t.Fatalf("the zero bound: %v", err)
	}
}

// K1-09 (e) and the limit: the set is full at 16 keys, retired ones included.
func TestFullSetRefusesNewKeys(t *testing.T) {
	var s Set
	for i := 1; i <= MaxKeys; i++ {
		s = okSet(t)(mustAdd(s, i, "", t0))
	}
	if _, err := mustAdd(s, MaxKeys+1, "", t0); !errors.Is(err, ErrTooManyKeys) {
		t.Fatalf("a 17th key: %v", err)
	}
}

// The seeds below give two keys with the same 32 bit key id under the origin
// collisionOrigin (found once by search, 83,653 seeds of the form counter).
func collisionSeed(n uint64) []byte {
	seed := make([]byte, SeedSize)
	binary.BigEndian.PutUint64(seed[24:], n)
	return seed
}

func collisionOrigin() string {
	id := make([]byte, 32)
	for j := range id {
		id[j] = byte(200*31 + j*7 + 1)
	}
	return fmt.Sprintf("ledger.test/kingres/c/%x/e1", id)
}

func collidingKeys(t testing.TB) (a, b ed25519.PublicKey) {
	t.Helper()
	ka, err := NewFileSigner(collisionSeed(12362))
	if err != nil {
		t.Fatal(err)
	}
	kb, err := NewFileSigner(collisionSeed(21524))
	if err != nil {
		t.Fatal(err)
	}
	a, b = ka.Public(), kb.Public()
	if KeyID(collisionOrigin(), a) != KeyID(collisionOrigin(), b) || bytes.Equal(a, b) {
		t.Fatal("the stored seeds do not collide any more")
	}
	return a, b
}

// K1-07: a collision is recognised from the public keys and said as what it is.
func TestCheckKeyIDs(t *testing.T) {
	a, b := collidingKeys(t)
	err := CheckKeyIDs(collisionOrigin(), pubN(1), a, pubN(2), b)
	var c *KeyIDCollision
	if !errors.Is(err, ErrKeyIDCollision) || !errors.As(err, &c) {
		t.Fatalf("no collision reported: %v", err)
	}
	if c.A != Fingerprint(a) || c.B != Fingerprint(b) || c.Name != collisionOrigin() || c.ID != KeyID(collisionOrigin(), a) {
		t.Errorf("the collision names %+v", c)
	}
	msg := err.Error()
	for _, want := range []string{Fingerprint(a)[:16], Fingerprint(b)[:16], fmt.Sprintf("%08x", c.ID), "another generated", "AddChecked", "CheckSetKeyIDs"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the message lacks %q: %s", want, msg)
		}
	}
	if strings.Contains(msg, "already signed") {
		t.Errorf("the message is the wrong one: %s", msg)
	}
	// The same key twice is not a collision, nor are two keys under a name
	// where their ids differ.
	if err := CheckKeyIDs(collisionOrigin(), a, a, pubN(1)); err != nil {
		t.Errorf("the same key twice: %v", err)
	}
	if err := CheckKeyIDs("another/name", a, b); err != nil {
		t.Errorf("another name: %v", err)
	}
	if err := CheckKeyIDs(collisionOrigin(), pubN(1)[:5]); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("a short key: %v", err)
	}
}

// K1-12: a key file or passphrase file that is swapped for another file
// between the Lstat and the open is refused (os.SameFile), not read.
func TestPrivateFileSwappedBetweenStatAndOpen(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("on Windows os.SameFile finds the identity of an Lstat result by its path, at the time of the call, so it cannot see a swap of one regular file for another")
	}
	dir := t.TempDir()
	key := seededKey(t, 31)
	other := seededKey(t, 32)
	keyPath := filepath.Join(dir, "a.key")
	otherPath := filepath.Join(dir, "b.key")
	for p, s := range map[string]*FileSigner{keyPath: key, otherPath: other} {
		if err := WriteFile(p, *s, Passphrase{}, false); err != nil {
			t.Fatal(err)
		}
	}
	swapped := false
	afterLstat = func(path string) {
		if swapped {
			return
		}
		swapped = true
		// Another regular file takes the name, as an attacker with write
		// access to the directory could make it.
		if err := os.Rename(otherPath, path); err != nil {
			t.Errorf("swap: %v", err)
		}
	}
	t.Cleanup(func() { afterLstat = nil })
	got, err := ReadFile(keyPath, Passphrase{})
	if err == nil {
		got.Close()
		t.Fatal("the swapped file was read")
	}
	if !strings.Contains(err.Error(), "changed while it was opened") {
		t.Errorf("the error is %v", err)
	}
	// Without a swap the same call reads the file (the hook is idle now).
	if got, err := ReadFile(keyPath, Passphrase{}); err != nil || !got.SameKey(*other) {
		t.Errorf("an unswapped read: %v", err)
	}
	// The same check guards a passphrase file.
	pwPath := filepath.Join(dir, "pw")
	pw2 := filepath.Join(dir, "pw2")
	if err := os.WriteFile(pwPath, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pw2, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	swapped = false
	afterLstat = func(path string) {
		if !swapped {
			swapped = true
			if err := os.Rename(pw2, path); err != nil {
				t.Errorf("swap: %v", err)
			}
		}
	}
	if p, err := ReadPassphraseFile(pwPath); err == nil {
		p.Destroy()
		t.Fatal("the swapped passphrase file was read")
	}
}

// K1-08: the plain file is parsed from bytes, and it is still strict: every
// departure from the canonical form is refused, without a panic.
func TestPlainFileStrictness(t *testing.T) {
	good := Encode0(t, 41)
	if f, err := Parse(good); err != nil {
		t.Fatal(err)
	} else {
		f.Destroy()
	}
	head := FileMagic + "\n" + plainHead
	b64 := string(good[len(head) : len(head)+44])
	flipLast := []byte(b64)
	flipLast[42] = 'B' // the last character before the padding carries bits that must be zero
	cases := map[string]string{
		"crlf inside base64":      head + b64[:20] + "\r\n" + b64[22:] + "\"}\n",
		"newline inside base64":   head + b64[:20] + "\n\n" + b64[22:] + "\"}\n",
		"short base64":            head + b64[:43] + "\"}\n",
		"long base64":             head + b64 + "AAAA" + "\"}\n",
		"no padding":              head + b64[:43] + "AA\"}\n",
		"trailing space":          head + b64 + "\"} \n",
		"no final newline":        head + b64 + "\"}",
		"second newline":          head + b64 + "\"}\n\n",
		"space after brace":       head + b64 + "\" }\n",
		"url alphabet":            head + b64[:3] + "-" + b64[4:] + "\"}\n",
		"other member":            FileMagic + "\n" + `{"seeds":"` + b64 + "\"}\n",
		"space before the seed":   FileMagic + "\n" + `{ "seed":"` + b64 + "\"}\n",
		"second member":           head + b64 + `","x":1}` + "\n",
		"empty object":            FileMagic + "\n{}\n",
		"kdf with one member":     FileMagic + "\n" + `{"kdf":"pbkdf2-hmac-sha256"}` + "\n",
		"magic only":              FileMagic + "\n",
		"wrong padding bits":      head + string(flipLast) + "\"}\n",
		"seed that is not base64": head + strings.Repeat("!", 44) + "\"}\n",
	}
	for name, in := range cases {
		if name == "wrong padding bits" && string(flipLast) == b64 {
			continue
		}
		f, err := Parse([]byte(in))
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", name, err)
			f.Destroy()
		}
	}
}

// Encode0 returns the plain key file of the seededKey n.
func Encode0(t *testing.T, n byte) []byte {
	t.Helper()
	s := seededKey(t, n)
	defer s.Close()
	b, err := Encode(*s, Passphrase{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
