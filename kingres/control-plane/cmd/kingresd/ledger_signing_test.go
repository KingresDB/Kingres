// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/ledgerkeys"
)

// The default origin prefix is made from the server id, which may be any host name or address; whatever it is, the
// result is a name the trust file and the signed notes accept.
func TestTheDefaultOriginPrefixIsAlwaysAValidName(t *testing.T) {
	for _, host := range []string{"db1.example.com", "10.0.0.7", "[fe80::1]", "with space", "plus+sign", "slash/in/it", "", "---", "caf\u00e9", "tab\there", strings.Repeat("h", 300), "host\x00name", "https://evil"} {
		p := defaultOriginPrefix(host)
		if err := ledgerkeys.ValidPrefix(p); err != nil {
			t.Errorf("host %q gives the prefix %q: %v", host, p, err)
		}
		if !strings.HasSuffix(p, "kingres-ledger") {
			t.Errorf("host %q gives %q", host, p)
		}
	}
	if got := defaultOriginPrefix("db1.example.com"); got != "db1.example.com/kingres-ledger" {
		t.Errorf("the usual case: %q", got)
	}
}
