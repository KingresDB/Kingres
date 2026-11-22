// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, p string) {
	t.Helper()
	if err := os.WriteFile(p, []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The markers are per group: the fence of one group never holds back the instance
// of another, which is the point of the instance mapping.
func TestMarkersAreForOneGroup(t *testing.T) {
	dir := tempDir(t)
	write(t, FencedPath(dir, 2))
	groupOf := func(id int) (int32, bool) { return int32(id), true }
	if err := Gate(dir, groupOf)(2); err == nil {
		t.Fatal("the primary of the fenced group must be held back")
	}
	if err := Gate(dir, groupOf)(3); err != nil {
		t.Fatalf("the primary of another group must not be held back: %v", err)
	}
	if !Waiting(dir, 2) || Waiting(dir, 3) {
		t.Fatal("Waiting must follow the group")
	}
	if !AnyFenced(dir) {
		t.Fatal("AnyFenced must see the marker of any group")
	}
}

// A marker of the version that ran one primary per server holds for every group until
// the lease manager has seen the catalog, then it is renamed to the groups of the
// server: an upgrade never loses a fence, and never keeps one for ever.
func TestLegacyMarkersHoldAndAreMigrated(t *testing.T) {
	dir := tempDir(t)
	write(t, filepath.Join(dir, FencedMarker))
	write(t, filepath.Join(dir, WaitMarker))
	if !markerExists(dir, FencedMarker, 7) || !waitMarkerExists(dir, 7) {
		t.Fatal("a marker without a suffix must hold for every group")
	}
	migrateLegacyMarkers(dir, nil)
	if _, err := os.Stat(filepath.Join(dir, FencedMarker)); err != nil {
		t.Fatal("with no known group the legacy marker must stay")
	}
	migrateLegacyMarkers(dir, []int32{1, 4})
	for _, g := range []int32{1, 4} {
		for _, base := range []string{FencedMarker, WaitMarker} {
			if _, err := os.Stat(MarkerPath(dir, base, g)); err != nil {
				t.Errorf("group %d has no %s marker after the migration", g, base)
			}
		}
	}
	for _, base := range []string{FencedMarker, WaitMarker} {
		if _, err := os.Stat(filepath.Join(dir, base)); err == nil {
			t.Errorf("the legacy %s marker is still there", base)
		}
	}
	if markerExists(dir, FencedMarker, 7) {
		t.Error("after the migration a group that is not the server's has no marker")
	}
}
