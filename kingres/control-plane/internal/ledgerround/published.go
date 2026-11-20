// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
)

// PublishedState is the record of what was published, outside the system database (internal/ledgerstate, P8-01).
type PublishedState interface {
	Allow(origin string, size int64, root []byte) error
	RecordCheckpoint(origin string, size int64, root []byte) error
	Check(have func(origin string, size int64) ([]byte, bool, error), events int64, eventNote func(seq int64) ([]byte, error)) error
	Initialized() bool
	Damaged() bool
	Replace(s ledgerstate.State) error
	Merge(s ledgerstate.State) error
	Snapshot() ledgerstate.State
}

// AlertPublishedState is the subject of the alert of a database that is older than what was published (kind ledgerSignerUnavailable: nothing
// can be signed; correction 171 says why no kind of its own).
const (
	AlertPublishedKind    = "ledgerSignerUnavailable"
	AlertPublishedSubject = "published-state"
)

// ErrPublishedState: the system database is older than what was published, or contradicts it; nothing is signed.
var ErrPublishedState = errors.New("ledgerround: the system database is older than what was published")

func (c *Collector) publishedAlert(ctx context.Context, msg string, raise bool) {
	if c.cfg.Alert == nil {
		return
	}
	c.cfg.Alert(context.WithoutCancel(ctx), AlertPublishedKind, "critical", AlertPublishedSubject, msg, raise)
}

// eventCount counts the events the database serves, starting from a number that is known to be there.
func (c *Collector) eventCount(ctx context.Context, from int64) (int64, error) {
	n := from
	for {
		_, err := c.cfg.Store.Object(ctx, "", fmt.Sprintf("events/%06d", n))
		if errors.Is(err, ledgerpub.ErrNotFound) {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
		n++
	}
}

func (c *Collector) eventNote(ctx context.Context) func(int64) ([]byte, error) {
	return func(seq int64) ([]byte, error) {
		o, err := c.cfg.Store.Object(ctx, "", fmt.Sprintf("events/%06d", seq))
		if errors.Is(err, ledgerpub.ErrNotFound) {
			return nil, fmt.Errorf("%w: event %d is missing", ledgerstate.ErrBehind, seq)
		}
		return o.Data, err
	}
}

// buildState reads what the database holds as the state: the first start of a tree that had none, and the state of a log set made by hand.
func (c *Collector) buildState(ctx context.Context) (ledgerstate.State, error) {
	st := ledgerstate.State{Logs: map[string]ledgerstate.Mark{}}
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return st, err
	}
	for _, l := range logs {
		cp, err := c.cfg.Store.LatestCheckpoint(ctx, l.ID)
		if errors.Is(err, ledgerpub.ErrNotFound) {
			continue
		}
		if err != nil {
			return st, err
		}
		st.Logs[l.Origin] = ledgerstate.Mark{Size: cp.Size, Root: hexOf(cp.Root)}
	}
	n, err := c.eventCount(ctx, 0)
	if err != nil {
		return st, err
	}
	st.Events = n
	if n > 0 {
		note, err := c.eventNote(ctx)(n - 1)
		if err != nil {
			return st, err
		}
		st.EventHash = ledgerstate.EventHashOf(note)
	}
	return st, nil
}

// checkPublished is the first act of every round and of every retirement: the database must hold what was published. When this server has no
// record yet (the first start after an upgrade from a tree without it, a new holder) it asks the other holders and merges what they have, and
// only if none answers does it take what the database holds as the starting point (a database restored before that moment cannot be told; a
// log line says so). A database that is behind or contradicts the record refuses the round with a critical alert, signs nothing and writes
// nothing; it clears itself when the database is as it was published.
func (c *Collector) checkPublished(ctx context.Context) error {
	p := c.cfg.Published
	if p == nil {
		return nil
	}
	if p.Damaged() {
		c.publishedAlert(ctx, "The record of what was published (published.json in the key directory) is damaged: nothing is signed until an operator checks it and removes it.", true)
		return fmt.Errorf("%w: the record of what was published is damaged", ErrPublishedState)
	}
	if !p.Initialized() {
		if c.cfg.PullState != nil {
			if st, ok := c.cfg.PullState(ctx); ok {
				if err := p.Merge(st); err != nil {
					c.publishedAlert(ctx, "The record of what was published that another server holds contradicts this server's: "+err.Error(), true)
					return fmt.Errorf("%w: %v", ErrPublishedState, err)
				}
			}
		}
	}
	if !p.Initialized() {
		st, err := c.buildState(ctx)
		if err != nil {
			return err
		}
		if err := p.Replace(st); err != nil {
			return err
		}
		c.log.Warn("this server had no record of what was published: the checkpoints and events of the system database are taken as published", "logs", len(st.Logs), "events", st.Events)
	}
	snap := p.Snapshot()
	have := func(origin string, size int64) ([]byte, bool, error) {
		l, err := c.cfg.Store.LogByOrigin(ctx, origin)
		if errors.Is(err, ledgerpub.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		cp, err := c.cfg.Store.Checkpoint(ctx, l.ID, size)
		if errors.Is(err, ledgerpub.ErrNotFound) {
			return nil, false, nil
		}
		if err != nil {
			return nil, false, err
		}
		return cp.Root, true, nil
	}
	from := snap.Events - 1
	if from < 0 {
		from = 0
	}
	n := from
	if snap.Events > 0 {
		if _, err := c.cfg.Store.Object(ctx, "", fmt.Sprintf("events/%06d", from)); errors.Is(err, ledgerpub.ErrNotFound) {
			n = from // fewer events than were published
		} else if err != nil {
			return err
		} else {
			var cerr error
			if n, cerr = c.eventCount(ctx, from+1); cerr != nil {
				return cerr
			}
		}
	}
	if n == 0 {
		var cerr error
		if n, cerr = c.eventCount(ctx, 0); cerr != nil {
			return cerr
		}
	}
	err := p.Check(have, n, c.eventNote(ctx))
	switch {
	case err == nil:
		c.publishedAlert(ctx, "", false)
		return nil
	case errors.Is(err, ledgerstate.ErrBehind), errors.Is(err, ledgerstate.ErrFork), errors.Is(err, ledgerstate.ErrDamaged):
		msg := "The system database is older than what was published, or contradicts it (a restore of an older backup?): " + err.Error() +
			". Nothing is signed and nothing is written until the database holds what was published. Events published: " + strconv.FormatInt(snap.Events, 10) + "."
		c.log.Error("the system database is older than what was published", "error", err)
		c.publishedAlert(ctx, msg, true)
		return fmt.Errorf("%w: %v", ErrPublishedState, err)
	}
	return err
}

// CheckPublished is checkPublished for the key service: a key task that would append an event first asks whether the database holds what
// was published (the hook ledgersign.Config.PublishedCheck).
func (c *Collector) CheckPublished(ctx context.Context) error { return c.checkPublished(ctx) }
