// SPDX-License-Identifier: AGPL-3.0-only

package fence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestTokenRoundTrip(t *testing.T) {
	tok := Token{Task: 12, Epoch: 3}
	got, err := Parse(tok.String())
	if err != nil || got != tok {
		t.Fatalf("Parse(%q) = %v, %v", tok.String(), got, err)
	}
	for _, bad := range []string{"", "1", "a:b", "0:1", "1:-1", "1:2:3"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) accepted", bad)
		}
	}
	ctx := With(context.Background(), tok)
	if g, ok := From(ctx); !ok || g != tok {
		t.Fatal("the token did not travel in the context")
	}
	if _, ok := From(context.Background()); ok {
		t.Fatal("a context without a token has one")
	}
}

func TestLateStepOfAnOlderEpochIsRefused(t *testing.T) {
	r := NewRegistry()
	if err := r.Check(Token{7, 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.Check(Token{7, 1}); err != nil {
		t.Fatalf("the same epoch must be accepted again: %v", err)
	}
	if err := r.Check(Token{7, 2}); err != nil {
		t.Fatalf("a newer epoch must be accepted: %v", err)
	}
	if err := r.Check(Token{7, 1}); !errors.Is(err, ErrFenced) {
		t.Fatalf("the late step of epoch 1 after epoch 2 = %v, want ErrFenced", err)
	}
	if err := r.Check(Token{8, 1}); err != nil {
		t.Fatalf("another task is independent: %v", err)
	}
}

func TestRegistrySurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Check(Token{5, 4}); err != nil {
		t.Fatal(err)
	}
	r2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := r2.Check(Token{5, 3}); !errors.Is(err, ErrFenced) {
		t.Fatalf("a restarted callee forgot: %v", err)
	}
}

func TestDamagedFileStartsEmptyAndReportsIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fence.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(dir)
	if err == nil || r == nil {
		t.Fatalf("a damaged file must be reported with a usable registry: %v", err)
	}
	if err := r.Check(Token{1, 0}); err != nil {
		t.Fatal(err)
	}
}

func TestOldEntriesArePruned(t *testing.T) {
	r := NewRegistry()
	for i := int64(1); i <= maxEntries+10; i++ {
		_ = r.Check(Token{i, 1})
	}
	if len(r.seen) != maxEntries {
		t.Fatalf("%d entries, want %d", len(r.seen), maxEntries)
	}
}

func TestNilRegistryAcceptsEverything(t *testing.T) {
	var r *Registry
	if err := r.Check(Token{1, 1}); err != nil {
		t.Fatal(err)
	}
}
