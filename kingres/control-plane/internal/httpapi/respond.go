// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// writeProblem sends p with the request path and id of r.
func writeProblem(w http.ResponseWriter, r *http.Request, p *problem.Problem) {
	problem.Write(w, r.URL.Path, stateOf(r).id, p)
}

// writeJSON sends v as application/json with the given status. The body is
// encoded first so that an encoding failure can still become a 500 problem.
func (s *Server) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		s.log.Error("encoding response", "request_id", stateOf(r).id, "error", err)
		writeProblem(w, r, problem.New(problem.InternalError, "The response could not be encoded."))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(append(body, '\n'))
}

// decodeJSON reads a request body into dst. Unknown fields, trailing data and
// bodies over the size limit are rejected with 400 problems.
func (s *Server) decodeJSON(w http.ResponseWriter, r *http.Request, dst any) *problem.Problem {
	return s.decodeJSONMode(w, r, dst, false)
}

// decodeJSONMode decodes a request body. closed makes a member that the request
// does not list a 400 validation_failed that names the member (the requests the
// contract declares closed); otherwise it is 400 bad_request.
func (s *Server) decodeJSONMode(w http.ResponseWriter, r *http.Request, dst any, closed bool) *problem.Problem {
	r.Body = http.MaxBytesReader(w, r.Body, s.deps.MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, io.EOF):
			return problem.New(problem.BadRequest, "The request body is empty.")
		case errors.As(err, &tooBig):
			return problem.New(problem.BadRequest, fmt.Sprintf("The request body exceeds %d bytes.", tooBig.Limit))
		case closed && strings.HasPrefix(err.Error(), "json: unknown field "):
			name, _ := strconv.Unquote(strings.TrimPrefix(err.Error(), "json: unknown field "))
			return problem.Validation("The request has a member that is not part of it.",
				problem.FieldError{Field: "/" + name, Message: "unknown member", Code: "unknown_property"})
		default:
			return problem.New(problem.BadRequest, "The request body is not valid JSON for this operation: "+safeJSONError(err))
		}
	}
	if dec.More() {
		return problem.New(problem.BadRequest, "The request body has data after the JSON document.")
	}
	return nil
}

// safeJSONError describes a decoding error without echoing request content.
func safeJSONError(err error) string {
	var ute *json.UnmarshalTypeError
	var se *json.SyntaxError
	switch {
	case errors.As(err, &ute):
		return "field " + strconv.Quote(ute.Field) + " has the wrong type"
	case errors.As(err, &se):
		return "syntax error at offset " + strconv.FormatInt(se.Offset, 10)
	default:
		// DisallowUnknownFields errors name the field, never a value.
		return err.Error()
	}
}

// storeProblem converts an error of the store into a problem. notFound is
// the detail for ErrNotFound.
func (s *Server) storeProblem(r *http.Request, err error, notFound string) *problem.Problem {
	var arg *store.ArgError
	switch {
	case errors.Is(err, store.ErrNotFound):
		return problem.New(problem.NotFound, notFound)
	case errors.Is(err, store.ErrInvalidCursor):
		return problem.Validation("The cursor is not valid.",
			problem.FieldError{Field: "cursor", Message: "not a cursor issued by this server", Code: "invalid_cursor"})
	case errors.As(err, &arg):
		return problem.Validation("A parameter is invalid.",
			problem.FieldError{Field: arg.Field, Message: arg.Message, Code: "invalid"})
	case errors.Is(err, store.ErrUnavailable) && s.deps.Ops != nil && s.deps.Ops.LocalInstanceDown():
		// A request that was already running when the instance was stopped or
		// restarted lost its connection: the documented answer, not a store failure.
		return s.stoppedProblem()
	case errors.Is(err, store.ErrUnavailable):
		s.log.Warn("store unavailable", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.ServiceUnavailable, "The local PostgreSQL instance is not available.").WithRetryAfter(5)
	default:
		s.log.Error("store error", "request_id", stateOf(r).id, "error", err)
		return problem.New(problem.InternalError, "An unexpected error occurred.")
	}
}
