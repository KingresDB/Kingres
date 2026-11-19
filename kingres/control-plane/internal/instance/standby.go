// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// StandbyBaseID is added to a shard group id to make the directory id of the
// standby of that group on a server: the primary of a server is instance 1, so
// the standbys live in 100, 101, ... A server runs at most one standby per
// group (never two instances of one group on one server), so the id is unique on
// the server and deterministic: a restarted kingresd finds its standbys again
// without a database.
const StandbyBaseID = 100

// StandbyDirID is the instance directory id of the standby of a group.
func StandbyDirID(group int32) int { return StandbyBaseID + int(group) }

// IsStandbyDirID reports whether an instance directory id is a standby's.
func IsStandbyDirID(id int) bool { return id >= StandbyBaseID }

// StandbySpec describes a standby to create from a base backup of its primary.
type StandbySpec struct {
	// ID is the directory id (StandbyDirID).
	ID int
	// Settings is the managed configuration (port, TLS, the recovery settings in
	// Extra). SocketDir is filled in.
	Settings ManagedSettings
	// Trust is the set of servers the instance trusts (recorded like the
	// primary's, so that it keeps working after a promotion in a later phase).
	Trust NodeTrust
	// Conninfo is the replication connection string of pg_basebackup: host, port,
	// user and the certificate options, never a password.
	Conninfo string
	// Slot is the physical slot created on the primary beforehand.
	Slot string
}

