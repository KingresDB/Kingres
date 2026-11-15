// SPDX-License-Identifier: PostgreSQL

// Package memstore is an in-memory store.Store for tests. It is seeded by
// assigning its exported fields and applies the same filtering, ordering and
// pagination rules as the PostgreSQL implementation.
package memstore

import (
	"context"
	"sort"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// Store is the fake. Set Err to make every method fail with that error, for
// example store.ErrUnavailable.
type Store struct {
	// Err, when non-nil, is returned by every method.
	Err error
	// PGVersion is returned by PostgresVersion.
	PGVersion string

	Summary       store.ClusterSummary
	Settings      api.ClusterSettings
	ServerList    []api.Server
	ServerDetails map[string][]api.DatabaseBucketCount
	LocalServer   *string
	Leader        string
	Groups        []api.ServerGroup
	Topology      api.ReplicationTopology
	DBs           []api.Database
	TablesByDB    map[string][]api.TableDetail
	Colocation    map[string][]api.ColocationGroup
	BucketsByCG   map[string]map[int32][]api.Bucket
	Distribution  map[string]api.BucketDistribution
	MapVersion    int64
	TaskList      []api.Task
	LedgerByDB    map[string]api.LedgerStatus
	BlocksByDB    map[string][]api.LedgerBlock
	CreateOptions api.DatabaseCreateOptions
	RoleList      []api.Role
	GrantList     []api.Grant
}

// New returns a store with empty but valid content.
func New() *Store {
	return &Store{
		PGVersion: "18.1",
		Summary: store.ClusterSummary{
			Health: "healthy", MapGroupAvailable: true, RebalanceNeeded: false,
			RebalanceReasons: []string{},
		},
		Settings: api.ClusterSettings{
			DefaultReplicaCount: 1, DefaultDurability: "sync",
			AutoFailover: api.AutoFailoverSettings{TimeoutSeconds: 120, MaxCount: 1},
			Rebalance:    api.RebalanceSettings{MaxConcurrentMoves: 4},
			Restart:      api.RestartState{ServerIDs: []string{}},
		},
		CreateOptions: api.DatabaseCreateOptions{
			Encodings:                   []api.EncodingOption{{Name: "UTF8", IsDefault: true}},
			Collations:                  []api.CollationOption{{Name: "C", IsDefault: true}},
			BucketCount:                 api.IntegerRange{Default: 128, Minimum: 1, Maximum: 4096},
			DefaultDurability:           "sync",
			LedgerDigestIntervalSeconds: api.IntegerRange{Default: 3600, Minimum: 60, Maximum: 86400},
		},
		Topology:      api.ReplicationTopology{Groups: []api.ShardGroup{}},
		ServerDetails: map[string][]api.DatabaseBucketCount{},
		TablesByDB:    map[string][]api.TableDetail{},
		Colocation:    map[string][]api.ColocationGroup{},
		BucketsByCG:   map[string]map[int32][]api.Bucket{},
		Distribution:  map[string]api.BucketDistribution{},
		LedgerByDB:    map[string]api.LedgerStatus{},
		BlocksByDB:    map[string][]api.LedgerBlock{},
	}
}

var _ store.Store = (*Store)(nil)

// Ping implements store.Store.
func (s *Store) Ping(context.Context) error { return s.Err }

// PostgresVersion implements store.Store.
func (s *Store) PostgresVersion(context.Context) (string, error) { return s.PGVersion, s.Err }

// ClusterSummary implements store.Store.
func (s *Store) ClusterSummary(context.Context) (store.ClusterSummary, error) {
	return s.Summary, s.Err
}

// ClusterSettings implements store.Store.
func (s *Store) ClusterSettings(context.Context) (api.ClusterSettings, error) {
	return s.Settings, s.Err
}

// Servers implements store.Store.
func (s *Store) Servers(context.Context) ([]api.Server, error) {
	out := append([]api.Server{}, s.ServerList...)
	for i := range out {
		out[i].IsManagerLeader = s.Leader != "" && out[i].ID == s.Leader
	}
	return out, s.Err
}

// Server implements store.Store.
func (s *Store) Server(_ context.Context, id string) (api.ServerDetail, error) {
	if s.Err != nil {
		return api.ServerDetail{}, s.Err
	}
	for _, sv := range s.ServerList {
		if sv.ID == id {
			d := s.ServerDetails[id]
			if d == nil {
				d = []api.DatabaseBucketCount{}
			}
			sv.IsManagerLeader = s.Leader != "" && sv.ID == s.Leader
			return api.ServerDetail{Server: sv, BucketsByDatabase: d}, nil
		}
	}
	return api.ServerDetail{}, store.ErrNotFound
}

// LocalServerID implements store.Store.
func (s *Store) LocalServerID(context.Context) (string, error) {
	if s.Err != nil {
		return "", s.Err
	}
	if s.LocalServer == nil {
		return "", store.ErrNotFound
	}
	return *s.LocalServer, nil
}

// ServerGroups implements store.Store.
func (s *Store) ServerGroups(context.Context) ([]api.ServerGroup, error) {
	return append([]api.ServerGroup{}, s.Groups...), s.Err
}

// ReplicationTopology implements store.Store.
func (s *Store) ReplicationTopology(context.Context) (api.ReplicationTopology, error) {
	return s.Topology, s.Err
}

// Databases implements store.Store.
func (s *Store) Databases(context.Context) ([]api.Database, error) {
	return append([]api.Database{}, s.DBs...), s.Err
}

func (s *Store) hasDB(name string) bool {
	for _, d := range s.DBs {
		if d.Name == name {
			return true
		}
	}
	return false
}

// Database implements store.Store.
func (s *Store) Database(_ context.Context, name string) (api.Database, error) {
	if s.Err != nil {
		return api.Database{}, s.Err
	}
	for _, d := range s.DBs {
		if d.Name == name {
			return d, nil
		}
	}
	return api.Database{}, store.ErrNotFound
}

// Tables implements store.Store.
func (s *Store) Tables(_ context.Context, db string, f store.TableFilter, p store.Page) (api.TablePage, error) {
	if s.Err != nil {
		return api.TablePage{}, s.Err
	}
	if !s.hasDB(db) {
		return api.TablePage{}, store.ErrNotFound
	}
	var sel []api.TableDetail
	for _, t := range s.TablesByDB[db] {
		if f.Schema != "" && t.Schema != f.Schema {
			continue
		}
		if f.Class != "" && !contains(t.Classes, f.Class) {
			continue
		}
		sel = append(sel, t)
	}
	sort.Slice(sel, func(i, j int) bool {
		if sel[i].Schema != sel[j].Schema {
			return sel[i].Schema < sel[j].Schema
		}
		return sel[i].Name < sel[j].Name
	})
	// A list item has no columns.
	tables := make([]api.Table, len(sel))
	for i := range sel {
		tables[i] = sel[i].Table
	}
	items, next, err := store.Paginate(tables, p)
	return api.TablePage{Items: items, NextCursor: next}, err
}

// Table implements store.Store.
func (s *Store) Table(_ context.Context, db, schema, name string) (api.TableDetail, error) {
	if s.Err != nil {
		return api.TableDetail{}, s.Err
	}
	for _, t := range s.TablesByDB[db] {
		if t.Schema == schema && t.Name == name {
			return t, nil
		}
	}
	return api.TableDetail{}, store.ErrNotFound
}

// DatabaseCreateOptions implements store.Store.
func (s *Store) DatabaseCreateOptions(context.Context) (api.DatabaseCreateOptions, error) {
	return s.CreateOptions, s.Err
}

// ColocationGroups implements store.Store.
func (s *Store) ColocationGroups(_ context.Context, db string) ([]api.ColocationGroup, error) {
	if s.Err != nil {
		return nil, s.Err
	}
	if !s.hasDB(db) {
		return nil, store.ErrNotFound
	}
	return append([]api.ColocationGroup{}, s.Colocation[db]...), nil
}

// Buckets implements store.Store.
func (s *Store) Buckets(_ context.Context, db string, cg int32, f store.BucketFilter, p store.Page) (api.BucketPage, error) {
	if s.Err != nil {
		return api.BucketPage{}, s.Err
	}
	if !s.hasDB(db) {
		return api.BucketPage{}, store.ErrNotFound
	}
	found := false
	for _, g := range s.Colocation[db] {
		if g.ColocationID == cg {
			found = true
		}
	}
	if !found {
		return api.BucketPage{}, store.ErrNotFound
	}
	var sel []api.Bucket
	for _, b := range s.BucketsByCG[db][cg] {
		if f.ShardGroupID != nil && b.ShardGroupID != *f.ShardGroupID {
			continue
		}
		if f.State != "" && b.State != f.State {
			continue
		}
		if f.ServerID != nil && !hasServer(b, *f.ServerID) {
			continue
		}
		sel = append(sel, b)
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Bucket < sel[j].Bucket })
	items, next, err := store.Paginate(sel, p)
	return api.BucketPage{MapVersion: s.MapVersion, Items: items, NextCursor: next}, err
}

