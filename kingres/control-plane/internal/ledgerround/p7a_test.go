// SPDX-License-Identifier: AGPL-3.0-only

package ledgerround

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerpub"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerstate"
)

// The tests of the fixes of the review of the wiring and of the publication fixes (P7A-02, P7A-06, P7A-11 and P7A-16).

// ---- P7A-06: one wrong answer does not end the real chain ----

func TestOneWrongAnswerBetweenRightOnesDoesNotEndTheRealChain(t *testing.T) {
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b)
	e.round()
	size := e.chainLog(0).Size
	e.src.chains[0] = newChain(120, 2) // one round in which the group answers a chain that is not its own
	e.advance(time.Minute)
	e.alerts = nil
	res := e.round()
	if l := e.chainLog(0); l.State != ledgerpub.StateActive || l.Size != size {
		t.Fatalf("after one wrong answer: %s %q size %d (was %d)", l.State, l.Detail, l.Size, size)
	}
	if len(eventsOf(e.keys)) != 0 {
		t.Fatalf("events after one wrong answer: %v", eventsOf(e.keys))
	}
	if !e.hasAlert(AlertDiverged + "/raise/critical/database:shop|group:0") {
		t.Errorf("no alert from the first answer: %v", e.alerts)
	}
	if res.Groups[0].Err == nil {
		t.Errorf("the group is not shown as unread in the round")
	}
	// the real chain answers again, with more blocks: it goes on, the alert ends, nothing was ever closed
	e.src.chains[0] = a
	a.add(4)
	e.advance(time.Minute)
	e.alerts = nil
	for i := 0; i < 3; i++ {
		e.advance(time.Minute)
		e.round()
	}
	if l := e.chainLog(0); l.State != ledgerpub.StateActive || l.Size != 7 {
		t.Fatalf("after the real chain came back: %s %q size %d", l.State, l.Detail, l.Size)
	}
	if len(eventsOf(e.keys)) != 0 {
		t.Errorf("events: %v", eventsOf(e.keys))
	}
	if !e.hasAlert(AlertDiverged + "/clear/critical/database:shop|group:0") {
		t.Errorf("the alert was not cleared when the real chain came back: %v", e.alerts)
	}
	// answers that alternate between two other chains never add up to a confirmation either
	e.src.chains[0] = newChain(130, 2)
	e.round()
	e.advance(20 * time.Minute)
	e.src.chains[0] = newChain(140, 2)
	e.round()
	e.advance(20 * time.Minute)
	e.src.chains[0] = newChain(130, 2)
	e.round()
	if len(eventsOf(e.keys)) != 0 || e.chainLog(0).State != ledgerpub.StateActive {
		t.Errorf("alternating answers ended the log: %v", eventsOf(e.keys))
	}
}

// ---- P7A-02: the restore of the system database has a deliberate way back ----

