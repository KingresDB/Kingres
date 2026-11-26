// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ledgersign_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerlog"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgersign"
)

// What the collector takes from the keys (steps 5 and 6): the settings of the publication, the keys that sign now with the revision they were
// read at, and the events of the chains.
func TestPublicationReportsTheSettingsTheSignersAndWhyNothingSigns(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, memFactory)
	pc, err := e.svc.Publication(ctx)
	if err != nil || pc.Enabled || !errors.Is(pc.SignErr, ledgersign.ErrNoKeys) || len(pc.Signers) != 0 || pc.Term == 0 {
		t.Fatalf("before a key: %+v %v", pc, err)
	}
	e.create(0)
	pc, err = e.svc.Publication(ctx)
	if err != nil || pc.SignErr != nil || len(pc.Signers) != 1 || len(pc.ClusterID) != 32 || pc.Prefix == "" || pc.Locked || pc.Interval != time.Minute {
		t.Fatalf("with a key: %+v %v", pc, err)
	}
	rev := pc.Revision
	// a change of the key set raises the revision, which is what makes a checkpoint signed before it be refused (ledgerpub.PutCheckpoint)
	e.clk.Add(time.Hour)
	e.create(10)
	pc, _ = e.svc.Publication(ctx)
	if pc.Revision <= rev || len(pc.Signers) != 2 {
		t.Errorf("after the rotation: revision %d (was %d), %d signers", pc.Revision, rev, len(pc.Signers))
	}
	// a follower never signs: the settings are still known, the signers are not given
	e.stopLeading()
	pc, err = e.svc.Publication(ctx)
	if err != nil || !errors.Is(pc.SignErr, ledgersign.ErrNotLeader) || len(pc.Signers) != 0 || pc.Term != 0 || pc.Prefix == "" {
		t.Errorf("a follower: %+v %v", pc, err)
	}
	e.becomeLeader()
	// a damaged key set is an alarm and no signer
	e.mem.Overwrite(func(s *ledgersign.Snapshot) { s.Cluster.TrustFile = []byte("{}") })
	pc, _ = e.svc.Publication(ctx)
	if !errors.Is(pc.SignErr, ledgersign.ErrAlarm) || len(pc.Signers) != 0 {
		t.Errorf("a damaged set: %+v", pc)
	}
}

// A key whose retirement was begun (its key-retired event is published) is not given to the collector any more: the bound in the event was decided
// from the checkpoints stored then, so a checkpoint signed with the key afterwards could be above it (corrections 128 and 129).
func TestAKeyWhoseRetirementWasBegunIsNotGivenToTheCollector(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, memFactory)
	e.create(0)
	k1 := keyIn(t, load(t, e), ledgerkeys.StateActive)
	e.clk.Add(time.Hour)
	e.create(10)
	pc, _ := e.svc.Publication(ctx)
	if len(pc.Signers) != 2 {
		t.Fatalf("%d signers", len(pc.Signers))
	}
	ev1 := realEvent(t, e, load(t, e).Events, ledgersign.EventKeyRetired, k1.Fingerprint())
	e.mem.Overwrite(func(s *ledgersign.Snapshot) { s.Events = append(s.Events, ev1) })
	pc, _ = e.svc.Publication(ctx)
	if pc.SignErr != nil || len(pc.Signers) != 1 || !pc.Signers[0].Public().Equal(keyOther(t, load(t, e), k1.Fingerprint()).Public) {
		t.Fatalf("signers after the event: %d, %v", len(pc.Signers), pc.SignErr)
	}
	// when it is the only key in force there is nobody to sign: do not sign, which is no alarm
	k2 := keyOther(t, load(t, e), k1.Fingerprint())
	ev2 := realEvent(t, e, load(t, e).Events, ledgersign.EventKeyRetired, k2.Fingerprint())
	e.mem.Overwrite(func(s *ledgersign.Snapshot) { s.Events = append(s.Events, ev2) })
	pc, _ = e.svc.Publication(ctx)
	if !errors.Is(pc.SignErr, ledgersign.ErrDoNotSign) || len(pc.Signers) != 0 {
		t.Errorf("every key being retired: %+v", pc)
	}
}

