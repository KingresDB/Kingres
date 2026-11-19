// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/checkpoint"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/format"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/logsjson"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/merkle"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/pubsrc"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/signednote"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/tlog"
)

// ---- bundle fetch ----

// The ceilings of one fetch: what a server (or an unsigned checkpoint, whose size nothing vouches for) can make this
// program write is bounded, and the auditor can raise the bounds.
const (
	defaultMaxFetchFiles = 100000
	defaultMaxFetchBytes = int64(2) << 30
)

type bundleFetchArgs struct {
	source, out, trust string
	logs               []string
	since              uint64
	now                nowArg
	maxFiles           int
	maxBytes           int64
}

func (e *env) runBundleFetch(args []string) int {
	fs, asJSON := e.newFlagsWithExit("bundle fetch", "--source URL --out DIR [--log LOG ...] [--trust FILE] [--since-size N] [--max-files N] [--max-bytes N] [--now TIME]",
		"Mirror logs of a publication to a directory in the layout of the publication: keyring.json, events/*, and for each\n"+
			"log its checkpoint, the tiles of that size and the entry bundles. The logs are the --log options, chain:<64 hex>:e<N>\n"+
			"or aggregate:<64 hex>, or, when none is given, the logs of the publication's logs.json (format 1 only; another\n"+
			"format is refused, not guessed). With --since-size N the tiles and bundles that end at or before leaf N are not\n"+
			"fetched (you hold them from an earlier fetch). Names are made from numbers only, never from what the server says.\n"+
			"With --trust every checkpoint is verified (under the keys the events of the copy leave valid) and the tiles are bound to\n"+
			"the signed root; without it the copy is NOT verified and the output says so. What the directory already holds is\n"+
			"compared before it is replaced: a checkpoint that is smaller than the held one (rollback), or of the same size and\n"+
			"another root (fork), an event whose text is not the held one, or events that are no longer served, are status 1 and\n"+
			"nothing is replaced (with --trust, a larger checkpoint must also be proved to extend the held one from the tiles of\n"+
			"the copy). A tile that is already here with the right size is kept without being read, and counted as kept, not as fetched.\n"+
			"The run writes at most --max-files files (default 100000) and --max-bytes bytes (default 2 GiB): a checkpoint that\n"+
			"claims a size that needs more files is refused before one tile is asked for (status 2), whoever signed it; raise\n"+
			"the ceilings when the log really is that big. Run bundle verify on the copy. Redirects are never followed; every\n"+
			"request has a timeout.", exitTextWith3)
	source := fs.String("source", "", "https address of the publication (or a directory)")
	out := fs.String("out", "", "directory to write")
	trustPath := fs.String("trust", "", "trust file: verify the checkpoints and bind the tiles to them")
	var logs logListFlag
	fs.Var(&logs, "log", "a log to fetch (repeatable); without it the logs of logs.json")
	since := fs.Uint64("since-size", 0, "skip tiles and bundles that end at or before this leaf")
	maxFiles := fs.Int("max-files", defaultMaxFetchFiles, "the most files this run writes")
	maxBytes := fs.Int64("max-bytes", defaultMaxFetchBytes, "the most bytes this run writes")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "bundle fetch"
	r := &result{cmd: cmd, word: "FETCHED"}
	o := e.bundleFetchRun(r, bundleFetchArgs{source: *source, out: *out, trust: *trustPath, logs: logs, since: *since, now: nowOf(fs, nowFlag), maxFiles: *maxFiles, maxBytes: *maxBytes})
	switch o.kind {
	case outInput:
		return e.inputErrorInfo(cmd, *asJSON, o.err, r.inputInfo())
	case outFailed:
		return e.finishFailed(r, *asJSON, o.step, o.err)
	case outIncomplete:
		return e.finishIncomplete(r, *asJSON, o.missing)
	}
	return e.finishOK(r, *asJSON)
}

// limitError is a ceiling of the run that was reached.
type limitError struct{ msg string }

func (e *limitError) Error() string { return e.msg }

// fetcher writes what it reads below one directory (os.Root: no path leaves it).
type fetcher struct {
	src      pubsrc.Source
	root     *os.Root
	files    int   // files written
	bytes    int64 // bytes written
	kept     int   // tiles that were here already and were not read
	maxFiles int
	maxBytes int64
}

