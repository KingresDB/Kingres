// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package rebalance

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
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
