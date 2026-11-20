// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// BehindWarn and BehindCritical are the ages of an anchor at which ledgerAnchorBehind is raised (design 6.6, with
// correction 182 for the second): three times the anchoring interval and an hour, and twelve times and an hour.
func BehindWarn(interval time.Duration) time.Duration     { return 3*interval + time.Hour }
func BehindCritical(interval time.Duration) time.Duration { return 12*interval + time.Hour }

// View is a target with what the alerts and the API show of it.
type View struct {
	Row TargetRow
	// NewestAnchoredAt is the time of the newest anchor of the target (its evidence time when it has one).
	NewestAnchoredAt *time.Time
	// Lag is how long the oldest checkpoint that the target has not covered has waited, 0 when it covers them all.
	Lag time.Duration
	// State is none, ok, behind or failed (LedgerAnchorTarget.state).
	State string
	// Failures are the failed and expired records of the target that carry an alert, newest first.
	Failures []Row
}

// Snapshot is the state of anchoring at one moment, read once for the alerts and the list of targets.
type Snapshot struct {
	Settings Settings
	Views    []View
	Now      time.Time
	// CredProblems are the targets whose credentials file this server or a holder lacks (credsync.go), by target id.
	CredProblems map[int64]CredProblem
}

// snapshot reads the targets, the newest anchored record of each (target, log), the failures and the aggregates,
// and computes the lag and the state of every target.
func (s *Service) snapshot(ctx context.Context) (Snapshot, error) {
	var sn Snapshot
	sn.Now = s.now()
	var err error
	if sn.Settings, err = s.cfg.Store.Settings(ctx); err != nil {
		return sn, err
	}
	targets, err := s.cfg.Store.Targets(ctx)
	if err != nil {
		return sn, err
	}
	aggs, err := s.cfg.Store.Aggregates(ctx)
	if err != nil {
		return sn, err
	}
	anchored, err := s.cfg.Store.NewestAnchored(ctx)
	if err != nil {
		return sn, err
	}
	fails, err := s.cfg.Store.Failures(ctx)
	if err != nil {
		return sn, err
	}
	newest := map[[2]int64]Anchored{}
	for _, a := range anchored {
		newest[[2]int64{a.TargetID, a.LogID}] = a
	}
	byTarget := map[int64][]Row{}
	for _, f := range fails {
		byTarget[f.TargetID] = append(byTarget[f.TargetID], f)
	}
	warn := BehindWarn(sn.Settings.Interval)
	for _, t := range targets {
		v := View{Row: t}
		for _, a := range anchored {
			if a.TargetID != t.ID {
				continue
			}
			at := a.AnchoredAt
			if a.EvidenceTime != nil {
				at = *a.EvidenceTime
			}
			if v.NewestAnchoredAt == nil || at.After(*v.NewestAnchoredAt) {
				x := at.UTC()
				v.NewestAnchoredAt = &x
			}
		}
		if t.Enabled {
			for _, a := range aggs {
				have := newest[[2]int64{t.ID, a.LogID}]
				if a.Size <= have.Size {
					continue
				}
				first, ok, err := s.cfg.Store.FirstAfter(ctx, a.LogID, have.Size)
				if err != nil {
					return sn, err
				}
				if !ok {
					continue
				}
				if t.EnabledSince != nil && first.Before(*t.EnabledSince) {
					first = *t.EnabledSince
				}
				if lag := sn.Now.Sub(first); lag > v.Lag {
					v.Lag = lag
				}
			}
		}
		fl := byTarget[t.ID]
		sort.Slice(fl, func(i, j int) bool { return fl[i].ID > fl[j].ID })
		v.Failures = fl
		switch {
		case len(fl) > 0:
			v.State = "failed"
		case t.Enabled && v.Lag > warn:
			v.State = "behind"
		case v.NewestAnchoredAt != nil:
			v.State = "ok"
		default:
			v.State = "none"
		}
		sn.Views = append(sn.Views, v)
	}
	sn.CredProblems = s.credProblemList()
	return sn, nil
}

// wanted is an alert that should be open.
type wanted struct {
	Kind, Subject, Severity, Message string
}

func targetSubject(id int64) string { return "target:" + strconv.FormatInt(id, 10) }

