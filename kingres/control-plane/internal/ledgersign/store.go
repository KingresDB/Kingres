// SPDX-License-Identifier: AGPL-3.0-only

// Package ledgersign is the daemon side of the signing keys of the ledger
// (design ledger-phase7.md, 3.2 to 3.4, 6.7, 8.2 to 8.4, 9.1 to 9.3, and the
// corrections of section 14). It wires internal/ledgerkeys and
// internal/ledgerlog, which hold the cryptography and the state machine as
// pure libraries, to what the daemon owns:
//
//   - the storage of the key set, the events and the cluster row in
//     kingres_cp (Store), written only under the term of the manager leader
//     (internal/scheduler) and under a revision number, so a deposed leader
//     cannot write and two changes cannot start from one stored set;
//   - the private key files on the servers that may become the leader
//     (Keyring, Agent) and their distribution (the internal calls
//     ledger/key/install, remove and list, task ledger.key.distribute);
//   - the tasks ledger.key.create, ledger.key.rotate, ledger.key.retire,
//     ledger.key.revoke and ledger.key.distribute, each step idempotent;
//   - the signed events of the key history, the trust file the daemon
//     publishes and the checks that follow every load (Validate, the trust
//     file compared with the one published at the last change, the clock);
//   - the operations getLedgerSettings, updateLedgerSettings,
//     getLedgerSigning, createLedgerSigningKey, retireLedgerSigningKey,
//     revokeLedgerSigningKey and getLedgerTrustFile.
//
// A private key, a seed or a passphrase never appears in a log line, a task
// row, an audit entry, an API answer, an error text or a store row: the only
// places a seed exists are the key file on the holders and the memory of
// internal/ledgerkeys.
package ledgersign

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// ErrConflict means the stored state changed after it was read (another change
// won the revision, or the database found a deadlock between two changes). The
// caller reads again and decides again.
var ErrConflict = errors.New("ledgersign: the stored state changed meanwhile")

// ClusterRow is the singleton row kingres_cp.ledger_cluster without the term and
// the revision, which the store keeps.
type ClusterRow struct {
	// ClusterID is nil until the first key is created.
	ClusterID []byte
	// OriginPrefix is "" for the default of the daemon.
	OriginPrefix        string
	PublicURL           *string
	PublicationEnabled  bool
	PublicationInterval int
	AnchorInterval      int
	AnchorMinimum       int
	AnchorRequired      bool
	FirstCheckpointAt   *time.Time
	// TrustFile is the trust file published at the last change of the key set; nil when none.
	TrustFile []byte
	// Holders are the servers that were given the private keys at the last distribution.
	Holders       []string
	BackupAdvised bool
	Discontinuity bool
}

// KeyRow is a key of the set with what the daemon adds to it.
type KeyRow struct {
	ledgerkeys.Key
	// Mode is "file" (the private key is a file on the holders) or "command".
	Mode string
	// TaskID is the task that made the key (0 when none).
	TaskID int64
}

// EventRow is a signed event of the key history.
type EventRow struct {
	Seq       int64
	Type      string
	Subject   string
	Note      []byte
	CreatedAt time.Time
}

// Snapshot is the stored state, read in one transaction.
type Snapshot struct {
	Cluster  ClusterRow
	Term     int64
	Revision int64
	Keys     []KeyRow
	Events   []EventRow
}

// Set returns the key set of the snapshot.
func (s Snapshot) Set() ledgerkeys.Set {
	set := make(ledgerkeys.Set, 0, len(s.Keys))
	for _, k := range s.Keys {
		key := k.Key
		key.Public = append(ed25519.PublicKey(nil), key.Public...)
		set = append(set, key)
	}
	return set
}

// Change is one atomic change of the stored state.
type Change struct {
	// Cluster, when not nil, replaces the cluster row (its term and revision are the store's).
	Cluster *ClusterRow
	// Keys, when not nil, is the complete key set after the change: rows are inserted or updated; a
	// fingerprint that is not in the list is left alone (the daemon never deletes a key row).
	Keys []KeyRow
	// Events are appended; the first must have the next sequence number.
	Events []EventRow
	// CheckpointBound, when not nil, makes the store refuse the change (ErrConflict) if any stored checkpoint has a size above it,
	// the check being made after the store holds the row of the cluster: the bound of a retired key must not be smaller than a tree
	// the collector signed with it (corrections 128 and 129). A checkpoint put at the same time either commits first, and the change is
	// refused and decided again with the new bound, or waits and finds the key set changed (ledgerpub.ErrKeysChanged).
	CheckpointBound *uint64
	// BoundKey, when not nil, narrows the check of CheckpointBound to the checkpoints that carry a signature of this key (P8-02): the
	// refusal of a retirement is about checkpoints signed by the key that retires, above its bound, and not about the largest tree of the
	// cluster, which the other key keeps growing after the key-retired event.
	BoundKey ed25519.PublicKey
}

// Store is the storage of the key set. Every write names the term of the leader
// and the revision it read.
type Store interface {
	// Load reads the whole state.
	Load(ctx context.Context) (Snapshot, error)
	// Commit applies a change if the stored term is term (scheduler.ErrDeposed otherwise) and the
	// stored revision is expect (ErrConflict otherwise); it raises the revision by one.
	Commit(ctx context.Context, term scheduler.Term, expect int64, ch Change) error
	// DBTime is the clock of the database: the second source the daemon compares its own clock with.
	DBTime(ctx context.Context) (time.Time, error)
	// LogOrigins are the origins of every log that exists (the key id of a new key is checked under each).
	LogOrigins(ctx context.Context) ([]string, error)
	// MaxCheckpointSize is the largest size of any stored checkpoint of any log (0 when none): the
	// bound of a retired key (ledgerkeys.AtMost) is at least this.
	MaxCheckpointSize(ctx context.Context) (uint64, error)
	// MaxCheckpointSizeSignedBy is the largest size of a stored checkpoint that carries a signature of the key (0 when none).
	MaxCheckpointSizeSignedBy(ctx context.Context, pub ed25519.PublicKey) (uint64, error)
}
