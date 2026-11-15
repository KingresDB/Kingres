// SPDX-License-Identifier: AGPL-3.0-only

package ops

import (
	"encoding/json"
	"strings"
	"testing"
)

func obj(t *testing.T, s string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestParseServerGroupCreate(t *testing.T) {
	long := strings.Repeat("x", 201)
	tests := []struct {
		name, body, field, code string // field "" means valid
	}{
		{"valid", `{"name":"rack-a"}`, "", ""},
		{"valid mixed case and dots", `{"name":"Rack_A.1-x","description":"Row 1"}`, "", ""},
		{"63 characters", `{"name":"` + strings.Repeat("a", 63) + `"}`, "", ""},
		{"64 characters", `{"name":"` + strings.Repeat("a", 64) + `"}`, "/name", "pattern"},
		{"leading dash", `{"name":"-a"}`, "/name", "pattern"},
		{"leading dot", `{"name":".a"}`, "/name", "pattern"},
		{"space", `{"name":"a b"}`, "/name", "pattern"},
		{"empty", `{"name":""}`, "/name", "pattern"},
		{"no name", `{}`, "/name", "required"},
		{"name number", `{"name":1}`, "/name", "type"},
		{"description 200", `{"name":"a","description":"` + strings.Repeat("x", 200) + `"}`, "", ""},
		{"description 201", `{"name":"a","description":"` + long + `"}`, "/description", "max_length"},
		{"description null", `{"name":"a","description":null}`, "/description", "type"},
		{"unknown", `{"name":"a","serverIds":[]}`, "/serverIds", "unknown_field"},
	}
	for _, tc := range tests {
		req, errs := ParseServerGroupCreate(obj(t, tc.body))
		if tc.field == "" {
			if len(errs) != 0 || req.Name == "" {
				t.Errorf("%s: %+v %+v", tc.name, req, errs)
			}
			continue
		}
		found := false
		for _, e := range errs {
			if e.Field == tc.field && e.Code == tc.code {
				found = true
			}
		}
		if !found {
			t.Errorf("%s: errors %+v do not contain %s/%s", tc.name, errs, tc.field, tc.code)
		}
	}
}

func TestGroupNamesAreCaseSensitiveAndExact(t *testing.T) {
	for _, n := range []string{"rack-a", "Rack-A", "RACK-A"} {
		if !ValidGroupName(n) {
			t.Errorf("%q must be valid", n)
		}
	}
	if ValidGroupName("rack-a\n") || ValidGroupName("rack-a ") {
		t.Error("a name with trailing whitespace or newline must not match")
	}
}

func TestParseServerUpdate(t *testing.T) {
	tests := []struct {
		name, body, field, code string
		group                   *string
		groupSet                bool
		shards                  *bool
	}{
		{name: "group", body: `{"serverGroup":"rack-b"}`, groupSet: true, group: ptr("rack-b")},
		{name: "unassign", body: `{"serverGroup":null}`, groupSet: true},
		{name: "shards", body: `{"shouldHaveShards":false}`, shards: ptrB(false)},
		{name: "both", body: `{"serverGroup":"a","shouldHaveShards":true}`, groupSet: true, group: ptr("a"), shards: ptrB(true)},
		{name: "empty", body: `{}`, field: "/", code: "required"},
		{name: "read-only", body: `{"status":"live"}`, field: "/status", code: "read_only"},
		{name: "read-only next to valid", body: `{"serverGroup":null,"instances":[]}`, field: "/instances", code: "read_only"},
		{name: "unknown", body: `{"x":1}`, field: "/x", code: "unknown_field"},
		{name: "group type", body: `{"serverGroup":false}`, field: "/serverGroup", code: "type"},
		{name: "shards type", body: `{"shouldHaveShards":"no"}`, field: "/shouldHaveShards", code: "type"},
		{name: "shards null", body: `{"shouldHaveShards":null}`, field: "/shouldHaveShards", code: "type"},
	}
	for _, tc := range tests {
		req, errs := ParseServerUpdate(obj(t, tc.body))
		if tc.field != "" {
			found := false
			for _, e := range errs {
				if e.Field == tc.field && e.Code == tc.code {
					found = true
				}
			}
			if !found {
				t.Errorf("%s: errors %+v do not contain %s/%s", tc.name, errs, tc.field, tc.code)
			}
			continue
		}
		if len(errs) != 0 {
			t.Errorf("%s: %+v", tc.name, errs)
			continue
		}
		if req.ServerGroupSet != tc.groupSet || (req.ServerGroup == nil) != (tc.group == nil) || (tc.group != nil && *req.ServerGroup != *tc.group) {
			t.Errorf("%s: group = %v (set %v)", tc.name, req.ServerGroup, req.ServerGroupSet)
		}
		if (req.ShouldHaveShards == nil) != (tc.shards == nil) || (tc.shards != nil && *req.ShouldHaveShards != *tc.shards) {
			t.Errorf("%s: shards = %v", tc.name, req.ShouldHaveShards)
		}
	}
}

func ptr(s string) *string { return &s }
func ptrB(b bool) *bool    { return &b }
