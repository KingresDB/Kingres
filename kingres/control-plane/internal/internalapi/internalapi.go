// SPDX-License-Identifier: AGPL-3.0-only

// Package internalapi is the server-to-server API of kingresd (design
// control-plane-3.md, D2), specified in kingres/control-plane/internal-api.md.
//
// It authenticates by client certificate only: the peer must present a
// certificate signed by the cluster CA that is valid and not revoked, and
// every call is authorized by a fixed table (Policy) of which peers may make
// it. It never accepts a session or a setup token, never executes a path or
// program name from a request, and logs every statement-execution call with
// its length and hash, never its text.
package internalapi

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fault"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
)

// Version is the version of the internal protocol; it is bumped with every
// incompatible change and is independent of the public contract revision.
const Version = "1"

// HeaderVersion carries the caller's internal protocol version.
const HeaderVersion = "X-Kingres-Internal-Version"

// Prefix is the path prefix of the internal API.
const Prefix = "/internal/v1"

// Policy says who may make a call.
type Policy int

const (
	// AnyPeer: every server of the cluster.
	AnyPeer Policy = iota
	// LeaderOnly: only the server that is the manager leader.
	LeaderOnly
	// ToLeader: any peer may call, but only the manager leader answers; a
	// server that is not the leader refuses with not_leader.
	ToLeader
	// LeaderOrJoining: the leader, and also any peer of the cluster while this
	// server is not part of the cluster yet (its catalog does not exist, so it
	// cannot know the leader). Every such call needs a valid cluster
	// certificate and is refused once the server is clustered unless the
	// caller is the leader.
	LeaderOrJoining
	// Bootstrap: no client certificate. The first contact of a join, before
	// the two sides share a CA: authenticated by the new server's setup token
	// or by administrator credentials, over TLS whose certificate the caller
	// pins or records (trust on first use).
	Bootstrap
)

// Calls is the authorization table. A path that is not in it does not exist.
var Calls = map[string]Policy{
	"GET /state":     AnyPeer,
	"GET /resources": LeaderOnly,
	"GET /logs":      LeaderOnly,
	"GET /metrics":   LeaderOnly,
	"POST /exec":     LeaderOnly,
	"GET /revoked":   AnyPeer,
	"GET /leader":    AnyPeer,
	"POST /forward":  ToLeader,
	// Cluster-wide reads: a server asks the others for their part of the logs, alerts or
	// metrics (and an acknowledgement that belongs to one server); every server answers
	// after it has checked the identity the leader's assertion vouches for.
	"POST /gather": AnyPeer,
	// The leader signs an assertion for a gather call: the asking server proves its session
	// to the leader only (the proof reveals the ClientKey), the members that answer the
	// gather call accept the leader's assertion and never see a proof.
	"POST /gather-authorize": ToLeader,

	// Joining and adding a server (D4), removing it (D5), and the calls the
	// leader needs on another server. Nothing in them takes a path, a program
	// name or SQL text.
	"POST /bootstrap/prepare": Bootstrap,
	"POST /bootstrap/install": Bootstrap,
	"POST /bootstrap/join":    Bootstrap,
	"POST /add-server":        ToLeader,
	"GET /instance/status":    LeaderOrJoining,
	"POST /instance/create":   LeaderOrJoining,
	"POST /instance/start":    LeaderOnly,
	"POST /instance/stop":     LeaderOnly,
	"POST /instance/remove":   LeaderOnly,
	"POST /instance/trust":    LeaderOrJoining,
	"POST /pki/install":       LeaderOrJoining,
	"POST /peers":             LeaderOrJoining,
	"POST /globals/apply":     LeaderOrJoining,
	"POST /node/finish":       LeaderOrJoining,
	"POST /node/reset":        LeaderOrJoining,
	"POST /apply":             LeaderOnly,

	// Certificate renewal: a server asks the manager leader, which holds the CA, for a
	// new certificate for itself; the leader asks a server for a certificate
	// request when an administrator renews its certificates.
	"POST /pki/renew": ToLeader,
	"GET /pki/csr":    LeaderOnly,

	// Leaving the cluster (D5, mirror of removing a server): the leaving server
	// asks the leader, which answers only for the caller named by the certificate.
	"POST /leave-cluster": ToLeader,
	"POST /leave-status":  ToLeader,

	// Recording the task of an instance start or stop of the calling server (the
	// system database is held by the leader).
	"POST /task/record":  ToLeader,
	"POST /task/settle":  ToLeader,
	"POST /task/reserve": ToLeader,

	// High availability, part 1 (internal-api.md, "Heartbeats", "Leases", "Standbys"). Heartbeats
	// and leases are between any two servers; the standby calls come from the leader.
	"POST /heartbeat":       AnyPeer,
	"POST /lease/grant":     AnyPeer,
	"GET /lease/state":      AnyPeer,
	"POST /standby/plan":    LeaderOnly,
	"POST /standby/create":  LeaderOnly,
	"POST /standby/remove":  LeaderOnly,
	"POST /standby/rebuild": LeaderOnly,
	"GET /standby/status":   LeaderOnly,

	// The read calls and the freeze of the failover (internal-api.md, "Failover groundwork").
	"GET /failover/position":       AnyPeer,
	"GET /liveness":                AnyPeer,
	"GET /failover/state":          LeaderOnly,
	"POST /failover/freeze":        LeaderOnly,
	"POST /failover/unfreeze":      LeaderOnly,
	"POST /failover/standbys-gone": LeaderOnly,

	// Configuration from the console (phase 13): the leader reads and sets the parameters of every
	// server; any server answers the clock probe.
	"POST /config/state":   LeaderOnly,
	"POST /config/apply":   LeaderOnly,
	"POST /config/check":   LeaderOnly,
	"POST /config/restart": LeaderOnly,
	"GET /time":            AnyPeer,
}

