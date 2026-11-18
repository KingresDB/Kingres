// SPDX-License-Identifier: AGPL-3.0-only

package failover

import (
	"context"
	"errors"
	"testing"
	"time"
)

// scripted is a Catalog that answers promote_node from a script and counts calls.
type scripted struct {
	answers   []any // PromoteResult or error
	calls     int
	recovered int
}

func (s *scripted) Databases(context.Context) ([]string, error)         { return []string{SystemDB}, nil }
func (s *scripted) Nodes(context.Context, string) ([]Node, error)       { return nil, nil }
func (s *scripted) Status(context.Context, string) (int64, bool, error) { return 1, false, nil }
func (s *scripted) RecoverPrepared(context.Context) error               { s.recovered++; return nil }
func (s *scripted) CheckCluster(context.Context) ([]Incident, error)    { return nil, nil }
func (s *scripted) PromoteNode(context.Context, string, int32) (PromoteResult, error) {
	i := s.calls
	s.calls++
	if i >= len(s.answers) {
		return PromoteResult{}, errors.New("script ended")
	}
	switch a := s.answers[i].(type) {
	case PromoteResult:
		return a, nil
	case error:
		return PromoteResult{}, a
	}
	panic("bad script")
}

func fast(o PromoteOptions) PromoteOptions {
	o.Pause = time.Millisecond
	o.Sleep = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	o.NewNode = 7
	return o
}

var already = PromoteResult{Status: "already", Term: 9, Final: true}

func sql(code, text string) error { return &SQLError{Code: code, Text: text} }

