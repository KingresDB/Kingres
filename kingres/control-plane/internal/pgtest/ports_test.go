// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgtest

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
)

func TestSplitPortRange(t *testing.T) {
	// 100 ports, 3 nodes: three parts of 33, in order, not overlapping, inside the range.
	prevLast := 0
	for i := 0; i < 3; i++ {
		first, last, ok := SplitPortRange(28000, 28099, i, 3)
		if !ok || last-first+1 != 33 || first < 28000 || last > 28099 {
			t.Fatalf("part %d: %d-%d %v", i, first, last, ok)
		}
		if i > 0 && first != prevLast+1 {
			t.Fatalf("part %d starts at %d, the one before ended at %d", i, first, prevLast)
		}
		prevLast = last
	}
	for _, bad := range [][4]int{{28000, 28099, 3, 3}, {28000, 28099, -1, 3}, {28000, 28099, 0, 0}, {28000, 28002, 0, 5}, {0, 10, 0, 1}, {28100, 28000, 0, 1}} {
		if _, _, ok := SplitPortRange(bad[0], bad[1], bad[2], bad[3]); ok {
			t.Errorf("%v is not a valid split", bad)
		}
	}
}

func TestStandbyPortRangeFromTheEnvironment(t *testing.T) {
	t.Setenv("KINGRES_TEST_STANDBY_PORT_BASE", "")
	t.Setenv("KINGRES_TEST_STANDBY_PORT_END", "")
	if _, _, ok := StandbyPortRange(0, 3); ok {
		t.Fatal("no range without the variables: kingresd's default applies")
	}
	t.Setenv("KINGRES_TEST_STANDBY_PORT_BASE", "28200")
	t.Setenv("KINGRES_TEST_STANDBY_PORT_END", "28299")
	a, b, ok := StandbyPortRange(0, 3)
	c, d, ok2 := StandbyPortRange(2, 3)
	if !ok || !ok2 || a != 28200 || b != 28232 || c != 28266 || d != 28298 {
		t.Fatalf("%d-%d %d-%d", a, b, c, d)
	}
}

// scriptVar reads NAME=number from scripts/run-wsl-tests.sh.
func scriptVar(t *testing.T, src, name string) int {
	t.Helper()
	m := regexp.MustCompile(`(?m)^` + name + `=(\d+)\s*$`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("run-wsl-tests.sh has no %s=<number>", name)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// The slice math of run-wsl-tests.sh: the ports of run N are PORT_LO + N*SLICE_SIZE ..+SLICE_SIZE-1 and its standby
// ports STANDBY_LO + N*STANDBY_SIZE ..+STANDBY_SIZE-1. No two runs may share a port of either kind, a standby port may
// not be a slice port of any run, and none may reach the range the kernel hands out to outgoing connections.
func TestRunPortRangesNeverOverlap(t *testing.T) {
	b, err := readRunScript()
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	lo, size, count := scriptVar(t, src, "PORT_LO"), scriptVar(t, src, "SLICE_SIZE"), scriptVar(t, src, "SLICE_COUNT")
	slo, ssize := scriptVar(t, src, "STANDBY_LO"), scriptVar(t, src, "STANDBY_SIZE")
	const ephemeral = 32768
	owner := map[int]string{}
	claim := func(port int, what string) {
		if prev, dup := owner[port]; dup {
			t.Fatalf("port %d is claimed by %s and by %s", port, prev, what)
		}
		owner[port] = what
	}
	for n := 0; n < count; n++ {
		for p := lo + n*size; p < lo+(n+1)*size; p++ {
			claim(p, "the ports of run "+strconv.Itoa(n))
		}
		for p := slo + n*ssize; p < slo+(n+1)*ssize; p++ {
			claim(p, "the standby ports of run "+strconv.Itoa(n))
		}
	}
	if top := slo + count*ssize; top > ephemeral {
		t.Fatalf("the standby ports reach %d, into the range of outgoing connections (%d and up)", top-1, ephemeral)
	}
	if top := lo + count*size; top > ephemeral {
		t.Fatalf("the test ports reach %d, into the range of outgoing connections", top-1)
	}
	// Three nodes of a cluster need a few ports each: the standby range of a run has to give every node of the largest cluster some.
	if ssize/5 < 4 {
		t.Fatalf("a standby range of %d ports leaves %d for each node of a five-node cluster", ssize, ssize/5)
	}
	// The script exports exactly that arithmetic.
	for _, want := range []string{
		`export KINGRES_TEST_STANDBY_PORT_BASE=$((STANDBY_LO + SLICE_N * STANDBY_SIZE))`,
		`export KINGRES_TEST_STANDBY_PORT_END=$((STANDBY_LO + SLICE_N * STANDBY_SIZE + STANDBY_SIZE - 1))`,
	} {
		if !regexp.MustCompile(regexp.QuoteMeta(want)).MatchString(src) {
			t.Errorf("run-wsl-tests.sh lacks %q", want)
		}
	}
}

// readRunScript finds scripts/run-wsl-tests.sh: beside this source (the path the compiler recorded), then relative to the
// working directory, then in KINGRES_TEST_BIN/src, where run-integration.ps1 stages a copy for a prebuilt test binary.
func readRunScript() ([]byte, error) {
	var paths []string
	if _, self, _, ok := runtime.Caller(0); ok {
		paths = append(paths, filepath.Join(filepath.Dir(self), "..", "..", "scripts", "run-wsl-tests.sh"))
	}
	paths = append(paths, filepath.Join("..", "..", "scripts", "run-wsl-tests.sh"))
	if bin := os.Getenv("KINGRES_TEST_BIN"); bin != "" {
		paths = append(paths, filepath.Join(bin, "src", "run-wsl-tests.sh"))
	}
	var err error
	for _, p := range paths {
		var b []byte
		if b, err = os.ReadFile(p); err == nil {
			return b, nil
		}
	}
	return nil, err
}
