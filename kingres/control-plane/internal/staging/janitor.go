// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package staging

import (
	"errors"
	"fmt"
	"io/fs"
	"sort"
)

// RowInfo is what the system database says about one transfer id that has a row.
type RowInfo struct {
	// Remove is true when the row is expired or deleted, so its file may go.
	Remove bool
}

// SweepInput is what the janitor is told. The store does not read the system database.
type SweepInput struct {
	// HighestID is the highest transfer id the system database has ever issued (the sequence). A file with a higher id than this
	// means the database was restored from an older copy, and the janitor deletes nothing.
	HighestID int64
	// Rows has one entry for every transfer id that has a row, whatever its state. A file whose id has no entry has no row.
	Rows map[int64]RowInfo
	// ReadyIDs are the ids of the rows that say ready; one without a file is reported as lost.
	ReadyIDs []int64
}

// SweepReport says what a sweep did.
type SweepReport struct {
	Deleted []string // file names removed
	Young   []string // names that matched but are younger than the minimum age and would otherwise have gone
	Foreign []string // entries that do not match the name pattern, or match and are not regular files: never touched
	Kept    int      // matching files kept because a row wants them or a writer or a reservation holds them
	Lost    []int64  // ready rows whose file is missing
}

// Sweep is the janitor. It deletes a file only when all of these hold: the marker of the directory is intact; the name matches
// ^transfer-[0-9]+\.(part|kdx)$ (up to 18 digits); the entry is a regular file; the file is at least MinAge old; no writer and no
// reservation holds the id; the row is missing or says Remove; and the highest id on the disk is not above HighestID. Anything else
// stays. The directory is not walked: only its own entries are looked at.
func (s *Store) Sweep(in SweepInput) (SweepReport, error) {
	var rep SweepReport
	if err := s.readMarker(); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return rep, ErrDirNotMarked
		}
		return rep, err
	}
	entries, err := s.fs.ReadDir(s.dir)
	if err != nil {
		return rep, fmt.Errorf("staging: %w", err)
	}
	type cand struct {
		name  string
		id    int64
		final bool
	}
	var cands []cand
	var highest int64 = -1
	present := map[int64]bool{}
	for _, e := range entries {
		id, final, ok := ParseName(e.Name())
		if !ok {
			rep.Foreign = append(rep.Foreign, e.Name())
			continue
		}
		if id > highest {
			highest = id
		}
		cands = append(cands, cand{e.Name(), id, final})
	}
	if highest > in.HighestID {
		return rep, ErrRestoredDatabase
	}
	now := s.cfg.Now()
	for _, c := range cands {
		p := s.path(c.name)
		info, err := s.fs.Lstat(p)
		if err != nil {
			continue // gone between the listing and now
		}
		if !info.Mode().IsRegular() {
			rep.Foreign = append(rep.Foreign, c.name)
			continue
		}
		if c.final {
			present[c.id] = true
		}
		ri, hasRow := in.Rows[c.id]
		if hasRow && !ri.Remove {
			rep.Kept++
			continue
		}
		s.mu.Lock()
		_, reserved := s.res[c.id]
		writing := s.open[c.id]
		s.mu.Unlock()
		if reserved || writing {
			rep.Kept++
			if c.final {
				present[c.id] = true
			}
			continue
		}
		if now.Sub(info.ModTime()) < s.cfg.MinAge {
			rep.Young = append(rep.Young, c.name)
			continue
		}
		if err := s.fs.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return rep, fmt.Errorf("staging: %w", err)
		}
		if c.final {
			delete(present, c.id)
		}
		rep.Deleted = append(rep.Deleted, c.name)
	}
	for _, id := range in.ReadyIDs {
		if !present[id] {
			rep.Lost = append(rep.Lost, id)
		}
	}
	sort.Slice(rep.Lost, func(i, j int) bool { return rep.Lost[i] < rep.Lost[j] })
	return rep, nil
}
