// SPDX-License-Identifier: PostgreSQL

// Package api holds the JSON request and response types of the REST API.
//
// The types mirror the component schemas of kingres/api/openapi.yaml. They
// are hand-written because no Go generator handles that OpenAPI 3.1 file
// (see the README, "Contract types and validation"); a contract test
// validates real server responses against the schemas of the contract, so a
// type that drifts from the contract fails the build.
//
// Conventions: a property that is "required" and nullable in the contract is a
// pointer without omitempty (it is written as null); an optional property has
// omitempty. Slices must be non-nil when they are serialized: use
// make([]T, 0) for an empty list.
package api

import "time"

// Time is the type of all timestamps; it marshals as RFC 3339 UTC.
type Time = time.Time

// HealthStatus is the response of GET /healthz.
type HealthStatus struct {
	// Status is always "ok".
	Status string `json:"status"`
}

// ReadinessCheck is one readiness check.
type ReadinessCheck struct {
	// Name is the check name, for example "localPostgres".
	Name string `json:"name"`
	// OK tells whether the check passed.
	OK bool `json:"ok"`
	// Detail explains a failed check.
	Detail string `json:"detail,omitempty"`
}

// ReadinessStatus is the response of GET /readyz.
type ReadinessStatus struct {
	// Status is always "ready".
	Status string `json:"status"`
	// NodeState is the lifecycle state of the node.
	NodeState string `json:"nodeState"`
	// Checks are the individual checks.
	Checks []ReadinessCheck `json:"checks"`
}

// LoginRequest is the body of POST /auth/login.
type LoginRequest struct {
	// Username is the PostgreSQL role name.
	Username string `json:"username"`
	// Password is the role password. It is never logged or stored.
	Password string `json:"password"`
	// IssueToken asks for a bearer token in addition to the cookie.
	IssueToken bool `json:"issueToken"`
}

// Session is the signed-in user and session.
type Session struct {
	// Username is the signed-in PostgreSQL role.
	Username string `json:"username"`
	// ClusterRoles are the built-in cluster roles of the user.
	ClusterRoles []string `json:"clusterRoles"`
	// ExpiresAt is the absolute expiry.
	ExpiresAt Time `json:"expiresAt"`
	// CSRFToken is the token for X-CSRF-Token; null for bearer sessions.
	CSRFToken *string `json:"csrfToken"`
	// ClusterID and ClusterName identify the cluster; null outside a cluster.
	ClusterID   *string `json:"clusterId"`
	ClusterName *string `json:"clusterName"`
	// ServerID is the server that issued the session; null outside a cluster.
	ServerID *string `json:"serverId"`
	// Token is the bearer token; only in the login response with issueToken.
	Token *string `json:"token"`
}

// NodeStateInfo is the response of GET /setup/state.
type NodeStateInfo struct {
	// State is uninitialized, standalone or clustered.
	State string `json:"state"`
	// Address is host:port of this server's panel and REST API.
	Address string `json:"address"`
	// Hostname is the machine name.
	Hostname string `json:"hostname"`
	// Version is the KingresDB version.
	Version string `json:"version"`
	// PostgresVersion is the version of the local PostgreSQL.
	PostgresVersion string `json:"postgresVersion"`
	// SetupTokenRequired is true while the node is uninitialized.
	SetupTokenRequired bool `json:"setupTokenRequired"`
	// LocalhostOnly is true while the panel listens on loopback only.
	LocalhostOnly bool `json:"localhostOnly"`
	// ServerID is the server id once the node is in a cluster.
	ServerID *string `json:"serverId"`
	// ClusterID is the cluster id once the node is in a cluster.
	ClusterID *string `json:"clusterId"`
	// SetupTask summarizes the task started by init-cluster or join-cluster;
	// null until such a request was accepted.
	SetupTask *SetupTaskSummary `json:"setupTask"`
}

// SetupTaskSummary is the setup task as served by GET /setup/state.
type SetupTaskSummary struct {
	// ID is the task id.
	ID int64 `json:"id"`
	// Kind is cluster.init or cluster.join.
	Kind string `json:"kind"`
	// State is the task state.
	State string `json:"state"`
	// ProgressPercent is the overall progress.
	ProgressPercent float64 `json:"progressPercent"`
	// Error is set when the task failed.
	Error *TaskError `json:"error"`
}

// ClusterRoleInfo describes a built-in cluster role.
type ClusterRoleInfo struct {
	// Name is the role name.
	Name string `json:"name"`
	// Description says what the role allows.
	Description string `json:"description"`
}

// ClusterRoleInfoList is the response of GET /cluster-roles.
type ClusterRoleInfoList struct {
	// Items are the four built-in roles.
	Items []ClusterRoleInfo `json:"items"`
}
