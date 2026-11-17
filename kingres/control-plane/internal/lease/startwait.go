// SPDX-License-Identifier: AGPL-3.0-only

package lease

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// The files of this file live in the lease directory next to topology.json.
//
// The markers are per group (a server can run the primaries of several groups
// after a failover, and the lease of one must not hold back or release another
// one's instance): "fenced.<group>" and "waitlease.<group>". A marker without the
// suffix is the one an earlier version wrote, when a server ran exactly one
// primary: it applies to every group of the server until the lease manager has
// seen the catalog and renamed it (migrateLegacyMarkers), so an upgrade never
// loses a fence.
const (
	// WaitMarker is the base name of the file whose presence means "this primary was not
	// running when kingresd started and its group lease is enforced: the lease
	// manager starts it once it holds the lease". It is not the fence marker (the
	// instance did not run, so nothing was fenced), and it is not an operator's
	// stop: that one is in memory and never makes the lease manager start anything.
	WaitMarker = "waitlease"
	// StandDownFile is the base name of the file where an orderly stop tells the
	// watchdog of an instance that it is shutting down on purpose (see StandDown);
	// the suffix is the instance directory id.
	StandDownFile = "standdown"

	enforcedFile = "enforced.json"
)

// MarkerPath is the path of the marker with base name base (FencedMarker or
// WaitMarker) of a group.
func MarkerPath(dir, base string, group int32) string {
	return filepath.Join(dir, base+"."+strconv.Itoa(int(group)))
}

// FencedPath is the fence marker of a group.
func FencedPath(dir string, group int32) string { return MarkerPath(dir, FencedMarker, group) }

// StandDownPath is the stand-down file of the watchdog of an instance.
func StandDownPath(dir string, instanceID int) string {
	return filepath.Join(dir, StandDownFile+"."+strconv.Itoa(instanceID))
}

// markerExists tells whether the marker of the group is there, or the marker of
// the version that had one primary per server (which holds for every group).
func markerExists(dir, base string, group int32) bool {
	for _, p := range []string{MarkerPath(dir, base, group), filepath.Join(dir, base)} {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// AnyFenced tells whether any group of this server has a fence marker.
func AnyFenced(dir string) bool {
	m, _ := filepath.Glob(filepath.Join(dir, FencedMarker+"*"))
	return len(m) > 0
}

// removeMarker removes both spellings of a marker of a group and says whether
// one was there.
func removeMarker(dir, base string, group int32) bool {
	removed := false
	for _, p := range []string{MarkerPath(dir, base, group), filepath.Join(dir, base)} {
		if err := os.Remove(p); err == nil {
			removed = true
		}
	}
	return removed
}

// migrateLegacyMarkers replaces a marker of the one-primary layout by a marker of
// every group the server runs the primary of now. It never removes the old file
// before the per-group ones are on disk.
func migrateLegacyMarkers(dir string, groups []int32) {
	if len(groups) == 0 {
		return
	}
	for _, base := range []string{FencedMarker, WaitMarker} {
		legacy := filepath.Join(dir, base)
		b, err := os.ReadFile(legacy) //nolint:gosec // fixed names in the lease directory
		if err != nil {
			continue
		}
		ok := true
		for _, g := range groups {
			if err := pki.WriteFile(MarkerPath(dir, base, g), b, 0o600); err != nil {
				ok = false
			}
		}
		if ok {
			_ = os.Remove(legacy)
		}
	}
}

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

// PrepareStart runs once when kingresd starts, before it starts the instances.
// For every group of the cached topology whose primary is not running (running
// says so, per group) and whose lease was enforced when the lease manager last
// knew, it writes the wait marker of the group: that instance is then not started
// until the lease is held. A running instance (kingresd was restarted, the
// database kept running) is left alone and a stale marker is removed; no record
// means not enforced. It returns whether any instance must wait.
func PrepareStart(dir string, running func(group int32) bool) (bool, error) {
	var topo Topology
	if b, err := os.ReadFile(filepath.Join(dir, topologyFile)); err == nil {
		_ = json.Unmarshal(b, &topo)
	}
	migrateLegacyMarkers(dir, topo.Groups)
	rec := readEnforced(dir)
	waiting := false
	for _, g := range topo.Groups {
		if running(g) || !slices.Contains(rec.Groups, g) {
			if err := os.Remove(MarkerPath(dir, WaitMarker, g)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return false, err
			}
			continue
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return false, err
		}
		if err := pki.WriteFile(MarkerPath(dir, WaitMarker, g), []byte("waiting for the group lease\n"), 0o600); err != nil {
			return false, err
		}
		waiting = true
	}
	return waiting, nil
}

// Waiting tells whether the primary instance of the group waits for its lease to
// be started (the wait marker exists) or was fenced and waits for it to be
// started again.
func Waiting(dir string, group int32) bool {
	return markerExists(dir, WaitMarker, group) || markerExists(dir, FencedMarker, group)
}

func waitMarkerExists(dir string, group int32) bool { return markerExists(dir, WaitMarker, group) }

// StandDown tells the watchdog of an instance that the postmaster identified by
// postmaster (its process id and start time, as instance.Manager.StopForShutdown
// passes them) was asked to shut down on purpose with a fast shutdown. It is
// written by the orderly stop of the service only, after that request: from then
// on PostgreSQL accepts no new session and ends the existing ones, so it can
// acknowledge no write after the lease ends, and an immediate shutdown by the
// watchdog could only turn a clean stop into a crash. The file names one
// postmaster (process id and start time), so it never applies to a later run of
// the instance, and a hung or killed kingresd is still covered because nothing
// else writes it.
func StandDown(dir string, instanceID int, postmaster string) error {
	if postmaster == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := pki.WriteFile(StandDownPath(dir, instanceID), []byte(postmaster+"\n"), 0o600); err != nil {
		return err
	}
	if instanceID == 1 {
		// A watchdog started by the version before the instance mapping reads the old name.
		return pki.WriteFile(filepath.Join(dir, StandDownFile), []byte(postmaster+"\n"), 0o600)
	}
	return nil
}
