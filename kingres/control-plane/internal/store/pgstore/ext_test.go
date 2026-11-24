// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgstore

import (
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// An extension that is not installed must not be remembered: the control plane installs it itself (PATCH sharding.enabled) and the very next read of
// the database must see it. A remembered "absent" answer made the PATCH answer 200 with sharding still off for extTTL.
func TestExtInfoDoesNotRememberAnAbsentExtension(t *testing.T) {
	s := newTestStore(t)
	c := ctx(t)
	p, err := s.pool(c, "plain_cp")
	if err != nil {
		t.Fatal(err)
	}
	info, err := s.extInfo(c, p, extguard.Shard)
	if err != nil {
		t.Fatal(err)
	}
	if info.Present {
		t.Fatal("kingres_shard is present in the plain fixture database")
	}
	s.ext.mu.Lock()
	n := len(s.ext.m)
	s.ext.mu.Unlock()
	if n != 0 {
		t.Fatalf("an absent extension was remembered (%d answers)", n)
	}
}
