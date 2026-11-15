// SPDX-License-Identifier: AGPL-3.0-only

package gather

import (
	"context"
	"errors"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
)

func ts(sec int) time.Time { return time.Date(2026, 11, 15, 10, 0, sec, 0, time.UTC) }

func TestFanoutCallsEveryServerInParallelWithItsOwnDeadline(t *testing.T) {
	peers := []Peer{{ID: "a", Ordinal: 1}, {ID: "b", Ordinal: 2}, {ID: "slow", Ordinal: 3}, {ID: "down", Ordinal: 4}, {ID: "bad", Ordinal: 5}}
	// Every call waits until all five are running: serial calls would never get there.
	var arrived atomic.Int32
	barrier := make(chan struct{})
	errSerial := errors.New("the calls did not run in parallel")
	start := time.Now()
	// The deadline is generous on purpose: on a loaded machine the five
	// goroutines must all start within it, or the barrier reports a false
	// "serial" failure.
	out := Fanout(context.Background(), peers, 2*time.Second, func(ctx context.Context, p Peer) ([]byte, error) {
		if arrived.Add(1) == int32(len(peers)) {
			close(barrier)
		}
		select {
		case <-barrier:
		case <-ctx.Done():
			return nil, errSerial
		}
		switch p.ID {
		case "slow":
			<-ctx.Done() // only the deadline ends it
			return nil, ctx.Err()
		case "down":
			return nil, ErrUnreachable
		case "bad":
			return nil, ErrInvalid
		}
		time.Sleep(100 * time.Millisecond)
		return []byte("ok-" + p.ID), nil
	})
	if took := time.Since(start); took > 6*time.Second {
		t.Errorf("took %v: the slow server held the others", took)
	}
	for _, o := range out {
		if errors.Is(o.Err, errSerial) {
			t.Fatal("the calls did not run in parallel")
		}
	}
	if len(out) != 5 {
		t.Fatalf("%d outcomes", len(out))
	}
	for i, p := range peers {
		if out[i].Peer.ID != p.ID {
			t.Errorf("outcome %d is for %s", i, out[i].Peer.ID)
		}
	}
	if string(out[0].Body) != "ok-a" || out[0].Err != nil || string(out[1].Body) != "ok-b" {
		t.Errorf("answers: %+v %+v", out[0], out[1])
	}
	want := map[string]string{"slow": ReasonTimeout, "down": ReasonUnreachable, "bad": ReasonInvalid}
	missing := MissingOf(out)
	if len(missing) != 3 {
		t.Fatalf("missing %+v", missing)
	}
	for _, m := range missing {
		if want[m.ServerID] != m.Reason {
			t.Errorf("%s: reason %q, want %q", m.ServerID, m.Reason, want[m.ServerID])
		}
	}
	if !sort.SliceIsSorted(missing, func(i, j int) bool { return missing[i].ServerID < missing[j].ServerID }) {
		t.Errorf("missing servers are not sorted: %+v", missing)
	}
}

