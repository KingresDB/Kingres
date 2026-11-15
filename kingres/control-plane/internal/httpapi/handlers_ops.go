// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// RoleConnector is what the sign-in needs from the proxy that later opens
// connections as the signed-in role.
type RoleConnector interface {
	// Allow makes the role connectable after a successful password sign-in.
	Allow(ctx context.Context, role string) error
}

// opsHandlers are the operations of package ops.
func (s *Server) opsHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"createRole":                s.createRole,
		"updateRole":                s.updateRole,
		"deleteRole":                s.deleteRole,
		"setRoleMemberships":        s.setRoleMemberships,
		"setRoleClusterRoles":       s.setRoleClusterRoles,
		"createGrant":               s.createGrant,
		"revokeGrants":              s.revokeGrants,
		"applyGrantTemplate":        s.applyGrantTemplate,
		"getNetworkAccess":          s.getNetworkAccess,
		"replaceNetworkAccess":      s.replaceNetworkAccess,
		"createDatabase":            s.createDatabase,
		"updateDatabase":            s.updateDatabase,
		"dropDatabase":              s.dropDatabase,
		"distributeTable":           s.distributeTable,
		"enableTableLedger":         s.enableTableLedger,
		"executeQuery":              s.executeQuery,
		"cancelStatement":           s.cancelStatement,
		"cancelTask":                s.cancelTask,
		"restartThisServerInstance": s.restartThisServerInstance,
		"updateClusterSettings":     s.updateClusterSettings,
		"getClusterSummary":         s.getClusterSummary,
		"listLedgerDigests":         s.listLedgerDigests,
		"generateLedgerDigest":      s.generateLedgerDigest,
		"startLedgerVerification":   s.startLedgerVerification,
		"createInclusionProof":      s.createInclusionProof,
		"listLedgerVerifications":   s.listLedgerVerifications,
		"getLedgerVerification":     s.getLedgerVerification,
	}
}

func (s *Server) needOps() (*ops.Ops, error) {
	if s.deps.Ops == nil {
		return nil, problem.New(problem.UnsupportedOp, "This server is not configured to manage a PostgreSQL instance (pg-bin-dir).")
	}
	return s.deps.Ops, nil
}

// body reads a JSON request body into dst.
func (s *Server) body(w http.ResponseWriter, r *http.Request, dst any) error {
	if p := requireJSON(r); p != nil {
		return p
	}
	if p := s.decodeJSON(w, r, dst); p != nil {
		return p
	}
	return nil
}

// bodyClosed is body for the requests the contract declares closed: a member that
// is not listed is 400 validation_failed.
func (s *Server) bodyClosed(w http.ResponseWriter, r *http.Request, dst any) error {
	if p := requireJSON(r); p != nil {
		return p
	}
	if p := s.decodeJSONMode(w, r, dst, true); p != nil {
		return p
	}
	return nil
}

func (s *Server) createRole(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.RoleCreateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.CreateRole(r.Context(), c.session.Username, req); err != nil {
		return err
	}
	role, err := s.deps.Store.Role(r.Context(), req.Name)
	if err != nil {
		return s.storeProblem(r, err, "Role does not exist.")
	}
	w.Header().Set("Location", "/api/v1/roles/"+req.Name)
	s.writeJSON(w, r, http.StatusCreated, role)
	return nil
}

