// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/peerclient"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
)

// The data transfer operations (contract 0.19.0, design data-export-import.md): the guards that are theirs (TLS, the cluster setting) and
// the streaming relay of the two operations that carry a body of any size.

// tagDataTransfer is the tag of the data transfer operations.
const tagDataTransfer = "Data transfer"

// transferOp says whether the operation belongs to the data transfer.
func transferOp(op contract.Operation) bool { return op.Tag == tagDataTransfer }

// MaxChunkBytes is the largest body of an uploadDataChunk request (the chunk limit of the contract, 256 MiB).
const MaxChunkBytes = 256 << 20

// StreamForwarder is what the HTTP layer needs besides Forwarder to relay a streaming operation: the Forwarder of the peer client has it.
type StreamForwarder interface {
	ForwardStream(ctx context.Context, leader string, req internalapi.ForwardRequest, body io.Reader) (relay.Response, io.ReadCloser, error)
}

// transferTLS is the guard of the data transfer operations that every server runs for the request it received: a request that did not
// arrive over TLS is refused (403 insecure_transport), so that a bearer token over plain HTTP cannot carry data. A request that was
// forwarded by a server of the cluster was checked there, and arrives on the internal listener, which is TLS as well.
func (s *Server) transferTLS(c *call, r *http.Request) *problem.Problem {
	if !transferOp(c.op) || forwardedOf(r) != nil || r.TLS != nil {
		return nil
	}
	return problem.New(problem.InsecureTransport, "The data transfer needs a connection over TLS.")
}

// transferEnabled is the guard of the cluster setting, run by the server that runs the operation (the leader: the setting is in its system
// database, and a follower cannot read it): while dataTransfer.enabled is false every operation of the tag answers 403.
func (s *Server) transferEnabled(c *call, r *http.Request) *problem.Problem {
	if !transferOp(c.op) {
		return nil
	}
	cs, err := s.deps.Store.ClusterSettings(r.Context())
	if err != nil {
		s.log.Error("reading the data transfer setting", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.ServiceUnavailable, "The data transfer setting cannot be read right now.").WithRetryAfter(5)
	}
	if cs.DataTransfer == nil || !cs.DataTransfer.Enabled {
		return problem.New(problem.DataTransferDisabled, "An administrator must turn on dataTransfer.enabled in the cluster settings.")
	}
	return nil
}

// streamLimits is what serveOp does instead of the 30 s request timeout and the body limit of the other operations for a streaming one:
// the body is limited to one chunk, and an upload has a limit of 15 minutes per chunk. The idle deadlines are set by the handler and by the
// relay as the bytes move.
func (s *Server) streamLimits(op contract.Operation, w http.ResponseWriter, r *http.Request) (*http.Request, context.CancelFunc) {
	cancel := context.CancelFunc(func() {})
	if op.Streams == "upload" {
		var ctx context.Context
		ctx, cancel = context.WithTimeout(r.Context(), relay.ChunkTimeout)
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, MaxChunkBytes)
	}
	return r, cancel
}

// streamHeaders are the request headers a streaming call carries to the leader.
func streamHeaders(r *http.Request) map[string]string {
	h := relay.Pick(r.Header, relay.RequestHeaders)
	if r.ContentLength >= 0 && r.Method != http.MethodGet {
		h["Content-Length"] = strconv.FormatInt(r.ContentLength, 10)
	}
	return h
}

