// SPDX-License-Identifier: AGPL-3.0-only

// Package store defines the read side of the control plane: the Store
// interface that the HTTP handlers use to read cluster state, and helpers
// shared by its implementations. There are two implementations: pgstore
// (PostgreSQL) and memstore (an in-memory fake for tests).
//
// Methods return the API types of package api directly; where a catalog
// table or view of the KingresDB extensions does not exist yet, a method
// returns an empty but valid result, not an error.
package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

// Errors that handlers map to problems.
var (
	// ErrNotFound means the addressed resource does not exist.
	ErrNotFound = errors.New("not found")
	// ErrInvalidCursor means a pagination cursor was not issued by this server.
	ErrInvalidCursor = errors.New("invalid cursor")
	// ErrUnavailable means the backing PostgreSQL instance cannot be used.
	ErrUnavailable = errors.New("backing store unavailable")
)

// ArgError reports an invalid argument that only the store can detect.
type ArgError struct {
	// Field is the name of the invalid parameter.
	Field string
	// Message explains the problem.
	Message string
}

// Error implements error.
func (e *ArgError) Error() string { return fmt.Sprintf("invalid %s: %s", e.Field, e.Message) }

// Page selects one page of a list.
type Page struct {
	// Limit is the page size, 1 to 500.
	Limit int
	// Cursor is the opaque cursor of the previous page, empty for the first.
	Cursor string
}

// TableFilter selects tables.
type TableFilter struct {
	// Schema restricts the result to one schema when non-empty.
	Schema string
	// Class restricts the result to tables having this class when non-empty.
	Class string
	// Storage restricts the result to tables stored in this format (heap or columnar) when non-empty; a partitioned table matches when any of
	// its partitions is.
	Storage string
}

// BucketFilter selects buckets.
type BucketFilter struct {
	// ShardGroupID restricts to one owning group when non-nil.
	ShardGroupID *int32
	// ServerID restricts to buckets with a copy on that server when non-nil.
	ServerID *string
	// State restricts to one bucket state when non-empty.
	State string
}

// TaskFilter selects tasks.
type TaskFilter struct {
	// State and Kind restrict the result when non-empty.
	State, Kind string
}

// RoleFilter selects roles.
type RoleFilter struct {
	// CanLogin restricts by the LOGIN attribute when non-nil.
	CanLogin *bool
	// Search is a case-insensitive substring of the role name.
	Search string
}

// GrantFilter selects grants.
type GrantFilter struct {
	// Grantee, Database, Level, Schema and Table restrict the result when
	// non-empty.
	Grantee, Database, Level, Schema, Table string
}

// ClusterSummary is the data of the cluster overview that comes from the
// catalog; the handler adds identity, version and settings.
type ClusterSummary struct {
	// Health is healthy, degraded or critical.
	Health string
	// MapVersion is kingres.map_version.
	MapVersion int64
	// MapGroupAvailable is false when the map group has no live primary.
	MapGroupAvailable bool
	// Servers counts servers per status.
	Servers api.ServerCounts
	// Buckets counts buckets over all databases.
	Buckets api.BucketCounts
	// DatabaseCount is the number of user databases.
	DatabaseCount int
	// RebalanceNeeded and RebalanceReasons describe the rebalance summary.
	RebalanceNeeded  bool
	RebalanceReasons []string
	// RebalanceRunning and RebalanceTaskID: a rebalance task is pending, running or
	// stopping (a stopped one is not running).
	RebalanceRunning bool
	RebalanceTaskID  *int64
}

// Store is everything the handlers read. Every method may return
// ErrUnavailable (wrapped) when PostgreSQL cannot be reached.
type Store interface {
	// Ping checks that the local PostgreSQL instance answers.
	Ping(ctx context.Context) error
	// PostgresVersion returns the version of the local PostgreSQL, such as
	// "18.7".
	PostgresVersion(ctx context.Context) (string, error)

	// ClusterSummary summarizes servers, buckets and databases.
	ClusterSummary(ctx context.Context) (ClusterSummary, error)
	// ClusterSettings returns the cluster-wide settings.
	ClusterSettings(ctx context.Context) (api.ClusterSettings, error)

	// Servers lists the servers; Server returns one with its buckets per
	// database; LocalServerID is the id of the server this node runs on
	// (ErrNotFound when the node has no catalog entry).
	Servers(ctx context.Context) ([]api.Server, error)
	Server(ctx context.Context, id string) (api.ServerDetail, error)
	LocalServerID(ctx context.Context) (string, error)
	// ManagerLeader is the id of the server that holds the primary of shard
	// group 0 (design control-plane-3.md, D6); "" when there is none or no
	// catalog yet.
	ManagerLeader(ctx context.Context) (string, error)
	// ServerGroups lists the server groups.
	ServerGroups(ctx context.Context) ([]api.ServerGroup, error)
	// ReplicationTopology reads the shard groups and pg_stat_replication.
	ReplicationTopology(ctx context.Context) (api.ReplicationTopology, error)

	// Databases lists the databases; Database returns one (ErrNotFound).
	Databases(ctx context.Context) ([]api.Database, error)
	Database(ctx context.Context, name string) (api.Database, error)
	// Tables and Table read the tables of a database.
	Tables(ctx context.Context, db string, f TableFilter, p Page) (api.TablePage, error)
	Table(ctx context.Context, db, schema, name string) (api.TableDetail, error)
	// DatabaseCreateOptions lists what the create-database dialog offers,
	// read from the PostgreSQL catalogs.
	DatabaseCreateOptions(ctx context.Context) (api.DatabaseCreateOptions, error)
	// ColocationGroups, Buckets and BucketDistribution read the shard
	// catalog of a database.
	ColocationGroups(ctx context.Context, db string) ([]api.ColocationGroup, error)
	Buckets(ctx context.Context, db string, colocationID int32, f BucketFilter, p Page) (api.BucketPage, error)
	BucketDistribution(ctx context.Context, db string, colocationID *int32) (api.BucketDistribution, error)

	// Tasks and Task read kingres_cp.task of the system database.
	Tasks(ctx context.Context, f TaskFilter, p Page) (api.TaskPage, error)
	Task(ctx context.Context, id int64) (api.Task, error)

	// LedgerStatuses lists the ledger status of every database with a
	// ledger; LedgerStatus returns one database; LedgerBlocks lists the
	// block headers of a chain, newest first.
	LedgerStatuses(ctx context.Context) ([]api.LedgerStatus, error)
	LedgerStatus(ctx context.Context, db string) (api.LedgerStatus, error)
	LedgerBlocks(ctx context.Context, db string, shardGroupID *int32, p Page) (api.LedgerBlockPage, error)

	// Roles, Role and Grants read the PostgreSQL catalogs.
	Roles(ctx context.Context, f RoleFilter, p Page) (api.RolePage, error)
	Role(ctx context.Context, name string) (api.Role, error)
	Grants(ctx context.Context, f GrantFilter, p Page) (api.GrantPage, error)
}

