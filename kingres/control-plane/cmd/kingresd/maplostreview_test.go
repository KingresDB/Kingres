// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/failover"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

// Phase 5 review M2: the declared server must be positively silent. "suspect" and "unknown" are not enough, it is asked as well (an answer refuses the release: a promoted
// standby or a bootstrap that names the old leader is named, any other answer says that it is heard), and the state is looked at again right before the hold ends.
func TestTheLostServerMustBeSeenDeadAndSilentRightBeforeTheHoldEnds(t *testing.T) {
	ctx := context.Background()
	in := mapBootstrapStatus{Standby: "in_recovery"}
	down := errors.New("down")
	for _, live := range []string{"live", "suspect", "unknown"} {
		r := newResolveRig(t, map[string]any{"s2": down, "s3": in})
		r.cand = "s2"
		r.m.liveOf = func(context.Context, string) string { return live }
		if got, why := r.m.resolveHeldBack(ctx, true, "s2"); got || !strings.Contains(why, live) || r.ended != 0 {
			t.Errorf("a server that is %s was released: %v %q ended %d", live, got, why, r.ended)
		}
	}
	// the lost server answers: promoted, a bootstrap of its own, or just heard
	for name, ans := range map[string]mapBootstrapStatus{
		"promoted": {Standby: "primary"},
		"active":   {Active: true, Standby: "in_recovery"},
		"heard":    {Standby: "in_recovery"},
	} {
		r := newResolveRig(t, map[string]any{"s2": ans, "s3": in})
		r.cand = "s2"
		r.m.liveOf = func(context.Context, string) string { return "dead" }
		if got, why := r.m.resolveHeldBack(ctx, true, "s2"); got || why == "" || r.ended != 0 {
			t.Errorf("the lost server answered (%s) and the hold was released: %v %q ended %d", name, got, why, r.ended)
		}
	}
	// it is heard while the others are asked: the second look refuses
	r := newResolveRig(t, map[string]any{"s2": down, "s3": in})
	r.cand = "s2"
	looks := 0
	r.m.liveOf = func(context.Context, string) string {
		looks++
		if looks == 1 {
			return "dead"
		}
		return "live"
	}
	if got, why := r.m.resolveHeldBack(ctx, true, "s2"); got || !strings.Contains(why, "any more") || r.ended != 0 || looks != 2 {
		t.Errorf("a server that was heard before the end was released: %v %q ended %d looks %d", got, why, r.ended, looks)
	}
	// the operator's call is refused as well for a server that is not seen dead
	m, _ := abandonRig(t)
	m.self = "s3"
	m.liveOf = func(context.Context, string) string { return "unknown" }
	if _, err := m.Abandon(ctx, "ops", opID, "x", "s2"); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Errorf("a server that is not yet known as dead was declared lost: %v", err)
	}
}

func problemStatus(err error) int {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Status
	}
	return 0
}

// declareRig is a member s3 that received the operator's declaration. The catalog names s4 as the primary of the map group (the old leader); held and resume are the answers of
// the servers, and sent records the servers that were sent a resume call.
type declareRig struct {
	m    *mapFailover
	sent []string
}

func newDeclareRig(t *testing.T, held func(server string) (bool, error), resume func(server string, body mapResumeBody) (mapResumeAnswer, error)) *declareRig {
	t.Helper()
	m, _ := abandonRig(t)
	r := &declareRig{m: m}
	m.self = "s3"
	m.memberList = func(context.Context) ([]string, error) { return []string{"s1", "s2", "s3", "s4"}, nil }
	m.oldLeaderOf = func(context.Context) (string, error) { return "s4", nil }
	m.heldBack = func() bool { return false }
	m.liveOf = func(context.Context, string) string { return "dead" }
	m.callPeer = func(_ context.Context, server, method, path string, in, out any) error {
		switch path {
		case "/failover/map/held":
			h, err := held(server)
			if err != nil {
				return err
			}
			b, _ := json.Marshal(mapHeldAnswer{Held: h})
			return json.Unmarshal(b, out)
		case "/failover/map/resume":
			r.sent = append(r.sent, server)
			a, err := resume(server, in.(mapResumeBody))
			if err != nil {
				return err
			}
			b, _ := json.Marshal(a)
			return json.Unmarshal(b, out)
		}
		b, _ := json.Marshal(mapAbandonAnswer{})
		return json.Unmarshal(b, out)
	}
	return r
}

