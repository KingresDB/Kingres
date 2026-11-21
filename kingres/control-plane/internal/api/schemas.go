// SPDX-License-Identifier: AGPL-3.0-only

package api

// SchemaDistribution is where a distributed schema lives (contract 0.15.0).
type SchemaDistribution struct {
	// State is active or moving.
	State string `json:"state"`
	// ColocationID is the colocation group of kind schema that holds the schema's one bucket.
	ColocationID int32 `json:"colocationId"`
	// ShardGroupID is the shard group that owns the schema.
	ShardGroupID int32 `json:"shardGroupId"`
	// ServerID runs the primary of the owner group; null while the group has no primary.
	ServerID *string `json:"serverId"`
	// MovingToShardGroupID is the target of the running move; null when none runs.
	MovingToShardGroupID *int32 `json:"movingToShardGroupId"`
}

// SchemaSummary is a schema of a database (contract 0.15.0).
type SchemaSummary struct {
	Name  string `json:"name"`
	Owner string `json:"owner"`
	// Distribution is null for a local schema.
	Distribution *SchemaDistribution `json:"distribution"`
	TableCount   int                 `json:"tableCount"`
	// SizeBytes is null where the answering server does not hold the tables.
	SizeBytes     *int64 `json:"sizeBytes"`
	EstimatedRows *int64 `json:"estimatedRows"`
}

// SchemaBlocker is an object that keeps a schema from being distributed.
type SchemaBlocker struct {
	ObjectKind string `json:"objectKind"`
	ObjectName string `json:"objectName"`
	Reason     string `json:"reason"`
}

// SchemaPinned says why a schema cannot move or become local.
type SchemaPinned struct {
	Reason string `json:"reason"`
}

// SchemaDetail is the response of getSchema.
type SchemaDetail struct {
	SchemaSummary
	DistributionBlockers []SchemaBlocker `json:"distributionBlockers"`
	Pinned               *SchemaPinned   `json:"pinned"`
	// MoveInProgress is the id of the unfinished task that moves the schema.
	MoveInProgress *int64 `json:"moveInProgress"`
}

// SchemaPage is a page of schemas.
type SchemaPage struct {
	Items      []SchemaSummary `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

// DistributeSchemaRequest is the body of PUT .../schemas/{schemaName}/distribution.
type DistributeSchemaRequest struct {
	ShardGroupID           *int32 `json:"shardGroupId,omitempty"`
	SetReplicaIdentityFull bool   `json:"setReplicaIdentityFull,omitempty"`
}

// MoveSchemaRequest is the body of POST .../schemas/{schemaName}/moves.
type MoveSchemaRequest struct {
	TargetShardGroupID     *int32 `json:"targetShardGroupId"`
	SetReplicaIdentityFull bool   `json:"setReplicaIdentityFull,omitempty"`
}
