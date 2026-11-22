// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
)

func problemCode(err error) problem.Code {
	var p *problem.Problem
	if errors.As(err, &p) {
		return p.Code
	}
	return ""
}

func TestClientProblemMapsEveryRefusalOfAnotherServer(t *testing.T) {
	cases := map[string]problem.Code{
		textLedgerOff:    problem.Conflict,
		textClientOld:    problem.UnsupportedOp,
		textNoOpenSSL:    problem.UnsupportedOp,
		textClientExists: problem.AlreadyExists,
		textClientRevoke: problem.Conflict,
		textClientNone:   problem.NotFound,
		textPolicyReq:    problem.Conflict,
	}
	for text, want := range cases {
		err := clientProblem(errors.New("apply_failed: "+text), "srv-2")
		if problemCode(err) != want {
			t.Errorf("%q: %v", text, err)
		}
		var p *problem.Problem
		if errors.As(err, &p) && !strings.Contains(p.Detail, "srv-2") {
			t.Errorf("%q does not name the server: %s", text, p.Detail)
		}
	}
	if clientProblem(nil, "") != nil {
		t.Error("nil is not nil")
	}
}

func TestRegisteringAKeyValidatesBeforeAnythingIsTouched(t *testing.T) {
	o := &Ops{}
	good := base64.StdEncoding.EncodeToString(make([]byte, 32))
	bad := []api.RegisterLedgerClientKeyRequest{
		{PublicKey: "not base64!", Label: "x"},
		{PublicKey: base64.StdEncoding.EncodeToString(make([]byte, 31)), Label: "x"},
		{PublicKey: base64.StdEncoding.EncodeToString(make([]byte, 33)), Label: "x"},
		{PublicKey: good, Label: ""},
		{PublicKey: good, Label: strings.Repeat("x", 101)},
		{PublicKey: good, Label: "x", RoleName: strings.Repeat("r", 64)},
	}
	for i, r := range bad {
		_, err := o.RegisterClientKey(context.Background(), "shop", "admin", r)
		if problemCode(err) != problem.ValidationFailed {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if _, err := o.RegisterClientKey(context.Background(), "", "admin", api.RegisterLedgerClientKeyRequest{PublicKey: good, Label: "x"}); problemCode(err) != problem.NotFound {
		t.Errorf("a bad database name: %v", err)
	}
}

func TestRevokingAndSettingAPolicyValidateTheirInput(t *testing.T) {
	o := &Ops{}
	ctx := context.Background()
	for _, id := range []string{"", "abc", "ABCDEF12", "abcdef123", "abcdefgh"} {
		if _, err := o.RevokeClientKey(ctx, "shop", "a", id, api.RevokeLedgerClientKeyRequest{Reason: "r"}); problemCode(err) != problem.NotFound {
			t.Errorf("key id %q: %v", id, err)
		}
	}
	for _, r := range []string{"", strings.Repeat("x", 201)} {
		if _, err := o.RevokeClientKey(ctx, "shop", "a", "abcdef12", api.RevokeLedgerClientKeyRequest{Reason: r}); problemCode(err) != problem.ValidationFailed {
			t.Errorf("a reason of %d characters: %v", len(r), err)
		}
	}
	if err := o.SetSignaturePolicy(ctx, "shop", "public", "t", api.SignaturePolicyRequest{Policy: "optional"}); problemCode(err) != problem.ValidationFailed {
		t.Errorf("the policy optional: %v", err)
	}
	if err := o.SetSignaturePolicy(ctx, "shop", "", "t", api.SignaturePolicyRequest{Policy: "required"}); problemCode(err) != problem.NotFound {
		t.Errorf("a bad table name: %v", err)
	}
}

func TestTheApplyOfTheClientKeysIsAClosedSet(t *testing.T) {
	o := &Ops{}
	for _, raw := range []string{`{"op":"drop","database":"shop"}`, `{"op":"register","database":"shop","extra":1}`, `{"op":"register"`, `[]`} {
		if err := o.ApplyLocal(context.Background(), ApplyLedgerClient, json.RawMessage(raw)); err == nil {
			t.Errorf("%s is accepted", raw)
		}
	}
	if err := o.ApplyLocal(context.Background(), ApplyLedgerClient, json.RawMessage(`{"op":"register","database":""}`)); err == nil {
		t.Error("a database name that is not an identifier is accepted")
	}
}

func TestTheRequestsOfStep20AreValidatedBeforeAnythingIsTouched(t *testing.T) {
	o := &Ops{}
	ctx := context.Background()
	yes, neg := true, int64(-1)
	for i, r := range []api.LedgerBundleRequest{
		{},
		{IncludeAnchors: &yes, SinceSize: &neg},
		{IncludeAnchors: &yes, Logs: manyOrigins(101)},
		{IncludeAnchors: &yes, Logs: []string{""}},
		{IncludeAnchors: &yes, Logs: []string{strings.Repeat("x", 301)}},
		{IncludeAnchors: &yes, Proofs: make([]api.InclusionProofRequest, 21)},
	} {
		if _, err := o.PrepareBundle(ctx, "shop", "a", r); problemCode(err) != problem.ValidationFailed {
			t.Errorf("bundle request %d: %v", i, err)
		}
	}
	for i, r := range []api.AcknowledgeLedgerDivergenceRequest{
		{Kind: "restore", Reason: "r", Confirm: "other_db"},
		{Kind: "unknown", Reason: "r", Confirm: "shop"},
		{Kind: "restore", Reason: "", Confirm: "shop"},
		{Kind: "restore", Reason: strings.Repeat("x", 201), Confirm: "shop"},
		{Kind: "restore", Reason: "café", Confirm: "shop"},
	} {
		if _, err := o.PrepareEpoch(ctx, "shop", 0, "a", r); problemCode(err) != problem.ValidationFailed {
			t.Errorf("acknowledgement %d: %v", i, err)
		}
	}
	if _, err := o.PrepareEpoch(ctx, "shop", 0, "a", api.AcknowledgeLedgerDivergenceRequest{Kind: "restore", Reason: "r", Confirm: "shop"}); problemCode(err) != problem.UnsupportedOp {
		t.Errorf("an acknowledgement on a server that does not publish: %v", err)
	}
	o.LedgerChainDiverged = func(context.Context, string, int32) (bool, error) { return false, nil }
	o.LedgerEpochStart = func(context.Context, string, int32, string, string, string) error { return nil }
	if _, err := o.PrepareEpoch(ctx, "shop", 0, "a", api.AcknowledgeLedgerDivergenceRequest{Kind: "failoverLoss", Reason: "r", Confirm: "shop"}); problemCode(err) != problem.ChainNotDiverged {
		t.Errorf("a chain that is not diverged: %v", err)
	}
	o.LedgerChainDiverged = func(context.Context, string, int32) (bool, error) { return true, nil }
	in, err := o.PrepareEpoch(ctx, "shop", 3, "alice", api.AcknowledgeLedgerDivergenceRequest{Kind: "failoverLoss", Reason: "r", Confirm: "shop"})
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := in.(epochParams); !ok || p.Kind != "failover-loss" || p.Group != 3 || p.Actor != "alice" {
		t.Errorf("the input of the task: %#v", in)
	}
	for i, r := range []api.LedgerVerifyRequest{
		{Checks: []string{"bogus"}},
		{Checks: []string{"chain", "chain"}},
	} {
		if _, err := o.PrepareVerification(ctx, "shop", r); problemCode(err) != problem.ValidationFailed {
			t.Errorf("checks %d: %v", i, err)
		}
	}
	if _, err := o.PrepareVerification(ctx, "shop", api.LedgerVerifyRequest{Checks: []string{"anchors"}}); problemCode(err) != problem.UnsupportedOp || !strings.Contains(err.Error(), "anchors") {
		t.Errorf("the check of the anchors: %v", err)
	}
	if _, err := o.PrepareVerification(ctx, "shop", api.LedgerVerifyRequest{Checks: []string{"publication"}}); problemCode(err) != problem.UnsupportedOp {
		t.Errorf("the check of the publication on a server that has no audit: %v", err)
	}
}

func manyOrigins(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "example.org/kingres/c/" + strings.Repeat("a", 10)
	}
	return out
}
