// SPDX-License-Identifier: AGPL-3.0-only

package pgstore

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// databaseRow is a row of pg_database with the size and connection count.
type databaseRow struct {
	oid         uint32
	name        string
	owner       string
	encoding    string
	collate     string
	ctype       string
	connLimit   int
	sizeBytes   int64
	connections int
}

// listDatabaseRows reads pg_database; name filters to one database when
// non-empty.
func (s *Store) listDatabaseRows(ctx context.Context, name string) ([]databaseRow, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return nil, err
	}
	rows, err := p.Query(ctx, `
SELECT d.oid, d.datname, pg_get_userbyid(d.datdba), pg_encoding_to_char(d.encoding),
       coalesce(d.datcollate, ''), coalesce(d.datctype, ''), d.datconnlimit,
       CASE WHEN has_database_privilege(d.oid, 'CONNECT') THEN pg_database_size(d.oid) ELSE 0 END,
       (SELECT count(*) FROM pg_stat_activity a WHERE a.datname = d.datname)
FROM pg_database d
WHERE NOT d.datistemplate AND d.datallowconn AND d.datname <> 'kingres_system' AND ($1 = '' OR d.datname = $1)
ORDER BY d.datname`, name)
	if err != nil {
		return nil, s.wrap("listing databases", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (databaseRow, error) {
		var d databaseRow
		err := r.Scan(&d.oid, &d.name, &d.owner, &d.encoding, &d.collate, &d.ctype, &d.connLimit, &d.sizeBytes, &d.connections)
		return d, err
	})
	return out, s.wrap("listing databases", err)
}

// dbProbe holds what is learned by connecting to a database.
type dbProbe struct {
	// ok is false when the database could not be probed.
	ok         bool
	tableCount int
	hasShard   bool
	hasLedger  bool
	// bucketCount is the largest colocation bucket count; nil when the
	// database has no colocation group.
	bucketCount *int
	// mode and distributeNew are the sharding mode of the database (extension 0.8); "" for an older one.
	mode          string
	distributeNew bool
	schemaCount   *int
}

func (s *Store) probeDatabase(ctx context.Context, db string) dbProbe {
	var probe dbProbe
	p, err := s.pool(ctx, db)
	if err != nil {
		return probe
	}
	err = p.QueryRow(ctx, `
SELECT (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relkind IN ('r', 'p') AND NOT c.relispartition
          AND n.nspname NOT IN ('pg_catalog', 'information_schema', 'kingres', 'kingres_cp')
          AND n.nspname !~ '^pg_(toast|temp)'),
       to_regclass('kingres.bucket_map') IS NOT NULL,
       to_regclass('kingres.ledger_blocks') IS NOT NULL`).Scan(&probe.tableCount, &probe.hasShard, &probe.hasLedger)
	if err != nil {
		s.opts.Logger.Debug("cannot probe database", "database", db, "error", err)
		return dbProbe{}
	}
	probe.ok = true
	if probe.hasShard {
		var n *int
		if err := p.QueryRow(ctx, "SELECT max(bucket_count) FROM kingres.colocation").Scan(&n); err == nil {
			probe.bucketCount = n
		}
		var hasMode bool
		if err := p.QueryRow(ctx, "SELECT to_regclass('kingres.sharding_settings') IS NOT NULL").Scan(&hasMode); err == nil && hasMode {
			_ = p.QueryRow(ctx, "SELECT mode, distribute_new_schemas FROM kingres.sharding_settings").Scan(&probe.mode, &probe.distributeNew)
			var n int
			if err := p.QueryRow(ctx, "SELECT count(*) FROM kingres.dist_schema").Scan(&n); err == nil {
				probe.schemaCount = &n
			}
			if probe.mode == "schema" {
				probe.bucketCount = nil
			}
		}
	}
	return probe
}

// durabilityOf maps a synchronous_commit value to the API enum.
func durabilityOf(v string) string {
	switch strings.ToLower(v) {
	case "local", "off":
		return "async"
	case "remote_apply":
		return "syncApply"
	default:
		return "sync"
	}
}

// databaseDurability reads a per-database synchronous_commit setting.
func (s *Store) databaseDurability(ctx context.Context, oid uint32, fallback string) string {
	p, err := s.pool(ctx, "")
	if err != nil {
		return fallback
	}
	var v string
	err = p.QueryRow(ctx, `SELECT split_part(c, '=', 2) FROM pg_db_role_setting r, unnest(r.setconfig) c
		WHERE r.setdatabase = $1 AND r.setrole = 0 AND c LIKE 'synchronous\_commit=%'`, oid).Scan(&v)
	if err != nil {
		return fallback
	}
	return durabilityOf(v)
}

