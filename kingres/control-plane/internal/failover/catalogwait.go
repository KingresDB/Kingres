// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package failover

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// DefaultCatalogReadWait is how long one step after the promotion waits, in all, for a catalog that cannot be read right now: the instance that holds
// it is the one that was just promoted, and it may be stopped by the fence of its lease (a lapse of the lease under load) and started again once the
// lease is won, or be starting. It is the bound of the wait for a promoted instance (mapgroup PromoteWait).
const DefaultCatalogReadWait = 3 * time.Minute

// catalogUnavailable says whether the error of a read of a catalog is the kind that ends by itself: an instance that is starting or stopping or
// not up (SQLSTATE class 08, 57P01, 57P02, 57P03, 53300), a call that did not finish (SQLError without a code), or a failure of the connection
// itself (a socket that is gone or refuses, a connection that was reset or ended, a time-out of the network, a deadline that is not the one of this step).
// Everything else does not end by waiting and is not waited for: an answer of the server with another code (a missing relation, a refused privilege),
// an error that this package made on purpose (a coded Error), a service that is not configured or has no credentials, a catalog that is not the
// extension's, a value that cannot be read.
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
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	for _, target := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, io.EOF, io.ErrUnexpectedEOF, context.DeadlineExceeded} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
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

// readCatalog runs one read of a catalog (f) in a step after the promotion; what names the read for the pause ("the nodes of database fo"). A read that
// fails because the instance cannot be reached right now (catalogUnavailable) is repeated every LeasePoll. The wait is one for the whole step: its deadline is
// set at the first such failure of the step and every read of the step shares it (CatalogReadWait). The promoted instance of the map group is stopped and
// started again by its lease manager when the lease lapsed once (a fence), and a failed task is not what the operator meets after the promotion: when the
// wait ends, and at once for any other error, the step pauses; the group was promoted and holds the lease, and the repeated request runs the step again.
func (sc *stepCtx) readCatalog(ctx context.Context, what string, f func() error) error {
	r := sc.r
	for {
		err := f()
		if err == nil {
			return nil
		}
		if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
			return err
		}
		if !catalogUnavailable(err) {
			return sc.readPause(ctx, what, err)
		}
		if sc.catDeadline.IsZero() {
			sc.catDeadline = r.now().Add(orDur(r.CatalogReadWait, DefaultCatalogReadWait))
		}
		if !r.now().Before(sc.catDeadline) {
			return sc.readPause(ctx, what, err)
		}
		if serr := r.sleep(ctx, orDur(r.LeasePoll, time.Second)); serr != nil {
			return serr
		}
	}
}

// readPause pauses the task at a step after the promotion because a catalog cannot be read: the alert is the one of the other pauses after the promotion,
// and the message names the step and the read.
func (sc *stepCtx) readPause(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil || errors.Is(err, ErrLeaseLost) {
		return err
	}
	sc.r.Plan.Alert(ctx, "failoverBlocked", "critical", subjectOf(sc.group, sc.old()), fmt.Sprintf("Step %q of shard group %d cannot read %s: %v", sc.key, sc.group, what, err), true)
	return &PauseError{Reason: fmt.Sprintf("The step %q of shard group %d cannot read %s (%v). The group was promoted and holds the lease; repair what the message names, then repeat the request.",
		sc.key, sc.group, what, err)}
}
