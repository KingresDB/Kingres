// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerlog

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// The tests of this file are the checks of the third round of fixes (the
// numbering is that of the third review: K3-nn; correction 107 of the design).

// usable lists keys as trust file entries with no restriction: not revoked, no
// size limit.
func usable(pubs ...ed25519.PublicKey) []ledgerkeys.TrustKey {
	var out []ledgerkeys.TrustKey
	for _, p := range pubs {
		out = append(out, ledgerkeys.TrustKey{Public: p})
	}
	return out
}

func sizeLimit(pub ed25519.PublicKey, n uint64) ledgerkeys.TrustKey {
	return ledgerkeys.TrustKey{Public: pub, MaxTreeSize: &n}
}

func revokedKey(pub ed25519.PublicKey) ledgerkeys.TrustKey {
	return ledgerkeys.TrustKey{Public: pub, Revoked: true}
}

// ---- K3-01: re-signing needs a note that rests on a key that counts ----

// The rule without the verifier: the standing of each key comes with the key.
func TestResignCheckpointNeedsAVouchingKey(t *testing.T) {
	k1, k2, k3 := testKey(t, 1), testKey(t, 2), testKey(t, 3)
	cp := Checkpoint{Origin: testOrigin(1), Size: 7, Root: refMTH(leafHashesOf(7))}
	by := func(signers ...ledgerkeys.Signer) []byte {
		n, err := SignCheckpoint(bg, cp, signers...)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, tc := range []struct {
		name    string
		note    []byte
		trusted []ledgerkeys.TrustKey
		ok      bool
	}{
		{"revoked key only", by(k1), []ledgerkeys.TrustKey{revokedKey(k1.Public()), {Public: k2.Public()}}, false},
		{"revoked key and a good key", by(k1, k2), []ledgerkeys.TrustKey{revokedKey(k1.Public()), {Public: k2.Public()}}, true},
		{"retired within its bound", by(k1), []ledgerkeys.TrustKey{sizeLimit(k1.Public(), 7), {Public: k2.Public()}}, true},
		{"retired a leaf beyond its bound", by(k1), []ledgerkeys.TrustKey{sizeLimit(k1.Public(), 6), {Public: k2.Public()}}, false},
		{"retired far below its size", by(k1), []ledgerkeys.TrustKey{sizeLimit(k1.Public(), 1), {Public: k2.Public()}}, false},
		{"retired beyond its bound, and a good key", by(k1, k2), []ledgerkeys.TrustKey{sizeLimit(k1.Public(), 6), {Public: k2.Public()}}, true},
		{"retired without a bound", by(k1), []ledgerkeys.TrustKey{{Public: k1.Public()}, {Public: k2.Public()}}, true},
		{"a bound of 2^62-1", by(k1), []ledgerkeys.TrustKey{sizeLimit(k1.Public(), ledgerkeys.MaxTrustTreeSize), {Public: k2.Public()}}, true},
	} {
		out, err := ResignCheckpoint(bg, tc.note, tc.trusted, k3)
		if tc.ok {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
				continue
			}
			c, signers, err := VerifyCheckpoint(out, cp.Origin, k1.Public(), k2.Public(), k3.Public())
			if err != nil || c.Size != cp.Size || len(signers) < 2 {
				t.Errorf("%s: the re-signed note: %v %d signers", tc.name, err, len(signers))
			}
			continue
		}
		if !errors.Is(err, ErrNoVouchingSignature) || out != nil {
			t.Errorf("%s: %v (a note was returned: %v)", tc.name, err, out != nil)
		}
		if errors.Is(err, ErrNoTrustedSignature) {
			t.Errorf("%s: the refusal is that of a note that does not verify", tc.name)
		}
	}
	// A note that no listed key signed keeps the error of VerifyNote, not the new one.
	_, err := ResignCheckpoint(bg, by(testKey(t, 9)), usable(k1.Public(), k2.Public()), k3)
	if !errors.Is(err, ErrNoTrustedSignature) || errors.Is(err, ErrNoVouchingSignature) {
		t.Errorf("a stranger's note: %v", err)
	}
	// With no new signer nothing is signed, but the rule still holds: a note of a
	// revoked key is refused.
	if _, err := ResignCheckpoint(bg, by(k1), []ledgerkeys.TrustKey{revokedKey(k1.Public())}); !errors.Is(err, ErrNoVouchingSignature) {
		t.Errorf("no new signer: %v", err)
	}
}

