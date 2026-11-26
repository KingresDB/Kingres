// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package internalapi

import (
	"context"
	"net/http/httptest"
	"testing"
)

// A server whose instance is held back cannot know the leader; that is "not yet" (503 with
// Retry-After, which a task step waits for), never "not you" (403 not_leader).
func TestHeldBackServerAnswers503NotNotLeader(t *testing.T) {
	held := true
	s := New(nil, Backend{Leader: func() string { return "" }, Unavailable: func() bool { return held }}, nil, nil)
	if !s.notReady(context.Background()) {
		t.Fatal("a held-back server with no known leader must be not ready")
	}
	w := httptest.NewRecorder()
	failNotReady(w)
	if w.Code != 503 || w.Header().Get("Retry-After") == "" || !contains(w.Body.String(), `"service_unavailable"`) {
		t.Fatalf("answer: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	held = false
	if s.notReady(context.Background()) {
		t.Fatal("a server that is not held back refuses with not_leader as before")
	}
	held = true
	s.be.Leader = func() string { return "s1" }
	if s.notReady(context.Background()) {
		t.Fatal("a server that knows its leader is ready")
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
