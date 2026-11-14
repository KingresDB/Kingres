// SPDX-License-Identifier: PostgreSQL
// Portions Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

func (s *Server) createInclusionProof(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.InclusionProofRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	proof, err := o.CreateInclusionProof(r.Context(), c.session.Username, db, req)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, proof)
	return nil
}

func (s *Server) listLedgerVerifications(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListVerifications(r.Context(), db, page)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getLedgerVerification(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	id := pp.pathInt64("verificationId")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.GetVerification(r.Context(), db, id)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
