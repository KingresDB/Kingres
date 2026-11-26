// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerpub

import "testing"

func TestTheEvidencePathIsCanonicalOnly(t *testing.T) {
	good := map[string][2]int64{"anchors/12/3.json": {12, 3}, "anchors/1/1.json": {1, 1}}
	for p, want := range good {
		size, target, ok := parseEvidencePath(p)
		if !ok || size != want[0] || target != want[1] {
			t.Errorf("%s: %d %d %v", p, size, target, ok)
		}
	}
	for _, p := range []string{"", "anchors", "anchors/12", "anchors/12/3", "anchors/012/3.json", "anchors/12/03.json", "anchors/+12/3.json", "anchors/0/3.json", "anchors/12/0.json", "anchors/-1/3.json", "anchors/12/3.json/x", "other/12/3.json", "anchors/12/a.json", "anchors/99999999999999999999/3.json"} {
		if _, _, ok := parseEvidencePath(p); ok {
			t.Errorf("%q is accepted", p)
		}
	}
}
