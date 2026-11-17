// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// WatchdogFile is the lease file next to the instance that the watchdog reads
// (the contract between kingresd and cmd/kingres-watchdog): kingresd replaces
// it atomically after every successful renewal.
//
//	{"seq": 7, "validMs": 4200}
//
// seq changes with every write; validMs is how long the lease still lets the
// primary run, measured by kingresd at the moment of writing (renewal start plus
// duration minus margin, minus the time since). It carries no time of day: the
// watchdog measures with its own monotonic clock how long ago it last saw seq
// change and stops the instance when that exceeds validMs.
type WatchdogFile struct {
	Seq     uint64 `json:"seq"`
	ValidMs int64  `json:"validMs"`
	// Off says that kingresd stopped enforcing the lease of this instance on purpose (the
	// group has no standby, or the instance is no longer a primary): the watchdog does not
	// fence on a file that is not written any more. A later file without Off arms it again.
	Off bool `json:"off,omitempty"`
}

func (m *Manager) writeWatchdogFile(group int32) {
	wf := m.watchdogFile(group)
	if wf == "" {
		return
	}
	if !m.c.Enforced(group) {
		m.writeWatchdogOff(wf)
		return
	}
	m.mu.Lock()
	st := m.groups[group]
	rem := (st.validUntil - m.c.Margin - m.c.Clock.Mono()).Milliseconds()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	if rem < 1 {
		return
	}
	// The sequence only has to differ from every earlier one, also across a
	// restart of kingresd: the process start time in nanoseconds is its base.
	b, _ := json.Marshal(WatchdogFile{Seq: seq + m.base, ValidMs: rem})
	if err := pki.WriteFile(filepath.Clean(wf), b, 0o600); err != nil {
		m.c.Log.Warn("writing the watchdog lease file", "error", err)
	}
}

// writeWatchdogOff tells the watchdog of an instance that nothing is enforced for it now. It is
// written on every renewal of a group that is not enforced (so a change of enforcement is
// told before the last file can go stale, and a restart of kingresd tells it again), but only
// when a lease file exists: a watchdog that was never started has nothing to be told.
func (m *Manager) writeWatchdogOff(wf string) {
	wf = filepath.Clean(wf)
	if _, err := os.Stat(wf); err != nil {
		return
	}
	m.mu.Lock()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	b, _ := json.Marshal(WatchdogFile{Seq: seq + m.base, Off: true})
	if err := pki.WriteFile(wf, b, 0o600); err != nil {
		m.c.Log.Warn("writing the watchdog lease file", "error", err)
	}
}

// DisarmWatchdog tells the watchdog of the group's instance that the instance must not be fenced
// for a lapsed lease file any more: it is not a primary now (a demotion), or its group is no
// longer enforced. The caller does it before it stops writing the file.
func (m *Manager) DisarmWatchdog(group int32) {
	if wf := m.watchdogFile(group); wf != "" {
		m.writeWatchdogOff(wf)
	}
}
