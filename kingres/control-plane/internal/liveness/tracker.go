// SPDX-License-Identifier: AGPL-3.0-only

// Package liveness tracks which other servers of the cluster are alive, from the
// heartbeats they send and the answers to the heartbeats this server sends.
//
// Every decision here is made on the monotonic clock of this server (package
// clock): a heartbeat carries no time at all, the receiver notes when it
// arrived, and "silent for the failure timeout" is a monotonic duration. A step
// of the system clock therefore changes nothing but the wall time shown as
// lastHeartbeatAt. A server that was itself stopped for a while (SIGSTOP, a
// frozen virtual machine) must not conclude that everybody else died when it
// wakes up, so the tracker notices a gap in its own ticks and gives every peer a
// fresh window instead of judging them on a silence it could not observe.
package liveness

import (
	"sort"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

// Config holds the timeouts.
type Config struct {
	// Interval is the heartbeat interval; a gap of three intervals between two
	// ticks of the tracker means this process was stalled.
	Interval time.Duration
	// Suspect and Failure are the silences after which a peer is suspect and dead.
	Suspect, Failure time.Duration
}

type entry struct {
	// since is when the peer was first expected; silence is measured from the
	// later of since and the last heartbeat, so a peer that never answers
	// becomes dead too.
	since time.Duration
	heard time.Duration
	ever  bool
}

// Tracker implements ha.Liveness. It is safe for concurrent use.
type Tracker struct {
	clk  clock.Clock
	cfg  Config
	self string

	mu       sync.Mutex
	peers    map[string]*entry
	lastTick time.Duration
	cached   map[string]ha.PeerState
}

// New returns a tracker for the server self.
func New(clk clock.Clock, cfg Config, self string) *Tracker {
	return &Tracker{clk: clk, cfg: cfg, self: self, peers: map[string]*entry{}, cached: map[string]ha.PeerState{}, lastTick: clk.Mono()}
}

// Expect sets the servers heartbeats are expected from (self is ignored). A new
// server starts its silence now; a server no longer listed is forgotten.
func (t *Tracker) Expect(ids []string) {
	now := t.clk.Mono()
	t.mu.Lock()
	defer t.mu.Unlock()
	keep := map[string]bool{}
	for _, id := range ids {
		if id == t.self || id == "" {
			continue
		}
		keep[id] = true
		if t.peers[id] == nil {
			t.peers[id] = &entry{since: now}
		}
	}
	for id := range t.peers {
		if !keep[id] {
			delete(t.peers, id)
			delete(t.cached, id)
		}
	}
}

// Expected lists the servers heartbeats are expected from, sorted.
func (t *Tracker) Expected() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]string, 0, len(t.peers))
	for id := range t.peers {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// Heard records a heartbeat from (or an answer of) a server. A server that is
// not expected is ignored, which bounds the memory a stranger can use.
func (t *Tracker) Heard(id string) {
	now := t.clk.Mono()
	t.mu.Lock()
	defer t.mu.Unlock()
	if e := t.peers[id]; e != nil {
		e.heard, e.ever = now, true
	}
}

func (e *entry) from() time.Duration {
	if e.ever && e.heard > e.since {
		return e.heard
	}
	return e.since
}

func (t *Tracker) stateAt(e *entry, now time.Duration) ha.PeerState {
	switch silence := now - e.from(); {
	case silence >= t.cfg.Failure:
		return ha.PeerDead
	case silence >= t.cfg.Suspect:
		return ha.PeerSuspect
	case e.ever:
		return ha.PeerLive
	default:
		return ha.PeerUnknown
	}
}

// Tick is called by the heartbeat loop once per interval. It detects that this
// process was stalled (the gap since the last tick is over three intervals) and
// then restarts the silence of every peer, and it remembers the states it
// computed, which State answers while the process is stalled again.
func (t *Tracker) Tick() {
	now := t.clk.Mono()
	t.mu.Lock()
	defer t.mu.Unlock()
	if gap := now - t.lastTick; gap > 3*t.cfg.Interval {
		for _, e := range t.peers {
			e.since = now
			if e.ever {
				e.heard = now
			}
		}
	}
	t.lastTick = now
	for id, e := range t.peers {
		t.cached[id] = t.stateAt(e, now)
	}
}

// State implements ha.Liveness. The answer for this server itself is always live.
func (t *Tracker) State(id string) ha.PeerState {
	if id == t.self {
		return ha.PeerLive
	}
	now := t.clk.Mono()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.peers[id]
	if e == nil {
		return ha.PeerUnknown
	}
	if now-t.lastTick > 3*t.cfg.Interval {
		// This process was stalled since its last tick: the silence it would
		// measure now is its own. Answer what it knew then.
		if s, ok := t.cached[id]; ok {
			return s
		}
		return ha.PeerUnknown
	}
	return t.stateAt(e, now)
}

// Silence is how long a server has been silent, and false when the server is
// not expected (or is this server).
func (t *Tracker) Silence(id string) (time.Duration, bool) {
	now := t.clk.Mono()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.peers[id]
	if e == nil {
		return 0, false
	}
	return now - e.from(), true
}

// LastHeard implements ha.Liveness: the wall-clock time of the last heartbeat,
// derived from its monotonic age for display only.
func (t *Tracker) LastHeard(id string) (time.Time, bool) {
	if id == t.self {
		return t.clk.Wall(), true
	}
	now := t.clk.Mono()
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.peers[id]
	if e == nil || !e.ever {
		return time.Time{}, false
	}
	return t.clk.Wall().Add(-(now - e.heard)).UTC(), true
}