// store writes data below the root through a temporary name and a rename, within the ceilings.
func (f *fetcher) store(rel string, data []byte) error {
	if f.files+1 > f.maxFiles {
		return &limitError{fmt.Sprintf("the run has written %d file(s), the ceiling --max-files: nothing more is written (raise it if the log is really that big)", f.files)}
	}
	if f.bytes+int64(len(data)) > f.maxBytes {
		return &limitError{fmt.Sprintf("the run has written %d byte(s) and %s has %d more, over the ceiling --max-bytes %d: nothing more is written (raise it if the log is really that big)", f.bytes, rel, len(data), f.maxBytes)}
	}
	if dir := filepath.Dir(filepath.FromSlash(rel)); dir != "." {
		if err := f.root.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("writing %s: %w", rel, err)
		}
	}
	tmp := rel + ".part"
	if err := f.root.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	if err := f.root.Rename(tmp, rel); err != nil {
		_ = f.root.Remove(tmp)
		return fmt.Errorf("writing %s: %w", rel, err)
	}
	f.files++
	f.bytes += int64(len(data))
	return nil
}

// held returns the file of the output directory, or nil when there is none. A file that is not a regular
// file, or is larger than limit, is an error: it is not replaced.
func (f *fetcher) held(rel string, limit int64) ([]byte, error) {
	st, err := f.root.Lstat(rel)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Size() > limit {
		return nil, fmt.Errorf("%s is not a regular file of at most %d bytes", rel, limit)
	}
	return f.root.ReadFile(rel)
}

// get reads rel from the source and writes it; an immutable file that is here with the size it must have is kept
// without being read from the source (data nil, counted as kept).
func (f *fetcher) get(rel string, limit int64, skipIfSize int64) (data []byte, err error) {
	if skipIfSize > 0 {
		if st, serr := f.root.Stat(rel); serr == nil && st.Mode().IsRegular() && st.Size() == skipIfSize {
			f.kept++
			return nil, nil
		}
	}
	data, err = f.src.ReadFile(rel, limit)
	if err != nil {
		return nil, err
	}
	if err := f.store(rel, data); err != nil {
		return nil, err
	}
	return data, nil
}

// noteText is the signed text of an event or checkpoint note as held: up to and including the line before the
// blank line (an event is identified by its text, never by its signature lines).
func noteText(b []byte) []byte {
	if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
		return b[:i+1]
	}
	return b
}

// heldCheckpoint parses the text of a checkpoint note of the output directory (nothing is believed: the size and
// root are what the note claims).
func heldCheckpoint(note []byte, rel string) (*checkpoint.Checkpoint, error) {
	n, err := signednote.Parse(note)
	if err != nil {
		return nil, err
	}
	cp, err := checkpoint.Parse(n.Text)
	if err != nil || !strings.HasSuffix(cp.Origin, "/"+rel) {
		return nil, errors.New("it is not a checkpoint of this log")
	}
	return cp, nil
}

