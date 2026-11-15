// SPDX-License-Identifier: PostgreSQL

package standby

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// stateFile is the name of this server's copy of the standby list, below
// <data-dir>/standby. It is written with fsync and an atomic replace
// (pki.WriteFile): a crash leaves the old list or the new one.
const stateFile = "state.json"

// metaFile is the name of the file in a standby's instance directory (next to the
// marker, outside the data directory) that says which standby the directory
// holds. A directory without it is a base backup that did not finish.
const metaFile = "kingres-standby.json"

// state is what this server keeps: the list the leader last pushed and the
// standbys whose slot this server (as their primary) lost.
type state struct {
	Version  int64   `json:"version"`
	Replicas int     `json:"replicas"`
	Standbys []Entry `json:"standbys"`
	// Dropped maps a standby id to the generation whose slot was dropped over the
	// retention limit or found lost; a plan that carries a higher generation of
	// the standby clears it.
	Dropped map[int64]int64 `json:"dropped,omitempty"`
}

// localState is the persisted state with its lock.
type localState struct {
	dir string
	mu  sync.Mutex
	st  state
}

func openLocalState(dataDir string) (*localState, error) {
	dir := filepath.Join(dataDir, "standby")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("standby: creating %s: %w", dir, err)
	}
	l := &localState{dir: dir}
	b, err := os.ReadFile(filepath.Join(dir, stateFile)) //nolint:gosec // fixed name below the data directory
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return nil, fmt.Errorf("standby: reading %s: %w", stateFile, err)
	default:
		if err := json.Unmarshal(b, &l.st); err != nil {
			return nil, fmt.Errorf("standby: %s is not valid: %w", stateFile, err)
		}
	}
	return l, nil
}

func (l *localState) saveLocked() error {
	b, err := json.Marshal(l.st)
	if err != nil {
		return err
	}
	return pki.WriteFile(filepath.Join(l.dir, stateFile), b, 0o600)
}

// snapshot returns a copy of the state.
func (l *localState) snapshot() state {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.st
	s.Standbys = append([]Entry(nil), l.st.Standbys...)
	s.Dropped = map[int64]int64{}
	for k, v := range l.st.Dropped {
		s.Dropped[k] = v
	}
	return s
}

// replace stores a plan unless an older one than the stored is offered; it
// reports whether the plan was taken. Marks of lost slots whose standby was
// rebuilt (a higher generation) or removed are cleared.
func (l *localState) replace(p PlanRequest) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.Version < l.st.Version {
		return false, nil
	}
	l.st.Version, l.st.Replicas = p.Version, p.Replicas
	l.st.Standbys = append([]Entry(nil), p.Standbys...)
	sort.Slice(l.st.Standbys, func(i, j int) bool { return l.st.Standbys[i].ID < l.st.Standbys[j].ID })
	for id, gen := range l.st.Dropped {
		keep := false
		for _, e := range l.st.Standbys {
			if e.ID == id && e.Generation <= gen {
				keep = true
			}
		}
		if !keep {
			delete(l.st.Dropped, id)
		}
	}
	return true, l.saveLocked()
}

// markDropped records that the slot of a standby is lost.
func (l *localState) markDropped(e Entry) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.st.Dropped == nil {
		l.st.Dropped = map[int64]int64{}
	}
	if g, ok := l.st.Dropped[e.ID]; ok && g >= e.Generation {
		return nil
	}
	l.st.Dropped[e.ID] = e.Generation
	return l.saveLocked()
}

// entriesOfGroup returns the standbys of a group.
func (s state) entriesOfGroup(g int32) []Entry {
	var out []Entry
	for _, e := range s.Standbys {
		if e.Group == g {
			out = append(out, e)
		}
	}
	return out
}
