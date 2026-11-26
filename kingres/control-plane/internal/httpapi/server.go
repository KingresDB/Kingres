// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package httpapi is the HTTP layer of kingresd: it routes every operation
// of the contract (internal/contract), enforces node-state gating,
// authentication, CSRF protection, role-based authorization and auditing in
// one place, and implements the read-side handlers on top of store.Store.
//
// Operations that the server version does not implement yet answer with the
// uniform problem "unsupported_operation" (HTTP 422) after authentication
// and authorization have run.
package httpapi

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"sync/atomic"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/audit"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/localsettings"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/monitor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgconfig"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// Deps are the dependencies of the HTTP layer. All fields except Web are
// required.
type Deps struct {
	// Logger receives access and diagnostic logs.
	Logger *slog.Logger
	// Store is the read side.
	Store store.Store
	// Setup holds the node state and the setup token.
	Setup *setup.Manager
	// Sessions is the session store.
	Sessions *auth.Store
	// Authenticator verifies PostgreSQL credentials at login.
	Authenticator auth.Authenticator
	// Audit receives audit events.
	Audit audit.Sink
	// Proxy makes a role connectable after sign-in; Ops are the operations
	// that change the cluster (nil makes them answer "not supported").
	Proxy RoleConnector
	Ops   *ops.Ops
	// SetupService creates the cluster; nil or disabled makes initCluster answer
	// "not supported".
	SetupService SetupService
	// DataDir is the configured data directory of kingresd (validated against
	// the request of initCluster).
	DataDir string
	// Started is the process start time.
	Started time.Time
	// LogDir returns the directory of the instance log. It is asked on every read:
	// the directory can be moved with updateThisServerSettings.
	LogDir func() string
	// Metrics is the sampler ring (nil: no metrics); MetricsToken is the
	// optional scrape token of /metrics (empty: only stats-role bearer tokens).
	Metrics      *monitor.Ring
	MetricsToken string
	// ExtraMetrics, when set, writes more lines in the Prometheus text format after those of the ring (counters of the leader jobs).
	ExtraMetrics func(w io.Writer)
	// Forwarder sends cluster operations to the manager leader when this server
	// is not it; nil means this server is alone and runs everything itself.
	Forwarder Forwarder
	// Gatherer asks the other servers for their part of a cluster-wide read (nil:
	// this server answers for itself only); GatherTimeout is the time each gets.
	Gatherer      Gatherer
	GatherTimeout time.Duration
	// PeerUptime asks another server how long its kingresd has run (nil: the
	// uptime of other servers is not known, null in a Server).
	PeerUptime func(ctx context.Context, serverID string) (seconds int64, err error)
	// InternalAPIDisabled is true when this server was started without its
	// internal API (no -internal-listen): the operations that need the other
	// servers answer 409 internal_api_disabled instead of "not supported".
	InternalAPIDisabled bool
	// LeaderTrusted tells whether this server has heard from the manager leader
	// recently enough (the leader-loss bound) to trust its own copy of roles and
	// grants for authorizing a session. nil: always. A false answer makes every
	// session-authorized operation answer 503 leader_unavailable (README, "Heartbeats
	// and liveness", decision on CP2-07).
	LeaderTrusted func() bool
	// Members adds and removes servers (design D4 and D5); nil makes addServer
	// and removeServer answer "not supported".
	Members Members
	// Rebalance serves the Rebalance operations; nil makes them answer "not supported".
	Rebalance Rebalancer
	// Failover serves the Failover operations; nil makes them answer "not supported".
	Failover Failoverer
	// MapFailover answers the operations about the failover of the map group while the manager leader is gone; nil: none.
	MapFailover MapFailoverer
	// LedgerPublication serves the aggregates and the state of the publication of the ledger (phase 7); nil makes them answer "not supported".
	LedgerPublication LedgerPublisher
	// LedgerSigning serves the signing keys and the settings of the ledger (phase 7); nil makes them answer "not supported".
	LedgerSigning LedgerSigner
	// LedgerAnchoring serves the anchor targets and the anchor records of the ledger (phase 7, step 10); nil makes them answer "not supported".
	LedgerAnchoring LedgerAnchoring
	// Config serves the Configuration operations (phase 13); nil makes them answer "not supported".
	Config *pgconfig.Leader
	// LocalSettings serves getThisServerSettings and updateThisServerSettings;
	// nil makes them answer "not supported".
	LocalSettings *localsettings.Manager
	// StaleRolesMaxAge bounds the age of the last successful role check that
	// startThisServerInstance may use while the instance is stopped
	// (DefaultStaleRolesMaxAge when zero).
	StaleRolesMaxAge time.Duration
	// Web serves the panel at "/" (static files with SPA fallback); when nil
	// unknown non-API paths answer with the 404 problem.
	Web http.Handler
	// Version is the KingresDB version; Commit the source commit of the build ("unknown" or empty: not reported).
	Version string
	Commit  string
	// Hostname is the machine name reported by the setup state.
	Hostname string
	// ListenAddr is the configured listen address (host:port).
	ListenAddr string
	// LoopbackOnly tells whether the listen address is loopback only.
	LoopbackOnly bool
	// MaxBodyBytes limits request bodies.
	MaxBodyBytes int64
	// RequestTimeout bounds the processing of one API request.
	RequestTimeout time.Duration
	// LoginRateLimit and LoginRateWindow configure the login limiter.
	LoginRateLimit  int
	LoginRateWindow time.Duration
	// TLS tells that kingresd itself serves HTTPS (tls-cert-file is set): the
	// server then sends HSTS and uses the __Host- session cookie. It is never
	// derived from a request header.
	TLS bool
	// SessionTTL is the lifetime of sessions (for the cookie lifetime).
	SessionTTL time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// publicRateLimit is the number of requests per minute and client address on
// unauthenticated operations; setupFailLimit is the number of setup token
// attempts per minute and client address.
const (
	publicRateLimit = 600
	setupFailLimit  = 10
)

// Server is the HTTP handler of kingresd.
type Server struct {
	deps    Deps
	log     *slog.Logger
	now     func() time.Time
	ready   atomic.Bool
	handler http.Handler

	routes []Route

	publicLimiter *auth.Limiter     // all unauthenticated operations, per client address
	reval         *auth.Revalidator // re-derives the cluster roles of every request
	loginIP       *auth.Limiter     // login attempts per client address
	loginUser     *auth.Limiter     // login attempts per user name
	setupLimiter  *auth.Limiter     // setup token attempts per client address
	inst          instanceState     // start and stop of the local instance
}

// Route is a method and a full path pattern the server routes.
type Route struct {
	// Method is the HTTP method.
	Method string
	// Path is the full path pattern, for example "/api/v1/servers/{serverId}".
	Path string
	// OperationID is the contract operation.
	OperationID string
}

// New builds the server. It fails if a handler is registered for an
// operation that is not in the contract.
func New(d Deps) (*Server, error) {
	return build(d, (*Server).handlers)
}

// build is New with the handler registry injected, so that tests can check
// the registry validation.
func build(d Deps, registry func(*Server) map[string]handlerFunc) (*Server, error) {
	if d.Logger == nil || d.Store == nil || d.Setup == nil || d.Sessions == nil ||
		d.Authenticator == nil || d.Audit == nil {
		return nil, fmt.Errorf("httpapi: missing required dependency")
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	s := &Server{deps: d, log: d.Logger, now: now}
	s.reval = auth.NewRevalidator(d.Authenticator, d.Sessions, 0, now)
	s.publicLimiter = auth.NewLimiter(publicRateLimit, time.Minute, now)
	s.loginIP = auth.NewLimiter(d.LoginRateLimit, d.LoginRateWindow, now)
	s.loginUser = auth.NewLimiter(d.LoginRateLimit, d.LoginRateWindow, now)
	s.setupLimiter = auth.NewLimiter(setupFailLimit, time.Minute, now)

	mux := http.NewServeMux()
	handlers := registry(s)
	byPath := map[string]map[string]http.Handler{}
	known := map[string]bool{}
	for _, op := range contract.Operations() {
		known[op.ID] = true
		h, ok := handlers[op.ID]
		if !ok {
			h = s.notImplemented
		}
		if byPath[op.Path] == nil {
			byPath[op.Path] = map[string]http.Handler{}
		}
		byPath[op.Path][op.Method] = s.serveOp(op, h)
		s.routes = append(s.routes, Route{Method: op.Method, Path: op.FullPath(), OperationID: op.ID})
	}
	for id := range handlers {
		if !known[id] {
			return nil, fmt.Errorf("httpapi: handler for %q, which is not an operation of the contract", id)
		}
	}
	paths := make([]string, 0, len(byPath))
	for p := range byPath {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		mux.Handle(contract.BasePath+p, s.methodMux(byPath[p]))
	}
	mux.HandleFunc("/metrics", s.metricsEndpoint)
	// Everything else below /api is an unknown route.
	mux.HandleFunc("/api", s.notFoundRoute)
	mux.HandleFunc("/api/", s.notFoundRoute)
	if d.Web != nil {
		mux.Handle("/", d.Web)
	} else {
		mux.HandleFunc("/", s.notFoundRoute)
	}
	s.handler = s.chain(mux)
	return s, nil
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// SetReady marks the server ready (true) or shutting down (false); /readyz
// answers 503 while it is not ready.
func (s *Server) SetReady(v bool) { s.ready.Store(v) }

// Routes returns the method and path pattern of every routed operation.
func (s *Server) Routes() []Route { return append([]Route(nil), s.routes...) }

// methodMux dispatches by HTTP method for one path; other methods get 405
// with an Allow header.
func (s *Server) methodMux(byMethod map[string]http.Handler) http.Handler {
	allow := make([]string, 0, len(byMethod))
	for m := range byMethod {
		allow = append(allow, m)
	}
	sort.Strings(allow)
	allowHeader := ""
	for i, m := range allow {
		if i > 0 {
			allowHeader += ", "
		}
		allowHeader += m
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if h, ok := byMethod[r.Method]; ok {
			h.ServeHTTP(w, r)
			return
		}
		writeProblem(w, r, problem.New(problem.MethodNotAllowed,
			fmt.Sprintf("Method %s is not allowed on this path.", r.Method)).
			WithHeader("Allow", allowHeader))
	})
}

// notFoundRoute answers for paths that are not in the contract.
func (s *Server) notFoundRoute(w http.ResponseWriter, r *http.Request) {
	writeProblem(w, r, problem.New(problem.NotFound, "No such API route: "+r.URL.Path))
}
