// SPDX-License-Identifier: AGPL-3.0-only

// Package ledgerstate keeps what the publisher of the ledger has published, outside the system database (design ledger-phase7.md, 9.4, with
// the correction 171 of the review of the collector, P8-01): the highest size and root of every log that has a signed checkpoint, and the
// number of the events and the hash of the last one. It is a small file next to the signing keys, <ledger-keys-dir>/published.json, written
// atomically with mode 0600 after every checkpoint and every event.
//
// Why it exists. The aggregate leaf and the events hold a clock time and what the chains looked like at that moment, so they are not
// reproducible: a restore of kingres_cp from an older backup makes a publisher that trusts only that database sign a second root for a size
// that was published, and a second event for a number that was used. Everything the collector compared with was in the restored database. This file
// is not in the database, so a restore does not take it back; the collector refuses to sign anything while the database is behind it or
// contradicts it, and raises a critical alert.
//
// What it does not do: it is not signed. Whoever can write the key directory can already sign with the keys, so a signature by the same keys
// would add nothing against that person; the file carries a checksum so that damage is told from a state, and a damaged file refuses to
// sign instead of being taken for an empty one. It is not a mirror: when a server that never saw the state becomes the leader (a new holder, a
// host restored from a backup of the key directory) it asks the other holders first (Pull) and merges what they have (Merge is monotone), and a
// leader that publishes sends its state to the holders at every pass (the job of kingresd). Where every copy of the state and the database
// are older than the publication, the publisher cannot know: a mirror or an auditor that holds the later checkpoint is what shows the fork,
// which is the reason the publication is mirrored at all.
package ledgerstate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
)

// FileName is the name of the state file in the key directory.
const FileName = "published.json"

// MaxFile is the most the file may hold: a log takes about 230 bytes, so about 140 000 logs. A save that would write more is refused (and says so),
// never written: a file that is larger than this is read as damaged at the next start, and a damaged record stops all signing (P7A-14).
const MaxFile = 32 << 20

// maxFile is MaxFile, which a test lowers.
var maxFile = MaxFile

// Mark is the highest checkpoint of one log: its size and root (hexadecimal).
type Mark struct {
	Size int64  `json:"size"`
	Root string `json:"root"`
}

// State is what has been published.
type State struct {
	// Logs by origin.
	Logs map[string]Mark `json:"logs"`
	// Events is the number of events published (the next sequence number) and EventHash the SHA-256 (hex) of the note of the last one.
	Events    int64  `json:"events"`
	EventHash string `json:"eventHash"`
	// Generation counts the restores of the system database that an operator accepted (Accept); it is 0 for a record that never saw one. A
	// record of a higher generation replaces one of a lower generation when two are merged, where within one generation the merge keeps the
	// larger of everything (P7A-02).
	Generation int64 `json:"generation,omitempty"`
	// Restore is set from the acceptance of a restore until the event that announces it is published, so that a crash between the two
	// leaves the announcement to be made and does not drop it.
	Restore *RestoreNote `json:"restore,omitempty"`
}

// RestoreNote is what the signed event restore-acknowledged says about the break that an operator accepted: what the record held and what the
// restored database held, as counts and SHA-256 sums of the two states (the sums let anyone who holds the old record check the event).
type RestoreNote struct {
	PublishedEvents int64  `json:"publishedEvents"`
	PublishedLogs   int    `json:"publishedLogs"`
	PublishedSHA    string `json:"publishedSha256"`
	DatabaseEvents  int64  `json:"databaseEvents"`
	DatabaseLogs    int    `json:"databaseLogs"`
	DatabaseSHA     string `json:"databaseSha256"`
	Reason          string `json:"reason"`
	Actor           string `json:"actor"`
}

// Errors. The texts hold no secret.
var (
	// ErrBehind: the database holds less than what was published (a restore of an older backup), or a log or an event that differs.
	ErrBehind = errors.New("ledgerstate: the system database is older than what was published, or contradicts it")
	// ErrDamaged: the state file cannot be read as written.
	ErrDamaged = errors.New("ledgerstate: the file of what was published is damaged")
	// ErrFork: a size would get a second root, or an event number a second note.
	ErrFork = errors.New("ledgerstate: a size or an event number that was published would get other content")
	// ErrNotWritten: the record could not be written to the disk (full, read only, or larger than MaxFile). What is in memory is current,
	// the file is behind: nothing is signed until a write succeeds, because the record is the guard that a signature cannot be repeated (P7A-11).
	ErrNotWritten = errors.New("ledgerstate: the record of what was published cannot be written")
)

