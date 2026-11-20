// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

// Keys is what the collector needs of the signing keys (implemented by *ledgersign.Service).
type Keys interface {
	Publication(ctx context.Context) (ledgersign.PubContext, error)
	AppendEvent(ctx context.Context, typ, subject string, fields []ledgersign.EventField) (int64, error)
}

// Database is a database whose ledger the collector reads.
type Database struct {
	Name string
	// DigestInterval is the setting digestIntervalSeconds (0 for none: no scheduled digest).
	DigestInterval time.Duration
}

// Config configures a Collector.
type Config struct {
	Store  ledgerpub.Store
	Source Source
	Keys   Keys
	// Databases lists the databases that have the ledger (ledger mode other than off).
	Databases func(ctx context.Context) ([]Database, error)
	// Clock is the clock of this server (default time.Now). The time of an aggregate leaf and of an alert is read here.
	Clock func() time.Time
	// Alert raises or clears an alert (kind, severity, subject in the stored form "database:<name>|group:<n>").
	Alert func(ctx context.Context, kind, severity, subject, message string, raise bool)
	Log   *slog.Logger
	// DatabaseExists tells whether a database exists in the cluster (nil: the databases of Databases only). It decides between 404 and an empty answer
	// for a database that has no digest, no aggregate and no log.
	DatabaseExists func(ctx context.Context, db string) (bool, error)
	// TermWait is how long a request waits for the term of a new leader (default 20 seconds): the scheduler of a leader that has just been
	// elected waits one lease margin before it takes the term, and a digest asked in that time is not refused at once.
	TermWait time.Duration
	// Listener reports the HTTP listener of the publication (configured address, TLS, running); nil when this server has none.
	Listener func() (address string, tlsOn, running bool)
	// MaxBatch is the most new blocks one round appends to a chain log (default 9999, at most ledgerlog.MaxBatch less one).
	MaxBatch int
	// MissingFor is how long every group must answer that a database does not exist, without a single answer in between, before the
	// collector ends the publication of its chains (default 30 minutes, P8-03: time, not rounds; the database must also be absent from
	// the system instance when DatabaseExists is set).
	MissingFor time.Duration
	// Published is the record of what was published, outside the system database (P8-01); nil means none is kept (tests of other things).
	// PullState asks the other holders of the keys for their copy (the first start of a server that has none).
	Published PublishedState
	PullState func(ctx context.Context) (ledgerstate.State, bool)
}

// Collector is the publisher: the only writer of the logs. It is safe for concurrent use; rounds of one database exclude each other (one
// writer per log, the first duty of ledgerlog).
type Collector struct {
	cfg Config
	log *slog.Logger

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	// state of the failure counters, in memory: a restart begins again (the alerts are raised again after two more rounds)
	signFails    int
	signErr      error
	missingSince map[string]time.Time
	tries        map[string]time.Time
	lastRound    time.Time
}

