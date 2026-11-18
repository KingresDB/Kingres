// SPDX-License-Identifier: AGPL-3.0-only

package problem

import "testing"

// The codes of contract 0.11.0 (ledger phase 7) are in the ProblemCode enum and answer 409.
func TestLedgerCodesOfContract011(t *testing.T) {
	for _, tc := range []struct {
		c    Code
		want string
	}{
		{LedgerNotPublished, "ledger_not_published"},
		{OriginPrefixLocked, "origin_prefix_locked"},
		{SigningKeyMissing, "signing_key_missing"},
		{LastSigningKey, "last_signing_key"},
		{ChainNotDiverged, "chain_not_diverged"},
	} {
		p := New(tc.c, "d")
		if string(p.Code) != tc.want || p.Status != 409 || p.Title == "" || p.Type != "https://kingresdb.dev/problems/"+slug(tc.want) {
			t.Errorf("%s: %+v", tc.want, p)
		}
	}
}

func slug(code string) string {
	b := []byte(code)
	for i := range b {
		if b[i] == '_' {
			b[i] = '-'
		}
	}
	return string(b)
}
