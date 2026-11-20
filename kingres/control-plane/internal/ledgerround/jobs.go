// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"errors"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Tick is how often the jobs look at the clock. The interval of a round and of a digest are settings that the console changes, so the jobs
// are woken often and decide for themselves when a pass is due (design 6.7).
const Tick = 10 * time.Second

// DefaultMissingFor is how long every group must answer that a database does not exist before the collector ends the
// publication of its chains (ReasonDatabaseMissing). One answer is not enough: an instance that is being rebuilt answers the same.
const DefaultMissingFor = 30 * time.Minute

// DropGrace is how long after the retirement of a chain for "dropped" the same chain may still be read without an alert: the task of a drop
// ends the publication first and drops the database in its later steps.
const DropGrace = 30 * time.Minute

// Jobs returns the jobs of the collector for the scheduler (design 6.7): ledger.round and ledger.digest. Both run on the manager leader
// only, under its term; a pass that returns scheduler.ErrDeposed ends the leadership.
func (c *Collector) Jobs() []scheduler.Job {
	return []scheduler.Job{
		{Name: "ledger.round", Every: Tick, Run: c.jobRound},
		{Name: "ledger.digest", Every: Tick, Run: c.jobDigest},
	}
}

func deposed(err error) bool { return errors.Is(err, scheduler.ErrDeposed) }

// jobRound is one pass of ledger.round: when the publication interval has passed since the last pass, every database with a ledger gets a
// round (the head of every group, the chain logs, the aggregate).
func (c *Collector) jobRound(ctx context.Context, _ scheduler.Term) error {
	pc, err := c.cfg.Keys.Publication(ctx)
	if err != nil {
		return err
	}
	if !pc.Enabled {
		return nil
	}
	now := c.cfg.Clock()
	c.mu.Lock()
	due := c.lastRound.IsZero() || now.Sub(c.lastRound) >= pc.Interval
	c.mu.Unlock()
	if !due {
		return nil
	}
	dbs, err := c.cfg.Databases(ctx)
	if err != nil {
		return err
	}
	for _, d := range dbs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if _, err := c.Round(ctx, d.Name, OpHead); err != nil {
			if deposed(err) {
				return err
			}
			c.log.Warn("a round of the ledger publication failed", "database", d.Name, "error", err)
			continue
		}
		if c.vanished(ctx, d.Name) {
			// every group says the database does not exist, and nobody dropped it through the control plane: end the publication of its
			// chains with an event that says so (the owner of a database can drop it from SQL; there is no hook there)
			if err := c.RetireDatabase(ctx, d.Name, ReasonDatabaseMissing, false); err != nil {
				if deposed(err) {
					return err
				}
				c.log.Warn("ending the publication of a database that vanished failed", "database", d.Name, "error", err)
			}
		}
	}
	c.mu.Lock()
	c.lastRound = now
	c.mu.Unlock()
	return nil
}

// jobDigest is one pass of ledger.digest: every database whose digestIntervalSeconds has passed since its last digest gets one (a round
// with the operation that records a digest and closes the open block). The alert ledgerDigestOverdue is raised when no digest was made
// within twice the interval.
func (c *Collector) jobDigest(ctx context.Context, _ scheduler.Term) error {
	dbs, err := c.cfg.Databases(ctx)
	if err != nil {
		return err
	}
	now := c.cfg.Clock()
	for _, d := range dbs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if d.DigestInterval <= 0 {
			continue
		}
		last, err := c.cfg.Store.LastDigestAt(ctx, d.Name)
		if err != nil {
			return err
		}
		// the time of the last attempt is kept, so that a database with an empty ledger is not tried at every tick
		c.mu.Lock()
		t, tried := c.tries[d.Name]
		c.mu.Unlock()
		if tried && now.Sub(t) < d.DigestInterval {
			continue
		}
		if !last.IsZero() && now.Sub(last) < d.DigestInterval {
			continue
		}
		c.mu.Lock()
		c.tries[d.Name] = now
		c.mu.Unlock()
		_, err = c.Digest(ctx, d.Name, "system")
		var pe *problem.Problem
		switch {
		case err == nil:
			c.alert(ctx, AlertDigestLate, "warning", d.Name, -1, "", false)
		case deposed(err):
			return err
		case errors.As(err, &pe) && pe.Code == problem.Conflict:
			// an empty ledger: nothing to digest yet
		default:
			c.log.Warn("a scheduled digest failed", "database", d.Name, "error", err)
			if !last.IsZero() && now.Sub(last) > 2*d.DigestInterval {
				c.alert(ctx, AlertDigestLate, "warning", d.Name, -1, "No digest was made within twice the digest interval of the database.", true)
			}
		}
	}
	return nil
}
