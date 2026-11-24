// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
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
	"listTasks": true, "getTask": true, "getClusterSettings": true, "getCaEscrow": true,
	// The extension versions of the other servers are read by the leader (the internal call exec is leader-only).
	"getApproximateAggregates": true,
	// The policies, their runs and the partitions of a plain table are read from the instance that the leader runs.
	"getPartitionPolicy": true, "listPartitionPolicyRuns": true, "getPartitionPolicyRun": true, "listTablePartitions": true,
	// Configuration from the console (phase 13): the leader can ask every server.
	"listConfigParameters": true, "getConfigParameter": true, "listConfigChanges": true, "listConfigServers": true,
	"getConfigSummary": true, "getClusterTime": true, "listTimeZones": true,
	// The groups are state of the leader's system database (see completeServers).
	"listServerGroups": true,
	// The records of a rebalance run are in the leader's system database.
	"getRebalanceStatus": true, "listRebalanceReports": true, "getRebalanceReport": true,
	// The records of a failover run are in the leader's system database too.
	"getFailoverStatus": true, "listFailoverReports": true, "getFailoverReport": true,
	// The verification records get their identifiers in the system database
	// the first time they are read (README, "Inclusion proofs and verification records").
	"listLedgerVerifications": true, "getLedgerVerification": true,
	// The signing keys, the events and the trust file are in the leader's system database (phase 7).
	"getLedgerSettings": true, "getLedgerSigning": true, "getLedgerTrustFile": true,
	// The logs, the aggregates and the digests of the publication are in the leader's system database (phase 7, steps 5 and 6).
	"getLedgerPublication": true, "getLedgerAggregate": true, "listLedgerAggregates": true, "listLedgerDigests": true,
	// The anchor targets and records are in the leader's system database too (phase 7, step 10).
	"listLedgerAnchorTargets": true, "getLedgerAnchorTarget": true, "listLedgerAnchors": true, "downloadLedgerAnchor": true, "getLedgerBundle": true,
	// The staged files of the data transfer and their records are on the leader (contract 0.19.0). The other operations of the tag
	// change something and go to the leader as every mutating operation does; the two streaming ones are relayed.
	"getDataUpload": true, "listDataExports": true, "getDataExport": true, "downloadDataExport": true,
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
	// createInclusionProof is a POST that changes nothing: it reads the row as
	// the signed-in role, like the query workbench, wherever the user is.
	// acknowledgeAlert goes to the server that holds the alert (the ordinal in its id).
	return op.ID != "executeQuery" && op.ID != "cancelStatement" && op.ID != "createInclusionProof" && op.ID != "acknowledgeAlert"
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
	if done, err := s.mapFailoverIntercept(c, w, r); done {
		return true, err
	}
	if c.op.Streams != "" {
		// The relay does not buffer: the body goes to the leader as it is read and the answer comes back as it is written.
		return s.tryForwardStream(c, w, r)
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
	pin, err := s.forwardPin(c.op, r, body)
	if err != nil {
		return true, err
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
		ContentType: r.Header.Get("Content-Type"), Body: body, Pin: pin, Prefer: r.Header.Get("Prefer"),
		Identity:  identityOf(c.session),
		RequestID: stateOf(r).id, SourceAddress: c.ip, Source: src,
	})
	var refused *peerclient.APIError
	if errors.As(err, &refused) && refused.Code == "identity_unproven" {
		// The leader does not accept this session's proof: the password changed, or the
		// role has no SCRAM verifier. Signing in again opens a session that can prove itself.
		return true, problem.New(problem.Unauthenticated, "This session cannot be used for operations of the cluster any more; sign in again.").
			WithHeader("WWW-Authenticate", `Bearer realm="kingresd"`)
	}
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
	"Preference-Applied": true, "Content-Disposition": true,
}

// ServeForwarded runs a forwarded operation on this server, which must be the
// leader (the internal API only calls it there). peer is the verified server
// id of the forwarding server.
func (s *Server) ServeForwarded(ctx context.Context, peer string, fr internalapi.ForwardRequest) (internalapi.ForwardResponse, error) {
	op, found := operationByID(fr.OperationID)
	if !found || !(forwardsToLeader(op) || forwardedReads[op.ID]) {
		return internalapi.ForwardResponse{}, fmt.Errorf("operation %q is not forwarded", fr.OperationID)
	}
	if err := validateIdentity(fr.Identity); err != nil {
		return internalapi.ForwardResponse{}, err
	}
	req, err := s.operationRequest(ctx, peer, fr, op)
	if err != nil {
		return internalapi.ForwardResponse{}, err
	}
	// The role is made connectable only after the guard accepted the identity
	// (Proxy.Allow writes pg_ident.conf and reloads PostgreSQL), see checkSession.
	return s.serveRequest(req, maxForwardedResponse), nil
}

