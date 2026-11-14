// SPDX-License-Identifier: PostgreSQL

// Package problem implements the error model of the REST API: RFC 9457
// "problem details" documents served as application/problem+json, exactly as
// the Problem schema of the contract defines them.
//
// Every error response of the server is produced by Write; handlers return a
// *Problem (which implements error) and never format error bodies themselves.
package problem

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// MediaType is the content type of every error response.
const MediaType = "application/problem+json"

// typeBase is the prefix of the "type" URIs used by the contract examples.
const typeBase = "https://kingresdb.dev/problems/"

// Code is a value of the ProblemCode enum of the contract.
type Code string

// The problem codes of the contract (schema ProblemCode).
const (
	BadRequest          Code = "bad_request"
	ValidationFailed    Code = "validation_failed"
	Unauthenticated     Code = "unauthenticated"
	InvalidCredentials  Code = "invalid_credentials"
	SessionExpired      Code = "session_expired"
	CSRFTokenInvalid    Code = "csrf_token_invalid"
	Forbidden           Code = "forbidden"
	SetupTokenInvalid   Code = "setup_token_invalid"
	NotFound            Code = "not_found"
	MethodNotAllowed    Code = "method_not_allowed"
	Conflict            Code = "conflict"
	AlreadyExists       Code = "already_exists"
	ImmutableField      Code = "immutable_field"
	RebalanceInProgress Code = "rebalance_in_progress"
	OperationInProgress Code = "operation_in_progress"
	TaskNotCancellable  Code = "task_not_cancellable"
	LastAdmin           Code = "last_admin"
	RoleInUse           Code = "role_in_use"
	OptimisticLockFail  Code = "optimistic_lock_failed"
	NoReplicaAvailable  Code = "no_replica_available"
	QuorumUnavailable   Code = "quorum_unavailable"
	Unprocessable       Code = "unprocessable"
	SQLError            Code = "sql_error"
	UnsupportedOp       Code = "unsupported_operation"
	NodeStateInvalid    Code = "node_state_invalid"
	NodeUninitialized   Code = "node_uninitialized"
	MapGroupUnavailable Code = "map_group_unavailable"
	LeaderUnavailable   Code = "leader_unavailable"
	RateLimited         Code = "rate_limited"
	ServiceUnavailable  Code = "service_unavailable"
	InternalError       Code = "internal_error"
)

// FieldError is one entry of the "errors" extension (schema ValidationIssue).
type FieldError struct {
	// Field is a JSON Pointer to the invalid body property, or the name of an
	// invalid query or path parameter.
	Field string `json:"field"`
	// Message explains the problem.
	Message string `json:"message"`
	// Code is a short machine-readable reason such as "required".
	Code string `json:"code,omitempty"`
}

// Problem is an RFC 9457 problem details document (schema Problem). It is
// also an error, so handlers can return it.
type Problem struct {
	// Type identifies the problem type (URI reference).
	Type string `json:"type"`
	// Title is a short, constant summary of the problem type.
	Title string `json:"title"`
	// Status is the HTTP status code of the response.
	Status int `json:"status"`
	// Detail explains this occurrence.
	Detail string `json:"detail,omitempty"`
	// Instance is the path of the request that caused the problem.
	Instance string `json:"instance,omitempty"`
	// Code is the machine-readable code clients branch on.
	Code Code `json:"code"`
	// RequestID is the value of the X-Request-Id response header.
	RequestID string `json:"requestId,omitempty"`
	// Errors are field-level problems for validation_failed.
	Errors []FieldError `json:"errors,omitempty"`
	// SQLState is the PostgreSQL SQLSTATE for sql_error.
	SQLState string `json:"sqlState,omitempty"`
	// SQLPosition is the 1-based error position for sql_error.
	SQLPosition int `json:"sqlPosition,omitempty"`

	// retryAfter, when positive, is sent as a Retry-After header (seconds).
	retryAfter int
	// headers are extra response headers such as WWW-Authenticate or Allow.
	headers map[string]string
}

// Error implements error.
func (p *Problem) Error() string {
	if p.Detail != "" {
		return string(p.Code) + ": " + p.Detail
	}
	return string(p.Code) + ": " + p.Title
}