// IDs lists the ids of the instance directories that carry our marker, ascending.
func (m *Manager) IDs() []int {
	entries, err := os.ReadDir(m.root)
	if err != nil {
		return nil
	}
	var ids []int
	for _, e := range entries {
		if id, err := strconv.Atoi(e.Name()); err == nil && m.Exists(id) {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// InitStandby creates the directory of a standby: it takes a base backup of the
// primary over the replication connection (the slot makes the primary keep the
// WAL the standby needs while the copy runs), writes the managed configuration
// of the standby and marks it as a standby with standby.signal. It does not start
// the instance. A failure removes what it created.
func (m *Manager) InitStandby(ctx context.Context, s StandbySpec) (err error) {
	dir, err := m.Dir(s.ID)
	if err != nil {
		return err
	}
	if !IsStandbyDirID(s.ID) {
		return invalid("instance id %d is not a standby id", s.ID)
	}
	if s.Conninfo == "" || s.Slot == "" {
		return invalid("a standby needs a connection string and a slot")
	}
	if err := s.Trust.Validate(); err != nil {
		return err
	}
	if _, statErr := os.Lstat(dir); statErr == nil {
		return fmt.Errorf("%w: %s", ErrExists, dir)
	}
	sock, err := m.SocketDir(s.ID)
	if err != nil {
		return err
	}
	if len(sock) > 90 {
		return invalid("socket directory path %q is too long (limit 90 bytes)", trunc(sock))
	}
	s.Settings.SocketDir = filepath.ToSlash(sock)
	managed, err := RenderManaged(s.Settings)
	if err != nil {
		return err
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		return fmt.Errorf("instance: creating %s: %w", dir, err)
	}
	defer func() {
		if err != nil {
			if rmErr := os.RemoveAll(dir); rmErr != nil {
				m.log.Error("removing failed standby directory", "dir", dir, "error", rmErr)
			}
			_ = os.RemoveAll(sock)
		}
	}()
	if err := os.MkdirAll(sock, 0o700); err != nil {
		return err
	}
	mk, _ := json.Marshal(Marker{NodeID: s.ID, Port: s.Settings.Port, CreatedAt: time.Now().UTC(), Magic: markerMagic})
	if err := os.WriteFile(filepath.Join(dir, MarkerName), mk, 0o600); err != nil {
		return err
	}
	// The mark comes before anything of the copy; the agent removes it when the standby is complete (F2-08).
	if err := m.MarkCopyUnfinished(s.ID); err != nil {
		return err
	}
	if err := m.WriteRole(s.ID, Role{Group: int32(s.ID - StandbyBaseID), Role: RoleStandby, Origin: OriginCopy}); err != nil {
		return err
	}
	if err := m.SetTrust(s.ID, s.Trust); err != nil {
		return err
	}
	data := filepath.Join(dir, "pgdata")
	// -X stream: the WAL needed to make the copy consistent travels on a second
	// connection; -w: never ask for a password (the certificate authenticates).
	out, runErr := m.run(ctx, "pg_basebackup", "-D", data, "-d", s.Conninfo, "-X", "stream", "-S", s.Slot,
		"--checkpoint=fast", "-w", "--no-password")
	if runErr != nil {
		return fmt.Errorf("instance: pg_basebackup: %w: %s", runErr, out)
	}
	if err := os.WriteFile(filepath.Join(data, "kingres-managed.conf"), []byte(managed), 0o600); err != nil {
		return err
	}
	if err := m.saveSettings(s.ID, s.Settings); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600)
}

// StandbyIdentMap is the pg_ident.conf map that lets kingresd, running as the
// operating system user it runs as, connect to a standby over the unix socket as
// the service role without a password. A standby is a copy of another server's
// data, so its service role has that server's password, not this one's; the
// socket directory is private to the user, which is the same trust as the password
// file of the data directory.
const StandbyIdentMap = "kingres_standby"

// SyncAccess makes pg_hba.conf and pg_ident.conf of the standby instance to the
// ones of the primary instance from, plus the rule that lets kingresd in through
// the unix socket (StandbyIdentMap), replacing a file only when it differs, and
// reloads the standby when one changed and it runs. It reports whether it changed
// anything.
func (m *Manager) SyncAccess(ctx context.Context, from, to int, osUser string) (bool, error) {
	if from == to {
		// A server whose only instance is a standby (no primary left on it) has nothing to copy the rules from; copying
		// a file onto itself would add the rule at every round.
		return false, nil
	}
	src, err := m.DataDir(from)
	if err != nil {
		return false, err
	}
	dst, err := m.DataDir(to)
	if err != nil {
		return false, err
	}
	if _, err := m.Managed(to); err != nil {
		return false, err
	}
	q, err := hbaName(osUser)
	if err != nil {
		return false, err
	}
	changed := false
	for _, f := range []string{"pg_hba.conf", "pg_ident.conf"} {
		b, err := os.ReadFile(filepath.Join(src, f)) //nolint:gosec // fixed names below the manager root
		if err != nil {
			return false, fmt.Errorf("instance: reading %s: %w", f, err)
		}
		var want string
		want = string(b)
		if f == "pg_hba.conf" {
			// A source that is itself a standby (or a rewound former primary) has the rule already.
			if !strings.Contains(want, "peer map="+StandbyIdentMap) {
				want = "local   all  " + ServiceRole + "  peer map=" + StandbyIdentMap + "\n" + want
			}
		} else if line := StandbyIdentMap + " " + q + " " + ServiceRole + "\n"; !strings.Contains(want, line) {
			want += line
		}
		have, err := os.ReadFile(filepath.Join(dst, f)) //nolint:gosec // fixed names below the manager root
		if err == nil && string(have) == want {
			continue
		}
		if err := writeAtomic(filepath.Join(dst, f), []byte(want)); err != nil {
			return false, err
		}
		changed = true
	}
	if changed {
		if up, _ := m.Running(ctx, to); up {
			return true, m.Reload(ctx, to)
		}
	}
	return changed, nil
}

// Rewind (it answers what pg_rewind printed, with --progress) brings the stopped data directory of instance id back in line with the
// primary that conninfo names (pg_rewind): it is for a standby whose history
// diverged from the primary's (a former primary), where it is cheaper than a new
// base backup. The source connection needs a database role, so conninfo names a
// database. The caller falls back to a new base backup when it fails.
//
// A former primary that was stopped with an immediate shutdown (a fence) was not shut down cleanly: pg_rewind then
// starts it in single-user mode to finish its crash recovery and stops it again (the program's default,
// --ensure-shutdown; the configuration of the instance, with the extension in shared_preload_libraries, is the one
// it runs with). A failure of that, or of the rewind itself, is the caller's reason to take a base backup (design
// 6.2 step 3 and 4, open point 11).
func (m *Manager) Rewind(ctx context.Context, id int, conninfo string) (string, error) {
	if err := m.rewindable(ctx, id); err != nil {
		return "", err
	}
	data, _ := m.DataDir(id)
	out, err := m.run(ctx, "pg_rewind", "--progress", "--target-pgdata="+data, "--source-server="+conninfo)
	if err != nil {
		return out, fmt.Errorf("instance: pg_rewind: %w: %s", err, out)
	}
	return out, os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600)
}

// RewindDryRun runs pg_rewind --dry-run --progress against the same source and returns what it printed: where the
// histories parted and how much would be copied. The target is not modified, except that pg_rewind finishes its crash
// recovery first when it was not shut down cleanly (the shutdown checkpoint it leaves is the end of its WAL).
func (m *Manager) RewindDryRun(ctx context.Context, id int, conninfo string) (string, error) {
	if err := m.rewindable(ctx, id); err != nil {
		return "", err
	}
	data, _ := m.DataDir(id)
	out, err := m.run(ctx, "pg_rewind", "--dry-run", "--progress", "--target-pgdata="+data, "--source-server="+conninfo)
	if err != nil {
		return out, fmt.Errorf("instance: pg_rewind --dry-run: %w: %s", err, out)
	}
	return out, nil
}

func (m *Manager) rewindable(ctx context.Context, id int) error {
	if _, err := m.Managed(id); err != nil {
		return err
	}
	if up, err := m.Running(ctx, id); err != nil || up {
		if err != nil {
			return err
		}
		return errors.New("instance: pg_rewind needs the instance stopped")
	}
	return nil
}

// ControlData reads pg_controldata of the stopped or running instance: the lines "name: value" as a map.
func (m *Manager) ControlData(ctx context.Context, id int) (map[string]string, error) {
	data, err := m.DataDir(id)
	if err != nil {
		return nil, err
	}
	out, err := m.run(ctx, "pg_controldata", "-D", data)
	if err != nil {
		return nil, fmt.Errorf("instance: pg_controldata: %w: %s", err, out)
	}
	res := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			res[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return res, nil
}

// RebaseStandby replaces the data directory of instance id with a base backup of the primary that s.Conninfo names and
// makes it a standby (standby.signal and the managed configuration of s.Settings). Everything outside the data
// directory (marker, role file, trust, settings, the files next to them) stays: it is for a former primary whose
// pg_rewind failed, where the instance directory is the server's own and must not be created again. The instance must
// be stopped, and the caller has proved that it may be overwritten (a role file that says standby and the mark of an
// unfinished copy). A failure leaves the data directory incomplete, which the mark says.
func (m *Manager) RebaseStandby(ctx context.Context, id int, s StandbySpec) error {
	mk, err := m.Managed(id)
	if err != nil {
		return err
	}
	if s.Conninfo == "" || s.Slot == "" {
		return invalid("a standby needs a connection string and a slot")
	}
	if up, err := m.Running(ctx, id); err != nil || up {
		if err != nil {
			return err
		}
		return errors.New("instance: the instance must be stopped before its data directory is replaced")
	}
	s.Settings.SocketDir = ""
	if sock, err := m.SocketDir(id); err == nil {
		s.Settings.SocketDir = filepath.ToSlash(sock)
	}
	if s.Settings.Port == 0 {
		s.Settings.Port = mk.Port
	}
	managed, err := RenderManaged(s.Settings)
	if err != nil {
		return err
	}
	data, err := m.DataDir(id)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(data); err != nil {
		return fmt.Errorf("instance: removing the old data directory: %w", err)
	}
	out, runErr := m.run(ctx, "pg_basebackup", "-D", data, "-d", s.Conninfo, "-X", "stream", "-S", s.Slot,
		"--checkpoint=fast", "-w", "--no-password")
	if runErr != nil {
		return fmt.Errorf("instance: pg_basebackup: %w: %s", runErr, out)
	}
	if err := os.WriteFile(filepath.Join(data, "kingres-managed.conf"), []byte(managed), 0o600); err != nil {
		return err
	}
	if err := m.saveSettings(id, s.Settings); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(data, "standby.signal"), nil, 0o600)
}

// withStandbyAccessRule adds, for an instance that is a copy of another server's data (a standby, or a promoted standby), the rule
// that lets kingresd in over the unix socket by peer authentication; the text of a primary's rules does not have it.
func withStandbyAccessRule(hba string, isCopy bool) string {
	if !isCopy || strings.Contains(hba, "peer map="+StandbyIdentMap) {
		return hba
	}
	return "local   all  " + ServiceRole + "  peer map=" + StandbyIdentMap + "\n" + hba
}

// withStandbyIdentLine adds the line of pg_ident.conf that maps the operating system user of kingresd to the service role for such
// an instance. A name that pg_hba.conf cannot carry leaves the text as it is.
func withStandbyIdentLine(ident, osUser string, isCopy bool) string {
	if !isCopy {
		return ident
	}
	q, err := hbaName(osUser)
	if err != nil {
		return ident
	}
	line := StandbyIdentMap + " " + q + " " + ServiceRole + "\n"
	if strings.Contains(ident, line) {
		return ident
	}
	if !strings.HasSuffix(ident, "\n") {
		ident += "\n"
	}
	return ident + line
}
