// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

func isDeposed(err error) bool { return errors.Is(err, scheduler.ErrDeposed) }

// jobEnqueue is one pass of ledger.anchor.enqueue: when the anchoring interval has passed since the last pass (or
// at the first pass of a new leader), every enabled target gets a record for the newest aggregate checkpoint of
// every database that has one, unless it has one for that size or a newer one already (6.1). The older records
// that are still pending are superseded by the store, in the same transaction as the new record.
func (s *Service) jobEnqueue(ctx context.Context, term scheduler.Term) error {
	set, err := s.cfg.Store.Settings(ctx)
	if err != nil {
		return err
	}
	now := s.now()
	s.mu.Lock()
	due := s.lastEnqueue.IsZero() || now.Sub(s.lastEnqueue) >= set.Interval
	s.mu.Unlock()
	if !due {
		return nil
	}
	if err := s.enqueueAll(ctx, term, now, ""); err != nil {
		return err
	}
	s.mu.Lock()
	s.lastEnqueue = now
	s.mu.Unlock()
	return nil
}

// enqueueAll creates the records of the newest checkpoints for the enabled targets; db "" means every database.
func (s *Service) enqueueAll(ctx context.Context, term scheduler.Term, now time.Time, db string) error {
	targets, err := s.cfg.Store.Targets(ctx)
	if err != nil {
		return err
	}
	aggs, err := s.cfg.Store.Aggregates(ctx)
	if err != nil {
		return err
	}
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		for _, a := range aggs {
			if db != "" && a.Database != db {
				continue
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, _, err := s.cfg.Store.Enqueue(ctx, term, Enqueue{Target: Target{ID: t.ID, Name: t.Name, Kind: t.Kind}, Agg: a, Now: now}); err != nil {
				if isDeposed(err) {
					return err
				}
				s.log.Warn("enqueueing an anchor record failed", "target", t.ID, "database", a.Database, "error", errText(err))
			}
		}
	}
	return nil
}

// errText is the text of an error for a log line: the errors of this package and of the libraries hold no
// credential (static text, statuses and sanitised store messages), and a text from outside is cut.
func errText(err error) string { return (anchor.Redactor{}).Clean(err.Error(), 300) }

// jobSubmit is one pass of ledger.anchor.submit: the pending records of enabled targets that are due. The targets
// are worked on side by side (a target that hangs for its timeout holds one place) and the records of one target
// one after the other.
func (s *Service) jobSubmit(ctx context.Context, term scheduler.Term) error {
	return s.pass(ctx, term, func(r Row) bool { return r.State == anchor.StatePending })
}

// jobUpgrade is one pass of ledger.anchor.upgrade: the submitted and claimed records that are due.
func (s *Service) jobUpgrade(ctx context.Context, term scheduler.Term) error {
	return s.pass(ctx, term, func(r Row) bool { return r.State == anchor.StateSubmitted || r.State == anchor.StateClaimed })
}

func (s *Service) pass(ctx context.Context, term scheduler.Term, want func(Row) bool) error {
	now := s.now()
	rows, err := s.cfg.Store.Due(ctx, now, BatchSize)
	if err != nil {
		return err
	}
	byTarget := map[int64][]Row{}
	var order []int64
	for _, r := range rows {
		if !want(r) || !r.Record.Due(now) {
			continue
		}
		if _, ok := byTarget[r.TargetID]; !ok {
			order = append(order, r.TargetID)
		}
		byTarget[r.TargetID] = append(byTarget[r.TargetID], r)
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, MaxParallelTargets)
	var mu sync.Mutex
	var fatal error
	for _, id := range order {
		wg.Add(1)
		sem <- struct{}{}
		go func(id int64, list []Row) {
			defer wg.Done()
			defer func() { <-sem }()
			// a panic of a step is the error of the pass and never the end of the daemon (the scheduler protects its own goroutine, not this one)
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("a job of anchoring panicked", "target", id, "panic", fmt.Sprint(r))
				}
			}()
			if err := s.workTarget(ctx, term, id, list); err != nil {
				mu.Lock()
				if fatal == nil {
					fatal = err
				}
				mu.Unlock()
			}
		}(id, byTarget[id])
	}
	wg.Wait()
	return fatal
}

// workTarget works on the records of one target; it returns only an error that ends the pass (the term is over).
func (s *Service) workTarget(ctx context.Context, term scheduler.Term, id int64, list []Row) error {
	trow, err := s.cfg.Store.Target(ctx, id)
	if err != nil {
		return nil
	}
	b, berr := s.build(trow)
	for _, r := range list {
		if ctx.Err() != nil {
			return nil
		}
		if !s.claim(r.ID) {
			continue
		}
		err := s.work(ctx, term, trow, b, berr, r)
		s.release(r.ID)
		if isDeposed(err) {
			return err
		}
		if err != nil {
			s.log.Warn("an anchor record could not be worked on", "record", r.ID, "target", id, "error", errText(err))
		}
	}
	return nil
}

