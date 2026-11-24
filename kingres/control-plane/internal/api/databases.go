// SPDX-License-Identifier: AGPL-3.0-only

package api

// DatabaseGeneral holds the general settings of a database.
type DatabaseGeneral struct {
	// Owner is the owner role.
	Owner *string `json:"owner"`
	// Encoding is the character set encoding.
	Encoding *string `json:"encoding"`
	// LCCollate is the collation locale.
	LCCollate *string `json:"lcCollate"`
	// LCCtype is the character classification locale.
	LCCtype *string `json:"lcCtype"`
	// ConnectionLimit is the maximum connections; -1 is unlimited.
	ConnectionLimit *int `json:"connectionLimit"`
}

// ShardingSettings are the sharding settings of a database.
type ShardingSettings struct {
	// Enabled tells whether tables can be distributed.
	Enabled bool `json:"enabled"`
	// BucketCount is the number of hash buckets per colocation group; null
	// when not available.
	BucketCount *int `json:"bucketCount"`
	// Mode is row, schema or both (contract 0.15.0, optional); BucketCount is null in mode schema.
	Mode string `json:"mode,omitempty"`
	// DistributeNewSchemas tells whether a new schema is placed on a shard group by itself (contract 0.15.0, optional).
	DistributeNewSchemas *bool `json:"distributeNewSchemas,omitempty"`
}

// ReplicationSettings are the replication settings of a database.
type ReplicationSettings struct {
	// Durability is async, sync or syncApply.
	Durability *string `json:"durability"`
	// ReplicaCount is the current cluster-wide replica count (read-only).
	ReplicaCount int `json:"replicaCount"`
}

// LedgerSettings are the ledger settings of a database.
type LedgerSettings struct {
	// Mode is off, optIn, appendOnly or updatable.
	Mode *string `json:"mode"`
	// DigestIntervalSeconds is the automatic digest interval; null when not available.
	DigestIntervalSeconds *int `json:"digestIntervalSeconds"`
}

// DatabaseStats are live statistics of a database.
type DatabaseStats struct {
	// SizeBytes is the size of the database.
	SizeBytes *int64 `json:"sizeBytes"`
	// DistributedSchemaCount is the number of distributed schemas (contract 0.15.0, optional).
	DistributedSchemaCount *int `json:"distributedSchemaCount,omitempty"`
	// TableCount is the number of user tables.
	TableCount *int `json:"tableCount"`
	// ActiveConnections is the number of current connections.
	ActiveConnections *int `json:"activeConnections"`
	// TransactionsPerSecond is the recent commit rate; null when not available.
	TransactionsPerSecond *float64 `json:"transactionsPerSecond"`
}

// DatabaseHealth is the replication health of a database.
type DatabaseHealth struct {
	// UnderReplicatedBuckets have fewer live copies than the replica count.
	UnderReplicatedBuckets *int `json:"underReplicatedBuckets"`
	// UnavailableBuckets have no live primary.
	UnavailableBuckets *int `json:"unavailableBuckets"`
}

// DatabaseLedgerSummary is the ledger summary of a database.
type DatabaseLedgerSummary struct {
	// VerificationState is never, running, passed or failed.
	VerificationState *string `json:"verificationState"`
	// LastVerifiedBlockHeight is the highest verified block; null if never.
	LastVerifiedBlockHeight *int64 `json:"lastVerifiedBlockHeight"`
	// Complete and UncoveredShardGroupIDs (contract 0.9.0) state the coverage of the
	// summary, with the meaning they have on LedgerStatus. Set for a sharded database.
	Complete               *bool   `json:"complete,omitempty"`
	UncoveredShardGroupIDs []int32 `json:"uncoveredShardGroupIds,omitempty"`
}

// Database is a database with its settings groups and statistics.
type Database struct {
	// Name is the database name.
	Name string `json:"name"`
	// Status is creating, ready, dropping or failed.
	Status string `json:"status"`
	// General are the general settings.
	General DatabaseGeneral `json:"general"`
	// Sharding are the sharding settings.
	Sharding ShardingSettings `json:"sharding"`
	// Replication are the replication settings.
	Replication ReplicationSettings `json:"replication"`
	// Ledger are the ledger settings.
	Ledger LedgerSettings `json:"ledger"`
	// Stats are live statistics.
	Stats DatabaseStats `json:"stats"`
	// Health is the replication health.
	Health DatabaseHealth `json:"health"`
	// LedgerSummary is the ledger status summary.
	LedgerSummary DatabaseLedgerSummary `json:"ledgerSummary"`
	// RebalanceRequired tells whether buckets need rebalancing.
	RebalanceRequired bool `json:"rebalanceRequired"`
	// ApproximateAggregates is the approximate-aggregates setting (contract 0.17.0, optional).
	ApproximateAggregates *DatabaseApproximate `json:"approximateAggregates,omitempty"`
	// CreatedBy is the role that created the database.
	CreatedBy *string `json:"createdBy"`
	// CreatedAt is the creation time.
	CreatedAt *Time `json:"createdAt"`
}

