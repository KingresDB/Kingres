// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
)

// CatalogStepName is the name of the one step of the reconcile task that records
// the standbys in the sharding catalog. Phase 2 of high availability fills it in
// (kingres_shard 0.4: kingres.add_node(..., role => 'standby'), the replacement
// of kingres.remove_node for a standby); nothing else in this package knows the
// catalog's idea of a standby.
const CatalogStepName = "standby.register_in_catalog"

// ErrCatalogUnsupported is what a registrar that cannot register standbys
// answers. The step records it in the task detail and succeeds: the standbys
// work (they replicate and are supervised) without being in the catalog.
var ErrCatalogUnsupported = errors.New("skipped: needs kingres_shard 0.4")

// CatalogRegistrar registers standbys in the sharding catalog as nodes with the
// role standby (they own no buckets and receive no forwarded statements). It is
// the single place phase 2 has to implement.
type CatalogRegistrar interface {
	// Register adds the standby (idempotent: registering a standby that is in the
	// catalog is not an error). It runs on the manager leader, which is where the
	// catalog can be written.
	Register(ctx context.Context, e Entry) error
	// Unregister removes the standby from the catalog (idempotent).
	Unregister(ctx context.Context, e Entry) error
}

// SkipCatalog is the registrar of this phase: the installed extension (0.2) has no
// standby nodes, so every call answers ErrCatalogUnsupported.
type SkipCatalog struct{}

// Register implements CatalogRegistrar.
func (SkipCatalog) Register(context.Context, Entry) error { return ErrCatalogUnsupported }

// Unregister implements CatalogRegistrar.
func (SkipCatalog) Unregister(context.Context, Entry) error { return ErrCatalogUnsupported }
