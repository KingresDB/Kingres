// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func (s *Server) exportTable(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.ExportTableRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	t, err := o.StartExport(r.Context(), c.session.Username, db, schema, table, req)
	if err != nil {
		return s.taskProblem(r, err)
	}
	// What the audit log keeps of the request: the table, the format as chosen and the task; never a value.
	null := 0
	if req.NullString != nil {
		null = len(*req.NullString)
	}
	c.addDetails(map[string]any{"database": db, "schema": schema, "table": table, "delimiter": req.Delimiter, "nullStringLength": null, "encoding": req.Encoding,
		"compression": req.Compression, "spreadsheetSafe": req.SpreadsheetSafe == nil || *req.SpreadsheetSafe, "columns": len(req.Columns), "acknowledgeOtherRolesCode": req.AcknowledgeOtherRolesCode, "taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

func (s *Server) importTable(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db, schema, table, err := s.tablePath(r)
	if err != nil {
		return err
	}
	var req api.ImportTableRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	if req.UploadID == "" {
		return problem.Validation("The import request is invalid.", problem.FieldError{Field: "/uploadId", Message: "is required", Code: "required"})
	}
	t, err := o.StartImport(r.Context(), c.session.Username, db, schema, table, req)
	if err != nil {
		return s.taskProblem(r, err)
	}
	// The audit event names the table, the options as chosen (lengths of texts, not texts) and the upload; never a value.
	null := 0
	if req.NullString != nil {
		null = len(*req.NullString)
	}
	c.addDetails(map[string]any{"database": db, "schema": schema, "table": table, "uploadId": req.UploadID, "header": req.Header, "delimiter": req.Delimiter, "nullStringLength": null,
		"encoding": req.Encoding, "compression": req.Compression, "columns": len(req.Columns), "keepUpload": req.KeepUpload, "taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

// exportDatabase and validateDatabaseArchive (contract 0.40.0) answer 501 until the operations behind them are built.
func (s *Server) exportDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	return problem.New(problem.UnsupportedOp, "Database export is not available yet.")
}

func (s *Server) validateDatabaseArchive(c *call, w http.ResponseWriter, r *http.Request) error {
	return problem.New(problem.UnsupportedOp, "Archive validation is not available yet.")
}
