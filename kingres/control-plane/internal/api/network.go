// SPDX-License-Identifier: AGPL-3.0-only

package api

// NetworkAccessRule is one pg_hba-style rule.
type NetworkAccessRule struct {
	// Type is host, hostssl or hostnossl.
	Type string `json:"type"`
	// Databases and Users are names or "all".
	Databases []string `json:"databases"`
	Users     []string `json:"users"`
	// Address is a CIDR or "all".
	Address string `json:"address"`
	// Method is scram-sha-256, cert or reject.
	Method string `json:"method"`
	// Comment is free text.
	Comment *string `json:"comment,omitempty"`
}

// NetworkApplyStatus is the application state of the rules on one server.
type NetworkApplyStatus struct {
	// ServerID is the server.
	ServerID string `json:"serverId"`
	// AppliedRevision is the revision applied there.
	AppliedRevision int64 `json:"appliedRevision"`
	// Error is the error of the last attempt.
	Error *string `json:"error"`
}

// NetworkAccess is the response of GET /settings/network-access.
type NetworkAccess struct {
	// Revision is the revision of the rule set.
	Revision int64 `json:"revision"`
	// TLSRequired is true when only hostssl and cert rules are accepted.
	TLSRequired bool `json:"tlsRequired"`
	// Rules are the ordered rules.
	Rules []NetworkAccessRule `json:"rules"`
	// ApplyStatus is the per-server application state.
	ApplyStatus []NetworkApplyStatus `json:"applyStatus"`
}

// NetworkAccessUpdateRequest is the body of PUT /settings/network-access.
type NetworkAccessUpdateRequest struct {
	// Revision is the revision the client based its change on.
	Revision int64 `json:"revision"`
	// TLSRequired requires TLS for client connections.
	TLSRequired bool `json:"tlsRequired"`
	// Rules is the complete new list.
	Rules []NetworkAccessRule `json:"rules"`
}

// NetworkAccessCheck is the answer of a dry run of PUT /settings/network-access
// (contract 0.6.0).
type NetworkAccessCheck struct {
	// Valid is always true: a refused change is a problem, not valid=false.
	Valid bool `json:"valid"`
	// Revision is the current revision, which the request matched.
	Revision int64 `json:"revision"`
}
