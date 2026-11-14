// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Task kinds of this package.
const (
	KindInstanceRestart = "instance.restart"
	KindDatabaseCreate  = "database.create"
	KindDatabaseDrop    = "database.drop"
	KindTableDistribute = "table.distribute"
	KindLedgerVerify    = "ledger.verify"
	KindNetworkApply    = "networkAccess.apply"
)

var (
	dbNameRE    = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)
	localeRE    = regexp.MustCompile(`^[A-Za-z0-9_.@-]{1,64}$`)
	protectedDB = map[string]bool{"postgres": true, "template0": true, "template1": true, sysdb.Database: true}
)

// durabilityGUC maps the contract's durability levels to synchronous_commit.
var durabilityGUC = map[string]string{"async": "local", "sync": "on", "syncApply": "remote_apply"}

type createParams struct {
	Name            string `json:"name"`
	Owner           string `json:"owner"`
	Encoding        string `json:"encoding"`
	LcCollate       string `json:"lcCollate,omitempty"`
	LcCtype         string `json:"lcCtype,omitempty"`
	ConnectionLimit int    `json:"connectionLimit"`
	Sharding        bool   `json:"sharding"`
	BucketCount     int    `json:"bucketCount"`
	Durability      string `json:"durability"`
	LedgerMode      string `json:"ledgerMode"`
	DigestInterval  int    `json:"digestIntervalSeconds"`
	CreatedBy       string `json:"createdBy"`
}

// Definitions returns the task definitions of this package; the engine that
// runs them is built by the caller (cluster wiring).
func (o *Ops) Definitions() []task.Definition {
	return []task.Definition{
		{Kind: KindDatabaseCreate, Steps: []task.Step{
			{Name: "Create the database", Run: o.stepCreateDB},
			{Name: "Install the extensions", Run: o.stepInstallExtensions},
			{Name: "Record the settings", Run: o.stepRecordSettings},
		}},
		{Kind: KindDatabaseDrop, Steps: []task.Step{
			{Name: "Drop the database", Irreversible: true, Run: o.stepDropDB},
			{Name: "Remove the settings", Run: o.stepDropSettings},
		}},
		{Kind: KindTableDistribute, Steps: []task.Step{
			{Name: "Distribute the table", Irreversible: true, Run: o.stepDistribute},
		}},
		{Kind: KindLedgerVerify, Steps: []task.Step{
			{Name: "Verify the ledger", Run: o.stepVerifyLedger},
		}},
		{Kind: KindInstanceRestart, Steps: []task.Step{
			{Name: "Restart the instance", Run: func(ctx context.Context, _ *task.Env) error {
				return o.Instances.Restart(ctx, 1)
			}},
		}},
		{Kind: KindNetworkApply, Steps: []task.Step{
			{Name: "Store the rules", Run: o.stepStoreRules},
			{Name: "Write pg_hba.conf and reload", Run: o.stepApplyRules},
		}},
	}
}

func inputOf[T any](t task.Task) (T, error) {
	var v T
	if err := json.Unmarshal(t.Input, &v); err != nil {
		return v, fmt.Errorf("reading the task input: %w", err)
	}
	return v, nil
}

// StartTask submits a task and runs it in the background.
func (o *Ops) StartTask(ctx context.Context, kind, subjType, subjID string, input any, actor string) (task.Task, error) {
	eng := o.Engine()
	if eng == nil {
		return task.Task{}, problem.New(problem.ServiceUnavailable, "The task engine is not running yet.").WithRetryAfter(5)
	}
	t, err := eng.Submit(ctx, kind, task.Subject{Type: subjType, ID: subjID}, input, actor)
	if err != nil {
		return t, err
	}
	o.wg.Add(1)
	go func() { //nolint:contextcheck // the run outlives the request
		defer o.wg.Done()
		if err := eng.RunNow(o.bg(), t.ID, nil); err != nil && o.bg().Err() == nil { //nolint:contextcheck // the background run outlives the request
			o.Log.Error("task ended with an error", "task", t.ID, "kind", kind, "error", err)
		}
	}()
	return t, nil
}

func dbExists(ctx context.Context, x execer, name string) (bool, error) {
	var ok bool
	err := x.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_database WHERE datname = $1)", name).Scan(&ok)
	return ok, err
}