func (s *Server) roleAfter(w http.ResponseWriter, r *http.Request, name string) error {
	role, err := s.deps.Store.Role(r.Context(), name)
	if err != nil {
		return s.storeProblem(r, err, "Role \""+name+"\" does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, role)
	return nil
}

func (s *Server) updateRole(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("roleName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.RoleUpdateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if req.Password == nil && req.CanLogin == nil && req.CanCreateDatabase == nil && req.CanCreateRole == nil &&
		req.ConnectionLimit == nil && !req.ValidUntil.Set {
		return problem.Validation("The role update is empty.", problem.FieldError{Field: "/", Message: "at least one property is required", Code: "required"})
	}
	if err := o.UpdateRole(r.Context(), c.session.Username, name, req); err != nil {
		return err
	}
	return s.roleAfter(w, r, name)
}

func (s *Server) deleteRole(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("roleName")
	if p := pp.err(); p != nil {
		return p
	}
	if err := o.DeleteRole(r.Context(), c.session.Username, name); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) setRoleMemberships(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("roleName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.RoleMembershipsRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.SetMemberships(r.Context(), c.session.Username, name, req.MemberOf); err != nil {
		return err
	}
	return s.roleAfter(w, r, name)
}

func (s *Server) setRoleClusterRoles(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("roleName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.RoleClusterRolesRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.SetClusterRoles(r.Context(), c.session.Username, name, req.ClusterRoles); err != nil {
		return err
	}
	return s.roleAfter(w, r, name)
}

func (s *Server) createGrant(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.GrantRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.Grant(r.Context(), c.session.Username, req); err != nil {
		return err
	}
	page, err := s.deps.Store.Grants(r.Context(), store.GrantFilter{Grantee: req.Grantee, Database: req.Database,
		Level: req.Level, Schema: req.Schema, Table: req.Table}, store.Page{Limit: 10})
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	for _, g := range page.Items {
		if g.Grantee == req.Grantee && g.Level == req.Level {
			s.writeJSON(w, r, http.StatusOK, g)
			return nil
		}
	}
	// PostgreSQL accepted the grant but the catalog shows no entry (for
	// example the grantee already had no such privilege): an empty entry.
	var sch, tbl *string
	if req.Schema != "" {
		sch = &req.Schema
	}
	if req.Table != "" {
		tbl = &req.Table
	}
	s.writeJSON(w, r, http.StatusOK, api.Grant{Grantee: req.Grantee, Level: req.Level, Database: req.Database,
		Schema: sch, Table: tbl, Privileges: []string{}})
	return nil
}

func (s *Server) revokeGrants(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.GrantRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.Revoke(r.Context(), c.session.Username, req); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) applyGrantTemplate(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.GrantTemplateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.ApplyTemplate(r.Context(), c.session.Username, req); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) getNetworkAccess(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	out, err := o.GetNetworkAccess(r.Context())
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// accepted writes the 202 response of a started task.
func (s *Server) accepted(w http.ResponseWriter, r *http.Request, t task.Task) {
	w.Header().Set("Location", "/api/v1/tasks/"+itoa64(t.ID))
	s.writeJSON(w, r, http.StatusAccepted, taskToAPI(t))
}

// taskProblem maps engine errors.
func (s *Server) taskProblem(r *http.Request, err error) error {
	switch {
	case errors.Is(err, task.ErrSubjectBusy):
		return problem.New(problem.OperationInProgress, "Another operation on this object is still running.")
	case errors.Is(err, task.ErrNotFound):
		return problem.New(problem.NotFound, "No such task.")
	case errors.Is(err, task.ErrNotCancellable):
		return problem.New(problem.TaskNotCancellable, "The task has finished and cannot be canceled.")
	}
	var p *problem.Problem
	if errors.As(err, &p) {
		return p
	}
	s.log.Error("task engine", "request_id", stateOf(r).id, "error", err)
	return problem.New(problem.InternalError, "An unexpected error occurred.")
}

func (s *Server) replaceNetworkAccess(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	dry := r.URL.Query().Get("dryRun")
	if dry != "" && dry != "true" && dry != "false" {
		return problem.Validation("The query is invalid.", problem.FieldError{Field: "dryRun", Message: "must be true or false", Code: "invalid"})
	}
	var req api.NetworkAccessUpdateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareReplaceNetworkAccess(r.Context(), c.session.Username, c.ip, req)
	if err != nil {
		return err
	}
	if dry == "true" {
		// Checked as for a real change, nothing done and nothing audited.
		c.audit = false
		s.writeJSON(w, r, http.StatusOK, api.NetworkAccessCheck{Valid: true, Revision: req.Revision})
		return nil
	}
	t, err := o.StartTask(r.Context(), ops.KindNetworkApply, "networkAccess", "network-access", in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) createDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.DatabaseCreateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareCreateDatabase(r.Context(), c.session.Username, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindDatabaseCreate, "database", req.Name, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) updateDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.DatabaseUpdateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if req.General == nil && req.Sharding == nil && req.Replication == nil && req.Ledger == nil {
		return problem.Validation("The update is empty.", problem.FieldError{Field: "/", Message: "at least one property is required", Code: "required"})
	}
	if err := o.UpdateDatabase(r.Context(), c.session.Username, name, req); err != nil {
		return err
	}
	db, err := s.deps.Store.Database(r.Context(), name)
	if err != nil {
		return s.storeProblem(r, err, "Database \""+name+"\" does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, db)
	return nil
}

func (s *Server) dropDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	name := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	in, err := o.PrepareDropDatabase(r.Context(), name)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindDatabaseDrop, "database", name, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) distributeTable(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db, schema, table := pp.pathName("databaseName"), pp.pathName("schemaName"), pp.pathName("tableName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.DistributeTableRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareDistribute(r.Context(), db, schema, table, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindTableDistribute, "table", db+"/"+schema+"."+table, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) enableTableLedger(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db, schema, table := pp.pathName("databaseName"), pp.pathName("schemaName"), pp.pathName("tableName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.EnableTableLedgerRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.EnableLedger(r.Context(), db, schema, table, req); err != nil {
		return err
	}
	t, err := s.deps.Store.Table(r.Context(), db, schema, table)
	if err != nil {
		return s.storeProblem(r, err, "Table does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, t.Table)
	return nil
}

func (s *Server) executeQuery(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.QueryRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	// The audit record carries the length and a hash of the statement, never
	// its text.
	n, h := ops.StatementFingerprint(req.SQL)
	c.details = map[string]any{"database": req.Database, "sqlLength": n, "sqlHash": h}
	if req.StatementID != "" {
		c.details["statementId"] = req.StatementID
	}
	ctx, stop := queryContext(w, r, req)
	defer stop()
	res, err := o.ExecuteQuery(ctx, c.session.Username, req)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, res)
	return nil
}

// queryContext gives a workbench statement the time its own timeoutMs asks for.
// The request context ends at RequestTimeout (30 s by default), which would end
// every statement with a longer timeoutMs as a service failure before its own
// statement_timeout (CP2-12); the statement runs under a context that follows
// the client (a disconnect cancels it) but not that deadline, and the response
// may be written until the statement's time plus a grace period has passed.
func queryContext(w http.ResponseWriter, r *http.Request, req api.QueryRequest) (context.Context, context.CancelFunc) {
	timeout := 30000
	if req.TimeoutMs != nil && *req.TimeoutMs > 0 && *req.TimeoutMs <= 300000 {
		timeout = *req.TimeoutMs
	}
	budget := time.Duration(timeout)*time.Millisecond + queryGrace
	// Best effort: a writer that cannot change its deadline keeps the server's.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(budget + 10*time.Second))
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	stopWatch := context.AfterFunc(r.Context(), func() {
		if errors.Is(r.Context().Err(), context.Canceled) { // the client went away, not the deadline
			cancel()
		}
	})
	return ctx, func() { stopWatch(); cancel() }
}

// queryGrace is added to timeoutMs for the whole request: the statement is
// ended by PostgreSQL at timeoutMs, and this is what the answer may still take.
const queryGrace = 5 * time.Second

func (s *Server) cancelTask(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("taskId")
	if p := pp.err(); p != nil {
		return p
	}
	eng := o.Engine()
	if eng == nil {
		return problem.New(problem.NotFound, "No such task.")
	}
	if err := eng.Cancel(r.Context(), id); err != nil {
		return s.taskProblem(r, err)
	}
	t, err := s.deps.Store.Task(r.Context(), id)
	if err != nil {
		return s.storeProblem(r, err, "No such task.")
	}
	s.writeJSON(w, r, http.StatusOK, t)
	return nil
}

func (s *Server) getClusterSummary(_ *call, w http.ResponseWriter, r *http.Request) error {
	sum, err := s.deps.Store.ClusterSummary(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	snap := s.deps.Setup.Snapshot()
	servers := sum.Servers.Total
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"clusterId": optStr(snap.ClusterID), "name": optStr(snap.ClusterName), "health": sum.Health,
		"serverCount": servers, "databaseCount": sum.DatabaseCount, "openAlerts": 0,
	})
	return nil
}

func (s *Server) listLedgerDigests(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListDigests(r.Context(), db, page)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) generateLedgerDigest(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	d, err := o.GenerateDigest(r.Context(), db, c.session.Username)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusCreated, d)
	return nil
}

func (s *Server) startLedgerVerification(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.LedgerVerifyRequest
	if r.ContentLength != 0 {
		if err := s.body(w, r, &req); err != nil {
			return err
		}
	}
	in, err := o.PrepareVerification(r.Context(), db, req)
	if err != nil {
		return err
	}
	// The verification record has no id until the run ends, so the task's
	// subject is the database (a deviation from the contract table; see the
	// README).
	t, err := o.StartTask(r.Context(), ops.KindLedgerVerify, "ledgerVerification", db, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

// restartThisServerInstance restarts the local instance as a task (start and
// stop are in handlers_instance.go). A stopped instance answers 409
// instance_already_stopped; the only up-to-date copy of a shard group needs force.
func (s *Server) restartThisServerInstance(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	id, err := s.instanceTarget(r)
	if err != nil {
		return err
	}
	force, err := s.forceOf(w, r)
	if err != nil {
		return err
	}
	if st, serr := o.InstanceState(r.Context()); serr == nil && (st == ops.StateStopped || st == ops.StateStopping) {
		return problem.New(problem.InstanceAlreadyStopped, "The instance is stopped; use startThisServerInstance.")
	}
	if err := s.checkReplicas(r, id, force); err != nil {
		return err
	}
	t, err := o.RestartInstance(r.Context(), id, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) updateClusterSettings(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.ClusterSettingsUpdateRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if err := o.UpdateClusterSettings(r.Context(), c.session.Username, req); err != nil {
		return err
	}
	out, err := s.deps.Store.ClusterSettings(r.Context())
	if err != nil {
		return s.storeProblem(r, err, "")
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// cancelStatement ends the caller's own running workbench statement (contract
// 0.7.0). The statement is found among the ones this server tracks; no process
// id ever comes from the client.
func (s *Server) cancelStatement(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	id := r.PathValue("statementId")
	c.details = map[string]any{"statementId": id}
	if err := o.CancelStatement(r.Context(), c.session.Username, id); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
