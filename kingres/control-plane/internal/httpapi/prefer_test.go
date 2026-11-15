// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"
	"testing"
)

func TestPrefersAsync(t *testing.T) {
	for v, want := range map[string]bool{"": false, "respond-async": true, "return=minimal, respond-async": true, "Respond-Async; wait=5": true, "return=minimal": false} {
		r, _ := http.NewRequest("PATCH", "/", nil)
		if v != "" {
			r.Header.Set("Prefer", v)
		}
		if got := prefersAsync(r); got != want {
			t.Errorf("%q: %v", v, got)
		}
	}
}
