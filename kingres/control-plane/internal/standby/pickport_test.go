// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package standby

import "testing"

func TestPickPortStaysInItsRange(t *testing.T) {
	always := func(int) bool { return true }
	if p, err := pickPort(28000, 28024, nil, always); err != nil || p != 28000 {
		t.Fatalf("%d %v", p, err)
	}
	// Ports in use or taken are skipped; the end is inclusive and never passed.
	used := map[int]bool{28000: true, 28001: true}
	taken := func(p int) bool { return p != 28002 }
	if p, err := pickPort(28000, 28024, used, taken); err != nil || p != 28003 {
		t.Fatalf("%d %v", p, err)
	}
	if p, err := pickPort(28000, 28024, nil, func(p int) bool { return p == 28024 }); err != nil || p != 28024 {
		t.Fatalf("the last port of the range is usable: %d %v", p, err)
	}
	if p, err := pickPort(28000, 28024, nil, func(p int) bool { return p > 28024 }); err == nil {
		t.Fatalf("a port past the end of the range was chosen: %d", p)
	}
	// Without an end: 500 ports from the base.
	if p, err := pickPort(28000, 0, nil, func(p int) bool { return p == 28499 }); err != nil || p != 28499 {
		t.Fatalf("%d %v", p, err)
	}
	if _, err := pickPort(28000, 0, nil, func(p int) bool { return p == 28500 }); err == nil {
		t.Fatal("the default range is 500 ports")
	}
}
