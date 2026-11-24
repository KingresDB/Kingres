// SPDX-License-Identifier: AGPL-3.0-only

package api

import "time"

// The escrow of the key of the certificate authority (contract 0.26.0).

// CaEscrowRecipient is one server that holds, or should hold, a sealed copy of the CA key (schema CaEscrowRecipient).
type CaEscrowRecipient struct {
	ServerID string `json:"serverId"`
	// State is current (the copy is the newest), pending (not delivered yet) or refused (the server answered no).
	State      string     `json:"state"`
	KeyID      *string    `json:"keyId"`
	Generation *int64     `json:"generation"`
	SealedAt   *time.Time `json:"sealedAt"`
	Error      *string    `json:"error"`
}

// CaEscrowStatus is the escrow of the CA key (schema CaEscrowStatus).
type CaEscrowStatus struct {
	Enabled bool `json:"enabled"`
	// HoldsKey says whether the server that answers holds the CA key (the manager leader does).
	HoldsKey   bool                `json:"holdsKey"`
	Generation int64               `json:"generation"`
	Recipients []CaEscrowRecipient `json:"recipients"`
	// Complete is true when every recipient holds the newest copy and at least one recipient exists.
	Complete bool   `json:"complete"`
	Message  string `json:"message"`
}

// CaEscrowUpdate is the request of setCaEscrow (schema CaEscrowUpdate).
type CaEscrowUpdate struct {
	Enabled *bool `json:"enabled"`
}
