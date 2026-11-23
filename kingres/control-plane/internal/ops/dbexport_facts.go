// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

// What the database export reads from the catalog (contract 0.40.0, design "The task database.export" and "The catalog document"). Everything here is read by
// the service role in one read-only transaction, in one pass, from the catalogs of PostgreSQL and of the extensions; the result is structured data. It is
// read twice: at the start of the structure step and in the check, and the two fingerprints must be equal (a table or a column that changed in between
// fails the task with schema_changed).

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/dumpnorm"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// dbTable is a user table of the database.
type dbTable struct {
	OID         int64
	Schema      string
	Name        string
	Relkind     string // r or p
	IsPartition bool
	Parent      int64 // the partition parent, 0 when the table is not a partition
	Persistence string
	AM          string
	RLS         bool
	Owner       string
	Rows        int64
	Bytes       int64
	// Cols are the columns of the data entry: the columns that are not generated, in order. All is every column, with its type, for the fingerprint.
	Cols []string
	// Gen are the generated columns.
	Gen []string
	All []string

	Method     string // "", hash or reference
	DistColumn string
	Coloc      int
	Inherits   []int64 // the inheritance parents (not partition parents)
	Children   int     // the number of inheritance children

	Ledger *dbLedger
}

func (t dbTable) qualified() string { return t.Schema + "." + t.Name }

type dbLedger struct {
	Mode, Policy string
	HeadHeight   int64
	HeadHash     string
	HistoryOID   int64
	HistoryName  string // schema.name
	HistoryCols  []string
}

type dbColoc struct {
	ID        int
	Buckets   int
	Type      string
	Collation string
	Groups    int
}

type dbPartition struct {
	Name, Bound string
}

type dbBucketRel struct {
	OID      int64
	Root     int64
	Name     string // schema.name
	Relkind  string
	Bound    string
	KeyDef   string
	LeafName string // schema.name
}

type dbSeq struct {
	Schema, Name, Owned string
	Last                int64
	Called              bool
}

type dbPolicy struct {
	Schema, Table                                                 string
	Every, Zone, Ahead, Retention, Action, ColAfter, ColSort, Run string
	ColOpts                                                       map[string]string
	Enabled                                                       bool
}

// dbFacts is everything the export needs to know of the database.
type dbFacts struct {
	Encoding, Collate, Ctype, Provider string
	PGMajor                            int
	Extensions                         []archive.ManifestExtension
	Tables                             []dbTable
	byOID                              map[int64]*dbTable
	Colocs                             map[int]*dbColoc
	UserPart                           map[int64][]dbPartition // by the OID of the distributed table, in the order of dist_partition
	PartKey                            map[int64]string        // "RANGE (ts)" by the OID of the distributed table
	Buckets                            map[int64]bool          // the OIDs of bucket partitions and their leaves (every descendant of a hash table)
	BucketNames                        []string                // schema.name of those
	BucketIdx                          []string                // schema.name of the indexes of those
	Seqs                               []dbSeq
	Policies                           []dbPolicy
	LargeObjects                       int64
	OIDColumns                         []string
	Unsupported                        []string // kinds of objects that the archive does not carry, with a name each
	Hash                               map[string]dumpnorm.HashTable
	columnar                           map[int64]map[string]string // the options of the columnar tables, by OID
	Schemas                            []dbSchema                  // the distributed schemas (extension 0.8 and later)
}

type dbSchema struct {
	Name  string
	Coloc int
}

const userSchemaFilter = `n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND n.nspname NOT IN ('kingres', 'kingres_transfer')`

func hasTable(ctx context.Context, tx pgx.Tx, rel string) bool {
	var ok bool
	_ = tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, rel).Scan(&ok)
	return ok
}

