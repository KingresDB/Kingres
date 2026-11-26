// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// Phase 12 (contract 0.17.0): approximate aggregates, storage and time partitioning.

func (s *Server) analyticsHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"getApproximateAggregates":       s.getApproximateAggregates,
		"configureApproximateAggregates": s.configureApproximateAggregates,
		"setTableStorage":                s.setTableStorage,
		"setPartitionStorage":            s.setPartitionStorage,
		"getPartitionPolicy":             s.getPartitionPolicy,
		"putPartitionPolicy":             s.putPartitionPolicy,
		"deletePartitionPolicy":          s.deletePartitionPolicy,
		"runPartitionPolicy":             s.runPartitionPolicy,
		"listPartitionPolicyRuns":        s.listPartitionPolicyRuns,
		"getPartitionPolicyRun":          s.getPartitionPolicyRun,
		"planPartitionPolicy":            s.planPartitionPolicy,
	}
}

func (s *Server) getApproximateAggregates(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.GetApproximateAggregates(r.Context(), db)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) configureApproximateAggregates(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.ApproximateAggregatesRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareApproximateAggregates(r.Context(), c.session.Username, db, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindApproximateAggregates, "database", db, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) setTableStorage(c *call, w http.ResponseWriter, r *http.Request) error {
	return s.setStorage(c, w, r, false)
}

func (s *Server) setPartitionStorage(c *call, w http.ResponseWriter, r *http.Request) error {
	return s.setStorage(c, w, r, true)
}

// setStorage starts the task table.setStorage for a table or, with partition, for one partition (its subject is the partition).
func (s *Server) setStorage(c *call, w http.ResponseWriter, r *http.Request, partition bool) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db, schema, table := pp.pathName("databaseName"), pp.pathName("schemaName"), pp.pathName("tableName")
	part := ""
	if partition {
		part = pp.pathName("partitionName")
	}
	dry := pp.boolQuery("dryRun")
	if p := pp.err(); p != nil {
		return p
	}
	var req api.TableStorageRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	if dry != nil && *dry {
		out, err := o.PreviewSetStorage(r.Context(), c.session.Username, db, schema, table, part, req)
		if err != nil {
			return err
		}
		s.writeJSON(w, r, http.StatusOK, out)
		return nil
	}
	in, err := o.PrepareSetStorage(r.Context(), c.session.Username, db, schema, table, part, req)
	if err != nil {
		return err
	}
	subject := db + "/" + schema + "." + table
	if partition {
		subject = db + "/" + schema + "." + part
	}
	t, err := o.StartTask(r.Context(), ops.KindSetStorage, "table", subject, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) getPartitionPolicy(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	out, err := o.GetPartitionPolicy(r.Context(), db, schema, table)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) putPartitionPolicy(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.PartitionPolicyRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	out, err := o.PutPartitionPolicy(r.Context(), c.session.Username, db, schema, table, req)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) deletePartitionPolicy(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	if err := o.DeletePartitionPolicy(r.Context(), db, schema, table); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) runPartitionPolicy(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.RunPartitionPolicyRequest
	if r.ContentLength != 0 { // the body is optional
		if err := s.bodyClosed(w, r, &req); err != nil {
			return err
		}
	}
	in, err := o.PrepareMaintain(r.Context(), c.session.Username, db, schema, table, req.AsOf, "manual")
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindMaintain, "table", db+"/"+schema+"."+table, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) listPartitionPolicyRuns(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	pp := newParams(r)
	state := pp.enum("state", "running", "done", "partial", "failed")
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListPartitionPolicyRuns(r.Context(), db, schema, table, state, page.Limit, page.Cursor)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getPartitionPolicyRun(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	pp := newParams(r)
	id := pp.pathInt64("runId")
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.GetPartitionPolicyRun(r.Context(), db, schema, table, id)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) planPartitionPolicy(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.PlanPartitionPolicyRequest
	if r.ContentLength != 0 {
		if err := s.bodyClosed(w, r, &req); err != nil {
			return err
		}
	}
	out, err := o.PlanPartitionPolicy(r.Context(), db, schema, table, req)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}
