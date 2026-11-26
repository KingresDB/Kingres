// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconfig

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// MaxChanges is the most changes one request may carry.
const MaxChanges = 100

// change is a checked change: the canonical name, the level and scope and the normalized value
// (nil removes the value at the level).
type change struct {
	Name  string  `json:"name"`
	Level string  `json:"level"`
	Scope string  `json:"scope"`
	Value *string `json:"value"`
}

type checked struct {
	changes  []change
	problems []api.ConfigProblem
	warnings []api.ConfigWarning
}

func prob(i int, field, code, msg string) api.ConfigProblem {
	return api.ConfigProblem{Index: i, Pointer: "/changes/" + strconv.Itoa(i) + field, Code: code, Message: msg}
}

// check runs layers 1 to 3 of the validation (design config-phase13.md, "Validation") for a request.
// Nothing is written.
func (l *Leader) check(ctx context.Context, sn *snapshot, req api.ConfigChangeRequest) (checked, error) {
	var out checked
	if len(req.Changes) == 0 || len(req.Changes) > MaxChanges {
		return out, problem.Validation("A request carries 1 to 100 changes.", problem.FieldError{Field: "/changes", Message: "must hold 1 to 100 changes", Code: "out_of_range"})
	}
	groups, err := l.st.groups(ctx)
	if err != nil {
		return out, err
	}
	dbs, err := l.Agent.Databases(ctx)
	if err != nil {
		return out, fmt.Errorf("pgconfig: reading the databases: %w", err)
	}
	dbSet := map[string]bool{}
	for _, d := range dbs {
		dbSet[d] = true
	}
	servers := map[string]bool{}
	for _, s := range sn.servers {
		servers[s.ID] = true
	}
	replicas := l.st.setting(ctx, "replica.default", 1)
	seen := map[string]bool{}
	for i, c := range req.Changes {
		bad := func(field, code, msg string) { out.problems = append(out.problems, prob(i, field, code, msg)) }
		name := strings.ToLower(strings.TrimSpace(c.Name))
		// A stored value can always be removed, whatever the parameter has become since it was set: a name that
		// an upgrade made managed or refused, a server group or database that is gone (P13R-05).
		if c.Value == nil && (c.Level == LevelCluster || c.ScopeID != nil) {
			sc := ""
			if c.Level != LevelCluster {
				sc = *c.ScopeID
			}
			if hasStored(sn.values, name, c.Level, sc) {
				key := name + "|" + c.Level + "|" + sc
				if seen[key] {
					bad("", "config_value_invalid", "The request changes "+trunc(name)+" twice at the same level.")
					continue
				}
				seen[key] = true
				out.changes = append(out.changes, change{Name: name, Level: c.Level, Scope: sc})
				continue
			}
		}
		meta, ok := sn.metaBy[name]
		if !ok {
			bad("/name", "config_parameter_unknown", "PostgreSQL on this server has no parameter "+trunc(c.Name)+".")
			continue
		}
		cl := Classify(meta, sn.hidden[name])
		scope := ""
		if c.ScopeID != nil {
			scope = *c.ScopeID
		}
		switch c.Level {
		case LevelCluster, LevelServerGroup, LevelServer, LevelDatabase:
		default:
			bad("/level", "config_level_not_allowed", "The level must be cluster, serverGroup, server or database.")
			continue
		}
		if code, msg := CheckLevel(meta, cl, c.Level); code != "" {
			bad("/level", code, msg)
			continue
		}
		switch c.Level {
		case LevelCluster:
			scope = ""
		case LevelServerGroup:
			if !groups[scope] {
				bad("/scopeId", "not_found", "There is no server group "+trunc(scope)+".")
				continue
			}
		case LevelServer:
			if !servers[scope] {
				bad("/scopeId", "not_found", "There is no server "+trunc(scope)+".")
				continue
			}
		case LevelDatabase:
			if !dbSet[scope] {
				bad("/scopeId", "not_found", "There is no database "+trunc(scope)+".")
				continue
			}
		}
		key := name + "|" + c.Level + "|" + scope
		if seen[key] {
			bad("", "config_value_invalid", "The request changes "+meta.Name+" twice at the same level.")
			continue
		}
		seen[key] = true
		ch := change{Name: name, Level: c.Level, Scope: scope}
		if c.Value != nil {
			r, err := Check(meta, *c.Value)
			if err != nil {
				bad("/value", "config_value_invalid", strings.TrimPrefix(err.Error(), "invalid value: "))
				continue
			}
			if f, why, ok := Floor(name, replicas); ok && r.BaseOK {
				if min, _ := new(big.Rat).SetString(strconv.FormatInt(f, 10)); r.Base.Cmp(min) < 0 {
					bad("/value", "config_value_below_floor", meta.Name+" must be at least "+strconv.FormatInt(f, 10)+": "+why+".")
					continue
				}
			}
			// PostgreSQL's own check (layer 3), on this server.
			var chk CheckResponse
			if err := l.callServer(ctx, l.Self, CallCheck, CheckRequest{Name: meta.Name, Value: r.Text}, &chk); err != nil {
				return out, fmt.Errorf("pgconfig: asking PostgreSQL to check the value: %w", err)
			}
			if chk.Error != "" {
				bad("/value", "config_value_invalid", chk.Error)
				continue
			}
			if w, ok := unusualWarning(i, meta, r); ok {
				out.warnings = append(out.warnings, w)
			}
			v := r.Text
			ch.Value = &v
		}
		out.changes = append(out.changes, ch)
	}
	return out, nil
}

