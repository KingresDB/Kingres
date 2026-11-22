// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	return s.pass(ctx, term, DueSubmit)
}

// jobUpgrade is one pass of ledger.anchor.upgrade: the submitted and claimed records that are due.
func (s *Service) jobUpgrade(ctx context.Context, term scheduler.Term) error {
	return s.pass(ctx, term, DueUpgrade)
}

// pass lists the records of the set that are due (the store selects by state and time before it limits, for each target, P7A-01) and starts one
// worker for each target that has none running. The targets are independent of the end of the pass (P7A-04): the pass waits for the workers
// at most PassWait, and a target that is still being worked on (it hangs for its timeouts on each of its records) keeps its worker and is
// left alone by the next passes until it ends, while the other targets are worked on in every pass.
func (s *Service) pass(ctx context.Context, term scheduler.Term, set DueSet) error {
	if err := s.takeFatal(term); err != nil {
		return err
	}
	now := s.now()
	rows, err := s.cfg.Store.Due(ctx, now, set, BatchSize)
	if err != nil {
		return err
	}
	byTarget := map[int64][]Row{}
	var order []int64
	for _, r := range rows {
		if !(r.Record.Due(now) || r.State == anchor.StateClaimed && r.NextAttemptAt == nil) {
			continue
		}
		if _, ok := byTarget[r.TargetID]; !ok {
			order = append(order, r.TargetID)
		}
		byTarget[r.TargetID] = append(byTarget[r.TargetID], r)
	}
	done := make(chan struct{}, len(order))
	started := 0
	for _, id := range order {
		if !s.beginTarget(set, id) {
			continue // an earlier pass still works on this target
		}
		started++
		go func(id int64, list []Row) {
			defer func() { done <- struct{}{} }()
			defer s.endTarget(set, id)
			// a panic of a step is the error of the pass and never the end of the daemon (the scheduler protects its own goroutine, not this one)
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("a job of anchoring panicked", "target", id, "panic", fmt.Sprint(r))
				}
			}()
			select {
			case s.sem <- struct{}{}:
				defer func() { <-s.sem }()
			case <-ctx.Done():
				return
			}
			if err := s.workTarget(ctx, term, id, list); err != nil {
				s.setFatal(term, err)
			}
		}(id, byTarget[id])
	}
	wait := s.cfg.PassWait
	if wait <= 0 {
		wait = PassWait
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	for i := 0; i < started; i++ {
		select {
		case <-done:
		case <-timer.C:
			i = started
		case <-ctx.Done():
			i = started
		}
	}
	return s.takeFatal(term)
}

// beginTarget takes the worker of a target for a set of records (false when one is running); endTarget gives it back.
func (s *Service) beginTarget(set DueSet, id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := [2]int64{int64(set), id}
	if s.working[k] {
		return false
	}
	s.working[k] = true
	return true
}

func (s *Service) endTarget(set DueSet, id int64) {
	s.mu.Lock()
	delete(s.working, [2]int64{int64(set), id})
	s.mu.Unlock()
}

// setFatal keeps the error that ends a term (a deposed leader) for the pass of that term that returns it: a worker may end after its
// pass did. The error is kept with the term of the worker (P7R-03). The error of a later term replaces one of an earlier term that is still kept,
// and the error of an earlier term never replaces one of a later term: a stale error cannot hide the news of the term that is running (P7F-05).
func (s *Service) setFatal(term scheduler.Term, err error) {
	s.mu.Lock()
	if s.fatal == nil || term > s.fatalTerm {
		s.fatal, s.fatalTerm = err, term
	}
	s.mu.Unlock()
}

// takeFatal returns the kept error when it was found in the term of the pass that asks, and drops it in either case: the error of a
// worker of an earlier term says that that term is over, which is no news to a pass of a later one, and ending the later term for it
// would be a flap of the leadership for nothing.
func (s *Service) takeFatal(term scheduler.Term) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err, from := s.fatal, s.fatalTerm
	s.fatal = nil
	if from != term {
		return nil
	}
	return err
}

