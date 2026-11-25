// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/transfer"
)

// This file is the part of the import that runs as the service role: the distribution, the sequences, the ledger and the policies, the verification and the final
// name. Nothing here runs text of the archive. Every call is made from the structured catalog document with identifiers that passed sqlx; the importing role
// cannot connect while it runs (importAccess withdraws CONNECT first); and before the service role touches a table that the role made, the catalog says that the
// table holds nothing that runs code (tableTrusted).

// The kinds of ApplyLocal of the import: each acts on the private database of the server that receives it, with parameters that are structured values.
const (
	// ApplyImportCreate: make the private database (it must not exist), install the schemas and the extensions of the closed list.
	ApplyImportCreate = "import.create"
	// ApplyImportAccess: give the importing role access to the private database, or take it away and end its sessions.
	ApplyImportAccess = "import.access"
	// ApplyImportSequences: raise the sequences of the private database to the given values.
	ApplyImportSequences = "import.sequences"
	// ApplyImportRename: take the final name (the database stays closed and owned by the service role), open it to its owner after the last check, put the name back, or end it.
	ApplyImportRename = "import.rename"
	// ApplyImportDrop: drop the private database of a task that failed, if this server made it for that task.
	ApplyImportDrop = "import.drop"
)

type importDropOp struct {
	Name string `json:"name"`
}

type importSeqItem struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Value  int64  `json:"value"`
	// Table and Column are the column that owns the sequence, as the catalog of the archive says; the server checks that the sequence is linked to it.
	Table  string `json:"table"`
	Column string `json:"column"`
}

type importSeqOp struct {
	Name  string          `json:"name"`
	Items []importSeqItem `json:"items"`
	// Owner is the importing role: a sequence that another role owns (the sequences of the extensions among them) is never touched.
	Owner string `json:"owner"`
	// Full is set on the server that holds the coordinator, where the link of every sequence to its column can be checked.
	Full bool `json:"full,omitempty"`
}

type importRenameOp struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Owner string `json:"owner"`
	// Back undoes a rename (a later server failed): the final name goes back to the private one, the owner back to the service role, the marker back and PUBLIC
	// loses CONNECT again, so that the database is as the failure hook expects it.
	Back   bool   `json:"back,omitempty"`
	Marker string `json:"marker,omitempty"`
	// Finish ends the rename once every server has taken the final name: the marker of the task, which stays on the final name until then, is removed. From is the final name.
	Finish bool `json:"finish,omitempty"`
	// Open opens the renamed database to its clients once every server has the final name and the catalogs were compared again: the importing role becomes the
	// owner and PUBLIC may connect. From is the final name, To the private one (the marker is derived from it).
	Open bool `json:"open,omitempty"`
}

// applyImportKind runs the kinds of this file for ApplyLocal; it reports whether the kind was one of them.
func (o *Ops) applyImportKind(ctx context.Context, kind string, raw json.RawMessage) (bool, error) {
	switch kind {
	case ApplyImportCreate:
		var p importCreateOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		if err := o.createPrivateLocal(ctx, p); err != nil {
			return true, err
		}
		return true, o.setupPrivateLocal(ctx, p)
	case ApplyImportAccess:
		var p importAccessOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		return true, o.importAccessLocal(ctx, p)
	case ApplyImportSequences:
		var p importSeqOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		return true, o.importSequencesLocal(ctx, p)
	case ApplyImportDrop:
		var p importDropOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		return true, o.importDropLocal(ctx, p.Name)
	case ApplyImportRename:
		var p importRenameOp
		if err := strictJSON(raw, &p); err != nil {
			return true, err
		}
		return true, o.importRenameLocal(ctx, p)
	}
	return false, nil
}

// ------------------------------------------------------------------ what the service role may touch

// tableTrusted checks, from the catalog alone (no row and no code of the table is read), that a table that the importing role made holds nothing that runs code
// when the service role reads or changes it: it is an ordinary or partitioned table owned by the role, it has no trigger of its own, no rule, no policy and no row-level
// security, and every function that a default, a generated column, a constraint or an index of it calls is allowed in an expression.
func tableTrusted(ctx context.Context, tx pgx.Tx, schema, table, owner string, extSchema map[string]string) error {
	// The deparsed form of an expression depends on the search path: a builtin function is written bare and every other one with its schema.
	if _, err := tx.Exec(ctx, "SET LOCAL search_path = pg_catalog"); err != nil {
		return err
	}
	var kind *string
	var ownerOK, trig, rule, pol, rls bool
	err := tx.QueryRow(ctx, `SELECT c.relkind::pg_catalog.text, pg_catalog.pg_get_userbyid(c.relowner) = $3::pg_catalog.text,
			EXISTS (SELECT 1 FROM pg_catalog.pg_trigger t WHERE t.tgrelid = c.oid AND NOT t.tgisinternal
			        AND NOT EXISTS (SELECT 1 FROM pg_catalog.pg_depend d3 JOIN pg_catalog.pg_extension e3 ON e3.oid = d3.refobjid
			                        WHERE d3.classid = 'pg_catalog.pg_proc'::pg_catalog.regclass AND d3.objid = t.tgfoid AND d3.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass
			                          AND d3.deptype = 'e' AND e3.extname = 'kingres_ledger' AND pg_catalog.pg_get_userbyid(e3.extowner) IN (SELECT rolname FROM pg_catalog.pg_roles WHERE rolsuper))),
			EXISTS (SELECT 1 FROM pg_catalog.pg_rewrite r WHERE r.ev_class = c.oid),
			EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid = c.oid), c.relrowsecurity
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text`,
		schema, table, owner).Scan(&kind, &ownerOK, &trig, &rule, &pol, &rls)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("table %s.%s does not exist", schema, table)
	}
	if err != nil {
		return err
	}
	switch {
	case kind == nil || (*kind != "r" && *kind != "p"):
		return fmt.Errorf("%s.%s is not an ordinary table", schema, table)
	case !ownerOK:
		return fmt.Errorf("%s.%s is not owned by the importing role", schema, table)
	case trig || rule || pol || rls:
		return fmt.Errorf("%s.%s has a trigger, a rule, a policy or row-level security", schema, table)
	}
	// The catalog walk: every object that carries code and is reached from the table, its columns, constraints, indexes, types (domains and their checks included),
	// statistics, triggers and partitions must be owned by a superuser or belong to an extension of the closed list.
	rel := qualifiedText(schema, table)
	if what, err := reachFinding(ctx, tx, rel); err != nil {
		return err
	} else if what != "" {
		return fmt.Errorf("%s.%s reaches %s, which carries code that is not trusted", schema, table, what)
	}
	// The expressions of the table, as the catalog deparses them with the search path of this session.
	rows, err := tx.Query(ctx, `SELECT pg_catalog.pg_get_expr(ad.adbin, ad.adrelid) FROM pg_catalog.pg_attrdef ad WHERE ad.adrelid = $1::pg_catalog.text::pg_catalog.regclass
		UNION ALL SELECT pg_catalog.pg_get_constraintdef(c.oid) FROM pg_catalog.pg_constraint c WHERE c.conrelid = $1::pg_catalog.text::pg_catalog.regclass
		UNION ALL SELECT pg_catalog.pg_get_indexdef(i.indexrelid) FROM pg_catalog.pg_index i WHERE i.indrelid = $1::pg_catalog.text::pg_catalog.regclass`, rel)
	if err != nil {
		return err
	}
	var texts []string
	for rows.Next() {
		var x *string
		if err := rows.Scan(&x); err != nil {
			rows.Close()
			return err
		}
		if x != nil {
			texts = append(texts, *x)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	seen := map[string]bool{}
	var names []string
	for _, x := range texts {
		for _, n := range exprFunctionNames(x) {
			if !seen[n] {
				seen[n] = true
				names = append(names, n)
			}
		}
	}
	refused, err := checkFunctions(ctx, tx, names, extSchema)
	if err != nil {
		return err
	}
	if len(refused) > 0 {
		return fmt.Errorf("%s.%s calls a function that is not allowed (%s)", schema, table, refused[0])
	}
	return nil
}

// untrustedError is the task error for a table that the service role may not touch.
func untrustedError(err error) error {
	return transfer.Errorf(transfer.CodeTableNotEligible, false, "the service role does not touch a table that the importing role made unless the catalog shows that it holds no code (%s)", safeSentence(err.Error()))
}

// serviceConnFor opens a service connection to the private database with the search path pinned.
func (o *Ops) serviceConnFor(ctx context.Context, priv string) (*pgx.Conn, error) {
	c, err := o.Service.ServiceConn(ctx, priv)
	if err != nil {
		return nil, transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to the private database now; repeat the operation")
	}
	if _, err := c.Exec(ctx, "SET search_path = pg_catalog"); err != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, transfer.SafeError(err)
	}
	return c, nil
}

// extensionsToRequire is the list requireExtensions verifies: the sharding extension first when the archive has it, then the others.
func extensionsToRequire(shard bool, exts []string) []string {
	if shard {
		return append([]string{extguard.Shard}, exts...)
	}
	return exts
}

// requireExtensions verifies the schemas of the extensions that the next calls use, as extguard does for every read of the schema kingres on a service session.
func requireExtensions(ctx context.Context, c *pgx.Conn, shard bool, exts ...string) error {
	g := extguard.NewGuard(c)
	for _, e := range extensionsToRequire(shard, exts) {
		if _, err := g.Require(ctx, e); err != nil {
			if errors.Is(err, extguard.ErrAbsent) {
				return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the extension %s is not installed in the private database", e)
			}
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the schema of %s in the private database cannot be trusted", e)
		}
	}
	return nil
}

