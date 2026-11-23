// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/relay"
)

// The handlers of the data transfer (contract 0.19.0). They run on the manager leader: the guards (TLS, the setting) ran before, and a request
// that reached a follower was relayed. Nothing of the data and nothing a client named is put into an audit detail.

func (s *Server) transferHandlers() map[string]handlerFunc {
	return map[string]handlerFunc{
		"createDataUpload":        s.createDataUpload,
		"getDataUpload":           s.getDataUpload,
		"uploadDataChunk":         s.uploadDataChunk,
		"deleteDataUpload":        s.deleteDataUpload,
		"listDataExports":         s.listDataExports,
		"getDataExport":           s.getDataExport,
		"downloadDataExport":      s.downloadDataExport,
		"deleteDataExport":        s.deleteDataExport,
		"exportTable":             s.exportTable,
		"importTable":             s.importTable,
		"exportDatabase":          s.exportDatabase,
		"validateDatabaseArchive": s.validateDatabaseArchive,
	}
}

func (s *Server) createDataUpload(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	var req api.CreateDataUploadRequest
	if err := s.body(w, r, &req); err != nil {
		return err
	}
	up, id, err := o.CreateUpload(r.Context(), c.session.Username, req)
	if err != nil {
		return err
	}
	// The file name is for display and is never kept or audited.
	c.addDetails(map[string]any{"transferId": idText(id), "declaredBytes": req.SizeBytes, "purpose": req.Purpose})
	w.Header().Set("Location", contractBase+"/data-uploads/"+up.UploadID)
	s.writeJSON(w, r, http.StatusCreated, up)
	return nil
}

const contractBase = "/api/v1"

func idText(id int64) string { return strconv.FormatInt(id, 10) }

func (s *Server) getDataUpload(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	up, err := o.GetUpload(r.Context(), c.session.Username, r.PathValue("uploadId"))
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, up)
	return nil
}