func hasStored(rows []valueRow, name, level, scope string) bool {
	for _, v := range rows {
		if v.Name == name && v.Level == level && v.Scope == scope {
			return true
		}
	}
	return false
}

// asProblem turns the first problem of a refused request into the API's answer.
func asProblem(ps []api.ConfigProblem) error {
	if len(ps) == 0 {
		return nil
	}
	var errs []problem.FieldError
	for _, p := range ps {
		if p.Code == "config_value_invalid" {
			errs = append(errs, problem.FieldError{Field: p.Pointer, Message: p.Message, Code: "invalid_value"})
		}
	}
	first := ps[0]
	switch first.Code {
	case "config_parameter_unknown":
		return problem.New(problem.ConfigParameterUnknown, first.Message)
	case "config_parameter_managed":
		return problem.New(problem.ConfigParameterManaged, first.Message)
	case "config_parameter_refused":
		return problem.New(problem.ConfigParameterRefused, first.Message)
	case "config_level_not_allowed":
		return problem.New(problem.ConfigLevelNotAllowed, first.Message)
	case "config_value_below_floor":
		return problem.New(problem.ConfigValueBelowFloor, first.Message)
	case "not_found":
		return problem.New(problem.NotFound, first.Message)
	}
	if len(errs) == 0 {
		for _, p := range ps {
			errs = append(errs, problem.FieldError{Field: p.Pointer, Message: p.Message, Code: "invalid_value"})
		}
	}
	return problem.Validation("A value is not valid for its parameter.", errs...)
}

// Preview answers previewConfigChange.
func (l *Leader) Preview(ctx context.Context, req api.ConfigChangeRequest) (api.ConfigChangePreview, error) {
	sn, err := l.snapshot(ctx)
	if err != nil {
		return api.ConfigChangePreview{}, err
	}
	ck, err := l.check(ctx, sn, req)
	if err != nil {
		return api.ConfigChangePreview{}, err
	}
	out := api.ConfigChangePreview{Valid: len(ck.problems) == 0, Problems: ck.problems, Warnings: []api.ConfigWarning{}, Effects: []api.ConfigEffect{}, RestartServerIDs: []string{}, RestartOrder: []api.ConfigRestartStep{}}
	if out.Problems == nil {
		out.Problems = []api.ConfigProblem{}
	}
	if !out.Valid {
		return out, nil
	}
	after := applyToValues(sn.values, ck.changes)
	out.Warnings = append(out.Warnings, ck.warnings...)
	identity := make([]int, len(ck.changes))
	for i := range identity {
		identity[i] = i
	}
	out.Warnings = append(out.Warnings, reservedWarnings(sn, after, ck.changes, identity)...)
	restart := map[string]bool{}
	for _, name := range changedNames(ck.changes) {
		meta, known := sn.metaBy[name]
		if !known {
			continue // the removal of a stored value of a parameter PostgreSQL no longer has
		}
		for _, s := range sn.servers {
			e := effect(sn, meta, s, sn.values, after, ck.changes)
			if e.TakesEffect == "unchanged" && !touchedAt(ck.changes, name, s) {
				continue
			}
			out.Effects = append(out.Effects, e)
			if e.TakesEffect == "restart" {
				restart[s.ID] = true
			}
		}
	}
	out.RestartServerIDs = sortedKeys(restart)
	if len(restart) > 0 {
		order, conflict := restartOrder(sn, restart, wantedValues(sn, after, ck.changes))
		for _, st := range order {
			out.RestartOrder = append(out.RestartOrder, api.ConfigRestartStep{ServerID: st.Server, InstanceID: int32(st.Instance), Role: st.Role})
		}
		if conflict != "" {
			out.RestartOrderConflict = strp(conflict)
		}
	}
	return out, nil
}

