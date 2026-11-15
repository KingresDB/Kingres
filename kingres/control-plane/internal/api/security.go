// SPDX-License-Identifier: PostgreSQL

package api

import "time"

// RoleCreateRequest is the body of POST /roles.
type RoleCreateRequest struct {
	// Name is the role name.
	Name string `json:"name"`
	// Password is write-only; it reaches PostgreSQL as a SCRAM verifier.
	Password *string `json:"password,omitempty"`
	// CanLogin defaults to true.
	CanLogin *bool `json:"canLogin,omitempty"`
	// CanCreateDatabase and CanCreateRole default to false.
	CanCreateDatabase *bool `json:"canCreateDatabase,omitempty"`
	CanCreateRole     *bool `json:"canCreateRole,omitempty"`
	// ConnectionLimit is -1 (unlimited) by default.
	ConnectionLimit *int `json:"connectionLimit,omitempty"`
	// ValidUntil is the password expiry; null or absent means none.
	ValidUntil *time.Time `json:"validUntil,omitempty"`
	// MemberOf are ordinary roles to join; ClusterRoles the cluster roles.
	MemberOf     []string `json:"memberOf,omitempty"`
	ClusterRoles []string `json:"clusterRoles,omitempty"`
}

// RoleUpdateRequest is the body of PATCH /roles/{roleName}. A present
// ValidUntil with a null value clears the expiry.
type RoleUpdateRequest struct {
	Password          *string      `json:"password,omitempty"`
	CanLogin          *bool        `json:"canLogin,omitempty"`
	CanCreateDatabase *bool        `json:"canCreateDatabase,omitempty"`
	CanCreateRole     *bool        `json:"canCreateRole,omitempty"`
	ConnectionLimit   *int         `json:"connectionLimit,omitempty"`
	ValidUntil        OptionalTime `json:"validUntil"`
}

// OptionalTime distinguishes an absent value, null and a time.
type OptionalTime struct {
	// Set is true when the property was present.
	Set bool
	// Value is nil for null.
	Value *time.Time
}

// UnmarshalJSON implements json.Unmarshaler; it is only called when the
// property is present.
func (o *OptionalTime) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.Value = nil
		return nil
	}
	var t time.Time
	if err := t.UnmarshalJSON(b); err != nil {
		return err
	}
	o.Value = &t
	return nil
}

// RoleMembershipsRequest is the body of PUT /roles/{roleName}/memberships.
type RoleMembershipsRequest struct {
	MemberOf []string `json:"memberOf"`
}

// RoleClusterRolesRequest is the body of PUT /roles/{roleName}/cluster-roles.
type RoleClusterRolesRequest struct {
	ClusterRoles []string `json:"clusterRoles"`
}

// GrantRequest is the body of POST /grants and POST /grants/revocations.
type GrantRequest struct {
	Grantee    string   `json:"grantee"`
	Level      string   `json:"level"`
	Database   string   `json:"database"`
	Schema     string   `json:"schema,omitempty"`
	Table      string   `json:"table,omitempty"`
	Privileges []string `json:"privileges"`
}

// GrantTemplateRequest is the body of POST /grants/template-applications.
type GrantTemplateRequest struct {
	Template string `json:"template"`
	Grantee  string `json:"grantee"`
	Database string `json:"database"`
}