// ------------------------------------------------------------------ step: distribute

func (o *Ops) stepIMDistribute(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	priv := p.private(env.Task.ID)
	// The service role works alone from here until the role is given access again.
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	res := a.st.res
	cat := res.Catalog
	extSchema := extensionSchemas(res.Manifest)
	c, err := o.serviceConnFor(ctx, priv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	sharded := hasExt(res.Manifest, extguard.Shard)
	// Which tables are distributed comes from the catalog, and the calls below reach the schema kingres: the extension must be in the manifest (and is then
	// verified by requireExtensions) before any statement that names it is run.
	if cat.NeedsShard() && !sharded {
		return transfer.Errorf(transfer.CodeArchiveInvalid, false, "the catalog has distributed tables and the manifest does not list kingres_shard")
	}
	var needColumnar bool
	for _, ct := range cat.Tables {
		if len(ct.Columnar) > 0 {
			needColumnar = true
		}
	}
	var guards []string
	if needColumnar {
		guards = append(guards, extguard.Columnar)
	}
	if err := requireExtensions(ctx, c, sharded, guards...); err != nil {
		return err
	}
	groups := map[string]archive.ColocationGroup{}
	for _, g := range cat.ColocationGroups {
		groups[g.Ref] = g
	}
	isDist := func(rel string) (bool, error) {
		var done bool
		err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_table WHERE relid = $1::pg_catalog.text::pg_catalog.regclass)`, rel).Scan(&done)
		return done, err
	}
	// call runs f in a transaction that holds a lock on the table and has checked it (lockTrusted): what was checked is what is acted on.
	call := func(schema, table string, f func(tx pgx.Tx) error) error {
		err := withTrustedTable(ctx, c, schema, table, p.Owner.Name, extSchema, f)
		if err == nil {
			return nil
		}
		var u *untrustedErr
		if errors.As(err, &u) {
			return untrustedError(u.err)
		}
		return o.distError(err, schema, table)
	}
	n := 0
	// Distributed schemas: the schema and its tables move to a group while the tables are empty.
	for _, ds := range cat.DistributedSchemas {
		id, err := sqlx.NewIdent(ds.Schema)
		if err != nil {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a distributed schema has a name that is not valid")
		}
		var done bool
		if err := c.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.dist_schema d JOIN pg_catalog.pg_namespace n ON n.oid = d.nsp::pg_catalog.oid WHERE n.nspname = $1::pg_catalog.text)`, ds.Schema).Scan(&done); err != nil {
			return transfer.SafeError(err)
		}
		if done {
			continue
		}
		// Every relation of the schema is listed, locked and verified, in one transaction, before the extension reads or moves it; a table that the catalog of
		// the archive does not name is refused.
		known := map[string]bool{}
		for _, ct := range cat.Tables {
			if ct.Schema == ds.Schema {
				known[ct.Name] = true
			}
		}
		if err := inTx(ctx, c, func(tx pgx.Tx) error {
			if err := lockSchemaTables(ctx, tx, ds.Schema, known, p.Owner.Name, extSchema); err != nil {
				return err
			}
			_, e := tx.Exec(ctx, `SELECT kingres.distribute_schema($1::pg_catalog.text::pg_catalog.regnamespace, NULL::pg_catalog.int4)`, id.String())
			return e
		}); err != nil {
			var u *untrustedErr
			if errors.As(err, &u) {
				return untrustedError(u.err)
			}
			return o.distError(err, ds.Schema, "")
		}
		n++
	}
	first := map[string]string{}  // colocation ref -> the first table that made it
	colocID := map[string]int64{} // colocation ref -> its id here
	for _, ct := range cat.Tables {
		if ct.TreeRole == "leaf" {
			continue
		}
		rel := qualifiedText(ct.Schema, ct.Name)
		if rel == "" {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a table has a name that is not valid")
		}
		switch ct.Kind {
		case "reference":
			if done, err := isDist(rel); err != nil {
				return transfer.SafeError(err)
			} else if !done {
				if err := call(ct.Schema, ct.Name, func(tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `SELECT kingres.create_reference_table($1::pg_catalog.text::pg_catalog.regclass)`, rel)
					return e
				}); err != nil {
					return err
				}
				n++
			}
		case "hash":
			col, cerr := sqlx.NewIdent(ct.DistributionColumn)
			if cerr != nil || ct.ColocationRef == "" {
				return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a distributed table has no usable distribution column or colocation group")
			}
			if done, err := isDist(rel); err != nil {
				return transfer.SafeError(err)
			} else if !done {
				var coloc *string
				var buckets *int
				fr, haveFirst := first[ct.ColocationRef]
				if haveFirst {
					coloc = &fr
				} else if g, ok := groups[ct.ColocationRef]; ok && g.BucketCount > 0 && g.BucketCount <= archive.MaxBuckets {
					b := g.BucketCount
					buckets = &b
				}
				if err := call(ct.Schema, ct.Name, func(tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `SELECT kingres.create_distributed_table($1::pg_catalog.text::pg_catalog.regclass, $2::pg_catalog.name, $3::pg_catalog.text::pg_catalog.regclass, $4::pg_catalog.int4)`, rel, col.Name(), coloc, buckets)
					return e
				}); err != nil {
					return err
				}
				n++
			}
			var id int64
			if err := c.QueryRow(ctx, `SELECT colocation_id FROM kingres.dist_table WHERE relid = $1::pg_catalog.text::pg_catalog.regclass`, rel).Scan(&id); err != nil {
				return transfer.SafeError(err)
			}
			if _, ok := first[ct.ColocationRef]; !ok {
				// Two groups of the source that have the same shape would join into one here; a group that the source kept apart is kept apart.
				for ref, other := range colocID {
					if other == id && ref != ct.ColocationRef {
						if err := call(ct.Schema, ct.Name, func(tx pgx.Tx) error {
							_, e := tx.Exec(ctx, `SELECT kingres.alter_distributed_table($1::pg_catalog.text::pg_catalog.regclass, NULL, NULL, 'none', false)`, rel)
							return e
						}); err != nil {
							return err
						}
						if err := c.QueryRow(ctx, `SELECT colocation_id FROM kingres.dist_table WHERE relid = $1::pg_catalog.text::pg_catalog.regclass`, rel).Scan(&id); err != nil {
							return transfer.SafeError(err)
						}
						break
					}
				}
				first[ct.ColocationRef] = rel
				colocID[ct.ColocationRef] = id
			}
		}
		if len(ct.Columnar) > 0 && ct.AccessMethod == "columnar" {
			if err := o.setColumnarOptions(ctx, c, ct, p.Owner.Name, extSchema); err != nil {
				return err
			}
		}
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d calls", n))
}

// distError maps the refusal of the sharding extension to the code of the task; the text of the server is never passed on.
func (o *Ops) distError(err error, schema, table string) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (pe.Code == "0A000" || pe.Code == "55000" || pe.Code == "22023" || strings.HasPrefix(pe.Code, "KS")) {
		return (&transfer.Error{Code: transfer.CodeTableNotEligible, Sentence: "the sharding extension refuses to distribute the table (" + pe.Code + ")"}).ForTarget(schema, table)
	}
	return transfer.SafeError(err).ForTarget(schema, table)
}

var columnarKeys = map[string][2]int{"compressionLevel": {1, 19}, "stripeRowCount": {1000, 10000000}, "chunkRowCount": {100, 100000}}

