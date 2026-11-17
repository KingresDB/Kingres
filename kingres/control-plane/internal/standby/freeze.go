// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
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
	if ttl <= 0 || ttl > MaxFreeze {
		ttl = MaxFreeze
	}
	a.mu.Lock()
	a.frozen[group] = a.Clock.Mono() + ttl
	a.mu.Unlock()
}

// Unfreeze lifts the freeze of a group.
func (a *Agent) Unfreeze(group int32) {
	a.mu.Lock()
	delete(a.frozen, group)
	a.mu.Unlock()
}

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
		return
	}
	if ttl <= 0 || ttl > MaxGone {
		ttl = MaxGone
	}
	a.gone[server] = a.Clock.Mono() + ttl
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
		a.Freeze(r.Group, time.Duration(r.TTLMs)*time.Millisecond)
		a.Log.Info("standby: the synchronous list of a group is frozen", "group", r.Group, "ttlMs", r.TTLMs, "task", r.Task, "epoch", r.Epoch)
		return struct{}{}, nil
	case CallUnfreeze:
		var r UnfreezeRequest
		if err := json.Unmarshal(body, &r); err != nil || r.Group < 0 {
			return nil, bad("A group is required.")
		}
		a.Unfreeze(r.Group)
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
