// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"testing"
	"time"
)

// F3-03: the leader's own silence is not the death of the server.
func TestUnobservedAgeSubtractsOnlyWhatAMajorityDidNotHear(t *testing.T) {
	const lease, margin = 10 * time.Second, 3 * time.Second
	plain := 111 * time.Second
	// The leader alone has not heard the server for 110 s; two other members hear it: the age stays the plain one.
	if got := UnobservedAge(plain, []time.Duration{110 * time.Second, time.Second}, 3, lease, margin); got != plain {
		t.Fatalf("a silence that only the leader has was subtracted: %v", got)
	}
	// Nobody else answered: the leader's silence stands alone.
	if got := UnobservedAge(plain, []time.Duration{110 * time.Second}, 3, lease, margin); got != plain {
		t.Fatalf("a silence without a majority of answers was subtracted: %v", got)
	}
	// A majority (the leader and one other of three) has not heard it for 110 s: the primary lost its lease with them and
	// stopped within lease+margin of the last renewal, so 110-13 s are subtracted.
	if got := UnobservedAge(plain, []time.Duration{110 * time.Second, 105 * time.Second}, 3, lease, margin); got != plain-92*time.Second {
		t.Fatalf("the silence of a majority was not subtracted as designed: %v", got)
	}
	// A silence shorter than the lease changes nothing.
	if got := UnobservedAge(plain, []time.Duration{9 * time.Second, 9 * time.Second}, 3, lease, margin); got != plain {
		t.Fatalf("a silence below the lease was subtracted: %v", got)
	}
	// The age never goes below zero.
	if got := UnobservedAge(20*time.Second, []time.Duration{300 * time.Second, 300 * time.Second}, 3, lease, margin); got != 20*time.Second {
		t.Fatalf("a cut larger than the age: %v", got)
	}
}