func (o *Ops) setColumnarOptions(ctx context.Context, c *pgx.Conn, ct archive.CatalogTable, owner string, extSchema map[string]string) error {
	var comp *string
	ints := map[string]*int{}
	for k, v := range ct.Columnar {
		switch k {
		case "compression":
			if !compressionRE.MatchString(v) {
				return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a columnar option is not valid")
			}
			s := v
			comp = &s
		case "compressionLevel", "stripeRowCount", "chunkRowCount":
			n, err := strconv.Atoi(v)
			r := columnarKeys[k]
			if err != nil || n < r[0] || n > r[1] {
				return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a columnar option is out of range")
			}
			ints[k] = &n
		default:
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a columnar option is not known")
		}
	}
	rel := qualifiedText(ct.Schema, ct.Name)
	if err := withTrustedTable(ctx, c, ct.Schema, ct.Name, owner, extSchema, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT kingres.columnar_set_options($1::pg_catalog.text::pg_catalog.regclass, $2, $3, $4, $5)`, rel, comp, ints["compressionLevel"], ints["stripeRowCount"], ints["chunkRowCount"])
		return e
	}); err != nil {
		return wrapTrust(err, ct.Schema, ct.Name)
	}
	return nil
}

// ------------------------------------------------------------------ step: the sequences

func (o *Ops) stepIMSequences(ctx context.Context, env *task.Env) error {
	p, a, conn, priv, err := o.openForRole(ctx, env)
	if err != nil {
		return err
	}
	defer a.close()
	// The largest value of the owning column is read as the role, through the router for a distributed table: the service role reads no table of the role.
	var items []importSeqItem
	for _, s := range a.st.res.Catalog.Sequences {
		parts := strings.Split(s.OwnedBy, ".")
		if len(parts) != 3 {
			_ = conn.Close(context.WithoutCancel(ctx))
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a sequence has an owner that is not understood")
		}
		tbl, terr := sqlx.QualifiedIdent(parts[0], parts[1])
		col, cerr := sqlx.NewIdent(parts[2])
		if terr != nil || cerr != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a sequence has an owner that is not valid")
		}
		var max *int64
		if err := conn.QueryRow(ctx, "SELECT pg_catalog.max("+col.String()+")::pg_catalog.int8 FROM "+tbl.String()).Scan(&max); err != nil {
			_ = conn.Close(context.WithoutCancel(ctx))
			return transfer.SafeError(err).ForTarget(parts[0], parts[1])
		}
		v := int64(0)
		if max != nil {
			v = *max
		}
		if s.IsCalled && s.LastValue > v {
			v = s.LastValue
		}
		if v > 0 {
			items = append(items, importSeqItem{Schema: s.Schema, Name: s.Name, Value: v, Table: parts[0] + "." + parts[1], Column: parts[2]})
		}
	}
	// The rows of every table, counted as the role (the service role scans no table of the role): the rows must equal the manifest's.
	if bad := o.countRows(ctx, conn, a); bad != "" {
		_ = conn.Close(context.WithoutCancel(ctx))
		return transfer.Errorf(transfer.CodeVerification, false, "the rows do not equal the manifest's: %s", bad)
	}
	_ = conn.Close(context.WithoutCancel(ctx))
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	op := importSeqOp{Name: priv, Items: items, Owner: p.Owner.Name, Full: true}
	if err := o.importSequencesLocal(ctx, op); err != nil {
		return transfer.SafeError(err)
	}
	op.Full = false
	for _, s := range p.Servers {
		s := s
		if o.Fanout == nil {
			break
		}
		if err := o.onServer(ctx, env, s, "set the sequences", func() error { return o.Fanout.Apply(ctx, s, ApplyImportSequences, op) }); err != nil {
			return err
		}
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d sequences", len(items)))
}

// protectedSequenceSchema says that a schema is the system's or the extensions' (kingres and kingres_*, pg_*, information_schema): no sequence of it is set.
func protectedSequenceSchema(s string) bool {
	l := strings.ToLower(s)
	return l == "kingres" || strings.HasPrefix(l, "kingres_") || strings.HasPrefix(l, "pg_") || l == "information_schema"
}

// raiseSequence sets one sequence, in ONE statement that holds the conditions: it is a sequence of a schema that is not the system's or an extension's, the importing
// role owns it, and (where full is set) it is linked, as an owned or an identity sequence, to the column that the catalog of the archive names. A sequence that is
// refused is an error on the server that holds the coordinator (full), and is skipped elsewhere (the node may not have the sequence). setval runs no code.
func raiseSequence(ctx context.Context, c *pgx.Conn, it importSeqItem, owner string, full bool) error {
	if _, err := sqlx.QualifiedIdent(it.Schema, it.Name); err != nil || protectedSequenceSchema(it.Schema) {
		return errors.New("a sequence is refused: its schema is not one that an import may change")
	}
	ts, tn, ok := splitQualified(it.Table)
	if !ok {
		return errors.New("a sequence is refused: its column is not named")
	}
	tag, err := c.Exec(ctx, `SELECT pg_catalog.setval(c.oid, GREATEST($3::pg_catalog.int8, coalesce(pg_catalog.pg_sequence_last_value(c.oid), 0)), true)
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text AND c.relkind = 'S'
		  AND n.nspname !~ '^(pg_|kingres$|kingres_|information_schema$)' AND pg_catalog.pg_get_userbyid(c.relowner) = $4::pg_catalog.text
		  AND ($5::pg_catalog.bool = false OR EXISTS (SELECT 1 FROM pg_catalog.pg_depend d JOIN pg_catalog.pg_class t ON t.oid = d.refobjid JOIN pg_catalog.pg_namespace tn ON tn.oid = t.relnamespace
		        JOIN pg_catalog.pg_attribute a ON a.attrelid = t.oid AND a.attnum = d.refobjsubid
		        WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid AND d.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.deptype IN ('a', 'i')
		          AND tn.nspname = $6::pg_catalog.text AND t.relname = $7::pg_catalog.text AND a.attname = $8::pg_catalog.text))`,
		it.Schema, it.Name, it.Value, owner, full, ts, tn, it.Column)
	if err != nil {
		return err
	}
	if full && tag.RowsAffected() != 1 {
		return errors.New("a sequence is refused: it is not owned by the importing role, or not linked to the column that the archive names")
	}
	return nil
}

// countRows counts the rows of every data table as the role, and says what is wrong ("" when every count is the manifest's).
func (o *Ops) countRows(ctx context.Context, conn *pgx.Conn, a *archiveFile) string {
	byTable := map[string]archive.CatalogTable{}
	for _, ct := range a.st.res.Catalog.Tables {
		byTable[ct.Qualified()] = ct
	}
	var bad []string
	for _, me := range dataEntriesOf(a.st.res.Manifest) {
		ct := byTable[me.Table]
		q := "SELECT pg_catalog.count(*) FROM "
		if ct.TreeRole == "inheritanceParent" {
			q += "ONLY "
		}
		var n int64
		if err := conn.QueryRow(ctx, q+qualifiedText(ct.Schema, ct.Name)).Scan(&n); err != nil {
			bad = append(bad, safeName(me.Table)+" could not be counted")
		} else if n != me.Rows {
			bad = append(bad, fmt.Sprintf("%s has %d rows and the manifest says %d", safeName(me.Table), n, me.Rows))
		}
		if len(bad) >= 5 {
			break
		}
	}
	return strings.Join(bad, "; ")
}

// importSequencesLocal raises the sequences to the values: setval(seq, greatest(value, the state here)). A sequence that this server does not have is skipped.
func (o *Ops) importSequencesLocal(ctx context.Context, op importSeqOp) error {
	if !strings.HasPrefix(op.Name, privatePrefix) || !dbNameRE.MatchString(op.Name) {
		return errors.New("this database cannot be changed")
	}
	c, err := o.Service.ServiceConn(ctx, op.Name)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if _, err := c.Exec(ctx, "SET search_path = pg_catalog"); err != nil {
		return err
	}
	if _, err := sqlx.NewIdent(op.Owner); err != nil {
		return errors.New("the importing role is not valid")
	}
	for _, it := range op.Items {
		if it.Value <= 0 {
			continue
		}
		if err := raiseSequence(ctx, c, it, op.Owner, op.Full); err != nil {
			return err
		}
	}
	return nil
}

// ------------------------------------------------------------------ step: ledger and policies

var compressionRE = regexp.MustCompile(`^[a-z0-9]{1,16}$`)

func (o *Ops) stepIMLedger(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	priv := p.private(env.Task.ID)
	res := a.st.res
	cat := res.Catalog
	var ledgers []archive.CatalogTable
	for _, ct := range cat.Tables {
		if ct.Ledger != nil {
			ledgers = append(ledgers, ct)
		}
	}
	if len(ledgers) == 0 && len(cat.TimepartPolicies) == 0 {
		return env.SetDetail(ctx, "nothing to enable")
	}
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	extSchema := extensionSchemas(res.Manifest)
	c, err := o.serviceConnFor(ctx, priv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var need []string
	if len(ledgers) > 0 {
		need = append(need, extguard.Ledger)
	}
	if len(cat.TimepartPolicies) > 0 {
		need = append(need, extguard.Timepart)
	}
	if err := requireExtensions(ctx, c, hasExt(res.Manifest, extguard.Shard), need...); err != nil {
		return err
	}
	locked := func(schema, table string, f func(tx pgx.Tx) error) error {
		return wrapTrust(withTrustedTable(ctx, c, schema, table, p.Owner.Name, extSchema, f), schema, table)
	}
	var notes []string
	// The rows were loaded first; ledger_enable enters them as the first entries of a new chain.
	for _, ct := range ledgers {
		mode := map[string]string{"appendOnly": "append_only", "updatable": "updatable"}[ct.Ledger.Mode]
		if mode == "" {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a ledger table has a mode that is not known")
		}
		if err := locked(ct.Schema, ct.Name, func(tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `SELECT kingres.ledger_enable($1::pg_catalog.text::pg_catalog.regclass, $2::pg_catalog.text)`, qualifiedText(ct.Schema, ct.Name), mode)
			return e
		}); err != nil {
			return err
		}
		notes = append(notes, fmt.Sprintf("ledger: %s.%s starts a new chain (the source had its head at height %d)", ct.Schema, ct.Name, ct.Ledger.HeadHeight))
	}
	if len(ledgers) > 0 {
		findings, verr := ledgerVerifyLocked(ctx, c, p.Owner.Name, extSchema)
		if verr != nil {
			return wrapTrust(verr, "", "")
		}
		if findings > 0 {
			return transfer.Errorf(transfer.CodeVerification, false, "ledger_verify reports %d findings on the new chain", findings)
		}
		// The policy of the source is set again after the verification: a copy that accepts unattested writes would be a downgrade of the protection.
		for _, ct := range ledgers {
			if ct.Ledger.Policy == "required" {
				if err := locked(ct.Schema, ct.Name, func(tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `SELECT kingres.ledger_set_signature_policy($1::pg_catalog.text::pg_catalog.regclass, 'required')`, qualifiedText(ct.Schema, ct.Name))
					return e
				}); err != nil {
					return err
				}
				notes = append(notes, fmt.Sprintf("ledger: %s.%s has the signature policy required again", ct.Schema, ct.Name))
			}
		}
	}
	// Time-partition policies come back DISABLED: a retention policy would drop old partitions on a schedule the user did not choose.
	for _, tp := range cat.TimepartPolicies {
		if err := locked(tp.Schema, tp.Table, func(tx pgx.Tx) error { return o.setPolicyDisabled(ctx, tx, tp) }); err != nil {
			var te *transfer.Error
			if errors.As(err, &te) && te.Code == transfer.CodeTableNotEligible {
				return err
			}
			notes = append(notes, fmt.Sprintf("policy: %s.%s was not imported (%s)", safeName(tp.Schema), safeName(tp.Table), firstSentence(err)))
			continue
		}
		notes = append(notes, fmt.Sprintf("policy disabled: %s.%s; enable it with PUT partition-policy and enabled true", tp.Schema, tp.Table))
	}
	if st := o.importState(env.Task.ID); st != nil {
		st.notes = append(st.notes, notes...)
	}
	return env.SetDetail(ctx, fmt.Sprintf("%d ledger tables, %d policies", len(ledgers), len(cat.TimepartPolicies)))
}

// setPolicyDisabled calls kingres.timepart_set_policy with enabled = false; every argument is a bound parameter.
func (o *Ops) setPolicyDisabled(ctx context.Context, c execer, tp archive.TimepartPolicy) error {
	nilIfEmpty := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	var sortBy any
	if tp.ColumnarSort != "" {
		sortBy = strings.Split(tp.ColumnarSort, ",")
	}
	var opts any
	if len(tp.ColumnarOptions) > 0 {
		b, err := json.Marshal(tp.ColumnarOptions)
		if err != nil {
			return err
		}
		opts = string(b)
	}
	_, err := c.Exec(ctx, `SELECT kingres.timepart_set_policy($1::pg_catalog.text::pg_catalog.regclass, $2::pg_catalog.interval, $3::pg_catalog.interval, $4::pg_catalog.interval, $5::pg_catalog.text,
			$6::pg_catalog.interval, $7::pg_catalog.text[], $8::pg_catalog.jsonb, $9::pg_catalog.text, $10::pg_catalog.interval, false)`,
		qualifiedText(tp.Schema, tp.Table), tp.PartitionEvery, nilIfEmpty(tp.CreateAhead), nilIfEmpty(tp.Retention), tp.RetentionAction, nilIfEmpty(tp.ColumnarAfter), sortBy, opts, tp.Timezone, nilIfEmpty(tp.RunEvery))
	return err
}

// ------------------------------------------------------------------ step: verify

func (o *Ops) stepIMVerify(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	priv := p.private(env.Task.ID)
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	res := a.st.res
	c, err := o.serviceConnFor(ctx, priv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	hasLedger := false
	for _, ct := range res.Catalog.Tables {
		hasLedger = hasLedger || ct.Ledger != nil
	}
	if hasLedger {
		if err := requireExtensions(ctx, c, false, extguard.Ledger); err != nil {
			return err
		}
		findings, verr := ledgerVerifyLocked(ctx, c, p.Owner.Name, extensionSchemas(res.Manifest))
		if verr != nil {
			return wrapTrust(verr, "", "")
		}
		if findings > 0 {
			return transfer.Errorf(transfer.CodeVerification, false, "ledger_verify reports %d findings", findings)
		}
	}
	if !p.keepNotValid() {
		var notValid int
		if err := c.QueryRow(ctx, `SELECT pg_catalog.count(*)::pg_catalog.int4 FROM pg_catalog.pg_constraint c JOIN pg_catalog.pg_namespace n ON n.oid = c.connamespace
				WHERE NOT c.convalidated AND n.nspname NOT LIKE 'pg\_%' AND n.nspname <> 'information_schema' AND n.nspname <> 'kingres'`).Scan(&notValid); err != nil {
			return transfer.SafeError(err)
		}
		if notValid > 0 {
			return transfer.Errorf(transfer.CodeVerification, false, "%d constraints are not valid", notValid)
		}
	}
	// The rows were counted as the role (the step before the withdrawal): the service role reads the catalog and the ledger only.
	return env.SetDetail(ctx, fmt.Sprintf("%d tables verified", len(dataEntriesOf(res.Manifest))))
}

// ------------------------------------------------------------------ step: the final name

// importRenameLocal gives the private database of this server its final name (a database at that name that carries the marker of the task is its own earlier rename). The importing role becomes the owner only in the Open step.
func (o *Ops) importRenameLocal(ctx context.Context, op importRenameOp) error {
	if op.Finish || op.Open {
		if !dbNameRE.MatchString(op.From) || protectedDB[op.From] || strings.HasPrefix(op.From, privatePrefix) || !strings.HasPrefix(op.To, privatePrefix) {
			return errors.New("this rename is not allowed")
		}
		if op.Open {
			return o.openRenamed(ctx, op.From, op.Owner, importMarker(op.To))
		}
		if op.Marker != importMarker(op.To) {
			return errors.New("this rename is not allowed")
		}
		return o.finishRename(ctx, op.From, op.Marker)
	}
	if op.Back {
		// The marker of the task is the proof that the database at the final name is the one that this task renamed: without it nothing is touched.
		if !strings.HasPrefix(op.To, privatePrefix) || !dbNameRE.MatchString(op.To) || !dbNameRE.MatchString(op.From) || protectedDB[op.From] || strings.HasPrefix(op.From, privatePrefix) ||
			op.Marker == "" || op.Marker != importMarker(op.To) {
			return errors.New("this rename is not allowed")
		}
		_, err := o.renameBack(ctx, op.From, op.To, op.Marker)
		return err
	}
	if !strings.HasPrefix(op.From, privatePrefix) || !dbNameRE.MatchString(op.From) || !dbNameRE.MatchString(op.To) || protectedDB[op.To] || strings.HasPrefix(op.To, privatePrefix) {
		return errors.New("this rename is not allowed")
	}
	from, err := sqlx.NewIdent(op.From)
	if err != nil {
		return err
	}
	to, err := sqlx.NewIdent(op.To)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// A database at the final name that carries the marker of this task is this task's own rename, made by an earlier run of the step that did not end (kingresd was
	// restarted): the rename is done. Any other database at that name is not ours.
	// The marker is readable by everybody, so it is not proof by itself: the database must also be owned by the service role (until the opening it is, and no role
	// that may create a database can give a database of its own to the service role).
	if st, err := databaseStateOf(ctx, c, op.To); err != nil {
		return err
	} else if st.Exists {
		if st.Comment == importMarker(op.From) && st.ServiceOwned {
			return nil
		}
		return errDatabaseExists
	}
	// A prepared transaction keeps the database in use for ever: it is rolled back first.
	if err := o.rollbackPreparedLocal(ctx, c, op.From); err != nil {
		return err
	}
	// A rename needs a database that nobody is connected to. The sessions that are there (the background workers of the extensions among them) are ended and the
	// rename is tried again when one of them came back in between.
	var last error
	for attempt := 0; attempt < 40; attempt++ {
		if _, err := c.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE datname = $1 AND pid <> pg_catalog.pg_backend_pid()`, op.From); err != nil {
			return err
		}
		last = run(ctx, c, stmt().Text("ALTER DATABASE").ID(from).Text("RENAME TO").ID(to))
		var pe *pgconn.PgError
		if last == nil || !errors.As(last, &pe) || pe.Code != "55006" {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
	// The rename is all that happens here. The database keeps its marker, its owner (the service role) and the closed door (no CONNECT for PUBLIC): no client can
	// connect to it or own it until every server has the final name and the catalogs were compared again (openRenamed). A rename that is undone on a peer, or after a
	// restart, only ever touches a database that carries the marker.
	return last
}

// dbState is what the catalog says about a database name on one server.
type dbState struct {
	Exists       bool
	Comment      string // "" without one
	Owner        string
	ServiceOwned bool // the owner is the role of the connection (the service role)
}

// databaseStateOf reads the existence, the comment (the marker of an import), the owner and whether the service role owns the database of this name.
func databaseStateOf(ctx context.Context, x execer, name string) (dbState, error) {
	var comment *string
	var owner string
	var svc bool
	err := x.QueryRow(ctx, `SELECT pg_catalog.shobj_description(d.oid, 'pg_database'), pg_catalog.pg_get_userbyid(d.datdba)::pg_catalog.text, pg_catalog.pg_get_userbyid(d.datdba) = current_user
		FROM pg_catalog.pg_database d WHERE d.datname = $1::pg_catalog.text::pg_catalog.name`, name).Scan(&comment, &owner, &svc)
	if errors.Is(err, pgx.ErrNoRows) {
		return dbState{}, nil
	}
	if err != nil {
		return dbState{}, err
	}
	st := dbState{Exists: true, Owner: owner, ServiceOwned: svc}
	if comment != nil {
		st.Comment = *comment
	}
	return st, nil
}

// openRenamed opens the database at the final name to its clients on this server: the importing role owns it and PUBLIC may connect (the default of a database that
// database.create makes). It is called only after the commit point of the import. The database that carries the marker of the task (owned by the service role, or
// by the importing role when the opening ran before) is opened, doing it twice is harmless; a database without the marker that the importing role owns and that has
// no comment is a database whose import is finished here (the marker is removed last): nothing is left to do. Any other database is not touched.
func (o *Ops) openRenamed(ctx context.Context, name, owner, marker string) error {
	db, err := sqlx.NewIdent(name)
	if err != nil {
		return err
	}
	role, err := sqlx.NewIdent(owner)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	st, err := databaseStateOf(ctx, c, name)
	if err != nil {
		return err
	}
	switch {
	case st.Exists && st.Comment == "" && st.Owner == owner:
		return nil
	case !st.Exists || st.Comment != marker || !(st.ServiceOwned || st.Owner == owner):
		return errors.New("the database at the final name does not carry the marker of this import")
	}
	if err := run(ctx, c, stmt().Text("ALTER DATABASE").ID(db).Text("OWNER TO").ID(role)); err != nil {
		return err
	}
	return run(ctx, c, stmt().Text("GRANT CONNECT ON DATABASE").ID(db).Text("TO PUBLIC"))
}

// importCommitMark is the value that the rename step writes to the mark of its task (Task.Mark) at the commit point: the task has decided that the database at the
// final name is the result of the import. It is written, and read back, before the first server opens the database.
// importCommitPrefix starts every commit mark.
const importCommitPrefix = "import-committed:"

func importCommitMark(priv string) string { return importCommitPrefix + priv }

// importCommitted tells whether the task state says that the commit point was passed (or cannot be told: the hook could not read the mark).
func importCommitted(t task.Task, priv string) bool {
	return t.Mark == importCommitMark(priv) || t.Mark == task.MarkUnreadable
}

// renameChecks are the checks around the rename: the withdrawal of the access of the role, the comparison of the servers and of the catalog before the rename, and
// the comparison after it (a test replaces them).
type renameChecks struct {
	access  func(ctx context.Context) error
	servers func(ctx context.Context) error
	catalog func(ctx context.Context) error
	recheck func(ctx context.Context, revert func() string) error
}

// renameFinal is what the last step of an import works with.
type renameFinal struct {
	p          dbImportParams
	priv       string
	sharded    bool
	buckets    int
	ledgerMode string
	checks     renameChecks
	// done is called once, after the import is finished (the audit record and the like); nil in a test.
	done func(ctx context.Context)
}

// importForwardPause is the pause between two tries of the part of the rename step that only goes forward.
var importForwardPause = 2 * time.Second

func (o *Ops) stepIMRename(ctx context.Context, env *task.Env) error {
	p, err := inputOf[dbImportParams](env.Task)
	if err != nil {
		return err
	}
	priv := p.private(env.Task.ID)
	committed := env.Task.Mark == importCommitMark(priv)
	// After the commit point the step needs neither the upload nor the archive (they are for the audit record only): a staging store that is gone does not stop it.
	var rec transfer.Record
	var a *archiveFile
	_, rec, x, lerr := o.loadImport(ctx, env)
	if lerr == nil {
		a, lerr = o.openImport(ctx, x, rec, p, env.Task.ID)
	}
	return o.renameWithArchive(ctx, env, p, committed, rec, a, lerr)
}

// renameWithArchive is stepIMRename after the archive was opened (a nil when it could not be, with lerr). The archive is closed on every path (a refusal, an
// error, a panic) and, as soon as the step has read what it needs, before the roll forward.
func (o *Ops) renameWithArchive(ctx context.Context, env *task.Env, p dbImportParams, committed bool, rec transfer.Record, a *archiveFile, lerr error) error {
	priv := p.private(env.Task.ID)
	closed := false
	closeArchive := func() {
		if a != nil && !closed {
			closed = true
			a.close()
		}
	}
	defer closeArchive()
	if lerr != nil && !committed {
		return lerr
	}
	if !committed {
		if err := o.refuseDangerousImporter(ctx, p.Owner.Name); err != nil {
			return err
		}
	}
	w := renameFinal{p: p, priv: priv, ledgerMode: "off", buckets: 128}
	w.checks = renameChecks{
		access:  func(ctx context.Context) error { return o.importAccess(ctx, env, p, priv, false) },
		servers: func(ctx context.Context) error { return o.checkImportServers(ctx, p.Servers) },
		catalog: func(ctx context.Context) error { return o.checkImportCatalog(ctx, priv) },
		recheck: func(ctx context.Context, revert func() string) error {
			return o.recheckImportCatalog(ctx, p.Database, p.Servers, revert)
		},
	}
	if a != nil {
		w.sharded = hasExt(a.st.res.Manifest, extguard.Shard)
		if hasExt(a.st.res.Manifest, extguard.Ledger) {
			w.ledgerMode = "optIn"
		}
		for _, g := range a.st.res.Catalog.ColocationGroups {
			if g.BucketCount > 0 && g.BucketCount <= archive.MaxBuckets {
				w.buckets = g.BucketCount
				break
			}
		}
		tables, rows := len(a.st.res.Catalog.Tables), a.st.res.Manifest.Totals.Rows
		w.done = func(ctx context.Context) {
			if o.Transfer != nil && o.Transfer.Audit != nil {
				o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "databaseImport", "database": p.Database, "uploadId": idText(p.UploadID), "sha256": rec.SHA256,
					"tables": tables, "rows": rows, "outcome": "success"})
			}
		}
		// The archive was read for what the step needs; it is not held open through the roll forward, which may take long.
		closeArchive()
	}
	return o.importRenameFinal(ctx, env, w)
}

