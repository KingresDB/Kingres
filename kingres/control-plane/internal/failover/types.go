// SPDX-License-Identifier: AGPL-3.0-only

// Package failover is the manual failover of an ordinary shard group (design
// ha-phase5-part2.md, sections 3.3 and 3.6, increment 3): the task server.failover,
// its steps, the choice of the standby, the repeat loop of kingres.promote_node and
// the report. The package knows the cluster only through the ports below (Peers,
// Catalog, Planner, Rows), so that every decision of every step is tested against
// fakes; cmd/kingresd supplies the real ones.
//
// The rules that decide every doubtful case are the safety standard of the design:
// two primaries of one group never accept writes at the same time beyond the lease
// margin; an acknowledged commit of a group that requires synchronous confirmation is
// never lost by a failover; a failover that cannot be proved safe refuses or stops in
// a state from which an operator, or a repeat of the task, can go on.
package failover

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/fence"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// KindFailover is the kind of the task (contract TaskKind server.failover).
const KindFailover = "server.failover"

// KindRestore is the kind of the task that restores the preferred primaries (contract TaskKind replication.restorePreferredPrimaries); its
// subject is "replication" / "preferred-primaries".
const (
	KindRestore      = "replication.restorePreferredPrimaries"
	SubjectRestoreID = "preferred-primaries"
	SubjectRestore   = "replication"
)

// Request modes.
const (
	ModeHard     = "hard"
	ModeGraceful = "graceful"
	// ModeRestore is the restore of the preferred primaries: a switchover per group whose primary is not on its preferred server.
	ModeRestore = "restore"
)

// Input is the input of the task: the request as it was resolved when the task
// was made. It holds no secret. Groups is ascending, the map group (0) first.
type Input struct {
	Mode     string `json:"mode"`
	ServerID string `json:"serverId"`
	// Olds is the server whose primary is handed over, per group, for a restore (a restore spans servers); empty for the others, where
	// it is ServerID.
	Olds map[int32]string `json:"olds,omitempty"`
	// Skipped are the groups that a restore does not touch, with the reason (preferredNotReady, mapGroup).
	Skipped       []SkippedGroup   `json:"skipped,omitempty"`
	Groups        []int32          `json:"groups"`
	Targets       map[int32]string `json:"targets,omitempty"`
	AllowDataLoss bool             `json:"allowDataLoss"`
	Comment       string           `json:"comment,omitempty"`
	Trigger       string           `json:"trigger"`
	RequestedBy   string           `json:"requestedBy"`
	// MapLive says that this hard failover includes the map group while its server is alive (the task runs on that server, the manager leader): group 0
	// is then the last group, its old primary is stopped fenced and the standby's server finishes the work (design ha-phase5-part2.md, "a hard failover
	// of a live leader's server"). It is set when the request is resolved; a task that the bootstrap of a dead leader recorded has it false.
	MapLive bool `json:"mapLive,omitempty"`
}

// SkippedGroup is a group that a restore leaves alone.
type SkippedGroup struct {
	Group  int32  `json:"group"`
	Reason string `json:"reason"`
	Text   string `json:"text"`
}

// oldOf is the server whose primary of group g is handed over.
func (in Input) oldOf(g int32) string {
	if s := in.Olds[g]; s != "" {
		return s
	}
	return in.ServerID
}

// Task error codes (README table of the control plane; the contract does not make
// them an enum).
const (
	CodeSuperseded         = "superseded"
	CodeLeaseStillHeld     = "lease_still_held"
	CodeLeaseHeldElsewhere = "lease_held_elsewhere"
	CodeLeaseLost          = "lease_lost"
	CodeLeaseUnavailable   = "lease_unavailable"
	CodeQuorumUnavailable  = "quorum_unavailable"
	CodeNoCandidate        = "no_candidate"
	CodeTargetUnavailable  = "target_not_available"
	CodeSplitPromotion     = "split_promotion"
	CodePromoteTimeout     = "promote_timeout"
	CodePromoteIncomplete  = "promote_incomplete"
	CodeNotMapPrimary      = "not_map_primary"
	CodeBadPromoteTarget   = "bad_promote_target"
	CodePromoteUnsafe      = "promote_unsafe"
	CodeStandbyUnreadable  = "standby_unreadable"
	// The codes of the planned switchover (design 5.3).
	CodeStandbyBehind      = "standby_behind"
	CodeSwitchoverUnproven = "switchover_unproven"
	CodeCannotQuiesce      = "cannot_quiesce"
	CodePreparedPending    = "prepared_transactions_pending"
)

