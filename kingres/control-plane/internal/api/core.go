// SPDX-License-Identifier: AGPL-3.0-only

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
	// ClientAddress is the network address this server saw the request come
	// from (contract 0.6.0, optional).
	ClientAddress string `json:"clientAddress,omitempty"`
	// Degraded and DegradedReason (contract 0.6.0, optional) say that the
	// answer was given from memory because the system database is stopped.
	Degraded       bool   `json:"degraded,omitempty"`
	DegradedReason string `json:"degradedReason,omitempty"`
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
	// Commit is the source commit of the build (contract 0.8.0), nil when unknown or not a hexadecimal id.
	Commit *string `json:"commit"`
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
	// InternalFingerprint is the fingerprint of the bootstrap certificate of an
	// uninitialized server, for the callers entitled to set it up (contract 0.6.0).
	InternalFingerprint *string `json:"internalFingerprint"`
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
	// Steps are the steps of the task with their state.
	Steps []SetupTaskStep `json:"steps"`
	// CreatedAt and UpdatedAt are the acceptance and last change times.
	CreatedAt Time `json:"createdAt"`
	UpdatedAt Time `json:"updatedAt"`
	// Error is set when the task failed.
	Error *TaskError `json:"error"`
	// Detail is the text the running step recorded (contract 0.6.0).
	Detail *string `json:"detail"`
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

// SetupTaskStep is one step of the setup task.
type SetupTaskStep struct {
	// Name is the step name.
	Name string `json:"name"`
	// State is pending, running, done or failed.
	State string `json:"state"`
}

// AdminAccount is the first administrator of a new cluster.
type AdminAccount struct {
	// Username is the role name.
	Username string `json:"username"`
	// Password is write-only and never logged.
	Password string `json:"password"`
}

// DataPaths are the directories of a server.
type DataPaths struct {
	// DataDirectory is the absolute data directory.
	DataDirectory string `json:"dataDirectory"`
	// WalDirectory and LogDirectory are optional directories.
	WalDirectory *string `json:"walDirectory,omitempty"`
	LogDirectory *string `json:"logDirectory,omitempty"`
}

// MemoryQuota is the memory budget of a server.
type MemoryQuota struct {
	// TotalMegabytes is the budget in MiB, at least 512.
	TotalMegabytes int `json:"totalMegabytes"`
}

// InitClusterRequest is the body of POST /setup/init-cluster.
type InitClusterRequest struct {
	// ClusterName is the display name of the cluster.
	ClusterName string `json:"clusterName"`
	// Admin is the first administrator.
	Admin AdminAccount `json:"admin"`
	// DataPaths are the directories of this server.
	DataPaths DataPaths `json:"dataPaths"`
	// MemoryQuota is the memory budget.
	MemoryQuota MemoryQuota `json:"memoryQuota"`
	// Features selects the extensions; both on when omitted.
	Features *FeatureFlags `json:"features,omitempty"`
	// DefaultReplicaCount is 0 to 3; 1 when omitted.
	DefaultReplicaCount *int `json:"defaultReplicaCount,omitempty"`
	// DefaultDurability is async, sync or syncApply; sync when omitted.
	DefaultDurability *string `json:"defaultDurability,omitempty"`
}

// JoinClusterRequest is the body of POST /setup/join-cluster.
type JoinClusterRequest struct {
	// ExistingNodeAddress is host:port of the REST API of a server of the cluster.
	ExistingNodeAddress string `json:"existingNodeAddress"`
	// Admin is an existing cluster administrator.
	Admin AdminAccount `json:"admin"`
	// DataPaths are the directories of this server.
	DataPaths DataPaths `json:"dataPaths"`
	// MemoryQuota is the memory budget (optional).
	MemoryQuota *MemoryQuota `json:"memoryQuota,omitempty"`
	// ServerGroup places the server in a group (not supported yet).
	ServerGroup *string `json:"serverGroup,omitempty"`
	// AdvertiseAddress is the address other servers use to reach this one.
	AdvertiseAddress *string `json:"advertiseAddress,omitempty"`
	// ExpectedFingerprint pins the SHA-256 fingerprint of the existing server's internal certificate.
	ExpectedFingerprint *string `json:"expectedFingerprint,omitempty"`
	// AllowUnpinned accepts a first contact whose certificate is not checked
	// (contract 0.6.0): the administrator's password goes to whatever answers.
	AllowUnpinned *bool `json:"allowUnpinned,omitempty"`
}

// AddServerRequest is the body of POST /servers.
type AddServerRequest struct {
	// Address is host:port of the new server's REST API.
	Address string `json:"address"`
	// SetupToken is the new server's one-time setup token (write-only).
	SetupToken string `json:"setupToken"`
	// ServerGroup places the server in a group (not supported yet).
	ServerGroup *string `json:"serverGroup,omitempty"`
	// ExpectedFingerprint pins the SHA-256 fingerprint of the new server's bootstrap certificate.
	ExpectedFingerprint *string `json:"expectedFingerprint,omitempty"`
	// AllowUnpinned accepts a first contact whose certificate is not checked
	// (contract 0.6.0): the setup token goes to, and a certificate of the cluster
	// to, whatever answers.
	AllowUnpinned *bool `json:"allowUnpinned,omitempty"`
}
