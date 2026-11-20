// SPDX-License-Identifier: AGPL-3.0-only

// Package fault is the fault-injection hook of the tests. It exists so that a
// partition, a one-way cut or a clock step can be produced inside the process
// that is under test, deterministically, without firewall rules or root.
//
// It is off unless kingresd was started with fault-dir. When it is on, the file
// <dir>/faults.json is read (at most every 100 ms) and describes the faults:
//
//	{"blockOutbound": ["127.0.0.3"], "blockInbound": ["127.0.0.3"], "clockOffsetMs": 3600000}
//
// "blockOutbound" makes this server's calls to the listed servers fail as
// unreachable; "blockInbound" makes it drop (close without an answer) the
// internal calls that come from them; the word "*" means every server.
// "clockOffsetMs" shifts the wall clock of the clock package. The file is
// written atomically by the test harness. With fault-dir unset nothing is read
// and the checks are a single nil comparison.
//
// The hook is on the internal API and the peer client only; it never touches the
// REST API, the instances or the files of the data directory, so a test that
// uses it cannot fake anything but the network and the wall clock.
package fault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileName is the name of the fault file inside the fault directory.
const FileName = "faults.json"

// Spec is the content of the fault file.
type Spec struct {
	BlockOutbound []string `json:"blockOutbound,omitempty"`
	BlockInbound  []string `json:"blockInbound,omitempty"`
	ClockOffsetMs int64    `json:"clockOffsetMs,omitempty"`
	// ForceEnforce makes the lease manager treat every group as having a standby
	// (self-fencing enforced), for tests that run before standbys exist.
	ForceEnforce bool `json:"forceEnforce,omitempty"`
	// DirectoryDelayMs holds a finishing server.add task for that long between reading the
	// servers of the cluster and writing the peer directory, which makes the window in
	// which another task introduces a server as wide as a test needs.
	DirectoryDelayMs int64 `json:"directoryDelayMs,omitempty"`
	// GrantDelayMs makes every lease request this server sends wait that long before it is
	// sent (then fail when the server is also blocked): a renewal round that takes long.
	GrantDelayMs int64 `json:"grantDelayMs,omitempty"`
	// FinishDelayMs holds a joining server between preparing its system database and
	// declaring itself a member.
	FinishDelayMs int64 `json:"finishDelayMs,omitempty"`
	// TaskExitPoint ends this process (as a kill does) when the task engine reaches that crash point
	// (task.Fault* names) of step TaskExitStep of a task of kind TaskExitKind; the tests of the failover use it
	// to end the leader at a step boundary. TaskExitStep -1 means any step.
	TaskExitPoint string `json:"taskExitPoint,omitempty"`
	TaskExitStep  int    `json:"taskExitStep,omitempty"`
	// FailoverPoint, with FailoverMode "fail" or "pause", makes the failover task fail (a code that is not retried) or pause
	// when it reaches "<step name>:before" (for example "Group 1: Make the catalog follow:before"): the tests of the failures
	// after the point of no return use it. Only tests write such a file.
	FailoverPoint string `json:"failoverPoint,omitempty"`
	FailoverMode  string `json:"failoverMode,omitempty"`
	// FailoverTimes limits how many times FailoverInjectionOnce fires for the point (the calls of the callee side, which a repeat
	// must meet healed); 0 means once.
	FailoverTimes int `json:"failoverTimes,omitempty"`
	// FailRewind makes the rebuild of an old primary treat pg_rewind as failed, so that the fallback (a new base backup)
	// is taken (the test of the old primary that comes back).
	FailRewind bool `json:"failRewind,omitempty"`
	// LedgerExitKind ends this process (as a kill does) at the write of the ledger publication of that kind ("tile", "frontier", "pending",
	// "checkpoint", "note", "state", "retired", "digest", "log"), after LedgerExitSkip writes of that kind since the fault was read: the tests of the
	// collector use it to cut a round at each of its writes.
	LedgerExitKind string `json:"ledgerExitKind,omitempty"`
	LedgerExitSkip int    `json:"ledgerExitSkip,omitempty"`
}

// Hook reads the fault file. A nil *Hook means "no faults" and is valid.
type Hook struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	checked time.Time
	spec    Spec
	// ledgerSeen counts the writes of the kind of the fault, from the moment the fault was first read.
	ledgerFor  string
	ledgerSkip int
	ledgerSeen int
	// onceKey and onceFired count the firings of FailoverInjectionOnce for the fault that is in force.
	onceKey   string
	onceFired int
}

// New returns a hook that polls dir, or nil when dir is empty.
func New(dir string) *Hook {
	if dir == "" {
		return nil
	}
	return &Hook{path: filepath.Join(dir, FileName), now: time.Now}
}