// openStateAt opens the record in dir.
func openStateAt(t *testing.T, dir string) *ledgerstate.File {
	t.Helper()
	f, err := ledgerstate.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// restoreSetup makes a live publisher and a restored database that is behind the record f (in dir), as the sequence of the P8-01 tests does.
func restoreSetup(t *testing.T, dir string) (f *ledgerstate.File, live, restored *env, a2 *fakeChain) {
	t.Helper()
	f = openStateAt(t, dir)
	a, b := newChain(1, 3), newChain(40, 2)
	live = newEnv(t, a, b).withState(f)
	live.round()
	a.add(2)
	live.advance(time.Minute)
	live.round()
	b.add(1)
	live.advance(time.Minute)
	live.round()
	a2, b2 := newChain(1, 3), newChain(40, 2)
	restored = newEnv(t, a2, b2)
	restored.round()
	a2.add(2)
	b2.add(1)
	restored.withState(f)
	restored.advance(7 * time.Hour)
	return f, live, restored, a2
}

func TestAnOperatorWhoWritesTheConfirmationAcceptsTheLossAndAnEventSaysSo(t *testing.T) {
	dir := t.TempDir()
	f, live, restored, _ := restoreSetup(t, dir)
	old := f.Snapshot()
	liveSize := live.aggregateLog().Size
	restoredSize := restored.aggregateLog().Size
	res := restored.round()
	if !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("the restored database was not refused: %v", res.SignErr)
	}
	// the alert names the confirmation, and the file that takes it
	db, err := restored.coll.buildState(bg)
	if err != nil {
		t.Fatal(err)
	}
	conf := ledgerstate.Confirmation(f.Snapshot(), db)
	if !restored.hasAlertText(conf) || !restored.hasAlertText(ledgerstate.AcceptFileName) {
		t.Errorf("the alert does not say what to write and where: %v", restored.alertTexts)
	}
	// a wrong confirmation, and one for another situation, change nothing
	for _, wrong := range []string{"0000000000000000", conf[:15], conf + "0", ""} {
		if err := os.WriteFile(filepath.Join(dir, ledgerstate.AcceptFileName), []byte(wrong+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		restored.advance(time.Minute)
		if res := restored.round(); !errors.Is(res.SignErr, ErrPublishedState) {
			t.Fatalf("a wrong confirmation %q was taken: %v", wrong, res.SignErr)
		}
	}
	if f.Snapshot().Generation != 0 {
		t.Fatal("the record was changed by a wrong confirmation")
	}
	// the right one
	if err := os.WriteFile(filepath.Join(dir, ledgerstate.AcceptFileName), []byte(conf+"\nrestored from the backup of Monday\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restored.advance(time.Minute)
	res = restored.round()
	if res.SignErr != nil {
		t.Fatalf("after the acceptance: %v", res.SignErr)
	}
	if _, err := os.Stat(filepath.Join(dir, ledgerstate.AcceptFileName)); err == nil {
		t.Error("the file of the confirmation was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, ledgerstate.FileName+".before-restore-1")); err != nil {
		t.Errorf("the record that was replaced was not kept: %v", err)
	}
	cur := f.Snapshot()
	if cur.Generation != 1 || cur.Restore != nil {
		t.Errorf("the record after the announcement: generation %d note %+v", cur.Generation, cur.Restore)
	}
	// the signed event: type, generation, what the record held and what the database held, the reason and who accepted
	var found bool
	for _, ev := range restored.keys.events {
		if ev.typ != EventRestoreAcknowledged {
			continue
		}
		found = true
		if ev.subject != "generation:1" {
			t.Errorf("subject %q", ev.subject)
		}
		got := map[string]string{}
		for _, fl := range ev.fields {
			got[fl.Name] = fl.Value
		}
		if got["generation"] != "1" || got["published-state-sha256"] != ledgerstate.SumOf(old) || got["database-state-sha256"] == "" ||
			got["reason"] != "restored from the backup of Monday" || !strings.Contains(got["acknowledged-by"], "operator") {
			t.Errorf("the fields of the event: %v", got)
		}
	}
	if !found {
		t.Fatalf("no event restore-acknowledged: %v", eventsOf(restored.keys))
	}
	// the publication goes on from the restored database: the aggregate grows past what the restored database held, and the record follows
	restored.advance(time.Minute)
	restored.round()
	if got := restored.aggregateLog().Size; got <= restoredSize {
		t.Errorf("the aggregate did not grow: %d (was %d, the live one had %d)", got, restoredSize, liveSize)
	}
	if m := f.Snapshot().Logs[restored.aggregateLog().Origin]; m.Size != restored.aggregateLog().Size {
		t.Errorf("the record does not follow: %+v", m)
	}
	// a holder that has the old record takes the acceptance when it meets the new one, and the new one never takes the floor back from the old
	other := openStateAt(t, t.TempDir())
	if err := other.Merge(old); err != nil {
		t.Fatal(err)
	}
	if err := other.Merge(f.Snapshot()); err != nil || other.Snapshot().Generation != 1 || other.Snapshot().Logs[restored.aggregateLog().Origin].Size != restored.aggregateLog().Size {
		t.Errorf("a holder did not take the acceptance: %v %+v", err, other.Snapshot())
	}
	if err := f.Merge(old); err != nil || f.Snapshot().Generation != 1 || f.Snapshot().Logs[restored.aggregateLog().Origin].Size < restoredSize {
		t.Errorf("an old record took the floor back: %v %+v", err, f.Snapshot())
	}
}

// The announcement is not lost when the event cannot be signed at once or the process ends between the acceptance and the event.
func TestTheEventOfAnAcceptedRestoreIsPublishedBeforeAnythingElseAndSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	f, _, restored, _ := restoreSetup(t, dir)
	restored.round()
	db, _ := restored.coll.buildState(bg)
	conf := ledgerstate.Confirmation(f.Snapshot(), db)
	if err := os.WriteFile(filepath.Join(dir, ledgerstate.AcceptFileName), []byte(conf+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// the event cannot be appended (the key service says no), the check passes: the round signs nothing
	failing := &failingKeys{fakeKeys: restored.keys}
	restored.coll.cfg.Keys = failing
	restored.advance(time.Minute)
	size := restored.aggregateLog().Size
	res := restored.round()
	if !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("a round with an event that cannot be published signed on: %v", res.SignErr)
	}
	if restored.aggregateLog().Size != size {
		t.Error("the aggregate grew before the event was published")
	}
	if f.Snapshot().Restore == nil || f.Snapshot().Generation != 1 {
		t.Fatalf("the note of the acceptance is gone: %+v", f.Snapshot())
	}
	// a restart: the record on the disk still has the note, and the next round publishes the event and goes on
	g := openStateAt(t, dir)
	if g.Snapshot().Restore == nil {
		t.Fatal("the note did not reach the disk")
	}
	restored.published = g
	restored.restart()
	restored.advance(time.Minute)
	if res := restored.round(); res.SignErr != nil {
		t.Fatalf("after the restart: %v", res.SignErr)
	}
	n := 0
	for _, ev := range restored.keys.events {
		if ev.typ == EventRestoreAcknowledged {
			n++
		}
	}
	if n != 1 || g.Snapshot().Restore != nil {
		t.Errorf("%d events, note %+v", n, g.Snapshot().Restore)
	}
}

// failingKeys is the fake keys with an AppendEvent that fails for the restore event.
type failingKeys struct{ *fakeKeys }

func (k *failingKeys) AppendEvent(ctx context.Context, typ, subject string, fields []ledgersign.EventField) (int64, error) {
	if typ == EventRestoreAcknowledged {
		return 0, errors.New("the key service is not available")
	}
	return k.fakeKeys.AppendEvent(ctx, typ, subject, fields)
}

// The acceptance made on another server reaches this one through the other holders (the record of a higher generation replaces).
func TestAServerThatHoldsTheOldRecordFindsTheAcceptanceAtTheOtherHolders(t *testing.T) {
	dir := t.TempDir()
	f, live, restored, _ := restoreSetup(t, dir)
	_ = live
	restored.round()
	db, _ := restored.coll.buildState(bg)
	// the other server accepted the loss: its record is the restored database's, one generation higher
	other := openStateAt(t, t.TempDir())
	if err := other.Accept(db, ledgerstate.RestoreNote{Reason: "restore", Actor: "operator"}); err != nil {
		t.Fatal(err)
	}
	_ = other.ClearRestore()
	restored.pull = func(context.Context) (ledgerstate.State, bool) { return other.Snapshot(), true }
	restored.withState(f) // this server still has the old record
	restored.advance(time.Minute)
	if res := restored.round(); res.SignErr != nil {
		t.Fatalf("with the record of another holder that accepted: %v", res.SignErr)
	}
	if f.Snapshot().Generation != 1 {
		t.Errorf("generation %d", f.Snapshot().Generation)
	}
}

// ---- P7A-11: a record that cannot be written stops the signing ----

func TestARecordThatCannotBeWrittenStopsSigningUntilItCan(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f := openStateAt(t, dir)
	a, b := newChain(1, 3), newChain(40, 2)
	e := newEnv(t, a, b).withState(f)
	e.round()
	if err := os.RemoveAll(dir); err != nil { // a disk that cannot be written (a full one behaves the same: the write fails)
		t.Fatal(err)
	}
	a.add(2)
	e.advance(time.Minute)
	e.alerts = nil
	res := e.round()
	if !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("a record that cannot be written: SignErr %v published %v", res.SignErr, res.Published)
	}
	if !e.hasAlert(AlertPublishedKind + "/raise/critical/" + AlertPublishedSubject) {
		t.Errorf("no critical alert: %v", e.alerts)
	}
	// nothing more is signed while it cannot be written, even for a log that has nothing to do with the failure
	b.add(1)
	e.advance(time.Minute)
	sizeB := e.chainLog(1).Size
	if res := e.round(); !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("a second round: %v", res.SignErr)
	}
	if e.chainLog(1).Size != sizeB {
		t.Errorf("a log grew while the record cannot be written")
	}
	// the directory is back: the record is written again before anything is signed, and the rounds go on
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	e.alerts = nil
	if res := e.round(); res.SignErr != nil {
		t.Fatalf("after the directory came back: %v", res.SignErr)
	}
	g := openStateAt(t, dir)
	if g.Damaged() || !g.Initialized() || g.Snapshot().Logs[e.chainLog(0).Origin].Size != e.chainLog(0).Size {
		t.Errorf("the record on the disk: %+v", g.Snapshot())
	}
}

// ---- P7A-17: the gaps that the mutants of the review showed (part a) ----

// The root of a published size is compared: the same size with another root is refused (a restored database that grew to the same size by other
// paths).
func TestTheSameSizeWithAnotherRootThanTheRecordIsRefused(t *testing.T) {
	f := openState(t)
	e := newEnv(t, newChain(1, 3)).withState(f)
	e.round()
	l := e.chainLog(0)
	st := f.Snapshot()
	st.Logs[l.Origin] = ledgerstate.Mark{Size: l.Size, Root: strings.Repeat("ab", 32)}
	if err := f.Replace(st); err != nil {
		t.Fatal(err)
	}
	e.advance(time.Minute)
	if res := e.round(); !errors.Is(res.SignErr, ErrPublishedState) {
		t.Fatalf("a record with another root for the stored size: %v", res.SignErr)
	}
}

// A group that cannot be reached is not a vote that the database is gone: only an answer "no such database" counts.
func TestAnUnreachableGroupIsNotAVoteThatTheDatabaseIsGone(t *testing.T) {
	e := newEnv(t, newChain(70, 2), newChain(80, 2))
	e.round()
	for g := int32(0); g < 2; g++ {
		e.src.err[g] = &ReadError{Group: g, Transport: true, Message: "connection refused"}
	}
	e.exists = func(string) (bool, error) { return false, nil }
	for i := 0; i < 40; i++ {
		e.advance(61 * time.Second)
		if err := e.coll.jobRound(bg, 5); err != nil {
			t.Fatal(err)
		}
	}
	if len(eventsOf(e.keys)) != 0 || e.chainLog(0).State != ledgerpub.StateActive {
		t.Errorf("unreachable groups ended the publication: %v", eventsOf(e.keys))
	}
}

// A damaged record is never replaced by what the database holds: only an operator removes it.
func TestADamagedRecordIsNotReplacedByWhatTheDatabaseHolds(t *testing.T) {
	dir := t.TempDir()
	f := openStateAt(t, dir)
	_ = f.RecordCheckpoint("o", 5, []byte{1, 2, 3})
	p := filepath.Join(dir, ledgerstate.FileName)
	b, _ := os.ReadFile(p)
	b[10] ^= 1
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	g := openStateAt(t, dir)
	e := newEnv(t, newChain(1, 3)).withState(g)
	res := e.round()
	if !errors.Is(res.SignErr, ErrPublishedState) || !g.Damaged() || g.Initialized() {
		t.Fatalf("a damaged record: %v damaged %v", res.SignErr, g.Damaged())
	}
	if len(e.logs()) != 0 {
		t.Errorf("something was written: %d logs", len(e.logs()))
	}
}
