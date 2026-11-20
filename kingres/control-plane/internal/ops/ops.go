// SPDX-License-Identifier: AGPL-3.0-only

// Package ops implements the operations of the contract that change the
// cluster: roles, grants, databases, tables, ledger and the query workbench.
//
// Rules that hold for everything in this package:
//
//   - No SQL is built by concatenating request text: identifiers go through
//     sqlx.Ident, values are bind parameters (sqlx.Literal only where
//     PostgreSQL has no parameters, for verifiers and enum-checked values),
//     keywords come from closed sqlx.KeywordSets.
//   - Role and grant changes run on a connection of the signed-in role
//     (Proxy), so PostgreSQL's permission checks apply in addition to the
//     cluster-role check. Only what needs superuser rights (extensions,
//     CREATE DATABASE ... with the shard catalog, ledger functions) uses the
//     service connection.
//   - Errors are *problem.Problem values; nothing returned to a client
//     contains a password, a verifier or driver internals.
package ops

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// ServiceConnector opens connections as the service role.
type ServiceConnector interface {
	ServiceConn(ctx context.Context, db string) (*pgx.Conn, error)
}

// RoleConnector opens connections as a signed-in role.
type RoleConnector interface {
	Connect(ctx context.Context, db, role string) (*pgx.Conn, error)
	Revoke(ctx context.Context, role string)
}

// Ops carries the dependencies of the operations.
type Ops struct {
	// Service opens service connections; Roles opens role connections.
	Service ServiceConnector
	Roles   RoleConnector
	// SessionEnder ends the sessions of a role on this server and forgets cached
	// role checks (the HTTP layer provides it); nil does nothing.
	SessionEnder func(role string)
	// Instances writes pg_hba.conf and reloads the local instance.
	Instances InstanceControl
	// WritableIDs lists the instance directories of this server that are a primary: the
	// ones the access rules are written to and the start, stop and restart of this
	// server's instance act on (nil: instance 1 only, the layout of a server that never
	// had a failover).
	WritableIDs func() []int
	// EnforcedPrimary tells whether this server runs the primary of a group that has a standby: a stop or restart of
	// its instances is then never run on a record that could not be written (F2-09): the exclusion of a failover
	// lives in that record. nil: no group is enforced.
	EnforcedPrimary func() bool
	// Recorder writes the tasks of instance start and stop (PeerRecorder on a
	// server that may not be the leader); nil records in this server's own task
	// engine.
	Recorder TaskRecorder
	// Fanout reaches the other servers (nil: this server is alone) and Local is
	// this server's cluster service, which applies roles and rules locally.
	Fanout Fanout
	Local  *cluster.Service
	// ServerHost and PGPort identify this server in the shard catalog.
	ServerHost string
	PGPort     int
	// InstanceOwner tells whether an instance (its id in the shard catalog) is one of
	// the named server's. A peer may record and settle tasks only of its own
	// instances (CP2-20); nil refuses every such call.
	InstanceOwner func(ctx context.Context, instanceID, server string) (bool, error)
	// OnReplicaCount is called after the cluster setting defaultReplicaCount was stored
	// (also when the value is the same): the standby task checks the standbys at once.
	OnReplicaCount func(ctx context.Context) (*task.Task, error)
	// LedgerDigest and LedgerDigests, when set, make generateLedgerDigest and listLedgerDigests work on every shard group and on the stored
	// digests of the cluster (ledger phase 7, step 5: internal/ledgerround); nil keeps the digest of this server's own chain.
	LedgerDigest  func(ctx context.Context, db, actor string) (api.LedgerDigest, error)
	LedgerDigests func(ctx context.Context, db string, p store.Page) (api.LedgerDigestPage, error)
	// LedgerRetireDatabase, when set, ends the publication of the chains of a database before it is dropped: a final round, the
	// chain-retired events and the aggregate leaf in which every member is retired (step 6). It does nothing for a database with no log.
	LedgerRetireDatabase func(ctx context.Context, db string) error
	// LedgerBundleBuild writes the bundle of a database to w (step 20); files are further files by their path in the bundle (the inclusion
	// proofs). LedgerPublished tells whether the database has a published log. BundleDir is where the files of the bundles are kept.
	LedgerBundleBuild func(ctx context.Context, db string, req api.LedgerBundleRequest, files map[string][]byte, w io.Writer) error
	LedgerPublished   func(ctx context.Context, db string) (bool, error)
	// LedgerChainDiverged and LedgerEpochStart serve acknowledgeLedgerDivergence (step 20): whether the chain of a group is diverged, and the
	// start of the new epoch (a task step, idempotent).
	LedgerChainDiverged func(ctx context.Context, db string, group int32) (bool, error)
	LedgerEpochStart    func(ctx context.Context, db string, group int32, kind, reason, actor string) error
	// LedgerAudit is the check "publication" of a verification: the findings of the audit of the stored logs of a database (nil: not supported).
	LedgerAudit func(ctx context.Context, db string) ([]string, error)
	BundleDir   string
	// BG bounds background task runs; Wait waits for them.
	BG  context.Context
	Log *slog.Logger

	engineP    atomic.Pointer[task.Engine]
	wg         sync.WaitGroup
	applyState applyState
	life       lifecycle
	// stmts tracks the running workbench statements (cancel.go).
	stmts statements
}

