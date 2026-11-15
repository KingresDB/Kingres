// SPDX-License-Identifier: AGPL-3.0-only

package problem

import "testing"

// The 409 codes of removeServer and leaveCluster are in the contract's
// ProblemCode enum and answer 409.
func TestServerRemovalCodes(t *testing.T) {
	for _, tc := range []struct {
		c    Code
		want string
	}{
		{ServerOwnsBuckets, "server_owns_buckets"},
		{ServerHostsMapGroup, "server_hosts_map_group"},
		{LastServer, "last_server"},
	} {
		p := New(tc.c, "d")
		if string(p.Code) != tc.want || p.Status != 409 || p.Title == "" {
			t.Errorf("%s: %+v", tc.want, p)
		}
	}
}
