// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// Keys of the internal calls of the freeze (design ha-phase5-part2.md, 3.1.2).
const (
	CallFreeze       = "POST /failover/freeze"
	CallUnfreeze     = "POST /failover/unfreeze"
	CallStandbysGone = "POST /failover/standbys-gone"
)

// FreezeCalls lists the keys, for registration.
var FreezeCalls = []string{CallFreeze, CallUnfreeze, CallStandbysGone}

// MaxFreeze and MaxGone bound what one call may ask for: a freeze or a "gone" that
// nobody renews must lapse by itself, or a forgotten one would keep a group from
// ever emptying its list again.
const (
	MaxFreeze = 10 * time.Minute
	MaxGone   = 10 * time.Minute
)

// FreezeRequest is the body of POST /failover/freeze: hold the synchronous list of
// the group for TTLMs (renewed by the task that decides the failover while it
// runs). Task and Epoch name the task, for the log.
type FreezeRequest struct {
	Group int32 `json:"group"`
	TTLMs int64 `json:"ttlMs"`
	Task  int64 `json:"task,omitempty"`
	Epoch int64 `json:"epoch,omitempty"`
}

// UnfreezeRequest is the body of POST /failover/unfreeze.
type UnfreezeRequest struct {
	Group int32 `json:"group"`
	// Task and Epoch name the run that lifts the freeze: a run of an older epoch than the one that holds the freeze
	// is refused (a deposed leader's late call must not lift the freeze of the run that took the task over).
	Task  int64 `json:"task,omitempty"`
	Epoch int64 `json:"epoch,omitempty"`
}

// GoneRequest is the body of POST /failover/standbys-gone: the standbys that run
// on Server are not planned any more as far as this server's primaries are
// concerned (the server failed and is being failed over), so a list that names
// them is emptied now and not after the grace. Clear takes the server back.
type GoneRequest struct {
	Server string `json:"server"`
	TTLMs  int64  `json:"ttlMs,omitempty"`
	Clear  bool   `json:"clear,omitempty"`
}

// Freeze holds the synchronous standby list of a group while a failover of the
// group is decided: the list is never emptied then (SyncState.Decide), so a standby
// that stops streaming does not make the old primary acknowledge commits that the
// new one will not have. It lapses after ttl unless renewed by another Freeze.
func (a *Agent) Freeze(group int32, ttl time.Duration) {
	_ = a.FreezeBy(group, ttl, 0, 0)
}

// ErrStaleFreeze says that the freeze is held by a run of the same task with a higher epoch.
var ErrStaleFreeze = errors.New("standby: the freeze belongs to a newer run of the task")

// FreezeBy is Freeze for the run (task, epoch) of a failover. A call of the same task with a lower epoch than the one
// that holds the freeze is refused (ErrStaleFreeze); a call of another task replaces it (a new failover of the group).
// An error is also returned when the freeze could not be written to disk: the caller was told "kept" only when it is
// kept across a restart, as the argument of the design needs.
func (a *Agent) FreezeBy(group int32, ttl time.Duration, task, epoch int64) error {
	if ttl <= 0 || ttl > MaxFreeze {
		ttl = MaxFreeze
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.frozenBy == nil {
		a.frozenBy = map[int32]freezeOwner{}
	}
	if own, ok := a.frozenBy[group]; ok && task != 0 && own.Task == task && epoch < own.Epoch {
		return ErrStaleFreeze
	}
	prev, had := a.frozen[group]
	a.frozen[group] = a.Clock.Mono() + ttl
	a.frozenBy[group] = freezeOwner{Task: task, Epoch: epoch}
	if err := a.persistLocked(); err != nil {
		// The freeze stays in memory (it is the safe direction) and the caller is told it is not durable.
		_ = prev
		_ = had
		return err
	}
	return nil
}

// Unfreeze lifts the freeze of a group.
func (a *Agent) Unfreeze(group int32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.frozen, group)
	delete(a.frozenBy, group)
	_ = a.persistLocked()
}

// UnfreezeBy lifts the freeze for the run (task, epoch). It lifts only a freeze of its own task (F3-14): a late call
// of another task, or a call of task 0, and a lower epoch of the task that holds the freeze are refused.
func (a *Agent) UnfreezeBy(group int32, task, epoch int64) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if task == 0 {
		return ErrStaleFreeze
	}
	if own, ok := a.frozenBy[group]; ok && (own.Task != task || epoch < own.Epoch) {
		return ErrStaleFreeze
	}
	delete(a.frozen, group)
	delete(a.frozenBy, group)
	return a.persistLocked()
}