// importRenameFinal is the last step of an import, built around ONE COMMIT POINT.
//
// Before it: the private database takes the final name on every server (closed: owned by the service role, PUBLIC may not connect, the marker of the task on it),
// the catalogs are compared again and the settings are recorded. Any failure puts the names back on the servers that were renamed, removes the settings and fails the
// import; the hook of the task may put back and drop only a database that the service role owns, and only when the task is not committed.
//
// The commit point is the mark of the task (Task.Mark), written to the system store and read back BEFORE the first server opens the database. From then on the step
// only goes forward: open on every server, then remove the marker on every server (the last act), every error is tried again and nothing is put back or dropped, by
// the step or by the hook. After a restart or a change of leader the mark decides, not the state of the databases.
func (o *Ops) importRenameFinal(ctx context.Context, env *task.Env, w renameFinal) error {
	p, priv := w.p, w.priv
	if env.Task.Mark == importCommitMark(priv) {
		return o.importRollForward(ctx, env, w)
	}
	// The name is looked at once more at the moment it is taken. A database at the final name that carries the marker of this task and that the service role still
	// owns is the work of an earlier run of this step (kingresd was restarted after the first rename and before the commit point): the step goes on from there. Any
	// other database at that name is not ours.
	var st dbState
	if err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		var e error
		st, e = databaseStateOf(ctx, tx, p.Database)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to this cluster now; repeat the operation")
	}
	resumed := st.Exists && st.Comment == importMarker(priv) && st.ServiceOwned
	if !resumed {
		if err := w.checks.access(ctx); err != nil {
			return err
		}
	}
	if st.Exists && !resumed {
		return transfer.Errorf(transfer.CodeForbidden, false, "the database %s exists; the import is not finished and the private database is removed", p.Database)
	}
	// A server that joined during the import does not have the private database, and a failover that ran during it leaves the catalog of the private database with the
	// old primary of a group: the database is not renamed. (After a restart the private name is gone: the recheck below, on the final name, is the check.)
	if err := w.checks.servers(ctx); err != nil {
		return err
	}
	if !resumed {
		if err := w.checks.catalog(ctx); err != nil {
			return err
		}
	}
	op := importRenameOp{From: priv, To: p.Database, Owner: p.Owner.Name}
	done := []string{}
	if resumed {
		// Any server may have taken the name before the restart: every one is asked, and the rename is a no-op where it is done.
		done = append(done, "")
		if o.Fanout != nil {
			done = append(done, p.Servers...)
		}
	}
	// A failure before the commit point puts the names back on the servers that took the final name (and removes the settings), so that the private name is the only
	// name of the work in progress and the hook of the task finds all of it.
	revert := func() string {
		bg := context.WithoutCancel(ctx)
		// A committed import is never undone: a run that lost its lease and reaches this point reads the mark first (the run that holds the task may have
		// written it meanwhile). A mark that cannot be read counts as committed; the hook of the task decides later, when it can read.
		if got, rerr := env.ReadMark(bg); rerr != nil || got == importCommitMark(priv) {
			if o.Log != nil {
				o.Log.Warn("an import that may be committed was not put back", "database", p.Database, "private", priv, "error", rerr)
			}
			return "; the import may be committed, so nothing was put back"
		}
		var failed []string
		for _, s := range done {
			if _, err := o.putBackOn(bg, s, p.Database, priv); err != nil {
				if o.Log != nil {
					o.Log.Error("the private name of an import could not be put back", "server", s, "database", p.Database, "private", priv, "error", err)
				}
				failed = append(failed, safeName(s))
			}
		}
		// A settings row of the final name is this import's: no database was there when the names were taken. When a put back failed the hook does it, after its own.
		if len(done) > 0 && len(failed) == 0 {
			if uerr := o.unrecordImportedSettings(bg, p.Database, p.Owner.Name); uerr != nil && o.Log != nil {
				o.Log.Error("the settings of an import that was undone could not be removed", "database", p.Database, "error", uerr)
			}
		}
		return putBackNote(failed, p.Database)
	}
	if err := o.importRenameLocal(ctx, op); err != nil {
		if errors.Is(err, errDatabaseExists) {
			return transfer.Errorf(transfer.CodeForbidden, false, "the database %s exists; the import is not finished and the private database is removed", p.Database)
		}
		return transfer.Errorf(transfer.CodeSQLError, false, "the private database could not be renamed to its final name")
	}
	if !resumed {
		done = append(done, "")
	}
	for _, s := range p.Servers {
		s := s
		if o.Fanout == nil {
			break
		}
		if err := o.onServer(ctx, env, s, "take the final name", func() error { return o.Fanout.Apply(ctx, s, ApplyImportRename, op) }); err != nil {
			if leaseOrStop(ctx, err) {
				return err
			}
			// The server may have renamed before it failed to answer: it is put back too, but only if the database at the final name carries the marker of this task
			// and the service role owns it (renameBack leaves any other database alone).
			if !resumed {
				done = append(done, s)
			}
			note := revert()
			return fmt.Errorf("%w%s", err, note)
		}
		if !resumed {
			done = append(done, s)
		}
	}
	// A failover that started after the check above and before the last server took the final name: the catalog is checked again, and the names are put back on a
	// difference (a failover that starts from now on follows the catalog of the database like that of any other).
	if err := w.checks.recheck(ctx, revert); err != nil {
		return err
	}
	// The registry: the settings of the database, as database.create writes them.
	if err := o.recordImportedSettings(ctx, p, w.sharded, w.buckets, w.ledgerMode); err != nil {
		note := revert()
		return transfer.Errorf(transfer.CodeSQLError, false, "the settings of the database could not be recorded, so the import was undone%s", note)
	}
	// A cancel request that is recorded now was refused by the guard of the kind (it accepts none from the sequences on) or arrived after it was read: it must not
	// pause the import after the commit point, where a cancel is a real request. It is dropped here, before the mark is written, so that nothing that arrives
	// later is lost (the guard of a task that is not committed refuses a cancel, so no real request can arrive in between).
	if err := env.ClearCancel(ctx); err != nil {
		if leaseOrStop(ctx, err) {
			return err
		}
		if o.Log != nil {
			o.Log.Warn("a cancel request that was refused could not be cleared before the commit point of an import", "database", p.Database, "error", err)
		}
	}
	// The commit point.
	switch res, cerr := o.commitImport(ctx, env, importCommitMark(priv)); res {
	case commitNot:
		note := revert()
		return transfer.Errorf(transfer.CodeSQLError, false, "the import could not be committed, so it was undone%s", note)
	case commitUnknown:
		// Whether the mark is written is not known (the store did not answer, the lease is gone): nothing is put back by the step. The hook of the task reads the mark
		// itself and puts the names back only when the task is not committed.
		return cerr
	}
	return o.importRollForward(ctx, env, w)
}

