// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

// Package ledgerpub is the storage and the serving of the publication of the ledger (design ledger-phase7.md, 4.4 to
// 4.7 and 8.2, with the corrections of section 14): the logs, their tiles, bundles, frontiers and checkpoints in
// kingres_cp, the digests of every group, and the HTTP handler that serves a publication in the layout of
// tlog-tiles.
//
// Every write of ledger state is made under the term of the manager leader (internal/scheduler): the first statement
// of its transaction is Term.Guard, so a deposed leader that wakes up writes nothing. Nothing here signs; the
// collector (internal/ledgerround) signs and hands the notes to PutCheckpoint, which refuses a note for a tree that
// is not the log's stored tree (never a smaller one, never a second root for a size) and never replaces a published
// tile or checkpoint.
package ledgerpub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Kinds and states of a log row.
const (
	KindChain     = "chain"
	KindAggregate = "aggregate"

	StateActive   = "active"
	StateDiverged = "diverged"
	StateClosed   = "closed"
)

// Errors a caller tests with errors.Is.
var (
	// ErrNotFound: no such log, tile, checkpoint or digest.
	ErrNotFound = errors.New("ledgerpub: not found")
	// ErrConflict: the write was refused because it would replace what was published (another root for a size, a
	// smaller tree, a note for a tree that is not the stored one), or because the keys changed after the signers
	// were loaded (ErrKeysChanged). Nothing was written.
	ErrConflict = errors.New("ledgerpub: the write conflicts with what is stored")
	// ErrBusy: the write met a concurrent writer of the same row (a deadlock between two share locks that both want it, a failed serialization). Nothing
	// was written and nothing is wrong: the next round makes the write again. It is not a conflict, and never a reason to stop a log.
	ErrBusy = errors.New("ledgerpub: the write met a concurrent writer, try again")
	// ErrKeysChanged: the key set changed after the signer was loaded (a key was retired or revoked, a key was
	// added): the checkpoint is not stored, the next round signs again with the keys of the new set. It is how a
	// signature and the retirement of a key exclude each other (correction 128, 129).
	ErrKeysChanged = errors.New("ledgerpub: the key set changed after the checkpoint was signed")
)

// LogRow is a row of kingres_cp.ledger_log.
type LogRow struct {
	ID            int64
	Kind          string
	Database      string // empty for none
	ShardGroupID  *int32
	ChainID       []byte
	Epoch         int
	Origin        string
	State         string
	Detail        string
	Size          int64
	Root          []byte
	Frontier      []byte
	HeadBlockHash []byte
	UpdatedAt     time.Time
	ClosedAt      *time.Time
	RetiredInSeq  *int64
	PendingEntry  []byte
	UnreadSince   *time.Time
	SignedAt      *time.Time
}

// Checkpoint is a row of kingres_cp.ledger_checkpoint.
type Checkpoint struct {
	LogID    int64
	Size     int64
	Root     []byte
	Note     []byte
	SignedAt time.Time
}

// DigestView is a row of kingres_cp.ledger_digest_view: the digest of every group of a database.
type DigestView struct {
	ID            int64
	Database      string
	GeneratedAt   time.Time
	GeneratedBy   string
	HashVersion   int
	Chains        []byte // the JSON array of the chains of the digest, as the API shows it
	Complete      bool
	Uncovered     []int32
	AggregateSize *int64
	AggregateHash []byte
}

// Object is a file of a publication.
type Object struct {
	Data []byte
	// Mutable is true for a checkpoint, a listing and the key ring: a short cache. A tile, a bundle and an event are
	// immutable.
	Mutable bool
	// ContentType is the media type to serve.
	ContentType string
}

