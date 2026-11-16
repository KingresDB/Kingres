// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// exitTextWith3 is the exit status line of the commands that can end with
// status 3.
const exitTextWith3 = "0 verified, 1 verification failed, 2 usage or input error,\n3 incomplete: nothing failed, but something asked for could not be checked, for example a file\nthe publication should hold is not there. Treat 3 as an alert, never as \"probably fine\"."

// ---- the log named on the command line ----

// logSpec is the value of --log: chain:<64 hex>[:e<N>] or aggregate:<64 hex>.
type logSpec struct {
	kind     string // "chain" or "aggregate"
	id       string
	epoch    uint64
	hasEpoch bool
}

var logSpecRE = regexp.MustCompile(`^(chain|aggregate):([0-9a-f]{64})(?::e(0|[1-9][0-9]{0,8}))?$`)

var epochDirRE = regexp.MustCompile(`^e([1-9][0-9]{0,8})$`)

func parseLogSpec(s string) (logSpec, error) {
	m := logSpecRE.FindStringSubmatch(s)
	if m == nil {
		return logSpec{}, fmt.Errorf("--log %q is not chain:<64 lower case hex>[:e<N>] or aggregate:<64 lower case hex>", clipArg(s))
	}
	ls := logSpec{kind: m[1], id: m[2]}
	if m[3] != "" {
		n, _ := strconv.ParseUint(m[3], 10, 64)
		if n == 0 {
			return logSpec{}, errors.New("--log epoch must be at least 1")
		}
		if ls.kind == "aggregate" {
			return logSpec{}, errors.New("an aggregate log has no epoch")
		}
		ls.epoch, ls.hasEpoch = n, true
	}
	return ls, nil
}

func clipArg(s string) string {
	if len(s) > 100 {
		return strings.ToValidUTF8(s[:100], "?") + "..."
	}
	return s
}

// rel is the directory of the log below the publication root (design 4.6).
func (l logSpec) rel() string {
	if l.kind == "aggregate" {
		return "a/" + l.id
	}
	return "c/" + l.id + "/e" + strconv.FormatUint(l.epoch, 10)
}

// origin is the origin line of the log (design 4.2).
func (l logSpec) origin(prefix string) string { return prefix + "/" + l.rel() }

// ---- what a publication does not contain, and what it contains wrongly ----
//
// One rule for every command (L7-07): a thing the auditor named (a file on the
// command line, the source directory, the trust file) that cannot be read is
// an input error, status 2. A thing the publication should hold and does not
// is "could not check", status 3: nothing failed, but the check was not made.
// A thing the publication holds in a form it must not have (a link, a pipe or
// device, a directory, a file over its size limit, a tile of the wrong size,
// bad content) is a failed verification, status 1.

type pubKind int

const (
	pubAbsent pubKind = iota
	pubMalformed
)

// pubError is a problem with the publication, not with the command line.
type pubError struct {
	kind pubKind
	what string // what was asked for, for the missing list
	err  error
}

func (p *pubError) Error() string { return p.err.Error() }
func (p *pubError) Unwrap() error { return p.err }

// classifyPub turns an error from reading the publication into a pubError,
// or returns it unchanged when it is an input problem (for example a
// permission error on the auditor's own copy).
func classifyPub(what string, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return &pubError{pubAbsent, what, err}
	case safefile.Unusable(err):
		return &pubError{pubMalformed, what, err}
	}
	return err
}

// finishPub ends a command after a pubError (status 3 or 1) or an input
// error (status 2).
func (e *env) finishPub(r *result, asJSON bool, err error) int {
	var pe *pubError
	if errors.As(err, &pe) {
		if pe.kind == pubAbsent {
			return e.finishIncomplete(r, asJSON, []string{"the publication does not contain " + pe.what + ": " + pe.err.Error()})
		}
		return e.finishFailed(r, asJSON, "publication", pe.err)
	}
	return e.inputError(r.cmd, asJSON, err)
}

// parseNow reads --now or takes the system clock.
func parseNow(s string) (time.Time, bool, error) {
	if s == "" {
		return time.Now().UTC(), false, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("--now %q is not an RFC 3339 time: %w", clipArg(s), err)
	}
	return t.UTC(), true, nil
}

// ---- shared loading ----

type logInputs struct {
	trust  *trust.File
	spec   logSpec
	source string
	dir    *safefile.Dir // the source directory, opened on first use
	now    time.Time
	notes  []string
}

// close releases the source directory.
func (in *logInputs) close() {
	if in.dir != nil {
		_ = in.dir.Close()
	}
}

