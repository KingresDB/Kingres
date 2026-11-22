// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package pgconfig

import (
	"math/big"
	"strings"
	"testing"

	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/instance"
)

// P13R-01: a parameter that changes how the own sessions of kingresd behave is refused at every level.
func TestControlPlaneSessionSettingsAreRefused(t *testing.T) {
	for _, name := range []string{"default_transaction_read_only", "transaction_read_only", "default_transaction_isolation", "session_replication_role",
		"statement_timeout", "lock_timeout", "idle_in_transaction_session_timeout", "idle_session_timeout", "transaction_timeout", "search_path", "row_security"} {
		s := Setting{Name: name, VarType: "string", Context: "user", Category: "Client Connection Defaults"}
		c := Classify(s, false)
		if c.Kind != KindRefused || c.Reason.Code != instance.ReasonControlPlane || len(c.Levels) != 0 {
			t.Errorf("%s: %+v", name, c)
		}
		for _, lv := range []string{LevelCluster, LevelServerGroup, LevelServer, LevelDatabase} {
			if code, _ := CheckLevel(s, c, lv); code != "config_parameter_refused" {
				t.Errorf("%s at %s: %q", name, lv, code)
			}
		}
	}
	// Other session defaults stay settable.
	if c := Classify(Setting{Name: "work_mem", Context: "user"}, false); c.Kind != KindSettable {
		t.Errorf("work_mem: %+v", c)
	}
}

