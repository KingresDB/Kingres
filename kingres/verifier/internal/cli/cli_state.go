// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// The state file (correction 15 and 17 of the design) is the auditor's record
// of the last checkpoint this tool proved. It holds the signed note itself, so
// that it is checked again with the trust file at every use and so that it is
// evidence a third party can check, and a few facts about when and under which
// key it was judged. The facts are for the reader; no decision depends on them
// (a person who can write the file can also delete it, and a note still has to
// carry a valid signature of a key of the trust file that is not revoked).

const maxStateFile = 3 << 20

type stateRecord struct {
	Format     int      `json:"format"`
	Log        string   `json:"log"`
	Size       uint64   `json:"size"`
	Root       string   `json:"root"`
	VerifiedAt string   `json:"verified_at"`
	VerifiedBy []string `json:"verified_by"`
	Note       string   `json:"note"`
}

func encodeState(cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, now time.Time) ([]byte, error) {
	rec := stateRecord{
		Format: 1, Log: cp.Origin, Size: cp.Size, Root: format.EncodeHex(cp.Root[:]),
		VerifiedAt: now.Format(time.RFC3339Nano), VerifiedBy: signerPrints(signers), Note: string(note),
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// decodeHeld reads a file that holds a checkpoint the auditor kept: a state
// file or a plain signed note. For a state file it returns the record too.
func decodeHeld(data []byte) ([]byte, *stateRecord, error) {
	if !bytes.HasPrefix(data, []byte("{")) {
		if len(data) > signednote.MaxNoteSize {
			return nil, nil, fmt.Errorf("note %s: %d bytes, expected at most %d", safefile.ErrTooLarge, len(data), signednote.MaxNoteSize)
		}
		return data, nil, nil
	}
	var rec stateRecord
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rec); err != nil {
		return nil, nil, fmt.Errorf("not a state file: %w", err)
	}
	if dec.More() {
		return nil, nil, errors.New("not a state file: text after the record")
	}
	if rec.Format != 1 {
		return nil, nil, fmt.Errorf("state file format %d is not known, expected 1", rec.Format)
	}
	return []byte(rec.Note), &rec, nil
}

// rebaseAdvice is what the auditor is told when a held checkpoint cannot be
// used. It never says to delete the file: with no state a rollback is not
// noticed. It says how to start a new record that keeps the size.
func rebaseAdvice(what string, claimedSize uint64, haveSize bool) string {
	size := "the size you believe the log had when you last verified it"
	if haveSize {
		size += " (the note claims " + fmt.Sprint(claimedSize) + ", which nothing here authenticates)"
	}
	if what == "--old" {
		return "Use a note signed by a key you trust, or give SIZE:ROOT from another source you trust."
	}
	return "The file was not changed and must not be deleted: keep it, it is what the log said to you. " +
		"Deleting it would give up the protection against rollback and forks. To go on without giving it up, " +
		"obtain a checkpoint of this log signed by a key you trust, from a source you trust, run " +
		"checkpoint --state NEWFILE --min-size N with N = " + size + ", and use NEWFILE from then on."
}

// verifyHeld verifies a note the auditor holds (the state, or --old FILE) as a
// checkpoint of the log. It is a statement about the past, so it is judged
// differently from a checkpoint under test:
//
//   - the signature must verify against a key of the trust file, and no
//     signature of a key of the file may be wrong;
//   - a revoked key's signature is not accepted at any time;
//   - a key limited by max_tree_size counts only up to its size;
//   - the validity window (not_before, not_after) is not applied: the window
//     says when a key may sign new checkpoints, and a note the auditor kept
//     was judged when it was kept. A key retired by not_after must not make the
//     auditor discard what they hold.
//
// Every failure is an input error (status 2) in both commands: the thing the
// auditor holds cannot serve as their anchor. The error says what to do and
// never says to delete the file.
func (in *logInputs) verifyHeld(note []byte, what string) (*checkpoint.Checkpoint, []signednote.Key, []string, error) {
	origin := in.spec.origin(in.trust.OriginPrefix)
	keys, err := in.trust.AllKeys(origin)
	if err != nil {
		return nil, nil, nil, &inputErr{err}
	}
	cp, signers, err := checkpoint.Verify(note, keys, origin)
	if err != nil {
		var ce *checkpoint.Error
		if errors.As(err, &ce) && ce.Step == checkpoint.StepSignature {
			return nil, nil, nil, &inputErr{fmt.Errorf("%s has no signature of a key of the trust file, or a wrong one (%w): the trust file or the log is not the one this note was made with. %s", what, err, rebaseAdvice(what, 0, false))}
		}
		return nil, nil, nil, &inputErr{fmt.Errorf("%s is not a checkpoint of this log: %w", what, err)}
	}
	var accepted []signednote.Key
	var notes, why []string
	for _, s := range signers {
		k, _ := in.trust.KeyFor(s.Public)
		id := fmt.Sprintf("key %q (%s)", untrusted(k.Name), k.Fingerprint[:16])
		switch {
		case k.Revoked:
			why = append(why, id+" is revoked in the trust file, so its signatures are not evidence of what the log was")
		case !in.trust.LimitFor(s.Public, cp.Size):
			why = append(why, fmt.Sprintf("%s counts only for tree sizes up to %d and the note has size %d", id, *k.MaxTreeSize, cp.Size))
		default:
			accepted = append(accepted, s)
			if st := k.StandingAt(in.now); st != trust.Usable {
				notes = append(notes, fmt.Sprintf("%s is %s now: accepted for %s because it is a checkpoint you hold, judged when you kept it (the window of a key applies to new checkpoints)", id, st, what))
			}
		}
	}
	if len(accepted) == 0 {
		return nil, nil, nil, &inputErr{fmt.Errorf("%s cannot be used as your earlier checkpoint: %s. %s", what, strings.Join(why, "; "), rebaseAdvice(what, cp.Size, true))}
	}
	for _, w := range why {
		notes = append(notes, "also on the note: "+w+"; not counted")
	}
	return cp, accepted, notes, nil
}

// ignoredSignatures names the signatures on a note under test that belong to
// a key of the trust file that was not counted (revoked, outside its window,
// above its size limit), so that a note signed by a revoked and a valid key does
// not look the same as one signed by the valid key alone.
func (in *logInputs) ignoredSignatures(note []byte, counted []signednote.Key) []string {
	n, err := signednote.Parse(note)
	if err != nil {
		return nil
	}
	origin := in.spec.origin(in.trust.OriginPrefix)
	var out []string
	for _, k := range in.trust.Keys {
		sk, err := signednote.NewKey(origin, k.Public)
		if err != nil {
			continue
		}
		on := false
		for _, s := range n.Signatures {
			if s.Name == sk.Name && s.ID == sk.ID() {
				on = true
			}
		}
		if !on {
			continue
		}
		was := false
		for _, c := range counted {
			if bytes.Equal(c.Public, k.Public) {
				was = true
			}
		}
		if !was {
			out = append(out, fmt.Sprintf("the note also carries a signature of key %q (%s), which was not counted: %s", untrusted(k.Name), k.Fingerprint[:16], k.Why(in.now)))
		}
	}
	return out
}

// proveExtension reads the tiles of the source on the proof path and checks
// that the tree of newCP extends the tree of oldCP. A tile the source lacks
// wraps tlog.ErrMissing, a damaged one tlog.ErrBadTile, a path that leads to the
// other history merkle.ErrOldRoot (a fork) and one that does not lead to the
// signed new root merkle.ErrNewRoot (the tiles do not belong to the
// checkpoint). Any other error is an input problem (*inputErr).
func (in *logInputs) proveExtension(oldCP, newCP *checkpoint.Checkpoint) (tiles, hashes int, err error) {
	d, err := in.openSource()
	if err != nil {
		return 0, 0, &inputErr{err}
	}
	reader, err := tlog.NewReader(tlog.FSSource{Files: logFiles{d, in.spec.rel()}}, newCP.Size)
	if err != nil {
		return 0, 0, &inputErr{err}
	}
	path, err := reader.ConsistencyProof(oldCP.Size)
	if err != nil {
		if errors.Is(err, tlog.ErrMissing) || errors.Is(err, tlog.ErrBadTile) {
			return reader.TilesRead(), 0, err
		}
		return 0, 0, &inputErr{err}
	}
	return reader.TilesRead(), len(path), merkle.VerifyConsistency(oldCP.Size, newCP.Size, oldCP.Root, newCP.Root, path)
}

// applyState is `checkpoint --state`. Invariant (correction 17): the state
// file only ever moves from a checkpoint to one that this run proved, for the
// very bytes it verified the signature of, to extend it. Status 0 with --state
// means exactly that (or that the state was empty and is now recorded, or that
// nothing changed). It returns done=true with the exit status when the command
// ends here.
func (e *env) applyState(r *result, in *logInputs, cp *checkpoint.Checkpoint, note []byte, signers []signednote.Key, statePath string, asJSON bool) (int, bool) {
	fail := func(err error) (int, bool) {
		return e.inputError(r.cmd, asJSON, err), true
	}
	path, err := statefile.Resolve(statePath)
	if err != nil {
		return fail(fmt.Errorf("--state %s: %w", statePath, err))
	}
	lock, err := statefile.Acquire(path, statefile.LockWait)
	if err != nil {
		if errors.Is(err, statefile.ErrLockTimeout) {
			return fail(fmt.Errorf("--state %s: another run has held the lock on the state file for %s: nothing was changed; wait for it, or if no other kingres-verify is running, look for a stuck process", statePath, statefile.LockWait))
		}
		return fail(fmt.Errorf("--state %s: cannot take the lock: %w", statePath, err))
	}
	defer lock.Release()

	var old *checkpoint.Checkpoint
	stored, err := safefile.ReadArg(path, maxStateFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.notes = append(r.notes, "the state file did not exist and was created: rollback and forks are checked against it from the next run on")
	case err != nil:
		return fail(fmt.Errorf("--state: %w", err))
	default:
		held, _, derr := decodeHeld(stored)
		if derr != nil {
			return fail(fmt.Errorf("--state %s: %w", clipArg(statePath), derr))
		}
		var hnotes []string
		old, _, hnotes, err = in.verifyHeld(held, "--state "+clipArg(statePath))
		if err != nil {
			var ie *inputErr
			if errors.As(err, &ie) {
				return fail(ie.error)
			}
			return fail(err)
		}
		r.notes = appendNew(r.notes, hnotes)
	}

	type outcome struct {
		action   string
		proved   any
		oldSize  any
		tilesMsg string
	}
	out := outcome{action: "created", oldSize: nil}
	write := true
	extends := ""
	switch {
	case old == nil:
		r.ok("state", fmt.Sprintf("no earlier checkpoint was recorded: size %d is recorded now", cp.Size))
		r.notProved = append(r.notProved, "that this checkpoint is not already a rollback: the state file was empty, so nothing earlier was known")
	case cp.Size < old.Size:
		return e.conflict(r, asJSON, path, "rollback", cp, note, old, fmt.Errorf("the checkpoint has size %d, smaller than %d, the size of the last checkpoint you verified: the log went back, or an old checkpoint is being shown", cp.Size, old.Size)), true
	case cp.Size == old.Size && cp.Root != old.Root:
		return e.conflict(r, asJSON, path, "fork", cp, note, old, fmt.Errorf("two trusted checkpoints of this log have the same size %d and different roots (%x and %x): the log has two histories", cp.Size, old.Root, cp.Root)), true
	case cp.Size == old.Size:
		r.ok("state", fmt.Sprintf("same size and root as the last verified checkpoint (%d): nothing to prove", old.Size))
		write = false
		out = outcome{action: "unchanged", proved: true, oldSize: old.Size}
		extends = fmt.Sprintf("is the same as the checkpoint in the state file (size %d)", old.Size)
	case old.Size == 0:
		r.ok("state", fmt.Sprintf("the recorded checkpoint is the empty tree, a prefix of every tree: nothing to prove for size %d", cp.Size))
		out = outcome{action: "advanced", proved: true, oldSize: old.Size}
		extends = "extends the empty tree of the state file"
	default:
		tiles, hashes, perr := in.proveExtension(old, cp)
		if perr != nil {
			switch {
			case errors.Is(perr, tlog.ErrMissing):
				r.notes = append(r.notes, fmt.Sprintf("the state file was not changed: it stays at size %d", old.Size))
				return e.finishIncomplete(r, asJSON, []string{fmt.Sprintf("the publication does not contain a tile that proves that the log at size %d extends the log at size %d of the state file: %s (under %s)", cp.Size, old.Size, perr.Error(), in.spec.rel())}), true
			case errors.Is(perr, tlog.ErrBadTile), errors.Is(perr, merkle.ErrNewRoot):
				r.notes = append(r.notes, fmt.Sprintf("the state file was not changed: it stays at size %d", old.Size))
				return e.finishFailed(r, asJSON, "tiles", perr), true
			case errors.Is(perr, merkle.ErrOldRoot):
				return e.conflict(r, asJSON, path, "fork", cp, note, old, fmt.Errorf("the log at size %d does not extend the log at size %d of the state file: both are signed by a key of the trust file, so the log has two histories (%w)", cp.Size, old.Size, perr)), true
			}
			var ie *inputErr
			if errors.As(perr, &ie) {
				return fail(ie.error)
			}
			r.notes = append(r.notes, fmt.Sprintf("the state file was not changed: it stays at size %d", old.Size))
			return e.finishFailed(r, asJSON, "consistency", perr), true
		}
		r.ok("state", fmt.Sprintf("size %d is larger than %d, the size of the last verified checkpoint", cp.Size, old.Size))
		r.ok("tiles", fmt.Sprintf("read %d tile(s) on the proof path of the tree of size %d: %d proof hash(es); no other tile was read", tiles, cp.Size, hashes))
		r.ok("consistency", fmt.Sprintf("RFC 9162 2.1.4.2: the log at size %d extends the log at size %d of the state file, for the checkpoint whose signature was verified in this run", cp.Size, old.Size))
		r.notProved = append(r.notProved, "the tile directory as a whole: only the tiles on the proof path were read, a damaged tile elsewhere is not noticed")
		out = outcome{action: "advanced", proved: true, oldSize: old.Size}
		extends = fmt.Sprintf("extends the checkpoint of size %d in the state file", old.Size)
	}
	r.notProved = append(r.notProved, "that the state file is the one you left: it is your record, keep it where the publisher cannot write")
	if write {
		data, err := encodeState(cp, note, signers, in.now)
		if err == nil {
			err = statefile.Replace(path, data)
		}
		if err != nil {
			return fail(fmt.Errorf("the checkpoint verified but --state %s could not be updated, so later runs do not know it: %w", clipArg(statePath), err))
		}
		r.notes = append(r.notes, fmt.Sprintf("the state file was updated to size %d", cp.Size))
	}
	if extends != "" {
		r.headline = fmt.Sprintf("the checkpoint of %s at size %d is signed by a trusted key and %s.", cp.Origin, cp.Size, extends)
	}
	st := map[string]any{"action": out.action, "new_size": cp.Size, "extension_proved": out.proved, "old_size": out.oldSize}
	r.info["state"] = st
	r.info["extends_state"] = out.proved
	return 0, false
}

// conflict ends a run that found a fork or a rollback: the state is left as it
// is, the conflicting checkpoint is saved beside it, and the output says where
// both are. Two notes signed by the same key for trees that are not one history
// are the proof of misbehaviour.
func (e *env) conflict(r *result, asJSON bool, statePath, step string, cp *checkpoint.Checkpoint, note []byte, old *checkpoint.Checkpoint, cause error) int {
	saved, serr := statefile.SaveConflict(statePath, cp.Size, cp.Root[:], note)
	if serr != nil {
		r.notes = append(r.notes, fmt.Sprintf("the conflicting checkpoint (size %d) could NOT be saved to %s: %v; save the note you were shown yourself", cp.Size, saved, serr))
	} else {
		r.notes = append(r.notes, fmt.Sprintf("evidence: the state file %s (size %d) was not changed, and the conflicting signed checkpoint (size %d) was saved to %s; keep both files", statePath, old.Size, cp.Size, saved))
	}
	if step == "rollback" {
		r.notes = append(r.notes, "a smaller size can be an old genuine checkpoint (rollback) or another history: this run does not tell which")
	}
	r.info["state"] = map[string]any{"action": "kept", "old_size": old.Size, "new_size": cp.Size, "extension_proved": false, "conflict_file": saved}
	r.info["extends_state"] = false
	return e.finishFailed(r, asJSON, step, cause)
}
