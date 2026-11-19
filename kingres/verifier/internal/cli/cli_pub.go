// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// The commands of phase 7 step 7 (aggregate, bundle, monitor, trust follow, proof with a log leg)
// and step 18 (attestation) read several logs of one publication. They share what is in this
// file: the inputs, the opening of a log (its checkpoint, verified), and the way a problem is
// told: as an outcome, never by printing. Each command builds one result and ends in the three
// finish functions in one place (see the end of each run function), so that the exit sites of the
// package stay few and each one has a scenario (exit_sites_test.go).

type outKind int

const (
	outOK outKind = iota
	outInput
	outFailed
	outIncomplete
)

// outcome is how a step of a command ended: fine, an input error (status 2), a failed verification
// (status 1, the step named) or something that could not be checked (status 3).
type outcome struct {
	kind    outKind
	step    string
	err     error
	missing []string
}

func (o outcome) ok() bool { return o.kind == outOK }

func failedOut(step string, err error) outcome { return outcome{kind: outFailed, step: step, err: err} }
func inputOut(err error) outcome               { return outcome{kind: outInput, err: err} }
func incompleteOut(what string) outcome {
	return outcome{kind: outIncomplete, missing: []string{what}}
}

// pubOutcome turns an error from reading the publication into an outcome by the one rule of the
// verifier (L7-07): absent or unreachable is "could not check", unusable is a failed verification,
// anything else is an input problem.
func pubOutcome(err error) outcome {
	var pe *pubError
	if errors.As(err, &pe) {
		switch pe.kind {
		case pubAbsent:
			return incompleteOut("the publication does not contain " + pe.what + ": " + pe.err.Error())
		case pubUnreachable:
			return incompleteOut("the publication could not be reached for " + pe.what + ": " + pe.err.Error())
		}
		return failedOut("publication", pe.err)
	}
	return inputOut(err)
}

// tileOutcome is pubOutcome for an error of the tile reader.
func tileOutcome(err error, where string) outcome {
	switch {
	case errors.Is(err, tlog.ErrMissing):
		return incompleteOut("the publication does not contain a tile that is needed: " + err.Error() + " (under " + where + ")")
	case errors.Is(err, tlog.ErrBadTile):
		return failedOut("tiles", err)
	}
	return inputOut(err)
}

// pubInputs are the inputs of a command that reads several logs of one source.
type pubInputs struct {
	// trust is the file the signatures are verified under: the file the auditor pinned, narrowed by the
	// verified key events of the publication (applyKeyEvents). base is the file as pinned, which the events
	// themselves are verified under.
	trust   *trust.File
	base    *trust.File
	spec    string // --source as given
	src     pubsrc.Source
	now     time.Time
	notes   []string
	walk    *events.Walk // the events, read once and tolerantly (see events)
	walkErr error
	walked  bool
}

// loadPubInputs reads --trust and --now and opens the source. A problem is an input error.
func loadPubInputs(trustPath, source string, nowFlag nowArg) (*pubInputs, error) {
	return loadPubInputsOpt(trustPath, source, nowFlag, true)
}

// loadPubInputsOpt is loadPubInputs; with applyEvents false the key events are not read (trust follow reads them itself,
// strictly, under the file as pinned).
func loadPubInputsOpt(trustPath, source string, nowFlag nowArg, applyEvents bool) (*pubInputs, error) {
	if trustPath == "" {
		return nil, errors.New("missing --trust")
	}
	p := &pubInputs{spec: source}
	data, err := readArg(trustPath, trust.MaxFileSize)
	if err != nil {
		return nil, err
	}
	if p.trust, err = trust.Parse(data); err != nil {
		return nil, fmt.Errorf("trust file %s: %w", trustPath, err)
	}
	var fixed bool
	if p.now, fixed, err = parseNow(nowFlag); err != nil {
		return nil, err
	}
	if fixed {
		p.notes = append(p.notes, "time used for key validity windows: "+timeText(p.now)+", fixed by --now (the clock was not read)")
	} else {
		p.notes = append(p.notes, "time used for key validity windows: system clock "+timeText(p.now))
	}
	if source != "" {
		if p.src, err = pubsrc.Open(source, pubsrc.Options{}); err != nil {
			return nil, fmt.Errorf("--source: %w", err)
		}
	}
	p.base = p.trust
	if p.src != nil && applyEvents {
		// the key events narrow the file for every command that judges a signature
		p.walked = true
		p.walk, p.walkErr = readEvents(p.base, p.src, false)
		if p.walk != nil {
			var kn []string
			p.trust, kn = applyKeyEvents(p.base, p.walk)
			p.notes = append(p.notes, kn...)
			p.notes = append(p.notes, skippedNotes(p.walk)...)
		}
	}
	return p, nil
}

func (p *pubInputs) close() {
	if p.src != nil {
		_ = p.src.Close()
	}
}

// logIn returns the inputs of one log, reading from src (the source of the publication when nil).
// The source stays the caller's.
func (p *pubInputs) logIn(spec logSpec, src pubsrc.Source) *logInputs {
	if src == nil {
		src = p.src
	}
	return &logInputs{trust: p.trust, spec: spec, source: p.spec, dir: src, shared: true, now: p.now}
}