type onDisk struct {
	State
	Sum string `json:"sum"`
}

func sumOf(s State) string {
	b, _ := json.Marshal(s)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// File is the state file of this server.
type File struct {
	path string
	mu   sync.Mutex
	st   State
	have bool
	// Warned is set when a damaged file was met (it stays damaged until an operator removes it).
	bad bool
	// dirty is set when a change could not be written: the file is behind the memory (see ErrNotWritten).
	dirty bool
}

// Open prepares the state in dir (which must exist). A missing file is not an error (Initialized reports it); a damaged one is remembered
// and every check refuses until it is removed.
func Open(dir string) (*File, error) {
	if dir == "" {
		return nil, errors.New("ledgerstate: the key directory is empty")
	}
	f := &File{path: filepath.Join(dir, FileName)}
	data, err := os.ReadFile(f.path) // #nosec G304 -- the key directory of the daemon
	switch {
	case errors.Is(err, os.ErrNotExist):
		return f, nil
	case err != nil || len(data) > maxFile:
		f.bad = true
		return f, nil
	}
	var d onDisk
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil || d.Sum != sumOf(d.State) {
		f.bad = true
		return f, nil
	}
	if d.Logs == nil {
		d.Logs = map[string]Mark{}
	}
	f.st, f.have = d.State, true
	return f, nil
}

// Initialized reports whether the file exists and is readable.
func (f *File) Initialized() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.have && !f.bad
}

// Damaged reports whether the file exists and cannot be read as written.
func (f *File) Damaged() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bad
}

// Snapshot returns a copy of the state.
func (f *File) Snapshot() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	return clone(f.st)
}

func clone(s State) State {
	out := State{Logs: map[string]Mark{}, Events: s.Events, EventHash: s.EventHash, Generation: s.Generation}
	for k, v := range s.Logs {
		out.Logs[k] = v
	}
	if s.Restore != nil {
		r := *s.Restore
		out.Restore = &r
	}
	return out
}

// persist writes the state and remembers whether it could (dirty). The caller holds the lock.
func (f *File) persist() error {
	if err := f.save(); err != nil {
		f.dirty = true
		return err
	}
	f.dirty = false
	return nil
}

// settle writes again a change that could not be written before, and refuses while it cannot (ErrNotWritten). The caller holds the lock.
func (f *File) settle() error {
	if !f.dirty {
		return nil
	}
	if err := f.persist(); err != nil {
		return fmt.Errorf("%w: %v", ErrNotWritten, err)
	}
	return nil
}

// save writes the state atomically. The caller holds the lock.
func (f *File) save() error {
	b, err := json.Marshal(onDisk{State: f.st, Sum: sumOf(f.st)})
	if err != nil {
		return err
	}
	if len(b) > maxFile {
		return fmt.Errorf("ledgerstate: the record is %d bytes, more than the %d that a file may hold", len(b), maxFile)
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, ".published-*")
	if err != nil {
		return errors.New("ledgerstate: the state file cannot be written")
	}
	name := tmp.Name()
	fail := func() error {
		_ = tmp.Close()
		_ = os.Remove(name)
		return errors.New("ledgerstate: the state file cannot be written")
	}
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			return fail()
		}
	}
	if _, err := tmp.Write(b); err != nil {
		return fail()
	}
	if err := tmp.Sync(); err != nil {
		return fail()
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return errors.New("ledgerstate: the state file cannot be written")
	}
	if err := os.Rename(name, f.path); err != nil {
		_ = os.Remove(name)
		return errors.New("ledgerstate: the state file cannot be written")
	}
	syncDir(dir)
	f.have = true
	return nil
}

// syncDir makes the rename durable on the systems that can sync a directory (a power loss would otherwise leave the old file, one write behind the
// database). A system that cannot is not an error.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	if d, err := os.Open(dir); err == nil { // #nosec G304 -- the key directory of the daemon
		_ = d.Sync()
		_ = d.Close()
	}
}

