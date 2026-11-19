// SPDX-License-Identifier: AGPL-3.0-only

package cli

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KingresDB/Kingres/kingres/verifier/internal/aggregate"
	"github.com/KingresDB/Kingres/kingres/verifier/internal/testdir"
)

// serve serves the files of the world over a local HTTP server, as a publication does: GET of the
// layout, 404 for anything else, no redirect. The world's files are read at each request, so a test
// changes what the next poll sees by changing the files between two polls.
func serve(t *testing.T, w *world) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if strings.Contains(p, "..") || p == "" {
			http.NotFound(rw, r)
			return
		}
		b, err := os.ReadFile(filepath.Join(w.dir, filepath.FromSlash(p)))
		if err != nil {
			http.NotFound(rw, r)
			return
		}
		_, _ = rw.Write(b)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// between sets what happens between poll n and poll n+1 (the sleep of the monitor).
func between(t *testing.T, steps ...func()) {
	t.Helper()
	var n atomic.Int32
	old := sleepFn
	sleepFn = func(time.Duration) {
		i := int(n.Add(1)) - 1
		if i < len(steps) {
			steps[i]()
		}
	}
	t.Cleanup(func() { sleepFn = old })
}

func (w *world) monitorArgs(srv *httptest.Server, state string, polls int, more ...string) []string {
	args := append([]string{"monitor", "--trust", w.trust, "--source", srv.URL, "--state", state, "--polls", fmtInt(polls), "--interval", "1ms", "--now", "2026-11-19T12:00:00Z",
		"--log", "chain:" + w.chains[0].idHex + ":e1", "--log", "aggregate:" + w.aggID}, more...)
	if _, err := os.Stat(filepath.Join(state, monitorMeta)); err != nil {
		args = append(args, "--init") // a first run is a run the user says is the first
	}
	return args
}

func fmtInt(n int) string { return itoa(n) }

func monitorWorld(t *testing.T) (*world, *chainFix, string) {
	w := newWorld(t)
	a := makeChain(0xa1, 8, nil)
	w.addChain(a, 5)
	w.appendRound(10_000_000, member(a, 5, 0))
	return w, a, filepath.Join(testdir.New(t), "state")
}

func TestMonitorFollowsAGrowingPublicationAndAuditsTheNewEntries(t *testing.T) {
	w, a, st := monitorWorld(t)
	srv := serve(t, w)
	between(t, func() {
		w.publishChain(a, 8)
		w.appendRound(20_000_000, member(a, 8, 0))
	})
	out := wantRun(t, 0, w.monitorArgs(srv, st, 2), "VERIFIED:", "poll 1 chain:", "poll 2 chain:", "block header(s) checked", "aggregate leaf(s) checked")
	if !strings.Contains(out, "size 8") {
		t.Fatal(out)
	}
	// a later run on the same state goes on from there and reads nothing new
	wantRun(t, 0, w.monitorArgs(srv, st, 1, "--once"), "VERIFIED:")
}

func stateBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".state") {
			b, _ := os.ReadFile(filepath.Join(dir, e.Name()))
			out[e.Name()] = string(b)
		}
	}
	return out
}

func TestMonitorReportsARollbackToAnOlderGenuineCheckpointAndKeepsItsState(t *testing.T) {
	w, a, st := monitorWorld(t)
	srv := serve(t, w)
	w.publishChain(a, 8)
	var before map[string]string
	between(t, func() {
		before = stateBytes(t, st)
		w.publishChain(a, 5) // an old genuine checkpoint: signed, verifies, and is smaller
	})
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step rollback", "smaller than 8")
	after := stateBytes(t, st)
	for k, v := range before {
		if after[k] != v {
			t.Fatalf("the state %s moved: a rollback must never change it", k)
		}
	}
	found := false
	ents, _ := os.ReadDir(st)
	for _, e := range ents {
		if strings.Contains(e.Name(), "conflict") {
			found = true
		}
	}
	if !found {
		t.Fatal("the conflicting note was not saved beside the state")
	}
}

func TestMonitorReportsAForkOfTheAggregateAcrossTwoPolls(t *testing.T) {
	// the case of P8-01: the publisher signs a second root for an aggregate size
	w, a, st := monitorWorld(t)
	srv := serve(t, w)
	w.appendRound(20_000_000, member(a, 8, 0)) // aggregate of size 2
	w.publishChain(a, 8)
	between(t, func() {
		w.leaves[1].GeneratedAtUS = 20_000_001 // another second leaf: the same size, another root
		w.aggEnc[1] = w.leaves[1].Encode()
		w.publishAggregate(2)
	})
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step fork", "two histories")
	// a fork that shows only at a larger size: the first two leaves are not the ones that were seen
	w, a, st = monitorWorld(t)
	srv = serve(t, w)
	w.appendRound(20_000_000, member(a, 5, 0))
	w.appendRound(30_000_000, member(a, 5, 0))
	between(t, func() {
		w.leaves[0].GeneratedAtUS = 9_999_999
		w.aggEnc[0] = w.leaves[0].Encode()
		w.appendRound(40_000_000, member(a, 5, 0))
	})
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step fork")
}

