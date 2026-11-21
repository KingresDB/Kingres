// SPDX-License-Identifier: AGPL-3.0-only

package api

import "time"

// DatabaseCreateRequest is the body of POST /databases.
type DatabaseCreateRequest struct {
	Name        string                `json:"name"`
	General     *DatabaseGeneralInput `json:"general,omitempty"`
	Sharding    *ShardingInput        `json:"sharding,omitempty"`
	Replication *ReplicationInput     `json:"replication,omitempty"`
	Ledger      *LedgerInput          `json:"ledger,omitempty"`
}

// DatabaseGeneralInput are the general settings of a new database.
type DatabaseGeneralInput struct {
	Owner           string `json:"owner,omitempty"`
	Encoding        string `json:"encoding,omitempty"`
	LcCollate       string `json:"lcCollate,omitempty"`
	LcCtype         string `json:"lcCtype,omitempty"`
	ConnectionLimit *int   `json:"connectionLimit,omitempty"`
}

// ShardingInput are the sharding settings of a new database.
type ShardingInput struct {
	Enabled     bool `json:"enabled"`
	BucketCount int  `json:"bucketCount,omitempty"`
}

// ReplicationInput are the replication settings of a new database.
type ReplicationInput struct {
	Durability string `json:"durability"`
}

// LedgerInput are the ledger settings of a new database.
type LedgerInput struct {
	Mode                  string `json:"mode"`
	DigestIntervalSeconds int    `json:"digestIntervalSeconds,omitempty"`
}

// DatabaseUpdateRequest is the body of PATCH /databases/{databaseName}.
type DatabaseUpdateRequest struct {
	General *struct {
		Owner           string `json:"owner,omitempty"`
		ConnectionLimit *int   `json:"connectionLimit,omitempty"`
	} `json:"general,omitempty"`
	Sharding *struct {
		Enabled bool `json:"enabled"`
	} `json:"sharding,omitempty"`
	Replication *ReplicationInput `json:"replication,omitempty"`
	Ledger      *LedgerInput      `json:"ledger,omitempty"`
}

// DistributeTableRequest is the body of PUT .../distribution.
type DistributeTableRequest struct {
	Method             string    `json:"method"`
	DistributionColumn string    `json:"distributionColumn,omitempty"`
	ColocateWith       *TableRef `json:"colocateWith,omitempty"`
	// Added in contract 0.14.0: the bucket count of a new colocation group, how the rows are copied (auto, online, blocking) and what to do with
	// the plain views that use the table (refuse, recreate).
	BucketCount    *int   `json:"bucketCount,omitempty"`
	Mode           string `json:"mode,omitempty"`
	DependentViews string `json:"dependentViews,omitempty"`
}

// TableRef names a table.
type TableRef struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
}

// EnableTableLedgerRequest is the body of PUT .../ledger.
type EnableTableLedgerRequest struct {
	Mode string `json:"mode"`
}

// QueryRequest is the body of POST /query.
type QueryRequest struct {
	Database  string `json:"database"`
	SQL       string `json:"sql"`
	RowLimit  *int   `json:"rowLimit,omitempty"`
	TimeoutMs *int   `json:"timeoutMs,omitempty"`
	// StatementID names the statement so that cancelStatement can end it (contract
	// 0.7.0, optional): 1 to 64 characters of A-Z, a-z, 0-9, _ and -.
	StatementID string `json:"statementId,omitempty"`
}

// QueryColumn describes a result column.
type QueryColumn struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// QueryResult is the response of POST /query.
type QueryResult struct {
	Columns    []QueryColumn `json:"columns"`
	Rows       [][]any       `json:"rows"`
	RowCount   int           `json:"rowCount"`
	Truncated  bool          `json:"truncated"`
	CommandTag string        `json:"commandTag"`
	DurationMs float64       `json:"durationMs"`
}

// LedgerDigestChain is the head of one chain in a digest.
type LedgerDigestChain struct {
	ShardGroupID int32  `json:"shardGroupId"`
	BlockHeight  int64  `json:"blockHeight"`
	BlockHash    string `json:"blockHash"`
	// Document is the digest of the chain exactly as the extension made it.
	Document JSONDocument `json:"document"`
}

// DigestAnchor is an external anchoring target (none exist yet).
type DigestAnchor map[string]any

// LedgerDigest is a recorded digest.
type LedgerDigest struct {
	ID            int64               `json:"id"`
	DatabaseName  string              `json:"databaseName"`
	GeneratedAt   Time                `json:"generatedAt"`
	GeneratedBy   *string             `json:"generatedBy"`
	HashVersion   int                 `json:"hashVersion"`
	Chains        []LedgerDigestChain `json:"chains"`
	AggregateHash *string             `json:"aggregateHash"`
	// AggregateSize is the size of the aggregate log the digest belongs to (contract 0.11.0); null until the aggregate exists.
	AggregateSize *int64         `json:"aggregateSize"`
	Anchors       []DigestAnchor `json:"anchors"`
	// Complete and UncoveredShardGroupIDs (contract 0.6.0): the digest covers
	// only the chain of the server that made it; the other shard groups of the
	// database are named here instead of being silently left out.
	Complete               bool    `json:"complete"`
	UncoveredShardGroupIDs []int32 `json:"uncoveredShardGroupIds"`
}