// P13R-02: settings whose bad value stops the postmaster or locks kingresd out are managed or refused.
func TestSettingsThatStopTheServerAreNotSettable(t *testing.T) {
	for _, name := range []string{"unix_socket_permissions", "unix_socket_group", "unix_socket_directories", "listen_addresses", "port"} {
		if c := Classify(Setting{Name: name, Context: "postmaster"}, false); c.Kind != KindManaged {
			t.Errorf("%s: %+v", name, c)
		}
	}
	for _, name := range []string{"huge_pages", "huge_page_size", "shared_memory_type", "dynamic_shared_memory_type", "event_source", "bonjour",
		"ssl_crl_dir", "ssl_ecdh_curve", "ssl_prefer_server_ciphers", "ssl_cert_file", "ssl_renegotiation_limit"} {
		if c := Classify(Setting{Name: name, Context: "postmaster"}, false); c.Kind == KindSettable {
			t.Errorf("%s is settable", name)
		}
	}
	// P13R-14: a log destination that leaves the collector files empty is managed.
	for _, name := range []string{"log_destination", "syslog_facility", "syslog_ident"} {
		if c := Classify(Setting{Name: name, Context: "sighup"}, false); c.Kind != KindManaged {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

// P13R-04: the value of a connection string is never returned, whatever PostgreSQL says to the reading role.
func TestConnectionStringsAreAlwaysHidden(t *testing.T) {
	for _, name := range []string{"primary_conninfo", "kingres_shard.node_conninfo"} {
		if c := Classify(Setting{Name: name, Context: "postmaster"}, false); !c.Hidden || c.Kind != KindManaged {
			t.Errorf("%s: %+v", name, c)
		}
	}
}

// P13R-05: a stored name that has become managed or refused is dropped from what is written.
func TestDroppedNames(t *testing.T) {
	for _, n := range []string{"wal_level", "unix_socket_permissions", "archive_command", "my_ext.restore_command", "default_transaction_read_only", "fsync", "ssl_crl_dir", "Port"} {
		if !Dropped(n) {
			t.Errorf("%s should be dropped", n)
		}
	}
	for _, n := range []string{"work_mem", "max_connections", "TimeZone", "kingres_shard.node_timeout"} {
		if Dropped(n) {
			t.Errorf("%s should not be dropped", n)
		}
	}
}

// P13R-05: a stored row is found by name, level and scope, so that its removal is accepted whatever the name has become.
func TestStoredRowsAreFound(t *testing.T) {
	stored := []valueRow{{Level: LevelCluster, Name: "wal_level", Value: "logical"}, {Level: LevelServerGroup, Scope: "gone", Name: "work_mem", Value: "1MB"}}
	if !hasStored(stored, "wal_level", LevelCluster, "") || !hasStored(stored, "work_mem", LevelServerGroup, "gone") {
		t.Fatal("stored rows are not found")
	}
	if hasStored(stored, "wal_level", LevelServer, "") || hasStored(stored, "fsync", LevelCluster, "") {
		t.Error("a row that is not stored was found")
	}
}

// P13R-06: after a failover the primary above a standby is the instance of the promoted server, not instance 1 of
// the server the standby was created for.
func TestRestartOrderAfterFailover(t *testing.T) {
	// Group 0 was created with its primary on a; standbys on b (instance 100) and c (instance 100). A failover promoted
	// the standby on b: the primary of group 0 is now b:100, and the standby on c follows it.
	pairs := []standbyPair{
		{Group: 0, Server: "b", InstanceID: 100, PrimaryServer: "a"},
		{Group: 0, Server: "c", InstanceID: 100, PrimaryServer: "a"},
	}
	cur := map[string]map[string]string{"a": {"max_connections": "100"}, "b": {"max_connections": "100"}, "c": {"max_connections": "100"}}
	sn := snap(pairs, cur, "a", "b", "c")
	sn.primaries = map[int32]string{0: "b"}
	sn.states["b"].Instances[1].Role = "primary" // the promoted instance
	all := map[string]bool{"b": true, "c": true}
	up := map[string]map[string]*big.Rat{"max_connections": {"a": rat("200"), "b": rat("200"), "c": rat("200")}}
	order, conflict := restartOrder(sn, all, up)
	if conflict != "" {
		t.Fatalf("conflict %q", conflict)
	}
	// Going up, a standby is restarted before its primary: c:100 before b:100.
	if index(order, "c", 100) < 0 || index(order, "b", 100) < 0 || index(order, "c", 100) > index(order, "b", 100) {
		t.Errorf("the standby on c must precede the promoted primary on b: %+v", order)
	}
	if got := order[index(order, "b", 100)].Role; got != "primary" {
		t.Errorf("the promoted instance is listed as %q", got)
	}
	down := map[string]map[string]*big.Rat{"max_connections": {"a": rat("50"), "b": rat("50"), "c": rat("50")}}
	order, _ = restartOrder(sn, all, down)
	if index(order, "b", 100) > index(order, "c", 100) {
		t.Errorf("going down the promoted primary on b precedes the standby on c: %+v", order)
	}
	if n, i := primaryNode(sn, pairs[1]); n != "b" || i != 100 {
		t.Errorf("primary node %s:%d", n, i)
	}
	// Without the record of the leader the server the standby was created for is the primary.
	sn.primaries = nil
	if n, i := primaryNode(sn, pairs[1]); n != "a" || i != 1 {
		t.Errorf("fallback primary node %s:%d", n, i)
	}
}

// P13R-12: the audit lists the names and levels of an attempt and never a value.
func TestAttemptedHasNoValues(t *testing.T) {
	v := "touch /tmp/x"
	sc := "s1"
	got := Attempted(api.ConfigChangeRequest{Changes: []api.ConfigChangeItem{{Name: "Archive_Command", Level: LevelServer, ScopeID: &sc, Value: &v}, {Name: "work_mem", Level: LevelCluster}}})
	if len(got) != 2 || got[0]["name"] != "archive_command" || got[0]["scopeId"] != "s1" || got[0]["remove"] != false || got[1]["remove"] != true {
		t.Fatalf("%v", got)
	}
	for _, g := range got {
		for _, x := range g {
			if s, ok := x.(string); ok && strings.Contains(s, "touch") {
				t.Errorf("a value is in the audit details: %v", g)
			}
		}
	}
}

// P13R-12: the audit of an accepted change has the old and the normalized new value, and never the stored value of a
// parameter that is no longer settable.
func TestDescribeChanges(t *testing.T) {
	v := "8MB"
	stored := []valueRow{{Level: LevelCluster, Name: "work_mem", Value: "4MB"}, {Level: LevelCluster, Name: "archive_command", Value: "secret"}}
	got := describe(stored, []change{{Name: "work_mem", Level: LevelCluster, Value: &v}, {Name: "archive_command", Level: LevelCluster}})
	if got[0]["oldValue"] != "4MB" || got[0]["newValue"] != "8MB" {
		t.Errorf("%v", got[0])
	}
	if got[1]["oldValue"] != nil || got[1]["newValue"] != nil {
		t.Errorf("a refused name leaked its stored value: %v", got[1])
	}
}

// P13R-10: every offending server is named, and the removal of an override that the running value comes from makes
// no claim (what the server falls back to is not known here).
func TestReservedConnectionsWarningEdges(t *testing.T) {
	a, b := srv{ID: "a"}, srv{ID: "b"}
	sn := connSnapshot("100", "3", "0", a, b)
	v := "3"
	cs := []change{{Name: "max_connections", Level: LevelCluster, Value: &v}}
	got := reservedWarnings(sn, applyToValues(nil, cs), cs, []int{0})
	if len(got) != 1 || !strings.Contains(got[0].Message, "server a") || !strings.Contains(got[0].Message, "same holds on b") {
		t.Errorf("both servers must be named: %+v", got)
	}
	// The running 2 comes from the override file; removing the override falls back to something unknown.
	low := connSnapshot("2", "3", "0", a)
	for i := range low.states["a"].Settings {
		if low.states["a"].Settings[i].Name == "max_connections" {
			low.states["a"].Settings[i].Source = "configuration file"
			low.states["a"].Settings[i].SourceFile = "/data/pgdata/" + instance.ConfigFileName
		}
	}
	rm := []change{{Name: "max_connections", Level: LevelCluster}}
	before := []valueRow{{Level: LevelCluster, Name: "max_connections", Value: "2"}}
	if got := reservedWarnings(low, applyToValues(before, rm), rm, []int{0}); len(got) != 0 {
		t.Errorf("a removal must not claim the old override value: %+v", got)
	}
}

// P13R-15: the failed validation and the incomplete apply carry their documented task codes.
func TestTaskErrorsAreCoded(t *testing.T) {
	var e error = &taskError{code: "validation_failed", msg: "x"}
	c, ok := e.(interface{ TaskCode() string })
	if !ok || c.TaskCode() != "validation_failed" {
		t.Fatal("not coded")
	}
	if e.(interface{ TaskRetryable() bool }).TaskRetryable() {
		t.Error("a failed validation is not retryable")
	}
}
