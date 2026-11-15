// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/trust"
)

// exitTextWith3 is the exit status line of the commands that can end with
// status 3.
const exitTextWith3 = "0 verified, 1 verification failed, 2 usage or input error,\n3 incomplete (nothing failed, but something asked for could not be checked)."

// ---- the log named on the command line ----

// logSpec is the value of --log: chain:<64 hex>[:e<N>] or aggregate:<64 hex>.
type logSpec struct {
	kind     string // "chain" or "aggregate"
	id       string
	epoch    uint64
	hasEpoch bool
}

var logSpecRE = regexp.MustCompile(`^(chain|aggregate):([0-9a-f]{64})(?::e(0|[1-9][0-9]{0,8}))?$`)

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

// resolveEpoch finds the epoch of a chain log from the directories of a
// source when --log does not carry one. It refuses to guess when there is
// not exactly one: an old closed epoch must never be taken for the current one.
func (l logSpec) resolveEpoch(source string) (logSpec, error) {
	if l.kind != "chain" || l.hasEpoch {
		return l, nil
	}
	entries, err := os.ReadDir(filepath.Join(source, "c", l.id))
	if err != nil {
		return l, fmt.Errorf("cannot find the epoch of the chain log in %s: %w; give --log chain:%s:e<N>", source, err, l.id)
	}
	var found []uint64
	for _, en := range entries {
		name := en.Name()
		if m := regexp.MustCompile(`^e([1-9][0-9]{0,8})$`).FindStringSubmatch(name); m != nil && en.IsDir() {
			n, _ := strconv.ParseUint(m[1], 10, 64)
			found = append(found, n)
		}
	}
	if len(found) != 1 {
		return l, fmt.Errorf("%s has %d epochs of the chain log, expected exactly one; give --log chain:%s:e<N>", source, len(found), l.id)
	}
	l.epoch, l.hasEpoch = found[0], true
	return l, nil
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

// ---- the result of a command with the exit status 3 ----

type result struct {
	cmd       string
	headline  string // after VERIFIED:
	steps     []stepJSON
	info      map[string]any
	notes     []string
	notProved []string
}

func (r *result) ok(name, detail string) {
	r.steps = append(r.steps, stepJSON{Name: name, OK: true, Detail: detail})
}

func (e *env) printSteps(steps []stepJSON) {
	for _, s := range steps {
		if s.OK {
			fmt.Fprintf(e.stdout, "  ok  %-16s %s\n", s.Name, s.Detail)
		}
	}
}

func (e *env) finishOK(r *result, asJSON bool) int {
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Verified: true, Steps: r.steps, Info: r.info, NotProved: r.notProved})
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "VERIFIED: %s\n", r.headline)
	e.printSteps(r.steps)
	for _, n := range r.notes {
		fmt.Fprintf(e.stdout, "  note: %s\n", n)
	}
	for _, n := range r.notProved {
		fmt.Fprintf(e.stdout, "  not proved: %s\n", n)
	}
	return ExitOK
}

func (e *env) finishFailed(r *result, asJSON bool, step string, err error) int {
	steps := append(append([]stepJSON{}, r.steps...), stepJSON{Name: step, OK: false, Detail: err.Error()})
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Steps: steps, FailedAt: step, Error: err.Error(), Info: r.info})
		return ExitFailed
	}
	fmt.Fprintf(e.stdout, "FAILED at step %s: %v\n", step, err)
	e.printSteps(r.steps)
	return ExitFailed
}

func (e *env) finishIncomplete(r *result, asJSON bool, missing []string) int {
	if asJSON {
		e.printJSON(jsonOutput{Command: r.cmd, Steps: r.steps, Incomplete: true, Missing: missing, Info: r.info, NotProved: r.notProved})
		return ExitIncomplete
	}
	fmt.Fprintln(e.stdout, "INCOMPLETE: nothing failed, but a check that was asked for could not be made.")
	e.printSteps(r.steps)
	for _, m := range missing {
		fmt.Fprintf(e.stdout, "  missing: %s\n", m)
	}
	for _, n := range r.notProved {
		fmt.Fprintf(e.stdout, "  not proved: %s\n", n)
	}
	return ExitIncomplete
}

// ---- shared loading ----

type logInputs struct {
	trust  *trust.File
	spec   logSpec
	source string
	now    time.Time
	notes  []string
}