func (s *Server) uploadDataChunk(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	hdr := r.Header.Get("Upload-Offset")
	offset, perr := strconv.ParseInt(hdr, 10, 64)
	if hdr == "" || perr != nil || offset < 0 {
		return problem.Validation("The header Upload-Offset is required.", problem.FieldError{Field: "Upload-Offset", Message: "must be a non-negative integer", Code: "pattern"})
	}
	// The body moves the deadlines of the connection with every read, so that a chunk that takes minutes is not ended by the 30 s read
	// timeout and its answer is not refused by the write timeout (spike S1).
	body := relay.NewIdleBody(r.Body, http.NewResponseController(w), relay.IdleTimeout)
	res, err := o.UploadChunk(r.Context(), c.session.Username, r.PathValue("uploadId"), offset, r.ContentLength, body)
	if err != nil {
		return err
	}
	w.Header().Set("Upload-Offset", strconv.FormatInt(res.Offset, 10))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) deleteDataUpload(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	id, err := o.DeleteUpload(r.Context(), c.session.Username, r.PathValue("uploadId"))
	if err != nil {
		return err
	}
	c.addDetails(map[string]any{"transferId": idText(id)})
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (s *Server) listDataExports(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	pp := newParams(r)
	pg := pp.page()
	if p := pp.err(); p != nil {
		return p
	}
	out, err := o.ListExports(r.Context(), c.session.Username, pg.Limit, pg.Cursor)
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) getDataExport(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	out, err := o.GetExport(r.Context(), c.session.Username, r.PathValue("exportId"))
	if err != nil {
		return err
	}
	s.writeJSON(w, r, http.StatusOK, out)
	return nil
}

func (s *Server) deleteDataExport(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	creator, err := o.DeleteExport(r.Context(), c.session.Username, r.PathValue("exportId"))
	if err != nil {
		return err
	}
	d := map[string]any{"transferId": r.PathValue("exportId")}
	if creator != "" {
		d["creator"] = creator
	}
	c.addDetails(d)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

var rangeRE = regexp.MustCompile(`^bytes=([0-9]*)-([0-9]*)$`)

// byteRange parses a Range header against a file of size bytes: one range of the forms a-b, a- and -n. ok is false when there is no usable
// range (the whole file is served); unsatisfiable is true when the range cannot be served (416).
func byteRange(h string, size int64) (first, last int64, ok, unsatisfiable bool) {
	if h == "" {
		return 0, 0, false, false
	}
	m := rangeRE.FindStringSubmatch(h)
	if m == nil {
		// More than one range, another unit, or text that is not a range: the whole file, as the standard says.
		return 0, 0, false, false
	}
	a, b := m[1], m[2]
	switch {
	case a == "" && b == "":
		return 0, 0, false, true
	case a == "":
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n == 0 {
			return 0, 0, false, true
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, size == 0
	}
	f, err := strconv.ParseInt(a, 10, 64)
	if err != nil || f >= size {
		return 0, 0, false, true
	}
	l := size - 1
	if b != "" {
		e, err := strconv.ParseInt(b, 10, 64)
		if err != nil || e < f {
			return 0, 0, false, true
		}
		if e < l {
			l = e
		}
	}
	return f, l, true, false
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// downloadName is the file name of a download: built by the server from the database, the table and the UTC time of the export, from the
// characters [A-Za-z0-9._-], at most 80. A client string is never part of it.
func downloadName(db, table string, created time.Time, format string) string {
	base := unsafeName.ReplaceAllString(db+"_"+table, "_")
	if base == "_" || base == "" {
		base = "export"
	}
	ext := ".csv"
	if format == "csv.gz" {
		ext = ".csv.gz"
	}
	stamp := created.UTC().Format("20060102T150405Z")
	name := base
	if max := 80 - len(stamp) - len(ext) - 1; len(name) > max {
		name = name[:max]
	}
	return name + "_" + stamp + ext
}

func (s *Server) downloadDataExport(c *call, w http.ResponseWriter, r *http.Request) error {
	o, err := s.needOps()
	if err != nil {
		return err
	}
	dl, err := o.OpenDownload(r.Context(), c.session.Username, r.PathValue("exportId"))
	if err != nil {
		return err
	}
	defer func() { _ = dl.Reader.Close() }()
	size := dl.Reader.Size()
	first, last, ranged, bad := byteRange(r.Header.Get("Range"), size)
	h := w.Header()
	if bad {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		p := problem.New(problem.BadRequest, "The range cannot be served; the file has "+strconv.FormatInt(size, 10)+" bytes.")
		p.Status, p.Title = http.StatusRequestedRangeNotSatisfiable, "Range not satisfiable"
		return p
	}
	if !ranged {
		first, last = 0, size-1
	}
	length := last - first + 1
	if size == 0 {
		length = 0
	}
	ct := "text/csv; charset=" + strings.ToLower(strings.NewReplacer("_", "-").Replace(dl.Encoding))
	if dl.Encoding == "" {
		ct = "text/csv"
	}
	if dl.Rec.Format == "csv.gz" {
		ct = "application/gzip"
	}
	h.Set("Content-Type", ct)
	h.Set("Content-Length", strconv.FormatInt(length, 10))
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", `"`+dl.Rec.SHA256+`"`)
	h.Set("Content-Disposition", `attachment; filename="`+downloadName(dl.Rec.Database, dl.Rec.Table, dl.Rec.CreatedAt, dl.Rec.Format)+`"`)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	status := http.StatusOK
	if ranged {
		status = http.StatusPartialContent
		h.Set("Content-Range", "bytes "+strconv.FormatInt(first, 10)+"-"+strconv.FormatInt(last, 10)+"/"+strconv.FormatInt(size, 10))
	}
	w.WriteHeader(status)
	var served int64
	if length > 0 {
		iw := relay.NewIdleWriter(w, http.NewResponseController(w), relay.IdleTimeout)
		served, err = relay.Copy(iw, io.NewSectionReader(dl.Reader, first, length))
	}
	complete := err == nil && served == length
	lastByte := complete && last == size-1
	// The audit event of a download says what was sent, when the stream ended: the size, the checksum, whether it was a range and whether the last
	// byte was served. It never has a value of the data.
	c.addDetails(map[string]any{"transferId": idText(dl.Rec.ID), "sizeBytes": size, "sha256": dl.Rec.SHA256, "range": ranged, "bytesServed": served, "lastByteServed": lastByte})
	if lastByte {
		o.FinishDownload(r.Context(), dl.Rec.ID)
	}
	if err != nil {
		// The status is out: a stream that failed half way is cut, and the Content-Length tells the client the file is short.
		panic(http.ErrAbortHandler)
	}
	return nil
}