// PrepareCreateDatabase validates a create request and returns the task
// input; nothing is created yet.
func (o *Ops) PrepareCreateDatabase(ctx context.Context, actor string, req api.DatabaseCreateRequest) (any, error) {
	var errs []problem.FieldError
	if !dbNameRE.MatchString(req.Name) || protectedDB[req.Name] {
		errs = append(errs, field("/name", "must match ^[a-z_][a-z0-9_]{0,62}$ and not be a system database", "pattern"))
	}
	p := createParams{Name: req.Name, Owner: actor, Encoding: "UTF8", ConnectionLimit: -1, BucketCount: 128,
		Durability: "sync", LedgerMode: "off", DigestInterval: 3600, CreatedBy: actor}
	if g := req.General; g != nil {
		if g.Owner != "" {
			p.Owner = g.Owner
		}
		if g.Encoding != "" {
			p.Encoding = g.Encoding
		}
		p.LcCollate, p.LcCtype = g.LcCollate, g.LcCtype
		if g.ConnectionLimit != nil {
			p.ConnectionLimit = *g.ConnectionLimit
		}
	}
	if _, err := sqlx.NewIdent(p.Owner); err != nil {
		errs = append(errs, field("/general/owner", "is not a valid role name", "pattern"))
	}
	for f, v := range map[string]string{"/general/encoding": p.Encoding, "/general/lcCollate": p.LcCollate, "/general/lcCtype": p.LcCtype} {
		if v != "" && !localeRE.MatchString(v) {
			errs = append(errs, field(f, "contains characters that are not allowed", "pattern"))
		}
	}
	if p.ConnectionLimit < -1 {
		errs = append(errs, field("/general/connectionLimit", "must be -1 or more", "out_of_range"))
	}
	if s := req.Sharding; s != nil {
		p.Sharding = s.Enabled
		if s.BucketCount != 0 {
			if s.BucketCount < 1 || s.BucketCount > 4096 {
				errs = append(errs, field("/sharding/bucketCount", "must be between 1 and 4096", "out_of_range"))
			}
			p.BucketCount = s.BucketCount
		}
	}
	if r := req.Replication; r != nil && r.Durability != "" {
		if _, ok := durabilityGUC[r.Durability]; !ok {
			errs = append(errs, field("/replication/durability", "must be async, sync or syncApply", "enum"))
		}
		p.Durability = r.Durability
	}
	if l := req.Ledger; l != nil {
		switch l.Mode {
		case "", "off", "optIn":
			if l.Mode != "" {
				p.LedgerMode = l.Mode
			}
		case "appendOnly", "updatable":
			return nil, problem.New(problem.UnsupportedOp, "A database-wide ledger mode of "+l.Mode+" is not supported yet; use optIn and enable the ledger per table.")
		default:
			errs = append(errs, field("/ledger/mode", "is not a ledger mode", "enum"))
		}
		if l.DigestIntervalSeconds != 0 {
			if l.DigestIntervalSeconds < 60 || l.DigestIntervalSeconds > 86400 {
				errs = append(errs, field("/ledger/digestIntervalSeconds", "must be between 60 and 86400", "out_of_range"))
			}
			p.DigestInterval = l.DigestIntervalSeconds
		}
	}
	if len(errs) > 0 {
		return nil, problem.Validation("The database request is invalid.", errs...)
	}
	err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		if ok, err := dbExists(ctx, tx, p.Name); err != nil {
			return err
		} else if ok {
			return problem.New(problem.AlreadyExists, fmt.Sprintf("Database %q already exists.", p.Name))
		}
		if ok, err := roleExistsTx(ctx, tx, p.Owner); err != nil {
			return err
		} else if !ok {
			return problem.Validation("The database request is invalid.", field("/general/owner", "role does not exist", "not_found"))
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}
	return p, nil
}

