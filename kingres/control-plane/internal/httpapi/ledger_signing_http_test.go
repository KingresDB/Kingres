// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/auth"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/contract"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/task"
)

// fakeSigner answers like internal/ledgersign does, with fixed data.
type fakeSigner struct {
	update  api.LedgerSettingsUpdate
	created api.CreateLedgerSigningKeyRequest
	retired string
	revoked api.RevokeLedgerSigningKeyRequest
	fail    error
}

const fakeFP = "3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a"

func fakeTask(kind string) task.Task {
	return task.Task{ID: 41, Kind: kind, State: task.Pending, SubjectType: "ledgerKey", SubjectID: "signing",
		Steps: []task.StepState{{Name: "Generate the key", State: task.StepPending}}, CreatedAt: time.Now(), UpdatedAt: time.Now(), CreatedBy: "alice"}
}

func settings() api.LedgerSettingsCluster {
	u := "https://ledger.example.com/kingres"
	return api.LedgerSettingsCluster{
		Publication: api.LedgerPublicationSettings{Enabled: true, OriginPrefix: "ledger.example.com/kingres", PublicURL: &u, IntervalSeconds: 60, OriginPrefixLocked: true},
		Anchor:      api.LedgerAnchorSettings{IntervalSeconds: 3600, MinimumAnchored: 1},
	}
}

func (f *fakeSigner) Settings(context.Context) (api.LedgerSettingsCluster, error) {
	return settings(), f.fail
}
func (f *fakeSigner) UpdateSettings(_ context.Context, in api.LedgerSettingsUpdate) (api.LedgerSettingsCluster, error) {
	f.update = in
	return settings(), f.fail
}
func (f *fakeSigner) Signing(context.Context) (api.LedgerSigning, error) {
	id := strings.Repeat("ab", 32)
	now := time.Now().UTC()
	return api.LedgerSigning{ClusterID: &id, Mode: "file", Holders: []string{"db1", "db2"},
		Keys:   []api.LedgerSigningKey{{Fingerprint: fakeFP, PublicKey: "AAAA", State: "active", CreatedAt: now, ActivatedAt: &now}},
		Events: []api.LedgerSigningEvent{{Seq: 0, Type: "key-added", Time: now}}, BackupAdvised: true}, f.fail
}
func (f *fakeSigner) TrustFile(context.Context) ([]byte, error) {
	return []byte("{\n  \"format\": 1,\n  \"origin_prefix\": \"x/kingres-ledger\",\n  \"keys\": [],\n  \"tsa_roots\": [],\n  \"client_keys\": [],\n  \"witnesses\": []\n}\n"), f.fail
}
func (f *fakeSigner) CreateKey(_ context.Context, _ string, req api.CreateLedgerSigningKeyRequest) (task.Task, error) {
	f.created = req
	return fakeTask("ledger.key.create"), f.fail
}
func (f *fakeSigner) RetireKey(_ context.Context, _, fp string) (task.Task, error) {
	f.retired = fp
	return fakeTask("ledger.key.retire"), f.fail
}
func (f *fakeSigner) RevokeKey(_ context.Context, _, _ string, req api.RevokeLedgerSigningKeyRequest) (task.Task, error) {
	f.revoked = req
	return fakeTask("ledger.key.revoke"), f.fail
}