func touchedAt(cs []change, name string, s srv) bool {
	for _, c := range cs {
		if c.Name != name {
			continue
		}
		switch c.Level {
		case LevelCluster, LevelDatabase:
			return true
		case LevelServerGroup:
			if c.Scope == s.Group {
				return true
			}
		case LevelServer:
			if c.Scope == s.ID {
				return true
			}
		}
	}
	return false
}

func changedNames(cs []change) []string {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.Name] = true
	}
	return sortedKeys(m)
}

// applyToValues returns the stored values after the changes.
func applyToValues(old []valueRow, cs []change) []valueRow {
	var out []valueRow
	for _, v := range old {
		replaced := false
		for _, c := range cs {
			if c.Name == v.Name && c.Level == v.Level && c.Scope == v.Scope {
				replaced = true
			}
		}
		if !replaced {
			out = append(out, v)
		}
	}
	for _, c := range cs {
		if c.Value != nil {
			out = append(out, valueRow{Level: c.Level, Scope: c.Scope, Name: c.Name, Value: *c.Value})
		}
	}
	return out
}

// effect says what the change does to a parameter on a server.
func effect(sn *snapshot, meta Setting, s srv, before, after []valueRow, cs []change) api.ConfigEffect {
	name := strings.ToLower(meta.Name)
	e := api.ConfigEffect{ServerID: s.ID, Name: meta.Name, TakesEffect: "unchanged"}
	cur, ok := sn.setting(s.ID, meta.Name)
	if !ok {
		e.TakesEffect = "waits"
		return e
	}
	e.Before = strp(Format(meta, cur.Setting))
	var ovs []valueRow
	for _, v := range after {
		if v.Name == name {
			ovs = append(ovs, v)
		}
	}
	// A database-level change is for sessions of that database: compare the stored values.
	for _, c := range cs {
		if c.Name == name && c.Level == LevelDatabase {
			var was *string
			for _, v := range before {
				if v.Name == name && v.Level == LevelDatabase && v.Scope == c.Scope {
					x := v.Value
					was = &x
				}
			}
			e.Before, e.After = was, c.Value
			if (was == nil) != (c.Value == nil) || was != nil && *was != *c.Value {
				e.TakesEffect = "reload"
			}
			return e
		}
	}
	if o, has := Pick(toOverrides(ovs), s.ID, s.Group); has {
		e.After = strp(o.Value)
		if !sameValue(meta, o.Value, cur.Setting) {
			e.TakesEffect = effectKind(meta)
		}
		return e
	}
	// No override at any level now: the server falls back to what its configuration files give.
	if _, had := Pick(toOverrides(before2(before, name)), s.ID, s.Group); had {
		e.TakesEffect = effectKind(meta)
	}
	return e
}

func before2(v []valueRow, name string) []valueRow {
	var out []valueRow
	for _, r := range v {
		if r.Name == name {
			out = append(out, r)
		}
	}
	return out
}

func effectKind(meta Setting) string {
	if meta.Context == "postmaster" {
		return "restart"
	}
	return "reload"
}

// RestartStep is one instance of a restart order.
type RestartStep struct {
	Server   string
	Instance int
	Role     string
	Group    int32
}

