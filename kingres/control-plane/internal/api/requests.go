// SPDX-License-Identifier: PostgreSQL

package api

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