// handled lists the calls dispatched to Backend.Handle (the rest have their
// own code in ServeHTTP).
var handled = map[string]bool{
	"POST /task/record": true, "POST /task/settle": true, "POST /task/reserve": true,
	"POST /leave-cluster": true, "POST /leave-status": true,
	"POST /add-server": true, "GET /instance/status": true, "POST /instance/create": true, "POST /instance/start": true,
	"POST /instance/stop": true, "POST /instance/remove": true, "POST /instance/trust": true, "POST /pki/install": true,
	"POST /peers": true, "POST /globals/apply": true, "POST /node/finish": true, "POST /node/reset": true, "POST /apply": true,
	"POST /pki/renew": true, "GET /pki/csr": true,
	"POST /heartbeat": true, "POST /lease/grant": true, "GET /lease/state": true, "POST /standby/plan": true,
	"POST /standby/create": true, "POST /standby/remove": true, "POST /standby/rebuild": true, "GET /standby/status": true,
	"GET /failover/position": true, "GET /liveness": true, "GET /failover/state": true, "POST /failover/freeze": true,
	"POST /failover/unfreeze": true, "POST /failover/standbys-gone": true,
	"POST /config/state": true, "POST /config/apply": true, "POST /config/check": true, "POST /config/restart": true, "GET /time": true,
}

// CallError is an error with the status and code the internal API answers.
type CallError struct {
	Status  int
	Code    string
	Message string
}

func (e *CallError) Error() string { return e.Code + ": " + e.Message }

// PeerVerifier accepts or refuses a peer certificate and returns the server
// id in it (*pki.CA and *pki.Verifier implement it).
type PeerVerifier interface {
	VerifyPeer(cert *x509.Certificate, now time.Time) (string, error)
}

// State is the answer of GET /state.
type State struct {
	// ServerID is this server's id; NodeState its node state.
	ServerID        string `json:"serverId"`
	NodeState       string `json:"nodeState"`
	Version         string `json:"version"`
	InternalVersion string `json:"internalVersion"`
	UptimeSeconds   int64  `json:"uptimeSeconds"`
	// PostgreSQL and Extensions are the versions of the instance of this server and
	// of the KingresDB extensions its binaries offer (the manager leader compares
	// them between the servers); empty when they could not be read.
	PostgreSQL string            `json:"postgresql,omitempty"`
	Extensions map[string]string `json:"extensions,omitempty"`
}

// VersionInfo is what a server knows of the versions of its instance.
type VersionInfo struct {
	PostgreSQL string
	Extensions map[string]string
}

// ExecRequest is the body of POST /exec: one statement on a local database.
type ExecRequest struct {
	Database string `json:"database"`
	SQL      string `json:"sql"`
	// RowLimit bounds the rows returned (default 1000, at most 10000).
	RowLimit int `json:"rowLimit,omitempty"`
}

// ExecResult is the answer of POST /exec.
type ExecResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
	Tag     string   `json:"commandTag"`
}

