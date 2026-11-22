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
// Which copy is the newer one is decided by the rotation generation, a counter that is inside the file (P7G-02): every change of the credentials
// writes a file with a generation one above the highest that the leader and the holders it could ask hold. A holder is current when its generation
// and the digest of its file equal the leader's. A holder with a higher generation gives its file to the leader (a server that became the leader
// after a rotation it missed); a holder with a lower one is given the leader's file. Two files of the same generation that differ (two rotations
// at once) are given to nobody: both stay, and the alert asks the operator to enter the credentials again, which makes a generation above both.
// No time decides anything: not the time of the file system, which a restore from a backup resets, and not a time that a holder reports. A server
// also refuses to write a file of a lower generation over a higher one (CredStore.Install), whoever sends it. A leader never anchors with a file
// that a reachable holder has newer (the gate in workTarget).

// HolderCopy is what a holder says about the file it has for a target.
type HolderCopy struct {
	Host string
	// Generation is the rotation counter of the file; Digest the keyed digest of its content (empty when the file has no stamp).
	Generation uint64
	Digest     string
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

// judgeCopies compares the copy of this server with those of the holders. newer is the holder with the highest generation above this server's
// (the first of them by name when two share it); stale lists the holders with a lower generation; conflict lists the holders with the same
// generation and another digest. A holder with the same generation and the same digest is current, and so is one of generation 0 when either
// side has no stamp (files of earlier versions cannot be told apart).
func judgeCopies(own CopyInfo, others []HolderCopy) (newer *HolderCopy, stale, conflict []string) {
	for i := range others {
		o := others[i]
		switch {
		case o.Generation > own.Generation:
			if newer == nil || o.Generation > newer.Generation || o.Generation == newer.Generation && o.Host < newer.Host {
				newer = &o
			}
		case o.Generation < own.Generation:
			stale = append(stale, o.Host)
		case sameDigest(o.Digest, own.Digest) || o.Digest == "" || own.Digest == "":
		default:
			conflict = append(conflict, o.Host)
		}
	}
	sort.Strings(stale)
	sort.Strings(conflict)
	return newer, stale, conflict
}

// generationFloor is the highest generation that a holder reports for a target: a leader that rotates credentials writes a generation above it.
// Holders that cannot be asked are not in it (a holder that has a higher one refuses the file, which is an alert).
func (s *Service) generationFloor(ctx context.Context, id int64) uint64 {
	if s.cfg.Holders == nil {
		return 0
	}
	rep, err := s.cfg.Holders(ctx, []int64{id})
	if err != nil {
		return 0
	}
	var floor uint64
	for _, c := range rep.Copies[id] {
		if c.Generation > floor {
			floor = c.Generation
		}
	}
	return floor
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
	// fetchNotInstalled: a holder gave a file and this server could not store it (P7F-02, P7G-04: the disk, or the passphrase file that could
	// not be read): a matter of this server, a retry later may work.
	fetchNotInstalled
	// fetchUnopenable: a holder gave a file that this server cannot open (it is sealed under another passphrase, or it is not a credentials
	// file): the operator must help.
	fetchUnopenable
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
		// a disk that could not be written or a passphrase file that could not be read is a matter of this server and the holders still have
		// the file; a file that does not open here is not
		if errors.Is(err, ErrInstallWrite) {
			return fetchNotInstalled
		}
		return fetchUnopenable
	}
	s.log.Info("the credentials file of an anchor target was fetched from another holder", "target", t.ID)
	return fetchOK
}

// takeKind says why a newer file could not be taken.
type takeKind int

const (
	// takeFetch: the holder did not give the file.
	takeFetch takeKind = iota
	// takeLocal: this server could not store it (disk, passphrase file).
	takeLocal
	// takeUnopenable: the file does not open here: only the operator can help.
	takeUnopenable
	// takeOlder: the file the holder gave is older than the one here, or another of the same generation, whatever the holder said.
	takeOlder
)

type takeError struct {
	kind  takeKind
	cause error
}

func (e takeError) Error() string { return e.cause.Error() }

// takeNewer replaces the file of this server by the newer copy of a holder (P7F-01). The holder's file must be newer in fact: Install refuses a
// file of a lower generation, so a holder that said it has a newer file and gives an older one is refused (P7G-02).
func (s *Service) takeNewer(ctx context.Context, t TargetRow, from HolderCopy) error {
	if s.cfg.FetchFrom == nil {
		return takeError{takeFetch, errors.New("this server cannot ask a holder for its file")}
	}
	data, err := s.cfg.FetchFrom(ctx, from.Host, t.ID)
	if err != nil {
		return takeError{takeFetch, fmt.Errorf("the file could not be fetched from %s: %s", from.Host, errText(err))}
	}
	defer clear(data)
	if err := s.cfg.Creds.Install(t.ID, t.Kind, data); err != nil {
		switch {
		case errors.Is(err, ErrOlderGeneration):
			return takeError{takeOlder, fmt.Errorf("the holder %s said it has generation %d and gave a file that is older than the one of this server or another of the same generation", from.Host, from.Generation)}
		case errors.Is(err, ErrInstallWrite):
			return takeError{takeLocal, fmt.Errorf("the file of %s could not be stored here: %s", from.Host, errText(err))}
		}
		return takeError{takeUnopenable, fmt.Errorf("the file of %s cannot be opened with the passphrase file of this server: %s", from.Host, errText(err))}
	}
	s.log.Warn("this server held an older credentials file than a holder; it was replaced by the newer one", "target", t.ID, "holder", from.Host, "generation", from.Generation)
	return nil
}

