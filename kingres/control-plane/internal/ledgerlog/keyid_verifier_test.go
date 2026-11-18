// SPDX-License-Identifier: AGPL-3.0-only

package ledgerlog

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// K2-02: the key id collision matrix, against the built verifier. A pair of
// keys with one key id under the origin of a log is put into a trust file in
// each combination of states, and the verifier is asked on both of its paths:
// a fresh checkpoint (checked against the usable keys of the trust file) and a
// note it already holds (checked against every key of the file, whatever its
// standing). ledgerkeys.CheckSetKeyIDs must say, for the same set, which of the
// two refuse.

type matrixRow struct {
	name   string
	build  func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set
	opt    ledgerkeys.TrustOptions
	signer string // "a" or "b": the key that signed the note
	// What the verifier is expected to do, from the review: a refusal is any
	// status other than 0. The held path is asked with a command that checks
	// the live checkpoint too, so in a row where the fresh path refuses the held
	// answer is that same refusal; only the rows where the fresh path accepts
	// show what the held path alone does (a status of 2).
	wantFresh, wantHeld bool // true: accepted
}

func matrixSet(t *testing.T, a, b *ledgerkeys.FileSigner, steps func(s ledgerkeys.Set) (ledgerkeys.Set, error)) ledgerkeys.Set {
	t.Helper()
	ok := func(s ledgerkeys.Set, err error) ledgerkeys.Set {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	var s ledgerkeys.Set
	s = ok(s.Add(a.Public(), "a", epoch0))
	s = ok(s.Activate(a.Fingerprint(), epoch0, 0))
	s = ok(s.Add(b.Public(), "b", epoch0))
	return ok(steps(s))
}

func matrixRows() []matrixRow {
	activate := func(s ledgerkeys.Set, fp string, at time.Duration) (ledgerkeys.Set, error) {
		return s.Activate(fp, epoch0.Add(at), 0)
	}
	return []matrixRow{
		{"both active", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) { return activate(s, b.Fingerprint(), time.Hour) })
		}, ledgerkeys.TrustOptions{}, "b", false, false},
		{"A retired, B active", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) {
				s, err := activate(s, b.Fingerprint(), time.Hour)
				if err != nil {
					return nil, err
				}
				return s.Retire(a.Fingerprint(), epoch0.Add(2*time.Hour), ledgerkeys.Unbounded())
			})
		}, ledgerkeys.TrustOptions{}, "b", false, false},
		{"A revoked, B active, note by B", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) {
				s, err := activate(s, b.Fingerprint(), time.Hour)
				if err != nil {
					return nil, err
				}
				return s.Revoke(a.Fingerprint(), epoch0.Add(2*time.Hour), epoch0, "test")
			})
		}, ledgerkeys.TrustOptions{}, "b", true, false},
		{"A revoked, B active, note by A", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) {
				s, err := activate(s, b.Fingerprint(), time.Hour)
				if err != nil {
					return nil, err
				}
				return s.Revoke(a.Fingerprint(), epoch0.Add(2*time.Hour), epoch0, "test")
			})
		}, ledgerkeys.TrustOptions{}, "a", false, false},
		{"B next, not listed, note by A", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) { return s, nil })
		}, ledgerkeys.TrustOptions{}, "a", true, true},
		{"B next, listed (IncludeNext), note by A", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) { return s, nil })
		}, ledgerkeys.TrustOptions{IncludeNext: true}, "a", false, false},
		{"B next and revoked, note by A", func(t *testing.T, a, b *ledgerkeys.FileSigner) ledgerkeys.Set {
			return matrixSet(t, a, b, func(s ledgerkeys.Set) (ledgerkeys.Set, error) {
				return s.Revoke(b.Fingerprint(), epoch0.Add(time.Hour), epoch0, "test")
			})
		}, ledgerkeys.TrustOptions{}, "a", true, false},
	}
}

func TestKeyIDCollisionMatrixAgainstTheVerifier(t *testing.T) {
	verifierBin(t)
	a, b := collisionSigners(t)
	id := chainID(200)
	origin := collisionOrigin()
	for _, row := range matrixRows() {
		t.Run(row.name, func(t *testing.T) {
			set := row.build(t, a, b)
			dir := t.TempDir()
			tf, err := ledgerkeys.TrustFile(testPrefix, set, row.opt)
			if err != nil {
				t.Fatal(err)
			}
			trust := filepath.Join(dir, "trust.json")
			if err := os.WriteFile(trust, tf, 0o644); err != nil {
				t.Fatal(err)
			}
			signer := a
			if row.signer == "b" {
				signer = b
			}
			note, err := SignCheckpoint(bg, Checkpoint{Origin: origin, Size: 0, Root: EmptyRoot()}, signer)
			if err != nil {
				t.Fatal(err)
			}
			logDir := filepath.Join(dir, "c", fmt.Sprintf("%x", id[:]), "e1")
			store, err := NewDirStore(logDir)
			if err != nil {
				t.Fatal(err)
			}
			store.NoSync = true
			if err := store.PutCheckpoint(bg, note); err != nil {
				t.Fatal(err)
			}
			oldNote := filepath.Join(dir, "old.note")
			if err := os.WriteFile(oldNote, note, 0o644); err != nil {
				t.Fatal(err)
			}
			logFlag := fmt.Sprintf("chain:%x:e1", id[:])
			now := epoch0.Add(48 * time.Hour).Format(time.RFC3339)
			// The fresh path: a checkpoint nobody has held yet.
			freshSt, freshOut := runVerifier(t, "checkpoint", "--trust", trust, "--source", dir, "--now", now, "--log", logFlag)
			// The held path: a note the auditor already has, given as the first
			// state of a state file (it is checked against every key of the file).
			state := filepath.Join(t.TempDir(), "state.json")
			heldSt, heldOut := runVerifier(t, "checkpoint", "--trust", trust, "--source", dir, "--now", now, "--log", logFlag,
				"--note", oldNote, "--state", state, "--allow-shared-state-dir")
			t.Logf("fresh: status %d, held: status %d", freshSt, heldSt)
			if (freshSt == 0) != row.wantFresh {
				t.Errorf("fresh checkpoint: status %d, accepted=%v expected\n%s", freshSt, row.wantFresh, freshOut)
			}
			if (heldSt == 0) != row.wantHeld {
				t.Errorf("held note: status %d, accepted=%v expected\n%s", heldSt, row.wantHeld, heldOut)
			}
			// What the library says about the same set. A refusal of the note by
			// the verifier that is not about two keys with one id (the note of
			// a revoked key) is not a collision, so only the rows whose note is
			// by a usable key are compared.
			cerr := ledgerkeys.CheckSetKeyIDs(set, row.opt, origin)
			var c *ledgerkeys.KeyIDCollision
			collides := errors.As(cerr, &c)
			if row.name == "A revoked, B active, note by A" {
				if !collides || c.Fresh || !c.Held {
					t.Errorf("CheckSetKeyIDs: %v (Fresh=%v Held=%v)", cerr, collides && c.Fresh, collides && c.Held)
				}
				return
			}
			if wantCollide := !row.wantHeld; collides != wantCollide {
				t.Fatalf("CheckSetKeyIDs: collision=%v (%v), the verifier's held path refuses=%v", collides, cerr, wantCollide)
			}
			if collides && (c.Fresh != !row.wantFresh || !c.Held) {
				t.Errorf("CheckSetKeyIDs says Fresh=%v Held=%v, the verifier refuses the fresh path=%v", c.Fresh, c.Held, !row.wantFresh)
			}
		})
	}
}
