// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package testutil holds helpers shared by tests.
package testutil

import (
	"os"
	"testing"
	"time"
)

// TempDir is t.TempDir for directories in which files are written with the
// write-fsync-rename pattern.
//
// On Windows, a file that was just written, synced and renamed (or removed) can
// stay in "delete pending" state for a few milliseconds because a scanner
// (antivirus, indexer) has it open; t.TempDir's cleanup then fails once in a few
// hundred runs with "directory is not empty". A program that only uses the
// standard library to create, sync, close and rename a file shows the same
// failure, so the handle is not held by the code under test (which closes
// every file it opens: see the Close methods of audit.FileSink and
// pgstore.Store). TempDir therefore removes the directory with a bounded
// retry (at most 5 s), and fails the test if the directory still cannot be
// removed, so a real leaked handle is still reported.
func TempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kingres-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		deadline := time.Now().Add(5 * time.Second)
		for {
			err := os.RemoveAll(dir)
			if err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Errorf("removing %s: %v (a file handle may be leaked)", dir, err)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	return dir
}