// ForwardIdentity is the authenticated identity of the user whose request is
// forwarded: the role that signed in and the cluster roles it had.
type ForwardIdentity struct {
	Username     string   `json:"username"`
	ClusterRoles []string `json:"clusterRoles"`
	// ProofKey is the key of the session that makes the Proof of the request. It
	// is only ever used by the sending side and is never serialized.
	ProofKey []byte `json:"-"`
}

// ForwardRequest is the body of POST /forward: one operation of the public
// contract, by operation id and path parameters (never a path), with its query
// string and body and the identity the forwarding server verified.
type ForwardRequest struct {
	OperationID   string            `json:"operationId"`
	PathParams    map[string]string `json:"pathParams,omitempty"`
	Query         string            `json:"query,omitempty"`
	ContentType   string            `json:"contentType,omitempty"`
	Body          []byte            `json:"body,omitempty"`
	Identity      ForwardIdentity   `json:"identity"`
	RequestID     string            `json:"requestId,omitempty"`
	SourceAddress string            `json:"sourceAddress,omitempty"`
	// Source is "panel" or "cli".
	Source string `json:"source,omitempty"`
	// Ordinal is, in a gather call, the ordinal of the server that answers (its
	// alert ids are made from it).
	Ordinal int `json:"ordinal,omitempty"`
	// Pin is the fingerprint the administrator pinned (addServer only).
	Pin string `json:"pin,omitempty"`
	// Prefer is the Prefer header of the request (updateClusterSettings: respond-async).
	Prefer string `json:"prefer,omitempty"`
	// Proof shows that the sending server holds a session of Identity.Username
	// (package fwdproof). Forward and gather-authorize calls are refused without
	// it; a gather call never carries one (it would reveal the user's ClientKey
	// to a member) and is refused if it does.
	Proof *fwdproof.Proof `json:"proof,omitempty"`
	// Assertion is what a gather call carries instead: the leader's signed
	// statement that the sending server holds that session.
	Assertion *fwdproof.Assertion `json:"assertion,omitempty"`
}

// ProofMessage is what the Proof of a request is bound to; from and to are the
// sending and the receiving server.
func (r ForwardRequest) ProofMessage(kind fwdproof.Kind, from, to string) fwdproof.Message {
	return fwdproof.Message{
		Kind: kind, From: from, To: to, Username: r.Identity.Username, Roles: r.Identity.ClusterRoles,
		Operation: r.OperationID, PathParams: r.PathParams, Query: r.Query, ContentType: r.ContentType, Body: r.Body,
		RequestID: r.RequestID, Source: r.Source, SourceAddress: r.SourceAddress, Pin: r.Pin, Ordinal: r.Ordinal,
	}
}

// ForwardResponse is the answer of the leader to a forwarded operation, to be
// relayed unchanged.
type ForwardResponse struct {
	Status int               `json:"status"`
	Header map[string]string `json:"header,omitempty"`
	Body   []byte            `json:"body,omitempty"`
}

