// SPDX-License-Identifier: AGPL-3.0-only

package failover

import "time"

// The JSON shapes of the contract 0.10.0 (kingres/api/openapi.yaml): the plan, the
// status and the report. The server answers with these directly.

// Blocker is FailoverBlocker.
type Blocker struct {
	Code         string `json:"code"`
	ShardGroupID *int32 `json:"shardGroupId"`
	Message      string `json:"message"`
}

// CandidateOut is FailoverCandidate.
type CandidateOut struct {
	ServerID         string  `json:"serverId"`
	StandbyID        int64   `json:"standbyId"`
	Readable         bool    `json:"readable"`
	Eligible         bool    `json:"eligible"`
	IneligibleReason *string `json:"ineligibleReason"`
	ReceiveLsn       *string `json:"receiveLsn"`
	ReplayLsn        *string `json:"replayLsn"`
	LagBytes         *int64  `json:"lagBytes"`
	SyncState        *string `json:"syncState"`
	Chosen           bool    `json:"chosen"`
}

// DataLoss is DataLossAssessment.
type DataLoss struct {
	Verdict                           string   `json:"verdict"`
	Message                           string   `json:"message"`
	LastKnownPrimaryLsn               *string  `json:"lastKnownPrimaryLsn"`
	ChosenReceiveLsn                  *string  `json:"chosenReceiveLsn"`
	LastKnownAgeSeconds               *int64   `json:"lastKnownAgeSeconds"`
	GroupRanWithoutSynchronousStandby bool     `json:"groupRanWithoutSynchronousStandby"`
	AsyncDatabases                    []string `json:"asyncDatabases"`
	UnreadableServerIDs               []string `json:"unreadableServerIds"`
}

// GroupPlan is FailoverGroupPlan.
type GroupPlan struct {
	ShardGroupID        int32          `json:"shardGroupId"`
	IsMapGroup          bool           `json:"isMapGroup"`
	FromServerID        string         `json:"fromServerId"`
	ToServerID          *string        `json:"toServerId"`
	Candidates          []CandidateOut `json:"candidates"`
	DataLoss            DataLoss       `json:"dataLoss"`
	EstimatedSeconds    *int64         `json:"estimatedSeconds"`
	WritesPausedSeconds *int64         `json:"writesPausedSeconds"`
	Steps               []string       `json:"steps"`
	MovesTheManager     bool           `json:"movesTheManager"`
	// Skipped is set for a group that a restore leaves alone (0.18.0).
	Skipped *SkippedOut `json:"skipped,omitempty"`
}

// Plan is FailoverPlan.
type Plan struct {
	GeneratedAt       time.Time   `json:"generatedAt"`
	Kind              string      `json:"kind"`
	Mode              string      `json:"mode"`
	ServerID          *string     `json:"serverId"`
	CanProceed        bool        `json:"canProceed"`
	Blockers          []Blocker   `json:"blockers"`
	WillStopRebalance bool        `json:"willStopRebalance"`
	Groups            []GroupPlan `json:"groups"`
}

// GroupProgress is FailoverGroupProgress.
type GroupProgress struct {
	ShardGroupID      int32      `json:"shardGroupId"`
	State             string     `json:"state"`
	Phase             *string    `json:"phase"`
	FromServerID      string     `json:"fromServerId"`
	ToServerID        *string    `json:"toServerId"`
	WritesPausedSince *time.Time `json:"writesPausedSince"`
	Message           *string    `json:"message"`
}

// Totals is the totals member of FailoverReportSummary.
type Totals struct {
	GroupsTotal     int   `json:"groupsTotal"`
	GroupsDone      int   `json:"groupsDone"`
	GroupsFailed    int   `json:"groupsFailed"`
	GroupsSkipped   int   `json:"groupsSkipped"`
	DurationSeconds int64 `json:"durationSeconds"`
}

// ReportSummary is FailoverReportSummary.
type ReportSummary struct {
	ID               int64      `json:"id"`
	Kind             string     `json:"kind"`
	Trigger          string     `json:"trigger"`
	ServerID         *string    `json:"serverId"`
	Outcome          string     `json:"outcome"`
	StartedAt        time.Time  `json:"startedAt"`
	FinishedAt       *time.Time `json:"finishedAt"`
	RequestedBy      string     `json:"requestedBy"`
	Totals           Totals     `json:"totals"`
	DataLossPossible bool       `json:"dataLossPossible"`
	Error            *string    `json:"error"`
}

