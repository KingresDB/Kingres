// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
)

// CookieName is the name of the session cookie (security scheme sessionCookie).
const CookieName = "kingres_session"

// Header names of the contract.
const (
	headerCSRF       = "X-CSRF-Token"
	headerSetupToken = "X-Setup-Token"
)

// call is the per-request context of an operation handler.
type call struct {
	op contract.Operation
	// session is the authenticated session, nil for public operations and
	// for setup-token requests.
	session *auth.Session
	// credential is the raw cookie value or bearer token of the session. It
	// lives only for the request and is used by logout to revoke the session.
	credential string
	// viaSetupToken is true when the request was authenticated with the
	// setup token.
	viaSetupToken bool
	// audit tells serveOp to record the request in the audit log after the
	// handler ran. auditSelf marks handlers that record their own events.
	audit     bool
	auditSelf bool
	ip        string
	// details are extra audit details a handler sets (never secrets or
	// statement text).
	details map[string]any
}

// handlerFunc implements one operation. It writes the success response and
// returns nil, or returns a *problem.Problem (or another error, which
// becomes a 500) without having written anything.
type handlerFunc func(c *call, w http.ResponseWriter, r *http.Request) error

// serveOp wraps an operation handler with the guard: node-state gating, rate
// limiting, authentication, CSRF check, authorization, query validation and
// audit. Every operation goes through here, so no handler can forget a step.
func (s *Server) serveOp(op contract.Operation, h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		st := stateOf(r)
		st.operation = op.ID
		ctx, cancel := context.WithTimeout(r.Context(), s.deps.RequestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		if op.Mutating() {
			r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes)
		}
		rec := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		c := &call{op: op, ip: clientIP(r)}
		defer func() {
			if c.audit && !c.auditSelf {
				s.recordAudit(c, r, rec.status)
			}
		}()
		if p := s.guard(c, r); p != nil {
			writeProblem(rec, r, p)
			return
		}
		if c.session != nil {
			st.actor = c.session.Username
		} else if c.viaSetupToken {
			st.actor = "setup"
		}
		if done, err := s.tryForward(c, rec, r); done {
			if err != nil {
				var p *problem.Problem
				if errors.As(err, &p) {
					writeProblem(rec, r, p)
				}
			}
			return
		}
		if p := s.auditReady(c, r); p != nil {
			writeProblem(rec, r, p)
			return
		}
		if err := h(c, rec, r); err != nil {
			var p *problem.Problem
			if !errors.As(err, &p) {
				s.log.Error("handler error", "request_id", st.id, "operation", op.ID, "error", err)
				p = problem.New(problem.InternalError, "An unexpected error occurred.")
			}
			writeProblem(rec, r, p)
		}
	})
}

// guard runs the checks in order and returns the problem that stops the
// request, or nil.
func (s *Server) guard(c *call, r *http.Request) *problem.Problem {
	op := c.op
	sessionOp := op.Accepts(contract.SchemeSessionCookie) || op.Accepts(contract.SchemeBearer)
	setupOp := op.Accepts(contract.SchemeSetupToken)
	uninitialized := s.deps.Setup.State() == setup.Uninitialized

	// 1. Node-state gating (README: while uninitialized only health and setup
	// work; everything else is 503 node_uninitialized).
	if uninitialized && op.Tag != contract.TagHealth && op.Tag != contract.TagSetup {
		return problem.New(problem.NodeUninitialized,
			"This server is not initialized yet. Use the setup wizard first.").WithRetryAfter(30)
	}

	// 2. Cross-site request check for every state-changing request.
	if op.Mutating() && r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return problem.New(problem.CSRFTokenInvalid, "Cross-site requests are not accepted.")
	}

	// 3. Rate limit on operations without a session. The liveness and readiness
	// probes of a load balancer are exempt: a flood from many addresses must not
	// make the health checks fail and take the server out of rotation (CP2-10).
	if !sessionOp && !probeOps[op.ID] {
		if ok, wait := s.publicLimiter.Allow(auth.AddressKey(c.ip)); !ok {
			return rateLimited(wait)
		}
	}

	// 3b. The instance of this server was stopped on purpose: everything that needs
	// the system database answers 503 system_database_stopped (README "Starting
	// and stopping the instance").
	if p := s.gateStoppedInstance(op); p != nil {
		return p
	}

	switch {
	case !sessionOp && !setupOp:
		// Public operation (health, login, setup state): nothing to check.
	case setupOp:
		// token-check, init-cluster and join-cluster. A valid request of the
		// audited operations is audited after the handler; a rejected token is
		// audited by checkSetupToken itself.
		c.audit = contract.IsAuditAction(op.ID)
		if p := s.checkSetupToken(c, r); p != nil {
			return p
		}
		c.viaSetupToken = true
	default:
		if p := s.checkSession(c, r); p != nil {
			return p
		}
	}
	if p := s.checkQuery(op, r); p != nil {
		return p
	}
	return nil
}

