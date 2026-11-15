// SPDX-License-Identifier: PostgreSQL

package internalapi

import "testing"

func TestURLRefusesAnythingButHostPort(t *testing.T) {
	for _, a := range []string{"", "host", "host:0", "host:99999", "user@host:8092", "host:8092/x", "host:8092?x", "ho st:1", "host:1#f", "//host:1"} {
		if u, err := URL(a, "/ping"); err == nil {
			t.Errorf("address %q accepted: %s", a, u)
		}
	}
	for _, p := range []string{"ping", "/a?x=1", "/a#b", "/../x", `/a\b`, "/a\nb"} {
		if u, err := URL("db1.example.com:8092", p); err == nil {
			t.Errorf("path %q accepted: %s", p, u)
		}
	}
	for _, p := range []string{"/bootstrap/prepare", "/globals/apply", "/instance/status", "/forward"} {
		if _, err := URL("db1.example.com:8092", p); err != nil {
			t.Errorf("path %q refused: %v", p, err)
		}
	}
	u, err := URL("db1.example.com:8092", "/ping")
	if err != nil || u != "https://db1.example.com:8092/internal/v1/ping" {
		t.Fatalf("%q %v", u, err)
	}
	if u, err = URL("[::1]:8092", "/ping"); err != nil || u != "https://[::1]:8092/internal/v1/ping" {
		t.Fatalf("%q %v", u, err)
	}
}
