// SPDX-License-Identifier: AGPL-3.0-only

//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// L12-04. The README's rule for the standard output, in the program that is
// built: a standard output that is the null device opened for reading and
// writing (what the Go runtime puts in place of a closed descriptor) is refused
// with status 2 before anything is run; one opened for writing only
// (>/dev/null, >>/dev/null) is the user's request for no output and runs as
// usual; a file, a pipe and a terminal are normal.

// runWith starts the test binary as the program under sh, with the redirection
// given, and returns its status and standard error.
func runWith(t *testing.T, redirect string, args ...string) (int, string, string) {
	t.Helper()
	dir := t.TempDir()
	script := `exec "$0" "$@" ` + redirect
	cmd := exec.Command("sh", append([]string{"-c", script, os.Args[0]}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), asMain+"=1")
	var errb bytes.Buffer
	cmd.Stderr = &errb
	var outb bytes.Buffer
	cmd.Stdout = &outb
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return code, outb.String(), errb.String()
}

func TestClosedStandardOutputIsStatus2(t *testing.T) {
	for _, redirect := range []string{">&-", "1<>/dev/null", ">&- 2>&-", ">&- 2>/dev/null"} {
		for _, args := range [][]string{{"version"}, {"--help"}, {"trust", "show", "--trust", "nowhere.json"}} {
			code, out, errs := runWith(t, redirect, args...)
			if code != 2 {
				t.Errorf("%s %v: status %d, want 2\n%s%s", redirect, args, code, out, errs)
			}
			if strings.Contains(redirect, "2>") {
				continue // the message has nowhere to go; the status is what is tested
			}
			if !strings.Contains(errs, "the standard output is closed") || !strings.Contains(errs, "nothing was run") {
				t.Errorf("%s %v: the standard error does not say so: %q", redirect, args, errs)
			}
		}
	}
}

func TestStandardOutputThatTheUserChoseRuns(t *testing.T) {
	dir := t.TempDir()
	for _, redirect := range []string{">/dev/null", ">>/dev/null", ">" + filepath.Join(dir, "out.txt"), "| cat"} {
		code, out, errs := runWith(t, redirect, "version")
		if code != 0 {
			t.Errorf("%s: status %d\n%s%s", redirect, code, out, errs)
		}
	}
	// the null device for writing only gives the status and no text
	if code, out, _ := runWith(t, ">/dev/null", "version"); code != 0 || out != "" {
		t.Errorf("version to the null device: %d %q", code, out)
	}
	// and a verdict that is not 0 keeps its status there
	if code, _, _ := runWith(t, ">/dev/null", "proof", "--proof", "nowhere.json", "--digest", "nowhere.json"); code != 2 {
		t.Errorf("a missing file with the output on the null device: %d", code)
	}
}
