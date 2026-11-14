// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// handlers maps operation ids to their implementations. Operations missing
// here are answered by notImplemented. The set is checked against the
// contract in New, so a handler for an unknown operation cannot exist.
func (s *Server) handlers() map[string]handlerFunc {
	m := s.opsHandlers()
	for k, v := range s.coreHandlers() {
		m[k] = v
	}
	return m
}

func (s *Server) coreHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"getHealth":    s.getHealth,
		"getReadiness": s.getReadiness,

		"login":      s.login,
		"logout":     s.logout,
		"getSession": s.getSession,

		"getNodeState": s.getNodeState,

		"getCluster":             s.getCluster,
		"listServers":            s.listServers,
		"getServer":              s.getServer,
		"listServerGroups":       s.listServerGroups,
		"getClusterSettings":     s.getClusterSettings,
		"listClusterRoles":       s.listClusterRoles,
		"getReplicationTopology": s.getReplicationTopology,

		"listThisServerInstances": s.listThisServerInstances,
		"getThisServerInstance":   s.getThisServerInstance,

		"listDatabases":         s.listDatabases,
		"getDatabase":           s.getDatabase,
		"listTables":            s.listTables,
		"getTable":              s.getTable,
		"listColocationGroups":  s.listColocationGroups,
		"listBuckets":           s.listBuckets,
		"getBucketDistribution": s.getBucketDistribution,

		"listTasks": s.listTasks,
		"getTask":   s.getTask,

		"checkSetupToken":          s.checkSetupTokenOp,
		"initCluster":              s.initCluster,
		"getDatabaseCreateOptions": s.getDatabaseCreateOptions,

		"listLedgerStatus": s.listLedgerStatus,
		"getLedgerStatus":  s.getLedgerStatus,
		"listLedgerBlocks": s.listLedgerBlocks,

		"listRoles":  s.listRoles,
		"getRole":    s.getRole,
		"listGrants": s.listGrants,

		"listAuditEvents": s.listAuditEvents,
	}
}

func (s *Server) getHealth(_ *call, w http.ResponseWriter, r *http.Request) error {
	s.writeJSON(w, r, http.StatusOK, api.HealthStatus{Status: "ok"})
	return nil
}

// getReadiness answers 503 while the server is starting or stopping. An
// uninitialized node is ready even when PostgreSQL is not running yet (it
// serves the setup wizard); for other states a failing local PostgreSQL makes
// the node not ready.
func (s *Server) getReadiness(_ *call, w http.ResponseWriter, r *http.Request) error {
	if !s.ready.Load() {
		return problem.New(problem.ServiceUnavailable, "The server is starting or shutting down.").WithRetryAfter(5)
	}
	state := s.deps.Setup.State()
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()
	check := api.ReadinessCheck{Name: "localPostgres", OK: true}
	if err := s.deps.Store.Ping(ctx); err != nil {
		s.log.Warn("readiness: local PostgreSQL check failed", "request_id", stateOf(r).id, "error", err)
		check.OK, check.Detail = false, "the local PostgreSQL instance is not reachable"
	}
	if !check.OK && state != setup.Uninitialized {
		return problem.New(problem.ServiceUnavailable, "The local PostgreSQL instance is not reachable.").WithRetryAfter(5)
	}
	s.writeJSON(w, r, http.StatusOK, api.ReadinessStatus{
		Status: "ready", NodeState: string(state), Checks: []api.ReadinessCheck{check},
	})
	return nil
}

func (s *Server) getNodeState(_ *call, w http.ResponseWriter, r *http.Request) error {
	snap := s.deps.Setup.Snapshot()
	pgv := "unknown"
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if v, err := s.deps.Store.PostgresVersion(ctx); err == nil {
		pgv = v
	}
	info := api.NodeStateInfo{
		State: string(snap.State), Address: s.advertisedAddress(), Hostname: s.deps.Hostname,
		Version: s.deps.Version, PostgresVersion: pgv,
		SetupTokenRequired: snap.State == setup.Uninitialized,
		LocalhostOnly:      snap.State == setup.Uninitialized && s.deps.LoopbackOnly,
		SetupTask:          snap.SetupTask,
	}
	if snap.State != setup.Uninitialized {
		info.ServerID = s.localServerID(ctx)
	}
	if snap.ClusterID != "" {
		id := snap.ClusterID
		info.ClusterID = &id
	}
	s.writeJSON(w, r, http.StatusOK, info)
	return nil
}

// advertisedAddress is host:port of this server; when the server listens on
// all interfaces the host name stands in for the unspecified address.
func (s *Server) advertisedAddress() string {
	host, port, err := net.SplitHostPort(s.deps.ListenAddr)
	if err != nil {
		return s.deps.ListenAddr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = s.deps.Hostname
	}
	return net.JoinHostPort(host, port)
}

// sessionBody builds the Session response: identity, roles, expiry, the cluster
// identity of the node (null outside a cluster) and the CSRF token of a cookie
// session (null for a bearer session).
func (s *Server) sessionBody(ctx context.Context, sess auth.Session) api.Session {
	snap := s.deps.Setup.Snapshot()
	out := api.Session{
		Username: sess.Username, ClusterRoles: nonNil(sess.ClusterRoles), ExpiresAt: sess.ExpiresAt.UTC(),
		ServerID: s.localServerID(ctx),
	}
	if snap.ClusterID != "" {
		out.ClusterID = &snap.ClusterID
	}
	if snap.ClusterName != "" {
		out.ClusterName = &snap.ClusterName
	}
	if sess.CSRFToken != "" {
		t := sess.CSRFToken
		out.CSRFToken = &t
	}
	return out
}