// openSource opens --source: an input error when it is not a directory.
func (in *logInputs) openSource() (*safefile.Dir, error) {
	if in.dir != nil {
		return in.dir, nil
	}
	d, err := safefile.OpenDir(in.source)
	if err != nil {
		return nil, fmt.Errorf("--source: %w", err)
	}
	in.dir = d
	return d, nil
}

// readPublished reads a file of the publication below the log directory.
func (in *logInputs) readPublished(rel string, limit int64) ([]byte, error) {
	d, err := in.openSource()
	if err != nil {
		return nil, err
	}
	data, err := d.ReadFile(in.spec.rel()+"/"+rel, limit)
	if err != nil {
		return nil, classifyPub(in.spec.rel()+"/"+rel, err)
	}
	return data, nil
}

// resolveEpoch finds the epoch of a chain log from the directories of the
// source when --log does not carry one. It refuses to guess when there is not
// exactly one: an old closed epoch must never be taken for the current one.
func (in *logInputs) resolveEpoch() error {
	l := in.spec
	if l.kind != "chain" || l.hasEpoch {
		return nil
	}
	d, err := in.openSource()
	if err != nil {
		return err
	}
	dirs, others, err := d.SubDirs("c/" + l.id)
	if err != nil {
		return classifyPub("the chain c/"+l.id, err)
	}
	var found []uint64
	for _, name := range dirs {
		if m := epochDirRE.FindStringSubmatch(name); m != nil {
			n, _ := strconv.ParseUint(m[1], 10, 64)
			found = append(found, n)
		}
	}
	for _, name := range others {
		if epochDirRE.MatchString(name) {
			return &pubError{pubMalformed, "an epoch directory", fmt.Errorf("c/%s/%s %w", l.id, name, safefile.ErrNotRegular)}
		}
	}
	if len(found) != 1 {
		return fmt.Errorf("the source has %d epochs of the chain log, expected exactly one; give --log chain:%s:e<N>", len(found), l.id)
	}
	in.spec.epoch, in.spec.hasEpoch = found[0], true
	return nil
}

// loadLogInputs reads --trust, --log, --source and --now. A problem with the
// command line is an input error (exit status 2); a problem with the
// publication is a pubError.
func loadLogInputs(trustPath, logFlag, source, nowFlag string, noteGiven bool) (*logInputs, error) {
	if trustPath == "" {
		return nil, errors.New("missing --trust")
	}
	if logFlag == "" {
		return nil, errors.New("missing --log")
	}
	in := &logInputs{source: source}
	var err error
	if in.spec, err = parseLogSpec(logFlag); err != nil {
		return nil, err
	}
	data, err := readArg(trustPath, trust.MaxFileSize)
	if err != nil {
		return nil, err
	}
	if in.trust, err = trust.Parse(data); err != nil {
		return nil, fmt.Errorf("trust file %s: %w", trustPath, err)
	}
	var fixed bool
	if in.now, fixed, err = parseNow(nowFlag); err != nil {
		return nil, err
	}
	if fixed {
		in.notes = append(in.notes, "time used for key validity windows: "+in.now.Format(time.RFC3339)+", fixed by --now (the clock was not read)")
	} else {
		in.notes = append(in.notes, "time used for key validity windows: system clock "+in.now.Format(time.RFC3339))
	}
	if !in.spec.hasEpoch && in.spec.kind == "chain" {
		if noteGiven {
			return nil, fmt.Errorf("give the epoch with --log chain:%s:e<N> when the checkpoint is a file", in.spec.id)
		}
		if err := in.resolveEpoch(); err != nil {
			in.close()
			return nil, err
		}
	}
	return in, nil
}

// verifyNote verifies note data as the checkpoint of the log: the signature of
// a trusted key that may be used now, and a tree size within what the trust file
// allows for that key.
func (in *logInputs) verifyNote(note []byte) (*checkpoint.Checkpoint, []signednote.Key, []string, error) {
	origin := in.spec.origin(in.trust.OriginPrefix)
	keys, notes, err := in.trust.SignerKeys(origin, in.now)
	if err != nil {
		return nil, nil, notes, &inputErr{err}
	}
	cp, signers, err := checkpoint.Verify(note, keys, origin)
	if err != nil {
		return nil, nil, notes, err
	}
	// The size is in the signed text, so a size limit can be enforced; a time
	// cannot (the checkpoint has none).
	var allowed []signednote.Key
	for _, k := range signers {
		if in.trust.LimitFor(k.Public, cp.Size) {
			allowed = append(allowed, k)
		}
	}
	if len(allowed) == 0 {
		return nil, nil, notes, &checkpoint.Error{Step: "key-size", Err: fmt.Errorf("the checkpoint has tree size %d, more than the trust file allows for the key that signed it (%s): a key limited with max_tree_size must not sign larger trees", cp.Size, describeSigners(signers))}
	}
	return cp, allowed, notes, nil
}

