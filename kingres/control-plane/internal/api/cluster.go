// SPDX-License-Identifier: AGPL-3.0-only

package api

// ServerCounts is the number of servers per status.
type ServerCounts struct {
	// Total is the number of all servers.
	Total int `json:"total"`
	// Live servers are healthy.
	Live int `json:"live"`
	// Suspect servers have late heartbeats.
	Suspect int `json:"suspect"`
	// Draining servers are being emptied.
	Draining int `json:"draining"`
	// Decommissioning servers are being rebalanced away from.
	Decommissioning int `json:"decommissioning"`
	// Dead servers have had no liveness for the failure timeout.
	Dead int `json:"dead"`
	// PendingRebalance servers joined but hold no data yet.
	PendingRebalance int `json:"pendingRebalance"`
}

// BucketCounts are bucket totals over all colocation groups.
type BucketCounts struct {
	// Total is the number of all buckets.
	Total int `json:"total"`
	// UnderReplicated buckets have fewer live copies than the replica count.
	UnderReplicated int `json:"underReplicated"`
	// Unavailable buckets have no live primary.
	Unavailable int `json:"unavailable"`
	// Moving buckets are in state moving.
	Moving int `json:"moving"`
	// Orphan buckets are left-over shards awaiting cleanup.
	Orphan int `json:"orphan"`
}

// ManagerLeader identifies the elected manager.
type ManagerLeader struct {
	// ServerID is the server running the active manager.
	ServerID string `json:"serverId"`
	// Address is the address of that server.
	Address string `json:"address"`
	// Since is when it became leader.
	Since *Time `json:"since,omitempty"`
}

// RebalanceSummary is the rebalance part of the cluster overview.
type RebalanceSummary struct {
	// Needed tells whether placement differs from the target placement.
	Needed bool `json:"needed"`
	// Reasons lists why a rebalance is needed.
	Reasons []string `json:"reasons"`
	// Running tells whether a rebalance is running.
	Running bool `json:"running"`
	// TaskID is the task of the running rebalance.
	TaskID *int64 `json:"taskId"`
}

// AutoFailoverSummary is the auto-failover part of the overview.
type AutoFailoverSummary struct {
	// Enabled tells whether auto-failover is on.
	Enabled bool `json:"enabled"`
	// EventsSinceReset counts automatic failovers since the last reset.
	EventsSinceReset int `json:"eventsSinceReset"`
	// MaxCount is the maximum automatic failovers.
	MaxCount int `json:"maxCount"`
}

// OpenAlerts counts open alerts per severity.
type OpenAlerts struct {
	// Critical alerts.
	Critical int `json:"critical"`
	// Warning alerts.
	Warning int `json:"warning"`
	// Info alerts.
	Info int `json:"info"`
}

// FeatureFlags are the optional engine features.
type FeatureFlags struct {
	// Sharding is true when kingres_shard is enabled.
	Sharding bool `json:"sharding"`
	// Ledger is true when kingres_ledger is enabled.
	Ledger bool `json:"ledger"`
}

// ClusterOverview is the response of GET /cluster.
type ClusterOverview struct {
	// ClusterID is the cluster identifier (UUID).
	ClusterID *string `json:"clusterId"`
	// Name is the cluster name.
	Name *string `json:"name"`
	// Health is healthy, degraded or critical.
	Health string `json:"health"`
	// Version is the KingresDB version.
	Version string `json:"version"`
	// MapVersion is the version of the bucket map.
	MapVersion int64 `json:"mapVersion"`
	// MapGroupAvailable is false while the map group fails over.
	MapGroupAvailable bool `json:"mapGroupAvailable"`
	// ManagerLeader is the manager leader; null when unknown.
	ManagerLeader *ManagerLeader `json:"managerLeader"`
	// Servers counts servers per status.
	Servers ServerCounts `json:"servers"`
	// Buckets counts buckets.
	Buckets BucketCounts `json:"buckets"`
	// DatabaseCount is the number of user databases.
	DatabaseCount int `json:"databaseCount"`
	// Rebalance summarizes rebalance state.
	Rebalance RebalanceSummary `json:"rebalance"`
	// AutoFailover summarizes auto-failover.
	AutoFailover AutoFailoverSummary `json:"autoFailover"`
	// OpenAlerts counts open alerts.
	OpenAlerts OpenAlerts `json:"openAlerts"`
	// Features are the engine feature flags.
	Features FeatureFlags `json:"features"`
	// UpdatedAt is when the summary was computed.
	UpdatedAt Time `json:"updatedAt"`
}

// Instance is one PostgreSQL instance on a server.
type Instance struct {
	// ID is the node id in kingres.node.
	ID int32 `json:"id"`
	// ServerID is the server (host) the instance runs on.
	ServerID string `json:"serverId"`
	// ShardGroupID is the catalog group_id (the map group is 0).
	ShardGroupID int32 `json:"shardGroupId"`
	// IsMapGroup tells whether the group is the map group.
	IsMapGroup bool `json:"isMapGroup"`
	// Role is primary or standby.
	Role string `json:"role"`
	// State is the process state.
	State string `json:"state"`
	// Port is the PostgreSQL port.
	Port int `json:"port"`
	// DataDirectory is the data directory, when known.
	DataDirectory string `json:"dataDirectory,omitempty"`
	// Version is the PostgreSQL version, when known.
	Version string `json:"version,omitempty"`
	// SizeBytes is the data directory size; null when not available.
	SizeBytes *int64 `json:"sizeBytes"`
	// UpstreamInstanceID is the instance a standby replicates from.
	UpstreamInstanceID *int32 `json:"upstreamInstanceId"`
	// ReplicationLagBytes is the lag of a standby in bytes.
	ReplicationLagBytes *int64 `json:"replicationLagBytes"`
	// ReplicationLagSeconds is the replay lag of a standby in seconds.
	ReplicationLagSeconds *float64 `json:"replicationLagSeconds"`
}

