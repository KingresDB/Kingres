// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extgate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/extguard"
)

// CatalogStepName is the name of the one step of the reconcile task that records
// the standbys in the sharding catalog (kingres_shard 0.5, call sequence 1:
// kingres.add_node(..., 'standby')). Nothing else in this package knows the
// catalog's idea of a standby.
const CatalogStepName = "standby.register_in_catalog"

// SQLSTATEs of the extension (README of kingres_shard, "SQLSTATEs of the
// answers"): kingresd decides by the code, never by the text.
const (
	sqlstateNodeNoAnswer   = "KS007" // a node accepted the session and did not answer: do not repeat against it
	sqlstateCatalogBusy    = "KS008" // another catalog call is running: repeat later
	sqlstateCannotCommit   = "KS009" // a primary's synchronous standbys do not confirm: fix the list first
	sqlstateWrongNode      = "KS010" // the call went to the wrong node
	sqlstateLockNotAvail   = "55P03" // a lock held on a node: repeat when it is gone
	sqlstateAlreadyThere   = "42710" // the node is registered already
	sqlstateNoSuchNode     = "42704" // the node (or the group) does not exist
	sqlstateStillHasMember = "55006" // a primary that still has standbys
)

// CatalogError is a refusal of the extension, with its SQLSTATE. Retry says
// whether repeating the same call later can succeed without anybody changing
// anything (the callers of the step repeat on the next check; none loops).
type CatalogError struct {
	Call  string
	Code  string
	Msg   string
	Retry bool
}

func (e *CatalogError) Error() string {
	return fmt.Sprintf("the catalog call %s was refused (%s): %s", e.Call, e.Code, e.Msg)
}

// classify turns an error of pgx into a CatalogError for the SQLSTATEs the
// extension documents; any other error is returned as it is (a broken
// connection or a deadline means "the call did not finish": repeat it, sequence 9).
func classify(call string, err error) error {
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case sqlstateNodeNoAnswer, sqlstateCatalogBusy, sqlstateCannotCommit, sqlstateLockNotAvail:
		return &CatalogError{Call: call, Code: pe.Code, Msg: pe.Message, Retry: true}
	case sqlstateWrongNode:
		return &CatalogError{Call: call, Code: pe.Code, Msg: pe.Message}
	}
	return err
}

// IsCatalogCode tells whether err is a CatalogError with the SQLSTATE code.
func IsCatalogCode(err error, code string) bool {
	var ce *CatalogError
	return errors.As(err, &ce) && ce.Code == code
}

// CatalogRegistrar registers standbys in the sharding catalog as nodes with the
// role standby (they own no buckets and receive no forwarded statements).
type CatalogRegistrar interface {
	// Register adds the standby (idempotent: registering a standby that is in the
	// catalog returns its node id) and returns the node id. It runs on the manager
	// leader, which is where the catalog can be written.
	Register(ctx context.Context, e Entry) (int32, error)
	// Unregister removes the standby from the catalog (idempotent). The caller has
	// already taken the standby's name out of the list of its primary (sequence 8:
	// the list first); a refusal with KS009 says that the list still names it.
	Unregister(ctx context.Context, e Entry) error
}

// PGRegistrar is the registrar over the extension's calls. Conn opens a service
// connection to the system database of the instance that serves the map group.
type PGRegistrar struct {
	Conn func(ctx context.Context) (*pgx.Conn, error)
	// Timeout is the deadline of one call; its expiry means "did not finish" and the
	// call is repeated by the next check (sequence 9). The extension bounds a call
	// by 2 x node_timeout + 2 s for each node that does not answer, so the default
	// of 5 minutes covers two stuck nodes at the default node_timeout of 60 s.
	Timeout time.Duration
	Log     *slog.Logger
	// Remote opens a service connection to another primary of the sharding catalog (host and port as the
	// catalog names them). The read-back of a registration or a removal reads the row on every primary, not
	// on the coordinator only (FO-08): the coordinator is the first to commit, so its row proves nothing about a
	// node that went silent at the very end. nil: only the coordinator is read.
	Remote func(ctx context.Context, host string, port int) (*pgx.Conn, error)
	// Every sharded database has a catalog of its own (kingres.node in that database, with its own node ids): a
	// standby that is registered only in the system database is unknown to the databases of the clients, and a
	// failover could not move their catalogs. Databases lists the databases that have a catalog besides the
	// system database; DBConn and DBRemote open a connection to one of them (the coordinator, and another primary).
	// Skip says that a server is dead and is not read back (nil: every primary is read).
	Skip      func(host string) bool
	Databases func(ctx context.Context) ([]string, error)
	DBConn    func(ctx context.Context, db string) (*pgx.Conn, error)
	DBRemote  func(ctx context.Context, host string, port int, db string) (*pgx.Conn, error)
}