// freezeOwner is the run that made a freeze.
type freezeOwner struct{ Task, Epoch int64 }

// isFrozen tells whether the group is frozen: by a call that has not lapsed, or by
// the Frozen hook.
func (a *Agent) isFrozen(group int32) bool {
	a.mu.Lock()
	until, ok := a.frozen[group]
	if ok && a.Clock.Mono() >= until {
		delete(a.frozen, group)
		ok = false
	}
	a.mu.Unlock()
	return ok || (a.Frozen != nil && a.Frozen(group))
}

// StandbysGone declares the standbys of a server gone for ttl (or takes the
// server back with clear).
func (a *Agent) StandbysGone(server string, ttl time.Duration, clear bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if clear {
		delete(a.gone, server)
		a.persistLocked()
		return
	}
	if ttl <= 0 || ttl > MaxGone {
		ttl = MaxGone
	}
	a.gone[server] = a.Clock.Mono() + ttl
	a.persistLocked()
}

// goneServers is the set of servers whose standbys are declared gone and not lapsed.
func (a *Agent) goneServers() map[string]bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := a.Clock.Mono()
	out := map[string]bool{}
	for s, until := range a.gone {
		if now >= until {
			delete(a.gone, s)
			continue
		}
		out[s] = true
	}
	return out
}

// dropGone returns the entries that are still planned. A frozen group keeps all of
// them: it is the group that is being failed over, and its list must not change.
func dropGone(entries []Entry, gone map[string]bool, frozen bool) []Entry {
	if frozen || len(gone) == 0 {
		return entries
	}
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if !gone[e.Server] {
			out = append(out, e)
		}
	}
	return out
}

