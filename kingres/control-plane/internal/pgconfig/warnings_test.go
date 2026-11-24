// SPDX-License-Identifier: AGPL-3.0-only
// Copyright (c) 2026 RooyeKhat Media Co

package pgconfig

import (
	"strings"
	"testing"
)

func memSetting(unit, boot string) Setting {
	return Setting{Name: "work_mem", VarType: "integer", Unit: unit, MinVal: "64", MaxVal: "2147483647", Context: "user", BootVal: boot}
}

func warnsFor(t *testing.T, s Setting, in string) bool {
	t.Helper()
	r, err := Check(s, in)
	if err != nil {
		t.Fatalf("%s: %v", in, err)
	}
	_, w := unusualWarning(0, s, r)
	return w
}

func TestUnusualWarningEdges(t *testing.T) {
	// work_mem: base unit kB, default 4096 kB (4MB); ten times is 40960 kB, a tenth is 409.6 kB.
	s := memSetting("kB", "4096")
	for in, want := range map[string]bool{
		"4MB":     false, // the default
		"40MB":    false, // exactly ten times: not flagged
		"40961kB": true,  // one unit above ten times
		"41MB":    true,
		"410kB":   false, // just above a tenth
		"409kB":   true,  // just below a tenth
		"64kB":    true,  // the smallest value
		"1GB":     true,
	} {
		if got := warnsFor(t, s, in); got != want {
			t.Errorf("default 4096 kB, %s: warning = %v, want %v", in, got, want)
		}
	}
	// A default that makes a tenth exact: 10240 kB gives exactly 1024 kB.
	s = memSetting("kB", "10240")
	for in, want := range map[string]bool{
		"100MB":   false, // exactly ten times
		"102401":  true,
		"1MB":     false, // exactly a tenth
		"1023kB":  true,
		"1024 kB": false,
	} {
		if got := warnsFor(t, s, in); got != want {
			t.Errorf("default 10240 kB, %s: warning = %v, want %v", in, got, want)
		}
	}
}

func TestUnusualWarningOnlyForMemory(t *testing.T) {
	// A time, a plain integer, a real, a bool and a string are never flagged, however far from the default.
	timeS := Setting{Name: "statement_timeout", VarType: "integer", Unit: "ms", MinVal: "0", MaxVal: "2147483647", BootVal: "100"}
	if warnsFor(t, timeS, "1h") {
		t.Error("a time parameter was flagged")
	}
	count := Setting{Name: "max_connections", VarType: "integer", MinVal: "1", MaxVal: "262143", BootVal: "100"}
	if warnsFor(t, count, "5000") {
		t.Error("a count was flagged")
	}
	realS := Setting{Name: "random_page_cost", VarType: "real", MinVal: "0", MaxVal: "1000", BootVal: "4"}
	if warnsFor(t, realS, "900") {
		t.Error("a real was flagged")
	}
	// A memory parameter whose default is zero, or unknown, is never flagged.
	if warnsFor(t, memSetting("kB", "0"), "1GB") {
		t.Error("a zero default was flagged")
	}
	if warnsFor(t, memSetting("kB", ""), "1GB") {
		t.Error("an unknown default was flagged")
	}
	// The other memory units.
	if !warnsFor(t, Setting{Name: "shared_buffers", VarType: "integer", Unit: "8kB", MinVal: "16", MaxVal: "1073741823", BootVal: "16384"}, "10GB") {
		t.Error("shared_buffers at 10GB (default 128MB) was not flagged")
	}
	if !warnsFor(t, Setting{Name: "x", VarType: "integer", Unit: "B", MinVal: "0", MaxVal: "1000000000", BootVal: "1000"}, "20000") {
		t.Error("a byte parameter at twenty times its default was not flagged")
	}
	// A string value has no base value and is never flagged.
	str := Setting{Name: "TimeZone", VarType: "string", BootVal: "GMT"}
	if warnsFor(t, str, "Europe/Paris") {
		t.Error("a string was flagged")
	}
}