// Error is a failure of a step that the task records with a code (task.Error).
type Error struct {
	Code      string
	Message   string
	Retryable bool
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// TaskCode and TaskRetryable make Error a task.Coded: the task is recorded with the code.
func (e *Error) TaskCode() string { return e.Code }

// TaskRetryable says whether repeating the request can succeed.
func (e *Error) TaskRetryable() bool { return e.Retryable }

// Fail makes an Error for another package (the bootstrap of the map group).
func Fail(code string, retryable bool, format string, a ...any) *Error {
	return fail(code, retryable, format, a...)
}

func fail(code string, retryable bool, format string, a ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, a...), Retryable: retryable}
}

// PauseError asks the operator: the task is paused with the reason (task.Pause).
type PauseError = task.PauseError

// ErrUnreachable is what a Peers method returns for a server that did not answer (a
// refused connection, a time-out, a server that is down). It does not say whether
// the call took effect; a step that needs to know reads the state.
var ErrUnreachable = errors.New("failover: the server cannot be reached")

// CallError is a refusal that a server answered with.
type CallError struct {
	Status  int
	Code    string
	Message string
}

func (e *CallError) Error() string {
	return "the server answered " + strconv.Itoa(e.Status) + " " + e.Code + ": " + e.Message
}

// Is makes a 409 "fenced" answer match fence.ErrFenced: the callee has seen a newer run of the task, and the
// engine then stands down instead of failing the task.
func (e *CallError) Is(target error) bool {
	return target == fence.ErrFenced && e.Status == 409 && e.Code == "fenced"
}

// IsCode tells whether err is a CallError with this code.
func IsCode(err error, code string) bool {
	var ce *CallError
	return errors.As(err, &ce) && ce.Code == code
}

// IsOlderServer says that a server answered 404 not_found: it does not know the call
// (an older version). A 404 with the code no_instance is a real answer: the server
// runs no instance of the group.
func IsOlderServer(err error) bool {
	var ce *CallError
	return errors.As(err, &ce) && ce.Status == 404 && ce.Code == "not_found"
}

// Position is the answer of GET /failover/position.
type Position struct {
	Server           string `json:"server"`
	Group            int32  `json:"group"`
	InstanceID       int    `json:"instanceId"`
	Role             string `json:"role"`
	InRecovery       bool   `json:"inRecovery"`
	Timeline         int64  `json:"timeline"`
	ReceiveLsn       string `json:"receiveLsn"`
	ReplayLsn        string `json:"replayLsn"`
	ReceiverState    string `json:"receiverState"`
	SystemIdentifier string `json:"systemIdentifier"`
	CatalogTerm      *int64 `json:"catalogTerm"`
}

// Flushed is the position that counts for the choice: ReceiveLsn is empty when no
// walreceiver has run since the start of the instance, so the larger of it and
// ReplayLsn is used (extension precondition 2; review FO-04).
func (p Position) Flushed() (uint64, bool) {
	a, okA := ParseLSN(p.ReceiveLsn)
	b, okB := ParseLSN(p.ReplayLsn)
	switch {
	case okA && okB:
		if b > a {
			return b, true
		}
		return a, true
	case okA:
		return a, true
	case okB:
		return b, true
	}
	return 0, false
}

// ParseLSN reads the X/Y form of a WAL position.
func ParseLSN(s string) (uint64, bool) {
	hi, lo, ok := strings.Cut(strings.TrimSpace(s), "/")
	if !ok || hi == "" || lo == "" {
		return 0, false
	}
	h, err1 := strconv.ParseUint(hi, 16, 32)
	l, err2 := strconv.ParseUint(lo, 16, 32)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return h<<32 | l, true
}

