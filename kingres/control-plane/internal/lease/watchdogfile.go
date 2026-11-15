// SPDX-License-Identifier: PostgreSQL

package lease

import (
	"encoding/json"
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
}

func (m *Manager) writeWatchdogFile(group int32) {
	if m.c.WatchdogFile == "" || !m.c.Enforced(group) {
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
	if err := pki.WriteFile(filepath.Clean(m.c.WatchdogFile), b, 0o600); err != nil {
		m.c.Log.Warn("writing the watchdog lease file", "error", err)
	}
}
