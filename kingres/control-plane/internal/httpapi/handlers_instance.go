// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// The operations on this server's instance and settings (README "Starting and
// stopping the instance", "Local settings"). They are local: never forwarded.

const (
	opStartInstance = "startThisServerInstance"
	opLogin         = "login"
	// fallbackInstanceID is the instance id used when the catalog cannot say
	// (the system database is down and nothing was learned yet): the id of the
	// first instance of a server.
	fallbackInstanceID = 1
	// DefaultStaleRolesMaxAge bounds how old the last successful role check may
	// be when startThisServerInstance is authorized from memory.
	DefaultStaleRolesMaxAge = time.Hour
)

// instanceState is the part of the Server that belongs to these operations.
type instanceState struct {
	// id is the catalog id of this server's instance, learned while the catalog
	// could be read (it is needed while the system database is down).
	id atomic.Int32
}

func (s *Server) instanceHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"startThisServerInstance":  s.startThisServerInstance,
		"stopThisServerInstance":   s.stopThisServerInstance,
		"getThisServerSettings":    s.getThisServerSettings,
		"updateThisServerSettings": s.updateThisServerSettings,
	}
}

// localInstanceID resolves the id of this server's instance: from the catalog
// while it can be read (and remembered), else the remembered one.
func (s *Server) localInstanceID(ctx context.Context) int32 {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if sid, err := s.deps.Store.LocalServerID(cctx); err == nil {
		if srv, err := s.deps.Store.Server(cctx, sid); err == nil && len(srv.Instances) > 0 {
			s.inst.id.Store(srv.Instances[0].ID)
			return srv.Instances[0].ID
		}
	}
	if id := s.inst.id.Load(); id != 0 {
		return id
	}
	return fallbackInstanceID
}

// instanceTarget validates the {instanceId} of the path and checks that it is
// the instance of this server (404 otherwise).
func (s *Server) instanceTarget(r *http.Request) (string, error) {
	pp := newParams(r)
	id := pp.pathInt32("instanceId")
	if p := pp.err(); p != nil {
		return "", p
	}
	if id != s.localInstanceID(r.Context()) {
		return "", problem.New(problem.NotFound, "Instance "+r.PathValue("instanceId")+" does not run on this server.")
	}
	return ops.InstanceIDString(id), nil
}

// forceOf reads the optional body {force} of stop and restart.
func (s *Server) forceOf(w http.ResponseWriter, r *http.Request) (bool, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return false, nil
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return false, problem.New(problem.BadRequest, "The request body could not be read.")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, nil
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	var req api.InstanceActionRequest
	if err := s.body(w, r, &req); err != nil {
		return false, err
	}
	return req.Force, nil
}

// checkReplicas refuses (409 no_replica_available) to take down the only
// up-to-date copy of a shard group while replicas exist that are not caught up.
// force overrides this and only this. A topology that cannot be read does not
// block (the instance is local and the operator asked for it).
func (s *Server) checkReplicas(r *http.Request, id string, force bool) error {
	if force {
		return nil
	}
	topo, err := s.deps.Store.ReplicationTopology(r.Context())
	if err != nil {
		s.log.Warn("reading the topology before an instance operation", "request_id", stateOf(r).id, "error", err)
		return nil
	}
	n, _ := strconv.ParseInt(id, 10, 32)
	if ops.SoleUpToDateCopy(topo, int32(n)) {
		return problem.New(problem.NoReplicaAvailable,
			"This instance is the only up-to-date copy of a shard group and replicas exist that are not caught up. Use force to proceed.")
	}
	return nil
}