// probeOps are the operations of health probes; they reveal nothing the
// contract does not already make public.
var probeOps = map[string]bool{"getHealth": true, "getReadiness": true}

func rateLimited(wait time.Duration) *problem.Problem {
	secs := int(wait.Seconds()) + 1
	return problem.New(problem.RateLimited, "Too many requests; try again later.").WithRetryAfter(secs)
}

// checkSetupToken validates X-Setup-Token for the setup operations
// (checkSetupToken, initCluster, joinCluster), which are invalid once the node
// has left the uninitialized state. While the panel is localhost-only the
// caller must be a local address (403). Only failed attempts count against
// the limiter. A wrong token is audited as setupTokenRejected and the request
// is not audited a second time under its operation id.
func (s *Server) checkSetupToken(c *call, r *http.Request) *problem.Problem {
	mgr := s.deps.Setup
	if mgr.State() != setup.Uninitialized {
		c.audit = false
		return problem.New(problem.NodeStateInvalid, "This server is already initialized; setup is no longer possible.")
	}
	if s.deps.LoopbackOnly && !isLoopback(c.ip) {
		c.audit = false
		return problem.New(problem.Forbidden, "Setup is only possible from this machine until it is completed.")
	}
	if blocked, wait := s.setupLimiter.Blocked(auth.AddressKey(c.ip)); blocked {
		c.audit = false
		return rateLimited(wait)
	}
	tok := r.Header.Get(headerSetupToken)
	if tok == "" {
		c.audit = false
		return problem.New(problem.SetupTokenInvalid, "The "+headerSetupToken+" header is required.").
			WithHeader("WWW-Authenticate", "X-Setup-Token")
	}
	ok, err := mgr.VerifyToken(tok)
	if err != nil {
		c.audit = false
		return problem.New(problem.NodeStateInvalid, "This server is already initialized; setup is no longer possible.")
	}
	if !ok {
		c.audit = false
		s.setupLimiter.Record(auth.AddressKey(c.ip))
		s.log.Warn("invalid setup token", "request_id", stateOf(r).id, "remote", c.ip)
		s.appendAudit(r, audit.Event{
			Actor: "anonymous", ActorAddress: c.ip, Source: "rest", Action: "setupTokenRejected",
			ResourceType: "setup", Outcome: audit.OutcomeDenied, RequestID: stateOf(r).id,
			Details: map[string]any{"operationId": c.op.ID},
		}, "setupTokenRejected")
		return problem.New(problem.SetupTokenInvalid, "The setup token is not valid.").
			WithHeader("WWW-Authenticate", "X-Setup-Token")
	}
	return nil
}

// isLoopback reports whether the host part of a peer address is a loopback IP.
func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkSession authenticates the request with a bearer token or the session
// cookie, applies the CSRF check for cookie sessions on state-changing
// requests, and enforces the required role.
func (s *Server) checkSession(c *call, r *http.Request) *problem.Problem {
	if f := forwardedOf(r); f != nil {
		// Identity verified by the forwarding server and delivered through the
		// internal API (mutual TLS); there is no cookie, token or CSRF here.
		sess := forwardedSession(f)
		cur, perr := s.currentRoles(r, sess.Username)
		if perr != nil {
			return perr
		}
		// The claimed roles must still be held by the role on this server (CP-07);
		// authorization uses the current ones.
		if !auth.Subset(sess.ClusterRoles, cur) {
			return problem.New(problem.Unauthenticated, "The forwarded identity does not hold the claimed cluster roles.")
		}
		sess.ClusterRoles = cur
		c.session = &sess
		c.audit = contract.IsAuditAction(c.op.ID)
		if f.sourceAddress != "" {
			c.ip = f.sourceAddress
		}
		if !auth.Allows(sess.ClusterRoles, c.op.Role) {
			return problem.New(problem.Forbidden, "This operation requires the "+c.op.Role+" cluster role.")
		}
		if s.deps.Proxy != nil {
			// The identity is proven and authorized; only now is the role made
			// connectable for this server's own connections as that role (it writes
			// pg_ident.conf and reloads PostgreSQL, so it must not be done for a
			// name that was not accepted).
			if err := s.deps.Proxy.Allow(r.Context(), sess.Username); err != nil {
				s.log.Error("making the forwarded role connectable", "request_id", stateOf(r).id, "error", err)
				return problem.New(problem.ServiceUnavailable, "The role cannot be made connectable right now.").WithRetryAfter(5)
			}
		}
		return nil
	}
	sess, cred, p := s.authenticate(r)
	if p != nil {
		return p
	}
	cur, perr := s.sessionRoles(c, r, sess)
	if perr != nil {
		return perr
	}
	sess.ClusterRoles = cur
	c.session, c.credential = &sess, cred
	c.audit = contract.IsAuditAction(c.op.ID)

	if sess.Kind == auth.KindCookie && c.op.Mutating() {
		if !auth.CSRFValid(sess, r.Header.Get(headerCSRF)) {
			return problem.New(problem.CSRFTokenInvalid, "The "+headerCSRF+" header is missing or wrong.")
		}
	}
	if !auth.Allows(sess.ClusterRoles, c.op.Role) {
		return problem.New(problem.Forbidden, "This operation requires the "+c.op.Role+" cluster role.")
	}
	return nil
}