// FrozenGroups lists the groups that are frozen now (for the state call).
func (a *Agent) FrozenGroups() []int32 {
	a.mu.Lock()
	groups := make([]int32, 0, len(a.frozen))
	for g := range a.frozen {
		groups = append(groups, g)
	}
	a.mu.Unlock()
	var out []int32
	for _, g := range groups {
		if a.isFrozen(g) {
			out = append(out, g)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// handleFreezeCall answers the three calls of the freeze.
func (a *Agent) handleFreezeCall(_ context.Context, key string, body []byte) (any, error) {
	bad := func(msg string) error {
		return &internalapi.CallError{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
	}
	switch key {
	case CallFreeze:
		var r FreezeRequest
		if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 || r.TTLMs <= 0 {
			return nil, bad("A group and a positive ttlMs are required.")
		}
		if err := a.FreezeBy(r.Group, time.Duration(r.TTLMs)*time.Millisecond, r.Task, r.Epoch); err != nil {
			if errors.Is(err, ErrStaleFreeze) {
				return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "fenced", Message: "The freeze belongs to a newer run of the task."}
			}
			a.Log.Error("standby: the freeze could not be recorded", "group", r.Group, "error", err)
			return nil, &internalapi.CallError{Status: http.StatusInternalServerError, Code: "freeze_not_recorded", Message: "The freeze is held in memory but could not be written to disk."}
		}
		a.Log.Info("standby: the synchronous list of a group is frozen", "group", r.Group, "ttlMs", r.TTLMs, "task", r.Task, "epoch", r.Epoch)
		return struct{}{}, nil
	case CallUnfreeze:
		var r UnfreezeRequest
		if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
			return nil, bad("A group is required.")
		}
		if err := a.UnfreezeBy(r.Group, r.Task, r.Epoch); err != nil {
			if errors.Is(err, ErrStaleFreeze) {
				return nil, &internalapi.CallError{Status: http.StatusConflict, Code: "fenced", Message: "The freeze belongs to a newer run of the task."}
			}
			return nil, &internalapi.CallError{Status: http.StatusInternalServerError, Code: "freeze_not_recorded", Message: "The freeze was lifted in memory but the file could not be written."}
		}
		return struct{}{}, nil
	case CallStandbysGone:
		var r GoneRequest
		if err := json.Unmarshal(body, &r); err != nil || r.Server == "" || len(r.Server) > 255 {
			return nil, bad("A server is required.")
		}
		a.StandbysGone(r.Server, time.Duration(r.TTLMs)*time.Millisecond, r.Clear)
		a.Log.Info("standby: the standbys of a server are declared gone", "server", r.Server, "clear", r.Clear)
		// Look at the lists now: the point of the call is that they are emptied at once.
		select {
		case a.kick <- struct{}{}:
		default:
		}
		return struct{}{}, nil
	}
	return nil, errors.New("no such call")
}

// dropGoneEntry says that the standby e is not planned any more for the list of its
// primary: its server was declared gone and the group is not the one being failed
// over (a frozen group keeps its list exactly as it is).
func dropGoneEntry(e Entry, gone map[string]bool, frozen bool) bool {
	return !frozen && gone[e.Server]
}

// freezeFileName is the file in the data directory that keeps the freezes and the "gone" servers across a restart of
// kingresd (FO-12): a restart on the primary's server that forgot a freeze would let the list be emptied after the
// grace while the failover is still being decided.
const freezeFileName = "failover-freeze.json"

// freezeFile holds the wall-clock end of each entry in milliseconds (the monotonic clock does not survive a restart).
// An entry whose end has passed by the wall clock when the file is read is dropped; one that is still in force gets
// the rest of its time on the monotonic clock. A wall clock that was set forward ends a freeze early: the task
// that decides a failover renews its freeze and checks the state (/failover/state) before it acts.
type freezeFile struct {
	Frozen map[int32]int64  `json:"frozen,omitempty"`
	Gone   map[string]int64 `json:"gone,omitempty"`
	// Owners are the runs that made the freezes (task and epoch).
	Owners map[int32]freezeOwner `json:"owners,omitempty"`
}

// persistLocked writes the freezes and the gone servers that have not lapsed. The caller holds a.mu.
func (a *Agent) persistLocked() error {
	if a.DataDir == "" {
		return nil
	}
	now := a.Clock.Mono()
	wall := time.Now()
	f := freezeFile{Frozen: map[int32]int64{}, Gone: map[string]int64{}, Owners: map[int32]freezeOwner{}}
	for g, until := range a.frozen {
		if until > now {
			f.Frozen[g] = wall.Add(until - now).UnixMilli()
		}
	}
	for s, until := range a.gone {
		if until > now {
			f.Gone[s] = wall.Add(until - now).UnixMilli()
		}
	}
	for g, o := range a.frozenBy {
		if _, ok := f.Frozen[g]; ok {
			f.Owners[g] = o
		}
	}
	b, _ := json.Marshal(f)
	if err := pki.WriteFile(filepath.Join(a.DataDir, freezeFileName), b, 0o600); err != nil {
		if a.Log != nil {
			a.Log.Warn("standby: recording the freezes", "error", err)
		}
		return err
	}
	return nil
}

// loadFreezes reads the file at the start of the agent.
func (a *Agent) loadFreezes() {
	if a.DataDir == "" {
		return
	}
	b, err := os.ReadFile(filepath.Join(a.DataDir, freezeFileName)) //nolint:gosec // fixed name in the data directory
	if err != nil {
		return
	}
	var f freezeFile
	if json.Unmarshal(b, &f) != nil {
		return
	}
	now := a.Clock.Mono()
	wall := time.Now()
	if a.frozenBy == nil {
		a.frozenBy = map[int32]freezeOwner{}
	}
	for g, end := range f.Frozen {
		if rest := time.UnixMilli(end).Sub(wall); rest > 0 {
			a.frozen[g] = now + min(rest, MaxFreeze)
			if o, ok := f.Owners[g]; ok {
				a.frozenBy[g] = o
			}
		}
	}
	for s, end := range f.Gone {
		if rest := time.UnixMilli(end).Sub(wall); rest > 0 {
			a.gone[s] = now + min(rest, MaxGone)
		}
	}
}

// ClearFreezeOnDisk removes the freeze of the group from the file of the data directory, while no kingresd runs on it (the local command that releases the hold of the
// map group: the failover that froze the list is given up, and a list that stays frozen would make the primary wait for a standby that is lost until the freeze
// lapses). It says whether a freeze was there. The "gone" servers are not touched.
func ClearFreezeOnDisk(dataDir string, group int32) (bool, error) {
	p := filepath.Join(dataDir, freezeFileName)
	b, err := os.ReadFile(p) //nolint:gosec // fixed name in the data directory
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var f freezeFile
	if err := json.Unmarshal(b, &f); err != nil {
		return false, fmt.Errorf("standby: %s cannot be read: %w", freezeFileName, err)
	}
	if _, ok := f.Frozen[group]; !ok {
		return false, nil
	}
	delete(f.Frozen, group)
	delete(f.Owners, group)
	b, _ = json.Marshal(f)
	return true, pki.WriteFile(p, b, 0o600)
}
