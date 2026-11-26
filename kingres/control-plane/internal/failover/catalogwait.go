// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultCatalogReadWait is how long a step after the promotion waits for a catalog that cannot be read right now: the instance that holds it
// is the one that was just promoted, and it may be stopped by the fence of its lease (a lapse of the lease under load) and started again once the
// lease is won, or be starting. It is the bound of the wait for a promoted instance (mapgroup PromoteWait).
const DefaultCatalogReadWait = 3 * time.Minute

// catalogUnavailable says whether the error of a read of a catalog is the kind that ends by itself: an instance that is starting, stopping or
// not up (SQLSTATE class 08 and 57P01, 57P02, 57P03, 53300) or any failure that is not an answer of the server (a socket that is gone, a
// connection that broke, a timeout). An answer of the server with another code (a missing relation, a refused privilege) and an error that this
// package made on purpose (a coded Error) do not end by waiting.
func catalogUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var fe *Error
	if errors.As(err, &fe) {
		return false
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return unavailableState(pe.Code)
	}
	var se *SQLError
	if errors.As(err, &se) {
		return se.Code == "" || unavailableState(se.Code)
	}
	return true
}

// unavailableState says whether a SQLSTATE is that of an instance that is not up right now.
func unavailableState(code string) bool {
	if strings.HasPrefix(code, "08") {
		return true
	}
	switch code {
	case "57P01", "57P02", "57P03", "53300":
		return true
	}
	return false
}

// readCatalog runs one read of a catalog (f) in a step after the promotion. A read that fails because the instance cannot be reached right now
// (catalogUnavailable) is repeated every LeasePoll for CatalogReadWait; the promoted instance of the map group is stopped and started again by
// its lease manager when the lease lapsed once (a fence), and a failed task is not what the operator meets after the promotion. When the wait
// ends, and for any other error, the step pauses as targetsPause does: the group was promoted and holds the lease, and the repeated request runs the step again.
func (sc *stepCtx) readCatalog(ctx context.Context, f func() error) error {
	r := sc.r
	var deadline time.Time
	for {
		err := f()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
			return err
		}
		if !catalogUnavailable(err) {
			return sc.targetsPause(ctx, err)
		}
		if deadline.IsZero() {
			deadline = r.now().Add(orDur(r.CatalogReadWait, DefaultCatalogReadWait))
		}
		if !r.now().Before(deadline) {
			return sc.targetsPause(ctx, err)
		}
		if serr := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); serr != nil {
			return serr
		}
	}
}