func keyOther(t *testing.T, s ledgersign.Snapshot, not string) ledgerkeys.Key {
	t.Helper()
	for _, k := range s.Keys {
		if k.Fingerprint() != not && k.State == ledgerkeys.StateActive {
			return k.Key
		}
	}
	t.Fatal("no other active key")
	return ledgerkeys.Key{}
}

// The bound of a retired key is decided inside the change that states it, and the store refuses the change when a checkpoint above the bound was
// stored after the bound was read (the exclusion of signing and retiring, correction 129): the step reads the bound again and states the new one.
func TestTheBoundOfARetiredKeyIsReadAgainWhenACheckpointWasStoredMeanwhile(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	e.create(1)
	e.mem.MaxSize = 4711
	e.mem.AfterMax = func(n int) {
		if n == 1 {
			e.mem.MaxSize = 9000 // the collector stored a checkpoint of size 9000 with the key that is being retired
		}
	}
	e.clk.Add(25 * time.Hour)
	k1 := load(t, e).Keys[0]
	e.mustDone(e.run("ledger.key.retire", map[string]any{"fingerprint": k1.Fingerprint()}))
	r, _ := load(t, e).Set().Get(k1.Fingerprint())
	if r.MaxTreeSize != 9000 {
		t.Errorf("bound %d, want 9000: the checkpoint stored between the read and the commit must be inside it", r.MaxTreeSize)
	}
	s := load(t, e)
	ev := s.Events[len(s.Events)-1]
	if !strings.Contains(string(ev.Note), "max-tree-size: 9000\n") {
		t.Errorf("the event: %s", ev.Note)
	}
}

func TestACollectorEventIsSignedByTheActiveKeysAndPublishedOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t, memFactory)
	e.create(0)
	k := keyIn(t, load(t, e), ledgerkeys.StateActive)
	fields := []ledgersign.EventField{{Name: "chain", Value: strings.Repeat("ab", 32)}, {Name: "epoch", Value: "1"}, {Name: "final-height", Value: "41"},
		{Name: "final-block-hash", Value: strings.Repeat("cd", 32)}, {Name: "reason", Value: "dropped"}}
	subject := strings.Repeat("ab", 32) + ":1"
	seq, err := e.svc.AppendEvent(ctx, ledgersign.EventChainRetired, subject, fields)
	if err != nil || seq != 1 {
		t.Fatalf("%d %v", seq, err)
	}
	again, err := e.svc.AppendEvent(ctx, ledgersign.EventChainRetired, subject, fields)
	if err != nil || again != 1 || len(load(t, e).Events) != 2 {
		t.Fatalf("repeated: %d %v, %d events", again, err, len(load(t, e).Events))
	}
	s := load(t, e)
	n := verifyEvent(t, s.Cluster.OriginPrefix, s.Events[1].Note, "chain-retired", k.Public)
	for _, want := range []string{"seq: 1\n", "type: chain-retired\n", "chain: " + strings.Repeat("ab", 32) + "\n", "final-height: 41\n", "reason: dropped\n"} {
		if !strings.Contains(string(n.Text), want) {
			t.Errorf("the event lacks %q:\n%s", want, n.Text)
		}
	}
	// a value that is not printable ASCII is refused, and so is an event without a subject
	if _, err := e.svc.AppendEvent(ctx, ledgersign.EventChainRetired, "x:2", []ledgersign.EventField{{Name: "reason", Value: "caf\u00e9"}}); err == nil {
		t.Error("a reason that is not ASCII was accepted")
	}
	if _, err := e.svc.AppendEvent(ctx, ledgersign.EventChainRetired, "", nil); err == nil {
		t.Error("an event with no subject")
	}
	// a follower writes no event
	e.stopLeading()
	if _, err := e.svc.AppendEvent(ctx, ledgersign.EventChainRetired, "y:1", fields); err == nil {
		t.Error("a follower wrote an event")
	}
	if len(load(t, e).Events) != 2 {
		t.Error("an event was written")
	}
	if p, err := e.svc.Prefix(ctx); err != nil || p != s.Cluster.OriginPrefix {
		t.Errorf("prefix %q %v", p, err)
	}
}