// loadLogInputs reads --trust, --log, --source and --now. Errors are input
// errors (exit status 2).
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
	data, err := readLimited(trustPath, trust.MaxFileSize)
	if err != nil {
		return nil, err
	}
	if in.trust, err = trust.Parse(data); err != nil {
		return nil, fmt.Errorf("trust file %s: %w", trustPath, err)
	}
	if len(in.trust.Keys) == 0 {
		return nil, fmt.Errorf("trust file %s has no keys: nothing can be verified", trustPath)
	}
	var fixed bool
	if in.now, fixed, err = parseNow(nowFlag); err != nil {
		return nil, err
	}
	if !fixed {
		in.notes = append(in.notes, "time used for key validity windows: system clock "+in.now.Format(time.RFC3339))
	}
	if !in.spec.hasEpoch && in.spec.kind == "chain" {
		if noteGiven {
			return nil, fmt.Errorf("give the epoch with --log chain:%s:e<N> when the checkpoint is a file", in.spec.id)
		}
		if in.spec, err = in.spec.resolveEpoch(source); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// verifyNote verifies note data as the checkpoint of the log.
func (in *logInputs) verifyNote(note []byte) (*checkpoint.Checkpoint, []signednote.Key, []string, error) {
	origin := in.spec.origin(in.trust.OriginPrefix)
	keys, notes, err := in.trust.SignerKeys(origin, in.now)
	if err != nil {
		return nil, nil, notes, &inputErr{err}
	}
	cp, signers, err := checkpoint.Verify(note, keys, origin)
	return cp, signers, notes, err
}

// inputErr marks an error that is an input problem (exit status 2) inside a
// function that otherwise returns verification failures.
type inputErr struct{ error }

func (in *logInputs) checkpointPath() string {
	return filepath.Join(in.source, filepath.FromSlash(in.spec.rel()), "checkpoint")
}

func describeSigners(signers []signednote.Key) string {
	var parts []string
	for _, k := range signers {
		parts = append(parts, "key "+trust.Fingerprint(k.Public)[:16])
	}
	return strings.Join(parts, ", ")
}

// ---- checkpoint ----

func (e *env) runCheckpoint(args []string) int {
	fs, asJSON := e.newFlagsWithExit("checkpoint", "--trust FILE --log chain:<id>[:e<N>]|aggregate:<id> [--source DIR | --note FILE] [--min-size N] [--now TIME] [--json]",
		"Verify a signed checkpoint of a tile log: the note is well formed, at least one signature is\n"+
			"by a key of the trust file, no signature of such a key is wrong, the origin is the one of the\n"+
			"named log below the trusted prefix, and the size and root are well formed. The checkpoint is\n"+
			"read from --note or from <source>/c/<id>/e<N>/checkpoint (aggregate: a/<id>/checkpoint).\n"+
			"It does not check any tile; use consistency for that.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	logFlag := fs.String("log", "", "the log: chain:<64 hex>[:e<N>] or aggregate:<64 hex>")
	source := fs.String("source", ".", "directory of the publication or bundle")
	note := fs.String("note", "", "checkpoint file to verify instead of the one in --source")
	minSize := fs.Uint64("min-size", 0, "fail when the tree size is smaller than this")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "checkpoint"
	in, err := loadLogInputs(*trustPath, *logFlag, *source, *nowFlag, *note != "")
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	path := *note
	if path == "" {
		path = in.checkpointPath()
	}
	data, err := readLimited(path, signednote.MaxNoteSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	r := &result{cmd: cmd}
	cp, signers, keyNotes, err := in.verifyNote(data)
	r.notes = append(in.notes, keyNotes...)
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
	r.headline = fmt.Sprintf("the checkpoint of %s at size %d is signed by a trusted key.", cp.Origin, cp.Size)
	r.info = map[string]any{
		"origin": cp.Origin, "size": cp.Size, "root": format.EncodeHex(cp.Root[:]),
		"signers": signerPrints(signers), "extension_lines": len(cp.Extensions),
	}
	r.notProved = []string{
		"the tiles and entries of the log: this checks the signed size and root only (use consistency and, later, proof with a bundle)",
		"when the checkpoint was made: no anchor is checked, the time is the signer's claim",
		"that the signer shows the same history to everyone: a split view needs witnesses or a monitor",
		"that the trust file was obtained outside the cluster: that is your responsibility",
	}
	return e.finishOK(r, *asJSON)
}

func signerPrints(signers []signednote.Key) []string {
	out := []string{}
	for _, k := range signers {
		out = append(out, trust.Fingerprint(k.Public))
	}
	return out
}

// ---- consistency ----

var sizeRootRE = regexp.MustCompile(`^(0|[1-9][0-9]{0,18}):([A-Za-z0-9+/=]+)$`)

func (e *env) runConsistency(args []string) int {
	fs, asJSON := e.newFlagsWithExit("consistency", "--trust FILE --log <log> --old NOTEFILE|SIZE:ROOT [--new NOTEFILE] [--source DIR] [--now TIME] [--json]",
		"Check that the log at the new checkpoint extends the log at the old one (RFC 9162 section\n"+
			"2.1.4.2). The new checkpoint is --new or the one in --source; both are verified like the\n"+
			"checkpoint command does. The old checkpoint is a note file (verified the same way) or\n"+
			"SIZE:ROOT with the root in base64 (taken as given: you hold it from elsewhere). The proof\n"+
			"hashes are read from the tiles under --source and checked against both roots. Missing tiles\n"+
			"give status 3, a tile that is wrong gives status 1.", exitTextWith3)
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
	if *oldFlag == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --old"))
	}
	in, err := loadLogInputs(*trustPath, *logFlag, *source, *nowFlag, *newFlag != "")
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
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
		oldData, err := readLimited(*oldFlag, signednote.MaxNoteSize)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		if _, err := signednote.Parse(oldData); err != nil {
			return e.inputError(cmd, *asJSON, fmt.Errorf("--old %s is not a signed note: %w", *oldFlag, err))
		}
		oldSigned = true
		cp, _, _, verr := in.verifyNote(oldData)
		if verr != nil {
			var ie *inputErr
			if errors.As(verr, &ie) {
				return e.inputError(cmd, *asJSON, ie.error)
			}
			r := &result{cmd: cmd, notes: in.notes}
			return e.finishFailed(r, *asJSON, "old-checkpoint", verr)
		}
		oldCP = cp
	}
	newPath := *newFlag
	if newPath == "" {
		newPath = in.checkpointPath()
	}
	newData, err := readLimited(newPath, signednote.MaxNoteSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	r := &result{cmd: cmd, notes: in.notes}
	newCP, signers, keyNotes, err := in.verifyNote(newData)
	r.notes = append(r.notes, keyNotes...)
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
	if newCP.Size == 0 {
		return e.finishOK(withHeadline(r, newCP, oldCP), *asJSON)
	}
	logDir := filepath.Join(in.source, filepath.FromSlash(in.spec.rel()))
	reader, err := tlog.NewReader(tlog.FSSource{FS: os.DirFS(logDir)}, newCP.Size)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	path, err := reader.ConsistencyProof(oldCP.Size)
	if err != nil {
		switch {
		case errors.Is(err, tlog.ErrMissing):
			return e.finishIncomplete(r, *asJSON, []string{err.Error() + " (under " + logDir + ")"})
		case errors.Is(err, tlog.ErrBadTile):
			return e.finishFailed(r, *asJSON, "tiles", err)
		}
		return e.inputError(cmd, *asJSON, err)
	}
	r.ok("tiles", fmt.Sprintf("proof of %d hash(es) read from the tiles of size %d", len(path), newCP.Size))
	if err := merkle.VerifyConsistency(oldCP.Size, newCP.Size, oldCP.Root, newCP.Root, path); err != nil {
		return e.finishFailed(r, *asJSON, "consistency", err)
	}
	r.ok("consistency", "RFC 9162 2.1.4.2: both roots follow from the proof")
	return e.finishOK(withHeadline(r, newCP, oldCP), *asJSON)
}

func withHeadline(r *result, newCP, oldCP *checkpoint.Checkpoint) *result {
	r.headline = fmt.Sprintf("the log at size %d extends the log at size %d.", newCP.Size, oldCP.Size)
	return r
}

// ---- trust ----

func (e *env) runTrust(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.stderr, "kingres-verify trust: expected init or show")
		return ExitUsage
	}
	switch args[0] {
	case "init":
		return e.runTrustInit(args[1:])
	case "show":
		return e.runTrustShow(args[1:])
	case "follow":
		fmt.Fprintln(e.stderr, "kingres-verify trust follow: not built yet (it needs the key events of the publication)")
		return ExitUsage
	case "-h", "--help", "help":
		fmt.Fprint(e.stdout, "Usage: kingres-verify trust init|show [flags]\n\n  init  write a trust file from a public key you typed or pasted and its fingerprint\n  show  print a trust file\n\nExit status: 0 ok, 2 usage or input error.\n")
		return ExitOK
	}
	fmt.Fprintf(e.stderr, "kingres-verify trust: unknown subcommand %q, expected init or show\n", args[0])
	return ExitUsage
}

