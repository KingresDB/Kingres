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
}

// Hook reads the fault file. A nil *Hook means "no faults" and is valid.
type Hook struct {
	path string
	now  func() time.Time

	mu      sync.Mutex
	checked time.Time
	spec    Spec
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