// Backend is what the server provides to the internal API.
type Backend struct {
	// Self is the server id of this server.
	Self string
	// Forward runs a forwarded operation as the identity in the request; peer is
	// the verified server id of the caller. It is only called on the leader.
	Forward func(ctx context.Context, peer string, req ForwardRequest) (ForwardResponse, error)
	// State reports this server.
	State func(ctx context.Context) State
	// Leader returns the server id of the manager leader, "" when unknown (then
	// no leader-only call is accepted).
	Leader func() string
	// Revoked returns the revoked serial numbers (hex).
	Revoked func() []string
	// Resources, Logs, Metrics return JSON-encodable values.
	Resources func(ctx context.Context) (any, error)
	Logs      func(ctx context.Context, query map[string]string) (any, error)
	Metrics   func(ctx context.Context) (any, error)
	// Exec runs a statement on a local instance through the local socket.
	Exec func(ctx context.Context, req ExecRequest) (ExecResult, error)
	// Gather answers one cluster-wide read for the server peer (nil: not
	// available). The answer is relayed as it is.
	Gather func(ctx context.Context, peer string, req ForwardRequest) (ForwardResponse, error)
	// Handle runs one of the membership calls (the keys of Calls that are not
	// handled by the server itself) with the raw JSON body; peer is the
	// verified server id of the caller. It returns the answer or a *CallError.
	Handle func(ctx context.Context, key, peer string, body []byte) (any, error)
	// Bootstrap runs a Bootstrap call. r gives the headers (the setup token)
	// and the address of the caller.
	Bootstrap func(ctx context.Context, key string, r *http.Request, body []byte) (any, error)
	// VerifyIdentity checks the proof that comes with a forwarded or gathered
	// request against the verifier of the user it names (nil: not checked, for
	// tests of the table only). The server never acts for an identity it did
	// not verify.
	VerifyIdentity func(ctx context.Context, kind fwdproof.Kind, peer string, req ForwardRequest) error
	// Authorize makes the leader's assertion for a gather call whose proof was
	// verified (only called on the leader).
	Authorize func(ctx context.Context, peer string, req ForwardRequest) (fwdproof.Assertion, error)
	// VerifyAssertion checks the assertion of a gather call (nil: not checked, for
	// tests of the table only).
	VerifyAssertion func(ctx context.Context, peer string, req ForwardRequest) error
	// Member tells whether a server is a member of the cluster: active, or
	// being removed. A server whose certificate was issued but which has not
	// been admitted yet (pending) is not one, and may make only the calls of
	// its own join (nil: every peer is one, for tests of the table only). It is
	// checked for every call answered by the leader except POST /forward, whose
	// Forward function checks it itself.
	Member func(ctx context.Context, peer string) (bool, error)
	// Joining reports whether this server is not part of a cluster yet but
	// holds a certificate of one (a join is under way).
	Joining func() bool
	// Unavailable reports that this server cannot serve for a reason that ends by itself: its
	// instance is held back until its group lease is won, so it cannot read the catalog and does not
	// know the leader (nil: never). Such a server answers 503 service_unavailable with Retry-After,
	// not 403 not_leader, and task steps wait for it.
	Unavailable func() bool
	// Fault is the fault-injection hook of the tests (nil in production): a call from
	// a blocked peer is dropped without an answer.
	Fault *fault.Hook
	// Fence remembers the highest epoch seen per task and refuses calls of a superseded
	// run (nil: calls are not fenced).
	Fence *fence.Registry
}

// Server is the HTTP handler of the internal API.
type Server struct {
	verifier PeerVerifier
	be       Backend
	log      *slog.Logger
	now      func() time.Time
	// leaderRetry is the pause between asks for an unknown leader.
	leaderRetry time.Duration
}

// New builds the server. now may be nil.
func New(v PeerVerifier, be Backend, log *slog.Logger, now func() time.Time) *Server {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Server{verifier: v, be: be, log: log, now: now, leaderRetry: 200 * time.Millisecond}
}

// TLSConfig returns the server TLS configuration of the internal listener: TLS
// 1.3, the server's own certificate, and a client certificate required (it is
// verified by the handler against the CA and the revocation list, so that the
// refusal names the reason).
func TLSConfig(cert tls.Certificate) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequestClientCert}
}