// wantedValues returns, for each of the five standby settings and each server, the value (in the
// base unit) a restart will bring.
func wantedValues(sn *snapshot, after []valueRow, cs []change) map[string]map[string]*big.Rat {
	out := map[string]map[string]*big.Rat{}
	for _, name := range fiveSettings {
		meta, ok := sn.metaBy[name]
		if !ok {
			continue
		}
		out[name] = map[string]*big.Rat{}
		var ovs []valueRow
		for _, v := range after {
			if v.Name == name {
				ovs = append(ovs, v)
			}
		}
		for _, s := range sn.servers {
			if o, has := Pick(toOverrides(ovs), s.ID, s.Group); has {
				if r, err := Check(meta, o.Value); err == nil && r.BaseOK {
					out[name][s.ID] = r.Base
				}
			}
		}
	}
	return out
}

// restartOrder orders the restart of the instances of the given servers so that no standby is left
// below its primary (design config-phase13.md, "Restart"): when a value goes up the standby is
// restarted before its primary, when it goes down after it. A pair with one value going up and
// another going down has no safe order.
func restartOrder(sn *snapshot, servers map[string]bool, want map[string]map[string]*big.Rat) ([]RestartStep, string) {
	type node struct {
		server string
		inst   int
	}
	var nodes []RestartStep
	idx := map[node]int{}
	add := func(server string, inst int, role string, g int32) {
		n := node{server, inst}
		if _, ok := idx[n]; !ok {
			idx[n] = len(nodes)
			nodes = append(nodes, RestartStep{Server: server, Instance: inst, Role: role, Group: g})
		}
	}
	for _, id := range sortedKeys(servers) {
		add(id, 1, roleAt(sn, id, 1, "primary"), 0)
		for _, p := range sn.pairs {
			if p.Server == id {
				add(id, p.InstanceID, roleAt(sn, id, p.InstanceID, "standby"), p.Group)
			}
		}
	}
	// edges: before -> after
	edges := map[int][]int{}
	conflict := ""
	for _, p := range sn.pairs {
		si, sok := idx[node{p.Server, p.InstanceID}]
		psrv, pinst := primaryNode(sn, p)
		if psrv == p.Server && pinst == p.InstanceID {
			continue // the standby was promoted: it has no primary above it
		}
		pi, pok := idx[node{psrv, pinst}]
		if !sok && !pok {
			continue
		}
		up, down := false, false
		var names []string
		for _, name := range fiveSettings {
			ws, wp := want[name][p.Server], want[name][psrv]
			cs, cp := current(sn, p.Server, p.InstanceID, name), current(sn, psrv, pinst, name)
			if wp == nil {
				wp = cp
			}
			if ws == nil {
				ws = cs
			}
			if wp == nil || cp == nil || ws == nil {
				continue
			}
			switch c := wp.Cmp(cp); {
			case c > 0:
				up = true
				names = append(names, name)
			case c < 0:
				down = true
				names = append(names, name)
			}
		}
		switch {
		case up && down:
			conflict = fmt.Sprintf("For the standby of shard group %d on %s and its primary on %s, %s change in different directions; change them one direction at a time.",
				p.Group, p.Server, psrv, strings.Join(names, ", "))
		case up && sok && pok:
			edges[si] = append(edges[si], pi)
		case down && sok && pok:
			edges[pi] = append(edges[pi], si)
		}
	}
	// Kahn's algorithm, ties broken by server and instance so the order is the same every time.
	indeg := make([]int, len(nodes))
	for _, to := range edges {
		for _, t := range to {
			indeg[t]++
		}
	}
	var out []RestartStep
	done := make([]bool, len(nodes))
	for len(out) < len(nodes) {
		pick := -1
		for i := range nodes {
			if !done[i] && indeg[i] == 0 {
				if pick < 0 || less(nodes[i], nodes[pick]) {
					pick = i
				}
			}
		}
		if pick < 0 {
			if conflict == "" {
				conflict = "The restart order of the standbys and their primaries is circular; restart them by hand, one server at a time."
			}
			for i := range nodes {
				if !done[i] {
					out = append(out, nodes[i])
				}
			}
			break
		}
		done[pick] = true
		out = append(out, nodes[pick])
		for _, t := range edges[pick] {
			indeg[t]--
		}
	}
	return out, conflict
}