// specs maps each code to its HTTP status, title and type slug. The statuses
// follow the table in kingres/api/README.md.
var specs = map[Code]struct {
	status int
	title  string
	slug   string
}{
	BadRequest:          {400, "Bad request", "bad-request"},
	ValidationFailed:    {400, "Validation failed", "validation-failed"},
	Unauthenticated:     {401, "Authentication required", "unauthenticated"},
	InvalidCredentials:  {401, "Invalid credentials", "invalid-credentials"},
	SessionExpired:      {401, "Session expired", "session-expired"},
	SetupTokenInvalid:   {401, "Setup token invalid", "setup-token-invalid"},
	CSRFTokenInvalid:    {403, "CSRF token invalid", "csrf-token-invalid"},
	Forbidden:           {403, "Forbidden", "forbidden"},
	NotFound:            {404, "Not found", "not-found"},
	MethodNotAllowed:    {405, "Method not allowed", "method-not-allowed"},
	Conflict:            {409, "Conflict", "conflict"},
	AlreadyExists:       {409, "Already exists", "already-exists"},
	ImmutableField:      {409, "Immutable field", "immutable-field"},
	RebalanceInProgress: {409, "Rebalance in progress", "rebalance-in-progress"},
	OperationInProgress: {409, "Operation in progress", "operation-in-progress"},
	TaskNotCancellable:  {409, "Task not cancellable", "task-not-cancellable"},
	LastAdmin:           {409, "Last administrator", "last-admin"},
	RoleInUse:           {409, "Role in use", "role-in-use"},
	OptimisticLockFail:  {409, "Optimistic lock failed", "optimistic-lock-failed"},
	NoReplicaAvailable:  {409, "No replica available", "no-replica-available"},
	Unprocessable:       {422, "Unprocessable request", "unprocessable"},
	SQLError:            {422, "SQL error", "sql-error"},
	UnsupportedOp:       {422, "Operation not supported yet", "unsupported-operation"},
	NodeStateInvalid:    {422, "Invalid node state", "node-state-invalid"},
	RateLimited:         {429, "Too many requests", "rate-limited"},
	NodeUninitialized:   {503, "Node uninitialized", "node-uninitialized"},
	MapGroupUnavailable: {503, "Map group unavailable", "map-group-unavailable"},
	LeaderUnavailable:   {503, "Leader unavailable", "leader-unavailable"},
	QuorumUnavailable:   {503, "Quorum unavailable", "quorum-unavailable"},
	ServiceUnavailable:  {503, "Service unavailable", "service-unavailable"},
	InternalError:       {500, "Internal error", "internal-error"},
}

// New returns a Problem for the code with the standard status, title and
// type, and the given occurrence-specific detail. An unknown code becomes
// internal_error.
func New(code Code, detail string) *Problem {
	s, ok := specs[code]
	if !ok {
		s = specs[InternalError]
		code = InternalError
	}
	return &Problem{
		Type:   typeBase + s.slug,
		Title:  s.title,
		Status: s.status,
		Detail: detail,
		Code:   code,
	}
}

// Validation returns a validation_failed problem carrying field errors.
func Validation(detail string, errs ...FieldError) *Problem {
	p := New(ValidationFailed, detail)
	p.Errors = errs
	return p
}

// SQL returns a sql_error problem with the SQLSTATE and, if positive, the
// 1-based position of the error in the statement.
func SQL(detail, sqlState string, position int) *Problem {
	p := New(SQLError, detail)
	p.SQLState = sqlState
	p.SQLPosition = position
	return p
}

// WithRetryAfter sets the Retry-After header in seconds and returns p.
func (p *Problem) WithRetryAfter(seconds int) *Problem {
	p.retryAfter = seconds
	return p
}

// WithHeader adds a response header and returns p.
func (p *Problem) WithHeader(name, value string) *Problem {
	if p.headers == nil {
		p.headers = map[string]string{}
	}
	p.headers[name] = value
	return p
}

// Write sends p as application/problem+json. instance is the request path
// and requestID the value of the X-Request-Id header; both are copied into
// the body. It is the single place where error bodies are written.
func Write(w http.ResponseWriter, instance, requestID string, p *Problem) {
	out := *p
	out.Instance = instance
	out.RequestID = requestID
	body, err := json.Marshal(&out)
	if err != nil {
		// A Problem holds only strings and integers; this cannot happen.
		body = []byte(`{"type":"about:blank","title":"Internal error","status":500,"code":"internal_error"}`)
		out.Status = http.StatusInternalServerError
	}
	h := w.Header()
	h.Set("Content-Type", MediaType)
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	for k, v := range p.headers {
		h.Set(k, v)
	}
	if p.retryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(p.retryAfter))
	}
	w.WriteHeader(out.Status)
	_, _ = w.Write(append(body, '\n'))
}
