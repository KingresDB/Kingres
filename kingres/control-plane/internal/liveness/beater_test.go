// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package liveness

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/clock"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ha"
)

func TestBeaterRecordsAnswersAndDoesNotPileUp(t *testing.T) {
	tr := New(clock.NewReal(), Config{Interval: 10 * time.Millisecond, Suspect: time.Hour, Failure: 2 * time.Hour}, "a")
	var mu sync.Mutex
	calls := map[string]int{}
	release := make(chan struct{})
	b := &Beater{Tracker: tr, Interval: 10 * time.Millisecond, Timeout: time.Minute,
		Peers: func() []string { return []string{"a", "ok", "hung", "bad"} },
		Send: func(ctx context.Context, s string) error {
			mu.Lock()
			calls[s]++
			mu.Unlock()
			switch s {
			case "hung":
				select {
				case <-release:
				case <-ctx.Done():
				}
				return ctx.Err()
			case "bad":
				return errors.New("unreachable")
			}
			return nil
		}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { b.Run(ctx); close(done) }()
	deadline := time.Now().Add(10 * time.Second)
	for tr.State("ok") != ha.PeerLive || func() bool { mu.Lock(); defer mu.Unlock(); return calls["bad"] < 3 || calls["hung"] < 1 }() {
		if time.Now().After(deadline) {
			t.Fatal("the beater did not call the servers")
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	hung, self := calls["hung"], calls["a"]
	mu.Unlock()
	if hung != 1 {
		t.Fatalf("a call in flight must not be repeated, %d calls", hung)
	}
	if self != 0 {
		t.Fatal("a server does not send itself heartbeats")
	}
	if tr.State("bad") == ha.PeerLive {
		t.Fatal("a failed call is no heartbeat")
	}
	cancel()
	close(release)
	<-done
}
