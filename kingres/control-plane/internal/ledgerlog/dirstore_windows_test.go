// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package ledgerlog

import (
	"errors"
	"io"
	"os"
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
