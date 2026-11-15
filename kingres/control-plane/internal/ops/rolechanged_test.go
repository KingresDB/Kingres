// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"testing"
)

type recordingFanout struct {
	mu    sync.Mutex
	calls []string
}

func (f *recordingFanout) Others(context.Context) ([]OtherServer, error) {
	return []OtherServer{{ID: "db2"}, {ID: "db3"}}, nil
}
func (f *recordingFanout) Apply(_ context.Context, server, kind string, params any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := json.Marshal(params)
	f.calls = append(f.calls, server+" "+kind+" "+string(b))
	return nil
}
func (f *recordingFanout) Register(context.Context, string, OtherServer) error { return nil }

// CP-02: a role change ends the role's sessions here and on every other server.
func TestRoleChangedEndsSessionsEverywhere(t *testing.T) {
	var ended []string
	f := &recordingFanout{}
	o := &Ops{Fanout: f, Log: slog.New(slog.DiscardHandler), SessionEnder: func(r string) { ended = append(ended, r) }}
	o.roleChanged(context.Background(), "carol")
	if len(ended) != 1 || ended[0] != "carol" {
		t.Fatalf("local sessions: %v", ended)
	}
	want := []string{`db2 role.changed {"name":"carol"}`, `db3 role.changed {"name":"carol"}`}
	if len(f.calls) != 2 || f.calls[0] != want[0] || f.calls[1] != want[1] {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestApplyRoleChangedLocalEndsSessionsWithoutBroadcast(t *testing.T) {
	var ended []string
	f := &recordingFanout{}
	o := &Ops{Fanout: f, Log: slog.New(slog.DiscardHandler), SessionEnder: func(r string) { ended = append(ended, r) }}
	if err := o.ApplyLocal(context.Background(), ApplyRoleChanged, json.RawMessage(`{"name":"carol"}`)); err != nil {
		t.Fatal(err)
	}
	if len(ended) != 1 || len(f.calls) != 0 {
		t.Fatalf("ended %v calls %v", ended, f.calls)
	}
	for _, bad := range []string{`{}`, `{"name":""}`, `{"name":"a","x":1}`, `[]`} {
		if err := o.ApplyLocal(context.Background(), ApplyRoleChanged, json.RawMessage(bad)); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}
