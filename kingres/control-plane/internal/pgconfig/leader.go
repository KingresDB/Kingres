// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconfig

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Task kinds and their subject (contract 0.8.0).
const (
	KindApply      = "config.apply"
	KindRestart    = "config.restart"
	SubjectType    = "config"
	SubjectApply   = "cluster"
	SubjectRestart = "restart"
)

// Leader is the manager leader's side of phase 13: it reads the state of every server, validates and
// previews changes, runs the tasks that apply them and restart instances, keeps the history and
// measures the clocks. Every method may be called on any server (reads are forwarded to the leader
// by the HTTP layer); the tasks run where the engine runs.
type Leader struct {
	Self string
	Sys  SysFunc
	// Agent answers the calls to this server itself.
	Agent *Agent
	// Call is the internal call to another server (method, path without /internal/v1).
	Call func(ctx context.Context, server, method, path string, in, out any) error
	// StartTask submits a task and runs it in the background.
	StartTask func(ctx context.Context, kind, subjType, subjID string, input any, actor string) (task.Task, error)
	// IsLeader reports whether this server is the manager leader now.
	IsLeader func(ctx context.Context) bool
	Log      *slog.Logger
	// GatherTimeout is the time each server gets to answer (default 3 s).
	GatherTimeout time.Duration
	// ClockEvery is how often the clocks of the other servers are measured for the alert (default 10 s).
	ClockEvery time.Duration

	st store
	// startMu makes the check "no configuration task is unfinished" and the start of a task one step, for the apply and the restart (they have different subjects, so the engine would let both run) (P13S-07).
	startMu sync.Mutex
	tm      timeCache
}

// NewLeader prepares the leader's side.
func NewLeader(l *Leader) *Leader {
	l.st = store{sys: l.Sys}
	if l.Log == nil {
		l.Log = slog.New(slog.DiscardHandler)
	}
	if l.GatherTimeout == 0 {
		l.GatherTimeout = 3 * time.Second
	}
	return l
}

// Definitions returns the task definitions of this package.
func (l *Leader) Definitions() []task.Definition {
	return []task.Definition{
		{Kind: KindApply, Dynamic: l.applySteps},
		{Kind: KindRestart, Dynamic: l.restartSteps},
	}
}

// callServer runs an internal call on a server; the call to this server itself does not leave the process.
func (l *Leader) callServer(ctx context.Context, server, key string, in, out any) error {
	if server == l.Self {
		body, err := json.Marshal(in)
		if err != nil {
			return err
		}
		res, err := l.Agent.HandleCall(ctx, key, body)
		if err != nil {
			return err
		}
		if out != nil {
			b, err := json.Marshal(res)
			if err != nil {
				return err
			}
			return json.Unmarshal(b, out)
		}
		return nil
	}
	method, path, _ := strings.Cut(key, " ")
	return l.Call(ctx, server, method, path, in, out)
}

// snapshot is a consistent look at the cluster's configuration: the stored values and the state each
// server reports.
type snapshot struct {
	rev     int64
	servers []srv
	states  map[string]*StateResponse
	missing []api.MissingServer
	meta    []Setting
	metaBy  map[string]Setting
	hidden  map[string]bool
	values  []valueRow
	stored  map[string]serverState
	pairs   []standbyPair
	// primaries is the server that runs the primary of each shard group now.
	primaries map[int32]string
}

