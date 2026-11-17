// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import (
	"context"
	"errors"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The instance mapping (design ha-phase5-part2.md, section 3.7). An instance
// directory keeps its id for life; what it serves is in its role file. The first
// instance of a server (NodeID, "the home instance") is where everything that is
// per server lives: the credentials, the managed settings, the rules every other
// instance copies. It is the primary of the server's own group until a failover
// makes it a standby, and then another instance of the server may be a primary
// (a promoted standby). Code that needs "the primary of group g", "the instance
// that serves the system database" or "every instance that takes writes" asks
// here and never assumes instance 1.

// LocalInstance is one instance directory of this server with what it serves.
type LocalInstance struct {
	// ID is the directory id; Port the TCP port the instance listens on.
	ID   int
	Port int
	Role instance.Role
}

// HomeID is the directory id of the home instance.
func (s *Service) HomeID() int { return NodeID }

// LocalInstances lists the instance directories of this server, ascending by id.
func (s *Service) LocalInstances() []LocalInstance {
	if !s.Enabled() {
		return nil
	}
	var out []LocalInstance
	for _, id := range s.mgr.IDs() {
		li := LocalInstance{ID: id, Role: s.mgr.MustRole(id)}
		if mk, err := s.mgr.Managed(id); err == nil {
			li.Port = mk.Port
		}
		if id == NodeID && li.Port == 0 {
			li.Port = s.cfg.PGPort
		}
		out = append(out, li)
	}
	return out
}

// WritableInstances lists the instances that are the primary of a group: the ones
// a statement that changes data, settings or access rules must reach. A standby
// receives all of that by replication or by the copy of the rules.
func (s *Service) WritableInstances() []LocalInstance {
	var out []LocalInstance
	for _, li := range s.LocalInstances() {
		if li.Role.Writable() {
			out = append(out, li)
		}
	}
	return out
}

// WritableIDs is the ids of WritableInstances. A server that has none (its home
// instance is a standby and it promoted nothing) answers the home instance, so
// that a caller that has to touch "the instance" of the server never gets
// nothing: it then meets the read-only instance's own refusal.
func (s *Service) WritableIDs() []int {
	var ids []int
	for _, li := range s.WritableInstances() {
		ids = append(ids, li.ID)
	}
	if len(ids) == 0 {
		ids = []int{NodeID}
	}
	return ids
}

// PrimaryOf returns the instance of this server that is the primary of group. An
// instance whose group is not known yet (the home instance of a server whose
// catalog row has not been read since the role file was added) stands in for any
// group when it is the only writable instance.
func (s *Service) PrimaryOf(group int32) (LocalInstance, bool) {
	w := s.WritableInstances()
	for _, li := range w {
		if li.Role.Group == group {
			return li, true
		}
	}
	if len(w) == 1 && w[0].Role.Group == instance.GroupUnknown {
		return w[0], true
	}
	return LocalInstance{}, false
}

// InstanceConn opens a service connection to the local instance with this id
// (the home instance included).
func (s *Service) InstanceConn(ctx context.Context, id int, db string) (*pgx.Conn, error) {
	if id == NodeID {
		return s.homeConn(ctx, db)
	}
	for _, li := range s.LocalInstances() {
		if li.ID == id {
			return s.ServiceConnAt(ctx, id, li.Port, db)
		}
	}
	return nil, errors.New("cluster: this server has no instance " + itoa(id))
}

// PrimaryConn opens a service connection to database db on the primary instance
// of group, which must run on this server.
func (s *Service) PrimaryConn(ctx context.Context, group int32, db string) (*pgx.Conn, error) {
	li, ok := s.PrimaryOf(group)
	if !ok {
		return nil, errors.New("cluster: this server does not run the primary of shard group " + itoa(int(group)))
	}
	return s.InstanceConn(ctx, li.ID, db)
}

// SystemConn opens a connection to the system database: on the instance of this
// server that is the primary of the map group when there is one, else on the home
// instance (a server that does not run the map group reads its own copy of the
// catalog there, and nothing is written through it).
func (s *Service) SystemConn(ctx context.Context) (*pgx.Conn, error) {
	if li, ok := s.PrimaryOf(0); ok && li.Role.Group == 0 {
		return s.InstanceConn(ctx, li.ID, sysdb.Database)
	}
	return s.homeConn(ctx, sysdb.Database)
}

// EachWritable calls f with a connection to database db of every writable
// instance of this server, in the order of the ids, and stops at the first error.
// A database or a role made on a server is made on every primary the server runs;
// the standbys get it by replication.
func (s *Service) EachWritable(ctx context.Context, db string, f func(c *pgx.Conn) error) error {
	for _, id := range s.WritableIDs() {
		c, err := s.InstanceConn(ctx, id, db)
		if err != nil {
			return err
		}
		err = f(c)
		_ = c.Close(context.WithoutCancel(ctx))
		if err != nil {
			return err
		}
	}
	return nil
}

