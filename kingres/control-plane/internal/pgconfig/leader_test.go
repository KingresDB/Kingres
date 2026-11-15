// SPDX-License-Identifier: AGPL-3.0-only

package pgconfig

import (
	"github.com/KingresDB/Kingres/kingres/control-plane/internal/api"
	"math/big"
	"strings"
	"testing"
	"time"
)

func TestEstimateOffsetAndUncertainty(t *testing.T) {
	w0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	// The remote clock is 2 s ahead; the exchange took 10 ms on the monotonic clock, 5 ms each way.
	s := Sample{W0: w0, W3: w0.Add(10 * time.Millisecond), Remote: w0.Add(5*time.Millisecond + 2*time.Second), M0: 100 * time.Millisecond, M3: 110 * time.Millisecond}
	m := Estimate(s)
	if m.Offset != 2*time.Second {
		t.Errorf("offset %v", m.Offset)
	}
	if m.RoundTrip != 10*time.Millisecond || m.Uncertainty != 6*time.Millisecond {
		t.Errorf("round trip %v, uncertainty %v", m.RoundTrip, m.Uncertainty)
	}
}

// A step of the wall clock during the probe must not change the round trip, which comes from the
// monotonic clock (the wall times of the sample are 1 hour apart here, the monotonic ones 8 ms).
func TestEstimateUsesMonotonicRoundTrip(t *testing.T) {
	w0 := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	m := Estimate(Sample{W0: w0, W3: w0.Add(time.Hour), Remote: w0, M0: time.Second, M3: time.Second + 8*time.Millisecond})
	if m.RoundTrip != 8*time.Millisecond {
		t.Errorf("round trip %v", m.RoundTrip)
	}
}

func TestBestTakesTheSmallestRoundTrip(t *testing.T) {
	w := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	mk := func(rtt time.Duration, ahead time.Duration) Sample {
		return Sample{W0: w, W3: w.Add(rtt), Remote: w.Add(rtt/2 + ahead), M0: 0, M3: rtt}
	}
	m, ok := Best([]Sample{mk(50*time.Millisecond, 3*time.Second+20*time.Millisecond), mk(4*time.Millisecond, 3*time.Second), mk(30*time.Millisecond, 3*time.Second)})
	if !ok || m.RoundTrip != 4*time.Millisecond || m.Offset != 3*time.Second {
		t.Errorf("%+v %v", m, ok)
	}
	if _, ok := Best(nil); ok {
		t.Error("no samples must give no measurement")
	}
}

func TestExceedsSubtractsTheUncertainty(t *testing.T) {
	th := time.Second
	if over, _ := Exceeds(Measurement{Offset: 1050 * time.Millisecond, Uncertainty: 100 * time.Millisecond}, th); over {
		t.Error("a difference within the uncertainty of the threshold must not alert")
	}
	if over, _ := Exceeds(Measurement{Offset: -1500 * time.Millisecond, Uncertainty: 5 * time.Millisecond}, th); !over {
		t.Error("1.5 s behind is over a 1 s threshold")
	}
	if over, times := Exceeds(Measurement{Offset: 12 * time.Second, Uncertainty: time.Millisecond}, th); !over || times < 10 {
		t.Errorf("12 s is critical (ten times): %v %v", over, times)
	}
	if over, _ := Exceeds(Measurement{Offset: 0, Uncertainty: time.Millisecond}, th); over {
		t.Error("equal clocks")
	}
}

func rat(s string) *big.Rat { r, _ := new(big.Rat).SetString(s); return r }

func snap(pairs []standbyPair, cur map[string]map[string]string, servers ...string) *snapshot {
	sn := &snapshot{pairs: pairs, states: map[string]*StateResponse{}}
	for _, s := range servers {
		sn.servers = append(sn.servers, srv{ID: s})
		st := &StateResponse{Server: s}
		st.Instances = append(st.Instances, InstanceInfo{ID: 1, Role: "primary", Values: cur[s]})
		for _, p := range pairs {
			if p.Server == s {
				st.Instances = append(st.Instances, InstanceInfo{ID: p.InstanceID, Role: "standby", Values: cur[s]})
			}
		}
		sn.states[s] = st
	}
	return sn
}

func index(order []RestartStep, server string, inst int) int {
	for i, o := range order {
		if o.Server == server && o.Instance == inst {
			return i
		}
	}
	return -1
}

func TestRestartOrderUpAndDown(t *testing.T) {
	// Group 0: primary on a, standby on b (instance 100). Group 1: primary on b, standby on a (instance 101).
	pairs := []standbyPair{{Group: 0, Server: "b", InstanceID: 100, PrimaryServer: "a"}, {Group: 1, Server: "a", InstanceID: 101, PrimaryServer: "b"}}
	cur := map[string]map[string]string{"a": {"max_connections": "100"}, "b": {"max_connections": "100"}}
	sn := snap(pairs, cur, "a", "b")
	all := map[string]bool{"a": true, "b": true}

	up := map[string]map[string]*big.Rat{"max_connections": {"a": rat("200"), "b": rat("200")}}
	order, conflict := restartOrder(sn, all, up)
	// Going up: a standby before its primary. b:100 (standby of a's group) before a:1, a:101 before b:1.
	// That is a circle through the two servers (a:1 after b:100, b:1 after a:101, and both servers
	// restart "one at a time" with their primary and standby): no pair-wise order is violated.
	if conflict != "" {
		t.Fatalf("conflict: %s", conflict)
	}
	if index(order, "b", 100) > index(order, "a", 1) || index(order, "a", 101) > index(order, "b", 1) {
		t.Errorf("a standby must be restarted before its primary when a value goes up: %+v", order)
	}

	down := map[string]map[string]*big.Rat{"max_connections": {"a": rat("50"), "b": rat("50")}}
	order, conflict = restartOrder(sn, all, down)
	if conflict != "" {
		t.Fatalf("conflict: %s", conflict)
	}
	if index(order, "a", 1) > index(order, "b", 100) || index(order, "b", 1) > index(order, "a", 101) {
		t.Errorf("a primary must be restarted before its standby when a value goes down: %+v", order)
	}

	same := map[string]map[string]*big.Rat{"max_connections": {"a": rat("100"), "b": rat("100")}}
	if order, conflict = restartOrder(sn, all, same); conflict != "" || len(order) != 4 {
		t.Errorf("unchanged values: %d steps, %q", len(order), conflict)
	}
}

