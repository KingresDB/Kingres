// SPDX-License-Identifier: AGPL-3.0-only

package cluster

import (
	"context"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/wire"
)

func TestInternalAddressIsThePortAboveTheRESTPort(t *testing.T) {
	for in, want := range map[string]string{"10.0.0.11:8091": "10.0.0.11:8092", "db1.example.com:9000": "db1.example.com:9001", "[::1]:8091": "[::1]:8092"} {
		got, err := InternalAddressOf(in)
		if err != nil || got != want {
			t.Errorf("InternalAddressOf(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "host", "host:0", "host:65535", "host:abc", "ho st:80", "host:80/path", "-x:80;rm:80"} {
		if _, err := InternalAddressOf(in); err == nil {
			t.Errorf("InternalAddressOf(%q) was accepted", in)
		}
	}
}

func TestValidateJoin(t *testing.T) {
	dir := t.TempDir()
	ok := api.JoinClusterRequest{ExistingNodeAddress: "10.0.0.11:8091", Admin: api.AdminAccount{Username: "root_admin", Password: "x"},
		DataPaths: api.DataPaths{DataDirectory: dir}}
	if errs := ValidateJoin(ok, dir); len(errs) != 0 {
		t.Fatalf("a valid request: %v", errs)
	}
	bad := ok
	bad.ExistingNodeAddress = "nonsense"
	bad.DataPaths.DataDirectory = "/elsewhere"
	g := "rack-a"
	bad.ServerGroup = &g
	w := "/wal"
	bad.DataPaths.WalDirectory = &w
	fields := map[string]bool{}
	for _, e := range ValidateJoin(bad, dir) {
		fields[e.Field] = true
	}
	for _, f := range []string{"/existingNodeAddress", "/dataPaths/dataDirectory", "/serverGroup", "/dataPaths/walDirectory"} {
		if !fields[f] {
			t.Errorf("no error for %s: %v", f, fields)
		}
	}
}

func TestRoleSpecsAreVerifiedNotPasswords(t *testing.T) {
	pw := "plain-text-password"
	if err := validateRoleSpec(wire.RoleSpec{Name: "alice", Verifier: &pw}); err == nil {
		t.Error("a clear text password was accepted as a verifier")
	}
	md5 := "md5" + strings.Repeat("a", 32)
	if err := validateRoleSpec(wire.RoleSpec{Name: "alice", Verifier: &md5}); err == nil {
		t.Error("an MD5 hash was accepted as a verifier")
	}
	good := "SCRAM-SHA-256$4096:c2FsdHNhbHQ=$a2V5a2V5:c2VydmVy"
	if err := validateRoleSpec(wire.RoleSpec{Name: "alice", Verifier: &good}); err != nil {
		t.Errorf("a SCRAM verifier: %v", err)
	}
	for _, n := range []string{"pg_monitor", "kingresd", "", "a\x00b"} {
		if err := validateRoleSpec(wire.RoleSpec{Name: n}); err == nil {
			t.Errorf("role name %q was accepted", n)
		}
	}
}

func TestResolveCIDRs(t *testing.T) {
	got, err := resolveCIDRs(context.Background(), "10.1.2.3")
	if err != nil || len(got) != 1 || got[0] != "10.1.2.3/32" {
		t.Errorf("10.1.2.3: %v %v", got, err)
	}
	got, err = resolveCIDRs(context.Background(), "127.0.0.2")
	if err != nil || len(got) != 3 || got[1] != "127.0.0.1/32" {
		t.Errorf("a loopback peer must also cover the loopback source: %v %v", got, err)
	}
	if _, err := resolveCIDRs(context.Background(), "no-such-host.invalid"); err == nil {
		t.Error("an unresolvable name was accepted")
	}
}