func TestLedgerSigningOperationsMatchTheContractAndTheRoles(t *testing.T) {
	fake := &fakeSigner{}
	e := newEnv(t, func(d *Deps) { d.LedgerSigning = fake })
	e.initialize()
	chk := newSchemaChecker(t)
	admin, ops, ro := e.token(auth.RoleAdmin), e.token(auth.RoleOps), e.token(auth.RoleReadonly)

	for _, c := range []struct{ op, path string }{
		{"getLedgerSettings", "/api/v1/ledger/settings"},
		{"getLedgerSigning", "/api/v1/ledger/signing"},
		{"getLedgerTrustFile", "/api/v1/ledger/signing/trust-file"},
	} {
		w := e.do(req{method: "GET", path: c.path, bearer: ro})
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", c.op, w.Code, w.Body.String())
		}
		if err := chk.validateResponse(c.op, "200", w.Body.Bytes()); err != nil {
			t.Errorf("%s does not match the contract: %v\n%s", c.op, err, w.Body.String())
		}
		if c.op == "getLedgerTrustFile" {
			if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="trust.json"` {
				t.Errorf("Content-Disposition %q", cd)
			}
			if w.Header().Get("Content-Type") != "application/json" || w.Header().Get("Cache-Control") != "no-store" {
				t.Errorf("headers %v", w.Header())
			}
		}
	}
	// A read needs readonly; nobody without a role gets it; the leader answers the three reads.
	for _, op := range []string{"getLedgerSettings", "getLedgerSigning", "getLedgerTrustFile"} {
		if !forwardsToLeader(operationOf(t, op)) {
			t.Errorf("%s is not forwarded to the leader", op)
		}
	}

	// The settings: the body is closed, ranges are the service's.
	w := e.do(req{method: "PUT", path: "/api/v1/ledger/settings", bearer: admin, body: map[string]any{"publication": map[string]any{"enabled": true, "publicUrl": nil}, "anchor": map[string]any{"minimumAnchored": 2}}})
	if w.Code != 200 {
		t.Fatalf("updateLedgerSettings: %d %s", w.Code, w.Body.String())
	}
	if err := chk.validateResponse("updateLedgerSettings", "200", w.Body.Bytes()); err != nil {
		t.Error(err)
	}
	if fake.update.Publication == nil || !fake.update.Publication.PublicURL.Set || fake.update.Publication.PublicURL.Value != nil || fake.update.Anchor == nil || *fake.update.Anchor.MinimumAnchored != 2 {
		t.Errorf("the update that reached the service: %+v", fake.update)
	}
	wantProblem(t, e.do(req{method: "PUT", path: "/api/v1/ledger/settings", bearer: admin, body: map[string]any{"nonsense": 1}}), 400, "validation_failed")
	if w := e.do(req{method: "PUT", path: "/api/v1/ledger/settings", bearer: ops, body: map[string]any{}}); w.Code != 403 {
		t.Errorf("the settings with the role ops: %d", w.Code)
	}

	// Creating a key is a task; the body is optional; unknown members are refused.
	w = e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys", bearer: admin})
	if w.Code != 202 || w.Header().Get("Location") != "/api/v1/tasks/41" {
		t.Fatalf("createLedgerSigningKey: %d %v %s", w.Code, w.Header(), w.Body.String())
	}
	if err := chk.validateSchema("Task", w.Body.Bytes()); err != nil {
		t.Error(err)
	}
	if w := e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys", bearer: admin, body: map[string]any{"overlapDays": 7}}); w.Code != 202 || *fake.created.OverlapDays != 7 {
		t.Errorf("overlapDays: %d %+v", w.Code, fake.created)
	}
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys", bearer: admin, body: map[string]any{"seed": "x"}}), 400, "validation_failed")
	for name, tok := range map[string]string{"ops": ops, "readonly": ro} {
		if w := e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys", bearer: tok}); w.Code != 403 {
			t.Errorf("creating a key with the role %s: %d", name, w.Code)
		}
	}
	// Retire and revoke.
	w = e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys/" + fakeFP + "/retire", bearer: admin, body: map[string]any{}})
	if w.Code != 202 || fake.retired != fakeFP {
		t.Fatalf("retire: %d %s (%q)", w.Code, w.Body.String(), fake.retired)
	}
	w = e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys/" + fakeFP + "/revoke", bearer: admin, body: map[string]any{"compromisedSince": "2026-11-17T10:00:00Z", "reason": "stolen laptop", "confirm": fakeFP}})
	if w.Code != 202 || fake.revoked.Reason != "stolen laptop" || fake.revoked.Confirm != fakeFP {
		t.Fatalf("revoke: %d %s %+v", w.Code, w.Body.String(), fake.revoked)
	}

	// What the audit log holds: the operation, the path and the task; never the reason, the confirmation or a key.
	var sb strings.Builder
	for _, ev := range e.audit.Events() {
		b, _ := json.Marshal(ev)
		sb.Write(b)
	}
	got := sb.String()
	for _, want := range []string{"updateLedgerSettings", "createLedgerSigningKey", "retireLedgerSigningKey", "revokeLedgerSigningKey"} {
		if !strings.Contains(got, want) {
			t.Errorf("the audit log lacks %s", want)
		}
	}
	for _, secret := range []string{"stolen laptop"} {
		if strings.Contains(got, secret) {
			t.Errorf("the audit log holds the text %q of the request", secret)
		}
	}
}

func TestLedgerSigningProblemsPassThroughAndOtherErrorsAreHidden(t *testing.T) {
	fake := &fakeSigner{}
	e := newEnv(t, func(d *Deps) { d.LedgerSigning = fake })
	e.initialize()
	admin := e.token(auth.RoleAdmin)
	fake.fail = problem.New(problem.LastSigningKey, "create a key first")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys/" + fakeFP + "/retire", bearer: admin, body: map[string]any{}}), 409, "last_signing_key")
	fake.fail = problem.New(problem.OriginPrefixLocked, "locked")
	wantProblem(t, e.do(req{method: "PUT", path: "/api/v1/ledger/settings", bearer: admin, body: map[string]any{"publication": map[string]any{"originPrefix": "x/y"}}}), 409, "origin_prefix_locked")
	fake.fail = problem.New(problem.SigningKeyMissing, "none")
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/ledger/signing/trust-file", bearer: admin}), 409, "signing_key_missing")
	// An internal error is logged and answered with a fixed text: a path, a server or a SQL text is not sent.
	fake.fail = errors.New("open /var/lib/kingres/ledger/keys/signing/secret.key: permission denied")
	w := e.do(req{method: "GET", path: "/api/v1/ledger/signing", bearer: admin})
	wantProblem(t, w, 503, "service_unavailable")
	if strings.Contains(w.Body.String(), "/var/lib") || strings.Contains(w.Body.String(), "secret.key") {
		t.Errorf("the answer names a path: %s", w.Body.String())
	}
}

func TestLedgerSigningWithoutAServiceIsNotSupported(t *testing.T) {
	e := newEnv(t)
	e.initialize()
	admin := e.token(auth.RoleAdmin)
	wantProblem(t, e.do(req{method: "GET", path: "/api/v1/ledger/signing", bearer: admin}), 422, "unsupported_operation")
	wantProblem(t, e.do(req{method: "POST", path: "/api/v1/ledger/signing/keys", bearer: admin}), 422, "unsupported_operation")
}

func operationOf(t *testing.T, id string) contract.Operation {
	t.Helper()
	for _, op := range contract.Operations() {
		if op.ID == id {
			return op
		}
	}
	t.Fatalf("no operation %s", id)
	return contract.Operation{}
}
