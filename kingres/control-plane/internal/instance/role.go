// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
)

// RoleFileName is the file in an instance directory (next to the marker, outside
// the data directory) that says what the instance serves.
//
// An instance directory keeps its id for life (the first instance of a server is
// 1, the standby of a group is 100 plus the group); what the instance is changes
// with a failover: a standby becomes the primary of its group, the old primary
// becomes a standby. The role file is what kingresd reads instead of assuming that
// directory 1 is the primary and the directories from 100 up are standbys
// (design ha-phase5-part2.md, section 3.7).
const RoleFileName = "kingres-role.json"

// Instance roles.
const (
	RolePrimary = "primary"
	RoleStandby = "standby"
	// RoleUnknown is what MustRole answers for a role file that is broken or cannot be read: neither a primary nor
	// a standby, so nothing is sent to it as a primary and nothing deletes it as a standby (FO-09).
	RoleUnknown = "unknown"
)

// Origins: where the data of an instance came from.
const (
	// OriginOwn is data this server initialised: its service role has this server's
	// service password.
	OriginOwn = "own"
	// OriginCopy is a physical copy of another server's instance (a standby, or an
	// old primary rebuilt from one): its service role has the other server's
	// password, so kingresd reaches it by peer authentication on the private socket.
	OriginCopy = "copy"
	// OriginUnknown goes with RoleUnknown.
	OriginUnknown = "unknown"
)

// GroupUnknown is the group of an instance that was initialised before the
// catalog existed (the home instance of a new server): the catalog's row of its
// host and port names the group, and the first look at the catalog writes it.
const GroupUnknown int32 = -1

// Role is the content of the role file.
type Role struct {
	// Group is the shard group the instance serves, GroupUnknown until the catalog says.
	Group int32 `json:"group"`
	// Role is RolePrimary or RoleStandby.
	Role string `json:"role"`
	// Origin is OriginOwn or OriginCopy.
	Origin string `json:"origin"`
	// Term is the catalog term the role was written under (0 when unknown).
	Term int64 `json:"term"`
}

// Writable reports whether the instance is a primary: the only kind a statement
// that changes data or settings is sent to.
func (r Role) Writable() bool { return r.Role == RolePrimary }

// IsStandby reports whether the role file says standby (an unknown role is not one).
func (r Role) IsStandby() bool { return r.Role == RoleStandby }

// Valid checks the fields.
func (r Role) Valid() error {
	if r.Role != RolePrimary && r.Role != RoleStandby {
		return fmt.Errorf("instance: the role %q is not primary or standby", r.Role)
	}
	if r.Origin != OriginOwn && r.Origin != OriginCopy {
		return fmt.Errorf("instance: the origin %q is not own or copy", r.Origin)
	}
	if r.Group < GroupUnknown {
		return errors.New("instance: the group is not valid")
	}
	return nil
}

// DefaultRole is what a directory without a role file is: the layout of the code
// before the role file existed. A directory from 100 up is the standby of group
// id-100 and a copy; any other directory is a primary this server initialised,
// whose group the catalog tells.
func DefaultRole(id int) Role {
	if IsStandbyDirID(id) {
		return Role{Group: int32(id - StandbyBaseID), Role: RoleStandby, Origin: OriginCopy}
	}
	return Role{Group: GroupUnknown, Role: RolePrimary, Origin: OriginOwn}
}

// RoleOf returns the role of the instance with directory id: the role file, or
// DefaultRole when there is none (exists tells which). An unreadable or invalid
// file is an error and is never replaced by the default: the answer "primary" for
// an instance that is a standby would send writes to it.
func (m *Manager) RoleOf(id int) (r Role, exists bool, err error) {
	dir, err := m.Dir(id)
	if err != nil {
		return Role{}, false, err
	}
	b, err := os.ReadFile(filepath.Join(dir, RoleFileName)) //nolint:gosec // fixed name below the manager root
	if errors.Is(err, os.ErrNotExist) {
		return DefaultRole(id), false, nil
	}
	if err != nil {
		return Role{}, false, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Role{}, false, fmt.Errorf("instance: %s of instance %d is not valid: %w", RoleFileName, id, err)
	}
	if err := r.Valid(); err != nil {
		return Role{}, false, fmt.Errorf("instance %d: %w", id, err)
	}
	return r, true, nil
}

