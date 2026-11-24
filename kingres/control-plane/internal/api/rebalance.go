// SPDX-License-Identifier: AGPL-3.0-only

package api

import "time"

// RebalanceOptions are the options of a rebalance (schema RebalanceOptions).
type RebalanceOptions struct {
	Databases          []string `json:"databases,omitempty"`
	MaxConcurrentMoves int      `json:"maxConcurrentMoves,omitempty"`
	// Strategy is byBucketCount or byDiskSize (contract 0.15.0).
	Strategy string `json:"strategy,omitempty"`
	// Threshold is the largest accepted deviation from the ideal load, 0 to 1.
	Threshold *float64 `json:"threshold,omitempty"`
	// DrainOnly moves only from servers that should not hold shards.
	DrainOnly bool `json:"drainOnly,omitempty"`
	// MaxMovesPerServer limits the moves that involve one server.
	MaxMovesPerServer int `json:"maxMovesPerServer,omitempty"`
	// AcknowledgeFailures clears a suspension by failed moves.
	AcknowledgeFailures bool `json:"acknowledgeFailures,omitempty"`
	// CleanupDelaySeconds overrides the orphan delay for the run.
	CleanupDelaySeconds int `json:"cleanupDelaySeconds,omitempty"`
	// Force starts a manual run although a standby lags by more than maxStandbyLagBytes (contract 0.27.0).
	Force bool `json:"force,omitempty"`
}

// RebalancePlanRequest is the body of previewRebalancePlan.
// BucketSplitChild is one bucket a split makes (contract 0.15.0).
type BucketSplitChild struct {
	Bucket             int    `json:"bucket"`
	Modulus            int    `json:"modulus"`
	Remainder          int    `json:"remainder"`
	EstimatedSizeBytes *int64 `json:"estimatedSizeBytes"`
}

// BucketSplitPreview is the answer of previewBucketSplit.
type BucketSplitPreview struct {
	CanSplit           bool               `json:"canSplit"`
	Parts              int                `json:"parts"`
	Children           []BucketSplitChild `json:"children"`
	EstimatedSizeBytes *int64             `json:"estimatedSizeBytes"`
	ExtraDiskBytes     *int64             `json:"extraDiskBytes"`
	Blockers           []DrainBlocker     `json:"blockers"`
}

// BucketSplitRequest is the body of splitBucket and previewBucketSplit.
type BucketSplitRequest struct {
	Parts  int    `json:"parts,omitempty"`
	Verify string `json:"verify,omitempty"`
}