func (s *Server) stopThisServerInstance(c *call, w http.ResponseWriter, r *http.Request) error {
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
	// The state conflicts come before the replica check: a stopped instance is
	// stopped whatever its replicas are.
	if st, serr := o.InstanceState(r.Context()); serr == nil && (st == ops.StateStopped || st == ops.StateStopping) {
		return problem.New(problem.InstanceAlreadyStopped, "The instance is already stopped (or being stopped).")
	}
	if err := s.checkReplicas(r, id, force); err != nil {
		return err
	}
	t, err := o.StopInstance(r.Context(), id, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) startThisServerInstance(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	id, err := s.instanceTarget(r)
	if err != nil {
		return err
	}
	t, err := o.StartInstance(r.Context(), id, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) getThisServerSettings(_ *call, w http.ResponseWriter, r *http.Request) error {
	if s.deps.LocalSettings == nil {
		return problem.New(problem.UnsupportedOp, "This server is not configured to manage a PostgreSQL instance (pg-bin-dir).")
	}
	out, err := s.deps.LocalSettings.Get()
	if err != nil {
		s.log.Error("reading the local settings", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "The local settings could not be read.")
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) updateThisServerSettings(c *call, w http.ResponseWriter, r *http.Request) error {
	if s.deps.LocalSettings == nil {
		return problem.New(problem.UnsupportedOp, "This server is not configured to manage a PostgreSQL instance (pg-bin-dir).")
	}
	if p := requireJSON(r); p != nil {
		return p
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes))
	if err != nil {
		return problem.New(problem.BadRequest, "The request body could not be read.")
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return problem.New(problem.BadRequest, "The request body is empty.")
	}
	out, err := s.deps.LocalSettings.Update(raw)
	if err != nil {
		var p *problem.Problem
		if errors.As(err, &p) {
			return p
		}
		s.log.Error("changing the local settings", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "The local settings could not be changed.")
	}
	c.details = map[string]any{"restartRequired": out.RestartRequired}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

// gateStoppedInstance answers 503 system_database_stopped for every
// authenticated operation (and login) except startThisServerInstance while the
// local instance was stopped on purpose. Health, readiness and the node state
// are not gated; neither are the setup operations.
func (s *Server) gateStoppedInstance(op contract.Operation) *problem.Problem {
	if s.deps.Ops == nil || op.Tag == contract.TagHealth || op.Tag == contract.TagSetup || op.ID == opStartInstance {
		return nil
	}
	if op.ID != opLogin && !op.Accepts(contract.SchemeSessionCookie) && !op.Accepts(contract.SchemeBearer) {
		return nil
	}
	if !s.deps.Ops.LocalInstanceDown() {
		return nil
	}
	id := s.inst.id.Load()
	if id == 0 {
		id = fallbackInstanceID
	}
	return problem.New(problem.SystemDatabaseStopped,
		"The instance that hosts the system database is stopped. Start it with POST /api/v1/this-server/instances/"+itoa64(int64(id))+"/start.").
		WithRetryAfter(10)
}

// sessionRoles is currentRoles for the guard, with one exception: when the
// roles cannot be asked of PostgreSQL because the local instance is stopped,
// startThisServerInstance (and only it) is authorized from the cluster roles of
// the last successful check, if that is not older than the bound. Every other
// operation fails closed as before (CP-02).
func (s *Server) sessionRoles(c *call, r *http.Request, sess auth.Session) ([]string, *problem.Problem) {
	roles, perr := s.currentRoles(r, sess.Username)
	if perr == nil || c.op.ID != opStartInstance || perr.Status != http.StatusServiceUnavailable ||
		s.deps.Ops == nil || !s.deps.Ops.StartMayUseStaleRoles(r.Context()) {
		return roles, perr
	}
	maxAge := s.deps.StaleRolesMaxAge
	if maxAge <= 0 {
		maxAge = DefaultStaleRolesMaxAge
	}
	if last, ok := s.reval.LastGood(sess.Username, maxAge); ok {
		return last, nil
	}
	// No check since the process started: the sign-in itself was one.
	if len(sess.ClusterRoles) > 0 && s.now().Sub(sess.CreatedAt) <= maxAge {
		return sess.ClusterRoles, nil
	}
	if s.deps.Ops.LocalInstanceDown() {
		return nil, problem.New(problem.SystemDatabaseStopped,
			"The instance is stopped and this session has no recent successful role check; start the instance on the machine.")
	}
	return nil, perr
}
