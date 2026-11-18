// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"errors"
	"testing"
)

type recordingControl struct {
	started, stopped, restarted []int
	refuse                      map[int]error
}

func (r *recordingControl) Running(context.Context, int) (bool, error) { return false, nil }
func (r *recordingControl) Start(_ context.Context, id int) error {
	r.started = append(r.started, id)
	return r.refuse[id]
}
func (r *recordingControl) Stop(_ context.Context, id int) error {
	r.stopped = append(r.stopped, id)
	return r.refuse[id]
}
func (r *recordingControl) Restart(_ context.Context, id int) error {
	r.restarted = append(r.restarted, id)
	return r.refuse[id]
}

// FO-16: a primary that is held back (its lease) does not keep the other primaries of the server from being
// started, stopped or restarted; the first refusal is still reported.
func TestEveryPrimaryIsTriedWhenOneRefuses(t *testing.T) {
	held := errors.New("held back by its lease")
	rc := &recordingControl{refuse: map[int]error{1: held}}
	p := primaries{inner: rc, ids: func() []int { return []int{1, 101} }}
	ctx := context.Background()
	if err := p.Start(ctx, 1); !errors.Is(err, held) {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(ctx, 1); !errors.Is(err, held) {
		t.Fatalf("Stop: %v", err)
	}
	if err := p.Restart(ctx, 1); !errors.Is(err, held) {
		t.Fatalf("Restart: %v", err)
	}
	for name, got := range map[string][]int{"start": rc.started, "stop": rc.stopped, "restart": rc.restarted} {
		if len(got) != 2 || got[1] != 101 {
			t.Errorf("%s did not reach the second primary: %v", name, got)
		}
	}
}
