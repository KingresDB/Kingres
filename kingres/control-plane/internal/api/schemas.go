// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package api

// Distributed schemas (contract 0.16.0, phase 11).

// SchemaDistribution says where a distributed schema lives.
type SchemaDistribution struct {
	// State is active or moving.
	State        string `json:"state"`
	ColocationID int32  `json:"colocationId"`
	ShardGroupID int32  `json:"shardGroupId"`
	// ServerID runs the primary of the group; null when it has none.
	ServerID *string `json:"serverId"`
	// MovingToShardGroupID is the target of a running move, null otherwise.
	MovingToShardGroupID *int32 `json:"movingToShardGroupId"`
}

// SchemaSummary is one row of the list of schemas.
type SchemaSummary struct {
	Name          string              `json:"name"`
	Owner         string              `json:"owner"`
	Distribution  *SchemaDistribution `json:"distribution"`
	TableCount    int                 `json:"tableCount"`
	SizeBytes     *int64              `json:"sizeBytes"`
	EstimatedRows *int64              `json:"estimatedRows"`
}

// SchemaPage is a page of schemas.
type SchemaPage struct {
	Items      []SchemaSummary `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

// SchemaBlocker is an object that keeps a schema from being distributed.
type SchemaBlocker struct {
	ObjectKind string `json:"objectKind"`
	ObjectName string `json:"objectName"`
	Reason     string `json:"reason"`
}

// SchemaPin says why a schema cannot move or become local.
type SchemaPin struct {
	Reason string `json:"reason"`
}

// SchemaDetail is getSchema.
type SchemaDetail struct {
	SchemaSummary
	DistributionBlockers []SchemaBlocker `json:"distributionBlockers"`
	Pinned               *SchemaPin      `json:"pinned"`
	MoveInProgress       *int64          `json:"moveInProgress"`
	// TablesWithoutKey are tables with no primary key and no replica identity, which a move needs.
	TablesWithoutKey []string `json:"tablesWithoutReplicaIdentity"`
}

// DistributeSchemaRequest is the body of distributeSchema.
type DistributeSchemaRequest struct {
	ShardGroupID *int32 `json:"shardGroupId,omitempty"`
	// SetReplicaIdentityFull makes the tables without a key movable (contract 0.16.0).
	SetReplicaIdentityFull bool `json:"setReplicaIdentityFull,omitempty"`
}

// MoveSchemaRequest is the body of moveSchema.
type MoveSchemaRequest struct {
	TargetShardGroupID     int32 `json:"targetShardGroupId"`
	SetReplicaIdentityFull bool  `json:"setReplicaIdentityFull,omitempty"`
}
