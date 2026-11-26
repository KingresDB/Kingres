// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// pinSpy records the pin that reaches the membership logic.
type pinSpy struct {
	fakeMembers
	calls int
	pin   string
}

func (p *pinSpy) AddServer(_ context.Context, actor string, _ api.AddServerRequest, pin string) (task.Task, error) {
	p.calls++
	p.pin = pin
	return task.Task{ID: 7, Kind: "server.add", State: task.Pending, CreatedBy: actor}, nil
}

const (
	pinA = "aabbccddeeff00112233445566778899aabbccddeeff00112233445566778899"
	pinB = "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"
)

// CP2-03: the pin of addServer survives forwarding (body and header), a
// malformed pin is refused on every server, and a request without a pin is
// refused unless the administrator accepted that (CP2-05).
func TestAddServerPinThroughAFollower(t *testing.T) {
	spy := &pinSpy{}
	fw := &direct{leader: "leader-1"}
	leader := newEnv(t, func(d *Deps) { d.Members = spy })
	leader.initialize()
	fw.target = leader.srv
	leader.testUser([]string{auth.RoleAdmin})
	follower := newEnv(t, func(d *Deps) { d.Forwarder = fw })
	follower.initialize()
	base := func() map[string]any { return map[string]any{"address": "db9.example.com:8091", "setupToken": "tok"} }
	do := func(srv *env, body map[string]any, header map[string]string) *httptest.ResponseRecorder {
		return srv.do(req{method: "POST", path: "/api/v1/servers", bearer: srv.token(auth.RoleAdmin), body: body, header: header})
	}
	colons := func(s string) string {
		var parts []string
		for i := 0; i < len(s); i += 2 {
			parts = append(parts, strings.ToUpper(s[i:i+2]))
		}
		return strings.Join(parts, ":")
	}

	type tc struct {
		name    string
		viaFol  bool
		body    func(map[string]any)
		header  map[string]string
		status  int
		wantPin string
		reaches bool
	}
	for _, c := range []tc{
		{"leader, body pin", false, func(b map[string]any) { b["expectedFingerprint"] = pinA }, nil, 202, pinA, true},
		{"follower, body pin", true, func(b map[string]any) { b["expectedFingerprint"] = pinA }, nil, 202, pinA, true},
		{"follower, body pin with colons and capitals", true, func(b map[string]any) { b["expectedFingerprint"] = colons(pinA) }, nil, 202, pinA, true},
		{"follower, header pin", true, nil, map[string]string{HeaderPin: pinB}, 202, pinB, true},
		{"follower, both agree", true, func(b map[string]any) { b["expectedFingerprint"] = pinA }, map[string]string{HeaderPin: pinA}, 202, pinA, true},
		{"follower, both differ", true, func(b map[string]any) { b["expectedFingerprint"] = pinA }, map[string]string{HeaderPin: pinB}, 400, "", false},
		{"follower, malformed header", true, nil, map[string]string{HeaderPin: "not-a-fingerprint"}, 400, "", false},
		{"follower, malformed body", true, func(b map[string]any) { b["expectedFingerprint"] = "12345" }, nil, 400, "", false},
		{"leader, malformed header", false, nil, map[string]string{HeaderPin: "zz"}, 400, "", false},
		{"follower, no pin", true, nil, nil, 400, "", false},
		{"leader, no pin", false, nil, nil, 400, "", false},
		{"follower, unpinned accepted", true, func(b map[string]any) { b["allowUnpinned"] = true }, nil, 202, "", true},
		{"follower, unpinned false", true, func(b map[string]any) { b["allowUnpinned"] = false }, nil, 400, "", false},
	} {
		spy.calls, spy.pin = 0, ""
		body := base()
		if c.body != nil {
			c.body(body)
		}
		srv := leader
		if c.viaFol {
			srv = follower
		}
		w := do(srv, body, c.header)
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d: %s", c.name, w.Code, c.status, w.Body.String())
			continue
		}
		if c.reaches != (spy.calls == 1) || spy.pin != c.wantPin {
			t.Errorf("%s: reached=%v pin=%q, want reached=%v pin=%q", c.name, spy.calls == 1, spy.pin, c.reaches, c.wantPin)
		}
		if c.status == 400 && !strings.Contains(w.Body.String(), "validation_failed") {
			t.Errorf("%s: not validation_failed: %s", c.name, w.Body.String())
		}
	}
}
