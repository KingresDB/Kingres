// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package audit

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/testutil"
)

type tick struct {
	mu sync.Mutex
	t  time.Time
}

func (c *tick) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(time.Second)
	return c.t
}

func TestFileSinkAppendQueryAndRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempDir(t), "audit.jsonl")
	c := &tick{t: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	s, err := OpenFile(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	events := []Event{
		{Actor: "alice", Action: "login", Outcome: OutcomeSuccess, RequestID: "r1", Source: "panel"},
		{Actor: "bob", Action: "login", Outcome: OutcomeFailure, Source: "rest"},
		{Actor: "alice", Action: "createRole", Outcome: OutcomeSuccess, Source: "cli", Details: map[string]any{"operationId": "createRole"}},
		{Actor: "alice", Action: "dropRole", Outcome: OutcomeDenied, Source: "cli"},
	}
	for i, e := range events {
		id, err := s.Append(ctx, e)
		if err != nil || id != int64(i+1) {
			t.Fatalf("Append %d = %d, %v", i, id, err)
		}
	}
	got, err := s.Query(ctx, Filter{Limit: 10})
	if err != nil || len(got) != 4 || got[0].ID != 4 || got[3].ID != 1 {
		t.Fatalf("newest first: %v %v", got, err)
	}
	if got[3].RequestID != "r1" || got[3].Actor != "alice" || got[3].Timestamp.IsZero() {
		t.Errorf("stored event = %+v", got[3])
	}
	tests := []struct {
		name string
		f    Filter
		ids  []int64
	}{
		{"actor", Filter{Actor: "alice", Limit: 10}, []int64{4, 3, 1}},
		{"action", Filter{Action: "login", Limit: 10}, []int64{2, 1}},
		{"outcome", Filter{Outcome: OutcomeDenied, Limit: 10}, []int64{4}},
		{"below", Filter{Below: &Position{Time: c.t.Add(-time.Second), ID: 3}, Limit: 10}, []int64{2, 1}},
		{"limit plus one", Filter{Limit: 2}, []int64{4, 3, 2}},
		{"from", Filter{From: c.t.Add(-time.Second - time.Millisecond), Limit: 10}, []int64{4, 3}},
		{"to", Filter{To: c.t.Add(-time.Second), Limit: 10}, []int64{2, 1}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := s.Query(ctx, tc.f)
			if err != nil {
				t.Fatal(err)
			}
			var ids []int64
			for _, e := range got {
				ids = append(ids, e.ID)
			}
			if len(ids) != len(tc.ids) {
				t.Fatalf("ids = %v, want %v", ids, tc.ids)
			}
			for i := range ids {
				if ids[i] != tc.ids[i] {
					t.Fatalf("ids = %v, want %v", ids, tc.ids)
				}
			}
		})
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopening continues the id sequence and keeps the history; a torn last
	// line from a crash is skipped.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"id":5,"timest`)
	_ = f.Close()
	s2, err := OpenFile(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	id, err := s2.Append(ctx, Event{Actor: "x", Action: "a", Outcome: OutcomeSuccess})
	if err != nil || id != 5 {
		t.Errorf("id after reopen = %d, %v", id, err)
	}
	all, _ := s2.Query(ctx, Filter{Limit: 100})
	if len(all) != 5 {
		t.Errorf("events after reopen = %d", len(all))
	}
}

func TestFileSinkIsAppendOnly(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "a.jsonl")
	s, _ := OpenFile(path, time.Now)
	_, _ = s.Append(context.Background(), Event{Actor: "a", Action: "x", Outcome: OutcomeSuccess})
	s.Close()
	before, _ := os.ReadFile(path)
	s, _ = OpenFile(path, time.Now)
	_, _ = s.Append(context.Background(), Event{Actor: "b", Action: "y", Outcome: OutcomeSuccess})
	s.Close()
	after, _ := os.ReadFile(path)
	if len(after) <= len(before) || string(after[:len(before)]) != string(before) {
		t.Error("reopening must keep existing content and only append")
	}
}

func TestFileSinkConcurrentAppend(t *testing.T) {
	s, err := OpenFile(filepath.Join(testutil.TempDir(t), "a.jsonl"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Append(context.Background(), Event{Actor: "a", Action: "x", Outcome: OutcomeSuccess}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	got, _ := s.Query(context.Background(), Filter{Limit: 100})
	seen := map[int64]bool{}
	for _, e := range got {
		seen[e.ID] = true
	}
	if len(got) != 20 || len(seen) != 20 {
		t.Errorf("got %d events, %d distinct ids", len(got), len(seen))
	}
}

func TestCleanReplacesControlCharacters(t *testing.T) {
	if got := Clean("root\x00options\nx\x1b[0m"); strings.ContainsAny(got, "\x00\n\x1b") {
		t.Errorf("control characters survived: %q", got)
	}
	if got := Clean("alice"); got != "alice" {
		t.Errorf("a clean name changed: %q", got)
	}
}

// Rotation keeps every event reachable: a query reads the live file and then the
// closed segments, and a reopened sink continues the id sequence even when the
// live file is empty (CP2-16).
func TestFileSinkRotatesAndQueriesAcrossSegments(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempDir(t), "audit.jsonl")
	c := &tick{t: time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)}
	s, err := OpenFile(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	s.SetSegmentBytes(600)
	for i := 0; i < 30; i++ {
		if _, err := s.Append(ctx, Event{Actor: "alice", Action: "createRole", Outcome: OutcomeSuccess, Source: "cli"}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.segmentPaths()) < 2 {
		t.Fatalf("expected several closed segments, got %v", s.segmentPaths())
	}
	all, err := s.Query(ctx, Filter{})
	if err != nil || len(all) != 30 {
		t.Fatalf("all events = %d, %v", len(all), err)
	}
	for i, e := range all {
		if e.ID != int64(30-i) {
			t.Fatalf("event %d has id %d, want %d", i, e.ID, 30-i)
		}
	}
	page, _ := s.Query(ctx, Filter{Limit: 4, Below: &Position{Time: all[9].Timestamp, ID: all[9].ID}})
	if len(page) != 5 || page[0].ID != 20 || page[4].ID != 16 {
		t.Fatalf("page across a boundary = %+v", page)
	}
	_ = s.Close()

	// The live file may be empty right after a rotation: the next id still follows.
	s2, err := OpenFile(path, c.now)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if id, err := s2.Append(ctx, Event{Actor: "a", Action: "x", Outcome: OutcomeSuccess}); err != nil || id != 31 {
		t.Fatalf("id after reopen = %d, %v", id, err)
	}
}

// A page is read from the end of the file: lines longer than a read block and
// lines that straddle a block boundary come out intact and in order.
func TestFileSinkBackwardReadHandlesBlockBoundaries(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(testutil.TempDir(t), "audit.jsonl")
	s, err := OpenFile(path, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.SetSegmentBytes(0)
	big := strings.Repeat("x", 70<<10)
	const n = 12
	for i := 0; i < n; i++ {
		d := map[string]any{"pad": strings.Repeat("y", i*9000)}
		if i == 5 {
			d["big"] = big
		}
		if _, err := s.Append(ctx, Event{Actor: "a", Action: "x", Outcome: OutcomeSuccess, Details: d}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Query(ctx, Filter{})
	if err != nil || len(got) != n {
		t.Fatalf("got %d events, %v", len(got), err)
	}
	for i, e := range got {
		if e.ID != int64(n-i) {
			t.Fatalf("event %d id %d", i, e.ID)
		}
	}
	top, _ := s.Query(ctx, Filter{Limit: 2})
	if len(top) != 3 || top[0].ID != n {
		t.Fatalf("limit page = %d events", len(top))
	}
}

func TestReadyDetectsAnUnwritableLog(t *testing.T) {
	ctx := context.Background()
	s, err := OpenFile(filepath.Join(testutil.TempDir(t), "audit.jsonl"), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Ready(ctx); err != nil {
		t.Fatalf("a healthy log is not ready: %v", err)
	}
	if got, _ := s.Query(ctx, Filter{}); len(got) != 0 {
		t.Errorf("Ready must not leave an event, got %d", len(got))
	}
	_ = s.Close()
	if err := s.Ready(ctx); err == nil {
		t.Error("a closed log must not be ready")
	}
}