// CredGateTTL is how long a check that this server holds the current credentials of a target is believed (P7F-01).
const CredGateTTL = 30 * time.Second

// CredStaleLimit is how long a newer file that cannot be taken is waited for: after it the records of the target fail for good, with an alert, and
// an operator uses Retry (P7G-04). A file that does not open here does not wait at all.
const CredStaleLimit = 30 * time.Minute

type credGateKey struct {
	id   int64
	term scheduler.Term
}

// staleFailure notes that the newer file of a target could not be taken and says whether the waiting is over. final is true for a file that
// cannot be opened here and for a failure that lasted CredStaleLimit.
func (s *Service) staleFailure(id int64, kind takeKind) (final bool) {
	if kind == takeUnopenable {
		return true
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	since, ok := s.credStaleSince[id]
	if !ok {
		s.credStaleSince[id] = now
		return false
	}
	return now.Sub(since) >= CredStaleLimit
}

func (s *Service) staleCleared(id int64) {
	s.mu.Lock()
	delete(s.credStaleSince, id)
	s.mu.Unlock()
}

// ensureCurrent is the gate before a target is built: a server that became the leader holds the file it was given, which may be one
// that a rotation it missed made stale. It asks the holders and, when one has a file of a higher generation, takes that file before anything is
// anchored with the old one. An error means the newer file is known to exist and could not be taken: the caller must not anchor. Holders that
// cannot be asked are no evidence either way (right after a failover the old leader is down), so they do not stop anything. The answer is kept
// for CredGateTTL.
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
		if newer, _, _ := judgeCopies(own, rep.Copies[t.ID]); newer != nil {
			if err := s.takeNewer(ctx, t, *newer); err != nil {
				var te takeError
				errors.As(err, &te)
				return credsStaleError{cause: err, final: s.staleFailure(t.ID, te.kind)}
			}
			s.staleCleared(t.ID)
		}
	}
	s.mu.Lock()
	// the entries of other terms are of no use any more
	for k := range s.credGate {
		if k.term != term {
			delete(s.credGate, k)
		}
	}
	s.credGate[key] = now
	s.mu.Unlock()
	return nil
}

// jobCredentials is one pass of ledger.anchor.credentials.
func (s *Service) jobCredentials(ctx context.Context, term scheduler.Term) error {
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
				text = fmt.Sprintf("This server has no credentials file for target %s: a holder gave it and it could not be stored here (check the disk, the permissions of the key directory and the passphrase file). It is asked again every minute.", t.Name)
			case fetchUnopenable:
				text = fmt.Sprintf("This server has no credentials file for target %s: a holder gave one that cannot be opened with the passphrase file of this server (another passphrase, or a plain server and a sealed file). Give the servers the same passphrase file or enter the credentials of the target again.", t.Name)
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
			newer, stale, conflict := judgeCopies(own, rep.Copies[id])
			if newer != nil {
				// this server holds the older file (it became the leader after a rotation it missed): it takes the newer one and gives
				// nothing away before that
				if terr := s.takeNewer(ctx, t, *newer); terr != nil {
					var te takeError
					errors.As(terr, &te)
					final := s.staleFailure(id, te.kind)
					text := fmt.Sprintf("This server holds an older credentials file for target %s than the holder %s (generation %d against %d) and could not take the newer one (%s): it does not anchor at this target until it has. It is asked again every minute.", t.Name, newer.Host, newer.Generation, own.Generation, errText(terr))
					if final {
						text = fmt.Sprintf("This server holds an older credentials file for target %s than the holder %s (generation %d against %d) and could not take the newer one (%s): the records of the target failed and are not tried again until Retry. Fix the cause or enter the credentials of the target again.", t.Name, newer.Host, newer.Generation, own.Generation, errText(terr))
					}
					problems[id] = CredProblem{Critical: t.Enabled, Text: text}
					delete(behind, id)
					continue
				}
				s.staleCleared(id)
				own, derr = s.cfg.Creds.Digest(id)
				if derr != nil {
					continue
				}
				_, stale, conflict = judgeCopies(own, rep.Copies[id])
			}
			if len(stale) > 0 {
				behind[id] = append(behind[id], stale...)
			}
			if len(conflict) > 0 {
				// two rotations of one generation: neither is given over the other
				problems[id] = CredProblem{Text: fmt.Sprintf("The holders %s hold another credentials file for target %s than this server, of the same generation %d (two changes of the credentials were made at once): neither is given to the other. Enter the credentials of the target again: that makes a newer generation that every holder takes.", strings.Join(conflict, ", "), t.Name, own.Generation)}
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
			} else if _, bad := problems[id]; !bad {
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
	// what is kept for a target that is gone, or for a term that is over, is dropped
	for k := range s.credGate {
		if !known[k.id] || k.term != term {
			delete(s.credGate, k)
		}
	}
	for id := range s.credStaleSince {
		if !known[id] {
			delete(s.credStaleSince, id)
		}
	}
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
			r.Copies[id] = append(r.Copies[id], HolderCopy{Host: host, Generation: c.Generation, Digest: c.Digest})
		}
	}
	for id := range have {
		if !want[id] {
			r.Orphans[id] = append(r.Orphans[id], host)
		}
	}
}
