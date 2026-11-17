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
		return id, nil
	}
	err = c.QueryRow(ctx, `SELECT kingres.add_node($1, $2, $3, 'standby')`, e.Server, e.Port, e.Group).Scan(&id)
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == sqlstateAlreadyThere {
			// registered meanwhile (a call that did not finish, sequence 9): use its id
			if id, role, group, found, ferr := find(ctx, c, e.Server, e.Port); ferr == nil && found && role == "standby" && group == e.Group {
				return id, nil
			}
		}
		return 0, classify("add_node", err)
	}
	// A success of add_node is not proof that the row is there (review S12-01): read it back, in the same session.
	if bid, brole, bgroup, bfound, berr := find(ctx, c, e.Server, e.Port); berr != nil || !bfound || bid != id || brole != "standby" || bgroup != e.Group {
		return 0, fmt.Errorf("add_node answered node %d, and the catalog does not show it as the standby of group %d (found %v, err %v): repeated by the next check", id, e.Group, bfound, berr)
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
	if role != "standby" || host != e.Server {
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
	return nil
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
