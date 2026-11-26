// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
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
	return runWithEnv(t, nil, redirect, args...)
}

// runWithEnv is runWith with more variables in the environment of the program.
func runWithEnv(t *testing.T, env []string, redirect string, args ...string) (int, string, string) {
	t.Helper()
	dir := testdir.New(t)
	script := `exec "$0" "$@" ` + redirect
	cmd := exec.Command("sh", append([]string{"-c", script, os.Args[0]}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), asMain+"=1"), env...)
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
	dir := testdir.New(t)
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

// L14-01. A caller that gives the null device for reading and writing on purpose
// (Python's subprocess.DEVNULL, daemon(3), the double-fork recipes) cannot be told
// from the runtime's stand-in for a closed descriptor, so it says so with
// KINGRES_VERIFY_NO_OUTPUT=1; without it the refusal stays, and the refusal names
// the variable.
func TestNullStandardOutputGivenOnPurposeRunsWhenTheCallerSaysSo(t *testing.T) {
	const env = "KINGRES_VERIFY_NO_OUTPUT"
	for _, redirect := range []string{"1<>/dev/null", "1<>/dev/null 2>/dev/null"} {
		code, _, errs := runWithEnv(t, []string{env + "=1"}, redirect, "version")
		if code != 0 {
			t.Errorf("%s with %s=1: status %d, want 0\n%s", redirect, env, code, errs)
		}
		// the status of a run that is not 0 is the answer, and it is kept
		if code, _, _ := runWithEnv(t, []string{env + "=1"}, redirect, "proof", "--proof", "nowhere.json", "--digest", "nowhere.json"); code != 2 {
			t.Errorf("%s with %s=1, a missing file: status %d, want 2", redirect, env, code)
		}
	}
	// without it (or with anything but 1) the refusal stays, and it names the variable
	for _, v := range []string{"", env + "=", env + "=0", env + "=yes", env + "=11", env + "=1 "} {
		var vars []string
		if v != "" {
			vars = []string{v}
		}
		code, _, errs := runWithEnv(t, vars, "1<>/dev/null", "version")
		if code != 2 || !strings.Contains(errs, env) {
			t.Errorf("%q: status %d, want 2 with a message that names %s: %q", v, code, env, errs)
		}
	}
	// the variable changes nothing for any other standard output
	if code, out, _ := runWithEnv(t, []string{env + "=1"}, "| cat", "version"); code != 0 || !strings.Contains(out, "kingres-verify") {
		t.Errorf("a pipe with the variable: %d %q", code, out)
	}
	if code, _, _ := runWithEnv(t, []string{env + "=1"}, ">/dev/null", "version"); code != 0 {
		t.Errorf("the null device for writing with the variable: %d", code)
	}
}

// The way the README tells a Python caller to start the program: the real
// subprocess.DEVNULL, with and without the variable.
func TestPythonDevnullCallerWithAndWithoutTheVariable(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("no python3 here")
	}
	script := `import os, subprocess, sys
env = dict(os.environ, KINGRES_VERIFY_TEST_AS_MAIN="1")
if sys.argv[2] == "yes":
    env["KINGRES_VERIFY_NO_OUTPUT"] = "1"
r = subprocess.run([sys.argv[1], "version"], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=env)
sys.stdout.write("%d %s" % (r.returncode, r.stderr.decode()[:120]))
`
	run := func(with string) string {
		out, err := exec.Command(py, "-I", "-c", script, os.Args[0], with).CombinedOutput()
		if err != nil {
			t.Fatalf("python: %v\n%s", err, out)
		}
		return string(out)
	}
	if got := run("no"); !strings.HasPrefix(got, "2 ") || !strings.Contains(got, "the standard output is closed") {
		t.Errorf("subprocess.DEVNULL without the variable: %q, want status 2 and the refusal", got)
	}
	if got := run("yes"); !strings.HasPrefix(got, "0") {
		t.Errorf("subprocess.DEVNULL with the variable: %q, want status 0", got)
	}
}
