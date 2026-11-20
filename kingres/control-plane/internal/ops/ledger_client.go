// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// Client keys, signature policies and the upgrade of the extension (phase 7, step 17; design 3.7 and 9.5).
//
// Every shard group has a chain and so a registry of client keys of its own (the three tables of kingres_ledger 0.3 are ledger tables that
// the group's own chain covers). A key is therefore registered, revoked and switched to a policy on every server, through the apply call the
// leader makes for roles: first a check here and on every server that can be reached (a server whose extension is older than 0.3 refuses the
// whole operation before anything changed), then the change here, then the change on the other servers; a server that cannot be reached gets
// the task replicate.apply, which waits for it and never gives up. Every apply is idempotent.

// ApplyLedgerClient is the kind of the apply call that carries a change of the client keys, of a signature policy or an upgrade.
const ApplyLedgerClient = "ledger.client"

// KindLedgerUpgrade is the task that runs ALTER EXTENSION kingres_ledger UPDATE in every group of a database.
const KindLedgerUpgrade = "ledger.upgrade"

// The texts that carry a refusal from another server (an apply error is text); each has a SQLSTATE of a class that is never retried.
const (
	textLedgerOff    = "SQLSTATE 22023: the ledger is not enabled on this database"
	textClientOld    = "SQLSTATE 0A000: the kingres_ledger extension of this database is older than 0.3"
	textClientExists = "SQLSTATE 22023: the client key is already registered"
	textClientRevoke = "SQLSTATE 22023: the client key is already revoked"
	textClientNone   = "SQLSTATE 22023: the client key is not registered"
	textPolicyReq    = "SQLSTATE 22023: the signature policy of the table is already required"
	textNoOpenSSL    = "SQLSTATE 0A000: this server cannot check attestations (it was built without OpenSSL)"
)

type ledgerClientOp struct {
	Op        string `json:"op"` // register, revoke, policy, upgrade
	Check     bool   `json:"check,omitempty"`
	Database  string `json:"database"`
	PublicKey []byte `json:"publicKey,omitempty"`
	Role      string `json:"role,omitempty"`
	Label     string `json:"label,omitempty"`
	KeyID     string `json:"keyId,omitempty"` // 32 hexadecimal characters
	Reason    string `json:"reason,omitempty"`
	Schema    string `json:"schema,omitempty"`
	Table     string `json:"table,omitempty"`
}

var keyID8RE = regexp.MustCompile(`^[0-9a-f]{8}$`)

func pgText(err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return fmt.Errorf("SQLSTATE %s: %s", pe.Code, pe.Message)
	}
	return err
}

// ledgerVersions reads the version of the extension in the database and the newest one that is installed.
func ledgerVersions(ctx context.Context, x execer) (have, want string, err error) {
	err = x.QueryRow(ctx, `SELECT
		coalesce((SELECT extversion FROM pg_catalog.pg_extension WHERE extname = 'kingres_ledger'), ''),
		coalesce((SELECT default_version FROM pg_catalog.pg_available_extensions WHERE name = 'kingres_ledger'), '')`).Scan(&have, &want)
	return
}

// needClientKeys detects version 0.3 by its objects, as the records of 0.2 are detected: the ledger must be there and the registry too.
func needClientKeys(ctx context.Context, x execer) error {
	var inst, keys bool
	if err := x.QueryRow(ctx, `SELECT to_regclass('kingres.ledger_blocks') IS NOT NULL, to_regclass('kingres.ledger_client_key') IS NOT NULL`).Scan(&inst, &keys); err != nil {
		return err
	}
	if !inst {
		return errors.New(textLedgerOff)
	}
	if !keys {
		return errors.New(textClientOld)
	}
	return nil
}

