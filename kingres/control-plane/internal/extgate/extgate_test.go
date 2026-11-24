// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package extgate

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func TestProblemNamesTheCheckNotTheObject(t *testing.T) {
	u := &extguard.UntrustedError{Extension: extguard.Shard, Schema: "kingres", Reason: extguard.ReasonForeignObject, Detail: "view kingres.evil\"; DROP"}
	p := Problem(u, "tenantdb")
	if p.Code != problem.ExtensionUnavailable {
		t.Fatalf("code %s", p.Code)
	}
	for _, want := range []string{"kingres_shard", "tenantdb", "cannot be trusted", "foreignObject"} {
		if !strings.Contains(p.Detail, want) {
			t.Errorf("the refusal does not say %q: %s", want, p.Detail)
		}
	}
	if strings.Contains(p.Detail, "evil") {
		t.Errorf("the refusal repeats the name of an object that the owner of the database chose: %s", p.Detail)
	}
	if q := Problem(u, ""); !strings.Contains(q.Detail, "this database") {
		t.Errorf("without a name: %s", q.Detail)
	}
}

// failing is a Querier that fails: the error of the query is what the gates return, never "absent".
type failing struct{ err error }

func (f failing) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, f.err }

func TestAQueryErrorIsNotAbsent(t *testing.T) {
	boom := errors.New("boom")
	q := failing{boom}
	ctx := context.Background()
	if ok, err := Usable(ctx, q, extguard.Shard, "d"); ok || !errors.Is(err, boom) {
		t.Fatalf("Usable: %v %v", ok, err)
	}
	if err := Require(ctx, q, extguard.Shard, "d"); !errors.Is(err, boom) {
		t.Fatalf("Require: %v", err)
	}
	if err := RefuseUntrusted(ctx, q, "d", extguard.Shard); !errors.Is(err, boom) {
		t.Fatalf("RefuseUntrusted: %v", err)
	}
}
