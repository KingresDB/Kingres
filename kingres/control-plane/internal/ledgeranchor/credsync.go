// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// The credentials files follow the targets (P7A-03). The leader hands a file to the holders when a target is created or changed, once. A
// holder that was down then, a member that was added later, a host restored without its key directory and a deleted target whose holder was down
// are caught up by the job "ledger.anchor.credentials", which runs on the leader: it fetches the files this server lacks from the other holders,
// gives the holders the files they lack and takes away those of targets that are gone, and it keeps what it could not do as a problem that the
// monitor raises as an alert (ledgerAnchorFailed, subject "target:<id>|credentials") until it is done.

// HolderReport is what the other holders say about their credentials files.
type HolderReport struct {
	// Lacking lists, for each target of the question, the holders that have no file for it.
	Lacking map[int64][]string
	// Orphans lists, for each target that no longer exists, the holders that still have its file.
	Orphans map[int64][]string
	// Unreached are the holders that could not be asked.
	Unreached []string
}

// CredProblem is a target whose credentials are not where they must be.
type CredProblem struct {
	// Text says what is missing and where; Critical is true when this server (the leader) cannot anchor the target.
	Text     string
	Critical bool
}

func (s *Service) setCredProblem(id int64, p *CredProblem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p == nil {
		delete(s.credProblems, id)
		return
	}
	s.credProblems[id] = *p
}

func (s *Service) credProblemList() map[int64]CredProblem {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int64]CredProblem, len(s.credProblems))
	for k, v := range s.credProblems {
		out[k] = v
	}
	return out
}

// fetchMissing makes the credentials file of a target exist on this server: it asks the other holders for it. It reports whether the file is
// there afterwards, and, when it is not, whether that is because the holders could not all be asked (unreached: the file may exist, so the
// caller waits and tries again) or because every holder said it has none (P7R-08).
func (s *Service) fetchMissing(ctx context.Context, t TargetRow) (ok, unreached bool) {
	if s.cfg.Creds.Has(t.ID) {
		return true, false
	}
	if s.cfg.Fetch == nil {
		return false, false
	}
	data, err := s.cfg.Fetch(ctx, t.ID)
	if err != nil {
		// only a definite "every holder has none" is final: any other error (holders not reached, the list of holders not read) is not
		return false, !errors.Is(err, ErrNoCredentials)
	}
	defer clear(data)
	if err := s.cfg.Creds.Install(t.ID, t.Kind, data); err != nil {
		s.log.Warn("the credentials file that another holder gave could not be installed", "target", t.ID, "error", errText(err))
		return false, false
	}
	s.log.Info("the credentials file of an anchor target was fetched from another holder", "target", t.ID)
	return true, false
}

// jobCredentials is one pass of ledger.anchor.credentials.
func (s *Service) jobCredentials(ctx context.Context, _ scheduler.Term) error {
	targets, err := s.cfg.Store.Targets(ctx)
	if err != nil {
		return err
	}
	problems := map[int64]CredProblem{}
	known := map[int64]bool{}
	var ids []int64
	kinds := map[int64]string{}
	for _, t := range targets {
		known[t.ID] = true
		if !t.CredentialsSet {
			continue
		}
		if ok, unreached := s.fetchMissing(ctx, t); !ok {
			text := fmt.Sprintf("This server has no credentials file for target %s and no other holder could give it: it cannot anchor at this target. Enter the credentials of the target again.", t.Name)
			if unreached {
				text = fmt.Sprintf("This server has no credentials file for target %s, and not every other holder could be asked for it (a holder did not answer): it cannot anchor at this target until one gives it. It is asked again every minute.", t.Name)
			}
			problems[t.ID] = CredProblem{Critical: t.Enabled, Text: text}
			continue
		}
		ids = append(ids, t.ID)
		kinds[t.ID] = t.Kind
	}
	if s.cfg.Holders != nil {
		rep, herr := s.cfg.Holders(ctx, ids)
		if herr != nil {
			s.log.Warn("the holders of the credentials could not be asked", "error", errText(herr))
		}
		lacking := map[int64][]string{}
		for id, hosts := range rep.Lacking {
			if len(hosts) > 0 {
				lacking[id] = hosts
			}
		}
		for _, id := range sortedIDs(lacking) {
			t := targetByID(targets, id)
			raw, err := s.cfg.Creds.Raw(id)
			if err != nil {
				continue
			}
			derr := error(nil)
			if s.cfg.Distribute != nil {
				derr = s.cfg.Distribute(ctx, id, kinds[id], raw)
			}
			clear(raw)
			if derr != nil {
				problems[id] = CredProblem{Text: fmt.Sprintf("The credentials file of target %s could not be given to the holders %s: after a change of the leader one of them cannot anchor at this target.",
					t.Name, strings.Join(lacking[id], ", "))}
			}
		}
		// A file that a holder has for a target that this pass did not see is the file of a deleted target only when the target is still
		// absent after the holders were asked. CreateTarget commits the row first and writes the files after, so a file that the report
		// shows belongs to a row that was committed before the report was made: a list of the targets read after the report holds it
		// (P7R-10). A target created between the first read and the report is therefore never taken for an orphan; one that is deleted
		// after the report loses its file at the next pass at the latest.
		orphans := sortedIDs(rep.Orphans)
		if len(orphans) > 0 && s.cfg.Undistribute != nil {
			after, aerr := s.cfg.Store.Targets(ctx)
			if aerr != nil {
				s.log.Warn("the targets could not be read again, the files of deleted targets are left for the next pass", "error", errText(aerr))
				orphans = nil
			}
			for _, t := range after {
				known[t.ID] = true
			}
		}
		for _, id := range orphans {
			if known[id] || s.cfg.Undistribute == nil {
				continue
			}
			if err := s.cfg.Undistribute(ctx, id); err != nil {
				s.log.Warn("the credentials file of a deleted anchor target could not be taken from every holder", "target", id, "error", errText(err))
			}
		}
		if len(rep.Unreached) > 0 && herr == nil {
			s.log.Warn("holders of the credentials did not answer", "holders", strings.Join(rep.Unreached, ", "))
		}
	}
	s.mu.Lock()
	s.credProblems = problems
	s.mu.Unlock()
	return nil
}

func targetByID(ts []TargetRow, id int64) TargetRow {
	for _, t := range ts {
		if t.ID == id {
			return t
		}
	}
	return TargetRow{ID: id}
}

func sortedIDs[V any](m map[int64]V) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
