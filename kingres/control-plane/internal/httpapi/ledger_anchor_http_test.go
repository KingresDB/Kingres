// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgeranchor"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/store"
)

// fakeAnchoring answers like internal/ledgeranchor does, with fixed data; it records what reached it.
type fakeAnchoring struct {
	fail     error
	create   api.LedgerAnchorTargetCreate
	update   api.LedgerAnchorTargetUpdate
	page     store.Page
	db       string
	state    string
	targetID int64
	format   string
}

func aTarget() api.LedgerAnchorTarget {
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	return api.LedgerAnchorTarget{ID: 3, Kind: "worm", Name: "vault", Enabled: true, Config: json.RawMessage(`{"bucket":"b"}`), CredentialsSet: true,
		LastTest:         &api.LedgerAnchorTest{Checks: []api.LedgerAnchorCheck{{Name: "bucket", Status: "ok", Detail: "bucket reachable"}}, OK: true, TestedAt: &at},
		NewestAnchoredAt: &at, State: "ok"}
}

func anAnchor() api.LedgerAnchor {
	tid := int64(3)
	return api.LedgerAnchor{ID: 9, TargetID: &tid, TargetName: "vault", Kind: "worm", AggregateSize: 5, State: "pending", Attempts: 1}
}

func (f *fakeAnchoring) Targets(context.Context) (api.LedgerAnchorTargetList, error) {
	return api.LedgerAnchorTargetList{Items: []api.LedgerAnchorTarget{aTarget()}}, f.fail
}
func (f *fakeAnchoring) Target(_ context.Context, id int64) (api.LedgerAnchorTarget, error) {
	f.targetID = id
	return aTarget(), f.fail
}
func (f *fakeAnchoring) CreateTarget(_ context.Context, _ string, in api.LedgerAnchorTargetCreate) (api.LedgerAnchorTarget, error) {
	f.create = in
	return aTarget(), f.fail
}
func (f *fakeAnchoring) UpdateTarget(_ context.Context, id int64, in api.LedgerAnchorTargetUpdate) (api.LedgerAnchorTarget, error) {
	f.targetID, f.update = id, in
	return aTarget(), f.fail
}
func (f *fakeAnchoring) DeleteTarget(_ context.Context, id int64) error {
	f.targetID = id
	return f.fail
}
func (f *fakeAnchoring) TestTarget(_ context.Context, id int64) (api.LedgerAnchorTest, error) {
	f.targetID = id
	return *aTarget().LastTest, f.fail
}
func (f *fakeAnchoring) List(_ context.Context, db string, tid int64, state string, p store.Page) (api.LedgerAnchorPage, error) {
	f.db, f.targetID, f.state, f.page = db, tid, state, p
	next := "abc"
	return api.LedgerAnchorPage{Items: []api.LedgerAnchor{anAnchor()}, NextCursor: &next}, f.fail
}
func (f *fakeAnchoring) AnchorNow(_ context.Context, db string) (api.LedgerAnchorList, error) {
	f.db = db
	return api.LedgerAnchorList{Items: []api.LedgerAnchor{anAnchor()}}, f.fail
}
func (f *fakeAnchoring) Retry(_ context.Context, db string, id int64) (api.LedgerAnchor, error) {
	f.db, f.targetID = db, id
	return anAnchor(), f.fail
}
func (f *fakeAnchoring) Evidence(_ context.Context, db string, id int64, format string) (ledgeranchor.Evidence, error) {
	f.db, f.targetID, f.format = db, id, format
	return ledgeranchor.Evidence{ContentType: "application/json", FileName: "anchor-9.json", Data: []byte(`{"format":1}`)}, f.fail
}