// inputErr marks an error that is an input problem (exit status 2) inside a
// function that otherwise returns verification failures.
type inputErr struct{ error }

func (e *inputErr) Unwrap() error { return e.error }

func describeSigners(signers []signednote.Key) string {
	var parts []string
	for _, k := range signers {
		parts = append(parts, "key "+trust.Fingerprint(k.Public)[:16])
	}
	return strings.Join(parts, ", ")
}

func signerPrints(signers []signednote.Key) []string {
	out := []string{}
	for _, k := range signers {
		out = append(out, trust.Fingerprint(k.Public))
	}
	return out
}

// ---- checkpoint ----

// rollbackLine is the item of "not proved" that every checkpoint check without
// a state carries: an older genuine checkpoint verifies.
const rollbackLine = "that this is the latest checkpoint: an older genuine checkpoint also verifies, so a publisher that serves one hides every later entry (rollback). " +
	"Keep the last checkpoint you verified and run consistency --old <that file>, or give --state FILE, or give --min-size"

func (e *env) runCheckpoint(args []string) int {
	fs, asJSON := e.newFlagsWithExit("checkpoint", "--trust FILE --log chain:<id>[:e<N>]|aggregate:<id> [--source DIR | --note FILE] [--min-size N] [--state FILE] [--now TIME] [--json]",
		"Verify a signed checkpoint of a tile log: the note is well formed, at least one signature is\n"+
			"by a key of the trust file that may be used now, no signature of such a key is wrong, the\n"+
			"origin is the one of the named log below the trusted prefix, the tree size is within what\n"+
			"the trust file allows for the signing key, and the size and root are well formed. The\n"+
			"checkpoint is read from --note or from <source>/c/<id>/e<N>/checkpoint (aggregate:\n"+
			"a/<id>/checkpoint). It reads no tile and does not show that the checkpoint is the latest:\n"+
			"an older genuine one verifies too. --min-size fails a smaller tree. --state FILE names a\n"+
			"file holding the last checkpoint this command verified: a smaller size, or the same size\n"+
			"with another root, then fails, and the file is replaced (atomically) after a success. A\n"+
			"larger size is NOT shown to extend the state's log; use consistency for that.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	logFlag := fs.String("log", "", "the log: chain:<64 hex>[:e<N>] or aggregate:<64 hex>")
	source := fs.String("source", ".", "directory of the publication or bundle")
	note := fs.String("note", "", "checkpoint file to verify instead of the one in --source")
	minSize := fs.Uint64("min-size", 0, "fail when the tree size is smaller than this")
	statePath := fs.String("state", "", "file with the last verified checkpoint: refuses a smaller size or a fork, updated after a success")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "checkpoint"
	r := &result{cmd: cmd}
	in, err := loadLogInputs(*trustPath, *logFlag, *source, *nowFlag, *note != "")
	if err != nil {
		return e.finishPub(r, *asJSON, err)
	}
	defer in.close()
	r.notes = append(r.notes, in.notes...)
	var data []byte
	if *note != "" {
		data, err = readArg(*note, signednote.MaxNoteSize)
	} else {
		data, err = in.readPublished("checkpoint", signednote.MaxNoteSize)
	}
	if err != nil {
		return e.finishPub(r, *asJSON, err)
	}
	cp, signers, keyNotes, err := in.verifyNote(data)
	r.notes = append(r.notes, keyNotes...)
	if err != nil {
		var ie *inputErr
		if errors.As(err, &ie) {
			return e.inputError(cmd, *asJSON, ie.error)
		}
		var ce *checkpoint.Error
		step := "checkpoint"
		if errors.As(err, &ce) {
			step = ce.Step
		}
		return e.finishFailed(r, *asJSON, step, err)
	}
	r.ok("parse", "the note is well formed")
	r.ok("signature", "verified by "+describeSigners(signers)+" of the trust file")
	r.ok("origin", cp.Origin)
	r.ok("checkpoint", fmt.Sprintf("tree size %d, root %x", cp.Size, cp.Root))
	if cp.Size < *minSize {
		return e.finishFailed(r, *asJSON, "min-size", fmt.Errorf("tree size %d is smaller than --min-size %d", cp.Size, *minSize))
	}
	if *minSize > 0 {
		r.ok("min-size", fmt.Sprintf("%d is at least %d", cp.Size, *minSize))
	}
	if len(cp.Extensions) > 0 {
		r.notes = append(r.notes, fmt.Sprintf("the checkpoint has %d extension line(s): they are signed but have no meaning here", len(cp.Extensions)))
	}
	r.notProved = []string{
		"the tiles and entries of the log: this checks the signed size and root only (use consistency and, later, proof with a bundle)",
		"when the checkpoint was made: no anchor is checked, the time is the signer's claim",
		"that the signer shows the same history to everyone: a split view needs witnesses or a monitor",
		"that the trust file was obtained outside the cluster: that is your responsibility",
	}
	r.headline = fmt.Sprintf("the checkpoint of %s at size %d is signed by a trusted key.", cp.Origin, cp.Size)
	if *statePath == "" {
		r.notProved = append([]string{rollbackLine}, r.notProved...)
	} else if code, done := e.checkState(r, in, cp, data, *statePath, *asJSON); done {
		return code
	}
	r.info = map[string]any{
		"origin": cp.Origin, "size": cp.Size, "root": format.EncodeHex(cp.Root[:]),
		"signers": signerPrints(signers), "extension_lines": len(cp.Extensions),
	}
	return e.finishOK(r, *asJSON)
}

// checkState applies --state: reads the state, compares, and after a success
// replaces it. It returns done=true with the exit status when the command ends
// here (a failure or an input error).
func (e *env) checkState(r *result, in *logInputs, cp *checkpoint.Checkpoint, note []byte, path string, asJSON bool) (int, bool) {
	oldNote, err := readArg(path, signednote.MaxNoteSize)
	var old *checkpoint.Checkpoint
	switch {
	case errors.Is(err, fs.ErrNotExist):
		r.notes = append(r.notes, "the state file did not exist and was created: rollback and forks are checked against it from the next run on")
	case err != nil:
		return e.inputError(r.cmd, asJSON, fmt.Errorf("--state: %w", err)), true
	default:
		var verr error
		old, _, _, verr = in.verifyNote(oldNote)
		if verr != nil {
			return e.inputError(r.cmd, asJSON, fmt.Errorf("--state %s does not verify as a checkpoint of this log with this trust file (%w): if you changed the trust file or the log on purpose, move the state file away", clipArg(path), verr)), true
		}
	}
	write := true
	switch {
	case old == nil:
		r.ok("state", fmt.Sprintf("no earlier checkpoint was recorded: size %d is recorded now", cp.Size))
		r.notProved = append(r.notProved, "that this checkpoint is not already a rollback: the state file was empty, so nothing earlier was known")
	case cp.Size < old.Size:
		return e.finishFailed(r, asJSON, "rollback", fmt.Errorf("the checkpoint has size %d, smaller than %d, the size of the last checkpoint you verified: the log went back, or an old checkpoint is being shown", cp.Size, old.Size)), true
	case cp.Size == old.Size && cp.Root != old.Root:
		return e.finishFailed(r, asJSON, "fork", fmt.Errorf("two trusted checkpoints of this log have the same size %d and different roots (%x and %x): the log has two histories", cp.Size, old.Root, cp.Root)), true
	case cp.Size == old.Size:
		r.ok("state", fmt.Sprintf("same size and root as the last verified checkpoint (%d)", old.Size))
		write = false
	default:
		r.ok("state", fmt.Sprintf("size %d is not smaller than %d, the size of the last verified checkpoint", cp.Size, old.Size))
		r.notProved = append(r.notProved, fmt.Sprintf("that the log at size %d extends the log at size %d recorded in the state: only the sizes were compared, a larger size can be another history (run consistency --old %s --source ... before this command to prove it)", cp.Size, old.Size, clipArg(path)))
	}
	r.notProved = append(r.notProved, "that the state file is the one you left: it is your record, keep it where the publisher cannot write")
	if write {
		if err := writeStateAtomic(path, note); err != nil {
			return e.inputError(r.cmd, asJSON, fmt.Errorf("the checkpoint verified but --state %s could not be updated, so later runs do not know it: %w", clipArg(path), err)), true
		}
		r.notes = append(r.notes, fmt.Sprintf("the state file was updated to size %d", cp.Size))
	}
	return 0, false
}

// writeStateAtomic replaces the state file with the verified note: a
// temporary file in the same directory, flushed to disk, then renamed, so a
// crash leaves the old state or the new one and never half of one. Two runs at
// the same time are not coordinated: do not run two against one state file.
func writeStateAtomic(path string, note []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(note); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// ---- consistency ----

var sizeRootRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,18}):([A-Za-z0-9+/=]+)$`)

func (e *env) runConsistency(args []string) int {
	fs, asJSON := e.newFlagsWithExit("consistency", "--trust FILE --log <log> --old NOTEFILE|SIZE:ROOT [--new NOTEFILE] [--source DIR] [--now TIME] [--json]",
		"Check that the log at the new checkpoint extends the log at the old one (RFC 9162 section\n"+
			"2.1.4.2). The new checkpoint is --new or the one in --source; both are verified like the\n"+
			"checkpoint command does. The old checkpoint is a note file (verified the same way) or\n"+
			"SIZE:ROOT with the root in base64 (taken as given: you hold it from elsewhere). The proof\n"+
			"hashes are read from the tiles under --source and checked against both roots. Only the\n"+
			"tiles on the proof path are read (a few per level of the tree), so a damaged tile that\n"+
			"is not on that path is not noticed. When the sizes are equal or the old size is 0 the\n"+
			"proof is empty and no tile is read; the output says so. A tile that is absent gives\n"+
			"status 3, a tile that is present and wrong gives status 1.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	logFlag := fs.String("log", "", "the log: chain:<64 hex>[:e<N>] or aggregate:<64 hex>")
	source := fs.String("source", ".", "directory of the publication or bundle")
	oldFlag := fs.String("old", "", "the earlier checkpoint: a note file or SIZE:ROOT")
	newFlag := fs.String("new", "", "the later checkpoint file (default: the checkpoint in --source)")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "consistency"
	r := &result{cmd: cmd}
	if *oldFlag == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --old"))
	}
	in, err := loadLogInputs(*trustPath, *logFlag, *source, *nowFlag, *newFlag != "")
	if err != nil {
		return e.finishPub(r, *asJSON, err)
	}
	defer in.close()
	r.notes = append(r.notes, in.notes...)
	// The old checkpoint is an anchor the caller supplies: if it cannot be
	// read or is not a note at all, that is an input error (status 2), not a
	// finding about the log.
	var oldCP *checkpoint.Checkpoint
	oldSigned := false
	if m := sizeRootRE.FindStringSubmatch(*oldFlag); m != nil {
		size, err := strconv.ParseUint(m[1], 10, 64)
		rb, derr := signednote.DecodeBase64(m[2])
		if err != nil || size >= tlog.MaxTreeSize || derr != nil || len(rb) != 32 {
			return e.inputError(cmd, *asJSON, fmt.Errorf("--old %q: expected a size below %d and a canonical base64 root of 32 bytes", clipArg(*oldFlag), tlog.MaxTreeSize))
		}
		oldCP = &checkpoint.Checkpoint{Size: size}
		copy(oldCP.Root[:], rb)
		if size == 0 && oldCP.Root != merkle.EmptyRoot() {
			return e.inputError(cmd, *asJSON, errors.New("--old has size 0 but not the root of the empty tree"))
		}
	} else {
		oldData, err := readArg(*oldFlag, signednote.MaxNoteSize)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		if _, err := signednote.Parse(oldData); err != nil {
			return e.inputError(cmd, *asJSON, fmt.Errorf("--old %s is not a signed note: %w", clipArg(*oldFlag), err))
		}
		oldSigned = true
		cp, _, oldNotes, verr := in.verifyNote(oldData)
		r.notes = appendNew(r.notes, oldNotes)
		if verr != nil {
			var ie *inputErr
			if errors.As(verr, &ie) {
				return e.inputError(cmd, *asJSON, ie.error)
			}
			return e.finishFailed(r, *asJSON, "old-checkpoint", verr)
		}
		oldCP = cp
	}
	var newData []byte
	if *newFlag != "" {
		newData, err = readArg(*newFlag, signednote.MaxNoteSize)
	} else {
		newData, err = in.readPublished("checkpoint", signednote.MaxNoteSize)
	}
	if err != nil {
		return e.finishPub(r, *asJSON, err)
	}
	newCP, signers, keyNotes, err := in.verifyNote(newData)
	r.notes = appendNew(r.notes, keyNotes)
	if err != nil {
		var ie *inputErr
		if errors.As(err, &ie) {
			return e.inputError(cmd, *asJSON, ie.error)
		}
		return e.finishFailed(r, *asJSON, "new-checkpoint", err)
	}
	r.ok("new-checkpoint", fmt.Sprintf("size %d, signed by %s", newCP.Size, describeSigners(signers)))
	if oldSigned {
		r.ok("old-checkpoint", fmt.Sprintf("size %d, signature verified", oldCP.Size))
	} else {
		r.ok("old-checkpoint", fmt.Sprintf("size %d, root given on the command line (not authenticated)", oldCP.Size))
	}
	if oldCP.Size > newCP.Size {
		return e.finishFailed(r, *asJSON, "sizes", fmt.Errorf("the new checkpoint has size %d, smaller than the old size %d: the log went back", newCP.Size, oldCP.Size))
	}
	r.ok("sizes", fmt.Sprintf("old %d, new %d", oldCP.Size, newCP.Size))
	r.info = map[string]any{"old_size": oldCP.Size, "new_size": newCP.Size, "old_root": format.EncodeHex(oldCP.Root[:]), "new_root": format.EncodeHex(newCP.Root[:])}
	r.notProved = []string{
		"that the entries behind the hashes are what the operator says: this checks the tree hashes only",
		"that nobody else was shown a different history: a split view needs a witness or a monitor",
	}
	if !oldSigned {
		r.notProved = append(r.notProved, "that the old size and root are the checkpoint you meant: they were given without a signature")
	}
	r.headline = fmt.Sprintf("the log at size %d extends the log at size %d.", newCP.Size, oldCP.Size)
	if oldCP.Size == 0 || oldCP.Size == newCP.Size {
		// The proof is empty: no tile is read, and the output must not say
		// that tiles were checked.
		if err := merkle.VerifyConsistency(oldCP.Size, newCP.Size, oldCP.Root, newCP.Root, nil); err != nil {
			return e.finishFailed(r, *asJSON, "consistency", err)
		}
		r.ok("tiles", "no tile was needed: the proof is empty, nothing was read from the source")
		if oldCP.Size == 0 {
			r.ok("consistency", "the empty tree is a prefix of every tree: there is nothing to prove")
		} else {
			r.ok("consistency", fmt.Sprintf("both checkpoints have size %d and the same root", oldCP.Size))
			r.headline = fmt.Sprintf("the log at size %d is the same as the log at size %d.", newCP.Size, oldCP.Size)
		}
		return e.finishOK(r, *asJSON)
	}
	d, err := in.openSource()
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	reader, err := tlog.NewReader(tlog.FSSource{Files: logFiles{d, in.spec.rel()}}, newCP.Size)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	path, err := reader.ConsistencyProof(oldCP.Size)
	if err != nil {
		switch {
		case errors.Is(err, tlog.ErrMissing):
			return e.finishIncomplete(r, *asJSON, []string{"the publication does not contain a tile the proof needs: " + err.Error() + " (under " + in.spec.rel() + ")"})
		case errors.Is(err, tlog.ErrBadTile):
			return e.finishFailed(r, *asJSON, "tiles", err)
		}
		return e.inputError(cmd, *asJSON, err)
	}
	r.ok("tiles", fmt.Sprintf("read %d tile(s) on the proof path of the tree of size %d: %d proof hash(es); no other tile was read", reader.TilesRead(), newCP.Size, len(path)))
	if err := merkle.VerifyConsistency(oldCP.Size, newCP.Size, oldCP.Root, newCP.Root, path); err != nil {
		return e.finishFailed(r, *asJSON, "consistency", err)
	}
	r.ok("consistency", "RFC 9162 2.1.4.2: both roots follow from the proof hashes")
	r.notProved = append(r.notProved, "the tile directory as a whole: only the tiles on the proof path were read, a damaged tile elsewhere is not noticed")
	return e.finishOK(r, *asJSON)
}

// logFiles reads the files below one log directory of a source.
type logFiles struct {
	d   *safefile.Dir
	rel string
}

func (l logFiles) ReadFile(name string, limit int64) ([]byte, error) {
	return l.d.ReadFile(l.rel+"/"+name, limit)
}

// appendNew appends the notes that are not in the list yet: the old and the
// new checkpoint are verified with the same keys and give the same notes.
func appendNew(list, add []string) []string {
	for _, a := range add {
		dup := false
		for _, l := range list {
			if l == a {
				dup = true
				break
			}
		}
		if !dup {
			list = append(list, a)
		}
	}
	return list
}
