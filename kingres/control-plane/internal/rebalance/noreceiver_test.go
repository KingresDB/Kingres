// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The extension says in three words (by version and by function) that no shard group may receive buckets; every one of them is the
// answer no_receiving_server, and nothing else is: a cluster that "cannot be inspected" is a 503 and a different story.
func TestNoReceivingGroupRecognisesEveryWording(t *testing.T) {
	for _, msg := range []string{"no shard group accepts buckets", "no shard group can receive buckets", "no shard group can hold buckets"} {
		if !noReceivingGroup(&pgconn.PgError{Code: "55000", Message: msg}) {
			t.Errorf("%q is not recognised", msg)
		}
		if !noReceivingGroup(fmt.Errorf("planning: %w", &pgconn.PgError{Code: "55000", Message: msg})) {
			t.Errorf("%q wrapped is not recognised", msg)
		}
		if noReceivingGroup(&pgconn.PgError{Code: "XX000", Message: msg}) {
			t.Errorf("%q with another SQLSTATE is recognised", msg)
		}
	}
	for _, err := range []error{nil, errors.New("no shard group accepts buckets"), &pgconn.PgError{Code: "55000", Message: "node 3 does not answer"}} {
		if noReceivingGroup(err) {
			t.Errorf("%v is recognised", err)
		}
	}
}

// The extension 0.9 gives the refusal its own SQLSTATE, KS017: that alone decides, whatever the wording is.
func TestNoReceivingGroupMatchesTheSQLSTATE(t *testing.T) {
	for _, msg := range []string{"no shard group can hold buckets", "any other wording of the same refusal", ""} {
		if !noReceivingGroup(&pgconn.PgError{Code: "KS017", Message: msg}) {
			t.Errorf("KS017 %q is not recognised", msg)
		}
		if !noReceivingGroup(fmt.Errorf("planning: %w", &pgconn.PgError{Code: "KS017", Message: msg})) {
			t.Errorf("KS017 %q wrapped is not recognised", msg)
		}
	}
	// the code of another refusal is not it, with the same words in the text
	if noReceivingGroup(&pgconn.PgError{Code: "KS018", Message: "no shard group can receive buckets"}) {
		t.Error("KS018 is recognised")
	}
}

// KS018 is the refusal of a plan by disk size when a group does not answer; the text of the extension 0.8 (55000) is the fallback.
func TestSizesUnreadableMatchesTheSQLSTATE(t *testing.T) {
	for _, pe := range []*pgconn.PgError{
		{Code: "KS018", Message: "the sizes of the buckets of shard group 1 cannot be read (node 2 did not answer)"},
		{Code: "KS018", Message: "another wording"},
		{Code: "55000", Message: "the sizes of the buckets of shard group 1 cannot be read (x): a plan by disk size does not guess"}, // 0.8
	} {
		if !sizesUnreadable(fmt.Errorf("planning: %w", pe)) {
			t.Errorf("%+v is not recognised", pe)
		}
	}
	for _, err := range []error{nil, errors.New("cannot be read"), &pgconn.PgError{Code: "XX000", Message: "cannot be read"},
		&pgconn.PgError{Code: "KS017", Message: "no shard group can receive buckets"}, &pgconn.PgError{Code: "55000", Message: "something else"}} {
		if sizesUnreadable(err) {
			t.Errorf("%v is recognised", err)
		}
	}
}

// settleSplit tells the refusals of split_bucket by SQLSTATE: KS0C1 cancelled, 0A000 unsupported, KS019 busy; the extension 0.8 is told by
// the text under 55000 and 55006 only.
func TestSettleSplitMatchesTheSQLSTATE(t *testing.T) {
	s := &Service{}
	code := func(pe *pgconn.PgError) any {
		err := s.settleSplit(context.Background(), splitInput{}, 1, fmt.Errorf("the call: %w", pe))
		if _, ok := err.(splitCancelled); ok {
			return "cancelled"
		}
		var p *problem.Problem
		if errors.As(err, &p) {
			return p.Code
		}
		return "failed"
	}
	for _, c := range []struct {
		pe   *pgconn.PgError
		want any
	}{
		{&pgconn.PgError{Code: "KS0C1", Message: "split 7 was cancelled"}, "cancelled"},
		{&pgconn.PgError{Code: "KS0C1", Message: "any wording"}, "cancelled"},
		{&pgconn.PgError{Code: "0A000", Message: "table t is partitioned by the user: a split is not supported yet"}, problem.SplitUnsupported},
		{&pgconn.PgError{Code: "0A000", Message: "any wording"}, problem.SplitUnsupported},
		{&pgconn.PgError{Code: "KS019", Message: "colocation group 3 is already being split"}, problem.BucketBusy},
		{&pgconn.PgError{Code: "KS019", Message: "bucket 1 of colocation group 3 is in state \"moving\""}, problem.BucketBusy},
		{&pgconn.PgError{Code: "KS019", Message: "any wording"}, problem.BucketBusy},
		// the extension 0.8
		{&pgconn.PgError{Code: "55000", Message: "table public.np has no primary key: a split matches the rows of the children by key"}, problem.SplitUnsupported},
		{&pgconn.PgError{Code: "55006", Message: "colocation group 3 is already being split"}, problem.BucketBusy},
		// the text alone, under another code, is nothing
		{&pgconn.PgError{Code: "XX000", Message: "colocation group 3 is already being split"}, "failed"},
		{&pgconn.PgError{Code: "XX000", Message: "split 7 was cancelled"}, "failed"},
		{&pgconn.PgError{Code: "XX000", Message: "no primary key"}, "failed"},
	} {
		if got := code(c.pe); got != c.want {
			t.Errorf("%s %q: got %v, want %v", c.pe.Code, c.pe.Message, got, c.want)
		}
	}
}
