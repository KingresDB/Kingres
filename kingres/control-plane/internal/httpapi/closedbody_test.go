// SPDX-License-Identifier: PostgreSQL

package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// A member that a closed request does not list is 400 validation_failed with the
// member named (contract 0.5.0 says so for InclusionProofRequest, as 0.4.0 did);
// other request bodies keep 400 bad_request for an unknown member.
func TestClosedRequestsAnswerValidationFailedForAnUnknownMember(t *testing.T) {
	s := &Server{deps: Deps{MaxBodyBytes: 1 << 16}}
	post := func(body string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		return r
	}
	var req api.InclusionProofRequest
	err := s.bodyClosed(httptest.NewRecorder(), post(`{"schema":"public","table":"t","primaryKey":{"id":1},"extra":1}`), &req)
	p, ok := err.(*problem.Problem)
	if !ok || p.Code != problem.ValidationFailed || p.Status != 400 || len(p.Errors) != 1 || p.Errors[0].Field != "/extra" || p.Errors[0].Code != "unknown_property" {
		t.Fatalf("closed: %v", err)
	}
	// A closed request with a wrong type or bad JSON stays bad_request.
	for _, body := range []string{`{"schema":1}`, `{`, ``} {
		var r2 api.InclusionProofRequest
		err := s.bodyClosed(httptest.NewRecorder(), post(body), &r2)
		if p, ok := err.(*problem.Problem); !ok || p.Code != problem.BadRequest {
			t.Errorf("%q: %v", body, err)
		}
	}
	// An ordinary request.
	var req2 api.InclusionProofRequest
	err = s.body(httptest.NewRecorder(), post(`{"schema":"public","table":"t","primaryKey":{"id":1},"extra":1}`), &req2)
	if p, ok := err.(*problem.Problem); !ok || p.Code != problem.BadRequest {
		t.Errorf("open: %v", err)
	}
	// And a valid one.
	var ok2 api.InclusionProofRequest
	if err := s.bodyClosed(httptest.NewRecorder(), post(`{"schema":"public","table":"t","primaryKey":{"id":1}}`), &ok2); err != nil || ok2.Table != "t" {
		t.Errorf("valid: %v %+v", err, ok2)
	}
}