func (e *env) bundleFetchRun(r *result, a bundleFetchArgs) outcome {
	switch {
	case a.source == "":
		return inputOut(errors.New("missing --source"))
	case a.out == "":
		return inputOut(errors.New("missing --out"))
	case a.maxFiles < 1 || a.maxBytes < 1:
		return inputOut(errors.New("--max-files and --max-bytes must be positive"))
	}
	var specs []logSpec
	for _, l := range a.logs {
		s, err := parseLogSpec(l)
		if err != nil {
			return inputOut(err)
		}
		if s.kind == "chain" && !s.hasEpoch {
			return inputOut(fmt.Errorf("--log %s: give the epoch, chain:%s:e<N>", l, s.id))
		}
		specs = append(specs, s)
	}
	src, err := pubsrc.Open(a.source, pubsrc.Options{})
	if err != nil {
		return inputOut(fmt.Errorf("--source: %w", err))
	}
	defer src.Close()
	if err := os.MkdirAll(a.out, 0o755); err != nil {
		return inputOut(fmt.Errorf("--out %s: %w", a.out, err))
	}
	root, err := os.OpenRoot(a.out)
	if err != nil {
		return inputOut(fmt.Errorf("--out %s: %w", a.out, err))
	}
	defer root.Close()
	f := &fetcher{src: src, root: root, maxFiles: a.maxFiles, maxBytes: a.maxBytes}
	r.notes = append(r.notes, fmt.Sprintf("ceilings of this run: at most %d file(s) and %d byte(s) are written (--max-files, --max-bytes); a log that needs more is refused, not cut", a.maxFiles, a.maxBytes))
	var p *pubInputs
	if a.trust != "" {
		if p, err = loadPubInputs(a.trust, "", a.now); err != nil {
			return inputOut(err)
		}
		r.notes = append(r.notes, p.notes...)
	}
	var inc []string
	// keyring: a copy that is not used by this program; a change is said
	if old, herr := f.held("keyring.json", 1<<20); herr != nil {
		return failedOut("held", herr)
	} else if data, err := src.ReadFile("keyring.json", 1<<20); err == nil {
		if old != nil && !bytes.Equal(old, data) {
			r.notes = append(r.notes, "keyring.json differs from the copy held, and was replaced (this program does not use it: keys come from your trust file and the events)")
		}
		if old == nil || !bytes.Equal(old, data) {
			if err := f.store("keyring.json", data); err != nil {
				return fetchOutcome(err, "keyring.json")
			}
		}
	} else if !absent(err) {
		return fetchOutcome(err, "keyring.json")
	}
	// events: each is compared with the held one before it is written
	nev := 0
	for seq := uint64(0); seq < events.MaxEvents; seq++ {
		name := fmt.Sprintf("events/%06d", seq)
		old, herr := f.held(name, signednote.MaxNoteSize)
		if herr != nil {
			return failedOut("held", herr)
		}
		data, err := src.ReadFile(name, signednote.MaxNoteSize)
		if err != nil {
			if absent(err) {
				if old != nil {
					return failedOut("events", fmt.Errorf("event %06d is held in %s and the source does not serve it any more: the publication went back, or events were removed (nothing was replaced)", seq, a.out))
				}
				break
			}
			return fetchOutcome(err, name)
		}
		if old != nil && !bytes.Equal(noteText(old), noteText(data)) {
			return failedOut("events", fmt.Errorf("event %06d is not the one held in %s: its text changed, so two different events carry one number (the held one was kept)", seq, a.out))
		}
		if old == nil || !bytes.Equal(old, data) {
			if err := f.store(name, data); err != nil {
				return fetchOutcome(err, name)
			}
		}
		nev++
	}
	r.ok("events", fmt.Sprintf("%d event(s) copied", nev))
	if p != nil {
		// the events of the copy decide which keys may vouch for the checkpoints that follow
		mir, err := pubsrc.OpenDir(a.out)
		if err != nil {
			return inputOut(err)
		}
		w, werr := readEvents(p.base, mir, false)
		_ = mir.Close()
		if werr != nil {
			return eventsGateOutcome(werr)
		}
		var kn []string
		p.trust, kn = applyKeyEvents(p.base, w)
		r.notes = appendNew(r.notes, kn)
		r.notes = appendNew(r.notes, skippedNotes(w))
	}
	// the logs: --log, or logs.json
	if len(specs) == 0 {
		raw, err := src.ReadFile("logs.json", logsjson.MaxSize)
		if err != nil {
			if absent(err) {
				return inputOut(errors.New("name the logs to fetch with --log: the publication has no logs.json and an HTTP source has no listing"))
			}
			return fetchOutcome(err, "logs.json")
		}
		list, err := logsjson.Parse(raw)
		if err != nil {
			return inputOut(fmt.Errorf("logs.json of the source: %w (name the logs with --log)", err))
		}
		if len(list) == 0 {
			return inputOut(errors.New("logs.json lists no log: nothing to fetch"))
		}
		for _, l := range list {
			if p != nil && l.Prefix != p.trust.OriginPrefix {
				return failedOut("logs-json", fmt.Errorf("logs.json lists the origin %q and the trust file pins the prefix %q: this is a publication of another cluster", l.Origin, p.trust.OriginPrefix))
			}
			if l.Kind == "chain" {
				specs = append(specs, chainSpec(l.ID, l.Epoch))
			} else {
				specs = append(specs, aggSpec(l.ID))
			}
			if l.State != "active" {
				r.notes = append(r.notes, fmt.Sprintf("logs.json lists %s as %s (the publisher's own word; the checkpoint and the aggregate say what is true)", l.Origin, l.State))
			}
		}
		if err := f.store("logs.json", raw); err != nil {
			return fetchOutcome(err, "logs.json")
		}
		r.ok("logs-json", fmt.Sprintf("logs.json (format 1) lists %d log(s); it is an index and nothing in it was believed", len(list)))
	}
	for _, s := range specs {
		o := e.fetchOneLog(r, f, p, a, src, s, &inc)
		if !o.ok() {
			return o
		}
	}
	r.info = map[string]any{"files": f.files, "bytes": f.bytes, "kept": f.kept, "verified": p != nil, "out": a.out, "max_files": a.maxFiles, "max_bytes": a.maxBytes}
	r.headline = fmt.Sprintf("%d file(s), %d byte(s) copied to %s.", f.files, f.bytes, a.out)
	if f.kept > 0 {
		r.notes = append(r.notes, fmt.Sprintf("%d tile file(s) were here already with the size they must have and were kept without being read (they are not counted as fetched)", f.kept))
		if p == nil {
			r.notes = append(r.notes, "the tiles that were kept were not compared with anything: no --trust was given")
		}
	}
	if p == nil {
		r.notes = append(r.notes, "the copy is NOT verified: no --trust was given")
		r.notProved = append(r.notProved, "everything: no --trust was given, so no signature was verified and the tiles were not bound to a root; run bundle verify on the copy")
	} else {
		r.notProved = append(r.notProved, "the entries behind the tiles and the events: bundle verify checks them")
	}
	if len(inc) > 0 {
		return outcome{kind: outIncomplete, missing: inc}
	}
	return outcome{}
}

