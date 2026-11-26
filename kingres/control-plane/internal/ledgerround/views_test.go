// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgerround

import (
	"context"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

func code(err error) problem.Code {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

func TestGetAggregateAndListAggregates(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	// not published yet: the database is known, nothing is there
	if _, err := e.coll.Aggregate(bg, "shop", nil); code(err) != problem.LedgerNotPublished {
		t.Fatalf("before the first round: %v", err)
	}
	if _, err := e.coll.Aggregate(bg, "nope", nil); code(err) != problem.NotFound {
		t.Fatalf("unknown database: %v", err)
	}
	e.round()
	a.add(1)
	e.advance(61 * time.Second)
	e.src.err[1] = &ReadError{Transport: true, Message: "down"}
	e.round()
	got, err := e.coll.Aggregate(bg, "shop", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 2 || len(got.Members) != 2 || got.Complete || len(got.UncoveredShardGroupIDs) != 1 || got.UncoveredShardGroupIDs[0] != 1 {
		t.Fatalf("aggregate %+v", got)
	}
	if !strings.Contains(got.Statement, "global snapshot of phase 8") || !strings.HasPrefix(got.Checkpoint, e.aggregateLog().Origin+"\n2\n") {
		t.Errorf("statement or checkpoint: %q", got.Checkpoint)
	}
	stale := 0
	for _, m := range got.Members {
		if len(m.Flags) == 1 && m.Flags[0] == "stale" {
			stale++
			if m.ShardGroupID != 1 {
				t.Errorf("stale member of group %d", m.ShardGroupID)
			}
		}
		if len(m.ChainID) != 64 || len(m.BlockHash) != 64 || len(m.LogRoot) != 64 || m.Epoch != 1 {
			t.Errorf("member %+v", m)
		}
	}
	if stale != 1 {
		t.Errorf("%d stale members", stale)
	}
	one := int64(1)
	first, err := e.coll.Aggregate(bg, "shop", &one)
	if err != nil || first.Size != 1 || !first.Complete || len(first.UncoveredShardGroupIDs) != 0 {
		t.Fatalf("size 1: %+v %v", first, err)
	}
	// the root in the answer is the root of the log at that size, which the checkpoint states
	cp, _ := ledgerlog.ParseNote([]byte(first.Checkpoint))
	c, err := ledgerlog.ParseCheckpoint(cp.Text)
	if err != nil || hex.EncodeToString(c.Root[:]) != first.Root {
		t.Errorf("root %s checkpoint %v %v", first.Root, c, err)
	}
	nine := int64(9)
	if _, err := e.coll.Aggregate(bg, "shop", &nine); code(err) != problem.NotFound {
		t.Errorf("a size that was not published: %v", err)
	}
	page, err := e.coll.Aggregates(bg, "shop", store.Page{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].Size != 2 || page.NextCursor == nil {
		t.Fatalf("page 1: %+v %v", page, err)
	}
	page2, err := e.coll.Aggregates(bg, "shop", store.Page{Limit: 1, Cursor: *page.NextCursor})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].Size != 1 || page2.NextCursor != nil {
		t.Fatalf("page 2: %+v %v", page2, err)
	}
	if _, err := e.coll.Aggregates(bg, "shop", store.Page{Limit: 1, Cursor: "!"}); code(err) != problem.ValidationFailed {
		t.Errorf("a bad cursor: %v", err)
	}
	if _, err := e.coll.Aggregates(bg, "nope", store.Page{Limit: 5}); code(err) != problem.NotFound {
		t.Errorf("unknown database: %v", err)
	}
	// a damaged tile is an error, never a wrong answer
	ar := e.aggregateLog()
	raw := e.store.RawTiles(ar.ID)
	k := ledgerlog.TileKey{Level: ledgerlog.EntryLevel, Index: 0, Width: 2}
	d, err := raw.GetTile(bg, k)
	if err != nil {
		t.Fatal(err)
	}
	d = append([]byte(nil), d...)
	d[len(d)-5] ^= 1
	raw.Corrupt(k, d)
	if _, err := e.coll.Aggregate(bg, "shop", nil); code(err) != problem.ServiceUnavailable {
		t.Errorf("a damaged tile: %v", err)
	}
}

func TestGetPublicationListsEveryLogWithoutNamingADatabase(t *testing.T) {
	e := newEnv(t, newChain(1, 3), newChain(40, 2))
	e.coll.cfg.Listener = func() (string, bool, bool) { return "127.0.0.1:8443", true, true }
	empty, err := e.coll.Publication(bg)
	if err != nil || !empty.Enabled || len(empty.Logs) != 0 || empty.LastCheckpointAt != nil || empty.Listener.Address == nil || !empty.Listener.TLS || !empty.Listener.Running {
		t.Fatalf("before a round: %+v %v", empty, err)
	}
	e.round()
	p, err := e.coll.Publication(bg)
	if err != nil || len(p.Logs) != 3 || p.LastCheckpointAt == nil {
		t.Fatalf("publication %+v %v", p, err)
	}
	kinds := map[string]int{}
	for _, l := range p.Logs {
		kinds[l.Kind]++
		if strings.Contains(l.Origin, "shop") {
			t.Errorf("the origin %q names the database", l.Origin)
		}
		if l.State != "active" || l.LastCheckpointAt == nil || l.Size < 1 {
			t.Errorf("log %+v", l)
		}
	}
	if kinds["chain"] != 2 || kinds["aggregate"] != 1 {
		t.Errorf("kinds %v", kinds)
	}
	e.keys.enabled = false
	off, _ := e.coll.Publication(bg)
	if off.Enabled {
		t.Error("enabled while off")
	}
}

func TestGenerateADigestOfEveryGroup(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 5)
	e := newEnv(t, a, b)
	d, err := e.coll.Digest(bg, "shop", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Chains) != 2 || !d.Complete || len(d.UncoveredShardGroupIDs) != 0 || d.GeneratedBy == nil || *d.GeneratedBy != "alice" {
		t.Fatalf("digest %+v", d)
	}
	if d.Chains[0].BlockHeight != 2 || d.Chains[1].BlockHeight != 4 || string(d.Chains[1].Document) != `{"hash_version":1,"height":4}` {
		t.Errorf("chains %+v", d.Chains)
	}
	if d.AggregateSize == nil || *d.AggregateSize != 1 || d.AggregateHash == nil || *d.AggregateHash != hex.EncodeToString(e.aggregateLogRoot()) {
		t.Errorf("aggregate %v %v", d.AggregateSize, d.AggregateHash)
	}
	// an unreadable group is named, the digest is incomplete, and the other group is still digested
	e.src.err[1] = &ReadError{Transport: true, Message: "timeout"}
	d2, err := e.coll.Digest(bg, "shop", "")
	if err != nil {
		t.Fatal(err)
	}
	if d2.Complete || len(d2.UncoveredShardGroupIDs) != 1 || d2.UncoveredShardGroupIDs[0] != 1 || len(d2.Chains) != 1 || *d2.GeneratedBy != "system" {
		t.Fatalf("digest with a group down: %+v", d2)
	}
	if d2.ID <= d.ID {
		t.Errorf("ids %d %d", d.ID, d2.ID)
	}
	// the list shows the stored views, newest first, with their aggregate hash
	page, err := e.coll.Digests(bg, "shop", store.Page{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != d2.ID || page.NextCursor == nil {
		t.Fatalf("list %+v %v", page, err)
	}
	page, err = e.coll.Digests(bg, "shop", store.Page{Limit: 5, Cursor: *page.NextCursor})
	if err != nil || len(page.Items) != 1 || page.Items[0].ID != d.ID || page.Items[0].AggregateHash == nil {
		t.Fatalf("second page %+v %v", page, err)
	}
	// every group down: a 503, not a digest that claims to be complete
	e.src.err[0] = &ReadError{Transport: true, Message: "timeout"}
	if _, err := e.coll.Digest(bg, "shop", ""); code(err) != problem.ServiceUnavailable {
		t.Errorf("no group answers: %v", err)
	}
	// a database with no ledger and an empty ledger
	e.src.err = map[int32]*ReadError{0: {Code: CodeNoLedger, Message: "x"}, 1: {Code: CodeNoLedger, Message: "x"}}
	if _, err := e.coll.Digest(bg, "shop", ""); code(err) != problem.Conflict {
		t.Errorf("no ledger: %v", err)
	}
	e.src.err = map[int32]*ReadError{}
	a.blocks, b.blocks = nil, nil
	if _, err := e.coll.Digest(bg, "shop", ""); code(err) != problem.Conflict {
		t.Errorf("empty ledgers: %v", err)
	}
	// publication off: the digest is made, with no aggregate
	a.add(1)
	e.keys.enabled = false
	d3, err := e.coll.Digest(bg, "shop", "bob")
	if err != nil || d3.AggregateHash != nil || d3.AggregateSize != nil || len(d3.Chains) != 1 {
		t.Fatalf("publication off: %+v %v", d3, err)
	}
	// a follower makes no digest
	e.keys.term = 0
	if _, err := e.coll.Digest(bg, "shop", ""); code(err) != problem.ServiceUnavailable {
		t.Errorf("a follower: %v", err)
	}
}

func (e *env) aggregateLogRoot() []byte { return e.aggregateLog().Root }

// A key rotation signs the latest checkpoint of every log again with the keys that sign now (correction 47): the old note keeps its line,
// the new key adds one, and it verifies under either key.
func TestAKeyRotationSignsEveryLatestCheckpointAgain(t *testing.T) {
	e := newEnv(t, newChain(1, 3), newChain(40, 2))
	e.round()
	old := e.keys.signers[0]
	next := signer(t, 50)
	// before anything is published the hook has nothing to do (and needs no signer)
	empty := newEnv(t)
	empty.keys.signErr = errors.New("nothing can sign")
	if err := empty.coll.Resign(bg, nil); err != nil {
		t.Fatalf("no log: %v", err)
	}
	e.keys.signers = []ledgerkeys.Signer{old, next}
	set := rotationSet(t, old, next)
	if err := e.coll.Resign(bg, set); err != nil {
		t.Fatal(err)
	}
	for _, l := range e.logs() {
		c := e.verifyLog(l, old, next)
		_ = c
		cp, _ := e.store.LatestCheckpoint(bg, l.ID)
		n, _ := ledgerlog.ParseNote(cp.Note)
		if len(n.Signatures) != 2 {
			t.Errorf("%s: %d signature lines", l.Origin, len(n.Signatures))
		}
		// each key alone verifies it
		for _, k := range []ledgerkeys.Signer{old, next} {
			if _, _, err := ledgerlog.VerifyCheckpoint(cp.Note, l.Origin, k.Public()); err != nil {
				t.Errorf("%s under one key: %v", l.Origin, err)
			}
		}
	}
	// again: nothing is added twice
	w := e.store.Writes()
	if err := e.coll.Resign(bg, set); err != nil || e.store.Writes() != w {
		t.Errorf("a second re-signing: %v, %d writes", err, e.store.Writes()-w)
	}
	// a follower does not re-sign
	e.keys.term = 0
	if err := e.coll.Resign(bg, set); !errors.Is(err, ErrNotLeader) {
		t.Errorf("a follower: %v", err)
	}
}

func rotationSet(t *testing.T, old, next ledgerkeys.Signer) ledgerkeys.Set {
	t.Helper()
	now := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	s, err := ledgerkeys.Set(nil).Add(old.Public(), "old", now)
	if err != nil {
		t.Fatal(err)
	}
	if s, err = s.Activate(ledgerkeys.Fingerprint(old.Public()), now, 0); err != nil {
		t.Fatal(err)
	}
	if s, err = s.Add(next.Public(), "new", now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if s, err = s.Activate(ledgerkeys.Fingerprint(next.Public()), now.Add(2*time.Hour), 30); err != nil {
		t.Fatal(err)
	}
	return s
}

var _ = context.Background
var _ api.LedgerDigest
var _ ledgerpub.Store

// A request that comes while a leader that has just been elected waits for its term is not refused at once: it waits, up to TermWait; a server that
// never gets the term (a follower) answers that it is not the leader after the wait.
func TestARequestWaitsForTheTermOfANewLeader(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	e.coll.cfg.TermWait = 3 * time.Second
	e.keys.term = 0
	go func() {
		time.Sleep(600 * time.Millisecond)
		e.keys.mu.Lock()
		e.keys.term = 5
		e.keys.mu.Unlock()
	}()
	start := time.Now()
	d, err := e.coll.Digest(bg, "shop", "alice")
	if err != nil || len(d.Chains) != 1 {
		t.Fatalf("%v %+v", err, d)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Errorf("the digest did not wait for the term (%v)", time.Since(start))
	}
	e.keys.mu.Lock()
	e.keys.term = 0
	e.keys.mu.Unlock()
	e.coll.cfg.TermWait = 300 * time.Millisecond
	if _, err := e.coll.Digest(bg, "shop", "alice"); code(err) != problem.ServiceUnavailable {
		t.Errorf("a follower: %v", err)
	}
}

// A database that does not exist is a 404 for the list of digests, and an empty list for one that exists and has none.
func TestTheListOfDigestsOfADatabaseThatDoesNotExistIsA404(t *testing.T) {
	e := newEnv(t, newChain(1, 3))
	exists := map[string]bool{"shop": true, "plain": true}
	e.coll.cfg.DatabaseExists = func(_ context.Context, db string) (bool, error) { return exists[db], nil }
	if _, err := e.coll.Digests(bg, "ghost", store.Page{Limit: 5}); code(err) != problem.NotFound {
		t.Errorf("a database that does not exist: %v", err)
	}
	page, err := e.coll.Digests(bg, "plain", store.Page{Limit: 5})
	if err != nil || len(page.Items) != 0 {
		t.Errorf("a database with no digest: %v %v", page, err)
	}
	if _, err := e.coll.Aggregates(bg, "ghost", store.Page{Limit: 5}); code(err) != problem.NotFound {
		t.Errorf("aggregates of a database that does not exist: %v", err)
	}
	if _, err := e.coll.Aggregate(bg, "plain", nil); code(err) != problem.LedgerNotPublished {
		t.Errorf("an aggregate of a database that has none: %v", err)
	}
}
