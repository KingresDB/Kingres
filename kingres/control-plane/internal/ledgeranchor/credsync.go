// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// The credentials files follow the targets (P7A-03). The leader hands a file to the holders when a target is created or changed, once. A
// holder that was down then, a member that was added later, a host restored without its key directory and a deleted target whose holder was down
// are caught up by the job "ledger.anchor.credentials", which runs on the leader: it fetches the files this server lacks from the other holders,
// gives the holders the files they lack or hold in an older version and takes away those of targets that are gone, and it keeps what it could not
// do as a problem that the monitor raises as an alert (ledgerAnchorFailed, subject "target:<id>|credentials") until it is done.
//
// A holder is current when the digest of what it holds equals the digest of the leader's file (P7F-01). When the digests differ the file that was
// written last is the right one: the holders say when their file was written, a holder whose file is newer than the leader's gives it to the
// leader (a server that became the leader after a rotation it missed), and the leader gives its own to the holders whose file is older or
// whose age is not known. A leader never gives away a file that a reachable holder has newer, and never anchors with one (the gate in
// workTarget).

// HolderCopy is what a holder says about the file it has for a target.
type HolderCopy struct {
	Host   string
	Digest string
	// Installed is when the holder wrote the file; zero when it did not say.
	Installed time.Time
}