// tryForwardStream relays a streaming operation to the leader when this server is not it. It returns true when it answered the request.
// The body is not read here: it is handed to the relay as a reader that moves the deadlines of the connection, and the answer is copied back
// as it comes. A stream that fails after the status went out cannot change the status: the connection is cut, and the Content-Length
// of a download makes the short file visible to the client.
func (s *Server) tryForwardStream(c *call, w http.ResponseWriter, r *http.Request) (bool, error) {
	sf, ok := s.deps.Forwarder.(StreamForwarder)
	if !ok {
		return false, nil
	}
	leaderID, self, err := s.deps.Forwarder.Leader(r.Context())
	if err != nil || leaderID == "" {
		if err != nil {
			s.log.Warn("manager leader unknown", "request_id", stateOf(r).id, "error", err)
		}
		return true, problem.New(problem.ServiceUnavailable, "The manager leader is not known; the data transfer cannot run now.").WithRetryAfter(5)
	}
	if self {
		return false, nil
	}
	rc := http.NewResponseController(w)
	var body io.Reader
	if r.Method != http.MethodGet && r.Body != nil && r.Body != http.NoBody {
		body = relay.NewIdleBody(r.Body, rc, relay.IdleTimeout)
	}
	params := map[string]string{}
	for _, p := range c.op.PathParams {
		params[p] = r.PathValue(p)
	}
	src := "cli"
	if c.session.Kind == auth.KindCookie {
		src = "panel"
	}
	resp, rest, err := sf.ForwardStream(r.Context(), leaderID, internalapi.ForwardRequest{
		OperationID: c.op.ID, PathParams: params, Query: r.URL.RawQuery, Header: streamHeaders(r),
		Identity: identityOf(c.session), RequestID: stateOf(r).id, SourceAddress: c.ip, Source: src,
	}, body)
	var refused *peerclient.APIError
	switch {
	case errors.As(err, &refused) && refused.Code == "identity_unproven":
		return true, problem.New(problem.Unauthenticated, "This session cannot be used for operations of the cluster any more; sign in again.").
			WithHeader("WWW-Authenticate", `Bearer realm="kingresd"`)
	case err != nil:
		s.log.Warn("relaying to the manager leader failed", "request_id", stateOf(r).id, "leader", leaderID, "error", err)
		c.details = map[string]any{"forwardedTo": leaderID}
		return true, problem.New(problem.ServiceUnavailable, "The manager leader cannot be reached; the data transfer cannot run now.").WithRetryAfter(5)
	}
	defer func() { _ = rest.Close() }()
	// The leader audited the operation (a download when its stream ended); this server must not do it again.
	c.audit = false
	h := w.Header()
	for k, v := range resp.Header {
		if relay.Allowed(k, relay.ResponseHeaders) {
			h.Set(k, v)
		}
	}
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(resp.Status)
	if resp.Status == http.StatusNoContent || resp.Status == http.StatusNotModified || r.Method == http.MethodHead {
		return true, nil
	}
	if _, cerr := relay.Copy(relay.NewIdleWriter(w, rc, relay.IdleTimeout), rest); cerr != nil {
		s.log.Warn("the relayed answer broke off", "request_id", stateOf(r).id, "operation", c.op.ID)
		panic(http.ErrAbortHandler)
	}
	return true, nil
}

// ServeForwardedStream runs a relayed streaming operation on this server, which must be the leader (the internal API only calls it there).
// body is the raw body that followed the frame; the answer goes to w, whose first WriteHeader sends the frame of the status and the headers.
func (s *Server) ServeForwardedStream(ctx context.Context, peer string, fr internalapi.ForwardRequest, body io.Reader, w http.ResponseWriter) error {
	op, found := operationByID(fr.OperationID)
	if !found || op.Streams == "" {
		return errors.New("operation is not a streaming operation")
	}
	if err := validateIdentity(fr.Identity); err != nil {
		return err
	}
	req, err := s.operationRequest(ctx, peer, fr, op)
	if err != nil {
		return err
	}
	if op.Method != http.MethodGet {
		req.Body = io.NopCloser(body)
		req.ContentLength = -1
		if n, perr := strconv.ParseInt(fr.Header["Content-Length"], 10, 64); perr == nil && n >= 0 {
			req.ContentLength = n
		}
	}
	for k, v := range fr.Header {
		if !relay.Allowed(k, relay.RequestHeaders) || strings.EqualFold(k, "Content-Length") {
			continue
		}
		if strings.ContainsAny(v, "\r\n\x00") {
			return errors.New("a header is not valid")
		}
		req.Header.Set(k, v)
	}
	s.handler.ServeHTTP(w, req)
	return nil
}
