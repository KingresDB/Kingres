// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package pgconfig

import (
	"context"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
)

// P13R-04: the classification of hidden parameters fails closed. A real PostgreSQL always hides some parameters from
// an ordinary role, so an empty answer is an error (a role that cannot be assumed, a catalog that answered nothing),
// and so is a failed query; the caller then answers 503 instead of classifying with an empty set.
func TestHiddenFailsClosed(t *testing.T) {
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	ctx := context.Background()
	set, err := readSettings(ctx, c, "")
	if err != nil {
		t.Fatal(err)
	}
	h, err := hidden(ctx, c, set)
	if err != nil || len(h) == 0 {
		t.Fatalf("hidden: %v %v", h, err)
	}
	isHidden := map[string]bool{}
	for _, n := range h {
		isHidden[n] = true
	}
	var visible []Setting
	for _, s := range set {
		if !isHidden[s.Name] {
			visible = append(visible, s)
		}
	}
	if _, err := hidden(ctx, c, visible); err == nil {
		t.Error("an empty hidden set must be an error")
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := hidden(cancelled, c, set); err == nil {
		t.Error("a failed query must be an error")
	}
}
