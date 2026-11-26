// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package clustertest_test

import (
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clustertest"
)

// foLeaseLines returns the lines of the log of a server that speak of leases, adoptions and the watchdog, for the message
// of a failing failover test.
func foLeaseLines(n *clustertest.Node) string {
	var out []string
	for _, ln := range strings.Split(n.Log(), "\n") {
		if strings.Contains(ln, "lease") || strings.Contains(ln, "adopt") || strings.Contains(ln, "watchdog") {
			if len(ln) > 260 {
				ln = ln[:260]
			}
			out = append(out, ln)
		}
	}
	return "\nlease lines of the promoted server:\n" + strings.Join(out, "\n")
}
