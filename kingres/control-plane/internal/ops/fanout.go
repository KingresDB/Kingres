// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// Cluster-wide operations (design control-plane-3.md, D4: "operations that
// act on every primary of the cluster really run on every server, as one task
// with one step per server; a server that is down makes the task wait and
// report which server it is waiting for, it does not skip it").
//
// Creating and dropping databases and replacing the network rules are tasks
// of the contract with one step per other server. Role and grant changes are
// synchronous operations of the contract: they run on this server as the
// signed-in role as before, then on every other server at once; a server that
// cannot be reached then gets a task (kind replicate.apply, not in the
// contract's list of task kinds and so hidden from the task API) that waits
// for it and never gives up.

// OtherServer is another server of the cluster.
type OtherServer struct {
	ID     string
	PGPort int
}

// Fanout reaches the other servers; nil means this server is alone.
type Fanout interface {
	// Others returns the other active servers.
	Others(ctx context.Context) ([]OtherServer, error)
	// Apply runs a cluster-wide operation on a server. A server that cannot be
	// reached gives an error wrapping peerclient.ErrUnreachable.
	Apply(ctx context.Context, server, kind string, params any) error
	// Register registers a server in a sharded database with kingres.add_node.
	Register(ctx context.Context, db string, s OtherServer) error
}

// Kinds of ApplyLocal.
const (
	ApplyDatabaseCreate = "database.create"
	ApplyDatabaseDrop   = "database.drop"
	ApplyNetwork        = "network.apply"
	ApplyRoleSync       = "role.sync"
	ApplyRoleDrop       = "role.drop"
	ApplyGrant          = "grant"
	// ApplyRoleChanged tells a server that a role changed: it ends the role's
	// sessions and forgets cached role checks (no database change).
	ApplyRoleChanged = "role.changed"
)

// KindReplicate is the hidden task kind that brings servers up to date.
const KindReplicate = "replicate.apply"

type networkApply struct {
	Rules       []api.NetworkAccessRule `json:"rules"`
	TLSRequired bool                    `json:"tlsRequired"`
}

type roleSync struct {
	Roles []wire.RoleSpec `json:"roles"`
}

type roleDrop struct {
	Name string `json:"name"`
}

// grantOp is a grant change as applied on another server.
type grantOp struct {
	Op       string                    `json:"op"` // grant, revoke, template
	Grant    *api.GrantRequest         `json:"grant,omitempty"`
	Template *api.GrantTemplateRequest `json:"template,omitempty"`
}

// ApplyLocal runs one cluster-wide operation on this server (the internal
// call POST /apply, made by the leader). The kinds are a closed set and the
// parameters are structured values, never SQL or paths.
func (o *Ops) ApplyLocal(ctx context.Context, kind string, raw json.RawMessage) error {
	switch kind {
	case ApplyDatabaseCreate:
		var p createParams
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		if !dbNameRE.MatchString(p.Name) || protectedDB[p.Name] {
			return errors.New("this database name cannot be created")
		}
		if err := o.createDB(ctx, p); err != nil {
			return err
		}
		// kingres_shard is installed by kingres.add_node on the leader.
		return o.installExtensions(ctx, p.Name, false, p.LedgerMode != "off")
	case ApplyDatabaseDrop:
		var p dropParams
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		return o.dropDB(ctx, p)
	case ApplyNetwork:
		var p networkApply
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		if o.Local == nil {
			return errors.New("this server has no instance")
		}
		return o.Local.ApplyRules(ctx, p.Rules, p.TLSRequired)
	case ApplyRoleSync:
		var p roleSync
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		if o.Local == nil {
			return errors.New("this server has no instance")
		}
		return o.Local.ApplyRoles(ctx, p.Roles)
	case ApplyRoleChanged:
		var p roleDrop
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		if p.Name == "" || len(p.Name) > 63 {
			return errors.New("the role name is not valid")
		}
		o.endSessionsLocal(ctx, p.Name)
		return nil
	case ApplyRoleDrop:
		var p roleDrop
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		if o.Local == nil {
			return errors.New("this server has no instance")
		}
		return o.Local.DropRole(ctx, p.Name)
	case ApplyGrant:
		var p grantOp
		if err := strictJSON(raw, &p); err != nil {
			return err
		}
		return o.applyGrantLocal(ctx, p)
	}
	return fmt.Errorf("unknown operation kind %q", kind)
}

