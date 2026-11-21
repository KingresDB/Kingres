// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package httpapi

import (
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// Distributed schemas (contract 0.16.0, design shard-phase11.md, section 13).

func (s *Server) schemaHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"listSchemas":        s.listSchemas,
		"getSchema":          s.getSchema,
		"distributeSchema":   s.distributeSchema,
		"undistributeSchema": s.undistributeSchema,
		"moveSchema":         s.moveSchema,
	}
}

func (s *Server) schemaPath(r *http.Request) (db, schema string, err error) {
	pp := newParams(r)
	db, schema = pp.pathName("databaseName"), pp.pathName("schemaName")
	if p := pp.err(); p != nil {
		return db, schema, p
	}
	return db, schema, nil
}

func (s *Server) listSchemas(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	db := pp.pathName("databaseName")
	f := ops.SchemaFilter{Distributed: pp.boolQuery("distributed"), ShardGroupID: pp.int32Query("shardGroupId"), State: pp.enum("state", "active", "moving")}
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListSchemas(r.Context(), db, f, page)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getSchema(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, err := s.schemaPath(r)
	if err != nil {
		return err
	}
	out, err := o.GetSchema(r.Context(), db, schema)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) distributeSchema(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, err := s.schemaPath(r)
	if err != nil {
		return err
	}
	var req api.DistributeSchemaRequest
	if err := s.optionalBody(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareDistributeSchema(r.Context(), db, schema, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindSchemaDistribute, ops.SubjectSchema, ops.SchemaSubjectID(db, schema), in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) undistributeSchema(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, err := s.schemaPath(r)
	if err != nil {
		return err
	}
	pp := newParams(r)
	target := pp.int32Query("shardGroupId")
	if p := pp.err(); p != nil {
		return p
	}
	in, err := o.PrepareUndistributeSchema(r.Context(), db, schema, target)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindSchemaUndistribute, ops.SubjectSchema, ops.SchemaSubjectID(db, schema), in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) moveSchema(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, err := s.schemaPath(r)
	if err != nil {
		return err
	}
	var req api.MoveSchemaRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareMoveSchema(r.Context(), db, schema, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindSchemaMove, ops.SubjectSchema, ops.SchemaSubjectID(db, schema), in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	c.addDetails(map[string]any{"taskId": t.ID, "targetShardGroupId": req.TargetShardGroupID})
	s.accepted(w, r, t)
	return nil
}