// clientProblem turns a refusal, here or from another server, into the problem of the contract; server is "" for this server.
func clientProblem(err error, server string) error {
	if err == nil {
		return nil
	}
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	where := ""
	if server != "" {
		where = " (server " + server + ")"
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, textLedgerOff):
		return problem.New(problem.Conflict, "The ledger is not enabled on this database"+where+".")
	case strings.Contains(msg, textClientOld):
		return problem.New(problem.UnsupportedOp, "The kingres_ledger extension of this database is older than 0.3"+where+"; upgrade it first (upgradeLedgerExtension).")
	case strings.Contains(msg, textNoOpenSSL):
		return problem.New(problem.UnsupportedOp, "This server was built without OpenSSL and cannot check attestations"+where+".")
	case strings.Contains(msg, textClientExists):
		return problem.New(problem.AlreadyExists, "This key is registered"+where+".")
	case strings.Contains(msg, textClientRevoke):
		return problem.New(problem.Conflict, "The key is already revoked"+where+".")
	case strings.Contains(msg, textClientNone):
		return problem.New(problem.NotFound, "The key does not exist"+where+".")
	case strings.Contains(msg, textPolicyReq):
		return problem.New(problem.Conflict, "The signature policy of the table is already required"+where+".")
	}
	return wrap(err)
}

