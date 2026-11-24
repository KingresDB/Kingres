// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// NeverStartedMarker is the file in the directory of a standby (next to the marker, outside the data directory) that says
// "this instance has not run once yet". InitStandby writes it, the first successful start removes it. While it exists
// the port of the instance is provisional: nobody was ever connected to it, so a start that fails because the port is
// taken may choose another one (StartStandby). A standby without the file (one that ran, or one made before this file
// existed) keeps its port for good.
const NeverStartedMarker = "kingres-never-started"

func (m *Manager) markNeverStarted(id int) error {
	dir, err := m.Dir(id)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, NeverStartedMarker), []byte("the instance has not run yet\n"))
}

func (m *Manager) clearNeverStarted(id int) {
	if dir, err := m.Dir(id); err == nil {
		_ = os.Remove(filepath.Join(dir, NeverStartedMarker))
	}
}

// NeverStarted says whether the instance was made as a standby and has not run once since.
func (m *Manager) NeverStarted(id int) bool {
	dir, err := m.Dir(id)
	if err != nil {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, NeverStartedMarker))
	return err == nil
}

// StartStandby starts the instance like Start. When the instance has never run and the start failed because its port
// could not be bound (another program took it between the choice of the port and the start), it asks pick for another
// free port, records it (managed configuration, settings and marker, in the one step under the lifecycle lock) and
// starts once more. It does this at most once per call, and never for another error or for an instance that has run:
// the port of such an instance is known to others and stays. The caller reads the port again with Managed and records
// it wherever it keeps the port (a standby's meta file) before it reports it.
func (m *Manager) StartStandby(ctx context.Context, id int, pick func() (int, error)) error {
	unlock, err := m.lifecycle(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	err = m.start(ctx, id)
	if err == nil || pick == nil || !IsPortInUse(err) || !IsStandbyDirID(id) || !m.NeverStarted(id) {
		return err
	}
	old, _ := m.Managed(id)
	p, perr := pick()
	if perr != nil {
		return fmt.Errorf("%w (and no other port: %w)", err, perr)
	}
	if p == old.Port {
		return err
	}
	if serr := m.setStandbyPort(id, p); serr != nil {
		return fmt.Errorf("%w (and the new port %d was not recorded: %w)", err, p, serr)
	}
	m.log.Warn("the port of a new standby instance was taken; another one is used", "instance", id, "from", old.Port, "to", p)
	return m.start(ctx, id)
}

// setStandbyPort changes the port of a standby that has never run: the managed configuration and the settings first,
// the marker last (the marker is what Managed reports, so a crash in between leaves the old port reported while the
// instance would start on the new one, and the next start that fails the same way chooses again).
func (m *Manager) setStandbyPort(id, port int) error {
	if !IsStandbyDirID(id) || !m.NeverStarted(id) {
		return errors.New("instance: only the port of a standby that has not run yet may be changed")
	}
	mk, err := m.Managed(id)
	if err != nil {
		return err
	}
	if up, err := m.Running(context.Background(), id); err != nil || up {
		if err != nil {
			return err
		}
		return errors.New("instance: the port of a running instance cannot be changed")
	}
	s, err := m.Settings(id)
	if err != nil {
		return err
	}
	s.Port = port
	if err := m.WriteManaged(id, s); err != nil {
		return err
	}
	dir, _ := m.Dir(id)
	b, _ := json.Marshal(Marker{NodeID: id, Port: port, CreatedAt: mk.CreatedAt, Magic: markerMagic})
	return writeAtomic(filepath.Join(dir, MarkerName), b)
}