// SetEngine installs the task engine once the system database exists.
func (o *Ops) SetEngine(e *task.Engine) { o.engineP.Store(e) }

// Engine returns the task engine, or nil before it exists.
func (o *Ops) Engine() *task.Engine { return o.engineP.Load() }

func (o *Ops) bg() context.Context {
	if o.BG == nil {
		return context.Background()
	}
	return o.BG
}

// Go runs f in the background, waited for by Wait.
func (o *Ops) Go(f func()) {
	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		f()
	}()
}

// Background is the context of background task runs.
func (o *Ops) Background() context.Context { return o.bg() }

// Wait waits for the background task runs started by StartTask.
func (o *Ops) Wait() { o.wg.Wait() }

// execer is what both pgx.Conn and pgx.Tx offer.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

var errBuild = errors.New("internal error: a statement could not be built")

func run(ctx context.Context, x execer, b *sqlx.Builder) error {
	st, err := b.Build()
	if err != nil {
		return errBuild
	}
	_, err = x.Exec(ctx, st.SQL, st.Args...)
	return err
}

func stmt() *sqlx.Builder { return new(sqlx.Builder) }

// asRole runs f in a transaction on a connection of role to database db.
func (o *Ops) asRole(ctx context.Context, db, role string, f func(tx pgx.Tx) error) error {
	if role == "" {
		// A change replayed from the leader: PostgreSQL's checks already passed there.
		return o.asService(ctx, db, f)
	}
	if o.Roles == nil {
		return problem.New(problem.UnsupportedOp, "This server cannot act as the signed-in role (no managed instance).")
	}
	c, err := o.Roles.Connect(ctx, db, role)
	if err != nil {
		o.logConnect(role, err)
		return connectProblem(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return inTx(ctx, c, f)
}

// asService runs f in a transaction on a service connection to db.
func (o *Ops) asService(ctx context.Context, db string, f func(tx pgx.Tx) error) error {
	if o.Service == nil {
		return problem.New(problem.UnsupportedOp, "This server has no managed instance.")
	}
	c, err := o.Service.ServiceConn(ctx, db)
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	return inTx(ctx, c, f)
}

func inTx(ctx context.Context, c *pgx.Conn, f func(tx pgx.Tx) error) error {
	tx, err := c.Begin(ctx)
	if err != nil {
		return connectProblem(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := f(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return PGProblem(err)
	}
	return nil
}

func connectProblem(err error) error {
	var pe *pgconn.PgError
	switch {
	case errors.Is(err, cluster.ErrNotEnabled):
		return problem.New(problem.SessionExpired, "The session has no database access any more; sign in again.")
	case errors.As(err, &pe) && len(pe.Code) >= 2 && pe.Code[:2] == "28":
		return problem.New(problem.Forbidden, "PostgreSQL does not allow this role to connect to the database.")
	case errors.As(err, &pe) && pe.Code == "3D000":
		return problem.New(problem.NotFound, "The database does not exist.")
	case errors.As(err, &pe) && pe.Code == "42501":
		return problem.New(problem.Forbidden, "PostgreSQL does not allow this role to connect to the database.")
	}
	return problem.New(problem.ServiceUnavailable, "The local PostgreSQL instance is not available.").WithRetryAfter(5)
}

// PGProblem maps a PostgreSQL error to a problem without leaking internals:
// only the SQLSTATE and PostgreSQL's own message for errors the caller can act
// on (which never contain passwords or verifiers).
func PGProblem(err error) error {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		if errors.Is(err, errBuild) {
			return problem.New(problem.InternalError, "An unexpected error occurred.")
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return problem.New(problem.ServiceUnavailable, "The operation timed out.").WithRetryAfter(5)
		}
		return problem.New(problem.ServiceUnavailable, "The local PostgreSQL instance is not available.").WithRetryAfter(5)
	}
	switch pe.Code {
	case "42501":
		return problem.New(problem.Forbidden, "PostgreSQL refused the operation: insufficient privilege.")
	case "42710", "42P04", "42P06", "42P07", "23505":
		return problem.New(problem.AlreadyExists, "The object already exists.")
	case "2BP01", "55006":
		return problem.New(problem.RoleInUse, "The object is in use: "+pe.Message)
	case "42704", "3D000", "3F000", "42P01":
		return problem.New(problem.NotFound, "PostgreSQL: "+pe.Message)
	case "57014":
		return problem.New(problem.Unprocessable, "The statement was canceled (timeout).")
	}
	if len(pe.Code) >= 2 && (pe.Code[:2] == "08" || pe.Code[:2] == "53" || pe.Code[:2] == "57") {
		return problem.New(problem.ServiceUnavailable, "The local PostgreSQL instance is not available.").WithRetryAfter(5)
	}
	return problem.SQL(pe.Message, pe.Code, int(pe.Position))
}

func (o *Ops) logConnect(role string, err error) {
	if o.Log != nil {
		o.Log.Warn("connecting as the signed-in role failed", "role", role, "error", err)
	}
}