// leaseOrStop tells whether an error means that this run must not do anything more in the name of the task: it was cancelled, lost its lease or was fenced.
func leaseOrStop(ctx context.Context, err error) bool {
	return ctx.Err() != nil || errors.Is(err, task.ErrLeaseLost) || errors.Is(err, fence.ErrFenced)
}

type commitResult int

const (
	commitDone    commitResult = iota // the mark is written and was read back
	commitNot                         // the mark is not written (the store said so and the store shows it)
	commitUnknown                     // it is not known
)

// commitImport writes the commit mark of the task and reads it back. A write that failed is looked at again in the store: only when the store shows that the mark
// is not there is the commit "not done" (and the step may undo its work); when the store cannot be asked, or this run lost its lease, nothing is known.
func (o *Ops) commitImport(ctx context.Context, env *task.Env, mark string) (commitResult, error) {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if err = env.SetMark(ctx, mark); err == nil {
			return commitDone, nil
		}
		if leaseOrStop(ctx, err) {
			return commitUnknown, err
		}
		select {
		case <-ctx.Done():
			return commitUnknown, ctx.Err()
		case <-time.After(importCommitPause):
		}
	}
	got, rerr := env.ReadMark(ctx)
	switch {
	case rerr != nil:
		return commitUnknown, fmt.Errorf("the commit of the import is not known: %w", err)
	case got == mark:
		return commitDone, nil
	}
	return commitNot, err
}

