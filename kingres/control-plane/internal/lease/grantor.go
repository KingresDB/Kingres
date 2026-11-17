// SPDX-License-Identifier: AGPL-3.0-only

// Package lease implements the group leases of high availability (design
// ha-phase5.md, "Fencing by lease"): a server that runs the primary of a shard
// group must keep a lease granted by a majority of the cluster's servers, and
// stops its primary when it cannot. Two roles:
//
//   - the Grantor answers lease requests: it remembers one holder per group until
//     the lease has expired plus the safety margin, and persists that in its own
//     data directory, so that a restart cannot grant twice;
//   - the Manager (manager.go) is the holder side: it renews, fences and reports.
//
// Time. Only durations travel between servers. Every server measures with the
// monotonic clock of package clock; no wall-clock time is ever compared. The
// argument for safety, in the order of events, is in the README ("Leases and
// fencing"): a holder's lease is valid until roundStart+duration-margin (it
// stops its primary then), a grantor that granted at receipt time r>=roundStart
// refuses every other holder until r+duration+margin.
package lease

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// GrantsFile is the name of the grantor's persisted state in its directory.
const GrantsFile = "grants.json"

// Request is the body of the internal call POST /lease/grant: the caller (named
// by its certificate, never by the body) asks for the lease of a group.
type Request struct {
	Group int32 `json:"group"`
	// DurationMs is the validity the caller wants, measured from the start of its
	// renewal round.
	DurationMs int64 `json:"durationMs"`
	// DryRun asks whether the lease would be granted now, without granting it (used
	// by tests and by the failover code of part 2 to wait for the old lease).
	DryRun bool `json:"dryRun,omitempty"`
	// TransferTo, when set, hands the lease of the group from the caller (which must be the
	// holder of record) to the named server for a fresh duration, with no instant in which the
	// group has no holder (Grantor.Transfer).
	TransferTo string `json:"transferTo,omitempty"`
	// Release deletes the grant of the group when the caller is its holder (Grantor.Release).
	Release bool `json:"release,omitempty"`
}

// Reply is the answer of a grantor.
type Reply struct {
	Granted bool `json:"granted"`
	// Holder is the server that holds the lease at this grantor after the call.
	Holder string `json:"holder"`
	// WaitMs, when the lease was refused, is the time until this grantor would
	// grant it to another holder (a duration, never a time of day).
	WaitMs int64 `json:"waitMs,omitempty"`
}

// Entry is one remembered grant, for the state call and the views.
type Entry struct {
	Group  int32  `json:"group"`
	Holder string `json:"holder"`
	// RemainingMs is the time until the lease expires (0 when it has, the safety
	// margin not included).
	RemainingMs int64 `json:"remainingMs"`
}

type grant struct {
	holder string
	// expiry is on the grantor's monotonic clock: receipt time plus the duration.
	expiry   time.Duration
	duration time.Duration
}

type persisted struct {
	Version int              `json:"version"`
	Grants  []persistedGrant `json:"grants"`
}

type persistedGrant struct {
	Group      int32  `json:"group"`
	Holder     string `json:"holder"`
	DurationMs int64  `json:"durationMs"`
}

// Grantor grants leases. It is safe for concurrent use.
type Grantor struct {
	clk    clock.Clock
	margin time.Duration
	path   string

	mu     sync.Mutex
	grants map[int32]grant
}

// NewGrantor opens the grantor whose state lives in dir (created when missing).
// Every grant found there is treated as if it had been made just now for its
// full duration: after a restart the grantor does not know how much of it had
// passed, and the safe assumption is none. It therefore refuses other holders
// for at most duration+margin after the restart, which is the longest a lease
// can still be alive.
func NewGrantor(clk clock.Clock, margin time.Duration, dir string) (*Grantor, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("lease: creating %s: %w", dir, err)
	}
	g := &Grantor{clk: clk, margin: margin, path: filepath.Join(dir, GrantsFile), grants: map[int32]grant{}}
	b, err := os.ReadFile(g.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return g, nil
	case err != nil:
		return nil, fmt.Errorf("lease: reading the grants: %w", err)
	}
	var p persisted
	if err := json.Unmarshal(b, &p); err != nil || p.Version != 1 {
		// An unreadable file cannot be trusted either way. Refusing to start is
		// the only safe answer; an operator removes the file knowing that this
		// server then grants nothing for the next duration+margin only if she
		// also restarts the whole cluster; see the README.
		return nil, fmt.Errorf("lease: the grants file %s is not valid", g.path)
	}
	now := clk.Mono()
	for _, e := range p.Grants {
		d := time.Duration(e.DurationMs) * time.Millisecond
		g.grants[e.Group] = grant{holder: e.Holder, expiry: now + d, duration: d}
	}
	return g, nil
}

// Grant decides a request of holder for group. The grant is persisted (fsync and
// atomic replace) before it is reported when it changes the holder; a failure to
// persist refuses the grant, because an unrecorded grant could be forgotten by a
// restart.
func (g *Grantor) Grant(group int32, holder string, duration time.Duration, dryRun bool) (Reply, error) {
	if holder == "" || duration <= 0 {
		return Reply{}, errors.New("lease: a holder and a positive duration are required")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Mono()
	cur, have := g.grants[group]
	if have && cur.holder != holder {
		if until := cur.expiry + g.margin; now < until {
			return Reply{Granted: false, Holder: cur.holder, WaitMs: (until - now).Milliseconds() + 1}, nil
		}
	}
	if dryRun {
		return Reply{Granted: true, Holder: holder}, nil
	}
	next := grant{holder: holder, expiry: now + duration, duration: duration}
	if have && cur.holder == holder {
		// A renewal never shortens what is persisted: the file's duration is what a
		// restarted grantor assumes is still running.
		if cur.duration > duration {
			next.duration = cur.duration
		}
		if cur.expiry > next.expiry {
			next.expiry = cur.expiry
		}
	}
	if !have || cur.holder != holder || cur.duration < next.duration {
		old := g.grants[group]
		g.grants[group] = next
		if err := g.persistLocked(); err != nil {
			if have {
				g.grants[group] = old
			} else {
				delete(g.grants, group)
			}
			return Reply{}, fmt.Errorf("lease: recording the grant: %w", err)
		}
	} else {
		g.grants[group] = next
	}
	return Reply{Granted: true, Holder: holder}, nil
}

func (g *Grantor) persistLocked() error {
	p := persisted{Version: 1}
	for grp, e := range g.grants {
		p.Grants = append(p.Grants, persistedGrant{Group: grp, Holder: e.holder, DurationMs: e.duration.Milliseconds()})
	}
	sort.Slice(p.Grants, func(i, j int) bool { return p.Grants[i].Group < p.Grants[j].Group })
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return pki.WriteFile(g.path, b, 0o600)
}

// Snapshot lists the grants this server remembers.
func (g *Grantor) Snapshot() []Entry {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.clk.Mono()
	out := make([]Entry, 0, len(g.grants))
	for grp, e := range g.grants {
		rem := (e.expiry - now).Milliseconds()
		if rem < 0 {
			rem = 0
		}
		out = append(out, Entry{Group: grp, Holder: e.holder, RemainingMs: rem})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}
