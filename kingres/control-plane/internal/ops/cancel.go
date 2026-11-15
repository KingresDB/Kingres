// SPDX-License-Identifier: PostgreSQL

package ops

import (
	"context"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// statementIDPattern is what a client may name its statement: short and free of
// anything that could matter anywhere (the name is only a key in a map and an
// audit detail).
var statementIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// cancelSafetyNet is how long a cancel request may take to end the statement on
// the server before the connection is cut from this side. The cancel is sent to
// the server by pg_cancel_backend; if it arrives while the backend is between two
// statements it is lost, and this timer ends the statement anyway.
const cancelSafetyNet = 2 * time.Second

// runningStatement is one workbench statement this server tracks. The key is the
// role that signed in and the statement id the client chose; the backend pid is
// the one of the connection kingresd itself opened for the statement, never a
// number that came from a client.
type runningStatement struct {
	role, database string
	pid            uint32
	stop           context.CancelFunc

	// mu serializes a cancel with the end of the statement: the entry is marked
	// done under mu before the connection is closed, so a late cancel can never
	// reach a backend process number that PostgreSQL has handed to someone else.
	mu        sync.Mutex
	done      bool
	cancelled atomic.Bool
}

// statements is the registry of the running statements of this server.
type statements struct {
	mu sync.Mutex
	m  map[string]*runningStatement
}

func statementKey(role, id string) string { return role + "\x00" + id }

// add registers a statement; false when the role already runs one with this id.
func (s *statements) add(id string, st *runningStatement) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string]*runningStatement{}
	}
	k := statementKey(st.role, id)
	if _, dup := s.m[k]; dup {
		return false
	}
	s.m[k] = st
	return true
}

// finish ends the tracking of a statement. It must run before the connection is
// closed.
func (s *statements) finish(id string, st *runningStatement) {
	st.mu.Lock()
	st.done = true
	st.mu.Unlock()
	s.mu.Lock()
	delete(s.m, statementKey(st.role, id))
	s.mu.Unlock()
}

func (s *statements) find(role, id string) *runningStatement {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.m[statementKey(role, id)]
}

// CancelStatement asks PostgreSQL to cancel the workbench statement the role
// named by actor is running under statementID on this server. Only the actor's
// own statements are found: the registry is keyed by role, and the backend that is
// cancelled is the one kingresd opened for that statement and tracked, checked
// again by role and database in pg_stat_activity. The statement itself ends with
// SQLSTATE 57014, like a statement timeout. A statement that is not running (it
// ended, never existed, or runs on another server, which the caller's session
// cannot reach) is not_found.
func (o *Ops) CancelStatement(ctx context.Context, actor, statementID string) error {
	if !statementIDPattern.MatchString(statementID) {
		return problem.New(problem.NotFound, "No such running statement.")
	}
	st := o.stmts.find(actor, statementID)
	if st == nil {
		return problem.New(problem.NotFound, "No such running statement.")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.done {
		return problem.New(problem.NotFound, "No such running statement.")
	}
	st.cancelled.Store(true)
	if o.Service != nil {
		if c, err := o.Service.ServiceConn(ctx, st.database); err == nil {
			// The statement runs on a different backend than this one; role and database
			// are checked once more so a wrong pid could not touch another role's session.
			_, _ = c.Exec(ctx, `SELECT pg_cancel_backend(pid) FROM pg_stat_activity
				WHERE pid = $1 AND usename = $2 AND datname = $3`, int32(st.pid), st.role, st.database) //nolint:gosec // a pid fits int32
			_ = c.Close(context.WithoutCancel(ctx))
		}
	}
	// A cancel that reaches an idle backend does nothing; make sure the statement ends.
	time.AfterFunc(cancelSafetyNet, st.stop)
	return nil
}

// trackStatement registers the statement under id (nothing when id is empty) and
// returns the function that ends the tracking and the entry. A duplicate id of a
// running statement of the same role is a conflict.
func (o *Ops) trackStatement(role, database, id string, conn *pgx.Conn, stop context.CancelFunc) (*runningStatement, func(), error) {
	st := &runningStatement{role: role, database: database, pid: conn.PgConn().PID(), stop: stop}
	if id == "" {
		return st, func() {}, nil
	}
	if !statementIDPattern.MatchString(id) {
		return nil, nil, problem.Validation("The query request is invalid.", field("/statementId", "must be 1 to 64 characters of A-Z, a-z, 0-9, _ and -", "pattern"))
	}
	if !o.stmts.add(id, st) {
		return nil, nil, problem.New(problem.Conflict, "A statement with this id is already running.")
	}
	return st, func() { o.stmts.finish(id, st) }, nil
}