// workTarget works on the records of one target; it returns only an error that ends the pass (the term is over).
func (s *Service) workTarget(ctx context.Context, term scheduler.Term, id int64, list []Row) error {
	trow, err := s.cfg.Store.Target(ctx, id)
	if err != nil {
		return nil
	}
	// a server that became the leader after a rotation it missed does not anchor with the old credentials (P7F-01)
	var b *Built
	var berr error
	if gerr := s.ensureCurrent(ctx, term, trow); gerr != nil {
		berr = gerr
	} else {
		b, berr = s.build(trow)
	}
	if berr != nil && isNoCredentials(berr) {
		// a server that became the leader without the file asks the other holders for it before it gives up (P7A-03). When no holder
		// gave it and not every holder could be asked, the file may exist: the records wait and try again, they do not fail (P7R-08)
		switch s.fetchMissing(ctx, trow) {
		case fetchOK:
			b, berr = s.build(trow)
		case fetchUnreached:
			berr = credsUnreachedError{}
		case fetchNotInstalled:
			berr = credsUnreachedError{notInstalled: true}
		}
	}
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
					return s.apply(ctx, term, w, row.Version, trow.Revision)
				}
			}
		}
	}
	if berr != nil {
		if isCredsStale(berr) {
			var se credsStaleError
			if errors.As(berr, &se) && se.final {
				// the newer file cannot be taken and will not be: the records fail, an operator uses Retry after the fix (P7G-04)
				return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "target", "credentials_stale", "%s", errText(berr)), nil, trow.Revision)
			}
			return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassRetryable, row.Kind, "target", "credentials_stale", "%s", errText(berr)), nil, trow.Revision)
		}
		if isCredsUnreached(berr) {
			// the file may exist on a holder that does not answer: a retry with backoff, as for any target that cannot be reached (P7R-08)
			return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassRetryable, row.Kind, "target", "credentials_unreached", "%s", errText(berr)), nil, trow.Revision)
		}
		// The target cannot be built: the credentials or the configuration are wrong or missing on this server. Nothing a later call does
		// changes that, so the record fails at once, with the cause in its text and in the alert of 6.9, and an operator uses Retry after
		// the fix (P7A-03); a retry with backoff would say nothing for hours.
		return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "target", "target_unbuildable", "%s", errText(berr)), nil, trow.Revision)
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
			return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "payload", "bad_payload", "%s", errText(err)), b, trow.Revision)
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
		return s.fail(ctx, term, row, err, b, trow.Revision)
	}
	return s.succeed(ctx, term, row, rc, b, trow.Revision)
}

// succeed applies the receipt of a call that returned no error. A receipt whose evidence the library's own
// parser refuses is a fault of the target's code and fails the record: nothing is anchored on evidence that does
// not read back.
func (s *Service) succeed(ctx context.Context, term scheduler.Term, row Row, rc anchor.Receipt, b *Built, rev int64) error {
	now := s.now()
	canon, err := anchor.CanonicalEvidence(row.Kind, rc.Evidence)
	if err != nil {
		return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "receipt", "bad_evidence", "the evidence of the receipt does not read back: %s", errText(err)), b, rev)
	}
	rc.Evidence = canon
	// the evidence must be the evidence of this record: the second check behind the library's own, at the one place where a record becomes
	// anchored (P7A-05); a mismatch is a fault of the target's code and fails the record for good
	if b != nil && b.Kind.Binds != nil {
		if berr := b.Kind.Binds(row, canon); berr != nil {
			return s.fail(ctx, term, row, anchor.Errorf(anchor.ClassPermanent, row.Kind, "receipt", "evidence_mismatch", "the evidence of the receipt is not the evidence of this checkpoint: %s", errText(berr)), b, rev)
		}
	}
	rec, err := row.Record.ApplyReceipt(rc, now)
	if err != nil {
		s.log.Warn("a receipt was refused by the state machine, the job is stale", "record", row.ID, "state", string(row.State), "receipt", rc.State)
		return nil
	}
	w := row
	w.Record = rec
	w.ErrorCode, w.Alert = "", ""
	if (rec.State == anchor.StateSubmitted || rec.State == anchor.StateClaimed) && rec.NextAttemptAt == nil {
		// a receipt that names no time for the next check still gets one, so that a claimed record is never left without (duty 13)
		next := now.Add(anchor.UpgradeInterval(now.Sub(rec.CreatedAt)))
		w.NextAttemptAt = &next
	}
	if n := rc.ContradictedClaims + rc.ClaimsAside + rc.ClaimsDropped + rc.ClaimsRepeated; n > 0 {
		// a calendar whose claim a header contradicts, or a wrong source: visible in the record and in the log (duty 14)
		w.LastError = fmt.Sprintf("claims of a calendar were contradicted by the header source: %d contradicted, %d kept aside, %d dropped, %d repeated", rc.ContradictedClaims, rc.ClaimsAside, rc.ClaimsDropped, rc.ClaimsRepeated)
		s.log.Warn("claims of an OpenTimestamps calendar were contradicted", "record", row.ID, "contradicted", rc.ContradictedClaims, "aside", rc.ClaimsAside, "dropped", rc.ClaimsDropped, "repeated", rc.ClaimsRepeated)
	}
	return s.apply(ctx, term, w, row.Version, rev)
}

// fail applies the failure of a call to the record, as table 6.6 says, and keeps the alert it names with the row.
func (s *Service) fail(ctx context.Context, term scheduler.Term, row Row, err error, b *Built, rev int64) error {
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
	// what the calendars that answered gave is kept with the record (a partial submission leaves it in the error, duty 12)
	if b != nil && b.Kind.PriorEvidence != nil && len(row.Evidence) == 0 {
		if raw, ok := b.Kind.PriorEvidence(err); ok {
			w.Evidence = raw
		}
	}
	w.Alert = alertKind
	if alertKind == anchor.AlertNone && (rec.State == anchor.StateFailed || rec.State == anchor.StateExpired) {
		w.Alert = AlertFailed
	}
	return s.apply(ctx, term, w, row.Version, rev)
}

func (s *Service) apply(ctx context.Context, term scheduler.Term, w Row, expect, rev int64) error {
	if err := s.hook("before-apply"); err != nil {
		return err
	}
	if err := s.cfg.Store.Apply(ctx, term, w, expect, rev); err != nil {
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
