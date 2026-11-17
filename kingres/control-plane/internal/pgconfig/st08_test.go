// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"testing"
	"time"
)

// ST-08: the wait between attempts for a server that keeps failing grows from one minute to ten.
func TestRetryWaitGrowsToTenMinutes(t *testing.T) {
	want := []time.Duration{time.Minute, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 10 * time.Minute, 10 * time.Minute}
	for i, w := range want {
		if got := retryWait(i); got != w {
			t.Errorf("retryWait(%d) = %s, want %s", i, got, w)
		}
	}
}
