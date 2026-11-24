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

// exportDatabase starts the task database.export (contract 0.29.0). The audit event names the database and the options as chosen; never a table name from
// the request, a value or SQL.
func (s *Server) exportDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	db := r.PathValue("databaseName")
	var req api.ExportDatabaseRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	t, err := o.StartDatabaseExport(r.Context(), c.session.Username, db, req)
	if err != nil {
		return s.taskProblem(r, err)
	}
	mode := req.DataMode
	if mode == "" {
		mode = "all"
	}
	c.addDetails(map[string]any{"database": db, "dataMode": mode, "excludedTables": len(req.ExcludeTables), "acknowledgeLargeObjectsLost": req.AcknowledgeLargeObjectsLost, "taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

// validateDatabaseArchive starts the task archive.validate (contract 0.29.0).
func (s *Server) validateDatabaseArchive(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	id := r.PathValue("uploadId")
	t, err := o.StartArchiveValidate(r.Context(), c.session.Username, id)
	if err != nil {
		return s.taskProblem(r, err)
	}
	c.addDetails(map[string]any{"uploadId": id, "taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}

// importDatabase starts the task database.import (contract 0.30.0). The audit event names the database, the archive, the options as chosen and the task; never a
// statement, a table name from the archive or a value.
func (s *Server) importDatabase(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.ImportDatabaseRequest
	if err := s.bodyClosed(w, r, &req); err != nil {
		return err
	}
	if req.UploadID == "" {
		return problem.Validation("The import request is invalid.", problem.FieldError{Field: "/uploadId", Message: "is required", Code: "required"})
	}
	t, err := o.StartDatabaseImport(r.Context(), c.session.Username, req)
	if err != nil {
		return s.taskProblem(r, err)
	}
	fk := req.OnForeignKeyViolation
	if fk == "" {
		fk = "fail"
	}
	c.addDetails(map[string]any{"database": t.SubjectID, "uploadId": req.UploadID, "expectedSha256": req.ExpectedSHA256, "acknowledgeLedgerRestart": req.AcknowledgeLedgerRestart,
		"onForeignKeyViolation": fk, "keepOnFailure": req.KeepOnFailure, "taskId": t.ID})
	s.accepted(w, r, t)
	return nil
}
