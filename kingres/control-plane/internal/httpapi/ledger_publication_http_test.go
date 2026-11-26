// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// fakePublisher answers like internal/ledgerround does, with fixed data.
type fakePublisher struct {
	size *int64
	page store.Page
	db   string
	fail error
}

func aggregate() api.LedgerAggregate {
	now := time.Now().UTC()
	return api.LedgerAggregate{
		LedgerAggregateSummary: api.LedgerAggregateSummary{Size: 2, Root: strings.Repeat("ab", 32), Checkpoint: "origin\n2\nAAAA\n\n- sig\n", GeneratedAt: now, Complete: false, UncoveredShardGroupIDs: []int32{1}},
		Members:                []api.LedgerAggregateMember{{ShardGroupID: 1, ChainID: strings.Repeat("cd", 32), Epoch: 1, Height: 4, BlockHash: strings.Repeat("ef", 32), LogRoot: strings.Repeat("01", 32), Flags: []string{"stale"}}},
		Anchors:                []api.DigestAnchor{}, Statement: "text",
	}
}

func (f *fakePublisher) Aggregate(_ context.Context, db string, size *int64) (api.LedgerAggregate, error) {
	f.db, f.size = db, size
	return aggregate(), f.fail
}

func (f *fakePublisher) Aggregates(_ context.Context, db string, p store.Page) (api.LedgerAggregatePage, error) {
	f.db, f.page = db, p
	next := "abc"
	return api.LedgerAggregatePage{Items: []api.LedgerAggregateSummary{aggregate().LedgerAggregateSummary}, NextCursor: &next}, f.fail
}

func (f *fakePublisher) Publication(context.Context) (api.LedgerPublication, error) {
	addr := "127.0.0.1:8443"
	now := time.Now().UTC()
	return api.LedgerPublication{Enabled: true, Listener: api.LedgerPublicationListener{Address: &addr, TLS: true, Running: true},
		Logs:             []api.LedgerPublicationLog{{Kind: "chain", Origin: "ledger.example.com/kingres/c/" + strings.Repeat("ab", 32) + "/e1", Size: 9, LastCheckpointAt: &now, State: "active"}},
		LastCheckpointAt: &now, Checks: map[string]any{}}, f.fail
}

func TestLedgerPublicationOperationsMatchTheContractAndAreAnsweredByTheLeader(t *testing.T) {
	fake := &fakePublisher{}
	e := newEnv(t, func(d *Deps) { d.LedgerPublication = fake })
	e.initialize()
	chk := newSchemaChecker(t)
	ro := e.token(auth.RoleReadonly)
	for _, c := range []struct{ op, path string }{
		{"getLedgerPublication", "/api/v1/ledger/publication"},
		{"getLedgerAggregate", "/api/v1/databases/shop/ledger/aggregate"},
		{"listLedgerAggregates", "/api/v1/databases/shop/ledger/aggregates?limit=7&cursor=xyz"},
	} {
		w := e.do(req{method: "GET", path: c.path, bearer: ro})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.op, w.Code, w.Body.String())
		}
		if err := chk.validateResponse(c.op, "200", w.Body.Bytes()); err != nil {
			t.Errorf("%s does not match the contract: %v\n%s", c.op, err, w.Body.String())
		}
		// the data is in the leader's system database: a follower forwards the read
		if !forwardsToLeader(operationOf(t, c.op)) {
			t.Errorf("%s is not forwarded to the leader", c.op)
		}
	}
	if fake.db != "shop" || fake.page.Limit != 7 || fake.page.Cursor != "xyz" {
		t.Errorf("the page that reached the service: %q %+v", fake.db, fake.page)
	}
	if w := e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/aggregate?size=5", bearer: ro}); w.Code != 200 || fake.size == nil || *fake.size != 5 {
		t.Errorf("size=5: %d %v", w.Code, fake.size)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/aggregate?size=0", bearer: ro}), 400, "validation_failed")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/aggregate?size=abc", bearer: ro}), 400, "validation_failed")
	// nobody without a session reads it
	if w := e.do(req{method: "GET", path: "/api/v1/ledger/publication"}); w.Code != 401 {
		t.Errorf("without a token: %d", w.Code)
	}
	// the problems of the service pass through, and an internal error is hidden behind a fixed text
	fake.fail = problem.New(problem.LedgerNotPublished, "none yet")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/aggregate", bearer: ro}), 409, "ledger_not_published")
	fake.fail = problem.New(problem.NotFound, "No such database.")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/aggregates", bearer: ro}), 404, "not_found")
	fake.fail = errors.New("open /var/lib/kingres/secret: denied")
	w := e.do(req{method: "GET", path: "/api/v1/ledger/publication", bearer: ro})
	wantProblem(t, w, 503, "service_unavailable")
	if strings.Contains(w.Body.String(), "/var/lib") {
		t.Errorf("the answer names a path: %s", w.Body.String())
	}
}

func TestLedgerPublicationWithoutAServiceIsNotSupported(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	ro := e.token(auth.RoleReadonly)
	for _, p := range []string{"/api/v1/ledger/publication", "/api/v1/databases/shop/ledger/aggregate", "/api/v1/databases/shop/ledger/aggregates"} {
		wantProblem(t, e.do(req{method: "GET", path: p, bearer: ro}), 422, "unsupported_operation")
	}
}