// MustRole is RoleOf for the places that cannot do anything with an error. A role file that cannot be read or is
// broken is never replaced by the default of the directory id (FO-09): a promoted primary in a directory from 100
// up would become a standby (no lease, no watchdog, deletable), a demoted instance of directory 1 a primary. The
// answer is RoleUnknown: not writable, not a standby, not a copy.
func (m *Manager) MustRole(id int) Role {
	r, _, err := m.RoleOf(id)
	if err != nil {
		m.log.Warn("reading the role file of an instance", "instance", id, "error", err)
		return Role{Group: GroupUnknown, Role: RoleUnknown, Origin: OriginUnknown}
	}
	return r
}

// WriteRole writes the role file atomically (a crash leaves the old file or the
// new one) and makes it durable before it returns.
func (m *Manager) WriteRole(id int, r Role) error {
	if err := r.Valid(); err != nil {
		return err
	}
	dir, err := m.Dir(id)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(r)
	return pki.WriteFile(filepath.Join(dir, RoleFileName), append(b, '\n'), 0o600)
}

// IsCopy reports whether the instance holds a physical copy of another server's
// data (its service password is not this server's): decided by the role file, and
// by the directory id only for a directory that has none.
func (m *Manager) IsCopy(id int) bool { return m.MustRole(id).Origin == OriginCopy }

// IsWritable reports whether the instance is the primary of its group.
func (m *Manager) IsWritable(id int) bool { return m.MustRole(id).Writable() }

// CopyUnfinishedMarker is the file in an instance directory (next to the role file, outside the data directory) that
// says "a copy is being made into this directory and is not complete". It is written first and removed last
// (F2-08): an unfinished copy is recognised by this positive mark, never by the absence of the last file, which a
// complete standby whose bookkeeping file cannot be read lacks as well.
const CopyUnfinishedMarker = "kingres-copy-unfinished"

// MarkCopyUnfinished writes the mark (durably) before a copy, a rewind or any other change that leaves the data
// directory incomplete.
func (m *Manager) MarkCopyUnfinished(id int) error {
	dir, err := m.Dir(id)
	if err != nil {
		return err
	}
	return pki.WriteFile(filepath.Join(dir, CopyUnfinishedMarker), []byte(CopyStarted+"\n"), 0o600)
}

// ClearCopyUnfinished removes the mark: the last thing a copy does.
func (m *Manager) ClearCopyUnfinished(id int) error {
	dir, err := m.Dir(id)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, CopyUnfinishedMarker)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// CopyUnfinished says whether the mark exists.
func (m *Manager) CopyUnfinished(id int) bool {
	dir, err := m.Dir(id)
	if err != nil {
		return false
	}
	_, err = os.Lstat(filepath.Join(dir, CopyUnfinishedMarker))
	return err == nil
}

// SetCopyStep records how far a copy into the directory has come, in the mark itself ("started" when the mark is
// written): a rewind that was interrupted leaves a directory that only a new base backup can repair, one that finished
// does not need to be done again.
func (m *Manager) SetCopyStep(id int, step string) error {
	dir, err := m.Dir(id)
	if err != nil {
		return err
	}
	return pki.WriteFile(filepath.Join(dir, CopyUnfinishedMarker), []byte(step+"\n"), 0o600)
}

// CopyStep returns the step recorded in the mark ("" when there is no mark).
func (m *Manager) CopyStep(id int) string {
	dir, err := m.Dir(id)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, CopyUnfinishedMarker)) //nolint:gosec // fixed name below the manager root
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Steps of a copy into an existing directory (CopyStep).
const (
	CopyStarted   = "started"
	CopyRewinding = "rewinding"
	CopyRewound   = "rewound"
	CopyBackup    = "basebackup"
)