// in is the registrar of one other database: the same calls on its catalog.
func (r *PGRegistrar) in(db string) *PGRegistrar {
	c := *r
	c.Databases, c.DBConn, c.DBRemote = nil, nil, nil
	// The sessions of a database of a client are the service role's, a superuser's, and the registrar calls and reads kingres.* on them: nothing is read or called until
	// extguard says that the schema is the extension's (the owner of the database could have made a schema kingres of its own).
	c.Conn = func(ctx context.Context) (*pgx.Conn, error) {
		return trusted(ctx, db, func() (*pgx.Conn, error) { return r.DBConn(ctx, db) })
	}
	if r.DBRemote != nil {
		c.Remote = func(ctx context.Context, host string, port int) (*pgx.Conn, error) {
			return trusted(ctx, db, func() (*pgx.Conn, error) { return r.DBRemote(ctx, host, port, db) })
		}
	}
	return &c
}

// others lists the other databases that have a catalog (none when the registrar was not given the means).
func (r *PGRegistrar) others(ctx context.Context) ([]string, error) {
	if r.Databases == nil || r.DBConn == nil {
		return nil, nil
	}
	return r.Databases(ctx)
}

// EnsureEverywhere registers the standby in the catalog of every other database where it is not (idempotent: a
// standby that is there is found by its host and port and left alone). The system database's node id is the
// one the records keep; the other databases have ids of their own, found again by host and port.
func (r *PGRegistrar) EnsureEverywhere(ctx context.Context, e Entry) error {
	dbs, err := r.others(ctx)
	if err != nil {
		return err
	}
	e.CatalogNode = 0
	for _, db := range dbs {
		if _, err := r.in(db).register(ctx, e); err != nil {
			return fmt.Errorf("registering in the catalog of database %s: %w", db, err)
		}
	}
	return nil
}

func (r *PGRegistrar) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 5 * time.Minute
}

func (r *PGRegistrar) logger() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// find returns the catalog node of host:port, with its role and group.
func find(ctx context.Context, c *pgx.Conn, host string, port int) (id int32, role string, group int32, found bool, err error) {
	err = c.QueryRow(ctx, `SELECT node_id, role, group_id FROM kingres.node WHERE host = $1 AND port = $2`, host, port).Scan(&id, &role, &group)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, "", 0, false, nil
	}
	return id, role, group, err == nil, err
}

// Register implements CatalogRegistrar (sequence 1, steps 1 and 3).
func (r *PGRegistrar) Register(ctx context.Context, e Entry) (int32, error) {
	id, err := r.register(ctx, e)
	if err != nil {
		return 0, err
	}
	if err := r.EnsureEverywhere(ctx, e); err != nil {
		return 0, err
	}
	return id, nil
}

