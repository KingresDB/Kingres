// SPDX-License-Identifier: AGPL-3.0-only

package ledgeranchor

import (
	"strconv"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
)

// logKey is the opaque log identifier of anchor.Record: the decimal id of the aggregate log.
func logKey(id int64) string { return strconv.FormatInt(id, 10) }

// logID reads logKey back (0 for a value that is not one).
func logID(k string) int64 {
	n, err := strconv.ParseInt(k, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// apiState is the state of a record as the contract shows it (LedgerAnchorState has no "claimed": a claimed
// OpenTimestamps record is shown as submitted, and is never shown or counted as anchored, correction 179).
func apiState(s anchor.State) string {
	if s == anchor.StateClaimed {
		return string(anchor.StateSubmitted)
	}
	return string(s)
}

// States of the API in the order of the contract.
var apiStates = []string{"pending", "submitted", "anchored", "failed", "superseded", "expired"}

func validAPIState(s string) bool {
	for _, x := range apiStates {
		if x == s {
			return true
		}
	}
	return false
}

// Alert kinds of anchoring (contract 0.11.0, design 6.9).
const (
	AlertNotAnchored = "ledgerNotAnchored"
	AlertBehind      = "ledgerAnchorBehind"
	AlertFailed      = anchor.AlertFailed
	AlertConflict    = anchor.AlertConflict
)