func TestReasonsOfRefusals(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{context.DeadlineExceeded, ReasonTimeout},
		{ErrUnreachable, ReasonUnreachable},
		{ErrRefused, ReasonRefused},
		{ErrInvalid, ReasonInvalid},
		{errors.New("something else"), ReasonUnreachable},
	} {
		if got := ReasonOf(tc.err); got != tc.want {
			t.Errorf("%v: %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestLogKeysAndCursor(t *testing.T) {
	k := LogKey{Time: ts(5), Server: "db2", Rank: 3}
	c := EncodeLogCursor(k)
	got, err := DecodeLogCursor(c)
	if err != nil || got != k {
		t.Fatalf("round trip: %+v %v", got, err)
	}
	for _, bad := range []string{"!", "e30", "eyJ2IjoyfQ", "eyJ2IjoxLCJ0IjoiNSJ9"} {
		if _, err := DecodeLogCursor(bad); err == nil {
			t.Errorf("cursor %q was accepted", bad)
		}
	}
	if _, err := DecodeLogCursor(EncodeAlertCursor(AlertKey{Time: ts(1), ID: 7})); err == nil {
		t.Error("an alert cursor was accepted as a log cursor")
	}
}

func e(sec int, server, msg string) api.LogEntry {
	s := server
	return api.LogEntry{Timestamp: ts(sec), Level: "info", Component: "postgres", ServerID: &s, Message: msg}
}

// The merged page is the newest entries of every server in one total order and
// the cursor of the last entry continues exactly after it, whichever servers
// answered, including entries with the same timestamp on different servers.
func TestMergeLogsPagesAreStableAcrossServers(t *testing.T) {
	a := []api.LogEntry{e(9, "a", "a9"), e(7, "a", "a7"), e(7, "a", "a7b"), e(3, "a", "a3")}
	b := []api.LogEntry{e(9, "b", "b9"), e(8, "b", "b8"), e(7, "b", "b7"), e(1, "b", "b1")}
	c := []api.LogEntry{e(7, "c", "c7"), e(2, "c", "c2")}
	lists := [][]RankedLog{RankLogs(a), RankLogs(b), RankLogs(c)}
	var all []string
	var cursor *LogKey
	pages := 0
	for {
		items, next := MergeLogs(lists, cursor, 3)
		for _, it := range items {
			all = append(all, it.Message)
		}
		pages++
		if next == nil {
			break
		}
		cursor = next
		if pages > 10 {
			t.Fatal("paging does not end")
		}
	}
	want := []string{"b9", "a9", "b8", "c7", "b7", "a7", "a7b", "a3", "c2", "b1"}
	if len(all) != len(want) {
		t.Fatalf("got %v, want %v", all, want)
	}
	for i := range want {
		if all[i] != want[i] {
			t.Fatalf("got %v, want %v", all, want)
		}
	}
	if pages != 4 {
		t.Errorf("%d pages for 10 entries of 3", pages)
	}
}

func TestMergeLogsWhenAServerIsMissingTheOthersStillPage(t *testing.T) {
	lists := [][]RankedLog{RankLogs([]api.LogEntry{e(9, "a", "a9"), e(5, "a", "a5")}), nil}
	items, next := MergeLogs(lists, nil, 10)
	if len(items) != 2 || next != nil {
		t.Errorf("%v %v", items, next)
	}
}

func TestAlertMergeAndKeys(t *testing.T) {
	a := []api.Alert{{ID: 1<<32 | 5, RaisedAt: ts(9)}, {ID: 1<<32 | 2, RaisedAt: ts(4)}}
	b := []api.Alert{{ID: 2<<32 | 1, RaisedAt: ts(9)}, {ID: 2<<32 | 7, RaisedAt: ts(6)}}
	items, next := MergeAlerts([][]api.Alert{a, b}, nil, 3)
	if len(items) != 3 || next == nil {
		t.Fatalf("%v %v", items, next)
	}
	// Newest first; the same time: the larger id first.
	if items[0].ID != 2<<32|1 || items[1].ID != 1<<32|5 || items[2].ID != 2<<32|7 {
		t.Errorf("order %v", []int64{items[0].ID, items[1].ID, items[2].ID})
	}
	rest, end := MergeAlerts([][]api.Alert{a, b}, next, 3)
	if len(rest) != 1 || rest[0].ID != 1<<32|2 || end != nil {
		t.Errorf("second page %v %v", rest, end)
	}
	k, err := DecodeAlertCursor(EncodeAlertCursor(*next))
	if err != nil || k != *next {
		t.Errorf("cursor round trip %+v %v", k, err)
	}
	if AlertID(3, 9) != 3<<32|9 {
		t.Error("alert id composition")
	}
	if o, l := SplitAlertID(AlertID(3, 9)); o != 3 || l != 9 {
		t.Errorf("split %d %d", o, l)
	}
	if AlertID(MaxAlertOrdinal, 1<<32-1) >= 1<<53 {
		t.Error("an alert id must stay below 2^53 (the panel reads JSON numbers as doubles)")
	}
}

// The answering server returns only what comes after the cursor, with the ranks
// of the whole list, so that the merging server's keys agree with its own.
func TestAfterLogKeepsTheRanksOfTheWholeList(t *testing.T) {
	list := RankLogs([]api.LogEntry{e(9, "a", "x"), e(7, "a", "y1"), e(7, "a", "y2"), e(7, "a", "y3"), e(3, "a", "z")})
	if list[1].Rank != 0 || list[2].Rank != 1 || list[3].Rank != 2 || list[4].Rank != 0 {
		t.Fatalf("ranks %+v", list)
	}
	after := AfterLog(list, &LogKey{Time: ts(7), Server: "a", Rank: 1}, 10)
	if len(after) != 2 || after[0].Entry.Message != "y3" || after[0].Rank != 2 || after[1].Entry.Message != "z" {
		t.Errorf("after the cursor: %+v", after)
	}
	if got := AfterLog(list, nil, 2); len(got) != 2 {
		t.Errorf("limit: %+v", got)
	}
}

// A page of no entries is empty and has no cursor, whatever the lists hold.
func TestMergeWithoutRoomGivesNothing(t *testing.T) {
	list := RankLogs([]api.LogEntry{e(9, "a", "x")})
	if items, next := MergeLogs([][]RankedLog{list}, nil, 0); len(items) != 0 || next != nil || items == nil {
		t.Errorf("logs: %v %v", items, next)
	}
	if items, next := MergeAlerts([][]api.Alert{{{ID: 1, RaisedAt: ts(1)}}}, nil, 0); len(items) != 0 || next != nil || items == nil {
		t.Errorf("alerts: %v %v", items, next)
	}
}
