// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package ops

import "testing"

// The failover and the registration of the standbys in the catalogs leave out the private database of an import, and nothing else: a database that a client made
// under the prefix, or that carries the comment of a task under another name, is a database like any other.
func TestIsPrivateImportDatabase(t *testing.T) {
	priv := privateName(15, "a8ad1b646370")
	for _, tc := range []struct {
		name, comment string
		want          bool
	}{
		{priv, importMarker(priv), true},
		{priv, "", false},
		{priv, importMarker(privateName(16, "a8ad1b646370")), false},
		{priv, "kingres-import:", false},
		{"src", importMarker(priv), false},
		{"dil_base", "", false},
		{"kingres_system", "", false},
		// the final name after the rename keeps the marker until the last server has it: it is a database of the client by then
		{"dif_x", importMarker(priv), false},
	} {
		if got := IsPrivateImportDatabase(tc.name, tc.comment); got != tc.want {
			t.Errorf("IsPrivateImportDatabase(%q, %q) = %v, want %v", tc.name, tc.comment, got, tc.want)
		}
	}
}