type problem struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, problem{code, msg})
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 1. Version and known call. The version check comes first for every
	// call, certificate or not, so that a refusal names both versions.
	rest := r.URL.Path
	key := ""
	if len(rest) >= len(Prefix) && rest[:len(Prefix)] == Prefix {
		key = r.Method + " " + rest[len(Prefix):]
	}
	policy, known := Calls[key]
	if !known {
		fail(w, http.StatusNotFound, "not_found", "No such call.")
		return
	}
	if v := r.Header.Get(HeaderVersion); v != Version {
		fail(w, http.StatusConflict, "version_mismatch",
			fmt.Sprintf("This server speaks internal API version %s, the caller %q.", Version, v))
		return
	}
	if policy == Bootstrap {
		s.bootstrap(w, r, key)
		return
	}
	// 2. Certificate.
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		fail(w, http.StatusUnauthorized, "no_certificate", "A client certificate signed by the cluster CA is required.")
		return
	}
	peer, err := s.verifier.VerifyPeer(r.TLS.PeerCertificates[0], s.now())
	if err != nil {
		s.log.Warn("internal call refused", "reason", "certificate", "error", err, "remote", r.RemoteAddr)
		fail(w, http.StatusUnauthorized, "certificate_refused", "The client certificate was not accepted.")
		return
	}
	if s.be.Fault.InboundBlocked(peer) {
		dropConnection(w)
		return
	}
	// 3. Authorization.
	if policy == ToLeader {
		if s.notReady(r.Context()) {
			failNotReady(w)
			return
		}
		if l := s.leader(r.Context()); l == "" || l != s.be.Self || (key == "POST /forward" && s.be.Forward == nil) {
			fail(w, http.StatusConflict, "not_leader", "This server is not the manager leader.")
			return
		}
	}
	if policy == LeaderOnly || policy == LeaderOrJoining {
		joining := policy == LeaderOrJoining && s.be.Joining != nil && s.be.Joining()
		// A joining server has no catalog and knows no leader: it is not asked (and not waited for).
		if !joining {
			if s.notReady(r.Context()) {
				failNotReady(w)
				return
			}
			if l := s.leader(r.Context()); l == "" || l != peer {
				s.log.Warn("internal call refused", "reason", "not the leader", "call", key, "peer", peer)
				fail(w, http.StatusForbidden, "not_leader", "Only the manager leader may make this call.")
				return
			}
			// The caller is the leader: the task run it makes the call for is fenced.
			if hdr := r.Header.Get(fence.Header); hdr != "" && s.be.Fence != nil {
				tok, err := fence.Parse(hdr)
				if err != nil {
					fail(w, http.StatusBadRequest, "bad_request", "The fence header is malformed.")
					return
				}
				if err := s.be.Fence.Check(tok); err != nil {
					s.log.Warn("internal call refused", "reason", "fenced", "call", key, "peer", peer, "task", tok.Task, "epoch", tok.Epoch)
					fail(w, http.StatusConflict, "fenced", "The task run that made this call was superseded by a newer run.")
					return
				}
			}
		}
	}
	// A forwarded operation is gated by the backend's Forward (it must answer the stopped
	// instance of the leader in the documented way, see cmd/kingresd forwardGate).
	if policy == ToLeader && key != "POST /forward" && s.be.Member != nil {
		member, err := s.isMember(r.Context(), peer)
		switch {
		case err != nil:
			s.log.Error("internal call refused", "reason", "membership unknown", "call", key, "peer", peer, "error", err)
			fail(w, http.StatusServiceUnavailable, "unavailable", "Whether the caller is a member cannot be checked now.")
			return
		case !member:
			s.log.Warn("internal call refused", "reason", "not a member", "call", key, "peer", peer)
			fail(w, http.StatusForbidden, "not_member", "The calling server is not a member of the cluster.")
			return
		}
	}
	limit := int64(1 << 20)
	if policy == ToLeader || key == "POST /globals/apply" || key == "POST /apply" {
		limit = 16 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	ctx := r.Context()
	switch key {
	case "GET /state":
		writeJSON(w, http.StatusOK, s.be.State(ctx))
	case "GET /leader":
		writeJSON(w, http.StatusOK, map[string]string{"leader": s.be.Leader()})
	case "GET /revoked":
		writeJSON(w, http.StatusOK, map[string]any{"serials": s.be.Revoked()})
	case "GET /resources":
		s.answer(w, func() (any, error) { return s.be.Resources(ctx) })
	case "GET /metrics":
		s.answer(w, func() (any, error) { return s.be.Metrics(ctx) })
	case "GET /logs":
		q := map[string]string{}
		for _, k := range []string{"level", "from", "to", "limit", "cursor"} {
			if v := r.URL.Query().Get(k); v != "" {
				q[k] = v
			}
		}
		s.answer(w, func() (any, error) { return s.be.Logs(ctx, q) })
	case "POST /exec":
		s.exec(w, r, peer)
	case "POST /forward":
		s.forward(w, r, peer)
	case "POST /gather":
		s.gather(w, r, peer)
	case "POST /gather-authorize":
		s.gatherAuthorize(w, r, peer)
	default:
		if handled[key] {
			s.handle(w, r, key, peer)
		}
	}
}

