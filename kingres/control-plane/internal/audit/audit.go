// SPDX-License-Identifier: PostgreSQL

// Package audit implements the append-only audit log.
//
// Events are written as JSON lines, one AuditEvent (schema of the contract)
// per line, and fsynced before Append returns, so an acknowledged event
// survives a crash. The file is only ever opened for appending. Secrets are
// never part of an event: callers pass only identifiers and outcomes.
package audit

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// Outcome values of the contract (schema AuditOutcome).
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
	OutcomeDenied  = "denied"
)

// Event is what a caller records. The sink assigns the id and timestamp.
type Event struct {
	// Actor is the role that acted, "anonymous" before authentication.
	Actor string
	// ActorAddress is the client IP address.
	ActorAddress string
	// Source is panel, cli, rest or system.
	Source string
	// Action is the action name, for example "login" or an operation id.
	Action string
	// ResourceType and ResourceID identify the affected resource.
	ResourceType string
	ResourceID   string
	// Outcome is OutcomeSuccess, OutcomeFailure or OutcomeDenied.
	Outcome string
	// RequestID correlates the event with the access log.
	RequestID string
	// Details is extra, non-secret data.
	Details map[string]any
}

// Position is a place in the listing of the audit log, which is newest first.
// ID is the cluster-wide event id (see GlobalID).
type Position struct {
	Time time.Time
	ID   int64
}

// GlobalID is the id of the event localID of the server with the ordinal: the
// ordinal in the high bits, so that the ids of the events of different servers
// never collide and the server that holds an event is known from its id (the
// scheme of the alert ids). Both parts are limited so that the id stays below
// 2^53, the largest integer a JSON number holds in a browser.
func GlobalID(ordinal int, localID int64) int64 { return int64(ordinal)<<32 | localID&(1<<32-1) }

// SplitID is the inverse of GlobalID.
func SplitID(id int64) (ordinal int, localID int64) { return int(id >> 32), id & (1<<32 - 1) }

// Before reports whether a is listed before b: the later instant first, the
// larger id first among events of the same instant.
func (a Position) Before(b Position) bool {
	if !a.Time.Equal(b.Time) {
		return a.Time.After(b.Time)
	}
	return a.ID > b.ID
}

// Filter selects events for Query.
type Filter struct {
	// From and To bound the timestamp (From inclusive, To exclusive); zero
	// values mean unbounded.
	From, To time.Time
	// Actor, Action and Outcome match exactly when non-empty.
	Actor, Action, Outcome string
	// Below returns only events listed after this position; nil means no
	// bound. Ordinal is the ordinal of the server that holds the sink (0 for a
	// server without a cluster): it turns the local id of an event into its
	// cluster-wide id before the comparison.
	Below   *Position
	Ordinal int
	// Limit is the maximum number of events returned (newest first).
	Limit int
}

// Sink is the audit log as the server uses it.
type Sink interface {
	// Append records an event and returns its id.
	Append(ctx context.Context, e Event) (int64, error)
	// Ready reports whether Append can write right now. A state-changing
	// operation asks before it starts, so that an action does not take effect
	// while the log cannot take its record (the record itself is written
	// afterwards, with the outcome).
	Ready(ctx context.Context) error
	// Query returns events newest first, at most f.Limit+1 so the caller can
	// tell whether another page exists.
	Query(ctx context.Context, f Filter) ([]api.AuditEvent, error)
}

// DefaultSegmentBytes is the size at which the live audit file is closed and a
// new one started.
const DefaultSegmentBytes = 64 << 20

// FileSink is a Sink backed by JSON-lines files: the live file at path and the
// closed segments next to it (path.<id of the first event>). Nothing is ever
// deleted; keeping or archiving old segments is the operator's decision.
type FileSink struct {
	path string
	now  func() time.Time

	mu       sync.Mutex
	file     *os.File
	nextID   int64
	size     int64
	firstID  int64 // id of the first event of the live file, 0 while it has none
	maxBytes int64
}

// OpenFile opens (creating if needed, mode 0600) the audit file at path and
// recovers the next event id from its content. now supplies the clock.
func OpenFile(path string, now func() time.Time) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600) //nolint:gosec // the path is operator configuration
	if err != nil {
		return nil, fmt.Errorf("opening audit file: %w", err)
	}
	s := &FileSink{path: path, now: now, file: f, maxBytes: DefaultSegmentBytes}
	if err := s.terminateTornLine(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("repairing audit file: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("reading audit file: %w", err)
	}
	s.size = st.Size()
	// The newest event of the live file (right after a rotation: of the newest
	// segment) gives the next id; the first line of the live file gives its first id.
	var last int64
	for _, p := range append([]string{path}, s.segmentPaths()...) {
		if _, err = scanBackward(p, func(e api.AuditEvent) bool { last = e.ID; return false }); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("reading audit file: %w", err)
		}
		if last != 0 {
			break
		}
	}
	s.nextID = last + 1
	if err = s.scanForward(func(e api.AuditEvent) bool { s.firstID = e.ID; return false }); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("reading audit file: %w", err)
	}
	return s, nil
}