// LedgerDigestPage is a page of digests.
type LedgerDigestPage struct {
	Items      []LedgerDigest `json:"items"`
	NextCursor *string        `json:"nextCursor"`
}

// LedgerVerifyRequest is the body of POST .../ledger/verifications.
type LedgerVerifyRequest struct {
	Scope    string `json:"scope,omitempty"`
	DigestID *int64 `json:"digestId,omitempty"`
	// Checks (contract 0.11.0): chain (the default), publication, anchors.
	Checks []string `json:"checks,omitempty"`
}

// RebalanceSettingsUpdate is the "rebalance" member of a settings update; every member is optional but one is
// needed (contract 0.16.0 added all but the first).
type RebalanceSettingsUpdate struct {
	MaxConcurrentMoves  *int     `json:"maxConcurrentMoves,omitempty"`
	Strategy            *string  `json:"strategy,omitempty"`
	Threshold           *float64 `json:"threshold,omitempty"`
	ThresholdSet        bool     `json:"-"`
	MaxMovesPerServer   *int     `json:"maxMovesPerServer,omitempty"`
	MaxMoveAttempts     *int     `json:"maxMoveAttempts,omitempty"`
	MaxDiskUsedFraction *float64 `json:"maxDiskUsedFraction,omitempty"`
	MaxStandbyLagBytes  *int64   `json:"maxStandbyLagBytes,omitempty"`
	// Automatic is the automatic mode (contract 0.16.0).
	Automatic *AutomaticSettingsUpdate `json:"automatic,omitempty"`
}

// ClusterSettingsUpdateRequest is the body of PATCH /settings/cluster.
type ClusterSettingsUpdateRequest struct {
	DefaultReplicaCount *int    `json:"defaultReplicaCount,omitempty"`
	DefaultDurability   *string `json:"defaultDurability,omitempty"`
	AutoFailover        *struct {
		Enabled        *bool `json:"enabled,omitempty"`
		TimeoutSeconds *int  `json:"timeoutSeconds,omitempty"`
		MaxCount       *int  `json:"maxCount,omitempty"`
	} `json:"autoFailover,omitempty"`
	Rebalance *RebalanceSettingsUpdate `json:"rebalance,omitempty"`
	Features  *struct {
		Sharding *bool `json:"sharding,omitempty"`
		Ledger   *bool `json:"ledger,omitempty"`
	} `json:"features,omitempty"`
	// Clock is added in contract 0.8.0.
	Clock *struct {
		MaxDifferenceMs *int `json:"maxDifferenceMs,omitempty"`
	} `json:"clock,omitempty"`
}

// LedgerClientKey is a registered client key of the ledger (contract 0.11.0).
type LedgerClientKey struct {
	KeyID            string     `json:"keyId"`
	PublicKey        string     `json:"publicKey"`
	RoleName         *string    `json:"roleName"`
	Label            string     `json:"label"`
	RegisteredBy     string     `json:"registeredBy"`
	RegisteredAt     time.Time  `json:"registeredAt"`
	RevokedAt        *time.Time `json:"revokedAt"`
	RevocationReason *string    `json:"revocationReason"`
}

// LedgerClientKeyList is the answer of listLedgerClientKeys.
type LedgerClientKeyList struct {
	Items []LedgerClientKey `json:"items"`
}

// RegisterLedgerClientKeyRequest is the body of POST .../ledger/client-keys.
type RegisterLedgerClientKeyRequest struct {
	PublicKey string `json:"publicKey"`
	RoleName  string `json:"roleName,omitempty"`
	Label     string `json:"label"`
}

// RevokeLedgerClientKeyRequest is the body of POST .../client-keys/{keyId}/revoke.
type RevokeLedgerClientKeyRequest struct {
	Reason string `json:"reason"`
}

// SignaturePolicyRequest is the body of PUT .../ledger/signature-policy.
type SignaturePolicyRequest struct {
	Policy string `json:"policy"`
}

// LedgerBundleRequest is the body of POST .../ledger/bundles.
type LedgerBundleRequest struct {
	Logs           []string                `json:"logs,omitempty"`
	SinceSize      *int64                  `json:"sinceSize,omitempty"`
	IncludeAnchors *bool                   `json:"includeAnchors"`
	Proofs         []InclusionProofRequest `json:"proofs,omitempty"`
}

// AcknowledgeLedgerDivergenceRequest is the body of POST .../ledger/chains/{shardGroupId}/acknowledge.
type AcknowledgeLedgerDivergenceRequest struct {
	Kind    string `json:"kind"`
	Reason  string `json:"reason"`
	Confirm string `json:"confirm"`
}