func (l *Leader) snapshot(ctx context.Context) (*snapshot, error) {
	sn := &snapshot{states: map[string]*StateResponse{}, metaBy: map[string]Setting{}, hidden: map[string]bool{}}
	var err error
	if sn.rev, err = l.st.revision(ctx); err != nil {
		return nil, fmt.Errorf("pgconfig: reading the revision: %w", err)
	}
	if sn.servers, err = l.st.servers(ctx); err != nil {
		return nil, fmt.Errorf("pgconfig: reading the servers: %w", err)
	}
	if sn.values, err = l.st.values(ctx); err != nil {
		return nil, err
	}
	if sn.stored, err = l.st.states(ctx); err != nil {
		return nil, fmt.Errorf("pgconfig: reading the server states: %w", err)
	}
	if sn.pairs, err = l.st.standbys(ctx); err != nil {
		sn.pairs = nil // a cluster without the standby table yet has no standbys
	}
	if sn.primaries, err = l.st.groupPrimaries(ctx); err != nil {
		sn.primaries = nil // without it the primary of a pair is the server the standby was created for
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, s := range sn.servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, l.GatherTimeout)
			defer cancel()
			var st StateResponse
			err := l.callServer(c, s.ID, CallState, StateRequest{Meta: s.ID == l.Self}, &st)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				reason := "refused"
				switch {
				case errors.Is(err, peerclient.ErrUnreachable):
					reason = "unreachable"
				case errors.Is(err, context.DeadlineExceeded):
					reason = "timeout"
				}
				if s.ID != l.Self {
					sn.missing = append(sn.missing, api.MissingServer{ServerID: s.ID, Reason: reason})
				}
				l.Log.Debug("a server's configuration state could not be read", "server", s.ID, "error", err)
				return
			}
			sn.states[s.ID] = &st
		}()
	}
	wg.Wait()
	sort.Slice(sn.missing, func(i, j int) bool { return sn.missing[i].ServerID < sn.missing[j].ServerID })
	self := sn.states[l.Self]
	if self == nil || !self.Reachable {
		return nil, errors.New("pgconfig: this server's PostgreSQL instance cannot be read")
	}
	sn.meta = self.Settings
	sort.Slice(sn.meta, func(i, j int) bool { return strings.ToLower(sn.meta[i].Name) < strings.ToLower(sn.meta[j].Name) })
	for _, s := range sn.meta {
		sn.metaBy[strings.ToLower(s.Name)] = s
	}
	for _, h := range self.Hidden {
		sn.hidden[strings.ToLower(h)] = true
	}
	return sn, nil
}

func (sn *snapshot) groupOf(server string) string {
	for _, s := range sn.servers {
		if s.ID == server {
			return s.Group
		}
	}
	return ""
}

func (sn *snapshot) overridesOf(name string) []valueRow {
	var out []valueRow
	for _, v := range sn.values {
		if v.Name == name {
			out = append(out, v)
		}
	}
	return out
}

func toOverrides(rows []valueRow) []Override {
	out := make([]Override, 0, len(rows))
	for _, r := range rows {
		if r.Level == LevelDatabase {
			continue
		}
		out = append(out, Override{Level: r.Level, Scope: r.Scope, Value: r.Value})
	}
	return out
}

func strp(s string) *string { return &s }

