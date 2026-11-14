// SPDX-License-Identifier: PostgreSQL

package api

// ServerGroupCreateRequest is the body of POST /server-groups.
type ServerGroupCreateRequest struct {
	// Name is the unique, case-sensitive group name.
	Name string `json:"name"`
	// Description is free text of at most 200 characters.
	Description string `json:"description,omitempty"`
}

// ServerUpdateRequest is the body of PATCH /servers/{serverId}. A member that
// is absent from the body is not changed, which is why presence is tracked:
// "serverGroup": null (remove from the group) differs from no serverGroup.
type ServerUpdateRequest struct {
	// ServerGroupSet tells that the body contained "serverGroup"; ServerGroup is
	// then the group name, or nil to unassign.
	ServerGroupSet bool
	ServerGroup    *string
	// ShouldHaveShards is the new value when the body contained it.
	ShouldHaveShards *bool
}
