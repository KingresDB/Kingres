// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
)

func leafAt(t *testing.T, e *env, seq uint64) ledgerlog.AggregateEntry {
	t.Helper()
	ar := e.aggregateLog()
	r, err := ledgerlog.NewReader(bg, e.store.Tiles(ar.ID, 0), uint64(ar.Size))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := r.Entry(seq)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ledgerlog.ParseAggregateEntry(raw)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func flagsOf(l ledgerlog.AggregateEntry) []byte {
	var out []byte
	for _, m := range l.Members {
		out = append(out, m.Flags)
	}
	return out
}

// A database that is dropped through the control plane: a final round, one chain-retired event per chain, every member flagged retired in
// one leaf, the logs closed and the aggregate log ended; the hash and height in the event are the last published head, so an auditor tells
// the end from a rollback.
func TestDroppingADatabaseEndsItsChainsWithEventsAndOneRetiredLeaf(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	a.add(4) // blocks that the final round must still publish
	if err := e.coll.RetireDatabase(bg, "shop", ReasonDropped, true); err != nil {
		t.Fatal(err)
	}
	ev := eventsOf(e.keys)
	if len(ev) != 2 {
		t.Fatalf("events %v", ev)
	}
	ah := a.hashOf(6)
	want := fmt.Sprintf("chain-retired %x:1 chain=%x epoch=1 final-height=6 final-block-hash=%x reason=dropped", a.id, a.id, ah)
	var got bool
	for _, x := range ev {
		if x == want {
			got = true
		}
	}
	if !got {
		t.Errorf("no event %q in %v", want, ev)
	}
	for _, l := range e.logs() {
		if l.Kind == ledgerpub.KindAggregate {
			if l.State != ledgerpub.StateActive {
				t.Errorf("the aggregate log is %s: it must stay open, a database of the same name continues it", l.State)
			}
			continue
		}
		if l.State != ledgerpub.StateClosed {
			t.Errorf("%s is %s", l.Origin, l.State)
		}
		if l.Kind == ledgerpub.KindChain {
			if l.RetiredInSeq == nil {
				t.Errorf("%s: no retired leaf recorded", l.Origin)
			}
			e.verifyLog(l, e.keys.signers...)
		}
	}
	if e.chainLog(0).Size != 7 {
		t.Errorf("the final round published %d blocks", e.chainLog(0).Size)
	}
	last := leafAt(t, e, uint64(e.aggregateLog().Size)-1)
	if len(last.Members) != 2 {
		t.Fatalf("members %d", len(last.Members))
	}
	for _, m := range last.Members {
		if m.Flags != ledgerlog.FlagRetired {
			t.Errorf("flags %x", m.Flags)
		}
	}
	// repeated (a task step that runs again): no new event, no new leaf
	sz := e.aggregateLog().Size
	if err := e.coll.RetireDatabase(bg, "shop", ReasonDropped, true); err != nil {
		t.Fatal(err)
	}
	if e.aggregateLog().Size != sz || len(eventsOf(e.keys)) != 2 {
		t.Errorf("a second retirement changed the publication")
	}
	// the chains are still readable (in this fake), but the closed logs are not opened again
	a.add(2) // blocks that the chain gets after its end was published: nothing may be appended to a closed log
	b.add(1)
	w := e.store.Writes()
	e.round()
	if e.store.Writes() != w {
		t.Errorf("a round after the retirement wrote %d times", e.store.Writes()-w)
	}
	if e.chainLog(0).Size != 7 || e.aggregateLog().Size != sz {
		t.Errorf("a closed log or the aggregate moved: %d, %d", e.chainLog(0).Size, e.aggregateLog().Size)
	}
	for _, l := range e.logs() {
		if l.Kind == ledgerpub.KindChain && l.State != ledgerpub.StateClosed {
			t.Errorf("%s was opened again", l.Origin)
		}
	}
}

// A group that is removed: only its chain is retired, in one leaf, and the leaf after omits it; the other chain goes on.
func TestRemovingAServerRetiresTheChainsOfItsGroupsOnly(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	b.add(1)
	if err := e.coll.RetireServer(bg, "srv1"); err != nil {
		t.Fatal(err)
	}
	ev := eventsOf(e.keys)
	if len(ev) != 1 || !strings.Contains(ev[0], fmt.Sprintf("chain=%x", b.id)) || !strings.Contains(ev[0], "reason=group-removed") || !strings.Contains(ev[0], "final-height=2") {
		t.Fatalf("events %v", ev)
	}
	if e.chainLog(0).State != ledgerpub.StateActive || e.chainLog(1).State != ledgerpub.StateClosed {
		t.Fatalf("states %s %s", e.chainLog(0).State, e.chainLog(1).State)
	}
	size := e.aggregateLog().Size
	last := leafAt(t, e, uint64(size)-1)
	var retired int
	for _, m := range last.Members {
		if m.Flags&ledgerlog.FlagRetired != 0 {
			retired++
		}
	}
	if retired != 1 || len(last.Members) != 2 {
		t.Fatalf("the retired leaf: %v", flagsOf(last))
	}
	// the group is gone from the catalog; the next round's leaf omits the retired chain, once
	e.src.groups = e.src.groups[:1]
	a.add(1)
	e.advance(61 * time.Second)
	e.round()
	next := leafAt(t, e, uint64(e.aggregateLog().Size)-1)
	if len(next.Members) != 1 || next.Members[0].Flags != 0 {
		t.Fatalf("the leaf after the retirement: %v", flagsOf(next))
	}
	e.round()
	e.round()
	if got := e.aggregateLog().Size; got != size+1 {
		t.Errorf("the aggregate grew to %d without a change", got)
	}
	// a server that runs no group with a chain does nothing
	w := e.store.Writes()
	if err := e.coll.RetireServer(bg, "other"); err != nil || e.store.Writes() != w {
		t.Errorf("an unrelated server: %v, %d writes", err, e.store.Writes()-w)
	}
}

// A database that disappears without dropDatabase (an owner who is not a superuser drops it from SQL): when every group says for two rounds
// that it does not exist, the publication of its chains ends with an event that says so, not as a rollback.
func TestADatabaseThatVanishedEndsWithAnEventThatSaysSo(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	size := e.aggregateLog().Size
	e.src.noDB = true
	e.exists = func(string) (bool, error) { return false, nil }
	step := func() {
		e.advance(61 * time.Second)
		if err := e.coll.jobRound(bg, 5); err != nil {
			t.Fatal(err)
		}
	}
	// rounds do not end a publication, time does (P8-03): a digest request counts as a round too and changes nothing
	step()
	_, _ = e.coll.Round(bg, "shop", OpHead)
	step()
	if len(eventsOf(e.keys)) != 0 || e.chainLog(0).State != ledgerpub.StateActive {
		t.Fatal("two rounds that say the database is missing are not enough")
	}
	if !e.hasAlert(AlertUnread + "/raise/critical/database:shop") {
		t.Errorf("no alert warns that the end is near: %v", e.alerts)
	}
	e.advance(DefaultMissingFor)
	step()
	ev := eventsOf(e.keys)
	if len(ev) != 2 {
		t.Fatalf("events %v", ev)
	}
	for _, x := range ev {
		if !strings.Contains(x, "reason=database-missing") {
			t.Errorf("event %q", x)
		}
	}
	for _, l := range e.logs() {
		if l.Kind == ledgerpub.KindChain && l.State != ledgerpub.StateClosed {
			t.Errorf("%s is %s", l.Origin, l.State)
		}
	}
	if e.hasAlert(AlertDiverged + "/raise/") {
		t.Error("a vanished database must not be reported as a divergence")
	}
	last := leafAt(t, e, uint64(e.aggregateLog().Size)-1)
	if e.aggregateLog().Size != size+2 || flagsOf(last)[0] != ledgerlog.FlagRetired { // a stale leaf first (no group answered), then the retired one
		t.Errorf("aggregate size %d flags %v", e.aggregateLog().Size, flagsOf(last))
	}
	// a group that answers is never counted: one group missing is only unread
	a2 := newChain(70, 2)
	e2 := newEnv(t, a2, newChain(80, 2))
	e2.round()
	e2.src.err[0] = &ReadError{Code: CodeNoDatabase, Message: "gone"}
	for i := 0; i < 4; i++ {
		e2.advance(61 * time.Second)
		if err := e2.coll.jobRound(bg, 5); err != nil {
			t.Fatal(err)
		}
	}
	if len(eventsOf(e2.keys)) != 0 || e2.chainLog(0).State != ledgerpub.StateActive {
		t.Errorf("a single group without the database ended the publication")
	}
}

// A group that holds another chain than the one published (the database was made again outside the control plane): the old log is retired
// with its own reason, and the new chain gets a log of its own.
func TestAChainThatWasReplacedRetiresTheOldLogAndStartsANewOne(t *testing.T) {
	a := newChain(1, 3)
	e := newEnv(t, a)
	e.round()
	e.src.chains[0] = newChain(99, 2)
	e.round()
	var chains int
	for _, l := range e.logs() {
		if l.Kind != ledgerpub.KindChain {
			continue
		}
		chains++
		if l.State == ledgerpub.StateClosed && !strings.Contains(l.Detail, ReasonChainReplaced) {
			t.Errorf("detail %q", l.Detail)
		}
	}
	if chains != 2 {
		t.Fatalf("%d chain logs", chains)
	}
	ev := eventsOf(e.keys)
	if len(ev) != 1 || !strings.Contains(ev[0], "reason=chain-replaced") {
		t.Fatalf("events %v", ev)
	}
	last := leafAt(t, e, uint64(e.aggregateLog().Size)-1)
	var retired, plain int
	for _, m := range last.Members {
		if m.Flags == ledgerlog.FlagRetired {
			retired++
		} else if m.Flags == 0 {
			plain++
		}
	}
	if retired != 1 || plain != 1 {
		t.Errorf("flags %v", flagsOf(last))
	}
}

// A retirement cut at any of its writes is finished by the next call, and the publication ends up as an uninterrupted one gives it.
func TestARetirementCutAtAnyWriteIsFinishedByTheNext(t *testing.T) {
	build := func() (*env, *fakeChain) {
		a, b := newChain(1, 3), newChain(40, 2)
		e := newEnv(t, a, b)
		e.round()
		a.add(2)
		return e, a
	}
	ref, _ := build()
	base := ref.store.Writes()
	if err := ref.coll.RetireDatabase(bg, "shop", ReasonDropped, true); err != nil {
		t.Fatal(err)
	}
	want := snapshot(t, ref)
	wantEvents := eventsOf(ref.keys)
	writes := ref.store.Writes() - base
	if writes < 6 {
		t.Fatalf("a retirement made %d writes", writes)
	}
	for cut := 1; cut <= writes; cut++ {
		e, _ := build()
		b := e.store.Writes()
		e.store.BeforeWrite = func(kind string, n int) error {
			if n == b+cut {
				return errInjected
			}
			return nil
		}
		if err := e.coll.RetireDatabase(bg, "shop", ReasonDropped, true); !errors.Is(err, errInjected) {
			t.Fatalf("cut %d: %v", cut, err)
		}
		e.store.BeforeWrite = nil
		e.restart()
		if err := e.coll.RetireDatabase(bg, "shop", ReasonDropped, true); err != nil {
			t.Fatalf("cut %d: repeating the retirement: %v", cut, err)
		}
		// The chain logs and their checkpoints are what an uninterrupted retirement gives. The aggregate may hold one leaf more (a round that
		// runs between the two halves of a cut retirement flags the chains that were closed so far), so its leaves are checked for the
		// rule of 5.2 instead: a chain is flagged retired in the last leaf in which it appears.
		got := snapshot(t, e)
		for k := range got {
			if strings.Contains(k, "/a/") {
				delete(got, k)
			}
		}
		ref := map[string]string{}
		for k, v := range want {
			if !strings.Contains(k, "/a/") {
				ref[k] = v
			}
		}
		diffSnapshots(t, got, ref, fmt.Sprintf("retirement cut at write %d", cut))
		checkRetiredOnlyInTheLastLeaf(t, e)
		e.verifyLog(e.aggregateLog(), e.keys.signers...)
		if got := eventsOf(e.keys); fmt.Sprint(got) != fmt.Sprint(wantEvents) {
			t.Errorf("cut %d: events %v, want %v", cut, got, wantEvents)
		}
	}
}

// checkRetiredOnlyInTheLastLeaf checks the rule of design 5.2 on the whole aggregate log: a chain appears in consecutive leaves from its first
// one, is flagged retired only in the last leaf in which it appears, and every closed chain log has such a leaf.
func checkRetiredOnlyInTheLastLeaf(t *testing.T, e *env) {
	t.Helper()
	n := e.aggregateLog().Size
	type span struct{ first, last int64 }
	spans := map[[32]byte]*span{}
	retiredAt := map[[32]byte]int64{}
	for i := int64(0); i < n; i++ {
		leaf := leafAt(t, e, uint64(i))
		seen := map[[32]byte]bool{}
		for _, m := range leaf.Members {
			seen[m.ChainID] = true
			s := spans[m.ChainID]
			if s == nil {
				spans[m.ChainID] = &span{i, i}
				s = spans[m.ChainID]
			}
			if s.last != i && s.last != i-1 {
				t.Errorf("chain %x appears again at leaf %d after leaf %d", m.ChainID[:4], i, s.last)
			}
			if _, gone := retiredAt[m.ChainID]; gone {
				t.Errorf("chain %x appears at leaf %d after it was retired at %d", m.ChainID[:4], i, retiredAt[m.ChainID])
			}
			s.last = i
			if m.Flags&ledgerlog.FlagRetired != 0 {
				retiredAt[m.ChainID] = i
			}
		}
	}
	for _, l := range e.logs() {
		if l.Kind != ledgerpub.KindChain || l.State != ledgerpub.StateClosed || l.Size == 0 {
			continue
		}
		var id [32]byte
		copy(id[:], l.ChainID)
		at, ok := retiredAt[id]
		if !ok {
			t.Errorf("the closed log of chain %x has no retired leaf", id[:4])
			continue
		}
		if l.RetiredInSeq == nil || *l.RetiredInSeq != at {
			t.Errorf("chain %x: the log row says leaf %v, the leaf is %d", id[:4], l.RetiredInSeq, at)
		}
	}
}
