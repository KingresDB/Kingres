// SPDX-License-Identifier: AGPL-3.0-only

// Package auth implements authentication and authorization: the cluster role
// hierarchy, the server-side session store, CSRF tokens, the login rate
// limiter and the PostgreSQL authenticator.
package auth

import (
	"unicode"
	"unicode/utf8"
)

// The four built-in cluster roles and the pseudo role "none".
const (
	RoleNone     = "none"
	RoleStats    = "kingres_stats"
	RoleReadonly = "kingres_readonly"
	RoleOps      = "kingres_ops"
	RoleAdmin    = "kingres_admin"
)

// ClusterRoles lists the built-in cluster roles from the most to the least
// privileged.
var ClusterRoles = []string{RoleAdmin, RoleOps, RoleReadonly, RoleStats}

// rank orders the roles: admin includes ops, ops includes readonly, readonly
// includes stats (kingres/api/README.md, "Authentication and roles").
var rank = map[string]int{
	RoleNone:     0,
	RoleStats:    1,
	RoleReadonly: 2,
	RoleOps:      3,
	RoleAdmin:    4,
}

// IsClusterRole reports whether name is one of the four cluster roles.
func IsClusterRole(name string) bool {
	return name != RoleNone && rank[name] > 0
}

// Allows reports whether a principal that is a member of the given cluster
// roles satisfies the required role of an operation. The required role
// "none" is satisfied by everybody, including a principal without roles.
// An unknown required role is never satisfied (fail closed).
func Allows(have []string, required string) bool {
	need, ok := rank[required]
	if !ok {
		return false
	}
	if need == 0 {
		return true
	}
	for _, r := range have {
		if rank[r] >= need && IsClusterRole(r) {
			return true
		}
	}
	return false
}

// Normalize returns the cluster roles of have, deduplicated and ordered from
// the most to the least privileged; other names are dropped.
func Normalize(have []string) []string {
	set := map[string]bool{}
	for _, r := range have {
		if IsClusterRole(r) {
			set[r] = true
		}
	}
	out := make([]string, 0, len(set))
	for _, r := range ClusterRoles {
		if set[r] {
			out = append(out, r)
		}
	}
	return out
}

// Subset reports whether every role of sub is in all.
func Subset(sub, all []string) bool {
	have := map[string]bool{}
	for _, r := range all {
		have[r] = true
	}
	for _, r := range sub {
		if !have[r] {
			return false
		}
	}
	return true
}

// ValidUsername reports whether s can be a user name at the boundary: valid
// UTF-8 with no control characters (NUL, newlines, escapes, the C1 range and
// the Unicode line and paragraph separators). Names are written to the audit
// file and to PostgreSQL; this keeps both free of them.
func ValidUsername(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) || r == 0xFEFF {
			return false
		}
	}
	return true
}
