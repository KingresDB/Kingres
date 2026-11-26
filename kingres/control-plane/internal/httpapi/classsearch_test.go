// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
)

// Class search of 2026-11-14 (CP-02 leftovers): the metrics scrape decided a
// role's privilege from the cached session. A role that lost its cluster
// roles, or was dropped, must lose the scrape right too.
func TestMetricsScrapeFollowsTheCurrentStateOfTheRole(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(e *env)
	}{
		{"loses every cluster role", func(e *env) { e.setUser("alice", &fakeUser{"s3cret", nil}) }},
		{"dropped", func(e *env) { e.setUser("alice", nil) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t)
			e.initialize()
			tok := loginToken(t, e, "alice", "s3cret")
			if w := e.do(req{method: "GET", path: "/metrics", bearer: tok}); w.Code == http.StatusUnauthorized {
				t.Fatalf("before the change the scrape is refused: %d", w.Code)
			}
			tc.change(e)
			e.clock.Advance(auth.DefaultRevalidateTTL + time.Second)
			if w := e.do(req{method: "GET", path: "/metrics", bearer: tok}); w.Code != http.StatusUnauthorized {
				t.Fatalf("after the change the scrape still works: %d", w.Code)
			}
		})
	}
}