func (s *Store) buildDatabases(ctx context.Context, name string) ([]api.Database, error) {
	rows, err := s.listDatabaseRows(ctx, name)
	if err != nil {
		return nil, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return nil, err
	}
	settings, err := s.ClusterSettings(ctx)
	if err != nil {
		return nil, err
	}
	summary, err := s.ClusterSummary(ctx)
	if err != nil {
		return nil, err
	}
	recorded, err := s.dbSettings(ctx)
	if err != nil {
		return nil, err
	}
	replicas := cat.replicaCount()
	out := make([]api.Database, 0, len(rows))
	for _, r := range rows {
		probe := s.probeDatabase(ctx, r.name)
		db := newDatabase(r, probe, replicas, s.databaseDurability(ctx, r.oid, settings.DefaultDurability))
		if st, ok := recorded[r.name]; ok {
			by, at, mode := st.createdBy, st.createdAt, st.ledgerMode
			db.CreatedBy, db.CreatedAt = &by, &at
			db.Ledger.Mode, db.Ledger.DigestIntervalSeconds = &mode, st.digest
			if st.sharding && st.bucketCnt != nil && db.Sharding.BucketCount == nil {
				db.Sharding.BucketCount = st.bucketCnt
			}
		}
		if probe.hasShard {
			stats, _, err := s.bucketStats(ctx, r.name, nil)
			if err != nil {
				return nil, err
			}
			var unavailable, under int
			for g, gb := range stats {
				un, ur := groupHealth(cat, g, replicas)
				if un {
					unavailable += gb.held()
				} else if ur {
					under += gb.held()
				}
			}
			db.Health = api.DatabaseHealth{UnderReplicatedBuckets: &under, UnavailableBuckets: &unavailable}
			// The ledger chains this server reads are those of its own shard group (see
			// ledgerStatus); the summary says so instead of implying it covers the others.
			own := int32(-1)
			if id, err := s.localNodeID(ctx); err == nil {
				for _, n := range cat.nodes {
					if n.ID == id {
						own = n.Group
					}
				}
			}
			unc := []int32{}
			for g := range stats {
				if g != own {
					unc = append(unc, g)
				}
			}
			sort.Slice(unc, func(i, j int) bool { return unc[i] < unc[j] })
			complete := len(unc) == 0
			db.LedgerSummary.Complete, db.LedgerSummary.UncoveredShardGroupIDs = &complete, unc
			db.RebalanceRequired = summary.RebalanceNeeded
		}
		out = append(out, db)
	}
	return out, nil
}

// Databases implements store.Store.
func (s *Store) Databases(ctx context.Context) ([]api.Database, error) {
	return s.buildDatabases(ctx, "")
}

// Database implements store.Store.
func (s *Store) Database(ctx context.Context, name string) (api.Database, error) {
	dbs, err := s.buildDatabases(ctx, name)
	if err != nil {
		return api.Database{}, err
	}
	if len(dbs) == 0 {
		return api.Database{}, store.ErrNotFound
	}
	return dbs[0], nil
}

// requireDatabase returns the pool of an existing, connectable database or
// store.ErrNotFound. The existence check keeps arbitrary names from creating
// connection pools.
func (s *Store) requireDatabase(ctx context.Context, name string) (*dbHandle, error) {
	rows, err := s.listDatabaseRows(ctx, name)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || name == "" {
		return nil, store.ErrNotFound
	}
	p, err := s.pool(ctx, name)
	if err != nil {
		return nil, err
	}
	return &dbHandle{name: name, pool: p}, nil
}