// New builds a collector.
func New(cfg Config) (*Collector, error) {
	if cfg.Store == nil || cfg.Source == nil || cfg.Keys == nil {
		return nil, errors.New("ledgerround: Store, Source and Keys are required")
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	if cfg.TermWait <= 0 {
		cfg.TermWait = 20 * time.Second
	}
	if cfg.MissingFor <= 0 {
		cfg.MissingFor = DefaultMissingFor
	}
	if cfg.MaxBatch <= 0 || cfg.MaxBatch >= MaxEntries {
		cfg.MaxBatch = MaxEntries - 1
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Collector{cfg: cfg, log: log, locks: map[string]*sync.Mutex{}, missingSince: map[string]time.Time{}, tries: map[string]time.Time{}}, nil
}

func (c *Collector) lock(db string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	l := c.locks[db]
	if l == nil {
		l = new(sync.Mutex)
		c.locks[db] = l
	}
	return l
}

func (c *Collector) alert(ctx context.Context, kind, severity, db string, group int32, msg string, raise bool) {
	if c.cfg.Alert == nil {
		return
	}
	subject := "database:" + db
	if group >= 0 {
		subject += "|group:" + strconv.Itoa(int(group))
	}
	c.cfg.Alert(context.WithoutCancel(ctx), kind, severity, subject, msg, raise)
}

// Alert kinds the collector raises (contract 0.11.0).
const (
	AlertDiverged   = "ledgerChainDiverged"
	AlertUnread     = "ledgerGroupUnread"
	AlertDigestLate = "ledgerDigestOverdue"
)

// Errors a caller tests with errors.Is.
var (
	// ErrNotLeader: this server holds no term (a follower, or a leader that waits out the lease margin).
	ErrNotLeader = errors.New("ledgerround: this server does not hold the leader's term")
	// ErrNoGroup: no shard group answered.
	ErrNoGroup = errors.New("ledgerround: no shard group answered")
	// ErrTooManyChains: a database with more chains than an aggregate leaf holds (512).
	ErrTooManyChains = errors.New("ledgerround: the database has more chains than an aggregate holds")
	// ErrKeysChanged: the key set changed while the round signed: nothing wrong, the next round signs again.
	ErrKeysChanged = ledgerpub.ErrKeysChanged
)

// GroupResult is what a round found in one shard group.
type GroupResult struct {
	Group Group
	// Resp is the answer when the group was read (Err nil); Empty is true for a ledger with no block.
	Resp ReadResponse
	// Err is why the group was not read; nil when it was. A group that holds no ledger (NoLedger) or no database is "not read" too: the
	// digest does not cover it.
	Err *ReadError
	// Log is the chain log of the group after the round (nil when there is none: empty ledger, or publication off).
	Log *ledgerpub.LogRow
	// Diverged is true when the log of this chain is stopped (in this round or before).
	Diverged bool
}

// Covered tells whether the group was read.
func (g GroupResult) Covered() bool { return g.Err == nil }

// Result is the outcome of one round of one database.
type Result struct {
	Database string
	// Term is the term of the leader under which the round ran.
	Term   scheduler.Term
	Groups []GroupResult
	// Published is true when the chain logs were updated and signed in this round.
	Published bool
	// SignErr is why nothing was signed, when publication is on and the keys could not sign.
	SignErr error
	// AggregateSize and AggregateHash are the checkpoint of the aggregate log after the round (0 and nil when there is none yet).
	AggregateSize int64
	AggregateHash []byte
	// KeysChanged is true when a checkpoint was not stored because the key set changed meanwhile.
	KeysChanged bool
}

// Uncovered lists the groups that were not read.
func (r Result) Uncovered() []int32 {
	out := []int32{}
	for _, g := range r.Groups {
		if !g.Covered() {
			out = append(out, g.Group.ID)
		}
	}
	return out
}

// Answered tells whether at least one group was read.
func (r Result) Answered() bool {
	for _, g := range r.Groups {
		if g.Covered() {
			return true
		}
	}
	return false
}

func hexOf(b []byte) string { return hex.EncodeToString(b) }

func hash32(b []byte) (ledgerlog.Hash, error) {
	var h ledgerlog.Hash
	if len(b) != len(h) {
		return h, fmt.Errorf("ledgerround: a hash of %d bytes", len(b))
	}
	copy(h[:], b)
	return h, nil
}

// Round runs one round of database db: every group is read with op (OpHead or OpDigest), the chain logs are extended and signed when
// publication is on and the keys can sign, and the aggregate is updated. A group that cannot be read does not stop the round: it is
// in the result as not covered, and its members are stale.
func (c *Collector) Round(ctx context.Context, db, op string) (Result, error) {
	l := c.lock(db)
	l.Lock()
	defer l.Unlock()
	return c.round(ctx, db, op, nil)
}

func (c *Collector) round(ctx context.Context, db, op string, only map[int32]bool) (Result, error) {
	res := Result{Database: db}
	pc, err := c.awaitTerm(ctx)
	if err != nil {
		return res, err
	}
	term := pc.Term
	res.Term = term
	groups, err := c.cfg.Source.Groups(ctx, db)
	if err != nil {
		return res, fmt.Errorf("listing the shard groups of %s: %w", db, err)
	}
	for _, g := range groups {
		if only != nil && !only[g.ID] {
			continue
		}
		gr := GroupResult{Group: g}
		resp, err := c.cfg.Source.Read(ctx, g, ReadRequest{Op: op, Database: db})
		if err != nil {
			var re *ReadError
			if !errors.As(err, &re) {
				re = &ReadError{Group: g.ID, Transport: true, Message: err.Error()}
			}
			gr.Err = re
		} else {
			gr.Resp = resp
		}
		res.Groups = append(res.Groups, gr)
	}
	c.noteMissing(db, res)
	if !pc.Enabled {
		return res, nil
	}
	if pc.SignErr != nil {
		res.SignErr = pc.SignErr
		c.signFailed(ctx, pc.SignErr, true)
		return res, nil
	}
	if len(pc.ClusterID) != 32 {
		res.SignErr = ledgersign.ErrNoKeys
		return res, nil
	}
	if err := c.checkPublished(ctx); err != nil {
		if errors.Is(err, ErrPublishedState) {
			res.SignErr = err
			return res, nil
		}
		return res, err
	}
	logs, err := c.cfg.Store.Logs(ctx)
	if err != nil {
		return res, err
	}
	for i := range res.Groups {
		gr := &res.Groups[i]
		if err := c.chain(ctx, &pc, term, db, gr, logs, &res); err != nil {
			if errors.Is(err, ErrKeysChanged) {
				res.KeysChanged = true
				return res, nil
			}
			return res, err
		}
	}
	if err := c.aggregate(ctx, pc, term, db, &res); err != nil {
		if errors.Is(err, ErrKeysChanged) {
			res.KeysChanged = true
			return res, nil
		}
		return res, err
	}
	c.signFailed(ctx, nil, false)
	res.Published = true
	c.reconcile(ctx, term, db, &res)
	return res, nil
}

// signFailed counts the rounds in which no checkpoint could be signed (SignFailure says so after two). The alert ledgerSignerUnavailable has one
// owner, the check job of the key service, which asks SignFailure besides looking at the key set (correction 134).
func (c *Collector) signFailed(_ context.Context, err error, failed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if failed && !errors.Is(err, ledgersign.ErrNotLeader) {
		c.signFails++
		c.signErr = err
		return
	}
	c.signFails, c.signErr = 0, nil
}

// SignFailure is "" while checkpoints are signed, and the reason once two rounds in a row could not sign one.
func (c *Collector) SignFailure() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.signFails >= 2 && c.signErr != nil {
		return "No checkpoint of the ledger could be signed for two rounds: " + c.signErr.Error()
	}
	return ""
}

// noteMissing remembers since when every group has been answering that the database does not exist (a single answer from any group ends it).
func (c *Collector) noteMissing(db string, res Result) {
	all := len(res.Groups) > 0
	for _, g := range res.Groups {
		if g.Err == nil || g.Err.Code != CodeNoDatabase {
			all = false
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if all {
		if _, ok := c.missingSince[db]; !ok {
			c.missingSince[db] = c.cfg.Clock()
		}
	} else {
		delete(c.missingSince, db)
	}
}

// reconcile raises and clears the alerts about the groups of a database: unread (warning after the publication interval, critical after an
// hour) and diverged.
func (c *Collector) reconcile(ctx context.Context, term scheduler.Term, db string, res *Result) {
	now := c.cfg.Clock()
	for i := range res.Groups {
		gr := &res.Groups[i]
		if gr.Log == nil {
			continue
		}
		g := gr.Group.ID
		switch {
		case gr.Err != nil:
			since := now
			if gr.Log.UnreadSince != nil {
				since = *gr.Log.UnreadSince
			} else if err := c.cfg.Store.SetUnread(ctx, term, gr.Log.ID, &since); err != nil {
				c.log.Warn("recording an unreadable group", "database", db, "group", g, "error", err)
			}
			age := now.Sub(since)
			if age >= time.Hour {
				c.alert(ctx, AlertUnread, "critical", db, g, "The ledger chain of shard group "+strconv.Itoa(int(g))+" has not been read for an hour: "+gr.Err.Message, true)
			} else if age >= time.Minute {
				c.alert(ctx, AlertUnread, "warning", db, g, "The ledger chain of shard group "+strconv.Itoa(int(g))+" could not be read: "+gr.Err.Message, true)
			}
		default:
			if gr.Log.UnreadSince != nil {
				if err := c.cfg.Store.SetUnread(ctx, term, gr.Log.ID, nil); err != nil {
					c.log.Warn("clearing an unreadable group", "database", db, "group", g, "error", err)
				}
			}
			c.alert(ctx, AlertUnread, "warning", db, g, "", false)
		}
	}
}

// diverge stops a chain log: the state becomes diverged with the reason, the alert ledgerChainDiverged is raised (critical), and nothing
// is signed for the log from now on. It never touches a tile or a checkpoint.
func (c *Collector) diverge(ctx context.Context, term scheduler.Term, db string, group int32, l ledgerpub.LogRow, why string) error {
	c.log.Error("ledger chain diverged", "database", db, "group", group, "origin", l.Origin, "size", l.Size, "reason", why)
	if err := c.cfg.Store.SetState(ctx, term, l.ID, ledgerpub.StateDiverged, why); err != nil {
		return err
	}
	c.alert(ctx, AlertDiverged, "critical", db, group, "The chain of shard group "+strconv.Itoa(int(group))+" no longer extends its published log: "+why, true)
	return nil
}

func microsSince2000(t time.Time) int64 {
	return t.UTC().Sub(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)).Microseconds()
}

// ledgerlogTime is the time of a count of microseconds since 2000-01-01 UTC, as the aggregate leaf and the ledger write it.
func ledgerlogTime(us int64) time.Time {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(us) * time.Microsecond)
}

// awaitTerm reads the publication context, waiting up to TermWait for the term of a leader that has just been elected. A server that never holds one
// (a follower) answers ErrNotLeader after the wait; the REST operations that reach here were forwarded to the leader, so the wait is the margin only.
func (c *Collector) awaitTerm(ctx context.Context) (ledgersign.PubContext, error) {
	deadline := time.Now().Add(c.cfg.TermWait)
	for {
		pc, err := c.cfg.Keys.Publication(ctx)
		if err != nil {
			return pc, err
		}
		if pc.Term != 0 {
			return pc, nil
		}
		if time.Now().After(deadline) {
			return pc, ErrNotLeader
		}
		select {
		case <-ctx.Done():
			return pc, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// vanished decides whether the publication of the chains of a database ends because the database is gone (design 148 with P8-03). It is the
// test of time, of the catalog and of the alert: every group has answered "no database" without a single other answer for MissingFor, the
// database is not in the system instance either (DatabaseExists, when set), and from the first answer on a critical alert says that the end
// of the publication is near, so that an operator who restores the database in the meantime sees it before anything irreversible happens.
func (c *Collector) vanished(ctx context.Context, db string) bool {
	c.mu.Lock()
	since, missing := c.missingSince[db]
	c.mu.Unlock()
	if !missing {
		c.alert(ctx, AlertUnread, "critical", db, -1, "", false)
		return false
	}
	left := c.cfg.MissingFor - c.cfg.Clock().Sub(since)
	if c.cfg.DatabaseExists != nil {
		exists, err := c.cfg.DatabaseExists(ctx, db)
		if err != nil || exists {
			// the system instance has the database (or cannot say): the groups are being rebuilt or restored; nothing ends
			c.alert(ctx, AlertUnread, "critical", db, -1, "Every shard group answers that database "+db+" does not exist, but the system instance still has it: the publication of its ledger waits.", true)
			return false
		}
	}
	if left > 0 {
		c.alert(ctx, AlertUnread, "critical", db, -1, fmt.Sprintf("Every shard group answers that database %s does not exist. If it is not back in %s the publication of its ledger ends for good (a signed chain-retired event).", db, left.Round(time.Minute)), true)
		return false
	}
	return true
}