// SetSegmentBytes sets the size at which the live file is closed (0: never).
// It is meant for start-up and for tests.
func (s *FileSink) SetSegmentBytes(n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxBytes = n
}

// Close closes the audit file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
}

// Ready implements Sink. It writes and syncs a blank line, which readers skip,
// so that a full disk or a lost file is noticed before an action is taken.
func (s *FileSink) Ready(_ context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.file.Write([]byte{'\n'}); err != nil {
		return fmt.Errorf("writing audit file: %w", err)
	}
	s.size++
	if err := s.file.Sync(); err != nil {
		return fmt.Errorf("syncing audit file: %w", err)
	}
	return nil
}

// Append implements Sink.
func (s *FileSink) Append(_ context.Context, e Event) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ev := api.AuditEvent{
		ID:           s.nextID,
		Timestamp:    s.now().UTC(),
		Actor:        Clean(e.Actor),
		ActorAddress: Clean(e.ActorAddress),
		Source:       e.Source,
		Action:       e.Action,
		ResourceType: e.ResourceType,
		ResourceID:   Clean(e.ResourceID),
		Outcome:      e.Outcome,
		RequestID:    e.RequestID,
		Details:      e.Details,
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("encoding audit event: %w", err)
	}
	if err := s.rotateIfFull(); err != nil {
		return 0, err
	}
	line = append(line, '\n')
	if _, err := s.file.Write(line); err != nil {
		return 0, fmt.Errorf("writing audit event: %w", err)
	}
	s.size += int64(len(line))
	if err := s.file.Sync(); err != nil {
		return 0, fmt.Errorf("syncing audit file: %w", err)
	}
	if s.firstID == 0 {
		s.firstID = ev.ID
	}
	s.nextID++
	return ev.ID, nil
}

// rotateIfFull closes the live file when it has reached its size and starts a
// new one. A file that cannot be renamed (a reader holds it open on Windows) is
// kept as the live file; the next Append tries again.
func (s *FileSink) rotateIfFull() error {
	if s.maxBytes <= 0 || s.size < s.maxBytes || s.firstID == 0 {
		return nil
	}
	if err := s.file.Close(); err != nil {
		return fmt.Errorf("closing audit file: %w", err)
	}
	renamed := os.Rename(s.path, fmt.Sprintf("%s.%d", s.path, s.firstID)) == nil
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600) //nolint:gosec // the path is operator configuration
	if err != nil {
		return fmt.Errorf("reopening audit file: %w", err)
	}
	s.file = f
	if renamed {
		s.size, s.firstID = 0, 0
	}
	return nil
}

// segmentPaths lists the closed segments, newest first.
func (s *FileSink) segmentPaths() []string {
	matches, _ := filepath.Glob(s.path + ".*")
	type seg struct {
		first int64
		path  string
	}
	var segs []seg
	for _, m := range matches {
		first, err := strconv.ParseInt(strings.TrimPrefix(m, s.path+"."), 10, 64)
		if err == nil && first > 0 {
			segs = append(segs, seg{first, m})
		}
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].first > segs[j].first })
	out := make([]string, len(segs))
	for i, g := range segs {
		out[i] = g.path
	}
	return out
}

