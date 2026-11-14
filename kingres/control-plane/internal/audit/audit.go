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

// Filter selects events for Query.
type Filter struct {
	// From and To bound the timestamp (From inclusive, To exclusive); zero
	// values mean unbounded.
	From, To time.Time
	// Actor, Action and Outcome match exactly when non-empty.
	Actor, Action, Outcome string
	// BeforeID returns only events with a smaller id; 0 means no bound.
	BeforeID int64
	// Limit is the maximum number of events returned (newest first).
	Limit int
}

// Sink is the audit log as the server uses it.
type Sink interface {
	// Append records an event and returns its id.
	Append(ctx context.Context, e Event) (int64, error)
	// Query returns events newest first, at most f.Limit+1 so the caller can
	// tell whether another page exists.
	Query(ctx context.Context, f Filter) ([]api.AuditEvent, error)
}

// FileSink is a Sink backed by a JSON-lines file.
type FileSink struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	file   *os.File
	nextID int64
}

// OpenFile opens (creating if needed, mode 0600) the audit file at path and
// recovers the next event id from its content. now supplies the clock.
func OpenFile(path string, now func() time.Time) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600) //nolint:gosec // the path is operator configuration
	if err != nil {
		return nil, fmt.Errorf("opening audit file: %w", err)
	}
	s := &FileSink{path: path, now: now, file: f}
	if err := s.terminateTornLine(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("repairing audit file: %w", err)
	}
	var last int64
	err = s.scan(func(e api.AuditEvent) bool { last = e.ID; return true })
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("reading audit file: %w", err)
	}
	s.nextID = last + 1
	return s, nil
}

// Close closes the audit file.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.file.Close()
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
	if _, err := s.file.Write(append(line, '\n')); err != nil {
		return 0, fmt.Errorf("writing audit event: %w", err)
	}
	if err := s.file.Sync(); err != nil {
		return 0, fmt.Errorf("syncing audit file: %w", err)
	}
	s.nextID++
	return ev.ID, nil
}

// Query implements Sink. It reads the whole file; the audit log of this
// increment is expected to be small. A later increment can add an index.
func (s *FileSink) Query(ctx context.Context, f Filter) ([]api.AuditEvent, error) {
	var all []api.AuditEvent
	err := s.scan(func(e api.AuditEvent) bool {
		if matches(e, f) {
			all = append(all, e)
		}
		return ctx.Err() == nil
	})
	if err != nil {
		return nil, fmt.Errorf("querying audit file: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// Newest first: ids grow with the file, so reverse.
	out := make([]api.AuditEvent, 0, len(all))
	for i := len(all) - 1; i >= 0; i-- {
		out = append(out, all[i])
		if f.Limit > 0 && len(out) >= f.Limit+1 {
			break
		}
	}
	return out, nil
}

func matches(e api.AuditEvent, f Filter) bool {
	if f.BeforeID > 0 && e.ID >= f.BeforeID {
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

// scan reads the file from the start through a separate descriptor, so it
// does not disturb the append handle. Corrupt lines (a torn final line after
// a crash) are skipped.
func (s *FileSink) scan(fn func(api.AuditEvent) bool) error {
	r, err := os.Open(s.path) //nolint:gosec // the path is operator configuration
	if err != nil {
		return err
	}
	defer r.Close()
	br := bufio.NewReader(r)
	for {
		line, err := br.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var e api.AuditEvent
			if json.Unmarshal(line, &e) == nil {
				if !fn(e) {
					return nil
				}
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// Memory is an in-memory Sink for tests.
type Memory struct {
	mu     sync.Mutex
	now    func() time.Time
	events []api.AuditEvent
}

// NewMemory returns an empty in-memory sink using the given clock.
func NewMemory(now func() time.Time) *Memory { return &Memory{now: now} }

// Append implements Sink.
func (m *Memory) Append(_ context.Context, e Event) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
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
