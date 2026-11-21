// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/events"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/safefile"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/statefile"
)

// monitor (design 7.5, with the requirements of the review of the publication, section 5): a third
// party that turns "a copy of an old checkpoint exists elsewhere" into a standing control. For every
// log it watches it keeps one state file, written by exactly the code of `checkpoint --state` (the state
// moves only to a checkpoint proved to extend it, a rollback or a fork is status 1 and the conflicting note
// is saved beside it), and besides that it reads the new entries of the log and checks them, reads the
// events of the publication and compares them with the ones it saw, and keeps what it audited.
//
// The state is a directory (decision, correction 163): one file <log>.state per log, in the form of
// `checkpoint --state`, and monitor.json with what is not a checkpoint: how far the entries of each log
// were audited, the digest of the text of each event seen, and the chains that were retired.

const monitorMeta = "monitor.json"

type monitorMetaFile struct {
	Format  int               `json:"format"`
	Audited map[string]uint64 `json:"audited"`
	Events  map[string]string `json:"events"`  // number (6 digits) -> SHA-256 of the text of the event
	Retired map[string]uint64 `json:"retired"` // "<chain>:<epoch>" -> the round of the aggregate that flagged it, or the number of the event + 1<<40
}

func newMeta() *monitorMetaFile {
	return &monitorMetaFile{Format: 1, Audited: map[string]uint64{}, Events: map[string]string{}, Retired: map[string]uint64{}}
}

var stateNameRE = regexp.MustCompile(`^(chain-[0-9a-f]{64}-e[1-9][0-9]{0,8}|aggregate-[0-9a-f]{64})\.state$`)

func slugOf(l logSpec) string {
	if l.kind == "aggregate" {
		return "aggregate-" + l.id
	}
	return "chain-" + l.id + "-e" + strconv.FormatUint(l.epoch, 10)
}

func specOfSlug(s string) (logSpec, bool) {
	if strings.HasPrefix(s, "aggregate-") {
		return aggSpec(strings.TrimPrefix(s, "aggregate-")), true
	}
	rest := strings.TrimPrefix(s, "chain-")
	id, e, ok := strings.Cut(rest, "-e")
	if !ok {
		return logSpec{}, false
	}
	n, err := strconv.ParseUint(e, 10, 64)
	return chainSpec(id, n), err == nil
}

type logListFlag []string

func (l *logListFlag) String() string     { return strings.Join(*l, ",") }
func (l *logListFlag) Set(v string) error { *l = append(*l, v); return nil }

