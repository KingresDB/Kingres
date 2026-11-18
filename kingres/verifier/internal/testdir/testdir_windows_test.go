// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package testdir

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A file that another program has open keeps its directory from being removed
// until it is closed ("The directory is not empty"). Remove waits for it.
func TestRemoveWaitsForAnEntryThatIsHeldForAMoment(t *testing.T) {
	dir, err := os.MkdirTemp("", "kingres-verify-testdir-")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	if os.RemoveAll(dir) == nil {
		t.Skip("this system lets a directory with an open file be removed")
	}
	go func() {
		time.Sleep(500 * time.Millisecond)
		f.Close()
	}()
	start := time.Now()
	if err := Remove(dir, 10*time.Second); err != nil {
		t.Fatalf("not removed after the file was closed: %v", err)
	}
	if time.Since(start) < 400*time.Millisecond {
		t.Errorf("removed in %v, before the file was closed", time.Since(start))
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Error("the directory is still there")
	}
}

func TestRemoveGivesUpAfterTheWaitAndSaysWhy(t *testing.T) {
	dir, err := os.MkdirTemp("", "kingres-verify-testdir-")
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, "held"))
	if err != nil {
		t.Fatal(err)
	}
	if os.RemoveAll(dir) == nil {
		t.Skip("this system lets a directory with an open file be removed")
	}
	start := time.Now()
	err = Remove(dir, 400*time.Millisecond)
	f.Close()
	_ = os.RemoveAll(dir)
	if err == nil || time.Since(start) < 300*time.Millisecond {
		t.Errorf("Remove returned %v after %v for a file that stays open", err, time.Since(start))
	}
}
