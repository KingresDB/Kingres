// SPDX-License-Identifier: AGPL-3.0-only

package api

import "strings"

// The types of phase 12 (contract 0.17.0): approximate aggregates, storage and time partitioning.

// DatabaseApproximate is the approximateAggregates member of Database (optional).
type DatabaseApproximate struct {
	Enabled       bool   `json:"enabled"`
	CountDistinct string `json:"countDistinct"`
}

// ApproximateServer is the state of the extension kingres_sketch on one server.
type ApproximateServer struct {
	ServerID         string  `json:"serverId"`
	InstalledVersion *string `json:"installedVersion"`
	// State is ok, notInstalled, versionMismatch or unreachable.
	State string `json:"state"`
}

// ApproximateAggregates is the response of getApproximateAggregates.
type ApproximateAggregates struct {
	Enabled          bool                `json:"enabled"`
	ExtensionVersion *string             `json:"extensionVersion"`
	CountDistinct    string              `json:"countDistinct"`
	ErrorRate        *float64            `json:"errorRate"`
	Servers          []ApproximateServer `json:"servers"`
}

// ApproximateAggregatesRequest is the request of configureApproximateAggregates.
type ApproximateAggregatesRequest struct {
	Enabled       bool     `json:"enabled"`
	CountDistinct string   `json:"countDistinct,omitempty"`
	ErrorRate     *float64 `json:"errorRate,omitempty"`
}

// ColumnarStats are the totals of the columnar format over a table or over its partitions.
type ColumnarStats struct {
	Compression       *string  `json:"compression,omitempty"`
	CompressionLevel  *int     `json:"compressionLevel,omitempty"`
	StripeRowCount    *int     `json:"stripeRowCount,omitempty"`
	ChunkRowCount     *int     `json:"chunkRowCount,omitempty"`
	Stripes           int64    `json:"stripes"`
	Rows              int64    `json:"rows"`
	BytesOnDisk       int64    `json:"bytesOnDisk"`
	BytesUncompressed int64    `json:"bytesUncompressed"`
	CompressionRatio  *float64 `json:"compressionRatio,omitempty"`
}

// PartitionFormats counts the partitions of a partitioned table in each storage format.
type PartitionFormats struct {
	Heap     int `json:"heap"`
	Columnar int `json:"columnar"`
	Other    int `json:"other"`
}

// TableStorage is the storage member of Table (contract 0.17.0).
type TableStorage struct {
	// AccessMethod is heap, columnar or another access method; null for a partitioned table.
	AccessMethod     *string           `json:"accessMethod"`
	PartitionFormats *PartitionFormats `json:"partitionFormats,omitempty"`
	Columnar         *ColumnarStats    `json:"columnar,omitempty"`
}

// TableStorageRequest is the request of setTableStorage; setPartitionStorage adds SortBy.
type TableStorageRequest struct {
	AccessMethod     string   `json:"accessMethod"`
	Compression      string   `json:"compression,omitempty"`
	CompressionLevel *int     `json:"compressionLevel,omitempty"`
	StripeRowCount   *int     `json:"stripeRowCount,omitempty"`
	ChunkRowCount    *int     `json:"chunkRowCount,omitempty"`
	SortBy           []string `json:"sortBy,omitempty"`
}

// ---------------------------------------------------------------- time partitioning (contract 0.17.0)