// ClusterSettings implements store.Store. Nothing is persisted yet, so the
// values are derived from the catalog and PostgreSQL settings, and the
// auto-failover and rebalance values are the defaults of the contract.
func (s *Store) ClusterSettings(ctx context.Context) (api.ClusterSettings, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return api.ClusterSettings{}, err
	}
	cat, err := s.loadCatalog(ctx)
	if err != nil {
		return api.ClusterSettings{}, err
	}
	var sc string
	if err := p.QueryRow(ctx, "SELECT current_setting('synchronous_commit')").Scan(&sc); err != nil {
		return api.ClusterSettings{}, s.wrap("reading synchronous_commit", err)
	}
	var shard, ledger bool
	if err := p.QueryRow(ctx, `SELECT to_regclass('kingres.node') IS NOT NULL,
		to_regclass('kingres.ledger_blocks') IS NOT NULL`).Scan(&shard, &ledger); err != nil {
		return api.ClusterSettings{}, s.wrap("checking features", err)
	}
	out := api.ClusterSettings{
		DefaultReplicaCount: cat.replicaCount(),
		DefaultDurability:   durabilityOf(sc),
		AutoFailover:        api.AutoFailoverSettings{Enabled: false, TimeoutSeconds: 120, MaxCount: 1},
		Rebalance: api.RebalanceSettings{MaxConcurrentMoves: 4, Strategy: "byBucketCount", MaxMovesPerServer: 2, MaxMoveAttempts: 3,
			MaxDiskUsedFraction: 0.85, MaxStandbyLagBytes: 64 << 20, Automatic: defaultAutomatic()},
		Features: api.FeatureFlags{Sharding: shard, Ledger: ledger},
		Restart:  api.RestartState{ServerIDs: []string{}},
		Clock:    &api.ClockSettings{MaxDifferenceMs: 1000},
	}
	// Values the administrator set (kingres_cp.cluster_setting) win over the
	// defaults derived from the catalogs.
	if ok, _ := s.regclassExists(ctx, p, "kingres_cp.cluster_setting"); ok {
		rows, err := p.Query(ctx, "SELECT key, value FROM kingres_cp.cluster_setting")
		if err != nil {
			return out, s.wrap("reading kingres_cp.cluster_setting", err)
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var v []byte
			if err := rows.Scan(&k, &v); err != nil {
				return out, s.wrap("reading kingres_cp.cluster_setting", err)
			}
			var n int
			var str string
			var b bool
			switch k {
			case "replica.default":
				if json.Unmarshal(v, &n) == nil {
					out.DefaultReplicaCount = n
				}
			case "durability.default":
				if json.Unmarshal(v, &str) == nil {
					out.DefaultDurability = str
				}
			case "autoFailover.enabled":
				if json.Unmarshal(v, &b) == nil {
					out.AutoFailover.Enabled = b
				}
			case "autoFailover.timeoutSeconds":
				if json.Unmarshal(v, &n) == nil {
					out.AutoFailover.TimeoutSeconds = n
				}
			case "autoFailover.eventsSinceReset":
				if json.Unmarshal(v, &n) == nil {
					out.AutoFailover.EventsSinceReset = n
				}
			case "autoFailover.maxCount":
				if json.Unmarshal(v, &n) == nil {
					out.AutoFailover.MaxCount = n
				}
			case "clock.maxDifferenceMs":
				if json.Unmarshal(v, &n) == nil {
					out.Clock = &api.ClockSettings{MaxDifferenceMs: n}
				}
			case "rebalance.maxConcurrentMoves":
				if json.Unmarshal(v, &n) == nil {
					out.Rebalance.MaxConcurrentMoves = n
				}
			case "rebalance.strategy":
				if json.Unmarshal(v, &str) == nil {
					out.Rebalance.Strategy = str
				}
			case "rebalance.threshold":
				var f float64
				if json.Unmarshal(v, &f) == nil && string(v) != "null" {
					out.Rebalance.Threshold = &f
				}
			case "rebalance.maxMovesPerServer":
				if json.Unmarshal(v, &n) == nil {
					out.Rebalance.MaxMovesPerServer = n
				}
			case "rebalance.maxMoveAttempts":
				if json.Unmarshal(v, &n) == nil {
					out.Rebalance.MaxMoveAttempts = n
				}
			case "rebalance.maxDiskUsedFraction":
				var f float64
				if json.Unmarshal(v, &f) == nil {
					out.Rebalance.MaxDiskUsedFraction = f
				}
			case "rebalance.automatic.enabled", "rebalance.automatic.strategy", "rebalance.automatic.threshold", "rebalance.automatic.checkIntervalSeconds",
				"rebalance.automatic.settleSeconds", "rebalance.automatic.settleAfterTopologySeconds", "rebalance.automatic.minIntervalSeconds",
				"rebalance.automatic.failoverSettleSeconds", "rebalance.automatic.window":
				applyAutomatic(out.Rebalance.Automatic, strings.TrimPrefix(k, "rebalance.automatic."), v)
			case "rebalance.maxStandbyLagBytes":
				var b int64
				if json.Unmarshal(v, &b) == nil {
					out.Rebalance.MaxStandbyLagBytes = b
				}
			}
		}
		if err := rows.Err(); err != nil {
			return out, s.wrap("reading kingres_cp.cluster_setting", err)
		}
	}
	return out, nil
}

func modeFlag(p dbProbe) *bool {
	if p.mode == "" {
		return nil
	}
	b := p.distributeNew
	return &b
}