// Store keeps the logs. All writes name the term of the leader.
type Store interface {
	// Logs lists every log row, ordered by id.
	Logs(ctx context.Context) ([]LogRow, error)
	// LogByOrigin returns the log with this origin, ErrNotFound when there is none.
	LogByOrigin(ctx context.Context, origin string) (LogRow, error)
	// CreateLog inserts a log row (size 0, the empty frontier, state active) or returns the one that exists with this
	// origin (a repeated step); a row for the same chain or database that is open under another origin is
	// ErrConflict.
	CreateLog(ctx context.Context, term scheduler.Term, row LogRow) (LogRow, error)
	// Tiles returns the tile store of a log: PutTile begins with the term guard.
	Tiles(logID int64, term scheduler.Term) ledgerlog.Store
	// SaveFrontier stores the frontier with its size and root, the head block hash of a chain log (nil for the aggregate)
	// and clears pending (the leaf of the aggregate that was being appended). It refuses a smaller size than the stored
	// one and another root for the stored size (ErrConflict), and a log that is not active.
	SaveFrontier(ctx context.Context, term scheduler.Term, logID, size int64, root, frontier, headBlockHash []byte) error
	// PutPending stores the leaf an aggregate round is about to append (a crash between its tiles and its frontier appends
	// the same bytes again).
	PutPending(ctx context.Context, term scheduler.Term, logID int64, entry []byte) error
	// PutCheckpoint stores the signed note of a log at its stored size and root (ErrConflict for any other, or for a note
	// that is stored with other content), after checking that the key set is still at keysRevision (ErrKeysChanged).
	// markFirst sets first_checkpoint_at of the cluster when it is not set, which locks the origin prefix and records
	// the prefix in force.
	PutCheckpoint(ctx context.Context, term scheduler.Term, c Checkpoint, keysRevision int64, markFirst bool, prefix string) error
	// ReplaceNote replaces the note of a stored checkpoint by one that adds signature lines to the same text (a
	// re-signing at a key rotation); the root must equal the stored root.
	ReplaceNote(ctx context.Context, term scheduler.Term, logID, size int64, note []byte, keysRevision int64) error
	// Checkpoint returns the checkpoint at a size, ErrNotFound when there is none.
	Checkpoint(ctx context.Context, logID, size int64) (Checkpoint, error)
	// LatestCheckpoint returns the newest checkpoint of a log, ErrNotFound when there is none.
	LatestCheckpoint(ctx context.Context, logID int64) (Checkpoint, error)
	// Checkpoints lists the checkpoints of a log below before (0: all), newest first, at most limit.
	Checkpoints(ctx context.Context, logID, before int64, limit int) ([]Checkpoint, error)
	// SetState sets the state and the detail of a log. diverged and closed are final for a round: only the
	// acknowledgement of a later step makes a diverged log active again (it never does here).
	SetState(ctx context.Context, term scheduler.Term, logID int64, state, detail string) error
	// SetRetired records the aggregate leaf in which a closed chain log was flagged retired.
	SetRetired(ctx context.Context, term scheduler.Term, logID, seq int64) error
	// SetUnread records (since not nil) or clears when a group of the log became unreadable.
	SetUnread(ctx context.Context, term scheduler.Term, logID int64, since *time.Time) error
	// PutDigest stores a digest view and returns its id.
	PutDigest(ctx context.Context, term scheduler.Term, v DigestView) (int64, error)
	// Digest returns one view; Digests lists the views of a database newest first below before (0: all).
	Digest(ctx context.Context, database string, id int64) (DigestView, error)
	Digests(ctx context.Context, database string, before int64, limit int) ([]DigestView, error)
	// LastDigestAt is the time of the newest digest view of a database (zero when none).
	LastDigestAt(ctx context.Context, database string) (time.Time, error)
	// Object reads a file below the publication prefix: the origin of the log and the path below it, or
	// ("", "keyring.json") and ("", "events/<seq>").
	Object(ctx context.Context, origin, rest string) (Object, error)
}

// emptyRoot is the root of the empty tree as a slice (the root a new log row starts with).
func emptyRoot() []byte { r := ledgerlog.EmptyRoot(); return r[:] }

// sameText reports whether a note adds signatures to the text of the stored one and changes nothing else: ReplaceNote's rule (P8-06).
func sameText(old, note []byte) error {
	o, err := ledgerlog.ParseNote(old)
	if err != nil {
		return fmt.Errorf("%w: the stored note does not parse", ErrConflict)
	}
	n, err := ledgerlog.ParseNote(note)
	if err != nil {
		return fmt.Errorf("%w: the new note does not parse", ErrConflict)
	}
	if !bytes.Equal(o.Text, n.Text) {
		return fmt.Errorf("%w: the text of the note is not the stored checkpoint's", ErrConflict)
	}
	return nil
}

// LogsJSON is the file logs.json of a publication (design 4.6, correction 178): the list of its logs, so that a client that follows a
// publication (kingres-verify bundle fetch) need not be told every origin. The text is one JSON object on one line, a newline at the end,
// the members in this order, the logs in the order of their creation:
//
//	{"format":1,"logs":[{"kind":"aggregate","origin":"<origin>","state":"active","size":3},...]}
//
// kind is "chain" or "aggregate"; origin is the origin of the log (its checkpoint is at <origin without the prefix>/checkpoint); state is
// "active", "diverged" or "closed"; size is the size of the stored tree, which can be larger than that of the latest checkpoint for a moment.
// A log without a signed checkpoint is listed too (its checkpoint answers 404). The file says nothing a client cannot find in the files it
// lists, and no name of a database: the origin of a chain log is made of its chain id.
func LogsJSON(rows []LogRow) []byte {
	type entry struct {
		Kind   string `json:"kind"`
		Origin string `json:"origin"`
		State  string `json:"state"`
		Size   int64  `json:"size"`
	}
	out := struct {
		Format int     `json:"format"`
		Logs   []entry `json:"logs"`
	}{Format: 1, Logs: []entry{}}
	for _, r := range rows {
		out.Logs = append(out.Logs, entry{Kind: r.Kind, Origin: r.Origin, State: r.State, Size: r.Size})
	}
	b, _ := json.Marshal(out)
	return append(b, '\n')
}
