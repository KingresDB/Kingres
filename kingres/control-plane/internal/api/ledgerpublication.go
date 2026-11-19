// SPDX-License-Identifier: AGPL-3.0-only

package api

// The schemas of the aggregate and publication operations of contract 0.11.0 (ledger phase 7, steps 5 and 6).

// LedgerAggregateMember is one chain in an aggregate.
type LedgerAggregateMember struct {
	ShardGroupID int32    `json:"shardGroupId"`
	ChainID      string   `json:"chainId"`
	Epoch        int      `json:"epoch"`
	Height       int64    `json:"height"`
	BlockHash    string   `json:"blockHash"`
	LogRoot      string   `json:"logRoot"`
	Flags        []string `json:"flags"`
}

// LedgerAggregateSummary is an aggregate checkpoint without its members.
type LedgerAggregateSummary struct {
	Size                   int64   `json:"size"`
	Root                   string  `json:"root"`
	Checkpoint             string  `json:"checkpoint"`
	GeneratedAt            Time    `json:"generatedAt"`
	Complete               bool    `json:"complete"`
	UncoveredShardGroupIDs []int32 `json:"uncoveredShardGroupIds"`
}

// LedgerAggregate is an aggregate checkpoint with its members, its anchors and the statement of what it proves.
type LedgerAggregate struct {
	LedgerAggregateSummary
	Members   []LedgerAggregateMember `json:"members"`
	Anchors   []DigestAnchor          `json:"anchors"`
	Statement string                  `json:"statement"`
}

// LedgerAggregatePage is a page of aggregates, newest first.
type LedgerAggregatePage struct {
	Items      []LedgerAggregateSummary `json:"items"`
	NextCursor *string                  `json:"nextCursor"`
}

// LedgerPublicationListener is the state of the HTTP listener of the publication.
type LedgerPublicationListener struct {
	Address *string `json:"address"`
	TLS     bool    `json:"tls"`
	Running bool    `json:"running"`
}

// LedgerPublicationLog is one published log.
type LedgerPublicationLog struct {
	Kind             string `json:"kind"`
	Origin           string `json:"origin"`
	Size             int64  `json:"size"`
	LastCheckpointAt *Time  `json:"lastCheckpointAt"`
	State            string `json:"state"`
}

// LedgerPublication is the answer of getLedgerPublication.
type LedgerPublication struct {
	Enabled          bool                      `json:"enabled"`
	Listener         LedgerPublicationListener `json:"listener"`
	Logs             []LedgerPublicationLog    `json:"logs"`
	LastCheckpointAt *Time                     `json:"lastCheckpointAt"`
	Checks           map[string]any            `json:"checks"`
}