// authenticate resolves the credential of the request. A bearer token only
// authenticates a token session and a cookie only a cookie session, so the
// CSRF-exempt bearer path cannot be used with a stolen cookie value.
func (s *Server) authenticate(r *http.Request) (auth.Session, string, *problem.Problem) {
	unauth := func(code problem.Code, detail string) *problem.Problem {
		return problem.New(code, detail).WithHeader("WWW-Authenticate", `Bearer realm="kingresd"`)
	}
	var cred string
	var want auth.Kind
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
			return auth.Session{}, "", unauth(problem.Unauthenticated, "The Authorization header must be \"Bearer <token>\".")
		}
		cred, want = strings.TrimSpace(token), auth.KindToken
	} else if ck, err := r.Cookie(s.sessionCookieName()); err == nil && ck.Value != "" {
		cred, want = ck.Value, auth.KindCookie
	} else {
		return auth.Session{}, "", unauth(problem.Unauthenticated, "No valid session or token was presented.")
	}
	sess, err := s.deps.Sessions.Lookup(cred)
	switch {
	case errors.Is(err, auth.ErrExpired):
		return auth.Session{}, "", unauth(problem.SessionExpired, "The session has expired; sign in again.")
	case err != nil || sess.Kind != want:
		return auth.Session{}, "", unauth(problem.Unauthenticated, "No valid session or token was presented.")
	}
	return sess, cred, nil
}