func TestRestartOrderMixedDirectionsConflict(t *testing.T) {
	pairs := []standbyPair{{Group: 0, Server: "b", InstanceID: 100, PrimaryServer: "a"}}
	cur := map[string]map[string]string{"a": {"max_connections": "100", "max_wal_senders": "10"}, "b": {"max_connections": "100", "max_wal_senders": "10"}}
	sn := snap(pairs, cur, "a", "b")
	want := map[string]map[string]*big.Rat{
		"max_connections": {"a": rat("200"), "b": rat("200")},
		"max_wal_senders": {"a": rat("5"), "b": rat("5")},
	}
	_, conflict := restartOrder(sn, map[string]bool{"a": true, "b": true}, want)
	if conflict == "" || !strings.Contains(conflict, "max_connections") || !strings.Contains(conflict, "max_wal_senders") {
		t.Errorf("conflict %q", conflict)
	}
}

func TestRestartOrderOnlyTheNamedServers(t *testing.T) {
	pairs := []standbyPair{{Group: 0, Server: "b", InstanceID: 100, PrimaryServer: "a"}}
	sn := snap(pairs, map[string]map[string]string{"a": {"max_connections": "100"}, "b": {"max_connections": "100"}}, "a", "b")
	order, _ := restartOrder(sn, map[string]bool{"b": true}, nil)
	if len(order) != 2 || order[0].Server != "b" {
		t.Errorf("%+v", order)
	}
}

func TestEffectKinds(t *testing.T) {
	sn := &snapshot{states: map[string]*StateResponse{"a": {Settings: []Setting{{Name: "work_mem", Setting: "4096"}, {Name: "shared_buffers", Setting: "16384"}}}}}
	a := srv{ID: "a"}
	wm := workMem
	sb := sharedBuf
	val := "64MB"
	ch := []change{{Name: "work_mem", Level: LevelCluster, Value: &val}}
	e := effect(sn, wm, a, nil, applyToValues(nil, ch), ch)
	if e.TakesEffect != "reload" || *e.Before != "4MB" || *e.After != "64MB" {
		t.Errorf("work_mem: %+v", e)
	}
	same := "4MB"
	ch = []change{{Name: "work_mem", Level: LevelCluster, Value: &same}}
	if e := effect(sn, wm, a, nil, applyToValues(nil, ch), ch); e.TakesEffect != "unchanged" {
		t.Errorf("same value: %+v", e)
	}
	big := "1GB"
	ch = []change{{Name: "shared_buffers", Level: LevelServer, Scope: "a", Value: &big}}
	if e := effect(sn, sb, a, nil, applyToValues(nil, ch), ch); e.TakesEffect != "restart" {
		t.Errorf("shared_buffers: %+v", e)
	}
	// A server that cannot be read waits.
	if e := effect(sn, wm, srv{ID: "gone"}, nil, nil, nil); e.TakesEffect != "waits" {
		t.Errorf("unreachable server: %+v", e)
	}
	// Removing the only override falls back to a lower level: not unchanged.
	before := []valueRow{{Level: LevelCluster, Name: "work_mem", Value: "64MB"}}
	ch = []change{{Name: "work_mem", Level: LevelCluster}}
	if e := effect(sn, wm, a, before, applyToValues(before, ch), ch); e.TakesEffect != "reload" {
		t.Errorf("removed override: %+v", e)
	}
}

func TestHashIgnoresTheHeader(t *testing.T) {
	a := "# Generated\n# kingresd configuration revision 3\nwork_mem = '8MB'\n"
	b := "# Generated\n# kingresd configuration revision 9\nwork_mem = '8MB'\n"
	if HashOf(a) != HashOf(b) {
		t.Error("the revision header must not change the hash")
	}
	if HashOf(a) == HashOf(a+"x = '1'\n") {
		t.Error("a setting must change the hash")
	}
}

func TestParamFilter(t *testing.T) {
	p := api.ConfigParameter{Name: "work_mem", Description: "Sets the base maximum amount of memory", Category: "Resource Usage / Memory", Kind: "settable",
		Servers: []api.ConfigServerValue{{ServerID: "a", State: StateApplied}, {ServerID: "b", State: StatePendingRestart}}, Differs: true}
	yes := []ParamFilter{{}, {Q: "MEM"}, {Q: "maximum amount"}, {Category: "resource usage"}, {Category: "Resource Usage / Memory"}, {Pending: true}, {State: StatePendingRestart}, {Differing: true}, {Kind: "settable"}}
	no := []ParamFilter{{Q: "zzz"}, {Category: "Resource"}, {Category: "Write-Ahead Log"}, {Changed: true}, {State: StateFailed}, {Kind: "managed"}}
	for i, f := range yes {
		if !f.match(p) {
			t.Errorf("filter %d should match: %+v", i, f)
		}
	}
	for i, f := range no {
		if f.match(p) {
			t.Errorf("filter %d should not match: %+v", i, f)
		}
	}
}
