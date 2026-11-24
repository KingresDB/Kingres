// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

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

// Phase 5 review M3: a declaration that releases nothing is an error, never an empty success: no server says its primary is held back, the held-back server does not answer,
// the held-back server refuses.
func TestADeclarationThatReleasesNothingIsAnError(t *testing.T) {
	ctx := context.Background()
	rig := func(held func(server string) (bool, error), resume func(server string) (mapResumeAnswer, error)) *mapFailover {
		m, _ := abandonRig(t)
		m.self = "s3"
		m.memberList = func(context.Context) ([]string, error) { return []string{"s1", "s2", "s3", "s4"}, nil }
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
				a, err := resume(server)
				if err != nil {
					return err
				}
				b, _ := json.Marshal(a)
				return json.Unmarshal(b, out)
			}
			b, _ := json.Marshal(mapAbandonAnswer{})
			return json.Unmarshal(b, out)
		}
		return m
	}
	nobody := func(string) (bool, error) { return false, nil }
	never := func(string) (mapResumeAnswer, error) { return mapResumeAnswer{}, errors.New("not called") }
	// nobody holds its primary back (the old leader was restarted and is not marked yet)
	m := rig(nobody, never)
	if res, err := m.Abandon(ctx, "ops", opID, "x", "s9"); err == nil || problemStatus(err) != http.StatusServiceUnavailable {
		t.Errorf("nothing was held back and the answer was %+v %v", res, err)
	}
	// the held-back server does not answer the question
	m = rig(func(s string) (bool, error) { return false, errors.New("timeout") }, never)
	if _, err := m.Abandon(ctx, "ops", opID, "x", "s9"); err == nil || problemStatus(err) != http.StatusServiceUnavailable {
		t.Errorf("no server answered and the answer was %v", err)
	}
	// it answers and then fails the resume call
	m = rig(func(s string) (bool, error) { return s == "s4", nil }, func(string) (mapResumeAnswer, error) { return mapResumeAnswer{}, errors.New("refused") })
	if _, err := m.Abandon(ctx, "ops", opID, "x", "s9"); err == nil || problemStatus(err) != http.StatusServiceUnavailable {
		t.Errorf("the resume call failed and the answer was %v", err)
	}
	// it refuses with a reason
	m = rig(func(s string) (bool, error) { return s == "s4", nil }, func(string) (mapResumeAnswer, error) {
		return mapResumeAnswer{Detail: "no standby of the map group could be shown to be in recovery"}, nil
	})
	if _, err := m.Abandon(ctx, "ops", opID, "x", "s9"); err == nil || problemStatus(err) != http.StatusConflict || !strings.Contains(err.Error(), "no standby of the map group") {
		t.Errorf("the refusal of the old leader did not reach the operator: %v", err)
	}
	// without a declaration nothing held back is still an empty answer (the operator asked every server to try)
	m = rig(nobody, never)
	if res, err := m.Abandon(ctx, "ops", opID, "x", ""); err != nil || res.ResumedServer != "" {
		t.Errorf("without a declaration: %+v %v", res, err)
	}
	// and a release is still an answer
	m = rig(func(s string) (bool, error) { return s == "s4", nil }, func(string) (mapResumeAnswer, error) { return mapResumeAnswer{Resumed: true}, nil })
	if res, err := m.Abandon(ctx, "ops", opID, "x", "s9"); err != nil || res.ResumedServer != "s4" {
		t.Errorf("a release: %+v %v", res, err)
	}
}