// CatalogRow is what the role files need of one row of kingres.node.
type CatalogRow struct {
	Host  string
	Port  int
	Group int32
	Role  string
}

// SyncRoles brings the role files in line with the catalog's rows of this
// server's host: an instance without a role file gets one (the layout of the code
// before the files existed: directory 1 a primary, a directory from 100 up a
// standby of group id-100), and an instance whose group is not known yet gets the
// group the catalog names for its port. It never changes the role or the origin of
// a file that exists: a promotion or a demotion writes those, with the instance
// it concerns, at the moment it happens. It returns the instances that changed.
func (s *Service) SyncRoles(host string, rows []CatalogRow) []int {
	if !s.Enabled() {
		return nil
	}
	var changed []int
	for _, li := range s.LocalInstances() {
		cur, exists, err := s.mgr.RoleOf(li.ID)
		if err != nil {
			s.log.Warn("the role file of an instance is not valid; it is left as it is", "instance", li.ID, "error", err)
			continue
		}
		next := cur
		if cur.Group == instance.GroupUnknown || !exists {
			for _, r := range rows {
				if r.Host == host && r.Port == li.Port && r.Role == cur.Role {
					next.Group = r.Group
				}
			}
		}
		if exists && next == cur {
			continue
		}
		if err := s.mgr.WriteRole(li.ID, next); err != nil {
			s.log.Warn("writing the role file of an instance", "instance", li.ID, "error", err)
			continue
		}
		changed = append(changed, li.ID)
	}
	sort.Ints(changed)
	return changed
}

// reloadAll reloads every local instance that runs (a changed certificate, CA,
// revocation list or rule is read by all of them, standbys included).
func (s *Service) reloadAll(ctx context.Context) error {
	var first error
	for _, li := range s.LocalInstances() {
		if up, _ := s.mgr.Running(ctx, li.ID); up {
			if err := s.mgr.Reload(ctx, li.ID); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// ruleIDs are the instances that hold the rules and the mapped roles of this
// server: every writable instance. A standby gets them by SyncAccess from one of
// these.
func (s *Service) ruleIDs() []int { return s.WritableIDs() }

// ConnectID is the instance a signed-in role's connection goes to: the primary of
// the map group when this server runs it, else the first writable instance.
func (s *Service) ConnectID() int {
	if li, ok := s.PrimaryOf(0); ok && li.Role.Group == 0 {
		return li.ID
	}
	return s.WritableIDs()[0]
}

// AccessSourceID is the instance whose pg_hba.conf and pg_ident.conf the standbys
// of this server copy.
func (s *Service) AccessSourceID() int { return s.WritableIDs()[0] }

// portOf is the TCP port of a local instance.
func (s *Service) portOf(id int) int {
	for _, li := range s.LocalInstances() {
		if li.ID == id {
			return li.Port
		}
	}
	return s.cfg.PGPort
}

// writeIdentAll writes pg_ident.conf of every instance that holds the mapped
// roles (the map of the other servers is added by the manager).
func (s *Service) writeIdentAll(osUser string, roles []string) error {
	for _, id := range s.ruleIDs() {
		if err := s.mgr.WriteIdent(id, osUser, roles); err != nil {
			return err
		}
	}
	return nil
}

// stopStandbys stops every local instance that is not a primary (a server stops
// its standbys first, so that nothing replicates from a primary that is going
// away), and returns the first error.
func (s *Service) stopStandbys(ctx context.Context) error {
	var first error
	for _, li := range s.LocalInstances() {
		if li.Role.Writable() {
			continue
		}
		if err := s.mgr.Stop(ctx, li.ID); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// startWritable starts every primary this server runs (the home instance when it
// is the only one). A primary whose group lease is not held is refused by the
// start gate (instance.ErrLeaseRequired): the lease manager starts it when it
// holds the lease; the others are still started.
func (s *Service) startWritable(ctx context.Context) error {
	var gated error
	for _, id := range s.WritableIDs() {
		if !s.mgr.Exists(id) {
			continue
		}
		if err := s.mgr.Start(ctx, id); err != nil {
			if errors.Is(err, instance.ErrLeaseRequired) {
				gated = err
				continue
			}
			return err
		}
	}
	return gated
}

// SystemInstance is the instance SystemConn reads: the primary of the map group
// when this server runs it, else the home instance.
func (s *Service) SystemInstance() LocalInstance {
	if li, ok := s.PrimaryOf(0); ok && li.Role.Group == 0 {
		return li
	}
	for _, li := range s.LocalInstances() {
		if li.ID == NodeID {
			return li
		}
	}
	return LocalInstance{ID: NodeID, Port: s.cfg.PGPort, Role: instance.DefaultRole(NodeID)}
}

// GateInfo tells the start gate of the instance manager which group a directory
// is the primary of: ok is false for a standby, which the lease never holds back.
func (s *Service) GateInfo(id int) (group int32, primary bool) {
	r := s.mgr.MustRole(id)
	return r.Group, r.Writable()
}