func (e *env) runMonitor(args []string) int {
	fs, asJSON := e.newFlagsWithExit("monitor", "--trust FILE --source SOURCE --state DIR [--log LOG ...] [--init] [--once] [--interval DURATION] [--polls N] [--allow-shared-state-dir] [--now TIME]",
		"Watch a publication from a machine that is not part of the cluster. For every log named in the state\n"+
			"directory (the first run: the --log flags) it fetches the checkpoint, verifies its signature, proves with\n"+
			"the tiles that it extends the one in the state (RFC 9162 2.1.4.2), reads the entries it has not audited\n"+
			"(block headers: height, prev_hash links, times not going back; aggregate leaves: sequence, times, a chain\n"+
			"flagged retired never in a later leaf) and moves the state forward. It reads every event and compares it\n"+
			"with the digest it kept: an event number seen twice with different text, an event that is gone, a chain\n"+
			"that a verified chain-retired event ended and that grows are findings. A rollback (a smaller checkpoint),\n"+
			"a fork (another root for a size) and a rewrite are status 1; an unreachable publication is status 3; the\n"+
			"state never moves backwards. Without --once it polls every --interval (default 60s) until --polls polls\n"+
			"were made (default: until it is stopped), and stops at the first finding.", exitTextWith3)
	trustPath := fs.String("trust", "", "trust file (JSON)")
	source := fs.String("source", "", "https address (or directory, or .tar.gz) of the publication")
	stateDir := fs.String("state", "", "state directory (private to you)")
	var logs logListFlag
	fs.Var(&logs, "log", "a log to watch: chain:<64 hex>:e<N> or aggregate:<64 hex> (repeatable; the first run)")
	once := fs.Bool("once", false, "poll once and exit")
	interval := fs.Duration("interval", 60*time.Second, "time between polls")
	polls := fs.Int("polls", 0, "stop after this many polls (0: until stopped)")
	initFlag := fs.Bool("init", false, "this is the first run: the state directory holds no record and no log state, and none is expected. Without it a missing or emptied monitor.json is an error, because what was seen is what makes a rollback visible")
	allowShared := fs.Bool("allow-shared-state-dir", false, "accept a state directory that others can write (Unix); refused by default")
	nowFlag := fs.String("now", "", "time for key validity windows, RFC 3339 (default: system clock)")
	if code, done := e.parseFlags(fs, args); done {
		return code
	}
	const cmd = "monitor"
	r := &result{cmd: cmd}
	o := e.monitorRun(r, monitorArgs{trust: *trustPath, source: *source, state: *stateDir, logs: logs, once: *once, interval: *interval, polls: *polls, allowShared: *allowShared, init: *initFlag, now: nowOf(fs, nowFlag)})
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

type monitorArgs struct {
	trust, source, state string
	logs                 []string
	once                 bool
	interval             time.Duration
	polls                int
	init                 bool
	allowShared          bool
	now                  nowArg
}

// sleepFn is replaced by tests.
var sleepFn = time.Sleep

func (e *env) monitorRun(r *result, a monitorArgs) outcome {
	switch {
	case a.source == "":
		return inputOut(errors.New("missing --source"))
	case a.state == "":
		return inputOut(errors.New("missing --state: the directory that keeps what was seen is what makes a rollback visible"))
	case a.polls < 0 || a.interval <= 0:
		return inputOut(errors.New("--polls must not be negative and --interval must be positive"))
	}
	if a.once {
		a.polls = 1
	}
	var specs []logSpec
	for _, l := range a.logs {
		s, err := parseLogSpec(l)
		if err != nil {
			return inputOut(err)
		}
		if s.kind == "chain" && !s.hasEpoch {
			return inputOut(fmt.Errorf("--log %s: give the epoch, chain:%s:e<N> (an HTTP source cannot be searched for it)", l, s.id))
		}
		specs = append(specs, s)
	}
	if err := os.MkdirAll(a.state, 0o700); err != nil {
		return inputOut(fmt.Errorf("--state %s: %w", a.state, err))
	}
	if perr := statefile.CheckPrivate(filepath.Join(a.state, monitorMeta)); perr != nil && !a.allowShared {
		return inputOut(fmt.Errorf("--state %s: %w: another user could replace or delete the state, or plant files in it. Keep it in a directory that only you can write (for example chmod 700), or pass --allow-shared-state-dir to accept the risk", a.state, perr))
	}
	for n := 0; a.polls == 0 || n < a.polls; n++ {
		if n > 0 {
			sleepFn(a.interval)
		}
		o := e.monitorPoll(r, a, specs, n+1)
		if o.kind != outOK {
			return o
		}
	}
	return outcome{}
}

// monitorPoll is one pass over every log. Its steps are added to r under the number of the poll.
func (e *env) monitorPoll(r *result, a monitorArgs, extra []logSpec, poll int) outcome {
	p, err := loadPubInputs(a.trust, a.source, a.now)
	if err != nil {
		return inputOut(err)
	}
	defer p.close()
	if poll == 1 {
		r.notes = append(r.notes, p.notes...)
	}
	// the logs: those of the state, and those named now
	set := map[string]logSpec{}
	ents, err := os.ReadDir(a.state)
	if err != nil {
		return inputOut(fmt.Errorf("--state %s: %w", a.state, err))
	}
	for _, en := range ents {
		if stateNameRE.MatchString(en.Name()) {
			if s, ok := specOfSlug(strings.TrimSuffix(en.Name(), ".state")); ok {
				set[slugOf(s)] = s
			}
		}
	}
	for _, s := range extra {
		set[slugOf(s)] = s
	}
	if len(set) == 0 {
		return inputOut(errors.New("no log to watch: the state directory holds none, and no --log was given"))
	}
	slugs := make([]string, 0, len(set))
	for s := range set {
		slugs = append(slugs, s)
	}
	sort.Strings(slugs)
	meta, metaRaw, merr := readMeta(a.state, a.init && poll == 1)
	if merr != nil {
		return inputOut(merr)
	}
	var first, inc *outcome
	note := func(o outcome) {
		switch o.kind {
		case outFailed, outInput:
			if first == nil {
				c := o
				first = &c
			}
		case outIncomplete:
			if inc == nil {
				c := o
				inc = &c
			} else {
				inc.missing = append(inc.missing, o.missing...)
			}
		}
	}
	// events first: the chains they retired are looked for in the logs below
	walk, eo := e.monitorEvents(r, p, meta, poll)
	note(eo)
	for _, s := range slugs {
		spec := set[s]
		code, sub, errText := e.checkpointSub(a, p, spec, filepath.Join(a.state, s+".state"))
		label := fmt.Sprintf("poll %d %s", poll, spec.kindAndShort())
		switch code {
		case ExitOK:
			info, _ := sub.Info.(map[string]any)
			st, _ := info["state"].(map[string]any)
			r.ok(label, fmt.Sprintf("checkpoint verified, size %v, state %v", info["size"], st["action"]))
			note(e.monitorAudit(r, p, spec, filepath.Join(a.state, s+".state"), s, meta, walk, label))
		case ExitIncomplete:
			note(incompleteOut(label + ": " + strings.Join(sub.Missing, "; ")))
		case ExitUsage:
			note(inputOut(fmt.Errorf("%s: %s", label, strings.TrimSpace(errText))))
		default:
			step := sub.FailedAt
			if step == "" {
				step = "checkpoint"
			}
			msg := sub.Error
			for _, n := range sub.Notes {
				if strings.HasPrefix(n, "evidence:") || strings.Contains(n, "was NOT saved") {
					msg += " [" + n + "]"
				}
			}
			note(failedOut(step, fmt.Errorf("%s: %s", label, msg)))
		}
	}
	if err := writeMeta(a.state, meta, metaRaw, a.allowShared); err != nil {
		note(inputOut(err))
	}
	switch {
	case first != nil:
		return *first
	case inc != nil:
		return *inc
	}
	return outcome{}
}

func (l logSpec) kindAndShort() string {
	if l.kind == "aggregate" {
		return "aggregate:" + l.id[:8]
	}
	return "chain:" + l.id[:8] + ":e" + strconv.FormatUint(l.epoch, 10)
}

// checkpointSub runs `checkpoint --state` for one log and returns its JSON result.
func (e *env) checkpointSub(a monitorArgs, p *pubInputs, spec logSpec, state string) (int, jsonOutput, string) {
	var out, errb bytes.Buffer
	sub := &env{stdout: &out, stderr: &errb, version: e.version}
	args := []string{"--trust", a.trust, "--log", spec.kindID(), "--source", a.source, "--state", state, "--json"}
	if a.now.given {
		args = append(args, "--now", a.now.val)
	}
	if a.allowShared {
		args = append(args, "--allow-shared-state-dir")
	}
	code := sub.runCheckpoint(args)
	var res jsonOutput
	_ = json.Unmarshal(out.Bytes(), &res)
	return code, res, errb.String()
}

func (l logSpec) kindID() string {
	if l.kind == "aggregate" {
		return "aggregate:" + l.id
	}
	return "chain:" + l.id + ":e" + strconv.FormatUint(l.epoch, 10)
}

func readMeta(dir string, init bool) (*monitorMetaFile, []byte, error) {
	data, err := safefile.ReadArg(filepath.Join(dir, monitorMeta), 4<<20)
	if errors.Is(err, fs.ErrNotExist) {
		// A first run is a run the user says is the first: what the state holds is what makes a rollback visible, so a
		// record that is missing (deleted, or never made) is not taken for "nothing was seen yet".
		if !init {
			if states := stateFilesIn(dir); len(states) > 0 {
				return nil, nil, fmt.Errorf("--state %s holds %d log state file(s) and no %s: what was seen (the events, how far the entries were audited, the chains retired) was lost or removed. Restore your copy of %s. If you really mean to start again, move the log state files away and give --init", dir, len(states), monitorMeta, monitorMeta)
			}
			return nil, nil, fmt.Errorf("--state %s holds no %s: this is a first run only if you say so, with --init (a state that was there and is gone would otherwise be taken for a new one, and a rollback would not show)", dir, monitorMeta)
		}
		if states := stateFilesIn(dir); len(states) > 0 {
			return nil, nil, fmt.Errorf("--init: --state %s already holds %d log state file(s) (%s ...): it is not a first run. Remove --init, or move the state files away to forget them on purpose", dir, len(states), states[0])
		}
		return newMeta(), nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("--state %s: %w", filepath.Join(dir, monitorMeta), err)
	}
	if init {
		return nil, nil, fmt.Errorf("--init: --state %s already holds %s: it is not a first run (remove --init)", dir, monitorMeta)
	}
	m := newMeta()
	var members map[string]json.RawMessage
	if err := json.Unmarshal(data, &members); err != nil {
		return nil, nil, fmt.Errorf("--state %s: %s is not a monitor record of this program (keep the file, it holds what was seen: restore your copy)", dir, monitorMeta)
	}
	if err := json.Unmarshal(data, m); err != nil || m.Format != 1 {
		return nil, nil, fmt.Errorf("--state %s: %s is not a monitor record of this program (keep the file, it holds what was seen: restore your copy)", dir, monitorMeta)
	}
	// the decoder keeps the maps that newMeta made when a member is absent, so the members are looked for in the file itself
	for _, name := range []string{"audited", "events", "retired"} {
		if raw, ok := members[name]; !ok || string(bytes.TrimSpace(raw)) == "null" {
			return nil, nil, fmt.Errorf("--state %s: %s is incomplete (it has no %q member): it was cut or emptied, and what was seen is lost; restore your copy", dir, monitorMeta, name)
		}
	}
	return m, data, nil
}

// stateFilesIn lists the log state files of a state directory.
func stateFilesIn(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, en := range ents {
		if stateNameRE.MatchString(en.Name()) {
			out = append(out, en.Name())
		}
	}
	return out
}

func writeMeta(dir string, m *monitorMetaFile, was []byte, allowShared bool) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if bytes.Equal(data, was) {
		return nil
	}
	typed := filepath.Join(dir, monitorMeta)
	path, err := statefile.Resolve(typed)
	if err == nil {
		err = statefile.CheckName(path)
	}
	if err != nil {
		return fmt.Errorf("--state %s: %w", dir, err)
	}
	lock, err := statefile.AcquireChecked(typed, path, statefile.LockWait, !allowShared)
	if err != nil {
		return fmt.Errorf("--state %s: cannot take the lock: %w", dir, err)
	}
	defer lock.Release()
	if err := statefile.ReplaceIfUnchanged(path, data, was, was != nil, 4<<20); err != nil {
		return fmt.Errorf("--state %s: the monitor record could not be updated: %w", dir, err)
	}
	return nil
}

