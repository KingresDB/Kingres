// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package cluster

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// extTarget is what installing the extensions of a database needs of the database of the server that receives them.
type extTarget interface {
	// Available returns the names of the extensions the package of this server contains.
	Available(ctx context.Context) (map[string]bool, error)
	// Fixed says that the control file of the extension fixes where it is installed: it names a schema, or it is not relocatable. Such an extension is
	// created without a schema clause (the kingres_* extensions make their own schema, and a schema made before them would stop them).
	Fixed(ctx context.Context, ext string) (bool, error)
	// SchemaOwner returns the owner of the schema, and whether it exists.
	SchemaOwner(ctx context.Context, schema string) (owner string, exists bool, err error)
	// Run executes the statements in one transaction: a schema that was made for an extension that then fails is not left behind.
	Run(ctx context.Context, stmts []*sqlx.Builder) error
}

// applyExtensions installs the extensions of d (kingres_shard excepted: kingres.add_node installs it). It tries every extension and reports the problems
// together: the extensions that the package of this server lacks in one message, a schema that exists with another owner than on the leader, a failed
// statement. pg_stat_statements that the package lacks is only a warning, as when a database is made on a server with an older package.
func applyExtensions(ctx context.Context, log *slog.Logger, t extTarget, d wire.DatabaseSpec) error {
	avail, err := t.Available(ctx)
	if err != nil {
		return err
	}
	var errs []error
	var missing []string
	for _, e := range d.Extensions {
		if e == "kingres_shard" {
			continue
		}
		if !avail[e] {
			if e == StatStatementsExtension {
				log.Warn("the package of this server does not contain pg_stat_statements; the database is copied without it", "database", d.Name)
				continue
			}
			missing = append(missing, truncName(e))
			continue
		}
		place, known := d.ExtensionPlaces[e]
		fixed, err := t.Fixed(ctx, e)
		if err != nil {
			errs = append(errs, fmt.Errorf("installing extension %s failed", truncName(e)))
			continue
		}
		stmts, err := extensionStatements(e, place, known, fixed)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(stmts) == 2 { // a schema is made: it must not be somebody else's
			owner, exists, err := t.SchemaOwner(ctx, place.Schema)
			if err != nil {
				errs = append(errs, fmt.Errorf("installing extension %s failed", truncName(e)))
				continue
			}
			if exists && place.Owner != "" && owner != place.Owner {
				errs = append(errs, fmt.Errorf("schema %s of extension %s is owned by %s on this server and by %s on the leader: make the owner the same, then add the server again",
					truncName(place.Schema), truncName(e), truncName(owner), truncName(place.Owner)))
				continue
			}
		}
		if err := t.Run(ctx, stmts); err != nil {
			errs = append(errs, fmt.Errorf("installing extension %s failed", truncName(e)))
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		errs = append(errs, fmt.Errorf("the package of this server does not contain the extensions %s of database %s", strings.Join(missing, ", "), truncName(d.Name)))
	}
	return errors.Join(errs...)
}

// extensionStatements builds the statements that install extension e in the schema it has on the sender: CREATE SCHEMA IF NOT EXISTS ... AUTHORIZATION owner
// (the schema is made as the sender has it), then CREATE EXTENSION IF NOT EXISTS ... SCHEMA schema. Both names are quoted identifiers. An extension whose
// control file fixes its schema (fixed) and an extension in pg_catalog get no schema clause and no schema made. A sender that does not say where an
// extension is (known false, a leader of an earlier version, which the cluster upgrades last) gets pg_stat_statements in public, where it is made
// everywhere, and any other extension without a clause, as before.
func extensionStatements(e string, place wire.ExtensionPlace, known, fixed bool) ([]*sqlx.Builder, error) {
	id, err := sqlx.NewIdent(e)
	if err != nil {
		return nil, errors.New("invalid extension name")
	}
	if !known && e == StatStatementsExtension {
		place, known = wire.ExtensionPlace{Schema: "public"}, true
	}
	create := new(sqlx.Builder).Text("CREATE EXTENSION IF NOT EXISTS").ID(id)
	if !known || fixed || place.Schema == "" || place.Schema == "pg_catalog" {
		return []*sqlx.Builder{create}, nil
	}
	sch, err := sqlx.NewIdent(place.Schema)
	if err != nil {
		return nil, errors.New("invalid schema name")
	}
	mk := new(sqlx.Builder).Text("CREATE SCHEMA IF NOT EXISTS").ID(sch)
	if place.Owner != "" {
		owner, err := sqlx.NewIdent(place.Owner)
		if err != nil {
			return nil, errors.New("invalid owner name")
		}
		mk.Text("AUTHORIZATION").ID(owner)
	}
	return []*sqlx.Builder{mk, create.Text("SCHEMA").ID(sch)}, nil
}

// pgTarget is the extTarget of a connection to the database.
type pgTarget struct{ c *pgx.Conn }

func (p pgTarget) Available(ctx context.Context) (map[string]bool, error) {
	rows, err := p.c.Query(ctx, "SELECT name FROM pg_catalog.pg_available_extensions")
	if err != nil {
		return nil, err
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out, nil
}

func (p pgTarget) Fixed(ctx context.Context, ext string) (bool, error) {
	var fixed bool
	err := p.c.QueryRow(ctx, "SELECT coalesce(bool_or(schema IS NOT NULL OR NOT relocatable), false) FROM pg_catalog.pg_available_extension_versions WHERE name = $1", ext).Scan(&fixed)
	return fixed, err
}

func (p pgTarget) SchemaOwner(ctx context.Context, schema string) (string, bool, error) {
	var owner string
	err := p.c.QueryRow(ctx, "SELECT r.rolname FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_roles r ON r.oid = n.nspowner WHERE n.nspname = $1", schema).Scan(&owner)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return owner, err == nil, err
}

func (p pgTarget) Run(ctx context.Context, stmts []*sqlx.Builder) error {
	tx, err := p.c.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, b := range stmts {
		st, err := b.Build()
		if err != nil {
			return errNotQuoted
		}
		if _, err := tx.Exec(ctx, st.SQL, st.Args...); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}