// readDBFacts reads the catalogs. tx is a transaction of the service role on the database.
func readDBFacts(ctx context.Context, tx pgx.Tx) (*dbFacts, error) {
	// The names of this session are those of the catalog and nothing a database owner set with ALTER DATABASE ... SET search_path.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return nil, err
	}
	f := &dbFacts{byOID: map[int64]*dbTable{}, Colocs: map[int]*dbColoc{}, UserPart: map[int64][]dbPartition{}, PartKey: map[int64]string{}, Buckets: map[int64]bool{}, Hash: map[string]dumpnorm.HashTable{}}
	if err := tx.QueryRow(ctx, `SELECT pg_encoding_to_char(encoding), datcollate, datctype, CASE datlocprovider WHEN 'c' THEN 'libc' WHEN 'b' THEN 'builtin' WHEN 'i' THEN 'icu' ELSE 'libc' END,
			(current_setting('server_version_num')::int / 10000)
		FROM pg_database WHERE datname = current_database()`).Scan(&f.Encoding, &f.Collate, &f.Ctype, &f.Provider, &f.PGMajor); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT e.extname::text, e.extversion, n.nspname::text FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace ORDER BY e.extname`)
	if err != nil {
		return nil, err
	}
	f.Extensions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (archive.ManifestExtension, error) {
		var x archive.ManifestExtension
		return x, r.Scan(&x.Name, &x.Version, &x.Schema)
	})
	if err != nil {
		return nil, err
	}
	// The tables.
	rows, err = tx.Query(ctx, `SELECT c.oid::int8, n.nspname::text, c.relname::text, c.relkind::text, c.relispartition, c.relpersistence::text, c.relrowsecurity,
			coalesce(am.amname::text, ''), pg_get_userbyid(c.relowner)::text, greatest(c.reltuples, 0)::int8, pg_total_relation_size(c.oid)::int8,
			coalesce((SELECT i.inhparent::int8 FROM pg_inherits i WHERE i.inhrelid = c.oid AND c.relispartition), 0)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace LEFT JOIN pg_am am ON am.oid = c.relam
		WHERE c.relkind IN ('r', 'p') AND `+userSchemaFilter+`
		  AND NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.deptype = 'e')
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, err
	}
	f.Tables, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (dbTable, error) {
		var t dbTable
		return t, r.Scan(&t.OID, &t.Schema, &t.Name, &t.Relkind, &t.IsPartition, &t.Persistence, &t.RLS, &t.AM, &t.Owner, &t.Rows, &t.Bytes, &t.Parent)
	})
	if err != nil {
		return nil, err
	}
	for i := range f.Tables {
		f.byOID[f.Tables[i].OID] = &f.Tables[i]
	}
	// The columns of every table (and of the history tables, which are in the list too).
	rows, err = tx.Query(ctx, `SELECT a.attrelid::int8, a.attname::text, a.attgenerated::text <> '', format_type(a.atttypid, a.atttypmod), t.typname::text
		FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace JOIN pg_type t ON t.oid = a.atttypid
		WHERE a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'p') AND `+userSchemaFilter+` ORDER BY a.attrelid, a.attnum`)
	if err != nil {
		return nil, err
	}
	oidTypes := map[string]bool{"oid": true, "regclass": true, "regproc": true, "regprocedure": true, "regoper": true, "regoperator": true, "regtype": true, "regconfig": true,
		"regdictionary": true, "regnamespace": true, "regrole": true, "regcollation": true, "lo": true}
	for rows.Next() {
		var rel int64
		var name, typ, typname string
		var gen bool
		if err := rows.Scan(&rel, &name, &gen, &typ, &typname); err != nil {
			rows.Close()
			return nil, err
		}
		t := f.byOID[rel]
		if t == nil {
			continue
		}
		t.All = append(t.All, name+" "+typ+boolText(gen, " generated"))
		if !gen {
			t.Cols = append(t.Cols, name)
		} else {
			t.Gen = append(t.Gen, name)
		}
		if oidTypes[typname] {
			f.OIDColumns = append(f.OIDColumns, t.qualified()+"."+name)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Inheritance that is not partitioning.
	rows, err = tx.Query(ctx, `SELECT i.inhrelid::int8, i.inhparent::int8 FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid WHERE NOT c.relispartition ORDER BY i.inhrelid, i.inhseqno`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var child, parent int64
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		if c, p := f.byOID[child], f.byOID[parent]; c != nil && p != nil {
			c.Inherits = append(c.Inherits, parent)
			p.Children++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Nothing in the schema kingres is read or called before extguard has shown that it belongs to the extension that is named: the service role is a superuser
	// and a database owner can make a schema of that name with views and functions of its own. An untrusted schema is a refusal, an absent extension is not.
	guard := extguard.NewGuard(tx)
	if err := f.checkFixedSchemas(ctx, tx, guard); err != nil {
		return nil, err
	}
	use := func(ext, rel string) (bool, error) {
		if ext == extguard.Shard {
			ok, err := f.shardUsable(ctx, tx)
			return ok && hasTable(ctx, tx, rel), err
		}
		info, err := guard.Verify(ctx, ext)
		if err != nil {
			return false, err
		}
		return info.Present && hasTable(ctx, tx, rel), nil
	}
	if ok, err := use(extguard.Shard, "kingres.dist_table"); err != nil {
		return nil, err
	} else if ok {
		if err := f.readDistribution(ctx, tx); err != nil {
			return nil, err
		}
	}
	if ok, err := use(extguard.Ledger, "kingres.ledger_table"); err != nil {
		return nil, err
	} else if ok {
		if err := f.readLedger(ctx, tx); err != nil {
			return nil, err
		}
	}
	if ok, err := use(extguard.Timepart, "kingres.timepart_policy_data"); err != nil {
		return nil, err
	} else if ok {
		if err := f.readPolicies(ctx, tx); err != nil {
			return nil, err
		}
	}
	for _, t := range f.Tables {
		if t.AM == "columnar" {
			// A table of the columnar access method needs the extension; the call below is the extension's own function.
			if _, err := guard.Require(ctx, extguard.Columnar); err != nil {
				return nil, err
			}
			if err := f.readColumnar(ctx, tx); err != nil {
				return nil, err
			}
			break
		}
	}
	if err := f.readSequences(ctx, tx); err != nil {
		return nil, err
	}
	if err := f.readUnsupported(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.QueryRow(ctx, `SELECT count(*)::int8 FROM pg_largeobject_metadata`).Scan(&f.LargeObjects); err != nil {
		return nil, err
	}
	// A name with a dot is ambiguous in "schema.table" (the manifest and the catalog write names so): it is refused instead of guessed.
	for _, t := range f.Tables {
		if strings.Contains(t.Schema, ".") || strings.Contains(t.Name, ".") {
			f.Unsupported = append(f.Unsupported, "name with a dot: a schema or a table")
			break
		}
	}
	sort.Strings(f.OIDColumns)
	return f, nil
}

func boolText(b bool, s string) string {
	if b {
		return s
	}
	return ""
}

// shardUsable says whether the sharding extension is installed and its catalog can be read. The extension is created with the schema pg_catalog, so extguard
// (which checks the schema of an extension) cannot decide about it; what is read is the relations of the schema kingres that the export reads, and each of them must
// be a member of the extension that a superuser owns, in a schema that a superuser owns and that no other role can create objects in.
func (f *dbFacts) shardUsable(ctx context.Context, tx pgx.Tx) (bool, error) {
	var present, ownerSuper bool
	if err := tx.QueryRow(ctx, `SELECT true, (SELECT rolsuper FROM pg_roles WHERE oid = e.extowner) FROM pg_extension e WHERE e.extname = 'kingres_shard'`).Scan(&present, &ownerSuper); err != nil {
		if err == pgx.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if !ownerSuper {
		return false, &extguard.UntrustedError{Extension: extguard.Shard, Schema: "kingres", Reason: extguard.ReasonExtensionOwner, Detail: "the extension is not owned by a superuser"}
	}
	var create bool
	var foreign *string
	err := tx.QueryRow(ctx, `SELECT
			EXISTS (SELECT 1 FROM pg_namespace n, aclexplode(coalesce(n.nspacl, acldefault('n', n.nspowner))) a
			        WHERE n.nspname = 'kingres' AND a.privilege_type = 'CREATE' AND (a.grantee = 0 OR NOT EXISTS (SELECT 1 FROM pg_roles g WHERE g.oid = a.grantee AND g.rolsuper))),
			(SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = 'kingres' AND c.relname IN ('dist_table', 'colocation', 'bucket_map', 'dist_partition', 'dist_schema')
			   AND NOT EXISTS (SELECT 1 FROM pg_depend d JOIN pg_extension m ON m.oid = d.refobjid JOIN pg_roles r ON r.oid = m.extowner
			                   WHERE d.classid = 'pg_class'::regclass AND d.objid = c.oid AND d.objsubid = 0 AND d.refclassid = 'pg_extension'::regclass
			                     AND d.deptype = 'e' AND m.extname = 'kingres_shard' AND r.rolsuper)
			 LIMIT 1)`).Scan(&create, &foreign)
	if err != nil {
		return false, err
	}
	switch {
	case create:
		return false, &extguard.UntrustedError{Extension: extguard.Shard, Schema: "kingres", Reason: extguard.ReasonSchemaCreate, Detail: "a role that is not a superuser can create objects in the schema"}
	case foreign != nil:
		return false, &extguard.UntrustedError{Extension: extguard.Shard, Schema: "kingres", Reason: extguard.ReasonForeignObject, Detail: "a relation that is read is not a member of the extension"}
	}
	return true, nil
}

// checkFixedSchemas refuses a database that has a schema of the name of the control plane's own (kingres, kingres_transfer) that does not belong to it: the
// schema kingres must be owned by a superuser (extguard decides about its objects when an extension is installed, and no object may be in it when none is), and
// the schema kingres_transfer must be owned by the service role that makes it.
func (f *dbFacts) checkFixedSchemas(ctx context.Context, tx pgx.Tx, g *extguard.Guard) error {
	anyExt, err := f.shardUsable(ctx, tx)
	if err != nil {
		return err
	}
	for _, e := range []string{extguard.Ledger, extguard.Timepart, extguard.Columnar} {
		info, err := g.Verify(ctx, e)
		if err != nil {
			return err
		}
		anyExt = anyExt || info.Present
	}
	var owner string
	var super bool
	var objects int
	err = tx.QueryRow(ctx, `SELECT pg_get_userbyid(n.nspowner)::text, (SELECT rolsuper FROM pg_roles WHERE oid = n.nspowner),
			(SELECT count(*) FROM pg_class WHERE relnamespace = n.oid) + (SELECT count(*) FROM pg_proc WHERE pronamespace = n.oid) + (SELECT count(*) FROM pg_type WHERE typnamespace = n.oid)
		FROM pg_namespace n WHERE n.nspname = 'kingres'`).Scan(&owner, &super, &objects)
	switch {
	case err == pgx.ErrNoRows:
	case err != nil:
		return err
	case !super:
		return &extguard.UntrustedError{Extension: "kingres", Schema: "kingres", Reason: extguard.ReasonSchemaOwner, Detail: "the schema kingres is owned by a role that is not a superuser"}
	case !anyExt && objects > 0:
		return &extguard.UntrustedError{Extension: "kingres", Schema: "kingres", Reason: extguard.ReasonForeignObject, Detail: "the schema kingres holds objects and no extension of KingresDB is installed"}
	}
	var ours bool
	err = tx.QueryRow(ctx, `SELECT n.nspowner = (SELECT oid FROM pg_roles WHERE rolname = current_user) FROM pg_namespace n WHERE n.nspname = 'kingres_transfer'`).Scan(&ours)
	switch {
	case err == pgx.ErrNoRows:
	case err != nil:
		return err
	case !ours:
		return &extguard.UntrustedError{Extension: "kingres", Schema: "kingres_transfer", Reason: extguard.ReasonSchemaOwner, Detail: "the schema kingres_transfer is not owned by the service role"}
	}
	return nil
}

func (f *dbFacts) readDistribution(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT c.colocation_id, c.bucket_count, c.dist_column_type::text, coalesce((SELECT collname::text FROM pg_collation WHERE oid = c.dist_collation), ''),
			(SELECT count(DISTINCT m.group_id)::int FROM kingres.bucket_map m WHERE m.colocation_id = c.colocation_id AND m.state IN ('active', 'moving'))
		FROM kingres.colocation c ORDER BY c.colocation_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var c dbColoc
		if err := rows.Scan(&c.ID, &c.Buckets, &c.Type, &c.Collation, &c.Groups); err != nil {
			rows.Close()
			return err
		}
		if c.Groups < 1 {
			c.Groups = 1
		}
		f.Colocs[c.ID] = &c
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	rows, err = tx.Query(ctx, `SELECT relid::oid::int8, method, coalesce(dist_column::text, ''), coalesce(colocation_id, 0) FROM kingres.dist_table`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var oid int64
		var method, col string
		var coloc int
		if err := rows.Scan(&oid, &method, &col, &coloc); err != nil {
			rows.Close()
			return err
		}
		if t := f.byOID[oid]; t != nil {
			t.Method, t.DistColumn, t.Coloc = method, col, coloc
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	if hasTable(ctx, tx, "kingres.dist_schema") {
		rows, err = tx.Query(ctx, `SELECT n.nspname::text, d.colocation_id FROM kingres.dist_schema d JOIN pg_namespace n ON n.oid = d.nsp::oid ORDER BY n.nspname`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s dbSchema
			if err := rows.Scan(&s.Name, &s.Coloc); err != nil {
				rows.Close()
				return err
			}
			f.Schemas = append(f.Schemas, s)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	// Every descendant of a hash table is a bucket partition or a leaf under one: it exists because of the distribution.
	rows, err = tx.Query(ctx, `WITH RECURSIVE t AS (
			SELECT i.inhrelid AS oid, dt.relid::oid AS root, 1 AS depth FROM kingres.dist_table dt JOIN pg_inherits i ON i.inhparent = dt.relid WHERE dt.method = 'hash'
			UNION ALL SELECT i.inhrelid, t.root, t.depth + 1 FROM t JOIN pg_inherits i ON i.inhparent = t.oid)
		SELECT t.oid::int8, t.root::int8, n.nspname::text || '.' || c.relname::text, c.relkind::text, t.depth,
			coalesce(pg_get_expr(c.relpartbound, c.oid), ''), CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) ELSE '' END
		FROM t JOIN pg_class c ON c.oid = t.oid JOIN pg_namespace n ON n.oid = c.relnamespace ORDER BY n.nspname, c.relname`)
	if err != nil {
		return err
	}
	type bucketRel struct {
		dbBucketRel
		depth int
	}
	var rels []bucketRel
	for rows.Next() {
		var b bucketRel
		if err := rows.Scan(&b.OID, &b.Root, &b.Name, &b.Relkind, &b.depth, &b.Bound, &b.KeyDef); err != nil {
			rows.Close()
			return err
		}
		rels = append(rels, b)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, b := range rels {
		f.Buckets[b.OID] = true
		f.BucketNames = append(f.BucketNames, b.Name)
		if b.depth == 1 && b.KeyDef != "" {
			if _, ok := f.PartKey[b.Root]; !ok {
				f.PartKey[b.Root] = b.KeyDef
			}
		}
	}
	// The indexes (and constraints, which carry their names) of those relations.
	rows, err = tx.Query(ctx, `WITH RECURSIVE t AS (
			SELECT i.inhrelid AS oid FROM kingres.dist_table dt JOIN pg_inherits i ON i.inhparent = dt.relid WHERE dt.method = 'hash'
			UNION ALL SELECT i.inhrelid FROM t JOIN pg_inherits i ON i.inhparent = t.oid)
		SELECT n.nspname::text || '.' || ic.relname::text FROM t JOIN pg_index x ON x.indrelid = t.oid JOIN pg_class ic ON ic.oid = x.indexrelid JOIN pg_namespace n ON n.oid = ic.relnamespace`)
	if err != nil {
		return err
	}
	f.BucketIdx, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	// The user partitions of distributed tables: the name from the catalog, the bound as PostgreSQL writes it (from a leaf of the first bucket that this
	// server holds), so that a bound with a time zone is exact.
	if hasTable(ctx, tx, "kingres.dist_partition") {
		rows, err = tx.Query(ctx, `SELECT p.parent::oid::int8, p.partition_name::text,
				coalesce((SELECT pg_get_expr(l.relpartbound, l.oid) FROM pg_class l
				          WHERE l.relnamespace = (SELECT relnamespace FROM pg_class WHERE oid = p.parent::oid) AND l.relispartition
				            AND starts_with(l.relname::text, p.partition_name::text || '_b') AND length(l.relname::text) = length(p.partition_name::text) + 6
				          ORDER BY l.relname LIMIT 1), '')
			FROM kingres.dist_partition p ORDER BY p.parent::oid, p.created_at, p.partition_name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var parent int64
			var name, bound string
			if err := rows.Scan(&parent, &name, &bound); err != nil {
				rows.Close()
				return err
			}
			bound = strings.TrimPrefix(bound, "FOR VALUES ")
			f.UserPart[parent] = append(f.UserPart[parent], dbPartition{Name: name, Bound: bound})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (f *dbFacts) readLedger(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT relid::oid::int8, mode, signature_policy, coalesce(history_relid::oid::int8, 0) FROM kingres.ledger_table ORDER BY table_id`)
	if err != nil {
		return err
	}
	type lt struct {
		rel     int64
		mode    string
		policy  string
		history int64
	}
	var all []lt
	for rows.Next() {
		var l lt
		if err := rows.Scan(&l.rel, &l.mode, &l.policy, &l.history); err != nil {
			rows.Close()
			return err
		}
		all = append(all, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var height int64
	var hash string
	if hasTable(ctx, tx, "kingres.ledger_blocks") {
		// The head is read without writing: no digest is generated (it would add a row to the source).
		err := tx.QueryRow(ctx, `SELECT height, encode(block_hash, 'hex') FROM kingres.ledger_blocks ORDER BY height DESC LIMIT 1`).Scan(&height, &hash)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
	}
	for _, l := range all {
		t := f.byOID[l.rel]
		if t == nil {
			continue
		}
		mode := map[string]string{"append_only": "appendOnly", "updatable": "updatable"}[l.mode]
		lg := &dbLedger{Mode: mode, Policy: l.policy, HeadHeight: height, HeadHash: hash}
		if h := f.byOID[l.history]; h != nil {
			lg.HistoryOID, lg.HistoryName, lg.HistoryCols = h.OID, h.qualified(), append([]string{}, h.Cols...)
		}
		t.Ledger = lg
	}
	return nil
}

func (f *dbFacts) readPolicies(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT schema_name::text, table_name::text, partition_every::text, timezone, create_ahead::text, coalesce(retention::text, ''), retention_action,
			coalesce(columnar_after::text, ''), coalesce(array_to_string(columnar_sort, ','), ''), coalesce(columnar_options::text, ''), run_every::text, enabled
		FROM kingres.timepart_policy_data ORDER BY schema_name, table_name`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var p dbPolicy
		var opts string
		if err := rows.Scan(&p.Schema, &p.Table, &p.Every, &p.Zone, &p.Ahead, &p.Retention, &p.Action, &p.ColAfter, &p.ColSort, &opts, &p.Run, &p.Enabled); err != nil {
			rows.Close()
			return err
		}
		p.ColOpts = jsonStrings(opts)
		f.Policies = append(f.Policies, p)
	}
	rows.Close()
	return rows.Err()
}

func (f *dbFacts) readColumnar(ctx context.Context, tx pgx.Tx) error {
	// The options are the ones the extension reports for each columnar table; the function needs SELECT, which the service role has.
	for i := range f.Tables {
		t := &f.Tables[i]
		if t.AM != "columnar" {
			continue
		}
		var comp string
		var level, stripe, chunk int
		err := tx.QueryRow(ctx, `SELECT compression, compression_level, stripe_row_count, chunk_row_count FROM kingres.columnar_options($1::oid::regclass)`, t.OID).Scan(&comp, &level, &stripe, &chunk)
		if err != nil {
			if err == pgx.ErrNoRows {
				continue
			}
			return err
		}
		f.colOpts(t.OID, map[string]string{"compression": comp, "compressionLevel": fmt.Sprint(level), "stripeRowCount": fmt.Sprint(stripe), "chunkRowCount": fmt.Sprint(chunk)})
	}
	return nil
}

// colOpts records the options of a columnar table.
func (f *dbFacts) colOpts(oid int64, m map[string]string) {
	if f.columnar == nil {
		f.columnar = map[int64]map[string]string{}
	}
	f.columnar[oid] = m
}

func (f *dbFacts) readSequences(ctx context.Context, tx pgx.Tx) error {
	rows, err := tx.Query(ctx, `SELECT sn.nspname::text, s.relname::text, tn.nspname::text || '.' || t.relname::text || '.' || a.attname::text,
			coalesce(pg_sequence_last_value(s.oid), 0)::int8, pg_sequence_last_value(s.oid) IS NOT NULL
		FROM pg_class s JOIN pg_namespace sn ON sn.oid = s.relnamespace
		JOIN pg_depend d ON d.classid = 'pg_class'::regclass AND d.objid = s.oid AND d.refclassid = 'pg_class'::regclass AND d.deptype IN ('a', 'i')
		JOIN pg_class t ON t.oid = d.refobjid JOIN pg_namespace tn ON tn.oid = t.relnamespace
		JOIN pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		WHERE s.relkind = 'S' AND `+strings.ReplaceAll(userSchemaFilter, "n.nspname", "sn.nspname")+`
		ORDER BY sn.nspname, s.relname`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s dbSeq
		if err := rows.Scan(&s.Schema, &s.Name, &s.Owned, &s.Last, &s.Called); err != nil {
			rows.Close()
			return err
		}
		f.Seqs = append(f.Seqs, s)
	}
	rows.Close()
	return rows.Err()
}

// readUnsupported lists the kinds of objects that the archive does not carry and that the database has, each with one name. pg_dump would write most of
// them and the shape grammar would refuse them by kind; a few it leaves out by its own options, and those are named here so that an archive that looks
// complete and is not is never made (design "What the archive does not carry").
func (f *dbFacts) readUnsupported(ctx context.Context, tx pgx.Tx) error {
	q := func(kind, sql string) error {
		var name *string
		err := tx.QueryRow(ctx, sql).Scan(&name)
		if err != nil && err != pgx.ErrNoRows {
			return err
		}
		if name != nil {
			f.Unsupported = append(f.Unsupported, kind+": "+*name)
		}
		return nil
	}
	ext := `NOT EXISTS (SELECT 1 FROM pg_depend d WHERE d.classid = '%s'::regclass AND d.objid = %s AND d.deptype = 'e')`
	for _, s := range []struct{ kind, sql string }{
		{"function", `SELECT n.nspname::text || '.' || p.proname::text FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE ` + userSchemaFilter + ` AND ` + fmt.Sprintf(ext, "pg_proc", "p.oid") + ` LIMIT 1`},
		{"trigger", `SELECT c.relname::text || '.' || t.tgname::text FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE NOT t.tgisinternal AND t.tgname NOT LIKE 'kingres\_ledger\_%' AND ` + userSchemaFilter + ` LIMIT 1`},
		{"policy", `SELECT c.relname::text || '.' || p.polname::text FROM pg_policy p JOIN pg_class c ON c.oid = p.polrelid LIMIT 1`},
		{"rule", `SELECT c.relname::text || '.' || r.rulename::text FROM pg_rewrite r JOIN pg_class c ON c.oid = r.ev_class JOIN pg_namespace n ON n.oid = c.relnamespace
			WHERE r.rulename <> '_RETURN' AND ` + userSchemaFilter + ` AND ` + fmt.Sprintf(ext, "pg_class", "c.oid") + ` LIMIT 1`},
		{"event trigger", `SELECT e.evtname::text FROM pg_event_trigger e WHERE ` + fmt.Sprintf(ext, "pg_event_trigger", "e.oid") + ` LIMIT 1`},
		{"publication", `SELECT pubname::text FROM pg_publication LIMIT 1`},
		{"extended statistics", `SELECT s.stxname::text FROM pg_statistic_ext s JOIN pg_namespace n ON n.oid = s.stxnamespace WHERE ` + userSchemaFilter + ` LIMIT 1`},
		{"foreign server", `SELECT srvname::text FROM pg_foreign_server LIMIT 1`},
		{"foreign table", `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relkind = 'f' AND ` + userSchemaFilter + ` LIMIT 1`},
		{"collation", `SELECT n.nspname::text || '.' || c.collname::text FROM pg_collation c JOIN pg_namespace n ON n.oid = c.collnamespace WHERE ` + userSchemaFilter + ` AND ` + fmt.Sprintf(ext, "pg_collation", "c.oid") + ` LIMIT 1`},
		{"unlogged table", `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relpersistence = 'u' AND c.relkind IN ('r', 'p') AND ` + userSchemaFilter + ` LIMIT 1`},
		{"operator", `SELECT o.oprname::text FROM pg_operator o JOIN pg_namespace n ON n.oid = o.oprnamespace WHERE ` + userSchemaFilter + ` AND ` + fmt.Sprintf(ext, "pg_operator", "o.oid") + ` LIMIT 1`},
		{"row-level security", `SELECT c.relname::text FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE c.relrowsecurity AND ` + userSchemaFilter + ` LIMIT 1`},
	} {
		if err := q(s.kind, s.sql); err != nil {
			return err
		}
	}
	return nil
}

func jsonStrings(s string) map[string]string {
	if s == "" {
		return nil
	}
	m := map[string]any{}
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// fingerprint is the SHA-256 of what the structure is: the tables with their columns and types, how each is distributed, the colocation relations and the
// access methods. A column added while the export runs changes it.
func (f *dbFacts) fingerprint() string {
	h := sha256.New()
	for _, t := range f.Tables {
		if f.Buckets[t.OID] {
			continue
		}
		fmt.Fprintf(h, "T|%s|%s|%s|%s|%s|%d|%s|%d\n", t.qualified(), t.Relkind, t.AM, t.Method, t.DistColumn, t.Coloc, strings.Join(t.All, ","), t.Parent)
	}
	ids := make([]int, 0, len(f.Colocs))
	for id := range f.Colocs {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		c := f.Colocs[id]
		fmt.Fprintf(h, "C|%d|%d|%s|%s\n", c.ID, c.Buckets, c.Type, c.Collation)
	}
	return hex.EncodeToString(h.Sum(nil))
}