func TestPromoteLoopAnswers(t *testing.T) {
	foreign := PromoteResult{Status: "promoted", Blocked: []Blocked{{Gid: "mine", Node: 2, Class: "foreign"}}}
	probeYes := func(context.Context) bool { return true }
	probeNo := func(context.Context) bool { return false }
	many := func() []any {
		var s []any
		for i := 0; i < 20; i++ {
			s = append(s, PromoteResult{Status: "promoted", Term: 8})
		}
		return s
	}()
	for _, tc := range []struct {
		name      string
		script    []any
		opts      PromoteOptions
		wantCalls int
		wantErr   string // "", "pause", or a task error code
		wantRec   int
		wantTerm  int64
		wantList  int
	}{
		{"already at once", []any{already}, PromoteOptions{}, 1, "", 0, 9, 0},
		{"promoted not final, then completed, then already", []any{PromoteResult{Status: "promoted", Term: 8}, PromoteResult{Status: "completed", Term: 9}, already}, PromoteOptions{}, 3, "", 0, 9, 0},
		{"already but not final is not done", []any{PromoteResult{Status: "already", Term: 8}, already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"unreached nodes are carried", []any{PromoteResult{Status: "already", Term: 9, Final: true, Unreached: []int32{4}}}, PromoteOptions{}, 1, "", 0, 9, 0},
		{"a cluster prepared transaction: recovery then repeat", []any{PromoteResult{Status: "promoted", Blocked: []Blocked{{Gid: "kingres_h1", Node: 2, Class: "cluster"}}}, already}, PromoteOptions{}, 2, "", 1, 9, 0},
		{"a foreign prepared transaction twice in a row asks", []any{foreign, foreign}, PromoteOptions{}, 2, "pause", 1, 0, 0},
		{"a foreign prepared transaction that went away", []any{foreign, already}, PromoteOptions{}, 2, "", 1, 9, 0},
		{"still in recovery, then done", []any{sql("KS011", "node 7 is still in recovery"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"cannot connect, then done", []any{sql("08001", "could not connect"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"catalog busy, then done", []any{sql("KS008", "another call"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"a lock held on a node, then done", []any{sql("55P03", "lock not available"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"KS005 of a participant, then done", []any{sql("KS005", "did not confirm that its part is durable"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"a broken connection is repeated", []any{errors.New("conn closed"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"a client time-out is repeated", []any{context.DeadlineExceeded, already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"cannot commit on the new primary: the list is applied again", []any{sql("KS009", "node 7 (h:1) cannot commit: its synchronous standbys do not confirm"), already}, PromoteOptions{}, 2, "", 0, 9, 1},
		{"cannot commit on another primary is waited for", []any{sql("KS009", "node 3 (h:2) cannot commit: its synchronous standbys do not confirm"), already}, PromoteOptions{}, 2, "", 0, 9, 0},
		{"KS014 is never repeated", []any{sql("KS014", "holds a cluster secret that no node has"), already}, PromoteOptions{}, 1, "pause", 0, 0, 0},
		{"KS012 asks", []any{sql("KS012", "holds catalog term 12"), already}, PromoteOptions{}, 1, "pause", 0, 0, 0},
		{"the wrong node", []any{sql("KS010", "this function can only run on the map group primary"), already}, PromoteOptions{}, 1, CodeNotMapPrimary, 0, 0, 0},
		{"a node that does not exist", []any{sql("42704", "node 7 does not exist")}, PromoteOptions{}, 1, CodeBadPromoteTarget, 0, 0, 0},
		{"KS007 and every node answers the probe: one repeat", []any{sql("KS007", "node 2 did not answer"), already}, PromoteOptions{Probe: probeYes}, 2, "", 0, 9, 0},
		{"KS007 and a node is silent: asks, never repeats", []any{sql("KS007", "node 2 did not answer"), already}, PromoteOptions{Probe: probeNo}, 1, "pause", 0, 0, 0},
		{"KS007 without a probe asks", []any{sql("KS007", "node 2 did not answer"), already}, PromoteOptions{}, 1, "pause", 0, 0, 0},
		{"an unknown refusal asks", []any{sql("55000", "something else")}, PromoteOptions{}, 1, "pause", 0, 0, 0},
		{"the count of calls is bounded", many, PromoteOptions{MaxCalls: 4}, 4, "pause", 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cat := &scripted{answers: tc.script}
			reapplied := 0
			o := fast(tc.opts)
			o.ReapplyList = func(context.Context) error { reapplied++; return nil }
			out, err := RunPromoteNode(context.Background(), cat, o)
			if cat.calls != tc.wantCalls {
				t.Errorf("%d calls, want %d", cat.calls, tc.wantCalls)
			}
			if cat.recovered != tc.wantRec {
				t.Errorf("%d recoveries, want %d", cat.recovered, tc.wantRec)
			}
			if reapplied != tc.wantList {
				t.Errorf("the list was applied %d times, want %d", reapplied, tc.wantList)
			}
			var pe *PauseError
			var fe *Error
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("unexpected %v", err)
			case tc.wantErr == "pause" && !errors.As(err, &pe):
				t.Errorf("want a pause, got %v", err)
			case tc.wantErr != "" && tc.wantErr != "pause" && (!errors.As(err, &fe) || fe.Code != tc.wantErr):
				t.Errorf("want %s, got %v", tc.wantErr, err)
			}
			if err == nil && out.Term != tc.wantTerm {
				t.Errorf("term %d", out.Term)
			}
		})
	}
}

func TestPromoteLoopWaitIsBounded(t *testing.T) {
	now := time.Unix(0, 0)
	o := fast(PromoteOptions{WaitBound: time.Minute, MaxCalls: 1000})
	o.Now = func() time.Time { return now }
	o.Sleep = func(ctx context.Context, d time.Duration) error { now = now.Add(10 * time.Second); return ctx.Err() }
	cat := &scripted{}
	for i := 0; i < 100; i++ {
		cat.answers = append(cat.answers, &SQLError{Code: "KS011", Text: "still in recovery"})
	}
	_, err := RunPromoteNode(context.Background(), cat, o)
	var fe *Error
	if !errors.As(err, &fe) || fe.Code != CodePromoteIncomplete {
		t.Fatalf("got %v after %d calls", err, cat.calls)
	}
	if cat.calls > 10 {
		t.Fatalf("%d calls for a wait bound of 6 pauses", cat.calls)
	}
}

func TestPromoteLoopBudget(t *testing.T) {
	now := time.Unix(0, 0)
	o := fast(PromoteOptions{Budget: time.Minute})
	o.Now = func() time.Time { return now }
	o.Sleep = func(ctx context.Context, d time.Duration) error { now = now.Add(20 * time.Second); return ctx.Err() }
	cat := &scripted{}
	for i := 0; i < 50; i++ {
		cat.answers = append(cat.answers, PromoteResult{Status: "promoted"})
	}
	_, err := RunPromoteNode(context.Background(), cat, o)
	var pe *PauseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
}
