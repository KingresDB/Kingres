// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package lease

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func holdDir(t *testing.T, candidate string) string {
	t.Helper()
	dir := filepath.Join(tempDir(t), "lease")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MarkerPath(dir, RelinquishedMarker, 0), []byte("holding back\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if candidate != "" {
		if err := os.WriteFile(MarkerPath(dir, CandidateMarker, 0), []byte(candidate+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The offline release ends the hold of the group: the markers of the hand-over and of the candidate go and the wait marker stays, so that the start gate still waits for
// the lease; it is repeatable.
func TestReleaseHoldEndsTheHoldAndKeepsTheStartWaitingForTheLease(t *testing.T) {
	dir := holdDir(t, "s3")
	cand, err := ReleaseHold(dir, 0, "s3")
	if err != nil || cand != "s3" {
		t.Fatalf("release: %q %v", cand, err)
	}
	if markerExists(dir, RelinquishedMarker, 0) || markerExists(dir, CandidateMarker, 0) {
		t.Fatal("a marker of the hand-over is still there")
	}
	if !markerExists(dir, WaitMarker, 0) {
		t.Fatal("the wait marker was not written: the primary would start without its lease")
	}
	if err := Gate(dir, func(int) (int32, bool) { return 0, true })(1); err == nil {
		t.Fatal("the start gate lets the primary start before it holds the lease")
	}
	if _, err := ReleaseHold(dir, 0, "s3"); !errors.Is(err, ErrNotHeldBack) {
		t.Fatalf("a second release finds nothing to release: %v", err)
	}
}

// The declaration must name the server the group was handed to; a hold without a record takes the declaration as given; no marker is an error.
func TestReleaseHoldRefusesAnotherServerThanTheOneTheHoldWasMadeFor(t *testing.T) {
	dir := holdDir(t, "s3")
	if _, err := ReleaseHold(dir, 0, "s4"); err == nil {
		t.Fatal("a hold made for s3 was released by the declaration that s4 is lost")
	}
	if !markerExists(dir, RelinquishedMarker, 0) || markerExists(dir, WaitMarker, 0) {
		t.Fatal("a refused release changed the markers")
	}
	dir = holdDir(t, "")
	if cand, err := ReleaseHold(dir, 0, "s4"); err != nil || cand != "" {
		t.Fatalf("a hold without a record: %q %v", cand, err)
	}
	if _, err := ReleaseHold(filepath.Join(tempDir(t), "none"), 0, "s4"); !errors.Is(err, ErrNotHeldBack) {
		t.Fatalf("no marker: %v", err)
	}
}
