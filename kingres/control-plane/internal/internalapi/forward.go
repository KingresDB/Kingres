// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package internalapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fwdproof"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
)

// verifyIdentity checks the proof of a forwarded or gathered request and
// writes the refusal itself; it reports whether the call may go on.
func (s *Server) verifyIdentity(w http.ResponseWriter, r *http.Request, kind fwdproof.Kind, peer string, req ForwardRequest) bool {
	if s.be.VerifyIdentity == nil {
		return true
	}
	err := s.be.VerifyIdentity(r.Context(), kind, peer, req)
	switch {
	case err == nil:
		return true
	case errors.Is(err, fwdproof.ErrUnavailable):
		s.log.Error("identity proof could not be checked", "peer", peer, "operation", req.OperationID, "error", err)
		fail(w, http.StatusServiceUnavailable, "unavailable", "The identity cannot be checked now.")
	default:
		s.log.Warn("identity proof refused", "peer", peer, "operation", req.OperationID, "actor", req.Identity.Username, "error", err)
		fail(w, http.StatusUnauthorized, "identity_unproven", "The identity of the request is not proven.")
	}
	return false
}

// forward serves POST /forward. The caller is a member of the cluster; the
// receiver is the leader (both checked by the table before this runs). A
// certificate is not enough to act as a user: the request must carry the
// proof of a session of that user (package fwdproof), which the leader checks
// against the user's verifier before anything else, and it then checks the
// operation, the role requirement and the shape of the identity again.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, peer string) {
	var req ForwardRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		if err == io.EOF {
			fail(w, http.StatusBadRequest, "bad_request", "The body is empty.")
			return
		}
		fail(w, http.StatusBadRequest, "bad_request", "The body is not a valid forward request.")
		return
	}
	if req.OperationID == "" || req.Identity.Username == "" {
		fail(w, http.StatusBadRequest, "bad_request", "operationId and identity are required.")
		return
	}
	if !s.verifyIdentity(w, r, fwdproof.KindForward, peer, req) {
		return
	}
	s.log.Info("internal forward", "peer", peer, "operation", req.OperationID, "actor", req.Identity.Username)
	resp, err := s.be.Forward(r.Context(), peer, req)
	if err != nil {
		s.log.Warn("forwarded operation refused", "peer", peer, "operation", req.OperationID, "error", err)
		fail(w, http.StatusBadRequest, "bad_request", "The forwarded operation was refused.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// gatherAuthorize serves POST /gather-authorize on the leader: the asking server
// sends the proof of the user's session (the leader learns the ClientKey, which
// it may: it holds every verifier) and gets the leader's assertion for the
// gather request in return.
func (s *Server) gatherAuthorize(w http.ResponseWriter, r *http.Request, peer string) {
	var req ForwardRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		fail(w, http.StatusBadRequest, "bad_request", "The body is not a valid gather authorization request.")
		return
	}
	if req.OperationID == "" || req.Identity.Username == "" || req.Assertion != nil {
		fail(w, http.StatusBadRequest, "bad_request", "operationId and identity are required.")
		return
	}
	if s.be.Authorize == nil {
		fail(w, http.StatusNotImplemented, "not_available", "This server cannot authorize gather calls.")
		return
	}
	if !s.verifyIdentity(w, r, fwdproof.KindGatherAuthorize, peer, req) {
		return
	}
	a, err := s.be.Authorize(r.Context(), peer, req)
	if err != nil {
		s.log.Error("signing a gather assertion", "peer", peer, "error", err)
		fail(w, http.StatusInternalServerError, "failed", "The call failed.")
		return
	}
	writeJSON(w, http.StatusOK, a)
}

// gather serves POST /gather: a server of the cluster asks for this server's
// part of a cluster-wide read (the logs, alerts or metrics) on behalf of an
// identity it verified. The answering side runs the operation through the same
// pipeline as a public request and checks the identity against its own catalog
// again (see package httpapi, ServeGathered); here only the shape is checked.
func (s *Server) gather(w http.ResponseWriter, r *http.Request, peer string) {
	var req ForwardRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil || dec.More() {
		if err == io.EOF {
			fail(w, http.StatusBadRequest, "bad_request", "The body is empty.")
			return
		}
		fail(w, http.StatusBadRequest, "bad_request", "The body is not a valid gather request.")
		return
	}
	if req.OperationID == "" || req.Identity.Username == "" {
		fail(w, http.StatusBadRequest, "bad_request", "operationId and identity are required.")
		return
	}
	if s.be.Gather == nil {
		fail(w, http.StatusNotImplemented, "not_available", "This server cannot answer gather calls.")
		return
	}
	if req.Proof != nil {
		// A proof reveals the ClientKey of the user to whoever checks it; a member that
		// answers a gather call must never be sent one.
		fail(w, http.StatusBadRequest, "bad_request", "A gather request carries an assertion of the leader, never a proof.")
		return
	}
	if s.be.VerifyAssertion != nil {
		err := s.be.VerifyAssertion(r.Context(), peer, req)
		switch {
		case err == nil:
		case errors.Is(err, fwdproof.ErrUnavailable):
			s.log.Error("gather assertion could not be checked", "peer", peer, "error", err)
			fail(w, http.StatusServiceUnavailable, "unavailable", "The identity cannot be checked now.")
			return
		default:
			s.log.Warn("gather assertion refused", "peer", peer, "operation", req.OperationID, "actor", req.Identity.Username, "error", err)
			fail(w, http.StatusUnauthorized, "identity_unproven", "The identity of the request is not proven.")
			return
		}
	}
	resp, err := s.be.Gather(r.Context(), peer, req)
	if err != nil {
		s.log.Warn("gather call refused", "peer", peer, "operation", req.OperationID, "error", err)
		fail(w, http.StatusBadRequest, "bad_request", "The request was refused.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// forwardStream serves POST /forward-stream (contract 0.19.0, the streaming relay of the data transfer). The body is a frame (4-byte length,
// at most 64 KiB, then JSON: the operation, its path parameters and query, the request headers the operation uses, the source address, the
// identity and its proof) and then the raw bytes of an upload chunk, and the answer is a frame (status and the headers of an allowlist)
// and then the raw bytes of the operation's answer. The proof is checked exactly as for /forward before anything runs, and the proof does
// not cover the raw body: it is bytes of any size on the mutual TLS connection between two servers of the cluster. Nothing is buffered
// beyond one segment.
func (s *Server) forwardStream(w http.ResponseWriter, r *http.Request, peer string) {
	var req ForwardRequest
	if err := relay.ReadFrame(r.Body, &req); err != nil {
		switch {
		case errors.Is(err, relay.ErrFrameTooLarge):
			fail(w, http.StatusRequestEntityTooLarge, "frame_too_large", "The frame is larger than 64 KiB.")
		default:
			fail(w, http.StatusBadRequest, "bad_request", "The body does not start with a valid frame.")
		}
		return
	}
	if req.OperationID == "" || req.Identity.Username == "" || len(req.Body) != 0 || req.Assertion != nil {
		fail(w, http.StatusBadRequest, "bad_request", "operationId and identity are required, and the frame holds no body.")
		return
	}
	for k := range req.Header {
		if !relay.Allowed(k, relay.RequestHeaders) {
			fail(w, http.StatusBadRequest, "bad_request", "The frame carries a header that is not one of the streaming operations'.")
			return
		}
	}
	if s.be.ForwardStream == nil {
		fail(w, http.StatusNotImplemented, "not_available", "This server cannot run streaming operations.")
		return
	}
	if !s.verifyIdentity(w, r, fwdproof.KindForwardStream, peer, req) {
		return
	}
	s.log.Info("internal forward-stream", "peer", peer, "operation", req.OperationID, "actor", req.Identity.Username)
	out := relay.NewResponseWriter(w)
	if err := s.be.ForwardStream(r.Context(), peer, req, r.Body, out); err != nil {
		s.log.Warn("streamed operation refused", "peer", peer, "operation", req.OperationID, "error", err)
		if out.Sent() {
			// The status is out and a part of the body may be too: the only honest end is a cut that the other side sees as a short body.
			panic(http.ErrAbortHandler)
		}
		fail(w, http.StatusBadRequest, "bad_request", "The streamed operation was refused.")
		return
	}
	out.Finish()
}
