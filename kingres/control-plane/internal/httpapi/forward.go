// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Forwarding to the manager leader (design control-plane-3.md, D6).
//
// A server that is not the leader does not run a cluster operation itself: it
// authenticates the caller as usual (session, CSRF, role), then sends the
// operation id, its path parameters, query and body together with the
// verified identity to the leader over the internal API and relays the
// leader's answer unchanged. The leader runs the operation through the same
// pipeline as a local request (query validation, role check, handler, audit),
// with the identity taken from the forward call instead of a session. The
// leader writes the audit event (details forwardedBy and originRequestId);
// the forwarding server writes none, unless the leader could not be reached.

// Forwarder is what the HTTP layer needs to forward an operation.
type Forwarder interface {
	// Leader returns the id of the manager leader and whether this server is
	// it. An error means the leader is not known; nothing is then forwarded
	// and nothing is run locally.
	Leader(ctx context.Context) (id string, self bool, err error)
	// Forward sends the operation to the leader.
	Forward(ctx context.Context, leader string, req internalapi.ForwardRequest) (internalapi.ForwardResponse, error)
}

// leaderBoundReads are read operations whose data lives only where the manager
// runs (the system database's task, alert and settings tables), so that they
// are answered by the leader too.
var leaderBoundReads = map[string]bool{
	"listTasks": true, "getTask": true, "listAlerts": true, "getClusterSettings": true,
}

// forwardsToLeader tells whether an operation is run by the leader. Mutating
// cluster operations are; the sign-in and sign-out of this server, the setup
// operations, the operations on this very server (tag "This server") and the
// query workbench (it runs as the signed-in role wherever the user is, and
// the sharding extension routes the statement) are not.
func forwardsToLeader(op contract.Operation) bool {
	if leaderBoundReads[op.ID] {
		return true
	}
	if !op.Mutating() {
		return false
	}
	switch op.Tag {
	case "Auth", contract.TagSetup, contract.TagHealth, "This server":
		return false
	}
	return op.ID != "executeQuery"
}

type fwdKey struct{}

// forwarded is the identity of a request that arrived through the internal
// API. Only ServeForwarded puts it into a context, so no public request can
// carry one.
type forwarded struct {
	peer          string
	identity      internalapi.ForwardIdentity
	requestID     string
	sourceAddress string
	source        string
	pin           string
}

func forwardedOf(r *http.Request) *forwarded {
	f, _ := r.Context().Value(fwdKey{}).(*forwarded)
	return f
}

// tryForward forwards the request when this server is not the leader. It
// returns true when it answered the request.
func (s *Server) tryForward(c *call, w http.ResponseWriter, r *http.Request) (bool, error) {
	fw := s.deps.Forwarder
	if fw == nil || forwardedOf(r) != nil || !forwardsToLeader(c.op) || c.session == nil {
		return false, nil
	}
	leaderID, self, err := fw.Leader(r.Context())
	if err != nil || leaderID == "" {
		if err != nil {
			s.log.Warn("manager leader unknown", "request_id", stateOf(r).id, "error", err)
		}
		return true, problem.New(problem.ServiceUnavailable, "The manager leader is not known; the cluster cannot change now.").WithRetryAfter(5)
	}
	if self {
		return false, nil
	}
	var body []byte
	if r.Body != nil && r.Body != http.NoBody {
		if body, err = io.ReadAll(r.Body); err != nil {
			return true, problem.New(problem.BadRequest, "The request body could not be read.")
		}
	}
	params := map[string]string{}
	for _, p := range c.op.PathParams {
		params[p] = r.PathValue(p)
	}
	src := "cli"
	if c.session.Kind == auth.KindCookie {
		src = "panel"
	}
	resp, err := fw.Forward(r.Context(), leaderID, internalapi.ForwardRequest{
		OperationID: c.op.ID, PathParams: params, Query: r.URL.RawQuery,
		ContentType: r.Header.Get("Content-Type"), Body: body, Pin: r.Header.Get(HeaderPin),
		Identity:  internalapi.ForwardIdentity{Username: c.session.Username, ClusterRoles: c.session.ClusterRoles},
		RequestID: stateOf(r).id, SourceAddress: c.ip, Source: src,
	})
	if err != nil {
		s.log.Warn("forwarding to the manager leader failed", "request_id", stateOf(r).id, "leader", leaderID, "error", err)
		c.details = map[string]any{"forwardedTo": leaderID}
		return true, problem.New(problem.ServiceUnavailable, "The manager leader cannot be reached; the cluster cannot change now.").WithRetryAfter(5)
	}
	// The leader audited the operation; this server must not do it again.
	c.audit = false
	h := w.Header()
	for k, v := range resp.Header {
		if relayedHeaders[k] {
			h.Set(k, v)
		}
	}
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
	return true, nil
}

// relayedHeaders are the response headers passed through in both directions.
var relayedHeaders = map[string]bool{
	"Content-Type": true, "Location": true, "Retry-After": true, "Allow": true, "Content-Language": true,
}

