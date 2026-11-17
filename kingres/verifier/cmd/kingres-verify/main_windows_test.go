// SPDX-License-Identifier: AGPL-3.0-only

//go:build windows

package main

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"
)

// L12-04 on Windows. A closed standard output is a null or invalid handle there,
// which the Go runtime does not replace by the null device, so the write fails and
// the run ends with status 2 like any other failed write; the rule needs no code
// of its own. This starts the program with the handle of the standard output
// left as 0 (what a parent that closed it passes) and reads what it says.
func TestClosedStandardOutputOnWindowsIsStatus2(t *testing.T) {
	nul, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nul.Close()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	attr := &syscall.ProcAttr{
		Env:   append(os.Environ(), asMain+"=1"),
		Files: []uintptr{nul.Fd(), 0, w.Fd()},
	}
	pid, handle, err := syscall.StartProcess(os.Args[0], []string{os.Args[0], "version"}, attr)
	if err != nil {
		t.Skipf("cannot start a process with a closed standard output here: %v", err)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))
	w.Close()
	p, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	state, err := p.Wait()
	if err != nil {
		t.Fatal(err)
	}
	msg, _ := io.ReadAll(r)
	if state.ExitCode() != 2 || !strings.Contains(string(msg), "could not be written") {
		t.Fatalf("status %d, standard error %q: a verdict that nobody could read must be status 2", state.ExitCode(), msg)
	}
	// and a standard output that is the null device, which the user asked for, runs
	r2, w2, _ := os.Pipe()
	attr.Files = []uintptr{nul.Fd(), nul.Fd(), w2.Fd()}
	pid, handle, err = syscall.StartProcess(os.Args[0], []string{os.Args[0], "version"}, attr)
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.CloseHandle(syscall.Handle(handle))
	w2.Close()
	p, _ = os.FindProcess(pid)
	state, _ = p.Wait()
	msg, _ = io.ReadAll(r2)
	if state.ExitCode() != 0 {
		t.Fatalf("version to the null device: status %d %q", state.ExitCode(), msg)
	}
}

// L14-01 on Windows: nothing is refused there, so the variable changes nothing;
// the null device (what Python's subprocess.DEVNULL opens as "nul") runs with and
// without it.
func TestNullStandardOutputOnWindowsRunsWithOrWithoutTheVariable(t *testing.T) {
	nul, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer nul.Close()
	for _, extra := range [][]string{nil, {"KINGRES_VERIFY_NO_OUTPUT=1"}} {
		r, w, _ := os.Pipe()
		attr := &syscall.ProcAttr{
			Env:   append(append(os.Environ(), asMain+"=1"), extra...),
			Files: []uintptr{nul.Fd(), nul.Fd(), w.Fd()},
		}
		pid, handle, err := syscall.StartProcess(os.Args[0], []string{os.Args[0], "version"}, attr)
		if err != nil {
			t.Fatal(err)
		}
		w.Close()
		p, _ := os.FindProcess(pid)
		state, _ := p.Wait()
		_ = syscall.CloseHandle(syscall.Handle(handle))
		msg, _ := io.ReadAll(r)
		if state.ExitCode() != 0 {
			t.Errorf("the null device with %v: status %d %q", extra, state.ExitCode(), msg)
		}
	}
}