// Cursors are opaque to clients: base64url of "o<offset>" (position in an
// ordered result) or "k<key>" (keyset: the last id or height returned).

// EncodeOffset returns the cursor for the position offset.
func EncodeOffset(offset int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o" + strconv.Itoa(offset)))
}

// DecodeOffset parses a cursor made by EncodeOffset; "" means 0.
func DecodeOffset(cursor string) (int, error) {
	if cursor == "" {
		return 0, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(b) < 2 || b[0] != 'o' {
		return 0, ErrInvalidCursor
	}
	n, err := strconv.Atoi(string(b[1:]))
	if err != nil || n < 0 {
		return 0, ErrInvalidCursor
	}
	return n, nil
}

// EncodeKey returns the keyset cursor for key.
func EncodeKey(key int64) string {
	return base64.RawURLEncoding.EncodeToString([]byte("k" + strconv.FormatInt(key, 10)))
}

// DecodeKey parses a cursor made by EncodeKey; "" means "no bound" (0, false).
func DecodeKey(cursor string) (key int64, present bool, err error) {
	if cursor == "" {
		return 0, false, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(b) < 2 || b[0] != 'k' {
		return 0, false, ErrInvalidCursor
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b[1:])), 10, 64)
	if err != nil {
		return 0, false, ErrInvalidCursor
	}
	return n, true, nil
}

// Paginate returns the page of items selected by p and the cursor of the next
// page (nil on the last page). items must already be in their final order.
// The returned slice is never nil.
func Paginate[T any](items []T, p Page) ([]T, *string, error) {
	off, err := DecodeOffset(p.Cursor)
	if err != nil {
		return nil, nil, err
	}
	if off > len(items) {
		off = len(items)
	}
	end := off + p.Limit
	if end > len(items) {
		end = len(items)
	}
	page := append(make([]T, 0, end-off), items[off:end]...)
	if end < len(items) {
		next := EncodeOffset(end)
		return page, &next, nil
	}
	return page, nil, nil
}

// GrantMatches reports whether g satisfies the filter. Both implementations
// use it so that filtering behaves identically.
func GrantMatches(g api.Grant, f GrantFilter) bool {
	if f.Grantee != "" && g.Grantee != f.Grantee {
		return false
	}
	if f.Database != "" && g.Database != f.Database {
		return false
	}
	if f.Level != "" && g.Level != f.Level {
		return false
	}
	if f.Schema != "" && (g.Schema == nil || *g.Schema != f.Schema) {
		return false
	}
	if f.Table != "" && (g.Table == nil || *g.Table != f.Table) {
		return false
	}
	return true
}

// EncodeTextKey returns the keyset cursor for a key made of several text parts
// (for example schema and table name): base64url of "t" and a JSON array.
func EncodeTextKey(parts ...string) string {
	b, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(append([]byte("t"), b...))
}

// DecodeTextKey parses a cursor made by EncodeTextKey with n parts; "" means
// "no bound" (nil).
func DecodeTextKey(cursor string, n int) ([]string, error) {
	if cursor == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(b) < 3 || b[0] != 't' || len(b) > 4096 {
		return nil, ErrInvalidCursor
	}
	var parts []string
	if json.Unmarshal(b[1:], &parts) != nil || len(parts) != n {
		return nil, ErrInvalidCursor
	}
	return parts, nil
}

// KeysetPage cuts items read with a limit of p.Limit+1 to the page and returns
// the cursor of the next one (nil on the last page); key makes the cursor from
// the last item of the page.
func KeysetPage[T any](items []T, p Page, key func(T) string) ([]T, *string) {
	if len(items) <= p.Limit {
		return items, nil
	}
	next := key(items[p.Limit-1])
	return items[:p.Limit], &next
}

// StorageMatches tells whether a table is stored in the format (heap or columnar): its own access method is it, or, for a partitioned table, any of
// its partitions is stored in it. A table without storage information matches nothing.
func StorageMatches(t api.Table, format string) bool {
	if t.Storage == nil {
		return false
	}
	if t.Storage.AccessMethod != nil && *t.Storage.AccessMethod == format {
		return true
	}
	if f := t.Storage.PartitionFormats; f != nil {
		switch format {
		case "heap":
			return f.Heap > 0
		case "columnar":
			return f.Columnar > 0
		}
	}
	return false
}