// clientLocal runs one operation of the client keys on the local primary. With Check it refuses what the apply would refuse and changes nothing;
// without it the apply is idempotent (a key that is registered, revoked or a table that is required is a repeat, not an error).
func (o *Ops) clientLocal(ctx context.Context, op ledgerClientOp) error {
	if _, err := ledgerDB(op.Database); err != nil {
		return err
	}
	return o.asService(ctx, op.Database, func(tx pgx.Tx) error {
		switch op.Op {
		case "upgrade":
			have, want, err := ledgerVersions(ctx, tx)
			if err != nil {
				return err
			}
			if have == "" {
				return errors.New(textLedgerOff)
			}
			if op.Check || have == want || want == "" {
				return nil
			}
			if _, err := tx.Exec(ctx, "ALTER EXTENSION kingres_ledger UPDATE"); err != nil {
				return pgText(err)
			}
			return nil
		case "register":
			if err := needClientKeys(ctx, tx); err != nil {
				return err
			}
			sum := sha256.Sum256(op.PublicKey)
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.ledger_client_key WHERE key_id = $1)`, sum[:16]).Scan(&exists); err != nil {
				return err
			}
			if exists {
				if op.Check {
					return errors.New(textClientExists)
				}
				return nil
			}
			if op.Role != "" {
				var role bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_roles WHERE rolname = $1)`, op.Role).Scan(&role); err != nil {
					return err
				}
				if !role {
					return problem.Validation("The request is invalid.", field("/roleName", "This role does not exist.", "not_found"))
				}
			}
			if op.Check {
				return nil
			}
			var role *string
			if op.Role != "" {
				role = &op.Role
			}
			if _, err := tx.Exec(ctx, `SELECT kingres.ledger_client_key_register($1, $2::name, $3)`, op.PublicKey, role, op.Label); err != nil {
				var pe *pgconn.PgError
				if errors.As(err, &pe) {
					switch pe.Code {
					case "22023":
						return problem.Validation("The request is invalid.", field("/publicKey", pe.Message, "invalid"))
					case "0A000":
						return errors.New(textNoOpenSSL)
					}
				}
				return pgText(err)
			}
			return nil
		case "revoke":
			if err := needClientKeys(ctx, tx); err != nil {
				return err
			}
			id, err := hex.DecodeString(op.KeyID)
			if err != nil || len(id) != 16 {
				return errors.New(textClientNone)
			}
			var known, revoked bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM kingres.ledger_client_key WHERE key_id = $1),
				EXISTS (SELECT 1 FROM kingres.ledger_client_key_revocation WHERE key_id = $1)`, id).Scan(&known, &revoked); err != nil {
				return err
			}
			switch {
			case !known && op.Check:
				return errors.New(textClientNone)
			case !known:
				return nil // another group that never had the key
			case revoked && op.Check:
				return errors.New(textClientRevoke)
			case revoked, op.Check:
				return nil
			}
			if _, err := tx.Exec(ctx, `SELECT kingres.ledger_client_key_revoke($1, $2)`, id, op.Reason); err != nil {
				return pgText(err)
			}
			return nil
		case "policy":
			if err := needClientKeys(ctx, tx); err != nil {
				return err
			}
			if _, err := sqlx.QualifiedIdent(op.Schema, op.Table); err != nil {
				return problem.New(problem.NotFound, "No such table.")
			}
			var policy *string
			if err := tx.QueryRow(ctx, `SELECT (SELECT signature_policy FROM kingres.ledger_table WHERE relid = to_regclass($1))`, qualified(op.Schema, op.Table)).Scan(&policy); err != nil {
				return err
			}
			if policy == nil {
				return nil // the ledger table is on another group, or this group does not have the table
			}
			if *policy == "required" {
				if op.Check {
					return errors.New(textPolicyReq)
				}
				return nil
			}
			if op.Check {
				return nil
			}
			if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '15s'`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT kingres.ledger_set_signature_policy($1::regclass, 'required')`, qualified(op.Schema, op.Table)); err != nil {
				return pgText(err)
			}
			return nil
		}
		return fmt.Errorf("unknown operation %q", op.Op)
	})
}

// ledgerClientApply is ApplyLocal for the kind ApplyLedgerClient.
func (o *Ops) ledgerClientApply(ctx context.Context, raw json.RawMessage) error {
	var op ledgerClientOp
	if err := strictJSON(raw, &op); err != nil {
		return err
	}
	switch op.Op {
	case "register", "revoke", "policy", "upgrade":
	default:
		return errors.New("the operation is not valid")
	}
	if err := o.clientLocal(ctx, op); err != nil {
		var p *problem.Problem
		if errors.As(err, &p) {
			return fmt.Errorf("SQLSTATE 22023: %s", p.Detail)
		}
		return err
	}
	return nil
}

// clientEverywhere checks here and on every other server that can be reached, runs the operation here, and brings the other servers up to date.
func (o *Ops) clientEverywhere(ctx context.Context, op ledgerClientOp) error {
	chk := op
	chk.Check = true
	if err := o.clientLocal(ctx, chk); err != nil {
		return clientProblem(err, "")
	}
	if o.Fanout != nil {
		for _, s := range o.others(ctx) {
			c, cancel := context.WithTimeout(ctx, 20*time.Second)
			err := o.Fanout.Apply(c, s.ID, ApplyLedgerClient, chk)
			cancel()
			if err == nil || errors.Is(err, peerclient.ErrUnreachable) || healable(err) {
				continue
			}
			return clientProblem(err, s.ID)
		}
	}
	if err := o.clientLocal(ctx, op); err != nil {
		return clientProblem(err, "")
	}
	o.replicate(ctx, replicateInput{Kind: ApplyLedgerClient, Ledger: &op})
	return nil
}

type clientRow struct {
	id        string
	public    []byte
	role      *string
	label     string
	by        string
	at        time.Time
	revokedAt *time.Time
	reason    *string
	revokedBy *string
}

func (o *Ops) readClientKeys(ctx context.Context, db string) ([]clientRow, error) {
	var rows []clientRow
	err := o.asService(ctx, db, func(tx pgx.Tx) error {
		if err := needClientKeys(ctx, tx); err != nil {
			return err
		}
		r, err := tx.Query(ctx, `SELECT encode(k.key_id, 'hex'), k.public_key, k.role_name::text, k.label, k.registered_by::text, k.registered_at,
				v.revoked_at, v.reason, v.revoked_by::text
			FROM kingres.ledger_client_key k LEFT JOIN kingres.ledger_client_key_revocation v ON v.key_id = k.key_id
			ORDER BY k.registered_at, k.key_id`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var c clientRow
			if err := r.Scan(&c.id, &c.public, &c.role, &c.label, &c.by, &c.at, &c.revokedAt, &c.reason, &c.revokedBy); err != nil {
				return err
			}
			rows = append(rows, c)
		}
		return r.Err()
	})
	return rows, err
}

func (o *Ops) clientActors(ctx context.Context, db string) (map[string][2]string, error) {
	out := map[string][2]string{}
	err := o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT key_id, coalesce(registered_by, ''), coalesce(revoked_by, '') FROM kingres_cp.ledger_client_key_actor WHERE database_name = $1`, db)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id, a, b string
			if err := r.Scan(&id, &a, &b); err != nil {
				return err
			}
			out[id] = [2]string{a, b}
		}
		return r.Err()
	})
	return out, err
}