// importCommitPause is the pause between two tries to write the commit mark.
var importCommitPause = time.Second

// importCancelPoll is how often the roll forward asks the store whether a cancel was requested.
var importCancelPoll = time.Second

// importRollForward is the part of the step after the commit point: open on every server, then remove the marker on every server. It does not give up on an
// error: it is logged and shown, and the work is tried again after a pause that grows to fifteen times importForwardPause, until it is done or this run is stopped
// (shutdown, lost lease; the next run goes on from the mark). A cancel request stops the run too, without undoing anything: the task is paused (committed, not
// finished) and goes on from the mark when it is resumed.
func (o *Ops) importRollForward(ctx context.Context, env *task.Env, w renameFinal) error {
	p := w.p
	fctx, stop := context.WithCancel(ctx)
	defer stop()
	var cancelled atomic.Bool
	go func() {
		t := time.NewTicker(importCancelPoll)
		defer t.Stop()
		for {
			select {
			case <-fctx.Done():
				return
			case <-t.C:
				if env.CancelRequested(fctx) {
					cancelled.Store(true)
					stop()
					return
				}
			}
		}
	}()
	paused := func() error {
		return task.Pause("A cancel was requested; the import is committed and was not undone. Repeat the import request to finish it, or cancel the task to give it up without undoing anything.")
	}
	pause := importForwardPause
	var left []string
	for {
		if cancelled.Load() {
			return paused()
		}
		l, err := o.importForwardOnce(fctx, env, w)
		left = l
		if err == nil {
			break
		}
		if cancelled.Load() {
			return paused()
		}
		if leaseOrStop(fctx, err) {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if o.Log != nil {
			o.Log.Error("the import is committed and goes on after an error", "database", p.Database, "error", err)
		}
		_ = env.SetDetail(ctx, "The import is committed and finishes again after an error: "+safeSentence(err.Error()))
		select {
		case <-fctx.Done():
			if cancelled.Load() {
				return paused()
			}
			return ctx.Err()
		case <-time.After(pause):
		}
		if pause *= 2; pause > 15*importForwardPause {
			pause = 15 * importForwardPause
		}
	}
	stop()
	detail := "imported " + p.Database
	if st := o.importState(env.Task.ID); st != nil && len(st.notes) > 0 {
		detail += "\n" + strings.Join(st.notes, "\n")
	}
	if len(left) > 0 {
		detail += "\n" + shortList("Servers that are no longer members of the cluster were skipped, the database was not opened on them: ", left, 330)
	}
	o.setImportState(env.Task.ID, nil)
	if w.done != nil {
		w.done(ctx)
	}
	// The import is over: a record of it that cannot be written does not undo it.
	if err := env.SetDetail(ctx, detail); err != nil {
		if leaseOrStop(ctx, err) {
			return err
		}
		if o.Log != nil {
			o.Log.Warn("the detail of a finished import could not be recorded", "database", p.Database, "error", err)
		}
	}
	return nil
}

// importForwardOnce opens the database on every server and then removes the marker on every server. Every part is idempotent (a database that is opened, or
// finished, counts as done), so it can run again after any error, after a restart or on another leader.
// It returns the servers of the request that left the cluster and were skipped.
func (o *Ops) importForwardOnce(ctx context.Context, env *task.Env, w renameFinal) (left []string, err error) {
	p, priv := w.p, w.priv
	// Every server that may hold the database: the others of the request, the starting server and the servers of the cluster now (the leader that runs this may
	// not be the one that renamed, and it is not in the list of the request).
	servers, left := o.importServersLeft(ctx, p)
	marker := importMarker(priv)
	if err := o.openRenamed(ctx, p.Database, p.Owner.Name, marker); err != nil {
		return left, err
	}
	open := importRenameOp{From: p.Database, To: priv, Owner: p.Owner.Name, Open: true}
	if o.Fanout != nil {
		for _, s := range servers {
			s := s
			if err := o.onServer(ctx, env, s, "open the database", func() error { return o.Fanout.Apply(ctx, s, ApplyImportRename, open) }); err != nil {
				return left, err
			}
		}
	}
	// The registry row is written before the commit point; a row that is gone (an operator removed it) is reported, not made again.
	if o.Service != nil {
		var n int
		if err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT pg_catalog.count(*)::pg_catalog.int4 FROM kingres_cp.database_setting WHERE database_name = $1`, p.Database).Scan(&n)
		}); err == nil && n == 0 && o.Log != nil {
			o.Log.Warn("the settings row of an imported database is not there", "database", p.Database)
		}
	}
	// The marker goes last, on every server. Until it is gone from all of them an error here is tried again.
	fin := importRenameOp{From: p.Database, To: priv, Finish: true, Marker: marker}
	if err := o.finishRename(ctx, p.Database, marker); err != nil {
		return left, err
	}
	if o.Fanout != nil {
		for _, s := range servers {
			s := s
			if err := o.onServer(ctx, env, s, "finish the import", func() error { return o.Fanout.Apply(ctx, s, ApplyImportRename, fin) }); err != nil {
				return left, err
			}
		}
	}
	return left, nil
}

// putBackOn puts the database at the final name back to its private name on one server ("" is this one); a database without the marker of the task, or that the
// service role does not own, is left alone. It reports whether this server's database was renamed (always false for another server, which does not answer).
func (o *Ops) putBackOn(ctx context.Context, server, final, priv string) (bool, error) {
	if server == "" {
		return o.renameBack(ctx, final, priv, importMarker(priv))
	}
	if o.Fanout == nil {
		return false, nil
	}
	return false, o.Fanout.Apply(ctx, server, ApplyImportRename, importRenameOp{From: final, To: priv, Back: true, Marker: importMarker(priv)})
}

// unrecordImportedSettings removes the registry row that recordImportedSettings wrote, for an import that was undone after it. Only a row that the importing role
// created is removed (recordImportedSettings takes the row over), so that the row of a database that somebody else made under that name is never deleted.
func (o *Ops) unrecordImportedSettings(ctx context.Context, database, owner string) error {
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM kingres_cp.database_setting WHERE database_name = $1 AND created_by = $2`, database, owner)
		return err
	})
}