// FormatLSN is the X/Y form of a position.
func FormatLSN(v uint64) string { return fmt.Sprintf("%X/%X", v>>32, v&0xffffffff) }

// StateInstance is one instance of a server in the answer of GET /failover/state.
type StateInstance struct {
	ID      int    `json:"id"`
	Port    int    `json:"port"`
	Role    string `json:"role"`
	Origin  string `json:"origin"`
	Group   int32  `json:"group"`
	Term    int64  `json:"term"`
	Running bool   `json:"running"`
	// RunningUnknown: the server could not say whether the instance runs (Running means nothing then).
	RunningUnknown bool  `json:"runningUnknown,omitempty"`
	InRecovery     *bool `json:"inRecovery"`
}

// State is the answer of GET /failover/state: one server's knowledge of one group.
type State struct {
	Server       string          `json:"server"`
	Group        int32           `json:"group"`
	Instances    []StateInstance `json:"instances"`
	Holds        bool            `json:"holds"`
	LeaseState   string          `json:"leaseState"`
	Adopted      string          `json:"adopted,omitempty"`
	Attached     bool            `json:"attached,omitempty"`
	Relinquished bool            `json:"relinquished"`
	Frozen       bool            `json:"frozen"`
	CatalogTerm  *int64          `json:"catalogTerm"`
	Prepared     *int            `json:"preparedTransactions"`
	Destination  string          `json:"destination,omitempty"`
	// Watchdog says whether a watchdog of the group's primary instance runs on the server (nil: unknown).
	Watchdog *bool `json:"watchdog"`
	// Quiesced says that the writes of the group are stopped on this server (a switchover is under way).
	Quiesced bool `json:"quiesced"`
	// Enforced says whether this server's lease manager would stop the primary of the group when its lease lapsed: it does
	// only for a group that its plan says has a standby (F3-02; nil: unknown or no primary here).
	Enforced *bool `json:"enforced"`
}

// OutOfRecovery lists the instances of the group on this server that run and are
// not in recovery: the writable ones.
func (s State) OutOfRecovery() []StateInstance {
	var out []StateInstance
	for _, i := range s.Instances {
		if i.Group == s.Group && i.Running && i.InRecovery != nil && !*i.InRecovery {
			out = append(out, i)
		}
	}
	return out
}

// LeaseSettings are the durations a server's grantor and manager use.
type LeaseSettings struct {
	DurationMs int64 `json:"durationMs"`
	MarginMs   int64 `json:"marginMs"`
}

// DemoteRequest is the body of POST /failover/demote: stop the instance that serves
// the group here, and only then hand the lease to TransferTo (design 3.3 F2).
type DemoteRequest struct {
	Group int32  `json:"group"`
	Mode  string `json:"mode"` // "immediate"
	// Mode "fast" is the clean stop of a switchover: the lease is released (not transferred) and the answer carries the proof.
	ExpectedTerm int64  `json:"expectedTerm"`
	TransferTo   string `json:"transferTo"`
	// Candidate is the server that a planned switchover of the map group hands the group to (mode fast, group 0): the old leader records it before it stops, and resumes
	// its primary later only on that server's word (F7-01).
	Candidate string `json:"candidate,omitempty"`
}

// DemoteAnswer is the answer of POST /failover/demote.
type DemoteAnswer struct {
	Stopped      bool   `json:"stopped"`
	ClusterState string `json:"clusterState,omitempty"`
	Transferred  bool   `json:"transferred"`
	// Checkpoint is "Latest checkpoint location" of pg_controldata, read after a clean stop; Released says that the lease was released.
	Checkpoint string `json:"checkpoint,omitempty"`
	Released   bool   `json:"released,omitempty"`
}

// The calls of the planned switchover (design 5.2). The same group number, task and epoch rules as the other calls apply.

// QuiesceRequest is the body of POST /failover/quiesce and /unquiesce.
type QuiesceRequest struct {
	Group int32 `json:"group"`
	// ExpectedTerm is the catalog term the caller knows (F4-09): the undo calls cannot carry the fence of a task run (the repair of a switchover has
	// none), so a call of a run or of a leader that is behind the catalog is refused by the term.
	ExpectedTerm int64 `json:"expectedTerm"`
}

