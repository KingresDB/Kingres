// SPDX-License-Identifier: AGPL-3.0-only

package standby

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"testing"
	"time"
)

func TestListedNames(t *testing.T) {
	cases := map[string][]string{
		"":                                     {},
		"ANY 1 (kingres_standby_1)":            {"kingres_standby_1"},
		"ANY 1 (Kingres_Standby_2, b)":         {"b", "kingres_standby_2"},
		`FIRST 1 ("My-Standby", plain)`:        {"my-standby", "plain"},
		"kingres_standby_3, kingres_standby_4": {"kingres_standby_3", "kingres_standby_4"},
	}
	for in, want := range cases {
		got := []string{}
		for n := range ListedNames(in) {
			got = append(got, n)
		}
		if len(got) == 0 && len(want) == 0 {
			continue
		}
		sortStrings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: got %v, want %v", in, got, want)
		}
	}
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

func TestSafeNameRefusesWhatChangesTheList(t *testing.T) {
	for _, bad := range []string{"", "*", "a b", "a,b", `a"b`, "a)", "My-Standby", "A"} {
		if SafeName(bad) {
			t.Errorf("%q must not be written into the list", bad)
		}
	}
	if !SafeName("kingres_standby_12") {
		t.Error("the name of a standby was refused")
	}
}

func TestStreamingUnderComparesWithoutCase(t *testing.T) {
	if !StreamingUnder(map[string]bool{"Kingres_Standby_1": true}, "kingres_standby_1") {
		t.Error("PostgreSQL compares names without regard to case")
	}
	if StreamingUnder(map[string]bool{"kingres_standby_1": false}, "kingres_standby_1") {
		t.Error("a walsender that does not stream is not streaming")
	}
}

// The name of a standby goes into synchronous_standby_names only when the standby
// is registered in the catalog, streams, and the extension agrees (README of
// kingres_shard, sequence 1 step 2); a name already in the list stays.
func TestMayNameNeedsRegistrationStreamingAndTheExtension(t *testing.T) {
	asked := 0
	answer := true
	a := &Agent{NameMatches: func(context.Context, int, string) (bool, error) { asked++; return answer, nil }}
	a.Log = slog.New(slog.DiscardHandler)
	name := AppName(7)
	streaming := map[string]bool{name: true}
	e := Entry{ID: 7, Group: 1, Ready: true, CatalogNode: 12}
	ctx := context.Background()

	if !a.mayName(ctx, 1, e, nil, streaming) || asked != 1 {
		t.Fatalf("a registered streaming standby the extension accepts must be named (asked %d)", asked)
	}
	unregistered := e
	unregistered.CatalogNode = 0
	if a.mayName(ctx, 1, unregistered, nil, streaming) {
		t.Error("a standby that is not in the catalog was named: the extension's check needs the registered standby")
	}
	if a.mayName(ctx, 1, e, nil, map[string]bool{}) {
		t.Error("a standby that does not stream was named")
	}
	answer = false
	if a.mayName(ctx, 1, e, nil, streaming) {
		t.Error("a name the extension says matches nobody was named")
	}
	asked = 0
	if !a.mayName(ctx, 1, unregistered, map[string]bool{name: true}, nil) || asked != 0 {
		t.Error("a name that is in the list already stays without a new check")
	}
	a.NameMatches = func(context.Context, int, string) (bool, error) { return true, errors.New("no extension") }
	if a.mayName(ctx, 1, e, nil, streaming) {
		t.Error("a failed check must not name the standby")
	}
}

type fakeRegistrar struct {
	refusals int
	calls    int
}

func (f *fakeRegistrar) Register(context.Context, Entry) (int32, error) { return 1, nil }
func (f *fakeRegistrar) Unregister(context.Context, Entry) error {
	f.calls++
	if f.calls <= f.refusals {
		return &CatalogError{Call: "remove_node", Code: sqlstateCannotCommit, Retry: true}
	}
	return nil
}

// remove_node is refused with KS009 while the primary still names the standby
// (sequence 8): the removal waits for the list and repeats, and gives up with the
// refusal when the list never changes.
func TestUnregisterRepeatsWhileThePrimaryStillNamesTheStandby(t *testing.T) {
	l := NewLeader(&Leader{UnregisterWait: time.Millisecond})
	f := &fakeRegistrar{refusals: 3}
	l.Registrar = f
	if err := l.unregister(context.Background(), Entry{ID: 1}); err != nil || f.calls != 4 {
		t.Fatalf("err %v after %d calls, want success on the fourth", err, f.calls)
	}
	f = &fakeRegistrar{refusals: 1000}
	l.Registrar = f
	err := l.unregister(context.Background(), Entry{ID: 1})
	if !IsCatalogCode(err, sqlstateCannotCommit) || f.calls != 12 {
		t.Fatalf("err %v after %d calls, want the refusal after 12", err, f.calls)
	}
	// another refusal is not repeated
	g := &otherRegistrar{}
	l.Registrar = g
	if err := l.unregister(context.Background(), Entry{ID: 1}); err == nil || g.calls != 1 {
		t.Fatalf("a refusal that is not KS009 must not be repeated: %v after %d", err, g.calls)
	}
}

type otherRegistrar struct{ calls int }

func (o *otherRegistrar) Register(context.Context, Entry) (int32, error) { return 1, nil }
func (o *otherRegistrar) Unregister(context.Context, Entry) error {
	o.calls++
	return errors.New("boom")
}

func TestDiffRegistersReadyStandbysThatAreNotInTheCatalog(t *testing.T) {
	l := NewLeader(&Leader{})
	top := topology{ring: ring("a", "", "b", ""), groups: []groupInfo{{ID: 1, Primary: "a", Port: 5432}}}
	one := int32(9)
	recs := []Record{{ID: 1, Group: 1, Server: "b", PrimaryServer: "a", State: stateReady, Port: 6000}}
	w := l.diff(top, recs, 1)
	if len(w.Register) != 1 || w.empty() {
		t.Fatalf("a ready standby without a node id must be registered: %+v", w)
	}
	recs[0].CatalogNode = &one
	if w := l.diff(top, recs, 1); !w.empty() {
		t.Fatalf("a registered standby needs nothing: %+v", w)
	}
}