// renameBack undoes the rename of this server (a failure before the commit point): the final name goes back to the private one, the marker is written again and
// PUBLIC may not connect. It acts only on a database that carries the marker of the task AND that the service role owns: an opened database (the importing role owns
// it) is never put back, whatever its marker says. It reports whether the database was renamed. Every part is tried and the errors are joined.
func (o *Ops) renameBack(ctx context.Context, from, to, marker string) (bool, error) {
	if marker == "" {
		return false, errors.New("the marker of the task is needed to put a name back")
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return false, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	// Only the database that this task renamed is put back: it carries the marker and the service role owns it. Another database at that name (a peer that did not
	// rename because the name was taken, a database that a role made in the meantime, a database that was opened to its owner) is left as it is.
	st, err := databaseStateOf(ctx, c, from)
	if err != nil {
		return false, err
	}
	if !st.Exists {
		return false, nil
	}
	if st.Comment != marker || !st.ServiceOwned {
		if o.Log != nil {
			o.Log.Warn("a database at the final name of an import was left alone: it does not carry the marker of the task or is not owned by the service role", "database", from)
		}
		return false, nil
	}
	var errs []error
	if err := o.rollbackPreparedLocal(ctx, c, from); err != nil {
		errs = append(errs, err)
	}
	var rerr error
	for attempt := 0; attempt < 40; attempt++ {
		_, _ = c.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE datname = $1 AND pid <> pg_catalog.pg_backend_pid()`, from)
		rerr = run(ctx, c, stmt().Text("ALTER DATABASE").ID(sqlx.MustIdent(from)).Text("RENAME TO").ID(sqlx.MustIdent(to)))
		var pe *pgconn.PgError
		if rerr == nil || !errors.As(rerr, &pe) || pe.Code != "55006" {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	if rerr != nil {
		return false, rerr
	}
	t := sqlx.MustIdent(to)
	if err := run(ctx, c, stmt().Text("COMMENT ON DATABASE").ID(t).Text("IS").Lit(marker)); err != nil {
		errs = append(errs, err)
	}
	if err := run(ctx, c, stmt().Text("REVOKE CONNECT ON DATABASE").ID(t).Text("FROM PUBLIC")); err != nil {
		errs = append(errs, err)
	}
	return true, errors.Join(errs...)
}

// finishRename removes the marker of the task from the database at the final name, if it still carries it.
func (o *Ops) finishRename(ctx context.Context, name, marker string) error {
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	var comment *string
	if err := c.QueryRow(ctx, `SELECT pg_catalog.shobj_description(d.oid, 'pg_database') FROM pg_catalog.pg_database d WHERE d.datname = $1::pg_catalog.text::pg_catalog.name`, name).Scan(&comment); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if comment == nil || *comment != marker {
		return nil
	}
	return run(ctx, c, stmt().Text("COMMENT ON DATABASE").ID(sqlx.MustIdent(name)).Text("IS NULL"))
}

func (o *Ops) recordImportedSettings(ctx context.Context, p dbImportParams, sharded bool, buckets int, ledgerMode string) error {
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		var bc any
		if sharded {
			bc = buckets
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kingres_cp.database_setting
			(database_name, sharding_enabled, bucket_count, durability, ledger_mode, ledger_digest_interval, created_by)
			VALUES ($1, $2, $3, 'sync', $4, 3600, $5)
			ON CONFLICT (database_name) DO UPDATE SET sharding_enabled = EXCLUDED.sharding_enabled, bucket_count = EXCLUDED.bucket_count,
			  durability = EXCLUDED.durability, ledger_mode = EXCLUDED.ledger_mode, ledger_digest_interval = EXCLUDED.ledger_digest_interval,
			  created_by = EXCLUDED.created_by`,
			p.Database, sharded, bc, ledgerMode, p.Owner.Name); err != nil {
			return err
		}
		return o.setDurability(ctx, tx, p.Database, "sync")
	})
}

// ------------------------------------------------------------------ failure

// importOnFailHook is the compensation of the task: it drops the private database, on every server, and nothing else. The name is derived from the id of the
// task, so a task cannot drop a database it did not make. A database that this task renamed and that is still at the final name with the marker of the task is put back first (putBackEverywhere; the hook runs only for a task that failed or was cancelled,
// and the rename is the last act). With keepOnFailure the private database stays, marked by its name.
func (o *Ops) importOnFailHook(ctx context.Context, t task.Task) {
	p, err := inputOf[dbImportParams](t)
	if err != nil {
		return
	}
	o.setImportState(t.ID, nil)
	// A task that passed the commit point (or whose mark cannot be read) is not undone: the database at the final name is the result of the import, it may be open
	// and written to by its owner. Nothing is put back, dropped or withdrawn, and no failure is audited for it.
	if importCommitted(t, p.private(t.ID)) {
		if o.Log != nil {
			o.Log.Error("a database import failed after its commit point; the database is left as it is", "task", t.ID, "database", p.Database)
		}
		return
	}
	code := ""
	if t.Error != nil {
		code = t.Error.Code
	}
	kept := p.KeepOnFailure
	if o.Transfer != nil && o.Transfer.Audit != nil {
		// The text of the error is a fixed sentence with the kind, the position and the fingerprint of a refused statement, never its text.
		msg := ""
		if t.Error != nil {
			msg = safeSentence(t.Error.Message)
		}
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "databaseImport", "database": p.Database, "uploadId": idText(p.UploadID), "outcome": "failure", "code": code, "message": msg, "kept": kept})
	}
	bg := context.WithoutCancel(ctx)
	// A database that this task renamed and could not put back (a server that did not answer, a restart in the middle of the step) is at the final name with the marker
	// of the task: it is put back to the private name first, on every server, and goes the way of the private database below. A database without the marker is left
	// alone, as in every rename back.
	o.putBackEverywhere(bg, p, t.ID)
	if kept {
		// The database stays for the operator to look at; the role that made it may not connect to it, on any server.
		o.withdrawEverywhere(bg, p, p.private(t.ID))
		return
	}
	priv := p.private(t.ID)
	if o.Service != nil {
		if err := o.importDropLocal(bg, priv); err != nil && o.Log != nil {
			o.Log.Warn("the private database of a failed import could not be dropped", "database", priv, "error", err)
		}
	}
	if o.Fanout != nil {
		// Every server that the task made the database on, and every server that is there now: the leader that runs this hook may not be the one that started the task.
		seen := map[string]bool{o.ServerHost: true}
		var servers []string
		for _, s := range o.importServers(bg, p) {
			if !seen[s] {
				seen[s] = true
				servers = append(servers, s)
			}
		}
		for _, s := range servers {
			if err := o.Fanout.Apply(bg, s, ApplyImportDrop, importDropOp{Name: priv}); err != nil && o.Log != nil {
				o.Log.Warn("the private database of a failed import could not be dropped on a server", "database", priv, "server", s, "error", err)
			}
		}
	}
}

// withdrawEverywhere withdraws the importing role from the private database on this server and on every other, logging what fails (the failure hook cannot wait).
func (o *Ops) withdrawEverywhere(ctx context.Context, p dbImportParams, priv string) {
	if o.Service == nil {
		return
	}
	op := importAccessOp{Name: priv, Role: p.Owner.Name, Allow: false, Full: true}
	if err := o.importAccessLocal(ctx, op); err != nil && o.Log != nil {
		o.Log.Warn("the access of the role to a kept private database could not be withdrawn", "database", priv, "error", err)
	}
	if o.Fanout == nil {
		return
	}
	op.Full = false
	seen := map[string]bool{o.ServerHost: true}
	for _, s := range o.importServers(ctx, p) {
		if seen[s] {
			continue
		}
		seen[s] = true
		if err := o.Fanout.Apply(ctx, s, ApplyImportAccess, op); err != nil && o.Log != nil {
			o.Log.Warn("the access of the role to a kept private database could not be withdrawn on a server", "database", priv, "server", s, "error", err)
		}
	}
}

