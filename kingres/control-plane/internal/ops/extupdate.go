// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"fmt"
	"strings"
)

// The update of kingres_timepart (phase 12, St-D1b). A run of a policy of a distributed table converts the leaves on the servers that hold them, so every server must
// have the version of the package, which has the procedure that does it (kingres.convert_leaf). A database that was made before a version that added a function keeps
// the version it has until someone updates it; this does it, for every database of this server that has the extension and on every other server, with the update path
// that the extension steps already have (ApplyExtensionInstall: CREATE EXTENSION when it is missing, ALTER EXTENSION UPDATE to the default version of the package of
// that server when it is at another version, nothing when it is the same). It runs as a job of the manager leader and does nothing when there is nothing to update.

// UpdateTimepart brings kingres_timepart up to the version of the package in every database of this server that has it, and in the same databases on the other
// servers. A database or a server that cannot be reached is skipped and looked at again by the next pass; it returns the number of servers it asked and the
// first error.
func (o *Ops) UpdateTimepart(ctx context.Context) (int, error) {
	dbs, err := PartitionBackend{O: o}.databasesWithTimepart(ctx)
	if err != nil {
		return 0, err
	}
	total := 0
	var first error
	for _, db := range dbs {
		n, err := o.updateExtensionEverywhere(ctx, db, ExtTimepart)
		total += n
		if err != nil && first == nil {
			first = err
		}
	}
	return total, first
}

// updateExtensionEverywhere updates one extension of one database on this server and on the others, and returns how many servers it asked (a server at the version already is asked and does nothing).
func (o *Ops) updateExtensionEverywhere(ctx context.Context, db, ext string) (int, error) {
	n := 0
	var bad []string
	b := o.ext()
	changed := func(ctx context.Context) (bool, error) {
		cur, err := b.Installed(ctx, db, ext)
		if err != nil {
			return false, err
		}
		def, err := b.Available(ctx, db, ext)
		if err != nil {
			return false, err
		}
		return cur != "" && def != "" && cur != def, nil
	}
	op := extensionOp{Database: db, Extension: ext}
	if need, err := changed(ctx); err != nil {
		bad = append(bad, fmt.Sprintf("%s: %v", o.ServerHost, err))
	} else if need {
		if err := o.extInstallLocal(ctx, op); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", o.ServerHost, err))
		} else {
			n++
		}
	}
	if o.Fanout != nil {
		for _, s := range o.others(ctx) {
			if err := o.Fanout.Apply(ctx, s.ID, ApplyExtensionInstall, op); err != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", s.ID, err))
				continue
			}
			n++
		}
	}
	if len(bad) > 0 {
		return n, fmt.Errorf("updating %s in database %s: %s", ext, db, strings.Join(bad, "; "))
	}
	return n, nil
}
