// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package statefile

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// L14-05. A program that has the state open without sharing delete (a backup, a
// scanner, a viewer) makes the rename fail with "Access is denied". The rename is
// tried again for a bounded time, and the error then says that another program
// may hold the file.

// scratchDir is a directory of our own: a scanner that looks at the new file can
// make the automatic removal of t.TempDir fail for a moment, which is not what is
// tested here, so the removal is tried again.
func scratchDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "kingres-rename")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for i := 0; i < 50; i++ {
			if os.RemoveAll(dir) == nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	})
	return dir
}

// holdWithoutDelete opens the file for reading the way a program does that does
// not allow others to delete or rename it.
func holdWithoutDelete(t *testing.T, path string) syscall.Handle {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ|syscall.FILE_SHARE_WRITE, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestRenameWaitsForAProgramThatHoldsTheStateAndGivesUp(t *testing.T) {
	old := renameWait
	renameWait = 300 * time.Millisecond
	defer func() { renameWait = old }()
	dir := scratchDir(t)
	p := filepath.Join(dir, "state")
	if err := Replace(p, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	h := holdWithoutDelete(t, p)
	defer syscall.CloseHandle(h)
	start := time.Now()
	err := Replace(p, []byte("new\n"))
	if err == nil {
		t.Fatal("the rename over a file that another program holds succeeded")
	}
	if took := time.Since(start); took < 250*time.Millisecond || took > 5*time.Second {
		t.Errorf("the rename was tried for %v, want about the wait of 300 ms", took)
	}
	for _, want := range []string{"another program may have the file open", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	if readState(t, p) != "old\n" || len(tmpFiles(t, dir)) != 0 {
		t.Errorf("the state or the directory changed: %q %v", readState(t, p), tmpFiles(t, dir))
	}
}

func TestRenameSucceedsWhenTheProgramLetsGoInTime(t *testing.T) {
	dir := scratchDir(t)
	p := filepath.Join(dir, "state")
	if err := Replace(p, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	h := holdWithoutDelete(t, p)
	go func() {
		time.Sleep(300 * time.Millisecond)
		syscall.CloseHandle(h)
	}()
	if err := Replace(p, []byte("new\n")); err != nil {
		t.Fatalf("a program that lets go after 300 ms stopped the replace: %v", err)
	}
	if readState(t, p) != "new\n" {
		t.Fatal("not replaced")
	}
}
