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

// MaxFile is the most the file may hold (a few hundred logs at most).
const MaxFile = 4 << 20

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
}

// Errors. The texts hold no secret.
var (
	// ErrBehind: the database holds less than what was published (a restore of an older backup), or a log or an event that differs.
	ErrBehind = errors.New("ledgerstate: the system database is older than what was published, or contradicts it")
	// ErrDamaged: the state file cannot be read as written.
	ErrDamaged = errors.New("ledgerstate: the file of what was published is damaged")
	// ErrFork: a size would get a second root, or an event number a second note.
	ErrFork = errors.New("ledgerstate: a size or an event number that was published would get other content")
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
	case err != nil || len(data) > MaxFile:
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
	out := State{Logs: map[string]Mark{}, Events: s.Events, EventHash: s.EventHash}
	for k, v := range s.Logs {
		out.Logs[k] = v
	}
	return out
}

// save writes the state atomically. The caller holds the lock.
func (f *File) save() error {
	b, err := json.Marshal(onDisk{State: f.st, Sum: sumOf(f.st)})
	if err != nil {
		return err
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
	f.have = true
	return nil
}

// Allow says whether a checkpoint of the log with this size and root may be signed: not when a checkpoint of a larger size was published,
// and not when the same size was published with another root. It is checked before anything is signed.
func (f *File) Allow(origin string, size int64, root []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
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
	return f.save()
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
	return f.save()
}

// Merge adds what another copy of the state knows: the larger size per log, the larger event count. Two copies that disagree on the root of
// one size, or on the last event, are ErrFork and nothing is merged.
func (f *File) Merge(o State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bad {
		return ErrDamaged
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
	f.st = next
	return f.save()
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

// Replace sets the state to the given one without any check: the first start (what the database holds is all there is to go on), and the
// operator's explicit acceptance after a restore.
func (f *File) Replace(s State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = clone(s)
	f.bad = false
	return f.save()
}