func hasServer(b api.Bucket, id string) bool {
	if b.PrimaryServerID != nil && *b.PrimaryServerID == id {
		return true
	}
	for _, r := range b.ReplicaServerIDs {
		if r == id {
			return true
		}
	}
	return false
}

// BucketDistribution implements store.Store.
func (s *Store) BucketDistribution(_ context.Context, db string, _ *int32) (api.BucketDistribution, error) {
	if s.Err != nil {
		return api.BucketDistribution{}, s.Err
	}
	if !s.hasDB(db) {
		return api.BucketDistribution{}, store.ErrNotFound
	}
	d, ok := s.Distribution[db]
	if !ok {
		return api.BucketDistribution{MapVersion: s.MapVersion, Servers: []api.ServerBucketCount{}}, nil
	}
	return d, nil
}

// Tasks implements store.Store.
func (s *Store) Tasks(_ context.Context, f store.TaskFilter, p store.Page) (api.TaskPage, error) {
	if s.Err != nil {
		return api.TaskPage{}, s.Err
	}
	before, bounded, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return api.TaskPage{}, err
	}
	var sel []api.Task
	for _, t := range s.TaskList {
		if bounded && t.ID >= before {
			continue
		}
		if f.State != "" && t.State != f.State {
			continue
		}
		if f.Kind != "" && t.Kind != f.Kind {
			continue
		}
		sel = append(sel, t)
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].ID > sel[j].ID })
	page := api.TaskPage{Items: []api.Task{}}
	if len(sel) > p.Limit {
		next := store.EncodeKey(sel[p.Limit-1].ID)
		page.NextCursor = &next
		sel = sel[:p.Limit]
	}
	page.Items = append(page.Items, sel...)
	return page, nil
}