func TestMonitorReportsAnEventNumberSeenTwiceWithDifferentText(t *testing.T) {
	w, _, st := monitorWorld(t)
	srv := serve(t, w)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	between(t, func() {
		// the same number, a different text, validly signed
		w.put("events/000000", []byte(signText(strings.Replace(eventText(0, "key-added", keyAddedLines(w.s1)...), "10:00:00Z", "10:00:07Z", 1), w.prefix, w.s1)))
	})
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step events", "event 000000 is not the one you saw")
	// an event that is gone
	w, _, st = monitorWorld(t)
	srv = serve(t, w)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	between(t, func() { _ = os.Remove(filepath.Join(w.dir, "events", "000000")) })
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step events", "is not there now")
	// an event that only gains a signature line is the same event
	w, _, st = monitorWorld(t)
	srv = serve(t, w)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	between(t, func() { w.event(0, "key-added", []signer{w.s1, w.s2}, keyAddedLines(w.s1)...) })
	wantRun(t, 0, w.monitorArgs(srv, st, 2), "VERIFIED:")
}

func TestMonitorReportsAChainThatWasRetiredAndContinues(t *testing.T) {
	w, a, st := monitorWorld(t)
	srv := serve(t, w)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "chain-retired", []signer{w.s1}, chainRetiredLines(a, 4, "group-removed")...) // 5 blocks
	between(t, func() { w.publishChain(a, 8) })
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step retired-continues", "a retired chain continues")
	// the same event signed by a key the trust chain does not authorise retires nothing
	w, a, st = monitorWorld(t)
	srv = serve(t, w)
	w.event(0, "key-added", []signer{w.s1}, keyAddedLines(w.s1)...)
	w.event(1, "chain-retired", []signer{w.s2}, chainRetiredLines(a, 4, "group-removed")...)
	between(t, func() { w.publishChain(a, 8) })
	wantRun(t, 0, w.monitorArgs(srv, st, 2), "VERIFIED:", "was skipped")
}

func TestMonitorReportsARetiredMemberThatAppearsAgain(t *testing.T) {
	w, a, st := monitorWorld(t)
	srv := serve(t, w)
	w.appendRound(15_000_000, member(a, 5, aggregate.FlagRetired))
	between(t, func() { w.appendRound(20_000_000, member(a, 5, 0)) })
	wantRun(t, 1, w.monitorArgs(srv, st, 2), "FAILED at step retired-reappears")
	// in one scan
	w, a, st = monitorWorld(t)
	srv = serve(t, w)
	w.appendRound(15_000_000, member(a, 5, aggregate.FlagRetired))
	w.appendRound(20_000_000, member(a, 5, 0))
	wantRun(t, 1, w.monitorArgs(srv, st, 1), "FAILED at step retired-reappears")
}

func TestMonitorUnreachableIsStatus3AndRedirectsAreNotFollowed(t *testing.T) {
	w, _, st := monitorWorld(t)
	down := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { http.Error(rw, "no", 503) }))
	defer down.Close()
	wantRun(t, 3, w.monitorArgs(down, st, 1), "INCOMPLETE", "could not be reached")
	moved := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) { http.Redirect(rw, r, "http://evil.example/x", 302) }))
	defer moved.Close()
	wantRun(t, 2, w.monitorArgs(moved, filepath.Join(testdir.New(t), "s2"), 1), "redirect")
	// a server that is not there
	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	args := append([]string{}, w.monitorArgs(&httptest.Server{URL: url}, filepath.Join(testdir.New(t), "s3"), 1)...)
	wantRun(t, 3, args, "INCOMPLETE")
}

func TestMonitorUsageAndPlainHTTPToAnotherHost(t *testing.T) {
	w, a, st := monitorWorld(t)
	_ = a
	wantRun(t, 2, []string{"monitor", "--trust", w.trust, "--state", st}, "missing --source")
	wantRun(t, 2, []string{"monitor", "--trust", w.trust, "--source", "http://example.com/x", "--state", st, "--log", "aggregate:" + w.aggID}, "plain http")
	wantRun(t, 2, []string{"monitor", "--trust", w.trust, "--source", "https://user:pw@example.com/x", "--state", st, "--log", "aggregate:" + w.aggID}, "user name or password")
	wantRun(t, 2, []string{"monitor", "--trust", w.trust, "--source", w.dir, "--state", st}, "no log to watch")
	wantRun(t, 2, []string{"monitor", "--trust", w.trust, "--source", w.dir, "--state", st, "--log", "chain:" + w.chains[0].idHex}, "give the epoch")
}

// The number of entries one run reads is bounded (the memory of a run does not depend on it either: one entry bundle at a
// time); a monitor that is far behind catches up in several runs, and says each time how far it got.
func TestMonitorCatchesUpInSeveralRunsWhenTheAuditIsBounded(t *testing.T) {
	old := maxAuditEntries
	maxAuditEntries = 3
	defer func() { maxAuditEntries = old }()
	w, a, st := monitorWorld(t)
	w.publishChain(a, 8)
	srv := serve(t, w)
	out := wantRun(t, 0, w.monitorArgs(srv, st, 6), "VERIFIED:")
	n := strings.Count(out, "block header(s) checked")
	if n < 3 {
		t.Fatalf("the 8 blocks were read in %d step(s), expected several: %s", n, out)
	}
	meta, _, err := readMeta(st, false)
	if err != nil || meta.Audited["chain-"+a.idHex+"-e1"] != 8 {
		t.Fatalf("%+v %v", meta, err)
	}
}
