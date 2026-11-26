// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"errors"
	"io"
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Client keys, the signature policy of a table and the upgrade of the extension (phase 7, step 17).

func (s *Server) ledgerClientHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"listLedgerClientKeys":    s.listLedgerClientKeys,
		"registerLedgerClientKey": s.registerLedgerClientKey,
		"revokeLedgerClientKey":   s.revokeLedgerClientKey,
		"setTableSignaturePolicy": s.setTableSignaturePolicy,
		"upgradeLedgerExtension":  s.upgradeLedgerExtension,
	}
}

func (s *Server) listLedgerClientKeys(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListClientKeys(r.Context(), db)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) registerLedgerClientKey(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.RegisterLedgerClientKeyRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	k, err := o.RegisterClientKey(r.Context(), db, c.session.Username, req)
	if err != nil {
		return err
	}
	c.addDetails(map[string]any{"keyId": k.KeyID})
	w.Header().Set("Location", "/api/v1/databases/"+db+"/ledger/client-keys")
	s.writeJSON(w, r, http.StatusCreated, k)
	return nil
}

func (s *Server) revokeLedgerClientKey(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	short := r.PathValue("keyId")
	var req api.RevokeLedgerClientKeyRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	c.addDetails(map[string]any{"keyId": short})
	k, err := o.RevokeClientKey(r.Context(), db, c.session.Username, short, req)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, k)
	return nil
}

func (s *Server) setTableSignaturePolicy(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db, schema, table := pp.pathName("databaseName"), pp.pathName("schemaName"), pp.pathName("tableName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.SignaturePolicyRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	t, err := s.deps.Store.Table(r.Context(), db, schema, table)
	if err != nil {
		return s.storeProblem(r, err, "Table does not exist.")
	}
	if t.Ledger == nil {
		return problem.New(problem.Unprocessable, "Table "+schema+"."+table+" is not a ledger table.")
	}
	c.addDetails(map[string]any{"table": schema + "." + table})
	if err := o.SetSignaturePolicy(r.Context(), db, schema, table, req); err != nil {
		return err
	}
	t, err = s.deps.Store.Table(r.Context(), db, schema, table)
	if err != nil {
		return s.storeProblem(r, err, "Table does not exist.")
	}
	s.writeJSON(w, r, http.StatusOK, t.Table)
	return nil
}

func (s *Server) upgradeLedgerExtension(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	if r.ContentLength != 0 {
		var req struct{}
		if err := s.bodyClosed(w, r, &req); err != nil {
			return err
		}
	}
	in, err := o.PrepareUpgrade(r.Context(), db)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindLedgerUpgrade, "database", db, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

// Bundles (phase 7, step 20).

func (s *Server) ledgerBundleHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"createLedgerBundle":          s.createLedgerBundle,
		"getLedgerBundle":             s.getLedgerBundle,
		"acknowledgeLedgerDivergence": s.acknowledgeLedgerDivergence,
	}
}

func (s *Server) createLedgerBundle(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.LedgerBundleRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareBundle(r.Context(), db, c.session.Username, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindLedgerBundle, "database", db, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) getLedgerBundle(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	id := pp.pathInt64("bundleId")
	if p := pp.err(); p != nil {
		return p
	}
	f, size, err := o.OpenBundle(r.Context(), db, id)
	if errors.Is(err, ops.ErrBundleGone) {
		p := problem.New(problem.NotFound, "The bundle has expired: bundles are kept for 24 hours.")
		p.Status = http.StatusGone
		return p
	}
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="ledger-bundle-`+itoa64(id)+`.tar.gz"`)
	w.Header().Set("Content-Length", itoa64(size))
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, f)
	return nil
}

func (s *Server) acknowledgeLedgerDivergence(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	group := pp.pathInt64("shardGroupId")
	if p := pp.err(); p != nil {
		return p
	}
	if group < 0 || group > 1<<31-1 {
		return problem.New(problem.NotFound, "No such shard group.")
	}
	var req api.AcknowledgeLedgerDivergenceRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	c.addDetails(map[string]any{"shardGroupId": group, "kind": req.Kind})
	in, err := o.PrepareEpoch(r.Context(), db, int32(group), c.session.Username, req) //nolint:gosec // bounded above
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindLedgerEpoch, "database", db, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}