// The same rule against the built verifier: the note that the verifier refuses
// as a fresh checkpoint is refused by ResignCheckpoint, and the notes that are
// re-signed are accepted.
func TestResignCheckpointAgainstTheVerifier(t *testing.T) {
	verifierBin(t)
	k1, k2, k3 := testKey(t, 1), testKey(t, 2), testKey(t, 3)
	type scenario struct {
		name   string
		n      int
		build  func(t *testing.T) ledgerkeys.Set
		signed []ledgerkeys.Signer
		// wantBefore: the verifier accepts the note as it is; wantResign: the
		// function re-signs it (and the verifier accepts the result).
		wantBefore, wantResign bool
	}
	ok := func(s ledgerkeys.Set, err error) ledgerkeys.Set {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	// k1 active from epoch0; k2 active from +1h (an overlap), then k1 leaves.
	base := func(t *testing.T) ledgerkeys.Set {
		s := ok(ledgerkeys.Set{}.Add(k1.Public(), "k1", epoch0))
		s = ok(s.Activate(k1.Fingerprint(), epoch0, 0))
		s = ok(s.Add(k2.Public(), "k2", epoch0))
		return ok(s.Activate(k2.Fingerprint(), epoch0.Add(time.Hour), 0))
	}
	revoked := func(t *testing.T) ledgerkeys.Set {
		return ok(base(t).Revoke(k1.Fingerprint(), epoch0.Add(2*time.Hour), epoch0, "leaked"))
	}
	retired := func(bound ledgerkeys.TreeBound) func(t *testing.T) ledgerkeys.Set {
		return func(t *testing.T) ledgerkeys.Set {
			return ok(base(t).Retire(k1.Fingerprint(), epoch0.Add(2*time.Hour), bound))
		}
	}
	// The new signer k3 joins in every scenario, activated after the others.
	withNew := func(f func(t *testing.T) ledgerkeys.Set) func(t *testing.T) ledgerkeys.Set {
		return func(t *testing.T) ledgerkeys.Set {
			s := ok(f(t).Add(k3.Public(), "k3", epoch0.Add(2*time.Hour)))
			return ok(s.Activate(k3.Fingerprint(), epoch0.Add(3*time.Hour), 0))
		}
	}
	for i, sc := range []scenario{
		{"revoked key only", 5, withNew(revoked), []ledgerkeys.Signer{k1}, false, false},
		{"revoked key and a good key", 5, withNew(revoked), []ledgerkeys.Signer{k1, k2}, true, true},
		{"retired, size within the bound", 5, withNew(retired(ledgerkeys.AtMost(5))), []ledgerkeys.Signer{k1}, true, true},
		{"retired, one leaf beyond the bound", 6, withNew(retired(ledgerkeys.AtMost(5))), []ledgerkeys.Signer{k1}, false, false},
		{"retired beyond the bound, and a good key", 6, withNew(retired(ledgerkeys.AtMost(5))), []ledgerkeys.Signer{k1, k2}, true, true},
		{"retired without a bound", 6, withNew(retired(ledgerkeys.Unbounded())), []ledgerkeys.Signer{k1}, true, true},
	} {
		t.Run(sc.name, func(t *testing.T) {
			set := sc.build(t)
			dir := t.TempDir()
			id := chainID(60 + i)
			origin, err := ChainOrigin(testPrefix, id, 1)
			if err != nil {
				t.Fatal(err)
			}
			store, err := NewDirStore(filepath.Join(dir, "c", fmt.Sprintf("%x", id[:]), "e1"))
			if err != nil {
				t.Fatal(err)
			}
			store.NoSync = true
			l, err := Create(bg, origin, store)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Append(bg, entriesRange(0, sc.n)); err != nil {
				t.Fatal(err)
			}
			note, err := SignCheckpoint(bg, l.Checkpoint(), sc.signed...)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PutCheckpoint(bg, note); err != nil {
				t.Fatal(err)
			}
			tf, err := ledgerkeys.TrustFile(testPrefix, set, ledgerkeys.TrustOptions{})
			if err != nil {
				t.Fatal(err)
			}
			trust := filepath.Join(dir, "trust.json")
			if err := os.WriteFile(trust, tf, 0o644); err != nil {
				t.Fatal(err)
			}
			now := epoch0.Add(48 * time.Hour).Format(time.RFC3339)
			args := []string{"checkpoint", "--trust", trust, "--source", dir, "--now", now, "--log", fmt.Sprintf("chain:%x:e1", id[:])}
			st, out := runVerifier(t, args...)
			if (st == 0) != sc.wantBefore {
				t.Fatalf("the verifier on the note as it is: status %d, accepted=%v expected\n%s", st, sc.wantBefore, out)
			}
			entries, err := ledgerkeys.TrustEntries(set, ledgerkeys.TrustOptions{IncludeNext: true})
			if err != nil {
				t.Fatal(err)
			}
			resigned, err := ResignCheckpoint(bg, note, entries, k3)
			if !sc.wantResign {
				if !errors.Is(err, ErrNoVouchingSignature) || resigned != nil {
					t.Fatalf("ResignCheckpoint: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResignCheckpoint: %v", err)
			}
			if err := store.PutCheckpoint(bg, resigned); err != nil {
				t.Fatal(err)
			}
			if st, out := runVerifier(t, args...); st != 0 {
				t.Fatalf("the verifier on the re-signed note: status %d\n%s", st, out)
			}
		})
	}
}

// ---- K3-07: constants against literals ----

func TestLimitsAreTheDocumentedNumbers(t *testing.T) {
	for _, c := range []struct {
		name      string
		got, want uint64
	}{
		{"MaxOriginSize", MaxOriginSize, 255}, {"MaxExtensionLines", MaxExtensionLines, 16},
		{"HashSize", HashSize, 32}, {"MaxTreeSize", MaxTreeSize, 4611686018427387904},
		{"MaxFrontierSize", MaxFrontierSize, 1 + 8 + 62*32}, {"MaxInclusionPath", MaxInclusionPath, 64},
		{"MaxConsistencyPath", MaxConsistencyPath, 65}, {"MaxMembers", MaxMembers, 512},
		{"AggregateHeaderSize", AggregateHeaderSize, 21}, {"AggregateMemberSize", AggregateMemberSize, 109},
		{"MaxAggregateSize", MaxAggregateSize, 21 + 512*109}, {"MaxNoteSize", MaxNoteSize, 65536},
		{"MaxSignatures", MaxSignatures, 32}, {"MaxLineLength", MaxLineLength, 1024}, {"MaxTextLines", MaxTextLines, 32},
		{"MaxEntrySize", MaxEntrySize, 65535}, {"MaxBatch", MaxBatch, 10000}, {"MaxBatchBytes", MaxBatchBytes, 64 * 1024 * 1024},
		{"MaxLevel", MaxLevel, 63}, {"MaxRequiredSize", MaxRequiredSize, 268435456},
		{"TileWidth", TileWidth, 256}, {"DefaultCheckpointRetry (s)", uint64(DefaultCheckpointRetry / time.Second), 3},
	} {
		if c.got != c.want {
			t.Errorf("%s is %d, the documentation says %d", c.name, c.got, c.want)
		}
	}
}

// ---- K3-09: EPERM is not only "no hard links" ----

func TestHardLinksErrorNamesTheOtherCause(t *testing.T) {
	msg := ErrHardLinksRequired.Error()
	for _, want := range []string{"file system with hard links", "immutable", "append-only"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q: %s", want, msg)
		}
	}
}