// QuiesceAnswer says whether no client session of the instance is left.
type QuiesceAnswer struct {
	Quiesced  bool `json:"quiesced"`
	Remaining int  `json:"remaining"`
}

// MarkRequest is the body of POST /failover/mark: the primary reports the position M that a standby must replay (design S4).
type MarkRequest struct {
	Group        int32 `json:"group"`
	ExpectedTerm int64 `json:"expectedTerm"`
}

// MarkAnswer is the answer of POST /failover/mark.
type MarkAnswer struct {
	Mark string `json:"mark"`
}

// VerifyRequest is the body of POST /failover/verify, sent to the server of the standby. Without Checkpoint it asks whether the standby
// replayed Mark (switchover_replayed); with it, whether the standby has everything the old primary wrote (switchover_verify).
type VerifyRequest struct {
	Group        int32  `json:"group"`
	InstanceID   int    `json:"instanceId"`
	Mark         string `json:"mark,omitempty"`
	Checkpoint   string `json:"checkpoint,omitempty"`
	ClusterState string `json:"clusterState,omitempty"`
}

// VerifyAnswer is the answer of POST /failover/verify: Ok is the t or f of the extension; a proof that can never be made is the
// call error "unprovable".
type VerifyAnswer struct {
	Ok bool `json:"ok"`
}

// ResumeRequest is the body of POST /failover/resume: the rollback of a switchover whose old primary was stopped.
type ResumeRequest struct {
	Group        int32 `json:"group"`
	ExpectedTerm int64 `json:"expectedTerm"`
}

// Lease roles of POST /failover/lease.
const (
	LeaseFence  = "fence"
	LeaseHolder = "holder"
	// LeaseRelease stops renewing an adopted lease (the run was cancelled or failed); it
	// runs out at the grantors by itself.
	LeaseRelease = "release"
)

// LeaseRequest is the body of POST /failover/lease: adopt the lease of the group (the
// fence holder, or the server of the standby that will be promoted), or hand it over
// to TransferTo, and report what the last round found.
type LeaseRequest struct {
	Group        int32  `json:"group"`
	Role         string `json:"role"`
	TransferTo   string `json:"transferTo,omitempty"`
	ExpectedTerm int64  `json:"expectedTerm"`
}

// LeaseAnswer is the answer of POST /failover/lease.
type LeaseAnswer struct {
	Held        bool              `json:"held"`
	Rounds      int               `json:"rounds"`
	WaitMs      int64             `json:"waitMs"`
	RefusedBy   map[string]string `json:"refusedBy,omitempty"`
	Transferred bool              `json:"transferred"`
	Holder      string            `json:"holder,omitempty"`
}

// PromoteRequest is the body of POST /failover/promote.
type PromoteRequest struct {
	Group        int32 `json:"group"`
	StandbyID    int64 `json:"standbyId"`
	InstanceID   int   `json:"instanceId"`
	ExpectedTerm int64 `json:"expectedTerm"`
	// ReapplyList only writes the empty synchronous_standby_names again (a KS009 for the
	// new primary's own list) and answers as a repeat would.
	ReapplyList bool `json:"reapplyList,omitempty"`
	// Checkpoint and ClusterState are the shutdown checkpoint and the state of the old primary's cluster, for a planned switchover: the callee
	// makes the proof (switchover_verify) again in the call that promotes, so that nothing that happened after the first proof is promoted over
	// (F4-07).
	Checkpoint   string `json:"checkpoint,omitempty"`
	ClusterState string `json:"clusterState,omitempty"`
}

// PromoteAnswer is the answer of POST /failover/promote.
type PromoteAnswer struct {
	Status   string `json:"status"` // promoted | already
	Timeline int64  `json:"timeline"`
	Lsn      string `json:"lsn"`
	TookMs   int64  `json:"tookMs"`
}