// Query implements Sink. It reads the live file and then the closed segments
// from their ends and stops as soon as Limit+1 events matched, so a page costs
// what it returns, not the size of the log.
func (s *FileSink) Query(ctx context.Context, f Filter) ([]api.AuditEvent, error) {
	var out []api.AuditEvent
	want := f.Limit + 1
	for _, p := range append([]string{s.path}, s.segmentPaths()...) {
		done, err := scanBackward(p, func(e api.AuditEvent) bool {
			if matches(e, f) {
				out = append(out, e)
			}
			return ctx.Err() == nil && (f.Limit <= 0 || len(out) < want)
		})
		if err != nil {
			return nil, fmt.Errorf("querying audit file: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !done {
			break
		}
	}
	return out, nil
}

func matches(e api.AuditEvent, f Filter) bool {
	if f.Below != nil && !f.Below.Before(Position{Time: e.Timestamp, ID: GlobalID(f.Ordinal, e.ID)}) {
		return false
	}
	if !f.From.IsZero() && e.Timestamp.Before(f.From) {
		return false
	}
	if !f.To.IsZero() && !e.Timestamp.Before(f.To) {
		return false
	}
	if f.Actor != "" && e.Actor != f.Actor {
		return false
	}
	if f.Action != "" && e.Action != f.Action {
		return false
	}
	if f.Outcome != "" && e.Outcome != f.Outcome {
		return false
	}
	return true
}

// scanForward reads the live file from the start through a separate
// descriptor, so it does not disturb the append handle. Corrupt lines (a torn
// final line after a crash) are skipped.
func (s *FileSink) scanForward(fn func(api.AuditEvent) bool) error {
	r, err := os.Open(s.path) //nolint:gosec // the path is operator configuration
	if err != nil {
		return err
	}
	defer r.Close()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if !emit(line, fn) {
			return nil
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// scanBackward reads a file from its end, newest line first, in blocks, so that
// only what fn asks for is read. It reports whether the whole file was read (fn
// never returned false). A file that does not exist is empty.
func scanBackward(path string, fn func(api.AuditEvent) bool) (bool, error) {
	r, err := os.Open(path) //nolint:gosec // the path is operator configuration
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer r.Close()
	st, err := r.Stat()
	if err != nil {
		return false, err
	}
	const block = 64 << 10
	pos := st.Size()
	var carry []byte // the start of a line whose beginning is in an earlier block
	for pos > 0 {
		n := int64(block)
		if pos < n {
			n = pos
		}
		pos -= n
		buf := make([]byte, n, n+int64(len(carry)))
		if _, err := r.ReadAt(buf, pos); err != nil {
			return false, err
		}
		buf = append(buf, carry...)
		carry = nil
		for end := len(buf); end > 0; {
			nl := bytes.LastIndexByte(buf[:end], '\n')
			if nl < 0 {
				carry = append([]byte(nil), buf[:end]...)
				break
			}
			if !emit(buf[nl+1:end], fn) {
				return false, nil
			}
			end = nl
		}
	}
	if len(carry) > 0 && !emit(carry, fn) {
		return false, nil
	}
	return true, nil
}

// emit decodes one line and hands it to fn (true: go on); blank and corrupt
// lines are skipped.
func emit(line []byte, fn func(api.AuditEvent) bool) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		return true
	}
	var e api.AuditEvent
	if json.Unmarshal(line, &e) != nil {
		return true
	}
	return fn(e)
}

// Memory is an in-memory Sink for tests.
type Memory struct {
	mu     sync.Mutex
	now    func() time.Time
	fail   error
	events []api.AuditEvent
}

// NewMemory returns an empty in-memory sink using the given clock.
func NewMemory(now func() time.Time) *Memory { return &Memory{now: now} }

// SetFailure makes Append and Ready fail with err (nil: work again), for tests of
// what happens when the log cannot be written.
func (m *Memory) SetFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fail = err
}

// Ready implements Sink.
func (m *Memory) Ready(_ context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.fail
}

// Append implements Sink.
func (m *Memory) Append(_ context.Context, e Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return 0, m.fail
	}
	id := int64(len(m.events)) + 1
	m.events = append(m.events, api.AuditEvent{
		ID: id, Timestamp: m.now().UTC(), Actor: Clean(e.Actor), ActorAddress: Clean(e.ActorAddress),
		Source: e.Source, Action: e.Action, ResourceType: e.ResourceType, ResourceID: Clean(e.ResourceID),
		Outcome: e.Outcome, RequestID: e.RequestID, Details: e.Details,
	})
	return id, nil
}

// Query implements Sink.
func (m *Memory) Query(_ context.Context, f Filter) ([]api.AuditEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]api.AuditEvent, 0)
	for i := len(m.events) - 1; i >= 0; i-- {
		if matches(m.events[i], f) {
			out = append(out, m.events[i])
			if f.Limit > 0 && len(out) >= f.Limit+1 {
				break
			}
		}
	}
	return out, nil
}

// Events returns a copy of all recorded events, oldest first.
func (m *Memory) Events() []api.AuditEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]api.AuditEvent(nil), m.events...)
}

// terminateTornLine ends a final line that was cut off by a crash with a
// newline, so that the next event starts on its own line instead of being
// glued to (and lost with) the fragment. The fragment itself stays in the file
// and is skipped when reading.
func (s *FileSink) terminateTornLine() error {
	st, err := s.file.Stat()
	if err != nil || st.Size() == 0 {
		return err
	}
	r, err := os.Open(s.path) //nolint:gosec // the path is operator configuration
	if err != nil {
		return err
	}
	defer r.Close()
	last := make([]byte, 1)
	if _, err := r.ReadAt(last, st.Size()-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}
	if _, err := s.file.Write([]byte{'\n'}); err != nil {
		return err
	}
	return s.file.Sync()
}

// Clean replaces control characters (and invalid UTF-8) in a text taken from a
// request with U+FFFD, so that one event is always one clean line whoever
// reads the file, and a name cannot forge a field in a plain-text view.
func Clean(s string) string {
	bad := false
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == utf8.RuneError {
			bad = true
			break
		}
	}
	if !bad {
		return s
	}
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == utf8.RuneError {
			return utf8.RuneError
		}
		return r
	}, s)
}