func less(a, b RestartStep) bool {
	if a.Server != b.Server {
		return a.Server < b.Server
	}
	return a.Instance < b.Instance
}

// current is the running value of a standby setting of an instance, in the base unit.
func current(sn *snapshot, server string, inst int, name string) *big.Rat {
	st := sn.states[server]
	if st == nil {
		return nil
	}
	for _, in := range st.Instances {
		if in.ID == inst {
			if v, ok := in.Values[name]; ok {
				r, _ := new(big.Rat).SetString(v)
				return r
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------- history and revert

// History answers listConfigChanges.
func (l *Leader) History(ctx context.Context, name, level, scope string, limit int, cursor string) (api.ConfigChangeList, error) {
	var after int64
	if cursor != "" {
		b, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return api.ConfigChangeList{}, problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor of this server", Code: "invalid_cursor"})
		}
		after, err = strconv.ParseInt(string(b), 10, 64)
		if err != nil {
			return api.ConfigChangeList{}, problem.Validation("The cursor is not valid.", problem.FieldError{Field: "cursor", Message: "not a cursor of this server", Code: "invalid_cursor"})
		}
	}
	if limit <= 0 {
		limit = 50
	}
	rows, err := l.st.changes(ctx, strings.ToLower(name), level, scope, after, limit+1)
	if err != nil {
		return api.ConfigChangeList{}, err
	}
	out := api.ConfigChangeList{Items: []api.ConfigChange{}}
	if len(rows) > limit {
		rows = rows[:limit]
		c := base64.RawURLEncoding.EncodeToString([]byte(strconv.FormatInt(rows[len(rows)-1].ID, 10)))
		out.NextCursor = &c
	}
	for _, r := range rows {
		latest, err := l.st.latestChange(ctx, r.Level, r.Scope, r.Name)
		if err != nil {
			return out, err
		}
		out.Items = append(out.Items, changeOf(r, latest == r.ID))
	}
	return out, nil
}

func changeOf(r changeRow, revertible bool) api.ConfigChange {
	c := api.ConfigChange{ChangeID: r.ID, Revision: r.Revision, Name: r.Name, Level: r.Level, OldValue: r.Old, NewValue: r.New, ChangedBy: r.By,
		ChangedAt: r.At.UTC(), TaskID: r.TaskID, Reverts: r.Reverts, Revertible: revertible, TaskState: r.TaskState}
	if r.Level != LevelCluster {
		c.ScopeID = strp(r.Scope)
	}
	return c
}

// ErrNoSuchChange is returned by Revert for an unknown change.
var ErrNoSuchChange = errors.New("no such change")

// sortChanges orders changes by name, level and scope (the order of the task input).
func sortChanges(cs []change) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].Name != cs[j].Name {
			return cs[i].Name < cs[j].Name
		}
		if cs[i].Level != cs[j].Level {
			return cs[i].Level < cs[j].Level
		}
		return cs[i].Scope < cs[j].Scope
	})
}

// primaryNode is the server and the instance that run the primary above a standby now. The pair records the
// server that ran the group's primary when the standby was created; after a failover the primary is the promoted
// standby on another server, and its instance is the one the standby record of that server names (instance 1
// when the server never held a standby of the group) (P13R-06).
func primaryNode(sn *snapshot, p standbyPair) (string, int) {
	srv := p.PrimaryServer
	if cur := sn.primaries[p.Group]; cur != "" {
		srv = cur
	}
	for _, q := range sn.pairs {
		if q.Group == p.Group && q.Server == srv {
			return srv, q.InstanceID
		}
	}
	return srv, 1
}

// roleAt is the role an instance has now, as its server reports it; def when the server did not answer.
func roleAt(sn *snapshot, server string, inst int, def string) string {
	if st := sn.states[server]; st != nil {
		for _, in := range st.Instances {
			if in.ID == inst && in.Role != "" {
				return in.Role
			}
		}
	}
	return def
}