// localServerID is the id of the server this node runs on: the one recorded
// in the node state, else the host of this node in the cluster catalog; nil
// when the node is not part of a cluster or the catalog cannot be read.
func (s *Server) localServerID(ctx context.Context) *string {
	if id := s.deps.Setup.Snapshot().ServerID; id != nil {
		return id
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	id, err := s.deps.Store.LocalServerID(ctx)
	if err != nil {
		return nil
	}
	return &id
}

// checkSetupTokenOp answers 204 for a valid setup token. The guard has done the
// whole check (token, state, address, rate limit); the token is not consumed.
func (s *Server) checkSetupTokenOp(_ *call, w http.ResponseWriter, _ *http.Request) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// login authenticates a PostgreSQL role. Attempts are rate limited per client
// address and per user name; a successful login resets only the user's
// counter, so signing in to one's own account does not erase the address'
// failure history. Passwords are used for the PostgreSQL connection and then
// dropped; they are never logged or stored.
func (s *Server) login(c *call, w http.ResponseWriter, r *http.Request) error {
	c.auditSelf = true
	ev := audit.Event{Actor: "anonymous", ActorAddress: c.ip, Source: "rest", Action: "loginFailed",
		Outcome: audit.OutcomeFailure, RequestID: stateOf(r).id, ResourceType: "session"}
	// Only attempts that reach PostgreSQL are audited: malformed requests and
	// rate-limited attempts are logged but not written to the audit file, so
	// an unauthenticated client cannot grow it without bound.
	audited := false
	defer func() {
		if audited {
			s.appendAudit(r, ev, "login")
		}
	}()

	if ok, wait := s.loginIP.Allow(c.ip); !ok {
		s.log.Warn("login rate limited", "request_id", stateOf(r).id, "remote", c.ip)
		return rateLimited(wait)
	}
	if p := requireJSON(r); p != nil {
		return p
	}
	var req api.LoginRequest
	if p := s.decodeJSON(w, r, &req); p != nil {
		return p
	}
	var errs []problem.FieldError
	if l := len(req.Username); l < 1 || l > 63 {
		errs = append(errs, problem.FieldError{Field: "/username", Message: "must be between 1 and 63 characters", Code: "out_of_range"})
	}
	if l := len(req.Password); l < 1 || l > 1024 {
		errs = append(errs, problem.FieldError{Field: "/password", Message: "must be between 1 and 1024 characters", Code: "out_of_range"})
	}
	if len(errs) > 0 {
		return problem.Validation("The login request is invalid.", errs...)
	}
	userKey := "user:" + req.Username
	if ok, wait := s.loginUser.Allow(userKey); !ok {
		s.log.Warn("login rate limited for user", "request_id", stateOf(r).id, "remote", c.ip)
		return rateLimited(wait)
	}
	audited = true
	ev.Actor, ev.ResourceID = req.Username, req.Username
	ev.Details = map[string]any{"issueToken": req.IssueToken}

	roles, err := s.deps.Authenticator.Authenticate(r.Context(), req.Username, req.Password)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		ev.Details["reason"] = "invalid_credentials"
		return problem.New(problem.InvalidCredentials, "The user name or password is wrong.").
			WithHeader("WWW-Authenticate", `Bearer realm="kingresd"`)
	case err != nil:
		s.log.Error("login backend failure", "request_id", stateOf(r).id, "error", err)
		ev.Details["reason"] = "backend_unavailable"
		return problem.New(problem.ServiceUnavailable, "Sign-in is not possible right now: PostgreSQL is not available.").WithRetryAfter(5)
	case len(roles) == 0:
		ev.Outcome = audit.OutcomeDenied
		ev.Details["reason"] = "no_cluster_role"
		return problem.New(problem.Forbidden, "The role is not a member of any KingresDB cluster role.")
	}

	cookieCred, sess, err := s.deps.Sessions.Create(req.Username, roles, auth.KindCookie)
	if err != nil {
		return s.sessionProblem(r, err)
	}
	if s.deps.Proxy != nil {
		if err := s.deps.Proxy.Allow(r.Context(), req.Username); err != nil {
			s.log.Warn("enabling the role for administration", "request_id", stateOf(r).id, "error", err)
		}
	}
	out := s.sessionBody(r.Context(), sess)
	if req.IssueToken {
		tokenCred, _, err := s.deps.Sessions.Create(req.Username, roles, auth.KindToken)
		if err != nil {
			s.deps.Sessions.Revoke(cookieCred)
			return s.sessionProblem(r, err)
		}
		out.Token = &tokenCred
	}
	s.loginUser.Reset(userKey)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: cookieCred, Path: "/api/v1", Expires: sess.ExpiresAt, MaxAge: int(s.deps.SessionTTL.Seconds()),
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	ev.Action, ev.Outcome = "login", audit.OutcomeSuccess
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) sessionProblem(r *http.Request, err error) *problem.Problem {
	s.log.Error("cannot create session", "request_id", stateOf(r).id, "error", err)
	if errors.Is(err, auth.ErrTooManySessions) {
		return problem.New(problem.ServiceUnavailable, "Too many active sessions; try again later.").WithRetryAfter(60)
	}
	return problem.New(problem.InternalError, "An unexpected error occurred.")
}

// logout revokes the session or token that authenticated the request and
// clears the cookie.
func (s *Server) logout(c *call, w http.ResponseWriter, _ *http.Request) error {
	s.deps.Sessions.Revoke(c.credential)
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: "", Path: "/api/v1", MaxAge: -1,
		HttpOnly: true, Secure: true, SameSite: http.SameSiteStrictMode,
	})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// getSession returns the session of the request, including its CSRF token
// (the cookie is HttpOnly, so a reloaded panel gets the token here).
func (s *Server) getSession(c *call, w http.ResponseWriter, r *http.Request) error {
	out := s.sessionBody(r.Context(), *c.session)
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