func (o *Ops) stepCreateDB(ctx context.Context, env *task.Env) error {
	p, err := inputOf[createParams](env.Task)
	if err != nil {
		return err
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if ok, err := dbExists(ctx, c, p.Name); err != nil || ok {
		return err // an earlier run of this step already created it
	}
	b := stmt().Text("CREATE DATABASE").ID(sqlx.MustIdent(p.Name)).Text("OWNER").ID(sqlx.MustIdent(p.Owner)).
		Text("TEMPLATE template0 ENCODING").Lit(p.Encoding)
	if p.LcCollate != "" {
		b.Text("LC_COLLATE").Lit(p.LcCollate)
	}
	if p.LcCtype != "" {
		b.Text("LC_CTYPE").Lit(p.LcCtype)
	}
	b.Text("CONNECTION LIMIT").Text(sqlx.Fixed(strconv.Itoa(p.ConnectionLimit)))
	return run(ctx, c, b)
}

// installExtensions installs the selected extensions in database db.
func (o *Ops) installExtensions(ctx context.Context, db string, sharding, ledger bool) error {
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if sharding {
		if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS kingres_shard"); err != nil {
			return fmt.Errorf("installing kingres_shard: %w", err)
		}
		var nodes int
		if err := c.QueryRow(ctx, "SELECT count(*) FROM kingres.node").Scan(&nodes); err != nil {
			return err
		}
		if nodes == 0 {
			if _, err := c.Exec(ctx, "SELECT kingres.init_cluster($1, $2)", o.ServerHost, o.PGPort); err != nil {
				return fmt.Errorf("registering the node in the database: %w", err)
			}
		}
	}
	if ledger {
		if _, err := c.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS kingres_ledger"); err != nil {
			return fmt.Errorf("installing kingres_ledger: %w", err)
		}
	}
	return nil
}

func (o *Ops) stepInstallExtensions(ctx context.Context, env *task.Env) error {
	p, err := inputOf[createParams](env.Task)
	if err != nil {
		return err
	}
	return o.installExtensions(ctx, p.Name, p.Sharding, p.LedgerMode != "off")
}