// DatabaseList is the response of GET /databases.
type DatabaseList struct {
	// Items are the databases.
	Items []Database `json:"items"`
}

// TableDistribution describes how a table is spread over shard groups.
type TableDistribution struct {
	// Method is hash or reference.
	Method string `json:"method"`
	// Column is the distribution column; null for reference tables.
	Column *string `json:"column"`
	// ColocationID is the colocation group; null for a reference table.
	ColocationID *int32 `json:"colocationId"`
	// BucketCount is the bucket count of the colocation group; null for a reference table.
	BucketCount *int `json:"bucketCount"`
}

// TableLedger are the ledger properties of a table.
type TableLedger struct {
	// Mode is appendOnly or updatable.
	Mode string `json:"mode"`
	// HistoryTable is the history table; null for appendOnly.
	HistoryTable *string `json:"historyTable"`
}

// Table is a table with its distribution and ledger properties.
type Table struct {
	// Schema is the schema name.
	Schema string `json:"schema"`
	// Name is the table name.
	Name string `json:"name"`
	// Classes are plain, distributed, reference and ledger.
	Classes []string `json:"classes"`
	// Distribution is null for a plain table.
	Distribution *TableDistribution `json:"distribution"`
	// Ledger is null when the table is not a ledger table.
	Ledger *TableLedger `json:"ledger"`
	// HomeShardGroupID is the group holding a plain table.
	HomeShardGroupID *int32 `json:"homeShardGroupId"`
	// EstimatedRows is the estimated row count.
	EstimatedRows *int64 `json:"estimatedRows"`
	// SizeBytes is the table size; null when not available.
	SizeBytes *int64 `json:"sizeBytes"`
	// Partitioning says how the user partitioned a distributed table (contract 0.14.0, optional); null when it is not partitioned.
	Partitioning *TablePartitioning `json:"partitioning,omitempty"`
	// Storage is the storage format (contract 0.17.0, optional); null when it is not known.
	Storage *TableStorage `json:"storage,omitempty"`
}

// TablePartitioning is the partitioning member of Table.
type TablePartitioning struct {
	Method         string `json:"method"`
	Key            string `json:"key"`
	PartitionCount int    `json:"partitionCount"`
	// ColumnType, HasPolicy and DefaultPartition (contract 0.17.0, optional) are set for a table that is range-partitioned on one time column.
	ColumnType       string `json:"columnType,omitempty"`
	HasPolicy        *bool  `json:"hasPolicy,omitempty"`
	DefaultPartition *bool  `json:"defaultPartition,omitempty"`
}

// TableColumn is a column of a table with its eligibility as distribution
// column.
type TableColumn struct {
	// Name is the column name.
	Name string `json:"name"`
	// Type is the formatted PostgreSQL type.
	Type string `json:"type"`
	// Nullable tells whether the column accepts NULL.
	Nullable bool `json:"nullable"`
	// CanBeDistributionColumn tells whether the column can be hashed.
	CanBeDistributionColumn bool `json:"canBeDistributionColumn"`
	// DistributionBlockedReason is the first applicable reason when the
	// column cannot be the distribution column.
	DistributionBlockedReason *string `json:"distributionBlockedReason"`
	// BlockingKeys are the primary and unique keys that lack the column.
	BlockingKeys []string `json:"blockingKeys"`
}

// TableDetail is a table with its columns (response of getTable).
type TableDetail struct {
	Table
	// Columns are the columns in ordinal position order.
	Columns []TableColumn `json:"columns"`
}