// checkQuery rejects query parameters that the contract does not declare for
// the operation.
func (s *Server) checkQuery(op contract.Operation, r *http.Request) *problem.Problem {
	var errs []problem.FieldError
	for k := range r.URL.Query() {
		if !op.HasQueryParam(k) {
			errs = append(errs, problem.FieldError{Field: k, Message: "unknown query parameter", Code: "unknown_parameter"})
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return problem.Validation("The request has unknown query parameters.", errs...)
}

// notImplemented is the handler of every operation this server version does
// not implement. It runs after the guard, so authentication, CSRF and the
// role check have already happened and are testable on these operations.
func (s *Server) notImplemented(c *call, _ http.ResponseWriter, _ *http.Request) error {
	return problem.New(problem.UnsupportedOp,
		fmt.Sprintf("Operation %s is not supported by this server version yet.", c.op.ID))
}

// recordAudit appends the audit event of a request that passed the audit
// condition. A failure to write is logged but does not change the response:
// the action has already happened.
func (s *Server) recordAudit(c *call, r *http.Request, status int) {
	ev := audit.Event{
		Actor: "anonymous", ActorAddress: c.ip, Source: "rest", Action: c.op.ID,
		Outcome: outcomeOf(status), RequestID: stateOf(r).id,
		Details: map[string]any{"operationId": c.op.ID, "method": r.Method, "path": r.URL.Path, "status": status},
	}
	switch {
	case c.session != nil:
		ev.Actor = c.session.Username
		if c.session.Kind == auth.KindCookie {
			ev.Source = "panel"
		} else {
			ev.Source = "cli"
		}
		if f := forwardedOf(r); f != nil && (f.source == "panel" || f.source == "cli") {
			ev.Source = f.source
		}
	case c.viaSetupToken:
		ev.Actor = "setup"
	}
	for k, v := range c.details {
		ev.Details[k] = v
	}
	// The facts of a forwarded request are added here, after whatever the
	// handler put into c.details, so that no handler can drop them.
	if f := forwardedOf(r); f != nil {
		ev.Details["forwardedBy"] = f.peer
		ev.Details["originRequestId"] = f.requestID
	}
	ev.ResourceType, ev.ResourceID = resourceOf(c.op, r)
	s.appendAudit(r, ev, c.op.ID)
}

// appendAudit writes an event. The write must survive a canceled request
// context, so it runs on a detached context with its own timeout. A failure
// is logged: the action has already happened and cannot be undone here.
func (s *Server) appendAudit(r *http.Request, ev audit.Event, operation string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if _, err := s.deps.Audit.Append(ctx, ev); err != nil {
		s.log.Error("audit write failed", "request_id", stateOf(r).id, "operation", operation, "error", err)
	}
}

func outcomeOf(status int) string {
	switch {
	case status < 400:
		return audit.OutcomeSuccess
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return audit.OutcomeDenied
	default:
		return audit.OutcomeFailure
	}
}

// resourceOf derives the audited resource from the path parameters: the type
// is the first parameter name without its Name or Id suffix, the id joins the
// values of all path parameters.
func resourceOf(op contract.Operation, r *http.Request) (typ, id string) {
	if len(op.PathParams) == 0 {
		return strings.ToLower(op.Tag), ""
	}
	typ = strings.TrimSuffix(strings.TrimSuffix(op.PathParams[0], "Name"), "Id")
	vals := make([]string, 0, len(op.PathParams))
	for _, p := range op.PathParams {
		vals = append(vals, r.PathValue(p))
	}
	return typ, strings.Join(vals, "/")
}

// requireJSON checks the media type of a request that has a body.
func requireJSON(r *http.Request) *problem.Problem {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		return problem.New(problem.BadRequest, "The request must have Content-Type application/json.")
	}
	return nil
}

// currentRoles re-derives the cluster roles of the session's role from
// PostgreSQL (through the short cache of the revalidator): a role that was
// dropped, can no longer log in, has expired or lost its cluster roles loses
// the session at once. A backend that cannot be asked fails closed.
func (s *Server) currentRoles(r *http.Request, username string) ([]string, *problem.Problem) {
	if s.deps.LeaderTrusted != nil && !s.deps.LeaderTrusted() {
		p := problem.New(problem.LeaderUnavailable, "This server has lost contact with the manager leader and cannot tell whether the roles of this session were changed; try again when the leader is reachable, or open another server.").WithRetryAfter(5)
		p.ServerIDs = s.alternativeServers(r.Context())
		return nil, p
	}
	roles, err := s.reval.Roles(r.Context(), username)
	switch {
	case errors.Is(err, auth.ErrInvalidCredentials):
		return nil, problem.New(problem.Unauthenticated, "The role of this session may no longer sign in; sign in again.").
			WithHeader("WWW-Authenticate", `Bearer realm="kingresd"`)
	case err != nil:
		s.log.Error("re-checking the role of a session", "request_id", stateOf(r).id, "error", err)
		return nil, problem.New(problem.ServiceUnavailable, "The role of this session cannot be checked right now.").WithRetryAfter(5)
	case len(roles) == 0:
		return nil, problem.New(problem.Unauthenticated, "The role of this session no longer has a cluster role; sign in again.").
			WithHeader("WWW-Authenticate", `Bearer realm="kingresd"`)
	}
	return roles, nil
}

// RoleChanged ends the sessions of a role on this server and forgets the
// cached role checks; it is called after every change to a role, here and
// on the request of the leader.
func (s *Server) RoleChanged(username string) { s.reval.Changed(username) }

// alternativeServers names the servers a client can open instead of this one, from this
// server's own copy of the catalog: the manager leader first when it is known,
// then the others in the order of the list. It never asks the network.
func (s *Server) alternativeServers(ctx context.Context) []string {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	self, _ := s.deps.Store.LocalServerID(ctx)
	servers, err := s.deps.Store.Servers(ctx)
	if err != nil {
		return nil
	}
	var leaders, rest []string
	for _, sv := range servers {
		switch {
		case sv.ID == self:
		case sv.IsManagerLeader:
			leaders = append(leaders, sv.ID)
		default:
			rest = append(rest, sv.ID)
		}
	}
	return append(leaders, rest...)
}
