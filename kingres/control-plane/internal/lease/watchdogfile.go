// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// WatchdogFile is the lease file next to the instance that the watchdog reads
// (the contract between kingresd and cmd/kingres-watchdog): kingresd replaces
// it atomically after every renewal pass.
//
//	{"seq": 7, "validMs": 4200}
//
// seq changes with every write; validMs is how long the file lets the primary
// run, measured by kingresd at the moment of writing. While the group is
// enforced it is the rest of the lease (renewal start plus duration minus
// margin, minus the time since); while it is not enforced (no standby) it is
// duration minus margin, written again at every pass whether or not the lease was
// renewed. It carries no time of day: the watchdog measures with its own monotonic
// clock how long ago it last saw seq change and stops the instance when that
// exceeds validMs.
//
// Why a file that is not refreshed fences also when the group is not enforced (FO-01): the watchdog
// exists for a kingresd that is dead or hung. A group without a standby gets one later, and a failover of
// it proves the old primary fenced by the expiry of its lease at the grantors; a primary that was told
// "nothing to protect" for ever would still run then, beside the promoted standby. So only a live kingresd
// keeps a primary running: the file stays valid for as long as kingresd keeps writing it.
//
// Every file has validMs above zero, also Off: a watchdog of the version before the Off state
// (FO-05) ignores the field and counts validMs down like for a lease, and it must not read zero.
type WatchdogFile struct {
	Seq     uint64 `json:"seq"`
	ValidMs int64  `json:"validMs"`
	// Off says that the instance is not a primary now (a demotion that keeps it running as a standby): the
	// watchdog never fences on it, however long ago the file was written. A later file without Off arms it again
	// (a promotion writes one before the instance can accept a write). ValidMs of an Off file is long (offValid)
	// for the sake of a watchdog that does not know the field.
	Off bool `json:"off,omitempty"`
}

// offValid is the validMs of an Off file: only a watchdog that ignores Off counts it down.
const offValid = 24 * time.Hour

func (m *Manager) writeWatchdogFile(group int32) {
	wf := m.watchdogFile(group)
	if wf == "" {
		return
	}
	if !m.c.Enforced(group) {
		m.writeWatchdogIdle(wf)
		return
	}
	m.mu.Lock()
	st := m.groups[group]
	if st == nil {
		// The state of the group was dropped (a release, an end of adoption, a demotion) between the round that found the lease valid and this write: there
		// is nothing to write for a lease that is not held any more. Found by a cluster test of the rollback of a map group switchover (a nil dereference).
		m.mu.Unlock()
		return
	}
	rem := (st.validUntil - m.c.Margin - m.c.Clock.Mono()).Milliseconds()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	if rem < 1 {
		return
	}
	m.writeFile(wf, WatchdogFile{Seq: seq + m.base, ValidMs: rem})
}

func (m *Manager) writeFile(wf string, f WatchdogFile) {
	b, _ := json.Marshal(f)
	// The sequence only has to differ from every earlier one, also across a
	// restart of kingresd: the process start time in nanoseconds is its base.
	if err := pki.WriteFile(filepath.Clean(wf), b, 0o600); err != nil {
		m.c.Log.Warn("writing the watchdog lease file", "error", err)
	}
}

// writeWatchdogIdle refreshes the file of a group that is not enforced: it stays valid for duration minus
// margin, as a lease would, and is written at every pass (also when the lease was not renewed: a group that is
// not enforced does not lose its primary for that, but a kingresd that stops writing does). It is written only when a
// lease file exists: a watchdog that was never started has nothing to be told.
func (m *Manager) writeWatchdogIdle(wf string) {
	if _, err := os.Stat(filepath.Clean(wf)); err != nil {
		return
	}
	m.mu.Lock()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	m.writeFile(wf, WatchdogFile{Seq: seq + m.base, ValidMs: (m.c.Duration - m.c.Margin).Milliseconds()})
}

// writeWatchdogOff tells the watchdog of an instance that it is not a primary now.
func (m *Manager) writeWatchdogOff(wf string) {
	if _, err := os.Stat(filepath.Clean(wf)); err != nil {
		return
	}
	m.mu.Lock()
	m.seq++
	seq := m.seq
	m.mu.Unlock()
	m.writeFile(wf, WatchdogFile{Seq: seq + m.base, ValidMs: offValid.Milliseconds(), Off: true})
}

// DisarmWatchdog tells the watchdog of the group's instance that the instance must not be fenced
// for a lapsed lease file any more: it is not a primary now (a demotion that keeps it running). The
// caller does it before it stops writing the file. It is not for a group that merely has no standby: that
// is the refreshed file of writeWatchdogIdle.
func (m *Manager) DisarmWatchdog(group int32) {
	if wf := m.watchdogFile(group); wf != "" {
		m.writeWatchdogOff(wf)
	}
}
