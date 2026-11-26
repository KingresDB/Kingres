// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package trust

import (
	"strings"
	"testing"
	"time"
)

// V8-01: the narrowing of a file by a verified event: Clone leaves the file alone, Revoke and LimitTo keep the smaller bound and say why.
func TestNarrowingByEvents(t *testing.T) {
	pub := make([]byte, 32)
	pub[0] = 9
	f, err := New("ledger.example.com/kingres", "k1", pub, Fingerprint(pub))
	if err != nil {
		t.Skip("the test key is refused by the three tests of a key: " + err.Error())
	}
	fp := f.Keys[0].Fingerprint
	c := f.Clone()
	if !c.LimitTo(fp, 10, "the event 000001") || *c.Keys[0].MaxTreeSize != 10 || f.Keys[0].MaxTreeSize != nil {
		t.Fatal("LimitTo changed the original or did not limit the copy")
	}
	c.LimitTo(fp, 20, "another")
	if *c.Keys[0].MaxTreeSize != 10 {
		t.Fatal("a larger bound loosened a smaller one")
	}
	c.LimitTo(fp, 5, "the event 000007")
	if *c.Keys[0].MaxTreeSize != 5 || !strings.Contains(c.Keys[0].Why(time.Now()), "the event 000007") {
		t.Fatal(c.Keys[0].Why(time.Now()))
	}
	if !c.Revoke(fp, "the event 000009") || !c.Keys[0].Revoked || c.Keys[0].MaxTreeSize != nil {
		t.Fatal("Revoke")
	}
	if c.LimitTo("00", 1, "x") || c.Revoke("00", "x") {
		t.Fatal("a key that is not in the file was changed")
	}
	if got := c.Keys[0].Why(time.Now()); !strings.Contains(got, "revoked by the event 000009") {
		t.Fatal(got)
	}
	if f.Keys[0].Revoked {
		t.Fatal("the original was revoked")
	}
}