// register is Register for the catalog this registrar's Conn opens.
func (r *PGRegistrar) register(ctx context.Context, e Entry) (int32, error) {
	if e.Port == 0 {
		return 0, errors.New("the standby has no port yet")
	}
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	c, err := r.Conn(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if err := readCommitted(ctx, c); err != nil {
		return 0, err
	}
	id, role, group, found, err := find(ctx, c, e.Server, e.Port)
	if err != nil {
		return 0, err
	}
	if found {
		if role != "standby" || group != e.Group {
			return 0, fmt.Errorf("the catalog has %s:%d as a %s of group %d, not as a standby of group %d", e.Server, e.Port, role, group, e.Group)
		}
		// Found on the coordinator is not proof for the other primaries: the repeat of a step that did not finish reads them too.
		if err := r.readBackEverywhere(ctx, c, e, id, e.Port, true); err != nil {
			return 0, err
		}
		return id, nil
	}
	err = c.QueryRow(ctx, `SELECT kingres.add_node($1, $2, $3, 'standby')`, e.Server, e.Port, e.Group).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == sqlstateAlreadyThere {
			// registered meanwhile (a call that did not finish, sequence 9): use its id
			if id, role, group, found, ferr := find(ctx, c, e.Server, e.Port); ferr == nil && found && role == "standby" && group == e.Group {
				// Found on the coordinator is not proof for the other primaries, and this branch is the repeat after a call that
				// did not finish, the case the read-back is for (F2-10).
				if berr := r.readBackEverywhere(ctx, c, e, id, e.Port, true); berr != nil {
					return 0, berr
				}
				return id, nil
			}
		}
		return 0, classify("add_node", err)
	}
	// A success of add_node is not proof that the row is there (review S12-01): read it back, in the same session.
	if bid, brole, bgroup, bfound, berr := find(ctx, c, e.Server, e.Port); berr != nil || !bfound || bid != id || brole != "standby" || bgroup != e.Group {
		return 0, fmt.Errorf("add_node answered node %d, and the catalog does not show it as the standby of group %d (found %v, err %v): repeated by the next check", id, e.Group, bfound, berr)
	}
	if err := r.readBackEverywhere(ctx, c, e, id, e.Port, true); err != nil {
		return 0, err
	}
	// Step 3, in a session of its own (the third review of the extension: a control call gets a session
	// that is not reused for anything else): the row must show the standby streaming.
	_ = c.Close(context.WithoutCancel(ctx))
	if c2, err := r.Conn(ctx); err == nil {
		defer func() { _ = c2.Close(context.WithoutCancel(ctx)) }()
		var state, sync, problem string
		if err := c2.QueryRow(ctx, `SELECT coalesce(replication_state, ''), coalesce(sync_state, ''), coalesce(problem, '') FROM kingres.standby_status() WHERE node_id = $1`, id).
			Scan(&state, &sync, &problem); err == nil && problem != "" {
			r.logger().Warn("standby: the catalog reports a problem with a registered standby", "node", id, "state", state, "sync", sync, "problem", problem)
		}
	}
	return id, nil
}

// Unregister implements CatalogRegistrar (sequence 8, step 2 and 3).
func (r *PGRegistrar) Unregister(ctx context.Context, e Entry) error {
	// The other databases first (their node ids are found by host and port), the system database last: its row is
	// the record that the removal is not done yet.
	if dbs, err := r.others(ctx); err != nil {
		return err
	} else {
		o := e
		o.CatalogNode = 0
		for _, db := range dbs {
			if err := r.in(db).unregister(ctx, o); err != nil {
				return fmt.Errorf("removing from the catalog of database %s: %w", db, err)
			}
		}
	}
	return r.unregister(ctx, e)
}

// unregister is Unregister for the catalog this registrar's Conn opens.
func (r *PGRegistrar) unregister(ctx context.Context, e Entry) error {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	c, err := r.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close(context.WithoutCancel(ctx)) }()
	if err := readCommitted(ctx, c); err != nil {
		return err
	}
	id := e.CatalogNode
	if id == 0 {
		if e.Port == 0 {
			return nil
		}
		fid, role, group, found, err := find(ctx, c, e.Server, e.Port)
		if err != nil {
			return err
		}
		if !found || role != "standby" || group != e.Group {
			return nil // never registered, or the row is another node's
		}
		id = fid
	}
	// The row of the catalog must still be that standby's: a node id is not reused,
	// but a row that names another host or port is not ours to remove.
	var host string
	var port int
	var role string
	if err := c.QueryRow(ctx, `SELECT host, port, role FROM kingres.node WHERE node_id = $1`, id).Scan(&host, &port, &role); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	if role != "standby" || host != e.Server || (e.Port != 0 && port != e.Port) {
		return nil
	}
	if _, err := c.Exec(ctx, `SELECT kingres.remove_node($1)`, id); err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == sqlstateNoSuchNode {
			return nil
		}
		return classify("remove_node", err)
	}
	// Read back: the row must be gone (review S12-01); if not, the next check repeats the removal.
	if _, _, _, still, ferr := find(ctx, c, e.Server, portOf(e, port)); ferr == nil && still {
		return fmt.Errorf("remove_node answered, and the catalog still has node %d", id)
	}
	return r.readBackEverywhere(ctx, c, e, id, portOf(e, port), false)
}