// RepointRequest is the body of POST /standby/repoint (design 3.3 F8).
type RepointRequest struct {
	Group        int32  `json:"group"`
	StandbyID    int64  `json:"standbyId"`
	InstanceID   int    `json:"instanceId"`
	PrimaryHost  string `json:"primaryHost"`
	PrimaryPort  int    `json:"primaryPort"`
	ExpectedTerm int64  `json:"expectedTerm"`
}

// RepointAnswer is the answer of POST /standby/repoint.
type RepointAnswer struct {
	Streaming bool   `json:"streaming"`
	State     string `json:"state"`
}

// Peers is how the leader's task reaches the other servers (and its own). Every
// method returns ErrUnreachable (possibly wrapped) for a server that did not answer,
// and a *CallError for a refusal.
type Peers interface {
	Position(ctx context.Context, server string, group int32) (Position, error)
	State(ctx context.Context, server string, group int32) (State, error)
	LeaseSettings(ctx context.Context, server string) (LeaseSettings, error)
	Freeze(ctx context.Context, server string, group int32, ttl time.Duration) error
	Unfreeze(ctx context.Context, server string, group int32) error
	StandbysGone(ctx context.Context, server, gone string, ttl time.Duration) error
	Demote(ctx context.Context, server string, r DemoteRequest) (DemoteAnswer, error)
	Lease(ctx context.Context, server string, r LeaseRequest) (LeaseAnswer, error)
	Promote(ctx context.Context, server string, r PromoteRequest) (PromoteAnswer, error)
	Repoint(ctx context.Context, server string, r RepointRequest) (RepointAnswer, error)
	// RecoverPrepared runs kingres.recover_prepared_transactions() on the instance of the group that is the primary
	// on that server (the promoted instance), as the extension asks after a failover.
	RecoverPrepared(ctx context.Context, server string, group int32, db string) error
	// The calls of the planned switchover.
	Quiesce(ctx context.Context, server string, r QuiesceRequest) (QuiesceAnswer, error)
	Unquiesce(ctx context.Context, server string, r QuiesceRequest) error
	Mark(ctx context.Context, server string, r MarkRequest) (MarkAnswer, error)
	Verify(ctx context.Context, server string, r VerifyRequest) (VerifyAnswer, error)
	Resume(ctx context.Context, server string, r ResumeRequest) error
	// The calls of the planned switchover of the map group (the old leader's task talks to the bootstrap of the candidate).
	MapStart(ctx context.Context, server string, r MapStartRequest) error
	MapProof(ctx context.Context, server string, r MapProofRequest) error
	MapAbandon(ctx context.Context, server, reason string) error
	// MapRefusal is the refusal of the proof that the candidate reported to this server, the old leader's (local: the task runs here).
	MapRefusal(ctx context.Context, taskID int64) (string, bool)
}

// Node is a row of kingres.node.
type Node struct {
	ID    int32
	Group int32
	Host  string
	Port  int
	Role  string
}

// PromoteResult is the answer of one call of kingres.promote_node.
type PromoteResult struct {
	Status    string
	Term      int64
	Final     bool
	Unreached []int32
	// Blocked lists the prepared transactions in the way: gid, node and class
	// ("cluster" or "foreign").
	Blocked []Blocked
}

// Blocked is one entry of "blocked_by".
type Blocked struct {
	Gid   string `json:"gid"`
	Node  int32  `json:"node"`
	Class string `json:"class"`
}

// SQLError is an error of the extension with its SQLSTATE. The adapter of Catalog
// turns every error of a statement into one (Code "" for a broken connection or a
// client time-out: the call did not finish).
type SQLError struct {
	Code string
	Text string
}

func (e *SQLError) Error() string {
	if e.Code == "" {
		return "the call did not finish: " + e.Text
	}
	return e.Code + ": " + e.Text
}

// Incident is a row of kingres.check_cluster().
type Incident struct {
	Kind    string
	Message string
}