func operationByID(id string) (contract.Operation, bool) {
	for _, o := range contract.Operations() {
		if o.ID == id {
			return o, true
		}
	}
	return contract.Operation{}, false
}

// operationRequest builds the request that carries a forwarded or gathered
// operation through the pipeline: the path from the contract's template, the
// verified identity in the context.
func (s *Server) operationRequest(ctx context.Context, peer string, fr internalapi.ForwardRequest, op contract.Operation) (*http.Request, error) {
	path := contract.BasePath + op.Path
	if len(fr.PathParams) != len(op.PathParams) {
		return nil, errors.New("path parameters do not match the operation")
	}
	for _, p := range op.PathParams {
		v, ok := fr.PathParams[p]
		if !ok || v == "" || len(v) > 512 {
			return nil, fmt.Errorf("path parameter %q is missing or too long", p)
		}
		path = strings.Replace(path, "{"+p+"}", url.PathEscape(v), 1)
	}
	if len(fr.Query) > 4096 || strings.ContainsAny(fr.Query, "\r\n") {
		return nil, errors.New("the query string is not valid")
	}
	if _, qerr := url.ParseQuery(fr.Query); qerr != nil {
		return nil, errors.New("the query string is not valid")
	}
	// The URL is assembled as a structure, never by concatenation: the path
	// is already escaped per segment, the query is only a query.
	unescaped, uerr := url.PathUnescape(path)
	if uerr != nil {
		return nil, errors.New("the path is not valid")
	}
	target := (&url.URL{Scheme: "http", Host: "leader.internal", Path: unescaped, RawPath: path, RawQuery: fr.Query}).String()
	req, err := http.NewRequestWithContext(context.WithValue(ctx, fwdKey{}, &forwarded{
		peer: peer, identity: fr.Identity, requestID: fr.RequestID, sourceAddress: fr.SourceAddress, source: fr.Source, pin: pinText(fr.Pin),
	}), op.Method, target, bytes.NewReader(fr.Body))
	if err != nil {
		return nil, fmt.Errorf("building the request: %w", err)
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
	if fr.Prefer != "" && len(fr.Prefer) <= 200 {
		req.Header.Set("Prefer", fr.Prefer)
	}
	req.RemoteAddr = "forwarded:0"
	return req, nil
}

// serveRequest runs a request built by operationRequest through the whole
// pipeline and returns what it answered, at most max bytes of body.
func (s *Server) serveRequest(req *http.Request, max int) internalapi.ForwardResponse {
	rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK, max: max}
	s.handler.ServeHTTP(rec, req)
	out := internalapi.ForwardResponse{Status: rec.status, Body: rec.body.Bytes(), Header: map[string]string{}}
	for k := range relayedHeaders {
		if v := rec.header.Get(k); v != "" {
			out.Header[k] = v
		}
	}
	return out
}

// identityOf is the identity a request is forwarded or gathered with: the
// session's role and roles, and the key it proves itself with.
func identityOf(sess *auth.Session) internalapi.ForwardIdentity {
	return internalapi.ForwardIdentity{Username: sess.Username, ClusterRoles: sess.ClusterRoles, ProofKey: sess.ProofKey}
}

// forwardPin is the pinned fingerprint a forwarded request carries. The
// receiving server reads it from the header and from the body field
// expectedFingerprint (addServer), refuses a malformed one with 400, and
// forwards the one normalized value, so that the pin is not lost on the way
// to the leader and a malformed pin is not read as "no pin" there.
func (s *Server) forwardPin(op contract.Operation, r *http.Request, body []byte) (string, error) {
	var field *string
	if op.ID == "addServer" {
		var b struct {
			ExpectedFingerprint *string `json:"expectedFingerprint"`
		}
		// A body that does not parse is for the leader's handler to refuse.
		_ = json.Unmarshal(body, &b)
		field = b.ExpectedFingerprint
	}
	return pinOf(r, field)
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
	// max bounds the body (0: maxForwardedResponse).
	max int
}

func (b *bufferedResponse) Header() http.Header  { return b.header }
func (b *bufferedResponse) WriteHeader(code int) { b.status = code }
func (b *bufferedResponse) Write(p []byte) (int, error) {
	limit := b.max
	if limit == 0 {
		limit = maxForwardedResponse
	}
	if b.body.Len()+len(p) > limit {
		return 0, errors.New("the answer is too large to forward")
	}
	return b.body.Write(p)
}

// pinText normalizes the pinned fingerprint of a forwarded request ("" when
// it is not one: a server of the cluster already checked it).
func pinText(s string) string { return pki.NormalizeFingerprint(s) }