// DrainBlocker is something that stops a drain (contract 0.15.0).
type DrainBlocker struct {
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

// DrainLocalData is a database in which the server holds ordinary tables that a drain does not move.
type DrainLocalData struct {
	DatabaseName string `json:"databaseName"`
	TableCount   int    `json:"tableCount"`
	SizeBytes    *int64 `json:"sizeBytes"`
}

// DrainPreview is the answer of previewServerDrain (contract 0.15.0).
type DrainPreview struct {
	ServerID        string           `json:"serverId"`
	CanDrain        bool             `json:"canDrain"`
	Strategy        string           `json:"strategy,omitempty"`
	MoveCount       int              `json:"moveCount"`
	TotalBytes      *int64           `json:"totalBytes"`
	Moves           []PlannedMove    `json:"moves"`
	StandbyGroups   []int32          `json:"standbyShardGroupIds"`
	CanKeepReplicas bool             `json:"canKeepReplicas"`
	LocalData       []DrainLocalData `json:"localData"`
	Blockers        []DrainBlocker   `json:"blockers"`
	Warnings        []string         `json:"warnings"`
}

// DrainRequest is the body of drainServer (contract 0.15.0).
type DrainRequest struct {
	RemoveWhenDone       bool `json:"removeWhenDone,omitempty"`
	MaxConcurrentMoves   int  `json:"maxConcurrentMoves,omitempty"`
	AcceptFewerReplicas  bool `json:"acceptFewerReplicas,omitempty"`
	AcknowledgeLocalData bool `json:"acknowledgeLocalData,omitempty"`
}

type RebalancePlanRequest struct {
	RebalanceOptions
	MoveLimit int `json:"moveLimit,omitempty"`
	// ExcludeServerIDs plans as if these servers should not hold shards (contract 0.15.0).
	ExcludeServerIDs []string `json:"excludeServerIds,omitempty"`
}

// PlannedMove is one move of a plan.
type PlannedMove struct {
	DatabaseName       string `json:"databaseName"`
	ColocationID       int32  `json:"colocationId"`
	Bucket             int32  `json:"bucket"`
	FromShardGroupID   int32  `json:"fromShardGroupId"`
	ToShardGroupID     int32  `json:"toShardGroupId"`
	FromServerID       string `json:"fromServerId,omitempty"`
	ToServerID         string `json:"toServerId,omitempty"`
	EstimatedSizeBytes *int64 `json:"estimatedSizeBytes"`
	// Kind is move or schema and SchemaName the schema (contract 0.15.0, optional; the extension plans no schema yet).
	Kind       string  `json:"kind,omitempty"`
	SchemaName *string `json:"schemaName,omitempty"`
	// Cost is what the plan counted for the bucket: 1, or its bytes by disk size (contract 0.15.0, optional).
	Cost *int64 `json:"cost,omitempty"`
	// Reason is balance or drain (contract 0.15.0, optional).
	Reason string `json:"reason,omitempty"`
	Rank   int    `json:"rank"`
}

// PlanTotals are the totals over a full plan.
type PlanTotals struct {
	MoveCount                int    `json:"moveCount"`
	TotalBytes               *int64 `json:"totalBytes"`
	EstimatedDurationSeconds *int64 `json:"estimatedDurationSeconds"`
}

// ServerImpact is the effect of a plan on one server.
type ServerImpact struct {
	ServerID             string `json:"serverId"`
	PrimaryBucketsBefore int    `json:"primaryBucketsBefore"`
	PrimaryBucketsAfter  int    `json:"primaryBucketsAfter"`
	// LoadBefore and LoadAfter are the load of the server by the strategy: buckets, or bytes (contract 0.15.0, optional).
	LoadBefore *float64 `json:"loadBefore,omitempty"`
	LoadAfter  *float64 `json:"loadAfter,omitempty"`
}

// RebalancePlan is a plan preview.
type RebalancePlan struct {
	Needed        bool           `json:"needed"`
	Totals        PlanTotals     `json:"totals"`
	Truncated     bool           `json:"truncated"`
	Moves         []PlannedMove  `json:"moves"`
	ServerImpacts []ServerImpact `json:"serverImpacts"`
	Warnings      []string       `json:"warnings"`
	// Strategy, Threshold and Imbalance (the largest deviation of a receiver from the ideal now) are added in contract 0.15.0.
	Strategy  string   `json:"strategy,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Imbalance *float64 `json:"imbalance,omitempty"`
}

// BucketMove is a move in flight.
type BucketMove struct {
	DatabaseName     string `json:"databaseName"`
	ColocationID     int32  `json:"colocationId"`
	Bucket           int32  `json:"bucket"`
	FromShardGroupID int32  `json:"fromShardGroupId"`
	ToShardGroupID   int32  `json:"toShardGroupId"`
	FromServerID     string `json:"fromServerId,omitempty"`
	ToServerID       string `json:"toServerId,omitempty"`
	// Kind is move, split or schema and SchemaName the schema (contract 0.15.0, optional).
	Kind       string  `json:"kind,omitempty"`
	SchemaName *string `json:"schemaName,omitempty"`
	Phase      string  `json:"phase"`
	Percent    float64 `json:"percent"`
	SizeBytes  *int64  `json:"sizeBytes"`
}

// RebalanceProgress are the counters of a run.
type RebalanceProgress struct {
	MovesTotal    int     `json:"movesTotal"`
	MovesDone     int     `json:"movesDone"`
	MovesInFlight int     `json:"movesInFlight"`
	MovesFailed   int     `json:"movesFailed"`
	MovesSkipped  int     `json:"movesSkipped"`
	BytesMoved    int64   `json:"bytesMoved"`
	Percent       float64 `json:"percent"`
}

// RebalanceStatus is the current state.
type RebalanceStatus struct {
	State         string                  `json:"state"`
	Needed        bool                    `json:"needed"`
	Reasons       []string                `json:"reasons"`
	TaskID        *int64                  `json:"taskId"`
	StartedAt     *time.Time              `json:"startedAt"`
	Options       *RebalanceOptions       `json:"options"`
	Progress      *RebalanceProgress      `json:"progress"`
	InFlightMoves []BucketMove            `json:"inFlightMoves"`
	StoppedBy     *string                 `json:"stoppedBy"`
	PausedReason  *string                 `json:"pausedReason"`
	LastReport    *RebalanceReportSummary `json:"lastReport"`
	// Strategy, Threshold and Trigger of the current run (contract 0.15.0, optional).
	Strategy  string   `json:"strategy,omitempty"`
	Threshold *float64 `json:"threshold,omitempty"`
	Trigger   string   `json:"trigger,omitempty"`
	// Automatic is the state of the automatic mode (contract 0.15.0, optional).
	Automatic *RebalanceAutomatic `json:"automatic,omitempty"`
}

// RebalanceAutomatic is RebalanceStatus.automatic.
type RebalanceAutomatic struct {
	Enabled        bool            `json:"enabled"`
	NextCheckAt    *time.Time      `json:"nextCheckAt"`
	LastEvaluation *AutoEvaluation `json:"lastEvaluation"`
}

// AutoEvaluation is the result of the last round of the automatic job.
type AutoEvaluation struct {
	At        time.Time `json:"at"`
	Outcome   string    `json:"outcome"`
	BlockedBy []string  `json:"blockedBy"`
}

// ReportTotals are the totals of a run.
type ReportTotals struct {
	MovesTotal      int   `json:"movesTotal"`
	MovesDone       int   `json:"movesDone"`
	MovesFailed     int   `json:"movesFailed"`
	MovesSkipped    int   `json:"movesSkipped"`
	BytesMoved      int64 `json:"bytesMoved"`
	DurationSeconds int64 `json:"durationSeconds"`
}

// RebalanceReportSummary is the summary of a run.
type RebalanceReportSummary struct {
	ID         int64             `json:"id"`
	Outcome    string            `json:"outcome"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	Options    *RebalanceOptions `json:"options,omitempty"`
	// Trigger, Strategy and Threshold of the run (contract 0.15.0, optional).
	Trigger   string       `json:"trigger,omitempty"`
	Strategy  string       `json:"strategy,omitempty"`
	Threshold *float64     `json:"threshold,omitempty"`
	StoppedBy *string      `json:"stoppedBy"`
	Error     *string      `json:"error"`
	Totals    ReportTotals `json:"totals"`
}

// PlacementEntry is the number of primary buckets of one server in one database.
type PlacementEntry struct {
	DatabaseName   string `json:"databaseName"`
	ServerID       string `json:"serverId"`
	PrimaryBuckets int    `json:"primaryBuckets"`
}

// ReportMove is the result of one move.
type ReportMove struct {
	DatabaseName     string  `json:"databaseName"`
	ColocationID     int32   `json:"colocationId"`
	Bucket           int32   `json:"bucket"`
	FromShardGroupID int32   `json:"fromShardGroupId"`
	ToShardGroupID   int32   `json:"toShardGroupId"`
	FromServerID     string  `json:"fromServerId,omitempty"`
	ToServerID       string  `json:"toServerId,omitempty"`
	SizeBytes        *int64  `json:"sizeBytes,omitempty"`
	Outcome          string  `json:"outcome"`
	Attempts         int     `json:"attempts"`
	SkipReason       *string `json:"skipReason"`
	// Reason is balance or drain (contract 0.15.0, optional).
	Reason          string  `json:"reason,omitempty"`
	DurationSeconds float64 `json:"durationSeconds"`
	Error           *string `json:"error"`
}

// RebalanceReport is the full report of a run.
type RebalanceReport struct {
	RebalanceReportSummary
	PlacementBefore []PlacementEntry `json:"placementBefore"`
	PlacementAfter  []PlacementEntry `json:"placementAfter"`
	Moves           []ReportMove     `json:"moves"`
}

// RebalanceReportPage is a page of summaries.
type RebalanceReportPage struct {
	Items      []RebalanceReportSummary `json:"items"`
	NextCursor *string                  `json:"nextCursor"`
}