// newDatabase builds the response for one database. Values that no catalog
// provides are left nil, which the API reports as null ("not available"):
// the ledger mode and digest interval (not stored anywhere), the commit rate
// and the ledger verification state. A database without a shard catalog has
// no buckets, so its bucket health is a true 0.
func newDatabase(r databaseRow, probe dbProbe, replicas int, durability string) api.Database {
	zero := 0
	db := api.Database{
		Name: r.name, Status: "ready",
		General: api.DatabaseGeneral{Owner: &r.owner, Encoding: &r.encoding, LCCollate: &r.collate,
			LCCtype: &r.ctype, ConnectionLimit: &r.connLimit},
		Sharding:    api.ShardingSettings{Enabled: probe.hasShard, BucketCount: probe.bucketCount, Mode: probe.mode, DistributeNewSchemas: modeFlag(probe)},
		Replication: api.ReplicationSettings{Durability: &durability, ReplicaCount: replicas},
		Stats:       api.DatabaseStats{SizeBytes: &r.sizeBytes, ActiveConnections: &r.connections},
		Health:      api.DatabaseHealth{UnderReplicatedBuckets: &zero, UnavailableBuckets: &zero},
	}
	if probe.ok {
		tables := probe.tableCount
		db.Stats.TableCount = &tables
		db.Stats.DistributedSchemaCount = probe.schemaCount
	}
	if !probe.hasLedger {
		off := "off"
		db.Ledger.Mode = &off // no ledger catalog: the ledger is certainly off
	}
	return db
}

// encodingsNotForServers lists the client-only encodings that
// pg_encoding_to_char knows but a database cannot use.
var encodingsNotForServers = []string{"BIG5", "GBK", "GB18030", "JOHAB", "SJIS", "SHIFT_JIS_2004", "UHC"}

// DatabaseCreateOptions implements store.Store. Encodings and locales come
// from the PostgreSQL catalogs; the defaults are the encoding and locale of
// template1, which is what CREATE DATABASE uses when none is given.
func (s *Store) DatabaseCreateOptions(ctx context.Context) (api.DatabaseCreateOptions, error) {
	p, err := s.pool(ctx, "")
	if err != nil {
		return api.DatabaseCreateOptions{}, err
	}
	out := api.DatabaseCreateOptions{
		Encodings: []api.EncodingOption{}, Collations: []api.CollationOption{},
		BucketCount:   api.IntegerRange{Default: 128, Minimum: 1, Maximum: 4096},
		ShardingModes: []string{"row", "schema", "both"}, ShardingModeDefault: "row",
		LedgerDigestIntervalSeconds: api.IntegerRange{Default: 3600, Minimum: 60, Maximum: 86400},
	}
	var defEnc, defCollate string
	if err := p.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding), coalesce(datcollate, '')
		FROM pg_database WHERE datname = 'template1'`).Scan(&defEnc, &defCollate); err != nil {
		return out, s.wrap("reading template1", err)
	}
	rows, err := p.Query(ctx, `SELECT pg_encoding_to_char(i) FROM generate_series(0, 60) i
		WHERE pg_encoding_to_char(i) <> '' AND NOT pg_encoding_to_char(i) = ANY($1) ORDER BY 1`, encodingsNotForServers)
	if err != nil {
		return out, s.wrap("listing encodings", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return out, s.wrap("listing encodings", err)
	}
	for _, n := range names {
		out.Encodings = append(out.Encodings, api.EncodingOption{Name: n, IsDefault: n == defEnc})
	}
	// libc collations; collencoding -1 means the name works with every encoding.
	crows, err := p.Query(ctx, `SELECT collname, array_agg(DISTINCT pg_encoding_to_char(collencoding)) FILTER (WHERE collencoding >= 0),
			bool_or(collencoding < 0)
		FROM pg_collation WHERE collprovider = 'c' GROUP BY collname ORDER BY collname`)
	if err != nil {
		return out, s.wrap("listing collations", err)
	}
	defer crows.Close()
	for crows.Next() {
		var name string
		var encs []string
		var all bool
		if err := crows.Scan(&name, &encs, &all); err != nil {
			return out, s.wrap("scanning collations", err)
		}
		opt := api.CollationOption{Name: name, IsDefault: name == defCollate}
		if !all {
			opt.Encodings = encs
		}
		out.Collations = append(out.Collations, opt)
	}
	if err := crows.Err(); err != nil {
		return out, s.wrap("listing collations", err)
	}
	// template1's locale may be a name that pg_collation does not list.
	found := false
	for _, c := range out.Collations {
		if c.IsDefault {
			found = true
		}
	}
	if !found && defCollate != "" {
		out.Collations = append(out.Collations, api.CollationOption{Name: defCollate, IsDefault: true})
	}
	settings, err := s.ClusterSettings(ctx)
	if err != nil {
		return out, err
	}
	out.ReplicaCount = settings.DefaultReplicaCount
	out.DefaultDurability = settings.DefaultDurability
	return out, nil
}
