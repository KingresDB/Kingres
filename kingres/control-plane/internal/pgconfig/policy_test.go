// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026, RooyeKhat Media Co

package pgconfig

import (
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

func TestManagedRefusedAtEveryLevel(t *testing.T) {
	for _, name := range instance.ManagedNames() {
		s := Setting{Name: name, VarType: "string", Context: "postmaster"}
		c := Classify(s, false)
		if c.Kind != KindManaged || len(c.Levels) != 0 {
			t.Errorf("%s: %+v", name, c)
		}
		for _, lv := range []string{LevelCluster, LevelServerGroup, LevelServer, LevelDatabase} {
			if code, _ := CheckLevel(s, c, lv); code != "config_parameter_managed" {
				t.Errorf("%s at %s: code %q", name, lv, code)
			}
		}
	}
}

func TestRefusedParameters(t *testing.T) {
	for _, name := range []string{"archive_command", "restore_command", "ssl_passphrase_command", "dynamic_library_path", "session_preload_libraries", "local_preload_libraries", "archive_cleanup_command", "jit_provider"} {
		c := Classify(Setting{Name: name, VarType: "string", Context: "sighup"}, true)
		if c.Kind != KindRefused || !c.Hidden || c.Reason.Code == "" || c.Reason.Code == instance.ReasonSuperuserOnly {
			t.Errorf("%s: %+v", name, c)
		}
		if code, _ := CheckLevel(Setting{Name: name}, c, LevelCluster); code != "config_parameter_refused" {
			t.Errorf("%s: %q", name, code)
		}
	}
	// A superuser-only parameter that nobody listed is refused all the same (fail safe).
	if c := Classify(Setting{Name: "future_setting", Context: "sighup"}, true); c.Kind != KindRefused || c.Reason.Code != instance.ReasonSuperuserOnly {
		t.Errorf("unknown hidden parameter: %+v", c)
	}
	for _, s := range []Setting{
		{Name: "fsync", Context: "sighup"}, {Name: "full_page_writes", Context: "sighup"},
		{Name: "allow_system_table_mods", Category: "Developer Options", Context: "superuser"},
		{Name: "block_size", Context: "internal"}, {Name: "kingres_shard.fault_injection", Context: "user"},
	} {
		if c := Classify(s, false); c.Kind != KindRefused {
			t.Errorf("%s: %+v", s.Name, c)
		}
	}
}

func TestLevelsAndClusterOnly(t *testing.T) {
	tz := Classify(Setting{Name: "TimeZone", Context: "user"}, false)
	if tz.ClusterOnly == "" || tz.Allows(LevelServer) || tz.Allows(LevelServerGroup) || !tz.Allows(LevelCluster) || !tz.Allows(LevelDatabase) {
		t.Errorf("TimeZone: %+v", tz)
	}
	if code, _ := CheckLevel(Setting{Name: "TimeZone"}, tz, LevelServer); code != "config_level_not_allowed" {
		t.Errorf("code %q", code)
	}
	lt := Classify(Setting{Name: "log_timezone", Context: "sighup"}, false)
	if lt.Allows(LevelDatabase) || !lt.Allows(LevelCluster) || lt.Allows(LevelServer) {
		t.Errorf("log_timezone: %+v", lt)
	}
	wm := Classify(Setting{Name: "work_mem", Context: "user"}, false)
	for _, l := range []string{LevelCluster, LevelServerGroup, LevelServer, LevelDatabase} {
		if !wm.Allows(l) {
			t.Errorf("work_mem at %s", l)
		}
	}
	sb := Classify(Setting{Name: "shared_buffers", Context: "postmaster"}, false)
	if sb.Allows(LevelDatabase) || !sb.Allows(LevelServer) {
		t.Errorf("shared_buffers: %+v", sb)
	}
	for _, n := range []string{"kingres_shard.default_bucket_count", "kingres_shard.enable_routing", "datestyle", "lc_numeric"} {
		if ClusterOnlyReason(n) == "" {
			t.Errorf("%s should be cluster-only", n)
		}
	}
	if ClusterOnlyReason("work_mem") != "" || ClusterOnlyReason("kingres_shard.node_timeout") != "" {
		t.Error("per-server tuning must not be cluster-only")
	}
}

func TestFloors(t *testing.T) {
	if m, _, ok := Floor("max_prepared_transactions", 1); !ok || m != 1 {
		t.Error("max_prepared_transactions")
	}
	if m, _, ok := Floor("max_wal_senders", 2); !ok || m != 6 {
		t.Errorf("max_wal_senders %d", m)
	}
	if m, _, ok := Floor("max_replication_slots", 3); !ok || m != 5 {
		t.Errorf("max_replication_slots %d", m)
	}
	if _, _, ok := Floor("work_mem", 1); ok {
		t.Error("work_mem has no floor")
	}
}

func TestPickPrecedence(t *testing.T) {
	os := []Override{
		{LevelCluster, "", "1"},
		{LevelServerGroup, "rack-a", "2"},
		{LevelServer, "s1", "3"},
		{LevelServerGroup, "rack-b", "9"},
		{LevelServer, "s2", "8"},
	}
	cases := []struct {
		server, group, want string
		ok                  bool
	}{
		{"s1", "rack-a", "3", true}, // server beats group beats cluster
		{"s3", "rack-a", "2", true},
		{"s3", "", "1", true},
		{"s3", "rack-c", "1", true},
		{"s2", "rack-b", "8", true},
	}
	for _, c := range cases {
		o, ok := Pick(os, c.server, c.group)
		if ok != c.ok || o.Value != c.want {
			t.Errorf("%s/%s: %v %v", c.server, c.group, o, ok)
		}
	}
	if _, ok := Pick(os[1:2], "s9", ""); ok {
		t.Error("a group override applies to no server without a group")
	}
	if _, ok := Pick(nil, "s", "g"); ok {
		t.Error("no overrides")
	}
}

// The settings of pg_stat_statements are the operator's: max and save need a restart or a reload and are set at the three file levels, track and the
// other session settings may also be set for a database. None is managed or refused (the library itself is loaded through the managed
// shared_preload_libraries, which cannot be set), and none must be equal on every server.
func TestStatStatementsSettingsAreSettable(t *testing.T) {
	for _, s := range []Setting{
		{Name: "pg_stat_statements.max", Context: "postmaster"},
		{Name: "pg_stat_statements.save", Context: "sighup"},
		{Name: "pg_stat_statements.track", Context: "superuser"},
		{Name: "pg_stat_statements.track_utility", Context: "superuser"},
		{Name: "pg_stat_statements.track_planning", Context: "superuser"},
	} {
		c := Classify(s, false)
		if c.Kind != KindSettable || c.ClusterOnly != "" || !c.Allows(LevelCluster) || !c.Allows(LevelServerGroup) || !c.Allows(LevelServer) {
			t.Errorf("%s: %+v", s.Name, c)
		}
		if Dropped(s.Name) {
			t.Errorf("%s is dropped", s.Name)
		}
		if want := s.Context == "superuser"; c.Allows(LevelDatabase) != want {
			t.Errorf("%s at database level: %v", s.Name, c.Allows(LevelDatabase))
		}
	}
	// the library list that loads the extension stays managed
	if c := Classify(Setting{Name: "shared_preload_libraries", Context: "postmaster"}, false); c.Kind != KindManaged {
		t.Errorf("shared_preload_libraries: %+v", c)
	}
}