// InstanceList is a list of instances.
type InstanceList struct {
	// Items are the instances.
	Items []Instance `json:"items"`
}

// BucketHolding is the number of buckets held as primary and replica.
type BucketHolding struct {
	// Primary buckets have their primary copy on the server.
	Primary int `json:"primary"`
	// Replica buckets have a replica copy on the server.
	Replica int `json:"replica"`
}

// ServerResourceSummary is the compact resource usage of a server.
type ServerResourceSummary struct {
	// All fields are null while the agent does not measure them.

	// CPUUsagePercent is the CPU utilization.
	CPUUsagePercent *float64 `json:"cpuUsagePercent"`
	// MemoryUsedBytes is the memory in use.
	MemoryUsedBytes *int64 `json:"memoryUsedBytes"`
	// MemoryTotalBytes is the physical memory.
	MemoryTotalBytes *int64 `json:"memoryTotalBytes"`
	// DiskUsedBytes is the used space of the data filesystem.
	DiskUsedBytes *int64 `json:"diskUsedBytes"`
	// DiskTotalBytes is the capacity of the data filesystem.
	DiskTotalBytes *int64 `json:"diskTotalBytes"`
}

// Server is a machine in the cluster.
type Server struct {
	// ID is the server identifier.
	ID string `json:"id"`
	// Address is host:port of the server's REST API.
	Address string `json:"address"`
	// Hostname is the host name.
	Hostname string `json:"hostname"`
	// Status is the server status.
	Status string `json:"status"`
	// ServerGroup is the rack or zone name.
	ServerGroup *string `json:"serverGroup"`
	// ShouldHaveShards tells whether rebalance may place buckets here.
	ShouldHaveShards bool `json:"shouldHaveShards"`
	// Weight is the capacity of the server relative to the others (contract
	// 0.16.0, optional): a server of weight 2 receives twice the load of one of
	// weight 1. Absent from a cluster whose extension has no weights.
	Weight *float64 `json:"weight,omitempty"`
	// IsManagerLeader tells whether the active manager runs here.
	IsManagerLeader bool `json:"isManagerLeader"`
	// Version is the KingresDB version.
	Version *string `json:"version"`
	// UptimeSeconds is the seconds since kingresd started; null when not available.
	UptimeSeconds *int64 `json:"uptimeSeconds"`
	// JoinedAt is when the server joined the cluster; null when not available.
	JoinedAt *Time `json:"joinedAt"`
	// LastHeartbeatAt is the last heartbeat seen by the manager; null when not available.
	LastHeartbeatAt *Time `json:"lastHeartbeatAt"`
	// CertificateExpiresAt is the notAfter of the server certificate the
	// cluster issued to the server; null when it has not been read.
	CertificateExpiresAt *Time `json:"certificateExpiresAt"`
	// CertificateRevokedAt is when the certificates of the server were revoked
	// (contract 0.5.0, optional: absent for a server whose are not).
	CertificateRevokedAt *Time `json:"certificateRevokedAt,omitempty"`
	// SeenFingerprint is the SHA-256 fingerprint (64 lower-case hex digits) of
	// the bootstrap certificate recorded on first contact; null when none was.
	SeenFingerprint *string `json:"seenFingerprint"`
	// FingerprintPinned tells whether the fingerprint was checked against one
	// the operator gave; false whenever SeenFingerprint is null.
	FingerprintPinned bool `json:"fingerprintPinned"`
	// Instances are the instances on the server.
	Instances []Instance `json:"instances"`
	// Buckets are the bucket counts of the server.
	Buckets BucketHolding `json:"buckets"`
	// SizeBytes is the data size on the server; null when not available.
	SizeBytes *int64 `json:"sizeBytes"`
	// ReplicationLagMaxSeconds is the largest replay lag of any standby.
	ReplicationLagMaxSeconds *float64 `json:"replicationLagMaxSeconds"`
	// Resources is the resource usage.
	Resources ServerResourceSummary `json:"resources"`
}

// DatabaseBucketCount is the bucket count of one database on a server.
type DatabaseBucketCount struct {
	// DatabaseName is the database.
	DatabaseName string `json:"databaseName"`
	// PrimaryBuckets have their primary copy on the server.
	PrimaryBuckets int `json:"primaryBuckets"`
	// ReplicaBuckets have a replica copy on the server.
	ReplicaBuckets int `json:"replicaBuckets"`
	// PrimarySizeBytes is the size of primary copies; null when not available.
	PrimarySizeBytes *int64 `json:"primarySizeBytes"`
	// ReplicaSizeBytes is the size of replica copies; null when not available.
	ReplicaSizeBytes *int64 `json:"replicaSizeBytes"`
}

// ServerDetail is a server with its bucket counts per database. It is the
// allOf of Server and an object with bucketsByDatabase.
type ServerDetail struct {
	Server
	// BucketsByDatabase are the bucket counts per database.
	BucketsByDatabase []DatabaseBucketCount `json:"bucketsByDatabase"`
}

// ServerList is the response of GET /servers.
type ServerList struct {
	// Items are the servers.
	Items []Server `json:"items"`
}

// ServerGroup is a rack or zone.
type ServerGroup struct {
	// Name is the group name.
	Name string `json:"name"`
	// Description is free text.
	Description string `json:"description,omitempty"`
	// ServerIDs are the member servers.
	ServerIDs []string `json:"serverIds"`
}

// ServerGroupList is the response of GET /server-groups.
type ServerGroupList struct {
	// Items are the groups.
	Items []ServerGroup `json:"items"`
}
