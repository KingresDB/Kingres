// SPDX-License-Identifier: AGPL-3.0-only
// Portions Copyright (c) 2026, RooyeKhat Media Co

package api

import (
	"encoding/json"
	"time"
)

// JSONDocument is a JSON value that is passed through verbatim (the
// extension's digest and proof documents).
type JSONDocument = json.RawMessage

// InclusionProofRequest is the body of POST .../ledger/inclusion-proofs.
type InclusionProofRequest struct {
	Schema string `json:"schema"`
	Table  string `json:"table"`
	// PrimaryKey maps every primary key column to the value of the row. The
	// values stay raw JSON so that big numbers are not rounded on the way.
	PrimaryKey map[string]json.RawMessage `json:"primaryKey"`
	LedgerTxID *int64                     `json:"ledgerTxId"`
	Op         string                     `json:"op"`
}

// InclusionProof is the document of kingres.ledger_inclusion_proof plus the
// member shard_group_id; its members are the extension's own (snake_case).
type InclusionProof map[string]json.RawMessage

// LedgerInconsistency is one finding of a verification.
type LedgerInconsistency struct {
	ShardGroupID int32   `json:"shardGroupId"`
	BlockHeight  *int64  `json:"blockHeight"`
	Kind         string  `json:"kind"`
	Table        *string `json:"table"`
	Detail       string  `json:"detail"`
}

// LedgerVerification is one finished verification run.
type LedgerVerification struct {
	ID              int64                 `json:"id"`
	DatabaseName    string                `json:"databaseName"`
	State           string                `json:"state"`
	Scope           string                `json:"scope"`
	DigestID        *int64                `json:"digestId"`
	TaskID          *int64                `json:"taskId"`
	StartedAt       time.Time             `json:"startedAt"`
	FinishedAt      *time.Time            `json:"finishedAt"`
	StartedBy       *string               `json:"startedBy"`
	BlocksChecked   *int64                `json:"blocksChecked"`
	RowsChecked     *int64                `json:"rowsChecked"`
	Inconsistencies []LedgerInconsistency `json:"inconsistencies"`
	// Complete and UncoveredShardGroupIDs (contract 0.6.0): a run checks one
	// chain; the shard groups of the database it did not check are named.
	Complete               bool    `json:"complete"`
	UncoveredShardGroupIDs []int32 `json:"uncoveredShardGroupIds"`
}

// LedgerVerificationPage is a page of verification runs.
type LedgerVerificationPage struct {
	Items      []LedgerVerification `json:"items"`
	NextCursor *string              `json:"nextCursor"`
}