func (s *Server) answer(w http.ResponseWriter, f func() (any, error)) {
	v, err := f()
	if err != nil {
		s.log.Error("internal call failed", "error", err)
		fail(w, http.StatusInternalServerError, "failed", "The call failed.")
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (s *Server) exec(w http.ResponseWriter, r *http.Request, peer string) {
	var req ExecRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		if err == io.EOF {
			fail(w, http.StatusBadRequest, "bad_request", "The body is empty.")
			return
		}
		fail(w, http.StatusBadRequest, "bad_request", "The body is not a valid exec request.")
		return
	}
	if req.Database == "" || req.SQL == "" || len(req.SQL) > 1_000_000 || req.RowLimit < 0 || req.RowLimit > 10000 {
		fail(w, http.StatusBadRequest, "bad_request", "database and sql are required; sql is at most 1000000 bytes; rowLimit at most 10000.")
		return
	}
	sum := sha256.Sum256([]byte(req.SQL))
	// The statement text is not logged, only its length and a hash.
	s.log.Info("internal exec", "peer", peer, "database", req.Database, "sql_length", len(req.SQL), "sql_hash", hex.EncodeToString(sum[:8]))
	if s.be.Exec == nil {
		fail(w, http.StatusNotImplemented, "not_available", "This server cannot run statements.")
		return
	}
	res, err := s.be.Exec(r.Context(), req)
	if err != nil {
		fail(w, http.StatusUnprocessableEntity, "statement_failed", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// readBody reads at most limit bytes of the request body.
func readBody(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// answerError writes the answer for an error of a membership call.
func (s *Server) answerError(w http.ResponseWriter, key string, err error) {
	var ce *CallError
	if errors.As(err, &ce) {
		fail(w, ce.Status, ce.Code, ce.Message)
		return
	}
	s.log.Error("internal call failed", "call", key, "error", err)
	fail(w, http.StatusInternalServerError, "failed", "The call failed.")
}

// handle runs one membership call through the backend.
func (s *Server) handle(w http.ResponseWriter, r *http.Request, key, peer string) {
	if s.be.Handle == nil {
		fail(w, http.StatusNotImplemented, "not_available", "This server cannot do that.")
		return
	}
	body, err := readBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The body could not be read.")
		return
	}
	if r.Method == http.MethodGet && len(body) == 0 {
		body = []byte(r.URL.RawQuery) // a GET carries its parameters in the query; the handler reads them from here
	}
	res, err := s.be.Handle(r.Context(), key, peer, body)
	if err != nil {
		s.answerError(w, key, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// bootstrap answers the calls without a client certificate. The backend
// authenticates them (setup token or administrator credentials) and rate
// limits failures per remote address; the body is size-limited here.
func (s *Server) bootstrap(w http.ResponseWriter, r *http.Request, key string) {
	if s.be.Bootstrap == nil {
		fail(w, http.StatusNotImplemented, "not_available", "This server cannot do that.")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	body, err := readBody(r)
	if err != nil {
		fail(w, http.StatusBadRequest, "bad_request", "The body could not be read.")
		return
	}
	res, err := s.be.Bootstrap(r.Context(), key, r, body)
	if err != nil {
		s.answerError(w, key, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// TLSConfigFunc is TLSConfig with the certificate chosen at every handshake:
// the server's cluster certificate once it has one, before that a bootstrap
// certificate (see package pki).
func TLSConfigFunc(get func(*tls.ClientHelloInfo) (*tls.Certificate, error)) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: get, ClientAuth: tls.RequestClientCert}
}

// dropConnection closes the connection without an answer, as a network that
// loses the packets would look to the caller (the fault hook of the tests).
func dropConnection(w http.ResponseWriter) {
	if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = c.Close()
		return
	}
	panic(http.ErrAbortHandler)
}

// leader returns the manager leader this server knows. "" means the catalog could not
// be read or names none, which on a loaded machine or right after a server joined is
// momentary: the answer is asked again a few times (about 0.6 s in all) before a call is
// refused with not_leader. A leader that is known is never second-guessed here.
func (s *Server) leader(ctx context.Context) string {
	l := s.be.Leader()
	for i := 0; l == "" && i < 3; i++ {
		select {
		case <-ctx.Done():
			return ""
		case <-time.After(s.leaderRetry):
		}
		l = s.be.Leader()
	}
	return l
}

// notReady tells whether the server cannot know the leader because it is held back (see
// Backend.Unavailable): only then is an unknown leader "not yet" instead of "not you".
func (s *Server) notReady(ctx context.Context) bool {
	if s.be.Unavailable == nil || s.be.Leader() != "" {
		return false
	}
	return s.be.Unavailable()
}

func failNotReady(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	fail(w, http.StatusServiceUnavailable, "service_unavailable", "This server cannot serve yet: its instance waits for its group lease.")
}

// memberWait bounds how long a call waits for the catalog when it cannot say
// whether the caller is a member: the leader's own instance may be restarting for a
// moment (fenced and started again by the lease manager), which a caller should not
// see as a refusal. After the bound the call is refused (fail closed).
const memberWait = 5 * time.Second

// isMember asks Backend.Member again, with short pauses, while it fails.
func (s *Server) isMember(ctx context.Context, peer string) (bool, error) {
	deadline := time.Now().Add(memberWait)
	for {
		ok, err := s.be.Member(ctx, peer)
		if err == nil || time.Now().Add(s.leaderRetry).After(deadline) {
			return ok, err
		}
		select {
		case <-ctx.Done():
			return ok, err
		case <-time.After(s.leaderRetry):
		}
	}
}
