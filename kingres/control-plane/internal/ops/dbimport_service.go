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
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/archive"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
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
	// ApplyImportRename: take the final name, make the importing role the owner.
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
}

type importSeqOp struct {
	Name  string          `json:"name"`
	Items []importSeqItem `json:"items"`
}

type importRenameOp struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Owner string `json:"owner"`
	// Back undoes a rename (a later server failed): the final name goes back to the private one, and nothing else changes.
	Back bool `json:"back,omitempty"`
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
	// The expressions of the table, as the catalog deparses them with the search path of this session.
	rel := qualifiedText(schema, table)
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

// requireExtensions verifies the schemas of the extensions that the next calls use, as extguard does for every read of the schema kingres on a service session.
func requireExtensions(ctx context.Context, c *pgx.Conn, shard bool, exts ...string) error {
	if shard {
		var ok bool
		err := inTx(ctx, c, func(tx pgx.Tx) error {
			var e error
			ok, e = shardTrusted(ctx, tx)
			return e
		})
		if err != nil {
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the schema kingres of the private database cannot be trusted")
		}
		if !ok {
			return transfer.Errorf(transfer.CodeSchemaUnrecognised, false, "the sharding extension is not installed in the private database")
		}
	}
	g := extguard.NewGuard(c)
	for _, e := range exts {
		if _, err := g.Require(ctx, e); err != nil {
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
	priv := privateName(env.Task.ID)
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
	trusted := func(schema, table string) error {
		if err := inTx(ctx, c, func(tx pgx.Tx) error { return tableTrusted(ctx, tx, schema, table, p.Owner.Name, extSchema) }); err != nil {
			return untrustedError(err)
		}
		return nil
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
		// Every table of the schema is verified before the extension reads or moves it.
		for _, ct := range cat.Tables {
			if ct.Schema == ds.Schema && ct.TreeRole != "leaf" {
				if err := trusted(ct.Schema, ct.Name); err != nil {
					return err
				}
			}
		}
		if _, err := c.Exec(ctx, `SELECT kingres.distribute_schema($1::pg_catalog.text::pg_catalog.regnamespace, NULL::pg_catalog.int4)`, id.String()); err != nil {
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
		case "reference", "hash":
			if err := trusted(ct.Schema, ct.Name); err != nil {
				return err
			}
		}
		switch ct.Kind {
		case "reference":
			if done, err := isDist(rel); err != nil {
				return transfer.SafeError(err)
			} else if !done {
				if _, err := c.Exec(ctx, `SELECT kingres.create_reference_table($1::pg_catalog.text::pg_catalog.regclass)`, rel); err != nil {
					return o.distError(err, ct.Schema, ct.Name)
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
				if _, err := c.Exec(ctx, `SELECT kingres.create_distributed_table($1::pg_catalog.text::pg_catalog.regclass, $2::pg_catalog.name, $3::pg_catalog.text::pg_catalog.regclass, $4::pg_catalog.int4)`, rel, col.Name(), coloc, buckets); err != nil {
					return o.distError(err, ct.Schema, ct.Name)
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
						if _, err := c.Exec(ctx, `SELECT kingres.alter_distributed_table($1::pg_catalog.text::pg_catalog.regclass, NULL, NULL, 'none', false)`, rel); err != nil {
							return o.distError(err, ct.Schema, ct.Name)
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
			if err := o.setColumnarOptions(ctx, c, ct, trusted); err != nil {
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

func (o *Ops) setColumnarOptions(ctx context.Context, c *pgx.Conn, ct archive.CatalogTable, trusted func(schema, table string) error) error {
	if err := trusted(ct.Schema, ct.Name); err != nil {
		return err
	}
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
	if _, err := c.Exec(ctx, `SELECT kingres.columnar_set_options($1::pg_catalog.text::pg_catalog.regclass, $2, $3, $4, $5)`, rel, comp, ints["compressionLevel"], ints["stripeRowCount"], ints["chunkRowCount"]); err != nil {
		return transfer.SafeError(err).ForTarget(ct.Schema, ct.Name)
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
			items = append(items, importSeqItem{Schema: s.Schema, Name: s.Name, Value: v})
		}
	}
	_ = conn.Close(context.WithoutCancel(ctx))
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	op := importSeqOp{Name: priv, Items: items}
	if err := o.importSequencesLocal(ctx, op); err != nil {
		return transfer.SafeError(err)
	}
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
	for _, it := range op.Items {
		if it.Value <= 0 {
			continue
		}
		if _, err := sqlx.QualifiedIdent(it.Schema, it.Name); err != nil {
			return errors.New("a sequence name is not valid")
		}
		// Only a sequence (relkind S) is touched; nothing that the role could have put under that name in another kind is read.
		if _, err := c.Exec(ctx, `SELECT pg_catalog.setval(c.oid, GREATEST($3::pg_catalog.int8, coalesce(pg_catalog.pg_sequence_last_value(c.oid), 0)), true)
			FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text AND c.relkind = 'S'`,
			it.Schema, it.Name, it.Value); err != nil {
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
	priv := privateName(env.Task.ID)
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
	trusted := func(schema, table string) error {
		if err := inTx(ctx, c, func(tx pgx.Tx) error { return tableTrusted(ctx, tx, schema, table, p.Owner.Name, extSchema) }); err != nil {
			return untrustedError(err)
		}
		return nil
	}
	var notes []string
	// The rows were loaded first; ledger_enable enters them as the first entries of a new chain.
	for _, ct := range ledgers {
		mode := map[string]string{"appendOnly": "append_only", "updatable": "updatable"}[ct.Ledger.Mode]
		if mode == "" {
			return transfer.Errorf(transfer.CodeArchiveInvalid, false, "a ledger table has a mode that is not known")
		}
		if err := trusted(ct.Schema, ct.Name); err != nil {
			return err
		}
		if _, err := c.Exec(ctx, `SELECT kingres.ledger_enable($1::pg_catalog.text::pg_catalog.regclass, $2::pg_catalog.text)`, qualifiedText(ct.Schema, ct.Name), mode); err != nil {
			return transfer.SafeError(err).ForTarget(ct.Schema, ct.Name)
		}
		notes = append(notes, fmt.Sprintf("ledger: %s.%s starts a new chain (the source had its head at height %d)", ct.Schema, ct.Name, ct.Ledger.HeadHeight))
	}
	if len(ledgers) > 0 {
		var findings int
		if err := c.QueryRow(ctx, `SELECT pg_catalog.count(*)::pg_catalog.int4 FROM kingres.ledger_verify()`).Scan(&findings); err != nil {
			return transfer.SafeError(err)
		}
		if findings > 0 {
			return transfer.Errorf(transfer.CodeVerification, false, "ledger_verify reports %d findings on the new chain", findings)
		}
		// The policy of the source is set again after the verification: a copy that accepts unattested writes would be a downgrade of the protection.
		for _, ct := range ledgers {
			if ct.Ledger.Policy == "required" {
				if _, err := c.Exec(ctx, `SELECT kingres.ledger_set_signature_policy($1::pg_catalog.text::pg_catalog.regclass, 'required')`, qualifiedText(ct.Schema, ct.Name)); err != nil {
					return transfer.SafeError(err).ForTarget(ct.Schema, ct.Name)
				}
				notes = append(notes, fmt.Sprintf("ledger: %s.%s has the signature policy required again", ct.Schema, ct.Name))
			}
		}
	}
	// Time-partition policies come back DISABLED: a retention policy would drop old partitions on a schedule the user did not choose.
	for _, tp := range cat.TimepartPolicies {
		if err := trusted(tp.Schema, tp.Table); err != nil {
			return err
		}
		if err := o.setPolicyDisabled(ctx, c, tp); err != nil {
			notes = append(notes, fmt.Sprintf("policy: %s.%s was not imported (%s)", tp.Schema, tp.Table, firstSentence(err)))
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
func (o *Ops) setPolicyDisabled(ctx context.Context, c *pgx.Conn, tp archive.TimepartPolicy) error {
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
	priv := privateName(env.Task.ID)
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	res := a.st.res
	extSchema := extensionSchemas(res.Manifest)
	c, err := o.serviceConnFor(ctx, priv)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	byTable := map[string]archive.CatalogTable{}
	for _, ct := range res.Catalog.Tables {
		byTable[ct.Qualified()] = ct
	}
	var bad []string
	for _, me := range dataEntriesOf(res.Manifest) {
		ct := byTable[me.Table]
		if err := inTx(ctx, c, func(tx pgx.Tx) error { return tableTrusted(ctx, tx, ct.Schema, ct.Name, p.Owner.Name, extSchema) }); err != nil {
			return untrustedError(err)
		}
		q := "SELECT pg_catalog.count(*) FROM "
		if ct.TreeRole == "inheritanceParent" {
			q += "ONLY "
		}
		var n int64
		if err := c.QueryRow(ctx, q+qualifiedText(ct.Schema, ct.Name)).Scan(&n); err != nil {
			return transfer.SafeError(err).ForTarget(ct.Schema, ct.Name)
		}
		if n != me.Rows {
			bad = append(bad, fmt.Sprintf("%s has %d rows and the manifest says %d", me.Table, n, me.Rows))
		}
	}
	if len(bad) > 0 {
		if len(bad) > 5 {
			bad = bad[:5]
		}
		return transfer.Errorf(transfer.CodeVerification, false, "the rows do not equal the manifest's: %s", strings.Join(bad, "; "))
	}
	hasLedger := false
	for _, ct := range res.Catalog.Tables {
		hasLedger = hasLedger || ct.Ledger != nil
	}
	if hasLedger {
		if err := requireExtensions(ctx, c, false, extguard.Ledger); err != nil {
			return err
		}
		var findings int
		if err := c.QueryRow(ctx, `SELECT pg_catalog.count(*)::pg_catalog.int4 FROM kingres.ledger_verify()`).Scan(&findings); err != nil {
			return transfer.SafeError(err)
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
	return env.SetDetail(ctx, fmt.Sprintf("%d tables verified", len(dataEntriesOf(res.Manifest))))
}

// ------------------------------------------------------------------ step: the final name

// importRenameLocal gives the private database of this server its final name and makes the importing role its owner. The name must be free at that moment.
func (o *Ops) importRenameLocal(ctx context.Context, op importRenameOp) error {
	if op.Back {
		if !strings.HasPrefix(op.To, privatePrefix) || !dbNameRE.MatchString(op.To) || !dbNameRE.MatchString(op.From) || protectedDB[op.From] || strings.HasPrefix(op.From, privatePrefix) {
			return errors.New("this rename is not allowed")
		}
		return o.renameBack(ctx, op.From, op.To)
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
	owner, err := sqlx.NewIdent(op.Owner)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if ok, err := dbExists(ctx, c, op.To); err != nil {
		return err
	} else if ok {
		return errDatabaseExists
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
	if last != nil {
		return last
	}
	if err := run(ctx, c, stmt().Text("ALTER DATABASE").ID(to).Text("OWNER TO").ID(owner)); err != nil {
		return err
	}
	// The marker of the private database goes with the name.
	if err := run(ctx, c, stmt().Text("COMMENT ON DATABASE").ID(to).Text("IS NULL")); err != nil {
		return err
	}
	// The default of a database that database.create makes: PUBLIC may connect.
	return run(ctx, c, stmt().Text("GRANT CONNECT ON DATABASE").ID(to).Text("TO PUBLIC"))
}

func (o *Ops) stepIMRename(ctx context.Context, env *task.Env) error {
	p, rec, x, err := o.loadImport(ctx, env)
	if err != nil {
		return err
	}
	a, err := o.openImport(ctx, x, rec, p, env.Task.ID)
	if err != nil {
		return err
	}
	defer a.close()
	if err := o.refuseDangerousImporter(ctx, p.Owner.Name); err != nil {
		return err
	}
	priv := privateName(env.Task.ID)
	if err := o.importAccess(ctx, env, p, priv, false); err != nil {
		return err
	}
	// The name is checked once more at the moment it is taken.
	var exists bool
	if err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		var e error
		exists, e = dbExists(ctx, tx, p.Database)
		return e
	}); err != nil {
		return transfer.Errorf(transfer.CodeSessionLost, true, "the service role cannot connect to this cluster now; repeat the operation")
	}
	if exists {
		return transfer.Errorf(transfer.CodeForbidden, false, "the database %s exists; the import is not finished and the private database is removed", p.Database)
	}
	op := importRenameOp{From: priv, To: p.Database, Owner: p.Owner.Name}
	done := []string{}
	// A server that fails after another one was renamed puts the others back, so that the private name is the only name of the work in progress and the hook of
	// the task finds all of it.
	revert := func() {
		bg := context.WithoutCancel(ctx)
		for _, s := range done {
			if s == "" {
				_ = o.renameBack(bg, p.Database, priv)
				continue
			}
			if o.Fanout != nil {
				_ = o.Fanout.Apply(bg, s, ApplyImportRename, importRenameOp{From: p.Database, To: priv, Back: true})
			}
		}
	}
	if err := o.importRenameLocal(ctx, op); err != nil {
		if errors.Is(err, errDatabaseExists) {
			return transfer.Errorf(transfer.CodeForbidden, false, "the database %s exists; the import is not finished and the private database is removed", p.Database)
		}
		revert()
		return transfer.Errorf(transfer.CodeSQLError, false, "the private database could not be renamed to its final name")
	}
	done = append(done, "")
	for _, s := range p.Servers {
		s := s
		if o.Fanout == nil {
			break
		}
		if err := o.onServer(ctx, env, s, "take the final name", func() error { return o.Fanout.Apply(ctx, s, ApplyImportRename, op) }); err != nil {
			revert()
			return err
		}
		done = append(done, s)
	}
	// The registry: the settings of the database, as database.create writes them.
	sharded := hasExt(a.st.res.Manifest, extguard.Shard)
	ledgerMode := "off"
	if hasExt(a.st.res.Manifest, extguard.Ledger) {
		ledgerMode = "optIn"
	}
	buckets := 128
	for _, g := range a.st.res.Catalog.ColocationGroups {
		if g.BucketCount > 0 && g.BucketCount <= archive.MaxBuckets {
			buckets = g.BucketCount
			break
		}
	}
	if err := o.recordImportedSettings(ctx, p, sharded, buckets, ledgerMode); err != nil {
		revert()
		return transfer.Errorf(transfer.CodeSQLError, false, "the settings of the database could not be recorded, so the import was undone")
	}
	detail := "imported " + p.Database
	if st := o.importState(env.Task.ID); st != nil && len(st.notes) > 0 {
		detail += "\n" + strings.Join(st.notes, "\n")
	}
	o.setImportState(env.Task.ID, nil)
	if o.Transfer != nil && o.Transfer.Audit != nil {
		o.Transfer.Audit(ctx, "dataTransfer.completed", map[string]any{"kind": "databaseImport", "database": p.Database, "uploadId": idText(p.UploadID), "sha256": rec.SHA256,
			"tables": len(a.st.res.Catalog.Tables), "rows": a.st.res.Manifest.Totals.Rows, "outcome": "success"})
	}
	return env.SetDetail(ctx, detail)
}

// renameBack undoes the rename of this server (a failure on another server after this one was renamed).
func (o *Ops) renameBack(ctx context.Context, from, to string) error {
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	_, _ = c.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend(pid) FROM pg_catalog.pg_stat_activity WHERE datname = $1 AND pid <> pg_catalog.pg_backend_pid()`, from)
	return run(ctx, c, stmt().Text("ALTER DATABASE").ID(sqlx.MustIdent(from)).Text("RENAME TO").ID(sqlx.MustIdent(to)))
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
			  durability = EXCLUDED.durability, ledger_mode = EXCLUDED.ledger_mode, ledger_digest_interval = EXCLUDED.ledger_digest_interval`,
			p.Database, sharded, bc, ledgerMode, p.Owner.Name); err != nil {
			return err
		}
		return o.setDurability(ctx, tx, p.Database, "sync")
	})
}

// ------------------------------------------------------------------ failure

// importOnFailHook is the compensation of the task: it drops the private database, on every server, and nothing else. The name is derived from the id of the
// task, so a task cannot drop a database it did not make. A database that was renamed is not touched (the hook runs only for a task that failed or was cancelled,
// and the rename is the last act). With keepOnFailure the private database stays, marked by its name.
func (o *Ops) importOnFailHook(ctx context.Context, t task.Task) {
	p, err := inputOf[dbImportParams](t)
	if err != nil {
		return
	}
	o.setImportState(t.ID, nil)
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
	if kept {
		return
	}
	bg := context.WithoutCancel(ctx)
	priv := privateName(t.ID)
	if o.Service != nil {
		if err := o.importDropLocal(bg, priv); err != nil && o.Log != nil {
			o.Log.Warn("the private database of a failed import could not be dropped", "database", priv, "error", err)
		}
	}
	if o.Fanout != nil {
		// Every server that the task made the database on, and every server that is there now: the leader that runs this hook may not be the one that started the task.
		seen := map[string]bool{o.ServerHost: true}
		var servers []string
		for _, s := range append(append([]string(nil), p.Servers...), ids(o.others(bg))...) {
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
