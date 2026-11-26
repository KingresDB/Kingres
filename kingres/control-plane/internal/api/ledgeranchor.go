// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package api

import (
	"encoding/json"
	"time"
)

// The schemas of the anchoring operations of contract 0.11.0 (ledger phase 7, step 10). Credentials are
// write-only: LedgerAnchorTarget has a member that says whether they are set and none that holds them, and
// the request types hold them as raw JSON that the service reads once and never returns.

// LedgerAnchorTarget is an anchor target as the API shows it.
type LedgerAnchorTarget struct {
	ID               int64             `json:"id"`
	Kind             string            `json:"kind"`
	Name             string            `json:"name"`
	Enabled          bool              `json:"enabled"`
	Config           json.RawMessage   `json:"config"`
	CredentialsSet   bool              `json:"credentialsSet"`
	LastTest         *LedgerAnchorTest `json:"lastTest"`
	NewestAnchoredAt *time.Time        `json:"newestAnchoredAt"`
	State            string            `json:"state"`
}

// LedgerAnchorTargetList is the answer of listLedgerAnchorTargets.
type LedgerAnchorTargetList struct {
	Items []LedgerAnchorTarget `json:"items"`
}

// LedgerAnchorTargetCreate is the body of createLedgerAnchorTarget.
type LedgerAnchorTargetCreate struct {
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	Enabled bool            `json:"enabled"`
	Config  json.RawMessage `json:"config"`
	// Credentials is write-only; absent or null means none.
	Credentials json.RawMessage `json:"credentials"`
}

// LedgerAnchorTargetUpdate is the body of updateLedgerAnchorTarget; a nil member is left as it is. Credentials
// is raw JSON: absent leaves the stored credentials, an object replaces them (the empty object removes them).
type LedgerAnchorTargetUpdate struct {
	Kind        *string         `json:"kind"`
	Name        *string         `json:"name"`
	Enabled     *bool           `json:"enabled"`
	Config      json.RawMessage `json:"config"`
	Credentials json.RawMessage `json:"credentials"`
}

// LedgerAnchorCheck is one check of a test.
type LedgerAnchorCheck struct {
	Name   string `json:"name"`
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// LedgerAnchorTest is the result of the test of a target.
type LedgerAnchorTest struct {
	Checks   []LedgerAnchorCheck `json:"checks"`
	OK       bool                `json:"ok"`
	TestedAt *time.Time          `json:"testedAt,omitempty"`
}

// LedgerAnchor is an anchor record.
type LedgerAnchor struct {
	ID            int64      `json:"id"`
	TargetID      *int64     `json:"targetId"`
	TargetName    string     `json:"targetName"`
	Kind          string     `json:"kind"`
	AggregateSize int64      `json:"aggregateSize"`
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"nextAttemptAt"`
	AnchoredAt    *time.Time `json:"anchoredAt"`
	EvidenceTime  *time.Time `json:"evidenceTime"`
	LastError     *string    `json:"lastError"`
}

// LedgerAnchorPage is the answer of listLedgerAnchors.
type LedgerAnchorPage struct {
	Items      []LedgerAnchor `json:"items"`
	NextCursor *string        `json:"nextCursor"`
}

// LedgerAnchorList is the answer of anchorLedgerNow.
type LedgerAnchorList struct {
	Items []LedgerAnchor `json:"items"`
}