// Catalog is the extension's side, run on the map group primary on the leader's
// server: one control call per session, READ COMMITTED, one statement per call,
// with a named deadline derived from node_timeout (the adapter's business).
type Catalog interface {
	// Databases lists the databases that have a sharding catalog, the system database first (each has its own node ids).
	Databases(ctx context.Context) ([]string, error)
	// Nodes reads kingres.node of a database.
	Nodes(ctx context.Context, db string) ([]Node, error)
	// Status reads the catalog term and whether this catalog was superseded.
	Status(ctx context.Context, db string) (term int64, superseded bool, err error)
	// PromoteNode makes one call of kingres.promote_node(id).
	PromoteNode(ctx context.Context, db string, id int32) (PromoteResult, error)
	// RecoverPrepared calls kingres.recover_prepared_transactions() in database db (every database has a catalog of its own, F3-10) on the map
	// group primary.
	RecoverPrepared(ctx context.Context, db string) error
	// CheckCluster reads kingres.check_cluster() of database db.
	CheckCluster(ctx context.Context, db string) ([]Incident, error)
}

// StandbyRec is a standby as the leader records it (kingres_cp.standby).
type StandbyRec struct {
	ID          int64
	Group       int32
	Server      string
	InstanceID  int
	Port        int
	State       string
	CatalogNode int32 // 0: not registered
	Generation  int64
}

// GroupFacts are what the verdict needs of a group besides the positions.
type GroupFacts struct {
	// SyncStreaming says that, at the last check of the leader (not older than
	// MaxCheckAge), a synchronous standby streamed.
	SyncStreaming bool
	// LastKnownLsn and LastKnownAge are the primary's flush position at that check.
	LastKnownLsn string
	LastKnownAge time.Duration
	// PrimaryPlanned is the number of standbys the primary's server held in its plan at the last check (F3-02).
	PrimaryPlanned int
	// AsyncDatabases are the databases whose durability is async.
	AsyncDatabases []string
}

// Planner is what the task reads of the cluster's state and writes to its records
// outside the task's own tables.
type Planner interface {
	// Self is this server (the leader's).
	Self() string
	// Members are the servers that vote: the distinct hosts of kingres.node.
	Members(ctx context.Context) ([]string, error)
	// Live is this server's view of a server ("live", "suspect", "dead", "unknown").
	Live(ctx context.Context, server string) string
	// Standbys are the standbys the leader records for a group.
	Standbys(ctx context.Context, group int32) ([]StandbyRec, error)
	// Facts reads GroupFacts for a group.
	Facts(ctx context.Context, group int32) (GroupFacts, error)
	// PrimaryNames reads the standby names a reachable primary lists in
	// synchronous_standby_names (the standby ids); ok false when it could not be read.
	PrimaryNames(ctx context.Context, server string, group int32) (ids []int64, ok bool)
	// Preferred says that the server is the preferred (home) server of the group.
	Preferred(ctx context.Context, group int32, server string) bool
	// Ring is the distance of a server from another in the placement order (0 when
	// not known).
	Ring(from, to string) int
	// AfterPromotion records, in one transaction that proves the lease of the run,
	// that the instance of the standby is the primary of the group now, and sends the
	// plan to every server (design 3.3 F9). The old primary is recorded in the same transaction as a standby that is
	// waiting to be rebuilt (state rebuilding, diverged): the reconcile loop rebuilds it when its server answers
	// (design 6.1 and 6.2).
	AfterPromotion(ctx context.Context, group int32, promoted StandbyRec, newPrimaryServer string, newPrimaryPort int, old OldPrimaryRec) error
	// FlagRebuild marks a standby that cannot follow the new primary as needing a rebuild.
	FlagRebuild(ctx context.Context, standbyID int64) error
	// RebalanceRunning says that a rebalance is pending or running.
	RebalanceRunning(ctx context.Context) (bool, error)
	// SyncNamesOf reads, from the status of a server's agent, the synchronous list
	// of the primary of the group and the standbys that stream (ok false: unknown).
	SyncNamesOf(ctx context.Context, server string, group int32) (names string, planned int, ok bool)
	// Alert raises (or clears, with raise false) an alert of a failover about a group.
	Alert(ctx context.Context, kind, severity, subject, message string, raise bool)
	// Audit records an event of the failover.
	Audit(ctx context.Context, action string, group int32, details map[string]any)
	// LiveReplication reads pg_stat_replication and the flush position from the primary of the map group, which must answer now (M2). An error is a primary that cannot be
	// asked; the caller then uses the recorded facts.
	LiveReplication(ctx context.Context) (LiveReplication, error)
}

