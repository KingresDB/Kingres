// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package peerclient

import "testing"

func TestQueryOfAnInternalGetIsRestricted(t *testing.T) {
	for q, ok := range map[string]bool{"group=1": true, "group=12&x=a.b": true, "group=1;rm": false, "a b=1": false, "group": false, "group=../x": false} {
		if queryRE.MatchString(q) != ok {
			t.Errorf("%q: %v", q, !ok)
		}
	}
}
