// SPDX-License-Identifier: AGPL-3.0-only

package instance

import (
	"errors"
	"strings"
	"testing"
)

func TestRenderHBA(t *testing.T) {
	got, err := RenderHBA([]HBARule{
		{Type: "hostssl", Databases: []string{"shop", "all"}, Users: []string{"alice"}, Address: "10.0.0.0/8", Method: "scram-sha-256"},
		{Type: "host", Databases: []string{"all"}, Users: []string{"all"}, Address: "all", Method: "reject"},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(got), "\n")
	// Service rules come first and are fixed.
	if !strings.HasPrefix(lines[2], "local   all  kingresd") || !strings.Contains(lines[3], "127.0.0.1/32") {
		t.Errorf("service rules missing:\n%s", got)
	}
	if !strings.Contains(got, `hostssl   "shop","all"`) && !strings.Contains(got, `"shop",all`) {
		t.Errorf("rule not rendered:\n%s", got)
	}
	if !strings.Contains(got, `"shop",all "alice" 10.0.0.0/8 scram-sha-256`) {
		t.Errorf("unexpected rendering:\n%s", got)
	}
}

func TestRenderHBARefusals(t *testing.T) {
	ok := HBARule{Type: "host", Databases: []string{"d"}, Users: []string{"u"}, Address: "10.0.0.0/8", Method: "scram-sha-256"}
	mut := func(f func(*HBARule)) HBARule { r := ok; f(&r); return r }
	bad := map[string]HBARule{
		"newline in db":      mut(func(r *HBARule) { r.Databases = []string{"a\nhost all all all trust"} }),
		"space in user":      mut(func(r *HBARule) { r.Users = []string{"a b"} }),
		"comma":              mut(func(r *HBARule) { r.Users = []string{"a,b"} }),
		"quote":              mut(func(r *HBARule) { r.Users = []string{`a"b`} }),
		"hash":               mut(func(r *HBARule) { r.Databases = []string{"a#b"} }),
		"file include":       mut(func(r *HBARule) { r.Users = []string{"@/etc/passwd"} }),
		"group":              mut(func(r *HBARule) { r.Users = []string{"+admins"} }),
		"empty list":         mut(func(r *HBARule) { r.Databases = nil }),
		"bad type":           mut(func(r *HBARule) { r.Type = "local" }),
		"trust":              mut(func(r *HBARule) { r.Method = "trust" }),
		"cert without ssl":   mut(func(r *HBARule) { r.Method = "cert" }),
		"address with words": mut(func(r *HBARule) { r.Address = "10.0.0.0/8 trust" }),
		"address no mask":    mut(func(r *HBARule) { r.Address = "10.0.0.1" }),
		"address newline":    mut(func(r *HBARule) { r.Address = "10.0.0.0/8\nhost all all all trust" }),
		"hostname":           mut(func(r *HBARule) { r.Address = "evil.example.com" }),
	}
	for name, r := range bad {
		if _, err := RenderHBA([]HBARule{r}, false); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, err := RenderHBA([]HBARule{ok}, true); err == nil {
		t.Error("plain host rule accepted while TLS is required")
	}
	if _, err := RenderHBA([]HBARule{mut(func(r *HBARule) { r.Type = "hostssl"; r.Method = "cert" })}, true); err != nil {
		t.Errorf("hostssl cert rejected under tlsRequired: %v", err)
	}
}

func FuzzRenderHBANoExtraLines(f *testing.F) {
	for _, s := range []string{"a", "a\nhost all all all trust", "x y", "\"", "all"} {
		f.Add(s, s, s)
	}
	f.Fuzz(func(t *testing.T, db, user, addr string) {
		out, err := RenderHBA([]HBARule{{Type: "host", Databases: []string{db}, Users: []string{user}, Address: addr, Method: "scram-sha-256"}}, false)
		if err != nil {
			return
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		if len(lines) != 7 {
			t.Fatalf("one rule produced %d lines:\n%s", len(lines)-6, out)
		}
		if strings.Contains(lines[6], "trust") && !strings.Contains(lines[6], `"`) {
			t.Fatalf("trust outside quotes: %s", lines[6])
		}
		if f := strings.Fields(lines[6]); len(f) != 5 {
			t.Fatalf("rule has %d fields: %q", len(f), lines[6])
		}
	})
}

func TestRenderManaged(t *testing.T) {
	got, err := RenderManaged(ManagedSettings{Port: 56010, ClusterName: "it's", SocketDir: "/tmp/x/run",
		TLSCertFile: "/etc/ssl/c.pem", TLSKeyFile: "/etc/ssl/k.pem", Extra: map[string]string{"work_mem": "8MB"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"port = '56010'", "shared_preload_libraries = 'kingres_shard,kingres_ledger,kingres_columnar'",
		"password_encryption = 'scram-sha-256'", "cluster_name = 'it''s'", "ssl = 'on'", "work_mem = '8MB'"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "max_prepared_transactions = '0'") || !strings.Contains(got, "max_prepared_transactions = '100'") {
		t.Errorf("max_prepared_transactions not positive:\n%s", got)
	}
	bad := []ManagedSettings{
		{Port: 80},
		{Port: 56010, TLSCertFile: "/a"},
		{Port: 56010, ClusterName: "a\nb"},
		{Port: 56010, SocketDir: "relative"},
		{Port: 56010, SocketDir: "/a/../etc"},
		{Port: 56010, SocketDir: "/a/./b"},
		{Port: 56010, Extra: map[string]string{"hba_file": "/etc/x"}},
		{Port: 56010, Extra: map[string]string{"data_directory": "/etc"}},
		{Port: 56010, Extra: map[string]string{"x = 'a'\nport": "1"}},
		{Port: 56010, Extra: map[string]string{"port": "1"}},
		{Port: 56010, Extra: map[string]string{"a": "b\nshared_preload_libraries = ''"}},
	}
	for i, s := range bad {
		if _, err := RenderManaged(s); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
