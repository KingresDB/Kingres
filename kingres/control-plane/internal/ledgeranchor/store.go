// SPDX-License-Identifier: AGPL-3.0-only

// Package ledgeranchor is the wiring of the anchoring libraries (internal/anchor and its subpackages) into the
// control plane (design ledger-phase7.md, 6, 8.2 to 8.4 and 9, with the corrections of section 14; step 10 of
// the plan): the anchor targets and their credentials, the anchor records in kingres_cp, the jobs that enqueue,
// submit and upgrade them on the manager leader, the alerts of 6.6 and 6.9, and the operations of the REST
// contract that read and change them.
//
// What this package decides, and the libraries do not:
//
//   - Every write of anchoring state is a transaction of its own that begins with the term guard of the scheduler
//     (Store methods take the term), so a deposed leader writes nothing; a follower never submits (its scheduler
//     holds no term, so no job runs).
//   - A record is written before its first submission with the start of that submission (duty 1 of internal/anchor),
//     the target is called with no transaction open, and the result is written by a compare and swap on the version of
//     the row: a result that finds the row changed (a Retry, a delete, a second leader) is dropped, and the next pass
//     decides again. Every step is idempotent, so a kill at any point leaves a row that the next pass completes.
//   - The state of a record changes only through the methods of anchor.Record, and a record is anchored only on a
//     receipt that the library of its kind returned.
//   - Credentials live in a sealed file (CredStore), are read when a target is built, are held as anchor.Secret and
//     are removed from every text that leaves the package (Redactor); nothing returns them.
//   - Requests go to the hosts that the configuration of a target names and to no other (Egress).
package ledgeranchor

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/anchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Errors a caller tests with errors.Is.
var (
	// ErrNotFound: no such target, record or aggregate.
	ErrNotFound = errors.New("ledgeranchor: not found")
	// ErrExists: a target with this name exists.
	ErrExists = errors.New("ledgeranchor: a target with this name exists")
	// ErrConflict: the row changed since it was read (its version or revision is not the one given).
	ErrConflict = errors.New("ledgeranchor: the row changed since it was read")
	// ErrNoAggregate: the database has no aggregate log with a checkpoint (ledger_not_published).
	ErrNoAggregate = errors.New("ledgeranchor: the database has no published aggregate")
)

