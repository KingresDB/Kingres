// SPDX-License-Identifier: AGPL-3.0-only

package api

import "time"

// RebalanceOptions are the options of a rebalance (schema RebalanceOptions).
type RebalanceOptions struct {
	Databases          []string `json:"databases,omitempty"`
	MaxConcurrentMoves int      `json:"maxConcurrentMoves,omitempty"`
}

// RebalancePlanRequest is the body of previewRebalancePlan.
type RebalancePlanRequest struct {
	RebalanceOptions
	MoveLimit int `json:"moveLimit,omitempty"`
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
	Rank               int    `json:"rank"`
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
}

// RebalancePlan is a plan preview.
type RebalancePlan struct {
	Needed        bool           `json:"needed"`
	Totals        PlanTotals     `json:"totals"`
	Truncated     bool           `json:"truncated"`
	Moves         []PlannedMove  `json:"moves"`
	ServerImpacts []ServerImpact `json:"serverImpacts"`
	Warnings      []string       `json:"warnings"`
}

// BucketMove is a move in flight.
type BucketMove struct {
	DatabaseName string `json:"databaseName"`
	// Kind is hash or schema (contract 0.15.0); empty reads as hash.
	Kind string `json:"kind,omitempty"`
	// SchemaName is the schema that moves, for kind schema.
	SchemaName       *string `json:"schemaName,omitempty"`
	ColocationID     int32   `json:"colocationId"`
	Bucket           int32   `json:"bucket"`
	FromShardGroupID int32   `json:"fromShardGroupId"`
	ToShardGroupID   int32   `json:"toShardGroupId"`
	FromServerID     string  `json:"fromServerId,omitempty"`
	ToServerID       string  `json:"toServerId,omitempty"`
	Phase            string  `json:"phase"`
	Percent          float64 `json:"percent"`
	SizeBytes        *int64  `json:"sizeBytes"`
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
	StoppedBy  *string           `json:"stoppedBy"`
	Error      *string           `json:"error"`
	Totals     ReportTotals      `json:"totals"`
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
	DurationSeconds  float64 `json:"durationSeconds"`
	Error            *string `json:"error"`
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