// portOf is the port of the row that was removed.
func portOf(e Entry, rowPort int) int {
	if rowPort != 0 {
		return rowPort
	}
	return e.Port
}

// readCommitted makes the session of a control call run READ COMMITTED explicitly: a role whose default is
// REPEATABLE READ or SERIALIZABLE cannot call promote_node or recover_prepared_transactions (25001).
func readCommitted(ctx context.Context, c *pgx.Conn) error {
	_, err := c.Exec(ctx, `SET SESSION CHARACTERISTICS AS TRANSACTION ISOLATION LEVEL READ COMMITTED`)
	return err
}

// ErrCatalogUnsupported is the detail of the step when a leader has no registrar
// (a unit test without a catalog): the step ends as skipped.
var ErrCatalogUnsupported = errors.New("skipped: no catalog registrar")

// readBackEverywhere reads the row of the standby (host:port) on every primary of the catalog and checks it is
// there as the standby of the group with node id id (present) or gone (not present). The extension answers success
// for add_node and remove_node although a node that went silent at the very end did not get its part, so the result
// is read on the nodes that the call should have changed (FO-08). A primary that cannot be read is a refusal that
// repeats on the next check: the catalog is not known to be right.
func (r *PGRegistrar) readBackEverywhere(ctx context.Context, c *pgx.Conn, e Entry, id int32, port int, present bool) error {
	if r.Remote == nil {
		return nil
	}
	rows, err := c.Query(ctx, `SELECT host, port FROM kingres.node WHERE role = 'primary' ORDER BY node_id`)
	if err != nil {
		return err
	}
	type hp struct {
		host string
		port int
	}
	var nodes []hp
	for rows.Next() {
		var n hp
		if err := rows.Scan(&n.host, &n.port); err != nil {
			rows.Close()
			return err
		}
		nodes = append(nodes, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, n := range nodes {
		if r.Skip != nil && r.Skip(n.host) {
			// A server that is dead (declared gone, a failover under way) cannot be read and must not block the
			// removal of its standbys; it is brought up to date by sync_catalog() when it returns.
			r.logger().Warn("standby: a dead server is not read back", "server", n.host)
			continue
		}
		nc, err := r.Remote(ctx, n.host, n.port)
		if err != nil {
			return &CatalogError{Call: "read back", Code: "unread", Msg: fmt.Sprintf("the catalog of %s:%d cannot be read to check the result: %v", n.host, n.port, err), Retry: true}
		}
		bid, brole, bgroup, found, ferr := find(ctx, nc, e.Server, port)
		_ = nc.Close(context.WithoutCancel(ctx))
		switch {
		case ferr != nil:
			return &CatalogError{Call: "read back", Code: "unread", Msg: fmt.Sprintf("reading the catalog of %s:%d: %v", n.host, n.port, ferr), Retry: true}
		case present && (!found || bid != id || brole != "standby" || bgroup != e.Group):
			return fmt.Errorf("node %d is not in the catalog of %s:%d as the standby of group %d (found %v): repeated by the next check", id, n.host, n.port, e.Group, found)
		case !present && found:
			return fmt.Errorf("the catalog of %s:%d still has %s:%d: repeated by the next check", n.host, n.port, e.Server, port)
		}
	}
	return nil
}

// trusted opens a session with open and hands it over only when kingres_shard is installed in the database with a schema that can be trusted.
func trusted(ctx context.Context, db string, open func() (*pgx.Conn, error)) (*pgx.Conn, error) {
	c, err := open()
	if err != nil {
		return nil, err
	}
	if err := extgate.Require(ctx, c, extguard.Shard, db); err != nil {
		_ = c.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	return c, nil
}
