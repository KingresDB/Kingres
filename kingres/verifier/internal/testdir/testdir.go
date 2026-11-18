// SPDX-License-Identifier: AGPL-3.0-only

// Package testdir gives the tests of the verifier their temporary directories.
//
// testing.T.TempDir removes a directory at the end of a test and retries for two
// seconds when Windows says "The directory is not empty". On a machine with a
// scanner or an indexer that opens new files, an entry of a test directory can stay
// "delete pending" for longer than that, with nothing of the test open (L16: a
// program that uses no code of the verifier reproduced it, one directory in
// several hundred took more than 20 ms to remove and one took 2.2 s). New removes
// the directory with a retry of at least 10 seconds on Windows and with one plain
// removal elsewhere, and a directory that cannot be removed is still an error of
// the test.
//
// The cleanup of a test is not a leak detector (L18-05). On Windows a test that
// opens a file in its directory and drops the reference can still pass its cleanup,
// because the runtime's finalizer may close the handle before the removal is tried
// (when other goroutines allocate), and on Linux a removal does not fail for an open
// file at all. A descriptor or handle that a run leaves open is for the handle tests
// of the cli package to find: they count them with the collector off.
package testdir

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// Wait is how long the removal of a directory is tried again on Windows. A test
// of this package shortens it.
var Wait = 10 * time.Second

// New creates an empty directory for the test and removes it, with everything in
// it, when the test and its subtests are over. It is used in place of t.TempDir.
func New(t testing.TB) string {
	t.Helper()
	// The directory is one level below one of ours, as t.TempDir makes it: a test that
	// names the directory itself as a state needs a parent that only the user can write
	// (the check of the state's folder), which the shared temporary directory is not.
	base, err := os.MkdirTemp("", "kingres-verify-test-")
	if err != nil {
		t.Fatalf("cannot make a temporary directory: %v", err)
	}
	t.Cleanup(func() {
		if err := Remove(base, Wait); err != nil {
			t.Errorf("removing the temporary directory %s: %v", base, err)
		}
	})
	dir := filepath.Join(base, "001")
	if err := os.Mkdir(dir, 0o777); err != nil {
		t.Fatalf("cannot make a temporary directory: %v", err)
	}
	return dir
}

// Remove deletes dir and everything in it. On Windows it tries again every 50 ms
// until wait has passed when the removal fails (a file that another program has
// open for a moment keeps its parent from being removed); elsewhere there is one
// attempt. It returns the last error.
func Remove(dir string, wait time.Duration) error {
	err := os.RemoveAll(dir)
	if err == nil || runtime.GOOS != "windows" {
		return err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
	}
	return err
}
