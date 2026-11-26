// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package instance

import (
	"strings"
	"testing"
)

func TestNodeRulesComeFirstAndMapAnyRole(t *testing.T) {
	trust := NodeTrust{Peers: []NodePeer{
		{ServerID: "127.0.0.3", CIDRs: []string{"127.0.0.3/32", "127.0.0.1/32"}},
		{ServerID: "127.0.0.2", CIDRs: []string{"127.0.0.2/32", "127.0.0.1/32"}},
	}}
	hba, err := RenderHBAWithNodes([]HBARule{{Type: "host", Databases: []string{"all"}, Users: []string{"all"}, Address: "10.0.0.0/8", Method: "scram-sha-256"}}, false, trust)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(hba), "\n")
	want := []string{"hostssl   all all 127.0.0.1/32 cert map=kingres_nodes", "hostssl   all all 127.0.0.2/32 cert map=kingres_nodes", "hostssl   all all 127.0.0.3/32 cert map=kingres_nodes"}
	for i, w := range want {
		if lines[2+i] != w {
			t.Errorf("line %d = %q, want %q", 2+i, lines[2+i], w)
		}
	}
	if strings.Contains(strings.Join(lines[2:5], "\n"), "scram") {
		t.Error("the certificate rules must precede the password rules")
	}
	ident, err := RenderIdentWithNodes("kingres", []string{"alice"}, trust)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{`kingres_nodes "127.0.0.2" all`, `kingres_nodes "127.0.0.3" all`, `kingres_proxy "kingres" "alice"`} {
		if !strings.Contains(ident, w) {
			t.Errorf("pg_ident.conf lacks %q:\n%s", w, ident)
		}
	}
}

func TestNodeTrustRefusesWhatCannotBeWritten(t *testing.T) {
	for _, tr := range []NodeTrust{
		{Peers: []NodePeer{{ServerID: "all", CIDRs: []string{"10.0.0.1/32"}}}},
		{Peers: []NodePeer{{ServerID: "a\"b", CIDRs: []string{"10.0.0.1/32"}}}},
		{Peers: []NodePeer{{ServerID: "h", CIDRs: []string{"not-a-cidr"}}}},
		{Peers: []NodePeer{{ServerID: "h"}}},
	} {
		if _, err := RenderHBAWithNodes(nil, false, tr); err == nil {
			t.Errorf("%+v was accepted", tr)
		}
	}
	if _, err := RenderHBA([]HBARule{{Type: "hostssl", Databases: []string{"all"}, Users: []string{"all"}, Address: "all", Method: "scram-sha-256", Map: "x"}}, false); err == nil {
		t.Error("a map on a password rule was accepted")
	}
}

func TestNodeConninfoAndTLSSettings(t *testing.T) {
	ci, err := NodeConninfo("/p/server.crt", "/p/server.key", "/p/ca.crt", "/p/revoked.crl")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"sslmode='verify-full'", "sslcert='/p/server.crt'", "sslkey='/p/server.key'", "sslrootcert='/p/ca.crt'", "sslcrl='/p/revoked.crl'"} {
		if !strings.Contains(ci, w) {
			t.Errorf("node_conninfo lacks %q: %s", w, ci)
		}
	}
	if _, err := NodeConninfo("relative.crt", "/k", "/c", ""); err == nil {
		t.Error("a relative path was accepted")
	}
	conf, err := RenderManaged(ManagedSettings{Port: 56000, TLSCertFile: "/p/server.crt", TLSKeyFile: "/p/server.key", TLSCAFile: "/p/ca.crt", TLSCRLFile: "/p/revoked.crl", NodeConninfo: ci})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"ssl = 'on'", "ssl_ca_file = '/p/ca.crt'", "ssl_crl_file = '/p/revoked.crl'", "ssl_min_protocol_version = 'TLSv1.3'", "kingres_shard.node_conninfo = "} {
		if !strings.Contains(conf, w) {
			t.Errorf("managed configuration lacks %q:\n%s", w, conf)
		}
	}
	if strings.Contains(conf, "sslpassword") {
		t.Error("the managed configuration must not carry a key password")
	}
}