// HolderReport is what the other holders say about their credentials files.
type HolderReport struct {
	// Lacking lists, for each target of the question, the holders that have no file for it.
	Lacking map[int64][]string
	// Orphans lists, for each target that no longer exists, the holders that still have its file.
	Orphans map[int64][]string
	// Copies lists, for each target of the question, what the holders that have a file say about it. A holder that has a file and says
	// nothing about it (an older server) is not in the list: nothing is known of its copy.
	Copies map[int64][]HolderCopy
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

// judgeCopies compares the copy of this server with those of the holders. newer is the holder whose copy differs and was written after this
// server's (the newest such); stale lists the holders whose copy differs and is not newer (older, or of an age that is not known on one side:
// the leader's copy is taken for the right one then).
func judgeCopies(own CopyInfo, others []HolderCopy) (newer *HolderCopy, stale []string) {
	for i := range others {
		o := others[i]
		if o.Digest == "" || o.Digest == own.Digest {
			continue
		}
		if !o.Installed.IsZero() && !own.Installed.IsZero() && o.Installed.After(own.Installed) {
			if newer == nil || o.Installed.After(newer.Installed) {
				newer = &o
			}
			continue
		}
		stale = append(stale, o.Host)
	}
	sort.Strings(stale)
	return newer, stale
}

// fetchOutcome is the result of fetchMissing.
type fetchOutcome int

const (
	// fetchOK: the file is there.
	fetchOK fetchOutcome = iota
	// fetchNone: every holder answered that it has none (or there is no other holder): final until the operator enters the credentials again.
	fetchNone
	// fetchUnreached: the holders could not all be asked, the file may exist (P7R-08).
	fetchUnreached
	// fetchNotInstalled: a holder gave a file and this server could not write it (P7F-02): a matter of this server, a retry later may work.
	fetchNotInstalled
)

// fetchMissing makes the credentials file of a target exist on this server: it asks the other holders for it. It reports whether the file is
// there afterwards, and, when it is not, why (see fetchOutcome).
func (s *Service) fetchMissing(ctx context.Context, t TargetRow) fetchOutcome {
	if s.cfg.Creds.Has(t.ID) {
		return fetchOK
	}
	if s.cfg.Fetch == nil {
		return fetchNone
	}
	data, err := s.cfg.Fetch(ctx, t.ID)
	if err != nil {
		// only a definite "every holder has none" is final: any other error (holders not reached, the list of holders not read) is not
		if errors.Is(err, ErrNoCredentials) {
			return fetchNone
		}
		return fetchUnreached
	}
	defer clear(data)
	if err := s.cfg.Creds.Install(t.ID, t.Kind, data); err != nil {
		s.log.Warn("the credentials file that another holder gave could not be installed", "target", t.ID, "error", errText(err))
		// a file that this server cannot open is the same as no file (the passphrase differs: only the operator can help); a disk that could not
		// be written is not, and the holders still have the file
		if errors.Is(err, ErrInstallWrite) {
			return fetchNotInstalled
		}
		return fetchNone
	}
	s.log.Info("the credentials file of an anchor target was fetched from another holder", "target", t.ID)
	return fetchOK
}

// takeNewer replaces the file of this server by the newer copy of a holder (P7F-01).
func (s *Service) takeNewer(ctx context.Context, t TargetRow, from HolderCopy) error {
	if s.cfg.FetchFrom == nil {
		return errors.New("this server cannot ask a holder for its file")
	}
	data, err := s.cfg.FetchFrom(ctx, from.Host, t.ID)
	if err != nil {
		return fmt.Errorf("the file could not be fetched from %s: %s", from.Host, errText(err))
	}
	defer clear(data)
	if err := s.cfg.Creds.Install(t.ID, t.Kind, data); err != nil {
		return fmt.Errorf("the file of %s could not be installed here: %s", from.Host, errText(err))
	}
	s.log.Warn("this server held an older credentials file than a holder; it was replaced by the newer one", "target", t.ID, "holder", from.Host)
	return nil
}

// CredGateTTL is how long a check that this server holds the current credentials of a target is believed (P7F-01).
const CredGateTTL = 30 * time.Second

type credGateKey struct {
	id   int64
	term scheduler.Term
}

// ensureCurrent is the gate before a target is built: a server that became the leader holds the file it was given, which may be one
// that a rotation it missed made stale. It asks the holders and, when one has a file that was written after this server's and differs, takes
// that file before anything is anchored with the old one. An error means the newer file is known to exist and could not be taken: the
// caller must not anchor. Holders that cannot be asked are no evidence either way (right after a failover the old leader is down), so
// they do not stop anything. The answer is kept for CredGateTTL.
func (s *Service) ensureCurrent(ctx context.Context, term scheduler.Term, t TargetRow) error {
	if s.cfg.Holders == nil || !t.CredentialsSet {
		return nil
	}
	key := credGateKey{t.ID, term}
	now := s.now()
	s.mu.Lock()
	at, ok := s.credGate[key]
	s.mu.Unlock()
	if ok && now.Sub(at) < CredGateTTL {
		return nil
	}
	own, err := s.cfg.Creds.Digest(t.ID)
	if err != nil {
		return nil // no file or one that cannot be opened: building the target says so
	}
	rep, herr := s.cfg.Holders(ctx, []int64{t.ID})
	if herr == nil {
		if newer, _ := judgeCopies(own, rep.Copies[t.ID]); newer != nil {
			if err := s.takeNewer(ctx, t, *newer); err != nil {
				return credsStaleError{cause: err}
			}
		}
	}
	s.mu.Lock()
	s.credGate[key] = now
	s.mu.Unlock()
	return nil
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
		if out := s.fetchMissing(ctx, t); out != fetchOK {
			var text string
			switch out {
			case fetchUnreached:
				text = fmt.Sprintf("This server has no credentials file for target %s, and not every other holder could be asked for it (a holder did not answer): it cannot anchor at this target until one gives it. It is asked again every minute.", t.Name)
			case fetchNotInstalled:
				text = fmt.Sprintf("This server has no credentials file for target %s: a holder gave it and it could not be written here (check the disk and the permissions of the key directory). It is asked again every minute.", t.Name)
			default:
				text = fmt.Sprintf("This server has no credentials file for target %s and no other holder could give it: it cannot anchor at this target. Enter the credentials of the target again.", t.Name)
			}
			problems[t.ID] = CredProblem{Critical: t.Enabled, Text: text}
			continue
		}
		ids = append(ids, t.ID)
		kinds[t.ID] = t.Kind
	}
	// the targets for which this pass gave the file to every holder that was behind
	delivered := map[int64]bool{}
	incomplete := false
	if s.cfg.Holders != nil {
		rep, herr := s.cfg.Holders(ctx, ids)
		if herr != nil {
			s.log.Warn("the holders of the credentials could not be asked", "error", errText(herr))
		}
		incomplete = herr != nil || len(rep.Unreached) > 0
		behind := map[int64][]string{}
		for id, hosts := range rep.Lacking {
			if len(hosts) > 0 {
				behind[id] = append(behind[id], hosts...)
			}
		}
		for _, id := range ids {
			t := targetByID(targets, id)
			own, derr := s.cfg.Creds.Digest(id)
			if derr != nil {
				continue
			}
			newer, stale := judgeCopies(own, rep.Copies[id])
			if newer != nil {
				// this server holds the older file (it became the leader after a rotation it missed): it takes the newer one and gives
				// nothing away before that
				if terr := s.takeNewer(ctx, t, *newer); terr != nil {
					problems[id] = CredProblem{Critical: t.Enabled, Text: fmt.Sprintf("This server holds an older credentials file for target %s than the holder %s and could not take the newer one (%s): it does not anchor at this target until it has. It is asked again every minute.", t.Name, newer.Host, errText(terr))}
					delete(behind, id)
					continue
				}
				own, derr = s.cfg.Creds.Digest(id)
				if derr != nil {
					continue
				}
				_, stale = judgeCopies(own, rep.Copies[id])
			}
			if len(stale) > 0 {
				behind[id] = append(behind[id], stale...)
			}
		}
		for _, id := range sortedIDs(behind) {
			t := targetByID(targets, id)
			hosts := behind[id]
			sort.Strings(hosts)
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
				problems[id] = CredProblem{Text: fmt.Sprintf("The credentials file of target %s could not be given to the holders %s (they have none or an older one): after a change of the leader one of them cannot anchor at this target correctly. It is given again every minute.",
					t.Name, strings.Join(hosts, ", "))}
			} else {
				delivered[id] = true
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
	// A problem of an earlier distribution (a holder that was down when the credentials changed) stays while a holder cannot be asked: the
	// pass has not seen that holder, so it does not know the holder is current, and "they are given again every minute" must stay true. It goes
	// when the report is complete and nothing is behind, or when this pass gave the file to every holder that was behind.
	if incomplete {
		for _, id := range ids {
			if _, now := problems[id]; now || delivered[id] {
				continue
			}
			if old, ok := s.credProblems[id]; ok && !old.Critical {
				problems[id] = old
			}
		}
	}
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

// NewHolderReport is an empty report.
func NewHolderReport() HolderReport {
	return HolderReport{Lacking: map[int64][]string{}, Orphans: map[int64][]string{}, Copies: map[int64][]HolderCopy{}}
}

// Add puts the answer of one holder to CallCredentialsList into the report, for the targets in ids: the targets it lacks, what it says of the
// files it has, and the files of targets that are not in ids.
func (r *HolderReport) Add(host string, out ListResponse, ids []int64) {
	info := map[int64]CopyInfo{}
	for _, c := range out.Copies {
		info[c.ID] = c
	}
	have := map[int64]bool{}
	for _, id := range out.IDs {
		have[id] = true
	}
	want := map[int64]bool{}
	for _, id := range ids {
		want[id] = true
		if !have[id] {
			r.Lacking[id] = append(r.Lacking[id], host)
		} else if c, ok := info[id]; ok {
			r.Copies[id] = append(r.Copies[id], HolderCopy{Host: host, Digest: c.Digest, Installed: c.Installed})
		}
	}
	for id := range have {
		if !want[id] {
			r.Orphans[id] = append(r.Orphans[id], host)
		}
	}
}
