// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ops"
)

// The table lifecycle (contract 0.14.0, increment 9.6): preview, undistribute, alter and the partition operations.

func (s *Server) tableLifecycleHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"previewTableChange":     s.previewTableChange,
		"undistributeTable":      s.undistributeTable,
		"alterTableDistribution": s.alterTableDistribution,
		"listTablePartitions":    s.listTablePartitions,
		"createTablePartition":   s.createTablePartition,
		"attachTablePartition":   s.attachTablePartition,
		"detachTablePartition":   s.detachTablePartition,
		"dropTablePartition":     s.dropTablePartition,
	}
}

func (s *Server) tablePath(r *http.Request) (db, schema, table string, err error) {
	pp := newParams(r)
	db, schema, table = pp.pathName("databaseName"), pp.pathName("schemaName"), pp.pathName("tableName")
	if p := pp.err(); p != nil {
		return "", "", "", p
	}
	return db, schema, table, nil
}

func (s *Server) previewTableChange(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.TableChangeRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	out, err := o.PreviewTableChange(r.Context(), db, schema, table, req)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) undistributeTable(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	pp := newParams(r)
	opt := api.UndistributeOptions{
		Mode:           pp.enum("mode", "auto", "online", "blocking"),
		DependentViews: pp.enum("dependentViews", "refuse", "recreate"),
	}
	if b := pp.boolQuery("cascadeViaForeignKeys"); b != nil {
		opt.CascadeViaForeignKeys = *b
	}
	if g := pp.int32Query("targetGroupId"); g != nil {
		opt.TargetGroupID = int(*g)
	}
	if p := pp.err(); p != nil {
		return p
	}
	in, err := o.PrepareUndistribute(r.Context(), db, schema, table, opt)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindTableUndistribute, "table", db+"/"+schema+"."+table, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) alterTableDistribution(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.AlterTableDistributionRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	in, err := o.PrepareAlter(r.Context(), db, schema, table, req)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindTableAlter, "table", db+"/"+schema+"."+table, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) listTablePartitions(_ *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	pp := newParams(r)
	page := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListTablePartitions(r.Context(), db, schema, table, page.Limit, page.Cursor)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) startPartitionTask(c *call, w http.ResponseWriter, r *http.Request, op, name string, create *api.CreateTablePartitionRequest, attach *api.AttachTablePartitionRequest) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	in, err := o.PreparePartition(r.Context(), db, schema, table, op, name, create, attach)
	if err != nil {
		return err
	}
	t, err := o.StartTask(r.Context(), ops.KindTablePartition, "table", db+"/"+schema+"."+table, in, c.session.Username)
	if err != nil {
		return s.taskProblem(r, err)
	}
	s.accepted(w, r, t)
	return nil
}

func (s *Server) createTablePartition(c *call, w http.ResponseWriter, r *http.Request) error {
	var req api.CreateTablePartitionRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	return s.startPartitionTask(c, w, r, "create", req.Name, &req, nil)
}

func (s *Server) attachTablePartition(c *call, w http.ResponseWriter, r *http.Request) error {
	var req api.AttachTablePartitionRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	return s.startPartitionTask(c, w, r, "attach", req.Table.Table, nil, &req)
}

func (s *Server) detachTablePartition(c *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	name := pp.pathName("partitionName")
	if p := pp.err(); p != nil {
		return p
	}
	// the body is optional and has no members yet
	if r.ContentLength > 0 {
		var req api.DetachTablePartitionRequest
		if err := s.bodyClosed(w, r, &req); err != nil {
			return err
		}
	}
	return s.startPartitionTask(c, w, r, "detach", name, nil, nil)
}

func (s *Server) dropTablePartition(c *call, w http.ResponseWriter, r *http.Request) error {
	pp := newParams(r)
	name := pp.pathName("partitionName")
	if p := pp.err(); p != nil {
		return p
	}
	return s.startPartitionTask(c, w, r, "drop", name, nil, nil)
}
