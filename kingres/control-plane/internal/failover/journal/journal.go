// SPDX-License-Identifier: AGPL-3.0-only

// Package journal is the local record of the bootstrap that promotes the map group (design ha-phase5-part2.md, 3.4). The
// system database, the task engine and the leader are on the map group, so while it is being promoted no task row can be
// written: the steps B1 to B8 are recorded in a file of this server, written atomically and durably after every step,
// and read first by a restarted kingresd. The same step functions as the task run against it.
package journal

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// FileName is the journal inside the failover directory of the data directory.
const FileName = "map-journal.json"

// Steps of the bootstrap, in order (design 3.4).
const (
	StepDecide   = "B1 decide"
	StepFreeze   = "B2 freeze"
	StepLease    = "B3 take the fence lease"
	StepPin      = "B4 read and pin"
	StepPromote  = "B5 promote"
	StepCatalog  = "B6 make the catalog follow"
	StepLeader   = "B7 make the new leader"
	StepGateOpen = "B8 open the gate"
)

// Order lists the steps in the order they run.
var Order = []string{StepDecide, StepFreeze, StepLease, StepPin, StepPromote, StepCatalog, StepLeader, StepGateOpen}

// Pin is the standby chosen at B4: it is written once and never replaced (invariant I5).
type Pin struct {
	StandbyID int64  `json:"standbyId"`
	Server    string `json:"server"`
	Node      int32  `json:"node"`
	Instance  int    `json:"instance"`
}

// Event is one line of what the bootstrap did; it becomes a row of the report when the system database is writable.
type Event struct {
	At     time.Time      `json:"at"`
	Step   string         `json:"step"`
	Kind   string         `json:"kind"`
	Detail map[string]any `json:"detail,omitempty"`
}

// Journal is the content of the file.
type Journal struct {
	// ID names this bootstrap; a task that is recorded later carries it.
	ID string `json:"id"`
	// OldServer is the server whose primary of the map group failed.
	OldServer string `json:"oldServer"`
	// Term is the catalog term this server read before it began: a journal of a lower term is a finished bootstrap.
	Term      int64     `json:"term"`
	StartedAt time.Time `json:"startedAt"`
	// Trigger is manual or automatic; RequestedBy names the operator of a manual one.
	Trigger     string `json:"trigger"`
	RequestedBy string `json:"requestedBy,omitempty"`
	// AllowDataLoss is the acceptance of the operator of a manual request.
	AllowDataLoss bool `json:"allowDataLoss,omitempty"`
	// Done are the steps that finished.
	Done   map[string]time.Time `json:"done"`
	Pin    *Pin                 `json:"pin,omitempty"`
	Events []Event              `json:"events"`
	// Recorded says that the task row of the failover was inserted into the system database (B7), so that a restart
	// does not insert a second one.
	Recorded bool  `json:"recorded,omitempty"`
	TaskID   int64 `json:"taskId,omitempty"`
}

// ErrNewerTerm is returned by Begin when the journal on disk belongs to a bootstrap of a higher term than the one asked for.
var ErrNewerTerm = errors.New("journal: the journal belongs to a bootstrap of a newer term")

// Store keeps the journal of one server.
type Store struct {
	path string
	mu   sync.Mutex
	j    *Journal
	now  func() time.Time
}

// Open reads the journal of dir (the failover directory of the data directory), or starts with none. A file that cannot be
// parsed is an error: a bootstrap that may have been in progress must not be forgotten.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, FileName), now: time.Now}
	b, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return s, nil
	case err != nil:
		return nil, err
	}
	var j Journal
	if err := json.Unmarshal(b, &j); err != nil {
		return nil, fmt.Errorf("journal: %s cannot be read: %w", s.path, err)
	}
	if j.Done == nil {
		j.Done = map[string]time.Time{}
	}
	s.j = &j
	return s, nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.j, "", "  ")
	if err != nil {
		return err
	}
	return pki.WriteFile(s.path, append(b, '\n'), 0o600)
}

// Begin starts a bootstrap, or returns the one in progress (a restart: the same old server and term). A journal of a
// finished bootstrap of a lower term is replaced; one of a higher term refuses (ErrNewerTerm).
func (s *Store) Begin(id, oldServer string, term int64, trigger, requestedBy string, allowDataLoss bool) (Journal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j != nil {
		switch {
		case s.j.Term > term:
			return Journal{}, ErrNewerTerm
		case s.j.Term == term && s.j.OldServer == oldServer && !s.finishedLocked():
			return s.copyLocked(), nil
		}
	}
	s.j = &Journal{ID: id, OldServer: oldServer, Term: term, StartedAt: s.now().UTC(), Trigger: trigger, RequestedBy: requestedBy,
		AllowDataLoss: allowDataLoss, Done: map[string]time.Time{}, Events: []Event{}}
	return s.copyLocked(), s.saveLocked()
}

func (s *Store) finishedLocked() bool { return s.j != nil && !s.j.Done[StepGateOpen].IsZero() }

func (s *Store) copyLocked() Journal {
	c := *s.j
	c.Done = map[string]time.Time{}
	for k, v := range s.j.Done {
		c.Done[k] = v
	}
	c.Events = append([]Event(nil), s.j.Events...)
	if s.j.Pin != nil {
		p := *s.j.Pin
		c.Pin = &p
	}
	return c
}

// Current returns the journal, and false when there is none.
func (s *Store) Current() (Journal, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j == nil {
		return Journal{}, false
	}
	return s.copyLocked(), true
}

// Active says that a bootstrap began and its gate is not open: the task engine of this server neither claims nor runs.
func (s *Store) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.j != nil && !s.finishedLocked()
}

// Done records that a step finished (idempotent: the first time stays).
func (s *Store) Done(step string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j == nil {
		return errors.New("journal: no bootstrap is in progress")
	}
	if _, ok := s.j.Done[step]; ok {
		return nil
	}
	s.j.Done[step] = s.now().UTC()
	return s.saveLocked()
}

// IsDone says whether a step finished.
func (s *Store) IsDone(step string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.j != nil && !s.j.Done[step].IsZero()
}

// SetPin records the chosen standby once and answers the recorded one afterwards (the pin of a journal is never replaced).
func (s *Store) SetPin(p Pin) (Pin, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j == nil {
		return Pin{}, errors.New("journal: no bootstrap is in progress")
	}
	if s.j.Pin != nil {
		return *s.j.Pin, nil
	}
	s.j.Pin = &p
	return p, s.saveLocked()
}

// Event appends a line.
func (s *Store) Event(step, kind string, detail map[string]any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j == nil {
		return errors.New("journal: no bootstrap is in progress")
	}
	s.j.Events = append(s.j.Events, Event{At: s.now().UTC(), Step: step, Kind: kind, Detail: detail})
	return s.saveLocked()
}

// Recorded says that the task row was inserted with this id.
func (s *Store) Recorded(taskID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.j == nil {
		return errors.New("journal: no bootstrap is in progress")
	}
	s.j.Recorded, s.j.TaskID = true, taskID
	return s.saveLocked()
}

// Gate is the function the task engine asks (task.Options.Gate): false while a bootstrap is in progress.
func (s *Store) Gate() bool { return !s.Active() }
