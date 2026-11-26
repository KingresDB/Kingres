// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

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

// A partition alert names its table in typed members, with a name that has a dot, a bar or a percent sign in it read back as it was (no parsing of an
// aspect, no ambiguity).
func TestParseSubjectOfAPartitionAlert(t *testing.T) {
	s := parseSubject("database:shop|tableschema:pub%7Clic|tablename:ev.ents%25")
	if s.DatabaseName == nil || *s.DatabaseName != "shop" || s.TableSchema == nil || *s.TableSchema != "pub|lic" || s.TableName == nil || *s.TableName != "ev.ents%" || s.Aspect != nil {
		t.Errorf("%+v", s)
	}
	if s := parseSubject("database:shop|ca"); s.TableName != nil || s.TableSchema != nil {
		t.Errorf("an alert that is not about a table: %+v", s)
	}
}
