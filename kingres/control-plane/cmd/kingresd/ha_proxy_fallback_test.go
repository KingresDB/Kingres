// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"errors"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clientproxy"
)

// F3-09: a server that cannot read its own catalog (its only primary was failed over) takes it from a peer, so that its entry
// point forwards instead of resetting the connection.
func TestEntryPointCatalogFallsBackToAPeer(t *testing.T) {
	rows := []clientproxy.Node{{ID: 1, Group: 1, Host: "s2", Port: 5400, Primary: true, Active: true}}
	bad := func(context.Context) ([]clientproxy.Node, error) { return nil, errors.New("connection refused") }
	good := func(context.Context) ([]clientproxy.Node, error) { return rows, nil }
	got, err := nodesWithFallback(context.Background(), bad, []func(context.Context) ([]clientproxy.Node, error){bad, good})
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", got, err)
	}
	if _, err := nodesWithFallback(context.Background(), bad, nil); err == nil {
		t.Fatal("no source answered and no error was reported")
	}
	local := func(context.Context) ([]clientproxy.Node, error) { return nil, nil }
	if got, err := nodesWithFallback(context.Background(), local, []func(context.Context) ([]clientproxy.Node, error){bad}); err != nil || got != nil {
		t.Fatalf("a readable local catalog must not be replaced: %v %v", got, err)
	}
}