// Allow says whether a checkpoint of the log with this size and root may be signed: not when a checkpoint of a larger size was published,
// and not when the same size was published with another root. It is checked before anything is signed.
func (f *File) Allow(origin string, size int64, root []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	if err := f.settle(); err != nil {
		return err
	}
	m, ok := f.st.Logs[origin]
	if !ok {
		return nil
	}
	switch {
	case size < m.Size:
		return fmt.Errorf("%w: a checkpoint of size %d was published for a log whose tree is now of size %d", ErrBehind, m.Size, size)
	case size == m.Size && hex.EncodeToString(root) != m.Root:
		return fmt.Errorf("%w: size %d of a log was published with another root", ErrFork, size)
	}
	return nil
}

// RecordCheckpoint notes that a checkpoint was stored (call it after the store accepted it). A smaller size is ignored, the same size with
// another root is ErrFork.
func (f *File) RecordCheckpoint(origin string, size int64, root []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	if f.st.Logs == nil {
		f.st.Logs = map[string]Mark{}
	}
	h := hex.EncodeToString(root)
	m, ok := f.st.Logs[origin]
	switch {
	case ok && size < m.Size:
		return nil
	case ok && size == m.Size:
		if m.Root != h {
			return ErrFork
		}
		return nil
	}
	f.st.Logs[origin] = Mark{Size: size, Root: h}
	return f.persist()
}

// EventHashOf is the hash that stands for the note of an event.
func EventHashOf(note []byte) string {
	h := sha256.Sum256(note)
	return hex.EncodeToString(h[:])
}

// AllowEvent says whether an event with number seq may be appended: it must be the number the state expects or a later one that follows
// events that exist; a number below the count is a repeat (the same event is found, not appended) and is not asked.
func (f *File) AllowEvent(count int64, lastNote []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	if err := f.settle(); err != nil {
		return err
	}
	return f.checkEvents(count, lastNote)
}

func (f *File) checkEvents(count int64, lastNote []byte) error {
	if count < f.st.Events {
		return fmt.Errorf("%w: %d events were published and the database holds %d", ErrBehind, f.st.Events, count)
	}
	if count == f.st.Events && f.st.Events > 0 && EventHashOf(lastNote) != f.st.EventHash {
		return fmt.Errorf("%w: the last event differs from the one that was published", ErrFork)
	}
	return nil
}

// RecordEvent notes that the event with number seq was stored with this note.
func (f *File) RecordEvent(seq int64, note []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	if seq+1 < f.st.Events {
		return nil
	}
	if seq+1 == f.st.Events {
		if EventHashOf(note) != f.st.EventHash {
			return ErrFork
		}
		return nil
	}
	f.st.Events, f.st.EventHash = seq+1, EventHashOf(note)
	return f.persist()
}

// Merge adds what another copy of the state knows: the larger size per log, the larger event count. Two copies that disagree on the root of
// one size, or on the last event, are ErrFork and nothing is merged.
func (f *File) Merge(o State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	switch {
	case o.Generation > f.st.Generation:
		// an operator accepted a restore that this copy has not seen: that record replaces this one (the loss it accepted is the new floor)
		f.st = clone(o)
		return f.persist()
	case o.Generation < f.st.Generation:
		return nil // a copy that has not seen the acceptance: nothing of it is merged (it would take the floor back)
	}
	next := clone(f.st)
	for k, v := range o.Logs {
		m, ok := next.Logs[k]
		switch {
		case !ok || v.Size > m.Size:
			next.Logs[k] = v
		case v.Size == m.Size && v.Root != m.Root:
			return fmt.Errorf("%w: log %s", ErrFork, clipOrigin(k))
		}
	}
	switch {
	case o.Events > next.Events:
		next.Events, next.EventHash = o.Events, o.EventHash
	case o.Events == next.Events && o.EventHash != next.EventHash:
		return fmt.Errorf("%w: the last event", ErrFork)
	}
	if next.Restore == nil {
		next.Restore = o.Restore
	}
	f.st = next
	return f.persist()
}