// failureText is the text of an alert for a failed record, worded from the code and the text of the error and not
// from its class (correction 84): "refused" covers credentials and also clocks and the validity of a certificate.
func failureText(r Row) string {
	what := "failed"
	if r.State == anchor.StateExpired {
		what = "expired"
	}
	code := r.ErrorCode
	if code == "" {
		code = "error"
	}
	return fmt.Sprintf("Anchoring of the aggregate checkpoint of size %d of database %s at target %s %s (%s): %s Fix the cause, then use Retry.",
		r.Size, r.Database, r.TargetName, what, code, r.LastError)
}

// Wanted computes the alerts that should be open for a snapshot (design 6.9). It is a function of the state, so
// the same state always gives the same alerts, a restart or a leader change changes nothing, and an alert is
// raised, changed or cleared only when this set differs from the open alerts.
func Wanted(sn Snapshot, databases []string) []wanted {
	var out []wanted
	enabled := 0
	for _, v := range sn.Views {
		if v.Row.Enabled {
			enabled++
		}
	}
	if enabled == 0 {
		sev := "info"
		if sn.Settings.Required {
			sev = "warning" // anchor.required (6.9): nothing is blocked either way
		}
		for _, db := range databases {
			out = append(out, wanted{AlertNotAnchored, "database:" + db, sev, "Not anchored: the ledger of database " + db +
				" can be rewritten undetected by anyone who controls this cluster and its backups. Add an anchor target and enable it."})
		}
	}
	for _, v := range sn.Views {
		id := v.Row.ID
		if v.Row.Enabled && v.Lag > BehindWarn(sn.Settings.Interval) {
			sev := "warning"
			if v.Lag > BehindCritical(sn.Settings.Interval) {
				sev = "critical"
			}
			out = append(out, wanted{AlertBehind, targetSubject(id), sev,
				fmt.Sprintf("Target %s has not anchored the newest aggregate checkpoints for %s.", v.Row.Name, v.Lag.Round(time.Minute))})
		}
		var failed *Row
		for i := range v.Failures {
			f := v.Failures[i]
			switch f.Alert {
			case AlertConflict:
				out = append(out, wanted{AlertConflict, "database:" + f.Database + "|" + targetSubject(id), "critical", failureText(f)})
			case AlertFailed:
				if failed == nil {
					failed = &v.Failures[i]
				}
			}
		}
		if failed != nil {
			out = append(out, wanted{AlertFailed, targetSubject(id), "critical", failureText(*failed)})
		}
		if p, ok := sn.CredProblems[id]; ok {
			sev := "warning"
			if p.Critical {
				sev = "critical"
			}
			out = append(out, wanted{AlertFailed, targetSubject(id) + "|credentials", sev, p.Text})
		}
	}
	return out
}

// jobMonitor is one pass of ledger.anchor.monitor: the alerts of 6.9 are computed from the state and compared with
// the open ones; nothing is written unless an alert appears, changes its severity or goes away.
func (s *Service) jobMonitor(ctx context.Context, _ scheduler.Term) error {
	if s.cfg.Alert == nil || s.cfg.OpenAlerts == nil {
		return nil
	}
	sn, err := s.snapshot(ctx)
	if err == nil {
		ts := make([]TargetRow, 0, len(sn.Views))
		for _, v := range sn.Views {
			ts = append(ts, v.Row)
		}
		s.logDuties(ctx, ts)
	}
	if err != nil {
		return err
	}
	var dbs []string
	if s.cfg.Databases != nil {
		if dbs, err = s.cfg.Databases(ctx); err != nil {
			return err
		}
	}
	want := Wanted(sn, dbs)
	open, err := s.cfg.OpenAlerts(ctx)
	if err != nil {
		return err
	}
	have := map[[2]string]string{}
	for _, o := range open {
		have[[2]string{o.Kind, o.Subject}] = o.Severity
	}
	cctx := context.WithoutCancel(ctx)
	wantSet := map[[2]string]bool{}
	for _, w := range want {
		k := [2]string{w.Kind, w.Subject}
		wantSet[k] = true
		if sev, ok := have[k]; !ok || sev != w.Severity {
			s.cfg.Alert(cctx, w.Kind, w.Severity, w.Subject, w.Message, true)
		}
	}
	for _, o := range open {
		if !wantSet[[2]string{o.Kind, o.Subject}] {
			s.cfg.Alert(cctx, o.Kind, o.Severity, o.Subject, "", false)
		}
	}
	return nil
}