// fetchOneLog copies one log: its checkpoint (compared with the one held before anything is replaced), the tiles and
// the entry bundles of that size, and, with a trust file, binds them to the signed root and proves the held checkpoint
// is a prefix of the new one.
func (e *env) fetchOneLog(r *result, f *fetcher, p *pubInputs, a bundleFetchArgs, src pubsrc.Source, s logSpec, inc *[]string) outcome {
	rel := s.rel()
	note, err := src.ReadFile(rel+"/checkpoint", signednote.MaxNoteSize)
	if err != nil {
		return fetchOutcome(err, rel+"/checkpoint")
	}
	var cp *checkpoint.Checkpoint
	if p != nil {
		in := p.logIn(s, src)
		c, _, notes, verr := in.verifyNote(note)
		r.notes = appendNew(r.notes, notes)
		if verr != nil {
			var ie *inputErr
			if errors.As(verr, &ie) {
				return inputOut(ie.error)
			}
			step := "checkpoint"
			var ce *checkpoint.Error
			if errors.As(verr, &ce) {
				step = ce.Step
			}
			return failedOut(step, fmt.Errorf("%s: %w", rel, verr))
		}
		cp = c
	} else {
		n, err := signednote.Parse(note)
		if err != nil {
			return failedOut("parse", fmt.Errorf("%s: %w", rel, err))
		}
		c, err := checkpoint.Parse(n.Text)
		if err != nil || !strings.HasSuffix(c.Origin, "/"+rel) {
			return failedOut("checkpoint", fmt.Errorf("%s: the note is not a checkpoint of this log (the signature was not checked: no --trust)", rel))
		}
		cp = c
	}
	claim := "signed"
	if p == nil {
		claim = "claimed (unsigned: no --trust)"
	}
	// what the directory holds is compared before it is replaced
	heldRaw, herr := f.held(rel+"/checkpoint", signednote.MaxNoteSize)
	if herr != nil {
		return failedOut("held", fmt.Errorf("%s: %w (it was not replaced: move it away if you want a new copy)", rel, herr))
	}
	var held *checkpoint.Checkpoint
	if heldRaw != nil {
		if held, err = heldCheckpoint(heldRaw, rel); err != nil {
			return failedOut("held", fmt.Errorf("%s/checkpoint in %s does not read as a checkpoint of this log (%w): it was not replaced, move it away if you want a new copy", rel, a.out, err))
		}
		switch {
		case cp.Size < held.Size:
			f.evidence(rel, note)
			return failedOut("rollback", fmt.Errorf("%s: the checkpoint served has size %d (%s) and the copy you hold has size %d: the log went back, or an old checkpoint is being shown (the held one was kept, the served one is saved as %s/checkpoint.conflict)", rel, cp.Size, claim, held.Size, rel))
		case cp.Size == held.Size && cp.Root != held.Root:
			f.evidence(rel, note)
			return failedOut("fork", fmt.Errorf("%s: the checkpoint served and the copy you hold have the same size %d and different roots (%x held, %x served, %s): the log has two histories (the held one was kept, the served one is saved as %s/checkpoint.conflict)", rel, cp.Size, held.Root, cp.Root, claim, rel))
		}
	}
	// the size of the checkpoint, signed or not, decides how many files are asked for: it is checked against the ceiling first
	if need := plannedFiles(cp.Size, a.since); need > uint64(a.maxFiles-f.files) { // #nosec G115 -- maxFiles >= files
		return inputOut(fmt.Errorf("%s: the checkpoint's size %d (%s) needs %d tile and entry file(s), and the ceiling --max-files %d leaves %d: nothing was asked for. Raise --max-files and --max-bytes if the log really is that big", rel, cp.Size, claim, need, a.maxFiles, a.maxFiles-f.files))
	}
	if err := f.store(rel+"/checkpoint", note); err != nil {
		return fetchOutcome(err, rel+"/checkpoint")
	}
	restore := func() {
		if heldRaw != nil {
			_ = f.root.WriteFile(rel+"/checkpoint", heldRaw, 0o644)
		}
	}
	nt, err := f.fetchTiles(rel, cp.Size, a.since, s.kind)
	if err != nil {
		restore()
		return fetchOutcome(err, rel)
	}
	r.ok("log "+s.kindAndShort(), fmt.Sprintf("checkpoint of size %d and %d tile/bundle file(s) copied", cp.Size, nt))
	if p == nil {
		if held != nil && held.Size < cp.Size {
			r.notes = appendNew(r.notes, []string{fmt.Sprintf("%s: the held checkpoint (size %d) was replaced by a larger one (size %d) and that it extends the held one was not checked: no --trust", rel, held.Size, cp.Size)})
		}
		return outcome{}
	}
	mp := &pubInputs{trust: p.trust, base: p.base, spec: a.out, now: p.now}
	if mp.src, err = pubsrc.OpenDir(a.out); err != nil {
		restore()
		return inputOut(err)
	}
	defer mp.close()
	ol, _, out := mp.openLog(s, nil)
	if out.ok() {
		out = bindLog(ol, a.since)
	}
	if !out.ok() {
		restore()
		return out
	}
	r.ok("bind "+s.kindAndShort(), "the tiles copied lead to the signed root")
	if held != nil && held.Size < cp.Size {
		switch {
		case held.Size == 0:
			r.notes = appendNew(r.notes, []string{fmt.Sprintf("%s: the held checkpoint has size 0: nothing was compared (an empty log is a prefix of every log), the size-0 held copy proves nothing about the history", rel)})
		default:
			path, err := ol.reader.ConsistencyProofBetween(held.Size, cp.Size)
			if err != nil {
				restore()
				o := tileOutcome(err, rel)
				if o.kind == outIncomplete {
					*inc = append(*inc, fmt.Sprintf("%s: that the new checkpoint extends the held one (size %d) could not be checked: %s (the held checkpoint was put back)", rel, held.Size, strings.Join(o.missing, "; ")))
					return outcome{}
				}
				return o
			}
			if err := merkle.VerifyConsistency(held.Size, cp.Size, held.Root, cp.Root, path); err != nil {
				restore()
				f.evidence(rel, note)
				return failedOut("fork", fmt.Errorf("%s: the log at size %d does not extend the copy you hold at size %d: %w (the held checkpoint was put back, the served one is saved as %s/checkpoint.conflict)", rel, cp.Size, held.Size, err, rel))
			}
			r.ok("extends "+s.kindAndShort(), fmt.Sprintf("RFC 9162 2.1.4.2: the checkpoint of size %d extends the one you held at size %d", cp.Size, held.Size))
		}
	}
	return outcome{}
}

