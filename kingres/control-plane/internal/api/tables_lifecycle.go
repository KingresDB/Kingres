// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"bytes"
	"encoding/json"
	"fmt"
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
