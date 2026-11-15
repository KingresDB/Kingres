// SPDX-License-Identifier: AGPL-3.0-only

package membership

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/cluster"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/internalapi"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/problem"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/setup"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

// The CA key lives on one server. A manager leader that does not hold it answers
// every operation that needs it with the dedicated problem, before it touches
// anything, and the internal call of the renewal job with its own code.
func TestOperationsThatNeedTheCAAnswerADedicatedProblemWithoutIt(t *testing.T) {
	dir := t.TempDir()
	node, err := setup.Open(dir, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := cluster.New(cluster.Config{DataDir: dir, ServerHost: "db1"}, node)
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{Svc: svc, Self: "db1"}
	ctx := context.Background()
	want := func(what string, err error) {
		t.Helper()
		var p *problem.Problem
		if !errors.As(err, &p) || p.Code != problem.CertificateAuthorityUnavailable || p.Status != http.StatusServiceUnavailable {
			t.Errorf("%s: %v", what, err)
		}
		if p != nil && (p.Detail == "" || len(p.Detail) > 400) {
			t.Errorf("%s: detail %q", what, p.Detail)
		}
	}
	_, err = m.RenewServerCertificates(ctx, "root", "db2")
	want("renewServerCertificates", err)
	_, err = m.RevokeServerCertificates(ctx, "root", "db2")
	want("revokeServerCertificates", err)
	_, err = m.RemoveServer(ctx, "root", "db2")
	want("removeServer", err)
	_, err = m.AddServer(ctx, "root", api.AddServerRequest{Address: "db3:8091", SetupToken: "x"}, "")
	want("addServer", err)
	_, err = m.JoinFromPeer(ctx, wire.AddServerRequest{Actor: "root", ServerID: "db3"})
	want("joinCluster", err)
	_, err = m.LeaveFromPeer(ctx, "db2", wire.LeaveRequest{Actor: "root"})
	want("leaveCluster", err)
	_, err = m.RenewFromPeer(ctx, "db2", wire.RenewRequest{CSR: "x"})
	want("the renewal call", err)
	// The internal API answers the renewal call with its own code.
	r := &Receiver{M: m}
	_, err = r.Handle(ctx, "POST /pki/renew", "db2", []byte(`{"csr":"x"}`))
	var ce *internalapi.CallError
	if !errors.As(err, &ce) || ce.Status != http.StatusServiceUnavailable || ce.Code != "ca_unavailable" {
		t.Errorf("POST /pki/renew: %v", err)
	}
	// The body is closed: nothing but the certificate request.
	if _, err := r.Handle(ctx, "POST /pki/renew", "db2", []byte(`{"csr":"x","serverId":"db3"}`)); !errors.As(err, &ce) || ce.Status != http.StatusBadRequest {
		t.Errorf("a renewal that names a server: %v", err)
	}
}
