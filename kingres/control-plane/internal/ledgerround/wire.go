// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package ledgerround is the collector of the publication of the ledger (design ledger-phase7.md, 4.5, 4.7, 5.2 to 5.5, 6.7 and 8.3, with the
// corrections of section 14 and the decisions of internal/ledgerround/README in the control plane README): it reads the head and the blocks
// of every chain from the primary of every shard group, extends the chain logs (the tiles, the frontier, the signed checkpoint), stops a log
// that diverges, builds the aggregate log of each database, and makes the digest of every group that generateLedgerDigest returns.
//
// It is the only writer of the logs, and it runs on the manager leader only: every write of ledger state goes through ledgerpub.Store under
// the term of the scheduler. Nothing of a database's rows leaves the cluster: a chain log holds the 87 bytes of a block header (4.3).
package ledgerround

import (
	"encoding/json"
	"time"
)

// The internal calls of the collector (internal-api.md, "The ledger read and publication calls").
const (
	// CallRead asks the server that runs the primary of a shard group for the head, a digest, the entries of blocks or the status of the
	// ledger of one database. Only the leader may call it. The call takes no SQL text.
	CallRead = "POST /ledger/read"
	// CallPublicationGet asks the manager leader for one file of the publication (a follower's listener answers a request with it).
	CallPublicationGet = "POST /ledger/publication/get"
)

// Calls lists the keys of the calls the collector's agents answer.
var Calls = []string{CallRead, CallPublicationGet}

// Operations of CallRead.
const (
	OpHead    = "head"
	OpDigest  = "digest"
	OpEntries = "entries"
	OpStatus  = "status"
)

// MaxEntries is the most blocks one read returns (the limit of ledger_blocks_entries).
const MaxEntries = 10000

// ReadRequest is the body of CallRead.
type ReadRequest struct {
	Op       string `json:"op"`
	Database string `json:"database"`
	// Group is the shard group whose primary the call is about: a server may run the primary of more than one group after a failover.
	Group int32 `json:"group"`
	// From and Max are for OpEntries.
	From int64 `json:"from,omitempty"`
	Max  int   `json:"max,omitempty"`
}

// EntryRow is the preimage of one block, as ledger_blocks_entries returns it (87 bytes, base64 in JSON).
type EntryRow struct {
	Height int64  `json:"height"`
	Entry  []byte `json:"entry"`
}

// DigestRecord is the digest the extension recorded, exactly as it made it.
type DigestRecord struct {
	ID          int64           `json:"id"`
	At          time.Time       `json:"at"`
	By          string          `json:"by"`
	Height      int64           `json:"height"`
	BlockHash   string          `json:"blockHash"`
	HashVersion int             `json:"hashVersion"`
	Document    json.RawMessage `json:"document"`
}

// ReadResponse is the answer of CallRead. Empty is true for a ledger with no block: there is no chain to publish yet.
type ReadResponse struct {
	Empty      bool            `json:"empty,omitempty"`
	ChainID    string          `json:"chainId,omitempty"`
	Height     int64           `json:"height,omitempty"`
	BlockHash  string          `json:"blockHash,omitempty"`
	ClosedAtUS int64           `json:"closedAtUs,omitempty"`
	Entries    []EntryRow      `json:"entries,omitempty"`
	Digest     *DigestRecord   `json:"digest,omitempty"`
	Status     json.RawMessage `json:"status,omitempty"`
}

// PublicationGetRequest is the body of CallPublicationGet: a path of the layout of design 4.6, without the leading slash.
type PublicationGetRequest struct {
	Path string `json:"path"`
}

// PublicationGetResponse is its answer; a file that does not exist is a 404 not_found of the call.
type PublicationGetResponse struct {
	Data        []byte `json:"data"`
	Mutable     bool   `json:"mutable"`
	ContentType string `json:"contentType"`
}
