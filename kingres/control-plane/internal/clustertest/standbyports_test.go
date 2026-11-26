// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest

import (
	"slices"
	"testing"
)

func argValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

func TestNodeArgsCarryTheStandbyPortRangeOfTheNode(t *testing.T) {
	n := &Node{Host: "127.0.0.2", APIPort: 20200, IntPort: 20201, PGPort: 20202, Dir: t.TempDir(), DataDir: t.TempDir(), StandbyBase: 28200, StandbyEnd: 28232}
	args := n.args()
	if argValue(args, "-standby-port-base") != "28200" || argValue(args, "-standby-port-end") != "28232" {
		t.Fatalf("%v", args)
	}
	// A test can still choose its own: its flags come last.
	n.Extra = []string{"-standby-port-base", "40000"}
	args = n.args()
	if args[len(args)-1] != "40000" || args[len(args)-2] != "-standby-port-base" {
		t.Fatalf("the flags of the test must come last: %v", args)
	}
}

func TestNodeArgsWithoutARangeLeaveTheDefault(t *testing.T) {
	n := &Node{Host: "127.0.0.2", APIPort: 20200, IntPort: 20201, PGPort: 20202, Dir: t.TempDir(), DataDir: t.TempDir()}
	if args := n.args(); slices.Contains(args, "-standby-port-base") || slices.Contains(args, "-standby-port-end") {
		t.Fatalf("%v", args)
	}
}
