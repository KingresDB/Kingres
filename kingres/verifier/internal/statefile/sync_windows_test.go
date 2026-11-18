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

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// L14-05. A program that has the state open without sharing delete (a backup, a
// scanner, a viewer) makes the rename fail with "Access is denied". The rename is
// tried again for a bounded time, and the error then says that another program
// may hold the file.

// scratchDir is a directory of our own, removed with the retry of testdir.
func scratchDir(t *testing.T) string {
	t.Helper()
	return testdir.New(t)
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

// errnoOf digs the Windows error out of what rename returns.
func errnoOf(err error) syscall.Errno {
	var e syscall.Errno
	for err != nil {
		if en, ok := err.(syscall.Errno); ok {
			return en
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			break
		}
		err = u.Unwrap()
	}
	return e
}

func setReadOnly(t *testing.T, p string, on bool) {
	t.Helper()
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		t.Fatal(err)
	}
	attr, err := syscall.GetFileAttributes(name)
	if err != nil {
		t.Fatal(err)
	}
	if on {
		attr |= syscall.FILE_ATTRIBUTE_READONLY
	} else {
		attr &^= syscall.FILE_ATTRIBUTE_READONLY
	}
	if err := syscall.SetFileAttributes(name, attr); err != nil {
		t.Fatal(err)
	}
}

// L16-06. What MoveFileEx answers for each kind of refusal, measured here (its
// documentation lists no codes): the three that are properties of the target and
// the one a handle of another program gives. rename classifies by what it can see
// of the target before it waits.
func TestRenameErrorCodes(t *testing.T) {
	old := renameWait
	renameWait = 100 * time.Millisecond
	defer func() { renameWait = old }()
	dir := scratchDir(t)
	mk := func(name string) (src, dst string) {
		src, dst = filepath.Join(dir, name+".src"), filepath.Join(dir, name+".dst")
		if err := os.WriteFile(src, []byte("new"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	// 1. read-only attribute on the target
	src, dst := mk("readonly")
	setReadOnly(t, dst, true)
	err := rename(src, dst)
	t.Logf("target with the read-only attribute: %v (code %d)", err, errnoOf(err))
	if err == nil || errnoOf(err) != errAccessDenied || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("read-only target: %v", err)
	}
	setReadOnly(t, dst, false)
	// 2. a directory at the name
	src, _ = mk("dir")
	dstDir := filepath.Join(dir, "dir.dst.d")
	if err := os.Mkdir(dstDir, 0o700); err != nil {
		t.Fatal(err)
	}
	err = rename(src, dstDir)
	t.Logf("target is a directory: %v (code %d)", err, errnoOf(err))
	if err == nil || errnoOf(err) != errAccessDenied || !strings.Contains(err.Error(), "is a directory") {
		t.Errorf("directory target: %v", err)
	}
	// 3. the target is open in another program without sharing delete: access denied
	// too, which the file alone does not tell apart from a refusal for ever, so it is
	// waited for
	src, dst = mk("held")
	h := holdWithoutDelete(t, dst)
	err = rename(src, dst)
	_ = syscall.CloseHandle(h)
	t.Logf("target held open without sharing delete: %v (code %d)", err, errnoOf(err))
	if err == nil || (errnoOf(err) != errAccessDenied && errnoOf(err) != errSharingViolation && errnoOf(err) != errLockViolation) || !strings.Contains(err.Error(), "another program may have the file open") {
		t.Errorf("held target: %v", err)
	}
	// 4. the source is open exclusively: a sharing violation or access denied, waited for
	src, dst = mk("srcheld")
	name, _ := syscall.UTF16PtrFromString(src)
	hs, cerr := syscall.CreateFile(name, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if cerr != nil {
		t.Fatal(cerr)
	}
	err = rename(src, dst)
	_ = syscall.CloseHandle(hs)
	t.Logf("source held open exclusively: %v (code %d)", err, errnoOf(err))
	if err == nil {
		t.Error("the rename of a source that is open exclusively succeeded")
	}
}

// A state or an evidence target with the read-only attribute is named at once and
// not after the wait for a handle that is not there: the retry is for the programs
// that hold a file open, not for an attribute.
func TestRenameOfAReadOnlyStateIsNamedAtOnce(t *testing.T) {
	old := renameWait
	renameWait = 5 * time.Second
	defer func() { renameWait = old }()
	dir := scratchDir(t)
	p := filepath.Join(dir, "state")
	if err := Replace(p, []byte("old\n")); err != nil {
		t.Fatal(err)
	}
	setReadOnly(t, p, true)
	defer setReadOnly(t, p, false)
	start := time.Now()
	err := Replace(p, []byte("new\n"))
	took := time.Since(start)
	if err == nil {
		t.Fatal("a read-only state was replaced")
	}
	if took > 2*time.Second {
		t.Errorf("the refusal took %v: the attribute was waited for", took)
	}
	for _, want := range []string{"read-only", "nothing was changed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not say %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "another program") {
		t.Errorf("the error blames another program for an attribute: %v", err)
	}
	if readState(t, p) != "old\n" || len(tmpFiles(t, dir)) != 0 {
		t.Errorf("the state or the directory changed: %q %v", readState(t, p), tmpFiles(t, dir))
	}
	// an evidence file with the attribute that is to be extended: the same
	root := make([]byte, 32)
	if _, err := SaveConflict(p, 4, root, []byte("e1\n")); err != nil {
		t.Fatal(err)
	}
	ev := ConflictPath(p, 4, root)
	setReadOnly(t, ev, true)
	defer setReadOnly(t, ev, false)
	merge := func(have []byte) ([]byte, bool) { return append(append([]byte(nil), have...), "more\n"...), true }
	start = time.Now()
	_, err = SaveConflictMerging(p, 4, root, []byte("e1\n"), merge)
	if err == nil || !strings.Contains(err.Error(), "read-only") || time.Since(start) > 2*time.Second {
		t.Errorf("the extension of a read-only evidence file: %v after %v", err, time.Since(start))
	}
}