// importDropLocal drops the private database of a task on this server, and only when this server made it for that task: the name starts with the prefix, the
// database is owned by the service role (a database that a role made with a guessed name is not) and it carries the marker of the task. A database that is not
// there is not an error.
func (o *Ops) importDropLocal(ctx context.Context, name string) error {
	if !strings.HasPrefix(name, privatePrefix) || !dbNameRE.MatchString(name) || protectedDB[name] {
		return errors.New("this database cannot be dropped")
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	var mine bool
	err = c.QueryRow(ctx, `SELECT pg_catalog.pg_get_userbyid(d.datdba) = current_user AND coalesce(pg_catalog.shobj_description(d.oid, 'pg_database'), '') = $2::pg_catalog.text
		FROM pg_catalog.pg_database d WHERE d.datname = $1::pg_catalog.text::pg_catalog.name`, name, importMarker(name)).Scan(&mine)
	_ = c.Close(context.WithoutCancel(ctx))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !mine {
		return errors.New("the database was not made by this import")
	}
	// A prepared transaction keeps the database in use: DROP ... WITH (FORCE) fails for ever while one is there.
	pc, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	perr := o.rollbackPreparedLocal(ctx, pc, name)
	_ = pc.Close(context.WithoutCancel(ctx))
	if perr != nil {
		return perr
	}
	return o.dropDB(ctx, dropParams{Name: name})
}

// sortedKeys of a set.
func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// safeSentence bounds and cleans a sentence that holds names from the catalog before it is put in a message.
func safeSentence(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n >= 240 {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r > 0x7e {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// putBackEverywhere puts the database at the final name of a failed import (a task that is not committed) back to its private name on this server and on every other
// one that the task used or that is in the cluster now (the leader that runs the hook may not be the one that started the task). It touches a database only when it
// carries the marker of this task and the service role owns it. The settings row of the final name is removed when a database was put back here, or when no database
// is at the final name on this server (a row without a database is stale); a database of somebody else at the final name keeps its row.
func (o *Ops) putBackEverywhere(ctx context.Context, p dbImportParams, id int64) {
	if o.Service == nil {
		return
	}
	priv := p.private(id)
	changed, err := o.putBackOn(ctx, "", p.Database, priv)
	if err != nil && o.Log != nil {
		o.Log.Warn("the final name of a failed import could not be put back", "database", p.Database, "error", err)
	}
	if err == nil {
		stale := changed
		if !stale {
			if c, cerr := o.Service.ServiceConn(ctx, "postgres"); cerr == nil {
				st, serr := databaseStateOf(ctx, c, p.Database)
				_ = c.Close(context.WithoutCancel(ctx))
				stale = serr == nil && !st.Exists
			}
		}
		if stale {
			if uerr := o.unrecordImportedSettings(ctx, p.Database, p.Owner.Name); uerr != nil && o.Log != nil {
				o.Log.Error("the settings of a failed import could not be removed", "database", p.Database, "error", uerr)
			}
		}
	}
	if o.Fanout == nil {
		return
	}
	seen := map[string]bool{o.ServerHost: true}
	for _, s := range o.importServers(ctx, p) {
		if seen[s] {
			continue
		}
		seen[s] = true
		if _, err := o.putBackOn(ctx, s, p.Database, priv); err != nil && o.Log != nil {
			o.Log.Warn("the final name of a failed import could not be put back on a server", "database", p.Database, "server", s, "error", err)
		}
	}
}

// putBackNote is the sentence that the error of an import gets when the final name could not be put back on some servers: the database is there under the final name
// (not under the private one), and the operator is told what happens and what to do.
func putBackNote(failed []string, final string) string {
	if len(failed) == 0 {
		return ""
	}
	return fmt.Sprintf("; the database could not be put back to its private name on %s: it is still there under the final name %s, with the marker of this import, and the removal of the failed import tries again; if %s is still there after it, drop it with DROP DATABASE on that server",
		strings.Join(failed, ", "), final, final)
}

// importServers are the other servers that an import may have touched: the servers that the request named, the server that accepted it and every other server of
// the cluster now, without this server and without repeats. The leader that runs a step after a change of leader is not the one that started the task, and the one
// that started it is not among the "others" of the request, so the list of the request alone is not enough.
//
// A server that is no longer a member of the cluster (removed, or it left) is not in the list: nothing can be done on it and nothing waits for it. The starting
// server is in it only while it is a member. A server that is a member and cannot be reached stays in it, and the import waits for it. When the members cannot be
// read, no server is dropped.
func (o *Ops) importServers(ctx context.Context, p dbImportParams) []string {
	out, _ := o.importServersLeft(ctx, p)
	return out
}

// importServersLeft is importServers and, besides, the servers of the request that are no longer members of the cluster (they are skipped).
func (o *Ops) importServersLeft(ctx context.Context, p dbImportParams) (servers, left []string) {
	seen := map[string]bool{o.ServerHost: true, "": true}
	var members map[string]bool
	var current []string
	if o.Fanout != nil {
		if others, err := o.Fanout.Others(ctx); err != nil {
			if o.Log != nil {
				o.Log.Warn("reading the other servers failed", "error", err)
			}
		} else {
			members = map[string]bool{}
			current = ids(others)
			for _, s := range current {
				members[s] = true
			}
		}
	}
	add := func(s string) {
		if seen[s] {
			return
		}
		seen[s] = true
		if members != nil && !members[s] {
			left = append(left, s)
			return
		}
		servers = append(servers, s)
	}
	for _, s := range p.Servers {
		add(s)
	}
	add(p.Origin)
	for _, s := range current {
		add(s)
	}
	return servers, left
}

// importNoteBudget is the length that the note of a paused import is kept to (the task engine cuts a longer one at 330 bytes).
const importNoteBudget = 330

// shortList writes prefix and the names, separated by commas, so that the whole is at most max bytes: names that do not fit are left out and counted ("and 3
// more"). A name is written whole or not at all, so the cut never splits a UTF-8 sequence. When not even one name fits, only their number is written.
func shortList(prefix string, names []string, max int) string {
	if len(names) == 0 {
		return prefix + "none"
	}
	for k := len(names); k >= 0; k-- {
		var b strings.Builder
		b.WriteString(prefix)
		b.WriteString(strings.Join(names[:k], ", "))
		switch {
		case k == len(names):
		case k == 0:
			if len(names) == 1 {
				b.WriteString("1 server")
			} else {
				fmt.Fprintf(&b, "%d servers", len(names))
			}
		default:
			fmt.Fprintf(&b, " and %d more", len(names)-k)
		}
		if b.Len() <= max || k == 0 {
			return b.String()
		}
	}
	return prefix
}

// importCommittedNote is Definition.Committed of the import: whether the task passed the commit point of its last step (or may have: the mark could not be read),
// and the sentence that the paused task carries then. The database is at the final name and is never undone from there. The instruction of the operator comes
// first; only the list of the servers is shortened to keep the note within its length.
func importCommittedNote(t task.Task) (string, bool) {
	p, err := inputOf[dbImportParams](t)
	if err != nil || !importCommitted(t, p.private(t.ID)) {
		return "", false
	}
	servers := append([]string(nil), p.Servers...)
	if p.Origin != "" {
		servers = append(servers, p.Origin)
	}
	state := " is committed and not finished;"
	if t.Mark == task.MarkUnreadable {
		state = " may be committed (the task store could not be read) and is not finished;"
	}
	head := "Repeat the same import request to finish it, or cancel the task to give it up. The import of " + p.Database + state +
		" nothing is undone. Servers where it may still be closed: "
	return head + shortList("", servers, importNoteBudget-len(head)-1) + ".", true
}

// importCancelNote is Definition.CanceledNote of the import: the message of the task that an operator ended while it was paused and committed. It names the
// final database, the servers where it is open and those where it is not (asked now, with a short time for each), and the manual steps.
func (o *Ops) importCancelNote(ctx context.Context, t task.Task) string {
	p, err := inputOf[dbImportParams](t)
	if err != nil {
		return "The import was ended while it was committed; nothing was undone."
	}
	open, notOpen, unknown := o.importOpenness(ctx, p)
	// The instruction comes first; the lists of servers are shortened to keep the message within the length the task row keeps.
	b := "Ended by a cancel after the commit point; nothing was undone. Finish or drop the database " + p.Database +
		" by hand as the operator guide says (a database import that cannot be finished)."
	b += " " + shortList("Open on: ", open, 85) + "."
	b += " " + shortList("Not open on: ", notOpen, 95) + "."
	if len(unknown) > 0 {
		b += " " + shortList("Not reached: ", unknown, 70) + "."
	}
	return b
}

// importOpenness asks every server that may hold the database whether it is open to its owner (the importing role owns it) or not (it is closed, or it is not
// there). A server that cannot be asked is "unknown". The servers are those of importServers and this one; each is given a short time.
func (o *Ops) importOpenness(ctx context.Context, p dbImportParams) (open, notOpen, unknown []string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cancel()
	judge := func(name string, exists bool, owner string, err error) {
		switch {
		case err != nil:
			unknown = append(unknown, safeName(name))
		case exists && owner == p.Owner.Name:
			open = append(open, safeName(name))
		default:
			notOpen = append(notOpen, safeName(name))
		}
	}
	if o.Service != nil && o.ServerHost != "" {
		var st dbState
		err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
			var e error
			st, e = databaseStateOf(ctx, tx, p.Database)
			return e
		})
		judge(o.ServerHost, st.Exists, st.Owner, err)
	}
	if !dbNameRE.MatchString(p.Database) {
		return
	}
	q := `SELECT pg_catalog.pg_get_userbyid(datdba)::pg_catalog.text FROM pg_catalog.pg_database WHERE datname = '` + p.Database + `'`
	for _, s := range o.importServers(ctx, p) {
		if o.Remote == nil {
			judge(s, false, "", errors.New("no way to ask"))
			continue
		}
		sctx, c := context.WithTimeout(ctx, 5*time.Second)
		rows, err := o.Remote.Query(sctx, s, "postgres", q)
		c()
		owner := ""
		if err == nil && len(rows) > 0 && len(rows[0]) > 0 {
			owner, _ = rows[0][0].(string)
		}
		judge(s, err == nil && len(rows) > 0, owner, err)
	}
	return
}

// resumeCommittedImport goes on with a paused import of this upload: a repeat of the import request is the way to resume it, whether the task passed its commit
// point (the archive is not read again, the mark decides), may have (the mark could not be read when it was paused) or did not (its mark is empty now, and the step
// runs as a step that can still be undone). The owner of the upload is checked by the caller, and a repeat for another database name is not a resume. Nothing else
// of the repeat (expectedSha256, keepOnFailure, the flags of the request) is read: the task goes on with what it was started with. It reports whether there was
// such a task.
func (o *Ops) resumeCommittedImport(ctx context.Context, up transfer.Record, database string) (task.Task, bool, error) {
	eng := o.Engine()
	if eng == nil || up.TaskID == nil {
		return task.Task{}, false, nil
	}
	t, err := eng.Get(ctx, *up.TaskID)
	if err != nil || t.Kind != KindDatabaseImport || t.State != task.Paused {
		return task.Task{}, false, nil
	}
	if p, perr := inputOf[dbImportParams](t); perr != nil || (database != "" && database != p.Database) {
		return task.Task{}, false, nil
	}
	if err := eng.Resume(ctx, t.ID); err != nil {
		var x *task.ExcludedError
		if errors.As(err, &x) {
			return task.Task{}, true, x.Problem()
		}
		if errors.Is(err, task.ErrNotPaused) {
			return task.Task{}, true, problem.New(problem.OperationInProgress, "The import is already running.")
		}
		return task.Task{}, true, err
	}
	o.wg.Add(1)
	go func() { //nolint:contextcheck // the run outlives the request
		defer o.wg.Done()
		if err := eng.RunNow(o.bg(), t.ID, nil); err != nil && o.bg().Err() == nil { //nolint:contextcheck // the background run outlives the request
			o.Log.Error("task ended with an error", "task", t.ID, "kind", KindDatabaseImport, "error", err)
		}
	}()
	t, err = eng.Get(ctx, t.ID)
	return t, true, err
}