// monitorEvents reads every event, compares it with what was seen, and records new ones.
func (e *env) monitorEvents(r *result, p *pubInputs, meta *monitorMetaFile, poll int) (*events.Walk, outcome) {
	digests := map[uint64]string{}
	rd := events.FromSource(p.src.ReadFile)
	w, err := events.Follow(p.base, func(seq uint64) ([]byte, error) {
		b, err := rd(seq)
		if err == nil {
			t := b
			if i := bytes.Index(b, []byte("\n\n")); i >= 0 {
				t = b[:i+1]
			}
			sum := sha256.Sum256(t)
			digests[seq] = hex.EncodeToString(sum[:])
		}
		return b, err
	}, false)
	if err != nil {
		return nil, eventsOutcome(err, "the events of the publication could not be read")
	}
	for k, d := range meta.Events {
		seq, _ := strconv.ParseUint(k, 10, 64)
		now, ok := digests[seq]
		switch {
		case !ok:
			return nil, failedOut("events", fmt.Errorf("event %s was in the publication when you looked before and is not there now (%d event(s) are served): the publication went back, or events were removed", k, w.Count))
		case now != d:
			return nil, failedOut("events", fmt.Errorf("event %s is not the one you saw: its text changed (digest %s before, %s now): two different events with one number", k, d[:16], now[:16]))
		}
	}
	for seq, d := range digests {
		meta.Events[fmt.Sprintf("%06d", seq)] = d
	}
	for _, sk := range w.Skipped {
		r.notes = appendNew(r.notes, []string{fmt.Sprintf("event %06d (%s) was skipped: %s", sk.Seq, sk.Type, sk.Reason)})
	}
	for k, ev := range w.Retired {
		if _, ok := meta.Retired[k]; !ok {
			meta.Retired[k] = 1<<40 + ev.Seq
		}
	}
	closedKeys := make([]string, 0, len(w.EpochClosed))
	for k := range w.EpochClosed {
		closedKeys = append(closedKeys, k)
	}
	sort.Strings(closedKeys)
	for _, k := range closedKeys {
		ec := w.EpochClosed[k]
		r.notes = appendNew(r.notes, []string{"finding: " + epochClosedText(memberKey{ec.Chain, ec.Epoch}, ec) + ". The closed log stays frozen at that size; the log of the next epoch is a different log and no consistency exists between them, so a state you keep for one is not a state of the other"})
	}
	if poll == 1 || len(digests) > 0 {
		r.ok(fmt.Sprintf("poll %d events", poll), fmt.Sprintf("%d event(s) read, none differs from what you saw, %d chain(s) retired by a verified event", w.Count, len(w.Retired)))
	}
	return w, outcome{}
}

