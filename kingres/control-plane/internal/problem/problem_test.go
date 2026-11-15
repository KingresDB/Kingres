// SPDX-License-Identifier: PostgreSQL

package problem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNewUsesContractStatuses(t *testing.T) {
	tests := []struct {
		code   Code
		status int
	}{
		{BadRequest, 400}, {MethodNotAllowed, 405}, {ValidationFailed, 400}, {Unauthenticated, 401}, {InvalidCredentials, 401},
		{SessionExpired, 401}, {SetupTokenInvalid, 401}, {CSRFTokenInvalid, 403}, {Forbidden, 403},
		{NotFound, 404}, {AlreadyExists, 409}, {LastAdmin, 409}, {Unprocessable, 422}, {SQLError, 422},
		{UnsupportedOp, 422}, {NodeStateInvalid, 422}, {RateLimited, 429}, {NodeUninitialized, 503},
		{ServiceUnavailable, 503}, {WorkbenchBusy, 429}, {QuorumUnavailable, 503}, {InternalError, 500},
	}
	for _, tc := range tests {
		p := New(tc.code, "d")
		if p.Status != tc.status || p.Code != tc.code || p.Title == "" || p.Type == "" {
			t.Errorf("New(%s) = %+v, want status %d", tc.code, p, tc.status)
		}
	}
	if p := New(Code("made_up"), ""); p.Code != InternalError || p.Status != 500 {
		t.Errorf("unknown code must become internal_error, got %+v", p)
	}
}

func TestWriteProducesProblemJSON(t *testing.T) {
	w := httptest.NewRecorder()
	p := Validation("bad", FieldError{Field: "/a", Message: "m", Code: "required"}).WithRetryAfter(7).WithHeader("X-Extra", "1")
	Write(w, "/api/v1/x", "req-1", p)
	if w.Code != 400 || w.Header().Get("Content-Type") != MediaType || w.Header().Get("Retry-After") != "7" || w.Header().Get("X-Extra") != "1" {
		t.Fatalf("status %d headers %v", w.Code, w.Header())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{"code": "validation_failed", "status": float64(400), "instance": "/api/v1/x", "requestId": "req-1", "title": "Validation failed"} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v", k, m[k], want)
		}
	}
	if errs := m["errors"].([]any); len(errs) != 1 || errs[0].(map[string]any)["field"] != "/a" {
		t.Errorf("errors = %v", errs)
	}
	// Writing must not mutate the shared Problem (handlers may reuse it).
	if p.Instance != "" || p.RequestID != "" {
		t.Error("Write modified its argument")
	}
}

func TestSQLProblem(t *testing.T) {
	m := map[string]any{}
	w := httptest.NewRecorder()
	Write(w, "/q", "r", SQL(`relation "x" does not exist`, "42P01", 15))
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusUnprocessableEntity || m["sqlState"] != "42P01" || m["sqlPosition"] != float64(15) || m["code"] != "sql_error" {
		t.Errorf("%d %v", w.Code, m)
	}
}

func TestOptionalFieldsAreOmitted(t *testing.T) {
	w := httptest.NewRecorder()
	Write(w, "", "", New(NotFound, ""))
	var m map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &m)
	for _, k := range []string{"detail", "errors", "sqlState", "sqlPosition", "requestId", "instance"} {
		if _, ok := m[k]; ok {
			t.Errorf("%s should be omitted when empty", k)
		}
	}
}
