// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"time"
)

// The types of the table lifecycle (contract 0.14.0, increment 9.6 of the shard design).

// TableBlocker is one line of the report of the sharding extension about a table (schema TableBlocker).
type TableBlocker struct {
	Code    string `json:"code"`
	Object  string `json:"object"`
	Message string `json:"message"`
}

// ColocateChoice is the colocateWith of AlterTableDistributionRequest: a table, or one of the words default and none.
type ColocateChoice struct {
	Table   *TableRef
	Keyword string
}

// UnmarshalJSON reads a TableRef object or the string default or none.
func (c *ColocateChoice) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		if s != "default" && s != "none" {
			return fmt.Errorf("colocateWith must be a table, default or none")
		}
		c.Keyword, c.Table = s, nil
		return nil
	}
	var t TableRef
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&t); err != nil {
		return err
	}
	c.Table, c.Keyword = &t, ""
	return nil
}

// MarshalJSON writes what UnmarshalJSON reads.
func (c ColocateChoice) MarshalJSON() ([]byte, error) {
	if c.Table != nil {
		return json.Marshal(c.Table)
	}
	return json.Marshal(c.Keyword)
}

// AlterTableDistributionRequest is the body of PATCH .../distribution.
type AlterTableDistributionRequest struct {
	DistributionColumn string          `json:"distributionColumn,omitempty"`
	BucketCount        *int            `json:"bucketCount,omitempty"`
	ColocateWith       *ColocateChoice `json:"colocateWith,omitempty"`
	CascadeToColocated bool            `json:"cascadeToColocated,omitempty"`
	Mode               string          `json:"mode,omitempty"`
	DependentViews     string          `json:"dependentViews,omitempty"`
}

// UndistributeOptions are the query parameters of DELETE .../distribution.
type UndistributeOptions struct {
	CascadeViaForeignKeys bool
	TargetGroupID         int
	Mode                  string
	DependentViews        string
}

// TableChangeRequest is the body of POST .../distribution/preview.
type TableChangeRequest struct {
	Kind         string                         `json:"kind"`
	Distribute   *DistributeTableRequest        `json:"distribute,omitempty"`
	Undistribute *UndistributeChange            `json:"undistribute,omitempty"`
	Alter        *AlterTableDistributionRequest `json:"alter,omitempty"`
	Partition    *PartitionChange               `json:"partition,omitempty"`
}

// UndistributeChange is the undistribute member of TableChangeRequest.
type UndistributeChange struct {
	CascadeViaForeignKeys bool   `json:"cascadeViaForeignKeys,omitempty"`
	TargetGroupID         int    `json:"targetGroupId,omitempty"`
	Mode                  string `json:"mode,omitempty"`
	DependentViews        string `json:"dependentViews,omitempty"`
}

// PartitionChange is the partition member of TableChangeRequest.
type PartitionChange struct {
	Name  string    `json:"name,omitempty"`
	Bound string    `json:"bound,omitempty"`
	Table *TableRef `json:"table,omitempty"`
}

// TableChangeTable is a table that a change touches.
type TableChangeTable struct {
	Schema        string `json:"schema"`
	Name          string `json:"name"`
	Role          string `json:"role"`
	SizeBytes     *int64 `json:"sizeBytes"`
	EstimatedRows *int64 `json:"estimatedRows"`
}

// TableChangeEstimate is the estimate of TableChangePreview.
type TableChangeEstimate struct {
	Rows                    *int64  `json:"rows"`
	Bytes                   *int64  `json:"bytes"`
	ModeRecommended         string  `json:"modeRecommended"`
	OnlineAvailable         bool    `json:"onlineAvailable"`
	OnlineUnavailableReason *string `json:"onlineUnavailableReason"`
	WritersBlocked          string  `json:"writersBlocked"`
}

// TableChangePreview is the answer of previewTableChange.
type TableChangePreview struct {
	Blockers       []TableBlocker      `json:"blockers"`
	Warnings       []TableBlocker      `json:"warnings"`
	Notes          []TableBlocker      `json:"notes"`
	AffectedTables []TableChangeTable  `json:"affectedTables"`
	Estimate       TableChangeEstimate `json:"estimate"`
}

// TablePartition is a user partition of a distributed table.
type TablePartition struct {
	Name      string `json:"name"`
	Method    string `json:"method"`
	Bound     string `json:"bound"`
	IsDefault bool   `json:"isDefault"`
	LeafCount int    `json:"leafCount"`
	SizeBytes *int64 `json:"sizeBytes"`
	// The members below are optional (contract 0.17.0).
	AccessMethod     *string  `json:"accessMethod,omitempty"`
	RangeFrom        *string  `json:"rangeFrom,omitempty"`
	RangeTo          *string  `json:"rangeTo,omitempty"`
	EstimatedRows    *int64   `json:"estimatedRows,omitempty"`
	CompressionRatio *float64 `json:"compressionRatio,omitempty"`
}

// TablePartitionPage is a page of partitions.
type TablePartitionPage struct {
	Items      []TablePartition `json:"items"`
	NextCursor *string          `json:"nextCursor"`
}

// CreateTablePartitionRequest is the body of POST .../partitions.
type CreateTablePartitionRequest struct {
	Name        string `json:"name"`
	Bound       string `json:"bound"`
	IfNotExists bool   `json:"ifNotExists,omitempty"`
}

// AttachTablePartitionRequest is the body of POST .../partitions/attach.
type AttachTablePartitionRequest struct {
	Table TableRef `json:"table"`
	Bound string   `json:"bound"`
}

// DetachTablePartitionRequest is the (empty) body of POST .../partitions/{partitionName}/detach.
type DetachTablePartitionRequest struct{}

// TableOperation is the long operation that runs on a table (contract 0.32.0, Table.operation).
type TableOperation struct {
	// Kind is distribute, undistribute, alter, partition, convertStorage, maintain, export, import or other.
	Kind string `json:"kind"`
	// Role is subject, or related for a table that a distribute with related tables of another table takes along.
	Role string `json:"role"`
	// Source is task (a task of the control plane) or extension (an operation recorded by the sharding extension).
	Source string `json:"source"`
	// TaskID is null for an operation that was started from SQL.
	TaskID *int64 `json:"taskId"`
	// State is the state of the task or the phase of the extension's record.
	State     string     `json:"state"`
	StartedAt *time.Time `json:"startedAt"`
	Percent   *float64   `json:"percent"`
	Step      *string    `json:"step"`
}

// RelatedKey is a foreign key between two tables of a distribute with related tables (contract 0.32.0).
type RelatedKey struct {
	Name             string `json:"name"`
	Schema           string `json:"schema"`
	Table            string `json:"table"`
	ReferencesSchema string `json:"referencesSchema"`
	ReferencesTable  string `json:"referencesTable"`
	Definition       string `json:"definition"`
}

// RelatedTable is a table of the foreign key closure of a table (contract 0.32.0).
type RelatedTable struct {
	Schema             string  `json:"schema"`
	Table              string  `json:"table"`
	DistributionColumn *string `json:"distributionColumn"`
	EstimatedRows      *int64  `json:"estimatedRows"`
	SizeBytes          *int64  `json:"sizeBytes"`
}

// RelatedTablesPlan is the answer of listRelatedTables (contract 0.32.0).
type RelatedTablesPlan struct {
	Tables       []RelatedTable `json:"tables"`
	Keys         []RelatedKey   `json:"keys"`
	Blockers     []TableBlocker `json:"blockers"`
	Warnings     []TableBlocker `json:"warnings"`
	ColocateWith TableRef       `json:"colocateWith"`
}