func (e *env) runTrustInit(args []string) int {
	fs, asJSON := e.newFlagsWithExit("trust init", "--origin-prefix PREFIX (--public-key BASE64 | --public-key-file FILE) --fingerprint HEX [--name LABEL] [--out FILE]",
		"Write a new trust file with one pinned signing key. The public key and the fingerprint must\n"+
			"come from you (typed or pasted from the console), not from the publication being checked:\n"+
			"the command refuses to run without --fingerprint and refuses a key that does not match it.\n"+
			"It does not overwrite a file. --out - writes to the standard output.", "0 written, 2 usage or input error.")
	prefix := fs.String("origin-prefix", "", "origin prefix of the cluster, for example ledger.example.com/kingres")
	pubFlag := fs.String("public-key", "", "Ed25519 public key, base64 of 32 bytes")
	pubFile := fs.String("public-key-file", "", "file holding the base64 public key")
	fpFlag := fs.String("fingerprint", "", "SHA-256 of the public key in hex, as the console shows it")
	name := fs.String("name", "key-1", "label for the key (not the note key name)")
	out := fs.String("out", "trust.json", "file to write, or - for standard output")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "trust init"
	if *prefix == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --origin-prefix"))
	}
	if *fpFlag == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --fingerprint: type or paste the fingerprint the console shows, so that a wrong key is noticed"))
	}
	if (*pubFlag == "") == (*pubFile == "") {
		return e.inputError(cmd, *asJSON, errors.New("give exactly one of --public-key and --public-key-file"))
	}
	text := *pubFlag
	if *pubFile != "" {
		b, err := readLimited(*pubFile, 4096)
		if err != nil {
			return e.inputError(cmd, *asJSON, err)
		}
		text = strings.TrimSpace(string(b))
	}
	pub, err := signednote.DecodeBase64(text)
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("public key: %w", err))
	}
	f, err := trust.New(*prefix, *name, pub, *fpFlag)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	data, err := f.Marshal()
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	if *out == "-" {
		_, _ = e.stdout.Write(data)
		return ExitOK
	}
	fh, err := os.OpenFile(*out, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644) // #nosec G304 -- the path is a command line argument
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("not writing %s: %w", *out, err))
	}
	if _, err := fh.Write(data); err != nil {
		_ = fh.Close()
		return e.inputError(cmd, *asJSON, err)
	}
	if err := fh.Close(); err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	if *asJSON {
		e.printJSON(jsonOutput{Command: cmd, Verified: true, Info: map[string]any{"file": *out, "fingerprint": f.Keys[0].Fingerprint, "origin_prefix": f.OriginPrefix}})
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "wrote %s: origin prefix %s, key %s (fingerprint %s)\n", *out, f.OriginPrefix, f.Keys[0].Name, f.Keys[0].Fingerprint)
	fmt.Fprintln(e.stdout, "  note: keep this file where the cluster's administrators cannot change it; a trust file that came from the cluster proves nothing")
	return ExitOK
}