func (o *Ops) stepRecordSettings(ctx context.Context, env *task.Env) error {
	p, err := inputOf[createParams](env.Task)
	if err != nil {
		return err
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		bc := any(nil)
		if p.Sharding {
			bc = p.BucketCount
		}
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.database_setting
			(database_name, sharding_enabled, bucket_count, durability, ledger_mode, ledger_digest_interval, created_by)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (database_name) DO UPDATE SET sharding_enabled = EXCLUDED.sharding_enabled,
			  bucket_count = EXCLUDED.bucket_count, durability = EXCLUDED.durability,
			  ledger_mode = EXCLUDED.ledger_mode, ledger_digest_interval = EXCLUDED.ledger_digest_interval`,
			p.Name, p.Sharding, bc, p.Durability, p.LedgerMode, p.DigestInterval, p.CreatedBy)
		if err != nil {
			return err
		}
		return o.setDurability(ctx, tx, p.Name, p.Durability)
	})
}

func (o *Ops) setDurability(ctx context.Context, x execer, db, level string) error {
	guc, ok := durabilityGUC[level]
	if !ok {
		return problem.Validation("The database request is invalid.", field("/replication/durability", "must be async, sync or syncApply", "enum"))
	}
	return run(ctx, x, stmt().Text("ALTER DATABASE").ID(sqlx.MustIdent(db)).Text("SET synchronous_commit =").Lit(guc))
}

type dropParams struct {
	Name string `json:"name"`
}

// PrepareDropDatabase validates a drop request.
func (o *Ops) PrepareDropDatabase(ctx context.Context, name string) (any, error) {
	if !dbNameRE.MatchString(name) || protectedDB[name] {
		return nil, problem.New(problem.Conflict, "This database cannot be dropped.")
	}
	err := o.asService(ctx, "postgres", func(tx pgx.Tx) error {
		if ok, err := dbExists(ctx, tx, name); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Database %q does not exist.", name))
		}
		return nil
	})
	if err != nil {
		return nil, wrap(err)
	}
	return dropParams{Name: name}, nil
}

func (o *Ops) stepDropDB(ctx context.Context, env *task.Env) error {
	p, err := inputOf[dropParams](env.Task)
	if err != nil {
		return err
	}
	if protectedDB[p.Name] || !dbNameRE.MatchString(p.Name) {
		return fmt.Errorf("refusing to drop %q", p.Name)
	}
	c, err := o.Service.ServiceConn(ctx, "postgres")
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return run(ctx, c, stmt().Text("DROP DATABASE IF EXISTS").ID(sqlx.MustIdent(p.Name)).Text("WITH (FORCE)"))
}

func (o *Ops) stepDropSettings(ctx context.Context, env *task.Env) error {
	p, err := inputOf[dropParams](env.Task)
	if err != nil {
		return err
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, "DELETE FROM kingres_cp.database_setting WHERE database_name = $1", p.Name)
		return err
	})
}

// UpdateDatabase applies the mutable settings synchronously.
func (o *Ops) UpdateDatabase(ctx context.Context, actor, name string, req api.DatabaseUpdateRequest) error {
	if !dbNameRE.MatchString(name) || protectedDB[name] {
		return problem.New(problem.NotFound, "No such database.")
	}
	var errs []problem.FieldError
	if g := req.General; g != nil {
		if g.Owner != "" {
			if _, err := sqlx.NewIdent(g.Owner); err != nil {
				errs = append(errs, field("/general/owner", "is not a valid role name", "pattern"))
			}
		}
		if g.ConnectionLimit != nil && *g.ConnectionLimit < -1 {
			errs = append(errs, field("/general/connectionLimit", "must be -1 or more", "out_of_range"))
		}
	}
	if r := req.Replication; r != nil {
		if _, ok := durabilityGUC[r.Durability]; !ok {
			errs = append(errs, field("/replication/durability", "must be async, sync or syncApply", "enum"))
		}
	}
	if s := req.Sharding; s != nil && !s.Enabled {
		return problem.New(problem.ImmutableField, "Sharding cannot be turned off.")
	}
	if l := req.Ledger; l != nil {
		switch l.Mode {
		case "", "optIn":
		case "off":
			return problem.New(problem.ImmutableField, "The ledger cannot be turned off.")
		default:
			return problem.New(problem.UnsupportedOp, "A database-wide ledger mode of "+l.Mode+" is not supported yet.")
		}
		if l.DigestIntervalSeconds != 0 && (l.DigestIntervalSeconds < 60 || l.DigestIntervalSeconds > 86400) {
			errs = append(errs, field("/ledger/digestIntervalSeconds", "must be between 60 and 86400", "out_of_range"))
		}
	}
	if len(errs) > 0 {
		return problem.Validation("The database update is invalid.", errs...)
	}
	db := sqlx.MustIdent(name)
	// Owner and connection limit run as the signed-in role: PostgreSQL decides.
	if g := req.General; g != nil {
		err := o.asRole(ctx, "postgres", actor, func(tx pgx.Tx) error {
			if ok, err := dbExists(ctx, tx, name); err != nil {
				return err
			} else if !ok {
				return problem.New(problem.NotFound, fmt.Sprintf("Database %q does not exist.", name))
			}
			if g.Owner != "" {
				if err := run(ctx, tx, stmt().Text("ALTER DATABASE").ID(db).Text("OWNER TO").ID(sqlx.MustIdent(g.Owner))); err != nil {
					return err
				}
			}
			if g.ConnectionLimit != nil {
				return run(ctx, tx, stmt().Text("ALTER DATABASE").ID(db).Text("CONNECTION LIMIT").Text(sqlx.Fixed(strconv.Itoa(*g.ConnectionLimit))))
			}
			return nil
		})
		if err != nil {
			return wrap(err)
		}
	}
	return wrap(o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		if ok, err := dbExists(ctx, tx, name); err != nil {
			return err
		} else if !ok {
			return problem.New(problem.NotFound, fmt.Sprintf("Database %q does not exist.", name))
		}
		var sharding bool
		var ledger string
		err := tx.QueryRow(ctx, "SELECT sharding_enabled, ledger_mode FROM kingres_cp.database_setting WHERE database_name = $1", name).Scan(&sharding, &ledger)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return problem.New(problem.Conflict, "This database was not created by the control plane; its settings cannot be changed here.")
			}
			return err
		}
		if s := req.Sharding; s != nil && s.Enabled && !sharding {
			if err := o.installExtensions(ctx, name, true, false); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE kingres_cp.database_setting SET sharding_enabled = true, bucket_count = coalesce(bucket_count, 128) WHERE database_name = $1", name); err != nil {
				return err
			}
		}
		if l := req.Ledger; l != nil {
			if l.Mode == "optIn" && ledger == "off" {
				if err := o.installExtensions(ctx, name, false, true); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, "UPDATE kingres_cp.database_setting SET ledger_mode = 'optIn' WHERE database_name = $1", name); err != nil {
					return err
				}
			}
			if l.DigestIntervalSeconds != 0 {
				if _, err := tx.Exec(ctx, "UPDATE kingres_cp.database_setting SET ledger_digest_interval = $2 WHERE database_name = $1", name, l.DigestIntervalSeconds); err != nil {
					return err
				}
			}
		}
		if r := req.Replication; r != nil {
			if err := o.setDurability(ctx, tx, name, r.Durability); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, "UPDATE kingres_cp.database_setting SET durability = $2 WHERE database_name = $1", name, r.Durability); err != nil {
				return err
			}
		}
		return nil
	}))
}
