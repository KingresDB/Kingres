// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/scheduler"
)

var bg = context.Background()

const testPrefix = "ledger.test/kingres"

// ---- a fake chain, as the extension would return it ----

type fakeChain struct {
	id     [32]byte
	blocks []ledgerlog.BlockEntry
	salt   byte
}

func newChain(id byte, n int) *fakeChain {
	c := &fakeChain{}
	for i := range c.id {
		c.id[i] = id + byte(i)
	}
	c.add(n)
	return c
}

func (c *fakeChain) hashOf(i int) [32]byte {
	h, err := c.blocks[i].BlockHash()
	if err != nil {
		panic(err)
	}
	return h
}

// add appends n blocks that link to the last one.
func (c *fakeChain) add(n int) {
	for i := 0; i < n; i++ {
		var b ledgerlog.BlockEntry
		b.HashVersion = 1
		b.Height = uint64(len(c.blocks))
		if len(c.blocks) == 0 {
			b.PrevHash = sha256.Sum256(c.id[:])
		} else {
			b.PrevHash = c.hashOf(len(c.blocks) - 1)
		}
		b.TxMerkleRoot = sha256.Sum256(binary.BigEndian.AppendUint64([]byte{c.salt}, b.Height))
		b.TxCount = uint32(b.Height%7) + 1
		b.ClosedAtUS = 1_000_000 * int64(b.Height+1)
		c.blocks = append(c.blocks, b)
	}
}

// rewrite changes block h and relinks every block after it, so that the chain is valid and differs from h on (the history of a restore
// that was made again with other content).
func (c *fakeChain) rewrite(h int) {
	n := len(c.blocks)
	c.blocks = c.blocks[:h]
	c.salt++
	c.add(n - h)
}

func (c *fakeChain) entry(i int) []byte {
	e, err := c.blocks[i].Marshal()
	if err != nil {
		panic(err)
	}
	return e
}

// ---- a fake source ----

type fakeSource struct {
	mu     sync.Mutex
	groups []Group
	chains map[int32]*fakeChain
	// err makes the read of a group fail, with this error.
	err map[int32]*ReadError
	// noDB makes every group answer that the database does not exist.
	noDB  bool
	calls []string
	// digestAt is the time of the digests it makes.
	digestID int64
}

func newSource(chains ...*fakeChain) *fakeSource {
	s := &fakeSource{chains: map[int32]*fakeChain{}, err: map[int32]*ReadError{}}
	for i, c := range chains {
		s.groups = append(s.groups, Group{ID: int32(i), Server: fmt.Sprintf("srv%d", i)})
		s.chains[int32(i)] = c
	}
	return s
}

func (s *fakeSource) Groups(context.Context, string) ([]Group, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Group(nil), s.groups...), nil
}

func (s *fakeSource) Read(_ context.Context, g Group, req ReadRequest) (ReadResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, fmt.Sprintf("%d:%s", g.ID, req.Op))
	if s.noDB {
		return ReadResponse{}, &ReadError{Group: g.ID, Code: CodeNoDatabase, Message: "no such database"}
	}
	if e := s.err[g.ID]; e != nil {
		c := *e
		c.Group = g.ID
		return ReadResponse{}, &c
	}
	c := s.chains[g.ID]
	if c == nil {
		return ReadResponse{}, &ReadError{Group: g.ID, Code: CodeNoLedger, Message: "no ledger"}
	}
	if req.Op == OpEntries {
		var out ReadResponse
		for i := int(req.From); i < len(c.blocks) && i < int(req.From)+req.Max; i++ {
			out.Entries = append(out.Entries, EntryRow{Height: int64(i), Entry: c.entry(i)})
		}
		return out, nil
	}
	if len(c.blocks) == 0 {
		return ReadResponse{Empty: true}, nil
	}
	last := len(c.blocks) - 1
	h := c.hashOf(last)
	out := ReadResponse{ChainID: fmt.Sprintf("%x", c.id), Height: int64(last), BlockHash: fmt.Sprintf("%x", h), ClosedAtUS: c.blocks[last].ClosedAtUS}
	if req.Op == OpDigest {
		s.digestID++
		out.Digest = &DigestRecord{ID: s.digestID, At: time.Unix(1_700_000_000, 0).UTC(), By: "kingres_service", Height: int64(last),
			BlockHash: out.BlockHash, HashVersion: 1, Document: []byte(fmt.Sprintf(`{"hash_version":1,"height":%d}`, last))}
	}
	return out, nil
}