// The alert ledgerSignerUnavailable has one owner, the check job. It is raised for a key set that cannot sign and, since the collector exists, for
// a collector that could not sign for two rounds (correction 134); it is cleared only when neither complains.
func TestTheCheckJobRaisesTheAlertWhenTheCollectorCannotSign(t *testing.T) {
	e := newEnv(t, memFactory)
	e.create(0)
	check := e.svc.Jobs()[1]
	ctx := context.Background()
	e.pubFail = "No checkpoint of the ledger could be signed for two rounds: the clock"
	e.alerts = nil
	if err := check.Run(ctx, 1); err != nil || len(e.alerts) != 1 || !strings.HasPrefix(e.alerts[0], "ledgerSignerUnavailable/raise/") || !strings.Contains(e.alerts[0], "for two rounds") {
		t.Fatalf("%v %v", err, e.alerts)
	}
	e.pubFail = ""
	e.alerts = nil
	if err := check.Run(ctx, 1); err != nil || len(e.alerts) != 1 || !strings.HasPrefix(e.alerts[0], "ledgerSignerUnavailable/clear/") {
		t.Fatalf("%v %v", err, e.alerts)
	}
}

// realEvent is a stored event with a real signature of an active key (the chain of events is verified at every load).
func realEvent(t *testing.T, e *env, have []ledgersign.EventRow, typ, subject string) ledgersign.EventRow {
	t.Helper()
	s := load(t, e)
	k := keyIn(t, s, ledgerkeys.StateActive)
	sg, err := e.kr.Signer(k.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	seq := int64(len(have))
	text := fmt.Sprintf("kingres-ledger-event/1\nseq: %d\ntime: %s\ntype: %s\n", seq, e.clk.Now().UTC().Truncate(time.Second).Format(time.RFC3339), typ)
	n, err := ledgerlog.SignNote(context.Background(), []byte(text), s.Cluster.OriginPrefix, sg)
	if err != nil {
		t.Fatal(err)
	}
	note, err := n.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return ledgersign.EventRow{Seq: seq, Type: typ, Subject: subject, Note: note, CreatedAt: e.clk.Now()}
}

// brokenRecord is a record of what was published whose file cannot be written.
type brokenRecord struct {
	recorded int
	broken   bool
}

func (b *brokenRecord) AllowEvent(int64, []byte) error { return nil }
func (b *brokenRecord) RecordEvent(int64, []byte) error {
	b.recorded++
	if b.broken {
		return errors.New("no space left on the device")
	}
	return nil
}

// P7A-11: an event that was stored while the record of what was published could not be written is an error of the call, not a line of the log.
func TestAnEventWhoseRecordCannotBeWrittenIsAnError(t *testing.T) {
	ctx := context.Background()
	br := &brokenRecord{}
	e := newEnv(t, memFactory, func(e *env) { e.published = br })
	e.create(0)
	br.recorded, br.broken = 0, true
	_, err := e.svc.AppendEvent(ctx, ledgersign.EventChainRetired, strings.Repeat("ab", 32)+":1", []ledgersign.EventField{{Name: "chain", Value: strings.Repeat("ab", 32)}})
	if err == nil || !strings.Contains(err.Error(), "could not be written") || br.recorded != 1 {
		t.Fatalf("an event whose record cannot be written: %v (recorded %d)", err, br.recorded)
	}
	// the event is stored, and a repeat finds it
	if n := len(load(t, e).Events); n < 2 {
		t.Errorf("%d events", n)
	}
}
