// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"sync"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// InstanceControl is what the network operations need from the instance
// manager.
type InstanceControl interface {
	WriteHBA(id int, rules []instance.HBARule, tlsRequired bool) error
	Reload(ctx context.Context, id int) error
	Restart(ctx context.Context, id int) error
}

type applyState struct {
	mu      sync.Mutex
	applied int64
	err     string
}

type networkParams struct {
	Revision    int64                   `json:"revision"` // the new revision
	TLSRequired bool                    `json:"tlsRequired"`
	Rules       []api.NetworkAccessRule `json:"rules"`
	Actor       string                  `json:"actor"`
	// Servers are the other servers when the task was made.
	Servers []string `json:"servers,omitempty"`
}

const maxRules = 1000

func toHBA(rules []api.NetworkAccessRule) []instance.HBARule { return cluster.HBARules(rules) }

// GetNetworkAccess returns the stored rules and the application state.
func (o *Ops) GetNetworkAccess(ctx context.Context) (api.NetworkAccess, error) {
	out := api.NetworkAccess{Rules: []api.NetworkAccessRule{}, ApplyStatus: []api.NetworkApplyStatus{}}
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, "SELECT revision, tls_required FROM kingres_cp.network_revision").Scan(&out.Revision, &out.TLSRequired); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, "SELECT rule FROM kingres_cp.network_rule WHERE revision = $1 ORDER BY position", out.Revision)
		if err != nil {
			return err
		}
		raws, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
		if err != nil {
			return err
		}
		for _, r := range raws {
			var rule api.NetworkAccessRule
			if err := json.Unmarshal(r, &rule); err != nil {
				return err
			}
			out.Rules = append(out.Rules, rule)
		}
		return nil
	})
	if err != nil {
		return out, wrap(err)
	}
	o.applyState.mu.Lock()
	st := api.NetworkApplyStatus{ServerID: o.ServerHost, AppliedRevision: o.applyState.applied}
	if o.applyState.err != "" {
		e := o.applyState.err
		st.Error = &e
	}
	o.applyState.mu.Unlock()
	out.ApplyStatus = append(out.ApplyStatus, st)
	return out, nil
}

// wouldLockOut reports whether, under the rules, a password login of user
// from clientIP would be rejected (the first matching rule rejects, or no
// rule matches). The service role's fixed rules are not considered: they only
// cover the local machine.
func wouldLockOut(rules []api.NetworkAccessRule, user, clientIP string) bool {
	ip, err := netip.ParseAddr(clientIP)
	if err != nil {
		return false // an address we cannot judge is not refused
	}
	for _, r := range rules {
		if !contains(r.Users, user) {
			continue
		}
		if r.Address != "all" {
			p, err := netip.ParsePrefix(r.Address)
			if err != nil || !p.Contains(ip.Unmap()) {
				continue
			}
		}
		return r.Method == "reject"
	}
	return true
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == "all" || x == s {
			return true
		}
	}
	return false
}

// PrepareReplaceNetworkAccess validates the new rule set and returns the task
// input. clientIP and actor identify the caller, who must not be locked out.
func (o *Ops) PrepareReplaceNetworkAccess(ctx context.Context, actor, clientIP string, req api.NetworkAccessUpdateRequest) (any, error) {
	if len(req.Rules) > maxRules {
		return nil, problem.Validation("The rule set is invalid.", field("/rules", "at most "+strconv.Itoa(maxRules)+" rules", "out_of_range"))
	}
	if _, err := instance.RenderHBA(toHBA(req.Rules), req.TLSRequired); err != nil {
		return nil, problem.Validation("The rule set is invalid.", field("/rules", err.Error(), "invalid"))
	}
	if wouldLockOut(req.Rules, actor, clientIP) {
		return nil, problem.New(problem.Unprocessable, "This rule set would lock out your own address; the change was not applied.")
	}
	cur, err := o.GetNetworkAccess(ctx)
	if err != nil {
		return nil, err
	}
	if cur.Revision != req.Revision {
		return nil, problem.New(problem.OptimisticLockFail, fmt.Sprintf("The rules changed (revision %d); reload and try again.", cur.Revision))
	}
	return networkParams{Revision: req.Revision + 1, TLSRequired: req.TLSRequired, Rules: req.Rules, Actor: actor, Servers: ids(o.others(ctx))}, nil
}

func (o *Ops) stepStoreRules(ctx context.Context, env *task.Env) error {
	p, err := inputOf[networkParams](env.Task)
	if err != nil {
		return err
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var cur int64
		if err := tx.QueryRow(ctx, "SELECT revision FROM kingres_cp.network_revision FOR UPDATE").Scan(&cur); err != nil {
			return err
		}
		if cur >= p.Revision {
			return nil // stored by an earlier run of this step
		}
		if cur != p.Revision-1 {
			return fmt.Errorf("the rules changed meanwhile (revision %d)", cur)
		}
		for i, r := range p.Rules {
			j, _ := json.Marshal(r)
			if _, err := tx.Exec(ctx, "INSERT INTO kingres_cp.network_rule (revision, position, rule) VALUES ($1, $2, $3)", p.Revision, i, j); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, "UPDATE kingres_cp.network_revision SET revision = $1, tls_required = $2, updated_at = now(), updated_by = $3", p.Revision, p.TLSRequired, p.Actor)
		return err
	})
}

func (o *Ops) stepApplyRules(ctx context.Context, env *task.Env) error {
	p, err := inputOf[networkParams](env.Task)
	if err != nil {
		return err
	}
	err = o.Instances.WriteHBA(cluster.NodeID, toHBA(p.Rules), p.TLSRequired)
	if err == nil {
		err = o.Instances.Reload(ctx, cluster.NodeID)
	}
	o.applyState.mu.Lock()
	defer o.applyState.mu.Unlock()
	if err != nil {
		o.applyState.err = "the rules could not be applied"
		return fmt.Errorf("applying the rules: %w", err)
	}
	o.applyState.applied, o.applyState.err = p.Revision, ""
	return nil
}

// ApplyStoredRules rewrites pg_hba.conf from the stored revision (start-up).
func (o *Ops) ApplyStoredRules(ctx context.Context) error {
	cur, err := o.GetNetworkAccess(ctx)
	if err != nil {
		return err
	}
	if err := o.Instances.WriteHBA(cluster.NodeID, toHBA(cur.Rules), cur.TLSRequired); err != nil {
		return err
	}
	o.applyState.mu.Lock()
	o.applyState.applied = cur.Revision
	o.applyState.mu.Unlock()
	return nil
}

func (o *Ops) stepApplyOn(ctx context.Context, env *task.Env, server string) error {
	p, err := inputOf[networkParams](env.Task)
	if err != nil {
		return err
	}
	return o.Fanout.Apply(ctx, server, ApplyNetwork, networkApply{Rules: p.Rules, TLSRequired: p.TLSRequired})
}