func TestLedgerAnchorOperationsMatchTheContractTheRolesAndTheLeaderRule(t *testing.T) {
	fake := &fakeAnchoring{}
	e := newEnv(t, func(d *Deps) { d.LedgerAnchoring = fake })
	e.initialize()
	chk := newSchemaChecker(t)
	ro, ops, admin := e.token(auth.RoleReadonly), e.token(auth.RoleOps), e.token(auth.RoleAdmin)
	create := map[string]any{"kind": "worm", "name": "vault", "config": map[string]any{"bucket": "b"}, "credentials": map[string]any{"accessKeyId": "AKIA1234", "secretAccessKey": "s3cr3t/value+1"}}
	for _, c := range []struct {
		op, method, path string
		body             any
		bearer           string
		want             string
		code             int
	}{
		{"listLedgerAnchorTargets", "GET", "/api/v1/ledger/anchor-targets", nil, ro, "200", 200},
		{"getLedgerAnchorTarget", "GET", "/api/v1/ledger/anchor-targets/3", nil, ro, "200", 200},
		{"createLedgerAnchorTarget", "POST", "/api/v1/ledger/anchor-targets", create, admin, "201", 201},
		{"updateLedgerAnchorTarget", "PATCH", "/api/v1/ledger/anchor-targets/3", map[string]any{"enabled": false}, admin, "200", 200},
		{"testLedgerAnchorTarget", "POST", "/api/v1/ledger/anchor-targets/3/test", map[string]any{}, admin, "200", 200},
		{"listLedgerAnchors", "GET", "/api/v1/databases/shop/ledger/anchors?targetId=3&state=pending&limit=7&cursor=xyz", nil, ro, "200", 200},
		{"anchorLedgerNow", "POST", "/api/v1/databases/shop/ledger/anchors", map[string]any{}, ops, "201", 201},
		{"retryLedgerAnchor", "POST", "/api/v1/databases/shop/ledger/anchors/9/retry", map[string]any{}, ops, "200", 200},
	} {
		w := e.do(req{method: c.method, path: c.path, body: c.body, bearer: c.bearer})
		if w.Code != c.code {
			t.Fatalf("%s: %d %s", c.op, w.Code, w.Body.String())
		}
		if err := chk.validateResponse(c.op, c.want, w.Body.Bytes()); err != nil {
			t.Errorf("%s does not match the contract: %v\n%s", c.op, err, w.Body.String())
		}
		op := operationOf(t, c.op)
		if !forwardsToLeader(op) {
			t.Errorf("%s is not run by the leader", c.op)
		}
		// the role
		if c.bearer != ro {
			wantProblem(t, e.do(req{method: c.method, path: c.path, body: c.body, bearer: ro}), 403, "forbidden")
		}
	}
	if w := e.do(req{method: "DELETE", path: "/api/v1/ledger/anchor-targets/3", bearer: admin}); w.Code != 204 {
		t.Errorf("delete: %d", w.Code)
	}
	wantProblem(t, e.do(req{method: "DELETE", path: "/api/v1/ledger/anchor-targets/3", bearer: ops}), 403, "forbidden")
	w := e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/anchors/9/evidence?format=ots", bearer: ro})
	if w.Code != 200 || fake.format != "ots" || w.Header().Get("Content-Disposition") == "" {
		t.Errorf("evidence: %d %v", w.Code, w.Header())
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/anchors/9/evidence?format=pdf", bearer: ro}), 400, "validation_failed")
	if fake.db != "shop" || fake.targetID != 9 {
		t.Errorf("what reached the service: %q %d", fake.db, fake.targetID)
	}
	fake.fail = nil
	e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/anchors?targetId=3&state=pending&limit=7&cursor=xyz", bearer: ro})
	if fake.state != "pending" || fake.targetID != 3 || fake.page.Limit != 7 || fake.page.Cursor != "xyz" {
		t.Errorf("the query reached the service as %q %d %+v", fake.state, fake.targetID, fake.page)
	}
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/databases/shop/ledger/anchors?state=claimed", bearer: ro}), 400, "validation_failed")
	if w := e.do(req{method: "GET", path: "/api/v1/ledger/anchor-targets"}); w.Code != 401 {
		t.Errorf("without a token: %d", w.Code)
	}
	// problems of the service pass through; any other error is a fixed 503
	fake.fail = problem.New(problem.AlreadyExists, "exists")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/ledger/anchor-targets", body: create, bearer: admin}), 409, "already_exists")
	fake.fail = errors.New("open /var/lib/kingres/anchor/3.json: permission denied")
	w = e.do(req{method: "GET", path: "/api/v1/ledger/anchor-targets", bearer: ro})
	wantProblem(t, w, 503, "service_unavailable")
	if strings.Contains(w.Body.String(), "/var/lib") {
		t.Errorf("the answer names a path: %s", w.Body.String())
	}
}

// No credential is in an answer, an error answer, a log line or an audit entry, whatever the request: a malformed body, an unknown member, a wrong
// type, a service that fails.
func TestNoCredentialIsInAnAnswerALogOrTheAudit(t *testing.T) {
	fake := &fakeAnchoring{}
	e := newEnv(t, func(d *Deps) { d.LedgerAnchoring = fake })
	e.initialize()
	admin := e.token(auth.RoleAdmin)
	const key, secret = "AKIDHOSTILE0123", "p@ss/w0rd+with=chars&more"
	cred := map[string]any{"accessKeyId": key, "secretAccessKey": secret}
	var all strings.Builder
	record := func(w interface{ String() string }) { all.WriteString(w.String()) }
	bodies := []any{
		map[string]any{"kind": "worm", "name": "v", "config": map[string]any{"bucket": "b"}, "credentials": cred},
		map[string]any{"kind": "worm", "name": "v", "config": map[string]any{}, "credentials": cred, "unknownMember": secret},
		map[string]any{"kind": 5, "name": "v", "config": map[string]any{}, "credentials": cred},
		fmt.Sprintf(`{"kind":"worm","name":"v","config":{},"credentials":{"secretAccessKey":"%s"`, secret), // truncated JSON
		fmt.Sprintf(`{"kind":"worm","name":"v","config":{},"credentials":{"secretAccessKey":"%s"}} trailing`, secret),
	}
	for _, b := range bodies {
		w := e.do(req{method: "POST", path: "/api/v1/ledger/anchor-targets", body: b, bearer: admin})
		record(w.Body)
	}
	fake.fail = errors.New("the store said key " + key + " secret " + secret)
	w := e.do(req{method: "POST", path: "/api/v1/ledger/anchor-targets", body: bodies[0], bearer: admin})
	record(w.Body)
	fake.fail = nil
	w = e.do(req{method: "PATCH", path: "/api/v1/ledger/anchor-targets/3", body: map[string]any{"credentials": cred}, bearer: admin})
	record(w.Body)
	all.WriteString(e.logs.String())
	for _, ev := range e.audit.Events() {
		b, _ := json.Marshal(ev)
		all.Write(b)
	}
	for _, s := range []string{key, secret} {
		for _, f := range []string{s, fmt.Sprintf("%x", s), strings.ReplaceAll(strings.ReplaceAll(s, "/", "%2F"), "+", "%2B")} {
			if strings.Contains(all.String(), f) {
				t.Errorf("a credential (%d characters) is in an answer, the log or the audit: ...%s...", len(s), around(all.String(), f))
			}
		}
	}
	// the audit names the target and says that credentials were changed
	found := false
	for _, ev := range e.audit.Events() {
		if ev.Action == "updateLedgerAnchorTarget" {
			b, _ := json.Marshal(ev.Details)
			found = strings.Contains(string(b), "credentialsChanged")
		}
	}
	if !found {
		t.Error("the audit entry of the update does not say that credentials changed")
	}
}

func around(s, f string) string {
	i := strings.Index(s, f)
	if i < 0 {
		return ""
	}
	a, b := i-20, i+len(f)+20
	if a < 0 {
		a = 0
	}
	if b > len(s) {
		b = len(s)
	}
	return s[a:b]
}

func TestLedgerAnchorOperationsWithoutAServiceAreNotSupported(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	ro := e.token(auth.RoleReadonly)
	for _, p := range []string{"/api/v1/ledger/anchor-targets", "/api/v1/ledger/anchor-targets/3", "/api/v1/databases/shop/ledger/anchors"} {
		wantProblem(t, e.do(req{method: "GET", path: p, bearer: ro}), 422, "unsupported_operation")
	}
}