// evidence saves a conflicting note beside the held one (best effort: the finding stands without it).
func (f *fetcher) evidence(rel string, note []byte) {
	_ = f.root.WriteFile(rel+"/checkpoint.conflict", note, 0o644)
}

func fetchOutcome(err error, what string) outcome {
	var le *limitError
	switch {
	case errors.As(err, &le):
		return inputOut(fmt.Errorf("%s: %w", what, err))
	case errors.Is(err, pubsrc.ErrUnreachable) || absent(err):
		return incompleteOut(fmt.Sprintf("%s could not be fetched: %v", what, err))
	case errors.Is(err, pubsrc.ErrRedirect), errors.Is(err, pubsrc.ErrNoListing):
		return inputOut(err)
	}
	return failedOut("fetch", fmt.Errorf("%s: %w", what, err))
}

// tileSkip is how many full tiles of a level end at or before leaf since (they are not fetched).
func tileSkip(level int, since, full uint64) uint64 {
	if since == 0 {
		return 0
	}
	return min(full, since>>uint(tlog.TileHeight*(level+1)))
}

// plannedFiles is the number of files fetchTiles asks for: the full and partial tiles of every level and the entry bundles.
func plannedFiles(size, since uint64) uint64 {
	var n uint64
	for level := 0; tlog.TileHeight*level < 63; level++ {
		count := size >> uint(tlog.TileHeight*level)
		if count == 0 {
			break
		}
		full := count / tlog.TileWidth
		n += full - tileSkip(level, since, full)
		if count%tlog.TileWidth > 0 {
			n++
		}
	}
	full := size / tlog.TileWidth
	skip := uint64(0)
	if since > 0 {
		skip = min(full, since/tlog.TileWidth)
	}
	n += full - skip
	if size%tlog.TileWidth > 0 {
		n++
	}
	return n
}

