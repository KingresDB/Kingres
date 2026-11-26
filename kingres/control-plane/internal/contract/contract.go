// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package contract exposes the operation table of the REST API contract
// (kingres/api/openapi.yaml): which operations exist, their paths, the
// minimum cluster role (x-required-role) and the accepted security schemes.
//
// The table in operations_gen.go is generated from the contract by
// "go generate ./internal/contract"; it is never edited by hand. A test
// regenerates it in memory and fails when the checked-in file is stale.
package contract

//go:generate go run ./gen -spec ../../../api/openapi.yaml -out operations_gen.go

// BasePath is the URL prefix of every API operation.
const BasePath = "/api/v1"

// Security scheme names as declared in the contract.
const (
	SchemeSessionCookie = "sessionCookie"
	SchemeBearer        = "bearerAuth"
	SchemeSetupToken    = "setupToken"
)

// Tag names of operations that are served while the node is uninitialized.
const (
	TagHealth = "Health"
	TagSetup  = "Setup"
)

// Operation is one operation of the contract.
type Operation struct {
	// ID is the operationId.
	ID string
	// Method is the upper-case HTTP method.
	Method string
	// Path is the path template relative to BasePath.
	Path string
	// Tag is the first tag of the operation.
	Tag string
	// Role is the minimum cluster role: "none", "kingres_stats",
	// "kingres_readonly", "kingres_ops" or "kingres_admin".
	Role string
	// Security lists the security schemes of which any one authenticates the
	// request; empty means the operation is public.
	Security []string
	// PathParams are the declared path parameter names.
	PathParams []string
	// QueryParams are the declared query parameter names, sorted.
	QueryParams []string
	// Streams is the value of the extension x-streams of the contract:
	// "upload" or "download" for the two operations whose body is relayed
	// without being held in memory (added in 0.19.0), empty otherwise.
	Streams string
}

// Operations returns a copy of the generated operation table.
func Operations() []Operation {
	out := make([]Operation, len(operations))
	copy(out, operations)
	return out
}

// Mutating reports whether the operation changes state by HTTP semantics
// (anything but GET and HEAD). It decides CSRF protection and auditing.
func (o Operation) Mutating() bool {
	return o.Method != "GET" && o.Method != "HEAD"
}

// Public reports whether the operation needs no credentials at all.
func (o Operation) Public() bool {
	return len(o.Security) == 0
}

// Accepts reports whether the operation lists the given security scheme.
func (o Operation) Accepts(scheme string) bool {
	for _, s := range o.Security {
		if s == scheme {
			return true
		}
	}
	return false
}

// FullPath returns BasePath plus the path template.
func (o Operation) FullPath() string {
	return BasePath + o.Path
}

// HasQueryParam reports whether the operation declares the query parameter.
func (o Operation) HasQueryParam(name string) bool {
	for _, q := range o.QueryParams {
		if q == name {
			return true
		}
	}
	return false
}

// AuditActions returns the values of the AuditAction enum of the contract: the
// operation ids that are audited plus the events that are not operations.
func AuditActions() []string {
	return append([]string(nil), auditActions...)
}

// IsAuditAction reports whether name is a value of the AuditAction enum.
func IsAuditAction(name string) bool {
	for _, a := range auditActions {
		if a == name {
			return true
		}
	}
	return false
}