// monitorAudit reads the entries of a log that were not audited yet, from the state file's checkpoint.
func (e *env) monitorAudit(r *result, p *pubInputs, spec logSpec, statePath, slug string, meta *monitorMetaFile, w *events.Walk, label string) outcome {
	in := p.logIn(spec, nil)
	raw, err := safefile.ReadArg(statePath, maxStateFile)
	if err != nil {
		return inputOut(err)
	}
	held, rec, err := decodeHeld(raw)
	if err != nil {
		return inputOut(fmt.Errorf("%s: %w", statePath, err))
	}
	h, _, err := in.verifyHeld(held, rec, "--state "+statePath, true)
	if err != nil {
		return inputOut(err)
	}
	ol := &openedLog{in: in, cp: h.cp}
	d, err := in.openSource()
	if err != nil {
		return inputOut(err)
	}
	ol.files = logFiles{d, spec.rel()}
	if ol.reader, err = newReader(ol); err != nil {
		return inputOut(err)
	}
	from := meta.Audited[slug]
	// a chain that a verified event ended must not grow
	if spec.kind == "chain" && w != nil {
		cid, _ := hex.DecodeString(spec.id)
		var c32 [32]byte
		copy(c32[:], cid)
		if ev, ok := w.Retired[events.ChainKey(c32, uint32(spec.epoch))]; ok && h.cp.Size > ev.FinalHeight+1 { // #nosec G115 -- range checked at parse
			return failedOut("retired-continues", fmt.Errorf("%s: the event %06d retired the chain at height %d (%d block(s)) and its log has size %d now: a retired chain continues", label, ev.Seq, ev.FinalHeight, ev.FinalHeight+1, h.cp.Size))
		}
	}
	if from >= h.cp.Size {
		return outcome{}
	}
	if spec.kind == "chain" {
		cid, _ := hex.DecodeString(spec.id)
		var c32 [32]byte
		copy(c32[:], cid)
		res, out := auditChainLog(ol, c32, from)
		if !out.ok() {
			return prefixed(label, out)
		}
		meta.Audited[slug] = res.to
		r.ok(label+" entries", fmt.Sprintf("%d block header(s) checked (prev_hash links, heights, times, leaf hashes against the tiles, tiles against the signed root)", res.checked))
		return outcome{}
	}
	seen := meta.Retired
	res, out := auditAggregateLogEach(ol, from, func(idx uint64, l *aggregate.Leaf) outcome {
		for _, m := range l.Members {
			k := events.ChainKey(m.ChainID, m.Epoch)
			if at, was := seen[k]; was && at < 1<<40 && at < l.Seq {
				return failedOut("retired-reappears", fmt.Errorf("the chain %s was flagged retired in round %d and is a member of round %d again", k, at, l.Seq))
			}
			if m.Retired() {
				if _, was := seen[k]; !was || seen[k] >= 1<<40 {
					seen[k] = l.Seq
				}
			}
		}
		return outcome{}
	})
	if !out.ok() {
		return prefixed(label, out)
	}
	meta.Audited[slug] = res.to
	r.ok(label+" entries", fmt.Sprintf("%d aggregate leaf(s) checked (sequence, times, retired chains never again)", res.checked))
	return outcome{}
}

func prefixed(label string, o outcome) outcome {
	switch o.kind {
	case outFailed, outInput:
		o.err = fmt.Errorf("%s: %w", label, o.err)
	case outIncomplete:
		for i := range o.missing {
			o.missing[i] = label + ": " + o.missing[i]
		}
	}
	return o
}