// fetchTiles copies the tiles and entry bundles of the tree of a size.
func (f *fetcher) fetchTiles(rel string, size, since uint64, kind string) (int, error) {
	n := 0
	get := func(name string, want int, immutable bool) error {
		skip := int64(0)
		if immutable {
			skip = int64(want)
		}
		data, err := f.get(rel+"/"+name, int64(want), skip)
		if err != nil {
			if absent(err) && strings.Contains(name, ".p/") {
				return nil // a partial tile may be gone once the full tile exists (tlog-tiles)
			}
			return err
		}
		if data != nil {
			n++
		}
		return nil
	}
	for level := 0; tlog.TileHeight*level < 63; level++ {
		count := size >> uint(tlog.TileHeight*level)
		if count == 0 {
			break
		}
		full := count / tlog.TileWidth
		for i := tileSkip(level, since, full); i < full; i++ {
			if err := get(tlog.TilePath(level, i, tlog.TileWidth), tlog.FullTile, true); err != nil {
				return n, err
			}
		}
		if w := int(count % tlog.TileWidth); w > 0 {
			if err := get(tlog.TilePath(level, full, w), w*tlog.HashSize, false); err != nil {
				return n, err
			}
		}
	}
	maxEntry := format.BlockLeafSize
	if kind == "aggregate" {
		maxEntry = 1 << 16
	}
	full := size / tlog.TileWidth
	start := uint64(0)
	if since > 0 {
		start = min(full, since/tlog.TileWidth)
	}
	for i := start; i < full; i++ {
		if _, err := f.get(rel+"/"+tlog.EntriesPath(i, tlog.TileWidth), int64(tlog.TileWidth)*int64(2+maxEntry), 0); err != nil {
			return n, err
		}
		n++
	}
	if w := int(size % tlog.TileWidth); w > 0 {
		if _, err := f.get(rel+"/"+tlog.EntriesPath(full, w), int64(w)*int64(2+maxEntry), 0); err != nil && !absent(err) {
			return n, err
		} else if err == nil {
			n++
		}
	}
	return n, nil
}
