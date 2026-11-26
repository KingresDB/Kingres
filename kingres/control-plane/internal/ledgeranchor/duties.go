// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgeranchor

import (
	"context"
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// What the library leaves to the caller and that is only a warning (duties 9 and 10 of internal/anchor, correction 188): the retention of an
// anchored WORM object ends one day and nothing renews it; a version whose evidence has no attempt (`attempt` null: the store dropped the
// user metadata) or was found and not written by this cluster (`found`) is weaker evidence; and the clock of this server must agree with the
// database's, because the time of the store is judged against it (store_time_skew, store_time_early) and an NTP step costs a checkpoint its
// key. The warnings are checks of the Test button and a line of the log once an hour; none of them changes a record.
const (
	// RetainWarn is how long before the end of a retention the warning starts.
	RetainWarn = 30 * 24 * time.Hour
	// ClockWarn and ClockFail are the differences between this server and the database that are a warning and a failure.
	ClockWarn = 30 * time.Second
	ClockFail = 5 * time.Minute
	dutyLog   = time.Hour
)

// evidenceChecks are the checks about the anchored evidence of one target (WORM only).
func (s *Service) evidenceChecks(ctx context.Context, t TargetRow) []anchor.Check {
	if t.Kind != anchor.KindWORM {
		return nil
	}
	rows, err := s.cfg.Store.Records(ctx, Filter{TargetID: t.ID, State: "anchored", Limit: 500})
	if err != nil {
		return nil
	}
	now := s.now()
	var first *time.Time
	var ends, soon, noAttempt, found int
	for _, r := range rows {
		full, err := s.cfg.Store.Record(ctx, r.ID)
		if err != nil {
			continue
		}
		ev, err := anchor.ParseWORMEvidence(full.Evidence)
		if err != nil {
			continue
		}
		if ru, err := anchor.ParseTime(ev.RetainUntil); err == nil {
			if now.After(ru) {
				ends++
			} else if ru.Sub(now) <= RetainWarn {
				soon++
			}
			if first == nil || ru.Before(*first) {
				x := ru
				first = &x
			}
		}
		if ev.Attempt == nil {
			noAttempt++
		}
		if ev.Found {
			found++
		}
	}
	var out []anchor.Check
	add := func(name, status, f string, a ...any) {
		out = append(out, anchor.Check{Name: name, Status: status, Text: fmt.Sprintf(f, a...)})
	}
	switch {
	case ends > 0:
		add("retention-ended", anchor.CheckFailed, "%d anchored object(s) are no longer protected: their retention has ended and nothing renews it; extend it with the tools of the store", ends)
	case soon > 0:
		add("retention-ends", anchor.CheckWarning, "%d anchored object(s) have a retention that ends within 30 days (the first on %s): nothing renews it; extend it with the tools of the store", soon, anchor.FormatTime(*first))
	case first != nil:
		add("retention", anchor.CheckOK, "the first retention among the anchored objects ends on %s", anchor.FormatTime(*first))
	}
	if noAttempt > 0 {
		add("evidence-attempt", anchor.CheckWarning, "%d anchored record(s) have no attempt in their evidence (the store dropped the user metadata): their time is bounded by the start of the first submission only", noAttempt)
	}
	if found > 0 {
		add("evidence-found", anchor.CheckWarning, "%d anchored record(s) were found in the store and not written by this cluster: another principal wrote a version of the same bytes", found)
	}
	return out
}

// clockCheck compares the clock of this server with the database's.
func (s *Service) clockCheck(ctx context.Context) anchor.Check {
	db, err := s.cfg.Store.DBTime(ctx)
	if err != nil {
		return anchor.Check{Name: "server-clock", Status: anchor.CheckWarning, Text: "the clock of the database could not be read"}
	}
	d := s.now().Sub(db)
	if d < 0 {
		d = -d
	}
	switch {
	case d > ClockFail:
		return anchor.Check{Name: "server-clock", Status: anchor.CheckFailed, Text: fmt.Sprintf("the clock of this server differs from the database's by %s: the time of the store is judged against it, and every submit would meet store_time_skew or store_time_early; correct the clock", d.Round(time.Second))}
	case d > ClockWarn:
		return anchor.Check{Name: "server-clock", Status: anchor.CheckWarning, Text: fmt.Sprintf("the clock of this server differs from the database's by %s; the store's time is checked against this server's clock at every submit (store_time_*)", d.Round(time.Second))}
	}
	return anchor.Check{Name: "server-clock", Status: anchor.CheckOK, Text: fmt.Sprintf("the clock of this server is within %s of the database's; the time of the store is checked against it at every submit", d.Round(time.Second))}
}

// logDuties writes the warnings of every enabled target to the log, once an hour for each.
func (s *Service) logDuties(ctx context.Context, targets []TargetRow) {
	now := s.now()
	for _, t := range targets {
		if !t.Enabled {
			continue
		}
		s.mu.Lock()
		last := s.dutyLogged[t.ID]
		due := now.Sub(last) >= dutyLog
		if due {
			if s.dutyLogged == nil {
				s.dutyLogged = map[int64]time.Time{}
			}
			s.dutyLogged[t.ID] = now
		}
		s.mu.Unlock()
		if !due {
			continue
		}
		for _, c := range s.evidenceChecks(ctx, t) {
			if c.Status != anchor.CheckOK {
				s.log.Warn("anchor evidence", "target", t.ID, "check", c.Name, "status", c.Status, "text", c.Text)
			}
		}
	}
	if c := s.clockCheck(ctx); c.Status != anchor.CheckOK {
		s.mu.Lock()
		last := s.dutyLogged[0]
		due := now.Sub(last) >= dutyLog
		if due {
			if s.dutyLogged == nil {
				s.dutyLogged = map[int64]time.Time{}
			}
			s.dutyLogged[0] = now
		}
		s.mu.Unlock()
		if due {
			s.log.Warn("anchoring: clock", "check", c.Name, "status", c.Status, "text", c.Text)
		}
	}
}