// openLog is a log of the publication whose checkpoint was read and verified.
type openedLog struct {
	in      *logInputs
	cp      *checkpoint.Checkpoint
	signers []signednote.Key
	note    []byte
	files   logFiles
	reader  *tlog.Reader
}

func (o *openedLog) rel() string { return o.in.spec.rel() }

// chainSpec and aggSpec build the specification of a log.
func chainSpec(id string, epoch uint64) logSpec {
	return logSpec{kind: "chain", id: id, epoch: epoch, hasEpoch: true}
}

func aggSpec(id string) logSpec { return logSpec{kind: "aggregate", id: id} }

// openLog reads the checkpoint of the log, verifies it as the checkpoint command does (a signature of a
// pinned key that may be used now, the origin, the size within the key's limit) and opens a reader
// of its tiles. notes are what the verification says about keys.
func (p *pubInputs) openLog(spec logSpec, src pubsrc.Source) (*openedLog, []string, outcome) {
	in := p.logIn(spec, src)
	note, err := in.readPublished("checkpoint", signednote.MaxNoteSize)
	if err != nil {
		return nil, nil, pubOutcome(err)
	}
	cp, signers, notes, verr := in.verifyNote(note)
	if verr != nil {
		var ie *inputErr
		if errors.As(verr, &ie) {
			return nil, notes, inputOut(ie.error)
		}
		step := "checkpoint"
		var ce *checkpoint.Error
		if errors.As(verr, &ce) {
			step = ce.Step
		}
		return nil, notes, failedOut(step, fmt.Errorf("%s: %w", spec.rel(), verr))
	}
	d, err := in.openSource()
	if err != nil {
		return nil, notes, inputOut(err)
	}
	files := logFiles{d, spec.rel()}
	rd, err := tlog.NewReader(tlog.FSSource{Files: files}, cp.Size)
	if err != nil {
		return nil, notes, inputOut(err)
	}
	return &openedLog{in: in, cp: cp, signers: signers, note: note, files: files, reader: rd}, notes, outcome{}
}

// events returns the events of the publication, read once, tolerantly: an event that no key
// authorised at its place signed is skipped and listed, a malformed one ends the walk (and is
// kept as the error). A publication with no events gives a walk of Count 0.
func (p *pubInputs) events() (*events.Walk, error) {
	if !p.walked {
		p.walked = true
		p.walk, p.walkErr = readEvents(p.base, p.src, false)
	}
	return p.walk, p.walkErr
}

// verifiedEntry reads entry idx of a log of the given size, from the entry bundles of the source,
// and proves that it is the leaf at idx of the tree with this root: the leaf hash of the entry
// leads to the root through the inclusion path RFC 9162 2.1.3.2 computed from the tiles. An entry
// of another size or content than the signed tree commits to fails here.
func verifiedEntry(files tlog.FileReader, rd *tlog.Reader, size, idx uint64, maxEntry int, root merkle.Hash) ([]byte, outcome) {
	es, err := tlog.EntriesAt(files, size, idx/tlog.TileWidth, maxEntry)
	if err != nil {
		return nil, tileOutcome(err, "tile/entries")
	}
	if int(idx%tlog.TileWidth) >= len(es) {
		return nil, failedOut("entries", fmt.Errorf("the entry bundle has %d entries and the entry %d is not in it", len(es), idx))
	}
	e := es[idx%tlog.TileWidth]
	path, err := rd.InclusionProofIn(idx, size)
	if err != nil {
		return nil, tileOutcome(err, "tile")
	}
	if err := merkle.VerifyPath(merkle.LeafHash(e), idx, size, path, root); err != nil {
		return nil, failedOut("inclusion", fmt.Errorf("the entry %d of the entry bundle is not the leaf %d of the signed tree of size %d: %w", idx, idx, size, err))
	}
	return e, outcome{}
}

// entriesAt reads entry bundle n of an opened log, at the size of its checkpoint.
func entriesAt(ol *openedLog, n uint64, maxEntry int) ([][]byte, error) {
	return tlog.EntriesAt(ol.files, ol.cp.Size, n, maxEntry)
}

// newReader opens a tile reader for the tree of an opened log's checkpoint.
func newReader(ol *openedLog) (*tlog.Reader, error) {
	return tlog.NewReader(tlog.FSSource{Files: ol.files}, ol.cp.Size)
}

// absent reports whether err says that a file is not there.
func absent(err error) bool { return errors.Is(err, fs.ErrNotExist) }

// eventsOutcome is the outcome of an error of reading or verifying the events: a source that cannot
// be reached is "could not check", a redirect is a problem of the address the auditor gave, anything
// else is a failed verification of the events.
func eventsOutcome(err error, what string) outcome {
	switch {
	case errors.Is(err, pubsrc.ErrUnreachable):
		return incompleteOut(what + ": " + err.Error())
	case errors.Is(err, pubsrc.ErrRedirect), errors.Is(err, pubsrc.ErrNoListing):
		return inputOut(err)
	}
	return failedOut("events", err)
}