// Task implements store.Store.
func (s *Store) Task(_ context.Context, id int64) (api.Task, error) {
	if s.Err != nil {
		return api.Task{}, s.Err
	}
	for _, t := range s.TaskList {
		if t.ID == id {
			return t, nil
		}
	}
	return api.Task{}, store.ErrNotFound
}

// LedgerStatuses implements store.Store.
func (s *Store) LedgerStatuses(context.Context) ([]api.LedgerStatus, error) {
	out := []api.LedgerStatus{}
	for _, d := range s.DBs {
		if st, ok := s.LedgerByDB[d.Name]; ok && st.Mode != nil && *st.Mode != "off" {
			out = append(out, st)
		}
	}
	return out, s.Err
}

// LedgerStatus implements store.Store.
func (s *Store) LedgerStatus(_ context.Context, db string) (api.LedgerStatus, error) {
	if s.Err != nil {
		return api.LedgerStatus{}, s.Err
	}
	if !s.hasDB(db) {
		return api.LedgerStatus{}, store.ErrNotFound
	}
	if st, ok := s.LedgerByDB[db]; ok {
		return st, nil
	}
	return api.LedgerStatus{DatabaseName: db, Mode: strPtr("off"),
		Chains: []api.LedgerChainStatus{}}, nil
}

// LedgerBlocks implements store.Store.
func (s *Store) LedgerBlocks(_ context.Context, db string, shardGroupID *int32, p store.Page) (api.LedgerBlockPage, error) {
	if s.Err != nil {
		return api.LedgerBlockPage{}, s.Err
	}
	if !s.hasDB(db) {
		return api.LedgerBlockPage{}, store.ErrNotFound
	}
	before, bounded, err := store.DecodeKey(p.Cursor)
	if err != nil {
		return api.LedgerBlockPage{}, err
	}
	var sel []api.LedgerBlock
	for _, b := range s.BlocksByDB[db] {
		if shardGroupID != nil && b.ShardGroupID != *shardGroupID {
			continue
		}
		if bounded && b.Height >= before {
			continue
		}
		sel = append(sel, b)
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Height > sel[j].Height })
	page := api.LedgerBlockPage{Items: []api.LedgerBlock{}}
	if len(sel) > p.Limit {
		next := store.EncodeKey(sel[p.Limit-1].Height)
		page.NextCursor = &next
		sel = sel[:p.Limit]
	}
	page.Items = append(page.Items, sel...)
	return page, nil
}

// Roles implements store.Store.
func (s *Store) Roles(_ context.Context, f store.RoleFilter, p store.Page) (api.RolePage, error) {
	if s.Err != nil {
		return api.RolePage{}, s.Err
	}
	var sel []api.Role
	for _, r := range s.RoleList {
		if f.CanLogin != nil && r.CanLogin != *f.CanLogin {
			continue
		}
		if f.Search != "" && !strings.Contains(strings.ToLower(r.Name), strings.ToLower(f.Search)) {
			continue
		}
		sel = append(sel, r)
	}
	sort.Slice(sel, func(i, j int) bool { return sel[i].Name < sel[j].Name })
	items, next, err := store.Paginate(sel, p)
	return api.RolePage{Items: items, NextCursor: next}, err
}

// Role implements store.Store.
func (s *Store) Role(_ context.Context, name string) (api.Role, error) {
	if s.Err != nil {
		return api.Role{}, s.Err
	}
	for _, r := range s.RoleList {
		if r.Name == name {
			return r, nil
		}
	}
	return api.Role{}, store.ErrNotFound
}

// Grants implements store.Store.
func (s *Store) Grants(_ context.Context, f store.GrantFilter, p store.Page) (api.GrantPage, error) {
	if s.Err != nil {
		return api.GrantPage{}, s.Err
	}
	var sel []api.Grant
	for _, g := range s.GrantList {
		if store.GrantMatches(g, f) {
			sel = append(sel, g)
		}
	}
	items, next, err := store.Paginate(sel, p)
	return api.GrantPage{Items: items, NextCursor: next}, err
}

func contains(list []string, v string) bool {
	for _, e := range list {
		if e == v {
			return true
		}
	}
	return false
}

func strPtr(s string) *string { return &s }

// ManagerLeader implements store.Store.
func (s *Store) ManagerLeader(context.Context) (string, error) {
	if s.Err != nil {
		return "", s.Err
	}
	return s.Leader, nil
}
