// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// rebaseAdvice is what the auditor is told when a held checkpoint cannot be
// used. It never says to delete the file: with no state a rollback is not
// noticed. It says how to start a new record that keeps the size.
func rebaseAdvice(what string, claimedSize uint64, haveSize bool) string {
	size := "the size you believe the log had when you last verified it"
	if haveSize {
		size += " (the note claims " + fmt.Sprint(claimedSize) + ", which nothing here authenticates)"
	}
	if what == readBackWhat {
		// Nothing exists yet that could be deleted: the file is the one that was
		// about to be written, and the run stops before it is.
		return "Nothing was written, and an earlier state file, if there is one, was not changed."
	}
	if strings.HasPrefix(what, "--old") {
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
// What a held note rests on (L12-01). The identity of a held checkpoint is its
// signed text, and the signature lines that were stored with it are only what a
// publisher without a key happened to serve; so the held note is usable when its
// text verifies under any key of the trust file, whatever the standing of that
// key now. When every key that verified it is revoked or beyond its limit the
// state is still the anchor, flagged as resting on keys the file no longer
// counts (lenient): the anchor only ever makes a run stricter (a smaller size, a
// fork, or a larger size that is not proved to extend it is a failure), so
// keeping it cannot make anything pass that would fail without it, while
// refusing it would let the choice of lines a publisher served decide whether
// later runs can go on. A note that is not a state of this program (--old FILE
// with a plain note) keeps the strict rule: it is refused then.
//
// Every failure is an input error (status 2) in both commands: the thing the
// auditor holds cannot serve as their anchor. The error says what to do and
// never says to delete the file.
func (in *logInputs) verifyHeld(note []byte, rec *stateRecord, what string, lenient bool) (*heldNote, []string, error) {
	origin := in.spec.origin(in.trust.OriginPrefix)
	keys, err := in.trust.AllKeys(origin)
	if err != nil {
		return nil, nil, &inputErr{err}
	}
	cp, signers, wrong, parsed, err := checkpoint.VerifyHeld(note, keys, origin)
	if err != nil {
		var ce *checkpoint.Error
		if errors.As(err, &ce) && ce.Step == checkpoint.StepSignature {
			return nil, nil, &inputErr{fmt.Errorf("%s has no signature of a key of the trust file that verifies (%w): the trust file or the log is not the one this note was made with%s. %s", what, err, removedKeysText(note, err), rebaseAdvice(what, 0, false))}
		}
		return nil, nil, &inputErr{fmt.Errorf("%s is not a checkpoint of this log: %w", what, err)}
	}
	if rec != nil {
		if err := rec.agrees(cp); err != nil {
			return nil, nil, &inputErr{fmt.Errorf("%s: %w. %s", what, err, rebaseAdvice(what, cp.Size, true))}
		}
	}
	var accepted []signednote.Key
	var notes, why []string
	var weakKeys []weakKey
	for _, s := range signers {
		k, _ := in.trust.KeyFor(s.Public)
		id := fmt.Sprintf("key %q (%s)", k.Name, k.Fingerprint[:16])
		switch {
		case k.Revoked:
			why = append(why, id+" is revoked in the trust file, so its signatures are not evidence of what the log was")
			weakKeys = append(weakKeys, weakKey{k.Name, k.Fingerprint, "revoked in the trust file"})
		case !in.trust.LimitFor(s.Public, cp.Size):
			why = append(why, fmt.Sprintf("%s counts only for tree sizes up to %d and the note has size %d", id, *k.MaxTreeSize, cp.Size))
			weakKeys = append(weakKeys, weakKey{k.Name, k.Fingerprint, fmt.Sprintf("counts only for tree sizes up to %d and the note has size %d", *k.MaxTreeSize, cp.Size)})
		default:
			accepted = append(accepted, s)
			if st := k.StandingAt(in.now); st != trust.Usable {
				notes = append(notes, fmt.Sprintf("%s is %s now: accepted for %s because it is a checkpoint you hold, judged when you kept it (the window of a key applies to new checkpoints)", id, st, what))
			}
		}
	}
	h := &heldNote{cp: cp, note: parsed, verified: signers, accepted: accepted}
	if len(accepted) == 0 {
		if !lenient {
			return nil, nil, &inputErr{fmt.Errorf("%s cannot be used as your earlier checkpoint: %s. %s", what, strings.Join(why, "; "), rebaseAdvice(what, cp.Size, true))}
		}
		h.weak = true
		h.weakWhy = strings.Join(why, "; ")
		h.weakKeys = weakKeys
		notes = append(notes, fmt.Sprintf("%s rests only on keys that the trust file no longer counts (%s): it is kept as your anchor, because it is what the log said when you verified it and it only makes this run stricter; it is not shown to be genuine now, and the next checkpoint signed by a key you trust that extends it replaces it", what, h.weakWhy))
	} else {
		for _, w := range why {
			notes = append(notes, "also on the note: "+w+"; not counted")
		}
	}
	if wrong > 0 {
		// A held note is judged by the signatures that verify. A line that names a
		// key of the file and does not verify is no evidence of anything, and it
		// cannot take away what a verified signature shows.
		notes = append(notes, fmt.Sprintf("%s has %d signature line(s) that name a key of the trust file and do not verify: ignored, the note is judged by the signatures that verify", what, wrong))
	}
	return h, notes, nil
}

// heldNote is a note the auditor holds, judged by verifyHeld.
type heldNote struct {
	cp   *checkpoint.Checkpoint
	note *signednote.Note
	// verified are the keys of the trust file, of any standing, whose line on the
	// note verifies over its text; accepted are those of them that count (not
	// revoked, within their size limit).
	verified []signednote.Key
	accepted []signednote.Key
	// weak is true when no verified key counts: the note is the anchor as a
	// record of what was verified, and weakWhy says why the keys do not count.
	weak     bool
	weakWhy  string
	weakKeys []weakKey
}

// weakKey is a key that a held checkpoint rests on and that the trust file no
// longer counts, and why: what a machine reader gets in the JSON (L14-07).
type weakKey struct {
	Name        string
	Fingerprint string
	Why         string
}

// weakJSON is the list of weak keys as the JSON carries it.
func weakJSON(keys []weakKey) []map[string]any {
	out := []map[string]any{}
	for _, k := range keys {
		out = append(out, map[string]any{"name": k.Name, "fingerprint": k.Fingerprint, "reason": k.Why})
	}
	return out
}

// allKeys returns every key of the trust file as a note key of the log, whatever
// its standing: the keys a signature line is verified against when it is stored.
func (in *logInputs) allKeys() ([]signednote.Key, error) {
	return in.trust.AllKeys(in.spec.origin(in.trust.OriginPrefix))
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
		var line *signednote.Signature
		for i, s := range n.Signatures {
			if s.Name == sk.Name && s.ID == sk.ID() {
				line = &n.Signatures[i]
			}
		}
		if line == nil {
			continue
		}
		was := false
		for _, c := range counted {
			if bytes.Equal(c.Public, k.Public) {
				was = true
			}
		}
		if was {
			continue
		}
		// A line is called a signature only when it verifies: a line that names
		// the key and does not is not evidence of anything, and the publisher
		// needs no key to write one.
		if len(line.Sig) == ed25519.SignatureSize && ed25519.Verify(k.Public, n.Text, line.Sig) {
			out = append(out, fmt.Sprintf("the note also carries a signature of key %q (%s), which was not counted: %s", k.Name, k.Fingerprint[:16], k.Why(in.now)))
		} else {
			out = append(out, fmt.Sprintf("the note also carries a line that names key %q (%s) and does not verify: it was not counted and is not kept", k.Name, k.Fingerprint[:16]))
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
//
// What is stored (L12-01, L12-03): the identity of a checkpoint is its signed
// text, never its signature lines. canon is the served note reduced to its text
// and every line that verifies under any key of the trust file (signers are the
// keys of those lines); when the state already holds the same text, the lines of
// both are merged, so that which of the genuine lines a publisher served on
// which run changes nothing that is stored, saved or decided.
func (e *env) applyState(r *result, in *logInputs, cp *checkpoint.Checkpoint, served *signednote.Note, canon []byte, signers []signednote.Key, statePath string, asJSON, allowShared bool) (int, bool) {
	// judged is true once the held state, or its absence, was looked at: every exit
	// from then on carries info.state.anchor_weak (L16-02).
	judged := false
	fail := func(err error) (int, bool) {
		var info map[string]any
		if judged {
			info = r.info
		}
		return e.inputErrorInfo(r.cmd, asJSON, err, info), true
	}
	path, err := statefile.Resolve(statePath)
	if err != nil {
		return fail(fmt.Errorf("--state %s: %w", statePath, err))
	}
	if perr := statefile.CheckPrivate(statePath); perr != nil {
		if !allowShared {
			return fail(fmt.Errorf("--state %s: %w: another user could replace or delete the state, or plant files beside it, and the protection against a rollback would be gone. Keep the state in a directory that only you can write (for example chmod 700), or pass --allow-shared-state-dir to accept the risk", statePath, perr))
		}
		r.notes = append(r.notes, fmt.Sprintf("the state is kept where others may be able to change it (%v): accepted by --allow-shared-state-dir", perr))
	}
	lock, err := statefile.AcquireChecked(statePath, path, statefile.LockWait, !allowShared)
	if err != nil {
		if errors.Is(err, statefile.ErrLockTimeout) {
			return fail(fmt.Errorf("--state %s: another run has held the lock on the state file for %s: nothing was changed; wait for it, or if no other kingres-verify is running, look for a stuck process", statePath, statefile.LockWait))
		}
		return fail(fmt.Errorf("--state %s: cannot take the lock: %w", statePath, err))
	}
	defer lock.Release()
	// The lock is one per directory: no run of this program is writing, so every
	// temporary file of the program that is left in the directory is a crash's (L16-04).
	statefile.RemoveLeftovers(path)
	keys, kerr := in.allKeys()
	if kerr != nil {
		return fail(kerr)
	}

	var old *checkpoint.Checkpoint
	var held *heldNote
	storedExists := true
	stored, err := safefile.ReadArg(path, maxStateFile)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		storedExists = false
		r.notes = append(r.notes, "the state file did not exist and was created: rollback and forks are checked against it from the next run on")
	case err != nil:
		return fail(fmt.Errorf("--state: %w", err))
	default:
		heldBytes, rec, derr := decodeHeld(stored)
		if derr != nil {
			if errors.Is(derr, errEmptyHeld) {
				return fail(fmt.Errorf("--state %s: %w. If you made it yourself (touch, or a redirect), it holds nothing to lose: remove it and run again, or give another name. If it ever held a state and lost it, restore your copy; with none, start a new record with checkpoint --state NEWFILE --min-size N, N being the size you believe the log had when you last verified it, and use NEWFILE from then on", statePath, derr))
			}
			return fail(fmt.Errorf("--state %s: %w", statePath, derr))
		}
		var hnotes []string
		held, hnotes, err = in.verifyHeld(heldBytes, rec, "--state "+statePath, true)
		if err != nil {
			var ie *inputErr
			if errors.As(err, &ie) {
				return fail(ie.error)
			}
			return fail(err)
		}
		old = held.cp
		r.notes = appendNew(r.notes, hnotes)
	}
	judged = true
	r.info["state"] = weakState(map[string]any{}, held)

	type outcome struct {
		action   string
		proved   bool
		oldSize  any
		tilesMsg string
	}
	out := outcome{action: "created", proved: false, oldSize: nil}
	write := true
	extends := ""
	switch {
	case old == nil:
		r.ok("state", fmt.Sprintf("no earlier checkpoint was recorded: size %d is recorded now", cp.Size))
		r.notProved = append(r.notProved, "that this checkpoint is not already a rollback: the state file was empty, so nothing earlier was known")
	case cp.Size < old.Size:
		cause := fmt.Errorf("the checkpoint has size %d, smaller than %d, the size of the last checkpoint you verified: the log went back, or an old checkpoint is being shown", cp.Size, old.Size)
		if held.weak {
			cause = fmt.Errorf("the checkpoint has size %d, smaller than %d, the size of the checkpoint in your state: the log went back, an old checkpoint is being shown, or your state is wrong: %s. %s", cp.Size, old.Size, weakCause(held), weakAdvice(old.Size))
		}
		return e.conflict(r, asJSON, keys, path, "rollback", cp, canon, held, cause), true
	case cp.Size == old.Size && cp.Root != old.Root:
		cause := fmt.Errorf("two trusted checkpoints of this log have the same size %d and different roots (%x and %x): the log has two histories", cp.Size, old.Root, cp.Root)
		if held.weak {
			cause = fmt.Errorf("the checkpoint in your state and the one served have the same size %d and different roots (%x held, %x served); the served one is signed by a key that the trust file counts, but %s. %s", old.Size, old.Root, cp.Root, weakCause(held), weakAdvice(old.Size))
		}
		return e.conflict(r, asJSON, keys, path, "fork", cp, canon, held, cause), true
	case cp.Size == old.Size:
		r.ok("state", fmt.Sprintf("same size and root as the last verified checkpoint (%d): nothing to prove", old.Size))
		write = false
		out = outcome{action: "unchanged", proved: true, oldSize: old.Size}
		extends = fmt.Sprintf("is the same as the checkpoint in the state file (size %d)", old.Size)
		// The same text is the same checkpoint, whichever of its genuine signature
		// lines each side carries: the state keeps the union, so that a state made
		// from a note served with fewer lines does not stay weaker than one made from
		// the whole note.
		//
		// Lines only accumulate (L14-03): a line the state already holds is kept as
		// it is, also when its key has left the trust file, and only the new lines
		// are verified. A served note with another text (the same size and root and
		// other extension lines) is not the same checkpoint: MergeStored says so
		// (ErrDifferentText) and the state is left as it is.
		if st, merr := signednote.MergeStored(keys, held.note, served); merr == nil && st.Added > 0 {
			canon, signers, write = st.Bytes, st.Verified, true
			out.action = "signatures_added"
			r.notes = append(r.notes, fmt.Sprintf("the state file already held this checkpoint with %d signature line(s); the note served now adds %d genuine line(s): the state keeps the union, the checkpoint itself is the same (a checkpoint is its signed text, not the lines that were served with it). The state now holds %d line(s): %s", len(held.note.Signatures), st.Added, len(st.Verified)+st.Unverifiable, holdsText(st)))
		}
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
				cause := fmt.Errorf("the log at size %d does not extend the log at size %d of the state file: both are signed by a key of the trust file, so the log has two histories (%w)", cp.Size, old.Size, perr)
				if held.weak {
					cause = fmt.Errorf("the log at size %d does not extend the log at size %d of your state (%w); the served checkpoint is signed by a key that the trust file counts, but %s. %s", cp.Size, old.Size, perr, weakCause(held), weakAdvice(old.Size))
				}
				return e.conflict(r, asJSON, keys, path, "fork", cp, canon, held, cause), true
			}
			var ie *inputErr
			if errors.As(perr, &ie) {
				return fail(ie.error)
			}
			r.notes = append(r.notes, fmt.Sprintf("the state file was not changed: it stays at size %d", old.Size))
			if held.weak {
				perr = fmt.Errorf("%w (%s. %s)", perr, weakCause(held), weakAdvice(old.Size))
				r.info["state"] = weakState(map[string]any{"action": "kept", "old_size": old.Size, "new_size": cp.Size, "extension_proved": false}, held)
			}
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
	// What the state is after this run (L16-03): the state that was written when
	// the run wrote one, the held one when it did not.
	final := held
	stateInfo := func() map[string]any {
		m := map[string]any{"action": out.action, "new_size": cp.Size, "extension_proved": out.proved, "old_size": out.oldSize}
		weakState(m, final)
		if held != nil && held.weak && !final.weak {
			m["anchor_weak_repaired"] = true
		}
		return m
	}
	if write {
		data, err := encodeStateFn(cp, canon, signers, in.now)
		if err == nil {
			var back *heldNote
			if back, err = in.readBackHeld(data, cp, canon); err != nil {
				return fail(err)
			}
			final = back
			err = replaceStateFn(path, data, stored, storedExists, maxStateFile)
			if errors.Is(err, statefile.ErrStateChanged) {
				return fail(fmt.Errorf("--state %s: %w: another run, or a program that does not take the lock, wrote it after this run read it, so this run wrote nothing (a decision made on an older state must not replace a newer one); run it again", statePath, err))
			}
		}
		var dse *statefile.DirSyncError
		if errors.As(err, &dse) {
			// The rename happened: the state is the new one (L16-05). The status stays 2.
			m := stateInfo()
			m["directory_flushed"] = false
			r.info["state"] = m
			return fail(fmt.Errorf("the checkpoint verified and --state %s was replaced (it holds size %d now), but the directory could not be flushed: a power loss may bring the old file back, and the next run then goes on from that one (%w)", statePath, cp.Size, dse.Err))
		}
		if err != nil {
			return fail(fmt.Errorf("the checkpoint verified but --state %s could not be updated, so later runs do not know it: %w", statePath, err))
		}
		if out.action == "signatures_added" {
			r.notes = append(r.notes, "the state file was updated: same checkpoint, more signature lines")
		} else {
			r.notes = append(r.notes, fmt.Sprintf("the state file was updated to size %d", cp.Size))
		}
		if held != nil && held.weak && !final.weak {
			r.notes = append(r.notes, fmt.Sprintf("the state file rested only on keys that the trust file no longer counts (%s); what was written now carries the signature of a key that counts, so it does not rest only on such keys any more: the output reports the state as it is after this run", held.weakWhy))
		}
	}
	// No non-proof is listed about a held checkpoint that rested only on keys the trust
	// file no longer counts (L16-03): a run that ends here has verified a checkpoint
	// signed by a key that counts, and either proved the held one a prefix of it or
	// merged that key's line into it, so the held checkpoint is shown to be the log's
	// and the state written rests on a key that counts. A held checkpoint that is not
	// proved ends the run before this point (a conflict or a failed proof).
	if extends != "" {
		r.headline = fmt.Sprintf("the checkpoint of %s at size %d is signed by a trusted key and %s.", cp.Origin, cp.Size, extends)
	}
	r.info["state"] = stateInfo()
	r.info["extends_state"] = out.proved
	return 0, false
}

// evidenceMerge is the merge of the evidence files of one signed text: a file at
// a name of the evidence is the same checkpoint as note when it parses and has the
// same signed text; the bytes it should hold are then the union of the lines
// (signednote.MergeStored). Lines only accumulate (L14-03): the lines the file
// holds are kept as they are, also those of a key that has left the trust file,
// and only new lines are verified. extended is set when lines were added.
func evidenceMerge(keys []signednote.Key, note []byte, extended **signednote.Stored) func(have []byte) ([]byte, bool) {
	return func(have []byte) ([]byte, bool) {
		hn, err := signednote.Parse(have)
		if err != nil {
			return nil, false
		}
		sn, err := signednote.Parse(note)
		if err != nil || !bytes.Equal(hn.Text, sn.Text) {
			return nil, false
		}
		st, err := signednote.MergeStored(keys, hn, sn)
		if err != nil {
			return nil, false
		}
		if st.Added == 0 {
			return have, true // nothing to add: the file stays as it is
		}
		*extended = st
		return st.Bytes, true
	}
}

// printNote puts a conflicting note that this run did not save in the output, so
// that it is not lost with the run: base64 of exactly the bytes that should have
// been saved (the note reduced to what verified), which a person can decode into a
// file and keep. info gets it as note_base64.
func printNote(r *result, info map[string]any, note []byte) {
	enc := base64.StdEncoding.EncodeToString(note)
	info["note_base64"] = enc
	r.notes = append(r.notes, "the note to keep, as base64 of its exact bytes (decode it into a file): "+enc)
}

// conflict ends a run that found a fork or a rollback: the state is left as it
// is, the conflicting checkpoint is saved beside it, and the output says where
// both are. Two notes signed by the same key for trees that are not one history
// are the proof of misbehaviour.
func (e *env) conflict(r *result, asJSON bool, keys []signednote.Key, statePath, step string, cp *checkpoint.Checkpoint, note []byte, held *heldNote, cause error) int {
	old := held.cp
	if held.weak {
		return e.weakConflict(r, asJSON, keys, statePath, step, cp, note, held, cause)
	}
	// An evidence file is one per signed text: the same text served with other
	// genuine lines is the same file, extended (L12-03), never another name.
	var extended *signednote.Stored
	saved, serr := saveConflictFn(statePath, cp.Size, cp.Root[:], note, evidenceMerge(keys, note, &extended))
	// A failed flush of the directory after the file was put in place is not a
	// failed save (L16-05): the file is there, a power loss may take it back.
	var dse *statefile.DirSyncError
	unflushed := errors.As(serr, &dse)
	code := ExitFailed
	info := map[string]any{"action": "kept", "old_size": old.Size, "new_size": cp.Size, "extension_proved": false, "evidence_saved": serr == nil || unflushed}
	weakState(info, held)
	switch {
	case unflushed:
		// The status stays 4: the evidence is not known to be durable.
		code = ExitEvidenceLost
		r.notes = append(r.notes, fmt.Sprintf("the conflicting signed checkpoint (size %d) was saved to %s, but the directory could not be flushed: a power loss may take the file back (%v); the state file %s (size %d) was not changed; keep the note printed below yourself, it is the proof of the conflict", cp.Size, saved.Path, dse.Err, statePath, old.Size))
		info["conflict_file"] = saved.Path
		info["evidence_flushed"] = false
		printNote(r, info, note)
	case serr != nil:
		// The finding stands, and so does the failure; what is added is that the
		// evidence is not on disk, which must never be claimed. The status says so
		// apart from the fork itself.
		code = ExitEvidenceLost
		r.notes = append(r.notes, fmt.Sprintf("the conflicting signed checkpoint (size %d) was NOT saved: %v; the state file %s (size %d) was not changed; save the note you were shown yourself, it is the proof of the conflict", cp.Size, serr, statePath, old.Size))
		info["conflict_file"] = nil
		printNote(r, info, note)
	case saved.Existing && saved.Extended:
		holds := ""
		if extended != nil {
			holds = fmt.Sprintf(" %d signature line(s), %s; this run added %d", len(extended.Verified)+extended.Unverifiable, holdsText(extended), extended.Added)
		}
		r.notes = append(r.notes, fmt.Sprintf("evidence: the state file %s (size %d) was not changed, and this very conflicting signed checkpoint (size %d) was already saved to %s by an earlier run, with fewer signature lines: the file now holds%s (a checkpoint is its signed text, not its lines; lines only accumulate); keep both files", statePath, old.Size, cp.Size, saved.Path, holds))
		info["conflict_file"] = saved.Path
	case saved.Existing:
		r.notes = append(r.notes, fmt.Sprintf("evidence: the state file %s (size %d) was not changed, and this very conflicting signed checkpoint (size %d) was already saved to %s by an earlier run; keep both files", statePath, old.Size, cp.Size, saved.Path))
		info["conflict_file"] = saved.Path
	default:
		r.notes = append(r.notes, fmt.Sprintf("evidence: the state file %s (size %d) was not changed, and the conflicting signed checkpoint (size %d) was saved to %s; keep both files", statePath, old.Size, cp.Size, saved.Path))
		info["conflict_file"] = saved.Path
	}
	if step == "rollback" {
		r.notes = append(r.notes, "a smaller size can be an old genuine checkpoint (rollback) or another history: this run does not tell which")
	}
	r.info["state"] = info
	r.info["extends_state"] = false
	return e.finishFailedWith(r, asJSON, step, cause, code)
}

// readBack reads the file that is about to be stored the way the next run
// reads it (decodeHeld, then verifyHeld with the same trust file) and requires
// the very checkpoint and note that were verified. A note that passes as a new
// checkpoint but would be refused as a held one (a corrupt signature of a key
// that is revoked now is ignored for a new note and refused for a held one) is
// never stored: the protection against rollback would be stuck on a file no
// later run accepts.
func (in *logInputs) readBack(data []byte, cp *checkpoint.Checkpoint, note []byte) error {
	_, err := in.readBackHeld(data, cp, note)
	return err
}

// readBackHeld is readBack that also returns the held note as the next run will
// judge it, so that the run can say whether the state it writes rests only on keys
// the trust file no longer counts (L16-03).
func (in *logInputs) readBackHeld(data []byte, cp *checkpoint.Checkpoint, note []byte) (*heldNote, error) {
	held, rec, err := decodeHeld(data)
	var back *heldNote
	if err == nil {
		back, _, err = in.verifyHeld(held, rec, readBackWhat, true)
		if err == nil && (back.cp.Size != cp.Size || back.cp.Root != cp.Root || !bytes.Equal(held, note)) {
			err = errors.New("the state file that would be written does not read back as the checkpoint that was verified")
		}
	}
	if err != nil {
		var ie *inputErr
		if errors.As(err, &ie) {
			err = ie.error
		}
		return nil, fmt.Errorf("this checkpoint cannot be recorded, because a later run would not accept the state file made from it (%w); the state file was not changed", err)
	}
	return back, nil
}

// readBackWhat names the file that readBack checks, in the messages.
const readBackWhat = "the state file that would be written"

// encodeStateFn is encodeState; a test replaces it to put a state that a later
// run would not read in front of readBack, which nothing else can reach.
var encodeStateFn = encodeState

// canonicalOf is the form in which a verified note is stored and saved as
// evidence (signednote.Merge of the note with itself): its text and every
// signature line that verifies under a key of the trust file, of any standing,
// and nothing a publisher without a key could add or vary. It returns the parsed
// note, the keys of the lines kept, and how many lines of the note as served are
// not in it.
func (in *logInputs) canonicalOf(note []byte) (*signednote.Note, []byte, []signednote.Key, int, error) {
	parsed, err := signednote.Parse(note)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("the verified note does not parse again: %w", err)
	}
	keys, err := in.allKeys()
	if err != nil {
		return nil, nil, nil, 0, err
	}
	canon, verified, err := signednote.Merge(keys, parsed)
	if err != nil {
		return nil, nil, nil, 0, fmt.Errorf("the verified note has no line that verifies under a key of the trust file: %w", err)
	}
	return parsed, canon, verified, len(parsed.Signatures) - len(verified), nil
}

// holdsText says what the lines of a stored note are: how many verify now and
// how many are of keys the trust file does not hold (L14-03).
func holdsText(st *signednote.Stored) string {
	s := fmt.Sprintf("%d verify under a key of the trust file", len(st.Verified))
	if st.Unverifiable > 0 {
		s += fmt.Sprintf(" and %d are of keys the trust file does not hold: kept as stored, not verified now, and they make nothing usable", st.Unverifiable)
	}
	if st.Dropped > 0 {
		s += fmt.Sprintf(" (%d more of such keys did not fit under the limit of %d lines and were left out)", st.Dropped, signednote.MaxSignatures)
	}
	return s
}

// weakCause is what a run says when it ends in a fork, a rollback or a failed
// extension against a held checkpoint that rests only on keys the trust file no
// longer counts (L14-02): the conflict may come from the state and not from the
// log, so that it is not an accusation of the operator.
func weakCause(h *heldNote) string {
	return "the checkpoint in your state rests only on keys that the trust file no longer counts (" + h.weakWhy + "), so it may have been signed by a stolen key and this conflict may come from your state and not from the log"
}

// weakAdvice is the way out for such a conflict: the same rebase that a state
// that cannot be used is given.
func weakAdvice(heldSize uint64) string {
	return fmt.Sprintf("If you did not expect the log to disagree with you, rebase: keep this state file as it is (it is what you were shown), obtain a checkpoint of this log signed by a key you trust, from a source you trust, run checkpoint --state NEWFILE --min-size N with N = the size you believe the log had when you last verified it (your state holds %d, which rests only on the keys above and which nothing here authenticates), and use NEWFILE from then on", heldSize)
}

// weakState adds to the info of a run whether the held checkpoint rests only on
// keys the trust file no longer counts, and which keys and why, so that a machine
// reader does not have to read the notes (L14-07). It returns m.
func weakState(m map[string]any, held *heldNote) map[string]any {
	weak := held != nil && held.weak
	m["anchor_weak"] = weak
	if weak {
		m["anchor_weak_keys"] = weakJSON(held.weakKeys)
	}
	return m
}

// weakConflict ends a run whose served checkpoint conflicts with a held one that
// rests only on keys the trust file no longer counts (L14-02). The status is 1,
// as for any conflict (the table of statuses: a verification failed): the held
// checkpoint is used as the anchor, and the served one does not agree with it.
// It is neither 2 (the held checkpoint is not refused: that was decided) nor 4
// (4 says that a fork, which is a finding about the log, was not saved; here the
// finding is not established).
//
// At most two evidence files are kept for a held state (statefile.SaveWeakConflict):
// the first note of each kind, a rollback and a fork, and never one per run. A
// party without a key can serve an old genuine checkpoint once, which is a
// rollback and takes the file of the rollbacks; a fork has a file of its own, so
// the replay cannot take its place (L16-01). The bound is two whatever is served,
// and a note that is not saved by this run, for any reason (the file of its kind
// holds another checkpoint, a file is planted there, the disk refuses), is printed
// in full, as the evidence of a state that counts is: the auditor does not learn
// that a note exists only in the run's source.
func (e *env) weakConflict(r *result, asJSON bool, keys []signednote.Key, statePath, step string, cp *checkpoint.Checkpoint, note []byte, held *heldNote, cause error) int {
	old := held.cp
	kind := statefile.WeakFork
	if step == "rollback" {
		kind = statefile.WeakRollback
	}
	var extended *signednote.Stored
	saved, serr := saveWeakConfFn(statePath, kind, old.Size, old.Root[:], note, evidenceMerge(keys, note, &extended))
	var dse *statefile.DirSyncError
	unflushed := errors.As(serr, &dse)
	info := map[string]any{"action": "kept", "old_size": old.Size, "new_size": cp.Size, "extension_proved": false, "evidence_saved": serr == nil || unflushed}
	weakState(info, held)
	switch {
	case unflushed:
		// The status stays 1 (never 4 here).
		r.notes = append(r.notes, fmt.Sprintf("the conflicting checkpoint (size %d) was saved to %s, but the directory could not be flushed: a power loss may take the file back (%v); the state file %s (size %d) was not changed; keep the note printed below yourself", cp.Size, saved.Path, dse.Err, statePath, old.Size))
		info["conflict_file"] = saved.Path
		info["evidence_flushed"] = false
		printNote(r, info, note)
	case errors.Is(serr, statefile.ErrSlotTaken):
		r.notes = append(r.notes, fmt.Sprintf("the conflicting checkpoint (size %d) was NOT saved by this run: the one file kept for a %s against this state, %s, holds another checkpoint (or is not a signed note of this log), and at most one file is kept for each kind, never one per run; the state file %s (size %d) was not changed; keep the note printed below yourself, and keep both files", cp.Size, kind, saved.Path, statePath, old.Size))
		info["conflict_file"] = nil
		printNote(r, info, note)
	case serr != nil:
		// Not status 4: that status is for a fork that is a finding about the log.
		r.notes = append(r.notes, fmt.Sprintf("the conflicting checkpoint (size %d) was NOT saved: %v; the state file %s (size %d) was not changed; the next run will find the same conflict; keep the note printed below yourself", cp.Size, serr, statePath, old.Size))
		info["conflict_file"] = nil
		printNote(r, info, note)
	case saved.Existing:
		holds := ""
		if extended != nil {
			holds = fmt.Sprintf(" (it now holds %d signature line(s), %s; this run added %d)", len(extended.Verified)+extended.Unverifiable, holdsText(extended), extended.Added)
		}
		r.notes = append(r.notes, fmt.Sprintf("evidence: the state file %s (size %d) was not changed, and this very conflicting checkpoint (size %d) is already kept for this state, at %s, by an earlier run%s; keep both files", statePath, old.Size, cp.Size, saved.Path, holds))
		info["conflict_file"] = saved.Path
	default:
		r.notes = append(r.notes, fmt.Sprintf("evidence: the state file %s (size %d) was not changed, and the conflicting checkpoint (size %d) was saved to %s; because the state rests only on keys the trust file no longer counts, one file is kept for the first %s against it and a later one that differs is printed and not saved (a log that grows would make a file at each run); keep both files", statePath, old.Size, cp.Size, saved.Path, kind))
		info["conflict_file"] = saved.Path
	}
	if step == "rollback" {
		r.notes = append(r.notes, "a smaller size can be an old genuine checkpoint (rollback) or another history: this run does not tell which")
	}
	r.info["state"] = info
	r.info["extends_state"] = false
	return e.finishFailedWith(r, asJSON, step, cause, ExitFailed)
}

// oldWeakInfo adds to the info of consistency whether the old checkpoint rests
// only on keys the trust file no longer counts (L14-07).
func oldWeakInfo(m map[string]any, held *heldNote) {
	if held == nil {
		return
	}
	m["old_anchor_weak"] = held.weak
	if held.weak {
		m["old_anchor_weak_keys"] = weakJSON(held.weakKeys)
	}
}

// removedKeysText is the warning of L14-08: a note whose lines are all of keys
// that the trust file does not hold (a key that was removed from the file, and not
// revoked) cannot be used, and the text says which key ids the note carries and
// that removing a key is what did it.
func removedKeysText(note []byte, cause error) string {
	if !errors.Is(cause, signednote.ErrNoTrustedSignature) {
		return ""
	}
	n, err := signednote.Parse(note)
	if err != nil || len(n.Signatures) == 0 {
		return ""
	}
	var ids []string
	for _, s := range n.Signatures {
		ids = append(ids, fmt.Sprintf("%08x", s.ID))
	}
	return fmt.Sprintf(" (the note carries lines of the key id(s) %s, and the trust file holds none of those keys: removing a key from the trust file leaves every state that rests only on it unreadable, which is why a key that is no longer wanted is marked revoked in the file and never deleted)", strings.Join(ids, ", "))
}

// The calls that write beside the state, as variables so that a test can put a
// failed flush of the directory after a write that did happen (L16-05), which
// nothing else can reach. Production code never sets them.
var (
	replaceStateFn = statefile.ReplaceIfUnchanged
	saveConflictFn = statefile.SaveConflictMerging
	saveWeakConfFn = statefile.SaveWeakConflict
)
