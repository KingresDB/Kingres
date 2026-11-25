// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
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

// setStandbyPort changes the port of a standby that has never run. The marker is the one authority for the port, so
// it is written first, then the settings, then kingres-managed.conf. A crash after any of the three leaves the marker
// with the new port and the other files with the old or the new one; reconcileStandbyPort, which runs before every
// start of a standby that has never run, makes them follow the marker. (The other order, which the first version
// had, left a conf with the new port under a marker with the old one: the instance then started on a port that
// nothing else knew.)
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
	dir, _ := m.Dir(id)
	b, _ := json.Marshal(Marker{NodeID: id, Port: port, CreatedAt: mk.CreatedAt, Magic: markerMagic})
	if err := writeAtomic(filepath.Join(dir, MarkerName), b); err != nil {
		return err
	}
	if err := m.crashPoint("marker"); err != nil {
		return err
	}
	s.Port = port
	if err := m.saveSettings(id, s); err != nil {
		return err
	}
	if err := m.crashPoint("settings"); err != nil {
		return err
	}
	if err := m.writeManagedConf(id, s); err != nil {
		return err
	}
	return m.crashPoint("conf")
}

// crashPoint lets a test end setStandbyPort after a step, as a crash would (nil in the product).
func (m *Manager) crashPoint(step string) error {
	if m.afterPortStep != nil {
		return m.afterPortStep(step)
	}
	return nil
}

// writeManagedConf renders and writes kingres-managed.conf of the instance from s (the port of s, not the marker's).
func (m *Manager) writeManagedConf(id int, s ManagedSettings) error {
	sock, _ := m.SocketDir(id)
	s.SocketDir = filepath.ToSlash(sock)
	s.Preload = m.preloadListFor(id)
	text, err := RenderManaged(s)
	if err != nil {
		return err
	}
	data, _ := m.DataDir(id)
	if err := writeAtomic(filepath.Join(data, "kingres-managed.conf"), []byte(text)); err != nil {
		return err
	}
	m.markColumnarPreload(id, text)
	return nil
}

var confPortLine = regexp.MustCompile(`(?m)^port = '(\d+)'\s*$`)

// reconcileStandbyPort makes the settings and kingres-managed.conf of a standby that has never run say the port of the
// marker, when a step of setStandbyPort was cut short. It does nothing for any other instance, for a marker without a
// port (the copy is not complete) or when the files already agree.
func (m *Manager) reconcileStandbyPort(id int) error {
	if !IsStandbyDirID(id) || !m.NeverStarted(id) {
		return nil
	}
	mk, err := m.Managed(id)
	if err != nil || mk.Port == 0 {
		return nil
	}
	s, err := m.Settings(id)
	if err != nil {
		return nil // no settings, no copy to start: the start itself reports it
	}
	data, _ := m.DataDir(id)
	conf, cerr := os.ReadFile(filepath.Join(data, "kingres-managed.conf")) //nolint:gosec // fixed name below the manager root
	confOK := false
	if cerr == nil {
		if sm := confPortLine.FindSubmatch(conf); sm != nil {
			confOK = string(sm[1]) == strconv.Itoa(mk.Port)
		}
	}
	if s.Port == mk.Port && confOK {
		return nil
	}
	m.log.Warn("the files of a standby that never ran named another port than the marker; they follow the marker", "instance", id, "port", mk.Port, "settings", s.Port)
	s.Port = mk.Port
	if err := m.saveSettings(id, s); err != nil {
		return err
	}
	return m.writeManagedConf(id, s)
}

// bindFailure says whether a startup log shows that the postmaster could not bind its TCP port because another program
// holds it: a line `could not bind IPv4 address ...: Address already in use` (or IPv6), or the hint "Is another postmaster
// already running on port" that PostgreSQL adds to it. A failure on a Unix-domain socket (`could not bind Unix address`,
// with the same hint) and an address that cannot be assigned (`Cannot assign requested address`) are not this: another
// port would not help, and a standby that cannot use the address it listens on must not wander to another port.
func bindFailure(log string) bool {
	prevUnix := false
	for _, line := range strings.Split(log, "\n") {
		switch {
		case strings.Contains(line, "could not bind Unix address"):
			prevUnix = true
			continue
		case strings.Contains(line, "could not bind IPv4") || strings.Contains(line, "could not bind IPv6"):
			if strings.Contains(line, "Address already in use") {
				return true
			}
		case strings.Contains(line, "Is another postmaster already running on port"):
			if !prevUnix {
				return true
			}
		}
		prevUnix = false
	}
	return false
}
