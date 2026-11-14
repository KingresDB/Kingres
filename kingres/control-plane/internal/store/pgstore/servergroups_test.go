// SPDX-License-Identifier: PostgreSQL

package pgstore

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pgtest"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/pki"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/sysdb"
)

// The groups and the records of servers come from the system database
// (kingres_cp) when it exists, and the server list shows them.
func TestIntegrationServerRecordsOverlay(t *testing.T) {
	if !pgtest.Available() {
		t.Skip("KINGRES_PG_BIN is not set; skipping the PostgreSQL integration test")
	}
	in := pgtest.Start(t, nil)
	c := in.Connect(t, "postgres")
	if _, err := sysdb.Migrate(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	crt, _, err := pki.NewBootstrapCert(pki.Names{CommonName: "10.0.0.2"}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := pki.ParseCertPEM(crt)
	fp := pki.Fingerprint(cert.Raw)
	ctx := context.Background()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO kingres_cp.server_group (name, description) VALUES ('rack-b', 'Row 2'), ('rack-a', 'Row 1'), ('empty', '')`, nil},
		{`INSERT INTO kingres_cp.server (server_id) VALUES ('10.0.0.1')`, nil},
		{`INSERT INTO kingres_cp.server (server_id, server_group, seen_fingerprint, fingerprint_pinned, certificate_pem) VALUES ('10.0.0.2', 'rack-a', $1, true, $2)`, []any{fp, string(crt)}},
		{`INSERT INTO kingres_cp.server (server_id, server_group, seen_fingerprint, fingerprint_pinned) VALUES ('10.0.0.3', 'rack-a', 'not a fingerprint', true)`, nil},
		{`INSERT INTO kingres_cp.server (server_id, server_group) VALUES ('10.0.0.4', 'rack-b')`, nil},
	} {
		if _, err := c.Exec(ctx, q.sql, q.args...); err != nil {
			t.Fatalf("%s: %v", q.sql, err)
		}
	}
	s, err := New(Options{DSN: in.DSN("postgres"), Logger: slog.New(slog.DiscardHandler), PanelPort: "8091", Version: "test", Started: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	groups, err := s.ServerGroups(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []api.ServerGroup{
		{Name: "empty", ServerIDs: []string{}},
		{Name: "rack-a", Description: "Row 1", ServerIDs: []string{"10.0.0.2", "10.0.0.3"}},
		{Name: "rack-b", Description: "Row 2", ServerIDs: []string{"10.0.0.4"}},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v", groups)
	}
	for i := range want {
		g := groups[i]
		if g.Name != want[i].Name || g.Description != want[i].Description || len(g.ServerIDs) != len(want[i].ServerIDs) {
			t.Fatalf("group %d = %+v, want %+v", i, g, want[i])
		}
		for j := range g.ServerIDs {
			if g.ServerIDs[j] != want[i].ServerIDs[j] {
				t.Errorf("group %s members = %v", g.Name, g.ServerIDs)
			}
		}
	}

	rack := "catalog-rack"
	servers := []api.Server{{ID: "10.0.0.1", ServerGroup: &rack}, {ID: "10.0.0.2"}, {ID: "10.0.0.3"}, {ID: "10.0.0.4"}, {ID: "10.0.0.9", ServerGroup: &rack}}
	s.applyRecords(ctx, servers)
	// The record wins over the catalog, also when it says "no group".
	if servers[0].ServerGroup != nil || servers[0].SeenFingerprint != nil || servers[0].FingerprintPinned || servers[0].CertificateExpiresAt != nil {
		t.Errorf("server 1 = %+v", servers[0])
	}
	s2 := servers[1]
	if s2.ServerGroup == nil || *s2.ServerGroup != "rack-a" || s2.SeenFingerprint == nil || *s2.SeenFingerprint != fp || !s2.FingerprintPinned ||
		s2.CertificateExpiresAt == nil || !s2.CertificateExpiresAt.Equal(cert.NotAfter) {
		t.Errorf("server 2 = %+v", s2)
	}
	// An invalid stored fingerprint is not shown, and then "pinned" is false.
	if servers[2].SeenFingerprint != nil || servers[2].FingerprintPinned {
		t.Errorf("server 3 = %+v", servers[2])
	}
	if servers[3].ServerGroup == nil || *servers[3].ServerGroup != "rack-b" {
		t.Errorf("server 4 = %+v", servers[3])
	}
	// A server without a record keeps what the catalog says.
	if servers[4].ServerGroup == nil || *servers[4].ServerGroup != "catalog-rack" {
		t.Errorf("server 9 = %+v", servers[4])
	}
}

// Without the control-plane schema the store shows the shard catalog's groups.
func TestIntegrationServerGroupsFallBackToTheCatalog(t *testing.T) {
	s := newTestStore(t)
	groups, err := s.ServerGroups(ctx(t))
	if err != nil || len(groups) != 2 || groups[0].Name != "rack-a" {
		t.Errorf("groups = %+v, %v", groups, err)
	}
}