// TargetRow is a row of kingres_cp.ledger_anchor_target. It holds no secret.
type TargetRow struct {
	ID      int64
	Kind    string
	Name    string
	Enabled bool
	// EnabledSince is when the target was last switched on (nil while it is off).
	EnabledSince   *time.Time
	Config         json.RawMessage
	CredentialsSet bool
	// LastTest is the JSON of an api.LedgerAnchorTest, nil when never tested.
	LastTest  json.RawMessage
	Revision  int64
	CreatedBy string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Row is a row of kingres_cp.ledger_anchor: the record of the library and what the wiring keeps with it. A list
// of rows does not carry the note (Note is nil); Record and Due do.
type Row struct {
	anchor.Record
	// TargetName and Kind stay after the target was deleted (TargetGone).
	TargetName string
	Kind       string
	TargetGone bool
	Database   string
	// AggregateID is the identifier that ends the origin of the aggregate log; Origin is that origin.
	AggregateID string
	Origin      string
	Note        []byte
	// ErrorCode is the machine tag of the last error, Alert the kind of the alert that the failure raised.
	ErrorCode string
	Alert     string
	// Version is raised by every write of the row.
	Version   int64
	UpdatedAt time.Time
}

// Aggregate is the newest checkpoint of the aggregate log of a database.
type Aggregate struct {
	LogID    int64
	Database string
	Origin   string
	// AggregateID is the last element of the origin.
	AggregateID string
	Size        int64
	Note        []byte
	SignedAt    time.Time
}

// Settings are the anchoring settings of the cluster (kingres_cp.ledger_cluster).
type Settings struct {
	Interval time.Duration
	Minimum  int
	Required bool
}

// Enqueue is what the enqueue job asks of the store for one target and one aggregate checkpoint.
type Enqueue struct {
	Target Target
	Agg    Aggregate
	Now    time.Time
}

// Target names a target in an Enqueue.
type Target struct {
	ID   int64
	Name string
	Kind string
}

// Anchored is the newest anchored record of one (target, log).
type Anchored struct {
	TargetID     int64
	LogID        int64
	Size         int64
	AnchoredAt   time.Time
	EvidenceTime *time.Time
}

// Filter selects records for listLedgerAnchors.
type Filter struct {
	Database string
	TargetID int64
	// State is "" for every state; the contract's states pending, submitted, anchored, failed, superseded and expired.
	// A record in the library's state claimed is a "submitted" one in the API.
	State  string
	Before int64
	Limit  int
}

// Store keeps targets and records. Every write names the term of the leader and begins with the term guard.
type Store interface {
	Targets(ctx context.Context) ([]TargetRow, error)
	Target(ctx context.Context, id int64) (TargetRow, error)
	// CreateTarget inserts a target (its ID and times are set by the store); ErrExists for a name in use.
	CreateTarget(ctx context.Context, term scheduler.Term, t TargetRow) (TargetRow, error)
	// UpdateTarget replaces name, enabled, config, credentials_set and the last test of the row whose revision is
	// expect and raises the revision (ErrConflict otherwise, ErrExists for a name in use, ErrNotFound).
	UpdateTarget(ctx context.Context, term scheduler.Term, t TargetRow, expect int64) (TargetRow, error)
	// DeleteTarget removes the target; its records stay with the name (target_id null) and are never worked on.
	DeleteTarget(ctx context.Context, term scheduler.Term, id int64) error
	// SetLastTest stores the result of a test (it does not raise the revision).
	SetLastTest(ctx context.Context, term scheduler.Term, id int64, test json.RawMessage) error

	Settings(ctx context.Context) (Settings, error)
	// Aggregates lists the newest checkpoint of every aggregate log that has one.
	Aggregates(ctx context.Context) ([]Aggregate, error)
	// AggregateOf returns the newest checkpoint of the aggregate log of a database: ErrNotFound when the database
	// has no aggregate log, ErrNoAggregate when the log has no checkpoint.
	AggregateOf(ctx context.Context, db string) (Aggregate, error)
	// FirstAfter is the time the first checkpoint of a log larger than size was signed (false when none is).
	FirstAfter(ctx context.Context, logID, size int64) (time.Time, bool, error)

	Records(ctx context.Context, f Filter) ([]Row, error)
	// Record returns one row with its note.
	Record(ctx context.Context, id int64) (Row, error)
	// Enqueue creates the record of the checkpoint for the target when there is none, and makes every older record of
	// the same (target, log) that is still pending superseded, in one transaction. It returns the record and whether
	// it was created. A checkpoint older than the newest record of the (target, log) creates nothing and returns that
	// newest record.
	Enqueue(ctx context.Context, term scheduler.Term, e Enqueue) (Row, bool, error)
	// Begin makes the start of the first submission of a pending record durable: when SubmitStart is not set it is set
	// to now, in the row, and the row is returned (with its note). It refuses a record that is not pending
	// (ErrConflict). It is called before the first Submit call for the key and never changes a start that is set.
	Begin(ctx context.Context, term scheduler.Term, id int64, now time.Time) (Row, error)
	// Apply writes the result of a job: the mutable fields of the record, the error code and the alert, when the row
	// still has the version expect (ErrConflict otherwise). A record that becomes anchored marks its checkpoint.
	Apply(ctx context.Context, term scheduler.Term, r Row, expect int64) error
	// Due lists the records of enabled targets that wait for work at now: pending ones to submit, submitted and
	// claimed ones to upgrade (the library decides what is due; this is a first cut by next_attempt_at), oldest
	// first, at most limit, with their notes.
	Due(ctx context.Context, now time.Time, limit int) ([]Row, error)
	// NewestAnchored lists the newest anchored record of every (target, log).
	NewestAnchored(ctx context.Context) ([]Anchored, error)
	// Failures lists the failed and expired records of live targets that carry an alert, without notes.
	Failures(ctx context.Context) ([]Row, error)
	// DBTime is the clock of the database.
	DBTime(ctx context.Context) (time.Time, error)
}
