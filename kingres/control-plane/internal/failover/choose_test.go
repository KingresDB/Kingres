// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"testing"
	"time"
)

func cand(id int64, server, recv, replay string, mod func(*Candidate)) Candidate {
	c := Candidate{StandbyID: id, Server: server, Readable: true, Pos: Position{Server: server, InRecovery: true, Timeline: 3, ReceiveLsn: recv, ReplayLsn: replay, SystemIdentifier: "77"}}
	if mod != nil {
		mod(&c)
	}
	return c
}

var goodFacts = GroupFacts{SyncStreaming: true, LastKnownAge: 5 * time.Second}

func TestFlushedIsTheLargerOfReceiveAndReplay(t *testing.T) {
	for _, tc := range []struct {
		recv, replay string
		want         uint64
		ok           bool
	}{
		{"0/5000000", "0/4000000", 0x5000000, true},
		{"", "0/4000000", 0x4000000, true}, // no walreceiver since the start: ReceiveLsn is empty
		{"0/4000000", "0/5000000", 0x5000000, true},
		{"1/0", "0/FFFFFFFF", 0x100000000, true},
		{"", "", 0, false},
		{"garbage", "", 0, false},
	} {
		got, ok := Position{ReceiveLsn: tc.recv, ReplayLsn: tc.replay}.Flushed()
		if got != tc.want || ok != tc.ok {
			t.Errorf("Flushed(%q, %q) = %x %v", tc.recv, tc.replay, got, ok)
		}
	}
}

func TestChooseOrder(t *testing.T) {
	sync := func(c *Candidate) { c.SyncState = "sync" }
	pref := func(c *Candidate) { c.Preferred = true }
	for _, tc := range []struct {
		name  string
		cands []Candidate
		want  int64
	}{
		{"highest receive position", []Candidate{cand(1, "a", "0/10", "0/10", nil), cand(2, "b", "0/20", "0/10", nil)}, 2},
		{"empty receive position uses replay", []Candidate{cand(1, "a", "", "0/30", nil), cand(2, "b", "0/20", "0/10", nil)}, 1},
		{"a tie goes to the synchronous standby", []Candidate{cand(1, "a", "0/10", "0/10", nil), cand(2, "b", "0/10", "0/10", sync)}, 2},
		{"then to the preferred server", []Candidate{cand(1, "a", "0/10", "0/10", nil), cand(2, "b", "0/10", "0/10", pref)}, 2},
		{"then to the nearest in the ring", []Candidate{cand(1, "a", "0/10", "0/10", func(c *Candidate) { c.RingDist = 2 }), cand(2, "b", "0/10", "0/10", func(c *Candidate) { c.RingDist = 1 })}, 2},
		{"then to the lowest id", []Candidate{cand(5, "a", "0/10", "0/10", nil), cand(3, "b", "0/10", "0/10", nil)}, 3},
		{"a standby on an older timeline never wins", []Candidate{cand(1, "a", "0/99", "0/99", func(c *Candidate) { c.Pos.Timeline = 2 }), cand(2, "b", "0/10", "0/10", nil)}, 2},
		{"a standby that is not in recovery is not eligible", []Candidate{cand(1, "a", "0/99", "0/99", func(c *Candidate) { c.Pos.InRecovery = false }), cand(2, "b", "0/10", "0/10", nil)}, 2},
	} {
		ch := Choose(ChooseInput{Candidates: tc.cands, Facts: goodFacts})
		if ch.Chosen == nil || ch.Chosen.StandbyID != tc.want {
			t.Errorf("%s: chose %+v, want %d", tc.name, ch.Chosen, tc.want)
		}
	}
}

func TestChooseIsAPureFunctionOfItsInput(t *testing.T) {
	in := ChooseInput{Candidates: []Candidate{cand(2, "b", "0/10", "0/10", nil), cand(1, "a", "0/10", "0/10", nil)}, Facts: goodFacts}
	a, b := Choose(in), Choose(in)
	if a.Chosen.StandbyID != b.Chosen.StandbyID || a.Chosen.StandbyID != 1 {
		t.Fatalf("%d %d", a.Chosen.StandbyID, b.Chosen.StandbyID)
	}
	if in.Candidates[0].StandbyID != 2 {
		t.Fatal("the input was reordered")
	}
}

func TestVerdicts(t *testing.T) {
	two := []Candidate{cand(1, "a", "0/20", "0/20", nil), cand(2, "b", "0/10", "0/10", nil)}
	for _, tc := range []struct {
		name   string
		cands  []Candidate
		target string
		facts  GroupFacts
		want   string
	}{
		{"a synchronous standby streamed, no async database", two, "", goodFacts, VerdictNone},
		{"an async database", two, "", GroupFacts{SyncStreaming: true, LastKnownAge: time.Second, AsyncDatabases: []string{"shop"}}, VerdictAsyncOnly},
		{"the group ran without a synchronous standby", two, "", GroupFacts{LastKnownAge: time.Second}, VerdictPossible},
		{"the last check is too old", two, "", GroupFacts{SyncStreaming: true, LastKnownAge: time.Minute}, VerdictPossible},
		{"the target is behind the best", two, "b", goodFacts, VerdictPossible},
		{"the target is the best", two, "a", goodFacts, VerdictNone},
		{"a ready standby cannot be read", append([]Candidate{{StandbyID: 3, Server: "c"}}, two...), "", goodFacts, VerdictUnknown},
		{"the target is not a candidate", two, "zzz", goodFacts, VerdictUnknown},
	} {
		ch := Choose(ChooseInput{Candidates: tc.cands, Target: tc.target, Facts: tc.facts})
		if ch.Verdict != tc.want {
			t.Errorf("%s: verdict %s (%s), want %s", tc.name, ch.Verdict, ch.Message, tc.want)
		}
	}
	if Accepts(VerdictPossible, false) || !Accepts(VerdictPossible, true) || !Accepts(VerdictNone, false) {
		t.Error("Accepts")
	}
}

func TestNoEligibleStandby(t *testing.T) {
	ch := Choose(ChooseInput{Candidates: []Candidate{{StandbyID: 1, Server: "a"}}, Facts: goodFacts})
	if ch.Chosen != nil || ch.Verdict != VerdictUnknown {
		t.Fatalf("%+v", ch)
	}
	if ch.Ineligible[1] == "" {
		t.Fatal("no reason")
	}
}