func (e *env) runTrustShow(args []string) int {
	fs, asJSON := e.newFlagsWithExit("trust show", "--trust FILE [--json]", "Validate and print a trust file.", "0 ok, 2 usage or input error.")
	path := fs.String("trust", "", "trust file (JSON)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "trust show"
	if *path == "" {
		return e.inputError(cmd, *asJSON, errors.New("missing --trust"))
	}
	data, err := readLimited(*path, trust.MaxFileSize)
	if err != nil {
		return e.inputError(cmd, *asJSON, err)
	}
	f, err := trust.Parse(data)
	if err != nil {
		return e.inputError(cmd, *asJSON, fmt.Errorf("trust file %s: %w", *path, err))
	}
	keys := []map[string]any{}
	for _, k := range f.Keys {
		m := map[string]any{"name": k.Name, "fingerprint": k.Fingerprint}
		if k.NotBefore != nil {
			m["not_before"] = k.NotBefore.Format(time.RFC3339)
		}
		if k.NotAfter != nil {
			m["not_after"] = k.NotAfter.Format(time.RFC3339)
		}
		keys = append(keys, m)
	}
	if *asJSON {
		e.printJSON(jsonOutput{Command: cmd, Verified: true, Info: map[string]any{"origin_prefix": f.OriginPrefix, "keys": keys, "tsa_roots": len(f.TSARoots), "client_keys": len(f.ClientKeys)}})
		return ExitOK
	}
	fmt.Fprintf(e.stdout, "trust file %s is valid\n  origin prefix: %s\n", *path, f.OriginPrefix)
	for _, k := range f.Keys {
		win := ""
		if k.NotBefore != nil {
			win += " not before " + k.NotBefore.Format(time.RFC3339)
		}
		if k.NotAfter != nil {
			win += " not after " + k.NotAfter.Format(time.RFC3339)
		}
		fmt.Fprintf(e.stdout, "  key %q fingerprint %s%s\n", k.Name, k.Fingerprint, win)
	}
	fmt.Fprintf(e.stdout, "  time-stamp authority roots: %d, client keys: %d\n", len(f.TSARoots), len(f.ClientKeys))
	fmt.Fprintln(e.stdout, "  note: compare the fingerprints with the ones the cluster's console shows, by a channel other than this file")
	return ExitOK
}
