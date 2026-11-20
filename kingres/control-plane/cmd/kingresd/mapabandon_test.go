// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover/journal"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func abandonRig(t *testing.T) (*mapFailover, *journal.Store) {
	t.Helper()
	js, err := journal.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &mapFailover{journal: js, log: slog.New(slog.NewTextHandler(io.Discard, nil)), self: "s2"}, js
}

// F4-06. An operator gives up a bootstrap that did not reach the promotion: it ends at once, nothing is in progress after it, and a second call
// finds none.
func TestABootstrapThatDidNotPromoteCanBeGivenUp(t *testing.T) {
	m, js := abandonRig(t)
	if _, err := js.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	ok, err := m.abandonLocal("stuck")
	if err != nil || !ok {
		t.Fatalf("abandon: %v %v", ok, err)
	}
	if js.Active() {
		t.Fatal("the bootstrap is still in progress")
	}
	if ok, err := m.abandonLocal("again"); ok || err != nil {
		t.Fatalf("a second call: %v %v", ok, err)
	}
}

// A bootstrap that promoted the standby cannot be given up: 409, and it stays in progress.
func TestABootstrapThatPromotedCannotBeGivenUp(t *testing.T) {
	m, js := abandonRig(t)
	if _, err := js.Begin("b1", "s1", 3, "manual", "alice", false); err != nil {
		t.Fatal(err)
	}
	if err := js.Done(journal.StepPromote); err != nil {
		t.Fatal(err)
	}
	ok, err := m.abandonLocal("stuck")
	var p *problem.Problem
	if ok || !errors.As(err, &p) || p.Code != problem.Conflict {
		t.Fatalf("abandon after the promotion: %v %v", ok, err)
	}
	if !js.Active() {
		t.Fatal("the bootstrap was given up after it promoted")
	}
}

// Nothing in progress: false, no error.
func TestNothingToGiveUp(t *testing.T) {
	m, _ := abandonRig(t)
	if ok, err := m.abandonLocal("x"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
}