// PartitionPolicyRequest is the request of putPartitionPolicy. Every duration is an ISO 8601 text (P1D, P1M, PT6H).
type PartitionPolicyRequest struct {
	PartitionEvery  string   `json:"partitionEvery"`
	CreateAhead     string   `json:"createAhead,omitempty"`
	Retention       *string  `json:"retention,omitempty"`
	RetentionAction string   `json:"retentionAction,omitempty"`
	ColumnarAfter   *string  `json:"columnarAfter,omitempty"`
	ColumnarSortBy  []string `json:"columnarSortBy,omitempty"`
	Compression     *string  `json:"compression,omitempty"`
	CompressionLvl  *int     `json:"compressionLevel,omitempty"`
	Timezone        string   `json:"timezone,omitempty"`
	RunEvery        string   `json:"runEvery,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
}

// PartitionRunSummary is the lastRun member of a policy.
type PartitionRunSummary struct {
	ID         int64  `json:"id"`
	State      string `json:"state"`
	StartedAt  Time   `json:"startedAt"`
	FinishedAt *Time  `json:"finishedAt"`
}

// PartitionPolicyHealth is the health member of a policy.
type PartitionPolicyHealth struct {
	PartitionsAhead *string  `json:"partitionsAhead"`
	OK              bool     `json:"ok"`
	Problems        []string `json:"problems"`
}

// PartitionPolicy is the response of getPartitionPolicy and putPartitionPolicy.
type PartitionPolicy struct {
	PartitionEvery      string                `json:"partitionEvery"`
	CreateAhead         string                `json:"createAhead"`
	Retention           *string               `json:"retention"`
	RetentionAction     string                `json:"retentionAction"`
	ColumnarAfter       *string               `json:"columnarAfter"`
	ColumnarSortBy      []string              `json:"columnarSortBy"`
	Compression         *string               `json:"compression"`
	CompressionLevel    *int                  `json:"compressionLevel"`
	Timezone            string                `json:"timezone"`
	RunEvery            string                `json:"runEvery"`
	Enabled             bool                  `json:"enabled"`
	PartitionColumn     string                `json:"partitionColumn"`
	PartitionColumnType string                `json:"partitionColumnType"`
	NextRunAt           *Time                 `json:"nextRunAt"`
	LastRun             *PartitionRunSummary  `json:"lastRun"`
	Health              PartitionPolicyHealth `json:"health"`
	CreatedBy           string                `json:"createdBy"`
	UpdatedAt           Time                  `json:"updatedAt"`
	// TableOwner and OwnerTrusted (optional) say who owns the table and whether runs are allowed for that owner now.
	TableOwner   string `json:"tableOwner,omitempty"`
	OwnerTrusted *bool  `json:"ownerTrusted,omitempty"`
}

// PartitionAction is one action of a run or of a plan.
type PartitionAction struct {
	Seq         int     `json:"seq"`
	Kind        string  `json:"kind"`
	Partition   string  `json:"partition"`
	RangeFrom   *Time   `json:"rangeFrom"`
	RangeTo     *Time   `json:"rangeTo"`
	State       string  `json:"state"`
	Detail      *string `json:"detail"`
	BytesBefore *int64  `json:"bytesBefore"`
	BytesAfter  *int64  `json:"bytesAfter"`
	StartedAt   *Time   `json:"startedAt"`
	FinishedAt  *Time   `json:"finishedAt"`
}

// PartitionRun is a run of a policy.
type PartitionRun struct {
	ID         int64             `json:"id"`
	AsOf       Time              `json:"asOf"`
	Trigger    string            `json:"trigger"`
	State      string            `json:"state"`
	StartedAt  Time              `json:"startedAt"`
	FinishedAt *Time             `json:"finishedAt"`
	StartedBy  string            `json:"startedBy"`
	TaskID     *int64            `json:"taskId"`
	Error      *string           `json:"error"`
	Actions    []PartitionAction `json:"actions"`
}

// PartitionRunPage is a page of runs.
type PartitionRunPage struct {
	Items      []PartitionRun `json:"items"`
	NextCursor *string        `json:"nextCursor"`
}

// RunPartitionPolicyRequest is the request of runPartitionPolicy.
type RunPartitionPolicyRequest struct {
	AsOf *Time `json:"asOf,omitempty"`
}

// PlanPartitionPolicyRequest is the request of planPartitionPolicy.
type PlanPartitionPolicyRequest struct {
	AsOf   *Time                   `json:"asOf,omitempty"`
	Policy *PartitionPolicyRequest `json:"policy,omitempty"`
}

// PartitionPlan is the response of planPartitionPolicy.
type PartitionPlan struct {
	AsOf    Time              `json:"asOf"`
	Actions []PartitionAction `json:"actions"`
}

// TableStoragePreview is the answer of a storage request with dryRun=true.
type TableStoragePreview struct {
	Current     *string        `json:"current"`
	Target      string         `json:"target"`
	WillConvert bool           `json:"willConvert"`
	Blockers    []TableBlocker `json:"blockers"`
	Notes       []string       `json:"notes"`
	// TableOwner and OwnerTrusted (optional) say who owns the table and whether the analytics operations may run for that owner now.
	TableOwner   string `json:"tableOwner,omitempty"`
	OwnerTrusted *bool  `json:"ownerTrusted,omitempty"`
}

// SubjectEscape and SubjectUnescape make a name safe for a segment of the stored subject of an alert (segments are separated by "|").
func SubjectEscape(s string) string {
	return strings.NewReplacer("%", "%25", "|", "%7C").Replace(s)
}

// SubjectUnescape reverses SubjectEscape.
func SubjectUnescape(s string) string {
	return strings.NewReplacer("%7C", "|", "%25", "%").Replace(s)
}
