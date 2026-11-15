// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// The files of this file live in the lease directory next to topology.json.
const (
	// WaitMarker is the name of the file whose presence means "this primary was not
	// running when kingresd started and its group lease is enforced: the lease
	// manager starts it once it holds the lease". It is not the fence marker (the
	// instance did not run, so nothing was fenced), and it is not an operator's
	// stop: that one is in memory and never makes the lease manager start anything.
	WaitMarker = "waitlease"
	// StandDownFile is where an orderly stop tells the watchdog that the instance is
	// shutting down on purpose (see StandDown).
	StandDownFile = "standdown"

	enforcedFile = "enforced.json"
)

// enforcedRecord is what the lease manager last knew, per group, about whether the
// lease is enforced. A cold start cannot ask the standby manager's database, so it
// reads this.
type enforcedRecord struct {
	Groups []int32 `json:"groups"`
}

func readEnforced(dir string) enforcedRecord {
	var r enforcedRecord
	if b, err := os.ReadFile(filepath.Join(dir, enforcedFile)); err == nil {
		_ = json.Unmarshal(b, &r)
	}
	return r
}

// recordEnforced notes that group is (or is no longer) enforced; it writes only a
// change. No record means not enforced.
func (m *Manager) recordEnforced(group int32, enforced bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.enf == nil {
		m.enf = map[int32]bool{}
		for _, g := range readEnforced(m.c.Dir).Groups {
			m.enf[g] = true
		}
	}
	if m.enf[group] == enforced {
		return
	}
	if enforced {
		m.enf[group] = true
	} else {
		delete(m.enf, group)
	}
	var rec enforcedRecord
	for g := range m.enf {
		rec.Groups = append(rec.Groups, g)
	}
	slices.Sort(rec.Groups)
	if rec.Groups == nil {
		rec.Groups = []int32{}
	}
	b, _ := json.Marshal(rec)
	if err := pki.WriteFile(filepath.Join(m.c.Dir, enforcedFile), b, 0o600); err != nil {
		m.c.Log.Warn("recording which group leases are enforced", "error", err)
	}
}

// PrepareStart runs once when kingresd starts, before it starts the instance.
// When the primary instance is not running and the lease of a group this server
// runs the primary of was enforced when the lease manager last knew, it writes
// the wait marker: the instance is then not started until the lease is held. A
// running instance (kingresd was restarted, the database kept running) is left
// alone and a stale marker is removed; no record means not enforced. It returns
// whether the instance must wait.
func PrepareStart(dir string, running bool) (bool, error) {
	marker := filepath.Join(dir, WaitMarker)
	wait := false
	if !running {
		var topo Topology
		if b, err := os.ReadFile(filepath.Join(dir, topologyFile)); err == nil {
			_ = json.Unmarshal(b, &topo)
		}
		rec := readEnforced(dir)
		for _, g := range topo.Groups {
			if slices.Contains(rec.Groups, g) {
				wait = true
			}
		}
	}
	if !wait {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		return false, nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return false, err
	}
	return true, pki.WriteFile(marker, []byte("waiting for the group lease\n"), 0o600)
}

// Waiting tells whether the instance waits for its lease to be started (the wait
// marker exists) or was fenced and waits for it to be started again.
func Waiting(dir string) bool {
	for _, n := range []string{WaitMarker, FencedMarker} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			return true
		}
	}
	return false
}

func waitMarkerExists(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, WaitMarker))
	return err == nil
}

// StandDown tells the watchdog that the postmaster identified by postmaster (its
// process id and start time, as instance.Manager.StopForShutdown passes them) was
// asked to shut down on purpose with a fast shutdown. It is written by the orderly stop of the service only, after that request: from then on
// PostgreSQL accepts no new session and ends the existing ones, so it can
// acknowledge no write after the lease ends, and an immediate shutdown by the
// watchdog could only turn a clean stop into a crash. The file names one postmaster
// (process id and start time), so it never applies to a later run of the instance,
// and a hung or killed kingresd is still covered because nothing else writes it.
func StandDown(dir, postmaster string) error {
	if postmaster == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return pki.WriteFile(filepath.Join(dir, StandDownFile), []byte(postmaster+"\n"), 0o600)
}