func (o *Ops) rememberClientActor(ctx context.Context, db, keyID, actor string, revoked bool) error {
	col := "registered_by"
	if revoked {
		col = "revoked_by"
	}
	return o.asService(ctx, sysdb.Database, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kingres_cp.ledger_client_key_actor (database_name, key_id, `+col+`) VALUES ($1, $2, $3)
			ON CONFLICT (database_name, key_id) DO UPDATE SET `+col+` = EXCLUDED.`+col, db, keyID, actor)
		return err
	})
}

func toClientKey(c clientRow, actors map[string][2]string) api.LedgerClientKey {
	by := actors[c.id][0]
	if by == "" {
		by = c.by
		if by == instance.ServiceRole {
			by = "system"
		}
	}
	k := api.LedgerClientKey{KeyID: c.id[:8], PublicKey: base64.StdEncoding.EncodeToString(c.public), RoleName: c.role, Label: c.label, RegisteredBy: by,
		RegisteredAt: c.at.UTC(), RevocationReason: c.reason}
	if c.revokedAt != nil {
		t := c.revokedAt.UTC()
		k.RevokedAt = &t
	}
	return k
}

// ListClientKeys lists the keys of the database as this server's group holds them.
func (o *Ops) ListClientKeys(ctx context.Context, db string) (api.LedgerClientKeyList, error) {
	out := api.LedgerClientKeyList{Items: []api.LedgerClientKey{}}
	if _, err := ledgerDB(db); err != nil {
		return out, err
	}
	rows, err := o.readClientKeys(ctx, db)
	if err != nil {
		return out, clientProblem(err, "")
	}
	actors, err := o.clientActors(ctx, db)
	if err != nil {
		return out, wrap(err)
	}
	for _, r := range rows {
		out.Items = append(out.Items, toClientKey(r, actors))
	}
	return out, nil
}

func (o *Ops) oneClientKey(ctx context.Context, db, id string) (api.LedgerClientKey, error) {
	rows, err := o.readClientKeys(ctx, db)
	if err != nil {
		return api.LedgerClientKey{}, clientProblem(err, "")
	}
	actors, err := o.clientActors(ctx, db)
	if err != nil {
		return api.LedgerClientKey{}, wrap(err)
	}
	for _, r := range rows {
		if r.id == id {
			return toClientKey(r, actors), nil
		}
	}
	return api.LedgerClientKey{}, problem.New(problem.NotFound, "The key does not exist.")
}

// RegisterClientKey registers a public Ed25519 key in every group of the database.
func (o *Ops) RegisterClientKey(ctx context.Context, db, actor string, req api.RegisterLedgerClientKeyRequest) (api.LedgerClientKey, error) {
	if _, err := ledgerDB(db); err != nil {
		return api.LedgerClientKey{}, err
	}
	var errs []problem.FieldError
	pub, err := base64.StdEncoding.DecodeString(req.PublicKey)
	if err != nil || len(pub) != 32 {
		errs = append(errs, field("/publicKey", "Give 32 bytes as standard base64.", "invalid"))
	}
	if n := len([]rune(req.Label)); n < 1 || n > 100 {
		errs = append(errs, field("/label", "Give a label of 1 to 100 characters.", "length"))
	}
	if len(req.RoleName) > 63 {
		errs = append(errs, field("/roleName", "A role name has at most 63 bytes.", "length"))
	}
	if len(errs) > 0 {
		return api.LedgerClientKey{}, problem.Validation("The request is invalid.", errs...)
	}
	sum := sha256.Sum256(pub)
	id := hex.EncodeToString(sum[:16])
	// the first eight characters name the key in the API: two keys that share them could not both be addressed
	rows, err := o.readClientKeys(ctx, db)
	if err != nil {
		return api.LedgerClientKey{}, clientProblem(err, "")
	}
	for _, r := range rows {
		if r.id != id && r.id[:8] == id[:8] {
			return api.LedgerClientKey{}, problem.New(problem.Conflict, "Another registered key has the same short identifier; make another key pair.")
		}
	}
	op := ledgerClientOp{Op: "register", Database: db, PublicKey: pub, Role: req.RoleName, Label: req.Label}
	if err := o.clientEverywhere(ctx, op); err != nil {
		return api.LedgerClientKey{}, err
	}
	if err := o.rememberClientActor(ctx, db, id, actor, false); err != nil {
		return api.LedgerClientKey{}, wrap(fmt.Errorf("recording who registered key %s: %w", id[:8], err))
	}
	return o.oneClientKey(ctx, db, id)
}

// RevokeClientKey revokes a key (named by its eight characters) in every group.
func (o *Ops) RevokeClientKey(ctx context.Context, db, actor, short string, req api.RevokeLedgerClientKeyRequest) (api.LedgerClientKey, error) {
	if _, err := ledgerDB(db); err != nil {
		return api.LedgerClientKey{}, err
	}
	if !keyID8RE.MatchString(short) {
		return api.LedgerClientKey{}, problem.New(problem.NotFound, "The key does not exist.")
	}
	if n := len([]rune(req.Reason)); n < 1 || n > 200 {
		return api.LedgerClientKey{}, problem.Validation("The request is invalid.", field("/reason", "Give a reason of 1 to 200 characters.", "length"))
	}
	rows, err := o.readClientKeys(ctx, db)
	if err != nil {
		return api.LedgerClientKey{}, clientProblem(err, "")
	}
	id := ""
	for _, r := range rows {
		if r.id[:8] == short {
			id = r.id
		}
	}
	if id == "" {
		return api.LedgerClientKey{}, problem.New(problem.NotFound, "The key does not exist.")
	}
	if err := o.clientEverywhere(ctx, ledgerClientOp{Op: "revoke", Database: db, KeyID: id, Reason: req.Reason}); err != nil {
		return api.LedgerClientKey{}, err
	}
	if err := o.rememberClientActor(ctx, db, id, actor, true); err != nil {
		return api.LedgerClientKey{}, wrap(fmt.Errorf("recording who revoked key %s: %w", short, err))
	}
	return o.oneClientKey(ctx, db, id)
}

// SetSignaturePolicy switches a ledger table to the policy required in every group that holds it. The caller has checked that the table is a
// ledger table of the catalog.
func (o *Ops) SetSignaturePolicy(ctx context.Context, db, schema, table string, req api.SignaturePolicyRequest) error {
	if _, err := ledgerDB(db); err != nil {
		return err
	}
	if req.Policy != "required" {
		return problem.Validation("The request is invalid.", field("/policy", "must be required", "enum"))
	}
	if _, err := sqlx.QualifiedIdent(schema, table); err != nil {
		return problem.New(problem.NotFound, "No such table.")
	}
	return o.clientEverywhere(ctx, ledgerClientOp{Op: "policy", Database: db, Schema: schema, Table: table})
}

type upgradeParams struct {
	Database string   `json:"database"`
	Servers  []string `json:"servers"`
}

// PrepareUpgrade checks that the database has the ledger and returns the input of the task ledger.upgrade.
func (o *Ops) PrepareUpgrade(ctx context.Context, db string) (any, error) {
	if _, err := ledgerDB(db); err != nil {
		return nil, err
	}
	if err := o.asService(ctx, db, func(tx pgx.Tx) error {
		have, _, err := ledgerVersions(ctx, tx)
		if err != nil {
			return err
		}
		if have == "" {
			return errors.New(textLedgerOff)
		}
		return nil
	}); err != nil {
		return nil, clientProblem(err, "")
	}
	return upgradeParams{Database: db, Servers: ids(o.others(ctx))}, nil
}

func (o *Ops) ledgerUpgradeDefinition() task.Definition {
	return task.Definition{Kind: KindLedgerUpgrade, Dynamic: func(in json.RawMessage) ([]task.Step, error) {
		var p upgradeParams
		if err := json.Unmarshal(in, &p); err != nil {
			return nil, err
		}
		steps := []task.Step{{Name: "Upgrade kingres_ledger on this server", Run: func(ctx context.Context, env *task.Env) error {
			cur, err := inputOf[upgradeParams](env.Task)
			if err != nil {
				return err
			}
			if err := o.clientLocal(ctx, ledgerClientOp{Op: "upgrade", Database: cur.Database}); err != nil {
				return fmt.Errorf("on this server: %w", err)
			}
			return nil
		}}}
		return append(steps, o.perServer(p.Servers, "Upgrade kingres_ledger on server %s", func(ctx context.Context, env *task.Env, server string) error {
			cur, err := inputOf[upgradeParams](env.Task)
			if err != nil {
				return err
			}
			return o.Fanout.Apply(ctx, server, ApplyLedgerClient, ledgerClientOp{Op: "upgrade", Database: cur.Database})
		})...), nil
	}}
}

// SyncClientKeys copies the client keys of this server's group (the registry of the manager leader) to every other server, revocations
// included, through the apply kind of the operations above. It is idempotent: a key a group has is skipped, so it is run when the log of a
// chain is created for the first time (the collector's OnNewChain) and finds a shard group added after the keys were registered. A server on
// a version without client keys is skipped (the upgrade is what brings it). The policies of the tables are not copied: a ledger table lives on
// one group, so a group that is added has none.
func (o *Ops) SyncClientKeys(ctx context.Context, db string) error {
	if o.Fanout == nil {
		return nil
	}
	rows, err := o.readClientKeys(ctx, db)
	if err != nil {
		if strings.Contains(err.Error(), textClientOld) || strings.Contains(err.Error(), textLedgerOff) {
			return nil
		}
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	var firstErr error
	for _, s := range o.others(ctx) {
		for _, r := range rows {
			ops := []ledgerClientOp{{Op: "register", Database: db, PublicKey: r.public, Label: r.label}}
			if r.role != nil {
				ops[0].Role = *r.role
			}
			if r.revokedAt != nil {
				reason := "revoked"
				if r.reason != nil && *r.reason != "" {
					reason = *r.reason
				}
				id := r.id
				ops = append(ops, ledgerClientOp{Op: "revoke", Database: db, KeyID: id, Reason: reason})
			}
			for _, op := range ops {
				c, cancel := context.WithTimeout(ctx, 20*time.Second)
				err := o.Fanout.Apply(c, s.ID, ApplyLedgerClient, op)
				cancel()
				if err != nil {
					if strings.Contains(err.Error(), textClientOld) || strings.Contains(err.Error(), textLedgerOff) {
						break // this server is not ready for client keys: the upgrade brings it
					}
					if firstErr == nil {
						firstErr = fmt.Errorf("server %s: %w", s.ID, err)
					}
					break
				}
			}
		}
	}
	return firstErr
}
