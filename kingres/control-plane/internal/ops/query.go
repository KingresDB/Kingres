// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgframe"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sqlx"
)

// StatementFingerprint is what the audit log records of a workbench
// statement instead of its text: its length and a hash prefix.
func StatementFingerprint(sql string) (length int, hash string) {
	sum := sha256.Sum256([]byte(sql))
	return len(sql), hex.EncodeToString(sum[:8])
}

// ExecuteQuery runs one statement as the signed-in role on a connection of
// that role, so PostgreSQL's permissions apply and nothing the statement does
// can exceed them. The statement runs in one transaction with a statement
// timeout; at most rowLimit rows are returned (reading stops by canceling the
// connection when more arrive).
func (o *Ops) ExecuteQuery(ctx context.Context, actor string, req api.QueryRequest) (api.QueryResult, error) {
	var errs []problem.FieldError
	if _, err := sqlx.NewIdent(req.Database); err != nil {
		errs = append(errs, field("/database", "is not a valid database name", "pattern"))
	}
	if req.SQL == "" || len(req.SQL) > 1_000_000 {
		errs = append(errs, field("/sql", "must be 1 to 1000000 bytes", "out_of_range"))
	}
	limit, timeout := 1000, 30000
	if req.RowLimit != nil {
		limit = *req.RowLimit
	}
	if req.TimeoutMs != nil {
		timeout = *req.TimeoutMs
	}
	if req.StatementID != "" && !statementIDPattern.MatchString(req.StatementID) {
		errs = append(errs, field("/statementId", "must be 1 to 64 characters of A-Z, a-z, 0-9, _ and -", "pattern"))
	}
	if limit < 1 || limit > 10000 {
		errs = append(errs, field("/rowLimit", "must be between 1 and 10000", "out_of_range"))
	}
	if timeout < 100 || timeout > 300000 {
		errs = append(errs, field("/timeoutMs", "must be between 100 and 300000", "out_of_range"))
	}
	if len(errs) > 0 {
		return api.QueryResult{}, problem.Validation("The query request is invalid.", errs...)
	}
	if o.Roles == nil {
		return api.QueryResult{}, problem.New(problem.UnsupportedOp, "This server cannot act as the signed-in role (no managed instance).")
	}
	release, ok := acquireWorkbench(actor)
	if !ok {
		return api.QueryResult{}, problem.New(problem.WorkbenchBusy, "Too many statements are running on this server (for this role or in all); try again shortly.").WithRetryAfter(2)
	}
	defer release()
	conn, err := o.Roles.Connect(ctx, req.Database, actor)
	if err != nil {
		return api.QueryResult{}, connectProblem(err)
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	if err := refuseDangerousRole(ctx, conn); err != nil {
		return api.QueryResult{}, err
	}

	started := time.Now()
	qctx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Millisecond+2*time.Second)
	defer cancel()
	// A statement the server did not end by itself (statement_timeout does that first, with
	// SQLSTATE 57014) is ended by the context at timeout plus a grace period: it is a failed
	// statement, not a failure of the service (CP2-12).
	st, untrack, err := o.trackStatement(actor, req.Database, req.StatementID, conn, cancel)
	if err != nil {
		return api.QueryResult{}, err
	}
	// Runs before the connection is closed (deferred later than its Close): see runningStatement.
	defer untrack()
	canceled := func() error { return problem.SQL("canceling statement due to user request", "57014", 0) }
	fail := func(err error) error {
		if st.cancelled.Load() {
			return canceled()
		}
		if ctx.Err() == nil && errors.Is(qctx.Err(), context.DeadlineExceeded) {
			return problem.SQL("canceling statement due to statement timeout", "57014", 0)
		}
		return queryProblem(err)
	}
	if st.cancelled.Load() {
		return api.QueryResult{}, canceled()
	}
	tx, err := conn.Begin(qctx)
	if err != nil {
		return api.QueryResult{}, connectProblem(err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	// The timeout is a bind parameter of set_config, not part of the text.
	if _, err := tx.Exec(qctx, "SELECT set_config('statement_timeout', $1, true)", strconv.Itoa(timeout)); err != nil {
		return api.QueryResult{}, fail(err)
	}
	// The extended protocol refuses text with several statements.
	rows, err := tx.Query(qctx, req.SQL, pgx.QueryExecModeExec)
	if err != nil {
		return api.QueryResult{}, fail(err)
	}
	// Only a plain read is cut short when its result is cut. Any other statement is read
	// to its end and its rows beyond the limit are dropped, so that it completes and is
	// committed: abandoning an UPDATE ... RETURNING half way would roll it back while the
	// answer says it succeeded (CP2-11).
	readOnly := plainRead(req.SQL)
	res := api.QueryResult{Columns: []api.QueryColumn{}, Rows: [][]any{}}
	bytesSoFar := 0
	tm := conn.TypeMap()
	for _, fd := range rows.FieldDescriptions() {
		name := fmt.Sprintf("oid:%d", fd.DataTypeOID)
		if t, ok := tm.TypeForOID(fd.DataTypeOID); ok {
			name = t.Name
		}
		res.Columns = append(res.Columns, api.QueryColumn{Name: fd.Name, Type: name})
	}
	for rows.Next() {
		if res.Truncated {
			continue // a statement that must complete: the rest of its rows is dropped
		}
		if len(res.Rows) >= limit {
			res.Truncated = true
			if readOnly {
				cancel() // stop the server from sending the rest
				break
			}
			continue
		}
		// The size is checked on the raw wire values before anything is converted.
		cells := 0
		for _, raw := range rows.RawValues() {
			if len(raw) > maxValueBytes {
				cancel()
				rows.Close()
				return api.QueryResult{}, valueTooLarge()
			}
			cells += len(raw) + 8
		}
		if bytesSoFar+cells > maxResultBytes {
			res.Truncated = true
			if readOnly {
				cancel()
				break
			}
			continue
		}
		bytesSoFar += cells
		vals, err := rows.Values()
		if err != nil {
			rows.Close()
			return api.QueryResult{}, fail(err)
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = jsonValue(v)
		}
		res.Rows = append(res.Rows, row)
	}
	if res.Truncated && readOnly {
		// A read whose result is cut: the transaction is abandoned, not committed (it changed nothing).
		rows.Close()
		res.RowCount = len(res.Rows)
		res.CommandTag = "SELECT " + strconv.Itoa(res.RowCount)
		res.DurationMs = float64(time.Since(started).Microseconds()) / 1000
		return res, nil
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return api.QueryResult{}, fail(err)
	}
	res.CommandTag = rows.CommandTag().String()
	res.RowCount = len(res.Rows)
	if n := int(rows.CommandTag().RowsAffected()); n > res.RowCount && (len(res.Columns) == 0 || res.Truncated) {
		res.RowCount = n
	}
	if err := tx.Commit(qctx); err != nil {
		return api.QueryResult{}, fail(err)
	}
	res.DurationMs = float64(time.Since(started).Microseconds()) / 1000
	return res, nil
}

// jsonValue converts a driver value to something encoding/json writes
// faithfully: integers beyond 2^53, bytea, numerics and uuids as strings.
func jsonValue(v any) any {
	switch x := v.(type) {
	case nil, bool, string, float32, float64:
		return x
	case int16:
		return x
	case int32:
		return x
	case int64:
		if x > 1<<53 || x < -(1<<53) {
			return strconv.FormatInt(x, 10)
		}
		return x
	case uint32:
		return x
	case uint64:
		if x > 1<<53 {
			return strconv.FormatUint(x, 10)
		}
		return x
	case []byte:
		return "\\x" + hex.EncodeToString(x)
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	case pgtype.Numeric:
		if !x.Valid {
			return nil
		}
		if x.NaN {
			return "NaN"
		}
		if x.InfinityModifier != pgtype.Finite {
			return "Infinity"
		}
		b, err := x.MarshalJSON()
		if err != nil {
			return fmt.Sprint(x)
		}
		return string(b)
	case map[string]any, []any:
		return x
	}
	if f, ok := v.(interface{ Float64Value() (pgtype.Float8, error) }); ok {
		if g, err := f.Float64Value(); err == nil && !math.IsNaN(g.Float64) && !math.IsInf(g.Float64, 0) {
			return g.Float64
		}
	}
	return fmt.Sprint(v)
}

func valueTooLarge() error {
	return problem.New(problem.Unprocessable, fmt.Sprintf("A value or row in the result is larger than the limit (%d bytes per value, %d per row); select a part of it (for example with substr).", maxValueBytes, pgframe.MaxRoleMessage))
}

// plainRead tells whether a statement is a read by its first keyword (comments
// skipped): SELECT, TABLE, VALUES or SHOW. Everything else, WITH included (it may
// hold a data-changing statement), is treated as one that may change data.
func plainRead(sql string) bool {
	s := strings.TrimSpace(sql)
	for {
		switch {
		case strings.HasPrefix(s, "--"):
			i := strings.IndexByte(s, '\n')
			if i < 0 {
				return false
			}
			s = strings.TrimSpace(s[i+1:])
		case strings.HasPrefix(s, "/*"):
			i := strings.Index(s, "*/")
			if i < 0 {
				return false
			}
			s = strings.TrimSpace(s[i+2:])
		default:
			word := s
			if i := strings.IndexFunc(s, func(r rune) bool { return !unicode.IsLetter(r) }); i >= 0 {
				word = s[:i]
			}
			switch strings.ToLower(word) {
			case "select", "table", "values", "show":
				return true
			}
			return false
		}
	}
}

// queryProblem maps an error of a workbench statement: whatever PostgreSQL
// rejected is a sql_error (422) with the SQLSTATE, as the contract says; only
// connection-level failures are service problems.
func queryProblem(err error) error {
	if errors.Is(err, pgframe.ErrTooLarge) {
		return valueTooLarge()
	}
	var pe *pgconn.PgError
	if errors.As(err, &pe) && (len(pe.Code) < 2 || (pe.Code[:2] != "08" && pe.Code[:2] != "53")) {
		return problem.SQL(pe.Message, pe.Code, int(pe.Position))
	}
	return PGProblem(err)
}

// Limits of the workbench (CP-04). A result is cut when its (wire) size passes
// maxResultBytes (Truncated is then true, as for the row limit); one value
// larger than maxValueBytes refuses the statement, because a cut value would
// look like data. At most maxWorkbench statements run at once on a server.
// Peak memory of one statement is the budget plus one row as it arrives from
// PostgreSQL; the statement is canceled as soon as a limit is passed.
const (
	maxResultBytes = pgframe.MaxRoleMessage
	maxValueBytes  = 1 << 20
	maxWorkbench   = 4
	// maxWorkbenchPerRole keeps one role from holding every slot (CP2-15).
	maxWorkbenchPerRole = 2
)

// workbench counts the statements running on this server, in all and per role.
var workbench = struct {
	sync.Mutex
	total  int
	byRole map[string]int
}{byRole: map[string]int{}}

// acquireWorkbench reserves a slot for role; the returned function frees it. It
// reports false when the server is full or the role has its share running.
func acquireWorkbench(role string) (release func(), ok bool) {
	workbench.Lock()
	defer workbench.Unlock()
	if workbench.total >= maxWorkbench || workbench.byRole[role] >= maxWorkbenchPerRole {
		return nil, false
	}
	workbench.total++
	workbench.byRole[role]++
	return func() {
		workbench.Lock()
		defer workbench.Unlock()
		workbench.total--
		if workbench.byRole[role]--; workbench.byRole[role] == 0 {
			delete(workbench.byRole, role)
		}
	}, true
}

// dangerousRolesSQL says whether the current role has an attribute or a
// membership that gives the operating system's file system or programs
// (CP-05): a superuser, replication or BYPASSRLS attribute, or membership,
// direct or indirect and inherited or not (SET ROLE can use it), in a
// predefined role that reads or writes server files, runs programs or reads
// the server log. A membership that has neither the INHERIT nor the SET option
// (PostgreSQL 16 and later give the creator of a role that bare ADMIN membership)
// cannot use the role and does not count: counting it locked the administrator
// that created a role out of the workbench as soon as that role was given a
// dangerous predefined role in SQL.
const dangerousRolesSQL = `
WITH RECURSIVE me AS (SELECT oid, rolsuper, rolreplication, rolbypassrls FROM pg_catalog.pg_roles WHERE rolname = coalesce($2::text, current_user)),
m(roleid) AS (
    SELECT a.roleid FROM pg_catalog.pg_auth_members a WHERE a.member = (SELECT oid FROM me) AND (a.inherit_option OR a.set_option)
  UNION
    SELECT a.roleid FROM pg_catalog.pg_auth_members a JOIN m ON a.member = m.roleid WHERE a.inherit_option OR a.set_option)
SELECT (SELECT rolsuper OR rolreplication OR rolbypassrls FROM me)
    OR EXISTS (SELECT 1 FROM pg_catalog.pg_roles r WHERE r.oid IN (SELECT roleid FROM m) AND r.rolname = ANY($1))`

// DangerousPredefinedRoles are the predefined roles that no cluster API grants
// and whose members the workbench refuses.
var DangerousPredefinedRoles = []string{"pg_execute_server_program", "pg_read_server_files", "pg_write_server_files", "pg_read_server_logs"}

// dangerousRole answers whether the role is one that the workbench and the imports refuse; an error means that the question could not be asked.
func dangerousRole(ctx context.Context, conn *pgx.Conn) (bool, error) {
	var bad bool
	if err := conn.QueryRow(ctx, dangerousRolesSQL, DangerousPredefinedRoles, nil).Scan(&bad); err != nil {
		return false, PGProblem(err)
	}
	return bad, nil
}

func refuseDangerousRole(ctx context.Context, conn *pgx.Conn) error {
	bad, err := dangerousRole(ctx, conn)
	if err != nil {
		return err
	}
	if bad {
		return problem.New(problem.Forbidden, "The query workbench does not run as a role with the SUPERUSER, REPLICATION or BYPASSRLS attribute or with access to server files, programs or logs.")
	}
	return nil
}

// harmlessPredefined are the predefined roles the API may grant. Every other
// pg_* role is refused: it is either dangerous (files, programs, logs) or not
// known to this version.
var harmlessPredefined = map[string]bool{
	"pg_monitor": true, "pg_read_all_settings": true, "pg_read_all_stats": true, "pg_stat_scan_tables": true,
	"pg_signal_backend": true, "pg_checkpoint": true, "pg_maintain": true, "pg_use_reserved_connections": true,
}

// memberOfRefusal says why a role may not be offered as a membership target by
// name alone ("" when the name is acceptable).
func memberOfRefusal(name string) string {
	if len(name) >= 3 && name[:3] == "pg_" && !harmlessPredefined[name] {
		return "memberships in this predefined role cannot be granted through the API"
	}
	return ""
}

// refuseDangerousTarget checks, in the catalog, that the role a membership is
// granted in is not a superuser or replication or BYPASSRLS role and does not
// itself lead to a dangerous predefined role.
func refuseDangerousTarget(ctx context.Context, x execer, name string) error {
	var bad bool
	if err := x.QueryRow(ctx, dangerousRolesSQL, DangerousPredefinedRoles, name).Scan(&bad); err != nil {
		return err
	}
	if bad {
		return problem.Validation("The memberships are invalid.", field("/memberOf", "role "+strconv.Quote(name)+" has the SUPERUSER, REPLICATION or BYPASSRLS attribute or access to server files, programs or logs and cannot be granted through the API", "invalid"))
	}
	return nil
}
