// SPDX-License-Identifier: AGPL-3.0-only

package httpapi

import (
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

const (
	defaultLimit = 50
	maxLimit     = 500
)

// params reads and validates the query and path parameters of a request and
// collects every problem, so the client gets all of them at once.
type params struct {
	q    url.Values
	r    *http.Request
	errs []problem.FieldError
}

func newParams(r *http.Request) *params { return &params{q: r.URL.Query(), r: r} }

func (p *params) fail(field, msg, code string) {
	p.errs = append(p.errs, problem.FieldError{Field: field, Message: msg, Code: code})
}

// err returns the validation problem, or nil when everything was valid.
func (p *params) err() *problem.Problem {
	if len(p.errs) == 0 {
		return nil
	}
	return problem.Validation("One or more parameters are invalid.", p.errs...)
}

// page reads limit and cursor.
func (p *params) page() store.Page {
	pg := store.Page{Limit: defaultLimit}
	if v := p.q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			p.fail("limit", "must be an integer between 1 and 500", "out_of_range")
		} else {
			pg.Limit = n
		}
	}
	if v := p.q.Get("cursor"); v != "" {
		if len(v) > 1024 {
			p.fail("cursor", "must be at most 1024 characters", "out_of_range")
		} else {
			pg.Cursor = v
		}
	}
	return pg
}

// enum reads a query parameter that must be one of allowed ("" when absent).
func (p *params) enum(name string, allowed ...string) string {
	v := p.q.Get(name)
	if v == "" {
		return ""
	}
	for _, a := range allowed {
		if v == a {
			return v
		}
	}
	p.fail(name, "must be one of: "+join(allowed), "unknown_value")
	return ""
}

func join(s []string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += ", "
		}
		out += v
	}
	return out
}

// int32Query reads an optional int32 query parameter.
func (p *params) int32Query(name string) *int32 {
	v := p.q.Get(name)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.fail(name, "must be a 32-bit integer", "type")
		return nil
	}
	id := int32(n)
	return &id
}

// boolQuery reads an optional boolean query parameter.
func (p *params) boolQuery(name string) *bool {
	v := p.q.Get(name)
	if v == "" {
		return nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil || (v != "true" && v != "false") {
		p.fail(name, "must be true or false", "type")
		return nil
	}
	return &b
}

// timeQuery reads an optional RFC 3339 timestamp.
func (p *params) timeQuery(name string) time.Time {
	v := p.q.Get(name)
	if v == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		p.fail(name, "must be an RFC 3339 timestamp", "format")
		return time.Time{}
	}
	return t
}

// maxLen reads an optional string parameter of at most n characters.
func (p *params) maxLen(name string, n int) string {
	v := p.q.Get(name)
	if len(v) > n {
		p.fail(name, "must be at most "+strconv.Itoa(n)+" characters", "out_of_range")
		return ""
	}
	return v
}

// pathInt32 reads an int32 path parameter.
func (p *params) pathInt32(name string) int32 {
	v := p.r.PathValue(name)
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil {
		p.fail(name, "must be a 32-bit integer", "type")
		return 0
	}
	return int32(n)
}

// pathInt64 reads an int64 path parameter.
func (p *params) pathInt64(name string) int64 {
	v := p.r.PathValue(name)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		p.fail(name, "must be an integer", "type")
		return 0
	}
	return n
}

// pathName reads a name path parameter of 1 to 63 characters.
func (p *params) pathName(name string) string {
	v := p.r.PathValue(name)
	if v == "" || len(v) > 63 {
		p.fail(name, "must be between 1 and 63 characters", "out_of_range")
	}
	return v
}

// pathServerID reads the ServerId path parameter (1 to 255 characters; the
// router has already percent-decoded it).
func (p *params) pathServerID(name string) string {
	v := p.r.PathValue(name)
	if v == "" || len(v) > 255 {
		p.fail(name, "must be between 1 and 255 characters", "out_of_range")
	}
	return v
}

// serverIDQuery reads an optional ServerId query parameter.
func (p *params) serverIDQuery(name string) *string {
	v := p.q.Get(name)
	if v == "" {
		return nil
	}
	if len(v) > 255 {
		p.fail(name, "must be at most 255 characters", "out_of_range")
		return nil
	}
	return &v
}

// int64Query reads an optional int64 query parameter of at least min.
func (p *params) int64Query(name string, min int64) *int64 {
	v := p.q.Get(name)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		p.fail(name, "must be a 64-bit integer", "type")
		return nil
	}
	if n < min {
		p.fail(name, "must be at least "+strconv.FormatInt(min, 10), "out_of_range")
		return nil
	}
	return &n
}
