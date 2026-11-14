// SPDX-License-Identifier: PostgreSQL

package api

// ReplicationMember is one instance of a shard group in the topology.
type ReplicationMember struct {
	// InstanceID is the instance identifier.
	InstanceID int32 `json:"instanceId"`
	// ServerID is the server the instance runs on.
	ServerID string `json:"serverId"`
	// Role is primary or standby.
	Role string `json:"role"`
	// State is the process state.
	State string `json:"state"`
	// Port is the PostgreSQL port.
	Port int `json:"port"`
	// Timeline is the PostgreSQL timeline id.
	Timeline int `json:"timeline"`
	// SyncState is the synchronous state; null for a primary.
	SyncState *string `json:"syncState"`
	// LagBytes is the WAL bytes behind the primary; null for a primary.
	LagBytes *int64 `json:"lagBytes"`
	// LagSeconds is the replay lag; null for a primary.
	LagSeconds *float64 `json:"lagSeconds"`
	// IsOnPreferredServer tells whether a primary is on its home server.
	IsOnPreferredServer bool `json:"isOnPreferredServer"`
}

// ReplicationSlot is a replication slot on a primary.
type ReplicationSlot struct {
	// Name is the slot name.
	Name string `json:"name"`
	// Type is physical or logical.
	Type string `json:"type"`
	// Active tells whether a consumer is connected.
	Active bool `json:"active"`
	// InstanceID owns the slot.
	InstanceID int32 `json:"instanceId"`
	// RetainedWalBytes is the WAL retained for the slot.
	RetainedWalBytes int64 `json:"retainedWalBytes"`
}

// ShardGroup is a shard group with its replication state.
type ShardGroup struct {
	// ID is the shard group identifier.
	ID int32 `json:"id"`
	// IsMapGroup tells whether this is the map group.
	IsMapGroup bool `json:"isMapGroup"`
	// PreferredPrimaryServerID is the home server.
	PreferredPrimaryServerID string `json:"preferredPrimaryServerId"`
	// Health is healthy, degraded or unavailable.
	Health string `json:"health"`
	// Primary is the current primary; null when none.
	Primary *ReplicationMember `json:"primary"`
	// Standbys are the standbys.
	Standbys []ReplicationMember `json:"standbys"`
	// Slots are the replication slots on the primary.
	Slots []ReplicationSlot `json:"slots"`
}

// ReplicationTopology is the replication state of the cluster.
type ReplicationTopology struct {
	// GeneratedAt is when the topology was read.
	GeneratedAt Time `json:"generatedAt"`
	// Groups are all shard groups including the map group.
	Groups []ShardGroup `json:"groups"`
}

// TaskStep is one step of a task.
type TaskStep struct {
	// Name is the step name.
	Name string `json:"name"`
	// State is pending, running, done, failed or skipped.
	State string `json:"state"`
	// Detail is extra information.
	Detail *string `json:"detail"`
	// ItemsDone is the items completed.
	ItemsDone *int `json:"itemsDone"`
	// ItemsTotal is the items in total.
	ItemsTotal *int `json:"itemsTotal"`
	// StartedAt is when the step started.
	StartedAt *Time `json:"startedAt"`
	// FinishedAt is when the step ended.
	FinishedAt *Time `json:"finishedAt"`
}

// TaskProgress is the progress of a task.
type TaskProgress struct {
	// Percent is the overall progress.
	Percent float64 `json:"percent"`
	// CurrentStep is the name of the running step.
	CurrentStep *string `json:"currentStep"`
	// Steps are the steps in execution order.
	Steps []TaskStep `json:"steps"`
}

// TaskError says why a task failed.
type TaskError struct {
	// Code is the machine-readable reason.
	Code string `json:"code"`
	// Message is the human-readable explanation.
	Message string `json:"message"`
	// Step is the step that failed.
	Step *string `json:"step,omitempty"`
	// Retryable tells whether trying again may succeed.
	Retryable *bool `json:"retryable,omitempty"`
}

// TaskSubject is the resource a task works on.
type TaskSubject struct {
	// Type is the resource type.
	Type string `json:"type"`
	// ID is the resource identifier.
	ID string `json:"id"`
}

