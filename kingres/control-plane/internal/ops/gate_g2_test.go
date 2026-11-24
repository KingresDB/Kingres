// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// The partitions of a distributed table are read from the schema kingres, so extguard decides about kingres_shard before the lookup of
// kingres.partition_leaf_storage and before the call of it (the service role is a superuser; the schema may be a tenant's).
func TestDistPartitionStorageIsReadAfterTheTrustCheck(t *testing.T) {
	src, err := os.ReadFile("tableprep.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(src)
	i := strings.Index(s, "func (o *Ops) fillDistPartitions(")
	if i < 0 {
		t.Fatal("fillDistPartitions is gone")
	}
	body := s[i:]
	gate := strings.Index(body, "extUsable(ctx, tx, extguard.Shard)")
	first := strings.Index(body, "partition_leaf_storage")
	if gate < 0 || first < 0 || gate > first {
		t.Errorf("fillDistPartitions must ask extguard (at %d) before it names kingres.partition_leaf_storage (at %d)", gate, first)
	}
}

// The import verifies the sharding extension with extguard like every other one, and before the others.
func TestImportVerifiesTheShardingExtensionFirst(t *testing.T) {
	if got := extensionsToRequire(true, []string{extguard.Ledger}); !reflect.DeepEqual(got, []string{extguard.Shard, extguard.Ledger}) {
		t.Errorf("with the sharding extension: %v", got)
	}
	if got := extensionsToRequire(false, []string{extguard.Ledger}); !reflect.DeepEqual(got, []string{extguard.Ledger}) {
		t.Errorf("without it: %v", got)
	}
}