// LiveReplication is what the primary of the map group says of its standbys at the moment it is asked.
type LiveReplication struct {
	// SyncStreaming says that a standby that streams (state streaming) is synchronous or a quorum member (sync_state sync or quorum) right now.
	SyncStreaming bool
	// FlushLsn is pg_current_wal_flush_lsn() of the primary.
	FlushLsn string
	// Streaming is the number of standbys in state streaming; SentLsn and ReplayLsn are the highest positions reported.
	Streaming          int
	SentLsn, ReplayLsn string
}

// GroupRow is the record of one group of a run (kingres_cp.failover_group).
type GroupRow struct {
	Group         int32
	Seq           int
	State         string
	OldServer     string
	OldNode       int32
	NewServer     string
	NewNode       int32
	ChosenStandby int64
	TermBefore    *int64
	TermAfter     *int64
	Timeline      *int64
	LeaseWaitMs   *int
	Decision      []byte
	DataLoss      []byte
	Unreached     []byte
	Incidents     []byte
	OldPrimary    string
	// OldPrimaryReport is what the rebuild of the old primary found and did (JSON: method, divergence, message).
	OldPrimaryReport []byte
	Error            string
	StartedAt        *time.Time
	PromotedAt       *time.Time
	FinishedAt       *time.Time
}

// GroupSet is an update of GroupRow: only the set members are written.
type GroupSet struct {
	State         *string
	NewServer     *string
	NewNode       *int32
	ChosenStandby *int64
	TermAfter     *int64
	Timeline      *int64
	LeaseWaitMs   *int
	Decision      []byte
	DataLoss      []byte
	Unreached     []byte
	Incidents     []byte
	OldPrimary    *string
	Error         *string
	Promoted      bool
	// ClearPin withdraws the choice of the standby (the pinned instance is not a standby).
	ClearPin bool
	Finished bool
	Started  bool
}

// RunRow is the record of the run.
type RunRow struct {
	TaskID       int64
	ExpectedTerm int64
	Outcome      string
	Counted      bool
	// Options is the request as the last start or resume recorded it (nil: the input of the task).
	Options *Input
}

// Rows is the persistence of a run. Every write proves holder, epoch and lease of
// the task run in the context (ErrLeaseLost otherwise), so a superseded run records
// nothing. The cluster implementation is in rows_pg.go.
type Rows interface {
	// Begin writes the run and its group rows (idempotent: existing rows are kept).
	Begin(ctx context.Context, in Input, expectedTerm int64, groups []GroupRow) error
	Run(ctx context.Context) (RunRow, bool, error)
	Group(ctx context.Context, group int32) (GroupRow, bool, error)
	Set(ctx context.Context, group int32, u GroupSet) error
	// Pin records the choice of the standby for the group once: it never replaces a
	// choice that is recorded and answers the recorded one (design 3.3 F4).
	Pin(ctx context.Context, group int32, standby StandbyRec, decision, dataLoss []byte) (StandbyRec, bool, error)
	Event(ctx context.Context, group *int32, step, kind string, detail map[string]any) error
	// Warned says whether the run recorded something the operator should read (WarningKinds), or a group has an incident.
	Warned(ctx context.Context) (bool, error)
	// Finish writes the outcome of the run.
	Finish(ctx context.Context, outcome, errText string) error
	// Abandon closes the record of a run whose task the engine recorded as failed or
	// cancelled (the lease is gone by then, so nothing proves it: the task state does).
	Abandon(ctx context.Context, outcome, errText string) error
}

// ErrLeaseLost is returned by Rows when the run no longer holds the task: the step
// stands down quietly (the engine does the same on this error).
var ErrLeaseLost = task.ErrLeaseLost

// OldPrimaryRec is the old primary of a group as the failover knows it when it records the new primary: the server,
// the node the catalog kept for it, the port of that node and, when the server answered, the directory of its
// instance (0: the server rebuilds the directory it finds, oldprimary.go in internal/standby).
type OldPrimaryRec struct {
	Server     string
	Node       int32
	Port       int
	InstanceID int
}