// Status is FailoverStatus.
type Status struct {
	State         string          `json:"state"`
	Phase         *string         `json:"phase"`
	Kind          *string         `json:"kind"`
	Trigger       *string         `json:"trigger"`
	TaskID        *int64          `json:"taskId"`
	ServerID      *string         `json:"serverId"`
	StartedAt     *time.Time      `json:"startedAt"`
	PausedReason  *string         `json:"pausedReason"`
	NeedsDecision []Blocker       `json:"needsDecision"`
	Groups        []GroupProgress `json:"groups"`
	Cancellable   bool            `json:"cancellable"`
	LastReport    *ReportSummary  `json:"lastReport"`
}

// ReportPage is FailoverReportPage.
type ReportPage struct {
	Items      []ReportSummary `json:"items"`
	NextCursor *string         `json:"nextCursor"`
}

// Incident is FailoverIncident.
type IncidentOut struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// OldPrimary is OldPrimaryReport.
type OldPrimary struct {
	State           string  `json:"state"`
	Method          *string `json:"method"`
	DivergenceBytes *int64  `json:"divergenceBytes"`
	RewindBytes     *int64  `json:"rewindBytes"`
	Message         *string `json:"message"`
}

// GroupReport is FailoverGroupReport.
type GroupReport struct {
	ShardGroupID       int32          `json:"shardGroupId"`
	Outcome            string         `json:"outcome"`
	SkipReason         *string        `json:"skipReason"`
	FromServerID       string         `json:"fromServerId"`
	ToServerID         *string        `json:"toServerId"`
	TermBefore         *int64         `json:"termBefore"`
	TermAfter          *int64         `json:"termAfter"`
	Timeline           *int64         `json:"timeline"`
	LeaseWaitMs        *int           `json:"leaseWaitMs"`
	DurationSeconds    int64          `json:"durationSeconds"`
	Candidates         []CandidateOut `json:"candidates"`
	DataLoss           DataLoss       `json:"dataLoss"`
	UnreachedServerIDs []string       `json:"unreachedServerIds"`
	Incidents          []IncidentOut  `json:"incidents"`
	OldPrimary         OldPrimary     `json:"oldPrimary"`
	Error              *string        `json:"error"`
}

// Event is FailoverEvent.
type Event struct {
	At           time.Time `json:"at"`
	ShardGroupID *int32    `json:"shardGroupId"`
	Step         string    `json:"step"`
	Kind         string    `json:"kind"`
	Message      string    `json:"message"`
}

// Report is FailoverReport (the summary members are inline).
type Report struct {
	ReportSummary
	Mode    string        `json:"mode"`
	Options Input2        `json:"options"`
	Groups  []GroupReport `json:"groups"`
	Events  []Event       `json:"events"`
}

// Input2 is FailoverRequest as the report shows it.
type Input2 struct {
	Mode          string          `json:"mode"`
	ShardGroupIDs []int32         `json:"shardGroupIds,omitempty"`
	Targets       []TargetRequest `json:"targets,omitempty"`
	AllowDataLoss bool            `json:"allowDataLoss"`
	DryRun        bool            `json:"dryRun"`
	Comment       string          `json:"comment,omitempty"`
}

// TargetRequest is FailoverTarget.
type TargetRequest struct {
	ShardGroupID int32  `json:"shardGroupId"`
	ServerID     string `json:"serverId"`
}

// Request is the body of failoverServer (FailoverRequest).
type Request struct {
	Mode          string          `json:"mode"`
	ShardGroupIDs []int32         `json:"shardGroupIds"`
	Targets       []TargetRequest `json:"targets"`
	AllowDataLoss bool            `json:"allowDataLoss"`
	DryRun        bool            `json:"dryRun"`
	Comment       string          `json:"comment"`
	// ActorIsAdmin is set by the handler from the session, never from the body: a hard failover of the live leader of the map group that accepts
	// a loss of data needs kingres_admin, as giving up a stuck bootstrap does (phase 5 review L7).
	ActorIsAdmin bool `json:"-"`
}

// SkippedOut is the skipped member of FailoverGroupPlan.
type SkippedOut struct {
	Reason  string `json:"reason"`
	Message string `json:"message"`
}