// Phase 5 review M3, F4, F6, F10: a declaration that releases nothing is an error that says why, a refusal of the old leader is a 409 and not a request to retry, a rerun after a
// release says that nothing is held back, and the proof goes to the server that the catalog names, never to another that only claims a hold.
func TestADeclarationThatReleasesNothingIsAnError(t *testing.T) {
	ctx := context.Background()
	yes := func(string) (bool, error) { return true, nil }
	never := func(string, mapResumeBody) (mapResumeAnswer, error) {
		return mapResumeAnswer{}, errors.New("not called")
	}
	call := func(r *declareRig) (error, int) {
		_, err := r.m.Abandon(ctx, "ops", opID, "x", "s9")
		return err, problemStatus(err)
	}
	// the old leader reports no hold (it was restarted and is not marked, or it was released by an earlier call): 503, and the text says it may be released already
	r := newDeclareRig(t, func(string) (bool, error) { return false, nil }, never)
	if err, st := call(r); err == nil || st != http.StatusServiceUnavailable || !strings.Contains(err.Error(), "already") {
		t.Errorf("no hold: %v %d", err, st)
	}
	// it does not answer the question
	r = newDeclareRig(t, func(string) (bool, error) { return false, errors.New("timeout") }, never)
	if err, st := call(r); err == nil || st != http.StatusServiceUnavailable {
		t.Errorf("no answer: %v %d", err, st)
	}
	// the call is lost on the way
	r = newDeclareRig(t, yes, func(string, mapResumeBody) (mapResumeAnswer, error) {
		return mapResumeAnswer{}, errors.New("connection reset")
	})
	if err, st := call(r); err == nil || st != http.StatusServiceUnavailable {
		t.Errorf("transport error: %v %d", err, st)
	}
	// the old leader refuses the proof (a stale cache): 409 with its message, not "retry"
	r = newDeclareRig(t, yes, func(string, mapResumeBody) (mapResumeAnswer, error) {
		return mapResumeAnswer{}, &failover.CallError{Status: http.StatusUnauthorized, Code: "identity_unproven", Message: "The identity of the declaration is not proven."}
	})
	if err, st := call(r); err == nil || st != http.StatusConflict || !strings.Contains(err.Error(), "not proven") || !strings.Contains(err.Error(), "-release-map-hold") {
		t.Errorf("a refusal of the proof: %v %d", err, st)
	}
	// it refuses with a reason
	r = newDeclareRig(t, yes, func(string, mapResumeBody) (mapResumeAnswer, error) {
		return mapResumeAnswer{Detail: "no standby of the map group could be shown to be in recovery"}, nil
	})
	if err, st := call(r); err == nil || st != http.StatusConflict || !strings.Contains(err.Error(), "no standby of the map group") {
		t.Errorf("a refusal with a reason: %v %d", err, st)
	}
	// the catalog cannot be read here
	r = newDeclareRig(t, yes, never)
	r.m.oldLeaderOf = func(context.Context) (string, error) { return "", errors.New("no copy") }
	if err, st := call(r); err == nil || st != http.StatusServiceUnavailable || len(r.sent) != 0 {
		t.Errorf("an unreadable catalog: %v %d sent %v", err, st, r.sent)
	}
	// the declared server is the primary of the catalog: it is a leader that is gone, not a candidate
	r = newDeclareRig(t, yes, never)
	r.m.oldLeaderOf = func(context.Context) (string, error) { return "s9", nil }
	if err, st := call(r); err == nil || st != http.StatusConflict || len(r.sent) != 0 {
		t.Errorf("the declared server is the primary: %v %d", err, st)
	}
	// a member that claims a hold gets nothing: only the server that the catalog names is asked and sent the proof
	r = newDeclareRig(t, yes, func(server string, b mapResumeBody) (mapResumeAnswer, error) {
		if server != "s4" || b.Proof == nil {
			t.Errorf("the declaration was sent to %s (proof %v)", server, b.Proof != nil)
		}
		return mapResumeAnswer{Resumed: true}, nil
	})
	if res, err := r.m.Abandon(ctx, "ops", opID, "x", "s9"); err != nil || res.ResumedServer != "s4" || len(r.sent) != 1 {
		t.Errorf("a release: %+v %v sent %v", res, err, r.sent)
	}
	// without a declaration nothing held back is still an empty answer (the operator asked every server to try)
	r = newDeclareRig(t, yes, func(string, mapResumeBody) (mapResumeAnswer, error) { return mapResumeAnswer{}, nil })
	if res, err := r.m.Abandon(ctx, "ops", opID, "x", ""); err != nil || res.ResumedServer != "" {
		t.Errorf("without a declaration: %+v %v", res, err)
	}
}