func TestUnusualWarningText(t *testing.T) {
	s := memSetting("kB", "4096")
	r, _ := Check(s, "1GB")
	w, ok := unusualWarning(2, s, r)
	if !ok || w.Code != WarnValueUnusual || w.Index != 2 || w.Pointer != "/changes/2/value" || !strings.Contains(w.Message, "ten times") || !strings.Contains(w.Message, "4MB") {
		t.Fatalf("warning = %+v", w)
	}
	r, _ = Check(s, "64kB")
	w, ok = unusualWarning(0, s, r)
	if !ok || !strings.Contains(w.Message, "tenth") {
		t.Fatalf("warning = %+v", w)
	}
}

func connSnapshot(max, su, rs string, servers ...srv) *snapshot {
	sn := &snapshot{states: map[string]*StateResponse{}, servers: servers}
	for _, s := range servers {
		sn.states[s.ID] = &StateResponse{Settings: []Setting{
			{Name: "max_connections", Setting: max},
			{Name: "superuser_reserved_connections", Setting: su},
			{Name: "reserved_connections", Setting: rs},
		}}
	}
	return sn
}

func identityOf(cs []change) []int {
	out := make([]int, len(cs))
	for i := range out {
		out[i] = i
	}
	return out
}

func TestReservedConnectionsWarning(t *testing.T) {
	a, b := srv{ID: "a", Group: "g1"}, srv{ID: "b", Group: "g2"}
	sn := connSnapshot("100", "3", "0", a, b)
	set := func(name, v string) []change { return []change{{Name: name, Level: LevelCluster, Value: &v}} }
	run := func(sn *snapshot, cs []change, rows []valueRow) []string {
		var out []string
		for _, w := range reservedWarnings(sn, applyToValues(rows, cs), cs, identityOf(cs)) {
			out = append(out, w.Code+"|"+w.Message)
		}
		return out
	}
	// max_connections 4 with 3 reserved: exactly sum + 1, allowed.
	if got := run(sn, set("max_connections", "4"), nil); len(got) != 0 {
		t.Errorf("sum+1: %v", got)
	}
	// max_connections 3 with 3 reserved: PostgreSQL refuses to start.
	got := run(sn, set("max_connections", "3"), nil)
	if len(got) != 1 || !strings.HasPrefix(got[0], WarnReservedConnections) || !strings.Contains(got[0], "server a") {
		t.Errorf("sum: %v", got)
	}
	// Raising a reserved count past the running max_connections.
	if got := run(sn, set("superuser_reserved_connections", "100"), nil); len(got) != 1 {
		t.Errorf("reserved up: %v", got)
	}
	if got := run(sn, set("reserved_connections", "96"), nil); len(got) != 0 {
		t.Errorf("3+96+1 = 100 is allowed: %v", got)
	}
	if got := run(sn, set("reserved_connections", "97"), nil); len(got) != 1 {
		t.Errorf("3+97+1 = 101 > 100: %v", got)
	}
	// A server-level value on b only: the warning names b.
	v := "2"
	cs := []change{{Name: "max_connections", Level: LevelServer, Scope: "b", Value: &v}}
	got = run(sn, cs, nil)
	if len(got) != 1 || !strings.Contains(got[0], "server b") {
		t.Errorf("server level: %v", got)
	}
	// A stored value that decides (a server group override of the reserved count) is read, too.
	rows := []valueRow{{Level: LevelServerGroup, Scope: "g2", Name: "superuser_reserved_connections", Value: "50"}}
	got = run(sn, set("max_connections", "40"), rows)
	if len(got) != 1 || !strings.Contains(got[0], "server b") {
		t.Errorf("stored override: %v", got)
	}
	// Removing the cluster value: the running one stays and is fine.
	cs = []change{{Name: "max_connections", Level: LevelCluster}}
	if got := run(sn, cs, nil); len(got) != 0 {
		t.Errorf("removal: %v", got)
	}
	// A change of another parameter never warns, even when the running values are already wrong.
	bad := connSnapshot("2", "3", "0", a)
	if got := run(bad, set("work_mem", "1MB"), nil); len(got) != 0 {
		t.Errorf("another parameter: %v", got)
	}
	// A server that did not answer (no running values) is skipped.
	none := &snapshot{states: map[string]*StateResponse{}, servers: []srv{a}}
	if got := run(none, set("max_connections", "1"), nil); len(got) != 0 {
		t.Errorf("unknown server: %v", got)
	}
}