func (h *Hook) load() Spec {
	h.mu.Lock()
	defer h.mu.Unlock()
	if now := h.now(); now.Sub(h.checked) >= 100*time.Millisecond {
		h.checked = now
		var s Spec
		if b, err := os.ReadFile(h.path); err == nil {
			_ = json.Unmarshal(b, &s) // a half-written or absent file means no faults
		}
		h.spec = s
	}
	return h.spec
}

func listed(list []string, server string) bool {
	for _, s := range list {
		if s == "*" || s == server {
			return true
		}
	}
	return false
}

// OutboundBlocked tells whether calls to server must fail.
func (h *Hook) OutboundBlocked(server string) bool {
	return h != nil && listed(h.load().BlockOutbound, server)
}

// InboundBlocked tells whether calls from server must be dropped.
func (h *Hook) InboundBlocked(server string) bool {
	return h != nil && listed(h.load().BlockInbound, server)
}

// ClockOffset is the step of the wall clock.
func (h *Hook) ClockOffset() time.Duration {
	if h == nil {
		return 0
	}
	return time.Duration(h.load().ClockOffsetMs) * time.Millisecond
}

// ForceEnforce tells the lease manager to enforce self-fencing for every group
// (a test stand-in for "the group has a standby").
func (h *Hook) ForceEnforce() bool {
	return h != nil && h.load().ForceEnforce
}

// RewindFails says that pg_rewind of an old primary must be treated as failed (FailRewind).
func (h *Hook) RewindFails() bool {
	return h != nil && h.load().FailRewind
}

// DirectoryDelay is the pause of DirectoryDelayMs.
func (h *Hook) DirectoryDelay() time.Duration {
	if h == nil {
		return 0
	}
	return time.Duration(h.load().DirectoryDelayMs) * time.Millisecond
}

// FinishDelay is the pause of FinishDelayMs.
func (h *Hook) FinishDelay() time.Duration {
	if h == nil {
		return 0
	}
	return time.Duration(h.load().FinishDelayMs) * time.Millisecond
}

// GrantDelay is the pause of GrantDelayMs.
func (h *Hook) GrantDelay() time.Duration {
	if h == nil {
		return 0
	}
	return time.Duration(h.load().GrantDelayMs) * time.Millisecond
}

// TaskPoint is the crash hook of the task engine: it ends the process when the fault file asks for this point
// and step. Only tests write such a file.
func (h *Hook) TaskPoint(point string, _ int64, step int) {
	if h == nil {
		return
	}
	sp := h.load()
	if sp.TaskExitPoint == "" || sp.TaskExitPoint != point {
		return
	}
	if sp.TaskExitStep >= 0 && sp.TaskExitStep != step {
		return
	}
	os.Exit(137)
}

// LedgerWrite is the crash hook of the writes of the ledger publication: it ends the process when the fault file asks for a write of this
// kind and the writes before it were let through. Only tests write such a file.
func (h *Hook) LedgerWrite(kind string) {
	if h == nil {
		return
	}
	sp := h.load()
	if sp.LedgerExitKind == "" || sp.LedgerExitKind != kind {
		return
	}
	h.mu.Lock()
	if h.ledgerFor != sp.LedgerExitKind || h.ledgerSkip != sp.LedgerExitSkip {
		h.ledgerFor, h.ledgerSkip, h.ledgerSeen = sp.LedgerExitKind, sp.LedgerExitSkip, 0
	}
	h.ledgerSeen++
	hit := h.ledgerSeen > h.ledgerSkip
	h.mu.Unlock()
	if hit {
		os.Exit(137)
	}
}

// FailoverInjection is the mode ("fail", "pause" or "") that the fault file asks for at this point of the failover task.
func (h *Hook) FailoverInjection(point string) string {
	if h == nil {
		return ""
	}
	sp := h.load()
	if sp.FailoverPoint == "" || sp.FailoverPoint != point {
		return ""
	}
	return sp.FailoverMode
}

// FailoverInjectionOnce is FailoverInjection for the callee side of the failover: it fires at most FailoverTimes times (once when
// the field is 0) for the fault that is in force, so that the repeat of the call meets the state a real interruption leaves.
func (h *Hook) FailoverInjectionOnce(point string) string {
	if h == nil {
		return ""
	}
	sp := h.load()
	if sp.FailoverPoint == "" || sp.FailoverPoint != point {
		return ""
	}
	key := sp.FailoverPoint + "|" + sp.FailoverMode + "|" + fmt.Sprint(sp.FailoverTimes)
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.onceKey != key {
		h.onceKey, h.onceFired = key, 0
	}
	limit := sp.FailoverTimes
	if limit <= 0 {
		limit = 1
	}
	if h.onceFired >= limit {
		return ""
	}
	h.onceFired++
	return sp.FailoverMode
}