// WarningKinds are the events that make a finished run "completedWithWarnings" (F3-15): a step that never fails but
// did not do all it could.
var WarningKinds = []string{"repointSkipped", "repointNotStreaming", "syncNamesPending", "preparedRemain", "oldPrimaryNotStopped",
	"recoverPreparedError", "recoverPreparedOnNewError", "checkClusterError", "standbysGoneSkipped"}

// CountAutomaticSQL adds one to the counter of automatic failovers when the run of $1 is automatic and was not counted yet (design 4.4).
const CountAutomaticSQL = `WITH c AS (UPDATE kingres_cp.failover_run SET counted = true WHERE task_id = $1 AND NOT counted AND trigger = 'automatic' RETURNING 1)
	INSERT INTO kingres_cp.cluster_setting (key, value)
	SELECT 'autoFailover.eventsSinceReset', to_jsonb(coalesce((SELECT (value #>> '{}')::int FROM kingres_cp.cluster_setting WHERE key = 'autoFailover.eventsSinceReset'), 0) + 1) FROM c
	ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`

// MapPlanned is what the old leader's task tells the candidate that starts the bootstrap of a planned switchover of the map group: the id of its task and
// the standby it chose (the same field names as journal.Planned).
type MapPlanned struct {
	TaskID    int64 `json:"taskId"`
	StandbyID int64 `json:"standbyId"`
	// Hard says that the old leader is stopped fenced (an immediate stop, its lease handed to the candidate) and not cleanly: there is no proof, the
	// candidate ranks the standbys after the stop and judges the loss with Facts, which the old leader read just before the stop (no check can follow it).
	Hard  bool      `json:"hard,omitempty"`
	Facts *MapFacts `json:"facts,omitempty"`
	// Target is the server the request named for the map group (empty: the best standby); the candidate ranks with it as the ordinary groups do.
	Target string `json:"target,omitempty"`
}

// MapFacts are the facts of the old leader's last check that a hard failover of the map group carries to the candidate (the field names of
// journal.PlannedFacts).
type MapFacts struct {
	SyncStreaming  bool     `json:"syncStreaming"`
	LastKnownLsn   string   `json:"lastKnownLsn,omitempty"`
	PrimaryPlanned int      `json:"primaryPlanned"`
	AsyncDatabases []string `json:"asyncDatabases,omitempty"`
}

// MapStartRequest is the body of POST /failover/map/start for a planned switchover (cmd/kingresd mapStartBody).
type MapStartRequest struct {
	Server        string      `json:"server"`
	RequestedBy   string      `json:"requestedBy"`
	AllowDataLoss bool        `json:"allowDataLoss"`
	Comment       string      `json:"comment"`
	Planned       *MapPlanned `json:"planned,omitempty"`
}

// MapProofRequest is the body of POST /failover/map/proof: how the old leader was left by its clean stop.
type MapProofRequest struct {
	Checkpoint   string `json:"checkpoint"`
	ClusterState string `json:"clusterState"`
}

// MapCheckpoint is the answer of GET /failover/map/checkpoint: how the stopped primary of the map group was left.
type MapCheckpoint struct {
	Checkpoint   string `json:"checkpoint"`
	ClusterState string `json:"clusterState"`
}

// MapAbandonResult is what giving up a bootstrap of the map group did (contract 0.18.0). ResumedServer names the server whose primary of the map group, held back
// for a planned switchover, was started again by it; it is empty when there was none to start.
type MapAbandonResult struct {
	Abandoned     bool   `json:"abandoned"`
	ResumedServer string `json:"resumedServerId,omitempty"`
}

// RunningUnknown says that the server could not tell whether an instance of the group runs: what the instance looks like is not known, so a leader
// that decides on it (the rebuild of an old primary) waits.
func (s State) RunningUnknown() bool {
	for _, i := range s.Instances {
		if i.Group == s.Group && i.RunningUnknown {
			return true
		}
	}
	return false
}
