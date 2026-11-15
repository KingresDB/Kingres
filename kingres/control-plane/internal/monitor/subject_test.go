// SPDX-License-Identifier: PostgreSQL

package monitor

import "testing"

func TestParseSubjectIsTyped(t *testing.T) {
	s := parseSubject("server:10.0.0.1|behind:10.0.0.3")
	if s.ServerID == nil || *s.ServerID != "10.0.0.1" || s.BehindServerID == nil || *s.BehindServerID != "10.0.0.3" || s.ShardGroupID != nil {
		t.Errorf("replicationPending subject: %+v", s)
	}
	s = parseSubject("server:a|group:4")
	if s.ShardGroupID == nil || *s.ShardGroupID != 4 || *s.ServerID != "a" {
		t.Errorf("group subject: %+v", s)
	}
	s = parseSubject("server:a|database:shop")
	if s.DatabaseName == nil || *s.DatabaseName != "shop" {
		t.Errorf("database subject: %+v", s)
	}
	s = parseSubject("server:a|ca")
	if s.Aspect == nil || *s.Aspect != "ca" {
		t.Errorf("aspect: %+v", s)
	}
	if s := parseSubject("cluster"); s.ServerID != nil || s.Aspect != nil {
		t.Errorf("cluster subject must be empty: %+v", s)
	}
}