// ---- fake keys ----

type fakeKeys struct {
	mu      sync.Mutex
	store   *ledgerpub.MemStore
	signers []ledgerkeys.Signer
	set     ledgersign.PubContext
	term    scheduler.Term
	enabled bool
	signErr error
	rev     int64
	// bumpAfter makes the next Publication change the revision after it returned (a key task that committed meanwhile).
	bumpAfter bool
	locked    bool
	events    []fakeEvent
}

type fakeEvent struct {
	typ, subject string
	fields       []ledgersign.EventField
}

func signer(t testing.TB, n byte) ledgerkeys.Signer {
	t.Helper()
	seed := make([]byte, ledgerkeys.SeedSize)
	for i := range seed {
		seed[i] = n + byte(i)
	}
	s, err := ledgerkeys.NewFileSigner(seed)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *fakeKeys) Publication(context.Context) (ledgersign.PubContext, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pc := ledgersign.PubContext{Term: f.term, Enabled: f.enabled, Interval: time.Minute, Prefix: testPrefix, ClusterID: make([]byte, 32),
		Revision: f.rev, Locked: f.locked, Signers: f.signers, SignErr: f.signErr}
	pc.ClusterID[0] = 7
	if f.signErr != nil {
		pc.Signers = nil
	}
	if f.bumpAfter {
		f.bumpAfter = false
		f.rev++
		f.store.SetKeysRevision(f.rev)
	}
	return pc, nil
}

func (f *fakeKeys) AppendEvent(_ context.Context, typ, subject string, fields []ledgersign.EventField) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.signErr != nil {
		return 0, f.signErr
	}
	for i, e := range f.events {
		if e.typ == typ && e.subject == subject {
			return int64(i), nil
		}
	}
	f.events = append(f.events, fakeEvent{typ, subject, fields})
	// like the key service, an event is a change of the key set: a new revision, which a checkpoint signed before it must not survive
	f.rev++
	f.store.SetKeysRevision(f.rev)
	return int64(len(f.events) - 1), nil
}

// ---- the environment ----

type env struct {
	t      *testing.T
	store  *ledgerpub.MemStore
	src    *fakeSource
	keys   *fakeKeys
	coll   *Collector
	now    time.Time
	mu     sync.Mutex
	alerts []string
	// alertTexts are the messages of the alerts that were raised
	alertTexts []string
	// exists answers DatabaseExists (nil: the collector is not given one)
	exists func(db string) (bool, error)
	// published and pull are the record of what was published and the call that asks the other holders for theirs (nil: none)
	published PublishedState
	pull      func(ctx context.Context) (ledgerstate.State, bool)
}