// ServeForwarded runs a forwarded operation on this server, which must be the
// leader (the internal API only calls it there). peer is the verified server
// id of the forwarding server.
func (s *Server) ServeForwarded(ctx context.Context, peer string, fr internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	var op contract.Operation
	found := false
	for _, o := range contract.Operations() {
		if o.ID == fr.OperationID {
			op, found = o, true
			break
		}
	}
	if !found || !forwardsToLeader(op) {
		return internalapi.ForwardResponse{}, fmt.Errorf("operation %q is not forwarded", fr.OperationID)
	}
	if err := validateIdentity(fr.Identity); err != nil {
		return internalapi.ForwardResponse{}, err
	}
	path := contract.BasePath + op.Path
	if len(fr.PathParams) != len(op.PathParams) {
		return internalapi.ForwardResponse{}, errors.New("path parameters do not match the operation")
	}
	for _, p := range op.PathParams {
		v, ok := fr.PathParams[p]
		if !ok || v == "" || len(v) > 512 {
			return internalapi.ForwardResponse{}, fmt.Errorf("path parameter %q is missing or too long", p)
		}
		path = strings.Replace(path, "{"+p+"}", url.PathEscape(v), 1)
	}
	if len(fr.Query) > 4096 || strings.ContainsAny(fr.Query, "\r\n") {
		return internalapi.ForwardResponse{}, errors.New("the query string is not valid")
	}
	if _, qerr := url.ParseQuery(fr.Query); qerr != nil {
		return internalapi.ForwardResponse{}, errors.New("the query string is not valid")
	}
	// The URL is assembled as a structure, never by concatenation: the path
	// is already escaped per segment, the query is only a query.
	unescaped, uerr := url.PathUnescape(path)
	if uerr != nil {
		return internalapi.ForwardResponse{}, errors.New("the path is not valid")
	}
	target := (&url.URL{Scheme: "http", Host: "leader.internal", Path: unescaped, RawPath: path, RawQuery: fr.Query}).String()
	req, err := http.NewRequestWithContext(context.WithValue(ctx, fwdKey{}, &forwarded{
		peer: peer, identity: fr.Identity, requestID: fr.RequestID, sourceAddress: fr.SourceAddress, source: fr.Source, pin: pinText(fr.Pin),
	}), op.Method, target, bytes.NewReader(fr.Body))
	if err != nil {
		return internalapi.ForwardResponse{}, fmt.Errorf("building the request: %w", err)
	}
	if len(fr.Body) > 0 {
		req.Header.Set("Content-Type", "application/json")
		if fr.ContentType != "" {
			req.Header.Set("Content-Type", fr.ContentType)
		}
	}
	if validRequestID(fr.RequestID) {
		req.Header.Set("X-Request-Id", fr.RequestID)
	}
	req.RemoteAddr = "forwarded:0"
	if s.deps.Proxy != nil {
		// The forwarding server verified the password; the leader makes the
		// role connectable for its own connections as that role.
		if err := s.deps.Proxy.Allow(ctx, fr.Identity.Username); err != nil {
			return internalapi.ForwardResponse{}, fmt.Errorf("making the role connectable: %w", err)
		}
	}
	rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
	s.handler.ServeHTTP(rec, req)
	out := internalapi.ForwardResponse{Status: rec.status, Body: rec.body.Bytes(), Header: map[string]string{}}
	for k := range relayedHeaders {
		if v := rec.header.Get(k); v != "" {
			out.Header[k] = v
		}
	}
	return out, nil
}

func validateIdentity(id internalapi.ForwardIdentity) error {
	if id.Username == "" || len(id.Username) > 63 || strings.ContainsRune(id.Username, 0) {
		return errors.New("the identity has no valid user name")
	}
	for _, r := range id.ClusterRoles {
		if !auth.IsClusterRole(r) {
			return errors.New("the identity names an unknown cluster role")
		}
	}
	return nil
}

// forwardedSession turns the verified identity of a forwarded call into the
// session the guard works with.
func forwardedSession(f *forwarded) auth.Session {
	return auth.Session{Username: f.identity.Username, ClusterRoles: auth.Normalize(f.identity.ClusterRoles), Kind: auth.KindToken}
}

// maxForwardedResponse bounds what a forwarded operation may answer.
const maxForwardedResponse = 32 << 20

// bufferedResponse is a minimal http.ResponseWriter that keeps the answer.
type bufferedResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header  { return b.header }
func (b *bufferedResponse) WriteHeader(code int) { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.body.Len()+len(p) > maxForwardedResponse {
		return 0, errors.New("the answer is too large to forward")
	}
	return b.body.Write(p)
}

// pinText normalizes the pinned fingerprint of a forwarded request ("" when
// it is not one: a server of the cluster already checked it).
func pinText(s string) string { return pki.NormalizeFingerprint(s) }