func strictJSON(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return errors.New("the parameters are not valid for this operation")
	}
	return nil
}

func (o *Ops) applyGrantLocal(ctx context.Context, p grantOp) error {
	// The actor is empty: the change is made with the service role, the
	// signed-in role already passed PostgreSQL's checks on the leader.
	switch {
	case p.Op == "grant" && p.Grant != nil:
		return o.Grant(ctx, "", *p.Grant)
	case p.Op == "revoke" && p.Grant != nil:
		return o.Revoke(ctx, "", *p.Grant)
	case p.Op == "template" && p.Template != nil:
		return o.ApplyTemplate(ctx, "", *p.Template)
	}
	return errors.New("the grant operation is not valid")
}

// others returns the other servers, or none when this server is alone.
func (o *Ops) others(ctx context.Context) []OtherServer {
	if o.Fanout == nil {
		return nil
	}
	s, err := o.Fanout.Others(ctx)
	if err != nil {
		o.Log.Warn("reading the other servers failed", "error", err)
		return nil
	}
	return s
}

func ids(s []OtherServer) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = x.ID
	}
	return out
}

// onServer runs f for a server, waiting while the server cannot be reached
// and saying which server the task waits for.
func (o *Ops) onServer(ctx context.Context, env *task.Env, server, doing string, f func() error) error {
	announced := false
	for {
		err := f()
		if err == nil {
			if announced {
				_ = env.SetDetail(ctx, "")
			}
			return nil
		}
		if !errors.Is(err, peerclient.ErrUnreachable) {
			return fmt.Errorf("on server %s: %w", server, err)
		}
		if !announced {
			o.Log.Warn("task waits for a server", "server", server, "doing", doing, "error", err)
			announced = true
		}
		_ = env.SetDetail(ctx, fmt.Sprintf("Waiting for server %s, which cannot be reached (%s).", server, doing))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// perServer builds one step for each server of ids.
func (o *Ops) perServer(servers []string, name string, f func(ctx context.Context, env *task.Env, server string) error) []task.Step {
	steps := make([]task.Step, 0, len(servers))
	for _, s := range servers {
		steps = append(steps, task.Step{Name: fmt.Sprintf(name, s), Run: func(ctx context.Context, env *task.Env) error {
			return o.onServer(ctx, env, s, strings.ToLower(fmt.Sprintf(name, s)), func() error { return f(ctx, env, s) })
		}})
	}
	return steps
}

func (o *Ops) pgPortOf(ctx context.Context, server string) int {
	for _, s := range o.others(ctx) {
		if s.ID == server {
			return s.PGPort
		}
	}
	return 0
}

// --------------------------------------------- roles and grants (synchronous)

// replicateRoles brings the other servers up to date with the named roles. The
// current state is read now and sent over the internal API (password verifiers
// travel, passwords never exist here); servers that cannot be reached get a
// task that waits for them.
func (o *Ops) replicateRoles(ctx context.Context, names ...string) {
	o.replicate(ctx, replicateInput{Kind: ApplyRoleSync, Names: names})
}

func (o *Ops) replicateRoleDrop(ctx context.Context, name string) {
	o.replicate(ctx, replicateInput{Kind: ApplyRoleDrop, Names: []string{name}})
}

func (o *Ops) replicateGrant(ctx context.Context, g grantOp) {
	o.replicate(ctx, replicateInput{Kind: ApplyGrant, Grant: &g})
}

type replicateInput struct {
	Kind    string   `json:"kind"`
	Names   []string `json:"names,omitempty"`
	Grant   *grantOp `json:"grant,omitempty"`
	Servers []string `json:"servers,omitempty"`
}

// params builds the parameters of the operation from the current state.
func (o *Ops) replicaParams(ctx context.Context, in replicateInput) (any, error) {
	switch in.Kind {
	case ApplyRoleSync:
		roles, err := o.Local.CollectRoles(ctx, in.Names...)
		if err != nil {
			return nil, err
		}
		return roleSync{Roles: roles}, nil
	case ApplyRoleDrop:
		return roleDrop{Name: in.Names[0]}, nil
	case ApplyGrant:
		return in.Grant, nil
	}
	return nil, fmt.Errorf("unknown replication kind %q", in.Kind)
}

func (o *Ops) replicate(ctx context.Context, in replicateInput) {
	servers := o.others(ctx)
	if len(servers) == 0 || o.Local == nil {
		return
	}
	ctx = context.WithoutCancel(ctx)
	params, err := o.replicaParams(ctx, in)
	if err != nil {
		o.Log.Error("reading the state to replicate failed", "kind", in.Kind, "error", err)
		return
	}
	var failed []string
	for _, s := range servers {
		c, cancel := context.WithTimeout(ctx, 20*time.Second)
		err := o.Fanout.Apply(c, s.ID, in.Kind, params)
		cancel()
		if err != nil {
			o.Log.Warn("a server could not be brought up to date at once; a task will", "server", s.ID, "kind", in.Kind, "error", err)
			failed = append(failed, s.ID)
		}
	}
	if len(failed) == 0 {
		return
	}
	in.Servers = failed
	subject := fmt.Sprintf("apply:%s:%s:%d", in.Kind, strings.Join(in.Names, ","), time.Now().UnixNano())
	if _, err := o.StartTask(ctx, KindReplicate, "replication", subject, in, "system"); err != nil {
		o.Log.Error("starting the task that brings servers up to date failed", "error", err)
	}
}

func (o *Ops) replicateDefinition() task.Definition {
	return task.Definition{Kind: KindReplicate, Dynamic: func(input json.RawMessage) ([]task.Step, error) {
		var in replicateInput
		if err := json.Unmarshal(input, &in); err != nil {
			return nil, err
		}
		return o.perServer(in.Servers, "Bring server %s up to date", func(ctx context.Context, env *task.Env, server string) error {
			cur, err := inputOf[replicateInput](env.Task)
			if err != nil {
				return err
			}
			params, err := o.replicaParams(ctx, cur)
			if err != nil {
				return err
			}
			return o.Fanout.Apply(ctx, server, cur.Kind, params)
		}), nil
	}}
}

// endSessionsLocal ends the sessions of a role on this server and removes its
// acting-as entry of the proxy.
func (o *Ops) endSessionsLocal(ctx context.Context, role string) {
	if o.SessionEnder != nil {
		o.SessionEnder(role)
	}
	if o.Roles != nil {
		o.Roles.Revoke(ctx, role)
	}
}

// roleChanged ends the sessions of a changed role here and, as the leader, on
// every other server (after the change reached them). A server that cannot be
// reached is not waited for here, and the "few seconds" of auth.Revalidator
// does NOT bound it: that server asks its own PostgreSQL, which still has the
// old role until the replicate.apply task of replicate reaches it. Until then
// the role keeps working there (CP2-07). The task never gives up, and the
// monitor raises the alert replicationPending for every server it waits for
// (monitor.ReadReplication), so that an operator sees the exposure and can
// stop the server or cut it off. Only after the task applied the change does
// the revalidator end the sessions within its few seconds.
func (o *Ops) roleChanged(ctx context.Context, role string) {
	o.endSessionsLocal(ctx, role)
	if o.Fanout == nil {
		return
	}
	for _, s := range o.others(ctx) {
		if err := o.Fanout.Apply(ctx, s.ID, ApplyRoleChanged, roleDrop{Name: role}); err != nil {
			o.Log.Warn("telling a server that a role changed failed; its sessions end when the replication task reaches it", "server", s.ID, "error", err)
		}
	}
}