func clipOrigin(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

// Check compares the state with what the database holds: for every log of the state the database must hold a checkpoint of that size with that
// root (a larger tree is fine: the database is ahead), and at least as many events, the one before the count being the one that was
// published. have gives the database's checkpoint root of a log at a size (false when it has none); eventNote the note of an event.
func (f *File) Check(have func(origin string, size int64) (root []byte, ok bool, err error), events int64, eventNote func(seq int64) ([]byte, error)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	if err := f.settle(); err != nil {
		return err
	}
	for origin, m := range f.st.Logs {
		root, ok, err := have(origin, m.Size)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: no checkpoint of size %d of log %s", ErrBehind, m.Size, clipOrigin(origin))
		}
		if hex.EncodeToString(root) != m.Root {
			return fmt.Errorf("%w: size %d of log %s has another root", ErrFork, m.Size, clipOrigin(origin))
		}
	}
	if events < f.st.Events {
		return fmt.Errorf("%w: %d events were published and the database holds %d", ErrBehind, f.st.Events, events)
	}
	if f.st.Events > 0 {
		note, err := eventNote(f.st.Events - 1)
		if err != nil {
			return err
		}
		if EventHashOf(note) != f.st.EventHash {
			return fmt.Errorf("%w: event %d differs from the one that was published", ErrFork, f.st.Events-1)
		}
	}
	return nil
}

// Replace sets the state to the given one without any check: the first start of a server that has no record and no holder to ask (what the
// database holds is all there is to go on). The acceptance of a restore is Accept.
func (f *File) Replace(s State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = clone(s)
	f.bad = false
	return f.persist()
}

// AcceptFileName is the file in the key directory by which an operator accepts the loss of a restore of the system database (see Accept).
const AcceptFileName = "accept-restore"

// Accept is the operator's acceptance of a restore of the system database: the record becomes what the restored database holds (db), one
// generation higher, with the note that announces the break in a signed event (RestoreNote; the collector publishes it and ClearRestore ends
// it). The record it replaces is kept beside it as published.json.before-restore-<generation> (an auditor who holds it can see what was given
// up), and the write of that copy must succeed before anything changes. Nothing else replaces a record that has been published.
func (f *File) Accept(db State, n RestoreNote) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	next := clone(db)
	next.Generation = f.st.Generation + 1
	nn := n
	next.Restore = &nn
	if data, err := os.ReadFile(f.path); err == nil { // #nosec G304 -- the key directory of the daemon
		keep := fmt.Sprintf("%s.before-restore-%d", f.path, next.Generation)
		if err := os.WriteFile(keep, data, 0o600); err != nil { // #nosec G306 -- mode 0600
			return errors.New("ledgerstate: the record to be replaced cannot be kept: nothing was changed")
		}
	}
	f.st = next
	return f.persist()
}

// ClearRestore ends the note of an accepted restore after its event was published.
func (f *File) ClearRestore() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
	}
	if f.st.Restore == nil {
		return nil
	}
	f.st.Restore = nil
	return f.persist()
}

// AcceptToken reads the file accept-restore of the key directory: the first line is the confirmation the operator copied from the alert, an
// optional second line is the reason (printable ASCII, at most 200 bytes). "" when there is no such file or it is not as described.
func (f *File) AcceptToken() (token, reason string) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(f.path), AcceptFileName)) // #nosec G304 -- the key directory of the daemon
	if err != nil || len(data) > 1024 {
		return "", ""
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	token = string(bytes.TrimSpace(lines[0]))
	if len(lines) > 1 {
		reason = string(bytes.TrimSpace(bytes.Join(lines[1:], []byte(" "))))
	}
	for i := 0; i < len(reason); i++ {
		if reason[i] < 0x20 || reason[i] > 0x7e {
			reason = ""
			break
		}
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	return token, reason
}

// ConsumeAcceptToken removes the file accept-restore (the acceptance was made).
func (f *File) ConsumeAcceptToken() {
	_ = os.Remove(filepath.Join(filepath.Dir(f.path), AcceptFileName))
}

// SumOf is the SHA-256 (hex) of the canonical encoding of a state without its generation and its note: the identity of what a record held.
func SumOf(s State) string {
	c := clone(s)
	c.Generation, c.Restore = 0, nil
	return sumOf(c)
}

// Confirmation is what an operator must write in the file accept-restore to accept the loss of this restore: sixteen hexadecimal digits that
// depend on the record (with its generation) and on what the restored database holds, so that it fits this one situation and no other.
func Confirmation(published, database State) string {
	p := clone(published)
	p.Restore = nil
	h := sha256.Sum256([]byte("kingres restore confirmation 1\n" + sumOf(p) + "\n" + SumOf(database)))
	return hex.EncodeToString(h[:8])
}