// Task is a durable long-running operation.
type Task struct {
	// ID is the task identifier.
	ID int64 `json:"id"`
	// Kind is the task kind, for example database.create.
	Kind string `json:"kind"`
	// State is pending, running, paused, failed or done.
	State string `json:"state"`
	// Progress is the task progress.
	Progress TaskProgress `json:"progress"`
	// Error is set when the task failed.
	Error *TaskError `json:"error"`
	// Subject is the resource the task works on.
	Subject *TaskSubject `json:"subject"`
	// Cancellable tells whether cancel is accepted now.
	Cancellable bool `json:"cancellable"`
	// CreatedAt is the creation time.
	CreatedAt Time `json:"createdAt"`
	// UpdatedAt is the last change.
	UpdatedAt Time `json:"updatedAt"`
	// CreatedBy is the role that started the task.
	CreatedBy string `json:"createdBy"`
}

// TaskPage is a page of tasks.
type TaskPage struct {
	// Items are the tasks, newest first.
	Items []Task `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// Role is a PostgreSQL role.
type Role struct {
	// Name is the role name.
	Name string `json:"name"`
	// CanLogin is the LOGIN attribute.
	CanLogin bool `json:"canLogin"`
	// IsSuperuser is the SUPERUSER attribute.
	IsSuperuser bool `json:"isSuperuser"`
	// CanCreateDatabase is the CREATEDB attribute.
	CanCreateDatabase bool `json:"canCreateDatabase"`
	// CanCreateRole is the CREATEROLE attribute.
	CanCreateRole bool `json:"canCreateRole"`
	// ConnectionLimit is the connection limit; -1 is unlimited.
	ConnectionLimit int `json:"connectionLimit"`
	// ValidUntil is the password expiry.
	ValidUntil *Time `json:"validUntil"`
	// MemberOf are ordinary roles this role belongs to.
	MemberOf []string `json:"memberOf"`
	// ClusterRoles are built-in cluster roles this role belongs to.
	ClusterRoles []string `json:"clusterRoles"`
}

// RolePage is a page of roles.
type RolePage struct {
	// Items are the roles ordered by name.
	Items []Role `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// Grant is the privileges of a role on one object.
type Grant struct {
	// Grantee holds the privileges.
	Grantee string `json:"grantee"`
	// Level is database, schema or table.
	Level string `json:"level"`
	// Database is the database.
	Database string `json:"database"`
	// Schema is null at database level.
	Schema *string `json:"schema"`
	// Table is null at database and schema level.
	Table *string `json:"table"`
	// Privileges are the privileges held.
	Privileges []string `json:"privileges"`
}

// GrantPage is a page of grants.
type GrantPage struct {
	// Items are the grants.
	Items []Grant `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// AuditEvent is one audit log entry.
type AuditEvent struct {
	// ID is the monotonic event identifier.
	ID int64 `json:"id"`
	// Timestamp is when the action happened.
	Timestamp Time `json:"timestamp"`
	// Actor is the role that performed it, or system.
	Actor string `json:"actor"`
	// ActorAddress is the client IP address.
	ActorAddress string `json:"actorAddress,omitempty"`
	// Source is panel, cli, rest or system.
	Source string `json:"source"`
	// Action is the action name.
	Action string `json:"action"`
	// ResourceType is the type of the affected resource.
	ResourceType string `json:"resourceType,omitempty"`
	// ResourceID identifies the affected resource.
	ResourceID string `json:"resourceId,omitempty"`
	// Outcome is success, failure or denied.
	Outcome string `json:"outcome"`
	// TaskID is the task started by the action.
	TaskID *int64 `json:"taskId"`
	// RequestID is the request identifier.
	RequestID string `json:"requestId,omitempty"`
	// Details is action-specific data; secrets are never recorded.
	Details map[string]any `json:"details,omitempty"`
}

// AuditPage is a page of audit events.
type AuditPage struct {
	// Items are the events, newest first.
	Items []AuditEvent `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// AutoFailoverSettings are the auto-failover cluster settings.
type AutoFailoverSettings struct {
	// Enabled tells whether failed servers fail over automatically.
	Enabled bool `json:"enabled"`
	// TimeoutSeconds is the time without heartbeat before failover.
	TimeoutSeconds int `json:"timeoutSeconds"`
	// MaxCount is the maximum automatic failovers.
	MaxCount int `json:"maxCount"`
	// EventsSinceReset is the automatic failovers since the last reset.
	EventsSinceReset int `json:"eventsSinceReset"`
}

// RebalanceSettings are the rebalance defaults.
type RebalanceSettings struct {
	// MaxConcurrentMoves is the default number of concurrent bucket moves.
	MaxConcurrentMoves int `json:"maxConcurrentMoves"`
}

// RestartState is the pending-restart part of the cluster settings.
type RestartState struct {
	// Required is true while a server must restart.
	Required bool `json:"required"`
	// ServerIDs are the servers that need a restart.
	ServerIDs []string `json:"serverIds"`
}

// ClusterSettings are the cluster-wide settings.
type ClusterSettings struct {
	// DefaultReplicaCount is the number of standbys per shard group.
	DefaultReplicaCount int `json:"defaultReplicaCount"`
	// DefaultDurability is async, sync or syncApply.
	DefaultDurability string `json:"defaultDurability"`
	// AutoFailover are the auto-failover settings.
	AutoFailover AutoFailoverSettings `json:"autoFailover"`
	// Rebalance are the rebalance defaults.
	Rebalance RebalanceSettings `json:"rebalance"`
	// Features are the engine feature flags.
	Features FeatureFlags `json:"features"`
	// Restart is the pending restart state.
	Restart RestartState `json:"restart"`
}

// LedgerChainStatus is the status of one ledger chain.
type LedgerChainStatus struct {
	// ShardGroupID owns the chain.
	ShardGroupID int32 `json:"shardGroupId"`
	// LatestBlockHeight is null before the first block.
	LatestBlockHeight *int64 `json:"latestBlockHeight"`
	// LatestBlockHash is the hash of the latest block.
	LatestBlockHash *string `json:"latestBlockHash"`
	// LatestBlockAt is the time of the latest block.
	LatestBlockAt *Time `json:"latestBlockAt"`
	// LastVerifiedBlockHeight is the highest verified height.
	LastVerifiedBlockHeight *int64 `json:"lastVerifiedBlockHeight"`
	// VerificationState is never, running, passed or failed; null when not
	// available.
	VerificationState *string `json:"verificationState"`
}

// LedgerStatus is the ledger status of a database.
type LedgerStatus struct {
	// DatabaseName is the database.
	DatabaseName string `json:"databaseName"`
	// Mode is off, optIn, appendOnly or updatable.
	Mode *string `json:"mode"`
	// DigestIntervalSeconds is the automatic digest interval.
	DigestIntervalSeconds *int `json:"digestIntervalSeconds"`
	// VerificationState is never, running, passed or failed; null when not
	// available.
	VerificationState *string `json:"verificationState"`
	// LastVerifiedAt is when the last verification finished.
	LastVerifiedAt *Time `json:"lastVerifiedAt"`
	// LedgerTableCount is the number of ledger tables.
	LedgerTableCount *int `json:"ledgerTableCount"`
	// Chains has one entry per chain.
	Chains []LedgerChainStatus `json:"chains"`
}

// LedgerStatusList is the response of GET /ledger/status.
type LedgerStatusList struct {
	// Items are the statuses.
	Items []LedgerStatus `json:"items"`
}

// LedgerBlock is a ledger block header.
type LedgerBlock struct {
	// ShardGroupID is the chain the block belongs to.
	ShardGroupID int32 `json:"shardGroupId"`
	// Height is the block height.
	Height int64 `json:"height"`
	// PrevHash is null for the first block.
	PrevHash *string `json:"prevHash"`
	// TxMerkleRoot is the Merkle root of the transactions.
	TxMerkleRoot string `json:"txMerkleRoot"`
	// BlockHash is the block hash.
	BlockHash string `json:"blockHash"`
	// Timestamp is when the block was closed.
	Timestamp Time `json:"timestamp"`
	// HashVersion is the canonical hash format version.
	HashVersion int `json:"hashVersion"`
	// TransactionCount is the transactions in the block.
	TransactionCount int `json:"transactionCount"`
	// Signed tells whether the block carries a signature.
	Signed bool `json:"signed"`
}

// LedgerBlockPage is a page of blocks.
type LedgerBlockPage struct {
	// Items are the blocks, highest first.
	Items []LedgerBlock `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}