// TablePage is a page of tables.
type TablePage struct {
	// Items are the tables.
	Items []Table `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// ColocationGroup is a set of tables that share one bucket map.
type ColocationGroup struct {
	// ColocationID is the group identifier.
	ColocationID int32 `json:"colocationId"`
	// BucketCount is the number of buckets.
	BucketCount int `json:"bucketCount"`
	// DistributionColumnType is the PostgreSQL type of the column.
	DistributionColumnType string `json:"distributionColumnType"`
	// TableCount is the number of tables in the group.
	TableCount int `json:"tableCount"`
	// SizeBytes is the size of the group's data; null when not available.
	SizeBytes *int64 `json:"sizeBytes"`
	// BaseBucketCount is the number of buckets the group was made with (contract 0.15.0, optional); BucketCount is the
	// current number, which a split raises.
	BaseBucketCount *int `json:"baseBucketCount,omitempty"`
	// MaxModulus is the greatest modulus of a bucket of the group (contract 0.15.0, optional).
	MaxModulus *int `json:"maxModulus,omitempty"`
	// Kind is hash or schema (contract 0.15.0, optional): a schema group has one bucket and holds one distributed schema.
	Kind string `json:"kind,omitempty"`
	// SchemaName is the distributed schema of a schema group, null for a hash group (contract 0.15.0, optional).
	SchemaName *string `json:"schemaName,omitempty"`
}

// ColocationGroupList is the response of the colocation group list.
type ColocationGroupList struct {
	// Items are the groups.
	Items []ColocationGroup `json:"items"`
}

// Bucket is one hash bucket of a colocation group.
type Bucket struct {
	// Bucket is the bucket number.
	Bucket int `json:"bucket"`
	// ShardGroupID owns the bucket.
	ShardGroupID int32 `json:"shardGroupId"`
	// State is active, moving or orphan.
	State string `json:"state"`
	// PrimaryServerID hosts the owning group's primary; null if none.
	PrimaryServerID *string `json:"primaryServerId"`
	// ReplicaServerIDs host the owning group's standbys.
	ReplicaServerIDs []string `json:"replicaServerIds"`
	// SizeBytes is the bucket size; null when not measured.
	SizeBytes *int64 `json:"sizeBytes"`
	// MovingToShardGroupID is the destination while moving.
	MovingToShardGroupID *int32 `json:"movingToShardGroupId"`
	// Modulus and Remainder say which hashes the bucket holds: the remainder of the hash modulo the modulus; the
	// remainder equals the bucket number (contract 0.15.0, optional).
	Modulus   *int `json:"modulus,omitempty"`
	Remainder *int `json:"remainder,omitempty"`
}

// BucketPage is a page of the bucket map.
type BucketPage struct {
	// MapVersion is the version of the bucket map.
	MapVersion int64 `json:"mapVersion"`
	// Items are the buckets.
	Items []Bucket `json:"items"`
	// NextCursor is null on the last page.
	NextCursor *string `json:"nextCursor"`
}

// ServerBucketCount is the buckets held by one server.
type ServerBucketCount struct {
	// ServerID is the server.
	ServerID string `json:"serverId"`
	// PrimaryBuckets have their primary copy here.
	PrimaryBuckets int `json:"primaryBuckets"`
	// ReplicaBuckets have a replica copy here.
	ReplicaBuckets int `json:"replicaBuckets"`
	// PrimarySizeBytes is the size of primary copies.
	PrimarySizeBytes *int64 `json:"primarySizeBytes"`
	// ReplicaSizeBytes is the size of replica copies.
	ReplicaSizeBytes *int64 `json:"replicaSizeBytes"`
}

// BucketDistribution is the bucket counts and sizes per server.
type BucketDistribution struct {
	// MapVersion is the version of the bucket map.
	MapVersion int64 `json:"mapVersion"`
	// Servers has one entry per server.
	Servers []ServerBucketCount `json:"servers"`
}

// IntegerRange is a default value with its allowed range.
type IntegerRange struct {
	// Default is the default value.
	Default int `json:"default"`
	// Minimum is the smallest allowed value.
	Minimum int `json:"minimum"`
	// Maximum is the largest allowed value.
	Maximum int `json:"maximum"`
}

// EncodingOption is a valid database encoding.
type EncodingOption struct {
	// Name is the encoding name, for example UTF8.
	Name string `json:"name"`
	// IsDefault tells whether it is used when none is given.
	IsDefault bool `json:"isDefault"`
}

// CollationOption is a valid locale name for lcCollate and lcCtype.
type CollationOption struct {
	// Name is the locale name.
	Name string `json:"name"`
	// Encodings are the encodings it can be combined with; null means all.
	Encodings []string `json:"encodings"`
	// IsDefault tells whether it is used when none is given.
	IsDefault bool `json:"isDefault"`
}

// DatabaseCreateOptions is what the create-database dialog offers.
type DatabaseCreateOptions struct {
	// Encodings are the valid encodings.
	Encodings []EncodingOption `json:"encodings"`
	// Collations are the valid locales.
	Collations []CollationOption `json:"collations"`
	// BucketCount is the bucket count default and range.
	BucketCount IntegerRange `json:"bucketCount"`
	// ReplicaCount is the cluster-wide replica count.
	ReplicaCount int `json:"replicaCount"`
	// DefaultDurability is the default durability level.
	DefaultDurability string `json:"defaultDurability"`
	// ShardingModes are the values of the sharding mode and ShardingModeDefault the default (contract 0.15.0, optional).
	ShardingModes       []string `json:"shardingModes,omitempty"`
	ShardingModeDefault string   `json:"shardingModeDefault,omitempty"`
	// LedgerDigestIntervalSeconds is the ledger digest interval range.
	LedgerDigestIntervalSeconds IntegerRange `json:"ledgerDigestIntervalSeconds"`
}