func (sn *snapshot) setting(server, name string) (Setting, bool) {
	st := sn.states[server]
	if st == nil {
		return Setting{}, false
	}
	for _, s := range st.Settings {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return Setting{}, false
}

// sourceOf says where a running value comes from.
func sourceOf(s Setting, ov *Override) string {
	switch s.Source {
	case "default":
		return "default"
	case "configuration file":
		switch filepath.Base(s.SourceFile) {
		case instance.ConfigFileName:
			if ov != nil {
				return ov.Level
			}
			return "other"
		case "kingres-managed.conf":
			if _, managed := instance.IsManaged(strings.ToLower(s.Name)); managed {
				return "managed"
			}
			return "derived"
		case "postgresql.conf":
			return "initdb"
		}
	}
	return "other"
}

// serverValue builds the value of one parameter on one server.
func (l *Leader) serverValue(sn *snapshot, meta Setting, cl Class, server string, overs []valueRow) api.ConfigServerValue {
	out := api.ConfigServerValue{ServerID: server, Source: "default", State: StateApplied}
	st := sn.states[server]
	if st == nil {
		out.State = "unknown"
		return out
	}
	s, ok := sn.setting(server, meta.Name)
	if !ok {
		out.State = "unknown"
		return out
	}
	ov, has := Pick(toOverrides(overs), server, sn.groupOf(server))
	var ovp *Override
	if has {
		ovp = &ov
		out.Level = strp(ov.Level)
	}
	out.Source = sourceOf(s, ovp)
	if !cl.Hidden {
		out.Value = strp(Format(meta, s.Setting))
	}
	switch {
	case s.Error != "":
		out.State, out.Error = StateFailed, strp(s.Error)
	case s.PendingRestart:
		out.State = StatePendingRestart
		if !cl.Hidden && s.PendingValue != "" {
			out.PendingValue = strp(s.PendingValue)
		}
	}
	if out.State == StateApplied {
		if sv, ok := sn.stored[server]; ok {
			if e, bad := sv.Errors[strings.ToLower(meta.Name)]; bad && sv.State == StateFailed {
				out.State, out.Error = StateFailed, strp(e)
			} else if e, bad := sv.Errors["(server)"]; bad && sv.State == StateFailed && has {
				// The server as a whole failed to apply (it could not write its file): every parameter that
				// is meant to differ from its default there is failed.
				out.State, out.Error = StateFailed, strp(e)
			}
		}
	}
	if out.State == StateApplied && has && ovp.Level != LevelDatabase {
		var rev int64
		for _, r := range overs {
			if r.Level == ov.Level && r.Scope == ov.Scope {
				rev = r.Revision
			}
		}
		if st.Revision < rev {
			out.State = StateWaiting
		}
	}
	return out
}

// parameter builds the API object of one parameter.
func (l *Leader) parameter(sn *snapshot, meta Setting) api.ConfigParameter {
	name := strings.ToLower(meta.Name)
	cl := Classify(meta, sn.hidden[name])
	p := api.ConfigParameter{
		Name: meta.Name, Category: meta.Category, Description: meta.ShortDesc, Type: meta.VarType, Context: meta.Context,
		RestartRequired: meta.Context == "postmaster", Kind: cl.Kind, Hidden: cl.Hidden,
		AllowedLevels: append([]string{}, cl.Levels...), Overrides: []api.ConfigOverride{}, Servers: []api.ConfigServerValue{},
	}
	if p.AllowedLevels == nil {
		p.AllowedLevels = []string{}
	}
	if meta.Unit != "" {
		p.Unit = strp(meta.Unit)
	}
	if meta.MinVal != "" {
		p.Min = strp(meta.MinVal)
	}
	if meta.MaxVal != "" {
		p.Max = strp(meta.MaxVal)
	}
	if meta.VarType == "enum" {
		p.EnumValues = append([]string{}, meta.EnumVals...)
	}
	if !cl.Hidden {
		p.DefaultValue = strp(Format(meta, meta.BootVal))
	}
	if cl.Kind != KindSettable {
		p.Reason = strp(cl.Reason.Text)
	}
	if cl.ClusterOnly != "" {
		p.ClusterOnly = strp(cl.ClusterOnly)
	}
	overs := sn.overridesOf(name)
	for _, v := range overs {
		o := api.ConfigOverride{Level: v.Level, Value: v.Value, Revision: v.Revision, UpdatedBy: v.By, UpdatedAt: v.At.UTC()}
		if v.Level != LevelCluster {
			o.ScopeID = strp(v.Scope)
		}
		p.Overrides = append(p.Overrides, o)
	}
	raw := map[string]string{}
	for _, s := range sn.servers {
		v := l.serverValue(sn, meta, cl, s.ID, overs)
		p.Servers = append(p.Servers, v)
		if v.State != "unknown" {
			if rs, ok := sn.setting(s.ID, meta.Name); ok {
				raw[s.ID] = rs.Setting
			}
		}
	}
	seen := map[string]bool{}
	for _, r := range raw {
		seen[r] = true
	}
	p.Differs = len(seen) > 1
	// Servers of one group that differ although neither has a server-level override.
	byGroup := map[string]map[string]bool{}
	for _, s := range sn.servers {
		r, ok := raw[s.ID]
		if !ok || s.Group == "" {
			continue
		}
		if _, has := Pick(toOverrides(overs), s.ID, s.Group); has {
			if o, _ := Pick(toOverrides(overs), s.ID, s.Group); o.Level == LevelServer {
				continue
			}
		}
		if byGroup[s.Group] == nil {
			byGroup[s.Group] = map[string]bool{}
		}
		byGroup[s.Group][r] = true
	}
	for _, vals := range byGroup {
		if len(vals) > 1 {
			p.DiffersInGroup = true
		}
	}
	return p
}

// ParamFilter selects parameters.
type ParamFilter struct {
	Q, Category, Kind, State    string
	Changed, Differing, Pending bool
}

func (f ParamFilter) match(p api.ConfigParameter) bool {
	if f.Q != "" {
		q := strings.ToLower(f.Q)
		if !strings.Contains(strings.ToLower(p.Name), q) && !strings.Contains(strings.ToLower(p.Description), q) {
			return false
		}
	}
	if f.Category != "" {
		c := strings.ToLower(f.Category)
		pc := strings.ToLower(p.Category)
		if pc != c && !strings.HasPrefix(pc, c+" / ") {
			return false
		}
	}
	if f.Kind != "" && p.Kind != f.Kind {
		return false
	}
	if f.Changed && len(p.Overrides) == 0 {
		return false
	}
	if f.Differing && !p.Differs {
		return false
	}
	if f.Pending || f.State != "" {
		ok := false
		for _, s := range p.Servers {
			if f.Pending && s.State == StatePendingRestart || f.State != "" && s.State == f.State {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// ListParameters answers listConfigParameters.
func (l *Leader) ListParameters(ctx context.Context, f ParamFilter, limit int, cursor string) (api.ConfigParameterList, error) {
	sn, err := l.snapshot(ctx)
	if err != nil {
		return api.ConfigParameterList{}, err
	}
	out := api.ConfigParameterList{Items: []api.ConfigParameter{}, Revision: sn.rev, Partial: len(sn.missing) > 0, MissingServers: sn.missing}
	if limit <= 0 {
		limit = 50
	}
	for _, m := range sn.meta {
		if cursor != "" && strings.ToLower(m.Name) <= strings.ToLower(cursor) {
			continue
		}
		p := l.parameter(sn, m)
		if !f.match(p) {
			continue
		}
		if len(out.Items) == limit {
			c := out.Items[len(out.Items)-1].Name
			out.NextCursor = &c
			break
		}
		out.Items = append(out.Items, p)
	}
	return out, nil
}

// ErrUnknownParameter is returned for a name PostgreSQL does not know.
var ErrUnknownParameter = errors.New("unknown parameter")

// GetParameter answers getConfigParameter.
func (l *Leader) GetParameter(ctx context.Context, name string) (api.ConfigParameter, error) {
	sn, err := l.snapshot(ctx)
	if err != nil {
		return api.ConfigParameter{}, err
	}
	m, ok := sn.metaBy[strings.ToLower(name)]
	if !ok {
		return api.ConfigParameter{}, ErrUnknownParameter
	}
	return l.parameter(sn, m), nil
}

// Servers answers listConfigServers.
func (l *Leader) Servers(ctx context.Context) (api.ConfigServerList, error) {
	sn, err := l.snapshot(ctx)
	if err != nil {
		return api.ConfigServerList{}, err
	}
	out := api.ConfigServerList{Items: []api.ConfigServerStatus{}, Revision: sn.rev}
	for _, s := range sn.servers {
		out.Items = append(out.Items, l.serverStatus(sn, s))
	}
	return out, nil
}

func (l *Leader) serverStatus(sn *snapshot, s srv) api.ConfigServerStatus {
	x := api.ConfigServerStatus{ServerID: s.ID, State: StateApplied, PendingRestart: []string{}, Failed: []api.ConfigServerFailure{}}
	if s.Group != "" {
		x.ServerGroup = strp(s.Group)
	}
	for _, v := range sn.values {
		switch {
		case v.Level == LevelCluster, v.Level == LevelServerGroup && v.Scope == s.Group && s.Group != "", v.Level == LevelServer && v.Scope == s.ID:
			x.OverrideCount++
		}
	}
	st := sn.states[s.ID]
	if st == nil {
		x.State = StateWaiting
		x.Detail = strp("The server cannot be reached; it gets the change when it returns.")
		return x
	}
	x.AppliedRevision = st.Revision
	pending := map[string]bool{}
	for _, in := range st.Instances {
		for _, n := range in.Pending {
			pending[n] = true
		}
		if !in.Running {
			x.State = StateWaiting
			x.Detail = strp(fmt.Sprintf("Instance %d is not running; it reads the configuration when it starts.", in.ID))
		}
	}
	x.PendingRestart = sortedKeys(pending)
	if sv, ok := sn.stored[s.ID]; ok && sv.State == StateFailed {
		for _, n := range sortedKeys(sv.Errors) {
			x.Failed = append(x.Failed, api.ConfigServerFailure{Name: n, Error: sv.Errors[n]})
		}
	}
	for _, m := range st.Settings {
		if m.Error != "" {
			x.Failed = append(x.Failed, api.ConfigServerFailure{Name: m.Name, Error: m.Error})
		}
	}
	switch {
	case len(x.Failed) > 0:
		x.State = StateFailed
	case x.State == StateWaiting:
	case sn.rev > 0 && st.Revision < latestRevision(sn, s):
		x.State = StateWaiting
		x.Detail = strp("The server has not received the latest change yet.")
	case len(x.PendingRestart) > 0:
		x.State = StatePendingRestart
	}
	return x
}

// latestRevision is the newest revision of a value that applies to the server.
func latestRevision(sn *snapshot, s srv) int64 {
	var rev int64
	for _, v := range sn.values {
		switch {
		case v.Level == LevelCluster, v.Level == LevelServerGroup && v.Scope == s.Group && s.Group != "", v.Level == LevelServer && v.Scope == s.ID:
			if v.Revision > rev {
				rev = v.Revision
			}
		}
	}
	return rev
}

// Summary answers getConfigSummary.
func (l *Leader) Summary(ctx context.Context) (api.ConfigSummary, error) {
	sn, err := l.snapshot(ctx)
	if err != nil {
		return api.ConfigSummary{}, err
	}
	out := api.ConfigSummary{Revision: sn.rev, ServersPendingRestart: []string{}, DifferingInGroup: []string{}, FailedServerIDs: []string{}, WaitingServerIDs: []string{},
		MaxDifferenceMs: l.st.setting(ctx, "clock.maxDifferenceMs", 1000)}
	for _, s := range sn.servers {
		x := l.serverStatus(sn, s)
		switch x.State {
		case StateFailed:
			out.FailedServerIDs = append(out.FailedServerIDs, s.ID)
		case StateWaiting:
			out.WaitingServerIDs = append(out.WaitingServerIDs, s.ID)
		}
		if len(x.PendingRestart) > 0 {
			out.ServersPendingRestart = append(out.ServersPendingRestart, s.ID)
		}
	}
	for _, m := range sn.meta {
		if len(out.DifferingInGroup) >= 50 {
			break
		}
		if cl := Classify(m, sn.hidden[strings.ToLower(m.Name)]); cl.Hidden {
			continue
		}
		if p := l.parameter(sn, m); p.DiffersInGroup {
			out.DifferingInGroup = append(out.DifferingInGroup, p.Name)
		}
	}
	if ct, err := l.Time(ctx); err == nil {
		for _, s := range ct.Servers {
			if s.DifferenceMs == nil || s.ServerID == ct.ReferenceServerID {
				continue
			}
			cur := out.LargestClockDifference.DifferenceMs
			if cur == nil || abs(*s.DifferenceMs) > abs(*cur) {
				d := *s.DifferenceMs
				out.LargestClockDifference = api.ClockDifference{ServerID: strp(s.ServerID), DifferenceMs: &d}
			}
		}
	}
	return out, nil
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
