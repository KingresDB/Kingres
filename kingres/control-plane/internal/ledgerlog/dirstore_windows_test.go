// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

//go:build windows

package ledgerlog

import (
	"errors"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// K1-04, with a real handle: a program that holds the checkpoint open (opened
// as os.Open does, without delete sharing) blocks the replacement on Windows.
func TestPutCheckpointBlockedByAnOpenHandle(t *testing.T) {
	d := newDirStore(t)
	if err := d.PutCheckpoint(bg, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(d.path("checkpoint"))
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
	}()
	// Held for the whole retry time: the typed error, the old note whole, no
	// temporary file.
	d.CheckpointRetry = 300 * time.Millisecond
	start := time.Now()
	err = d.PutCheckpoint(bg, []byte("new\n"))
	took := time.Since(start)
	if !errors.Is(err, ErrCheckpointBusy) {
		t.Fatalf("a checkpoint held open: %v", err)
	}
	if took < 250*time.Millisecond {
		t.Errorf("gave up after %v, before the retry time of 300ms", took)
	}
	if got, _ := io.ReadAll(f); string(got) != "old\n" {
		t.Errorf("the old checkpoint reads %q", got)
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
	// Released during the retry time: the put succeeds.
	d.CheckpointRetry = 10 * time.Second
	go func() {
		time.Sleep(400 * time.Millisecond)
		f.Close()
	}()
	if err := d.PutCheckpoint(bg, []byte("new\n")); err != nil {
		t.Fatalf("a checkpoint released during the retry: %v", err)
	}
	closed = true
	if got, err := d.GetCheckpoint(bg); err != nil || string(got) != "new\n" {
		t.Errorf("the checkpoint is %q, %v", got, err)
	}
	if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
		t.Errorf("temporary files are left: %v", tmp)
	}
}

// The errors that mean "this file system has no hard links" and the ones that
// are about this one call (see platformNoHardLinks for each).
var (
	noLinkErrnos    = []error{syscall.Errno(1), syscall.Errno(17), syscall.Errno(50), syscall.Errno(120)}
	otherLinkErrnos = []error{syscall.Errno(112), syscall.Errno(39), syscall.Errno(1142), syscall.Errno(5), syscall.Errno(3), syscall.Errno(29)}
)

// K2-08, with real files: a directory in the place of the checkpoint, and a
// checkpoint with the read-only attribute, are named as what they are at once,
// not as a file that another program holds open, and not after the retry time.
func TestPutCheckpointTargetThatWaitingCannotCure(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(t *testing.T, path string)
		want  string
	}{
		"a directory": {func(t *testing.T, path string) {
			if err := os.Mkdir(path, 0o755); err != nil {
				t.Fatal(err)
			}
		}, "directory"},
		"a read-only file": {func(t *testing.T, path string) {
			if err := os.WriteFile(path, []byte("old\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0o444); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
		}, "read-only"},
	} {
		d := newDirStore(t)
		d.CheckpointRetry = 10 * time.Second // a wait would be seen
		if err := os.MkdirAll(d.Root(), 0o755); err != nil {
			t.Fatal(err)
		}
		tc.setup(t, d.path("checkpoint"))
		start := time.Now()
		err := d.PutCheckpoint(bg, []byte("new\n"))
		took := time.Since(start)
		if err == nil || errors.Is(err, ErrCheckpointBusy) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s in the place of the checkpoint: %v", name, err)
		}
		if took > 2*time.Second {
			t.Errorf("%s: it waited %v", name, took)
		}
		if tmp := tempFilesBelow(t, d.Root()); len(tmp) != 0 {
			t.Errorf("%s: temporary files are left: %v", name, tmp)
		}
		if tc.want == "read-only" {
			if got, _ := os.ReadFile(d.path("checkpoint")); string(got) != "old\n" {
				t.Errorf("the old checkpoint reads %q", got)
			}
		}
	}
}