func newEnv(t *testing.T, chains ...*fakeChain) *env {
	t.Helper()
	e := &env{t: t, now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
	e.store = ledgerpub.NewMemStore(5)
	e.store.Clock = func() time.Time { return e.now }
	e.src = newSource(chains...)
	e.keys = &fakeKeys{store: e.store, signers: []ledgerkeys.Signer{signer(t, 1)}, term: 5, enabled: true}
	e.coll = e.collector()
	return e
}

func (e *env) collector() *Collector {
	c, err := New(Config{Store: e.store, Source: e.src, Keys: e.keys, Clock: func() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now },
		Databases: func(context.Context) ([]Database, error) {
			return []Database{{Name: "shop", DigestInterval: time.Hour}}, nil
		},
		TermWait: 30 * time.Millisecond, Published: e.published, PullState: e.pull,
		DatabaseExists: func(_ context.Context, db string) (bool, error) {
			if e.exists == nil {
				return db == "shop", nil
			}
			return e.exists(db)
		},
		Alert: func(_ context.Context, kind, severity, subject, msg string, raise bool) {
			e.mu.Lock()
			defer e.mu.Unlock()
			state := "clear"
			if raise {
				state = "raise"
			}
			e.alerts = append(e.alerts, fmt.Sprintf("%s/%s/%s/%s", kind, state, severity, subject))
			if raise {
				e.alertTexts = append(e.alertTexts, msg)
			}
		}})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

// restart is a new process: a new collector over the same store and the same source.
func (e *env) restart() { e.coll = e.collector() }

func (e *env) advance(d time.Duration) { e.mu.Lock(); e.now = e.now.Add(d); e.mu.Unlock() }

func (e *env) round() Result {
	e.t.Helper()
	res, err := e.coll.Round(bg, "shop", OpHead)
	if err != nil {
		e.t.Fatalf("round: %v", err)
	}
	return res
}

func (e *env) hasAlert(prefix string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, a := range e.alerts {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func (e *env) logs() []ledgerpub.LogRow {
	e.t.Helper()
	l, err := e.store.Logs(bg)
	if err != nil {
		e.t.Fatal(err)
	}
	return l
}

// chainLog returns the chain log of a group.
func (e *env) chainLog(group int32) ledgerpub.LogRow {
	e.t.Helper()
	for _, l := range e.logs() {
		if l.Kind == ledgerpub.KindChain && l.ShardGroupID != nil && *l.ShardGroupID == group {
			return l
		}
	}
	e.t.Fatalf("no chain log for group %d", group)
	return ledgerpub.LogRow{}
}

func (e *env) aggregateLog() ledgerpub.LogRow {
	e.t.Helper()
	for _, l := range e.logs() {
		if l.Kind == ledgerpub.KindAggregate {
			return l
		}
	}
	e.t.Fatal("no aggregate log")
	return ledgerpub.LogRow{}
}

// verifyLog checks the whole published state of a log against the leaves it must hold: every tile and bundle of the stored size
// (ledgerlog.Audit) and the signed checkpoint under the key.
func (e *env) verifyLog(l ledgerpub.LogRow, keys ...ledgerkeys.Signer) ledgerlog.Checkpoint {
	e.t.Helper()
	cp, err := e.store.LatestCheckpoint(bg, l.ID)
	if err != nil {
		e.t.Fatalf("log %s has no checkpoint: %v", l.Origin, err)
	}
	pubs := make([]ed25519.PublicKey, 0, len(keys))
	for _, k := range keys {
		pubs = append(pubs, k.Public())
	}
	c, _, err := ledgerlog.VerifyCheckpoint(cp.Note, l.Origin, pubs...)
	if err != nil {
		e.t.Fatalf("the checkpoint of %s does not verify: %v", l.Origin, err)
	}
	if int64(c.Size) != cp.Size || c.Size != uint64(l.Size) {
		e.t.Fatalf("checkpoint size %d, row size %d, stored size %d", c.Size, cp.Size, l.Size)
	}
	if err := ledgerlog.Audit(bg, e.store.Tiles(l.ID, 0), c.Size, c.Root); err != nil {
		e.t.Fatalf("audit of %s: %v", l.Origin, err)
	}
	return c
}

// referenceRoot is the root of the tree of the leaves of a fake chain, computed by the plain algorithm of RFC 9162 (no tile, no
// frontier).
func referenceRoot(c *fakeChain, n int) ledgerlog.Hash {
	hs := make([]ledgerlog.Hash, n)
	for i := 0; i < n; i++ {
		hs[i] = ledgerlog.LeafHash(c.entry(i))
	}
	return ledgerlog.RootOf(hs)
}

func eventsOf(k *fakeKeys) []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, e := range k.events {
		var f []string
		for _, x := range e.fields {
			f = append(f, x.Name+"="+x.Value)
		}
		out = append(out, e.typ+" "+e.subject+" "+strings.Join(f, " "))
	}
	sort.Strings(out)
	return out
}

var errInjected = errors.New("injected failure")

// mangledSource changes what a source answers (a group that returns a range with a block missing, an entry that is not a header).
type mangledSource struct {
	inner Source
	f     func(req ReadRequest, resp ReadResponse) ReadResponse
}

func (m *mangledSource) Groups(ctx context.Context, db string) ([]Group, error) {
	return m.inner.Groups(ctx, db)
}

func (m *mangledSource) Read(ctx context.Context, g Group, req ReadRequest) (ReadResponse, error) {
	resp, err := m.inner.Read(ctx, g, req)
	if err != nil {
		return resp, err
	}
	return m.f(req, resp), nil
}

// hasAlertText reports whether the message of a raised alert holds the text.
func (e *env) hasAlertText(text string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, m := range e.alertTexts {
		if strings.Contains(m, text) {
			return true
		}
	}
	return false
}