// work is one step of one record: Submit of a pending record, Upgrade of a submitted or claimed one. The order is
// the one that survives a kill at any point: the start of the first submission is written before the call, the
// call is made with no transaction open, and the result is written by a compare and swap on the version of the
// row that the call started from; a kill between two of these leaves a row that the next pass completes (the
// libraries make each call idempotent).
func (s *Service) work(ctx context.Context, term scheduler.Term, trow TargetRow, b *Built, berr error, r Row) error {
	now := s.now()
	row := r
	if r.State == anchor.StatePending {
		var err error
		row, err = s.cfg.Store.Begin(ctx, term, r.ID, now)
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return nil // the row changed under us: the next pass decides again
		}
		if err != nil {
			return err
		}
		if err := s.hook("after-begin"); err != nil {
			return err
		}
	} else {
		// ExpireIfDue first: a record that waited long enough ends without another call (duty 13)
		if b != nil && b.Kind.Timeouts != nil {
			sub, cl := b.Kind.Timeouts(b.Config)
			if sub > 0 || cl > 0 {
				if exp, ok := row.Record.ExpireIfDue(now, sub, cl); ok {
					w := row
					w.Record = exp
					w.Alert, w.ErrorCode = AlertFailed, "expired"
					w.LastError = "the target did not confirm the record in time; use Retry to wait again"
					return s.apply(ctx, term, w, row.Version)
				}
			}
		}
	}
	if berr != nil {
		// the target cannot be built: the credentials or the configuration are wrong or missing here
		return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassRetryable, row.Kind, "target", "target_unavailable", "%s", errText(berr)), nil)
	}
	var (
		rc  anchor.Receipt
		err error
	)
	switch row.State {
	case anchor.StatePending:
		var p anchor.Payload
		p, err = anchor.PayloadFor(row.Record, row.AggregateID, row.Note)
		if err != nil {
			return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "payload", "bad_payload", "%s", errText(err)), b)
		}
		cctx, cancel := context.WithTimeout(ctx, s.timeout(s.cfg.SubmitTimeout, SubmitTimeout))
		if res, ok := b.Target.(anchor.Resumer); ok && len(row.Evidence) > 0 {
			rc, err = res.Resume(cctx, p, row.Evidence)
		} else {
			rc, err = b.Target.Submit(cctx, p)
		}
		cancel()
	default:
		in := anchor.Receipt{State: string(row.State), Evidence: row.Evidence, EvidenceTime: row.EvidenceTime, NextCheck: row.NextAttemptAt}
		cctx, cancel := context.WithTimeout(ctx, s.timeout(s.cfg.UpgradeTimeout, UpgradeTimeout))
		rc, err = b.Target.Upgrade(cctx, in)
		cancel()
	}
	if ctx.Err() != nil {
		return nil // the leadership ended during the call: the next leader decides again
	}
	if err := s.hook("after-call"); err != nil {
		return err
	}
	if err != nil {
		return s.fail(ctx, term, row, err, b)
	}
	return s.succeed(ctx, term, row, rc, b)
}

// succeed applies the receipt of a call that returned no error. A receipt whose evidence the library's own
// parser refuses is a fault of the target's code and fails the record: nothing is anchored on evidence that does
// not read back.
func (s *Service) succeed(ctx context.Context, term scheduler.Term, row Row, rc anchor.Receipt, b *Built) error {
	now := s.now()
	canon, err := anchor.CanonicalEvidence(row.Kind, rc.Evidence)
	if err != nil {
		return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "receipt", "bad_evidence", "the evidence of the receipt does not read back: %s", errText(err)), b)
	}
	rc.Evidence = canon
	rec, err := row.Record.ApplyReceipt(rc, now)
	if err != nil {
		s.log.Warn("a receipt was refused by the state machine, the job is stale", "record", row.ID, "state", string(row.State), "receipt", rc.State)
		return nil
	}
	w := row
	w.Record = rec
	w.ErrorCode, w.Alert = "", ""
	return s.apply(ctx, term, w, row.Version)
}

// fail applies the failure of a call to the record, as table 6.6 says, and keeps the alert it names with the row.
func (s *Service) fail(ctx context.Context, term scheduler.Term, row Row, err error, b *Built) error {
	now := s.now()
	rec, alertKind, aerr := row.Record.AfterError(err, now, s.cfg.Rand(), s.cfg.Limits)
	if aerr != nil {
		s.log.Warn("a failure was refused by the state machine, the job is stale", "record", row.ID, "state", string(row.State))
		return nil
	}
	if b != nil {
		rec.LastError = b.Red.Clean(rec.LastError, anchor.MaxLastError)
	}
	w := row
	w.Record = rec
	w.ErrorCode = anchor.CodeOf(err)
	w.Alert = alertKind
	if alertKind == anchor.AlertNone && (rec.State == anchor.StateFailed || rec.State == anchor.StateExpired) {
		w.Alert = AlertFailed
	}
	return s.apply(ctx, term, w, row.Version)
}

func (s *Service) apply(ctx context.Context, term scheduler.Term, w Row, expect int64) error {
	if err := s.hook("before-apply"); err != nil {
		return err
	}
	if err := s.cfg.Store.Apply(ctx, term, w, expect); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotFound) {
			return nil // changed meanwhile (a Retry, a delete, a second pass): this result is dropped
		}
		return err
	}
	return nil
}

// evidenceJSON returns the evidence object of a row as JSON for a reader, or nil when there is none.
func evidenceJSON(r Row) json.RawMessage {
	if len(r.Evidence) == 0 {
		return nil
	}
	return r.Evidence
}
