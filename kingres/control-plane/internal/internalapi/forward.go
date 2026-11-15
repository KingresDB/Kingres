// SPDX-License-Identifier: PostgreSQL

package internalapi

import (
	"encoding/json"
	"io"
	"net/http"
)

// forward serves POST /forward. The caller is any server of the cluster; the
// receiver is the leader (checked by the table before this runs). The
// identity in the body is trusted because only a server holding a certificate
// of the cluster can get here: every server is fully trusted by every other
// (design D3), and the leader checks the operation, its role requirement and
// the shape of the identity again.
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
	s.log.Info("internal forward", "peer", peer, "operation", req.OperationID, "actor", req.Identity.Username)
	resp, err := s.be.Forward(r.Context(), peer, req)
	if err != nil {
		s.log.Warn("forwarded operation refused", "peer", peer, "operation", req.OperationID, "error", err)
		fail(w, http.StatusBadRequest, "bad_request", "The forwarded operation was refused.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
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
	resp, err := s.be.Gather(r.Context(), peer, req)
	if err != nil {
		s.log.Warn("gather call refused", "peer", peer, "operation", req.OperationID, "error", err)
		fail(w, http.StatusBadRequest, "bad_request", "The request was refused.")
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
